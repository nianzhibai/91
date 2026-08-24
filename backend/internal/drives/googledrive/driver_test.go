package googledrive

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/video-site/backend/internal/drives"
)

func TestInitUsesCustomOAuthClient(t *testing.T) {
	var savedAccess, savedRefresh string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" || r.Method != http.MethodPost {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if got := r.Form.Get("refresh_token"); got != "old-refresh" {
			t.Fatalf("refresh_token = %q", got)
		}
		if got := r.Form.Get("client_id"); got != "client-id" {
			t.Fatalf("client_id = %q", got)
		}
		if got := r.Form.Get("client_secret"); got != "client-secret" {
			t.Fatalf("client_secret = %q", got)
		}
		writeTestJSON(w, tokenResp{AccessToken: "new-access"})
	}))
	defer srv.Close()

	d := New(Config{
		ID:           "g",
		RefreshToken: "old-refresh",
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		OAuthURL:     srv.URL + "/token",
		OnTokenUpdate: func(access, refresh string) {
			savedAccess = access
			savedRefresh = refresh
		},
	})
	if err := d.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if d.accessToken != "new-access" || d.refreshToken != "old-refresh" {
		t.Fatalf("tokens not applied: access=%q refresh=%q", d.accessToken, d.refreshToken)
	}
	if savedAccess != "new-access" || savedRefresh != "old-refresh" {
		t.Fatalf("tokens not persisted: access=%q refresh=%q", savedAccess, savedRefresh)
	}
}

func TestConcurrentUnauthorizedRequestsShareOneTokenRefresh(t *testing.T) {
	var oldRequests atomic.Int32
	var refreshes atomic.Int32
	var releaseOld sync.Once
	bothOldRequestsArrived := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			refreshes.Add(1)
			writeTestJSON(w, tokenResp{AccessToken: "new-access"})
		case strings.HasPrefix(r.URL.Path, "/drive/v3/files/"):
			if r.Header.Get("Authorization") == "Bearer old-access" {
				if oldRequests.Add(1) == 2 {
					releaseOld.Do(func() { close(bothOldRequestsArrived) })
				}
				<-bothOldRequestsArrived
				writeTestJSONStatus(w, http.StatusUnauthorized, apiErrorResp{Error: apiErrorBody{Code: http.StatusUnauthorized, Message: "expired"}})
				return
			}
			writeTestJSON(w, driveFile{ID: strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	var persisted atomic.Int32
	d := New(Config{
		ID:           "google-main",
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		OAuthURL:     srv.URL + "/token",
		APIBaseURL:   srv.URL + "/drive/v3",
		OnTokenUpdate: func(_, _ string) {
			persisted.Add(1)
		},
	})

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, fileID := range []string{"file-a", "file-b"} {
		wg.Add(1)
		go func(fileID string) {
			defer wg.Done()
			_, err := d.Stat(context.Background(), fileID)
			errs <- err
		}(fileID)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent stat: %v", err)
		}
	}
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("token refreshes = %d, want 1", got)
	}
	if got := persisted.Load(); got != 1 {
		t.Fatalf("token persistence callbacks = %d, want 1", got)
	}
	if got := d.tokenSnapshot(); got.access != "new-access" || got.refresh != "old-refresh" {
		t.Fatalf("tokens = %#v, want refreshed access with retained refresh token", got)
	}
}

func TestListMapsGoogleDriveFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer access" {
			t.Fatalf("Authorization = %q", got)
		}
		if r.URL.Path != "/drive/v3/files" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if !strings.Contains(r.URL.Query().Get("q"), "'root' in parents") {
			t.Fatalf("unexpected q = %q", r.URL.Query().Get("q"))
		}
		writeTestJSON(w, filesResp{Files: []driveFile{
			{ID: "folder-1", Name: "Movies", MimeType: "application/vnd.google-apps.folder"},
			{
				ID:            "file-1",
				Name:          "clip.mp4",
				MimeType:      "video/mp4",
				Size:          "1234",
				MD5Checksum:   "abc",
				ThumbnailLink: "https://thumb.example/1",
			},
		}})
	}))
	defer srv.Close()

	d := New(Config{ID: "g", RootID: "root", APIBaseURL: srv.URL + "/drive/v3"})
	d.accessToken = "access"
	d.listInterval = -1

	entries, err := d.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d", len(entries))
	}
	if !entries[0].IsDir || entries[0].ID != "folder-1" {
		t.Fatalf("folder entry = %+v", entries[0])
	}
	if entries[1].ID != "file-1" || entries[1].Size != 1234 || entries[1].Hash != "abc" || entries[1].ThumbnailURL == "" {
		t.Fatalf("file entry = %+v", entries[1])
	}
}

