package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/video-site/backend/internal/auth"
	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives/scriptcrawler"
)

type adminTestDriveConfigLease struct {
	busyReason     string
	deferred       bool
	scope          DriveConfigUpdateScope
	committedScope DriveConfigUpdateScope
	pendingApply   []func() error
	released       bool
}

func (lease *adminTestDriveConfigLease) Authorize(scope DriveConfigUpdateScope) string {
	lease.scope = scope
	return lease.busyReason
}

func (lease *adminTestDriveConfigLease) Commit(scope DriveConfigUpdateScope, apply func() error) (bool, error) {
	lease.committedScope |= scope
	if lease.deferred {
		if apply != nil {
			lease.pendingApply = append(lease.pendingApply, apply)
		}
		return true, nil
	}
	if apply == nil {
		return false, nil
	}
	return false, apply()
}

func (lease *adminTestDriveConfigLease) Release() {
	lease.released = true
}

func TestHandleUpdateVideoRejectsReadOnlyTitleAndAuthor(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID: "video-read-only", DriveID: "drive", FileID: "file", FileName: "file.mp4",
		Title: "file", Author: "author", PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/admin/api/videos/video-read-only", strings.NewReader(`{"title":"changed"}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "video-read-only")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	(&AdminServer{Catalog: cat}).handleUpdateVideo(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	got, err := cat.GetVideo(ctx, "video-read-only")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.Title != "file" || got.Author != "author" {
		t.Fatalf("metadata changed: title=%q author=%q", got.Title, got.Author)
	}
}

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
	ok, _, err := cat.ValidateSession(context.Background(), cookies[0].Value)
	if err != nil || !ok {
		t.Fatalf("setup session valid=%v err=%v", ok, err)
	}
}

