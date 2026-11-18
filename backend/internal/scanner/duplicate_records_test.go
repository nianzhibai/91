package scanner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/dedupe"
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

func TestRescanKeepsOnlyCurrentContentEvidence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		size     int64
		hash     string
		wantHash string
		changed  bool
	}{
		{"same size and new hash", 100, "UPDATED", "updated", true},
		{"new size without a hash", 101, "", "", true},
		{"hash omitted for unchanged file", 100, "", "old", false},
		{"hash differs only in case", 100, "OLD", "old", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "catalog.db")
			cat, err := catalog.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer cat.Close()
			before := &catalog.Video{ID: "existing", DriveID: "drive", FileID: "file", FileName: "file.mp4", Size: 100,
				ContentHash: "old", SampledSHA256: "sample", DurationSeconds: 180,
				ThumbnailURL: "/p/thumb/existing", PreviewStatus: "ready", PreviewLocal: "existing.mp4"}
			if err := cat.UpsertVideo(ctx, before); err != nil {
				t.Fatal(err)
			}
			if err := cat.UpsertVideo(ctx, &catalog.Video{ID: "copy", DriveID: "other", FileID: "other-file",
				FileName: "different.mp4", Size: 100, SampledSHA256: "sample"}); err != nil {
				t.Fatal(err)
			}

			drive := &scannerFakeDrive{entries: []drives.Entry{{ID: "file", Name: "file.mp4", Size: tc.size, Hash: tc.hash}}}
			result, err := New(cat, drive, []string{".mp4"}, nil, nil).Scan(ctx, "root")
			if err != nil || len(result.Issues) != 0 {
				t.Fatalf("scan issues=%v err=%v", result.Issues, err)
			}
			got, err := cat.GetVideo(ctx, "existing")
			if err != nil {
				t.Fatal(err)
			}
			if got.ContentHash != tc.wantHash || got.Size != tc.size {
				t.Fatalf("observed size/hash = %d/%q", got.Size, got.ContentHash)
			}
			if tc.changed {
				if result.Duplicates != 0 || got.SampledSHA256 != "" || got.FingerprintStatus != "pending" ||
					got.DurationSeconds != 0 || got.ThumbnailURL != "" || got.PreviewLocal != "" || got.PreviewStatus != "pending" {
					t.Fatalf("stale evidence after replacement: duplicates=%d row=%+v", result.Duplicates, got)
				}
				if err := cat.UpdateVideoFingerprintForSource(ctx, before, "late", "ready", ""); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("late fingerprint from previous file: %v", err)
				}
				if err := cat.UpdatePreviewForSource(ctx, before, "late.mp4", "ready"); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("late preview from previous file: %v", err)
				}
				if _, err := cat.IncrementThumbnailFailuresForSource(ctx, before); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("late thumbnail failure from previous file: %v", err)
				}
				return
			}
			if got.SampledSHA256 != "sample" || got.PreviewStatus != "ready" || result.Duplicates != 1 {
				t.Fatalf("unchanged file: duplicates=%d row=%+v", result.Duplicates, got)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var reason string
			if err := db.QueryRow("SELECT reason FROM duplicate_records").Scan(&reason); err != nil || reason != "sampled_sha256" {
				t.Fatalf("recorded reason=%q err=%v", reason, err)
			}
		})
	}
}

