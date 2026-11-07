package integration_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"github.com/nianzhibai/91/backend/internal/api"
	"github.com/nianzhibai/91/backend/internal/auth"
	"github.com/nianzhibai/91/backend/internal/backup"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
	"github.com/nianzhibai/91/backend/internal/drives/scriptcrawler"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
	"github.com/nianzhibai/91/backend/internal/proxy"
)

func TestRestoredPreviewServesWithRelativeTargetStorageConfig(t *testing.T) {
	for _, scenario := range []struct{ name, sourceDBDir, targetDBDir string }{
		{"default-to-default", "", ""},
		{"default-to-separate", "", "./fast-db"},
		{"separate-to-default", "./source-db", ""},
		{"separate-to-separate", "./source-db", "./fast-db"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			testRestoredPreviewWithDatabaseDirectories(t, scenario.sourceDBDir, scenario.targetDBDir)
		})
	}
}

func testRestoredPreviewWithDatabaseDirectories(t *testing.T, sourceDBDir, targetDBDir string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()

	sourceRoot := filepath.Join(root, "source")
	sourceFileConfig := relativeStorageConfig(sourceDBDir)
	sourceRuntimeStorage := mustResolveStorage(t, sourceFileConfig.Storage, sourceRoot)
	// Archived payloads are filename-independent: simulate a backup created by
	// the old server and restore it into the current 91.db layout.
	sourceRuntimeStorage.DBPath = filepath.Join(sourceRuntimeStorage.DBDir, "video-site.db")
	mustWriteConfig(t, filepath.Join(sourceRoot, "config.yaml"), sourceFileConfig)
	if err := os.MkdirAll(sourceRuntimeStorage.LocalPreviewDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sourceRuntimeStorage.DBDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sourceCatalog, err := catalog.Open(sourceRuntimeStorage.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	sourceManager, err := backup.NewManager(backup.Config{
		Catalog:        sourceCatalog,
		AppConfig:      sourceFileConfig,
		RuntimeStorage: sourceRuntimeStorage,
		ConfigPath:     filepath.Join(sourceRoot, "config.yaml"),
		AppVersion:     "integration-test",
		RestartManaged: true,
	})
	if err != nil {
		_ = sourceCatalog.Close()
		t.Fatal(err)
	}

	previewReference := "nested/" + mediaasset.PreviewFilename("video-1")
	previewPath := filepath.Join(sourceRuntimeStorage.LocalPreviewDir, previewReference)
	if err := os.MkdirAll(filepath.Dir(previewPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previewPath, []byte("restored-preview"), 0o644); err != nil {
		sourceManager.Close()
		_ = sourceCatalog.Close()
		t.Fatal(err)
	}
	now := time.Now()
	if err := sourceCatalog.UpsertVideo(ctx, &catalog.Video{
		ID:            "video-1",
		DriveID:       "drive-1",
		FileID:        "file-1",
		Title:         "Restored video",
		PreviewLocal:  previewReference,
		PreviewStatus: "ready",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		sourceManager.Close()
		_ = sourceCatalog.Close()
		t.Fatal(err)
	}
	scriptPath := filepath.Join(filepath.Dir(sourceRuntimeStorage.LocalPreviewDir), "crawler-scripts", "demo.py")
	if err := os.MkdirAll(filepath.Dir(scriptPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, []byte("CRAWLER_NAME = 'Restored crawler'\nCRAWLER_PROTOCOL = 'crawler.v3'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sourceCatalog.UpsertDrive(ctx, &catalog.Drive{ID: "crawler", Kind: "scriptcrawler", Name: "Restored crawler", Credentials: map[string]string{"script_file": "demo.py"}}); err != nil {
		t.Fatal(err)
	}
	record := createBackup(t, sourceManager)
	archive, _, archiveName, err := sourceManager.OpenBackup(record.ID)
	if err != nil {
		sourceManager.Close()
		_ = sourceCatalog.Close()
		t.Fatal(err)
	}

	targetRoot := filepath.Join(root, "target")
	targetFileConfig := relativeStorageConfig(targetDBDir)
	targetRuntimeStorage := mustResolveStorage(t, targetFileConfig.Storage, targetRoot)
	targetConfigPath := filepath.Join(targetRoot, "config.yaml")
	mustWriteConfig(t, targetConfigPath, targetFileConfig)
	if err := os.MkdirAll(targetRuntimeStorage.LocalPreviewDir, 0o755); err != nil {
		_ = archive.Close()
		sourceManager.Close()
		_ = sourceCatalog.Close()
		t.Fatal(err)
	}
	if err := os.MkdirAll(targetRuntimeStorage.DBDir, 0o755); err != nil {
		t.Fatal(err)
	}
	targetCatalog, err := catalog.Open(targetRuntimeStorage.DBPath)
	if err != nil {
		_ = archive.Close()
		sourceManager.Close()
		_ = sourceCatalog.Close()
		t.Fatal(err)
	}
	targetManager, err := backup.NewManager(backup.Config{
		Catalog:        targetCatalog,
		AppConfig:      targetFileConfig,
		RuntimeStorage: targetRuntimeStorage,
		ConfigPath:     targetConfigPath,
		AppVersion:     "integration-test",
		RestartManaged: true,
	})
	if err != nil {
		_ = archive.Close()
		_ = targetCatalog.Close()
		sourceManager.Close()
		_ = sourceCatalog.Close()
		t.Fatal(err)
	}
	targetArchivePath := filepath.Join(
		targetRuntimeStorage.DataDir,
		"backups",
		archiveName,
	)
	targetArchive, err := os.Create(targetArchivePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(targetArchive, archive); err != nil {
		_ = targetArchive.Close()
		t.Fatal(err)
	}
	if err := targetArchive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	sourceManager.Close()
	if err := sourceCatalog.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := targetManager.PrepareRestore(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	targetManager.Close()
	if err := targetCatalog.Close(); err != nil {
		t.Fatal(err)
	}
	applied, err := backup.ApplyPendingRestore(targetRuntimeStorage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if applied == nil {
		t.Fatal("restore was not applied")
	}
	restoredCatalog, err := catalog.Open(targetRuntimeStorage.DBPath)
	if err != nil {
		_ = backup.RollbackAppliedRestore(applied, err)
		t.Fatal(err)
	}
	defer restoredCatalog.Close()
	if err := backup.CommitAppliedRestore(applied); err != nil {
		t.Fatal(err)
	}

	restoredVideo, err := restoredCatalog.GetVideo(ctx, "video-1")
	if err != nil {
		t.Fatal(err)
	}
	wantPreviewPath := previewReference
	if restoredVideo.PreviewLocal != wantPreviewPath {
		t.Fatalf("restored preview path = %q, want %q", restoredVideo.PreviewLocal, wantPreviewPath)
	}
	if changed, err := restoredCatalog.MigrateManagedPaths(ctx, targetRuntimeStorage.LocalPreviewDir); err != nil || changed != 0 {
		t.Fatalf("restored portable references changed at startup: changed=%d err=%v", changed, err)
	}
	crawler, err := restoredCatalog.GetDrive(ctx, "crawler")
	if err != nil || crawler.Credentials["script_file"] != "demo.py" || crawler.Credentials["script_path"] != "" {
		t.Fatalf("restored crawler reference = %+v, err=%v", crawler, err)
	}
	resolvedScript, err := scriptcrawler.ScriptPath(crawler.Credentials, filepath.Join(filepath.Dir(targetRuntimeStorage.LocalPreviewDir), "crawler-scripts"))
	if err != nil {
		t.Fatal(err)
	}
	if metadata, err := scriptcrawler.ReadMetadata(resolvedScript); err != nil || metadata.Name != "Restored crawler" {
		t.Fatalf("restored script metadata = %+v, err=%v", metadata, err)
	}
	restoredFileConfig, err := config.Load(targetConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if restoredFileConfig.Storage != targetFileConfig.Storage {
		t.Fatalf("restored file storage config = %+v, want %+v", restoredFileConfig.Storage, targetFileConfig.Storage)
	}

	const sessionToken = "integration-session"
	userID, err := restoredCatalog.CreateUser(ctx, "integration-admin", "unused-password-hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := restoredCatalog.CreateSession(ctx, sessionToken, time.Hour, userID); err != nil {
		t.Fatal(err)
	}
	authenticator := &auth.Authenticator{Catalog: restoredCatalog}
	server := &api.Server{
		Catalog:  restoredCatalog,
		Proxy:    proxy.New(proxy.NewRegistry()),
		LocalDir: targetRuntimeStorage.LocalPreviewDir,
	}
	router := chi.NewRouter()
	server.RegisterRoutes(router, authenticator)
	request := httptest.NewRequest(http.MethodGet, "/p/preview/video-1", nil)
	request.AddCookie(&http.Cookie{Name: "vs_admin", Value: sessionToken})
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body = %s", response.Code, response.Body.String())
	}
	if response.Body.String() != "restored-preview" {
		t.Fatalf("preview body = %q", response.Body.String())
	}
	if targetDBDir != "" {
		for _, name := range []string{"backups", ".backup-snapshots", ".restore-staging", "previews"} {
			if _, err := os.Stat(filepath.Join(targetRuntimeStorage.DBDir, name)); !os.IsNotExist(err) {
				t.Fatalf("restore placed non-database data in db_dir: %s, err=%v", name, err)
			}
		}
		if _, err := os.Stat(filepath.Join(targetRuntimeStorage.DataDir, "91.db")); !os.IsNotExist(err) {
			t.Fatalf("restore created database outside db_dir: %v", err)
		}
	}
}

func relativeStorageConfig(dbDir string) *config.Config {
	dbRoot := dbDir
	if dbRoot == "" {
		dbRoot = "./data"
	}
	return &config.Config{
		Server: config.Server{
			Listen: "127.0.0.1:9192",
		},
		Storage: config.Storage{
			DataDir:         "./data",
			DBDir:           dbDir,
			DBPath:          filepath.Join(dbRoot, "91.db"),
			LocalPreviewDir: filepath.Join("data", "previews"),
		},
	}
}

func mustResolveStorage(t *testing.T, storage config.Storage, root string) config.Storage {
	t.Helper()
	resolved, err := config.ResolveStoragePaths(storage, root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func mustWriteConfig(t *testing.T, path string, cfg *config.Config) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func createBackup(t *testing.T, manager *backup.Manager) backup.BackupRecord {
	t.Helper()
	if _, err := manager.Create(context.Background()); err != nil {
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
				t.Fatal("backup completed without archive")
			}
			return result.Backups[0]
		}
		if status != nil && (status.State == "failed" || status.State == "canceled") {
			t.Fatalf("backup ended as %s: %s", status.State, status.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for backup")
	return backup.BackupRecord{}
}
