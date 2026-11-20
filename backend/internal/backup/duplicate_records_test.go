package backup

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/nianzhibai/91/backend/internal/catalog"
)

func seedBackupDuplicateRecord(t *testing.T, cat *catalog.Catalog, id string, occurrences int) {
	t.Helper()
	ctx := context.Background()
	if err := cat.UpsertDrive(ctx, &catalog.Drive{ID: "cloud", Kind: "quark", Name: "Cloud", RootID: "0"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID: "kept-" + id, DriveID: "cloud", FileID: "kept-" + id, FileName: id + ".mp4", Size: 123, ContentHash: id,
		ParentID: "cloud-parent", DirName: "Originals", AncestorDirIDs: []string{"0", "cloud-parent"},
	}); err != nil {
		t.Fatal(err)
	}
	for range occurrences {
		inserted, err := cat.InsertScannedVideo(ctx, &catalog.Video{ID: id, DriveID: "local-upload", FileID: id, FileName: id + ".mp4", Size: 123, ContentHash: id}, nil)
		if err != nil || inserted {
			t.Fatalf("seed duplicate record: inserted=%v err=%v", inserted, err)
		}
	}
}

func TestDuplicateHistoryBackupRespectsResourceSelection(t *testing.T) {
	for _, all := range []bool{false, true} {
		name := "cloud_only"
		selection := BackupSelection{CloudDrives: true}
		want := 0
		if all {
			name, selection, want = "all_resources", FullBackupSelection(), 1
		}
		t.Run(name, func(t *testing.T) {
			env := newTestBackupEnv(t)
			seedBackupDuplicateRecord(t, env.cat, "source", 1)
			path := filepath.Join(t.TempDir(), "snapshot.db")
			if err := env.cat.BackupTo(context.Background(), path); err != nil {
				t.Fatal(err)
			}
			if _, err := filterSnapshotDatabase(context.Background(), path, selection); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM duplicate_records`).Scan(&count); err != nil || count != want {
				t.Fatalf("archive history=%d err=%v, want %d", count, err, want)
			}
			if err := validateArchiveDatabaseScope(context.Background(), path, Manifest{Selection: &selection}); err != nil {
				t.Fatal(err)
			}
			if !all {
				// A partial archive must reject hidden cross-resource history even
				// when all its remaining live videos belong to the selected drive.
				if _, err := db.Exec(`ATTACH DATABASE ? AS original`, env.cfg.Storage.DBPath); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO duplicate_records SELECT * FROM original.duplicate_records`); err != nil {
					t.Fatal(err)
				}
				if err := validateArchiveDatabaseScope(context.Background(), path, Manifest{Selection: &selection}); err == nil {
					t.Fatal("partial archive accepted cross-resource duplicate history")
				}
			}
		})
	}
}

