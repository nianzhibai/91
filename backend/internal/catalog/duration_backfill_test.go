package catalog

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDurationBackfillSelectionUsesCurrentMetadataAndCanonicalVisibility(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	now := time.Now()
	for _, id := range []string{"pending", "known", "hidden", "duplicate", "other", "thumbnail-failed"} {
		video := &Video{ID: id, DriveID: "drive", FileID: id, Title: id, Size: 100, PublishedAt: now, CreatedAt: now, UpdatedAt: now}
		if id == "known" {
			video.DurationSeconds = 42
		}
		if id == "hidden" {
			video.Hidden = true
		}
		if id == "other" {
			video.DriveID = "other"
		}
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cat.db.ExecContext(ctx, `UPDATE videos SET is_canonical = 0 WHERE id = 'duplicate'`); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateVideoMeta(ctx, "thumbnail-failed", VideoMetaPatch{ThumbnailStatus: "failed"}); err != nil {
		t.Fatal(err)
	}
	if videos := dueForStep(t, cat, "drive", true, needsDuration); !reflect.DeepEqual(videos, []string{"pending", "thumbnail-failed"}) {
		t.Fatalf("candidates = %v", videos)
	}
	if err := cat.UpdateVideoMeta(ctx, "pending", VideoMetaPatch{DurationSeconds: 19}); err != nil {
		t.Fatal(err)
	}
	if videos := dueForStep(t, cat, "drive", true, needsDuration); !reflect.DeepEqual(videos, []string{"thumbnail-failed"}) {
		t.Fatalf("completed candidate returned: %v", videos)
	}
}

func TestLegacyDurationFailureMigrationKeepsExistingCoverReady(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "catalog.db")
	cat, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	video := &Video{ID: "legacy", DriveID: "drive", FileID: "legacy", Title: "Legacy", ThumbnailURL: "/p/thumb/legacy", PublishedAt: now, CreatedAt: now, UpdatedAt: now}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.db.ExecContext(ctx, `UPDATE videos SET thumbnail_status = 'skipped' WHERE id = 'legacy'`); err != nil {
		t.Fatal(err)
	}
	if err := cat.SetSetting(ctx, "videos.duration_backfill.finish_round.v1", ""); err != nil {
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	cat, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	ready, err := cat.ListVideosByThumbnailStatus(ctx, "drive", "ready", 100)
	if err != nil || len(ready) != 1 {
		t.Fatalf("legacy cover not ready: %v, %v", ready, err)
	}
	if missing := dueForStep(t, cat, "drive", true, needsDuration); len(missing) != 1 {
		t.Fatalf("legacy duration not eligible: %v", missing)
	}
}
