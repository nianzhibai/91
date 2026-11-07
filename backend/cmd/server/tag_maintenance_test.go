package main

import (
	"context"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
)

func openServerTagMaintenanceCatalog(t *testing.T) (*catalog.Catalog, context.Context) {
	t.Helper()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	return cat, context.Background()
}

func seedServerTagVideo(t *testing.T, cat *catalog.Catalog, id, title string) {
	t.Helper()
	now := time.Now()
	if err := cat.UpsertVideo(context.Background(), &catalog.Video{
		ID:          id,
		DriveID:     "drive",
		FileID:      "file-" + id,
		FileName:    id + ".mp4",
		Title:       title,
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func mustListTagsForServer(t *testing.T, ctx context.Context, cat *catalog.Catalog) []catalog.Tag {
	t.Helper()
	tags, err := cat.ListTags(ctx)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	return tags
}

func TestRunTagRetagRefreshesExistingMatches(t *testing.T) {
	cat, ctx := openServerTagMaintenanceCatalog(t)
	seedServerTagVideo(t, cat, "retag-video", "retag-keyword")
	if _, err := cat.EnsureTag(ctx, "retag-keyword", "user"); err != nil {
		t.Fatalf("ensure retag label: %v", err)
	}
	app := &App{cat: cat, cfg: &config.Config{}}
	if !app.beginTagJob("retag") {
		t.Fatal("begin retag job rejected")
	}
	app.runTagRetag(ctx)

	status := app.tagJobStatus()
	if status.State != "completed" || status.Running || status.Processed != 1 {
		t.Fatalf("status = %#v", status)
	}
	video, _ := cat.GetVideo(ctx, "retag-video")
	if len(video.Tags) != 1 || video.Tags[0] != "retag-keyword" {
		t.Fatalf("retagged labels = %#v, want retag-keyword", video.Tags)
	}
}

func TestRunTagRetagRemovesStaleAutomaticAssignments(t *testing.T) {
	cat, ctx := openServerTagMaintenanceCatalog(t)
	seedServerTagVideo(t, cat, "retag-video", "matching-keyword clip")
	if _, err := cat.EnsureTag(ctx, "matching-keyword", "user"); err != nil {
		t.Fatalf("ensure user label: %v", err)
	}
	if _, err := cat.EnsureTag(ctx, "stale-label", "user"); err != nil {
		t.Fatalf("ensure old label: %v", err)
	}
	if _, err := cat.AddVideoTagAssignments(ctx, "retag-video", []catalog.TagAssignment{{
		Label: "stale-label", Source: "auto", Evidence: "old",
	}}); err != nil {
		t.Fatalf("seed old auto assignment: %v", err)
	}

	app := &App{cat: cat, cfg: &config.Config{}}
	if !app.beginTagJob("retag") {
		t.Fatal("begin retag job rejected")
	}
	app.runTagRetag(ctx)

	status := app.tagJobStatus()
	if status.State != "completed" || status.Running || status.Processed != 1 {
		t.Fatalf("status = %#v", status)
	}
	video, err := cat.GetVideo(ctx, "retag-video")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if len(video.Tags) != 1 || video.Tags[0] != "matching-keyword" {
		t.Fatalf("refreshed tags = %#v, want matching-keyword", video.Tags)
	}
	foundOldLabel := false
	for _, tag := range mustListTagsForServer(t, ctx, cat) {
		if tag.Label == "stale-label" {
			foundOldLabel = true
		}
	}
	if !foundOldLabel {
		t.Fatal("user tag definition stale-label was removed")
	}
	if _, err := cat.EnsureTag(ctx, "blocked-generated", "generated"); err != catalog.ErrInvalidTagSource {
		t.Fatalf("ordinary generated source should be rejected, got %v", err)
	}
}
