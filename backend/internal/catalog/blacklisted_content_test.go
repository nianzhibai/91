package catalog

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func seedVideos(t *testing.T, cat *Catalog, videos ...*Video) {
	t.Helper()
	for _, v := range videos {
		if err := cat.UpsertVideo(context.Background(), v); err != nil {
			t.Fatal(err)
		}
	}
}

func blacklist(t *testing.T, cat *Catalog, id string) {
	t.Helper()
	if err := cat.DeleteVideoGroup(context.Background(), id, VideoDeletionOptions{}, time.Time{}, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func admit(t *testing.T, cat *Catalog, v *Video) bool {
	t.Helper()
	added, err := cat.InsertScannedVideo(context.Background(), v, nil)
	if err != nil {
		t.Fatal(err)
	}
	return added
}

func TestBlacklistedContentStaysOutFromEveryIngress(t *testing.T) {
	ctx := context.Background()
	cat := openCanonicalTestCatalog(t)
	seedVideos(t, cat, &Video{ID: "A", DriveID: "one", FileID: "a", ContentHash: "h", SampledSHA256: "s2:s", FileName: "a.mp4", Size: 10})
	blacklist(t, cat, "A")

	if admit(t, cat, &Video{ID: "hash-copy", DriveID: "two", FileID: "x", ContentHash: "H", FileName: "x.mp4", Size: 10}) {
		t.Fatal("same provider hash on another drive was admitted")
	}

	// A drive with another hash algorithm is recognized once the fingerprint is ready.
	copy := &Video{ID: "other-algorithm", DriveID: "three", FileID: "y", ContentHash: "gcid", FileName: "renamed.mp4", Size: 10}
	if !admit(t, cat, copy) {
		t.Fatal("unrecognized copy was not admitted")
	}
	if replaced, err := cat.UpdateVideoFingerprintForSource(ctx, copy, "S2:S", "ready", ""); err != nil || replaced {
		t.Fatalf("fingerprint: replaced=%t err=%v", replaced, err)
	}
	if _, err := cat.GetVideo(ctx, copy.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("fingerprinted copy remains visible: %v", err)
	}
	var reason string
	if err := cat.db.QueryRow(`SELECT reason FROM deleted_videos WHERE id = ?`, copy.ID).Scan(&reason); err != nil || reason != DeletedVideoReasonBlacklistedContent {
		t.Fatalf("copy entry reason=%q err=%v", reason, err)
	}

	crawled := &Video{ID: "crawl", DriveID: "crawler", SampledSHA256: "s2:s", Size: 10}
	seen := CrawlerSourceSeen{Kind: "scriptcrawler", DriveID: "crawler", SourceID: "src", DiscoveryKey: "key"}
	if skipped, err := cat.SkipBlacklistedCrawlerSource(ctx, crawled, seen); err != nil || !skipped {
		t.Fatalf("crawler skip=%t err=%v", skipped, err)
	}
	ids, err := cat.ListCrawlerSourceIDs(ctx, "scriptcrawler", "crawler")
	if err != nil || len(ids) != 1 || ids[0] != "src" {
		t.Fatalf("known crawler sources=%v err=%v", ids, err)
	}
	other := &Video{ID: "fresh", DriveID: "crawler", SampledSHA256: "s2:t", Size: 10}
	if skipped, err := cat.SkipBlacklistedCrawlerSource(ctx, other, CrawlerSourceSeen{Kind: "scriptcrawler", DriveID: "crawler", SourceID: "other"}); err != nil || skipped {
		t.Fatalf("unrelated crawler source skip=%t err=%v", skipped, err)
	}
}

func TestFingerprintKeepsContentRepresentedByVisibleVideo(t *testing.T) {
	ctx := context.Background()
	cat := openCanonicalTestCatalog(t)
	seedVideos(t, cat, &Video{ID: "gone", DriveID: "one", FileID: "g", SampledSHA256: "s2:s", FileName: "g.mp4", Size: 10})
	blacklist(t, cat, "gone")
	// A visible copy that predates content blocking represents the content.
	seedVideos(t, cat, &Video{ID: "kept", DriveID: "two", FileID: "k", SampledSHA256: "s2:s", FileName: "k.mp4", Size: 10})
	late := &Video{ID: "late", DriveID: "three", FileID: "l", FileName: "l.mp4", Size: 10}
	seedVideos(t, cat, late)
	if _, err := cat.UpdateVideoFingerprintForSource(ctx, late, "s", "ready", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.GetVideo(ctx, "late"); err != nil {
		t.Fatalf("duplicate of a visible video was blacklisted: %v", err)
	}
}

func TestRestoreLiftsContentBlock(t *testing.T) {
	ctx := context.Background()
	cat := openCanonicalTestCatalog(t)
	seedVideos(t, cat,
		&Video{ID: "A", DriveID: "one", FileID: "a", ContentHash: "h", FileName: "a.mp4", Size: 10},
		&Video{ID: "B", DriveID: "two", FileID: "b", ContentHash: "h", FileName: "b.mp4", Size: 10},
	)
	blacklist(t, cat, "A")
	if res, err := cat.RestoreDeletedVideo(ctx, "A", nil); err != nil || res.RestorePolicy != DeletedVideoRestorePolicyScan {
		t.Fatalf("restore=%+v err=%v", res, err)
	}
	if !admit(t, cat, &Video{ID: "A", DriveID: "one", FileID: "a", ContentHash: "h", FileName: "a.mp4", Size: 10}) {
		t.Fatal("restored source was blocked by its group's content")
	}
	if blocked, err := cat.IsDeletedVideoCandidate(ctx, "B", "two", "b", "h", "b.mp4", 10); err != nil || !blocked {
		t.Fatalf("blacklisted sibling source released: %t %v", blocked, err)
	}
	if admit(t, cat, &Video{ID: "C", DriveID: "three", FileID: "c", ContentHash: "h", FileName: "c.mp4", Size: 10}) {
		t.Fatal("new copy of restored content was admitted beside it")
	}
	if blocked, err := cat.IsDeletedVideoCandidate(ctx, "C", "three", "c", "h", "c.mp4", 10); err != nil || blocked {
		t.Fatalf("new copy is still treated as blacklisted: %t %v", blocked, err)
	}

	// Blacklisting the content again blocks it everywhere.
	blacklist(t, cat, "A")
	if blocked, err := cat.IsDeletedVideoCandidate(ctx, "D", "four", "d", "h", "d.mp4", 10); err != nil || !blocked {
		t.Fatalf("re-blacklisted content admitted: %t %v", blocked, err)
	}
}

func TestFileNameMatchRequiresAgreeingSignals(t *testing.T) {
	ctx := context.Background()
	cat := openCanonicalTestCatalog(t)
	base := time.UnixMilli(1_800_000_000_000)
	seedVideos(t, cat,
		&Video{ID: "A", DriveID: "one", FileID: "a", ContentHash: "x", SampledSHA256: "s2:s1", FileName: "1.mp4", Size: 10, CreatedAt: base},
		&Video{ID: "X", DriveID: "two", FileID: "x", SampledSHA256: "s2:s2", FileName: "1.mp4", Size: 10, CreatedAt: base.Add(time.Millisecond)},
		&Video{ID: "Y", DriveID: "one", FileID: "y", ContentHash: "y", SampledSHA256: "s2:s3", FileName: "1.mp4", Size: 10, CreatedAt: base.Add(2 * time.Millisecond)},
		&Video{ID: "D", DriveID: "two", FileID: "d", FileName: "1.mp4", Size: 10, CreatedAt: base.Add(3 * time.Millisecond)},
	)
	assertCanonicalMatchesDynamic(t, cat)
	// Different fingerprints, or different hashes on one drive, are different videos.
	assertCanonicalFlag(t, cat, "A", true)
	assertCanonicalFlag(t, cat, "X", true)
	assertCanonicalFlag(t, cat, "Y", true)
	assertCanonicalFlag(t, cat, "D", false)

	// A name and size copy is admitted and stays hidden until its fingerprint decides.
	if !admit(t, cat, &Video{ID: "N", DriveID: "three", FileID: "n", FileName: "1.mp4", Size: 10, CreatedAt: base.Add(4 * time.Millisecond)}) {
		t.Fatal("name and size copy was skipped before its fingerprint could decide")
	}
	assertCanonicalFlag(t, cat, "N", false)

	blacklist(t, cat, "A")
	for _, id := range []string{"X", "Y"} {
		if _, err := cat.GetVideo(ctx, id); err != nil {
			t.Fatalf("%s was blacklisted with A: %v", id, err)
		}
	}
	if _, err := cat.GetVideo(ctx, "D"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("D shared A's card and should be blacklisted with it: %v", err)
	}
}

func TestScanAdmitsSameNameWithDifferentHashOnOneDrive(t *testing.T) {
	cat := openCanonicalTestCatalog(t)
	base := time.UnixMilli(1_800_000_000_000)
	seedVideos(t, cat, &Video{ID: "A", DriveID: "one", FileID: "a", ContentHash: "x", FileName: "1.mp4", Size: 10, CreatedAt: base})
	if !admit(t, cat, &Video{ID: "Z", DriveID: "one", FileID: "z", ContentHash: "z", FileName: "1.mp4", Size: 10, CreatedAt: base.Add(time.Millisecond)}) {
		t.Fatal("same-drive file with a different provider hash was skipped")
	}
	// Without comparable signals the match stays provisional: the copy is
	// admitted but hidden until its fingerprint decides.
	if !admit(t, cat, &Video{ID: "W", DriveID: "two", FileID: "w", FileName: "1.mp4", Size: 10, CreatedAt: base.Add(2 * time.Millisecond)}) {
		t.Fatal("copy on another drive was skipped before its fingerprint could decide")
	}
	assertCanonicalFlag(t, cat, "W", false)
}

func TestDeletionIncludesCopiesAwaitingUsableSize(t *testing.T) {
	ctx := context.Background()
	for _, physical := range []bool{false, true} {
		cat := openCanonicalTestCatalog(t)
		seedVideos(t, cat,
			&Video{ID: "A", DriveID: "one", FileID: "a", ContentHash: "h", FileName: "a.mp4", Size: 10},
			&Video{ID: "C", DriveID: "two", FileID: "c", ContentHash: "h", FileName: "c.mp4", Size: 10},
		)
		if err := cat.MarkInvalidSizeSources(ctx, "two", []string{"c"}); err != nil {
			t.Fatal(err)
		}
		if !physical {
			blacklist(t, cat, "A")
			if deleted, err := cat.IsVideoDeleted(ctx, "C"); err != nil || !deleted {
				t.Fatalf("held copy escaped the blacklist: %t %v", deleted, err)
			}
			continue
		}
		preview, err := cat.PreviewVideoDeletion(ctx, "A")
		if err != nil || len(preview.Sources) != 2 {
			t.Fatalf("preview=%+v err=%v", preview, err)
		}
		all := true
		var removed []string
		err = cat.DeleteVideoGroup(ctx, "A", VideoDeletionOptions{DeleteSource: true, IncludeDuplicates: &all, GroupRevision: preview.GroupRevision}, time.Time{},
			func(_ context.Context, v *Video, _ bool) error { removed = append(removed, v.ID); return nil }, nil)
		if err != nil || len(removed) != 2 {
			t.Fatalf("removed=%v err=%v", removed, err)
		}
	}
}

func TestKeepingDuplicatesHandsMergedCopiesOn(t *testing.T) {
	ctx := context.Background()
	keep := false
	remove := func(context.Context, *Video, bool) error { return nil }
	deleteSelected := func(t *testing.T, cat *Catalog) {
		t.Helper()
		preview, err := cat.PreviewVideoDeletion(ctx, "A")
		if err != nil {
			t.Fatal(err)
		}
		options := VideoDeletionOptions{DeleteSource: true, IncludeDuplicates: &keep, GroupRevision: preview.GroupRevision}
		if err := cat.DeleteVideoGroup(ctx, "A", options, time.Time{}, remove, nil); err != nil {
			t.Fatal(err)
		}
	}
	merged := &Video{ID: "B", DriveID: "two", FileID: "b", ContentHash: "h", FileName: "b.mp4", Size: 10}

	t.Run("successor", func(t *testing.T) {
		cat := openCanonicalTestCatalog(t)
		base := time.UnixMilli(1_800_000_000_000)
		seedVideos(t, cat,
			&Video{ID: "A", DriveID: "one", FileID: "a", ContentHash: "h", FileName: "a.mp4", Size: 10, CreatedAt: base},
			&Video{ID: "B", DriveID: "two", FileID: "b", ContentHash: "h", FileName: "b.mp4", Size: 10, CreatedAt: base.Add(time.Millisecond)},
			&Video{ID: "C", DriveID: "three", FileID: "c", ContentHash: "h", FileName: "c.mp4", Size: 10, CreatedAt: base.Add(2 * time.Millisecond)},
		)
		if err := cat.ApplyDuplicateVideoDeletions(ctx, []DuplicateVideoDeletion{{VideoID: "B", CanonicalVideoID: "A"}}); err != nil {
			t.Fatal(err)
		}
		deleteSelected(t, cat)
		if got, err := cat.ResolveVideoID(ctx, "B"); err != nil || got != "C" {
			t.Fatalf("B resolves to %q, err=%v", got, err)
		}
		copy := *merged
		if admit(t, cat, &copy) {
			t.Fatalf("merged copy was admitted as %s beside its new canonical", copy.ID)
		}
	})

	t.Run("released", func(t *testing.T) {
		cat := openCanonicalTestCatalog(t)
		seedVideos(t, cat, &Video{ID: "A", DriveID: "one", FileID: "a", ContentHash: "h", FileName: "a.mp4", Size: 10})
		copy := *merged
		seedVideos(t, cat, &copy)
		if err := cat.ApplyDuplicateVideoDeletions(ctx, []DuplicateVideoDeletion{{VideoID: "B", CanonicalVideoID: "A"}}); err != nil {
			t.Fatal(err)
		}
		deleteSelected(t, cat)
		copy = *merged
		if !admit(t, cat, &copy) || copy.ID != "B" {
			t.Fatalf("kept copy admitted=%s, want its original ID", copy.ID)
		}
	})
}

func TestBlacklistFingerprintsMigrateFromRestoreSnapshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
CREATE TABLE deleted_videos (
	id              TEXT PRIMARY KEY,
	drive_id        TEXT NOT NULL DEFAULT '',
	file_id         TEXT NOT NULL DEFAULT '',
	content_hash    TEXT NOT NULL DEFAULT '',
	file_name       TEXT NOT NULL DEFAULT '',
	size_bytes      INTEGER NOT NULL DEFAULT 0,
	restore_payload TEXT NOT NULL DEFAULT '',
	deleted_at      INTEGER NOT NULL
);
INSERT INTO deleted_videos (id, drive_id, file_id, content_hash, file_name, size_bytes, restore_payload, deleted_at)
VALUES ('old', 'one', 'f', 'ABC', 'old.mp4', 10, '{"version":1,"video":{"id":"old","sampledSha256":"S2:SAMPLE"}}', 1);`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	cat, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	ctx := context.Background()
	if skipped, err := cat.SkipBlacklistedCrawlerSource(ctx, &Video{SampledSHA256: "s2:sample", Size: 10},
		CrawlerSourceSeen{Kind: "scriptcrawler", DriveID: "crawler", SourceID: "src"}); err != nil || !skipped {
		t.Fatalf("migrated fingerprint skip=%t err=%v", skipped, err)
	}
	if blocked, err := cat.IsDeletedVideoCandidate(ctx, "new", "two", "g", "abc", "new.mp4", 10); err != nil || !blocked {
		t.Fatalf("migrated hash blocked=%t err=%v", blocked, err)
	}
}

func TestBlacklistIndexesReplaceDriveHashIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	// The second open runs every migration against an existing database.
	for open := 1; open <= 2; open++ {
		cat, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		indexes := map[string]bool{}
		rows, err := cat.db.Query(`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'deleted_videos'`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			indexes[name] = true
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		_ = cat.Close()
		if indexes["idx_deleted_videos_drive_hash"] || !indexes["idx_deleted_videos_content_hash"] || !indexes["idx_deleted_videos_sampled"] {
			t.Fatalf("open %d: deleted_videos indexes = %v", open, indexes)
		}
	}
}
