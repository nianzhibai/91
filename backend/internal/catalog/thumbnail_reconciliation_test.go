package catalog

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestResetMissingLocalThumbnailsGuardsVersionAndDeduplicates(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()

	now := time.Now()
	for _, video := range []*Video{
		{
			ID: "local", DriveID: "drive", FileID: "local-file", Title: "Local",
			ThumbnailURL: "/p/thumb/local", PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "remote", DriveID: "drive", FileID: "remote-file", Title: "Remote",
			ThumbnailURL: "https://example.test/remote.jpg", PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "refreshed", DriveID: "drive", FileID: "refreshed-file", Title: "Refreshed",
			ThumbnailURL: "/p/thumb/refreshed", PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		},
	} {
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatalf("seed %s: %v", video.ID, err)
		}
	}
	if _, err := cat.IncrementThumbnailFailuresForSource(ctx, storedVideo(t, cat, "local")); err != nil {
		t.Fatalf("increment local thumbnail failures: %v", err)
	}
	references, err := cat.ListCanonicalLocalThumbnailReferences(ctx)
	if err != nil {
		t.Fatalf("list local thumbnail references: %v", err)
	}
	if len(references) != 2 {
		t.Fatalf("references = %#v, want local and refreshed", references)
	}
	if err := cat.UpdateVideoMeta(ctx, "refreshed", VideoMetaPatch{
		ThumbnailURL: "/p/thumb/refreshed",
	}); err != nil {
		t.Fatalf("refresh thumbnail concurrently: %v", err)
	}
	references = append(references, references[0])

	reset, err := cat.ResetMissingLocalThumbnails(ctx, references)
	if err != nil {
		t.Fatalf("reset missing local thumbnails: %v", err)
	}
	if reset != 1 {
		t.Fatalf("reset = %d, want 1", reset)
	}
	local, err := cat.GetVideo(ctx, "local")
	if err != nil {
		t.Fatalf("get local video: %v", err)
	}
	if local.ThumbnailURL != "" || !local.ThumbnailUpdatedAt.IsZero() {
		t.Fatalf("local thumbnail = %q updated=%v", local.ThumbnailURL, local.ThumbnailUpdatedAt)
	}
	failures, err := cat.IncrementThumbnailFailuresForSource(ctx, storedVideo(t, cat, "local"))
	if err != nil {
		t.Fatalf("increment reconciled thumbnail failures: %v", err)
	}
	if failures != 1 {
		t.Fatalf("reconciled thumbnail failures = %d, want reset baseline of 1", failures)
	}
	remote, err := cat.GetVideo(ctx, "remote")
	if err != nil {
		t.Fatalf("get remote video: %v", err)
	}
	if remote.ThumbnailURL != "https://example.test/remote.jpg" {
		t.Fatalf("remote thumbnail = %q", remote.ThumbnailURL)
	}
	refreshed, err := cat.GetVideo(ctx, "refreshed")
	if err != nil {
		t.Fatalf("get refreshed video: %v", err)
	}
	if refreshed.ThumbnailURL != "/p/thumb/refreshed" {
		t.Fatalf("refreshed thumbnail url=%q", refreshed.ThumbnailURL)
	}
	pending, err := cat.ListVideosByThumbnailStatus(ctx, "drive", "pending", 0)
	if err != nil {
		t.Fatalf("list pending thumbnails: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "local" {
		t.Fatalf("pending thumbnails = %#v, want local only", pending)
	}
}

func TestResetMissingLocalPreviewsGuardsExactReadyReference(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()

	now := time.Now()
	for _, video := range []*Video{
		{
			ID: "missing", DriveID: "drive", FileID: "missing-file", Title: "Missing",
			PreviewLocal: "/previews/missing.mp4", PreviewStatus: "ready",
			PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "refreshed", DriveID: "drive", FileID: "refreshed-file", Title: "Refreshed",
			PreviewLocal: "/previews/old.mp4", PreviewStatus: "ready",
			PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		},
	} {
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatalf("seed %s: %v", video.ID, err)
		}
	}
	references, err := cat.ListReadyLocalPreviewReferences(ctx)
	if err != nil {
		t.Fatalf("list ready local previews: %v", err)
	}
	if len(references) != 2 {
		t.Fatalf("references = %#v", references)
	}
	if err := cat.UpdatePreviewForSource(ctx, storedVideo(t, cat, "refreshed"), "/previews/new.mp4", "ready"); err != nil {
		t.Fatalf("refresh preview concurrently: %v", err)
	}
	references = append(references, references[0])

	reset, err := cat.ResetMissingLocalPreviews(ctx, references)
	if err != nil {
		t.Fatalf("reset missing local previews: %v", err)
	}
	if reset != 1 {
		t.Fatalf("reset = %d, want 1", reset)
	}
	missing, err := cat.GetVideo(ctx, "missing")
	if err != nil {
		t.Fatalf("get missing: %v", err)
	}
	if missing.PreviewStatus != "pending" || missing.PreviewLocal != "" || !missing.PreviewUpdatedAt.IsZero() {
		t.Fatalf("missing preview status=%q local=%q updated=%v", missing.PreviewStatus, missing.PreviewLocal, missing.PreviewUpdatedAt)
	}
	refreshed, err := cat.GetVideo(ctx, "refreshed")
	if err != nil {
		t.Fatalf("get refreshed: %v", err)
	}
	if refreshed.PreviewStatus != "ready" || refreshed.PreviewLocal != "/previews/new.mp4" {
		t.Fatalf("refreshed preview status=%q local=%q", refreshed.PreviewStatus, refreshed.PreviewLocal)
	}
}
