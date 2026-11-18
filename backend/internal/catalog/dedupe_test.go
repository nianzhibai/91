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
		{ID: "old-a", DriveID: "drive", FileID: "a", PreviewLocal: "/previews/a.mp4", Size: 100, Tags: []string{"loser-a", "shared"}, Views: 3, Favorites: 1, Comments: 2, Likes: 4, Dislikes: 1, LastViewedAt: now.Add(time.Minute), LastLikedAt: now.Add(2 * time.Minute), PublishedAt: now, CreatedAt: now},
		{ID: "old-b", DriveID: "drive", FileID: "b", PreviewLocal: "/previews/b.mp4", Size: 100, Tags: []string{"loser-b"}, Views: 5, Favorites: 2, Comments: 1, Likes: 2, Dislikes: 3, LastViewedAt: now.Add(3 * time.Minute), LastLikedAt: now.Add(4 * time.Minute), PublishedAt: now, CreatedAt: now.Add(time.Second)},
		{ID: "final", DriveID: "drive", FileID: "c", Size: 200, Tags: []string{"canonical", "shared"}, Views: 7, Favorites: 3, Comments: 4, Likes: 6, Dislikes: 2, PublishedAt: now, CreatedAt: now.Add(2 * time.Second)},
	}
	for _, video := range videos {
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatalf("seed %s: %v", video.ID, err)
		}
	}
	for i, video := range videos {
		stored, err := cat.GetVideo(ctx, video.ID)
		if err != nil {
			t.Fatalf("reload %s: %v", video.ID, err)
		}
		videos[i] = stored
	}
	if _, err := cat.db.ExecContext(ctx, `
UPDATE video_tags
   SET source = 'auto'
 WHERE video_id = 'final'
   AND tag_id = (SELECT id FROM tags WHERE label = 'shared')`); err != nil {
		t.Fatalf("downgrade canonical shared tag for merge test: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx, `
INSERT INTO merged_videos (video_id,canonical_video_id,merged_at) VALUES ('historical','old-a',1)`); err != nil {
		t.Fatalf("seed historical tombstone: %v", err)
	}
	if err := cat.MarkCrawlerSourceSeen(ctx, "scriptcrawler", "crawler", "source", "duplicate", "old-a", "", 0); err != nil {
		t.Fatalf("seed crawler seen: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx, `
INSERT INTO video_reaction_visits (video_id, visit_id, reaction, created_at, updated_at)
VALUES
  ('old-a', 'visit-old-a-0001', 'like', 1, 1),
  ('old-b', 'visit-old-b-0001', 'dislike', 2, 2),
  ('final', 'visit-final-0001', 'like', 3, 3),
  ('old-a', 'visit-shared-0001', 'dislike', 4, 5),
  ('final', 'visit-shared-0001', 'like', 4, 4)`); err != nil {
		t.Fatalf("seed reactions: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx, `
INSERT INTO video_shares (id, token_hash, video_id, created_at)
VALUES ('share-a', 'token-a', 'old-a', 1), ('share-b', 'token-b', 'old-b', 2)`); err != nil {
		t.Fatalf("seed shares: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx, `
INSERT INTO remote_upload_jobs (id, state, completed_video_id, created_at, updated_at)
VALUES ('upload-a', 'completed', 'old-a', 1, 1)`); err != nil {
		t.Fatalf("seed upload history: %v", err)
	}

	if err := cat.ApplyDuplicateVideoDeletions(ctx, []DuplicateVideoDeletion{
		{VideoID: "old-a", CanonicalVideoID: "final", ExpectedRevision: DuplicateRevision(videos[0]), CanonicalExpectedRevision: DuplicateRevision(videos[2])},
		{VideoID: "old-b", CanonicalVideoID: "final", ExpectedRevision: DuplicateRevision(videos[1]), CanonicalExpectedRevision: DuplicateRevision(videos[2])},
	}); err != nil {
		t.Fatalf("ApplyDuplicateVideoDeletions: %v", err)
	}
	for _, id := range []string{"old-a", "old-b"} {
		if _, err := cat.GetVideo(ctx, id); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("video %s still exists: %v", id, err)
		}
	}
	final, err := cat.GetVideo(ctx, "final")
	if err != nil {
		t.Fatalf("final canonical missing: %v", err)
	}
	if final.Views != 15 || final.Favorites != 6 || final.Comments != 7 || final.Likes != 12 || final.Dislikes != 6 {
		t.Fatalf("merged counters = views:%d favorites:%d comments:%d likes:%d dislikes:%d", final.Views, final.Favorites, final.Comments, final.Likes, final.Dislikes)
	}
	for _, label := range []string{"canonical", "shared", "loser-a", "loser-b"} {
		if !containsString(final.Tags, label) {
			t.Fatalf("merged tags = %#v, missing %q", final.Tags, label)
		}
	}
	var sharedTagSource string
	if err := cat.db.QueryRowContext(ctx, `
SELECT vt.source
  FROM video_tags vt
  JOIN tags t ON t.id = vt.tag_id
 WHERE vt.video_id = 'final' AND t.label = 'shared'`).Scan(&sharedTagSource); err != nil {
		t.Fatalf("read shared tag source: %v", err)
	}
	if sharedTagSource != "manual" {
		t.Fatalf("shared tag source = %q, want stronger manual assignment", sharedTagSource)
	}
	var historicalCanonical string
	if err := cat.db.QueryRowContext(ctx, `SELECT canonical_video_id FROM merged_videos WHERE video_id = 'historical'`).Scan(&historicalCanonical); err != nil {
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
	for _, id := range []string{"old-a", "old-b", "historical"} {
		resolved, err := cat.ResolveVideoID(ctx, id)
		if err != nil || resolved != "final" {
			t.Fatalf("ResolveVideoID(%q) = %q, %v; want final", id, resolved, err)
		}
	}
	var mergedShares, mergedReactions, staleReactions int
	if err := cat.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM video_shares WHERE video_id = 'final'`).Scan(&mergedShares); err != nil {
		t.Fatalf("count merged shares: %v", err)
	}
	if err := cat.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM video_reaction_visits WHERE video_id = 'final'`).Scan(&mergedReactions); err != nil {
		t.Fatalf("count merged reactions: %v", err)
	}
	if err := cat.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM video_reaction_visits WHERE video_id IN ('old-a', 'old-b')`).Scan(&staleReactions); err != nil {
		t.Fatalf("count stale reactions: %v", err)
	}
	if mergedShares != 2 || mergedReactions != 4 || staleReactions != 0 {
		t.Fatalf("merged relations = shares:%d reactions:%d stale_reactions:%d", mergedShares, mergedReactions, staleReactions)
	}
	var sharedReaction string
	if err := cat.db.QueryRowContext(ctx, `SELECT reaction FROM video_reaction_visits WHERE video_id = 'final' AND visit_id = 'visit-shared-0001'`).Scan(&sharedReaction); err != nil {
		t.Fatalf("read merged reaction: %v", err)
	}
	if sharedReaction != "dislike" {
		t.Fatalf("merged reaction = %q, want latest dislike", sharedReaction)
	}
	var completedVideoID string
	if err := cat.db.QueryRowContext(ctx, `SELECT completed_video_id FROM remote_upload_jobs WHERE id = 'upload-a'`).Scan(&completedVideoID); err != nil {
		t.Fatalf("read upload history: %v", err)
	}
	if completedVideoID != "final" {
		t.Fatalf("upload history video = %q, want final", completedVideoID)
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
		VideoID: "duplicate", CanonicalVideoID: "canonical", ExpectedRevision: "stale",
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

func TestApplyDuplicateVideoDeletionsIgnoresEngagementButRejectsMatchingChanges(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	revisions := map[string]string{}
	for _, video := range []*Video{
		{ID: "duplicate", DriveID: "drive", FileID: "a", Title: "same", Size: 100, PublishedAt: now, CreatedAt: now},
		{ID: "canonical", DriveID: "drive", FileID: "b", Title: "same", Size: 200, PublishedAt: now, CreatedAt: now},
		{ID: "edited", DriveID: "drive", FileID: "c", Title: "same", Size: 100, PublishedAt: now, CreatedAt: now},
	} {
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatalf("seed %s: %v", video.ID, err)
		}
		stored, err := cat.GetVideo(ctx, video.ID)
		if err != nil {
			t.Fatalf("reload %s: %v", video.ID, err)
		}
		revisions[video.ID] = DuplicateRevision(stored)
	}
	// Engagement after planning bumps updated_at but not the decision inputs.
	for _, id := range []string{"duplicate", "canonical"} {
		if _, err := cat.IncrementView(ctx, id); err != nil {
			t.Fatalf("view %s: %v", id, err)
		}
	}
	if _, err := cat.db.ExecContext(ctx, `UPDATE videos SET title = 'renamed' WHERE id = 'edited'`); err != nil {
		t.Fatalf("rename: %v", err)
	}

	err = cat.ApplyDuplicateVideoDeletions(ctx, []DuplicateVideoDeletion{{
		VideoID: "edited", CanonicalVideoID: "canonical",
		ExpectedRevision: revisions["edited"], CanonicalExpectedRevision: revisions["canonical"],
	}})
	if !errors.Is(err, ErrDuplicatePlanStale) {
		t.Fatalf("renamed duplicate error = %v, want ErrDuplicatePlanStale", err)
	}
	if err := cat.ApplyDuplicateVideoDeletions(ctx, []DuplicateVideoDeletion{{
		VideoID: "duplicate", CanonicalVideoID: "canonical",
		ExpectedRevision: revisions["duplicate"], CanonicalExpectedRevision: revisions["canonical"],
	}}); err != nil {
		t.Fatalf("viewed rows must still merge: %v", err)
	}
	if got, err := cat.GetVideo(ctx, "canonical"); err != nil || got.Views != 2 {
		t.Fatalf("canonical = %#v, err=%v; want both views merged", got, err)
	}
}

