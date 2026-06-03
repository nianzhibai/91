package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/video-site/backend/internal/auth"
	"github.com/video-site/backend/internal/catalog"
)

func TestHandleLoginReturnsForbiddenForBannedIP(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	if err := cat.BanLoginIP(ctx, "203.0.113.20", "test"); err != nil {
		t.Fatalf("ban ip: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/login", strings.NewReader(`{"username":"admin","password":"secret"}`))
	req.RemoteAddr = "203.0.113.20:12345"
	rr := httptest.NewRecorder()

	(&AdminServer{
		Catalog: cat,
		Auth:    &auth.Authenticator{Username: "admin", Password: "secret", Catalog: cat},
	}).handleLogin(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rr.Code, rr.Body.String())
	}
}

func TestHandleLoginRequiresSetupBeforeDefaultLogin(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/admin/api/login", strings.NewReader(`{"username":"admin","password":"admin123"}`))
	rr := httptest.NewRecorder()
	(&AdminServer{
		Catalog:       cat,
		Auth:          &auth.Authenticator{Username: "admin", Password: "admin123", Catalog: cat},
		SetupRequired: func() bool { return true },
	}).handleLogin(rr, req)

	if rr.Code != http.StatusPreconditionRequired {
		t.Fatalf("status = %d, want 428; body = %s", rr.Code, rr.Body.String())
	}
}

func TestHandleSetupStoresCredentialsAndCreatesSession(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	authr := &auth.Authenticator{Username: "admin", Password: "admin123", Catalog: cat}
	setupRequired := true
	var savedUser, savedPass string
	req := httptest.NewRequest(http.MethodPost, "/admin/api/setup", strings.NewReader(`{"username":"owner","password":"secret123"}`))
	rr := httptest.NewRecorder()

	(&AdminServer{
		Catalog:       cat,
		Auth:          authr,
		SetupRequired: func() bool { return setupRequired },
		OnSetup: func(username, password string) error {
			savedUser, savedPass = username, password
			authr.SetCredentials(username, password)
			setupRequired = false
			return nil
		},
	}).handleSetup(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	if savedUser != "owner" || savedPass != "secret123" {
		t.Fatalf("saved credentials = %q/%q, want owner/secret123", savedUser, savedPass)
	}
	cookies := rr.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("setup did not set a session cookie")
	}
	ok, err := cat.ValidateSession(context.Background(), cookies[0].Value)
	if err != nil || !ok {
		t.Fatalf("setup session valid=%v err=%v", ok, err)
	}
}

func TestHandleCheckUpdateReportsNewRelease(t *testing.T) {
	dir := t.TempDir()
	versionFile := filepath.Join(dir, ".version")
	if err := os.WriteFile(versionFile, []byte("v0.1.0\n2026-05-29 12:00:00\n"), 0o644); err != nil {
		t.Fatalf("write version file: %v", err)
	}
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			http.Error(w, "missing user agent", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"tag_name": "v0.2.0",
			"html_url": "https://github.com/nianzhibai/91/releases/tag/v0.2.0",
		})
	}))
	t.Cleanup(releaseServer.Close)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/update/check", nil)
	rr := httptest.NewRecorder()
	(&AdminServer{
		VersionFilePath: versionFile,
		ReleaseAPIURL:   releaseServer.URL,
	}).handleCheckUpdate(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got updateCheckDTO
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CurrentVersion != "v0.1.0" {
		t.Fatalf("currentVersion = %q, want v0.1.0", got.CurrentVersion)
	}
	if got.LatestVersion != "v0.2.0" {
		t.Fatalf("latestVersion = %q, want v0.2.0", got.LatestVersion)
	}
	if !got.HasUpdate {
		t.Fatalf("hasUpdate = false, want true")
	}
	if got.ReleaseURL == "" {
		t.Fatalf("releaseUrl is empty")
	}
}

func TestHandleCheckUpdateReportsUpToDate(t *testing.T) {
	dir := t.TempDir()
	versionFile := filepath.Join(dir, ".version")
	if err := os.WriteFile(versionFile, []byte("v0.2.0\n"), 0o644); err != nil {
		t.Fatalf("write version file: %v", err)
	}
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"tag_name": "v0.2.0",
			"html_url": "https://github.com/nianzhibai/91/releases/tag/v0.2.0",
		})
	}))
	t.Cleanup(releaseServer.Close)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/update/check", nil)
	rr := httptest.NewRecorder()
	(&AdminServer{
		VersionFilePath: versionFile,
		ReleaseAPIURL:   releaseServer.URL,
	}).handleCheckUpdate(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got updateCheckDTO
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.HasUpdate {
		t.Fatalf("hasUpdate = true, want false")
	}
}

