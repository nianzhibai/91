package catalog

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// reopenAfterSchemeChange reopens a catalog as if an earlier sampling scheme
// had written it.
func reopenAfterSchemeChange(t *testing.T, cat *Catalog, path string) *Catalog {
	t.Helper()
	if err := cat.DeleteSettings(context.Background(), sampledFingerprintSchemeKey); err != nil {
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	cat, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	return cat
}

func openSchemeTestCatalog(t *testing.T, videos ...*Video) (*Catalog, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.db")
	cat, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, v := range videos {
		v.Title, v.PublishedAt, v.CreatedAt = v.ID, now, now.Add(time.Duration(i)*time.Millisecond)
		if err := cat.UpsertVideo(context.Background(), v); err != nil {
			t.Fatal(err)
		}
	}
	return cat, path
}

func deletionSourceCount(t *testing.T, cat *Catalog, id string) int {
	t.Helper()
	preview, err := cat.PreviewVideoDeletion(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return len(preview.Sources)
}

func TestUpgradeKeepsLegacyFingerprintsUntilReplaced(t *testing.T) {
	ctx := context.Background()
	cat, path := openSchemeTestCatalog(t,
		// Two copies grouped by an equal old-scheme fingerprint.
		&Video{ID: "legacy-a", DriveID: "one", FileID: "a", FileName: "a.mp4", Size: 10, SampledSHA256: "abc", FingerprintStatus: "ready"},
		&Video{ID: "legacy-b", DriveID: "two", FileID: "b", FileName: "b.mp4", Size: 10, SampledSHA256: "abc", FingerprintStatus: "ready"},
		&Video{ID: "current", DriveID: "one", FileID: "c", FileName: "c.mp4", Size: 20, SampledSHA256: "s2:def", FingerprintStatus: "ready"},
	)
	payload := `{"version":1,"video":{"id":"gone","sampledSha256":"abc","fingerprintStatus":"ready"}}`
	if _, err := cat.db.ExecContext(ctx, `
INSERT INTO deleted_videos (id, drive_id, file_id, size_bytes, sampled_sha256, restore_payload, deleted_at)
VALUES ('gone', 'one', 'g', 10, 'abc', ?, 1);
INSERT INTO merged_videos (video_id, canonical_video_id, drive_id, file_id, size_bytes, sampled_sha256, merged_at)
VALUES ('merged', 'current', 'two', 'm', 20, 'abc', 1);
INSERT INTO crawler_seen_sources (kind, drive_id, source_id, sampled_sha256, size_bytes, first_seen_at, last_seen_at)
VALUES ('scriptcrawler', 'crawler', 'src', 'abc', 10, 1, 1);`, payload); err != nil {
		t.Fatal(err)
	}
	cat = reopenAfterSchemeChange(t, cat, path)

	for id, want := range map[string][2]string{
		"legacy-a": {"abc", "pending"}, "legacy-b": {"abc", "pending"}, "current": {"s2:def", "ready"},
	} {
		got, err := cat.GetVideo(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if [2]string{got.SampledSHA256, got.FingerprintStatus} != want {
			t.Fatalf("%s fingerprint = %q/%q, want %q", id, got.SampledSHA256, got.FingerprintStatus, want)
		}
	}
	queued, err := cat.ListVideosNeedingFingerprint(ctx, "one", 0)
	if err != nil || len(queued) != 1 || queued[0].ID != "legacy-a" {
		t.Fatalf("queued fingerprints = %v err=%v, want legacy-a", queued, err)
	}
	// Equal old fingerprints may stand for different files, so they group
	// nothing until both videos are fingerprinted again.
	if canonical := canonicalVideoIDs(t, cat); !canonical["legacy-a"] || !canonical["legacy-b"] {
		t.Fatalf("canonical rows = %v, want both videos shown despite equal old fingerprints", canonical)
	}

	var deleted, restorePayload, merged, crawler string
	if err := cat.db.QueryRowContext(ctx, `SELECT sampled_sha256, restore_payload FROM deleted_videos WHERE id = 'gone'`).Scan(&deleted, &restorePayload); err != nil {
		t.Fatal(err)
	}
	if err := cat.db.QueryRowContext(ctx, `SELECT sampled_sha256 FROM merged_videos WHERE video_id = 'merged'`).Scan(&merged); err != nil {
		t.Fatal(err)
	}
	if err := cat.db.QueryRowContext(ctx, `SELECT sampled_sha256 FROM crawler_seen_sources WHERE source_id = 'src'`).Scan(&crawler); err != nil {
		t.Fatal(err)
	}
	if deleted != "" || merged != "" || crawler != "" {
		t.Fatalf("stored fingerprints = %q/%q/%q, want all cleared", deleted, merged, crawler)
	}
	var restored struct {
		Video Video `json:"video"`
	}
	if err := json.Unmarshal([]byte(restorePayload), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Video.SampledSHA256 != "" || restored.Video.FingerprintStatus != "pending" {
		t.Fatalf("restore payload fingerprint = %q/%q, want empty and pending", restored.Video.SampledSHA256, restored.Video.FingerprintStatus)
	}

	// Records copied out of a video keep only current fingerprints.
	if err := cat.DeleteVideoGroup(ctx, "legacy-a", VideoDeletionOptions{}, time.Time{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	var copied int
	if err := cat.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deleted_videos WHERE sampled_sha256 = 'abc'`).Scan(&copied); err != nil || copied != 0 {
		t.Fatalf("blacklist entries with an old fingerprint = %d err=%v", copied, err)
	}
}

// Files with the same name and size that the old scheme proved different stay
// apart while the library is fingerprinted again.
func TestUpgradeKeepsProvenDifferentFilesApart(t *testing.T) {
	ctx := context.Background()
	cat, path := openSchemeTestCatalog(t,
		&Video{ID: "first", DriveID: "one", FileID: "a", FileName: "same.mp4", Size: 40, SampledSHA256: "old-a", FingerprintStatus: "ready"},
		&Video{ID: "second", DriveID: "two", FileID: "b", FileName: "same.mp4", Size: 40, SampledSHA256: "old-b", FingerprintStatus: "ready"},
	)
	if got := deletionSourceCount(t, cat, "first"); got != 1 {
		t.Fatalf("setup: deletion sources = %d, want 1", got)
	}
	cat = reopenAfterSchemeChange(t, cat, path)

	check := func(stage string) {
		t.Helper()
		if got := deletionSourceCount(t, cat, "first"); got != 1 {
			t.Fatalf("%s: deletion sources = %d, want 1", stage, got)
		}
		if canonical := canonicalVideoIDs(t, cat); !canonical["first"] || !canonical["second"] {
			t.Fatalf("%s: canonical rows = %v, want both files visible", stage, canonical)
		}
	}
	check("after upgrade")

	first, err := cat.GetVideo(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	// A failed attempt keeps the old fingerprint and can be retried.
	if _, err := cat.UpdateVideoFingerprintForSource(ctx, first, "", "failed", "stalled"); err != nil {
		t.Fatal(err)
	}
	if first, err = cat.GetVideo(ctx, "first"); err != nil || first.SampledSHA256 != "old-a" || first.FingerprintStatus != "failed" {
		t.Fatalf("failed fingerprint = %+v err=%v, want the old value kept", first, err)
	}
	check("after a failed attempt")
	if counts, err := cat.ResetFailedGeneration(ctx, "one", GenerationKinds{Fingerprints: true}); err != nil || counts.Fingerprints != 1 {
		t.Fatalf("retry counts = %+v err=%v", counts, err)
	}

	if _, err := cat.UpdateVideoFingerprintForSource(ctx, first, "s2:new-a", "ready", ""); err != nil {
		t.Fatal(err)
	}
	check("with one current fingerprint")

	second, err := cat.GetVideo(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.UpdateVideoFingerprintForSource(ctx, second, "s2:new-b", "ready", ""); err != nil {
		t.Fatal(err)
	}
	check("with both current fingerprints")
}

// A video awaiting content verification is checked against an old-scheme
// fingerprint, so it counts as replaced and loses everything derived from the
// previous bytes, including the provider hash.
func TestUpgradeTreatsUnverifiedContentAsReplaced(t *testing.T) {
	ctx := context.Background()
	cat, path := openSchemeTestCatalog(t,
		&Video{ID: "verifying", DriveID: "one", FileID: "d", FileName: "d.mp4", Size: 30, ContentHash: "stale",
			DurationSeconds: 60, ThumbnailURL: "/p/thumb/verifying", PreviewStatus: "ready", PreviewLocal: "verifying.mp4"},
		&Video{ID: "hash-copy", DriveID: "one", FileID: "e", FileName: "e.mp4", Size: 50, ContentHash: "stale"},
	)
	if _, err := cat.db.ExecContext(ctx,
		`UPDATE videos SET previous_sampled_sha256 = 'old', sampled_sha256 = '', fingerprint_status = 'pending' WHERE id = 'verifying'`); err != nil {
		t.Fatal(err)
	}
	cat = reopenAfterSchemeChange(t, cat, path)

	verifying, err := cat.GetVideo(ctx, "verifying")
	if err != nil {
		t.Fatal(err)
	}
	replaced, err := cat.UpdateVideoFingerprintForSource(ctx, verifying, "s2:new", "ready", "")
	if err != nil || !replaced {
		t.Fatalf("replaced=%t err=%v, want replaced", replaced, err)
	}
	if verifying, err = cat.GetVideo(ctx, "verifying"); err != nil {
		t.Fatal(err)
	}
	if verifying.ContentHash != "" || verifying.ThumbnailURL != "" || verifying.PreviewStatus != "pending" || verifying.DurationSeconds != 0 {
		t.Fatalf("replaced source kept derived state: %+v", verifying)
	}
	if canonical := canonicalVideoIDs(t, cat); !canonical["verifying"] || !canonical["hash-copy"] {
		t.Fatalf("canonical rows = %v, want the stale hash to no longer match", canonical)
	}
	if got := deletionSourceCount(t, cat, "verifying"); got != 1 {
		t.Fatalf("deletion sources = %d, want 1", got)
	}
}

func canonicalVideoIDs(t *testing.T, cat *Catalog) map[string]bool {
	t.Helper()
	rows, err := cat.db.Query(`SELECT id FROM videos WHERE is_canonical = 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out[id] = true
	}
	return out
}
