package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	"github.com/video-site/backend/internal/proxy"
)

type apiPlaybackDrive struct {
	drives.Drive
	sources []drives.PlaybackSource
	err     error
	calls   int
}

func (d *apiPlaybackDrive) Kind() string { return "wopan" }
func (d *apiPlaybackDrive) PlaybackSources(context.Context, string) ([]drives.PlaybackSource, error) {
	d.calls++
	return d.sources, d.err
}

type apiPreparingPlaybackDrive struct {
	apiPlaybackDrive
	prepares atomic.Int32
}

func (d *apiPreparingPlaybackDrive) PreparePlayback(context.Context, string) ([]drives.PlaybackSource, error) {
	d.prepares.Add(1)
	return d.sources, d.err
}

func TestPlaybackManifestAndLazyCloudResolution(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	now := time.Now()
	if err := cat.UpsertDrive(ctx, &catalog.Drive{ID: "wo", Kind: "wopan", Name: "Wo", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{"ts", "mp4"} {
		if err := cat.UpsertVideo(ctx, &catalog.Video{ID: ext, DriveID: "wo", FileID: ext, Ext: ext, CreatedAt: now, UpdatedAt: now, PublishedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.CreateSession(ctx, "login", time.Hour, createSessionUser(t, cat)); err != nil {
		t.Fatal(err)
	}
	media := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("metadata fetched media bytes") }))
	defer media.Close()
	d := &apiPlaybackDrive{err: errors.New("preview unavailable")}
	registry := proxy.NewRegistry()
	registry.Set("wo", d)
	s := &Server{Catalog: cat, Proxy: proxy.New(registry)}
	router := chi.NewRouter()
	s.RegisterRoutes(router, &auth.Authenticator{Catalog: cat})
	request := func(path string, authorized bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if authorized {
			req.AddCookie(&http.Cookie{Name: "vs_admin", Value: "login"})
		}
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}
	decode := func(rr *httptest.ResponseRecorder) PlaybackManifestDTO {
		t.Helper()
		var manifest PlaybackManifestDTO
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &manifest); err != nil {
			t.Fatal(err)
		}
		if rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("metadata must not be HTTP cached")
		}
		return manifest
	}
	for _, path := range []string{"/api/video/ts/playback", "/api/video/ts/playback?source=cloud"} {
		if rr := request(path, false); rr.Code != http.StatusUnauthorized {
			t.Fatalf("unauth status=%d", rr.Code)
		}
	}
	for _, ext := range []string{"ts", "mp4"} {
		manifest := decode(request("/api/video/"+ext+"/playback", true))
		if len(manifest.Sources) != 2 || manifest.Sources[0].ID != "original" || manifest.Sources[0].URL != "/p/stream/wo/"+ext || manifest.Sources[1].Status != "unknown" || !strings.HasSuffix(manifest.Sources[1].ResolveURL, "?source=cloud") {
			t.Fatalf("manifest=%+v", manifest)
		}
		decode(request("/api/video/"+ext+"/playback?source=original&refresh=1", true))
	}
	if d.calls != 0 {
		t.Fatal("original playback waited for cloud resolution")
	}
	for _, tc := range []struct {
		name, status string
		link         *drives.StreamLink
	}{
		{"ready", "ready", &drives.StreamLink{URL: media.URL + "/index.m3u8", ClientRedirectSafe: true, Expires: now.Add(time.Minute)}},
		{"processing", "processing", nil},
		{"unavailable", "unavailable", nil},
		{"secret headers", "ready", &drives.StreamLink{URL: media.URL + "/index.m3u8", ClientRedirectSafe: true, Headers: http.Header{"Cookie": {"account-secret"}}}},
		{"unsafe credentials", "ready", &drives.StreamLink{URL: "https://account-secret@example.com/index.m3u8", ClientRedirectSafe: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d.err = nil
			d.sources = []drives.PlaybackSource{{ID: "cloud", Label: "兼容播放", Kind: "transcoded", Protocol: "hls", Status: tc.status, Link: tc.link}}
			manifest := decode(request("/api/video/ts/playback?source=cloud&refresh=1", true))
			if len(manifest.Sources) != 2 || manifest.Sources[0].URL != "/p/stream/wo/ts" {
				t.Fatalf("cloud removed original: %+v", manifest)
			}
			cloud := manifest.Sources[1]
			want := tc.status
			if strings.HasPrefix(tc.name, "secret") || strings.HasPrefix(tc.name, "unsafe") {
				want = "unavailable"
			}
			if cloud.Status != want {
				t.Fatalf("cloud=%+v", cloud)
			}
			if want != "ready" && cloud.URL != "" {
				t.Fatal("unavailable link exposed")
			}
			if strings.Contains(request("/api/video/ts/playback", true).Body.String(), "account-secret") {
				t.Fatal("account credential leaked")
			}
		})
	}
	d.err = errors.New("request failed with rsp_code: 1001,rep_desc: 无效登录信息")
	if rr := request("/api/video/ts/playback?source=cloud&refresh=1", true); rr.Code != http.StatusFailedDependency || !strings.Contains(rr.Body.String(), "drive_auth_failed") {
		t.Fatalf("provider error lost: %d %s", rr.Code, rr.Body.String())
	}
	decode(request("/api/video/ts/playback", true))
	if rr := request("/api/video/mp4/playback?source=missing", true); rr.Code != 400 {
		t.Fatalf("invalid source status=%d", rr.Code)
	}
	registry.Set("wo", &apiOriginalOnlyDrive{})
	manifest := decode(request("/api/video/mp4/playback", true))
	if len(manifest.Sources) != 1 || manifest.Sources[0].ID != "original" {
		t.Fatalf("original-only drive=%+v", manifest)
	}
	preparing := &apiPreparingPlaybackDrive{apiPlaybackDrive: apiPlaybackDrive{sources: []drives.PlaybackSource{{
		ID: "cloud", Label: "兼容播放", Kind: "transcoded", Protocol: "hls", Status: "processing", Message: "正在转码",
	}}}}
	registry.Set("wo", preparing)
	s.Proxy.InvalidateDrive("wo")
	manifest = decode(request("/api/video/ts/playback?source=cloud", true))
	if manifest.Sources[1].PrepareURL != "/api/video/ts/playback/prepare" || preparing.prepares.Load() != 0 {
		t.Fatalf("query prepared media or lost preparation URL: %+v", manifest)
	}
	for _, authorized := range []bool{false, true} {
		req := httptest.NewRequest("POST", "/api/video/ts/playback/prepare", nil)
		if authorized {
			req.AddCookie(&http.Cookie{Name: "vs_admin", Value: "login"})
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if !authorized {
			if w.Code != 401 || preparing.prepares.Load() != 0 {
				t.Fatalf("unauthorized preparation: status=%d calls=%d", w.Code, preparing.prepares.Load())
			}
			continue
		}
		manifest = decode(w)
		if manifest.Sources[1].ResolveURL != "/api/video/ts/playback?source=cloud" || preparing.prepares.Load() != 1 {
			t.Fatalf("POST did not prepare or returned a POST-only polling URL: %+v", manifest)
		}
	}
}

type apiOriginalOnlyDrive struct{ drives.Drive }

func (*apiOriginalOnlyDrive) Kind() string { return "webdav" }

func TestPlaybackMetadataPrioritizesDriveInitializationErrors(t *testing.T) {
	for _, tc := range []struct {
		name, code, message string
		err                 error
	}{
		{"authorization", "drive_auth_failed", "联通网盘登录或授权已失效，请联系管理员重新登录", errors.New("request failed with rsp_code: 1001,rep_desc: 无效的令牌！")},
		{"unavailable", "drive_upstream_unavailable", "联通网盘上游服务暂时不可用，请稍后重试", &drives.ProviderError{Kind: drives.ProviderErrorUnavailable, Err: errors.New("connection failed")}},
		{"throttled", "drive_rate_limited", "联通网盘当前正在限流，请稍后重试", &drives.RateLimitError{Provider: "wopan", RetryAfter: time.Minute, Err: errors.New("rate limited")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := proxy.NewRegistry()
			p := proxy.New(registry)
			p.SetDriveInitError("wo", "wopan", tc.err)
			s := &Server{Proxy: p}
			for _, ext := range []string{"ts", "mp4"} {
				video := &catalog.Video{ID: ext, DriveID: "wo", FileID: ext, Ext: ext}
				for _, base := range []string{"/api/video/" + ext + "/playback", "/api/share/claimed/playback"} {
					for _, target := range []struct{ method, suffix string }{
						{http.MethodGet, ""},
						{http.MethodGet, "?source=original&refresh=1"},
						{http.MethodGet, "?source=cloud&refresh=1"},
						{http.MethodPost, "/prepare"},
					} {
						r := httptest.NewRequest(target.method, base+target.suffix, nil)
						w := httptest.NewRecorder()
						s.servePlaybackMetadata(w, r, video, "/p/stream/wo/"+ext)
						if w.Code != http.StatusFailedDependency {
							t.Fatalf("%s %s: status=%d body=%s", target.method, r.URL, w.Code, w.Body.String())
						}
						var payload struct{ Code, Message string }
						if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
							t.Fatal(err)
						}
						if payload.Code != tc.code || payload.Message != tc.message {
							t.Fatalf("%s %s: payload=%+v", target.method, r.URL, payload)
						}
						if strings.Contains(w.Body.String(), `"sources"`) || w.Header().Get("Cache-Control") != "no-store" {
							t.Fatalf("failed drive advertised or cached playback sources: %s", w.Body.String())
						}
					}
				}
			}

			// Reattaching after credentials are repaired clears the mount error;
			// metadata remains lazy and does not request cloud previews or bytes.
			p.InvalidateDrive("wo")
			d := &apiPlaybackDrive{}
			registry.Set("wo", d)
			w := httptest.NewRecorder()
			s.servePlaybackMetadata(w, httptest.NewRequest(http.MethodGet, "/api/video/ts/playback", nil),
				&catalog.Video{ID: "ts", DriveID: "wo", FileID: "ts", Ext: "ts"}, "/p/stream/wo/ts")
			var manifest PlaybackManifestDTO
			if err := json.Unmarshal(w.Body.Bytes(), &manifest); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusOK || len(manifest.Sources) != 2 || manifest.Sources[1].Status != "unknown" || d.calls != 0 {
				t.Fatalf("recovered drive: status=%d manifest=%+v preview calls=%d", w.Code, manifest, d.calls)
			}
		})
	}
}

