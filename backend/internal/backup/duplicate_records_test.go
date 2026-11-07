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
		ID: "kept-" + id, DriveID: "cloud", FileID: "kept-" + id, FileName: id + ".mp4", Size: 123,
		ParentID: "cloud-parent", DirName: "Originals", AncestorDirIDs: []string{"0", "cloud-parent"},
	}); err != nil {
		t.Fatal(err)
	}
	for range occurrences {
		inserted, err := cat.InsertScannedVideo(ctx, &catalog.Video{ID: id, DriveID: "local-upload", FileID: id, FileName: id + ".mp4", Size: 123}, nil)
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
	if err := db.QueryRow(`SELECT COUNT(*) FROM deleted_videos WHERE id='legacy-duplicate' AND reason='duplicate'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("old archive tombstone lost: count=%d err=%v", count, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM crawler_seen_sources WHERE source_id='legacy-source' AND status='duplicate'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("old archive crawler state lost: count=%d err=%v", count, err)
	}
}
