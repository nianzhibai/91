package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/config"
	"github.com/video-site/backend/internal/mediaasset"
	"github.com/video-site/backend/internal/videoid"
	"gopkg.in/yaml.v3"
)

type testBackupEnv struct {
	root       string
	configPath string
	cfg        *config.Config
	cat        *catalog.Catalog
	manager    *Manager
}

func newTestBackupEnv(t *testing.T) *testBackupEnv {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Config{
		Server: config.Server{
			Listen:         "127.0.0.1:9192",
			AllowedOrigins: []string{"https://source.example"},
		},
		Storage: config.Storage{
			DataDir:         root,
			DBPath:          filepath.Join(root, "video-site.db"),
			LocalPreviewDir: filepath.Join(root, "previews"),
		},
		Preview: config.Preview{
			Enabled:     true,
			FFmpegPath:  "/source/bin/ffmpeg",
			FFprobePath: "/source/bin/ffprobe",
		},
	}
	configPath := filepath.Join(root, "config.yaml")
	writeTestConfig(t, configPath, cfg)
	if err := os.MkdirAll(cfg.Storage.LocalPreviewDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Open(cfg.Storage.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{
		Catalog:        cat,
		AppConfig:      cfg,
		ConfigPath:     configPath,
		AppVersion:     "v1.2.3",
		RestartManaged: true,
	})
	if err != nil {
		_ = cat.Close()
		t.Fatal(err)
	}
	env := &testBackupEnv{
		root:       root,
		configPath: configPath,
		cfg:        cfg,
		cat:        cat,
		manager:    manager,
	}
	t.Cleanup(func() {
		manager.Close()
		_ = cat.Close()
	})
	return env
}

func writeTestConfig(t *testing.T, path string, cfg *config.Config) {
	t.Helper()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeTestFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func createAndWaitForBackup(t *testing.T, manager *Manager, selection ...BackupSelection) BackupRecord {
	t.Helper()
	if _, err := manager.Create(context.Background(), selection...); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		status := manager.Current()
		if status != nil && status.State == "completed" {
			result, err := manager.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Backups) == 0 {
				t.Fatal("backup task completed without an archive")
			}
			return result.Backups[0]
		}
		if status != nil && (status.State == "failed" || status.State == "canceled") {
			t.Fatalf("backup task ended as %s: %s", status.State, status.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for backup")
	return BackupRecord{}
}

func TestCatalogBackupToIncludesUncheckpointedWALData(t *testing.T) {
	env := newTestBackupEnv(t)
	if _, err := env.cat.CreateUser(context.Background(), "wal-user", "hash", "user"); err != nil {
		t.Fatal(err)
	}
	walInfo, err := os.Stat(env.cfg.Storage.DBPath + "-wal")
	if err != nil || walInfo.Size() == 0 {
		t.Fatalf("expected non-empty WAL before snapshot: info=%v err=%v", walInfo, err)
	}
	snapshot := filepath.Join(env.root, "snapshot.sqlite")
	if err := env.cat.BackupTo(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", snapshot+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE username = 'wal-user'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("snapshot user count = %d, want 1", count)
	}
}

func TestSelectiveBackupRemovesExcludedSecretsFromSQLiteBytes(t *testing.T) {
	env := newTestBackupEnv(t)
	ctx := context.Background()
	passwordSecret := "excluded-user-password-" + strings.Repeat("p", 3000)
	driveSecret := "excluded-local-drive-token-" + strings.Repeat("d", 3000)
	if _, err := env.cat.CreateUser(ctx, "excluded-user", passwordSecret, "user"); err != nil {
		t.Fatal(err)
	}
	if err := env.cat.UpsertDrive(ctx, &catalog.Drive{
		ID:     "excluded-local-drive",
		Kind:   "localstorage",
		Name:   "Excluded Local Drive",
		RootID: "/",
		Credentials: map[string]string{
			"path":  filepath.Join(env.root, "excluded-local-storage"),
			"token": driveSecret,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.cat.UpsertDrive(ctx, &catalog.Drive{
		ID:     "selected-cloud-drive",
		Kind:   "quark",
		Name:   "Selected Cloud Drive",
		RootID: "0",
	}); err != nil {
		t.Fatal(err)
	}

	record := createAndWaitForBackup(t, env.manager, BackupSelection{CloudDrives: true})
	archivePath, _, err := env.manager.resolveBackup(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var databaseBytes []byte
	for _, file := range reader.File {
		if file.Name == "payload/database.sqlite" {
			databaseBytes = readZipFile(t, file)
			break
		}
	}
	if len(databaseBytes) == 0 {
		t.Fatal("backup database is missing")
	}
	for label, secret := range map[string]string{
		"excluded user password":    passwordSecret,
		"excluded drive credential": driveSecret,
	} {
		if bytes.Contains(databaseBytes, []byte(secret)) {
			t.Fatalf("%s remains recoverable from filtered SQLite bytes", label)
		}
	}
}

func TestSelectedFileSnapshotDoesNotFollowEscapingParentSymlink(t *testing.T) {
	root := filepath.Join(t.TempDir(), "selected-root")
	destination := filepath.Join(t.TempDir(), "snapshot")
	outside := filepath.Join(t.TempDir(), "outside")
	writeTestFile(t, filepath.Join(outside, "secret.mp4"), []byte("outside-secret"))
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escaped")); err != nil {
		t.Fatal(err)
	}
	if err := snapshotSelectedFile(
		context.Background(),
		root,
		destination,
		filepath.Join(root, "escaped", "secret.mp4"),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "escaped", "secret.mp4")); !os.IsNotExist(err) {
		t.Fatalf("escaping symlink content was copied into snapshot: %v", err)
	}
}

func TestFullBackupContainsPersistentFilesAndExcludesTemporaryData(t *testing.T) {
	env := newTestBackupEnv(t)
	ctx := context.Background()
	adminID, err := env.cat.CreateUser(ctx, "backup-admin", "admin-password-hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.cat.CreateUser(ctx, "backup-user", "user-password-hash", "user"); err != nil {
		t.Fatal(err)
	}
	if err := env.cat.CreateSession(ctx, "backup-session", time.Hour, adminID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := env.cat.RecordLoginAttempt(ctx, "203.0.113.31", false, time.Now(), 30*time.Minute, 3); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(env.root, "previews", "cover.jpg"), []byte("cover"))
	writeTestFile(t, filepath.Join(env.root, "previews", "teaser.mp4"), []byte("teaser"))
	writeTestFile(t, filepath.Join(env.root, "previews", "framesigs", "video.fsig"), []byte("framesig"))
	writeTestFile(t, filepath.Join(env.root, "uploads", "upload.mp4"), []byte("upload"))
	writeTestFile(t, filepath.Join(env.root, "crawler-scripts", "crawler.py"), []byte("print('ok')"))
	writeTestFile(t, filepath.Join(env.root, "crawler-scripts", "import-revision", "91Porn.py"), []byte("print('versioned script')"))
	writeTestFile(t, filepath.Join(env.root, "scriptcrawlers", "demo", "videos", "crawl.mp4"), []byte("crawl"))
	writeTestFile(t, filepath.Join(env.root, "spider91", "legacy.mp4"), []byte("legacy"))
	writeTestFile(t, filepath.Join(env.root, "previews", "ignored.part"), []byte("partial"))
	writeTestFile(t, filepath.Join(env.root, "crawler-scripts", "__pycache__", "ignored.pyc"), []byte("cache"))
	writeTestFile(t, filepath.Join(env.root, "upload-tmp", "ignored.mp4"), []byte("temp"))
	outside := filepath.Join(env.root, "outside-secret")
	writeTestFile(t, outside, []byte("secret"))
	if err := os.Symlink(outside, filepath.Join(env.root, "previews", "linked-secret")); err != nil {
		t.Fatal(err)
	}

	record := createAndWaitForBackup(t, env.manager)
	if !strings.HasPrefix(record.Name, backupNamePrefix) ||
		strings.HasPrefix(record.Name, legacyBackupNamePrefix) {
		t.Fatalf("new backup name = %q, want current neutral prefix", record.Name)
	}
	archivePath, _, err := env.manager.resolveBackup(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	files := make(map[string]*zip.File)
	for _, file := range reader.File {
		files[file.Name] = file
	}
	for _, expected := range []string{
		"manifest.json",
		"payload/database.sqlite",
		"payload/previews/cover.jpg",
		"payload/previews/teaser.mp4",
		"payload/previews/framesigs/video.fsig",
		"payload/uploads/upload.mp4",
		"payload/crawler-scripts/crawler.py",
		"payload/crawler-scripts/import-revision/91Porn.py",
		"payload/scriptcrawlers/demo/videos/crawl.mp4",
		"payload/spider91/legacy.mp4",
	} {
		if files[expected] == nil {
			t.Errorf("archive is missing %s", expected)
		}
	}
	for _, excluded := range []string{
		"payload/config.yaml",
		"payload/previews/ignored.part",
		"payload/crawler-scripts/__pycache__/ignored.pyc",
		"payload/previews/linked-secret",
	} {
		if files[excluded] != nil {
			t.Errorf("archive unexpectedly contains %s", excluded)
		}
	}
	if record.VerificationStatus != "verified" || !validSHA256(record.SHA256) {
		t.Fatalf("record verification = %q sha=%q", record.VerificationStatus, record.SHA256)
	}

	databasePath := filepath.Join(t.TempDir(), "database.sqlite")
	if err := os.WriteFile(databasePath, readZipFile(t, files["payload/database.sqlite"]), 0o600); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", databasePath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var adminCount, userCount, sessionCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin'`).Scan(&adminCount); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM users WHERE username = 'backup-user' AND role = 'user'`).Scan(&userCount); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM admin_sessions`).Scan(&sessionCount); err != nil {
		t.Fatal(err)
	}
	var loginFailuresTable, loginBans int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'login_failures'`).Scan(&loginFailuresTable); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM banned_login_ips`).Scan(&loginBans); err != nil {
		t.Fatal(err)
	}
	if loginFailuresTable != 0 || loginBans != 0 {
		t.Fatalf("backup retained login protection: failures table=%d bans=%d", loginFailuresTable, loginBans)
	}
	if banned, err := env.cat.IsLoginIPBanned(ctx, "203.0.113.31"); err != nil || !banned {
		t.Fatalf("backup changed live login protection: banned=%v err=%v", banned, err)
	}
	if adminCount != 1 || userCount != 1 || sessionCount != 0 {
		t.Fatalf(
			"user-info snapshot has admins=%d users=%d sessions=%d, want 1/1/0",
			adminCount,
			userCount,
			sessionCount,
		)
	}

	manifest, err := InspectArchive(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Selection == nil || !manifest.Selection.UserInfo {
		t.Fatalf("default manifest selection = %+v", manifest.Selection)
	}
	liveConfig, err := config.Load(env.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if liveConfig.Server.Listen != env.cfg.Server.Listen {
		t.Fatalf("creating a backup changed the live server config: %+v", liveConfig.Server)
	}
}

func TestUserInfoBackupMergesAccountsByUsername(t *testing.T) {
	env := newTestBackupEnv(t)
	ctx := context.Background()

	sourceAdminID, err := env.cat.CreateUser(ctx, "source-admin-user", "source-admin-hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	sourceUserID, err := env.cat.CreateUser(ctx, "source-regular-user", "source-user-hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	conflictingSourceID, err := env.cat.CreateUser(ctx, "same-user", "source-conflict-hash", "user")
	if err != nil {
		t.Fatal(err)
	}

	record := createAndWaitForBackup(t, env.manager, BackupSelection{UserInfo: true})
	archivePath, _, err := env.manager.resolveBackup(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := InspectArchive(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Selection == nil || !manifest.Selection.UserInfo ||
		manifestIncludes(manifest, "config") {
		t.Fatalf("user-info manifest = %+v", manifest)
	}

	for _, id := range []int64{sourceAdminID, sourceUserID, conflictingSourceID} {
		if err := env.cat.DeleteUser(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	targetConflictID, err := env.cat.CreateUser(ctx, "SAME-USER", "target-conflict-hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.cat.SetUserBanned(ctx, targetConflictID, true); err != nil {
		t.Fatal(err)
	}
	targetOnlyID, err := env.cat.CreateUser(ctx, "target-only-user", "target-only-hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.cat.CreateSession(ctx, "target-user-info-session", time.Hour, targetOnlyID); err != nil {
		t.Fatal(err)
	}

	if _, err := env.manager.PrepareRestore(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	env.manager.Close()
	if err := env.cat.Close(); err != nil {
		t.Fatal(err)
	}
	applied, err := ApplyPendingRestore(env.root)
	if err != nil {
		t.Fatal(err)
	}
	if applied == nil {
		t.Fatal("pending user-info restore was not applied")
	}
	restored, err := catalog.Open(env.cfg.Storage.DBPath)
	if err != nil {
		_ = RollbackAppliedRestore(applied, err)
		t.Fatal(err)
	}
	defer restored.Close()
	if err := CommitAppliedRestore(applied); err != nil {
		t.Fatal(err)
	}

	for _, expected := range []struct {
		username string
		password string
		role     string
	}{
		{username: "source-admin-user", password: "source-admin-hash", role: "admin"},
		{username: "source-regular-user", password: "source-user-hash", role: "user"},
		{username: "target-only-user", password: "target-only-hash", role: "user"},
	} {
		actual, err := restored.GetUserByUsername(ctx, expected.username)
		if err != nil {
			t.Fatal(err)
		}
		if actual.Password != expected.password || actual.Role != expected.role {
			t.Fatalf("user %q = %+v", expected.username, actual)
		}
	}
	conflictingTarget, err := restored.GetUserByUsername(ctx, "same-user")
	if err != nil {
		t.Fatal(err)
	}
	if conflictingTarget.Username != "SAME-USER" ||
		conflictingTarget.Password != "target-conflict-hash" ||
		conflictingTarget.Role != "admin" ||
		!conflictingTarget.Banned {
		t.Fatalf("target account was overwritten by same-name source account: %+v", conflictingTarget)
	}
	if valid, _, err := restored.ValidateSession(ctx, "target-user-info-session"); err != nil || valid {
		t.Fatalf("restored session valid=%v err=%v, want cleared", valid, err)
	}
}

func TestSelectiveCloudBackupRestoresOnlySelectedResources(t *testing.T) {
	env := newTestBackupEnv(t)
	ctx := context.Background()
	now := time.Now()
	if err := env.cat.UpsertDrive(ctx, &catalog.Drive{
		ID:     "cloud-drive",
		Kind:   "quark",
		Name:   "Cloud",
		RootID: "0",
	}); err != nil {
		t.Fatal(err)
	}
	cloudPreview := mediaasset.PreviewPath(env.cfg.Storage.LocalPreviewDir, "cloud-video")
	writeTestFile(t, cloudPreview, []byte("cloud-backup-preview"))
	if err := env.cat.UpsertVideo(ctx, &catalog.Video{
		ID:            "cloud-video",
		DriveID:       "cloud-drive",
		FileID:        "cloud-file",
		Title:         "cloud at backup time",
		PreviewLocal:  cloudPreview,
		PreviewStatus: "ready",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "upload-before-backup",
		DriveID:     "local-upload",
		FileID:      "before.mp4",
		Title:       "upload before backup",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(env.root, "uploads", "before.mp4"), []byte("before"))

	record := createAndWaitForBackup(t, env.manager, BackupSelection{CloudDrives: true})
	archivePath, _, err := env.manager.resolveBackup(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := InspectArchive(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Selection == nil || !manifest.Selection.CloudDrives || manifest.Selection.UserInfo ||
		manifestIncludes(manifest, "config") || manifestIncludes(manifest, "uploads") {
		t.Fatalf("selective manifest = %+v", manifest)
	}

	if err := env.cat.UpsertVideo(ctx, &catalog.Video{
		ID:            "cloud-video",
		DriveID:       "cloud-drive",
		FileID:        "cloud-file",
		Title:         "cloud after backup",
		PreviewLocal:  cloudPreview,
		PreviewStatus: "ready",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, cloudPreview, []byte("cloud-target-preview"))
	if err := env.cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "upload-after-backup",
		DriveID:     "local-upload",
		FileID:      "after.mp4",
		Title:       "upload after backup",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(env.root, "uploads", "after.mp4"), []byte("after"))
	env.cfg.Server.Listen = "127.0.0.1:9933"
	writeTestConfig(t, env.configPath, env.cfg)

	if _, err := env.manager.PrepareRestore(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	env.manager.Close()
	if err := env.cat.Close(); err != nil {
		t.Fatal(err)
	}
	applied, err := ApplyPendingRestore(env.root)
	if err != nil {
		t.Fatal(err)
	}
	if applied == nil {
		t.Fatal("pending selective restore was not applied")
	}
	restored, err := catalog.Open(env.cfg.Storage.DBPath)
	if err != nil {
		_ = RollbackAppliedRestore(applied, err)
		t.Fatal(err)
	}
	defer restored.Close()
	if err := CommitAppliedRestore(applied); err != nil {
		t.Fatal(err)
	}
	cloud, err := restored.GetVideo(ctx, "cloud-video")
	if err != nil {
		t.Fatal(err)
	}
	if cloud.Title != "cloud at backup time" {
		t.Fatalf("cloud title = %q", cloud.Title)
	}
	for _, id := range []string{"upload-before-backup", "upload-after-backup"} {
		if _, err := restored.GetVideo(ctx, id); err != nil {
			t.Fatalf("unselected upload %s was not preserved: %v", id, err)
		}
	}
	if body, err := os.ReadFile(filepath.Join(env.root, "uploads", "after.mp4")); err != nil || string(body) != "after" {
		t.Fatalf("unselected upload file = %q err=%v", body, err)
	}
	if body, err := os.ReadFile(cloudPreview); err != nil || string(body) != "cloud-backup-preview" {
		t.Fatalf("selected preview file = %q err=%v", body, err)
	}
	restoredConfig, err := config.Load(env.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if restoredConfig.Server.Listen != "127.0.0.1:9933" {
		t.Fatalf("unselected config was replaced: %+v", restoredConfig.Server)
	}
}

func TestPreparedRestoreBlocksWritesAfterTargetSnapshot(t *testing.T) {
	env := newTestBackupEnv(t)
	record := createAndWaitForBackup(t, env.manager, BackupSelection{CloudDrives: true})
	if _, err := env.manager.PrepareRestore(context.Background(), record.ID); err != nil {
		t.Fatal(err)
	}
	var marker restoreMarker
	if err := readJSONFile(env.manager.pendingPath, &marker); err != nil {
		t.Fatal(err)
	}
	if len(marker.Report.Manifest.Files) != 0 {
		t.Fatalf("durable restore marker retained %d per-file manifest entries", len(marker.Report.Manifest.Files))
	}

	writeCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := env.cat.CreateUser(writeCtx, "post-prepare-user", "hash", "user"); err == nil {
		t.Fatal("catalog write succeeded after the restore target snapshot")
	}
}

func TestRestoreRejectsCrossKindDriveIDConflict(t *testing.T) {
	env := newTestBackupEnv(t)
	ctx := context.Background()
	now := time.Now()
	if err := env.cat.UpsertDrive(ctx, &catalog.Drive{
		ID: "shared-drive-id", Kind: "quark", Name: "Source Cloud", RootID: "0",
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.cat.UpsertVideo(ctx, &catalog.Video{
		ID: "source-cloud-video", DriveID: "shared-drive-id", FileID: "source-file",
		Title: "Source Cloud Video", PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	record := createAndWaitForBackup(t, env.manager, BackupSelection{CloudDrives: true})

	if err := env.cat.UpsertDrive(ctx, &catalog.Drive{
		ID: "shared-drive-id", Kind: "scriptcrawler", Name: "Target Crawler", RootID: "0",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.manager.PrepareRestore(ctx, record.ID); err == nil ||
		!strings.Contains(err.Error(), "conflicts between restored kind") {
		t.Fatalf("cross-kind drive restore error = %v", err)
	}
	if _, err := os.Stat(env.manager.pendingPath); !os.IsNotExist(err) {
		t.Fatalf("failed restore left a pending marker: %v", err)
	}
	if _, err := env.cat.CreateUser(ctx, "write-after-conflict", "hash", "user"); err != nil {
		t.Fatalf("failed restore did not release its maintenance barrier: %v", err)
	}
}

func TestRestorePreservesVisitReactionIdempotency(t *testing.T) {
	env := newTestBackupEnv(t)
	ctx := context.Background()
	now := time.Now()
	if err := env.cat.UpsertDrive(ctx, &catalog.Drive{
		ID: "reaction-cloud-drive", Kind: "quark", Name: "Reaction Cloud", RootID: "0",
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.cat.UpsertVideo(ctx, &catalog.Video{
		ID: "reaction-video", DriveID: "reaction-cloud-drive", FileID: "reaction-file",
		Title: "Reaction Video", Size: 1, Ext: "mp4", PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	const visitID = "restore-visit-000000000001"
	if result, err := env.cat.SetVisitReaction(ctx, "reaction-video", visitID, catalog.VideoReactionLike); err != nil || result.Likes != 1 {
		t.Fatalf("initial reaction = %+v err=%v", result, err)
	}
	record := createAndWaitForBackup(t, env.manager, BackupSelection{CloudDrives: true})
	if _, err := env.manager.PrepareRestore(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	env.manager.Close()
	if err := env.cat.Close(); err != nil {
		t.Fatal(err)
	}
	applied, err := ApplyPendingRestore(env.root)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := catalog.Open(env.cfg.Storage.DBPath)
	if err != nil {
		_ = RollbackAppliedRestore(applied, err)
		t.Fatal(err)
	}
	defer restored.Close()
	if err := CommitAppliedRestore(applied); err != nil {
		t.Fatal(err)
	}
	restoredVideo, err := restored.GetVideo(ctx, "reaction-video")
	if err != nil {
		t.Fatalf("restored reaction video: %v", err)
	}
	if restoredVideo.Hidden {
		t.Fatalf("restored reaction video is unexpectedly hidden: %+v", restoredVideo)
	}
	result, err := restored.SetVisitReaction(ctx, "reaction-video", visitID, catalog.VideoReactionLike)
	if err != nil {
		t.Fatal(err)
	}
	if result.Likes != 1 || result.Reaction != catalog.VideoReactionLike {
		t.Fatalf("replayed restored reaction = %+v, want one idempotent like", result)
	}
}

func TestSelectiveLocalStorageBackupCopiesOnlyReferencedVideos(t *testing.T) {
	env := newTestBackupEnv(t)
	ctx := context.Background()
	now := time.Now()
	localRoot := filepath.Join(env.root, "external-local-storage")
	videoRelative := filepath.Join("videos", "movie.mp4")
	videoID := base64.RawURLEncoding.EncodeToString([]byte(filepath.ToSlash(videoRelative)))
	sourceOnlyRelative := filepath.Join("videos", "source-only.mp4")
	sourceOnlyFileID := base64.RawURLEncoding.EncodeToString([]byte(filepath.ToSlash(sourceOnlyRelative)))
	targetOnlyRelative := filepath.Join("videos", "target-only.mp4")
	targetOnlyFileID := base64.RawURLEncoding.EncodeToString([]byte(filepath.ToSlash(targetOnlyRelative)))
	sourceOnlyPreview := mediaasset.PreviewPath(env.cfg.Storage.LocalPreviewDir, "local-source-only")
	sourceOnlyThumbnail := mediaasset.ThumbnailPath(env.cfg.Storage.LocalPreviewDir, "local-source-only")
	writeTestFile(t, filepath.Join(localRoot, videoRelative), []byte("local-at-backup"))
	writeTestFile(t, filepath.Join(localRoot, sourceOnlyRelative), []byte("source-only-at-backup"))
	writeTestFile(t, filepath.Join(localRoot, "unrelated.txt"), []byte("keep"))
	writeTestFile(t, sourceOnlyPreview, []byte("source-only-preview"))
	writeTestFile(t, sourceOnlyThumbnail, []byte("source-only-thumbnail"))
	if err := env.cat.UpsertDrive(ctx, &catalog.Drive{
		ID:     "local-drive",
		Kind:   "localstorage",
		Name:   "Local",
		RootID: "/",
		Credentials: map[string]string{
			"path": localRoot,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "local-video",
		DriveID:     "local-drive",
		FileID:      videoID,
		Title:       "local at backup time",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.cat.UpsertVideo(ctx, &catalog.Video{
		ID:            "local-source-only",
		DriveID:       "local-drive",
		FileID:        sourceOnlyFileID,
		Title:         "source only at backup time",
		ThumbnailURL:  "/p/thumb/local-source-only",
		PreviewLocal:  sourceOnlyPreview,
		PreviewStatus: "ready",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.cat.CreateTagAndClassify(ctx, "localrestore", "user"); err != nil {
		t.Fatal(err)
	}
	if err := env.cat.SetManualVideoTags(ctx, "local-source-only", []string{"localrestore"}); err != nil {
		t.Fatal(err)
	}

	record := createAndWaitForBackup(t, env.manager, BackupSelection{LocalStorage: true})
	archivePath, _, err := env.manager.resolveBackup(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := InspectArchive(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.LocalStorage) != 1 {
		t.Fatalf("local storage manifest = %+v", manifest.LocalStorage)
	}
	localArchivePath := "payload/localstorage/" + manifest.LocalStorage[0].ArchivePath + "/videos/movie.mp4"
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	archiveFiles := make(map[string]bool)
	for _, file := range reader.File {
		archiveFiles[file.Name] = true
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if !archiveFiles[localArchivePath] || archiveFiles["payload/localstorage/"+manifest.LocalStorage[0].ArchivePath+"/unrelated.txt"] {
		t.Fatalf("local storage archive files = %#v", archiveFiles)
	}

	if err := env.cat.DeleteVideo(ctx, "local-source-only"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(localRoot, sourceOnlyRelative)); err != nil {
		t.Fatal(err)
	}
	if err := env.cat.UpsertDrive(ctx, &catalog.Drive{
		ID:     "local-drive",
		Kind:   "localstorage",
		Name:   "Target Local",
		RootID: "/",
		Credentials: map[string]string{
			"path": localRoot,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "local-video",
		DriveID:     "local-drive",
		FileID:      videoID,
		Title:       "local after backup",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(localRoot, videoRelative), []byte("local-target"))
	if err := env.cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "local-target-only",
		DriveID:     "local-drive",
		FileID:      targetOnlyFileID,
		Title:       "target only",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(localRoot, targetOnlyRelative), []byte("target-only"))
	writeTestFile(t, filepath.Join(localRoot, "new-unrelated.txt"), []byte("keep-new"))
	if _, err := env.manager.PrepareRestore(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	env.manager.Close()
	if err := env.cat.Close(); err != nil {
		t.Fatal(err)
	}
	applied, err := ApplyPendingRestore(env.root)
	if err != nil {
		t.Fatal(err)
	}
	if applied == nil {
		t.Fatal("pending local storage restore was not applied")
	}
	restored, err := catalog.Open(env.cfg.Storage.DBPath)
	if err != nil {
		_ = RollbackAppliedRestore(applied, err)
		t.Fatal(err)
	}
	defer restored.Close()
	if err := CommitAppliedRestore(applied); err != nil {
		t.Fatal(err)
	}
	video, err := restored.GetVideo(ctx, "local-video")
	if err != nil {
		t.Fatal(err)
	}
	if video.Title != "local after backup" {
		t.Fatalf("local video title = %q", video.Title)
	}
	if body, err := os.ReadFile(filepath.Join(localRoot, videoRelative)); err != nil || string(body) != "local-target" {
		t.Fatalf("restored local file = %q err=%v", body, err)
	}
	if _, err := restored.GetVideo(ctx, "local-target-only"); err != nil {
		t.Fatalf("target-only local video was lost: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(localRoot, targetOnlyRelative)); err != nil || string(body) != "target-only" {
		t.Fatalf("target-only local file = %q err=%v", body, err)
	}
	localDrive, err := restored.GetDrive(ctx, "local-drive")
	if err != nil || localDrive.Name != "Target Local" {
		t.Fatalf("target local drive = %+v err=%v", localDrive, err)
	}
	drives, err := restored.ListDrives(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var restoredDrive *catalog.Drive
	for _, drive := range drives {
		if drive != nil && drive.Kind == "localstorage" && drive.ID != "local-drive" {
			restoredDrive = drive
			break
		}
	}
	if restoredDrive == nil || !strings.HasPrefix(restoredDrive.Name, "恢复的本地存储 ") {
		t.Fatalf("isolated restored local drive = %+v", restoredDrive)
	}
	restoredRoot := restoredDrive.Credentials["path"]
	if restoredRoot == "" || restoredRoot == localRoot {
		t.Fatalf("restored local root = %q, original = %q", restoredRoot, localRoot)
	}
	for _, expected := range []struct {
		id       string
		relative string
		title    string
		content  string
	}{
		{
			id:       videoid.ForDrive("localstorage", restoredDrive.ID, videoID),
			relative: videoRelative,
			title:    "local at backup time",
			content:  "local-at-backup",
		},
		{
			id:       videoid.ForDrive("localstorage", restoredDrive.ID, sourceOnlyFileID),
			relative: sourceOnlyRelative,
			title:    "source only at backup time",
			content:  "source-only-at-backup",
		},
	} {
		restoredVideo, err := restored.GetVideo(ctx, expected.id)
		if err != nil || restoredVideo.DriveID != restoredDrive.ID || restoredVideo.Title != expected.title {
			t.Fatalf("isolated restored video %s = %+v err=%v", expected.id, restoredVideo, err)
		}
		if body, err := os.ReadFile(filepath.Join(restoredRoot, expected.relative)); err != nil || string(body) != expected.content {
			t.Fatalf("isolated restored file %s = %q err=%v", expected.relative, body, err)
		}
	}
	restoredSourceOnlyID := videoid.ForDrive("localstorage", restoredDrive.ID, sourceOnlyFileID)
	restoredSourceOnly, err := restored.GetVideo(ctx, restoredSourceOnlyID)
	if err != nil {
		t.Fatal(err)
	}
	if restoredSourceOnly.PreviewLocal != mediaasset.PreviewFilename(restoredSourceOnlyID) ||
		restoredSourceOnly.PreviewStatus != "ready" ||
		restoredSourceOnly.ThumbnailURL != "/p/thumb/"+restoredSourceOnlyID {
		t.Fatalf("restored local preview metadata = %+v", restoredSourceOnly)
	}
	for _, expected := range []struct {
		path    string
		content string
	}{
		{path: mediaasset.PreviewPath(env.cfg.Storage.LocalPreviewDir, restoredSourceOnlyID), content: "source-only-preview"},
		{path: mediaasset.ThumbnailPath(env.cfg.Storage.LocalPreviewDir, restoredSourceOnlyID), content: "source-only-thumbnail"},
	} {
		if body, err := os.ReadFile(expected.path); err != nil || string(body) != expected.content {
			t.Fatalf("restored local preview asset %s = %q err=%v", expected.path, body, err)
		}
	}
	metadata, err := restored.ListVideoTagMetadata(ctx, []string{restoredSourceOnlyID})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := metadata[restoredSourceOnlyID]["localrestore"]; !ok {
		t.Fatalf("restored local video tag mapping = %#v", metadata)
	}
	for _, relative := range []string{targetOnlyRelative, "unrelated.txt", "new-unrelated.txt"} {
		if _, err := os.Stat(filepath.Join(restoredRoot, relative)); !os.IsNotExist(err) {
			t.Fatalf("unselected file %s appeared in isolated restore: %v", relative, err)
		}
	}
	for _, name := range []string{"unrelated.txt", "new-unrelated.txt"} {
		if _, err := os.Stat(filepath.Join(localRoot, name)); err != nil {
			t.Fatalf("unrelated local file %s was lost: %v", name, err)
		}
	}
}

func TestLocalStorageRestorePreservesSourceDriveNamespaces(t *testing.T) {
	env := newTestBackupEnv(t)
	ctx := context.Background()
	now := time.Now()
	type sourceVideo struct {
		driveID  string
		root     string
		relative string
		fileID   string
		videoID  string
		content  string
	}
	sources := []sourceVideo{
		{
			driveID:  "local-source-a",
			root:     filepath.Join(env.root, "source-a"),
			relative: filepath.Join("videos", "movie.mp4"),
			videoID:  "source-video-a",
			content:  "video-a",
		},
		{
			driveID:  "local-source-b",
			root:     filepath.Join(env.root, "source-b"),
			relative: filepath.Join("videos", "movie.mp4"),
			videoID:  "source-video-b",
			content:  "video-b",
		},
	}
	for index := range sources {
		source := &sources[index]
		source.fileID = base64.RawURLEncoding.EncodeToString([]byte(filepath.ToSlash(source.relative)))
		writeTestFile(t, filepath.Join(source.root, source.relative), []byte(source.content))
		if err := env.cat.UpsertDrive(ctx, &catalog.Drive{
			ID:     source.driveID,
			Kind:   "localstorage",
			Name:   source.driveID,
			RootID: "/",
			Credentials: map[string]string{
				"path": source.root,
			},
		}); err != nil {
			t.Fatal(err)
		}
		if err := env.cat.UpsertVideo(ctx, &catalog.Video{
			ID:          source.videoID,
			DriveID:     source.driveID,
			FileID:      source.fileID,
			Title:       source.videoID,
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatal(err)
		}
	}

	record := createAndWaitForBackup(t, env.manager, BackupSelection{LocalStorage: true})
	if _, err := env.manager.PrepareRestore(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	env.manager.Close()
	if err := env.cat.Close(); err != nil {
		t.Fatal(err)
	}
	applied, err := ApplyPendingRestore(env.root)
	if err != nil {
		t.Fatal(err)
	}
	if applied == nil {
		t.Fatal("pending multi-drive local restore was not applied")
	}
	restored, err := catalog.Open(env.cfg.Storage.DBPath)
	if err != nil {
		_ = RollbackAppliedRestore(applied, err)
		t.Fatal(err)
	}
	defer restored.Close()
	if err := CommitAppliedRestore(applied); err != nil {
		t.Fatal(err)
	}

	drives, err := restored.ListDrives(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var isolated []*catalog.Drive
	for _, drive := range drives {
		if drive != nil && strings.HasPrefix(drive.ID, "localstorage-restore-") {
			isolated = append(isolated, drive)
		}
	}
	if len(isolated) != len(sources) {
		t.Fatalf("isolated local drive count = %d, want %d", len(isolated), len(sources))
	}
	for _, source := range sources {
		if _, err := restored.GetDrive(ctx, source.driveID); err != nil {
			t.Fatalf("existing target local drive %s was removed: %v", source.driveID, err)
		}
		var restoredDrive *catalog.Drive
		for _, candidate := range isolated {
			body, err := os.ReadFile(filepath.Join(candidate.Credentials["path"], source.relative))
			if err == nil && string(body) == source.content {
				restoredDrive = candidate
				break
			}
		}
		if restoredDrive == nil {
			t.Fatalf("isolated local drive for %s was not found", source.driveID)
		}
		restoredID := videoid.ForDrive("localstorage", restoredDrive.ID, source.fileID)
		video, err := restored.GetVideo(ctx, restoredID)
		if err != nil || video.DriveID != restoredDrive.ID {
			t.Fatalf("restored video %s = %+v err=%v", restoredID, video, err)
		}
		if body, err := os.ReadFile(filepath.Join(restoredDrive.Credentials["path"], source.relative)); err != nil || string(body) != source.content {
			t.Fatalf("restored file %s = %q err=%v", source.relative, body, err)
		}
	}
}

func TestSelectiveUploadStorageRestoreMergesTargetContent(t *testing.T) {
	env := newTestBackupEnv(t)
	ctx := context.Background()
	now := time.Now()
	uploadRoot := filepath.Join(env.root, "uploads")
	sourcePreview := mediaasset.PreviewPath(env.cfg.Storage.LocalPreviewDir, "upload-source-only")
	conflictPreview := mediaasset.PreviewPath(env.cfg.Storage.LocalPreviewDir, "upload-conflict")
	targetPreview := mediaasset.PreviewPath(env.cfg.Storage.LocalPreviewDir, "upload-target-only")

	writeTestFile(t, filepath.Join(uploadRoot, "source-only.mp4"), []byte("source-only-at-backup"))
	writeTestFile(t, filepath.Join(uploadRoot, "conflict.mp4"), []byte("conflict-at-backup"))
	writeTestFile(t, sourcePreview, []byte("source-preview-at-backup"))
	writeTestFile(t, conflictPreview, []byte("conflict-preview-at-backup"))
	for _, video := range []*catalog.Video{
		{
			ID:            "upload-source-only",
			DriveID:       "local-upload",
			FileID:        "source-only.mp4",
			Title:         "backupmerge source",
			PreviewLocal:  sourcePreview,
			PreviewStatus: "ready",
			PublishedAt:   now,
			CreatedAt:     now,
			UpdatedAt:     now,
		},
		{
			ID:            "upload-conflict",
			DriveID:       "local-upload",
			FileID:        "conflict.mp4",
			Title:         "backupmerge conflict",
			PreviewLocal:  conflictPreview,
			PreviewStatus: "ready",
			PublishedAt:   now,
			CreatedAt:     now,
			UpdatedAt:     now,
		},
	} {
		if err := env.cat.UpsertVideo(ctx, video); err != nil {
			t.Fatal(err)
		}
	}
	backupTag, err := env.cat.EnsureTag(ctx, "backupmerge", "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.cat.ClassifyTagByID(ctx, backupTag.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.cat.CreateRemoteUploadJob(
		ctx,
		"source-upload-job",
		"https://source.example/source-only.mp4",
		"source-only.mp4",
		"Source job",
		nil,
	); err != nil {
		t.Fatal(err)
	}
	record := createAndWaitForBackup(t, env.manager, BackupSelection{UploadStorage: true})

	if err := env.cat.DeleteVideo(ctx, "upload-source-only"); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{filepath.Join(uploadRoot, "source-only.mp4"), sourcePreview} {
		if err := os.Remove(candidate); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.cat.DeleteTag(ctx, backupTag.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.cat.MarkRemoteUploadCanceled(ctx, "source-upload-job"); err != nil {
		t.Fatal(err)
	}
	if deleted, err := env.cat.DeleteExpiredRemoteUploadJobs(ctx, time.Now().Add(time.Hour)); err != nil || deleted != 1 {
		t.Fatalf("delete source upload job: deleted=%d err=%v", deleted, err)
	}

	if err := env.cat.UpsertVideo(ctx, &catalog.Video{
		ID:            "upload-conflict",
		DriveID:       "local-upload",
		FileID:        "conflict.mp4",
		Title:         "targetmerge conflict",
		PreviewLocal:  conflictPreview,
		PreviewStatus: "ready",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(uploadRoot, "conflict.mp4"), []byte("conflict-in-target"))
	writeTestFile(t, conflictPreview, []byte("conflict-preview-in-target"))
	if err := env.cat.UpsertVideo(ctx, &catalog.Video{
		ID:            "upload-target-only",
		DriveID:       "local-upload",
		FileID:        "target-only.mp4",
		Title:         "targetmerge only",
		PreviewLocal:  targetPreview,
		PreviewStatus: "ready",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(uploadRoot, "target-only.mp4"), []byte("target-only"))
	writeTestFile(t, targetPreview, []byte("target-only-preview"))
	if _, err := env.cat.CreateTagAndClassify(ctx, "targetmerge", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.cat.CreateRemoteUploadJob(
		ctx,
		"target-upload-job",
		"https://target.example/target-only.mp4",
		"target-only.mp4",
		"Target job",
		nil,
	); err != nil {
		t.Fatal(err)
	}

	if _, err := env.manager.PrepareRestore(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	env.manager.Close()
	if err := env.cat.Close(); err != nil {
		t.Fatal(err)
	}
	applied, err := ApplyPendingRestore(env.root)
	if err != nil {
		t.Fatal(err)
	}
	if applied == nil {
		t.Fatal("pending upload storage restore was not applied")
	}
	restored, err := catalog.Open(env.cfg.Storage.DBPath)
	if err != nil {
		_ = RollbackAppliedRestore(applied, err)
		t.Fatal(err)
	}
	defer restored.Close()
	if err := CommitAppliedRestore(applied); err != nil {
		t.Fatal(err)
	}

	conflict, err := restored.GetVideo(ctx, "upload-conflict")
	if err != nil || conflict.Title != "targetmerge conflict" {
		t.Fatalf("conflicting upload video = %+v err=%v", conflict, err)
	}
	for _, expected := range []struct {
		id      string
		file    string
		content string
	}{
		{id: "upload-source-only", file: "source-only.mp4", content: "source-only-at-backup"},
		{id: "upload-target-only", file: "target-only.mp4", content: "target-only"},
		{id: "upload-conflict", file: "conflict.mp4", content: "conflict-in-target"},
	} {
		if _, err := restored.GetVideo(ctx, expected.id); err != nil {
			t.Fatalf("merged upload video %s is missing: %v", expected.id, err)
		}
		if body, err := os.ReadFile(filepath.Join(uploadRoot, expected.file)); err != nil || string(body) != expected.content {
			t.Fatalf("merged upload file %s = %q err=%v", expected.file, body, err)
		}
	}
	for _, expected := range []struct {
		path    string
		content string
	}{
		{path: sourcePreview, content: "source-preview-at-backup"},
		{path: targetPreview, content: "target-only-preview"},
		{path: conflictPreview, content: "conflict-preview-in-target"},
	} {
		if body, err := os.ReadFile(expected.path); err != nil || string(body) != expected.content {
			t.Fatalf("merged preview %s = %q err=%v", expected.path, body, err)
		}
	}
	metadata, err := restored.ListVideoTagMetadata(ctx, []string{"upload-conflict"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := metadata["upload-conflict"]["backupmerge"]; !ok {
		t.Fatalf("backup tag was not merged: %#v", metadata)
	}
	if _, ok := metadata["upload-conflict"]["targetmerge"]; !ok {
		t.Fatalf("target tag was not preserved: %#v", metadata)
	}
	sourceJob, err := restored.GetRemoteUploadJob(ctx, "source-upload-job")
	if err != nil || sourceJob.State != catalog.RemoteUploadCanceled || sourceJob.SourceURL != "" || !sourceJob.CancelRequested {
		t.Fatalf("source upload job = %+v err=%v", sourceJob, err)
	}
	targetJob, err := restored.GetRemoteUploadJob(ctx, "target-upload-job")
	if err != nil || targetJob.State != catalog.RemoteUploadQueued || targetJob.SourceURL != "https://target.example/target-only.mp4" {
		t.Fatalf("target upload job = %+v err=%v", targetJob, err)
	}
}

func TestEmptyUploadAndLocalBackupDoesNotClearTargetContent(t *testing.T) {
	env := newTestBackupEnv(t)
	ctx := context.Background()
	record := createAndWaitForBackup(t, env.manager, BackupSelection{
		UploadStorage: true,
		LocalStorage:  true,
	})

	now := time.Now()
	writeTestFile(t, filepath.Join(env.root, "uploads", "target-upload.mp4"), []byte("target-upload"))
	if err := env.cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "target-upload-after-empty-backup",
		DriveID:     "local-upload",
		FileID:      "target-upload.mp4",
		Title:       "target upload",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatal(err)
	}
	localRoot := filepath.Join(env.root, "target-local-storage")
	localRelative := filepath.Join("videos", "target-local.mp4")
	localFileID := base64.RawURLEncoding.EncodeToString([]byte(filepath.ToSlash(localRelative)))
	writeTestFile(t, filepath.Join(localRoot, localRelative), []byte("target-local"))
	if err := env.cat.UpsertDrive(ctx, &catalog.Drive{
		ID:     "target-local-drive",
		Kind:   "localstorage",
		Name:   "Target Local Drive",
		RootID: "/",
		Credentials: map[string]string{
			"path": localRoot,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "target-local-after-empty-backup",
		DriveID:     "target-local-drive",
		FileID:      localFileID,
		Title:       "target local",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := env.manager.PrepareRestore(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	env.manager.Close()
	if err := env.cat.Close(); err != nil {
		t.Fatal(err)
	}
	applied, err := ApplyPendingRestore(env.root)
	if err != nil {
		t.Fatal(err)
	}
	if applied == nil {
		t.Fatal("pending empty resource restore was not applied")
	}
	restored, err := catalog.Open(env.cfg.Storage.DBPath)
	if err != nil {
		_ = RollbackAppliedRestore(applied, err)
		t.Fatal(err)
	}
	defer restored.Close()
	if err := CommitAppliedRestore(applied); err != nil {
		t.Fatal(err)
	}
	for _, videoID := range []string{"target-upload-after-empty-backup", "target-local-after-empty-backup"} {
		if _, err := restored.GetVideo(ctx, videoID); err != nil {
			t.Fatalf("target video %s was removed by empty backup: %v", videoID, err)
		}
	}
	if _, err := restored.GetDrive(ctx, "target-local-drive"); err != nil {
		t.Fatalf("target local drive was removed by empty backup: %v", err)
	}
	for _, expected := range []struct {
		path    string
		content string
	}{
		{path: filepath.Join(env.root, "uploads", "target-upload.mp4"), content: "target-upload"},
		{path: filepath.Join(localRoot, localRelative), content: "target-local"},
	} {
		if body, err := os.ReadFile(expected.path); err != nil || string(body) != expected.content {
			t.Fatalf("target file %s = %q err=%v", expected.path, body, err)
		}
	}
}

func TestBackupTaskIsExclusiveCancelableAndRejectsLowDisk(t *testing.T) {
	env := newTestBackupEnv(t)
	writeTestFile(t, filepath.Join(env.root, "uploads", "data.mp4"), bytes.Repeat([]byte("x"), 8<<20))
	if _, err := env.manager.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := env.manager.Create(context.Background()); !errors.Is(err, ErrTaskRunning) {
		t.Fatalf("second Create error = %v, want ErrTaskRunning", err)
	}
	if err := env.manager.Cancel(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status := env.manager.Current()
		if status != nil && status.State == "canceled" {
			break
		}
		if status != nil && status.State == "completed" {
			t.Fatal("backup completed despite immediate cancellation")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status := env.manager.Current(); status == nil || status.State != "canceled" {
		t.Fatalf("status after cancel = %+v", status)
	}

	env.manager.mu.Lock()
	env.manager.estimateUntil = time.Time{}
	env.manager.mu.Unlock()
	env.manager.availableBytes = func(string) (int64, error) { return 1, nil }
	if _, err := env.manager.Create(context.Background()); !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("low-disk Create error = %v, want ErrInsufficientSpace", err)
	}
}

func TestStartupCleanupAndUploadExpiryPreserveCompletedBackups(t *testing.T) {
	env := newTestBackupEnv(t)
	completedPath := filepath.Join(env.root, "backups", "video-site-91-full-kept.zip")
	writeTestFile(t, completedPath, []byte("completed"))
	interruptedPath := filepath.Join(env.root, "backups", "video-site-91-full-interrupted.zip.part")
	writeTestFile(t, interruptedPath, []byte("partial"))
	orphanSnapshot := filepath.Join(env.root, ".backup-snapshots", "orphan", "payload", "file")
	writeTestFile(t, orphanSnapshot, []byte("snapshot"))

	env.manager.Close()
	restarted, err := NewManager(Config{
		Catalog:    env.cat,
		AppConfig:  env.cfg,
		ConfigPath: env.configPath,
		AppVersion: "v1.2.3",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err := os.Stat(interruptedPath); !os.IsNotExist(err) {
		t.Fatalf("interrupted backup part still exists: %v", err)
	}
	if _, err := os.Stat(orphanSnapshot); !os.IsNotExist(err) {
		t.Fatalf("orphan snapshot still exists: %v", err)
	}
	if _, err := os.Stat(completedPath); err != nil {
		t.Fatalf("completed backup was removed during cleanup: %v", err)
	}

	now := time.Unix(1_800_000_000, 0).UTC()
	restarted.now = func() time.Time { return now }
	session, err := restarted.BeginUpload(context.Background(), BeginUploadInput{
		FileName: "unfinished.zip",
		Size:     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.FinalizeUpload(
		context.Background(), session.ID, strings.Repeat("0", 64),
	); !errors.Is(err, ErrUploadIncomplete) {
		t.Fatalf("incomplete finalize error = %v, want ErrUploadIncomplete", err)
	}
	now = now.Add(UploadTTL + time.Second)
	if err := restarted.cleanupExpiredUploads(); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.UploadStatus(session.ID); !errors.Is(err, ErrUploadNotFound) {
		t.Fatalf("expired upload status error = %v, want ErrUploadNotFound", err)
	}
	if _, err := os.Stat(completedPath); err != nil {
		t.Fatalf("completed backup was removed with expired upload: %v", err)
	}
}

func TestVerifyArchiveRejectsUnsafeDuplicateTamperedAndNewerArchives(t *testing.T) {
	env := newTestBackupEnv(t)
	writeTestFile(t, filepath.Join(env.root, "uploads", "safe.mp4"), []byte("safe"))
	record := createAndWaitForBackup(t, env.manager)
	validPath, _, err := env.manager.resolveBackup(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	var progressEvents []OperationProgress
	verified, err := VerifyArchive(context.Background(), validPath, VerifyOptions{
		CurrentVersion: "v1.2.3",
		Progress: func(progress OperationProgress) {
			progressEvents = append(progressEvents, progress)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(progressEvents) == 0 {
		t.Fatal("archive verification did not report progress")
	}
	lastProgress := progressEvents[len(progressEvents)-1]
	if lastProgress.Phase != "database" ||
		lastProgress.ProcessedBytes != verified.Manifest.TotalSize ||
		lastProgress.ProcessedFiles != verified.Manifest.FileCount {
		t.Fatalf("final archive progress = %+v, manifest = %+v", lastProgress, verified.Manifest)
	}

	traversal := filepath.Join(env.root, "traversal.zip")
	writeRawZip(t, traversal, []rawZipEntry{
		{name: "manifest.json", body: []byte(`{"formatVersion":3,"fileCount":1,"totalSize":1}`)},
		{name: "../escape", body: []byte("x")},
	})
	if _, err := VerifyArchive(context.Background(), traversal, VerifyOptions{CurrentVersion: "v1.2.3"}); err == nil {
		t.Fatal("path traversal archive was accepted")
	}

	duplicate := filepath.Join(env.root, "duplicate.zip")
	writeRawZip(t, duplicate, []rawZipEntry{
		{name: "manifest.json", body: []byte(`{"formatVersion":3,"fileCount":1,"totalSize":1}`)},
		{name: "payload/database.sqlite", body: []byte("a")},
		{name: "payload/database.sqlite", body: []byte("b")},
	})
	if _, err := VerifyArchive(context.Background(), duplicate, VerifyOptions{CurrentVersion: "v1.2.3"}); err == nil {
		t.Fatal("duplicate-entry archive was accepted")
	}

	symlink := filepath.Join(env.root, "symlink.zip")
	writeRawZip(t, symlink, []rawZipEntry{
		{name: "manifest.json", body: []byte(`{"formatVersion":3,"fileCount":1,"totalSize":6}`)},
		{name: "payload/previews/link", body: []byte("target"), mode: os.ModeSymlink | 0o777},
	})
	if _, err := VerifyArchive(context.Background(), symlink, VerifyOptions{CurrentVersion: "v1.2.3"}); err == nil ||
		!strings.Contains(err.Error(), "non-regular") {
		t.Fatalf("symlink archive error = %v", err)
	}

	corruptDatabase := filepath.Join(env.root, "corrupt-database.zip")
	writePayloadArchive(t, corruptDatabase, []rawZipEntry{
		{name: "payload/database.sqlite", body: []byte("not a sqlite database")},
	})
	if _, err := VerifyArchive(context.Background(), corruptDatabase, VerifyOptions{CurrentVersion: "v1.2.3"}); err == nil ||
		!strings.Contains(err.Error(), "SQLite") {
		t.Fatalf("corrupt database archive error = %v", err)
	}

	tampered := filepath.Join(env.root, "tampered.zip")
	rewriteZip(t, validPath, tampered, func(name string, body []byte) []byte {
		if name == "payload/database.sqlite" {
			return append(body, []byte("\n# tampered")...)
		}
		return body
	})
	if _, err := VerifyArchive(context.Background(), tampered, VerifyOptions{CurrentVersion: "v1.2.3"}); err == nil ||
		!strings.Contains(err.Error(), "SHA-256") && !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("tampered archive error = %v", err)
	}

	newer := filepath.Join(env.root, "newer.zip")
	rewriteZip(t, validPath, newer, func(name string, body []byte) []byte {
		if name != manifestName {
			return body
		}
		var manifest Manifest
		if err := json.Unmarshal(body, &manifest); err != nil {
			t.Fatal(err)
		}
		manifest.AppVersion = "v99.0.0"
		updated, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		return updated
	})
	if _, err := VerifyArchive(context.Background(), newer, VerifyOptions{CurrentVersion: "v1.2.3"}); err == nil ||
		!strings.Contains(err.Error(), "newer") {
		t.Fatalf("newer archive error = %v", err)
	}
}

func TestVerifyArchiveRejectsManifestAndDatabaseScopeMismatch(t *testing.T) {
	env := newTestBackupEnv(t)
	ctx := context.Background()
	writeTestFile(t, filepath.Join(env.root, "uploads", "scope.mp4"), []byte("scope"))
	fullRecord := createAndWaitForBackup(t, env.manager)
	fullPath, _, err := env.manager.resolveBackup(fullRecord.ID)
	if err != nil {
		t.Fatal(err)
	}
	inconsistentIncluded := filepath.Join(env.root, "inconsistent-included.zip")
	rewriteZip(t, fullPath, inconsistentIncluded, func(name string, body []byte) []byte {
		if name != manifestName {
			return body
		}
		var manifest Manifest
		if err := json.Unmarshal(body, &manifest); err != nil {
			t.Fatal(err)
		}
		manifest.Included = []string{"database"}
		updated, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		return updated
	})
	if _, err := VerifyArchive(ctx, inconsistentIncluded, VerifyOptions{CurrentVersion: "v1.2.3"}); err == nil {
		t.Fatal("archive with an incomplete included declaration was accepted")
	}

	if err := env.cat.UpsertDrive(ctx, &catalog.Drive{
		ID: "undeclared-cloud-drive", Kind: "quark", Name: "Undeclared Cloud", RootID: "0",
	}); err != nil {
		t.Fatal(err)
	}
	cloudRecord := createAndWaitForBackup(t, env.manager, BackupSelection{CloudDrives: true})
	cloudPath, _, err := env.manager.resolveBackup(cloudRecord.ID)
	if err != nil {
		t.Fatal(err)
	}
	databaseScopeMismatch := filepath.Join(env.root, "database-scope-mismatch.zip")
	rewriteZip(t, cloudPath, databaseScopeMismatch, func(name string, body []byte) []byte {
		if name != manifestName {
			return body
		}
		var manifest Manifest
		if err := json.Unmarshal(body, &manifest); err != nil {
			t.Fatal(err)
		}
		selection := BackupSelection{UserInfo: true}
		manifest.Selection = &selection
		manifest.Included = []string{"database"}
		updated, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		return updated
	})
	if _, err := VerifyArchive(ctx, databaseScopeMismatch, VerifyOptions{CurrentVersion: "v1.2.3"}); err == nil {
		t.Fatal("archive containing database rows outside its declared selection was accepted")
	}

	reader, err := zip.OpenReader(cloudPath)
	if err != nil {
		t.Fatal(err)
	}
	var databaseBytes []byte
	for _, file := range reader.File {
		if file.Name == "payload/database.sqlite" {
			databaseBytes = readZipFile(t, file)
			break
		}
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(t.TempDir(), "missing-restore-table.sqlite")
	if err := os.WriteFile(databasePath, databaseBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`DROP TABLE video_reaction_visits`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	databaseBytes, err = os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	missingRestoreTable := filepath.Join(env.root, "missing-restore-table.zip")
	writePayloadArchive(t, missingRestoreTable, []rawZipEntry{
		{name: "payload/database.sqlite", body: databaseBytes},
	})
	if _, err := VerifyArchive(ctx, missingRestoreTable, VerifyOptions{CurrentVersion: "v1.2.3"}); err == nil ||
		!strings.Contains(err.Error(), "video_reaction_visits") {
		t.Fatalf("archive missing a restore table error = %v", err)
	}
}

func TestRangeUploadStreamsWithoutPerRangeDigestAndFinalizesWholeArchive(t *testing.T) {
	env := newTestBackupEnv(t)
	writeTestFile(t, filepath.Join(env.root, "uploads", "range-source.mp4"), []byte("range-upload"))
	record := createAndWaitForBackup(t, env.manager)
	sourcePath, _, err := env.manager.resolveBackup(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	archiveBytes, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	session, err := env.manager.BeginRangeUpload(context.Background(), BeginUploadInput{
		FileName: "server-transfer.zip",
		Size:     int64(len(archiveBytes)),
		SHA256:   sha256Hex(archiveBytes),
	}, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	if session.ChunkSize != 16<<20 {
		t.Fatalf("server-transfer range size = %d, want %d", session.ChunkSize, int64(16<<20))
	}
	var streamed int64
	updated, wrote, err := env.manager.PutRange(
		context.Background(),
		session.ID,
		0,
		bytes.NewReader(archiveBytes),
		func(bytes int64) { streamed += bytes },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote || streamed != int64(len(archiveBytes)) || len(updated.Received) != 1 {
		t.Fatalf("range result = wrote %v, streamed %d, session %+v", wrote, streamed, updated)
	}
	if _, wrote, err := env.manager.PutRange(
		context.Background(), session.ID, 0, bytes.NewReader(archiveBytes), nil,
	); err != nil || wrote {
		t.Fatalf("idempotent range = wrote %v, err %v", wrote, err)
	}
	imported, err := env.manager.FinalizeUpload(
		context.Background(), session.ID, sha256Hex(archiveBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !imported.Imported || imported.VerificationStatus != "verified" ||
		!strings.EqualFold(imported.SHA256, record.SHA256) {
		t.Fatalf("imported record = %+v", imported)
	}
}

func TestCancelUploadInterruptsActiveRangeAndRemovesStaging(t *testing.T) {
	env := newTestBackupEnv(t)
	session, err := env.manager.BeginRangeUpload(context.Background(), BeginUploadInput{
		FileName: "cancel-active-range.zip",
		Size:     16 << 20,
		SHA256:   strings.Repeat("a", 64),
	}, 16<<20)
	if err != nil {
		t.Fatal(err)
	}

	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	type rangeResult struct {
		wrote bool
		err   error
	}
	result := make(chan rangeResult, 1)
	started := make(chan struct{}, 1)
	go func() {
		_, wrote, putErr := env.manager.PutRange(
			context.Background(),
			session.ID,
			0,
			reader,
			func(int64) {
				select {
				case started <- struct{}{}:
				default:
				}
			},
		)
		result <- rangeResult{wrote: wrote, err: putErr}
	}()
	writeResult := make(chan error, 1)
	go func() {
		_, writeErr := writer.Write(make([]byte, 1<<20))
		writeResult <- writeErr
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("range writer did not start")
	}

	if err := env.manager.CancelUpload(session.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err == nil || got.wrote {
			t.Fatalf("canceled range = wrote %v, err %v", got.wrote, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("active range did not stop after upload cancellation")
	}
	select {
	case <-writeResult:
	case <-time.After(5 * time.Second):
		t.Fatal("range source remained blocked after upload cancellation")
	}
	if _, err := env.manager.UploadStatus(session.ID); !errors.Is(err, ErrUploadNotFound) {
		t.Fatalf("upload status after cancellation = %v, want ErrUploadNotFound", err)
	}
}

func TestChunkUploadStreamsWithoutDigestsAndSupportsOutOfOrderRestartResume(t *testing.T) {
	env := newTestBackupEnv(t)
	large := bytes.Repeat([]byte{0x5a}, int(ChunkSize)+4096)
	writeTestFile(t, filepath.Join(env.root, "uploads", "large.mp4"), large)
	record := createAndWaitForBackup(t, env.manager)
	sourcePath, _, err := env.manager.resolveBackup(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	archiveBytes, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	session, err := env.manager.BeginUpload(context.Background(), BeginUploadInput{
		FileName: "migration.zip",
		Size:     int64(len(archiveBytes)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.TotalChunks < 2 {
		t.Fatalf("total chunks = %d, want at least 2", session.TotalChunks)
	}
	partInfo, err := os.Stat(env.manager.uploadPartPath(session.ID))
	if err != nil {
		t.Fatal(err)
	}
	if partInfo.Size() != session.Size {
		t.Fatalf("part size = %d, want %d", partInfo.Size(), session.Size)
	}
	put := func(index int) error {
		start := int64(index) * session.ChunkSize
		end := min(start+session.ChunkSize, int64(len(archiveBytes)))
		_, err := env.manager.PutChunk(
			context.Background(),
			session.ID,
			index,
			bytes.NewReader(archiveBytes[start:end]),
		)
		return err
	}
	lastIndex := session.TotalChunks - 1
	lastStart := int64(lastIndex) * session.ChunkSize
	putErrors := make(chan error, 2)
	go func() { putErrors <- put(lastIndex) }()
	go func() { putErrors <- put(0) }()
	for range 2 {
		if err := <-putErrors; err != nil {
			t.Fatal(err)
		}
	}
	if err := put(0); err != nil {
		t.Fatalf("idempotent repeated chunk failed: %v", err)
	}
	uploadEntries, err := os.ReadDir(env.manager.uploadDir(session.ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range uploadEntries {
		if strings.HasSuffix(entry.Name(), ".chunk") {
			t.Fatalf("new upload wrote legacy chunk file %q", entry.Name())
		}
	}
	part, err := os.Open(env.manager.uploadPartPath(session.ID))
	if err != nil {
		t.Fatal(err)
	}
	lastOnDisk := make([]byte, len(archiveBytes[lastStart:]))
	if _, err := part.ReadAt(lastOnDisk, lastStart); err != nil {
		_ = part.Close()
		t.Fatal(err)
	}
	if err := part.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(lastOnDisk, archiveBytes[lastStart:]) {
		t.Fatal("out-of-order chunk was not written at its fixed offset")
	}

	// A deployment can be upgraded while a part-v1 browser upload is still in
	// progress. Keep accepting its obsolete per-chunk digest fields, but never
	// use them as validation inputs or expose them again.
	stored, err := env.manager.loadUpload(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacyJSON, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	var legacySidecar map[string]any
	if err := json.Unmarshal(legacyJSON, &legacySidecar); err != nil {
		t.Fatal(err)
	}
	legacySidecar["storageFormat"] = uploadStoragePartV1
	received, ok := legacySidecar["received"].(map[string]any)
	if !ok {
		t.Fatalf("legacy received sidecar = %#v", legacySidecar["received"])
	}
	for key, value := range received {
		chunk, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("legacy chunk %s = %#v", key, value)
		}
		chunk["sha256"] = strings.Repeat("a", 64)
	}
	if err := writeJSONAtomic(env.manager.uploadSidecar(session.ID), legacySidecar, 0o600); err != nil {
		t.Fatal(err)
	}

	env.manager.Close()
	resumed, err := NewManager(Config{
		Catalog:        env.cat,
		AppConfig:      env.cfg,
		ConfigPath:     env.configPath,
		AppVersion:     "v1.2.3",
		RestartManaged: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	status, err := resumed.UploadStatus(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Received) != 2 {
		t.Fatalf("received after restart = %d, want 2", len(status.Received))
	}
	resumed.setUploadProgress(status.ID, OperationProgress{
		Phase:          "hashing",
		ProcessedBytes: 1024,
		TotalBytes:     status.Size,
	})
	status, err = resumed.UploadStatus(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Progress == nil || status.Progress.Phase != "hashing" ||
		status.Progress.ProcessedBytes != 1024 {
		t.Fatalf("upload progress snapshot = %+v", status.Progress)
	}
	resumed.clearUploadProgress(status.ID)
	for index := 1; index < lastIndex; index++ {
		start := int64(index) * status.ChunkSize
		end := min(start+status.ChunkSize, int64(len(archiveBytes)))
		chunk := archiveBytes[start:end]
		if _, err := resumed.PutChunk(
			context.Background(),
			status.ID,
			index,
			bytes.NewReader(chunk),
		); err != nil {
			t.Fatal(err)
		}
	}
	imported, err := resumed.FinalizeUpload(
		context.Background(), session.ID, sha256Hex(archiveBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !imported.Imported || imported.VerificationStatus != "verified" {
		t.Fatalf("imported record = %+v", imported)
	}
}

func TestChunkUploadMigratesLegacySessionToSinglePart(t *testing.T) {
	env := newTestBackupEnv(t)
	writeTestFile(t, filepath.Join(env.root, "uploads", "legacy.mp4"), []byte("legacy-upload"))
	record := createAndWaitForBackup(t, env.manager)
	sourcePath, _, err := env.manager.resolveBackup(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	archiveBytes, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	session, err := env.manager.BeginUpload(context.Background(), BeginUploadInput{
		FileName: "legacy.zip",
		Size:     int64(len(archiveBytes)),
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := env.manager.loadUpload(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(env.manager.uploadPartPath(session.ID)); err != nil {
		t.Fatal(err)
	}
	stored.StorageFormat = ""
	for index := 0; index < stored.TotalChunks; index++ {
		start := int64(index) * stored.ChunkSize
		end := min(start+stored.ChunkSize, int64(len(archiveBytes)))
		chunk := archiveBytes[start:end]
		writeTestFile(t, env.manager.uploadChunkPath(session.ID, index), chunk)
		stored.Received[index] = UploadChunk{
			Index: index,
			Size:  int64(len(chunk)),
		}
	}
	if err := writeJSONAtomic(env.manager.uploadSidecar(session.ID), stored, 0o600); err != nil {
		t.Fatal(err)
	}

	imported, err := env.manager.FinalizeUpload(
		context.Background(), session.ID, sha256Hex(archiveBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	importedPath, _, err := env.manager.resolveBackup(imported.ID)
	if err != nil {
		t.Fatal(err)
	}
	importedBytes, err := os.ReadFile(importedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(importedBytes, archiveBytes) {
		t.Fatal("legacy chunks changed while migrating to the part file")
	}
	if _, err := os.Stat(env.manager.uploadDir(session.ID)); !os.IsNotExist(err) {
		t.Fatalf("completed legacy upload session still exists: %v", err)
	}
}

func TestChunkUploadWholeHashMismatchClearsAllCheckpointsForRetry(t *testing.T) {
	env := newTestBackupEnv(t)
	writeTestFile(
		t,
		filepath.Join(env.root, "uploads", "retry.mp4"),
		bytes.Repeat([]byte("retry-upload"), int(ChunkSize/12)+1),
	)
	record := createAndWaitForBackup(t, env.manager)
	sourcePath, _, err := env.manager.resolveBackup(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	archiveBytes, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	session, err := env.manager.BeginUpload(context.Background(), BeginUploadInput{
		FileName: "retry.zip",
		Size:     int64(len(archiveBytes)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.TotalChunks < 2 {
		t.Fatalf("test archive chunks = %d, want at least 2", session.TotalChunks)
	}
	for index := 0; index < session.TotalChunks; index++ {
		start := int64(index) * session.ChunkSize
		end := min(start+session.ChunkSize, int64(len(archiveBytes)))
		if _, err := env.manager.PutChunk(
			context.Background(), session.ID, index, bytes.NewReader(archiveBytes[start:end]),
		); err != nil {
			t.Fatal(err)
		}
	}
	part, err := os.OpenFile(env.manager.uploadPartPath(session.ID), os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.WriteAt([]byte{archiveBytes[0] ^ 0xff}, 0); err != nil {
		_ = part.Close()
		t.Fatal(err)
	}
	if err := part.Sync(); err != nil {
		_ = part.Close()
		t.Fatal(err)
	}
	if err := part.Close(); err != nil {
		t.Fatal(err)
	}
	archiveHash := sha256Hex(archiveBytes)
	if _, err := env.manager.FinalizeUpload(
		context.Background(), session.ID, archiveHash,
	); err == nil || !strings.Contains(err.Error(), "完整备份包 SHA-256 校验失败") {
		t.Fatalf("corrupt part finalize error = %v", err)
	}
	status, err := env.manager.UploadStatus(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "uploading" || len(status.Received) != 0 {
		t.Fatalf("status after corrupt chunk = %+v", status)
	}
	for index := 0; index < session.TotalChunks; index++ {
		start := int64(index) * session.ChunkSize
		end := min(start+session.ChunkSize, int64(len(archiveBytes)))
		if _, err := env.manager.PutChunk(
			context.Background(), session.ID, index, bytes.NewReader(archiveBytes[start:end]),
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.manager.FinalizeUpload(context.Background(), session.ID, archiveHash); err != nil {
		t.Fatalf("finalize after retransmitting corrupt chunk: %v", err)
	}
}

func TestRestoreSwitchesAllDataPreservesTargetRuntimeConfigAndClearsSessions(t *testing.T) {
	env := newTestBackupEnv(t)
	ctx := context.Background()
	previewPath := mediaasset.PreviewPath(env.cfg.Storage.LocalPreviewDir, "video-1")
	thumbPath := mediaasset.ThumbnailPath(env.cfg.Storage.LocalPreviewDir, "video-1")
	writeTestFile(t, previewPath, []byte("old-preview"))
	writeTestFile(t, thumbPath, []byte("old-thumb"))
	writeTestFile(t, filepath.Join(env.root, "uploads", "old.mp4"), []byte("old-upload"))
	writeTestFile(t, filepath.Join(env.root, "uploads", "conflict.mp4"), []byte("backup-conflict"))
	scriptPath := filepath.Join(env.root, "crawler-scripts", "restore.py")
	writeTestFile(t, scriptPath, []byte("print('restore')"))
	now := time.Now()
	if err := env.cat.UpsertVideo(ctx, &catalog.Video{
		ID:                "video-1",
		DriveID:           "drive-1",
		FileID:            "remote-1",
		FileName:          "video.mp4",
		Title:             "Backup Video",
		ThumbnailURL:      "/p/thumb/video-1",
		PreviewLocal:      previewPath,
		PreviewStatus:     "ready",
		FingerprintStatus: "ready",
		PublishedAt:       now,
		CreatedAt:         now,
		UpdatedAt:         now,
	}); err != nil {
		t.Fatal(err)
	}
	missingPreviewPath := mediaasset.PreviewPath(env.cfg.Storage.LocalPreviewDir, "video-missing-assets")
	if err := env.cat.UpsertVideo(ctx, &catalog.Video{
		ID:            "video-missing-assets",
		DriveID:       "drive-1",
		FileID:        "remote-missing-assets",
		FileName:      "missing.mp4",
		Title:         "Missing Assets",
		ThumbnailURL:  "/p/thumb/video-missing-assets",
		PreviewLocal:  missingPreviewPath,
		PreviewStatus: "ready",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.cat.UpsertDrive(ctx, &catalog.Drive{
		ID:     "crawler-1",
		Kind:   "scriptcrawler",
		Name:   "Crawler",
		RootID: "/",
		Credentials: map[string]string{
			"script_path": scriptPath,
		},
	}); err != nil {
		t.Fatal(err)
	}
	missingLocal := filepath.Join(env.root, "missing-local-disk")
	if err := env.cat.UpsertDrive(ctx, &catalog.Drive{
		ID:     "local-missing",
		Kind:   "localstorage",
		Name:   "Missing Local",
		RootID: "/",
		Credentials: map[string]string{
			"path": missingLocal,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.cat.CreateSession(ctx, "old-session", time.Hour, 0); err != nil {
		t.Fatal(err)
	}
	if err := env.cat.CreateVideoShare(ctx, "old-share", "old-share-token", "video-1", now); err != nil {
		t.Fatal(err)
	}
	if _, err := env.cat.CreateRemoteUploadJob(
		ctx,
		"old-remote-job",
		"https://source.example/private.mp4",
		"private.mp4",
		"Remote Job",
		nil,
	); err != nil {
		t.Fatal(err)
	}
	record := createAndWaitForBackup(t, env.manager)
	if err := env.cat.MarkRemoteUploadCanceled(ctx, "old-remote-job"); err != nil {
		t.Fatal(err)
	}
	if deleted, err := env.cat.DeleteExpiredRemoteUploadJobs(ctx, time.Now().Add(time.Hour)); err != nil || deleted != 1 {
		t.Fatalf("delete backed-up upload job: deleted=%d err=%v", deleted, err)
	}
	if _, err := env.cat.CreateRemoteUploadJob(
		ctx,
		"target-remote-job",
		"https://target.example/current.mp4",
		"current.mp4",
		"Target Remote Job",
		nil,
	); err != nil {
		t.Fatal(err)
	}

	if _, err := env.cat.CreateUser(ctx, "post-backup-user", "hash", "user"); err != nil {
		t.Fatal(err)
	}
	if err := env.cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "orphan-target-video",
		DriveID:     "drive-1",
		FileID:      "target-orphan",
		Title:       "Target-only orphan video",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, previewPath, []byte("new-preview"))
	writeTestFile(t, filepath.Join(env.root, "uploads", "new.mp4"), []byte("new-upload"))
	writeTestFile(t, filepath.Join(env.root, "uploads", "conflict.mp4"), []byte("target-conflict"))
	env.cfg.Server.Listen = "0.0.0.0:7777"
	env.cfg.Server.AllowedOrigins = []string{"https://target.example"}
	loggingEnabled := true
	env.cfg.Logging = config.Logging{
		FileEnabled:    &loggingEnabled,
		Directory:      filepath.Join(env.root, "logs"),
		MaxFileSizeMB:  25,
		MaxTotalSizeMB: 300,
	}
	env.cfg.Preview.FFmpegPath = "/target/bin/ffmpeg"
	env.cfg.Preview.FFprobePath = "/target/bin/ffprobe"
	env.cfg.Preview.Enabled = false
	writeTestConfig(t, env.configPath, env.cfg)

	report, err := env.manager.PrepareRestore(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	restoreProgress := env.manager.restoreProgressSnapshot()
	if restoreProgress == nil || restoreProgress.Phase != "ready" ||
		restoreProgress.ProcessedBytes != report.Manifest.TotalSize ||
		restoreProgress.ProcessedFiles != report.Manifest.FileCount {
		t.Fatalf("restore progress after prepare = %+v", restoreProgress)
	}
	if report.VerificationStatus != "verified" ||
		!strings.Contains(strings.Join(report.PathRewrites, "\n"), "新的独立存储") ||
		len(report.MissingAssets) < 2 {
		t.Fatalf("restore report = %+v", report)
	}
	env.manager.Close()
	if err := env.cat.Close(); err != nil {
		t.Fatal(err)
	}
	applied, err := ApplyPendingRestore(env.root)
	if err != nil {
		t.Fatal(err)
	}
	if applied == nil {
		t.Fatal("pending restore was not applied")
	}
	restoredCatalog, err := catalog.Open(env.cfg.Storage.DBPath)
	if err != nil {
		_ = RollbackAppliedRestore(applied, err)
		t.Fatal(err)
	}
	defer restoredCatalog.Close()
	if err := CommitAppliedRestore(applied); err != nil {
		t.Fatal(err)
	}
	postBackupUser, err := restoredCatalog.GetUserByUsername(ctx, "post-backup-user")
	if err != nil {
		t.Fatalf("post-backup user was removed by account merge: %v", err)
	}
	if postBackupUser.Password != "hash" || postBackupUser.Role != "user" {
		t.Fatalf("post-backup user changed by account merge: %+v", postBackupUser)
	}
	if _, err := restoredCatalog.GetVideo(ctx, "orphan-target-video"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("target-only resource survived a complete resource restore: %v", err)
	}
	if valid, _, err := restoredCatalog.ValidateSession(ctx, "old-session"); err != nil || valid {
		t.Fatalf("restored session valid=%v err=%v, want cleared", valid, err)
	}
	video, err := restoredCatalog.GetVideo(ctx, "video-1")
	if err != nil {
		t.Fatal(err)
	}
	if video.PreviewStatus != "ready" || video.PreviewLocal != filepath.Base(previewPath) ||
		video.ThumbnailURL != "/p/thumb/video-1" {
		t.Fatalf("restored video asset state = %+v", video)
	}
	missingVideo, err := restoredCatalog.GetVideo(ctx, "video-missing-assets")
	if err != nil {
		t.Fatal(err)
	}
	if missingVideo.PreviewStatus != "pending" || missingVideo.PreviewLocal != "" ||
		missingVideo.ThumbnailURL != "" {
		t.Fatalf("missing restored assets were not marked pending: %+v", missingVideo)
	}
	pendingThumbnails, err := restoredCatalog.ListVideosByThumbnailStatus(ctx, "drive-1", "pending", 100)
	if err != nil {
		t.Fatal(err)
	}
	missingThumbnailPending := false
	for _, candidate := range pendingThumbnails {
		if candidate.ID == missingVideo.ID {
			missingThumbnailPending = true
			break
		}
	}
	if !missingThumbnailPending {
		t.Fatal("missing restored thumbnail was not marked pending")
	}
	remoteJob, err := restoredCatalog.GetRemoteUploadJob(ctx, "old-remote-job")
	if err != nil {
		t.Fatal(err)
	}
	if remoteJob.State != catalog.RemoteUploadCanceled || remoteJob.SourceURL != "" ||
		!remoteJob.CancelRequested {
		t.Fatalf("restored remote upload was not canceled and scrubbed: %+v", remoteJob)
	}
	targetRemoteJob, err := restoredCatalog.GetRemoteUploadJob(ctx, "target-remote-job")
	if err != nil || targetRemoteJob.State != catalog.RemoteUploadQueued ||
		targetRemoteJob.SourceURL != "https://target.example/current.mp4" {
		t.Fatalf("target remote upload job = %+v err=%v", targetRemoteJob, err)
	}
	if err := restoredCatalog.CreateVideoShare(
		ctx,
		"old-share",
		"old-share-token",
		"video-1",
		now.Add(time.Hour),
	); err != nil {
		t.Fatalf("restored one-time shares were not cleared: %v", err)
	}
	if body, err := os.ReadFile(previewPath); err != nil || string(body) != "old-preview" {
		t.Fatalf("restored preview = %q err=%v", body, err)
	}
	if body, err := os.ReadFile(filepath.Join(env.root, "uploads", "new.mp4")); err != nil || string(body) != "new-upload" {
		t.Fatalf("target-only upload = %q err=%v", body, err)
	}
	if body, err := os.ReadFile(filepath.Join(env.root, "uploads", "old.mp4")); err != nil ||
		string(body) != "old-upload" {
		t.Fatalf("restored upload = %q err=%v", body, err)
	}
	if body, err := os.ReadFile(filepath.Join(env.root, "uploads", "conflict.mp4")); err != nil ||
		string(body) != "target-conflict" {
		t.Fatalf("conflicting upload = %q err=%v", body, err)
	}
	restoredConfig, err := config.Load(env.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if restoredConfig.Server.Listen != "0.0.0.0:7777" ||
		len(restoredConfig.Server.AllowedOrigins) != 1 ||
		restoredConfig.Server.AllowedOrigins[0] != "https://target.example" {
		t.Fatalf("target network config was not preserved: %+v", restoredConfig.Server)
	}
	if restoredConfig.Preview.Enabled {
		t.Fatal("restore replaced the target global preview switch")
	}
	if restoredConfig.Preview.FFmpegPath != "/target/bin/ffmpeg" ||
		restoredConfig.Preview.FFprobePath != "/target/bin/ffprobe" {
		t.Fatalf("target executable paths were not preserved: %+v", restoredConfig.Preview)
	}
	if !restoredConfig.Logging.IsFileEnabled() ||
		restoredConfig.Logging.Directory != filepath.Join(env.root, "logs") ||
		restoredConfig.Logging.MaxFileSizeMB != 25 ||
		restoredConfig.Logging.MaxTotalSizeMB != 300 {
		t.Fatalf("target logging config was not preserved: %+v", restoredConfig.Logging)
	}
	localDrive, err := restoredCatalog.GetDrive(ctx, "local-missing")
	if err != nil {
		t.Fatal(err)
	}
	if localDrive.Name != "Missing Local" || localDrive.Credentials["path"] != missingLocal {
		t.Fatalf("target local drive was overwritten: %+v", localDrive)
	}
}

func TestRestoreAcceptsOnlyCurrentBackupProtocol(t *testing.T) {
	env := newTestBackupEnv(t)
	record := createAndWaitForBackup(t, env.manager)
	currentPath, _, err := env.manager.resolveBackup(record.ID)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		id      string
		version int
	}{
		{id: "video-site-91-full-protocol-v1", version: 1},
		{id: "video-site-91-full-protocol-v2", version: 2},
	} {
		archivePath := filepath.Join(env.manager.backupDir, test.id+".zip")
		rewriteZip(t, currentPath, archivePath, func(name string, body []byte) []byte {
			if name != manifestName {
				return body
			}
			var manifest Manifest
			if err := json.Unmarshal(body, &manifest); err != nil {
				t.Fatal(err)
			}
			manifest.FormatVersion = test.version
			updated, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			return updated
		})
		if _, err := InspectArchive(archivePath); err == nil ||
			!strings.Contains(err.Error(), "unsupported format version") {
			t.Fatalf("protocol v%d inspection error = %v", test.version, err)
		}
		if _, err := env.manager.PrepareRestore(context.Background(), test.id); err == nil ||
			!strings.Contains(err.Error(), "unsupported format version") {
			t.Fatalf("protocol v%d restore error = %v", test.version, err)
		}
	}

	const missingSelectionID = "video-site-91-full-protocol-missing-selection"
	rewriteZip(
		t,
		currentPath,
		filepath.Join(env.manager.backupDir, missingSelectionID+".zip"),
		func(name string, body []byte) []byte {
			if name != manifestName {
				return body
			}
			var manifest Manifest
			if err := json.Unmarshal(body, &manifest); err != nil {
				t.Fatal(err)
			}
			manifest.Selection = nil
			updated, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			return updated
		},
	)
	if _, err := InspectArchive(filepath.Join(env.manager.backupDir, missingSelectionID+".zip")); err == nil ||
		!strings.Contains(err.Error(), "selection is missing") {
		t.Fatalf("missing selection inspection error = %v", err)
	}
	if _, err := env.manager.PrepareRestore(context.Background(), missingSelectionID); err == nil ||
		!strings.Contains(err.Error(), "selection is missing") {
		t.Fatalf("missing selection restore error = %v", err)
	}
}

func TestAppliedRestoreCanRollBackToOldData(t *testing.T) {
	env := newTestBackupEnv(t)
	writeTestFile(t, filepath.Join(env.root, "uploads", "state.txt"), []byte("backup-state"))
	record := createAndWaitForBackup(t, env.manager)
	writeTestFile(t, filepath.Join(env.root, "uploads", "state.txt"), []byte("current-state"))
	if _, err := env.cat.CreateUser(context.Background(), "current-user", "hash", "user"); err != nil {
		t.Fatal(err)
	}
	currentOwnerID, err := env.cat.CreateUser(
		context.Background(),
		"current-owner",
		"current-owner-password-hash",
		"admin",
	)
	if err != nil {
		t.Fatal(err)
	}
	currentAuditorID, err := env.cat.CreateUser(
		context.Background(),
		"current-auditor",
		"current-auditor-password-hash",
		"admin",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.cat.SetUserBanned(context.Background(), currentAuditorID, true); err != nil {
		t.Fatal(err)
	}
	currentOwner, err := env.cat.GetUserByID(context.Background(), currentOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	currentAuditor, err := env.cat.GetUserByID(context.Background(), currentAuditorID)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.cat.CreateSession(
		context.Background(),
		"rollback-admin-session",
		time.Hour,
		currentOwnerID,
	); err != nil {
		t.Fatal(err)
	}
	writeTestConfig(t, env.configPath, env.cfg)
	if _, err := env.manager.PrepareRestore(context.Background(), record.ID); err != nil {
		t.Fatal(err)
	}
	env.manager.Close()
	if err := env.cat.Close(); err != nil {
		t.Fatal(err)
	}
	applied, err := ApplyPendingRestore(env.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := RollbackAppliedRestore(applied, errors.New("injected migration failure")); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(env.root, "uploads", "state.txt"))
	if err != nil || string(body) != "current-state" {
		t.Fatalf("rolled back file = %q err=%v", body, err)
	}
	oldCatalog, err := catalog.Open(env.cfg.Storage.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer oldCatalog.Close()
	if _, err := oldCatalog.GetUserByUsername(context.Background(), "current-user"); err != nil {
		t.Fatalf("current database was not restored after rollback: %v", err)
	}
	for _, expected := range []*catalog.User{currentOwner, currentAuditor} {
		actual, err := oldCatalog.GetUserByUsername(context.Background(), expected.Username)
		if err != nil {
			t.Fatalf("rolled back administrator %q is missing: %v", expected.Username, err)
		}
		if actual.Password != expected.Password ||
			actual.Role != expected.Role ||
			actual.Banned != expected.Banned ||
			actual.CreatedAt != expected.CreatedAt {
			t.Fatalf("rolled back administrator %q = %+v, want %+v", expected.Username, actual, expected)
		}
	}
	if valid, userID, err := oldCatalog.ValidateSession(
		context.Background(),
		"rollback-admin-session",
	); err != nil || !valid || userID != currentOwnerID {
		t.Fatalf("rolled back administrator session valid=%v userID=%d err=%v", valid, userID, err)
	}
	rolledBackConfig, err := config.Load(env.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if rolledBackConfig.Server.Listen != env.cfg.Server.Listen {
		t.Fatalf("server config was not restored after rollback: %+v", rolledBackConfig.Server)
	}
}

func TestInterruptedRestoreRollbackResumesWithoutMixingData(t *testing.T) {
	env := newTestBackupEnv(t)
	statePath := filepath.Join(env.root, "uploads", "state.txt")
	writeTestFile(t, statePath, []byte("backup-state"))
	record := createAndWaitForBackup(t, env.manager)
	writeTestFile(t, statePath, []byte("current-state"))
	if _, err := env.cat.CreateUser(context.Background(), "current-user", "hash", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.manager.PrepareRestore(context.Background(), record.ID); err != nil {
		t.Fatal(err)
	}
	env.manager.Close()
	if err := env.cat.Close(); err != nil {
		t.Fatal(err)
	}
	applied, err := ApplyPendingRestore(env.root)
	if err != nil {
		t.Fatal(err)
	}
	if applied == nil {
		t.Fatal("pending restore was not applied")
	}

	// Simulate a crash after one operation has already put its old target back,
	// but before that operation's rolledback state reached the marker.
	interrupted := applied.marker
	var uploadOperation *restoreSwitch
	for index := range interrupted.Operations {
		if interrupted.Operations[index].Name == "uploads" {
			uploadOperation = &interrupted.Operations[index]
			break
		}
	}
	if uploadOperation == nil || !uploadOperation.HadTarget {
		t.Fatalf("uploads restore operation = %+v", uploadOperation)
	}
	if err := os.Rename(uploadOperation.Target, uploadOperation.Staged); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(uploadOperation.Rollback, uploadOperation.Target); err != nil {
		t.Fatal(err)
	}
	interrupted.State = "rolling-back"
	interrupted.LastError = "injected crash during rollback"
	if err := writeJSONAtomic(applied.markerPath, interrupted, 0o600); err != nil {
		t.Fatal(err)
	}

	reapplied, err := ApplyPendingRestore(env.root)
	if err != nil {
		t.Fatal(err)
	}
	if reapplied != nil {
		t.Fatal("interrupted rollback unexpectedly reapplied restored data")
	}
	body, err := os.ReadFile(statePath)
	if err != nil || string(body) != "current-state" {
		t.Fatalf("resumed rollback file = %q err=%v", body, err)
	}
	currentCatalog, err := catalog.Open(env.cfg.Storage.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer currentCatalog.Close()
	if _, err := currentCatalog.GetUserByUsername(context.Background(), "current-user"); err != nil {
		t.Fatalf("resumed rollback left the restored database active: %v", err)
	}
}

func TestArchiveEntryLimitsCountFilesAndDirectoriesSeparately(t *testing.T) {
	if err := validateArchiveEntryCounts(maxArchiveFiles+1, maxArchiveDirectories); err != nil {
		t.Fatalf("entry limits rejected their exact boundary: %v", err)
	}
	if err := validateArchiveEntryCounts(maxArchiveFiles+2, 0); err == nil ||
		!strings.Contains(err.Error(), "file count") {
		t.Fatalf("file limit error = %v", err)
	}
	if err := validateArchiveEntryCounts(1, maxArchiveDirectories+1); err == nil ||
		!strings.Contains(err.Error(), "directory count") {
		t.Fatalf("directory limit error = %v", err)
	}

	archivePath := filepath.Join(t.TempDir(), "too-many-directories.zip")
	entries := make([]rawZipEntry, 0, maxArchiveDirectories+2)
	entries = append(entries, rawZipEntry{
		name: manifestName,
		body: []byte(`{"formatVersion":3,"fileCount":0,"totalSize":0}`),
	})
	for index := 0; index <= maxArchiveDirectories; index++ {
		entries = append(entries, rawZipEntry{
			name: fmt.Sprintf("payload/previews/directory-%04d/", index),
			mode: os.ModeDir | 0o755,
		})
	}
	writeRawZip(t, archivePath, entries)
	if _, err := InspectArchive(archivePath); err == nil ||
		!strings.Contains(err.Error(), "directory count") {
		t.Fatalf("oversized directory set error = %v", err)
	}
}

func TestRestoreMarkerLimitsAndStagingRootAreValidated(t *testing.T) {
	dataRoot := t.TempDir()
	stageRoot := filepath.Join(dataRoot, restoreStageDirName, "stage-id")
	target := filepath.Join(dataRoot, "video-site.db")
	marker := restoreMarker{
		MarkerVersion: 1,
		BackupID:      backupNamePrefix + "marker-test",
		DataRoot:      dataRoot,
		StageRoot:     stageRoot,
		State:         "pending",
		Operations: []restoreSwitch{{
			Name:     "database",
			Kind:     "file",
			Target:   target,
			Staged:   filepath.Join(dataRoot, ".video-site.db.restore-stage-stage-id"),
			Rollback: filepath.Join(dataRoot, ".video-site.db.restore-rollback-stage-id"),
			State:    "pending",
		}},
	}
	if err := validateRestoreMarker(marker, dataRoot); err != nil {
		t.Fatalf("valid restore marker was rejected: %v", err)
	}
	unsafeStage := marker
	unsafeStage.StageRoot = filepath.Dir(dataRoot)
	if err := validateRestoreMarker(unsafeStage, dataRoot); err == nil ||
		!strings.Contains(err.Error(), "staging root") {
		t.Fatalf("unsafe staging root error = %v", err)
	}
	if err := validateRestoreOperationCount(maxRestoreOperations + 1); err == nil {
		t.Fatal("restore operation count above the durable marker limit was accepted")
	}
	manifest := Manifest{
		Included:     []string{"database", "previews", "uploads"},
		LocalStorage: make([]LocalStorageRoot, maxRestoreOperations),
	}
	if count := manifestRestoreOperationCount(manifest); count <= maxRestoreOperations {
		t.Fatalf("oversized manifest restore operation count = %d", count)
	}
}

func TestWriteJSONAtomicLeavesDurableReplacementWithoutTemporaryFiles(t *testing.T) {
	directory := t.TempDir()
	filePath := filepath.Join(directory, "state.json")
	if err := writeJSONAtomic(filePath, map[string]int{"generation": 1}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filePath, map[string]int{"generation": 2}, 0o600); err != nil {
		t.Fatal(err)
	}
	var state map[string]int
	if err := readJSONFile(filePath, &state); err != nil {
		t.Fatal(err)
	}
	if state["generation"] != 2 {
		t.Fatalf("atomic JSON generation = %d, want 2", state["generation"])
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(filePath) {
		t.Fatalf("atomic JSON directory entries = %v", entries)
	}
	oversizedPath := filepath.Join(directory, "oversized.json")
	oversized, err := os.OpenFile(oversizedPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := oversized.Truncate(maxJSONSidecarBytes + 1); err != nil {
		_ = oversized.Close()
		t.Fatal(err)
	}
	if err := oversized.Close(); err != nil {
		t.Fatal(err)
	}
	if err := readJSONFile(oversizedPath, &state); err == nil ||
		!strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized JSON sidecar error = %v", err)
	}
}

type rawZipEntry struct {
	name string
	body []byte
	mode os.FileMode
}

func readZipFile(t *testing.T, file *zip.File) []byte {
	t.Helper()
	if file == nil {
		t.Fatal("ZIP entry is missing")
	}
	input, err := file.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	body, err := io.ReadAll(input)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func writeRawZip(t *testing.T, path string, entries []rawZipEntry) {
	t.Helper()
	output, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(output)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
}

func writePayloadArchive(t *testing.T, archivePath string, payload []rawZipEntry) {
	t.Helper()
	selection := FullBackupSelection()
	manifest := Manifest{
		FormatVersion:  FormatVersion,
		AppVersion:     "v1.2.3",
		CreatedAt:      time.Now().UTC(),
		SourceDataRoot: "/source/data",
		SourcePreview:  "/source/data/previews",
		Included:       includedForSelection(selection, false),
		Selection:      &selection,
	}
	for _, entry := range payload {
		manifest.Files = append(manifest.Files, ManifestFile{
			Path:   entry.name,
			Size:   int64(len(entry.body)),
			SHA256: sha256Hex(entry.body),
			Mode:   0o600,
		})
		manifest.TotalSize += int64(len(entry.body))
	}
	manifest.FileCount = len(manifest.Files)
	manifestBody, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	entries := append([]rawZipEntry{{name: manifestName, body: manifestBody}}, payload...)
	writeRawZip(t, archivePath, entries)
}

func rewriteZip(t *testing.T, source, destination string, mutate func(string, []byte) []byte) {
	t.Helper()
	reader, err := zip.OpenReader(source)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	output, err := os.Create(destination)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(output)
	for _, sourceFile := range reader.File {
		header := sourceFile.FileHeader
		target, err := writer.CreateHeader(&header)
		if err != nil {
			t.Fatal(err)
		}
		if sourceFile.FileInfo().IsDir() {
			continue
		}
		input, err := sourceFile.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(input)
		_ = input.Close()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := target.Write(mutate(sourceFile.Name, body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