func TestApplyDuplicateVideoDeletionsRejectsRegeneratedAssets(t *testing.T) {
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	regenerate := map[string]func(*Catalog, string) error{
		"thumbnail": func(cat *Catalog, id string) error {
			return cat.UpdateVideoMeta(context.Background(), id, VideoMetaPatch{ThumbnailURL: "/p/thumb/" + id})
		},
		"preview": func(cat *Catalog, id string) error {
			return cat.UpdatePreview(context.Background(), id, id+".mp4", "ready")
		},
	}
	for asset, regen := range regenerate {
		for _, role := range []string{"duplicate", "canonical"} {
			t.Run(asset+"/"+role, func(t *testing.T) {
				ctx := context.Background()
				cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
				t.Cleanup(func() { _ = cat.Close() })
				revisions := map[string]string{}
				for _, id := range []string{"duplicate", "canonical"} {
					if err := cat.UpsertVideo(ctx, &Video{
						ID: id, DriveID: "drive", FileID: id, Size: 100,
						ThumbnailURL: "/p/thumb/" + id, PreviewLocal: id + ".mp4", PreviewStatus: "ready",
						PublishedAt: now, CreatedAt: now,
					}); err != nil {
						t.Fatalf("seed %s: %v", id, err)
					}
					stored, err := cat.GetVideo(ctx, id)
					if err != nil {
						t.Fatalf("reload %s: %v", id, err)
					}
					revisions[id] = DuplicateRevision(stored)
				}
				// Regeneration keeps the path, so only the asset version changes.
				if err := regen(cat, role); err != nil {
					t.Fatalf("regenerate %s: %v", asset, err)
				}
				err = cat.ApplyDuplicateVideoDeletions(ctx, []DuplicateVideoDeletion{{
					VideoID: "duplicate", CanonicalVideoID: "canonical",
					ExpectedRevision: revisions["duplicate"], CanonicalExpectedRevision: revisions["canonical"],
				}})
				if !errors.Is(err, ErrDuplicatePlanStale) {
					t.Fatalf("error = %v, want ErrDuplicatePlanStale", err)
				}
			})
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