func TestStreamURLReturnsAuthenticatedMediaLinkWithoutRedirectRequirement(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer access" {
			t.Fatalf("Authorization = %q", got)
		}
		if r.URL.Path != "/drive/v3/files/file-1" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		writeTestJSON(w, driveFile{
			ID:       "file-1",
			Name:     "clip.mp4",
			MimeType: "video/mp4",
			Size:     "1234",
		})
	}))
	defer srv.Close()

	d := New(Config{ID: "g", APIBaseURL: srv.URL + "/drive/v3"})
	d.accessToken = "access"

	link, err := d.StreamURL(context.Background(), "file-1")
	if err != nil {
		t.Fatalf("StreamURL() error = %v", err)
	}
	if !strings.HasPrefix(link.URL, srv.URL+"/drive/v3/files/file-1?") {
		t.Fatalf("link URL = %q", link.URL)
	}
	if !strings.Contains(link.URL, "alt=media") {
		t.Fatalf("link URL missing alt=media: %q", link.URL)
	}
	if got := link.Headers.Get("Authorization"); got != "Bearer access" {
		t.Fatalf("link Authorization = %q", got)
	}
}

func TestUploadAndReportHashUsesResumableSession(t *testing.T) {
	body := "hello google drive"
	wantHash := md5.Sum([]byte(body))
	var sawSession bool
	var sawUpload bool
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload/drive/v3/files":
			sawSession = true
			if got := r.Header.Get("Authorization"); got != "Bearer access" {
				t.Fatalf("session Authorization = %q", got)
			}
			if got := r.URL.Query().Get("uploadType"); got != "resumable" {
				t.Fatalf("uploadType = %q", got)
			}
			if got := r.Header.Get("X-Upload-Content-Length"); got != "18" {
				t.Fatalf("X-Upload-Content-Length = %q", got)
			}
			var meta struct {
				Name    string   `json:"name"`
				Parents []string `json:"parents"`
			}
			if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
				t.Fatalf("decode session metadata: %v", err)
			}
			if meta.Name != "clip.mp4" || len(meta.Parents) != 1 || meta.Parents[0] != "parent-1" {
				t.Fatalf("metadata = %+v", meta)
			}
			w.Header().Set("Location", srv.URL+"/upload/session/1")
			w.WriteHeader(http.StatusOK)
		case "/upload/session/1":
			sawUpload = true
			if got := r.Header.Get("Authorization"); got != "Bearer access" {
				t.Fatalf("upload Authorization = %q", got)
			}
			if got := r.Header.Get("Content-Range"); got != "bytes 0-17/18" {
				t.Fatalf("Content-Range = %q", got)
			}
			gotBody, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read upload body: %v", err)
			}
			if string(gotBody) != body {
				t.Fatalf("upload body = %q", string(gotBody))
			}
			writeTestJSONStatus(w, http.StatusCreated, driveFile{
				ID:          "file-uploaded",
				Name:        "clip.mp4",
				Size:        "18",
				MD5Checksum: hex.EncodeToString(wantHash[:]),
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	d := New(Config{ID: "g", APIBaseURL: srv.URL + "/drive/v3"})
	d.accessToken = "access"
	res, err := d.UploadAndReportHash(context.Background(), "parent-1", "clip.mp4", strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("UploadAndReportHash() error = %v", err)
	}
	if !sawSession || !sawUpload {
		t.Fatalf("saw session/upload = %v/%v, want both", sawSession, sawUpload)
	}
	if res.FileID != "file-uploaded" || res.Size != int64(len(body)) || res.Hash != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("upload result = %+v", res)
	}
}

func TestEnsureDirAndRenameUseGoogleDriveFileAPI(t *testing.T) {
	var madeDir bool
	var renamed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/drive/v3/files":
			writeTestJSON(w, filesResp{})
		case r.Method == http.MethodPost && r.URL.Path == "/drive/v3/files":
			madeDir = true
			var meta struct {
				Name     string   `json:"name"`
				Parents  []string `json:"parents"`
				MimeType string   `json:"mimeType"`
			}
			if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
				t.Fatalf("decode mkdir body: %v", err)
			}
			if meta.Name != "Crawler Uploads" || len(meta.Parents) != 1 || meta.Parents[0] != "root" || meta.MimeType != "application/vnd.google-apps.folder" {
				t.Fatalf("mkdir body = %+v", meta)
			}
			writeTestJSON(w, driveFile{ID: "folder-crawler", Name: "Crawler Uploads", MimeType: "application/vnd.google-apps.folder"})
		case r.Method == http.MethodPatch && r.URL.Path == "/drive/v3/files/file-1":
			renamed = true
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode rename body: %v", err)
			}
			if body["name"] != "new-name.mp4" {
				t.Fatalf("rename body = %+v", body)
			}
			writeTestJSON(w, driveFile{ID: "file-1", Name: "new-name.mp4"})
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	d := New(Config{ID: "g", RootID: "root", APIBaseURL: srv.URL + "/drive/v3"})
	d.accessToken = "access"
	d.listInterval = -1

	dirID, err := d.EnsureDir(context.Background(), "Crawler Uploads")
	if err != nil {
		t.Fatalf("EnsureDir() error = %v", err)
	}
	if dirID != "folder-crawler" || !madeDir {
		t.Fatalf("dirID/madeDir = %q/%v, want folder-crawler/true", dirID, madeDir)
	}
	if err := d.Rename(context.Background(), "file-1", "new-name.mp4"); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	if !renamed {
		t.Fatal("rename endpoint was not called")
	}
}

