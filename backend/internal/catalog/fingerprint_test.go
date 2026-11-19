package catalog

import (
	"context"
	"testing"
	"time"
)

func TestListVideosDeduplicatesBySampledSHA256(t *testing.T) {
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

	now := time.Now()
	for _, v := range []*Video{
		{
			ID:          "drive-a-file-a",
			DriveID:     "drive-a",
			FileID:      "file-a",
			FileName:    "first-name.mp4",
			Title:       "First",
			Size:        1234,
			PublishedAt: now.Add(-time.Minute),
			CreatedAt:   now.Add(-time.Minute),
			UpdatedAt:   now.Add(-time.Minute),
		},
		{
			ID:          "drive-b-file-b",
			DriveID:     "drive-b",
			FileID:      "file-b",
			FileName:    "second-name.mp4",
			Title:       "Second",
			Size:        1234,
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("upsert %s: %v", v.ID, err)
		}
	}

	items, total, err := cat.ListVideos(ctx, ListParams{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("list before fingerprint: %v", err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("before fingerprint total=%d len=%d, want 2", total, len(items))
	}

	const sampled = "abc123"
	if _, err := cat.UpdateVideoFingerprintForSource(ctx, storedVideo(t, cat, "drive-a-file-a"), sampled, "ready", ""); err != nil {
		t.Fatalf("update a fingerprint: %v", err)
	}
	if _, err := cat.UpdateVideoFingerprintForSource(ctx, storedVideo(t, cat, "drive-b-file-b"), sampled, "ready", ""); err != nil {
		t.Fatalf("update b fingerprint: %v", err)
	}

	items, total, err = cat.ListVideos(ctx, ListParams{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("list after fingerprint: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("after fingerprint total=%d len=%d, want 1", total, len(items))
	}
	if items[0].ID != "drive-a-file-a" {
		t.Fatalf("canonical id = %q, want earliest created video", items[0].ID)
	}
}

func TestDuplicateAssetCleanupCandidates(t *testing.T) {
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

	base := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	videos := []*Video{
		{
			ID:            "drive-a-canonical",
			DriveID:       "drive-a",
			FileID:        "file-a",
			FileName:      "canonical.mp4",
			Title:         "Canonical",
			Size:          1234,
			ThumbnailURL:  "/p/thumb/drive-a-canonical",
			PreviewLocal:  "/tmp/previews/canonical.mp4",
			PreviewStatus: "ready",
			PublishedAt:   base,
			CreatedAt:     base,
			UpdatedAt:     base,
		},
		{
			ID:            "drive-b-duplicate",
			DriveID:       "drive-b",
			FileID:        "file-b",
			FileName:      "duplicate.mp4",
			Title:         "Duplicate",
			Size:          1234,
			ThumbnailURL:  "/p/thumb/drive-b-duplicate",
			PreviewLocal:  "/tmp/previews/duplicate.mp4",
			PreviewStatus: "ready",
			PublishedAt:   base.Add(time.Second),
			CreatedAt:     base.Add(time.Second),
			UpdatedAt:     base.Add(time.Second),
		},
		{
			ID:            "drive-c-remote-thumb",
			DriveID:       "drive-c",
			FileID:        "file-c",
			FileName:      "remote-thumb.mp4",
			Title:         "Remote Thumbnail",
			Size:          1234,
			ThumbnailURL:  "https://thumb.example/file-c.jpg",
			PreviewStatus: "ready",
			PublishedAt:   base.Add(2 * time.Second),
			CreatedAt:     base.Add(2 * time.Second),
			UpdatedAt:     base.Add(2 * time.Second),
		},
	}
	for _, v := range videos {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed %s: %v", v.ID, err)
		}
	}
	const sampled = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, v := range videos {
		if _, err := cat.UpdateVideoFingerprintForSource(ctx, storedVideo(t, cat, v.ID), sampled, "ready", ""); err != nil {
			t.Fatalf("fingerprint %s: %v", v.ID, err)
		}
	}

	items, err := cat.ListDuplicateAssetCleanupCandidates(ctx, 0)
	if err != nil {
		t.Fatalf("list cleanup candidates: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("candidates = %#v, want only local duplicate", items)
	}
	item := items[0]
	if item.VideoID != "drive-b-duplicate" || item.CanonicalID != "drive-a-canonical" {
		t.Fatalf("candidate = %#v, want duplicate with canonical", item)
	}

	if err := cat.ClearGeneratedAssets(ctx, item.VideoID, true, true); err != nil {
		t.Fatalf("clear generated assets: %v", err)
	}
	got, err := cat.GetVideo(ctx, item.VideoID)
	if err != nil {
		t.Fatalf("get duplicate: %v", err)
	}
	if got.PreviewLocal != "" || got.PreviewStatus != "pending" {
		t.Fatalf("preview after cleanup local=%q status=%q, want empty pending", got.PreviewLocal, got.PreviewStatus)
	}
	if got.ThumbnailURL != "" {
		t.Fatalf("thumbnail after cleanup = %q, want empty", got.ThumbnailURL)
	}
	var thumbStatus string
	if err := cat.db.QueryRowContext(ctx, `SELECT thumbnail_status FROM videos WHERE id = ?`, item.VideoID).Scan(&thumbStatus); err != nil {
		t.Fatalf("query thumbnail status: %v", err)
	}
	if thumbStatus != "pending" {
		t.Fatalf("thumbnail_status = %q, want pending", thumbStatus)
	}
}

// storedVideo loads the current row for writes guarded by the row's source.
func storedVideo(t *testing.T, cat *Catalog, id string) *Video {
	t.Helper()
	v, err := cat.GetVideo(context.Background(), id)
	if err != nil {
		t.Fatalf("load %s: %v", id, err)
	}
	return v
}
