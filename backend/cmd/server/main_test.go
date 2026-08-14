package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/video-site/backend/internal/api"
	"github.com/video-site/backend/internal/auth"
	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/config"
	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/drives/scriptcrawler"
	"github.com/video-site/backend/internal/fingerprint"
	"github.com/video-site/backend/internal/mediaasset"
	"github.com/video-site/backend/internal/preview"
	"github.com/video-site/backend/internal/proxy"
)

func TestHashPasswordCommandProducesBcryptHash(t *testing.T) {
	var out bytes.Buffer
	if err := runHashPasswordCommand(strings.NewReader("secret123"), &out); err != nil {
		t.Fatalf("hash password: %v", err)
	}
	hash := strings.TrimSpace(out.String())
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("secret123")); err != nil {
		t.Fatalf("hash does not verify: %v", err)
	}
}

func TestLoadApplicationConfigSeparatesFileAndRuntimeStoragePaths(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(`
storage:
  db_path: "./data/video-site.db"
  local_preview_dir: "./data/previews"
logging:
  directory: "./data/logs"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	workingDir := t.TempDir()

	fileConfig, runtimeConfig, err := loadApplicationConfig(configPath, workingDir)
	if err != nil {
		t.Fatal(err)
	}
	if fileConfig.Storage.DBPath != "./data/video-site.db" ||
		fileConfig.Storage.LocalPreviewDir != "./data/previews" {
		t.Fatalf("file storage paths changed: %+v", fileConfig.Storage)
	}
	if fileConfig.Logging.Directory != "./data/logs" {
		t.Fatalf("file logging path changed: %+v", fileConfig.Logging)
	}
	if runtimeConfig.Storage.DBPath != filepath.Join(workingDir, "data", "video-site.db") ||
		runtimeConfig.Storage.LocalPreviewDir != filepath.Join(workingDir, "data", "previews") {
		t.Fatalf("runtime storage paths = %+v", runtimeConfig.Storage)
	}
	if runtimeConfig.Logging.Directory != filepath.Join(workingDir, "data", "logs") {
		t.Fatalf("runtime logging path = %+v", runtimeConfig.Logging)
	}
}

func TestGuangYaPanLegacyRootPath(t *testing.T) {
	credentials := map[string]string{"root_path": "  影视/电影  "}
	if got := guangYaPanLegacyRootPath("", credentials); got != "影视/电影" {
		t.Fatalf("legacy root path = %q", got)
	}
	if got := guangYaPanLegacyRootPath("folder-id", credentials); got != "" {
		t.Fatalf("root ID should take precedence, legacy path = %q", got)
	}
}

func TestPersistDriveCredentialsPreservesSkipDirsSavedAfterAttach(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:         "pikpak-main",
		Kind:       "pikpak",
		Name:       "PikPak",
		RootID:     "root",
		SkipDirIDs: []string{"old-dir"},
		Credentials: map[string]string{
			"access_token":  "old-access",
			"refresh_token": "old-refresh",
		},
		Status: "ok",
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}
	if err := cat.SetDriveSkipDirIDs(ctx, "pikpak-main", []string{"latest-dir"}); err != nil {
		t.Fatalf("save skip dirs: %v", err)
	}

	app := &App{cat: cat}
	app.persistDriveCredentials("pikpak-main", map[string]string{
		"access_token":  "new-access",
		"refresh_token": "new-refresh",
	})

	got, err := cat.GetDrive(ctx, "pikpak-main")
	if err != nil {
		t.Fatalf("get drive: %v", err)
	}
	if len(got.SkipDirIDs) != 1 || got.SkipDirIDs[0] != "latest-dir" {
		t.Fatalf("skip dir ids = %#v, want latest setting", got.SkipDirIDs)
	}
	if got.Credentials["access_token"] != "new-access" || got.Credentials["refresh_token"] != "new-refresh" {
		t.Fatalf("credentials = %#v, want refreshed tokens", got.Credentials)
	}
}

func TestListDriveDirChildrenPersistsFailureAndRecovery(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:     "drive-id",
		Kind:   "fake",
		Name:   "Fake Drive",
		RootID: "root",
		Status: "ok",
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	drv := &serverListResultDrive{err: errors.New("token expired")}
	registry := proxy.NewRegistry()
	registry.Set("drive-id", drv)
	app := &App{cat: cat, registry: registry}

	if _, err := app.listDriveDirChildren(ctx, "drive-id", ""); err == nil {
		t.Fatal("list directory succeeded, want provider error")
	}
	got, err := cat.GetDrive(ctx, "drive-id")
	if err != nil {
		t.Fatalf("get drive after failure: %v", err)
	}
	if got.Status != "error" || !strings.Contains(got.LastError, "token expired") {
		t.Fatalf("status=%q lastError=%q, want error containing provider failure", got.Status, got.LastError)
	}

	drv.err = nil
	drv.entries = []drives.Entry{
		{ID: "folder-id", Name: "Movies", IsDir: true},
		{ID: "file-id", Name: "clip.mp4", IsDir: false},
	}
	children, err := app.listDriveDirChildren(ctx, "drive-id", "")
	if err != nil {
		t.Fatalf("list directory after recovery: %v", err)
	}
	if len(children) != 1 || children[0].ID != "folder-id" {
		t.Fatalf("children = %#v, want only the directory", children)
	}
	got, err = cat.GetDrive(ctx, "drive-id")
	if err != nil {
		t.Fatalf("get drive after recovery: %v", err)
	}
	if got.Status != "ok" || got.LastError != "" {
		t.Fatalf("status=%q lastError=%q, want recovered ok status", got.Status, got.LastError)
	}
}

func TestListDriveDirChildrenPreservesOriginalAttachFailure(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	const attachError = "pikpak error_code=4126 error=invalid_grant description=AccessProhibited"
	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:        "pikpak-main",
		Kind:      "pikpak",
		Name:      "PikPak",
		RootID:    "",
		Status:    "error",
		LastError: attachError,
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	app := &App{cat: cat, registry: proxy.NewRegistry()}
	if _, err := app.listDriveDirChildren(ctx, "pikpak-main", ""); err == nil || !strings.Contains(err.Error(), attachError) {
		t.Fatalf("dirtree error = %v, want original attach failure", err)
	}

	got, err := cat.GetDrive(ctx, "pikpak-main")
	if err != nil {
		t.Fatalf("get drive: %v", err)
	}
	if got.Status != "error" || got.LastError != attachError {
		t.Fatalf("status=%q lastError=%q, want original attach failure preserved", got.Status, got.LastError)
	}
}

func TestListDriveDirChildrenRecordsMissingAttachWithoutOriginalFailure(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:     "drive-id",
		Kind:   "pikpak",
		Name:   "PikPak",
		RootID: "",
		Status: "disconnected",
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	app := &App{cat: cat, registry: proxy.NewRegistry()}
	if _, err := app.listDriveDirChildren(ctx, "drive-id", ""); err == nil {
		t.Fatal("list directory succeeded, want missing-attach error")
	}

	got, err := cat.GetDrive(ctx, "drive-id")
	if err != nil {
		t.Fatalf("get drive: %v", err)
	}
	if got.Status != "error" || !strings.Contains(got.LastError, "drive drive-id not attached") {
		t.Fatalf("status=%q lastError=%q, want missing-attach failure recorded", got.Status, got.LastError)
	}
}

func TestEnsureConfigAdminUserMigratesCustomConfigAdmin(t *testing.T) {
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

	cfg := &config.Config{}
	cfg.Server.Admin.Username = "owner"
	cfg.Server.Admin.Password = "secret123"

	if err := ensureConfigAdminUser(ctx, cat, cfg); err != nil {
		t.Fatalf("ensure config admin: %v", err)
	}
	u, err := cat.GetUserByUsername(ctx, "owner")
	if err != nil {
		t.Fatalf("get migrated user: %v", err)
	}
	if u.Role != "admin" {
		t.Fatalf("role = %q, want admin", u.Role)
	}

	authr := &auth.Authenticator{Catalog: cat}
	role, err := authr.UserLogin(httptest.NewRecorder(), httptest.NewRequest("POST", "/admin/api/login", nil), "owner", "secret123")
	if err != nil {
		t.Fatalf("login migrated user: %v", err)
	}
	if role != "admin" {
		t.Fatalf("role = %q, want admin", role)
	}
}

func TestRegisterPreviewWorkerBackfillsPendingWhenDriveTeaserEnabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	seedDriveWithTeaser(t, cat, "drive-id", true)
	video := &catalog.Video{
		ID:            "video-1",
		DriveID:       "drive-id",
		FileID:        "file-id",
		Title:         "Clip",
		PreviewStatus: "pending",
		PublishedAt:   time.Now(),
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	app := &App{
		cat:          cat,
		workers:      make(map[string]*preview.Worker),
		thumbWorkers: make(map[string]*preview.ThumbWorker),
	}
	worker := preview.NewWorker(&serverFakeTeaserGenerator{}, cat, &serverFakeDrive{})
	go worker.Run(ctx)

	app.registerPreviewWorkers(ctx, "drive-id", worker, nil, nil, func() {})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := cat.GetVideo(ctx, video.ID)
		if err != nil {
			t.Fatalf("get video: %v", err)
		}
		if got.PreviewStatus == "ready" {
			if got.PreviewLocal != "/tmp/video-1.mp4" {
				t.Fatalf("preview local = %q, want generated local teaser path", got.PreviewLocal)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video after timeout: %v", err)
	}
	t.Fatalf("preview status = %q, want ready", got.PreviewStatus)
}

func TestRegisterPreviewWorkersRunThumbnailsAndPreviewsIndependently(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	seedDriveWithTeaser(t, cat, "drive-id", true)
	now := time.Now()
	video := &catalog.Video{
		ID:            "video-1",
		DriveID:       "drive-id",
		FileID:        "file-1",
		Title:         "Clip 1",
		PreviewStatus: "pending",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	app := &App{
		cat:          cat,
		workers:      make(map[string]*preview.Worker),
		thumbWorkers: make(map[string]*preview.ThumbWorker),
	}
	gen := &serverBlockingThumbGenerator{
		started: make(chan string, 1),
		release: make(chan struct{}),
	}
	drv := &serverFakeDrive{}
	worker := preview.NewWorker(gen, cat, drv)
	thumbWorker := preview.NewThumbWorker(gen, cat, drv)
	go worker.Run(ctx)
	go thumbWorker.Run(ctx)

	app.registerPreviewWorkers(ctx, "drive-id", worker, thumbWorker, nil, func() {})

	select {
	case got := <-gen.started:
		if got != video.ID {
			t.Fatalf("thumbnail started for %q, want %q", got, video.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("thumbnail generation did not start")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := cat.GetVideo(ctx, video.ID)
		if err != nil {
			t.Fatalf("get video: %v", err)
		}
		if got.PreviewStatus == "ready" {
			if got.ThumbnailURL != "" {
				t.Fatalf("thumbnail url = %q, want preview ready while thumbnail is still blocked", got.ThumbnailURL)
			}
			close(gen.release)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video after timeout: %v", err)
	}
	t.Fatalf("preview status=%q thumbnail=%q, want preview ready before thumbnail finishes", got.PreviewStatus, got.ThumbnailURL)
}

func TestRegisterPreviewWorkersBackfillsHistoricalFingerprints(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	dataPath := filepath.Join(t.TempDir(), "video.mp4")
	data := []byte("historical video content for fingerprint")
	if err := os.WriteFile(dataPath, data, 0o644); err != nil {
		t.Fatalf("write video data: %v", err)
	}

	now := time.Now()
	video := &catalog.Video{
		ID:                "historical-video",
		DriveID:           "drive-id",
		FileID:            "file-id",
		Title:             "Historical",
		Size:              int64(len(data)),
		FingerprintStatus: "pending",
		PublishedAt:       now,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	app := &App{
		cat:                cat,
		workers:            make(map[string]*preview.Worker),
		thumbWorkers:       make(map[string]*preview.ThumbWorker),
		fingerprintWorkers: make(map[string]*fingerprint.Worker),
	}
	drv := &serverFingerprintFakeDrive{path: dataPath}
	fingerprintWorker := fingerprint.NewWorker(cat, drv, fingerprint.Config{})
	go fingerprintWorker.Run(ctx)

	app.registerPreviewWorkers(ctx, "drive-id", nil, nil, fingerprintWorker, func() {})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := cat.GetVideo(ctx, video.ID)
		if err != nil {
			t.Fatalf("get video: %v", err)
		}
		if got.SampledSHA256 != "" && got.FingerprintStatus == "ready" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video after timeout: %v", err)
	}
	t.Fatalf("fingerprint status=%q sampled=%q, want ready with hash", got.FingerprintStatus, got.SampledSHA256)
}

func TestUpdateScriptCrawlerRunStatePreservesCurrentTeaserSwitch(t *testing.T) {
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
		ID:     "crawler-id",
		Kind:   scriptcrawler.Kind,
		Name:   "Crawler",
		RootID: "/",
		Credentials: map[string]string{
			"script_path": "/tmp/crawler.py",
			"target_new":  "10",
		},
		TeaserEnabled: false,
	}); err != nil {
		t.Fatalf("seed crawler drive: %v", err)
	}
	if err := cat.SetDriveTeaserEnabled(ctx, "crawler-id", true); err != nil {
		t.Fatalf("toggle teaser: %v", err)
	}

	app := &App{cat: cat}
	if err := app.updateScriptCrawlerRunState(ctx, "crawler-id", nil); err != nil {
		t.Fatalf("update run state: %v", err)
	}
	got, err := cat.GetDrive(ctx, "crawler-id")
	if err != nil {
		t.Fatalf("get crawler drive: %v", err)
	}
	if !got.TeaserEnabled {
		t.Fatal("teaserEnabled = false after run state update, want preserved true")
	}
	if got.Status != "ok" || got.LastError != "" {
		t.Fatalf("status=%q lastError=%q, want ok with no error", got.Status, got.LastError)
	}
	if got.Credentials["last_crawl_at"] == "" || got.Credentials["target_new"] != "10" {
		t.Fatalf("credentials after run state update = %#v", got.Credentials)
	}
}

func TestStopDriveTasksCancelsQueuedTasksAndReplacesWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	drv := &serverFakeDrive{}
	registry := proxy.NewRegistry()
	registry.Set("drive-id", drv)

	gen := &serverFakeTeaserGenerator{}
	oldWorker := preview.NewWorker(gen, cat, drv)
	oldThumbWorker := preview.NewThumbWorker(gen, cat, drv)
	oldFingerprintWorker := fingerprint.NewWorker(cat, drv, fingerprint.Config{})
	oldCanceled := make(chan struct{})

	app := &App{
		cfg:                &config.Config{},
		cat:                cat,
		registry:           registry,
		workers:            map[string]*preview.Worker{"drive-id": oldWorker},
		thumbWorkers:       map[string]*preview.ThumbWorker{"drive-id": oldThumbWorker},
		fingerprintWorkers: map[string]*fingerprint.Worker{"drive-id": oldFingerprintWorker},
		cancels: map[string]context.CancelFunc{
			"drive-id": func() { close(oldCanceled) },
		},
		scanQueued:          map[string]bool{"drive-id": true},
		scanProgress:        map[string]driveScanProgress{"drive-id": {Scanned: 8, Added: 2}},
		fingerprintQueueing: map[string]bool{"drive-id": true},
	}
	taskCtx, done := app.registerDriveTaskContext(ctx, "drive-id")
	defer done()

	if !app.stopDriveTasks(ctx, "drive-id") {
		t.Fatal("stopDriveTasks returned false, want true")
	}
	select {
	case <-oldCanceled:
	case <-time.After(time.Second):
		t.Fatal("old worker cancel was not called")
	}
	if err := taskCtx.Err(); err == nil {
		t.Fatal("registered drive task context was not canceled")
	}
	if app.scanQueued["drive-id"] {
		t.Fatal("scan queue marker was not cleared")
	}
	if _, ok := app.scanProgress["drive-id"]; ok {
		t.Fatal("scan progress marker was not cleared")
	}
	if app.fingerprintQueueing["drive-id"] {
		t.Fatal("fingerprint queue marker was not cleared")
	}

	app.mu.Lock()
	newWorker := app.workers["drive-id"]
	newThumbWorker := app.thumbWorkers["drive-id"]
	newFingerprintWorker := app.fingerprintWorkers["drive-id"]
	newCancel := app.cancels["drive-id"]
	app.mu.Unlock()
	if newWorker == nil || newWorker == oldWorker {
		t.Fatalf("preview worker was not replaced")
	}
	if newThumbWorker == nil || newThumbWorker == oldThumbWorker {
		t.Fatalf("thumb worker was not replaced")
	}
	if newFingerprintWorker == nil || newFingerprintWorker == oldFingerprintWorker {
		t.Fatalf("fingerprint worker was not replaced")
	}
	if newCancel == nil {
		t.Fatalf("replacement worker cancel was not registered")
	}
	newCancel()
}

func TestScheduleScanRejectsDriveWithActiveGenerationWork(t *testing.T) {
	ctx := context.Background()
	thumbWorker := preview.NewThumbWorker(&serverFakeTeaserGenerator{}, nil, &serverFakeDrive{})
	if !thumbWorker.Enqueue(&catalog.Video{ID: "busy-video", DriveID: "drive-id", Title: "Busy Video"}) {
		t.Fatal("failed to enqueue busy thumbnail task")
	}
	app := &App{
		thumbWorkers: map[string]*preview.ThumbWorker{"drive-id": thumbWorker},
	}

	if app.scheduleScan(ctx, "drive-id") {
		t.Fatal("scheduleScan accepted a drive with active generation work")
	}
}

func TestScheduleScanRunsDifferentDrivesConcurrently(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	seedDriveWithTeaser(t, cat, "drive-a", true)
	seedDriveWithTeaser(t, cat, "drive-b", true)

	started := make(chan string, 2)
	release := make(chan struct{})
	registry := proxy.NewRegistry()
	registry.Set("drive-a", &serverBlockingListDrive{id: "drive-a", started: started, release: release})
	registry.Set("drive-b", &serverBlockingListDrive{id: "drive-b", started: started, release: release})

	app := &App{
		cfg: &config.Config{
			Scanner: config.Scanner{VideoExtensions: []string{".mp4"}},
		},
		cat:      cat,
		registry: registry,
	}

	if !app.scheduleScan(ctx, "drive-a") {
		t.Fatal("scheduleScan drive-a was rejected")
	}
	if !app.scheduleScan(ctx, "drive-b") {
		t.Fatal("scheduleScan drive-b was rejected")
	}

	seen := map[string]struct{}{}
	deadline := time.After(time.Second)
	for len(seen) < 2 {
		select {
		case id := <-started:
			seen[id] = struct{}{}
		case <-deadline:
			close(release)
			t.Fatalf("started drives = %#v, want both drives before releasing List", seen)
		}
	}
	close(release)
}

func TestDriveGenerationStatusIncludesScanState(t *testing.T) {
	app := &App{
		scanQueued:   map[string]bool{"drive-id": true},
		scanProgress: map[string]driveScanProgress{"drive-id": {Scanned: 12, Added: 3}},
	}

	status := app.driveGenerationStatuses()["drive-id"].Scan
	if status.State != "scanning" {
		t.Fatalf("scan status = %#v, want scanning", status)
	}
	if status.ScannedCount != 12 || status.AddedCount != 3 {
		t.Fatalf("scan counts = scanned %d added %d, want 12 and 3", status.ScannedCount, status.AddedCount)
	}
}

func TestDriveGenerationStatusIncludesScanCooldown(t *testing.T) {
	until := time.Now().Add(time.Hour).Round(time.Second)
	app := &App{
		scanQueued: map[string]bool{"drive-id": true},
		scanProgress: map[string]driveScanProgress{
			"drive-id": {Scanned: 12, Added: 3, CooldownUntil: until},
		},
	}

	status := app.driveGenerationStatuses()["drive-id"].Scan
	if status.State != "cooling" {
		t.Fatalf("scan status = %#v, want cooling", status)
	}
	if status.CooldownUntil != until.Format(time.RFC3339) {
		t.Fatalf("cooldown until = %q, want %q", status.CooldownUntil, until.Format(time.RFC3339))
	}
}

func TestGuangYaPanGenerationCooldowns(t *testing.T) {
	drv := &serverFakeKindDrive{id: "gy", kind: "guangyapan"}
	if got := generationCooldownForDrive(drv); got != 10*time.Minute {
		t.Fatalf("generation cooldown = %s, want 10m", got)
	}
	if got := fingerprintConfigForDrive(drv).RateLimitCooldown; got != 10*time.Minute {
		t.Fatalf("fingerprint cooldown = %s, want 10m", got)
	}
	if got := scanCooldownForDrive(drv); got != 10*time.Minute {
		t.Fatalf("scan cooldown = %s, want 10m", got)
	}
}

func TestRunCrawlerMigrationAfterManualCrawlRequiresCrawlerUploadTarget(t *testing.T) {
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
		ID:     "crawler-main",
		Kind:   scriptcrawler.Kind,
		Name:   "Crawler",
		RootID: "/",
		Credentials: map[string]string{
			"script_path": "/tmp/crawler.py",
		},
	}); err != nil {
		t.Fatalf("seed crawler: %v", err)
	}

	registry := proxy.NewRegistry()
	migrator := &serverFakeCrawlerUploadRunner{}
	app := &App{
		cat:                cat,
		registry:           registry,
		crawlerUploader:    migrator,
		workers:            map[string]*preview.Worker{},
		thumbWorkers:       map[string]*preview.ThumbWorker{},
		fingerprintWorkers: map[string]*fingerprint.Worker{},
	}

	app.runCrawlerMigrationAfterManualCrawl(ctx, "crawler-main")
	if migrator.called.Load() != 0 {
		t.Fatalf("migration called without upload target")
	}

	d, err := cat.GetDrive(ctx, "crawler-main")
	if err != nil {
		t.Fatalf("get crawler: %v", err)
	}
	d.Credentials["upload_drive_id"] = "pikpak"
	if err := cat.UpsertDrive(ctx, d); err != nil {
		t.Fatalf("set upload target: %v", err)
	}
	app.runCrawlerMigrationAfterManualCrawl(ctx, "crawler-main")
	if migrator.called.Load() != 1 {
		t.Fatalf("migration calls = %d, want 1", migrator.called.Load())
	}
	if got := migrator.lastDriveID(); got != "crawler-main" {
		t.Fatalf("post-crawl migration drive = %q, want crawler-main", got)
	}
}

func TestReloadSavedCrawlerDoesNotStartUploadMigration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	cat, err := catalog.Open(filepath.Join(root, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	scriptPath := filepath.Join(root, "crawler.py")
	if err := os.WriteFile(scriptPath, []byte("CRAWLER_NAME = \"Saved Crawler\"\n"), 0o644); err != nil {
		t.Fatalf("write crawler script: %v", err)
	}
	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:     "crawler-saved",
		Kind:   scriptcrawler.Kind,
		Name:   "Saved Crawler",
		RootID: "/",
		Credentials: map[string]string{
			"script_path":     scriptPath,
			"upload_drive_id": "pikpak-target",
		},
		TeaserEnabled: true,
	}); err != nil {
		t.Fatalf("seed crawler: %v", err)
	}

	migrator := &serverFakeCrawlerUploadRunner{}
	app := &App{
		cfg: &config.Config{
			Storage: config.Storage{LocalPreviewDir: filepath.Join(root, "previews")},
		},
		cat:             cat,
		registry:        proxy.NewRegistry(),
		scriptCrawlers:  make(map[string]*scriptcrawler.Crawler),
		crawlerUploader: migrator,
	}
	if err := app.reloadSavedDrive(ctx, "crawler-saved"); err != nil {
		t.Fatalf("reload saved crawler: %v", err)
	}
	if _, ok := app.registry.Get("crawler-saved"); !ok {
		t.Fatal("saved crawler was not reattached")
	}

	// The old save hook scheduled migration asynchronously. Allow enough time
	// for such a regression to reach the fake runner before asserting.
	time.Sleep(100 * time.Millisecond)
	if migrator.called.Load() != 0 {
		t.Fatalf("saving crawler started %d upload migration(s), want 0", migrator.called.Load())
	}
}

func TestScheduleManualCrawlerUploadMigrationRunsWhenAssetsReady(t *testing.T) {
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
		ID:            "crawler-ready",
		Kind:          scriptcrawler.Kind,
		Name:          "Ready Crawler",
		RootID:        "/",
		TeaserEnabled: true,
		Credentials: map[string]string{
			"script_path":     "/tmp/ready.py",
			"upload_drive_id": "pikpak-target",
		},
	}); err != nil {
		t.Fatalf("seed crawler: %v", err)
	}
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:                scriptcrawler.BuildVideoID("crawler-ready", "source-1"),
		DriveID:           "crawler-ready",
		FileID:            "source-1.mp4",
		FileName:          "source-1.mp4",
		Title:             "Source 1",
		Size:              123,
		Ext:               "mp4",
		SampledSHA256:     "sampled-source-1",
		FingerprintStatus: "ready",
		PreviewStatus:     "ready",
		PublishedAt:       time.Now(),
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	registry := proxy.NewRegistry()
	registry.Set("crawler-ready", &serverFakeKindDrive{id: "crawler-ready", kind: scriptcrawler.Kind})
	registry.Set("pikpak-target", &serverFakeKindDrive{id: "pikpak-target", kind: "pikpak"})
	migrator := &serverFakeCrawlerUploadRunner{}
	app := &App{
		cat:                cat,
		registry:           registry,
		crawlerUploader:    migrator,
		workers:            map[string]*preview.Worker{},
		thumbWorkers:       map[string]*preview.ThumbWorker{},
		fingerprintWorkers: map[string]*fingerprint.Worker{},
	}

	accepted, message := app.scheduleManualCrawlerUploadMigration(ctx, "crawler-ready")
	if !accepted {
		t.Fatalf("accepted = false, message = %q", message)
	}
	deadline := time.After(time.Second)
	for migrator.called.Load() == 0 {
		select {
		case <-deadline:
			t.Fatalf("migration calls = %d, want 1", migrator.called.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if got := migrator.lastDriveID(); got != "crawler-ready" {
		t.Fatalf("migration drive = %q, want crawler-ready", got)
	}

	deadline = time.After(time.Second)
	for app.driveHasActiveWork("crawler-ready") {
		select {
		case <-deadline:
			t.Fatal("manual upload task did not finish")
		case <-time.After(10 * time.Millisecond):
		}
	}
	migrator.rejectStart.Store(true)
	accepted, message = app.scheduleManualCrawlerUploadMigration(ctx, "crawler-ready")
	if accepted {
		t.Fatal("accepted = true while global uploader is busy")
	}
	if !strings.Contains(message, "其他爬虫上传任务") {
		t.Fatalf("message = %q, want global upload busy reason", message)
	}
}

func TestScheduleManualCrawlerUploadMigrationRejectsPendingFingerprint(t *testing.T) {
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
		ID:            "crawler-pending",
		Kind:          scriptcrawler.Kind,
		Name:          "Pending Crawler",
		RootID:        "/",
		TeaserEnabled: true,
		Credentials: map[string]string{
			"script_path":     "/tmp/pending.py",
			"upload_drive_id": "pikpak-target",
		},
	}); err != nil {
		t.Fatalf("seed crawler: %v", err)
	}
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:            scriptcrawler.BuildVideoID("crawler-pending", "source-1"),
		DriveID:       "crawler-pending",
		FileID:        "source-1.mp4",
		FileName:      "source-1.mp4",
		Title:         "Source 1",
		Size:          123,
		Ext:           "mp4",
		PreviewStatus: "ready",
		PublishedAt:   time.Now(),
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	migrator := &serverFakeCrawlerUploadRunner{}
	app := &App{cat: cat, registry: proxy.NewRegistry(), crawlerUploader: migrator}

	accepted, message := app.scheduleManualCrawlerUploadMigration(ctx, "crawler-pending")
	if accepted {
		t.Fatal("accepted = true, want false")
	}
	if !strings.Contains(message, "指纹") {
		t.Fatalf("message = %q, want fingerprint reason", message)
	}
	if migrator.called.Load() != 0 {
		t.Fatalf("migration calls = %d, want 0", migrator.called.Load())
	}
}

func TestDriveGenerationStatusUsesWorkerQueueNotPendingCatalogRows(t *testing.T) {
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
		ID:            "pending-thumb",
		DriveID:       "drive-id",
		FileID:        "file-id",
		Title:         "Pending Thumb",
		PreviewStatus: "ready",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if err := cat.UpdateVideoMeta(ctx, "pending-thumb", catalog.VideoMetaPatch{ThumbnailStatus: "pending"}); err != nil {
		t.Fatalf("mark thumbnail pending: %v", err)
	}

	thumbWorker := preview.NewThumbWorker(&serverFakeTeaserGenerator{}, cat, &serverFakeDrive{})
	app := &App{
		cat:                cat,
		workers:            map[string]*preview.Worker{},
		thumbWorkers:       map[string]*preview.ThumbWorker{"drive-id": thumbWorker},
		fingerprintWorkers: map[string]*fingerprint.Worker{},
	}

	status := app.driveGenerationStatuses()["drive-id"].Thumbnail
	if status.State != "idle" || status.QueueLength != 0 {
		t.Fatalf("thumbnail status = %#v, want idle with empty worker queue", status)
	}
}

func TestRegenFailedThumbnailsQueuesPendingRowsAfterStop(t *testing.T) {
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
		ID:            "pending-thumb",
		DriveID:       "drive-id",
		FileID:        "file-id",
		Title:         "Pending Thumb",
		PreviewStatus: "ready",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if err := cat.UpdateVideoMeta(ctx, "pending-thumb", catalog.VideoMetaPatch{ThumbnailStatus: "pending"}); err != nil {
		t.Fatalf("mark thumbnail pending: %v", err)
	}

	thumbWorker := preview.NewThumbWorker(&serverFakeTeaserGenerator{}, cat, &serverFakeDrive{})
	app := &App{
		cat:          cat,
		thumbWorkers: map[string]*preview.ThumbWorker{"drive-id": thumbWorker},
	}

	app.regenFailedThumbnails(ctx, "drive-id")

	if got := thumbWorker.Status().QueueLength; got != 1 {
		t.Fatalf("thumb queue length = %d, want pending row re-enqueued", got)
	}
}

func TestRunScanStartsFingerprintBeforeThumbnailAndPreviewDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	seedDriveWithTeaser(t, cat, "drive-id", true)

	dataPath := filepath.Join(t.TempDir(), "scan-video.mp4")
	data := []byte("scan video content for independent fingerprint")
	if err := os.WriteFile(dataPath, data, 0o644); err != nil {
		t.Fatalf("write video data: %v", err)
	}

	drv := &serverScanFingerprintFakeDrive{
		serverFingerprintFakeDrive: serverFingerprintFakeDrive{path: dataPath},
		entries: []drives.Entry{{
			ID:       "file-id",
			Name:     "scan-video.mp4",
			Size:     int64(len(data)),
			ParentID: "root",
		}},
	}
	registry := proxy.NewRegistry()
	registry.Set("drive-id", drv)

	gen := &serverFakeTeaserGenerator{}
	worker := preview.NewWorker(gen, cat, drv)
	thumbWorker := preview.NewThumbWorker(gen, cat, drv)
	fingerprintWorker := fingerprint.NewWorker(cat, drv, fingerprint.Config{})
	go fingerprintWorker.Run(ctx)

	app := &App{
		cfg: &config.Config{
			Scanner: config.Scanner{VideoExtensions: []string{".mp4"}},
		},
		cat:                cat,
		registry:           registry,
		workers:            map[string]*preview.Worker{"drive-id": worker},
		thumbWorkers:       map[string]*preview.ThumbWorker{"drive-id": thumbWorker},
		fingerprintWorkers: map[string]*fingerprint.Worker{"drive-id": fingerprintWorker},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		app.runScan(ctx, "drive-id")
	}()

	videoID := "fake-drive-id-file-id"
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := cat.GetVideo(ctx, videoID)
		if err == nil && got.SampledSHA256 != "" && got.FingerprintStatus == "ready" {
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("scan did not stop after context cancel")
			}
			if got.ThumbnailURL != "" {
				t.Fatalf("thumbnail url = %q, want fingerprint before thumbnail generation", got.ThumbnailURL)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scan did not stop after context cancel")
	}
	got, err := cat.GetVideo(context.Background(), videoID)
	if err != nil {
		t.Fatalf("get video after timeout: %v", err)
	}
	t.Fatalf("fingerprint status=%q sampled=%q, want ready before thumbnail/preview drain", got.FingerprintStatus, got.SampledSHA256)
}

func TestNightlyTargetsComeFromCatalogBeforeDriveAttach(t *testing.T) {
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
		{ID: "115", Kind: "p115", Name: "115", RootID: "0", TeaserEnabled: true},
		{ID: "pikpak", Kind: "pikpak", Name: "PikPak", RootID: "0", TeaserEnabled: true},
		{ID: "crawler-main", Kind: scriptcrawler.Kind, Name: "Crawler", RootID: "/", Credentials: map[string]string{"script_path": "/tmp/crawler.py"}, TeaserEnabled: true},
		{ID: "crawler-paused", Kind: scriptcrawler.Kind, Name: "Paused Crawler", RootID: "/", Credentials: map[string]string{"script_path": "/tmp/paused.py", "paused": "true"}, TeaserEnabled: true},
		{ID: "crawler-deleted", Kind: scriptcrawler.Kind, Name: "Deleted Crawler", RootID: "/", Credentials: map[string]string{}, TeaserEnabled: true},
	} {
		if err := cat.UpsertDrive(ctx, d); err != nil {
			t.Fatalf("seed drive %s: %v", d.ID, err)
		}
	}

	app := &App{cat: cat}
	scanIDs := app.listScanTargetIDs(ctx)
	if len(scanIDs) != 2 || scanIDs[0] != "115" || scanIDs[1] != "pikpak" {
		t.Fatalf("scan target ids = %#v, want 115 and pikpak from catalog", scanIDs)
	}
	crawlerIDs := app.listCrawlerDriveIDs(ctx)
	if len(crawlerIDs) != 1 || crawlerIDs[0] != "crawler-main" {
		t.Fatalf("crawler ids = %#v, want crawler-page script drive", crawlerIDs)
	}
}

func TestAttachDriveRejectsUnknownKind(t *testing.T) {
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
	d := &catalog.Drive{
		ID:            "unknown-main",
		Kind:          "unknown",
		Name:          "Unknown",
		RootID:        "/",
		TeaserEnabled: true,
	}
	if err := cat.UpsertDrive(ctx, d); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	app := &App{cat: cat, registry: proxy.NewRegistry()}
	err = app.attachDrive(ctx, d)
	if err == nil || !strings.Contains(err.Error(), "unknown drive kind: unknown") {
		t.Fatalf("attach err = %v, want unknown kind error", err)
	}
	if _, ok := app.registry.Get(d.ID); ok {
		t.Fatal("unknown drive should not be registered")
	}
}

func TestFailedThumbnailsDoNotBlockPreviewGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	seedDriveWithTeaser(t, cat, "drive-id", true)
	now := time.Now()
	video := &catalog.Video{
		ID:            "video-failed-thumb",
		DriveID:       "drive-id",
		FileID:        "file-1",
		Title:         "Clip With Failed Thumb",
		PreviewStatus: "pending",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if err := cat.UpdateVideoMeta(ctx, video.ID, catalog.VideoMetaPatch{ThumbnailStatus: "failed"}); err != nil {
		t.Fatalf("mark thumbnail failed: %v", err)
	}
	missing, err := cat.CountVideosNeedingThumbnail(ctx, "drive-id")
	if err != nil {
		t.Fatalf("count missing thumbnails: %v", err)
	}
	if missing != 0 {
		t.Fatalf("missing thumbnails = %d, want failed thumbnails excluded", missing)
	}

	app := &App{
		cat:          cat,
		workers:      make(map[string]*preview.Worker),
		thumbWorkers: make(map[string]*preview.ThumbWorker),
	}
	gen := &serverFakeTeaserGenerator{}
	drv := &serverFakeDrive{}
	worker := preview.NewWorker(gen, cat, drv)
	thumbWorker := preview.NewThumbWorker(gen, cat, drv)
	go worker.Run(ctx)
	go thumbWorker.Run(ctx)

	app.registerPreviewWorkers(ctx, "drive-id", worker, thumbWorker, nil, func() {})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := cat.GetVideo(ctx, video.ID)
		if err != nil {
			t.Fatalf("get video: %v", err)
		}
		if got.PreviewStatus == "ready" {
			events := gen.Events()
			if len(events) != 1 || events[0] != "preview:"+video.ID {
				t.Fatalf("events = %#v, want preview only", events)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video after timeout: %v", err)
	}
	t.Fatalf("preview status = %q, want ready; events=%#v", got.PreviewStatus, gen.Events())
}

func TestRegenFailedPreviewsQueuesOnlyFailedVideosForDrive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	seedDriveWithTeaser(t, cat, "drive-id", true)
	seedDriveWithTeaser(t, cat, "other-drive", true)
	now := time.Now()
	for _, v := range []*catalog.Video{
		{ID: "target-failed", DriveID: "drive-id", FileID: "file-1", Title: "Target Failed", PreviewStatus: "failed"},
		{ID: "target-ready", DriveID: "drive-id", FileID: "file-2", Title: "Target Ready", PreviewStatus: "ready", PreviewLocal: "/tmp/ready.mp4"},
		{ID: "other-failed", DriveID: "other-drive", FileID: "file-3", Title: "Other Failed", PreviewStatus: "failed"},
	} {
		v.PublishedAt = now
		v.CreatedAt = now
		v.UpdatedAt = now
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	app := &App{
		cat:          cat,
		workers:      make(map[string]*preview.Worker),
		thumbWorkers: make(map[string]*preview.ThumbWorker),
	}
	worker := preview.NewWorker(&serverFakeTeaserGenerator{}, cat, &serverFakeDrive{})
	go worker.Run(ctx)
	app.mu.Lock()
	app.workers["drive-id"] = worker
	app.mu.Unlock()

	app.regenFailedPreviews(ctx, "drive-id")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := cat.GetVideo(ctx, "target-failed")
		if err != nil {
			t.Fatalf("get target failed: %v", err)
		}
		if got.PreviewStatus == "ready" {
			if got.PreviewLocal != "/tmp/target-failed.mp4" {
				t.Fatalf("target preview local = %q, want regenerated local teaser path", got.PreviewLocal)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	target, err := cat.GetVideo(ctx, "target-failed")
	if err != nil {
		t.Fatalf("get regenerated target: %v", err)
	}
	if target.PreviewStatus != "ready" {
		t.Fatalf("target preview status = %q, want ready", target.PreviewStatus)
	}
	ready, err := cat.GetVideo(ctx, "target-ready")
	if err != nil {
		t.Fatalf("get target ready: %v", err)
	}
	if ready.PreviewLocal != "/tmp/ready.mp4" || ready.PreviewStatus != "ready" {
		t.Fatalf("ready video changed: status=%q local=%q", ready.PreviewStatus, ready.PreviewLocal)
	}
	other, err := cat.GetVideo(ctx, "other-failed")
	if err != nil {
		t.Fatalf("get other failed: %v", err)
	}
	if other.PreviewStatus != "failed" {
		t.Fatalf("other drive preview status = %q, want failed", other.PreviewStatus)
	}
}

func TestEnqueueUploadedVideoQueuesLocalGenerationByDefault(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	video := &catalog.Video{
		ID:            "local-upload-video",
		DriveID:       "local-upload",
		FileID:        "upload-1.mp4",
		Title:         "Uploaded",
		PreviewStatus: "pending",
		PublishedAt:   time.Now(),
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	app := &App{
		cat:          cat,
		workers:      make(map[string]*preview.Worker),
		thumbWorkers: make(map[string]*preview.ThumbWorker),
	}
	gen := &serverFakeTeaserGenerator{}
	drv := &serverLocalUploadFakeDrive{}
	worker := preview.NewWorker(gen, cat, drv)
	thumbWorker := preview.NewThumbWorker(gen, cat, drv)
	go worker.Run(ctx)
	go thumbWorker.Run(ctx)
	app.mu.Lock()
	app.workers["local-upload"] = worker
	app.thumbWorkers["local-upload"] = thumbWorker
	app.mu.Unlock()

	app.enqueueUploadedVideo(ctx, video)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := cat.GetVideo(ctx, video.ID)
		if err != nil {
			t.Fatalf("get video: %v", err)
		}
		if got.PreviewStatus == "ready" && got.ThumbnailURL != "" {
			if got.PreviewLocal != "/tmp/local-upload-video.mp4" {
				t.Fatalf("preview local = %q, want generated local teaser path", got.PreviewLocal)
			}
			if got.ThumbnailURL != "/p/thumb/local-upload-video" {
				t.Fatalf("thumbnail url = %q, want generated thumbnail URL", got.ThumbnailURL)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video after timeout: %v", err)
	}
	t.Fatalf("preview status = %q, thumbnail url = %q; want generated local teaser and thumbnail", got.PreviewStatus, got.ThumbnailURL)
}

func TestRestoreDeletedVideoQueuesDerivedAssetsAndInvalidatesTags(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	video := &catalog.Video{
		ID: "local-upload-restored", DriveID: "local-upload", FileID: "restored.mp4",
		FileName: "restored.mp4", Title: "Restored", Size: 4096,
		ThumbnailURL: "/p/thumb/local-upload-restored", PreviewLocal: "/tmp/restored.mp4", PreviewStatus: "ready",
		PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if err := cat.DeleteVideoWithTombstone(ctx, video.ID); err != nil {
		t.Fatalf("tombstone video: %v", err)
	}

	drv := &serverRestorableLocalUploadDrive{entry: &drives.Entry{
		ID: video.FileID, Name: video.FileName, Size: video.Size, ModTime: now,
	}}
	registry := proxy.NewRegistry()
	registry.Set(drv.ID(), drv)
	gen := &serverFakeTeaserGenerator{}
	worker := preview.NewWorker(gen, cat, drv)
	thumbWorker := preview.NewThumbWorker(gen, cat, drv)
	tagInvalidations := 0
	app := &App{
		cat:          cat,
		registry:     registry,
		workers:      map[string]*preview.Worker{"local-upload": worker},
		thumbWorkers: map[string]*preview.ThumbWorker{"local-upload": thumbWorker},
		onTagsChanged: func() {
			tagInvalidations++
		},
	}

	if err := app.restoreDeletedVideo(ctx, video.ID); err != nil {
		t.Fatalf("restore deleted video: %v", err)
	}
	if got := worker.Status().QueueLength; got != 1 {
		t.Fatalf("preview queue length = %d, want 1", got)
	}
	if got := thumbWorker.Status().QueueLength; got != 1 {
		t.Fatalf("thumbnail queue length = %d, want 1", got)
	}
	if tagInvalidations != 1 {
		t.Fatalf("tag cache invalidations = %d, want 1", tagInvalidations)
	}
	restored, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get restored video: %v", err)
	}
	if restored.PreviewStatus != "pending" || restored.ThumbnailURL != "" {
		t.Fatalf("restored derived state = preview:%q thumbnail:%q", restored.PreviewStatus, restored.ThumbnailURL)
	}
}

func TestBlacklistSourceDeleteSkipsSnapshotRestoredBeforeProcessing(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	video := &catalog.Video{
		ID: "local-upload-stale-delete", DriveID: "local-upload", FileID: "stale.mp4",
		FileName: "stale.mp4", Title: "Stale", Size: 2048,
		PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if err := cat.DeleteVideoWithTombstone(ctx, video.ID); err != nil {
		t.Fatalf("tombstone video: %v", err)
	}
	snapshot, err := cat.ListDeletedVideosPendingSourceDeletion(ctx)
	if err != nil || len(snapshot) != 1 {
		t.Fatalf("source deletion snapshot = %d err=%v, want one", len(snapshot), err)
	}

	drv := &serverRestorableLocalUploadDrive{entry: &drives.Entry{
		ID: video.FileID, Name: video.FileName, Size: video.Size, ModTime: now,
	}}
	registry := proxy.NewRegistry()
	registry.Set(drv.ID(), drv)
	app := &App{cat: cat, registry: registry}
	if err := app.restoreDeletedVideo(ctx, video.ID); err != nil {
		t.Fatalf("restore deleted video: %v", err)
	}
	skipped, err := app.deleteBlacklistedVideoSource(ctx, snapshot[0])
	if err != nil {
		t.Fatalf("process stale source deletion snapshot: %v", err)
	}
	if !skipped {
		t.Fatal("stale source deletion snapshot was not skipped")
	}
	drv.mu.Lock()
	removeCalls := drv.removeCalls
	drv.mu.Unlock()
	if removeCalls != 0 {
		t.Fatalf("source remove calls = %d, want 0", removeCalls)
	}
	if _, err := cat.GetVideo(ctx, video.ID); err != nil {
		t.Fatalf("restored video was damaged by stale source deletion: %v", err)
	}

	// Reusing the same stable video ID must not let the old job claim a newer
	// tombstone generation created after the restore.
	time.Sleep(2 * time.Millisecond)
	if err := cat.DeleteVideoWithTombstone(ctx, video.ID); err != nil {
		t.Fatalf("create newer tombstone: %v", err)
	}
	skipped, err = app.deleteBlacklistedVideoSource(ctx, snapshot[0])
	if err != nil {
		t.Fatalf("process snapshot against newer tombstone: %v", err)
	}
	if !skipped {
		t.Fatal("stale snapshot claimed a newer tombstone generation")
	}
	drv.mu.Lock()
	removeCalls = drv.removeCalls
	drv.mu.Unlock()
	if removeCalls != 0 {
		t.Fatalf("source remove calls after newer tombstone = %d, want 0", removeCalls)
	}
	if deleted, err := cat.IsVideoDeleted(ctx, video.ID); err != nil || !deleted {
		t.Fatalf("newer tombstone changed: deleted=%v err=%v", deleted, err)
	}
}

func TestBlacklistSourceDeleteAndRestoreAreSerializedPerVideo(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	video := &catalog.Video{
		ID: "local-upload-delete-restore-race", DriveID: "local-upload", FileID: "race.mp4",
		FileName: "race.mp4", Title: "Race", Size: 2048,
		PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if err := cat.DeleteVideoWithTombstone(ctx, video.ID); err != nil {
		t.Fatalf("tombstone video: %v", err)
	}
	snapshot, err := cat.ListDeletedVideosPendingSourceDeletion(ctx)
	if err != nil || len(snapshot) != 1 {
		t.Fatalf("source deletion snapshot = %d err=%v, want one", len(snapshot), err)
	}

	removeStarted := make(chan struct{}, 1)
	allowRemove := make(chan struct{})
	var releaseRemoveOnce sync.Once
	releaseRemove := func() { releaseRemoveOnce.Do(func() { close(allowRemove) }) }
	t.Cleanup(releaseRemove)
	drv := &serverRestorableLocalUploadDrive{
		entry:         &drives.Entry{ID: video.FileID, Name: video.FileName, Size: video.Size, ModTime: now},
		removeStarted: removeStarted,
		allowRemove:   allowRemove,
	}
	registry := proxy.NewRegistry()
	registry.Set(drv.ID(), drv)
	app := &App{cat: cat, registry: registry}

	type deleteResult struct {
		skipped bool
		err     error
	}
	deleteDone := make(chan deleteResult, 1)
	go func() {
		skipped, err := app.deleteBlacklistedVideoSource(ctx, snapshot[0])
		deleteDone <- deleteResult{skipped: skipped, err: err}
	}()
	select {
	case <-removeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("source deletion did not reach provider remove")
	}

	restoreDone := make(chan error, 1)
	go func() { restoreDone <- app.restoreDeletedVideo(ctx, video.ID) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		app.blacklistVideoLocks.mu.Lock()
		item := app.blacklistVideoLocks.items[video.ID]
		refs := 0
		if item != nil {
			refs = item.refs
		}
		app.blacklistVideoLocks.mu.Unlock()
		if refs == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restore did not wait on the per-video operation lock")
		}
		time.Sleep(time.Millisecond)
	}
	drv.mu.Lock()
	statCalls := drv.statCalls
	drv.mu.Unlock()
	if statCalls != 0 {
		t.Fatalf("restore inspected source during deletion: stat calls = %d", statCalls)
	}

	releaseRemove()
	if result := <-deleteDone; result.err != nil || result.skipped {
		t.Fatalf("source deletion result = skipped:%v err:%v", result.skipped, result.err)
	}
	if err := <-restoreDone; !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("restore after source deletion error = %v, want sql.ErrNoRows", err)
	}
	if _, err := cat.GetVideo(ctx, video.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("source-deleted video was restored: %v", err)
	}
}

func TestRestoreDeletedVideoRejectsEmptySource(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	video := &catalog.Video{
		ID: "local-upload-empty", DriveID: "local-upload", FileID: "empty.mp4",
		FileName: "empty.mp4", Title: "Empty", Size: 100,
		PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if err := cat.DeleteVideoWithTombstone(ctx, video.ID); err != nil {
		t.Fatalf("tombstone video: %v", err)
	}
	drv := &serverRestorableLocalUploadDrive{entry: &drives.Entry{ID: video.FileID, Name: video.FileName}}
	registry := proxy.NewRegistry()
	registry.Set(drv.ID(), drv)
	app := &App{cat: cat, registry: registry}

	if err := app.restoreDeletedVideo(ctx, video.ID); !errors.Is(err, catalog.ErrDeletedVideoSourceMissing) {
		t.Fatalf("empty source restore error = %v, want ErrDeletedVideoSourceMissing", err)
	}
	if deleted, err := cat.IsVideoDeleted(ctx, video.ID); err != nil || !deleted {
		t.Fatalf("empty source tombstone changed: deleted=%v err=%v", deleted, err)
	}
}

func TestShouldScanDriveSkipsLocalUpload(t *testing.T) {
	if shouldScanDrive(&serverLocalUploadFakeDrive{}) {
		t.Fatal("local upload drive should not be scanned")
	}
	if !shouldScanDrive(&serverFakeDrive{}) {
		t.Fatal("normal drive should be scanned")
	}
}

func TestCleanupMissingPikPakVideosRemovesDatabaseRowsAndLocalAssets(t *testing.T) {
	ctx := context.Background()
	localDir := t.TempDir()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	obsoletePreview := filepath.Join(localDir, "obsolete.mp4")
	obsoleteThumb := filepath.Join(localDir, "thumbs", "pikpak-PikPak-obsolete.jpg")
	keptPreview := filepath.Join(localDir, "kept.mp4")
	for _, path := range []string{obsoletePreview, obsoleteThumb, keptPreview} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte("asset"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	now := time.Now()
	for _, v := range []*catalog.Video{
		{
			ID:            "pikpak-PikPak-obsolete",
			DriveID:       "PikPak",
			FileID:        "obsolete",
			Title:         "Obsolete",
			PreviewStatus: "ready",
			PreviewLocal:  obsoletePreview,
		},
		{
			ID:            "pikpak-PikPak-kept",
			DriveID:       "PikPak",
			FileID:        "kept",
			Title:         "Kept",
			PreviewStatus: "ready",
			PreviewLocal:  keptPreview,
		},
		{
			ID:            "onedrive-OneDrive-obsolete",
			DriveID:       "OneDrive",
			FileID:        "obsolete",
			Title:         "Other Drive",
			PreviewStatus: "ready",
		},
	} {
		v.PublishedAt = now
		v.CreatedAt = now
		v.UpdatedAt = now
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed %s: %v", v.ID, err)
		}
	}

	app := &App{
		cfg: &config.Config{Storage: config.Storage{LocalPreviewDir: localDir}},
		cat: cat,
	}
	removed, err := app.cleanupMissingDriveVideos(ctx, "PikPak", map[string]struct{}{"kept": {}}, nil, true)
	if err != nil {
		t.Fatalf("first cleanup missing videos: %v", err)
	}
	if removed != 0 {
		t.Fatalf("first removed = %d, want 0 before confirmation", removed)
	}
	if _, err := cat.GetVideo(ctx, "pikpak-PikPak-obsolete"); err != nil {
		t.Fatalf("obsolete video removed after one scan: %v", err)
	}
	for _, path := range []string{obsoletePreview, obsoleteThumb} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("asset %s removed after one scan: %v", path, err)
		}
	}

	removed, err = app.cleanupMissingDriveVideos(ctx, "PikPak", map[string]struct{}{"kept": {}}, nil, true)
	if err != nil {
		t.Fatalf("confirmed cleanup missing videos: %v", err)
	}
	if removed != 1 {
		t.Fatalf("confirmed removed = %d, want 1", removed)
	}
	if _, err := cat.GetVideo(ctx, "pikpak-PikPak-obsolete"); err != sql.ErrNoRows {
		t.Fatalf("obsolete video lookup error = %v, want sql.ErrNoRows", err)
	}
	if _, err := cat.GetVideo(ctx, "pikpak-PikPak-kept"); err != nil {
		t.Fatalf("kept video missing after cleanup: %v", err)
	}
	if _, err := cat.GetVideo(ctx, "onedrive-OneDrive-obsolete"); err != nil {
		t.Fatalf("other drive video missing after cleanup: %v", err)
	}
	for _, path := range []string{obsoletePreview, obsoleteThumb} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("obsolete asset %s still exists, stat err=%v", path, err)
		}
	}
	if _, err := os.Stat(keptPreview); err != nil {
		t.Fatalf("kept preview missing: %v", err)
	}
}

func TestCleanupDriveVideosForDeleteRemovesRowsAndGeneratedAssetsOnly(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	localDir := filepath.Join(root, "previews")
	originalDir := filepath.Join(root, "local-videos")
	originalVideo := filepath.Join(originalDir, "clip.mp4")
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	for _, path := range []string{originalVideo} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:            "local-main",
		Kind:          "localstorage",
		Name:          "Local",
		RootID:        "/",
		Credentials:   map[string]string{"path": originalDir},
		TeaserEnabled: true,
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	previewPath := filepath.Join(localDir, "localstorage-local-main-file.mp4")
	thumbPath := filepath.Join(localDir, "thumbs", "localstorage-local-main-file.jpg")
	backgroundPath := mediaasset.ShortsBackgroundPath(localDir, "localstorage-local-main-file")
	for _, path := range []string{previewPath, thumbPath, backgroundPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte("generated"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:            "localstorage-local-main-file",
		DriveID:       "local-main",
		FileID:        "encoded-local-file",
		Title:         "Local File",
		PreviewLocal:  previewPath,
		PreviewStatus: "ready",
		ThumbnailURL:  "/p/thumb/localstorage-local-main-file",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("seed local video: %v", err)
	}
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:            "pikpak-other",
		DriveID:       "PikPak",
		FileID:        "other",
		Title:         "Other",
		PreviewStatus: "ready",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("seed other video: %v", err)
	}

	app := &App{
		cfg:                &config.Config{Storage: config.Storage{LocalPreviewDir: localDir}},
		cat:                cat,
		registry:           proxy.NewRegistry(),
		workers:            make(map[string]*preview.Worker),
		thumbWorkers:       make(map[string]*preview.ThumbWorker),
		fingerprintWorkers: make(map[string]*fingerprint.Worker),
	}
	removed, err := app.cleanupDriveVideosForDelete(ctx, "local-main")
	if err != nil {
		t.Fatalf("cleanup drive videos: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := cat.GetVideo(ctx, "localstorage-local-main-file"); err != sql.ErrNoRows {
		t.Fatalf("deleted video lookup error = %v, want sql.ErrNoRows", err)
	}
	if _, err := cat.GetVideo(ctx, "pikpak-other"); err != nil {
		t.Fatalf("other drive video missing: %v", err)
	}
	for _, path := range []string{previewPath, thumbPath, backgroundPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("generated asset %s still exists, stat err=%v", path, err)
		}
	}
	if _, err := os.Stat(originalVideo); err != nil {
		t.Fatalf("original local video should remain, stat err=%v", err)
	}
}

func TestDeleteVideoRemovesGeneratedAssetsKeepsLocalOriginalAndTombstones(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	localDir := filepath.Join(root, "previews")
	originalDir := filepath.Join(root, "local-videos")
	originalVideo := filepath.Join(originalDir, "clip.mp4")
	if err := os.MkdirAll(originalDir, 0o755); err != nil {
		t.Fatalf("mkdir original dir: %v", err)
	}
	if err := os.WriteFile(originalVideo, []byte("original"), 0o644); err != nil {
		t.Fatalf("write original: %v", err)
	}

	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:            "local-main",
		Kind:          "localstorage",
		Name:          "Local",
		RootID:        "/",
		Credentials:   map[string]string{"path": originalDir},
		TeaserEnabled: true,
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	previewPath := filepath.Join(localDir, "localstorage-local-main-file.mp4")
	thumbPath := filepath.Join(localDir, "thumbs", "localstorage-local-main-file.jpg")
	backgroundPath := mediaasset.ShortsBackgroundPath(localDir, "localstorage-local-main-file")
	for _, path := range []string{previewPath, thumbPath, backgroundPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte("generated"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:                "localstorage-local-main-file",
		DriveID:           "local-main",
		FileID:            "file",
		FileName:          "clip.mp4",
		SampledSHA256:     "sampled",
		FingerprintStatus: "ready",
		Title:             "Local File",
		PreviewLocal:      previewPath,
		PreviewStatus:     "ready",
		ThumbnailURL:      "/p/thumb/localstorage-local-main-file",
		Size:              123,
		PublishedAt:       now,
		CreatedAt:         now,
		UpdatedAt:         now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	app := &App{
		cfg: &config.Config{Storage: config.Storage{LocalPreviewDir: localDir}},
		cat: cat,
	}
	result, err := app.deleteVideo(ctx, "localstorage-local-main-file", false)
	if err != nil {
		t.Fatalf("delete video: %v", err)
	}
	if !result.OK || result.DeletedSource {
		t.Fatalf("delete result = %#v, want ok without source deletion", result)
	}
	if _, err := cat.GetVideo(ctx, "localstorage-local-main-file"); err != sql.ErrNoRows {
		t.Fatalf("deleted video lookup error = %v, want sql.ErrNoRows", err)
	}
	deleted, err := cat.IsDeletedVideoCandidate(ctx, "localstorage-local-main-file", "local-main", "file", "", "clip.mp4", 123)
	if err != nil {
		t.Fatalf("check tombstone: %v", err)
	}
	if !deleted {
		t.Fatal("deleted video tombstone missing")
	}
	for _, path := range []string{previewPath, thumbPath, backgroundPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("generated asset %s still exists, stat err=%v", path, err)
		}
	}
	if _, err := os.Stat(originalVideo); err != nil {
		t.Fatalf("original local video was removed: %v", err)
	}
}

func TestDeleteVideoRemovesSourceFileWhenRequested(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	localDir := filepath.Join(root, "previews")
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	previewPath := filepath.Join(localDir, "video-with-source.mp4")
	thumbPath := filepath.Join(localDir, "thumbs", "video-with-source.jpg")
	for _, path := range []string{previewPath, thumbPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte("file"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:            "video-with-source",
		DriveID:       "source-drive",
		FileID:        "source-file",
		FileName:      "clip.mp4",
		Title:         "Source File",
		PreviewLocal:  previewPath,
		PreviewStatus: "ready",
		ThumbnailURL:  "/p/thumb/video-with-source",
		Size:          123,
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	registry := proxy.NewRegistry()
	drv := &serverRemovableFakeDrive{id: "source-drive"}
	registry.Set(drv.ID(), drv)
	app := &App{
		cfg:      &config.Config{Storage: config.Storage{LocalPreviewDir: localDir}},
		cat:      cat,
		registry: registry,
	}
	result, err := app.deleteVideo(ctx, "video-with-source", true)
	if err != nil {
		t.Fatalf("delete video: %v", err)
	}
	if !result.OK || !result.DeletedSource {
		t.Fatalf("delete result = %#v, want source deleted", result)
	}
	if got, want := drv.removedFileID, "source-file"; got != want {
		t.Fatalf("removed source fileID = %q, want %q", got, want)
	}
	if _, err := cat.GetVideo(ctx, "video-with-source"); err != sql.ErrNoRows {
		t.Fatalf("deleted video lookup error = %v, want sql.ErrNoRows", err)
	}
	deletedItems, _, err := cat.ListDeletedVideos(ctx, catalog.ListParams{Page: 1, PageSize: 10, IncludeSourceDeleted: true})
	if err != nil {
		t.Fatalf("list deleted videos: %v", err)
	}
	if len(deletedItems) != 0 {
		t.Fatalf("source-deleted video kept tombstone = %#v", deletedItems)
	}
	for _, path := range []string{previewPath, thumbPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("generated asset %s still exists, stat err=%v", path, err)
		}
	}
}

func TestDeleteVideoUsesSourceRemoverWithCatalogMetadata(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "video-with-rich-source",
		DriveID:     "source-drive",
		FileID:      "source-fid",
		ParentID:    "parent-dir",
		FileName:    "clip.mp4",
		Title:       "Source File",
		Size:        123,
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	registry := proxy.NewRegistry()
	drv := &serverSourceRemovableFakeDrive{id: "source-drive"}
	registry.Set(drv.ID(), drv)
	app := &App{
		cfg:      &config.Config{Storage: config.Storage{LocalPreviewDir: filepath.Join(t.TempDir(), "previews")}},
		cat:      cat,
		registry: registry,
	}
	result, err := app.deleteVideo(ctx, "video-with-rich-source", true)
	if err != nil {
		t.Fatalf("delete video: %v", err)
	}
	if !result.OK || !result.DeletedSource {
		t.Fatalf("delete result = %#v, want source deleted", result)
	}
	if drv.fallbackRemoveCalled {
		t.Fatal("fallback Remove was called, want SourceRemover")
	}
	want := drives.SourceFile{
		FileID:   "source-fid",
		ParentID: "parent-dir",
		Name:     "clip.mp4",
		Size:     123,
	}
	if drv.removedSource != want {
		t.Fatalf("removed source = %#v, want %#v", drv.removedSource, want)
	}
}

func TestDeleteVideoRemovesScriptCrawlerSourceFile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	localDir := filepath.Join(root, "previews")
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:            "crawler-main",
		Kind:          scriptcrawler.Kind,
		Name:          "Crawler",
		RootID:        "/",
		TeaserEnabled: true,
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}
	app := &App{
		cfg:      &config.Config{Storage: config.Storage{LocalPreviewDir: localDir}},
		cat:      cat,
		registry: proxy.NewRegistry(),
	}
	sourceDir := app.scriptCrawlerDriveDir("crawler-main")
	app.registry.Set("crawler-main", scriptcrawler.New(scriptcrawler.Config{
		ID:      "crawler-main",
		RootDir: sourceDir,
	}))
	sourceVideo := filepath.Join(sourceDir, "videos", "source.mp4")
	sourceThumb := filepath.Join(sourceDir, "thumbs", "source.jpg")
	previewPath := filepath.Join(localDir, "scriptcrawler-crawler-main-source.mp4")
	commonThumb := filepath.Join(localDir, "thumbs", "scriptcrawler-crawler-main-source.jpg")
	for _, path := range []string{sourceVideo, sourceThumb, previewPath, commonThumb} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte("file"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:            "scriptcrawler-crawler-main-source",
		DriveID:       "crawler-main",
		FileID:        "source.mp4",
		FileName:      "source.mp4",
		Ext:           "mp4",
		Title:         "Crawler Source",
		PreviewLocal:  previewPath,
		PreviewStatus: "ready",
		ThumbnailURL:  "/p/thumb/scriptcrawler-crawler-main-source",
		Size:          456,
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	result, err := app.deleteVideo(ctx, "scriptcrawler-crawler-main-source", true)
	if err != nil {
		t.Fatalf("delete crawler video: %v", err)
	}
	if !result.OK || !result.DeletedSource {
		t.Fatalf("delete result = %#v, want source deleted", result)
	}
	for _, path := range []string{sourceVideo, sourceThumb, previewPath, commonThumb} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("deleted file %s still exists, stat err=%v", path, err)
		}
	}
	if _, err := cat.GetVideo(ctx, "scriptcrawler-crawler-main-source"); err != sql.ErrNoRows {
		t.Fatalf("deleted video lookup error = %v, want sql.ErrNoRows", err)
	}
	deleted, err := cat.IsVideoDeleted(ctx, "scriptcrawler-crawler-main-source")
	if err != nil {
		t.Fatalf("check tombstone: %v", err)
	}
	if deleted {
		t.Fatal("deleted crawler source kept tombstone")
	}
}

func TestRunBlacklistSourceDeleteMarksSuccessAndKeepsFailuresPending(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	for _, v := range []*catalog.Video{
		{
			ID: "source-ok", DriveID: "source-drive", FileID: "file-ok", ParentID: "parent-ok",
			FileName: "ok.mp4", Title: "OK", Size: 123,
			PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "source-fail", DriveID: "missing-drive", FileID: "file-fail",
			FileName: "fail.mp4", Title: "Fail", Size: 456,
			PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed %s: %v", v.ID, err)
		}
		if err := cat.DeleteVideoWithTombstone(ctx, v.ID); err != nil {
			t.Fatalf("tombstone %s: %v", v.ID, err)
		}
	}

	registry := proxy.NewRegistry()
	drv := &serverSourceRemovableFakeDrive{id: "source-drive"}
	registry.Set(drv.ID(), drv)
	app := &App{cat: cat, registry: registry}

	app.runBlacklistSourceDelete(ctx)

	wantSource := drives.SourceFile{
		FileID: "file-ok", ParentID: "parent-ok", Name: "ok.mp4", Size: 123,
	}
	if drv.removedSource != wantSource {
		t.Fatalf("removed source = %#v, want %#v", drv.removedSource, wantSource)
	}
	status := app.blacklistSourceDeleteStatus()
	if status.State != "completed" ||
		status.Running ||
		status.Total != 2 ||
		status.Processed != 2 ||
		status.Deleted != 1 ||
		status.Failed != 1 {
		t.Fatalf("source delete status = %#v", status)
	}

	items, _, err := cat.ListDeletedVideos(ctx, catalog.ListParams{Page: 1, PageSize: 10, IncludeSourceDeleted: true})
	if err != nil {
		t.Fatalf("list tombstones: %v", err)
	}
	remaining := make(map[string]bool, len(items))
	for _, item := range items {
		remaining[item.ID] = true
	}
	if remaining["source-ok"] || !remaining["source-fail"] {
		t.Fatalf("remaining tombstones = %#v, want only failed source", remaining)
	}
	pending, err := cat.CountDeletedVideosPendingSourceDeletion(ctx)
	if err != nil || pending != 1 {
		t.Fatalf("pending after job = %d, err=%v, want 1", pending, err)
	}
}

func TestRunBlacklistSourceDeleteCanTargetSelectedIDs(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	for _, v := range []*catalog.Video{
		{
			ID: "selected-source", DriveID: "source-drive", FileID: "file-selected", ParentID: "parent-selected",
			FileName: "selected.mp4", Title: "Selected", Size: 123,
			PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "unselected-source", DriveID: "source-drive", FileID: "file-unselected", ParentID: "parent-unselected",
			FileName: "unselected.mp4", Title: "Unselected", Size: 456,
			PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed %s: %v", v.ID, err)
		}
		if err := cat.DeleteVideoWithTombstone(ctx, v.ID); err != nil {
			t.Fatalf("tombstone %s: %v", v.ID, err)
		}
	}

	registry := proxy.NewRegistry()
	drv := &serverSourceRemovableFakeDrive{id: "source-drive"}
	registry.Set(drv.ID(), drv)
	app := &App{cat: cat, registry: registry}

	app.runBlacklistSourceDelete(ctx, api.BlacklistSourceDeleteRequest{IDs: []string{"selected-source"}})

	wantSource := drives.SourceFile{
		FileID: "file-selected", ParentID: "parent-selected", Name: "selected.mp4", Size: 123,
	}
	if drv.removedSource != wantSource {
		t.Fatalf("removed source = %#v, want %#v", drv.removedSource, wantSource)
	}
	status := app.blacklistSourceDeleteStatus()
	if status.State != "completed" ||
		status.Total != 1 ||
		status.Processed != 1 ||
		status.Deleted != 1 ||
		status.Failed != 0 {
		t.Fatalf("source delete status = %#v", status)
	}

	items, _, err := cat.ListDeletedVideos(ctx, catalog.ListParams{Page: 1, PageSize: 10, IncludeSourceDeleted: true})
	if err != nil {
		t.Fatalf("list tombstones: %v", err)
	}
	remaining := make(map[string]bool, len(items))
	for _, item := range items {
		remaining[item.ID] = true
	}
	if remaining["selected-source"] || !remaining["unselected-source"] {
		t.Fatalf("remaining tombstones = %#v, want only unselected source", remaining)
	}
}

func TestCleanupDriveVideosForDeleteScriptCrawlerRemovesOnlyLocalRows(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	localDir := filepath.Join(root, "previews")
	driveID := "crawler-main"
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:            driveID,
		Kind:          scriptcrawler.Kind,
		Name:          "Crawler",
		RootID:        "/",
		TeaserEnabled: true,
	}); err != nil {
		t.Fatalf("seed crawler drive: %v", err)
	}

	app := &App{
		cfg:                &config.Config{Storage: config.Storage{LocalPreviewDir: localDir}},
		cat:                cat,
		registry:           proxy.NewRegistry(),
		workers:            make(map[string]*preview.Worker),
		thumbWorkers:       make(map[string]*preview.ThumbWorker),
		fingerprintWorkers: make(map[string]*fingerprint.Worker),
	}
	crawlerStorage := app.scriptCrawlerDriveDir(driveID)
	localSource := filepath.Join(crawlerStorage, "videos", "source.mp4")
	migratedSourceResidue := filepath.Join(crawlerStorage, "videos", "migrated.mp4")
	sourceThumb := filepath.Join(crawlerStorage, "thumbs", "source.jpg")
	crawlState := filepath.Join(crawlerStorage, ".crawl", "seen.txt")
	localPreview := filepath.Join(localDir, "scriptcrawler-crawler-main-source.mp4")
	localThumb := filepath.Join(localDir, "thumbs", "scriptcrawler-crawler-main-source.jpg")
	migratedPreview := filepath.Join(localDir, "scriptcrawler-crawler-main-migrated.mp4")
	migratedThumb := filepath.Join(localDir, "thumbs", "scriptcrawler-crawler-main-migrated.jpg")
	for _, path := range []string{
		localSource,
		migratedSourceResidue,
		sourceThumb,
		crawlState,
		localPreview,
		localThumb,
		migratedPreview,
		migratedThumb,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte("asset"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	now := time.Now()
	for _, v := range []*catalog.Video{
		{
			ID:            "scriptcrawler-crawler-main-source",
			DriveID:       driveID,
			FileID:        "source.mp4",
			Title:         "Source",
			PreviewLocal:  localPreview,
			PreviewStatus: "ready",
			ThumbnailURL:  "/p/thumb/scriptcrawler-crawler-main-source",
		},
		{
			ID:            "scriptcrawler-crawler-main-migrated",
			DriveID:       "PikPak",
			FileID:        "pikpak-file-id",
			Title:         "Migrated",
			PreviewLocal:  migratedPreview,
			PreviewStatus: "ready",
			ThumbnailURL:  "/p/thumb/scriptcrawler-crawler-main-migrated",
		},
		{
			ID:            "pikpak-PikPak-other",
			DriveID:       "PikPak",
			FileID:        "other",
			Title:         "Other",
			PreviewStatus: "ready",
		},
	} {
		v.PublishedAt = now
		v.CreatedAt = now
		v.UpdatedAt = now
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	removed, err := app.cleanupDriveVideosForDelete(ctx, driveID)
	if err != nil {
		t.Fatalf("cleanup crawler videos: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := cat.GetVideo(ctx, "scriptcrawler-crawler-main-source"); err != sql.ErrNoRows {
		t.Fatalf("local crawler video lookup error = %v, want sql.ErrNoRows", err)
	}
	if _, err := cat.GetVideo(ctx, "scriptcrawler-crawler-main-migrated"); err != nil {
		t.Fatalf("migrated crawler video missing: %v", err)
	}
	if _, err := cat.GetVideo(ctx, "pikpak-PikPak-other"); err != nil {
		t.Fatalf("unrelated pikpak video missing: %v", err)
	}
	for _, path := range []string{localPreview, localThumb} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s still exists, stat err=%v", path, err)
		}
	}
	for _, path := range []string{migratedPreview, migratedThumb} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s missing, stat err=%v", path, err)
		}
	}
	if _, err := os.Stat(crawlerStorage); !os.IsNotExist(err) {
		t.Fatalf("crawler storage still exists, stat err=%v", err)
	}
}

func TestRemoveScriptCrawlerStorageForDeleteRejectsPathsOutsideDriveRoot(t *testing.T) {
	root := t.TempDir()
	localDir := filepath.Join(root, "previews")
	marker := filepath.Join(root, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	app := &App{cfg: &config.Config{Storage: config.Storage{LocalPreviewDir: localDir}}}
	for _, driveID := range []string{".", ".."} {
		if err := app.removeScriptCrawlerStorageForDelete(driveID); err == nil {
			t.Fatalf("drive %q cleanup succeeded, want unsafe path error", driveID)
		}
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker was removed: %v", err)
	}
}

func TestCleanupOrphanDriveVideosRemovesRowsAndGeneratedAssets(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	localDir := filepath.Join(root, "previews")
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	if err := cat.UpsertDrive(ctx, &catalog.Drive{
		ID:            "active-drive",
		Kind:          "pikpak",
		Name:          "Active",
		RootID:        "root",
		TeaserEnabled: true,
	}); err != nil {
		t.Fatalf("seed active drive: %v", err)
	}

	previewPath := filepath.Join(localDir, "p123-123-orphan.mp4")
	thumbPath := filepath.Join(localDir, "thumbs", "p123-123-orphan.jpg")
	for _, path := range []string{previewPath, thumbPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte("generated"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	now := time.Now()
	for _, v := range []*catalog.Video{
		{
			ID:            "p123-123-orphan",
			DriveID:       "123",
			FileID:        "orphan-file",
			Title:         "Orphan",
			PreviewLocal:  previewPath,
			PreviewStatus: "ready",
			ThumbnailURL:  "/p/thumb/p123-123-orphan",
			PublishedAt:   now,
			CreatedAt:     now,
			UpdatedAt:     now,
		},
		{
			ID:          "pikpak-active",
			DriveID:     "active-drive",
			FileID:      "active-file",
			Title:       "Active",
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	app := &App{
		cfg: &config.Config{Storage: config.Storage{LocalPreviewDir: localDir}},
		cat: cat,
	}
	removed, err := app.cleanupOrphanDriveVideos(ctx)
	if err != nil {
		t.Fatalf("cleanup orphan videos: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := cat.GetVideo(ctx, "p123-123-orphan"); err != sql.ErrNoRows {
		t.Fatalf("orphan video lookup error = %v, want sql.ErrNoRows", err)
	}
	if _, err := cat.GetVideo(ctx, "pikpak-active"); err != nil {
		t.Fatalf("active video missing: %v", err)
	}
	for _, path := range []string{previewPath, thumbPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("orphan asset %s still exists, stat err=%v", path, err)
		}
	}
}

func TestCleanupDuplicateVideoAssetsDeletesExactDuplicateRows(t *testing.T) {
	ctx := context.Background()
	localDir := t.TempDir()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	canonicalPreview := filepath.Join(localDir, "canonical.mp4")
	duplicatePreview := filepath.Join(localDir, "duplicate.mp4")
	canonicalThumb := filepath.Join(localDir, "thumbs", "canonical-video.jpg")
	duplicateThumb := filepath.Join(localDir, "thumbs", "duplicate-video.jpg")
	for _, path := range []string{canonicalPreview, duplicatePreview, canonicalThumb, duplicateThumb} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte("asset"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	for _, v := range []*catalog.Video{
		{
			ID:            "canonical-video",
			DriveID:       "115",
			FileID:        "file-a",
			Title:         "Canonical",
			Size:          2048,
			ThumbnailURL:  "/p/thumb/canonical-video",
			PreviewLocal:  canonicalPreview,
			PreviewStatus: "ready",
			PublishedAt:   now,
			CreatedAt:     now,
			UpdatedAt:     now,
		},
		{
			ID:            "duplicate-video",
			DriveID:       "onedrive",
			FileID:        "file-b",
			Title:         "Duplicate",
			Size:          2048,
			ThumbnailURL:  "/p/thumb/duplicate-video",
			PreviewLocal:  duplicatePreview,
			PreviewStatus: "ready",
			PublishedAt:   now.Add(time.Second),
			CreatedAt:     now.Add(time.Second),
			UpdatedAt:     now.Add(time.Second),
		},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed %s: %v", v.ID, err)
		}
		if err := cat.UpdateVideoFingerprint(ctx, v.ID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "ready", ""); err != nil {
			t.Fatalf("fingerprint %s: %v", v.ID, err)
		}
	}

	app := &App{
		cfg: &config.Config{Storage: config.Storage{LocalPreviewDir: localDir}},
		cat: cat,
	}
	if err := app.cleanupDuplicateVideoAssets(ctx); err != nil {
		t.Fatalf("cleanup duplicate video assets: %v", err)
	}

	for _, path := range []string{canonicalPreview, canonicalThumb} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("canonical asset %s missing: %v", path, err)
		}
	}
	for _, path := range []string{duplicatePreview, duplicateThumb} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("duplicate asset %s still exists, stat err=%v", path, err)
		}
	}
	if _, err := cat.GetVideo(ctx, "duplicate-video"); err != sql.ErrNoRows {
		t.Fatalf("duplicate lookup error = %v, want sql.ErrNoRows", err)
	}
	deleted, err := cat.IsVideoDeleted(ctx, "duplicate-video")
	if err != nil {
		t.Fatalf("check duplicate tombstone: %v", err)
	}
	if !deleted {
		t.Fatalf("duplicate tombstone missing")
	}
	deletedItems, _, err := cat.ListDeletedVideos(ctx, catalog.ListParams{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("list deleted videos: %v", err)
	}
	if len(deletedItems) != 1 ||
		deletedItems[0].ID != "duplicate-video" ||
		deletedItems[0].Reason != catalog.DeletedVideoReasonDuplicate ||
		deletedItems[0].CanonicalVideoID != "canonical-video" ||
		deletedItems[0].RestorePolicy != catalog.DeletedVideoRestorePolicyNone {
		t.Fatalf("duplicate tombstone = %#v, want reason %q", deletedItems, catalog.DeletedVideoReasonDuplicate)
	}
	canon, err := cat.GetVideo(ctx, "canonical-video")
	if err != nil {
		t.Fatalf("get canonical: %v", err)
	}
	if canon.PreviewLocal != canonicalPreview || canon.ThumbnailURL != "/p/thumb/canonical-video" {
		t.Fatalf("canonical changed: preview=%q thumb=%q", canon.PreviewLocal, canon.ThumbnailURL)
	}
}

func TestCleanupDuplicateVideoAssetsDeletesNearDuplicateRowsKeepingLargest(t *testing.T) {
	ctx := context.Background()
	localDir := t.TempDir()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	smallPreview := filepath.Join(localDir, "small-video.mp4")
	largePreview := filepath.Join(localDir, "large-video.mp4")
	smallThumb := filepath.Join(localDir, "thumbs", "small-video.jpg")
	largeThumb := filepath.Join(localDir, "thumbs", "large-video.jpg")
	for _, path := range []string{smallPreview, largePreview} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte("preview"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	writeSolidJPEG(t, smallThumb, color.RGBA{R: 180, G: 80, B: 40, A: 255})
	writeSolidJPEG(t, largeThumb, color.RGBA{R: 180, G: 80, B: 40, A: 255})

	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	for _, v := range []*catalog.Video{
		{
			ID:              "small-video",
			DriveID:         "scriptcrawler-a",
			FileID:          "file-small",
			FileName:        "small.mp4",
			Title:           "反差极品大二女友，叫声可射～，“射进小骚逼里面～” - 91porn",
			DurationSeconds: 313,
			Size:            1024,
			ThumbnailURL:    "/p/thumb/small-video",
			PreviewLocal:    smallPreview,
			PreviewStatus:   "ready",
			PublishedAt:     now,
			CreatedAt:       now,
			UpdatedAt:       now,
		},
		{
			ID:              "large-video",
			DriveID:         "scriptcrawler-b",
			FileID:          "file-large",
			FileName:        "large.mp4",
			Title:           "反差极品大二女友，叫声可射～，“射进小骚逼里面～”_91pinse",
			DurationSeconds: 313,
			Size:            4096,
			ThumbnailURL:    "/p/thumb/large-video",
			PreviewLocal:    largePreview,
			PreviewStatus:   "ready",
			PublishedAt:     now.Add(time.Second),
			CreatedAt:       now.Add(time.Second),
			UpdatedAt:       now.Add(time.Second),
		},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed %s: %v", v.ID, err)
		}
	}

	app := &App{
		cfg: &config.Config{Storage: config.Storage{LocalPreviewDir: localDir}},
		cat: cat,
	}
	if err := app.cleanupDuplicateVideoAssets(ctx); err != nil {
		t.Fatalf("cleanup duplicate video assets: %v", err)
	}

	if _, err := cat.GetVideo(ctx, "small-video"); err != sql.ErrNoRows {
		t.Fatalf("small duplicate lookup error = %v, want sql.ErrNoRows", err)
	}
	deleted, err := cat.IsVideoDeleted(ctx, "small-video")
	if err != nil {
		t.Fatalf("check small tombstone: %v", err)
	}
	if !deleted {
		t.Fatalf("small duplicate tombstone missing")
	}
	deletedItems, _, err := cat.ListDeletedVideos(ctx, catalog.ListParams{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("list deleted videos: %v", err)
	}
	if len(deletedItems) != 1 ||
		deletedItems[0].ID != "small-video" ||
		deletedItems[0].Reason != catalog.DeletedVideoReasonDuplicate ||
		deletedItems[0].CanonicalVideoID != "large-video" ||
		deletedItems[0].RestorePolicy != catalog.DeletedVideoRestorePolicyNone {
		t.Fatalf("small duplicate tombstone = %#v, want reason %q", deletedItems, catalog.DeletedVideoReasonDuplicate)
	}
	large, err := cat.GetVideo(ctx, "large-video")
	if err != nil {
		t.Fatalf("large canonical missing: %v", err)
	}
	if large.Size != 4096 {
		t.Fatalf("large canonical size = %d, want 4096", large.Size)
	}
	for _, path := range []string{smallPreview, smallThumb} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("small duplicate asset %s still exists, stat err=%v", path, err)
		}
	}
	for _, path := range []string{largePreview, largeThumb} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("large canonical asset %s missing: %v", path, err)
		}
	}
}

func writeSolidJPEG(t *testing.T, path string, c color.RGBA) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
}

type serverFakeTeaserGenerator struct {
	mu     sync.Mutex
	events []string
}

func (g *serverFakeTeaserGenerator) record(event string) {
	g.mu.Lock()
	g.events = append(g.events, event)
	g.mu.Unlock()
}

func (g *serverFakeTeaserGenerator) Events() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.events...)
}

func (g *serverFakeTeaserGenerator) Probe(context.Context, *drives.StreamLink) (float64, error) {
	return 30, nil
}

func (g *serverFakeTeaserGenerator) Generate(context.Context, *drives.StreamLink, float64) (string, error) {
	g.record("preview")
	return "/tmp/source-teaser.mp4", nil
}

func (g *serverFakeTeaserGenerator) MoveToLocal(_ string, videoID string) (string, error) {
	g.mu.Lock()
	if len(g.events) > 0 && g.events[len(g.events)-1] == "preview" {
		g.events[len(g.events)-1] = "preview:" + videoID
	}
	g.mu.Unlock()
	return "/tmp/" + videoID + ".mp4", nil
}

func (g *serverFakeTeaserGenerator) GenerateThumbnail(_ context.Context, _ *drives.StreamLink, videoID string, _ float64) (string, error) {
	g.record("thumb:" + videoID)
	return "/tmp/" + videoID + ".jpg", nil
}

type serverBlockingThumbGenerator struct {
	serverFakeTeaserGenerator
	started chan string
	release chan struct{}
}

func (g *serverBlockingThumbGenerator) GenerateThumbnail(ctx context.Context, _ *drives.StreamLink, videoID string, _ float64) (string, error) {
	g.record("thumb:" + videoID)
	if g.started != nil {
		select {
		case g.started <- videoID:
		default:
		}
	}
	select {
	case <-g.release:
		return "/tmp/" + videoID + ".jpg", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

type serverFakeDrive struct{}

func (d *serverFakeDrive) Kind() string { return "fake" }
func (d *serverFakeDrive) ID() string   { return "drive-id" }
func (d *serverFakeDrive) Init(context.Context) error {
	return nil
}
func (d *serverFakeDrive) List(context.Context, string) ([]drives.Entry, error) {
	return nil, nil
}
func (d *serverFakeDrive) Stat(context.Context, string) (*drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *serverFakeDrive) StreamURL(context.Context, string) (*drives.StreamLink, error) {
	return &drives.StreamLink{URL: "https://video.example/clip.mp4"}, nil
}
func (d *serverFakeDrive) Upload(context.Context, string, string, io.Reader, int64) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *serverFakeDrive) EnsureDir(context.Context, string) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *serverFakeDrive) RootID() string { return "root" }

type serverFakeKindDrive struct {
	serverFakeDrive
	id   string
	kind string
}

func (d *serverFakeKindDrive) Kind() string { return d.kind }
func (d *serverFakeKindDrive) ID() string   { return d.id }

type serverListResultDrive struct {
	serverFakeDrive
	entries []drives.Entry
	err     error
}

func (d *serverListResultDrive) List(context.Context, string) ([]drives.Entry, error) {
	return d.entries, d.err
}

type serverRemovableFakeDrive struct {
	serverFakeDrive
	id            string
	removedFileID string
}

func (d *serverRemovableFakeDrive) Kind() string { return "fake-removable" }
func (d *serverRemovableFakeDrive) ID() string   { return d.id }
func (d *serverRemovableFakeDrive) Remove(ctx context.Context, fileID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.removedFileID = fileID
	return nil
}

type serverSourceRemovableFakeDrive struct {
	serverFakeDrive
	id                   string
	removedSource        drives.SourceFile
	fallbackRemoveCalled bool
}

func (d *serverSourceRemovableFakeDrive) Kind() string { return "fake-source-removable" }
func (d *serverSourceRemovableFakeDrive) ID() string   { return d.id }
func (d *serverSourceRemovableFakeDrive) RemoveSource(ctx context.Context, source drives.SourceFile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.removedSource = source
	return nil
}
func (d *serverSourceRemovableFakeDrive) Remove(ctx context.Context, fileID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.fallbackRemoveCalled = true
	return nil
}

type serverFakeCrawlerUploadRunner struct {
	called      atomic.Int32
	rejectStart atomic.Bool
	mu          sync.Mutex
	driveIDs    []string
}

func (r *serverFakeCrawlerUploadRunner) RunOnce(context.Context) error {
	r.called.Add(1)
	return nil
}

func (r *serverFakeCrawlerUploadRunner) StartDrive(_ context.Context, driveID string) (<-chan error, bool) {
	if r.rejectStart.Load() {
		return nil, false
	}
	r.called.Add(1)
	r.mu.Lock()
	r.driveIDs = append(r.driveIDs, driveID)
	r.mu.Unlock()
	done := make(chan error, 1)
	done <- nil
	close(done)
	return done, true
}

func (r *serverFakeCrawlerUploadRunner) lastDriveID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.driveIDs) == 0 {
		return ""
	}
	return r.driveIDs[len(r.driveIDs)-1]
}

type serverBlockingListDrive struct {
	id      string
	started chan string
	release chan struct{}
}

func (d *serverBlockingListDrive) Kind() string { return "fake" }
func (d *serverBlockingListDrive) ID() string   { return d.id }
func (d *serverBlockingListDrive) Init(context.Context) error {
	return nil
}
func (d *serverBlockingListDrive) List(ctx context.Context, _ string) ([]drives.Entry, error) {
	if d.started != nil {
		select {
		case d.started <- d.id:
		default:
		}
	}
	select {
	case <-d.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (d *serverBlockingListDrive) Stat(context.Context, string) (*drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *serverBlockingListDrive) StreamURL(context.Context, string) (*drives.StreamLink, error) {
	return &drives.StreamLink{URL: "https://video.example/clip.mp4"}, nil
}
func (d *serverBlockingListDrive) Upload(context.Context, string, string, io.Reader, int64) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *serverBlockingListDrive) EnsureDir(context.Context, string) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *serverBlockingListDrive) RootID() string { return "root" }

type serverFingerprintFakeDrive struct {
	serverFakeDrive
	path string
}

func (d *serverFingerprintFakeDrive) StreamURL(context.Context, string) (*drives.StreamLink, error) {
	return &drives.StreamLink{URL: d.path}, nil
}

type serverScanFingerprintFakeDrive struct {
	serverFingerprintFakeDrive
	entries []drives.Entry
}

func (d *serverScanFingerprintFakeDrive) List(context.Context, string) ([]drives.Entry, error) {
	return d.entries, nil
}

type serverLocalUploadFakeDrive struct {
	serverFakeDrive
}

func (d *serverLocalUploadFakeDrive) ID() string { return "local-upload" }

type serverRestorableLocalUploadDrive struct {
	serverFakeDrive
	mu            sync.Mutex
	entry         *drives.Entry
	statCalls     int
	removeCalls   int
	removeStarted chan struct{}
	allowRemove   <-chan struct{}
}

func (d *serverRestorableLocalUploadDrive) ID() string { return "local-upload" }

func (d *serverRestorableLocalUploadDrive) Stat(context.Context, string) (*drives.Entry, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.statCalls++
	if d.entry == nil {
		return nil, os.ErrNotExist
	}
	copy := *d.entry
	return &copy, nil
}

func (d *serverRestorableLocalUploadDrive) Remove(ctx context.Context, _ string) error {
	d.mu.Lock()
	d.removeCalls++
	removeStarted := d.removeStarted
	allowRemove := d.allowRemove
	d.mu.Unlock()
	if removeStarted != nil {
		select {
		case removeStarted <- struct{}{}:
		default:
		}
	}
	if allowRemove != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-allowRemove:
		}
	}
	d.mu.Lock()
	d.entry = nil
	d.mu.Unlock()
	return nil
}

// seedDriveWithTeaser 在 catalog 里 upsert 一个测试用的 drive 行，把 TeaserEnabled
// 设为 enabled。teaser 入队判断现在按 per-drive 而不是全局 setting，所以涉及到
// teaser worker 的测试都要先把 drive 行写进 catalog。
func seedDriveWithTeaser(t *testing.T, cat *catalog.Catalog, driveID string, enabled bool) {
	t.Helper()
	if err := cat.UpsertDrive(context.Background(), &catalog.Drive{
		ID:            driveID,
		Kind:          "fake",
		Name:          driveID,
		RootID:        "0",
		TeaserEnabled: enabled,
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}
}
