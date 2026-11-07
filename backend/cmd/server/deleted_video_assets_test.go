package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
	"github.com/nianzhibai/91/backend/internal/preview"
	"github.com/nianzhibai/91/backend/internal/proxy"
)

func seedDeferredVideoAssets(t *testing.T) (*App, *catalog.Video, []string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	cat, err := catalog.Open(filepath.Join(root, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	app := &App{cat: cat, cfg: &config.Config{Storage: config.Storage{LocalPreviewDir: filepath.Join(root, "previews")}}}
	t.Cleanup(func() { _ = app.cat.Close() })
	now := time.Now()
	video := &catalog.Video{
		ID: "deferred-video", DriveID: "source-drive", FileID: "source.mp4", FileName: "source.mp4",
		Title: "Original title", Size: 4096, ThumbnailURL: "/p/thumb/deferred-video",
		PreviewLocal: "deferred-video.mp4", PreviewStatus: "ready", SampledSHA256: "sampled", FingerprintStatus: "ready",
		PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatal(err)
	}
	dir := app.cfg.Storage.LocalPreviewDir
	paths := []string{
		mediaasset.PreviewPath(dir, video.ID), mediaasset.ThumbnailPath(dir, video.ID),
		mediaasset.ShortsBackgroundPath(dir, video.ID), mediaasset.FrameSignaturePath(dir, video.ID),
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("retained asset"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return app, video, paths
}

func assertDeferredAssets(t *testing.T, paths []string, retained bool) {
	t.Helper()
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if retained && (err != nil || string(data) != "retained asset") {
			t.Fatalf("retained asset %s: %q, %v", path, data, err)
		}
		if !retained && !os.IsNotExist(err) {
			t.Fatalf("asset %s still exists: %v", path, err)
		}
	}
}

func TestCancelBlacklistDuringGraceKeepsAssetsAndSkipsRegeneration(t *testing.T) {
	ctx := context.Background()
	app, video, paths := seedDeferredVideoAssets(t)
	gen := &serverFakeTeaserGenerator{}
	worker := preview.NewWorker(gen, app.cat, nil)
	thumbWorker := preview.NewThumbWorker(gen, app.cat, nil)
	app.workers = map[string]*preview.Worker{video.DriveID: worker}
	app.thumbWorkers = map[string]*preview.ThumbWorker{video.DriveID: thumbWorker}
	invalidations := 0
	app.onTagsChanged = func() { invalidations++ }
	if _, err := app.deleteVideo(ctx, video.ID, false); err != nil {
		t.Fatal(err)
	}
	assertDeferredAssets(t, paths, true)
	if err := app.restoreDeletedVideo(ctx, video.ID); err != nil {
		t.Fatal(err)
	}
	restored, err := app.cat.GetVideo(ctx, video.ID)
	if err != nil || restored.PreviewLocal != video.PreviewLocal || restored.PreviewStatus != "ready" || restored.ThumbnailURL != video.ThumbnailURL {
		t.Fatalf("restored video = %#v, %v", restored, err)
	}
	if worker.Status().QueueLength != 0 || thumbWorker.Status().QueueLength != 0 || invalidations != 1 {
		t.Fatalf("undo generation queues = %d/%d, tag invalidations=%d", worker.Status().QueueLength, thumbWorker.Status().QueueLength, invalidations)
	}
	if err := app.cleanupExpiredDeletedVideoAssets(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertDeferredAssets(t, paths, true)
}

func TestExpiredDeletionAssetsAreCleanedAfterRestart(t *testing.T) {
	ctx := context.Background()
	app, video, paths := seedDeferredVideoAssets(t)
	deadline := time.Now().Add(-time.Second)
	if err := app.cat.DeleteVideoWithTombstoneOptions(ctx, video.ID, catalog.DeleteVideoTombstoneOptions{AssetCleanupAfter: deadline}); err != nil {
		t.Fatal(err)
	}
	if err := app.cat.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	app.cat, err = catalog.Open(filepath.Join(filepath.Dir(app.cfg.Storage.LocalPreviewDir), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.cleanupExpiredDeletedVideoAssets(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	assertDeferredAssets(t, paths, false)
	if deleted, err := app.cat.IsVideoDeleted(ctx, video.ID); err != nil || !deleted {
		t.Fatalf("cleanup removed blacklist: deleted=%v, %v", deleted, err)
	}
}

func TestCancelBlacklistAfterGraceCleansAssetsBeforeRediscovery(t *testing.T) {
	ctx := context.Background()
	app, video, paths := seedDeferredVideoAssets(t)
	if err := app.cat.DeleteVideoWithTombstoneOptions(ctx, video.ID, catalog.DeleteVideoTombstoneOptions{AssetCleanupAfter: time.Now().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := app.restoreDeletedVideo(ctx, video.ID); err != nil {
		t.Fatal(err)
	}
	assertDeferredAssets(t, paths, false)
	if _, err := app.cat.GetVideo(ctx, video.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("scan source restored before rediscovery: %v", err)
	}
	if deleted, err := app.cat.IsVideoDeleted(ctx, video.ID); err != nil || deleted {
		t.Fatalf("blacklist still present: %v, %v", deleted, err)
	}
}

func TestStaleAssetCleanupCannotDeleteNewDeletionDuringGrace(t *testing.T) {
	ctx := context.Background()
	app, video, paths := seedDeferredVideoAssets(t)
	oldDeadline := time.Now().Add(-time.Second)
	if err := app.cat.DeleteVideoWithTombstoneOptions(ctx, video.ID, catalog.DeleteVideoTombstoneOptions{AssetCleanupAfter: oldDeadline}); err != nil {
		t.Fatal(err)
	}
	ids, err := app.cat.ListExpiredDeletedVideoAssets(ctx, time.Now(), 10)
	if err != nil || len(ids) != 1 {
		t.Fatalf("old cleanup snapshot = %v, %v", ids, err)
	}
	if err := app.cat.PurgeDeletedVideo(ctx, video.ID); err != nil {
		t.Fatal(err)
	}
	if err := app.cat.UpsertVideo(ctx, video); err != nil {
		t.Fatal(err)
	}
	if _, err := app.deleteVideo(ctx, video.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := app.cleanupDeletedVideoAssets(ctx, ids[0], time.Now(), false); err != nil {
		t.Fatal(err)
	}
	assertDeferredAssets(t, paths, true)
}

func TestExplicitBlacklistSourceDeletionBypassesAssetGrace(t *testing.T) {
	ctx := context.Background()
	app, video, paths := seedDeferredVideoAssets(t)
	drv := &serverRemovableFakeDrive{id: video.DriveID}
	app.registry = proxy.NewRegistry()
	app.registry.Set(drv.ID(), drv)
	if _, err := app.deleteVideo(ctx, video.ID, false); err != nil {
		t.Fatal(err)
	}
	items, err := app.cat.ListDeletedVideosPendingSourceDeletionByIDs(ctx, []string{video.ID})
	if err != nil || len(items) != 1 {
		t.Fatalf("source delete snapshot = %#v, %v", items, err)
	}
	if skipped, err := app.deleteBlacklistedVideoSource(ctx, items[0]); err != nil || skipped {
		t.Fatalf("explicit source deletion: skipped=%v, %v", skipped, err)
	}
	assertDeferredAssets(t, paths, false)
	if drv.removedFileID != video.FileID {
		t.Fatalf("removed source %q, want %q", drv.removedFileID, video.FileID)
	}
	if deleted, err := app.cat.IsVideoDeleted(ctx, video.ID); err != nil || deleted {
		t.Fatalf("source-deleted tombstone still present: %v, %v", deleted, err)
	}
}