func TestRestoreMergesDuplicateHistoryWithoutRecounting(t *testing.T) {
	dir := t.TempDir()
	sourcePath, targetPath := filepath.Join(dir, "source.db"), filepath.Join(dir, "target.db")
	for _, item := range []struct {
		path string
		id   string
		n    int
	}{{sourcePath, "source-only", 3}, {targetPath, "target-only", 1}} {
		cat, err := catalog.Open(item.path)
		if err != nil {
			t.Fatal(err)
		}
		seedBackupDuplicateRecord(t, cat, "shared", item.n)
		seedBackupDuplicateRecord(t, cat, item.id, 1)
		if err := cat.Close(); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	for range 2 {
		if err := mergeSelectiveRestoreDatabase(ctx, targetPath, sourcePath, FullBackupSelection()); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", targetPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count, occurrences int
	if err := db.QueryRow(`SELECT COUNT(*) FROM duplicate_records`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("merged history count=%d err=%v, want 3", count, err)
	}
	if err := db.QueryRow(`SELECT occurrences FROM duplicate_records WHERE video_id='shared'`).Scan(&occurrences); err != nil || occurrences != 3 {
		t.Fatalf("restoring counted observations again: count=%d err=%v", occurrences, err)
	}
	var parent, name, ancestors string
	if err := db.QueryRow(`SELECT json_extract(canonical_snapshot, '$.parentId'),
json_extract(canonical_snapshot, '$.dirName'), json_extract(canonical_snapshot, '$.ancestorDirIds')
FROM duplicate_records WHERE video_id='source-only'`).Scan(&parent, &name, &ancestors); err != nil {
		t.Fatal(err)
	}
	if parent != "cloud-parent" || name != "Originals" || ancestors != `["0","cloud-parent"]` {
		t.Fatalf("restore lost historical location: parent=%s name=%s ancestors=%s", parent, name, ancestors)
	}
	// Archives made before this feature must still restore, preserving target
	// history rather than requiring or inventing records in the old archive.
	source, err := sql.Open("sqlite", sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Exec(`DROP TABLE duplicate_records;
INSERT INTO deleted_videos (id,drive_id,reason,canonical_video_id,deleted_at)
VALUES ('legacy-duplicate','cloud','duplicate','kept-shared',1);
INSERT INTO crawler_seen_sources (kind,drive_id,source_id,status,canonical_video_id,first_seen_at,last_seen_at)
VALUES ('scriptcrawler','crawler','legacy-source','duplicate','kept-shared',1,2)`); err != nil {
		source.Close()
		t.Fatal(err)
	}
	source.Close()
	if err := mergeSelectiveRestoreDatabase(ctx, targetPath, sourcePath, FullBackupSelection()); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM duplicate_records`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("old archive changed target history: count=%d err=%v", count, err)
	}
	reopened, err := catalog.Open(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM duplicate_records`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("restart backfilled old archive history: count=%d err=%v", count, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM merged_videos WHERE video_id='legacy-duplicate' AND canonical_video_id='kept-shared'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("old archive tombstone lost: count=%d err=%v", count, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM crawler_seen_sources WHERE source_id='legacy-source' AND status='duplicate'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("old archive crawler state lost: count=%d err=%v", count, err)
	}
}

func TestRestoreDropsCleanupJobsOfRevivedVideos(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sourcePath, targetPath := filepath.Join(dir, "source.db"), filepath.Join(dir, "target.db")
	for _, path := range []string{sourcePath, targetPath} {
		cat, err := catalog.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := cat.UpsertDrive(ctx, &catalog.Drive{ID: "cloud", Kind: "quark", Name: "Cloud", RootID: "0"}); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"kept", "retired"} {
			if err := cat.UpsertVideo(ctx, &catalog.Video{ID: id, DriveID: "cloud", FileID: id, FileName: id + ".mp4", Size: 123}); err != nil {
				t.Fatal(err)
			}
		}
		// Only the target retires the video after the archive was made.
		if path == targetPath {
			if err := cat.ApplyDuplicateVideoDeletions(ctx, []catalog.DuplicateVideoDeletion{{VideoID: "retired", CanonicalVideoID: "kept"}}); err != nil {
				t.Fatal(err)
			}
			if jobs, err := cat.ListDuplicateAssetCleanupJobs(ctx, 10); err != nil || len(jobs) != 1 {
				t.Fatalf("target cleanup jobs = %+v, %v", jobs, err)
			}
		}
		if err := cat.Close(); err != nil {
			t.Fatal(err)
		}
	}

	if err := mergeSelectiveRestoreDatabase(ctx, targetPath, sourcePath, FullBackupSelection()); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", targetPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var live, jobs int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM videos WHERE id = 'retired'),
		(SELECT COUNT(*) FROM duplicate_asset_cleanup_jobs)`).Scan(&live, &jobs); err != nil {
		t.Fatal(err)
	}
	if live != 1 || jobs != 0 {
		t.Fatalf("after restore: retired live=%d, cleanup jobs=%d; want 1 and 0", live, jobs)
	}
}

// Older versions stored provider hashes as reported. A selective restore merges
// archived rows into a catalog that already normalized its own hashes once.
func TestRestoreNormalizesArchivedProviderHashes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "archive.db")
	cat, err := catalog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.UpsertVideo(ctx, &catalog.Video{ID: "video", DriveID: "cloud", FileID: "file", FileName: "video.mp4", Size: 123}); err != nil {
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE videos SET content_hash = ' ABCDEF ' WHERE id = 'video'`); err != nil {
		t.Fatal(err)
	}
	if _, err := rewriteRestoredDatabase(ctx, path, catalogRewrite{
		stagePreviewDir:   filepath.Join(dir, "stage-previews"),
		sourcePreviewRoot: filepath.Join(dir, "source-previews"),
		targetPreviewRoot: filepath.Join(dir, "target-previews"),
	}); err != nil {
		t.Fatal(err)
	}
	var hash string
	if err := db.QueryRow(`SELECT content_hash FROM videos WHERE id = 'video'`).Scan(&hash); err != nil || hash != "abcdef" {
		t.Fatalf("restored hash = %q, err=%v; want abcdef", hash, err)
	}
}

func TestRestoreRequeuesArchivedFingerprintsOfEarlierScheme(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "archive.db")
	cat, err := catalog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []*catalog.Video{
		{ID: "legacy", DriveID: "cloud", FileID: "a", FileName: "a.mp4", Size: 123, FingerprintStatus: "ready"},
		{ID: "current", DriveID: "cloud", FileID: "b", FileName: "b.mp4", Size: 456, SampledSHA256: "s2:abc", FingerprintStatus: "ready"},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The archive was written before the current sampling scheme.
	if _, err := db.Exec(`UPDATE videos SET sampled_sha256 = 'abc', fingerprint_status = 'ready' WHERE id = 'legacy'`); err != nil {
		t.Fatal(err)
	}
	if _, err := rewriteRestoredDatabase(ctx, path, catalogRewrite{
		stagePreviewDir:   filepath.Join(dir, "stage-previews"),
		sourcePreviewRoot: filepath.Join(dir, "source-previews"),
		targetPreviewRoot: filepath.Join(dir, "target-previews"),
	}); err != nil {
		t.Fatal(err)
	}
	// The earlier fingerprint stays as evidence until the worker replaces it.
	for id, want := range map[string][2]string{"legacy": {"abc", "pending"}, "current": {"s2:abc", "ready"}} {
		var sampled, status string
		if err := db.QueryRow(`SELECT sampled_sha256, fingerprint_status FROM videos WHERE id = ?`, id).Scan(&sampled, &status); err != nil || [2]string{sampled, status} != want {
			t.Fatalf("restored %s fingerprint = %q/%q, err=%v; want %q", id, sampled, status, err, want)
		}
	}
}
