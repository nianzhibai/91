package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
)

func TestTelegramSourceCleanupRunsWithoutUploadTarget(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	cat, err := catalog.Open(filepath.Join(root, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("telegram:\n  enabled: false\n  upload_drive_id: \"\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewManager(path)
	if err != nil {
		t.Fatal(err)
	}
	v := &catalog.Video{ID: "video", DriveID: "local-upload", FileID: "already-deleted.mp4", Title: "video", Size: 5}
	if err := cat.UpsertVideo(ctx, v); err != nil {
		t.Fatal(err)
	}
	if err := cat.MigrateTelegramVideoToDrive(ctx, v.ID, catalog.VideoDriveMigration{
		SourceDriveID: v.DriveID, SourceFileID: v.FileID, DriveID: "cloud", FileID: "remote",
	}, catalog.TelegramUploadCleanup{
		VideoID: v.ID, SourceDriveID: v.DriveID, SourceFileID: v.FileID,
		Size: 5, ModTimeNS: 123, FileIdentity: "original-file-already-deleted",
	}); err != nil {
		t.Fatal(err)
	}
	app := &App{cat: cat, configManager: manager, cfg: &config.Config{Storage: config.Storage{LocalPreviewDir: filepath.Join(root, "previews")}}}
	if err := app.runTelegramUploadMigration(ctx); err != nil {
		t.Fatal(err)
	}
	jobs, err := cat.ListTelegramUploadCleanups(ctx, "", 100)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("cleanup depended on cloud target: %+v %v", jobs, err)
	}
}
