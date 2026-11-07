package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
)

func TestDeleteDuplicateWithAssetsRemovesThumbnailDerivedAssets(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	localDir := filepath.Join(root, "previews")
	cat, err := catalog.Open(filepath.Join(root, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	canonical := &catalog.Video{
		ID:          "video-canonical",
		DriveID:     "drive-a",
		FileID:      "canonical.mp4",
		FileName:    "canonical.mp4",
		Title:       "Canonical",
		Size:        200,
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	duplicate := &catalog.Video{
		ID:            "video-duplicate",
		DriveID:       "drive-b",
		FileID:        "duplicate.mp4",
		FileName:      "duplicate.mp4",
		Title:         "Duplicate",
		Size:          100,
		PreviewLocal:  mediaasset.PreviewPath(localDir, "video-duplicate"),
		PreviewStatus: "ready",
		ThumbnailURL:  "/p/thumb/video-duplicate",
		PublishedAt:   now.Add(time.Second),
		CreatedAt:     now.Add(time.Second),
		UpdatedAt:     now.Add(time.Second),
	}
	for _, video := range []*catalog.Video{canonical, duplicate} {
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatalf("seed %s: %v", video.ID, err)
		}
	}

	assets := []string{
		duplicate.PreviewLocal,
		mediaasset.ThumbnailPath(localDir, duplicate.ID),
		mediaasset.ShortsBackgroundPath(localDir, duplicate.ID),
		mediaasset.FrameSignaturePath(localDir, duplicate.ID),
	}
	for _, path := range assets {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte("generated asset"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	if err := deleteDuplicateWithAssets(ctx, cat, localDir, duplicate, canonical.ID); err != nil {
		t.Fatalf("delete duplicate: %v", err)
	}
	for _, path := range assets {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("asset %s still exists, stat error=%v", path, err)
		}
	}
	if _, err := cat.GetVideo(ctx, duplicate.ID); err != sql.ErrNoRows {
		t.Fatalf("duplicate lookup error=%v, want sql.ErrNoRows", err)
	}
	if _, err := cat.GetVideo(ctx, canonical.ID); err != nil {
		t.Fatalf("canonical video missing: %v", err)
	}
	if deleted, err := cat.IsVideoDeleted(ctx, duplicate.ID); err != nil || !deleted {
		t.Fatalf("duplicate tombstone deleted=%v error=%v, want true/nil", deleted, err)
	}
}
