package catalog

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestGeneratedThumbnailNeverReplacesAnotherOwnersCover(t *testing.T) {
	ctx := context.Background()
	cat := openMediaTestCatalog(t, mediaTestVideo("bare", 90), mediaTestVideo("custom", 90))
	custom, _ := cat.GetVideo(ctx, "custom")
	custom.ThumbnailURL, custom.ThumbnailSource = "https://covers.example/c.jpg", ThumbnailCustom
	if err := cat.UpsertVideo(ctx, custom); err != nil {
		t.Fatal(err)
	}

	bare, _ := cat.GetVideo(ctx, "bare")
	if !bare.GeneratesThumbnail() {
		t.Fatal("a video without a cover must get a generated one")
	}
	if err := cat.SaveGeneratedThumbnailForSource(ctx, bare); err != nil {
		t.Fatal(err)
	}
	saved, _ := cat.GetVideo(ctx, "bare")
	if saved.ThumbnailURL != "/p/thumb/bare" || saved.ThumbnailSource != ThumbnailGenerated || saved.ThumbnailUpdatedAt.IsZero() || !saved.GeneratesThumbnail() {
		t.Fatalf("saved cover = %q/%q at %v", saved.ThumbnailURL, saved.ThumbnailSource, saved.ThumbnailUpdatedAt)
	}
	// A renewal replaces the generated cover and bumps its revision.
	if err := cat.SaveGeneratedThumbnailForSource(ctx, saved); err != nil {
		t.Fatal(err)
	}
	if renewed, _ := cat.GetVideo(ctx, "bare"); !renewed.ThumbnailUpdatedAt.After(saved.ThumbnailUpdatedAt) {
		t.Fatal("renewed cover kept its revision")
	}

	// A job that started before the administrator set a cover loses.
	stale, _ := cat.GetVideo(ctx, "custom")
	stale.ThumbnailURL, stale.ThumbnailSource = "", ""
	if err := cat.SaveGeneratedThumbnailForSource(ctx, stale); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("overwriting a custom cover: err = %v, want sql.ErrNoRows", err)
	}
	if kept, _ := cat.GetVideo(ctx, "custom"); kept.ThumbnailURL != "https://covers.example/c.jpg" || kept.ThumbnailSource != ThumbnailCustom || kept.GeneratesThumbnail() {
		t.Fatalf("custom cover = %q/%q", kept.ThumbnailURL, kept.ThumbnailSource)
	}

	// Replaced content clears the cover with its owner.
	if err := cat.UpdateVideoMeta(ctx, "custom", VideoMetaPatch{ResetContent: true, Size: 200}); err != nil {
		t.Fatal(err)
	}
	if reset, _ := cat.GetVideo(ctx, "custom"); reset.ThumbnailURL != "" || reset.ThumbnailSource != "" {
		t.Fatalf("reset cover = %q/%q", reset.ThumbnailURL, reset.ThumbnailSource)
	}
}

func TestThumbnailSourcesAreBackfilledWhenTheColumnIsAdded(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/catalog.db"
	cat, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, video := range []*Video{
		{ID: "generated", ThumbnailURL: "/p/thumb/generated"},
		{ID: "crawled", ThumbnailURL: "/p/thumb/crawled"},
		{ID: "custom", ThumbnailURL: "https://covers.example/c.jpg"},
		{ID: "bare"},
	} {
		video.DriveID, video.FileID, video.Title, video.Size = "drive", video.ID, video.ID, 100
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cat.db.ExecContext(ctx, `INSERT INTO crawler_seen_sources
		(kind, drive_id, source_id, status, canonical_video_id, first_seen_at, last_seen_at)
		VALUES ('scriptcrawler', 'crawler', 'item', 'imported', 'crawled', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.db.ExecContext(ctx, `ALTER TABLE videos DROP COLUMN thumbnail_source`); err != nil {
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	for id, want := range map[string]string{
		"generated": ThumbnailGenerated, "crawled": ThumbnailProvided, "custom": ThumbnailCustom, "bare": "",
	} {
		if got, _ := reopened.GetVideo(ctx, id); got.ThumbnailSource != want {
			t.Errorf("%s: source = %q, want %q", id, got.ThumbnailSource, want)
		}
	}
}
