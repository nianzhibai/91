package backup

import (
	"archive/zip"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/video-site/backend/internal/catalog"
)

type backupTagDefinition struct {
	Label, MatchRules, Source, Origin string
	CreatedAt, UpdatedAt              int64
	Count                             int
}

func readBackupTagDefinitions(t *testing.T, path string) map[string]backupTagDefinition {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`
SELECT t.label, t.match_rules, t.source, t.origin, t.created_at, t.updated_at, COUNT(vt.video_id)
  FROM tags t
  LEFT JOIN video_tags vt ON vt.tag_id = t.id
 GROUP BY t.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	definitions := make(map[string]backupTagDefinition)
	for rows.Next() {
		var tag backupTagDefinition
		if err := rows.Scan(&tag.Label, &tag.MatchRules, &tag.Source, &tag.Origin, &tag.CreatedAt, &tag.UpdatedAt, &tag.Count); err != nil {
			t.Fatal(err)
		}
		definitions[tag.Label] = tag
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return definitions
}

func TestBackupAndRestorePreserveUnreferencedTags(t *testing.T) {
	for _, test := range []struct {
		name           string
		selection      BackupSelection
		seedLocalVideo bool
	}{
		{name: "full", selection: FullBackupSelection()},
		{name: "cloud only", selection: BackupSelection{CloudDrives: true}},
		{name: "empty local storage", selection: BackupSelection{LocalStorage: true}},
		{name: "local storage with videos", selection: BackupSelection{LocalStorage: true}, seedLocalVideo: true},
		{name: "user information only", selection: BackupSelection{UserInfo: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newTestBackupEnv(t)
			ctx := context.Background()
			db, err := sql.Open("sqlite", env.cfg.Storage.DBPath)
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.Exec(`
INSERT INTO tags (label, match_rules, source, origin, created_at, updated_at) VALUES
  ('empty-custom', '{"keywords":["future-match"]}', 'user', '', 12345, 23456),
  ('empty-crawler', '{}', 'generated', 'crawler', 12346, 23457),
  ('TG', '{}', 'generated', 'telegram', 12347, 23458)`)
			if closeErr := db.Close(); err != nil || closeErr != nil {
				t.Fatalf("seed empty tags: insert=%v close=%v", err, closeErr)
			}
			if test.seedLocalVideo {
				localRoot := filepath.Join(env.root, "local-source")
				writeTestFile(t, filepath.Join(localRoot, "retained.mp4"), []byte("video"))
				if err := env.cat.UpsertDrive(ctx, &catalog.Drive{
					ID: "local-source", Kind: "localstorage", Name: "Local Source", RootID: "/",
					Credentials: map[string]string{"path": localRoot},
				}); err != nil {
					t.Fatal(err)
				}
				now := time.Now()
				if err := env.cat.UpsertVideo(ctx, &catalog.Video{
					ID: "local-video", DriveID: "local-source", FileID: "retained.mp4", FileName: "retained.mp4",
					Title: "Local video", Size: 5, PublishedAt: now, CreatedAt: now,
				}); err != nil {
					t.Fatal(err)
				}
			}
			original := readBackupTagDefinitions(t, env.cfg.Storage.DBPath)
			record := createAndWaitForBackup(t, env.manager, test.selection)
			archivePath, _, err := env.manager.resolveBackup(record.ID)
			if err != nil {
				t.Fatal(err)
			}
			reader, err := zip.OpenReader(archivePath)
			if err != nil {
				t.Fatal(err)
			}
			archiveDatabase := filepath.Join(t.TempDir(), "archive.sqlite")
			for _, file := range reader.File {
				if file.Name == "payload/database.sqlite" {
					writeTestFile(t, archiveDatabase, readZipFile(t, file))
					break
				}
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			archived := readBackupTagDefinitions(t, archiveDatabase)
			if len(archived) != len(original) {
				t.Fatalf("archive tags = %#v, want %#v", archived, original)
			}
			for label, expected := range original {
				if archived[label] != expected || expected.Count != 0 {
					t.Fatalf("archived tag %s = %#v, want unreferenced definition %#v", label, archived[label], expected)
				}
			}
			// Remove source definitions from the live target so restoration must
			// recover them from the archive rather than retain existing rows.
			tags, err := env.cat.ListTags(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, tag := range tags {
				if _, err := env.cat.DeleteTag(ctx, tag.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := env.cat.EnsureTag(ctx, "target-only", "user"); err != nil {
				t.Fatal(err)
			}
			original["target-only"] = readBackupTagDefinitions(t, env.cfg.Storage.DBPath)["target-only"]
			if _, err := env.manager.PrepareRestore(ctx, record.ID); err != nil {
				t.Fatal(err)
			}
			env.manager.Close()
			if err := env.cat.Close(); err != nil {
				t.Fatal(err)
			}
			applied, err := ApplyPendingRestore(env.root)
			if err != nil || applied == nil {
				t.Fatalf("apply pending restore: applied=%v err=%v", applied, err)
			}
			restored, err := catalog.Open(env.cfg.Storage.DBPath)
			if err != nil {
				_ = RollbackAppliedRestore(applied, err)
				t.Fatal(err)
			}
			defer restored.Close()
			if err := CommitAppliedRestore(applied); err != nil {
				t.Fatal(err)
			}
			actual := readBackupTagDefinitions(t, env.cfg.Storage.DBPath)
			if len(actual) != len(original) {
				t.Fatalf("restored tags = %#v, want %#v", actual, original)
			}
			for label, expected := range original {
				if actual[label] != expected {
					t.Fatalf("restored tag %s = %#v, want %#v", label, actual[label], expected)
				}
			}
		})
	}
}

func TestArchiveScopeAcceptsEmptyTagsButRejectsDanglingAssignments(t *testing.T) {
	env := newTestBackupEnv(t)
	ctx := context.Background()
	tag, err := env.cat.EnsureTag(ctx, "empty-tag", "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.cat.UpsertDrive(ctx, &catalog.Drive{ID: "cloud", Kind: "quark", Name: "Cloud", RootID: "0"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := env.cat.UpsertVideo(ctx, &catalog.Video{
		ID: "cloud-video", DriveID: "cloud", FileID: "retained.mp4", Title: "Video",
		Size: 5, PublishedAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err := env.cat.BackupTo(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	selection := BackupSelection{CloudDrives: true}
	if _, err := filterSnapshotDatabase(ctx, snapshot, selection); err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{Selection: &selection}
	if err := validateArchiveDatabaseScope(ctx, snapshot, manifest); err != nil {
		t.Fatalf("archive with unreferenced tag was rejected: %v", err)
	}
	db, err := sql.Open("sqlite", snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, assignment := range []struct {
		name, videoID string
		tagID         int64
	}{
		{name: "missing video", videoID: "missing-video", tagID: tag.ID},
		{name: "missing tag", videoID: "cloud-video", tagID: tag.ID + 1000},
	} {
		t.Run(assignment.name, func(t *testing.T) {
			if _, err := db.Exec(`DELETE FROM video_tags`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO video_tags(video_id, tag_id, source, created_at) VALUES (?, ?, 'auto', 1)`, assignment.videoID, assignment.tagID); err != nil {
				t.Fatal(err)
			}
			if err := validateArchiveDatabaseScope(ctx, snapshot, manifest); err == nil || !strings.Contains(err.Error(), "video tag assignments") {
				t.Fatalf("archive with dangling tag assignment: %v", err)
			}
		})
	}
}