func TestHandleCheckUpdateUsesDockerImageVersion(t *testing.T) {
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"tag_name": "v0.2.0",
			"html_url": "https://github.com/nianzhibai/91/releases/tag/v0.2.0",
		})
	}))
	t.Cleanup(releaseServer.Close)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/update/check", nil)
	rr := httptest.NewRecorder()
	(&AdminServer{
		ImageVersion:  "v0.1.0",
		ReleaseAPIURL: releaseServer.URL,
	}).handleCheckUpdate(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got updateCheckDTO
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CurrentVersion != "v0.1.0" {
		t.Fatalf("currentVersion = %q, want v0.1.0", got.CurrentVersion)
	}
	if !got.HasUpdate {
		t.Fatalf("hasUpdate = false, want true")
	}
}

func TestInstalledVersionPrefersDockerImageVersionOverVersionFile(t *testing.T) {
	dir := t.TempDir()
	versionFile := filepath.Join(dir, ".version")
	if err := os.WriteFile(versionFile, []byte("v0.1.0\n"), 0o644); err != nil {
		t.Fatalf("write version file: %v", err)
	}

	got := (&AdminServer{
		VersionFilePath: versionFile,
		ImageVersion:    "v0.2.0",
	}).installedVersion()

	if got != "v0.2.0" {
		t.Fatalf("installedVersion = %q, want v0.2.0", got)
	}
}

