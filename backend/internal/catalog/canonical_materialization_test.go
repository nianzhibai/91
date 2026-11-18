package catalog

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestCanonicalLatestQueryPlanUsesMaterializedIndex(t *testing.T) {
	cat := openCanonicalTestCatalog(t)
	rows, err := cat.db.Query(`EXPLAIN QUERY PLAN
SELECT ` + allVideoCols + `
  FROM videos
 WHERE COALESCE(hidden, 0) = 0
   AND ` + activeDriveWhereSQL + `
   AND ` + uniqueVideoWhereSQL + `
 ORDER BY CASE WHEN COALESCE(thumbnail_url, '') != '' THEN 0 ELSE 1 END,
          published_at DESC,
          id ASC
 LIMIT 24`)
	if err != nil {
		t.Fatalf("explain latest query: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan query plan: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read query plan: %v", err)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "idx_videos_canonical_latest_ready") {
		t.Fatalf("latest query did not use canonical ready index:\n%s", joined)
	}
	if strings.Contains(joined, "USE TEMP B-TREE FOR ORDER BY") {
		t.Fatalf("latest query still uses a temporary sort:\n%s", joined)
	}
}

func TestCanonicalMaterializationPreservesIndependentDedupRepresentatives(t *testing.T) {
	ctx := context.Background()
	cat := openCanonicalTestCatalog(t)
	base := time.UnixMilli(1_800_000_000_000)

	seedCanonicalVideo(t, cat, Video{
		ID: "hash-representative", ContentHash: "shared-hash", SampledSHA256: "sample-a",
		FileName: "a.mp4", Size: 100, CreatedAt: base,
	})
	seedCanonicalVideo(t, cat, Video{
		ID: "sample-representative", ContentHash: "hash-b", SampledSHA256: "shared-sample",
		FileName: "b.mp4", Size: 100, CreatedAt: base.Add(time.Millisecond),
	})
	seedCanonicalVideo(t, cat, Video{
		ID: "filename-representative", ContentHash: "hash-c", SampledSHA256: "sample-c",
		FileName: "shared-name.mp4", Size: 100, CreatedAt: base.Add(2 * time.Millisecond),
	})
	seedCanonicalVideo(t, cat, Video{
		ID: "bridge", ContentHash: "shared-hash", SampledSHA256: "shared-sample",
		FileName: "shared-name.mp4", Size: 100, CreatedAt: base.Add(3 * time.Millisecond),
	})

	assertCanonicalMatchesDynamic(t, cat)
	assertCanonicalFlag(t, cat, "hash-representative", true)
	assertCanonicalFlag(t, cat, "sample-representative", true)
	assertCanonicalFlag(t, cat, "filename-representative", true)
	assertCanonicalFlag(t, cat, "bridge", false)

	result, err := cat.db.ExecContext(ctx, `
INSERT INTO tags (label, match_rules, source, created_at, updated_at)
VALUES ('bridge-tag', '{}', 'user', ?, ?)
`, base.UnixMilli(), base.UnixMilli())
	if err != nil {
		t.Fatalf("insert tag: %v", err)
	}
	tagID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("read tag id: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx, `
INSERT INTO video_tags (video_id, tag_id, source, evidence, created_at)
VALUES ('bridge', ?, 'manual', 'test', ?)
`, tagID, base.UnixMilli()); err != nil {
		t.Fatalf("tag bridge video: %v", err)
	}

	videos, _, err := cat.ListVideos(ctx, ListParams{
		Tag: "bridge-tag", SkipTotal: true, PageSize: 10,
	})
	if err != nil {
		t.Fatalf("list bridge tag: %v", err)
	}
	gotIDs := make([]string, 0, len(videos))
	for _, video := range videos {
		gotIDs = append(gotIDs, video.ID)
	}
	sort.Strings(gotIDs)
	wantIDs := []string{"filename-representative", "hash-representative", "sample-representative"}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("tag representatives = %v, want %v", gotIDs, wantIDs)
	}
	for index := range wantIDs {
		if gotIDs[index] != wantIDs[index] {
			t.Fatalf("tag representatives = %v, want %v", gotIDs, wantIDs)
		}
	}
}

func TestCanonicalMaterializationTracksInsertUpdateAndDelete(t *testing.T) {
	ctx := context.Background()
	cat := openCanonicalTestCatalog(t)
	base := time.UnixMilli(1_800_000_000_000)

	seedCanonicalVideo(t, cat, Video{
		ID: "late", ContentHash: "same", FileName: "late.mp4", Size: 100,
		CreatedAt: base.Add(time.Hour),
	})
	seedCanonicalVideo(t, cat, Video{
		ID: "early", ContentHash: "same", FileName: "early.mp4", Size: 100,
		CreatedAt: base,
	})
	assertCanonicalFlag(t, cat, "early", true)
	assertCanonicalFlag(t, cat, "late", false)

	if _, err := cat.db.ExecContext(ctx, `DELETE FROM videos WHERE id = 'early'`); err != nil {
		t.Fatalf("delete early representative: %v", err)
	}
	assertCanonicalFlag(t, cat, "late", true)

	seedCanonicalVideo(t, cat, Video{
		ID: "other", ContentHash: "other", FileName: "other.mp4", Size: 100,
		CreatedAt: base.Add(2 * time.Hour),
	})
	if _, err := cat.db.ExecContext(ctx,
		`UPDATE videos SET content_hash = 'same' WHERE id = 'other'`); err != nil {
		t.Fatalf("join duplicate group: %v", err)
	}
	assertCanonicalFlag(t, cat, "other", false)
	if _, err := cat.db.ExecContext(ctx,
		`UPDATE videos SET content_hash = 'other' WHERE id = 'other'`); err != nil {
		t.Fatalf("leave duplicate group: %v", err)
	}
	assertCanonicalFlag(t, cat, "other", true)
	assertCanonicalMatchesDynamic(t, cat)
}

func TestCanonicalMaterializationRequiresEligibleRepresentatives(t *testing.T) {
	ctx := context.Background()
	cat := openCanonicalTestCatalog(t)
	base := time.UnixMilli(1_800_000_000_000)

	seedCanonicalVideo(t, cat, Video{
		ID: "hidden-early", ContentHash: "same", FileName: "hidden.mp4", Size: 100,
		Hidden: true, CreatedAt: base,
	})
	seedCanonicalVideo(t, cat, Video{
		ID: "visible-late", ContentHash: "same", FileName: "visible.mp4", Size: 100,
		CreatedAt: base.Add(time.Millisecond),
	})
	assertCanonicalFlag(t, cat, "hidden-early", false)
	assertCanonicalFlag(t, cat, "visible-late", true)

	videos, total, err := cat.ListVideos(ctx, ListParams{PageSize: 10})
	if err != nil {
		t.Fatalf("list visible videos: %v", err)
	}
	if total != 1 || len(videos) != 1 || videos[0].ID != "visible-late" {
		t.Fatalf("visible group = %v/%d, want visible-late", videos, total)
	}
	for _, change := range []struct {
		sql                string
		wantRepresentative string
	}{
		{"UPDATE videos SET hidden = 0 WHERE id = 'hidden-early'", "hidden-early"},
		{"UPDATE videos SET file_id = '' WHERE id = 'hidden-early'", "visible-late"},
		{"UPDATE videos SET file_id = 'restored-file' WHERE id = 'hidden-early'", "hidden-early"},
		{"UPDATE videos SET hidden = 1 WHERE id = 'hidden-early'", "visible-late"},
	} {
		if _, err := cat.db.ExecContext(ctx, change.sql); err != nil {
			t.Fatal(err)
		}
		assertCanonicalFlag(t, cat, "visible-late", change.wantRepresentative == "visible-late")
		assertCanonicalMatchesDynamic(t, cat)
		var representative string
		if err := cat.db.QueryRowContext(ctx, `SELECT representative_id FROM video_dedup_representatives
 WHERE video_id = 'visible-late' AND basis = 'content_hash'`).Scan(&representative); err != nil || representative != change.wantRepresentative {
			t.Fatalf("%s: representative=%q, err=%v", change.sql, representative, err)
		}
	}
}

func TestCanonicalMaterializationBackfillsWhenMigrationMarkerIsMissing(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "catalog.db")
	cat, err := Open(databasePath)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	base := time.UnixMilli(1_800_000_000_000)
	seedCanonicalVideo(t, cat, Video{
		ID: "first", ContentHash: "same", FileName: "first.mp4", Size: 100, CreatedAt: base,
	})
	seedCanonicalVideo(t, cat, Video{
		ID: "second", ContentHash: "same", FileName: "second.mp4", Size: 100,
		CreatedAt: base.Add(time.Millisecond),
	})
	if _, err := cat.db.ExecContext(ctx, `UPDATE videos SET is_canonical = 1`); err != nil {
		t.Fatalf("corrupt materialized flags: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx, `DELETE FROM video_dedup_representatives`); err != nil {
		t.Fatalf("corrupt representative materialization: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx, `DELETE FROM settings WHERE key IN (?, ?)`, canonicalMaterializationMarker, dedupRepresentativesMarker); err != nil {
		t.Fatalf("delete migration markers: %v", err)
	}
	if err := cat.Close(); err != nil {
		t.Fatalf("close catalog: %v", err)
	}

	reopened, err := Open(databasePath)
	if err != nil {
		t.Fatalf("reopen catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Fatalf("close reopened catalog: %v", err)
		}
	})
	assertCanonicalFlag(t, reopened, "first", true)
	assertCanonicalFlag(t, reopened, "second", false)
	assertCanonicalMatchesDynamic(t, reopened)
}

func openCanonicalTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})
	return cat
}

func seedCanonicalVideo(t *testing.T, cat *Catalog, video Video) {
	t.Helper()
	video.DriveID = "drive-1"
	video.FileID = video.ID + "-file"
	video.Title = video.ID
	video.PublishedAt = video.CreatedAt
	if err := cat.UpsertVideo(context.Background(), &video); err != nil {
		t.Fatalf("seed %s: %v", video.ID, err)
	}
}

func assertCanonicalFlag(t *testing.T, cat *Catalog, videoID string, want bool) {
	t.Helper()
	var got int
	if err := cat.db.QueryRow(`SELECT is_canonical FROM videos WHERE id = ?`, videoID).Scan(&got); err != nil {
		t.Fatalf("read canonical flag for %s: %v", videoID, err)
	}
	if (got != 0) != want {
		t.Fatalf("is_canonical(%s) = %d, want %t", videoID, got, want)
	}
}

func assertCanonicalMatchesDynamic(t *testing.T, cat *Catalog) {
	t.Helper()
	var mismatches int
	if err := cat.db.QueryRow(`
SELECT COUNT(*)
  FROM videos
 WHERE is_canonical != CASE WHEN ` + dynamicUniqueVideoWhereSQL + ` THEN 1 ELSE 0 END
`).Scan(&mismatches); err != nil {
		t.Fatalf("compare canonical materialization: %v", err)
	}
	if mismatches != 0 {
		t.Fatalf("canonical materialization has %d mismatch(es)", mismatches)
	}

	if err := cat.db.QueryRow(`
WITH eligible AS (
	SELECT * FROM videos WHERE COALESCE(hidden, 0) = 0 AND file_id != ''
), expected(video_id, basis, representative_id) AS (
	SELECT videos.id, 'self', videos.id
	  FROM videos
	UNION ALL
	SELECT videos.id,
	       'content_hash',
	       (SELECT canonical.id
	          FROM eligible canonical
	         WHERE canonical.content_hash = videos.content_hash
	           AND COALESCE(canonical.content_hash, '') != ''
	         ORDER BY canonical.created_at ASC, canonical.id ASC
	         LIMIT 1)
	  FROM eligible AS videos
	 WHERE COALESCE(videos.content_hash, '') != ''
	UNION ALL
	SELECT videos.id,
	       'sampled_sha256',
	       (SELECT canonical.id
	          FROM eligible canonical
	         WHERE canonical.sampled_sha256 = videos.sampled_sha256
	           AND canonical.size_bytes = videos.size_bytes
	           AND COALESCE(canonical.sampled_sha256, '') != ''
	           AND canonical.size_bytes > 0
	         ORDER BY canonical.created_at ASC, canonical.id ASC
	         LIMIT 1)
	  FROM eligible AS videos
	 WHERE COALESCE(videos.sampled_sha256, '') != ''
	   AND videos.size_bytes > 0
	UNION ALL
	SELECT videos.id,
	       'file_name_size',
	       (SELECT canonical.id
	          FROM eligible canonical
	         WHERE canonical.file_name = videos.file_name
	           AND canonical.size_bytes = videos.size_bytes
	           AND COALESCE(canonical.file_name, '') != ''
	           AND canonical.size_bytes > 0
	         ORDER BY canonical.created_at ASC, canonical.id ASC
	         LIMIT 1)
	  FROM eligible AS videos
	 WHERE COALESCE(videos.file_name, '') != ''
	   AND videos.size_bytes > 0
),
missing AS (
	SELECT video_id, basis, representative_id FROM expected
	EXCEPT
	SELECT video_id, basis, representative_id FROM video_dedup_representatives
),
extra AS (
	SELECT video_id, basis, representative_id FROM video_dedup_representatives
	EXCEPT
	SELECT video_id, basis, representative_id FROM expected
)
SELECT (SELECT COUNT(*) FROM missing) + (SELECT COUNT(*) FROM extra)
`).Scan(&mismatches); err != nil {
		t.Fatalf("compare dedup representatives: %v", err)
	}
	if mismatches != 0 {
		t.Fatalf("dedup representative materialization has %d mismatch(es)", mismatches)
	}
}

func TestCanonicalUpgradeNormalizesMigratedProviderHashes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "catalog.db")
	cat, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seedCanonicalVideo(t, cat, Video{ID: "migrated", Size: 100, CreatedAt: time.Unix(1, 0)})
	if err := cat.MigrateVideoToDrive(ctx, "migrated", VideoDriveMigration{DriveID: "target", FileID: "remote-file", ContentHash: " ABCDEF "}); err != nil {
		t.Fatal(err)
	}
	copy := &Video{ID: "copy", DriveID: "elsewhere", FileID: "other-file", FileName: "renamed.mp4", ContentHash: "ABCDEF", Size: 100}
	check := func(cat *Catalog) {
		t.Helper()
		found, err := cat.FindEquivalentVideo(ctx, copy)
		if err != nil || found.ID != "migrated" || found.ContentHash != "abcdef" {
			t.Fatalf("hash match=%+v err=%v", found, err)
		}
	}
	check(cat)
	// Simulate a v1 catalog where a hidden row shadowed the migrated copy.
	seedCanonicalVideo(t, cat, Video{ID: "hidden", Size: 100, ContentHash: "abcdef", Hidden: true, CreatedAt: time.Unix(0, 0)})
	if _, err := cat.db.Exec(`UPDATE videos SET content_hash = 'ABCDEF' WHERE id = 'migrated';
		UPDATE videos SET is_canonical = 0 WHERE id = 'migrated';
		DELETE FROM video_dedup_representatives;
		DELETE FROM settings WHERE key IN (?, ?)`, canonicalMaterializationMarker, dedupRepresentativesMarker); err != nil {
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	check(upgraded)
	assertCanonicalFlag(t, upgraded, "migrated", true)
	var representative string
	if err := upgraded.db.QueryRow("SELECT representative_id FROM video_dedup_representatives WHERE video_id = 'migrated' AND basis = 'content_hash'").Scan(&representative); err != nil || representative != "migrated" {
		t.Fatalf("upgrade representative=%q err=%v", representative, err)
	}
}
