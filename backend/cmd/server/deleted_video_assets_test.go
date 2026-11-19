package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/api"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
	"github.com/nianzhibai/91/backend/internal/preview"
	"github.com/nianzhibai/91/backend/internal/proxy"
	"github.com/nianzhibai/91/backend/internal/scanner"
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
	if _, err := app.deleteVideo(ctx, video.ID, catalog.VideoDeletionOptions{}); err != nil {
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
	if err := app.cat.RecordSourceDeleted(ctx, video); err != nil {
		t.Fatal(err)
	}
	if err := app.cat.UpsertVideo(ctx, video); err != nil {
		t.Fatal(err)
	}
	if _, err := app.deleteVideo(ctx, video.ID, catalog.VideoDeletionOptions{}); err != nil {
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
	if _, err := app.deleteVideo(ctx, video.ID, catalog.VideoDeletionOptions{}); err != nil {
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
	if pending, err := app.cat.ListDeletedVideosPendingSourceDeletionByIDs(ctx, []string{video.ID}); err != nil || len(pending) != 0 {
		t.Fatalf("source deletion still pending: %#v, %v", pending, err)
	}
	if deleted, err := app.cat.IsVideoDeleted(ctx, video.ID); err != nil || !deleted {
		t.Fatalf("source-deleted tombstone no longer blocks content: %v, %v", deleted, err)
	}
}

func TestDuplicateMaintenanceProtectsSharedSourceDuringBlacklistCleanup(t *testing.T) {
	ctx := context.Background()
	app, crawler, _ := seedDeferredVideoAssets(t)
	target := &catalog.Video{
		ID: "on-target", DriveID: "remote", FileID: "remote-file", FileName: "on-target.mp4",
		ContentHash: "aabbcc", Size: crawler.Size,
	}
	if err := app.cat.UpsertVideo(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := app.cat.MigrateVideoToDrive(ctx, crawler.ID, catalog.VideoDriveMigration{
		DriveID: target.DriveID, FileID: target.FileID, ContentHash: "AABBCC",
	}); err != nil {
		t.Fatal(err)
	}
	drv := &serverRemovableFakeDrive{id: target.DriveID}
	app.registry = proxy.NewRegistry()
	app.registry.Set(drv.ID(), drv)
	if err := app.cleanupDuplicateVideoAssets(ctx); err != nil {
		t.Fatal(err)
	}
	if count, err := app.cat.CountDeletedVideosPendingSourceDeletion(ctx); err != nil || count != 0 {
		t.Fatalf("pending sources = %d, %v", count, err)
	}
	app.runBlacklistSourceDelete(ctx)
	app.runBlacklistSourceDelete(ctx, api.BlacklistSourceDeleteRequest{IDs: []string{target.ID}})
	if drv.removedFileID != "" {
		t.Fatalf("deleted shared provider file %q", drv.removedFileID)
	}
	kept, err := app.cat.ResolveVideoID(ctx, target.ID)
	if err != nil || kept != crawler.ID {
		t.Fatalf("old link resolves to %q, want %q, err=%v", kept, crawler.ID, err)
	}
	v, err := app.cat.GetVideo(ctx, kept)
	if err != nil || v.DriveID != target.DriveID || v.FileID != target.FileID {
		t.Fatalf("survivor source = %+v, %v", v, err)
	}
	drive := &serverTreeScanDrive{id: target.DriveID, entries: map[string][]drives.Entry{
		"root": {{ID: target.FileID, Name: "renamed.mp4", Hash: target.ContentHash, Size: target.Size}},
	}}
	result, err := scanner.New(app.cat, drive, []string{".mp4"}, nil, nil).Scan(ctx, "root")
	if err != nil || len(result.Issues) != 0 || result.Tombstoned != 0 || result.Updated != 1 {
		t.Fatalf("rescan of survivor: %+v, %v", result, err)
	}
	v, err = app.cat.GetVideo(ctx, kept)
	if err != nil || v.FileName != "renamed.mp4" {
		t.Fatalf("renamed survivor = %+v, %v", v, err)
	}
}

func TestBlacklistCleanupSkipsSourceReassignedAfterSnapshot(t *testing.T) {
	ctx := context.Background()
	app, video, _ := seedDeferredVideoAssets(t)
	if _, err := app.deleteVideo(ctx, video.ID, catalog.VideoDeletionOptions{}); err != nil {
		t.Fatal(err)
	}
	items, err := app.cat.ListDeletedVideosPendingSourceDeletion(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("unclaimed source snapshot = %+v, %v", items, err)
	}
	drv := &serverRemovableFakeDrive{id: video.DriveID}
	app.registry = proxy.NewRegistry()
	app.registry.Set(drv.ID(), drv)
	if err := app.cat.UpsertVideo(ctx, &catalog.Video{
		ID: "other-owner", DriveID: video.DriveID, FileID: video.FileID, Hidden: true,
	}); err != nil {
		t.Fatal(err)
	}
	if skipped, err := app.deleteBlacklistedVideoSource(ctx, items[0]); err != nil || !skipped {
		t.Fatalf("cleanup after reassignment: skipped=%v, err=%v", skipped, err)
	}
	if drv.removedFileID != "" {
		t.Fatalf("removed reassigned provider file %q", drv.removedFileID)
	}
}

func TestDuplicateAssetCleanupSkipsVideoRestoredFromBackup(t *testing.T) {
	ctx := context.Background()
	app, video, paths := seedDeferredVideoAssets(t)
	if err := app.cat.UpsertVideo(ctx, &catalog.Video{ID: "kept", DriveID: "source-drive", FileID: "kept.mp4", Size: video.Size}); err != nil {
		t.Fatal(err)
	}
	if err := app.cat.ApplyDuplicateVideoDeletions(ctx, []catalog.DuplicateVideoDeletion{{VideoID: video.ID, CanonicalVideoID: "kept"}}); err != nil {
		t.Fatal(err)
	}
	// A restore brings the retired row back while the deployment-local job
	// survives it. The administrator then deletes only the record, which
	// promises to keep the assets during the undo window.
	if err := app.cat.UpsertVideo(ctx, video); err != nil {
		t.Fatal(err)
	}
	if _, err := app.deleteVideo(ctx, video.ID, catalog.VideoDeletionOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := app.cleanupPendingDuplicateAssets(ctx, app.cfg.Storage.LocalPreviewDir); err != nil {
		t.Fatal(err)
	}
	assertDeferredAssets(t, paths, true)
	if jobs, err := app.cat.ListDuplicateAssetCleanupJobs(ctx, 10); err != nil || len(jobs) != 0 {
		t.Fatalf("cleanup jobs after restore = %+v, %v", jobs, err)
	}
}

func TestBlacklistCleanupRemovesSharedFileOnce(t *testing.T) {
	ctx := context.Background()
	app, video, paths := seedDeferredVideoAssets(t)
	alias := *video
	alias.ID = "alias"
	if err := app.cat.UpsertVideo(ctx, &alias); err != nil {
		t.Fatal(err)
	}
	if _, err := app.deleteVideo(ctx, video.ID, catalog.VideoDeletionOptions{}); err != nil {
		t.Fatal(err)
	}
	rows, err := app.cat.ListDeletedVideosPendingSourceDeletion(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("pending=%v, err=%v", rows, err)
	}
	drive := &serverRemovableFakeDrive{id: video.DriveID}
	app.registry = proxy.NewRegistry()
	app.registry.Set(drive.ID(), drive)
	if skipped, err := app.deleteBlacklistedVideoSource(ctx, rows[0]); err != nil || skipped {
		t.Fatalf("first skipped=%t err=%v", skipped, err)
	}
	if drive.removedFileID != video.FileID {
		t.Fatalf("removed %q", drive.removedFileID)
	}
	drive.removedFileID = ""
	if skipped, err := app.deleteBlacklistedVideoSource(ctx, rows[1]); err != nil || !skipped || drive.removedFileID != "" {
		t.Fatalf("second skipped=%t err=%v removed=%s", skipped, err, drive.removedFileID)
	}
	if err := app.cleanupPendingDuplicateAssets(ctx, app.cfg.Storage.LocalPreviewDir); err != nil {
		t.Fatal(err)
	}
	assertDeferredAssets(t, paths, false)
}

func TestDeletionChecksMergedFileAgainstCurrentProvider(t *testing.T) {
	ctx := context.Background()
	app, root, _ := seedDeferredVideoAssets(t)
	old := &catalog.Video{ID: "retired", DriveID: "local-upload", FileID: "restored.mp4", FileName: "restored.mp4", Size: 10, ContentHash: "old"}
	if err := app.cat.UpsertVideo(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := app.cat.ApplyDuplicateVideoDeletions(ctx, []catalog.DuplicateVideoDeletion{{VideoID: old.ID, CanonicalVideoID: root.ID}}); err != nil {
		t.Fatal(err)
	}
	other := &serverRestorableLocalUploadDrive{entry: &drives.Entry{ID: old.FileID, Name: old.FileName, Size: old.Size, Hash: "new"}}
	selected := &serverRemovableFakeDrive{id: root.DriveID}
	app.registry = proxy.NewRegistry()
	app.registry.Set(other.ID(), other)
	app.registry.Set(selected.ID(), selected)
	preview, err := app.cat.PreviewVideoDeletion(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	all := true
	options := catalog.VideoDeletionOptions{DeleteSource: true, IncludeDuplicates: &all, GroupRevision: preview.GroupRevision}
	if _, err := app.deleteVideo(ctx, root.ID, options); err == nil {
		t.Fatal("removed a replaced historical file")
	}
	if selected.removedFileID != "" || other.removeCalls != 0 {
		t.Fatal("provider was asked to remove a file")
	}
	if _, err := app.cat.GetVideo(ctx, root.ID); err != nil {
		t.Fatal(err)
	}
	other.entry.Hash = old.ContentHash
	if _, err := app.deleteVideo(ctx, root.ID, options); err != nil {
		t.Fatal(err)
	}
	if other.removeCalls != 1 || selected.removedFileID != root.FileID {
		t.Fatalf("merged removal: historical=%d selected=%q", other.removeCalls, selected.removedFileID)
	}

}

func TestAdminMergedSourceDeletionVerifiesProvider(t *testing.T) {
	for _, tc := range []struct {
		name, storedHash, providerHash string
		missing                        bool
		wantErr                        error
		state                          string
		calls                          int
	}{
		{"confirmed", "old", "old", false, nil, catalog.MergedSourceGone, 1},
		{"already absent", "old", "", true, nil, catalog.MergedSourceGone, 0},
		{"replaced", "old", "new", false, catalog.ErrMergedSourceReplaced, catalog.MergedSourceReplaced, 0},
		{"identity unavailable", "", "", false, catalog.ErrMergedSourceUnverified, catalog.MergedSourceRecorded, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			app, kept, _ := seedDeferredVideoAssets(t)
			v := &catalog.Video{ID: "alias", DriveID: "local-upload", FileID: "stored.mp4", FileName: "stored.mp4", Size: 50, ContentHash: tc.storedHash}
			if err := app.cat.UpsertVideo(ctx, v); err != nil {
				t.Fatal(err)
			}
			if err := app.cat.ApplyDuplicateVideoDeletions(ctx, []catalog.DuplicateVideoDeletion{{VideoID: v.ID, CanonicalVideoID: kept.ID}}); err != nil {
				t.Fatal(err)
			}
			drv := &serverRestorableLocalUploadDrive{}
			if !tc.missing {
				drv.entry = &drives.Entry{ID: v.FileID, Name: v.FileName, Size: v.Size, Hash: tc.providerHash}
			}
			app.registry = proxy.NewRegistry()
			app.registry.Set(drv.ID(), drv)
			app.runBlacklistSourceDelete(ctx)
			if drv.removeCalls != 0 || drv.statCalls != 0 {
				t.Fatal("blacklist cleanup touched automatically merged source")
			}
			m, err := app.cat.GetMergedVideo(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			err = app.deleteMergedSource(ctx, v.ID, m.SourceRevision)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("removal error=%v want=%v", err, tc.wantErr)
			}
			if tc.wantErr == nil {
				if err := app.deleteMergedSource(ctx, v.ID, m.SourceRevision); err != nil {
					t.Fatal(err)
				}
			}
			m, err = app.cat.GetMergedVideo(ctx, v.ID)
			if err != nil || m.SourceState != tc.state || drv.removeCalls != tc.calls {
				t.Fatalf("record=%+v calls=%d err=%v", m, drv.removeCalls, err)
			}
			if got, err := app.cat.ResolveVideoID(ctx, v.ID); err != nil || got != kept.ID {
				t.Fatalf("resolved=%s err=%v", got, err)
			}
		})
	}
}