func TestHLSPlaybackAuthenticatesEveryResourceAndShare(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	now := time.Now()
	if err := cat.UpsertDrive(ctx, &catalog.Drive{ID: "115", Kind: "p115", Name: "115", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"video", "other"} {
		if err := cat.UpsertVideo(ctx, &catalog.Video{ID: id, DriveID: "115", FileID: id, Ext: "mp4", CreatedAt: now, UpdatedAt: now, PublishedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.CreateSession(ctx, "login", time.Hour, createSessionUser(t, cat)); err != nil {
		t.Fatal(err)
	}
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.URL.Path != "/segment" || r.Header.Get("Cookie") != "" || r.UserAgent() != "fixed-ua" {
			t.Errorf("invalid media request: %s %v", r.URL, r.Header)
		}
		io.WriteString(w, "video-bytes")
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	d := &apiPlaybackDrive{sources: []drives.PlaybackSource{{ID: "cloud", Label: "兼容播放", Kind: "transcoded", Protocol: "hls", Status: "ready",
		Link:  &drives.StreamLink{URL: upstream.URL + "/master", Headers: http.Header{"User-Agent": {"fixed-ua"}}, Expires: now.Add(time.Minute)},
		Relay: &drives.HLSRelay{Playlist: "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nsegment?signature=private\n#EXT-X-ENDLIST\n", AllowedHosts: []string{u.Host}},
	}}}
	registry := proxy.NewRegistry()
	registry.Set("115", d)
	s := &Server{Catalog: cat, Proxy: proxy.New(registry), shareNow: func() time.Time { return now }}
	router := chi.NewRouter()
	s.RegisterRoutes(router, &auth.Authenticator{Catalog: cat})
	login := &http.Cookie{Name: "vs_admin", Value: "login"}
	request := func(path string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	decode := func(w *httptest.ResponseRecorder) PlaybackManifestDTO {
		t.Helper()
		var manifest PlaybackManifestDTO
		if w.Code != 200 {
			t.Fatalf("metadata=%d %s", w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), &manifest); err != nil {
			t.Fatal(err)
		}
		return manifest
	}
	manifest := decode(request("/api/video/video/playback", login))
	if d.calls != 0 || manifest.Sources[1].Status != "unknown" {
		t.Fatal("HLS was resolved before selection")
	}
	manifest = decode(request("/api/video/video/playback?source=cloud", login))
	if len(manifest.Sources) != 2 || manifest.Sources[0].URL != "/p/stream/115/video" || manifest.Sources[1].Status != "ready" {
		t.Fatalf("manifest=%+v", manifest)
	}
	root := manifest.Sources[1].URL
	if !strings.HasPrefix(root, "/p/hls/video/") || strings.Contains(root, upstream.URL) {
		t.Fatalf("invalid relay URL %s", root)
	}
	if w := request(root, nil); w.Code != 401 {
		t.Fatalf("unauthenticated master=%d", w.Code)
	}
	w := request(root, login)
	if w.Code != 200 || strings.Contains(w.Body.String(), "signature=private") || upstreamCalls.Load() != 0 {
		t.Fatalf("master=%d %s", w.Code, w.Body.String())
	}
	var segment string
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			segment = line
		}
	}
	if w := request(segment, nil); w.Code != 401 {
		t.Fatalf("unauthenticated segment=%d", w.Code)
	}
	if w := request(strings.Replace(segment, "/p/hls/video/", "/p/hls/other/", 1), login); w.Code != 404 {
		t.Fatalf("cross-video request=%d", w.Code)
	}
	if w := request(segment+"?signature=attacker", login); w.Code != 200 || w.Body.String() != "video-bytes" {
		t.Fatalf("media=%d %s", w.Code, w.Body.String())
	}

	// A claimed share can resolve and fetch the same source with only its
	// share cookie; none of its rewritten links escape into login-only routes.
	create := httptest.NewRequest("POST", "/api/video/video/share", nil)
	create.AddCookie(login)
	createdRR := httptest.NewRecorder()
	router.ServeHTTP(createdRR, create)
	if createdRR.Code != 201 {
		t.Fatalf("share creation=%d %s", createdRR.Code, createdRR.Body.String())
	}
	var created createVideoShareResponse
	json.Unmarshal(createdRR.Body.Bytes(), &created)
	consumeRR := httptest.NewRecorder()
	router.ServeHTTP(consumeRR, newConsumeShareRequest(strings.TrimPrefix(created.URL, "/share#")))
	if consumeRR.Code != 200 {
		t.Fatalf("share claim=%d %s", consumeRR.Code, consumeRR.Body.String())
	}
	var consumed consumeVideoShareResponse
	json.Unmarshal(consumeRR.Body.Bytes(), &consumed)
	shareCookie := responseCookieByName(t, consumeRR, videoShareCookieName)
	shared := decode(request(consumed.Video.PlaybackURL+"?source=cloud", shareCookie))
	sharedRoot := shared.Sources[1].URL
	if !strings.HasPrefix(sharedRoot, "/p/share/"+consumed.ShareID+"/hls/") {
		t.Fatalf("share escaped its route: %s", sharedRoot)
	}
	if w := request(sharedRoot, nil); w.Code != 404 {
		t.Fatalf("unclaimed HLS=%d", w.Code)
	}
	w = request(sharedRoot, shareCookie)
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			segment = line
		}
	}
	if w.Code != 200 || !strings.HasPrefix(segment, "/p/share/"+consumed.ShareID+"/hls/") {
		t.Fatalf("invalid shared playlist: %d %s", w.Code, w.Body.String())
	}
	if w := request(segment, shareCookie); w.Code != 200 {
		t.Fatalf("shared media=%d", w.Code)
	}
	preparing := &apiPreparingPlaybackDrive{apiPlaybackDrive: apiPlaybackDrive{sources: []drives.PlaybackSource{{
		ID: "cloud", Label: "兼容播放", Kind: "transcoded", Protocol: "hls", Status: "processing",
	}}}}
	registry.Set("115", preparing)
	s.Proxy.InvalidateDrive("115")
	shared = decode(request(consumed.Video.PlaybackURL+"?source=cloud", shareCookie))
	preparePath := shared.Sources[1].PrepareURL
	if preparePath != consumed.Video.PlaybackURL+"/prepare" {
		t.Fatalf("shared preparation escaped its route: %q", preparePath)
	}
	prepareShared := func(cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", preparePath, nil)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	if w := prepareShared(nil); w.Code != 404 || preparing.prepares.Load() != 0 {
		t.Fatal("unclaimed share prepared media")
	}
	shared = decode(prepareShared(shareCookie))
	if preparing.prepares.Load() != 1 || shared.Sources[1].ResolveURL != consumed.Video.PlaybackURL+"?source=cloud" {
		t.Fatalf("shared preparation: %+v", shared)
	}
	now = now.Add(videoShareSessionTTL)
	if w := prepareShared(shareCookie); w.Code != 404 || preparing.prepares.Load() != 1 {
		t.Fatal("expired share prepared media")
	}
	if w := request(segment, shareCookie); w.Code != 404 {
		t.Fatalf("expired share media=%d", w.Code)
	}
	if err := cat.HideVideo(ctx, "video"); err != nil {
		t.Fatal(err)
	}
	if w := request(root, login); w.Code != 404 {
		t.Fatalf("hidden video master=%d", w.Code)
	}
}

func TestNamedRenditionsCreateOnlyTheSelectedMediaSession(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("metadata must not fetch media bytes") }))
	defer upstream.Close()
	host, _ := url.Parse(upstream.URL)
	makeSource := func(id, label string) drives.PlaybackSource {
		return drives.PlaybackSource{ID: id, Label: label, Kind: "transcoded", Protocol: "hls", Status: "ready", Resolution: strings.TrimPrefix(id, "cloud-"), Width: 540, Height: 960, Bitrate: 1200, Codec: "h264", Link: &drives.StreamLink{URL: upstream.URL + "/" + id + ".m3u8", Expires: time.Now().Add(time.Minute)}, Relay: &drives.HLSRelay{AllowedHosts: []string{host.Host}, Authorize: func(r *http.Request) { r.Header.Set("Cookie", "account-secret") }}}
	}
	drive := &apiPlaybackDrive{sources: []drives.PlaybackSource{makeSource("cloud-high", "高清"), makeSource("cloud-low", "流畅")}}
	registry := proxy.NewRegistry()
	registry.Set("drive", drive)
	server := &Server{Proxy: proxy.New(registry)}
	request := func(source string) PlaybackManifestDTO {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/video/video/playback?source="+source, nil)
		server.servePlaybackMetadata(w, r, &catalog.Video{ID: "video", DriveID: "drive", FileID: "file", Ext: "mp4"}, "/p/stream/drive/file")
		if w.Code != 200 {
			t.Fatalf("status=%d %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "account-secret") || strings.Contains(w.Body.String(), upstream.URL) {
			t.Fatal("provider credentials or signed URL exposed")
		}
		var manifest PlaybackManifestDTO
		if err := json.Unmarshal(w.Body.Bytes(), &manifest); err != nil {
			t.Fatal(err)
		}
		return manifest
	}
	group := request("cloud")
	if len(group.Sources) != 3 || group.Sources[1].Status != "ready" || group.Sources[2].Status != "unknown" || group.Sources[2].URL != "" {
		t.Fatalf("group=%+v", group)
	}
	high := group.Sources[1]
	if high.Width != 540 || high.Bitrate != 1200 || high.ResolveURL != "/api/video/video/playback?source=cloud-high" {
		t.Fatalf("lost rendition metadata: %+v", high)
	}
	low := request("cloud-low")
	if low.Sources[1].Status != "unknown" || low.Sources[1].URL != "" || low.Sources[2].Status != "ready" || !strings.HasPrefix(low.Sources[2].URL, "/p/hls/video/") {
		t.Fatalf("low=%+v", low)
	}
	if drive.calls != 1 {
		t.Fatalf("selecting a cached rendition re-fetched provider metadata: %d", drive.calls)
	}
}