func TestHandleRunNightlyJobReturnsAcceptedStatus(t *testing.T) {
	called := false
	req := httptest.NewRequest(http.MethodPost, "/admin/api/jobs/nightly/run", nil)
	rr := httptest.NewRecorder()

	(&AdminServer{
		OnRunNightlyJob: func() bool {
			called = true
			return true
		},
		GetNightlyJobStatus: func() NightlyJobStatus {
			return NightlyJobStatus{State: "queued", Queued: true}
		},
	}).handleRunNightlyJob(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rr.Code, rr.Body.String())
	}
	if !called {
		t.Fatal("OnRunNightlyJob was not called")
	}
	var got struct {
		OK       bool             `json:"ok"`
		Accepted bool             `json:"accepted"`
		Status   NightlyJobStatus `json:"status"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK || !got.Accepted || got.Status.State != "queued" || !got.Status.Queued {
		t.Fatalf("response = %#v, want accepted queued status", got)
	}
}

func TestHandleNightlyJobStatusDefaultsToIdle(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/admin/api/jobs/nightly/status", nil)
	rr := httptest.NewRecorder()

	(&AdminServer{}).handleNightlyJobStatus(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	var got NightlyJobStatus
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != "idle" || got.Running || got.Queued {
		t.Fatalf("status = %#v, want idle", got)
	}
}

func TestHandleUpsertDrivePreservesExistingCredentialsWhenRequestCredentialsEmpty(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:         "quark-main",
		Kind:       "quark",
		Name:       "Old name",
		RootID:     "0",
		ScanRootID: "0",
		Credentials: map[string]string{
			"cookie": "existing-cookie",
		},
		Status: "ok",
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/drives", strings.NewReader(`{
		"id": "quark-main",
		"kind": "quark",
		"name": "New name",
		"rootId": "0",
		"scanRootId": "scan-root",
		"credentials": {}
	}`))
	rr := httptest.NewRecorder()

	(&AdminServer{Catalog: cat}).handleUpsertDrive(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got, err := cat.GetDrive(ctx, "quark-main")
	if err != nil {
		t.Fatalf("get drive: %v", err)
	}
	if got.Name != "New name" {
		t.Fatalf("name = %q, want New name", got.Name)
	}
	if got.ScanRootID != "0" {
		t.Fatalf("scanRootId = %q, want rootId 0", got.ScanRootID)
	}
	if got.Credentials["cookie"] != "existing-cookie" {
		t.Fatalf("cookie credential = %q, want existing-cookie", got.Credentials["cookie"])
	}
}

func TestHandleUpsertDriveDefaultsEmptyRootID(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/admin/api/drives", strings.NewReader(`{
		"id": "onedrive-main",
		"kind": "onedrive",
		"name": "OneDrive",
		"rootId": "",
		"credentials": {"refresh_token": "token"}
	}`))
	rr := httptest.NewRecorder()

	(&AdminServer{Catalog: cat}).handleUpsertDrive(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got, err := cat.GetDrive(ctx, "onedrive-main")
	if err != nil {
		t.Fatalf("get drive: %v", err)
	}
	if got.RootID != "root" {
		t.Fatalf("rootId = %q, want root", got.RootID)
	}
	if got.ScanRootID != got.RootID {
		t.Fatalf("scanRootId = %q, want rootId %q", got.ScanRootID, got.RootID)
	}
}

func TestHandleUpsertDriveReplacesExistingCredentialsWhenProvided(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:         "quark-main",
		Kind:       "quark",
		Name:       "Old name",
		RootID:     "0",
		ScanRootID: "0",
		Credentials: map[string]string{
			"cookie": "existing-cookie",
		},
		Status: "ok",
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/drives", bytes.NewBufferString(`{
		"id": "quark-main",
		"kind": "quark",
		"name": "New name",
		"rootId": "0",
		"scanRootId": "0",
		"credentials": {"cookie": "new-cookie"}
	}`))
	rr := httptest.NewRecorder()

	(&AdminServer{Catalog: cat}).handleUpsertDrive(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got, err := cat.GetDrive(ctx, "quark-main")
	if err != nil {
		t.Fatalf("get drive: %v", err)
	}
	if got.Credentials["cookie"] != "new-cookie" {
		t.Fatalf("cookie credential = %q, want new-cookie", got.Credentials["cookie"])
	}
}

func TestHandleUpsertSpider91ProxyPreservesRuntimeCredentials(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:     "spider91-main",
		Kind:   "spider91",
		Name:   "91 Spider",
		RootID: "/",
		Credentials: map[string]string{
			"last_crawl_at": "1800000000",
			"proxy":         "http://old-proxy.local:7890",
			"script_path":   "/opt/video-site-91/91VideoSpider/spider_91porn.py",
		},
		Status: "ok",
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/drives", strings.NewReader(`{
		"id": "spider91-main",
		"kind": "spider91",
		"name": "91 Spider",
		"rootId": "/",
		"credentials": {"proxy": " socks5h://proxy-user:proxy-pass@127.0.0.1:7891 "}
	}`))
	rr := httptest.NewRecorder()
	(&AdminServer{Catalog: cat}).handleUpsertDrive(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	got, err := cat.GetDrive(ctx, "spider91-main")
	if err != nil {
		t.Fatalf("get drive: %v", err)
	}
	if got.Credentials["proxy"] != "socks5h://proxy-user:proxy-pass@127.0.0.1:7891" {
		t.Fatalf("proxy = %q, want trimmed new proxy", got.Credentials["proxy"])
	}
	if got.Credentials["last_crawl_at"] != "1800000000" {
		t.Fatalf("last_crawl_at = %q, want preserved", got.Credentials["last_crawl_at"])
	}
	if got.Credentials["script_path"] == "" {
		t.Fatalf("script_path should be preserved")
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/api/drives", strings.NewReader(`{
		"id": "spider91-main",
		"kind": "spider91",
		"name": "91 Spider",
		"rootId": "/",
		"credentials": {"proxy": " "}
	}`))
	rr = httptest.NewRecorder()
	(&AdminServer{Catalog: cat}).handleUpsertDrive(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("clear status = %d, body = %s", rr.Code, rr.Body.String())
	}

	got, err = cat.GetDrive(ctx, "spider91-main")
	if err != nil {
		t.Fatalf("get cleared drive: %v", err)
	}
	if _, ok := got.Credentials["proxy"]; ok {
		t.Fatalf("proxy should be removed after empty save, got %q", got.Credentials["proxy"])
	}
	if got.Credentials["last_crawl_at"] != "1800000000" {
		t.Fatalf("last_crawl_at after clear = %q, want preserved", got.Credentials["last_crawl_at"])
	}
}

func TestHandleUpsertSpider91RejectsUnsupportedProxyScheme(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/admin/api/drives", strings.NewReader(`{
		"id": "spider91-main",
		"kind": "spider91",
		"name": "91 Spider",
		"rootId": "/",
		"credentials": {"proxy": "ftp://127.0.0.1:21"}
	}`))
	rr := httptest.NewRecorder()
	(&AdminServer{Catalog: cat}).handleUpsertDrive(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "socks5:// 或 socks5h://") {
		t.Fatalf("body = %q, want supported schemes message", rr.Body.String())
	}
}

func TestHandleDeleteDriveRunsRequestedCleanupBeforeDeletingDrive(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:            "drive-one",
		Kind:          "pikpak",
		Name:          "Drive One",
		RootID:        "root",
		TeaserEnabled: true,
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	cleanupCalled := ""
	removedCalled := ""
	req := httptest.NewRequest(http.MethodDelete, "/admin/api/drives/drive-one", strings.NewReader(`{"deleteVideos":true}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "drive-one")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()

	(&AdminServer{
		Catalog: cat,
		OnDriveDeleteCleanup: func(cleanupCtx context.Context, driveID string) (int, error) {
			cleanupCalled = driveID
			if _, err := cat.GetDrive(cleanupCtx, driveID); err != nil {
				t.Fatalf("drive should still exist during cleanup: %v", err)
			}
			return 3, nil
		},
		OnDriveRemoved: func(driveID string) {
			removedCalled = driveID
		},
	}).handleDeleteDrive(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if cleanupCalled != "drive-one" {
		t.Fatalf("cleanup called with %q, want drive-one", cleanupCalled)
	}
	if removedCalled != "drive-one" {
		t.Fatalf("removed hook called with %q, want drive-one", removedCalled)
	}
	if _, err := cat.GetDrive(ctx, "drive-one"); err != sql.ErrNoRows {
		t.Fatalf("drive lookup error = %v, want sql.ErrNoRows", err)
	}
	var got struct {
		OK            bool `json:"ok"`
		DeletedVideos int  `json:"deletedVideos"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK || got.DeletedVideos != 3 {
		t.Fatalf("response = %#v, want ok with deletedVideos=3", got)
	}
}

func TestHandleDeleteDriveRequiresCleanupConfirmation(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:            "drive-one",
		Kind:          "pikpak",
		Name:          "Drive One",
		RootID:        "root",
		TeaserEnabled: true,
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/admin/api/drives/drive-one", strings.NewReader(`{"deleteVideos":false}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "drive-one")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()

	(&AdminServer{
		Catalog: cat,
		OnDriveDeleteCleanup: func(context.Context, string) (int, error) {
			t.Fatal("cleanup hook should not be called without confirmation")
			return 0, nil
		},
	}).handleDeleteDrive(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	if _, err := cat.GetDrive(ctx, "drive-one"); err != nil {
		t.Fatalf("drive should remain after rejected delete: %v", err)
	}
}

func TestHandleListDrivesIncludesSpider91Proxy(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	for _, d := range []*catalog.Drive{
		{
			ID:     "spider91-main",
			Kind:   "spider91",
			Name:   "91 Spider",
			RootID: "/",
			Credentials: map[string]string{
				"last_crawl_at": "1800000000",
				"proxy":         " http://127.0.0.1:7890 ",
			},
			Status: "ok",
		},
		{
			ID:     "onedrive-main",
			Kind:   "onedrive",
			Name:   "OneDrive",
			RootID: "root",
			Credentials: map[string]string{
				"proxy": "http://should-not-leak.local:7890",
			},
			Status: "ok",
		},
	} {
		if err := cat.UpsertDrive(ctx, d); err != nil {
			t.Fatalf("seed drive %s: %v", d.ID, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/api/drives", nil)
	rr := httptest.NewRecorder()
	(&AdminServer{Catalog: cat}).handleListDrives(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var got []struct {
		ID            string `json:"id"`
		Spider91Proxy string `json:"spider91Proxy"`
		LastCrawlAt   int64  `json:"lastCrawlAt"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]struct {
		Spider91Proxy string
		LastCrawlAt   int64
	}{}
	for _, d := range got {
		byID[d.ID] = struct {
			Spider91Proxy string
			LastCrawlAt   int64
		}{Spider91Proxy: d.Spider91Proxy, LastCrawlAt: d.LastCrawlAt}
	}
	if byID["spider91-main"].Spider91Proxy != "http://127.0.0.1:7890" {
		t.Fatalf("spider91 proxy = %q, want trimmed proxy", byID["spider91-main"].Spider91Proxy)
	}
	if byID["spider91-main"].LastCrawlAt != 1800000000 {
		t.Fatalf("lastCrawlAt = %d, want 1800000000", byID["spider91-main"].LastCrawlAt)
	}
	if byID["onedrive-main"].Spider91Proxy != "" {
		t.Fatalf("onedrive spider91Proxy = %q, want empty", byID["onedrive-main"].Spider91Proxy)
	}
}

func TestHandleListDrivesIncludesTeaserCounts(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	for _, d := range []*catalog.Drive{
		{ID: "OneDrive", Kind: "onedrive", Name: "OneDrive", RootID: "root", Status: "ok"},
		{ID: "PikPak", Kind: "pikpak", Name: "PikPak", RootID: "", Status: "ok"},
	} {
		if err := cat.UpsertDrive(ctx, d); err != nil {
			t.Fatalf("seed drive %s: %v", d.ID, err)
		}
	}

	now := time.Now()
	videos := []*catalog.Video{
		{ID: "od-ready-1", DriveID: "OneDrive", FileID: "od-file-1", Title: "OD Ready 1", Size: 100, ThumbnailURL: "/p/thumb/od-ready-1", PreviewStatus: "ready", PublishedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: "od-ready-2", DriveID: "OneDrive", FileID: "od-file-2", Title: "OD Ready 2", Size: 100, PreviewStatus: "ready", PublishedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: "od-pending", DriveID: "OneDrive", FileID: "od-file-3", Title: "OD Pending", Size: 100, PreviewStatus: "pending", PublishedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: "pp-pending", DriveID: "PikPak", FileID: "pp-file-1", Title: "PP Pending", Size: 100, PreviewStatus: "pending", PublishedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: "pp-failed", DriveID: "PikPak", FileID: "pp-file-2", Title: "PP Failed", Size: 100, ThumbnailURL: "/p/thumb/pp-failed", PreviewStatus: "failed", PublishedAt: now, CreatedAt: now, UpdatedAt: now},
	}
	for _, v := range videos {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}
	if err := cat.UpdateVideoMeta(ctx, "od-ready-2", catalog.VideoMetaPatch{ThumbnailStatus: "failed"}); err != nil {
		t.Fatalf("mark thumbnail failed: %v", err)
	}
	if err := cat.UpdateVideoFingerprint(ctx, "od-ready-1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "ready", ""); err != nil {
		t.Fatalf("mark fingerprint ready: %v", err)
	}
	if err := cat.UpdateVideoFingerprint(ctx, "od-ready-2", "", "failed", "sample failed"); err != nil {
		t.Fatalf("mark fingerprint failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/api/drives", nil)
	rr := httptest.NewRecorder()
	(&AdminServer{
		Catalog: cat,
		GetDriveGenerationStatuses: func() map[string]DriveGenerationStatuses {
			return map[string]DriveGenerationStatuses{
				"OneDrive": {
					Thumbnail:   GenerationStatus{State: "cooling", QueueLength: 3, CooldownUntil: "2026-05-16T21:00:00+08:00"},
					Preview:     GenerationStatus{State: "generating", CurrentTitle: "OD Pending"},
					Fingerprint: GenerationStatus{State: "generating", CurrentTitle: "OD Pending"},
				},
			}
		},
	}).handleListDrives(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got []struct {
		ID                            string           `json:"id"`
		ThumbnailGenerationStatus     GenerationStatus `json:"thumbnailGenerationStatus"`
		PreviewGenerationStatus       GenerationStatus `json:"previewGenerationStatus"`
		FingerprintGenerationStatus   GenerationStatus `json:"fingerprintGenerationStatus"`
		ThumbnailReadyCount           int              `json:"thumbnailReadyCount"`
		ThumbnailPendingCount         int              `json:"thumbnailPendingCount"`
		ThumbnailFailedCount          int              `json:"thumbnailFailedCount"`
		ThumbnailDurationPendingCount int              `json:"thumbnailDurationPendingCount"`
		TeaserReadyCount              int              `json:"teaserReadyCount"`
		TeaserPendingCount            int              `json:"teaserPendingCount"`
		TeaserFailedCount             int              `json:"teaserFailedCount"`
		FingerprintReadyCount         int              `json:"fingerprintReadyCount"`
		FingerprintPendingCount       int              `json:"fingerprintPendingCount"`
		FingerprintFailedCount        int              `json:"fingerprintFailedCount"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]struct {
		TeaserReady              int
		TeaserPending            int
		TeaserFailed             int
		ThumbnailReady           int
		ThumbnailPending         int
		ThumbnailFailed          int
		ThumbnailDurationPending int
		FingerprintReady         int
		FingerprintPending       int
		FingerprintFailed        int
		Thumbnail                GenerationStatus
		Preview                  GenerationStatus
		Fingerprint              GenerationStatus
	}{}
	for _, d := range got {
		byID[d.ID] = struct {
			TeaserReady              int
			TeaserPending            int
			TeaserFailed             int
			ThumbnailReady           int
			ThumbnailPending         int
			ThumbnailFailed          int
			ThumbnailDurationPending int
			FingerprintReady         int
			FingerprintPending       int
			FingerprintFailed        int
			Thumbnail                GenerationStatus
			Preview                  GenerationStatus
			Fingerprint              GenerationStatus
		}{
			TeaserReady:              d.TeaserReadyCount,
			TeaserPending:            d.TeaserPendingCount,
			TeaserFailed:             d.TeaserFailedCount,
			ThumbnailReady:           d.ThumbnailReadyCount,
			ThumbnailPending:         d.ThumbnailPendingCount,
			ThumbnailFailed:          d.ThumbnailFailedCount,
			ThumbnailDurationPending: d.ThumbnailDurationPendingCount,
			FingerprintReady:         d.FingerprintReadyCount,
			FingerprintPending:       d.FingerprintPendingCount,
			FingerprintFailed:        d.FingerprintFailedCount,
			Thumbnail:                d.ThumbnailGenerationStatus,
			Preview:                  d.PreviewGenerationStatus,
			Fingerprint:              d.FingerprintGenerationStatus,
		}
	}
	if byID["OneDrive"].TeaserReady != 2 || byID["OneDrive"].TeaserPending != 1 || byID["OneDrive"].TeaserFailed != 0 {
		t.Fatalf("OneDrive counts = %#v, want ready=2 pending=1 failed=0", byID["OneDrive"])
	}
	if byID["OneDrive"].ThumbnailReady != 1 || byID["OneDrive"].ThumbnailPending != 1 || byID["OneDrive"].ThumbnailFailed != 1 {
		t.Fatalf("OneDrive thumbnail counts = %#v, want ready=1 pending=1 failed=1", byID["OneDrive"])
	}
	if byID["OneDrive"].ThumbnailDurationPending != 1 {
		t.Fatalf("OneDrive thumbnail duration pending = %#v, want 1", byID["OneDrive"])
	}
	if byID["OneDrive"].Thumbnail.State != "cooling" || byID["OneDrive"].Preview.State != "generating" {
		t.Fatalf("OneDrive generation statuses = %#v, want thumbnail cooling and preview generating", byID["OneDrive"])
	}
	if byID["OneDrive"].FingerprintReady != 1 || byID["OneDrive"].FingerprintPending != 1 || byID["OneDrive"].FingerprintFailed != 1 {
		t.Fatalf("OneDrive fingerprint counts = %#v, want ready=1 pending=1 failed=1", byID["OneDrive"])
	}
	if byID["OneDrive"].Fingerprint.State != "generating" {
		t.Fatalf("OneDrive fingerprint status = %#v, want generating", byID["OneDrive"].Fingerprint)
	}
	if byID["PikPak"].TeaserReady != 0 || byID["PikPak"].TeaserPending != 1 || byID["PikPak"].TeaserFailed != 1 {
		t.Fatalf("PikPak counts = %#v, want ready=0 pending=1 failed=1", byID["PikPak"])
	}
	if byID["PikPak"].ThumbnailReady != 1 || byID["PikPak"].ThumbnailPending != 1 || byID["PikPak"].ThumbnailFailed != 0 {
		t.Fatalf("PikPak thumbnail counts = %#v, want ready=1 pending=1 failed=0", byID["PikPak"])
	}
	if byID["PikPak"].FingerprintPending != 2 {
		t.Fatalf("PikPak fingerprint counts = %#v, want pending=2", byID["PikPak"])
	}
	if byID["PikPak"].Thumbnail.State != "idle" || byID["PikPak"].Preview.State != "idle" || byID["PikPak"].Fingerprint.State != "idle" {
		t.Fatalf("PikPak generation statuses = %#v, want idle defaults", byID["PikPak"])
	}
}

func TestHandleRegenFailedFingerprintsInvokesHook(t *testing.T) {
	called := ""
	req := httptest.NewRequest(http.MethodPost, "/admin/api/drives/drive-one/fingerprints/failed/regenerate", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "drive-one")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()

	(&AdminServer{
		OnRegenFailedFingerprints: func(driveID string) {
			called = driveID
		},
	}).handleRegenFailedFingerprints(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if called != "drive-one" {
		t.Fatalf("called drive = %q, want drive-one", called)
	}
}

func TestHandleDriveStorageReportsLocalMediaUsage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	cat, err := catalog.Open(filepath.Join(root, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	localDir := filepath.Join(root, "previews")
	thumbDir := filepath.Join(localDir, "thumbs")
	if err := os.MkdirAll(thumbDir, 0o755); err != nil {
		t.Fatalf("mkdir thumbs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "drive-one-video.mp4"), []byte("teaser-one"), 0o644); err != nil {
		t.Fatalf("write teaser one: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "drive-two-video.mp4"), []byte("teaser-two!!"), 0o644); err != nil {
		t.Fatalf("write teaser two: %v", err)
	}
	if err := os.WriteFile(filepath.Join(thumbDir, "drive-one-video.jpg"), []byte("jpg-one"), 0o644); err != nil {
		t.Fatalf("write thumb one: %v", err)
	}
	if err := os.WriteFile(filepath.Join(thumbDir, "drive-two-video.jpg"), []byte("jpg-two!!"), 0o644); err != nil {
		t.Fatalf("write thumb two: %v", err)
	}

	for _, d := range []*catalog.Drive{
		{ID: "drive-one", Kind: "onedrive", Name: "Drive One", RootID: "root", Status: "ok"},
		{ID: "drive-two", Kind: "pikpak", Name: "Drive Two", RootID: "", Status: "ok"},
	} {
		if err := cat.UpsertDrive(ctx, d); err != nil {
			t.Fatalf("seed drive %s: %v", d.ID, err)
		}
	}
	now := time.Now()
	for _, v := range []*catalog.Video{
		{
			ID:            "drive-one-video",
			DriveID:       "drive-one",
			FileID:        "file-one",
			Title:         "Video One",
			PreviewLocal:  filepath.Join(localDir, "drive-one-video.mp4"),
			PreviewStatus: "ready",
			PublishedAt:   now,
			CreatedAt:     now,
			UpdatedAt:     now,
		},
		{
			ID:            "drive-two-video",
			DriveID:       "drive-two",
			FileID:        "file-two",
			Title:         "Video Two",
			PreviewLocal:  filepath.Join(localDir, "drive-two-video.mp4"),
			PreviewStatus: "ready",
			PublishedAt:   now,
			CreatedAt:     now,
			UpdatedAt:     now,
		},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/api/drives/storage", nil)
	rr := httptest.NewRecorder()
	(&AdminServer{Catalog: cat, LocalPreviewDir: localDir}).handleDriveStorage(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		ThumbnailBytes int64 `json:"thumbnailBytes"`
		TeaserBytes    int64 `json:"teaserBytes"`
		TotalBytes     int64 `json:"totalBytes"`
		AvailableBytes int64 `json:"availableBytes"`
		Drives         map[string]struct {
			ThumbnailBytes int64 `json:"thumbnailBytes"`
			TeaserBytes    int64 `json:"teaserBytes"`
			TotalBytes     int64 `json:"totalBytes"`
		} `json:"drives"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ThumbnailBytes != int64(len("jpg-one")+len("jpg-two!!")) {
		t.Fatalf("thumbnail bytes = %d, want %d", got.ThumbnailBytes, len("jpg-one")+len("jpg-two!!"))
	}
	if got.TeaserBytes != int64(len("teaser-one")+len("teaser-two!!")) {
		t.Fatalf("teaser bytes = %d, want %d", got.TeaserBytes, len("teaser-one")+len("teaser-two!!"))
	}
	if got.TotalBytes != got.ThumbnailBytes+got.TeaserBytes {
		t.Fatalf("total bytes = %d, want thumbnail + teaser", got.TotalBytes)
	}
	if got.AvailableBytes <= 0 {
		t.Fatalf("available bytes = %d, want positive", got.AvailableBytes)
	}
	if got.Drives["drive-one"].ThumbnailBytes != int64(len("jpg-one")) ||
		got.Drives["drive-one"].TeaserBytes != int64(len("teaser-one")) {
		t.Fatalf("drive-one usage = %#v", got.Drives["drive-one"])
	}
	if got.Drives["drive-two"].TotalBytes != int64(len("jpg-two!!")+len("teaser-two!!")) {
		t.Fatalf("drive-two total = %d, want %d", got.Drives["drive-two"].TotalBytes, len("jpg-two!!")+len("teaser-two!!"))
	}
}

func TestHandleCreateTagClassifiesExistingVideos(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "video-1",
		DriveID:     "drive",
		FileID:      "file-1",
		Title:       "清纯短发",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/tags", strings.NewReader(`{"label":"清纯"}`))
	rr := httptest.NewRecorder()
	(&AdminServer{Catalog: cat}).handleCreateTag(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Label      string `json:"label"`
		Classified int    `json:"classified"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Label != "清纯" || got.Classified != 1 {
		t.Fatalf("response = %#v, want 清纯 classified 1", got)
	}

	video, err := cat.GetVideo(ctx, "video-1")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if len(video.Tags) != 1 || video.Tags[0] != "清纯" {
		t.Fatalf("video tags = %#v, want 清纯", video.Tags)
	}
}

func TestHandleDeleteTagRemovesTagFromVideos(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "video-1",
		DriveID:     "drive",
		FileID:      "file-1",
		Title:       "清纯短发",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if _, err := cat.CreateTagAndClassify(ctx, "清纯", nil, "user"); err != nil {
		t.Fatalf("create tag: %v", err)
	}
	tags, err := cat.ListTags(ctx)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	var tagID int64
	for _, tag := range tags {
		if tag.Label == "清纯" {
			tagID = tag.ID
			break
		}
	}
	if tagID == 0 {
		t.Fatal("created tag not found")
	}

	req := requestWithRouteParam(http.MethodDelete, "/admin/api/tags/1", "id", strconv.FormatInt(tagID, 10), strings.NewReader(``))
	rr := httptest.NewRecorder()
	(&AdminServer{Catalog: cat}).handleDeleteTag(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	video, err := cat.GetVideo(ctx, "video-1")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if len(video.Tags) != 0 {
		t.Fatalf("video tags = %#v, want none", video.Tags)
	}
}

func TestHandleSetVideoFrontendSelectedRequiresAllowedFrontendTag(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "video-1",
		DriveID:     "drive",
		FileID:      "file-1",
		Title:       "Video",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	server := &AdminServer{Catalog: cat}
	badReq := requestWithRouteParam(http.MethodPost, "/admin/api/videos/video-1/frontend-selected", "id", "video-1", strings.NewReader(`{"enabled":true}`))
	badRR := httptest.NewRecorder()
	server.handleSetVideoFrontendSelected(badRR, badReq)
	if badRR.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when no frontend-access tag; body = %s", badRR.Code, badRR.Body.String())
	}

	ensureAPITestTag(t, ctx, cat, "public-tag")
	if err := cat.SetManualVideoTags(ctx, "video-1", []string{"public-tag"}); err != nil {
		t.Fatalf("tag video: %v", err)
	}
	tag := mustAPITestTagByLabel(t, ctx, cat, "public-tag")
	if err := cat.SetTagFrontendAccess(ctx, tag.ID, true); err != nil {
		t.Fatalf("set tag frontend access: %v", err)
	}
	req := requestWithRouteParam(http.MethodPost, "/admin/api/videos/video-1/frontend-selected", "id", "video-1", strings.NewReader(`{"enabled":true}`))
	rr := httptest.NewRecorder()
	server.handleSetVideoFrontendSelected(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got catalog.Video
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.FrontendSelected {
		t.Fatalf("frontendSelected = false, want true: %#v", got)
	}
}

func TestHandleSetTagFrontendAccessTogglesTag(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	if _, err := cat.CreateTagAndClassify(ctx, "清纯", nil, "user"); err != nil {
		t.Fatalf("create tag: %v", err)
	}
	tag := mustAPITestTagByLabel(t, ctx, cat, "清纯")

	req := requestWithRouteParam(http.MethodPost, "/admin/api/tags/1/frontend-access", "id", strconv.FormatInt(tag.ID, 10), strings.NewReader(`{"enabled":true}`))
	rr := httptest.NewRecorder()
	(&AdminServer{Catalog: cat}).handleSetTagFrontendAccess(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		FrontendAccess bool `json:"frontendAccess"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.FrontendAccess {
		t.Fatalf("frontendAccess = false, want true")
	}
	updated := mustAPITestTagByLabel(t, ctx, cat, "清纯")
	if !updated.FrontendAccess {
		t.Fatalf("stored frontendAccess = false, want true")
	}
}

func TestHandleAdminListVideosFiltersByDriveID(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	videos := []*catalog.Video{
		{
			ID:          "od-video",
			DriveID:     "OneDrive",
			FileID:      "od-file",
			Title:       "OneDrive video",
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
		{
			ID:          "pp-video",
			DriveID:     "PikPak",
			FileID:      "pp-file",
			Title:       "PikPak video",
			PublishedAt: now.Add(-time.Hour),
			CreatedAt:   now,
			UpdatedAt:   now,
		},
	}
	for _, v := range videos {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/api/videos?driveId=OneDrive", nil)
	rr := httptest.NewRecorder()
	(&AdminServer{Catalog: cat}).handleAdminListVideos(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Items []catalog.Video `json:"items"`
		Total int             `json:"total"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 1 || len(got.Items) != 1 {
		t.Fatalf("response total/items = %d/%d, want 1/1: %#v", got.Total, len(got.Items), got.Items)
	}
	if got.Items[0].DriveID != "OneDrive" || got.Items[0].ID != "od-video" {
		t.Fatalf("item = %#v, want OneDrive od-video", got.Items[0])
	}
}

func TestHandleAdminListVideosPaginates(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	for i, title := range []string{"first", "second", "third"} {
		v := &catalog.Video{
			ID:          title,
			DriveID:     "OneDrive",
			FileID:      title + "-file",
			Title:       title,
			PublishedAt: now.Add(-time.Duration(i) * time.Hour),
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/api/videos?driveId=OneDrive&page=2&size=2", nil)
	rr := httptest.NewRecorder()
	(&AdminServer{Catalog: cat}).handleAdminListVideos(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Items []catalog.Video `json:"items"`
		Total int             `json:"total"`
		Page  int             `json:"page"`
		Size  int             `json:"size"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 3 || got.Page != 2 || got.Size != 2 {
		t.Fatalf("pagination meta = total:%d page:%d size:%d, want 3/2/2", got.Total, got.Page, got.Size)
	}
	if len(got.Items) != 1 || got.Items[0].ID != "third" {
		t.Fatalf("items = %#v, want only third", got.Items)
	}
}

func TestHandleRegenAllPreviewsInvokesHook(t *testing.T) {
	called := false
	server := &AdminServer{
		OnRegenAllPreviews: func() {
			called = true
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/videos/regen-preview", nil)
	rr := httptest.NewRecorder()
	server.handleRegenAllPreviews(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if !called {
		t.Fatal("regen all previews hook was not called")
	}
}

func TestHandleRegenFailedPreviewsInvokesHookWithDriveID(t *testing.T) {
	calledWith := ""
	server := &AdminServer{
		OnRegenFailedPreviews: func(driveID string) {
			calledWith = driveID
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/drives/PikPak/previews/failed/regenerate", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "PikPak")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	server.handleRegenFailedPreviews(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if calledWith != "PikPak" {
		t.Fatalf("hook called with %q, want PikPak", calledWith)
	}
}
