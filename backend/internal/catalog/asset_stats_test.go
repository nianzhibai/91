package catalog

import (
	"context"
	"testing"
	"time"
)

func TestCountDriveAssetStatsKeepsCanonicalAndFingerprintScopes(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	for _, video := range []*Video{
		{
			ID: "canonical", DriveID: "drive", FileID: "canonical.mp4",
			Title: "Canonical", Size: 100, PreviewStatus: "pending", PublishedAt: now,
		},
		{
			ID: "duplicate", DriveID: "drive", FileID: "duplicate.mp4",
			Title: "Duplicate", Size: 200, ThumbnailURL: "/p/thumb/duplicate",
			PreviewStatus: "ready", PublishedAt: now,
		},
	} {
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatalf("seed %s: %v", video.ID, err)
		}
	}
	if _, err := cat.UpdateVideoFingerprintForSource(ctx, storedVideo(t, cat, "duplicate"), "sha-duplicate", "ready", ""); err != nil {
		t.Fatalf("mark duplicate fingerprint ready: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx, `UPDATE videos SET is_canonical = 0 WHERE id = 'duplicate'`); err != nil {
		t.Fatalf("mark duplicate non-canonical: %v", err)
	}

	stats, err := cat.CountDriveAssetStats(ctx)
	if err != nil {
		t.Fatalf("count drive assets: %v", err)
	}
	if got := stats.Teasers["drive"]; got.Ready != 0 || got.Pending != 1 {
		t.Fatalf("teaser counts = %#v, want only canonical pending row", got)
	}
	if got := stats.Thumbnails["drive"]; got.Ready != 0 || got.Pending != 1 {
		t.Fatalf("thumbnail counts = %#v, want only canonical pending row", got)
	}
	if got := stats.Fingerprints["drive"]; got.Ready != 1 || got.Pending != 1 {
		t.Fatalf("fingerprint counts = %#v, want ready duplicate and pending canonical", got)
	}
}
