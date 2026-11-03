package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/video-site/backend/internal/auth"
	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/drives/googledrive"
	"github.com/video-site/backend/internal/proxy"
)

type apiUAFilePlaybackDrive struct{ apiPlaybackDrive }

func (d *apiUAFilePlaybackDrive) PlaybackSourcesWithUserAgent(ctx context.Context, file, userAgent string) ([]drives.PlaybackSource, error) {
	sources, err := d.PlaybackSources(ctx, file)
	if err != nil {
		return nil, err
	}
	result := append([]drives.PlaybackSource(nil), sources...)
	for i := range result {
		if result[i].Link == nil {
			continue
		}
		link := *result[i].Link
		link.Headers = http.Header{"User-Agent": {userAgent}}
		result[i].Link = &link
	}
	return result, nil
}

func TestPlaybackFileRoutesAuthorizeVideosAndSharesOnEveryRange(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	now := time.Now()
	if err := cat.UpsertDrive(ctx, &catalog.Drive{ID: "google", Kind: "googledrive", Name: "Google", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpsertVideo(ctx, &catalog.Video{ID: "video", DriveID: "google", FileID: "file", Ext: "ts", CreatedAt: now, UpdatedAt: now, PublishedAt: now}); err != nil {
		t.Fatal(err)
	}
	userID := createSessionUser(t, cat)
	if err := cat.CreateSession(ctx, "login", time.Hour, userID); err != nil {
		t.Fatal(err)
	}
	var mediaCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaCalls.Add(1)
		if r.URL.Query().Get("sig") != "private" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("invalid upstream request")
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 0-2/10")
		w.Header().Set("Content-Length", "3")
		w.WriteHeader(206)
		if r.Method != "HEAD" {
			_, _ = w.Write([]byte("mp4"))
		}
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	d := &apiUAFilePlaybackDrive{apiPlaybackDrive: apiPlaybackDrive{sources: []drives.PlaybackSource{{
		ID: "cloud-22", Label: "720p", Kind: "transcoded", Protocol: "file", Status: "ready", MediaType: "video/mp4",
		Link:      &drives.StreamLink{URL: upstream.URL + "/movie?sig=private", Expires: now.Add(time.Hour)},
		FileRelay: &drives.FileRelay{AllowedHosts: []string{u.Host}},
	}}}}
	lower := d.sources[0]
	lower.ID, lower.Label = "cloud-18", "360p"
	d.sources = append(d.sources, lower)
	registry := proxy.NewRegistry()
	registry.Set("google", d)
	s := &Server{Catalog: cat, Proxy: proxy.New(registry), shareNow: func() time.Time { return now }}
	router := chi.NewRouter()
	s.RegisterRoutes(router, &auth.Authenticator{Catalog: cat})
	login := &http.Cookie{Name: "vs_admin", Value: "login"}
	request := func(method, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Range", "bytes=0-2")
		r.Header.Set("User-Agent", "site-browser")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	decode := func(w *httptest.ResponseRecorder) PlaybackManifestDTO {
		t.Helper()
		var m PlaybackManifestDTO
		if w.Code != 200 {
			t.Fatalf("metadata=%d %s", w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(w.Body.String(), "sig=private") {
			t.Fatal("signed URL leaked in metadata")
		}
		return m
	}
	decode(request("GET", "/api/video/video/playback", login))
	if d.calls != 0 || mediaCalls.Load() != 0 {
		t.Fatal("listing candidates fetched cloud data")
	}
	manifest := decode(request("GET", "/api/video/video/playback?source=cloud", login))
	if len(manifest.Sources) != 3 {
		t.Fatal("MP4 resolution lost a rendition")
	}
	for _, source := range manifest.Sources[1:] {
		if source.Status != "ready" || source.URL != "/p/playback/video/"+source.ID || source.ExpiresAt != nil {
			t.Fatal("stable file routes inherited HLS resolution or upstream expiry state")
		}
	}
	mediaURL := manifest.Sources[1].URL
	if mediaURL != "/p/playback/video/cloud-22" || manifest.Sources[1].Protocol != "file" || manifest.Sources[0].URL != "/p/stream/google/file" {
		t.Fatalf("manifest=%+v", manifest)
	}
	if mediaCalls.Load() != 0 {
		t.Fatal("metadata transferred video bytes")
	}
	for _, method := range []string{"GET", "HEAD"} {
		if w := request(method, mediaURL, nil); w.Code != 401 {
			t.Fatalf("unauth %s=%d", method, w.Code)
		}
		if w := request(method, mediaURL, login); w.Code != 206 {
			t.Fatalf("media %s=%d %s", method, w.Code, w.Body.String())
		}
	}
	if w := request("GET", "/p/playback/missing/cloud-22", login); w.Code != 404 {
		t.Fatalf("missing video=%d", w.Code)
	}
	if w := request("GET", "/p/playback/video/original", login); w.Code != 404 {
		t.Fatalf("original was exposed through cloud route: %d", w.Code)
	}
	create := httptest.NewRequest("POST", "/api/video/video/share", nil)
	create.AddCookie(login)
	createdRR := httptest.NewRecorder()
	router.ServeHTTP(createdRR, create)
	if createdRR.Code != 201 {
		t.Fatalf("create share=%d %s", createdRR.Code, createdRR.Body.String())
	}
	var created createVideoShareResponse
	_ = json.Unmarshal(createdRR.Body.Bytes(), &created)
	consumedRR := httptest.NewRecorder()
	router.ServeHTTP(consumedRR, newConsumeShareRequest(strings.TrimPrefix(created.URL, "/share#")))
	if consumedRR.Code != 200 {
		t.Fatalf("consume=%d %s", consumedRR.Code, consumedRR.Body.String())
	}
	var consumed consumeVideoShareResponse
	_ = json.Unmarshal(consumedRR.Body.Bytes(), &consumed)
	shareCookie := responseCookieByName(t, consumedRR, videoShareCookieName)
	shared := decode(request("GET", consumed.Video.PlaybackURL+"?source=cloud", shareCookie))
	sharedMedia := shared.Sources[1].URL
	if sharedMedia != "/p/share/"+consumed.ShareID+"/playback/cloud-22" {
		t.Fatalf("shared media escaped its route: %s", sharedMedia)
	}
	if w := request("GET", sharedMedia, nil); w.Code != 404 {
		t.Fatalf("unclaimed share=%d", w.Code)
	}
	if w := request("GET", sharedMedia, shareCookie); w.Code != 206 {
		t.Fatalf("shared media=%d %s", w.Code, w.Body.String())
	}
	if err := cat.CreateSession(ctx, "other-login", time.Hour, userID); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", sharedMedia, &http.Cookie{Name: "vs_admin", Value: "other-login"}); w.Code != 404 {
		t.Fatalf("another viewer reused share=%d", w.Code)
	}
	// Redirect delivery still enters the same authenticated video/share routes.
	d.sources[0].Link.ClientRedirectSafe = true
	s.Proxy.InvalidateDrive("google")
	before := mediaCalls.Load()
	for _, method := range []string{"GET", "HEAD"} {
		if w := request(method, mediaURL, nil); w.Code != 401 {
			t.Fatalf("unauth redirect=%d", w.Code)
		}
		if w := request(method, sharedMedia, nil); w.Code != 404 {
			t.Fatalf("unclaimed redirect=%d", w.Code)
		}
		for path, cookie := range map[string]*http.Cookie{mediaURL: login, sharedMedia: shareCookie} {
			w := request(method, path, cookie)
			if w.Code != 302 || w.Header().Get("Location") != d.sources[0].Link.URL {
				t.Fatalf("authorized redirect=%d %v", w.Code, w.Header())
			}
		}
	}
	if mediaCalls.Load() != before {
		t.Fatal("authorized redirects transferred media bytes")
	}
	for name, sources := range map[string][]drives.PlaybackSource{
		"rendition disappeared":     {lower},
		"compatibility unavailable": {{ID: "cloud", Label: "兼容播放", Kind: "transcoded", Protocol: "file", Status: "unavailable", Message: "网页登录已失效，请更新 Cookie"}},
	} {
		t.Run(name, func(t *testing.T) {
			d.sources = sources
			s.Proxy.InvalidateDriveLinks("google")
			for _, viewer := range []struct {
				media, metadata string
				cookie          *http.Cookie
				anonymousStatus int
			}{
				{mediaURL, "/api/video/video/playback", login, 401},
				{sharedMedia, consumed.Video.PlaybackURL, shareCookie, 404},
			} {
				for _, method := range []string{"GET", "HEAD"} {
					if w := request(method, viewer.media, nil); w.Code != viewer.anonymousStatus {
						t.Fatalf("unauthorized stale rendition=%d", w.Code)
					}
					w := request(method, viewer.media, viewer.cookie)
					var payload map[string]string
					_ = json.Unmarshal(w.Body.Bytes(), &payload)
					if w.Code != 410 || payload["code"] != "playback_source_expired" {
						t.Fatalf("rendition failure became an access failure: %d %s", w.Code, w.Body.String())
					}
				}
				updated := decode(request("GET", viewer.metadata+"?source=cloud-22&refresh=1", viewer.cookie))
				if len(updated.Sources) != 2 || updated.Sources[0].Status != "ready" || updated.Sources[1].ID != sources[0].ID || updated.Sources[1].Status != sources[0].Status {
					t.Fatalf("stale selection did not return current candidates: %+v", updated)
				}
			}
			if mediaCalls.Load() != before {
				t.Fatal("unavailable rendition transferred media bytes")
			}
		})
	}
}

func TestGoogleDriveWebLoginFailurePreservesOriginalPlayback(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/playback") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer oauth-secret" || r.Header.Get("Cookie") != "" {
			t.Error("original playback did not retain independent OAuth authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"file","name":"video.mp4","mimeType":"video/mp4"}`))
	}))
	defer upstream.Close()
	d := googledrive.New(googledrive.Config{AccessToken: "oauth-secret", PlaybackCookie: "SAPISID=web-secret", APIBaseURL: upstream.URL, PlaybackAPIURL: upstream.URL})
	registry := proxy.NewRegistry()
	registry.Set("google", d)
	s := &Server{Proxy: proxy.New(registry)}
	video := &catalog.Video{ID: "video", DriveID: "google", FileID: "file", Ext: "mp4"}
	r := httptest.NewRequest("GET", "/api/video/video/playback?source=cloud", nil)
	r.Header.Set("User-Agent", "viewer")
	w := httptest.NewRecorder()
	s.servePlaybackMetadata(w, r, video, "/p/stream/google/file")
	var manifest PlaybackManifestDTO
	if err := json.Unmarshal(w.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || len(manifest.Sources) != 2 || manifest.Sources[0].Status != "ready" || manifest.Sources[1].Status != "unavailable" || !strings.Contains(manifest.Sources[1].Message, "Cookie") || strings.Contains(w.Body.String(), "web-secret") {
		t.Fatalf("web login failure affected original playback: %d %s", w.Code, w.Body.String())
	}
	link, err := d.StreamURL(context.Background(), "file")
	if err != nil || link == nil || link.Headers.Get("Authorization") != "Bearer oauth-secret" {
		t.Fatalf("original OAuth playback is no longer usable: %v", err)
	}
}

func TestGoogleDriveWebCookieCanBeRemovedIndependently(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if err := cat.UpsertDrive(ctx, &catalog.Drive{ID: "google", Kind: "googledrive", Name: "Google", RootID: "root", Credentials: map[string]string{"client_id": "client", "client_secret": "secret", "refresh_token": "refresh", "playback_cookie": "old-cookie"}}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/admin/api/drives", strings.NewReader(`{"id":"google","kind":"googledrive","name":"Google","rootId":"root","credentials":{"playback_cookie":""}}`))
	w := httptest.NewRecorder()
	(&AdminServer{Catalog: cat}).handleUpsertDrive(w, r)
	if w.Code != 200 {
		t.Fatalf("save=%d %s", w.Code, w.Body.String())
	}
	d, err := cat.GetDrive(ctx, "google")
	if err != nil {
		t.Fatal(err)
	}
	if d.Credentials["playback_cookie"] != "" || d.Credentials["refresh_token"] != "refresh" || d.Credentials["client_secret"] != "secret" {
		t.Fatal("cookie removal changed OAuth credentials or retained the cookie")
	}
}

func TestGoogleDriveWebAccountChangesPreserveOtherCredentials(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID: "google", Kind: "googledrive", Name: "Google",
		Credentials: map[string]string{"refresh_token": "oauth-refresh", "playback_cookie": "web-cookie", "playback_auth_user": "3"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		value, want string
		status      int
	}{
		{"2", "2", http.StatusOK},
		{"", "0", http.StatusOK},
		{"-1", "0", http.StatusBadRequest},
		{"account", "0", http.StatusBadRequest},
	} {
		body, _ := json.Marshal(map[string]any{
			"id": "google", "kind": "googledrive", "name": "Google",
			"credentials": map[string]string{"playback_auth_user": tc.value},
		})
		r := httptest.NewRequest(http.MethodPost, "/admin/api/drives", strings.NewReader(string(body)))
		w := httptest.NewRecorder()
		(&AdminServer{Catalog: cat}).handleUpsertDrive(w, r)
		if w.Code != tc.status {
			t.Fatalf("save account=%q returned HTTP %d, want %d", tc.value, w.Code, tc.status)
		}
		d, err := cat.GetDrive(ctx, "google")
		if err != nil {
			t.Fatal(err)
		}
		if d.Credentials["playback_auth_user"] != tc.want || d.Credentials["refresh_token"] != "oauth-refresh" || d.Credentials["playback_cookie"] != "web-cookie" {
			t.Fatal("account selection reset other credentials or retained an invalid account")
		}
	}
}
