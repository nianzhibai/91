package catalog

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestClearScannerParsedAuthorsOnce(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	if err := cat.UpsertDrive(ctx, &Drive{
		ID:   "crawler-demo",
		Kind: "scriptcrawler",
		Name: "DemoCrawler",
	}); err != nil {
		t.Fatalf("upsert crawler drive: %v", err)
	}
	now := time.Now()
	seed := func(id, driveID, fileName, author string) {
		t.Helper()
		if err := cat.UpsertVideo(ctx, &Video{
			ID:          id,
			DriveID:     driveID,
			FileID:      id,
			FileName:    fileName,
			Title:       id,
			Author:      author,
			Size:        1,
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatalf("upsert video %s: %v", id, err)
		}
	}

	seed("pikpak-drive-hnd", "drive", "HND-970.mp4", "970")
	seed("local-drive-bracket", "drive", "[4K] 标题 - 某作者.mp4", "某作者")
	seed("local-drive-manual", "drive", "HND-970.mp4", "Explicit Author")
	seed("scriptcrawler-crawler-demo-item", "drive", "Title - Real Author.mp4", "Real Author")
	seed("custom-crawler-id", "crawler-demo", "Title - Crawler Author.mp4", "Crawler Author")

	if err := cat.SetSetting(ctx, settingScannerParsedAuthorsCleaned, ""); err != nil {
		t.Fatalf("reset migration marker: %v", err)
	}
	removed, err := cat.clearScannerParsedAuthorsOnce(ctx)
	if err != nil {
		t.Fatalf("clear scanner parsed authors: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}

	assertAuthor := func(id, want string) {
		t.Helper()
		video, err := cat.GetVideo(ctx, id)
		if err != nil {
			t.Fatalf("get video %s: %v", id, err)
		}
		if video.Author != want {
			t.Fatalf("video %s author = %q, want %q", id, video.Author, want)
		}
	}
	assertAuthor("pikpak-drive-hnd", "")
	assertAuthor("local-drive-bracket", "")
	assertAuthor("local-drive-manual", "Explicit Author")
	assertAuthor("scriptcrawler-crawler-demo-item", "Real Author")
	assertAuthor("custom-crawler-id", "Crawler Author")

	seed("pikpak-drive-after-marker", "drive", "SSIS-001.mp4", "001")
	removed, err = cat.clearScannerParsedAuthorsOnce(ctx)
	if err != nil {
		t.Fatalf("rerun migration: %v", err)
	}
	if removed != 0 {
		t.Fatalf("second removed = %d, want 0", removed)
	}
	assertAuthor("pikpak-drive-after-marker", "001")
}