func TestRescanKeepsTagsOnMatcherFailureAndClearsThemOnNoMatch(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "catalog.db")
	cat, err := catalog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if _, err := cat.EnsureTag(ctx, "example", "user"); err != nil {
		t.Fatal(err)
	}
	v := &catalog.Video{ID: "existing", DriveID: "drive", FileID: "file", FileName: "example.mp4", Title: "example", Size: 100, ContentHash: "hash"}
	if err := cat.UpsertVideo(ctx, v); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpsertVideo(ctx, &catalog.Video{ID: "other", DriveID: "other", FileID: "copy", Size: 100, ContentHash: "hash"}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Make just the rule-version read fail. The catalog can still persist tags.
	if _, err := db.Exec("ALTER TABLE settings RENAME TO unavailable_settings"); err != nil {
		t.Fatal(err)
	}
	drive := &scannerFakeDrive{entries: []drives.Entry{{ID: "file", Name: "renamed.mp4", Size: 100, Hash: "hash"}}}
	scan := New(cat, drive, []string{".mp4"}, nil, nil)
	first, err := scan.Scan(ctx, "root")
	if err != nil || len(first.Issues) != 1 || first.Issues[0].Stage != IssueTags || first.Duplicates != 1 {
		t.Fatalf("scan issues=%v duplicates=%d err=%v", first.Issues, first.Duplicates, err)
	}
	got, err := cat.GetVideo(ctx, v.ID)
	if err != nil || !slices.Equal(got.Tags, []string{"example"}) {
		t.Fatalf("tags lost on lookup failure: row=%+v err=%v", got, err)
	}
	if _, err := db.Exec("ALTER TABLE unavailable_settings RENAME TO settings"); err != nil {
		t.Fatal(err)
	}
	second, err := scan.Scan(ctx, "root")
	if err != nil || len(second.Issues) != 0 || second.Duplicates != 1 {
		t.Fatalf("scan issues=%v duplicates=%d err=%v", second.Issues, second.Duplicates, err)
	}
	got, err = cat.GetVideo(ctx, v.ID)
	if err != nil || len(got.Tags) != 0 {
		t.Fatalf("tags after successful empty match: row=%+v err=%v", got, err)
	}
}

func TestRescanReplacesHashBeforeAnotherFileCanMatchIt(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	drive := &scannerFakeDrive{entries: []drives.Entry{
		{ID: "a", Name: "first.mp4", Size: 100, Hash: "first"},
		{ID: "b", Name: "second.mp4", Size: 101},
	}}
	scan := New(cat, drive, []string{".mp4"}, nil, nil)
	if result, err := scan.Scan(ctx, "root"); err != nil || result.Stats.Added != 2 {
		t.Fatalf("initial scan: %+v, %v", result, err)
	}
	drive.entries[0].Hash = "replacement"
	drive.entries[1].Hash = "first"
	drive.entries[1].Size = 100
	if result, err := scan.Scan(ctx, "root"); err != nil || len(result.Issues) != 0 || result.Duplicates != 0 {
		t.Fatalf("rescan: %+v, %v", result, err)
	}
	videos, err := cat.ListVideoMaintenanceCandidates(ctx)
	if err != nil || len(videos) != 2 {
		t.Fatalf("maintenance candidates: %+v, %v", videos, err)
	}
	candidates := make([]dedupe.Candidate, 0, len(videos))
	for _, v := range videos {
		candidates = append(candidates, dedupe.Candidate{ID: v.ID, Size: v.Size, ContentHash: v.ContentHash, SampledSHA256: v.SampledSHA256})
	}
	plan, err := dedupe.Build(ctx, candidates, dedupe.Options{Channels: dedupe.ChannelExact})
	if err != nil || len(plan.Actions) != 0 {
		t.Fatalf("maintenance would merge distinct files: %+v, %v", plan.Actions, err)
	}
}

func TestRescanRefreshesAllRowsSharingAProviderFile(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	for _, id := range []string{"migrated", "scanned"} {
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID: id, DriveID: "drive", FileID: "file", Size: 100, ContentHash: "old", SampledSHA256: "sample",
		}); err != nil {
			t.Fatal(err)
		}
	}
	drive := &scannerFakeDrive{entries: []drives.Entry{{ID: "file", Name: "new.mp4", Size: 100, Hash: "new"}}}
	result, err := New(cat, drive, []string{".mp4"}, nil, nil).Scan(ctx, "root")
	if err != nil || len(result.Issues) != 0 || result.Updated != 2 {
		t.Fatalf("shared source rescan: %+v, %v", result, err)
	}
	rows, err := cat.ListVideosByDriveFileID(ctx, "drive", "file")
	if err != nil || len(rows) != 2 {
		t.Fatalf("shared source rows: %+v, %v", rows, err)
	}
	for _, row := range rows {
		if row.ContentHash != "new" || row.SampledSHA256 != "" {
			t.Fatalf("row retained stale identity: %+v", row)
		}
	}
}