func TestHandleBanUserDeletesSessions(t *testing.T) {
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
	hash, err := auth.HashPassword("secret123")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	userID, err := cat.CreateUser(ctx, "viewer", hash, "user")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := cat.CreateSession(ctx, "viewer-token", time.Hour, userID); err != nil {
		t.Fatalf("create session: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/users/1/ban", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", strconv.FormatInt(userID, 10))
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()

	(&AdminServer{Catalog: cat}).handleBanUser(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	ok, _, err := cat.ValidateSession(ctx, "viewer-token")
	if err != nil {
		t.Fatalf("validate session: %v", err)
	}
	if ok {
		t.Fatal("banned user session is still valid")
	}
}

func TestHandleBanUserRejectsLastActiveAdmin(t *testing.T) {
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
	hash, err := auth.HashPassword("secret123")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	adminID, err := cat.CreateUser(ctx, "owner", hash, "admin")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/users/1/ban", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", strconv.FormatInt(adminID, 10))
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()

	(&AdminServer{Catalog: cat}).handleBanUser(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
}

func TestHandleDeleteVideoDefaultsDeleteSourceFalse(t *testing.T) {
	req := httptest.NewRequest(http.MethodDelete, "/admin/api/videos/video-1", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "video-1")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()

	called := false
	(&AdminServer{
		OnDeleteVideo: func(ctx context.Context, videoID string, deleteSource bool) (DeleteVideoResult, error) {
			called = true
			if videoID != "video-1" {
				t.Fatalf("videoID = %q, want video-1", videoID)
			}
			if deleteSource {
				t.Fatal("deleteSource defaulted to true")
			}
			return DeleteVideoResult{OK: true}, nil
		},
	}).handleDeleteVideo(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	if !called {
		t.Fatal("OnDeleteVideo was not called")
	}
}

func TestHandleDeleteVideoPassesDeleteSourceOption(t *testing.T) {
	req := httptest.NewRequest(http.MethodDelete, "/admin/api/videos/video-1", strings.NewReader(`{"deleteSource":true}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "video-1")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()

	(&AdminServer{
		OnDeleteVideo: func(ctx context.Context, videoID string, deleteSource bool) (DeleteVideoResult, error) {
			if !deleteSource {
				t.Fatal("deleteSource = false, want true")
			}
			return DeleteVideoResult{OK: true, DeletedSource: true}, nil
		},
	}).handleDeleteVideo(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	var got DeleteVideoResult
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !got.DeletedSource {
		t.Fatalf("DeletedSource = false, want true; response = %s", rr.Body.String())
	}
}

func TestHandleRemoveBlacklistRejectsNonRestorableVideo(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	// 被去重删掉的视频不可恢复，应当去看保留下来的那一条。
	now := time.Now()
	for _, id := range []string{"canonical-video", "duplicate-video"} {
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID: id, DriveID: "remote", FileID: id + ".mp4",
			Title: id, PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	if err := cat.DeleteVideoWithTombstoneOptions(ctx, "duplicate-video", catalog.DeleteVideoTombstoneOptions{
		Reason:           catalog.DeletedVideoReasonDuplicate,
		CanonicalVideoID: "canonical-video",
	}); err != nil {
		t.Fatalf("tombstone video: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/admin/api/blacklist/duplicate-video", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "duplicate-video")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()

	(&AdminServer{Catalog: cat}).handleRemoveBlacklist(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rr.Code, rr.Body.String())
	}
	if deleted, err := cat.IsVideoDeleted(ctx, "duplicate-video"); err != nil || !deleted {
		t.Fatalf("non-restorable tombstone was removed: deleted=%v err=%v", deleted, err)
	}
}

// 本地上传的视频没有任何重新发现的途径，取消拉黑必须当场把记录放回媒体库。
func TestHandleRemoveBlacklistRestoresLocalUploadImmediately(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID: "local-upload-video", DriveID: "local-upload", FileID: "upload.mp4",
		FileName: "upload.mp4", Title: "Upload", PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if err := cat.DeleteVideoWithTombstone(ctx, "local-upload-video"); err != nil {
		t.Fatalf("tombstone video: %v", err)
	}

	verified := ""
	server := &AdminServer{
		Catalog: cat,
		OnRemoveBlacklist: func(ctx context.Context, id string) error {
			_, err := cat.RestoreDeletedVideo(ctx, id, func(driveID, fileID string) (catalog.DeletedVideoSourceInfo, error) {
				verified = driveID + "/" + fileID
				return catalog.DeletedVideoSourceInfo{Size: 1024, ModTime: now}, nil
			})
			return err
		},
	}

	req := httptest.NewRequest(http.MethodDelete, "/admin/api/blacklist/local-upload-video", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "local-upload-video")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()

	server.handleRemoveBlacklist(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	if verified != "local-upload/upload.mp4" {
		t.Fatalf("verified source = %q, want local-upload/upload.mp4", verified)
	}
	if _, err := cat.GetVideo(ctx, "local-upload-video"); err != nil {
		t.Fatalf("video was not restored: %v", err)
	}
	if deleted, err := cat.IsVideoDeleted(ctx, "local-upload-video"); err != nil || deleted {
		t.Fatalf("tombstone still present: deleted=%v err=%v", deleted, err)
	}
}

// 源文件已经不在时必须拒绝恢复，否则库里会多出一条点开就播放失败的视频。
func TestHandleRemoveBlacklistRejectsDirectRestoreWhenSourceMissing(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID: "local-upload-video", DriveID: "local-upload", FileID: "upload.mp4",
		FileName: "upload.mp4", Title: "Upload", PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if err := cat.DeleteVideoWithTombstone(ctx, "local-upload-video"); err != nil {
		t.Fatalf("tombstone video: %v", err)
	}

	server := &AdminServer{
		Catalog: cat,
		OnRemoveBlacklist: func(ctx context.Context, id string) error {
			_, err := cat.RestoreDeletedVideo(ctx, id, func(string, string) (catalog.DeletedVideoSourceInfo, error) {
				return catalog.DeletedVideoSourceInfo{}, errors.New("stat upload.mp4: no such file")
			})
			return err
		},
	}

	req := httptest.NewRequest(http.MethodDelete, "/admin/api/blacklist/local-upload-video", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "local-upload-video")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()

	server.handleRemoveBlacklist(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rr.Code, rr.Body.String())
	}
	if deleted, err := cat.IsVideoDeleted(ctx, "local-upload-video"); err != nil || !deleted {
		t.Fatalf("tombstone was removed despite missing source: deleted=%v err=%v", deleted, err)
	}
	if _, err := cat.GetVideo(ctx, "local-upload-video"); err == nil {
		t.Fatalf("video must not be restored when the source file is gone")
	}
}

// 可扫描来源不该触发源文件校验：它们本来就要等下一轮扫盘，此刻文件在不在
// 都不影响「删掉墓碑」这个动作。
func TestHandleRemoveBlacklistSkipsSourceCheckForScannableDrive(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID: "remote-video", DriveID: "remote", FileID: "remote.mp4",
		FileName: "remote.mp4", Title: "Remote", PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if err := cat.DeleteVideoWithTombstone(ctx, "remote-video"); err != nil {
		t.Fatalf("tombstone video: %v", err)
	}

	server := &AdminServer{
		Catalog: cat,
		OnRemoveBlacklist: func(ctx context.Context, id string) error {
			_, err := cat.RestoreDeletedVideo(ctx, id, func(string, string) (catalog.DeletedVideoSourceInfo, error) {
				t.Fatalf("scannable drive must not be source-checked")
				return catalog.DeletedVideoSourceInfo{}, nil
			})
			return err
		},
	}

	req := httptest.NewRequest(http.MethodDelete, "/admin/api/blacklist/remote-video", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "remote-video")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()

	server.handleRemoveBlacklist(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	// 扫盘来源只是移除墓碑，不会当场重建记录。
	if _, err := cat.GetVideo(ctx, "remote-video"); err == nil {
		t.Fatalf("scannable restore must wait for the next scan")
	}
	if deleted, err := cat.IsVideoDeleted(ctx, "remote-video"); err != nil || deleted {
		t.Fatalf("tombstone not removed: deleted=%v err=%v", deleted, err)
	}
}

func TestHandleStartBlacklistSourceDeleteReturnsBackgroundStatus(t *testing.T) {
	started := false
	server := &AdminServer{
		OnStartBlacklistSourceDelete: func(req BlacklistSourceDeleteRequest) bool {
			if !req.DeleteAllSources || len(req.IDs) != 0 {
				t.Fatalf("request = %#v, want all sources", req)
			}
			started = true
			return true
		},
		GetBlacklistSourceDeleteStatus: func() BlacklistSourceDeleteStatus {
			return BlacklistSourceDeleteStatus{
				State: "running", Running: true, Total: 12, Processed: 3, Deleted: 2, Failed: 1,
			}
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/blacklist/source-delete", strings.NewReader(`{"deleteAllSources":true}`))
	rr := httptest.NewRecorder()

	server.handleStartBlacklistSourceDelete(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rr.Code, rr.Body.String())
	}
	if !started {
		t.Fatal("source delete hook was not called")
	}
	var got struct {
		Accepted bool                        `json:"accepted"`
		Status   BlacklistSourceDeleteStatus `json:"status"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !got.Accepted ||
		!got.Status.Running ||
		got.Status.Total != 12 ||
		got.Status.Processed != 3 ||
		got.Status.Deleted != 2 ||
		got.Status.Failed != 1 {
		t.Fatalf("response = %#v", got)
	}
}

func TestHandleStartBlacklistSourceDeleteRequiresExplicitConfirmation(t *testing.T) {
	called := false
	server := &AdminServer{
		OnStartBlacklistSourceDelete: func(_ BlacklistSourceDeleteRequest) bool {
			called = true
			return true
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/blacklist/source-delete", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()

	server.handleStartBlacklistSourceDelete(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	if called {
		t.Fatal("source delete hook ran without explicit confirmation")
	}
}

func TestHandleStartBlacklistSourceDeleteAcceptsExplicitIDs(t *testing.T) {
	var got BlacklistSourceDeleteRequest
	server := &AdminServer{
		OnStartBlacklistSourceDelete: func(req BlacklistSourceDeleteRequest) bool {
			got = req
			return true
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/blacklist/source-delete", strings.NewReader(`{"ids":[" a ","","b","a"]}`))
	rr := httptest.NewRecorder()

	server.handleStartBlacklistSourceDelete(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rr.Code, rr.Body.String())
	}
	if got.DeleteAllSources || len(got.IDs) != 2 || got.IDs[0] != "a" || got.IDs[1] != "b" {
		t.Fatalf("request = %#v", got)
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
			"body":     "## Changes\n\n- Added update notes dialog",
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
	if got.ReleaseNotes != "## Changes\n\n- Added update notes dialog" {
		t.Fatalf("releaseNotes = %q", got.ReleaseNotes)
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

func TestHandleRunScanAllJobReturnsAcceptedStatus(t *testing.T) {
	called := false
	req := httptest.NewRequest(http.MethodPost, "/admin/api/jobs/scan-all/run", nil)
	rr := httptest.NewRecorder()

	(&AdminServer{
		OnRunScanAllJob: func() bool {
			called = true
			return true
		},
		GetNightlyJobStatus: func() NightlyJobStatus {
			return NightlyJobStatus{State: "queued", Queued: true}
		},
	}).handleRunScanAllJob(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rr.Code, rr.Body.String())
	}
	if !called {
		t.Fatal("OnRunScanAllJob was not called")
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

func TestHandleRunScanAllJobReturnsBusyMessageWhenRejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/admin/api/jobs/scan-all/run", nil)
	rr := httptest.NewRecorder()

	(&AdminServer{
		OnRunScanAllJob: func() bool {
			return false
		},
		GetNightlyJobStatus: func() NightlyJobStatus {
			return NightlyJobStatus{State: "running", Running: true}
		},
	}).handleRunScanAllJob(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		OK       bool             `json:"ok"`
		Accepted bool             `json:"accepted"`
		Message  string           `json:"message"`
		Status   NightlyJobStatus `json:"status"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK || got.Accepted || got.Message != fullScanBusyMessage || !got.Status.Running {
		t.Fatalf("response = %#v, want rejected busy message", got)
	}
}

func TestHandleRescanRejectsWhenNightlyBusy(t *testing.T) {
	called := false
	req := httptest.NewRequest(http.MethodPost, "/admin/api/drives/PikPak/rescan", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "PikPak")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()

	(&AdminServer{
		OnScanRequested: func(driveID string) bool {
			called = true
			return true
		},
		GetNightlyJobStatus: func() NightlyJobStatus {
			return NightlyJobStatus{State: "running", Running: true}
		},
	}).handleRescan(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rr.Code, rr.Body.String())
	}
	if called {
		t.Fatal("OnScanRequested was called while nightly job was busy")
	}
	var got struct {
		OK       bool             `json:"ok"`
		Accepted bool             `json:"accepted"`
		Message  string           `json:"message"`
		Status   NightlyJobStatus `json:"status"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK || got.Accepted || got.Message != fullScanBusyMessage || !got.Status.Running {
		t.Fatalf("response = %#v, want rejected full scan busy message", got)
	}
}

func TestHandleRescanReturnsAcceptedFlagAndBusyMessage(t *testing.T) {
	calledWith := ""
	req := httptest.NewRequest(http.MethodPost, "/admin/api/drives/PikPak/rescan", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "PikPak")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()

	(&AdminServer{
		OnScanRequested: func(driveID string) bool {
			calledWith = driveID
			return false
		},
	}).handleRescan(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		OK       bool   `json:"ok"`
		Accepted bool   `json:"accepted"`
		Message  string `json:"message"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if calledWith != "PikPak" {
		t.Fatalf("hook called with %q, want PikPak", calledWith)
	}
	if !got.OK || got.Accepted || got.Message != driveTaskBusyMessage {
		t.Fatalf("response = %#v, want rejected busy message", got)
	}
}

func TestHandleScanAllJobStatusDefaultsToIdle(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/admin/api/jobs/scan-all/status", nil)
	rr := httptest.NewRecorder()

	(&AdminServer{}).handleScanAllJobStatus(rr, req)

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

func TestHandleStopDriveTasksInvokesHookWithDriveID(t *testing.T) {
	calledWith := ""
	server := &AdminServer{
		OnStopDriveTasks: func(driveID string) bool {
			calledWith = driveID
			return true
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/drives/PikPak/tasks/stop", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "PikPak")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	server.handleStopDriveTasks(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if calledWith != "PikPak" {
		t.Fatalf("hook called with %q, want PikPak", calledWith)
	}
	var got struct {
		OK      bool `json:"ok"`
		Stopped bool `json:"stopped"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK || !got.Stopped {
		t.Fatalf("response = %#v, want stopped", got)
	}
}

func TestHandleStopAllTasksInvokesHookAndReturnsStatus(t *testing.T) {
	called := false
	server := &AdminServer{
		OnStopAllTasks: func() int {
			called = true
			return 2
		},
		GetNightlyJobStatus: func() NightlyJobStatus {
			return NightlyJobStatus{State: "running", Running: true}
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/tasks/stop", nil)
	rr := httptest.NewRecorder()
	server.handleStopAllTasks(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if !called {
		t.Fatal("OnStopAllTasks was not called")
	}
	var got struct {
		OK            bool             `json:"ok"`
		StoppedDrives int              `json:"stoppedDrives"`
		Status        NightlyJobStatus `json:"status"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK || got.StoppedDrives != 2 || got.Status.State != "running" || !got.Status.Running {
		t.Fatalf("response = %#v, want stopped drives and status", got)
	}
}

func TestHandleUpsertDriveMetadataOnlySaveDoesNotReloadRuntime(t *testing.T) {
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
		SkipDirIDs: []string{"keep-skipped"},
		Credentials: map[string]string{
			"cookie": "existing-cookie",
		},
		Status:        "ok",
		TeaserEnabled: true,
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
	runtimeReloads := 0
	lease := &adminTestDriveConfigLease{}
	server := &AdminServer{
		Catalog: cat,
		BeginDriveConfigUpdate: func(string) (DriveConfigUpdateLease, string) {
			return lease, ""
		},
		OnDriveRuntimeConfigChanged: func(string) error {
			runtimeReloads++
			return nil
		},
	}

	server.handleUpsertDrive(rr, req)

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
	if len(got.SkipDirIDs) != 1 || got.SkipDirIDs[0] != "keep-skipped" {
		t.Fatalf("skip dir ids = %#v, want preserved setting", got.SkipDirIDs)
	}
	if got.Status != "ok" {
		t.Fatalf("status = %q, want preserved ok", got.Status)
	}
	if runtimeReloads != 0 {
		t.Fatalf("runtime reloads = %d, want 0 for metadata-only save", runtimeReloads)
	}
	if lease.scope != 0 || !lease.released {
		t.Fatalf("config lease scope/released = %v/%v, want metadata-only released lease", lease.scope, lease.released)
	}
}

func TestDriveRuntimeReloadRequired(t *testing.T) {
	base := &catalog.Drive{
		ID:            "drive-main",
		Kind:          "onedrive",
		Name:          "Old name",
		RootID:        "root",
		TeaserEnabled: true,
		Credentials:   map[string]string{"refresh_token": "persisted-refresh"},
	}
	tests := []struct {
		name string
		next *catalog.Drive
		want bool
	}{
		{
			name: "metadata only",
			next: &catalog.Drive{ID: "drive-main", Kind: "onedrive", Name: "New name", RootID: "root", TeaserEnabled: false},
		},
		{
			name: "root changed",
			next: &catalog.Drive{ID: "drive-main", Kind: "onedrive", RootID: "other-root"},
			want: true,
		},
		{
			name: "credential patch",
			next: &catalog.Drive{ID: "drive-main", Kind: "onedrive", RootID: "root", Credentials: map[string]string{"refresh_token": "replacement"}},
			want: true,
		},
		{
			name: "kind changed",
			next: &catalog.Drive{ID: "drive-main", Kind: "pikpak", RootID: "root"},
			want: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := driveRuntimeReloadRequired(base, test.next); got != test.want {
				t.Fatalf("driveRuntimeReloadRequired() = %v, want %v", got, test.want)
			}
		})
	}
	if !driveRuntimeReloadRequired(nil, &catalog.Drive{Kind: "onedrive", RootID: "root"}) {
		t.Fatal("new drive did not require runtime reload")
	}
	crawler := &catalog.Drive{ID: "crawler-main", Kind: scriptcrawler.Kind, RootID: "/"}
	if !driveRuntimeReloadRequired(crawler, crawler) {
		t.Fatal("script crawler save did not require runtime reload")
	}
}

func TestHandleUpsertDriveClearsSkipDirsWhenExplicitlyRequested(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:         "quark-main",
		Kind:       "quark",
		Name:       "Quark",
		RootID:     "0",
		SkipDirIDs: []string{"remove-me"},
		Credentials: map[string]string{
			"cookie": "existing-cookie",
		},
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/drives", strings.NewReader(`{
		"id": "quark-main",
		"kind": "quark",
		"name": "Quark",
		"rootId": "0",
		"credentials": {},
		"skipDirIds": []
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
	if len(got.SkipDirIDs) != 0 {
		t.Fatalf("skip dir ids = %#v, want cleared", got.SkipDirIDs)
	}
}

func TestDriveTaskSensitiveSettingHandlersSaveAndDeferApply(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:            "drive-main",
		Kind:          "onedrive",
		Name:          "OneDrive",
		RootID:        "root",
		TeaserEnabled: true,
		SkipDirIDs:    []string{"old-dir"},
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	withDriveID := func(req *http.Request) *http.Request {
		routeCtx := chi.NewRouteContext()
		routeCtx.URLParams.Add("id", "drive-main")
		return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))
	}

	t.Run("preview", func(t *testing.T) {
		lease := &adminTestDriveConfigLease{deferred: true}
		callbackCalled := false
		server := &AdminServer{
			Catalog: cat,
			BeginDriveConfigUpdate: func(string) (DriveConfigUpdateLease, string) {
				return lease, ""
			},
			OnTeaserEnabledChanged: func(string, bool) { callbackCalled = true },
		}
		req := withDriveID(httptest.NewRequest(http.MethodPost, "/admin/api/drives/drive-main/teaser-enabled", strings.NewReader(`{"enabled":false}`)))
		rr := httptest.NewRecorder()
		server.handleSetDriveTeaserEnabled(rr, req)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"deferred":true`) {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
		}
		got, err := cat.GetDrive(ctx, "drive-main")
		if err != nil {
			t.Fatalf("get drive: %v", err)
		}
		if got.TeaserEnabled || callbackCalled {
			t.Fatalf("teaser persisted/callback = %v/%v, want false/deferred", got.TeaserEnabled, callbackCalled)
		}
		if lease.scope != DriveConfigUpdatePreview || lease.committedScope != DriveConfigUpdatePreview || !lease.released {
			t.Fatalf("lease scopes/released = %v/%v/%v", lease.scope, lease.committedScope, lease.released)
		}
	})

	t.Run("skip directories", func(t *testing.T) {
		lease := &adminTestDriveConfigLease{deferred: true}
		server := &AdminServer{
			Catalog: cat,
			BeginDriveConfigUpdate: func(string) (DriveConfigUpdateLease, string) {
				return lease, ""
			},
		}
		req := withDriveID(httptest.NewRequest(http.MethodPost, "/admin/api/drives/drive-main/skip-dirs", strings.NewReader(`{"dirIds":["new-dir"]}`)))
		rr := httptest.NewRecorder()
		server.handleSetDriveSkipDirs(rr, req)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"deferred":true`) {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
		}
		got, err := cat.GetDrive(ctx, "drive-main")
		if err != nil {
			t.Fatalf("get drive: %v", err)
		}
		if len(got.SkipDirIDs) != 1 || got.SkipDirIDs[0] != "new-dir" {
			t.Fatalf("skip dirs = %#v, want saved desired value", got.SkipDirIDs)
		}
		if lease.scope != DriveConfigUpdateScan || lease.committedScope != DriveConfigUpdateScan || !lease.released {
			t.Fatalf("lease scopes/released = %v/%v/%v", lease.scope, lease.committedScope, lease.released)
		}
	})
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

func TestHandleUpsertDriveSavesBusyRuntimeAndDefersReload(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:          "quark-main",
		Kind:        "quark",
		Name:        "Quark",
		RootID:      "0",
		Credentials: map[string]string{"cookie": "old-cookie"},
		Status:      "ok",
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	lease := &adminTestDriveConfigLease{deferred: true}
	reloaded := false
	server := &AdminServer{
		Catalog: cat,
		BeginDriveConfigUpdate: func(string) (DriveConfigUpdateLease, string) {
			return lease, ""
		},
		OnDriveRuntimeConfigChanged: func(string) error {
			reloaded = true
			return nil
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/drives", strings.NewReader(`{
		"id":"quark-main",
		"kind":"quark",
		"name":"Quark",
		"rootId":"0",
		"credentials":{"cookie":"new-cookie"}
	}`))
	rr := httptest.NewRecorder()
	server.handleUpsertDrive(rr, req)

	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"deferred":true`) {
		t.Fatalf("status/body = %d/%q, want deferred success", rr.Code, rr.Body.String())
	}
	got, err := cat.GetDrive(ctx, "quark-main")
	if err != nil {
		t.Fatalf("get drive: %v", err)
	}
	if got.Credentials["cookie"] != "new-cookie" {
		t.Fatalf("desired credentials were not saved: %#v", got.Credentials)
	}
	if reloaded {
		t.Fatal("runtime reload ran before deferred callback")
	}
	if lease.scope != DriveConfigUpdateRuntime || lease.committedScope != DriveConfigUpdateRuntime || !lease.released {
		t.Fatalf("lease scopes/released = %v/%v/%v", lease.scope, lease.committedScope, lease.released)
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
	runtimeReloads := 0
	server := &AdminServer{
		Catalog: cat,
		OnDriveRuntimeConfigChanged: func(string) error {
			runtimeReloads++
			return nil
		},
	}

	server.handleUpsertDrive(rr, req)

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
	if runtimeReloads != 1 {
		t.Fatalf("runtime reloads = %d, want 1 for credential change", runtimeReloads)
	}
}

func TestHandleUpsertPikPakPatchesCredentials(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:     "pikpak-main",
		Kind:   "pikpak",
		Name:   "PikPak",
		RootID: "",
		Credentials: map[string]string{
			"username":      "old-user",
			"password":      "old-password",
			"access_token":  "runtime-access",
			"refresh_token": "runtime-refresh",
			"captcha_token": "runtime-captcha",
			"device_id":     "runtime-device",
		},
		Status: "ok",
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/drives", bytes.NewBufferString(`{
		"id": "pikpak-main",
		"kind": "pikpak",
		"name": "Renamed PikPak",
		"rootId": "",
		"credentials": {"username": "new-user"}
	}`))
	rr := httptest.NewRecorder()

	(&AdminServer{Catalog: cat}).handleUpsertDrive(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got, err := cat.GetDrive(ctx, "pikpak-main")
	if err != nil {
		t.Fatalf("get drive: %v", err)
	}
	if got.Name != "Renamed PikPak" || got.Credentials["username"] != "new-user" {
		t.Fatalf("editable fields not updated: %+v", got)
	}
	for key, want := range map[string]string{
		"password":      "old-password",
		"access_token":  "runtime-access",
		"refresh_token": "runtime-refresh",
		"captcha_token": "runtime-captcha",
		"device_id":     "runtime-device",
	} {
		if got.Credentials[key] != want {
			t.Fatalf("credential %s = %q, want %q; all=%#v", key, got.Credentials[key], want, got.Credentials)
		}
	}
}

func TestHandleUpsertOneDrivePatchesRuntimeTokens(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:     "onedrive-main",
		Kind:   "onedrive",
		Name:   "OneDrive",
		RootID: "root",
		Credentials: map[string]string{
			"access_token":  "runtime-access",
			"refresh_token": "runtime-refresh",
			"region":        "global",
		},
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/drives", bytes.NewBufferString(`{
		"id": "onedrive-main",
		"kind": "onedrive",
		"name": "Renamed OneDrive",
		"rootId": "root",
		"credentials": {"region": "cn"}
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
	if got.Name != "Renamed OneDrive" || got.Credentials["region"] != "cn" {
		t.Fatalf("editable settings not updated: %+v", got)
	}
	if got.Credentials["access_token"] != "runtime-access" || got.Credentials["refresh_token"] != "runtime-refresh" {
		t.Fatalf("runtime tokens were replaced: %#v", got.Credentials)
	}
}

func TestHandleUpsertPikPakDoesNotInheritCredentialsFromAnotherKind(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:     "drive-main",
		Kind:   "quark",
		Name:   "Quark",
		RootID: "0",
		Credentials: map[string]string{
			"cookie": "old-quark-cookie",
		},
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/drives", bytes.NewBufferString(`{
		"id": "drive-main",
		"kind": "pikpak",
		"name": "PikPak",
		"rootId": "",
		"credentials": {"refresh_token": "pikpak-refresh"}
	}`))
	rr := httptest.NewRecorder()

	(&AdminServer{Catalog: cat}).handleUpsertDrive(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got, err := cat.GetDrive(ctx, "drive-main")
	if err != nil {
		t.Fatalf("get drive: %v", err)
	}
	if got.Kind != "pikpak" || got.Credentials["refresh_token"] != "pikpak-refresh" {
		t.Fatalf("pikpak fields not saved: %+v", got)
	}
	if _, ok := got.Credentials["cookie"]; ok {
		t.Fatalf("credentials retained from prior drive kind: %#v", got.Credentials)
	}
}

func TestHandleUpsertGoogleDriveMergesOAuthCredentials(t *testing.T) {
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
		ID:     "google-main",
		Kind:   "googledrive",
		Name:   "Google Drive",
		RootID: "root",
		Credentials: map[string]string{
			"refresh_token":   "existing-refresh",
			"access_token":    "existing-access",
			"use_online_api":  "true",
			"api_url_address": "https://api.oplist.org/googleui/renewapi",
		},
		Status: "ok",
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/drives", bytes.NewBufferString(`{
		"id": "google-main",
		"kind": "googledrive",
		"name": "Google Drive",
		"rootId": "root",
		"credentials": {
			"use_online_api": "false",
			"client_id": "google-client-id",
			"client_secret": "google-client-secret"
		}
	}`))
	rr := httptest.NewRecorder()

	(&AdminServer{Catalog: cat}).handleUpsertDrive(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got, err := cat.GetDrive(ctx, "google-main")
	if err != nil {
		t.Fatalf("get drive: %v", err)
	}
	if got.Credentials["refresh_token"] != "existing-refresh" || got.Credentials["access_token"] != "existing-access" {
		t.Fatalf("tokens were not preserved: %#v", got.Credentials)
	}
	if got.Credentials["client_id"] != "google-client-id" || got.Credentials["client_secret"] != "google-client-secret" {
		t.Fatalf("oauth client credentials = %#v, want saved", got.Credentials)
	}
	if _, ok := got.Credentials["use_online_api"]; ok {
		t.Fatalf("deprecated use_online_api was not removed: %#v", got.Credentials)
	}
	if _, ok := got.Credentials["api_url_address"]; ok {
		t.Fatalf("deprecated api_url_address was not removed: %#v", got.Credentials)
	}
}

func TestHandleUpsertOneDriveCanClearCustomOAuthCredentials(t *testing.T) {
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
		ID:     "onedrive-main",
		Kind:   "onedrive",
		Name:   "OneDrive",
		RootID: "root",
		Credentials: map[string]string{
			"refresh_token": "existing-refresh",
			"access_token":  "existing-access",
			"auth_mode":     "custom_app",
			"client_id":     "custom-client-id",
			"client_secret": "custom-client-secret",
		},
		Status: "ok",
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/drives", bytes.NewBufferString(`{
		"id": "onedrive-main",
		"kind": "onedrive",
		"name": "OneDrive",
		"rootId": "root",
		"credentials": {
			"auth_mode": "openlist_api",
			"client_id": "",
			"client_secret": ""
		}
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
	if got.Credentials["client_id"] != "" || got.Credentials["client_secret"] != "" {
		t.Fatalf("custom oauth credentials were not cleared: %#v", got.Credentials)
	}
	if got.Credentials["auth_mode"] != "openlist_api" {
		t.Fatalf("auth mode = %q, want openlist_api", got.Credentials["auth_mode"])
	}
	if got.Credentials["refresh_token"] != "existing-refresh" || got.Credentials["access_token"] != "existing-access" {
		t.Fatalf("tokens were not preserved: %#v", got.Credentials)
	}
}

func TestHandleUpsertWebDAVMergesCredentials(t *testing.T) {
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
		ID:     "webdav-main",
		Kind:   "webdav",
		Name:   "WebDAV",
		RootID: "/media",
		Credentials: map[string]string{
			"base_url": "https://openlist.example.com/dav",
			"username": "existing-user",
			"password": "existing-password",
		},
		Status: "ok",
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/drives", bytes.NewBufferString(`{
		"id": "webdav-main",
		"kind": "webdav",
		"name": "WebDAV",
		"rootId": "/media",
		"credentials": {
			"base_url": "",
			"username": "updated-user",
			"password": ""
		}
	}`))
	rr := httptest.NewRecorder()
	(&AdminServer{Catalog: cat}).handleUpsertDrive(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got, err := cat.GetDrive(ctx, "webdav-main")
	if err != nil {
		t.Fatalf("get drive: %v", err)
	}
	if got.Credentials["base_url"] != "https://openlist.example.com/dav" {
		t.Fatalf("base_url = %q, want preserved", got.Credentials["base_url"])
	}
	if got.Credentials["username"] != "updated-user" {
		t.Fatalf("username = %q, want updated-user", got.Credentials["username"])
	}
	if got.Credentials["password"] != "existing-password" {
		t.Fatalf("password = %q, want preserved", got.Credentials["password"])
	}
	if !isCrawlerUploadTargetKind(got.Kind) {
		t.Fatalf("kind %q should be a crawler upload target", got.Kind)
	}
}

func TestHandleGetDriveCredentialsReturnsStoredValues(t *testing.T) {
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

	want := map[string]string{
		"base_url": "https://openlist.example.com/dav",
		"username": "stored-user",
		"password": "stored-password",
	}
	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:          "webdav-main",
		Kind:        "webdav",
		Name:        "WebDAV",
		RootID:      "/media",
		Credentials: want,
		Status:      "ok",
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/api/drives/webdav-main/credentials", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "webdav-main")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	(&AdminServer{Catalog: cat}).handleGetDriveCredentials(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var got struct {
		Credentials map[string]string `json:"credentials"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode credentials: %v", err)
	}
	if len(got.Credentials) != len(want) {
		t.Fatalf("credentials = %#v, want %#v", got.Credentials, want)
	}
	for key, value := range want {
		if got.Credentials[key] != value {
			t.Fatalf("credential %q = %q, want %q", key, got.Credentials[key], value)
		}
	}
}

func TestHandleUpsertUnknownDriveKindIsRejected(t *testing.T) {
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
		ID:     "unknown-main",
		Kind:   "unknown",
		Name:   "Unknown",
		RootID: "/",
		Credentials: map[string]string{
			"token": "old-token",
		},
		Status: "ok",
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/drives", strings.NewReader(`{
		"id": "unknown-main",
		"kind": "unknown",
		"name": "Unknown",
		"rootId": "/",
		"credentials": {"token": "new-token"}
	}`))
	rr := httptest.NewRecorder()
	(&AdminServer{Catalog: cat}).handleUpsertDrive(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "unsupported drive kind\n" {
		t.Fatalf("body = %q, want unsupported kind", rr.Body.String())
	}

	got, err := cat.GetDrive(ctx, "unknown-main")
	if err != nil {
		t.Fatalf("get drive: %v", err)
	}
	if got.Credentials["token"] != "old-token" {
		t.Fatalf("token = %q, want unchanged old token", got.Credentials["token"])
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
	preparedCalled := ""
	removedCalled := ""
	lease := &adminTestDriveConfigLease{}
	req := httptest.NewRequest(http.MethodDelete, "/admin/api/drives/drive-one", strings.NewReader(`{"deleteVideos":true}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "drive-one")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()

	(&AdminServer{
		Catalog: cat,
		BeginDriveConfigUpdate: func(driveID string) (DriveConfigUpdateLease, string) {
			if driveID != "drive-one" {
				t.Fatalf("begin delete lease for %q", driveID)
			}
			return lease, ""
		},
		OnPrepareDriveDelete: func(_ context.Context, driveID string) error {
			preparedCalled = driveID
			return nil
		},
		OnDriveDeleteCleanup: func(cleanupCtx context.Context, driveID string) (int, error) {
			if preparedCalled != driveID {
				t.Fatal("cleanup ran before task stop-and-drain preparation")
			}
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
	if preparedCalled != "drive-one" {
		t.Fatalf("prepare called with %q, want drive-one", preparedCalled)
	}
	if cleanupCalled != "drive-one" {
		t.Fatalf("cleanup called with %q, want drive-one", cleanupCalled)
	}
	if lease.scope != DriveConfigUpdateDestructive || lease.committedScope != DriveConfigUpdateDestructive || !lease.released {
		t.Fatalf("delete lease scope/committed/released = %d/%d/%v", lease.scope, lease.committedScope, lease.released)
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

func TestHandleDeleteDriveKeepsDriveWhenTaskDrainFails(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID: "drive-one", Kind: "pikpak", Name: "Drive One", RootID: "root",
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}
	lease := &adminTestDriveConfigLease{}
	req := httptest.NewRequest(http.MethodDelete, "/admin/api/drives/drive-one", strings.NewReader(`{"deleteVideos":true}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "drive-one")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()

	(&AdminServer{
		Catalog: cat,
		BeginDriveConfigUpdate: func(string) (DriveConfigUpdateLease, string) {
			return lease, ""
		},
		OnPrepareDriveDelete: func(context.Context, string) error {
			return errors.New("task did not exit")
		},
		OnDriveDeleteCleanup: func(context.Context, string) (int, error) {
			t.Fatal("cleanup must not run before tasks drain")
			return 0, nil
		},
	}).handleDeleteDrive(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rr.Code, rr.Body.String())
	}
	if _, err := cat.GetDrive(ctx, "drive-one"); err != nil {
		t.Fatalf("drive was removed after drain failure: %v", err)
	}
	if lease.committedScope != 0 || !lease.released {
		t.Fatalf("delete lease committed/released = %d/%v, want 0/true", lease.committedScope, lease.released)
	}
}

func TestHandleListCrawlersOnlyIncludesCrawlerPageScripts(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	scriptPath := filepath.Join(tmp, "demo_crawler.py")
	if err := os.WriteFile(scriptPath, []byte("CRAWLER_NAME = \"Demo Crawler\"\nCRAWLER_PROTOCOL = \"crawler.v2\"\n"), 0o644); err != nil {
		t.Fatalf("write crawler script: %v", err)
	}

	for _, d := range []*catalog.Drive{
		{
			ID:     "crawler-main",
			Kind:   "scriptcrawler",
			Name:   "Crawler",
			RootID: "/",
			Credentials: map[string]string{
				"last_crawl_at":   "1800000000",
				"proxy":           " http://127.0.0.1:7890 ",
				"upload_proxy":    " socks5h://upload-proxy.example:1080 ",
				"script_path":     scriptPath,
				"upload_drive_id": "p115-target",
				"paused":          "true",
			},
			Status:        "ok",
			TeaserEnabled: false,
		},
		{
			ID:          "p115-target",
			Kind:        "p115",
			Name:        "115",
			RootID:      "0",
			Credentials: map[string]string{"cookie": "x"},
			Status:      "ok",
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
		{
			ID:          "crawler-script-deleted",
			Kind:        "scriptcrawler",
			Name:        "Deleted Script",
			RootID:      "/",
			Credentials: map[string]string{},
			Status:      "disconnected",
		},
	} {
		if err := cat.UpsertDrive(ctx, d); err != nil {
			t.Fatalf("seed drive %s: %v", d.ID, err)
		}
	}
	for _, v := range []*catalog.Video{
		{
			ID:              "scriptcrawler-crawler-main-local",
			DriveID:         "crawler-main",
			FileID:          "local.mp4",
			FileName:        "local.mp4",
			Title:           "Local",
			Size:            123,
			Ext:             "mp4",
			ThumbnailURL:    "/p/thumb/scriptcrawler-crawler-main-local",
			PreviewStatus:   "ready",
			DurationSeconds: 12,
			PublishedAt:     time.Now(),
		},
		{
			ID:              "scriptcrawler-crawler-main-migrated",
			DriveID:         "p115-target",
			FileID:          "uploaded-id",
			FileName:        "migrated.mp4",
			Title:           "Migrated",
			Size:            456,
			Ext:             "mp4",
			ThumbnailURL:    "/p/thumb/scriptcrawler-crawler-main-migrated",
			PreviewStatus:   "ready",
			DurationSeconds: 34,
			PublishedAt:     time.Now(),
		},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed crawler video %s: %v", v.ID, err)
		}
		if err := cat.UpdateVideoFingerprint(ctx, v.ID, "sha-"+v.ID, "ready", ""); err != nil {
			t.Fatalf("seed crawler fingerprint %s: %v", v.ID, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/api/crawlers", nil)
	rr := httptest.NewRecorder()
	srv := &AdminServer{Catalog: cat}
	srv.handleListCrawlers(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var got []struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		Protocol         string `json:"protocol"`
		Kind             string `json:"kind"`
		Proxy            string `json:"proxy"`
		UploadProxy      string `json:"uploadProxy"`
		UploadDriveID    string `json:"uploadDriveId"`
		Paused           bool   `json:"paused"`
		TeaserEnabled    bool   `json:"teaserEnabled"`
		LastCrawlAt      int64  `json:"lastCrawlAt"`
		TotalCrawled     int    `json:"totalCrawledCount"`
		LocalVideos      int    `json:"localVideoCount"`
		MigratedVideo    int    `json:"migratedVideoCount"`
		ThumbnailReady   int    `json:"thumbnailReadyCount"`
		TeaserReady      int    `json:"teaserReadyCount"`
		FingerprintReady int    `json:"fingerprintReadyCount"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	type crawlerListRow struct {
		Name             string
		Protocol         string
		Kind             string
		Proxy            string
		UploadProxy      string
		UploadDriveID    string
		Paused           bool
		TeaserEnabled    bool
		LastCrawlAt      int64
		TotalCrawled     int
		LocalVideos      int
		MigratedVideo    int
		ThumbnailReady   int
		TeaserReady      int
		FingerprintReady int
	}
	byID := map[string]crawlerListRow{}
	for _, d := range got {
		byID[d.ID] = crawlerListRow{
			Name:             d.Name,
			Protocol:         d.Protocol,
			Kind:             d.Kind,
			Proxy:            d.Proxy,
			UploadProxy:      d.UploadProxy,
			UploadDriveID:    d.UploadDriveID,
			Paused:           d.Paused,
			TeaserEnabled:    d.TeaserEnabled,
			LastCrawlAt:      d.LastCrawlAt,
			TotalCrawled:     d.TotalCrawled,
			LocalVideos:      d.LocalVideos,
			MigratedVideo:    d.MigratedVideo,
			ThumbnailReady:   d.ThumbnailReady,
			TeaserReady:      d.TeaserReady,
			FingerprintReady: d.FingerprintReady,
		}
	}
	if _, ok := byID["crawler-script-deleted"]; ok {
		t.Fatal("crawler without script_path should not be returned by crawler list")
	}
	if byID["crawler-main"].Kind != "scriptcrawler" {
		t.Fatalf("crawler kind = %q, want scriptcrawler", byID["crawler-main"].Kind)
	}
	if byID["crawler-main"].Name != "Demo Crawler" {
		t.Fatalf("crawler name = %q, want script metadata name", byID["crawler-main"].Name)
	}
	if byID["crawler-main"].Protocol != scriptcrawler.ProtocolV2 {
		t.Fatalf("crawler protocol = %q, want %q", byID["crawler-main"].Protocol, scriptcrawler.ProtocolV2)
	}
	if byID["crawler-main"].Proxy != "http://127.0.0.1:7890" {
		t.Fatalf("crawler proxy = %q, want trimmed proxy", byID["crawler-main"].Proxy)
	}
	if byID["crawler-main"].UploadProxy != "socks5h://upload-proxy.example:1080" {
		t.Fatalf("crawler upload proxy = %q, want trimmed proxy", byID["crawler-main"].UploadProxy)
	}
	if byID["crawler-main"].UploadDriveID != "p115-target" {
		t.Fatalf("uploadDriveId = %q, want p115-target", byID["crawler-main"].UploadDriveID)
	}
	if !byID["crawler-main"].Paused {
		t.Fatal("paused = false, want true from crawler drive")
	}
	if byID["crawler-main"].TeaserEnabled {
		t.Fatal("teaserEnabled = true, want false from crawler drive")
	}
	if byID["crawler-main"].LastCrawlAt != 1800000000 {
		t.Fatalf("lastCrawlAt = %d, want 1800000000", byID["crawler-main"].LastCrawlAt)
	}
	if byID["crawler-main"].TotalCrawled != 2 || byID["crawler-main"].LocalVideos != 1 || byID["crawler-main"].MigratedVideo != 1 {
		t.Fatalf("crawler counts = total %d local %d migrated %d, want 2/1/1", byID["crawler-main"].TotalCrawled, byID["crawler-main"].LocalVideos, byID["crawler-main"].MigratedVideo)
	}
	if byID["crawler-main"].ThumbnailReady != 2 || byID["crawler-main"].TeaserReady != 2 || byID["crawler-main"].FingerprintReady != 2 {
		t.Fatalf("asset ready counts = thumb %d teaser %d fingerprint %d, want 2/2/2", byID["crawler-main"].ThumbnailReady, byID["crawler-main"].TeaserReady, byID["crawler-main"].FingerprintReady)
	}
	if _, ok := byID["onedrive-main"]; ok {
		t.Fatal("onedrive should not be returned by crawler list")
	}

	driveReq := httptest.NewRequest(http.MethodGet, "/admin/api/drives", nil)
	driveRR := httptest.NewRecorder()
	srv.handleListDrives(driveRR, driveReq)
	if driveRR.Code != http.StatusOK {
		t.Fatalf("drive status = %d, body = %s", driveRR.Code, driveRR.Body.String())
	}
	var drives []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(driveRR.Body).Decode(&drives); err != nil {
		t.Fatalf("decode drives: %v", err)
	}
	driveIDs := map[string]bool{}
	for _, d := range drives {
		driveIDs[d.ID] = true
	}
	if driveIDs["crawler-main"] {
		t.Fatal("scriptcrawler should not be returned by drive list")
	}
}

func TestHandleUpsertCrawlerRequiresScriptPath(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	srv := &AdminServer{Catalog: cat}
	scriptPath := filepath.Join(tmp, "custom.py")
	if err := os.WriteFile(scriptPath, []byte("CRAWLER_NAME = \"Demo Crawler\"\n"), 0o644); err != nil {
		t.Fatalf("write crawler script: %v", err)
	}

	// 不再内置任何爬虫：没有脚本路径的保存请求必须被拒绝，
	// 旧的 builtin 字段也不再有"免脚本"特权。
	req := httptest.NewRequest(http.MethodPost, "/admin/api/crawlers", strings.NewReader(`{
		"id": "crawler-main",
		"builtin": "legacy",
		"scriptPath": "",
		"targetNew": "15"
	}`))
	rr := httptest.NewRecorder()
	srv.handleUpsertCrawler(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", rr.Code, rr.Body.String())
	}

	// 带脚本路径时正常保存，且请求中的 builtin 字段被忽略，不会写入凭证。
	req = httptest.NewRequest(http.MethodPost, "/admin/api/crawlers", strings.NewReader(`{
		"id": "crawler-main",
		"builtin": "legacy",
		"scriptPath": "`+scriptPath+`",
		"targetNew": "15",
		"teaserEnabled": false
	}`))
	rr = httptest.NewRecorder()
	srv.handleUpsertCrawler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	got, err := cat.GetDrive(ctx, "crawler-main")
	if err != nil {
		t.Fatalf("get crawler drive: %v", err)
	}
	if got.Kind != "scriptcrawler" || got.Credentials["builtin"] != "" {
		t.Fatalf("kind/builtin = %q/%q, want scriptcrawler with no builtin credential", got.Kind, got.Credentials["builtin"])
	}
	if got.Credentials["python_path"] != "" || got.Credentials["config_json"] != "" {
		t.Fatalf("legacy hidden credentials should not be saved: %+v", got.Credentials)
	}
	if got.Name != "Demo Crawler" {
		t.Fatalf("name = %q, want script metadata name", got.Name)
	}
	if got.Credentials["script_path"] != scriptPath {
		t.Fatalf("script_path = %q, want %q", got.Credentials["script_path"], scriptPath)
	}
	if got.TeaserEnabled {
		t.Fatal("teaserEnabled = true, want false from request")
	}
}

func TestHandleUpsertCrawlerGeneratesIDFromScriptName(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:          "crawler-my-spider",
		Kind:        scriptcrawler.Kind,
		Name:        "Existing",
		RootID:      "/",
		Credentials: map[string]string{"script_path": "/opt/crawlers/existing.py"},
	}); err != nil {
		t.Fatalf("seed crawler: %v", err)
	}
	scriptPath := filepath.Join(tmp, "custom.py")
	if err := os.WriteFile(scriptPath, []byte("CRAWLER_NAME = \"My Spider\"\n"), 0o644); err != nil {
		t.Fatalf("write crawler script: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/crawlers", strings.NewReader(`{
		"scriptPath": "`+scriptPath+`",
		"targetNew": "15"
	}`))
	rr := httptest.NewRecorder()
	(&AdminServer{Catalog: cat}).handleUpsertCrawler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		OK bool   `json:"ok"`
		ID string `json:"id"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.OK || resp.ID != "crawler-my-spider-2" {
		t.Fatalf("response = %+v, want generated suffix id", resp)
	}
	got, err := cat.GetDrive(ctx, resp.ID)
	if err != nil {
		t.Fatalf("get generated crawler: %v", err)
	}
	if got.Name != "My Spider" || got.Kind != scriptcrawler.Kind {
		t.Fatalf("generated crawler = %+v", got)
	}
}

func TestHandleUpsertCrawlerPersistsAndValidatesUploadDrive(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	scriptPath := filepath.Join(tmp, "custom.py")
	if err := os.WriteFile(scriptPath, []byte("CRAWLER_NAME = \"Upload Spider\"\n"), 0o644); err != nil {
		t.Fatalf("write crawler script: %v", err)
	}
	for _, d := range []*catalog.Drive{
		{ID: "quark-target", Kind: "quark", Name: "夸克", RootID: "0", Credentials: map[string]string{"cookie": "x"}},
		{ID: "p115-target", Kind: "p115", Name: "115", RootID: "0", Credentials: map[string]string{"cookie": "x"}},
		{ID: "wopan-target", Kind: "wopan", Name: "沃盘", RootID: "0", Credentials: map[string]string{"access_token": "a", "refresh_token": "r"}},
		{ID: "guangyapan-target", Kind: "guangyapan", Name: "光鸭", RootID: "", Credentials: map[string]string{"access_token": "a", "refresh_token": "r"}},
		{ID: "local-target", Kind: "localstorage", Name: "Local", RootID: "/", Credentials: map[string]string{"path": tmp}},
	} {
		if err := cat.UpsertDrive(ctx, d); err != nil {
			t.Fatalf("seed drive %s: %v", d.ID, err)
		}
	}
	var teaserCallbackID string
	var teaserCallbackEnabled bool
	srv := &AdminServer{
		Catalog: cat,
		OnTeaserEnabledChanged: func(id string, enabled bool) {
			teaserCallbackID = id
			teaserCallbackEnabled = enabled
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/crawlers", strings.NewReader(`{
		"id": "crawler-upload",
		"scriptPath": "`+scriptPath+`",
		"uploadDriveId": "p115-target",
		"uploadProxy": "  http://upload-proxy.example:7890  ",
		"teaserEnabled": false
	}`))
	rr := httptest.NewRecorder()
	srv.handleUpsertCrawler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got, err := cat.GetDrive(ctx, "crawler-upload")
	if err != nil {
		t.Fatalf("get crawler: %v", err)
	}
	if got.Credentials["upload_drive_id"] != "p115-target" {
		t.Fatalf("upload_drive_id = %q, want p115-target", got.Credentials["upload_drive_id"])
	}
	if got.Credentials["upload_proxy"] != "http://upload-proxy.example:7890" {
		t.Fatalf("upload_proxy = %q, want normalized proxy", got.Credentials["upload_proxy"])
	}
	if got.TeaserEnabled {
		t.Fatal("teaserEnabled = true, want false")
	}
	if teaserCallbackID != "" {
		t.Fatalf("teaser callback on create = %q, want none", teaserCallbackID)
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/api/crawlers", strings.NewReader(`{
		"id": "crawler-upload",
		"scriptPath": "`+scriptPath+`",
		"uploadDriveId": "wopan-target"
	}`))
	rr = httptest.NewRecorder()
	srv.handleUpsertCrawler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("wopan target status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got, err = cat.GetDrive(ctx, "crawler-upload")
	if err != nil {
		t.Fatalf("get crawler after wopan target: %v", err)
	}
	if got.Credentials["upload_drive_id"] != "wopan-target" {
		t.Fatalf("upload_drive_id = %q, want wopan-target", got.Credentials["upload_drive_id"])
	}
	if got.Credentials["upload_proxy"] != "http://upload-proxy.example:7890" {
		t.Fatalf("omitted upload_proxy = %q, want preserved proxy", got.Credentials["upload_proxy"])
	}
	if got.TeaserEnabled {
		t.Fatal("teaserEnabled after edit without field = true, want preserved false")
	}
	if teaserCallbackID != "" {
		t.Fatalf("teaser callback after preserved edit = %q, want none", teaserCallbackID)
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/api/crawlers", strings.NewReader(`{
		"id": "crawler-upload",
		"scriptPath": "`+scriptPath+`",
		"uploadDriveId": "guangyapan-target"
	}`))
	rr = httptest.NewRecorder()
	srv.handleUpsertCrawler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("guangyapan target status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got, err = cat.GetDrive(ctx, "crawler-upload")
	if err != nil {
		t.Fatalf("get crawler after guangyapan target: %v", err)
	}
	if got.Credentials["upload_drive_id"] != "guangyapan-target" {
		t.Fatalf("upload_drive_id = %q, want guangyapan-target", got.Credentials["upload_drive_id"])
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/api/crawlers", strings.NewReader(`{
		"id": "crawler-upload",
		"scriptPath": "`+scriptPath+`",
		"uploadDriveId": "quark-target"
	}`))
	rr = httptest.NewRecorder()
	srv.handleUpsertCrawler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("quark target status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got, err = cat.GetDrive(ctx, "crawler-upload")
	if err != nil {
		t.Fatalf("get crawler after quark target: %v", err)
	}
	if got.Credentials["upload_drive_id"] != "quark-target" {
		t.Fatalf("upload_drive_id = %q, want quark-target", got.Credentials["upload_drive_id"])
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/api/crawlers", strings.NewReader(`{
		"id": "crawler-upload",
		"scriptPath": "`+scriptPath+`",
		"uploadDriveId": "wopan-target",
		"teaserEnabled": true
	}`))
	rr = httptest.NewRecorder()
	srv.handleUpsertCrawler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("enable teaser status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got, err = cat.GetDrive(ctx, "crawler-upload")
	if err != nil {
		t.Fatalf("get crawler after teaser enable: %v", err)
	}
	if !got.TeaserEnabled {
		t.Fatal("teaserEnabled after explicit enable = false, want true")
	}
	if teaserCallbackID != "crawler-upload" || !teaserCallbackEnabled {
		t.Fatalf("teaser callback = %q/%v, want crawler-upload/true", teaserCallbackID, teaserCallbackEnabled)
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/api/crawlers", strings.NewReader(`{
		"id": "crawler-upload",
		"scriptPath": "`+scriptPath+`",
		"uploadDriveId": "local-target"
	}`))
	rr = httptest.NewRecorder()
	srv.handleUpsertCrawler(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid target status = %d, body = %s, want 400", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/api/crawlers", strings.NewReader(`{
		"id": "crawler-upload",
		"scriptPath": "`+scriptPath+`",
		"uploadDriveId": "p115-target",
		"uploadProxy": "ftp://upload-proxy.example"
	}`))
	rr = httptest.NewRecorder()
	srv.handleUpsertCrawler(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid upload proxy status = %d, body = %s, want 400", rr.Code, rr.Body.String())
	}
}

func TestHandleImportCrawlerScriptFile(t *testing.T) {
	tmp := t.TempDir()
	script := "CRAWLER_NAME = \"Demo Crawler\"\nCRAWLER_PROTOCOL = \"crawler.v2\"\nprint('crawler')\n"
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "../demo crawler.py")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write([]byte(script)); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/crawlers/import-file", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	(&AdminServer{LocalPreviewDir: filepath.Join(tmp, "previews")}).handleImportCrawlerScriptFile(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		ScriptPath string `json:"scriptPath"`
		Name       string `json:"name"`
		Protocol   string `json:"protocol"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantRoot := filepath.Join(tmp, "crawler-scripts")
	if !strings.HasPrefix(got.ScriptPath, wantRoot+string(os.PathSeparator)) {
		t.Fatalf("script path = %q, want under %q", got.ScriptPath, wantRoot)
	}
	if filepath.Ext(got.ScriptPath) != ".py" {
		t.Fatalf("script path = %q, want .py", got.ScriptPath)
	}
	if filepath.Base(got.ScriptPath) != "demo_crawler.py" {
		t.Fatalf("script filename = %q, want original sanitized filename", filepath.Base(got.ScriptPath))
	}
	data, err := os.ReadFile(got.ScriptPath)
	if err != nil {
		t.Fatalf("read imported script: %v", err)
	}
	if got.Name != "Demo Crawler" {
		t.Fatalf("name = %q, want script metadata name", got.Name)
	}
	if got.Protocol != scriptcrawler.ProtocolV2 {
		t.Fatalf("protocol = %q, want %q", got.Protocol, scriptcrawler.ProtocolV2)
	}
	if string(data) != script {
		t.Fatalf("script content = %q", string(data))
	}
}

func TestHandleImportCrawlerScriptFileDoesNotCreateCrawlerTagWithoutVideos(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	if err := cat.SetAutoGenerateTagsEnabled(ctx, false); err != nil {
		t.Fatalf("disable auto-generate tags: %v", err)
	}

	script := "CRAWLER_NAME = \"Imported Crawler\"\nprint('crawler')\n"
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "imported.py")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write([]byte(script)); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/crawlers/import-file", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	(&AdminServer{
		Catalog:         cat,
		LocalPreviewDir: filepath.Join(tmp, "previews"),
	}).handleImportCrawlerScriptFile(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	label, ok, err := cat.LookupTagLabel(ctx, "Imported Crawler")
	if err != nil {
		t.Fatalf("lookup crawler tag: %v", err)
	}
	if ok {
		t.Fatalf("lookup tag = %q/%v, want no crawler tag before any video exists", label, ok)
	}
	enabled, err := cat.AutoGenerateTagsEnabled(ctx)
	if err != nil {
		t.Fatalf("read auto-generate setting: %v", err)
	}
	if enabled {
		t.Fatal("import changed auto-generate setting to enabled")
	}
}

func TestHandleImportCrawlerScriptFileRejectsMissingName(t *testing.T) {
	tmp := t.TempDir()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "crawler.py")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write([]byte("print('crawler')\n")); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/crawlers/import-file", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	(&AdminServer{LocalPreviewDir: filepath.Join(tmp, "previews")}).handleImportCrawlerScriptFile(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "CRAWLER_NAME") {
		t.Fatalf("body = %s, want CRAWLER_NAME error", rr.Body.String())
	}
}

func TestHandleImportCrawlerScriptFileRejectsNonPython(t *testing.T) {
	tmp := t.TempDir()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "crawler.txt")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write([]byte("print('crawler')\n")); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/crawlers/import-file", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	(&AdminServer{LocalPreviewDir: filepath.Join(tmp, "previews")}).handleImportCrawlerScriptFile(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), ".py") {
		t.Fatalf("body = %s, want .py error", rr.Body.String())
	}
}

func TestHandleImportCrawlerScriptURL(t *testing.T) {
	tmp := t.TempDir()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/crawler.py" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("CRAWLER_NAME = \"URL Crawler\"\n# crawler from url\n"))
	}))
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodPost, "/admin/api/crawlers/import-url", strings.NewReader(`{
		"url": "`+upstream.URL+`/crawler.py"
	}`))
	rr := httptest.NewRecorder()
	(&AdminServer{LocalPreviewDir: filepath.Join(tmp, "previews")}).handleImportCrawlerScriptURL(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		ScriptPath string `json:"scriptPath"`
		Name       string `json:"name"`
		Protocol   string `json:"protocol"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantRoot := filepath.Join(tmp, "crawler-scripts")
	if !strings.HasPrefix(got.ScriptPath, wantRoot+string(os.PathSeparator)) {
		t.Fatalf("script path = %q, want under %q", got.ScriptPath, wantRoot)
	}
	data, err := os.ReadFile(got.ScriptPath)
	if err != nil {
		t.Fatalf("read imported script: %v", err)
	}
	if got.Name != "URL Crawler" {
		t.Fatalf("name = %q, want script metadata name", got.Name)
	}
	if got.Protocol != scriptcrawler.ProtocolV1 {
		t.Fatalf("protocol = %q, want %q", got.Protocol, scriptcrawler.ProtocolV1)
	}
	if filepath.Base(got.ScriptPath) != "crawler.py" {
		t.Fatalf("script filename = %q, want original filename", filepath.Base(got.ScriptPath))
	}
	if string(data) != "CRAWLER_NAME = \"URL Crawler\"\n# crawler from url\n" {
		t.Fatalf("script content = %q", string(data))
	}
}

func TestCrawlerScriptDownloadURLConvertsGitHubBlob(t *testing.T) {
	input, err := url.Parse("https://github.com/Just-Spider/SpiderFor91/blob/main/91Porn/91Porn.py")
	if err != nil {
		t.Fatalf("parse input: %v", err)
	}
	got := crawlerScriptDownloadURL(input)
	want := "https://raw.githubusercontent.com/Just-Spider/SpiderFor91/main/91Porn/91Porn.py"
	if got.String() != want {
		t.Fatalf("download URL = %q, want %q", got.String(), want)
	}
}

func TestCrawlerScriptDownloadURLKeepsNonGitHubURL(t *testing.T) {
	input, err := url.Parse("https://example.com/crawlers/demo.py")
	if err != nil {
		t.Fatalf("parse input: %v", err)
	}
	got := crawlerScriptDownloadURL(input)
	if got.String() != input.String() {
		t.Fatalf("download URL = %q, want original %q", got.String(), input.String())
	}
}

func TestHandleDeleteCrawlerRemovesScriptLocalVideosAndDrive(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	scriptDir := filepath.Join(tmp, "crawler-scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("mkdir script dir: %v", err)
	}
	scriptPath := filepath.Join(scriptDir, "crawler.py")
	if err := os.WriteFile(scriptPath, []byte("CRAWLER_NAME = \"Delete Me\"\n"), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:     "crawler-delete-me",
		Kind:   scriptcrawler.Kind,
		Name:   "Delete Me",
		RootID: "/",
		Credentials: map[string]string{
			"script_path": scriptPath,
			"proxy":       "http://127.0.0.1:7890",
			"target_new":  "10",
			"paused":      "true",
		},
	}); err != nil {
		t.Fatalf("seed crawler: %v", err)
	}
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "video-from-crawler",
		DriveID:     "crawler-delete-me",
		FileID:      "video.mp4",
		Title:       "Keep Me",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "migrated-video-from-crawler",
		DriveID:     "pikpak",
		FileID:      "remote-file-id",
		Title:       "Keep Migrated Video",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed migrated video: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/admin/api/crawlers/crawler-delete-me", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "crawler-delete-me")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()

	stopped := false
	removed := false
	(&AdminServer{
		Catalog:         cat,
		LocalPreviewDir: filepath.Join(tmp, "previews"),
		OnDriveDeleteCleanup: func(cleanupCtx context.Context, driveID string) (int, error) {
			if driveID != "crawler-delete-me" {
				t.Fatalf("cleanup drive = %q, want crawler-delete-me", driveID)
			}
			if err := cat.DeleteVideo(cleanupCtx, "video-from-crawler"); err != nil {
				t.Fatalf("delete local crawler video: %v", err)
			}
			return 1, nil
		},
		OnPrepareDriveDelete: func(_ context.Context, driveID string) error {
			stopped = driveID == "crawler-delete-me"
			return nil
		},
		OnDriveRemoved: func(driveID string) {
			removed = driveID == "crawler-delete-me"
		},
	}).handleDeleteCrawler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(scriptPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("script stat error = %v, want not exist", err)
	}
	if !stopped {
		t.Fatal("stop hook was not called")
	}
	if !removed {
		t.Fatal("drive removed hook was not called")
	}
	if _, err := cat.GetDrive(ctx, "crawler-delete-me"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("crawler drive lookup error = %v, want sql.ErrNoRows", err)
	}
	if _, err := cat.GetVideo(ctx, "video-from-crawler"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("local crawler video lookup error = %v, want sql.ErrNoRows", err)
	}
	if _, err := cat.GetVideo(ctx, "migrated-video-from-crawler"); err != nil {
		t.Fatalf("migrated crawler video should remain: %v", err)
	}
	var got struct {
		OK            bool `json:"ok"`
		DeletedVideos int  `json:"deletedVideos"`
		DeletedScript bool `json:"deletedScript"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK || got.DeletedVideos != 1 || !got.DeletedScript {
		t.Fatalf("response = %#v", got)
	}
}

func TestHandleSetCrawlerPausedPersistsCredential(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cat, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:          "crawler-main",
		Kind:        scriptcrawler.Kind,
		Name:        "Crawler",
		RootID:      "/",
		Credentials: map[string]string{"script_path": "/tmp/crawler.py"},
	}); err != nil {
		t.Fatalf("seed crawler: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/crawlers/crawler-main/paused", strings.NewReader(`{"paused":true}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "crawler-main")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	(&AdminServer{Catalog: cat}).handleSetCrawlerPaused(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		OK     bool `json:"ok"`
		Paused bool `json:"paused"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || !resp.Paused {
		t.Fatalf("response = %#v, want paused true", resp)
	}
	drive, err := cat.GetDrive(ctx, "crawler-main")
	if err != nil {
		t.Fatalf("get crawler: %v", err)
	}
	if drive.Credentials["paused"] != "true" {
		t.Fatalf("paused credential = %q, want true", drive.Credentials["paused"])
	}
}

func TestHandleImportCrawlerScriptURLRejectsNonPython(t *testing.T) {
	tmp := t.TempDir()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/crawler.txt" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("# crawler from url\n"))
	}))
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodPost, "/admin/api/crawlers/import-url", strings.NewReader(`{
		"url": "`+upstream.URL+`/crawler.txt"
	}`))
	rr := httptest.NewRecorder()
	(&AdminServer{LocalPreviewDir: filepath.Join(tmp, "previews")}).handleImportCrawlerScriptURL(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), ".py") {
		t.Fatalf("body = %s, want .py error", rr.Body.String())
	}
}

func TestHandleQuarkQRStartAndStatusReturnCookie(t *testing.T) {
	var pollCalls int
	var accountCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/cas/ajax/getTokenForQrcodeLogin":
			http.SetCookie(w, &http.Cookie{Name: "_UP_LOGIN", Value: "temporary", Path: "/", HttpOnly: true})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": 2000000,
				"data":   map[string]any{"members": map[string]string{"token": "quark-token"}},
			})
		case "/cas/ajax/getServiceTicketByQrcodeToken":
			pollCalls++
			if r.URL.Query().Get("token") != "quark-token" {
				t.Errorf("poll token = %q", r.URL.Query().Get("token"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": 2000000,
				"data":   map[string]any{"members": map[string]string{"service_ticket": "service-ticket"}},
			})
		case "/account/info":
			accountCalls++
			if r.URL.Query().Get("st") != "service-ticket" {
				t.Errorf("service ticket = %q", r.URL.Query().Get("st"))
			}
			http.SetCookie(w, &http.Cookie{Name: "__pus", Value: "pus-value", Path: "/", HttpOnly: true})
			http.SetCookie(w, &http.Cookie{Name: "__puus", Value: "puus-value", Path: "/", HttpOnly: true})
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "code": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	server := &AdminServer{
		QuarkQRUOPBaseURL: upstream.URL,
		QuarkQRPanBaseURL: upstream.URL,
	}
	startReq := httptest.NewRequest(http.MethodPost, "/admin/api/drives/quark/qr", nil)
	startRR := httptest.NewRecorder()
	server.handleQuarkQRStart(startRR, startReq)
	if startRR.Code != http.StatusOK {
		t.Fatalf("start status = %d, body = %s", startRR.Code, startRR.Body.String())
	}
	if startRR.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("start Cache-Control = %q", startRR.Header().Get("Cache-Control"))
	}
	var session struct {
		Token          string `json:"token"`
		QRCodeURL      string `json:"qrCodeUrl"`
		QRImageDataURL string `json:"qrImageDataUrl"`
	}
	if err := json.NewDecoder(startRR.Body).Decode(&session); err != nil {
		t.Fatalf("decode start: %v", err)
	}
	if session.Token != "quark-token" || !strings.HasPrefix(session.QRCodeURL, "https://su.quark.cn/4_eMHBJ?") {
		t.Fatalf("session = %#v", session)
	}
	if !strings.HasPrefix(session.QRImageDataURL, "data:image/png;base64,") {
		t.Fatalf("qr image = %q", session.QRImageDataURL)
	}

	statusReq := httptest.NewRequest(
		http.MethodPost,
		"/admin/api/drives/quark/qr/status",
		strings.NewReader(`{"token":"quark-token"}`),
	)
	if statusReq.URL.RawQuery != "" {
		t.Fatalf("admin status request unexpectedly contains query: %s", statusReq.URL.RawQuery)
	}
	statusRR := httptest.NewRecorder()
	server.handleQuarkQRStatus(statusRR, statusReq)
	if statusRR.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", statusRR.Code, statusRR.Body.String())
	}
	if statusRR.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status Cache-Control = %q", statusRR.Header().Get("Cache-Control"))
	}
	var status struct {
		State  string `json:"state"`
		Status int    `json:"status"`
		Cookie string `json:"cookie"`
	}
	if err := json.NewDecoder(statusRR.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.State != "success" || status.Status != 2000000 || status.Cookie != "__pus=pus-value; __puus=puus-value" {
		t.Fatalf("status response = %#v", status)
	}
	if pollCalls != 1 || accountCalls != 1 {
		t.Fatalf("upstream calls: poll=%d account=%d", pollCalls, accountCalls)
	}
}

func TestHandleQuarkQRStatusRejectsMissingToken(t *testing.T) {
	req := httptest.NewRequest(
		http.MethodPost,
		"/admin/api/drives/quark/qr/status",
		strings.NewReader(`{"token":""}`),
	)
	rr := httptest.NewRecorder()
	(&AdminServer{}).handleQuarkQRStatus(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", rr.Header().Get("Cache-Control"))
	}
}

type p115QRRoundTripFunc func(*http.Request) (*http.Response, error)

func (f p115QRRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func p115QRJSONResponse(req *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func TestHandleP115QRStart(t *testing.T) {
	client := &http.Client{Transport: p115QRRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "qrcodeapi.115.com" || req.URL.Path != "/api/1.0/web/1.0/token" {
			t.Fatalf("unexpected upstream request: %s", req.URL.String())
		}
		return p115QRJSONResponse(req, `{
			"state":1,
			"code":0,
			"data":{
				"uid":"qr-uid",
				"time":1784127027,
				"sign":"qr-sign",
				"qrcode":"https://115.com/scan/dg-qr-uid"
			}
		}`), nil
	})}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/drives/p115/qr", nil)
	rr := httptest.NewRecorder()
	(&AdminServer{P115QRHTTPClient: client}).handleP115QRStart(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", rr.Header().Get("Cache-Control"))
	}
	var got struct {
		UID            string `json:"uid"`
		Time           int64  `json:"time"`
		Sign           string `json:"sign"`
		QRCodeURL      string `json:"qrCodeUrl"`
		QRImageDataURL string `json:"qrImageDataUrl"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.UID != "qr-uid" || got.Time != 1784127027 || got.Sign != "qr-sign" || got.QRCodeURL != "https://115.com/scan/dg-qr-uid" {
		t.Fatalf("response = %#v", got)
	}
	if !strings.HasPrefix(got.QRImageDataURL, "data:image/png;base64,") {
		t.Fatalf("qr image = %q", got.QRImageDataURL)
	}
}

func TestHandleP115QRStatusUsesPOSTBodyAndReturnsCookie(t *testing.T) {
	upstreamCalls := 0
	client := &http.Client{Transport: p115QRRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		upstreamCalls++
		switch req.URL.Host + req.URL.Path {
		case "qrcodeapi.115.com/get/status/":
			if req.URL.Query().Get("uid") != "qr-uid" || req.URL.Query().Get("sign") != "qr-sign" {
				t.Fatalf("status query = %s", req.URL.RawQuery)
			}
			return p115QRJSONResponse(req, `{"state":1,"code":0,"data":{"status":2}}`), nil
		case "passportapi.115.com/app/1.0/web/1.0/login/qrcode":
			return p115QRJSONResponse(req, `{
				"state":1,
				"code":0,
				"data":{"cookie":{"UID":"user-uid","CID":"cid","SEID":"seid","KID":"kid"}}
			}`), nil
		default:
			t.Fatalf("unexpected upstream request: %s", req.URL.String())
			return nil, nil
		}
	})}

	req := httptest.NewRequest(
		http.MethodPost,
		"/admin/api/drives/p115/qr/status",
		strings.NewReader(`{"uid":"qr-uid","time":1784127027,"sign":"qr-sign"}`),
	)
	if req.URL.RawQuery != "" {
		t.Fatalf("admin status request unexpectedly contains query: %s", req.URL.RawQuery)
	}
	rr := httptest.NewRecorder()
	(&AdminServer{P115QRHTTPClient: client}).handleP115QRStatus(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", rr.Header().Get("Cache-Control"))
	}
	var got struct {
		State  string `json:"state"`
		Status int    `json:"status"`
		Cookie string `json:"cookie"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != "success" || got.Status != 2 || got.Cookie != "UID=user-uid;CID=cid;SEID=seid;KID=kid" {
		t.Fatalf("response = %#v", got)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstream calls = %d, want 2", upstreamCalls)
	}
}

func TestHandleP115QRStatusRejectsIncompleteSession(t *testing.T) {
	upstreamCalls := 0
	client := &http.Client{Transport: p115QRRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		upstreamCalls++
		return p115QRJSONResponse(req, `{}`), nil
	})}
	req := httptest.NewRequest(
		http.MethodPost,
		"/admin/api/drives/p115/qr/status",
		strings.NewReader(`{"uid":"qr-uid","time":1784127027}`),
	)
	rr := httptest.NewRecorder()
	(&AdminServer{P115QRHTTPClient: client}).handleP115QRStatus(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}
}

func TestHandleWopanQRStart(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/QRCode/generate" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"meta": map[string]string{"code": "0000", "message": "ok"},
			"result": map[string]string{
				"uuid":  "uuid-1",
				"image": "iVBORw0KGgo=",
			},
		})
	}))
	t.Cleanup(upstream.Close)

	req := httptest.NewRequest(http.MethodPost, "/admin/api/drives/wopan/qr", nil)
	rr := httptest.NewRecorder()
	(&AdminServer{WopanQRAPIBaseURL: upstream.URL + "/QRCode"}).handleWopanQRStart(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		UUID           string `json:"uuid"`
		QRImageDataURL string `json:"qrImageDataUrl"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.UUID != "uuid-1" || got.QRImageDataURL != "data:image/png;base64,iVBORw0KGgo=" {
		t.Fatalf("response = %#v", got)
	}
}

func TestHandleWopanQRStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/QRCode/query" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("uuid") != "uuid-1" {
			t.Fatalf("uuid = %q, want uuid-1", r.URL.Query().Get("uuid"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"meta": map[string]string{"code": "0000", "message": "ok"},
			"result": map[string]any{
				"state":        3,
				"token":        "access-1",
				"refreshToken": "refresh-1",
			},
		})
	}))
	t.Cleanup(upstream.Close)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/drives/wopan/qr/uuid-1", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("uuid", "uuid-1")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	(&AdminServer{WopanQRAPIBaseURL: upstream.URL + "/QRCode"}).handleWopanQRStatus(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		State        int    `json:"state"`
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != 3 || got.AccessToken != "access-1" || got.RefreshToken != "refresh-1" {
		t.Fatalf("response = %#v", got)
	}
}

func TestHandleGuangYaPanQRStart(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/auth/device/code" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["scope"] != "user" {
			t.Fatalf("scope = %#v, want user", body["scope"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":               "device-1",
			"verification_uri_complete": "https://account.guangyapan.example/device?code=abc",
			"interval":                  5,
			"expires_in":                300,
		})
	}))
	t.Cleanup(upstream.Close)

	req := httptest.NewRequest(http.MethodPost, "/admin/api/drives/guangyapan/qr", nil)
	rr := httptest.NewRecorder()
	(&AdminServer{GuangYaPanAccountBaseURL: upstream.URL}).handleGuangYaPanQRStart(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		DeviceCode     string `json:"deviceCode"`
		QRCodeURL      string `json:"qrCodeUrl"`
		QRImageDataURL string `json:"qrImageDataUrl"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.DeviceCode != "device-1" || got.QRCodeURL != "https://account.guangyapan.example/device?code=abc" {
		t.Fatalf("response = %#v", got)
	}
	if !strings.HasPrefix(got.QRImageDataURL, "data:image/png;base64,") {
		t.Fatalf("qr image = %q", got.QRImageDataURL)
	}
}

func TestHandleGuangYaPanQRStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/auth/token" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["device_code"] != "device-1" {
			t.Fatalf("device_code = %#v, want device-1", body["device_code"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-1",
			"refresh_token": "refresh-1",
			"token_type":    "Bearer",
		})
	}))
	t.Cleanup(upstream.Close)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/drives/guangyapan/qr/status?deviceCode=device-1", nil)
	rr := httptest.NewRecorder()
	(&AdminServer{GuangYaPanAccountBaseURL: upstream.URL}).handleGuangYaPanQRStatus(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		State        string `json:"state"`
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != "success" || got.AccessToken != "access-1" || got.RefreshToken != "refresh-1" {
		t.Fatalf("response = %#v", got)
	}
}

func TestHandleTestCrawlerScriptRunsImportedScript(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required for crawler script dry-run")
	}
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/video.mp4" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		if r.Header.Get("Range") == "bytes=0-0" {
			w.Header().Set("Content-Range", "bytes 0-0/2048")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte{0})
			return
		}
		_, _ = w.Write([]byte("video"))
	}))
	defer media.Close()

	script := filepath.Join(t.TempDir(), "crawler.py")
	body := `CRAWLER_NAME = "Dry Run Test"
import json
print(json.dumps({"title": "Dry Run Video", "source_id": "dry-1", "media_url": "` + media.URL + `/video.mp4", "thumbnail_url": "` + media.URL + `/thumb.jpg", "detail_url": "` + media.URL + `/detail"}))
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	reqBody, err := json.Marshal(map[string]string{
		"scriptPath": script,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/crawlers/test-script", bytes.NewReader(reqBody))
	rr := httptest.NewRecorder()
	(&AdminServer{}).handleTestCrawlerScript(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var got struct {
		OK    bool `json:"ok"`
		Items []struct {
			Title    string `json:"title"`
			SourceID string `json:"sourceId"`
			MediaURL string `json:"mediaUrl"`
		} `json:"items"`
		MediaCheck *struct {
			OK            bool   `json:"ok"`
			Status        int    `json:"status"`
			ContentType   string `json:"contentType"`
			ContentLength int64  `json:"contentLengthBytes"`
		} `json:"mediaCheck"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK {
		t.Fatalf("ok = false, body = %s", rr.Body.String())
	}
	if len(got.Items) != 1 || got.Items[0].Title != "Dry Run Video" || got.Items[0].SourceID != "dry-1" {
		t.Fatalf("items = %#v", got.Items)
	}
	if got.Items[0].MediaURL != media.URL+"/video.mp4" {
		t.Fatalf("mediaUrl = %q", got.Items[0].MediaURL)
	}
	if got.MediaCheck == nil || !got.MediaCheck.OK || got.MediaCheck.Status != http.StatusPartialContent {
		t.Fatalf("mediaCheck = %#v", got.MediaCheck)
	}
	if got.MediaCheck.ContentLength != 2048 {
		t.Fatalf("contentLength = %d, want 2048", got.MediaCheck.ContentLength)
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
					Scan:        GenerationStatus{State: "scanning", ScannedCount: 12, AddedCount: 3},
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
		ScanGenerationStatus          GenerationStatus `json:"scanGenerationStatus"`
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
		Scan                     GenerationStatus
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
			Scan                     GenerationStatus
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
			Scan:                     d.ScanGenerationStatus,
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
	if byID["OneDrive"].Scan.State != "scanning" {
		t.Fatalf("OneDrive scan status = %#v, want scanning", byID["OneDrive"].Scan)
	}
	if byID["OneDrive"].Scan.ScannedCount != 12 || byID["OneDrive"].Scan.AddedCount != 3 {
		t.Fatalf("OneDrive scan counts = %#v, want scanned=12 added=3", byID["OneDrive"].Scan)
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
	if byID["PikPak"].Scan.State != "idle" || byID["PikPak"].Thumbnail.State != "idle" || byID["PikPak"].Preview.State != "idle" || byID["PikPak"].Fingerprint.State != "idle" {
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

func TestHandleUpdateTagSavesMatchRulesAndClassifies(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID: "rule-video", DriveID: "drive", FileID: "file", Title: "custom matching phrase",
		PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if _, err := cat.CreateTagAndClassify(ctx, "展示标签", nil, "user"); err != nil {
		t.Fatalf("create tag: %v", err)
	}
	tags, err := cat.ListTags(ctx)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	var tagID int64
	for _, tag := range tags {
		if tag.Label == "展示标签" {
			tagID = tag.ID
		}
	}
	req := requestWithRouteParam(
		http.MethodPut,
		"/admin/api/tags/1",
		"id",
		strconv.FormatInt(tagID, 10),
		strings.NewReader(`{"matchRules":{"keywords":["custom matching phrase"]}}`),
	)
	rr := httptest.NewRecorder()
	(&AdminServer{Catalog: cat}).handleUpdateTag(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Tag catalog.Tag `json:"tag"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Tag.MatchRules.Keywords) != 1 || len(got.Tag.Aliases) != 0 {
		t.Fatalf("response = %#v", got)
	}
	video, err := cat.GetVideo(ctx, "rule-video")
	if err != nil || len(video.Tags) != 1 || video.Tags[0] != "展示标签" {
		t.Fatalf("classified video = %#v, %v", video, err)
	}

	req = requestWithRouteParam(
		http.MethodPut,
		"/admin/api/tags/1",
		"id",
		strconv.FormatInt(tagID, 10),
		strings.NewReader(`{"matchRules":{"keywords":["other phrase"]}}`),
	)
	rr = httptest.NewRecorder()
	(&AdminServer{Catalog: cat}).handleUpdateTag(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("second update status = %d, body = %s", rr.Code, rr.Body.String())
	}
	video, err = cat.GetVideo(ctx, "rule-video")
	if err != nil {
		t.Fatalf("get video after second update: %v", err)
	}
	if len(video.Tags) != 0 {
		t.Fatalf("tag rule deletion did not refresh video tags: %#v", video.Tags)
	}
}

func TestTagJobHandlersExposeStatus(t *testing.T) {
	server := &AdminServer{
		OnStartTagRetag: func() bool { return true },
		GetTagJobStatus: func() TagJobStatus {
			return TagJobStatus{State: "running", Running: true, Kind: "retag", Total: 10, Processed: 4}
		},
	}
	rr := httptest.NewRecorder()
	server.handleStartTagRetag(rr, httptest.NewRequest(http.MethodPost, "/admin/api/tags/retag", nil))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("retag status = %d", rr.Code)
	}

	rr = httptest.NewRecorder()
	server.handleTagJobStatus(rr, httptest.NewRequest(http.MethodGet, "/admin/api/tags/jobs/status", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"processed":4`) {
		t.Fatalf("job status = %d, %s", rr.Code, rr.Body.String())
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

func TestHandleDeleteTagAllowsBuiltinTag(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	tags, err := cat.ListTags(ctx)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	var builtinID int64
	for _, tag := range tags {
		if tag.Label == "奶子" {
			builtinID = tag.ID
			break
		}
	}
	if builtinID == 0 {
		t.Fatal("奶子 builtin tag missing")
	}
	req := requestWithRouteParam(
		http.MethodDelete,
		"/admin/api/tags/1",
		"id",
		strconv.FormatInt(builtinID, 10),
		strings.NewReader(""),
	)
	rr := httptest.NewRecorder()
	(&AdminServer{Catalog: cat}).handleDeleteTag(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
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

func TestHandleAdminListVideosTreatsUploadsAsOneSource(t *testing.T) {
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
	for _, video := range []*catalog.Video{
		{
			ID:          "uploaded-from-file",
			DriveID:     "local-upload",
			FileID:      "from-file.mp4",
			Title:       "uploaded from file",
			PublishedAt: now,
			CreatedAt:   now,
		},
		{
			ID:          "uploaded-from-link",
			DriveID:     "local-upload",
			FileID:      "from-link.mp4",
			Title:       "uploaded from link",
			PublishedAt: now.Add(-time.Second),
			CreatedAt:   now.Add(-time.Second),
		},
		{
			ID:          "cloud-video",
			DriveID:     "cloud",
			FileID:      "cloud-file",
			Title:       "cloud video",
			PublishedAt: now.Add(-2 * time.Second),
			CreatedAt:   now.Add(-2 * time.Second),
		},
	} {
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatalf("seed %s: %v", video.ID, err)
		}
	}

	req := httptest.NewRequest(
		http.MethodGet,
		"/admin/api/videos?driveId=local-upload",
		nil,
	)
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
	if got.Total != 2 || len(got.Items) != 2 {
		t.Fatalf("response total/items = %d/%d, want both uploads: %#v", got.Total, len(got.Items), got.Items)
	}
	for _, item := range got.Items {
		if item.DriveID != "local-upload" {
			t.Fatalf("unexpected non-upload item: %#v", item)
		}
	}
}

func TestHandleAdminListVideosAppliesAdvancedFilters(t *testing.T) {
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

	seed := func(id, driveID string, createdAt, publishedAt time.Time, durationSeconds int) {
		t.Helper()
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID:              id,
			DriveID:         driveID,
			FileID:          "file-" + id,
			Title:           id,
			DurationSeconds: durationSeconds,
			PublishedAt:     publishedAt,
			CreatedAt:       createdAt,
			UpdatedAt:       createdAt,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed(
		"scriptcrawler-crawler-a-source-1",
		"cloud-a",
		time.Date(2026, time.July, 10, 23, 30, 0, 0, time.Local),
		time.Date(2026, time.June, 1, 23, 30, 0, 0, time.Local),
		5*60,
	)
	seed(
		"scriptcrawler-crawler-a-source-2",
		"cloud-a",
		time.Date(2026, time.July, 11, 0, 30, 0, 0, time.Local),
		time.Date(2026, time.June, 2, 0, 30, 0, 0, time.Local),
		15*60,
	)
	seed(
		"scriptcrawler-crawler-b-source-3",
		"cloud-b",
		time.Date(2026, time.July, 10, 12, 0, 0, 0, time.Local),
		time.Date(2026, time.June, 1, 12, 0, 0, 0, time.Local),
		5*60,
	)
	for _, source := range []struct {
		crawlerID string
		sourceID  string
		videoID   string
	}{
		{"crawler-a", "source-1", "scriptcrawler-crawler-a-source-1"},
		{"crawler-a", "source-2", "scriptcrawler-crawler-a-source-2"},
		{"crawler-b", "source-3", "scriptcrawler-crawler-b-source-3"},
	} {
		if err := cat.MarkCrawlerSourceSeen(ctx, "scriptcrawler", source.crawlerID, source.sourceID, "imported", source.videoID, "", 0); err != nil {
			t.Fatalf("mark crawler source %s: %v", source.sourceID, err)
		}
	}

	req := httptest.NewRequest(
		http.MethodGet,
		"/admin/api/videos?driveId=cloud-a&crawlerId=crawler-a&createdFrom=2026-07-10&createdTo=2026-07-10&durationMinMinutes=4&durationMaxMinutes=6",
		nil,
	)
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
	if got.Total != 1 || len(got.Items) != 1 || got.Items[0].ID != "scriptcrawler-crawler-a-source-1" {
		t.Fatalf("filtered response = total %d items %#v, want source-1", got.Total, got.Items)
	}
}

func TestHandleAdminListVideosRejectsInvalidAdvancedRanges(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	tomorrow := time.Now().In(time.Local).AddDate(0, 0, 1).Format(adminVideoDateLayout)
	for _, target := range []string{
		"/admin/api/videos?createdFrom=2026/07/10",
		"/admin/api/videos?createdFrom=" + tomorrow,
		"/admin/api/videos?createdTo=" + tomorrow,
		"/admin/api/videos?durationMinMinutes=10&durationMaxMinutes=5",
		"/admin/api/videos?durationMinMinutes=1.5",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rr := httptest.NewRecorder()
		(&AdminServer{Catalog: cat}).handleAdminListVideos(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, body = %s", target, rr.Code, rr.Body.String())
		}
	}
}

func TestHandleAdminListVideosDoesNotExposeCategory(t *testing.T) {
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
	if _, err := cat.EnsureTag(ctx, "source-tag", "user"); err != nil {
		t.Fatalf("ensure source tag: %v", err)
	}
	if _, err := cat.AddVideoTagAssignments(ctx, "video-1", []catalog.TagAssignment{{
		Label: "source-tag", Source: "crawler", Evidence: "脚本标签",
	}}); err != nil {
		t.Fatalf("assign source tag: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/api/videos", nil)
	rr := httptest.NewRecorder()
	(&AdminServer{Catalog: cat}).handleAdminListVideos(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(got.Items))
	}
	if _, ok := got.Items[0]["category"]; ok {
		t.Fatalf("admin video response exposed category: %#v", got.Items[0])
	}
	sources, ok := got.Items[0]["tagSources"].(map[string]any)
	if !ok || sources["source-tag"] != "crawler" {
		t.Fatalf("tag sources = %#v", got.Items[0]["tagSources"])
	}
	evidence, ok := got.Items[0]["tagEvidence"].(map[string]any)
	if !ok || evidence["source-tag"] != "脚本标签" {
		t.Fatalf("tag evidence = %#v", got.Items[0]["tagEvidence"])
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

func TestHandleAdminListVideosMarksActivePreviewGeneration(t *testing.T) {
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
	for _, v := range []*catalog.Video{
		{
			ID:            "active-video",
			DriveID:       "OneDrive",
			FileID:        "active-file",
			Title:         "Active video",
			PreviewStatus: "ready",
			PublishedAt:   now,
			CreatedAt:     now,
			UpdatedAt:     now,
		},
		{
			ID:            "idle-video",
			DriveID:       "OneDrive",
			FileID:        "idle-file",
			Title:         "Idle video",
			PreviewStatus: "ready",
			PublishedAt:   now.Add(-time.Hour),
			CreatedAt:     now,
			UpdatedAt:     now,
		},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/api/videos?driveId=OneDrive", nil)
	rr := httptest.NewRecorder()
	(&AdminServer{
		Catalog: cat,
		GetPreviewGenerationVideoIDs: func() map[string]bool {
			return map[string]bool{"active-video": true}
		},
	}).handleAdminListVideos(rr, req)

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
	if got.Total != 2 || len(got.Items) != 2 {
		t.Fatalf("response total/items = %d/%d, want 2/2", got.Total, len(got.Items))
	}
	statusByID := map[string]string{}
	for _, item := range got.Items {
		statusByID[item.ID] = item.PreviewStatus
	}
	if statusByID["active-video"] != "generating" {
		t.Fatalf("active status = %q, want generating", statusByID["active-video"])
	}
	if statusByID["idle-video"] != "ready" {
		t.Fatalf("idle status = %q, want ready", statusByID["idle-video"])
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
