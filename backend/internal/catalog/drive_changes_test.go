package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/driveevents"
)

func TestDriveChangesFollowCommittedWritesAndExcludeFailedWrites(t *testing.T) {
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	ctx := context.Background()
	if err := cat.UpsertDrive(ctx, &Drive{ID: "a", Kind: "quark"}); err != nil {
		t.Fatal(err)
	}
	sub := cat.DriveEvents().Subscribe("a")
	defer sub.Close()
	before := cat.DriveEvents().Version("a", driveevents.DriveMetadataChanged)
	if err := cat.SetDriveSkipDirIDs(ctx, "missing", []string{"x"}); err == nil {
		t.Fatal("missing write unexpectedly succeeded")
	}
	if cat.DriveEvents().Version("a", driveevents.DriveMetadataChanged) != before {
		t.Fatal("failed write invalidated config")
	}
	if err := cat.SetDriveSkipDirIDs(ctx, "a", []string{"hidden"}); err != nil {
		t.Fatal(err)
	}
	<-sub.Wake
	drive, err := cat.GetDrive(ctx, "a")
	if err != nil || len(drive.SkipDirIDs) != 1 || drive.SkipDirIDs[0] != "hidden" {
		t.Fatal("notification preceded committed configuration")
	}
	sub.Take()
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &Video{ID: "video", DriveID: "a", FileID: "file", Title: "Video", Size: 10, CreatedAt: now, PublishedAt: now}); err != nil {
		t.Fatal(err)
	}
	<-sub.Wake
	change := sub.Take()
	if _, ok := change.Kinds[driveevents.MediaChanged]; !ok {
		t.Fatal("insert did not invalidate counts")
	}
	stats, err := cat.CountDriveAssetStatsForDrive(ctx, "a")
	if err != nil || stats.Teasers["a"].Pending != 1 {
		t.Fatal("notification preceded committed video")
	}
	before = cat.DriveEvents().Version("a", driveevents.MediaChanged)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := cat.UpdatePreview(canceled, "video", "preview.mp4", "ready"); err == nil {
		t.Fatal("canceled write succeeded")
	}
	if cat.DriveEvents().Version("a", driveevents.MediaChanged) != before {
		t.Fatal("canceled write invalidated counts")
	}
}