type apiObservedOriginalDrive struct {
	apiPlaybackDrive
	link        *drives.StreamLink
	resolutions int
}

func (d *apiObservedOriginalDrive) OriginalPlaybackURL(context.Context, string) (*drives.StreamLink, error) {
	d.resolutions++
	return d.link, nil
}

func TestOriginalSourceInfoReportsActualStreamsWithoutFetchingOrCrossingViewers(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "video") }))
	defer upstream.Close()
	d := &apiObservedOriginalDrive{link: &drives.StreamLink{URL: upstream.URL, URLSource: "quark-raw", Headers: http.Header{"Cookie": {"account-secret"}}, Expires: time.Now().Add(time.Minute)}}
	reg := proxy.NewRegistry()
	reg.Set("drive", d)
	p := proxy.New(reg)
	s := &Server{Proxy: p}
	v := &catalog.Video{ID: "video", DriveID: "drive", FileID: "file", Ext: "mp4"}
	metadata := func(target string) PlaybackSourceDTO {
		t.Helper()
		w := httptest.NewRecorder()
		s.servePlaybackMetadata(w, httptest.NewRequest("GET", target, nil), v, "/p/stream/drive/file")
		if w.Code != 200 {
			t.Fatalf("metadata=%d %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "account-secret") || strings.Contains(w.Body.String(), upstream.URL) {
			t.Fatal("private upstream data exposed")
		}
		var m PlaybackManifestDTO
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m.Sources[0]
	}
	first := metadata("/api/video/video/playback")
	second := metadata("/api/video/video/playback")
	if first.SourceInfoURL == "" || first.SourceInfoURL == second.SourceInfoURL || first.URLSource != "" || d.resolutions != 0 {
		t.Fatal("rendering inferred a source or fetched a link")
	}
	stream := func(source PlaybackSourceDTO, want string) {
		t.Helper()
		w := httptest.NewRecorder()
		p.ServeStream(w, httptest.NewRequest("GET", source.URL, nil), "drive", "file")
		if w.Code != 200 || w.Header().Get("X-Playback-Source") != want {
			t.Fatalf("stream=%d %v", w.Code, w.Header())
		}
	}
	stream(first, "quark-raw")
	if got := metadata(first.SourceInfoURL).URLSource; got != "quark-raw" {
		t.Fatalf("first source=%q", got)
	}
	if got := metadata(second.SourceInfoURL).URLSource; got != "" {
		t.Fatal("unplayed viewer inherited another viewer's result")
	}
	d.link = &drives.StreamLink{URL: upstream.URL, URLSource: "quark-download", Expires: time.Now().Add(time.Minute)}
	p.InvalidateDriveLinks("drive")
	stream(second, "quark-download")
	if got := metadata(second.SourceInfoURL).URLSource; got != "quark-download" {
		t.Fatalf("second source=%q", got)
	}
	if got := metadata(first.SourceInfoURL).URLSource; got != "quark-raw" {
		t.Fatal("later resolution changed an older viewer's provenance")
	}
	if d.resolutions != 2 || d.calls != 0 {
		t.Fatalf("source info resolved links: original=%d cloud=%d", d.resolutions, d.calls)
	}
	p.InvalidateDrive("drive")
	if got := metadata(first.SourceInfoURL).URLSource; got != "" {
		t.Fatal("account remount retained source info")
	}
}
