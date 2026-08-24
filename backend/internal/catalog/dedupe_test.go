package catalog

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestApplyDuplicateVideoDeletionsIsAtomicAndRetargetsReferences(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	videos := []*Video{
		{ID: "old-a", DriveID: "drive", FileID: "a", PreviewLocal: "/previews/a.mp4", Size: 100, PublishedAt: now, CreatedAt: now},
		{ID: "old-b", DriveID: "drive", FileID: "b", PreviewLocal: "/previews/b.mp4", Size: 100, PublishedAt: now, CreatedAt: now.Add(time.Second)},
		{ID: "final", DriveID: "drive", FileID: "c", Size: 200, PublishedAt: now, CreatedAt: now.Add(2 * time.Second)},
	}
	for _, video := range videos {
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatalf("seed %s: %v", video.ID, err)
		}
	}
	if _, err := cat.db.ExecContext(ctx, `
INSERT INTO deleted_videos (id, reason, canonical_video_id, deleted_at)
VALUES ('historical', 'duplicate', 'old-a', 1)`); err != nil {
		t.Fatalf("seed historical tombstone: %v", err)
	}
	if err := cat.MarkCrawlerSourceSeen(ctx, "scriptcrawler", "crawler", "source", "duplicate", "old-a", "", 0); err != nil {
		t.Fatalf("seed crawler seen: %v", err)
	}

	if err := cat.ApplyDuplicateVideoDeletions(ctx, []DuplicateVideoDeletion{
		{VideoID: "old-a", CanonicalVideoID: "final", ExpectedUpdatedAt: videos[0].UpdatedAt.UnixMilli()},
		{VideoID: "old-b", CanonicalVideoID: "final", ExpectedUpdatedAt: videos[1].UpdatedAt.UnixMilli()},
	}); err != nil {
		t.Fatalf("ApplyDuplicateVideoDeletions: %v", err)
	}
	for _, id := range []string{"old-a", "old-b"} {
		if _, err := cat.GetVideo(ctx, id); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("video %s still exists: %v", id, err)
		}
	}
	if _, err := cat.GetVideo(ctx, "final"); err != nil {
		t.Fatalf("final canonical missing: %v", err)
	}
	var historicalCanonical string
	if err := cat.db.QueryRowContext(ctx, `SELECT canonical_video_id FROM deleted_videos WHERE id = 'historical'`).Scan(&historicalCanonical); err != nil {
		t.Fatalf("read historical tombstone: %v", err)
	}
	if historicalCanonical != "final" {
		t.Fatalf("historical canonical = %q, want final", historicalCanonical)
	}
	var seenCanonical string
	if err := cat.db.QueryRowContext(ctx, `SELECT canonical_video_id FROM crawler_seen_sources WHERE source_id = 'source'`).Scan(&seenCanonical); err != nil {
		t.Fatalf("read crawler seen: %v", err)
	}
	if seenCanonical != "final" {
		t.Fatalf("crawler canonical = %q, want final", seenCanonical)
	}
	jobs, err := cat.ListDuplicateAssetCleanupJobs(ctx, 10)
	if err != nil {
		t.Fatalf("ListDuplicateAssetCleanupJobs: %v", err)
	}
	if len(jobs) != 2 || jobs[0].VideoID != "old-a" || jobs[1].VideoID != "old-b" {
		t.Fatalf("jobs = %#v", jobs)
	}
}

func TestApplyDuplicateVideoDeletionsRejectsStalePlanWithoutPartialWrites(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	for _, video := range []*Video{
		{ID: "duplicate", DriveID: "drive", FileID: "a", Size: 100, PublishedAt: now, CreatedAt: now},
		{ID: "canonical", DriveID: "drive", FileID: "b", Size: 200, PublishedAt: now, CreatedAt: now},
	} {
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatalf("seed %s: %v", video.ID, err)
		}
	}
	err = cat.ApplyDuplicateVideoDeletions(ctx, []DuplicateVideoDeletion{{
		VideoID: "duplicate", CanonicalVideoID: "canonical", ExpectedUpdatedAt: 1,
	}})
	if !errors.Is(err, ErrDuplicatePlanStale) {
		t.Fatalf("error = %v, want ErrDuplicatePlanStale", err)
	}
	if _, err := cat.GetVideo(ctx, "duplicate"); err != nil {
		t.Fatalf("duplicate was partially deleted: %v", err)
	}
	if jobs, err := cat.ListDuplicateAssetCleanupJobs(ctx, 10); err != nil || len(jobs) != 0 {
		t.Fatalf("jobs = %#v, err=%v", jobs, err)
	}
}

func TestReplaceDuplicateVideoPublishesNewCanonicalAtomically(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	old := &Video{ID: "old", DriveID: "crawler-a", FileID: "old.mp4", Size: 100, PreviewLocal: "/previews/old.mp4", PublishedAt: now, CreatedAt: now}
	if err := cat.UpsertVideo(ctx, old); err != nil {
		t.Fatalf("seed old: %v", err)
	}
	newVideo := &Video{ID: "new", DriveID: "crawler-a", FileID: "new.mp4", Size: 200, PublishedAt: now, CreatedAt: now.Add(time.Second)}
	if err := cat.ReplaceDuplicateVideo(ctx, DuplicateVideoReplacement{
		NewVideo:                  newVideo,
		ReplacedVideoID:           old.ID,
		ExpectedReplacedUpdatedAt: old.UpdatedAt.UnixMilli(),
		CrawlerSource: &CrawlerSourceSeen{
			Kind: "scriptcrawler", DriveID: "crawler-a", SourceID: "source-new", Status: "imported", Size: 200,
		},
	}); err != nil {
		t.Fatalf("ReplaceDuplicateVideo: %v", err)
	}
	if _, err := cat.GetVideo(ctx, old.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("old video still exists: %v", err)
	}
	if got, err := cat.GetVideo(ctx, newVideo.ID); err != nil || got.Size != 200 {
		t.Fatalf("new canonical = %#v, err=%v", got, err)
	}
	deleted, _, err := cat.ListDeletedVideos(ctx, ListParams{Page: 1, PageSize: 10})
	if err != nil || len(deleted) != 1 || deleted[0].ID != old.ID || deleted[0].CanonicalVideoID != newVideo.ID {
		t.Fatalf("deleted = %#v, err=%v", deleted, err)
	}
	var seenCanonical, status string
	if err := cat.db.QueryRowContext(ctx, `SELECT canonical_video_id, status FROM crawler_seen_sources WHERE source_id = 'source-new'`).Scan(&seenCanonical, &status); err != nil {
		t.Fatalf("read crawler seen: %v", err)
	}
	if seenCanonical != newVideo.ID || status != "imported" {
		t.Fatalf("crawler seen canonical=%q status=%q", seenCanonical, status)
	}
}