func TestRequestRefreshesOnUnauthorized(t *testing.T) {
	var fileCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			writeTestJSON(w, tokenResp{AccessToken: "new-access"})
		case "/drive/v3/files/file-1":
			fileCalls++
			if fileCalls == 1 {
				writeTestJSONStatus(w, http.StatusUnauthorized, apiErrorResp{Error: apiErrorBody{
					Code:    http.StatusUnauthorized,
					Message: "Invalid Credentials",
				}})
				return
			}
			if got := r.Header.Get("Authorization"); got != "Bearer new-access" {
				t.Fatalf("Authorization after refresh = %q", got)
			}
			writeTestJSON(w, driveFile{ID: "file-1", Name: "clip.mp4", Size: "1"})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	d := New(Config{
		ID:           "g",
		RefreshToken: "old-refresh",
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		OAuthURL:     srv.URL + "/token",
		APIBaseURL:   srv.URL + "/drive/v3",
	})
	d.accessToken = "old-access"

	if _, err := d.Stat(context.Background(), "file-1"); err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if fileCalls != 2 {
		t.Fatalf("fileCalls = %d", fileCalls)
	}
	if d.accessToken != "new-access" || d.refreshToken != "old-refresh" {
		t.Fatalf("tokens not refreshed: access=%q refresh=%q", d.accessToken, d.refreshToken)
	}
}

func TestRateLimitReasonsFollowGoogleDriveErrorShape(t *testing.T) {
	reasons := []string{
		"rateLimitExceeded",
		"userRateLimitExceeded",
		"dailyLimitExceeded",
		"dailyLimitExceededUnreg",
		"downloadQuotaExceeded",
		"sharingRateLimitExceeded",
		"quotaExceeded",
	}
	for _, reason := range reasons {
		body := apiErrorBody{
			Code:    http.StatusForbidden,
			Message: "google drive quota or rate limited",
			Errors: []struct {
				Domain       string `json:"domain"`
				Reason       string `json:"reason"`
				Message      string `json:"message"`
				LocationType string `json:"location_type"`
				Location     string `json:"location"`
			}{
				{Domain: "usageLimits", Reason: reason, Message: reason},
			},
		}
		if !isGoogleRateLimit(nil, body) {
			t.Fatalf("reason %q not treated as rate limit", reason)
		}
	}
}

func TestStreamURLRateLimitStartsSharedLinkCooldown(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "120")
		writeTestJSONStatus(w, http.StatusForbidden, apiErrorResp{Error: apiErrorBody{
			Code:    http.StatusForbidden,
			Message: "User rate limit exceeded.",
			Errors: []struct {
				Domain       string `json:"domain"`
				Reason       string `json:"reason"`
				Message      string `json:"message"`
				LocationType string `json:"location_type"`
				Location     string `json:"location"`
			}{
				{Domain: "usageLimits", Reason: "userRateLimitExceeded", Message: "User rate limit exceeded."},
			},
		}})
	}))
	defer srv.Close()

	d := New(Config{ID: "g", APIBaseURL: srv.URL})
	d.accessToken = "access"
	d.linkCooldownDuration = time.Hour

	_, err := d.StreamURL(context.Background(), "file-1")
	if err == nil {
		t.Fatal("first StreamURL succeeded, want rate limit")
	}
	var rateLimit *drives.RateLimitError
	if !errors.As(err, &rateLimit) {
		t.Fatalf("first error = %T %[1]v, want RateLimitError", err)
	}
	if rateLimit.RetryAfter != 2*time.Minute {
		t.Fatalf("retry after = %s, want 2m", rateLimit.RetryAfter)
	}

	_, err = d.StreamURL(context.Background(), "file-1")
	if err == nil {
		t.Fatal("second StreamURL succeeded during cooldown")
	}
	if !errors.As(err, &rateLimit) {
		t.Fatalf("second error = %T %[1]v, want RateLimitError", err)
	}
	if calls != 1 {
		t.Fatalf("remote calls = %d, want 1; second call should use shared cooldown", calls)
	}
	if rateLimit.RetryAfter <= 0 || rateLimit.RetryAfter > 2*time.Minute {
		t.Fatalf("second retry after = %s, want remaining cooldown", rateLimit.RetryAfter)
	}
}

func writeTestJSON(w http.ResponseWriter, v any) {
	writeTestJSONStatus(w, http.StatusOK, v)
}

func writeTestJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
