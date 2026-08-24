package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/config"
	"github.com/video-site/backend/internal/drives/scriptcrawler"
)

func TestCleanupLegacyDeletedCrawlersRemovesOnlyUnconfiguredSources(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	previewDir := filepath.Join(root, "previews")
	cat, err := catalog.Open(filepath.Join(root, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	ghostID := "crawler-deleted"
	activeID := "crawler-active"
	targetID := "target-drive"
	for _, drive := range []*catalog.Drive{
		{
			ID: ghostID, Kind: scriptcrawler.Kind, Name: "Deleted", RootID: "/",
			Credentials: map[string]string{"upload_drive_id": targetID}, TeaserEnabled: true,
		},
		{
			ID: activeID, Kind: scriptcrawler.Kind, Name: "Active", RootID: "/",
			Credentials: map[string]string{"script_path": "/tmp/active.py"}, TeaserEnabled: true,
		},
		{ID: targetID, Kind: "pikpak", Name: "Target", RootID: "", TeaserEnabled: true},
	} {
		if err := cat.UpsertDrive(ctx, drive); err != nil {
			t.Fatalf("seed drive %s: %v", drive.ID, err)
		}
	}

	ghostDir := filepath.Join(root, "scriptcrawlers", ghostID)
	if err := os.MkdirAll(filepath.Join(ghostDir, "videos"), 0o755); err != nil {
		t.Fatalf("mkdir ghost storage: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ghostDir, "videos", "source.mp4"), []byte("local"), 0o644); err != nil {
		t.Fatalf("write ghost video: %v", err)
	}
	activeDir := filepath.Join(root, "scriptcrawlers", activeID)
	if err := os.MkdirAll(activeDir, 0o755); err != nil {
		t.Fatalf("mkdir active storage: %v", err)
	}
	activeSentinel := filepath.Join(activeDir, "keep.txt")
	if err := os.WriteFile(activeSentinel, []byte("keep"), 0o644); err != nil {
		t.Fatalf("write active sentinel: %v", err)
	}

	now := time.Now()
	localVideoID := scriptcrawler.BuildVideoID(ghostID, "source")
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID: localVideoID, DriveID: ghostID, FileID: "source.mp4", FileName: "source.mp4",
		Title: "Local", Size: 5, PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed local crawler video: %v", err)
	}
	migratedVideoID := scriptcrawler.BuildVideoID(ghostID, "migrated")
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID: migratedVideoID, DriveID: targetID, FileID: "remote-file", FileName: "remote.mp4",
		Title: "Migrated", Size: 10, PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed migrated crawler video: %v", err)
	}
	if err := cat.MarkCrawlerSourceSeen(ctx, scriptcrawler.Kind, ghostID, "source", "imported", localVideoID, "", 5); err != nil {
		t.Fatalf("seed crawler source history: %v", err)
	}

	app := &App{cat: cat, cfg: &config.Config{Storage: config.Storage{LocalPreviewDir: previewDir}}}
	stats, err := app.cleanupLegacyDeletedCrawlers(ctx)
	if err != nil {
		t.Fatalf("cleanup legacy crawlers: %v", err)
	}
	if stats.RemovedCrawlers != 1 || stats.RemovedVideos != 1 {
		t.Fatalf("cleanup stats = %+v, want one crawler and one local video", stats)
	}
	if _, err := cat.GetDrive(ctx, ghostID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ghost drive lookup error = %v, want sql.ErrNoRows", err)
	}
	if _, err := cat.GetVideo(ctx, localVideoID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("local ghost video lookup error = %v, want sql.ErrNoRows", err)
	}
	if _, err := cat.GetVideo(ctx, migratedVideoID); err != nil {
		t.Fatalf("migrated video should survive cleanup: %v", err)
	}
	if sourceIDs, err := cat.ListCrawlerSourceIDs(ctx, scriptcrawler.Kind, ghostID); err != nil || len(sourceIDs) != 1 || sourceIDs[0] != "migrated" {
		t.Fatalf("crawler source IDs = %#v err=%v, want only preserved migrated video", sourceIDs, err)
	}
	if _, err := os.Stat(ghostDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ghost storage stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(activeSentinel); err != nil {
		t.Fatalf("active crawler storage changed: %v", err)
	}
	if _, err := cat.GetDrive(ctx, activeID); err != nil {
		t.Fatalf("active crawler was removed: %v", err)
	}

	second, err := app.cleanupLegacyDeletedCrawlers(ctx)
	if err != nil {
		t.Fatalf("repeat cleanup: %v", err)
	}
	if second.RemovedCrawlers != 0 || second.RemovedVideos != 0 {
		t.Fatalf("repeat cleanup stats = %+v, want zero", second)
	}
}
