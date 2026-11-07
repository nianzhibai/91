package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/nianzhibai/91/backend/internal/api"
	"github.com/nianzhibai/91/backend/internal/auth"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives/googledrive"
	"github.com/nianzhibai/91/backend/internal/proxy"
)

// This test contacts Google's real playback API and CDN. Credentials stay in a
// local file; all site users, videos and shares belong to a temporary catalog.
func TestGoogleDrivePlaybackLive(t *testing.T) {
	cookieFile := os.Getenv("VIDEO_TEST_GOOGLE_DRIVE_COOKIE_FILE")
	fileID := os.Getenv("VIDEO_TEST_GOOGLE_DRIVE_FILE_ID")
	if cookieFile == "" && fileID == "" {
		t.Skip("set VIDEO_TEST_GOOGLE_DRIVE_COOKIE_FILE and VIDEO_TEST_GOOGLE_DRIVE_FILE_ID for live Google playback checks")
	}
	if cookieFile == "" || fileID == "" {
		t.Fatal("both a Google cookie file and an accessible video file ID are required")
	}
	cookie, err := os.ReadFile(cookieFile)
	if err != nil {
		t.Fatal("cannot read the Google cookie file")
	}
	if strings.TrimSpace(string(cookie)) == "" {
		t.Fatal("the Google cookie file is empty")
	}
	userAgent := os.Getenv("VIDEO_TEST_GOOGLE_DRIVE_USER_AGENT")
	if userAgent == "" {
		userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/157.0.0.0 Safari/537.36"
	}
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	now := time.Now()
	if err := cat.UpsertDrive(ctx, &catalog.Drive{ID: "google-live", Kind: googledrive.Kind, Name: "Live Google playback", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpsertVideo(ctx, &catalog.Video{ID: "live-video", DriveID: "google-live", FileID: fileID, Ext: "mp4", CreatedAt: now, UpdatedAt: now, PublishedAt: now}); err != nil {
		t.Fatal(err)
	}
	userID, err := cat.CreateUser(ctx, "live-test", "unused-password-hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.CreateSession(ctx, "live-test-session", time.Hour, userID); err != nil {
		t.Fatal(err)
	}
	registry := proxy.NewRegistry()
	registry.Set("google-live", googledrive.New(googledrive.Config{
		ID:               "google-live",
		PlaybackCookie:   strings.TrimSpace(string(cookie)),
		PlaybackAuthUser: os.Getenv("VIDEO_TEST_GOOGLE_DRIVE_AUTH_USER"),
	}))
	playbackProxy := proxy.New(registry)
	router := chi.NewRouter()
	(&api.Server{Catalog: cat, Proxy: playbackProxy}).RegisterRoutes(router, &auth.Authenticator{Catalog: cat})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(client.CloseIdleConnections)
	login := &http.Cookie{Name: "vs_admin", Value: "live-test-session"}

	request := func(method, path string, login *http.Cookie, payload io.Reader) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, payload)
		if err != nil {
			t.Fatal("cannot construct a site request")
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Range", "bytes=0-1023")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if login != nil {
			req.AddCookie(login)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("site request failed (%T)", err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	wantStatus := func(resp *http.Response, status int) {
		t.Helper()
		if resp.StatusCode != status {
			// Log only stable error codes, never signed URLs or response bodies.
			var failure struct{ Code string }
			_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&failure)
			t.Fatalf("%s %s returned HTTP %d (code=%q), want %d", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, failure.Code, status)
		}
	}
	manifest := func(path string, login *http.Cookie) api.PlaybackManifestDTO {
		t.Helper()
		resp := request(http.MethodGet, path, login, nil)
		defer resp.Body.Close()
		wantStatus(resp, http.StatusOK)
		if resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("playback metadata can be cached")
		}
		var result api.PlaybackManifestDTO
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
			t.Fatal("invalid playback metadata")
		}
		return result
	}
	readyFile := func(result api.PlaybackManifestDTO) api.PlaybackSourceDTO {
		t.Helper()
		for _, source := range result.Sources {
			if source.Kind == "transcoded" && source.Protocol == "file" && source.Status == "ready" && strings.HasPrefix(source.MediaType, "video/mp4") && source.URL != "" {
				return source
			}
		}
		t.Fatal("Google did not provide a playable MP4 rendition for this video")
		return api.PlaybackSourceDTO{}
	}

	wantStatus(request(http.MethodGet, "/api/video/live-video/playback?source=cloud", nil, nil), http.StatusUnauthorized)
	initial := manifest("/api/video/live-video/playback", login)
	if len(initial.Sources) != 2 || initial.Sources[0].Kind != "original" || initial.Sources[1].Status != "unknown" {
		t.Fatal("the initial manifest lost lazy cloud resolution or original playback")
	}
	resolved := manifest("/api/video/live-video/playback?source=cloud&refresh=1", login)
	source := readyFile(resolved)
	for _, rendition := range resolved.Sources[1:] {
		if rendition.Status != "ready" || rendition.URL != "/p/playback/live-video/"+rendition.ID || rendition.ExpiresAt != nil {
			t.Fatal("Google MP4 renditions need stable, ready authenticated routes")
		}
	}
	t.Logf("Google playback API: HTTP 200, %d compatible rendition(s), selected %s (%s)", len(resolved.Sources)-1, source.ID, source.Resolution)

	// Read a real CDN range without site cookies, Google cookies or OAuth headers.
	direct := request(http.MethodGet, source.URL, login, nil)
	wantStatus(direct, http.StatusFound)
	mediaURL, err := direct.Location()
	if err != nil || mediaURL.Scheme != "https" || !strings.HasSuffix(mediaURL.Hostname(), ".c.drive.google.com") {
		t.Fatal("invalid Google media redirect")
	}
	mediaReq, err := http.NewRequest(http.MethodGet, mediaURL.String(), nil)
	if err != nil {
		t.Fatal("cannot construct a CDN request")
	}
	mediaReq.Header.Set("User-Agent", userAgent)
	mediaReq.Header.Set("Range", "bytes=0-1023")
	mediaClient := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) >= 10 || next.URL.Scheme != "https" || !strings.HasSuffix(next.URL.Hostname(), ".c.drive.google.com") {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	t.Cleanup(mediaClient.CloseIdleConnections)
	media, err := mediaClient.Do(mediaReq)
	if err != nil {
		t.Fatalf("Google CDN request failed (%T)", err)
	}
	defer media.Body.Close()
	if media.StatusCode != http.StatusPartialContent {
		t.Fatalf("Google CDN returned HTTP %d, want 206", media.StatusCode)
	}
	expected, err := io.ReadAll(io.LimitReader(media.Body, 1025))
	if err != nil || len(expected) != 1024 || !strings.HasPrefix(media.Header.Get("Content-Range"), "bytes 0-1023/") {
		t.Fatal("Google CDN did not return the requested 1024-byte video range")
	}
	t.Log("Google CDN: HTTP 206, read 1024 video bytes using the viewer UA and no account credentials")

	checkDelivery := func(path string, login *http.Cookie, anonymousStatus int) {
		t.Helper()
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			for _, relay := range []bool{false, true} {
				target := path
				status := http.StatusFound
				if relay {
					target += "?tripleScreenRelay=1"
					status = http.StatusPartialContent
				}
				anonymous := request(method, target, nil, nil)
				wantStatus(anonymous, anonymousStatus)
				_ = anonymous.Body.Close()
				resp := request(method, target, login, nil)
				wantStatus(resp, status)
				if !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") || resp.Header.Get("Referrer-Policy") != "no-referrer" {
					t.Fatal("media delivery lost its cache or referrer policy")
				}
				body, err := io.ReadAll(io.LimitReader(resp.Body, 1025))
				_ = resp.Body.Close()
				if err != nil {
					t.Fatal("cannot read the playback response")
				}
				if method == http.MethodHead && len(body) != 0 {
					t.Fatal("HEAD returned a media body")
				}
				if relay && (!strings.HasPrefix(resp.Header.Get("Content-Range"), "bytes 0-1023/") || method == http.MethodGet && !bytes.Equal(body, expected)) {
					t.Fatal("the authenticated relay changed or lost the requested video range")
				}
			}
		}
	}
	checkDelivery(source.URL, login, http.StatusUnauthorized)
	t.Log("Signed-in playback: GET/HEAD 302; triple-screen GET/HEAD 206; anonymous requests 401")

	created := request(http.MethodPost, "/api/video/live-video/share", login, nil)
	wantStatus(created, http.StatusCreated)
	var share struct{ URL string }
	if err := json.NewDecoder(created.Body).Decode(&share); err != nil {
		t.Fatal("cannot decode the new share")
	}
	_ = created.Body.Close()
	claimBody, _ := json.Marshal(map[string]string{"token": strings.TrimPrefix(share.URL, "/share#")})
	claimed := request(http.MethodPost, "/api/share/consume", nil, bytes.NewReader(claimBody))
	wantStatus(claimed, http.StatusOK)
	var claim struct{ ShareID string }
	if err := json.NewDecoder(claimed.Body).Decode(&claim); err != nil {
		t.Fatal("cannot decode the claimed share")
	}
	_ = claimed.Body.Close()
	var shareCookie *http.Cookie
	for _, cookie := range claimed.Cookies() {
		if cookie.Name == "vs_video_share" {
			shareCookie = cookie
			break
		}
	}
	if shareCookie == nil || claim.ShareID == "" {
		t.Fatal("the claimed share has no viewer session")
	}
	sharedSource := readyFile(manifest("/api/share/"+url.PathEscape(claim.ShareID)+"/playback?source="+url.QueryEscape(source.ID), shareCookie))
	if sharedSource.URL != "/p/share/"+claim.ShareID+"/playback/"+source.ID {
		t.Fatal("shared playback escaped its viewer's route")
	}
	checkDelivery(sharedSource.URL, shareCookie, http.StatusNotFound)
	t.Log("Shared playback: GET/HEAD 302; triple-screen GET/HEAD 206; unclaimed requests 404")

	playbackProxy.SetAllowForcedRelay(false)
	resp := request(http.MethodGet, source.URL+"?tripleScreenRelay=1", login, nil)
	wantStatus(resp, http.StatusFound)
	_ = resp.Body.Close()
	t.Log("Disabling forced relay keeps triple-screen requests on 302 delivery")
}
