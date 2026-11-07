package scanner

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
)

func TestScanRecordsDuplicatesForNewAndExistingSources(t *testing.T) {
	for _, existing := range []bool{false, true} {
		outcome := "skipped_import"
		if existing {
			outcome = "matched_existing"
		}
		t.Run(outcome, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "catalog.db")
			cat, err := catalog.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer cat.Close()
			if err := cat.UpsertVideo(ctx, &catalog.Video{
				ID: "kept", DriveID: "other", FileID: "kept", FileName: "same.mp4", Title: "Kept", Size: 123,
				ParentID: "other-parent", DirName: "Originals", AncestorDirIDs: []string{"other-root", "other-parent"},
			}); err != nil {
				t.Fatal(err)
			}
			if existing {
				if err := cat.UpsertVideo(ctx, &catalog.Video{
					ID: "fake-drive-file", DriveID: "drive", FileID: "file", FileName: "same.mp4", Title: "same", Size: 123,
					ParentID: "old-parent", DirName: "Old", AncestorDirIDs: []string{"root", "old-parent"},
				}); err != nil {
					t.Fatal(err)
				}
			}
			drive := &scannerTreeFakeDrive{entries: map[string][]drives.Entry{
				"root":         {{ID: "category", Name: "Category", IsDir: true}},
				"category":     {{ID: "/library/a|b", Name: "Clips", IsDir: true}},
				"/library/a|b": {{ID: "file", Name: "same.mp4", Size: 123, ParentID: "stale-provider-parent"}},
			}}
			scanner := New(cat, drive, []string{".mp4"}, nil, nil)
			for range 2 {
				result, err := scanner.Scan(ctx, "root")
				if err != nil || len(result.Issues) != 0 || result.Duplicates != 1 || result.Stats.Added != 0 {
					t.Fatalf("scan result=%+v err=%v", result, err)
				}
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var reason, gotOutcome, fileID string
			var count int
			if err := db.QueryRow(`SELECT reason,outcome,file_id,occurrences FROM duplicate_records`).Scan(&reason, &gotOutcome, &fileID, &count); err != nil {
				t.Fatal(err)
			}
			if reason != "file_name_size" || gotOutcome != outcome || fileID != "file" || count != 2 {
				t.Fatalf("record = %s/%s/%s occurrences=%d", reason, gotOutcome, fileID, count)
			}
			var key, sourceJSON string
			if err := db.QueryRow(`SELECT record_key,source_snapshot FROM duplicate_records`).Scan(&key, &sourceJSON); err != nil {
				t.Fatal(err)
			}
			var source catalog.Video
			if err := json.Unmarshal([]byte(sourceJSON), &source); err != nil {
				t.Fatal(err)
			}
			if source.ParentID != "/library/a|b" || source.DirName != "Clips" || !slices.Equal(source.AncestorDirIDs, []string{"root", "category", "/library/a|b"}) {
				t.Fatalf("source directory must come from traversal: %+v", source)
			}
			for _, column := range []string{"canonical_snapshot", "matched_snapshot", "selected_snapshot"} {
				var encoded string
				if err := db.QueryRow(`SELECT ` + column + ` FROM duplicate_records`).Scan(&encoded); err != nil {
					t.Fatal(err)
				}
				var snapshot catalog.Video
				if err := json.Unmarshal([]byte(encoded), &snapshot); err != nil {
					t.Fatal(err)
				}
				if snapshot.ParentID != "other-parent" || snapshot.DirName != "Originals" || !slices.Equal(snapshot.AncestorDirIDs, []string{"other-root", "other-parent"}) {
					t.Fatalf("%s lost the comparison location: %+v", column, snapshot)
				}
			}

			drive.entries = map[string][]drives.Entry{
				"root":  {{ID: "moved", Name: "Moved", IsDir: true}},
				"moved": {{ID: "file", Name: "same.mp4", Size: 123}},
			}
			for range 2 {
				result, err := scanner.Scan(ctx, "root")
				if err != nil || len(result.Issues) != 0 || result.Duplicates != 1 || result.Stats.Added != 0 {
					t.Fatalf("scan after move=%+v err=%v", result, err)
				}
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM duplicate_records`).Scan(&count); err != nil || count != 2 {
				t.Fatalf("move must create one new observation: count=%d err=%v", count, err)
			}
			var preserved string
			if err := db.QueryRow(`SELECT source_snapshot,occurrences FROM duplicate_records WHERE record_key=?`, key).Scan(&preserved, &count); err != nil || preserved != sourceJSON || count != 2 {
				t.Fatalf("move changed the historical observation: occurrences=%d err=%v", count, err)
			}
			var movedJSON string
			if err := db.QueryRow(`SELECT source_snapshot,occurrences FROM duplicate_records WHERE record_key!=?`, key).Scan(&movedJSON, &count); err != nil || count != 2 {
				t.Fatalf("moved observations did not coalesce: occurrences=%d err=%v", count, err)
			}
			if err := json.Unmarshal([]byte(movedJSON), &source); err != nil {
				t.Fatal(err)
			}
			if source.ParentID != "moved" || source.DirName != "Moved" || !slices.Equal(source.AncestorDirIDs, []string{"root", "moved"}) {
				t.Fatalf("moved observation has stale directory metadata: %+v", source)
			}
		})
	}
}
