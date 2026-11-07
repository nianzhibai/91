package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/nianzhibai/91/backend/internal/auth"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/proxy"
	"github.com/nianzhibai/91/backend/internal/subtitles"
)

func TestVideoShareSessionTTL(t *testing.T) {
	if videoShareSessionTTL != 6*time.Hour {
		t.Fatalf("video share session TTL = %s, want 6h", videoShareSessionTTL)
	}
}

func TestOneTimeShareRouteClaimsOnceAndStreamsWithoutLogin(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	uploadDir := t.TempDir()
	const fileName = "shared.mp4"
	const fileBody = "video-bytes"
	if err := os.WriteFile(filepath.Join(uploadDir, fileName), []byte(fileBody), 0o644); err != nil {
		t.Fatalf("write upload: %v", err)
	}
	now := time.Now().Truncate(time.Millisecond)
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "video-1",
		DriveID:     localUploadDriveID,
		FileID:      fileName,
		Title:       "Shared video",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	const loginToken = "authenticated-session"
	if err := cat.CreateSession(ctx, loginToken, time.Hour, createSessionUser(t, cat)); err != nil {
		t.Fatalf("create login session: %v", err)
	}
	authenticator := &auth.Authenticator{Catalog: cat}
	subtitleUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/shared.srt" {
			t.Fatalf("subtitle upstream path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte("shared subtitle"))
	}))
	defer subtitleUpstream.Close()
	server := &Server{
		Catalog: cat,
		Proxy:   proxy.New(proxy.NewRegistry()),
		SubtitleClient: &apiFakeSubtitleClient{subtitles: []subtitles.Subtitle{
			{Name: "简体中文", Ext: "srt", Language: "zh-CN", URL: subtitleUpstream.URL + "/shared.srt"},
		}},
		UploadDir: uploadDir,
		shareNow:  func() time.Time { return now },
	}
	router := chi.NewRouter()
	server.RegisterRoutes(router, authenticator)

	unauthorizedCreate := httptest.NewRequest(http.MethodPost, "/api/video/video-1/share", nil)
	unauthorizedCreateRR := httptest.NewRecorder()
	router.ServeHTTP(unauthorizedCreateRR, unauthorizedCreate)
	if unauthorizedCreateRR.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized create status = %d, want 401", unauthorizedCreateRR.Code)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/video/video-1/share", nil)
	createReq.AddCookie(&http.Cookie{Name: "vs_admin", Value: loginToken})
	createRR := httptest.NewRecorder()
	router.ServeHTTP(createRR, createReq)
	if createRR.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", createRR.Code, createRR.Body.String())
	}
	var created createVideoShareResponse
	if err := json.NewDecoder(createRR.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if !strings.HasPrefix(created.URL, "/share#") {
		t.Fatalf("share URL = %q", created.URL)
	}
	token := strings.TrimPrefix(created.URL, "/share#")

	consumeReq := newConsumeShareRequest(token)
	consumeRR := httptest.NewRecorder()
	router.ServeHTTP(consumeRR, consumeReq)
	if consumeRR.Code != http.StatusOK {
		t.Fatalf("consume status = %d, body = %s", consumeRR.Code, consumeRR.Body.String())
	}
	var consumed consumeVideoShareResponse
	if err := json.NewDecoder(consumeRR.Body).Decode(&consumed); err != nil {
		t.Fatalf("decode consume response: %v", err)
	}
	if consumed.ShareID == "" {
		t.Fatal("consume response has empty share id")
	}
	wantStream := "/p/share/" + consumed.ShareID + "/stream"
	if consumed.Video.VideoSrc != wantStream {
		t.Fatalf("shared video src = %q, want %q", consumed.Video.VideoSrc, wantStream)
	}
	shareCookie := responseCookieByName(t, consumeRR, videoShareCookieName)
	for _, authorized := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodGet, consumed.Video.PlaybackURL, nil)
		if authorized {
			req.AddCookie(shareCookie)
		}
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if !authorized && rr.Code != http.StatusNotFound {
			t.Fatalf("unclaimed playback status = %d", rr.Code)
		}
		if authorized {
			var manifest PlaybackManifestDTO
			if err := json.Unmarshal(rr.Body.Bytes(), &manifest); err != nil {
				t.Fatal(err)
			}
			if rr.Code != http.StatusOK || len(manifest.Sources) == 0 || manifest.Sources[0].URL != wantStream {
				t.Fatalf("share playback = %d %+v", rr.Code, manifest)
			}
		}
	}
	if !shareCookie.HttpOnly || shareCookie.MaxAge != int(videoShareSessionTTL.Seconds()) {
		t.Fatalf("share cookie = %#v", shareCookie)
	}

	secondBrowserReq := newConsumeShareRequest(token)
	secondBrowserRR := httptest.NewRecorder()
	router.ServeHTTP(secondBrowserRR, secondBrowserReq)
	if secondBrowserRR.Code != http.StatusGone {
		t.Fatalf("second browser status = %d, want 410; body = %s", secondBrowserRR.Code, secondBrowserRR.Body.String())
	}

	sameBrowserReq := newConsumeShareRequest(token)
	sameBrowserReq.AddCookie(shareCookie)
	sameBrowserRR := httptest.NewRecorder()
	router.ServeHTTP(sameBrowserRR, sameBrowserReq)
	if sameBrowserRR.Code != http.StatusGone {
		t.Fatalf("same browser retry status = %d, want 410; body = %s", sameBrowserRR.Code, sameBrowserRR.Body.String())
	}

	streamWithoutCookie := httptest.NewRequest(http.MethodGet, wantStream, nil)
	streamWithoutCookieRR := httptest.NewRecorder()
	router.ServeHTTP(streamWithoutCookieRR, streamWithoutCookie)
	if streamWithoutCookieRR.Code != http.StatusNotFound {
		t.Fatalf("stream without share cookie status = %d, want 404", streamWithoutCookieRR.Code)
	}

	streamReq := httptest.NewRequest(http.MethodGet, wantStream, nil)
	streamReq.AddCookie(shareCookie)
	streamRR := httptest.NewRecorder()
	router.ServeHTTP(streamRR, streamReq)
	if streamRR.Code != http.StatusOK || streamRR.Body.String() != fileBody {
		t.Fatalf("shared stream status=%d body=%q", streamRR.Code, streamRR.Body.String())
	}

	rangeReq := httptest.NewRequest(http.MethodGet, wantStream, nil)
	rangeReq.AddCookie(shareCookie)
	rangeReq.Header.Set("Range", "bytes=0-4")
	rangeRR := httptest.NewRecorder()
	router.ServeHTTP(rangeRR, rangeReq)
	if rangeRR.Code != http.StatusPartialContent || rangeRR.Body.String() != "video" {
		t.Fatalf("shared range status=%d body=%q", rangeRR.Code, rangeRR.Body.String())
	}

	subtitleListURL := "/api/share/" + consumed.ShareID + "/subtitles"
	subtitleListReq := httptest.NewRequest(http.MethodGet, subtitleListURL, nil)
	subtitleListReq.AddCookie(shareCookie)
	subtitleListRR := httptest.NewRecorder()
	router.ServeHTTP(subtitleListRR, subtitleListReq)
	if subtitleListRR.Code != http.StatusOK {
		t.Fatalf("shared subtitle list status=%d body=%s", subtitleListRR.Code, subtitleListRR.Body.String())
	}
	var subtitleList []SubtitleDTO
	if err := json.NewDecoder(subtitleListRR.Body).Decode(&subtitleList); err != nil {
		t.Fatalf("decode shared subtitles: %v", err)
	}
	wantSubtitleURL := "/p/share/" + consumed.ShareID + "/subtitle/0"
	if len(subtitleList) != 1 || subtitleList[0].URL != wantSubtitleURL {
		t.Fatalf("shared subtitles = %#v, want proxy URL %q", subtitleList, wantSubtitleURL)
	}
	subtitleReq := httptest.NewRequest(http.MethodGet, wantSubtitleURL, nil)
	subtitleReq.AddCookie(shareCookie)
	subtitleRR := httptest.NewRecorder()
	router.ServeHTTP(subtitleRR, subtitleReq)
	if subtitleRR.Code != http.StatusOK || subtitleRR.Body.String() != "shared subtitle" {
		t.Fatalf("shared subtitle proxy status=%d body=%q", subtitleRR.Code, subtitleRR.Body.String())
	}

	now = now.Add(videoShareSessionTTL)
	expiredStreamReq := httptest.NewRequest(http.MethodGet, wantStream, nil)
	expiredStreamReq.AddCookie(shareCookie)
	expiredStreamRR := httptest.NewRecorder()
	router.ServeHTTP(expiredStreamRR, expiredStreamReq)
	if expiredStreamRR.Code != http.StatusNotFound {
		t.Fatalf("expired stream status = %d, want 404", expiredStreamRR.Code)
	}
}

func newConsumeShareRequest(token string) *http.Request {
	body, _ := json.Marshal(consumeVideoShareRequest{Token: token})
	return httptest.NewRequest(http.MethodPost, "/api/share/consume", bytes.NewReader(body))
}

func responseCookieByName(t *testing.T, rr *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range rr.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("response cookie %q not found", name)
	return nil
}
