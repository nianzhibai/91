package catalog

import (
	"context"
	"testing"
	"time"
)

func TestPreviewRevisionChangesOnlyWithPreviewAsset(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	revision := time.UnixMilli(1_778_863_000_123)
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &Video{
		ID:               "video-1",
		DriveID:          "drive-1",
		FileID:           "file-1",
		Title:            "Video",
		PreviewLocal:     "/tmp/video-1.mp4",
		PreviewStatus:    "ready",
		PreviewUpdatedAt: revision,
		PublishedAt:      now,
		CreatedAt:        now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	initial, err := cat.GetVideo(ctx, "video-1")
	if err != nil {
		t.Fatalf("get initial video: %v", err)
	}
	if !initial.PreviewUpdatedAt.Equal(revision) {
		t.Fatalf("initial preview revision = %v, want %v", initial.PreviewUpdatedAt, revision)
	}

	if _, err := cat.IncrementView(ctx, "video-1"); err != nil {
		t.Fatalf("increment view: %v", err)
	}
	afterMetadata, err := cat.GetVideo(ctx, "video-1")
	if err != nil {
		t.Fatalf("get video after metadata update: %v", err)
	}
	if !afterMetadata.PreviewUpdatedAt.Equal(revision) {
		t.Fatalf("metadata update changed preview revision: got %v, want %v", afterMetadata.PreviewUpdatedAt, revision)
	}

	if err := cat.UpdatePreviewForSource(ctx, storedVideo(t, cat, "video-1"), "/tmp/video-1-v2.mp4", "ready"); err != nil {
		t.Fatalf("update preview: %v", err)
	}
	afterPreview, err := cat.GetVideo(ctx, "video-1")
	if err != nil {
		t.Fatalf("get video after preview update: %v", err)
	}
	if !afterPreview.PreviewUpdatedAt.After(revision) {
		t.Fatalf("preview update revision = %v, want after %v", afterPreview.PreviewUpdatedAt, revision)
	}

	if err := cat.ClearGeneratedAssets(ctx, "video-1", true, false); err != nil {
		t.Fatalf("clear preview: %v", err)
	}
	afterClear, err := cat.GetVideo(ctx, "video-1")
	if err != nil {
		t.Fatalf("get video after clear: %v", err)
	}
	if !afterClear.PreviewUpdatedAt.IsZero() {
		t.Fatalf("cleared preview revision = %v, want zero", afterClear.PreviewUpdatedAt)
	}
}

func TestReadyPreviewRequeuesFailedThumbnail(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	video := &Video{
		ID:            "preview-recovers-thumbnail",
		DriveID:       "drive-1",
		FileID:        "file-1",
		Title:         "Video",
		PreviewStatus: "pending",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if err := cat.UpdateVideoMeta(ctx, video.ID, VideoMetaPatch{ThumbnailStatus: "failed"}); err != nil {
		t.Fatalf("mark thumbnail failed: %v", err)
	}
	if err := cat.UpdatePreviewForSource(ctx, storedVideo(t, cat, video.ID), "/tmp/preview.mp4", "ready"); err != nil {
		t.Fatalf("mark preview ready: %v", err)
	}

	pending, err := cat.ListVideosByThumbnailStatus(ctx, video.DriveID, "pending", 0)
	if err != nil {
		t.Fatalf("list pending thumbnails: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != video.ID {
		t.Fatalf("pending thumbnails = %#v, want recovered video", pending)
	}
}

func TestReadyPreviewRefreshesPendingThumbnailRetryBudget(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	video := &Video{
		ID:            "preview-refreshes-active-thumbnail",
		DriveID:       "drive-1",
		FileID:        "file-1",
		Title:         "Video",
		PreviewStatus: "pending",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := cat.IncrementThumbnailFailuresForSource(ctx, storedVideo(t, cat, video.ID)); err != nil {
			t.Fatalf("increment thumbnail failures: %v", err)
		}
	}
	if err := cat.UpdatePreviewForSource(ctx, storedVideo(t, cat, video.ID), "/tmp/preview.mp4", "ready"); err != nil {
		t.Fatalf("mark preview ready: %v", err)
	}

	failures, err := cat.IncrementThumbnailFailuresForSource(ctx, storedVideo(t, cat, video.ID))
	if err != nil {
		t.Fatalf("read refreshed failure budget: %v", err)
	}
	if failures != 1 {
		t.Fatalf("thumbnail failures after preview recovery = %d, want reset baseline of 1", failures)
	}
}

func TestLegacyFailedThumbnailPreviewRecoveryRunsOnce(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	const marker = "videos.failed_thumbnail.ready_preview_requeued_v2"
	if _, err := cat.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, marker); err != nil {
		t.Fatalf("clear migration marker: %v", err)
	}

	now := time.Now()
	video := &Video{
		ID:            "legacy-preview-recovers-thumbnail",
		DriveID:       "drive-1",
		FileID:        "file-1",
		Title:         "Video",
		PreviewLocal:  "/tmp/existing-preview.mp4",
		PreviewStatus: "ready",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if err := cat.UpdateVideoMeta(ctx, video.ID, VideoMetaPatch{ThumbnailStatus: "failed"}); err != nil {
		t.Fatalf("mark thumbnail failed: %v", err)
	}
	if err := cat.requeueFailedThumbnailsWithReadyPreviewOnce(ctx); err != nil {
		t.Fatalf("run recovery migration: %v", err)
	}
	pending, err := cat.ListVideosByThumbnailStatus(ctx, video.DriveID, "pending", 0)
	if err != nil || len(pending) != 1 || pending[0].ID != video.ID {
		t.Fatalf("pending after migration = %#v, err=%v", pending, err)
	}

	if err := cat.UpdateVideoMeta(ctx, video.ID, VideoMetaPatch{ThumbnailStatus: "failed"}); err != nil {
		t.Fatalf("mark thumbnail failed again: %v", err)
	}
	if err := cat.requeueFailedThumbnailsWithReadyPreviewOnce(ctx); err != nil {
		t.Fatalf("rerun recovery migration: %v", err)
	}
	failed, err := cat.ListVideosByThumbnailStatus(ctx, video.DriveID, "failed", 0)
	if err != nil || len(failed) != 1 || failed[0].ID != video.ID {
		t.Fatalf("failed after guarded rerun = %#v, err=%v", failed, err)
	}
}
