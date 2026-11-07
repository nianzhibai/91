package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/dedupe"
)

func duplicateRecordTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	return cat
}

func TestScanDuplicateRecordsPreserveBasisAndCoalesce(t *testing.T) {
	for _, tc := range []struct {
		name, sourceHash, canonicalHash, reason string
	}{
		{"hash takes precedence", " SAME ", "same", dedupe.ReasonContentHash},
		{"different provider hashes", "provider-a", "provider-b", dedupe.ReasonFileNameSize},
		{"missing hashes", "", "", dedupe.ReasonFileNameSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat := duplicateRecordTestCatalog(t)
			ctx := context.Background()
			canonical := &Video{ID: "kept", DriveID: "drive-a", FileID: "kept", FileName: "same.mp4", Title: "Kept", Size: 123456789, ContentHash: tc.canonicalHash}
			if err := cat.UpsertVideo(ctx, canonical); err != nil {
				t.Fatal(err)
			}
			source := &Video{ID: "skipped", DriveID: "drive-b", FileID: "skipped", FileName: "same.mp4", Title: "Skipped", Size: canonical.Size, ContentHash: tc.sourceHash}
			for i := range 3 {
				source.CreatedAt = time.Unix(int64(i+1), 0)
				source.UpdatedAt = source.CreatedAt
				if _, err := cat.IncrementView(ctx, canonical.ID); err != nil {
					t.Fatal(err)
				}
				if inserted, err := cat.InsertScannedVideo(ctx, source, nil); err != nil || inserted {
					t.Fatalf("inserted=%v err=%v", inserted, err)
				}
			}
			var reason, origin, outcome, sourceJSON, canonicalJSON, evidenceJSON string
			var occurrences, count int
			var first, last int64
			if err := cat.db.QueryRow(`SELECT reason,origin,outcome,source_snapshot,canonical_snapshot,evidence,occurrences,first_seen_at,last_seen_at FROM duplicate_records`).Scan(
				&reason, &origin, &outcome, &sourceJSON, &canonicalJSON, &evidenceJSON, &occurrences, &first, &last); err != nil {
				t.Fatal(err)
			}
			if err := cat.db.QueryRow(`SELECT COUNT(*) FROM duplicate_records`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if reason != tc.reason || origin != DuplicateOriginScan || outcome != DuplicateOutcomeSkipped || occurrences != 3 || count != 1 || first <= 0 || last < first {
				t.Fatalf("record = %s/%s/%s occurrences=%d count=%d times=%d,%d", reason, origin, outcome, occurrences, count, first, last)
			}
			if tc.name == "missing hashes" {
				// Removing migration-only wrappers must not change existing decision keys.
				const expectedKey = "eee2cd7f4122f622d40770774b0d996e10d2019f14e05a52611d8af83a3e8422"
				var key string
				if err := cat.db.QueryRow(`SELECT record_key FROM duplicate_records`).Scan(&key); err != nil || key != expectedKey {
					t.Fatalf("decision key changed: key=%s err=%v", key, err)
				}
			}
			var snapshot duplicateSnapshot
			if err := json.Unmarshal([]byte(sourceJSON), &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.ContentHash != normalizeContentHash(tc.sourceHash) || snapshot.SizeBytes != source.Size || snapshot.FileName != source.FileName {
				t.Fatalf("source observation was not preserved: %+v", snapshot)
			}
			var evidence dedupe.Evidence
			if err := json.Unmarshal([]byte(evidenceJSON), &evidence); err != nil {
				t.Fatal(err)
			}
			if evidence.Version != dedupe.EvidenceVersion || evidence.MatchedVideoID != canonical.ID || evidence.SelectedVideoID != canonical.ID {
				t.Fatalf("evidence = %+v", evidence)
			}
			if _, err := cat.GetVideo(ctx, source.ID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("skipped video unexpectedly admitted: %v", err)
			}
			if deleted, err := cat.IsVideoDeleted(ctx, source.ID); err != nil || deleted {
				t.Fatalf("audit record created a tombstone: %v, %v", deleted, err)
			}
			// Removing the survivor must allow future admission while keeping the
			// original decision's snapshot available for internal investigation.
			if err := cat.DeleteVideo(ctx, canonical.ID); err != nil {
				t.Fatal(err)
			}
			if inserted, err := cat.InsertScannedVideo(ctx, source, nil); err != nil || !inserted {
				t.Fatalf("audit record blocked later admission: %v, %v", inserted, err)
			}
			var preserved string
			if err := cat.db.QueryRow(`SELECT canonical_snapshot FROM duplicate_records`).Scan(&preserved); err != nil || preserved != canonicalJSON {
				t.Fatalf("canonical snapshot lost after deletion: %v", err)
			}
		})
	}
}

func TestScanDuplicateAuditFailureDoesNotReportSuccess(t *testing.T) {
	cat := duplicateRecordTestCatalog(t)
	ctx := context.Background()
	canonical := &Video{ID: "kept", DriveID: "a", FileID: "kept", FileName: "same.mp4", Size: 100}
	if err := cat.UpsertVideo(ctx, canonical); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.db.Exec(`CREATE TRIGGER fail_duplicate_record BEFORE INSERT ON duplicate_records BEGIN SELECT RAISE(ABORT, 'audit failed'); END`); err != nil {
		t.Fatal(err)
	}
	source := &Video{ID: "skipped", DriveID: "b", FileID: "skipped", FileName: "same.mp4", Size: 100}
	if inserted, err := cat.InsertScannedVideo(ctx, source, nil); err == nil || inserted {
		t.Fatalf("audit failure hidden: inserted=%v err=%v", inserted, err)
	}
	if _, err := cat.db.Exec(`DROP TRIGGER fail_duplicate_record`); err != nil {
		t.Fatal(err)
	}
	if inserted, err := cat.InsertScannedVideo(ctx, source, nil); err != nil || inserted {
		t.Fatalf("retry after audit failure: inserted=%v err=%v", inserted, err)
	}
	assertDuplicateRecordCount(t, cat, 1)
}

func TestDuplicateRecordsCommitWithTransitiveMerges(t *testing.T) {
	for _, failure := range []string{"", "audit", "retirement"} {
		t.Run("failure="+failure, func(t *testing.T) {
			cat := duplicateRecordTestCatalog(t)
			ctx := context.Background()
			var candidates []dedupe.Candidate
			for i, id := range []string{"a", "b", "c"} {
				video := &Video{
					ID: id, DriveID: "drive", FileID: id, FileName: id + ".mp4", Title: "同一个测试视频标题", DurationSeconds: 300, Size: int64(100 * (i + 1)),
					ParentID: id + "-parent", DirName: id + "-directory", AncestorDirIDs: []string{"root", id + "-parent"},
				}
				if err := cat.UpsertVideo(ctx, video); err != nil {
					t.Fatal(err)
				}
				candidates = append(candidates, dedupe.Candidate{ID: id, Title: video.Title, Size: video.Size, DurationSeconds: 300, ThumbnailPath: id})
			}
			plan, err := dedupe.Build(ctx, candidates, dedupe.Options{
				Channels: dedupe.ChannelNear,
				CompareImages: func(a, b string) (float64, error) {
					if a == "a" && b == "c" {
						return 0.10, nil
					}
					return 0.96, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if failure == "audit" {
				_, err = cat.db.Exec(`CREATE TRIGGER reject_second_audit BEFORE INSERT ON duplicate_records WHEN NEW.video_id = 'b' BEGIN SELECT RAISE(ABORT, 'audit failed'); END`)
			} else if failure == "retirement" {
				_, err = cat.db.Exec(`CREATE TRIGGER reject_second_retirement BEFORE DELETE ON videos WHEN OLD.id = 'b' BEGIN SELECT RAISE(ABORT, 'retirement failed'); END`)
			}
			if err != nil {
				t.Fatal(err)
			}
			var deletions []DuplicateVideoDeletion
			for _, action := range plan.Actions {
				deletions = append(deletions, DuplicateVideoDeletion{VideoID: action.VideoID, CanonicalVideoID: action.CanonicalVideoID, Evidence: action.Evidence})
			}
			err = cat.ApplyDuplicateVideoDeletions(ctx, deletions)
			if failure != "" {
				if err == nil {
					t.Fatal("expected transaction failure")
				}
				assertDuplicateRecordCount(t, cat, 0)
				for _, id := range []string{"a", "b", "c"} {
					if _, err := cat.GetVideo(ctx, id); err != nil {
						t.Fatalf("partially committed merge for %s: %v", id, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertDuplicateRecordCount(t, cat, 2)
			var matched, canonical, matchedJSON, evidenceJSON string
			if err := cat.db.QueryRow(`SELECT matched_video_id,canonical_video_id,matched_snapshot,evidence FROM duplicate_records WHERE video_id='a'`).Scan(&matched, &canonical, &matchedJSON, &evidenceJSON); err != nil {
				t.Fatal(err)
			}
			var snapshot duplicateSnapshot
			if err := json.Unmarshal([]byte(matchedJSON), &snapshot); err != nil {
				t.Fatal(err)
			}
			var evidence dedupe.Evidence
			if err := json.Unmarshal([]byte(evidenceJSON), &evidence); err != nil {
				t.Fatal(err)
			}
			if matched != "b" || canonical != "c" || snapshot.FileName != "b.mp4" || evidence.Match == nil || evidence.Match.LeftID != "a" || evidence.Match.RightID != "b" || evidence.Match.Score != 0.96 {
				t.Fatalf("transitive evidence attributed to wrong video: matched=%s canonical=%s snapshot=%+v evidence=%+v", matched, canonical, snapshot, evidence)
			}
			if _, err := cat.GetVideo(ctx, "b"); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("intermediate row remains: %v", err)
			}
			for column, id := range map[string]string{"source_snapshot": "a", "canonical_snapshot": "c", "matched_snapshot": "b", "selected_snapshot": "c"} {
				var encoded string
				if err := cat.db.QueryRow(`SELECT ` + column + ` FROM duplicate_records WHERE video_id='a'`).Scan(&encoded); err != nil {
					t.Fatal(err)
				}
				var snapshot duplicateSnapshot
				if err := json.Unmarshal([]byte(encoded), &snapshot); err != nil {
					t.Fatal(err)
				}
				if snapshot.ParentID != id+"-parent" || snapshot.DirName != id+"-directory" || !slices.Equal(snapshot.AncestorDirIDs, []string{"root", id + "-parent"}) {
					t.Fatalf("%s lost its location after merge: %+v", column, snapshot)
				}
			}
		})
	}
}

func TestCrawlerDuplicateRecordCommitsWithSeenState(t *testing.T) {
	cat := duplicateRecordTestCatalog(t)
	ctx := context.Background()
	canonical := &Video{ID: "kept", DriveID: "other", FileID: "kept", FileName: "kept.mp4", Size: 100, SampledSHA256: "sample"}
	if err := cat.UpsertVideo(ctx, canonical); err != nil {
		t.Fatal(err)
	}
	canonical, err := cat.GetVideo(ctx, canonical.ID)
	if err != nil {
		t.Fatal(err)
	}
	source := &Video{ID: "skipped", DriveID: "crawler", FileID: "skipped", FileName: "skipped.mp4", Size: 100, SampledSHA256: "sample"}
	seen := CrawlerSourceSeen{Kind: "scriptcrawler", DriveID: "crawler", SourceID: "source", Status: "duplicate", Size: 100, SampledSHA256: "sample"}
	evidence := dedupe.NewEvidence(dedupe.ReasonSampledSHA256, canonical.ID, canonical.ID, "existing_match")
	if _, err := cat.db.Exec(`CREATE TRIGGER fail_crawler_record BEFORE INSERT ON duplicate_records BEGIN SELECT RAISE(ABORT, 'audit failed'); END`); err != nil {
		t.Fatal(err)
	}
	if err := cat.RecordCrawlerDuplicate(ctx, source, canonical, seen, evidence); err == nil {
		t.Fatal("expected audit failure")
	}
	assertDuplicateRecordCount(t, cat, 0)
	var count int
	if err := cat.db.QueryRow(`SELECT COUNT(*) FROM crawler_seen_sources`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed audit marked crawler source seen: count=%d err=%v", count, err)
	}
	if _, err := cat.db.Exec(`DROP TRIGGER fail_crawler_record`); err != nil {
		t.Fatal(err)
	}
	if err := cat.RecordCrawlerDuplicate(ctx, source, canonical, seen, evidence); err != nil {
		t.Fatal(err)
	}
	assertDuplicateRecordCount(t, cat, 1)
	if err := cat.db.QueryRow(`SELECT COUNT(*) FROM crawler_seen_sources WHERE canonical_video_id='kept' AND status='duplicate'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("seen state not committed: count=%d err=%v", count, err)
	}
	if _, err := cat.IncrementView(ctx, canonical.ID); err != nil {
		t.Fatal(err)
	}
	if err := cat.RecordCrawlerDuplicate(ctx, source, canonical, seen, evidence); err != nil {
		t.Fatal(err)
	}
	if err := cat.db.QueryRow(`SELECT occurrences FROM duplicate_records`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("repeat observation not coalesced: count=%d err=%v", count, err)
	}
}

func TestReplacementFailureRollsBackDuplicateRecord(t *testing.T) {
	cat := duplicateRecordTestCatalog(t)
	ctx := context.Background()
	if err := cat.UpsertVideo(ctx, &Video{ID: "old", DriveID: "drive", FileID: "old", Size: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.db.Exec(`CREATE TRIGGER reject_replacement BEFORE DELETE ON videos WHEN OLD.id='old' BEGIN SELECT RAISE(ABORT, 'retire failed'); END`); err != nil {
		t.Fatal(err)
	}
	evidence := dedupe.NewEvidence(dedupe.ReasonTitleThumb, "new", "new", "larger_file")
	evidence.Match = &dedupe.Match{Stage: dedupe.StageNear, LeftID: "new", RightID: "old", Score: 0.96, TitleScore: 1}
	err := cat.ReplaceDuplicateVideo(ctx, DuplicateVideoReplacement{
		NewVideo:        &Video{ID: "new", DriveID: "drive", FileID: "new", Size: 200},
		ReplacedVideoID: "old", Evidence: evidence,
	})
	if err == nil {
		t.Fatal("expected replacement failure")
	}
	assertDuplicateRecordCount(t, cat, 0)
	if _, err := cat.GetVideo(ctx, "old"); err != nil {
		t.Fatalf("old row lost: %v", err)
	}
	if _, err := cat.GetVideo(ctx, "new"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("replacement was partially published: %v", err)
	}
}

func TestDuplicateRecordSchemaUpgradeDoesNotBackfillHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	cat, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cat != nil {
			_ = cat.Close()
		}
	})
	_, err = cat.db.Exec(`DROP TABLE duplicate_records;
INSERT INTO deleted_videos (id,reason,canonical_video_id,deleted_at) VALUES ('historical','duplicate','kept',1);
INSERT INTO crawler_seen_sources (kind,drive_id,source_id,status,canonical_video_id,first_seen_at,last_seen_at)
VALUES ('scriptcrawler','crawler','historical-source','duplicate','kept',1,2)`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := cat.Close(); err != nil {
			t.Fatal(err)
		}
		cat, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		assertDuplicateRecordCount(t, cat, 0)
		var indexes int
		if err := cat.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
WHERE type='index' AND tbl_name='duplicate_records' AND name IN (
  'idx_duplicate_records_source', 'idx_duplicate_records_reason',
  'idx_duplicate_records_canonical', 'idx_duplicate_records_video_outcome'
)`).Scan(&indexes); err != nil || indexes != 4 {
			t.Fatalf("audit indexes=%d err=%v", indexes, err)
		}
		var reason, canonical string
		var deletedAt int64
		if err := cat.db.QueryRow(`SELECT reason,canonical_video_id,deleted_at FROM deleted_videos WHERE id='historical'`).Scan(&reason, &canonical, &deletedAt); err != nil {
			t.Fatal(err)
		}
		if reason != DeletedVideoReasonDuplicate || canonical != "kept" || deletedAt != 1 {
			t.Fatalf("old tombstone changed: %s/%s/%d", reason, canonical, deletedAt)
		}
		var status string
		var first, last int64
		if err := cat.db.QueryRow(`SELECT status,canonical_video_id,first_seen_at,last_seen_at
FROM crawler_seen_sources WHERE kind='scriptcrawler' AND drive_id='crawler' AND source_id='historical-source'`).Scan(&status, &canonical, &first, &last); err != nil {
			t.Fatal(err)
		}
		if status != "duplicate" || canonical != "kept" || first != 1 || last != 2 {
			t.Fatalf("old crawler state changed: %s/%s/%d/%d", status, canonical, first, last)
		}
	}
}

func TestDuplicateRecordsPersistAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	cat, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cat != nil {
			_ = cat.Close()
		}
	})
	ctx := context.Background()
	canonical := &Video{ID: "kept", DriveID: "drive-a", FileID: "kept", FileName: "same.mp4", Size: 100}
	if err := cat.UpsertVideo(ctx, canonical); err != nil {
		t.Fatal(err)
	}
	source := &Video{ID: "skipped", DriveID: "drive-b", FileID: "skipped", FileName: "same.mp4", Size: 100}
	if inserted, err := cat.InsertScannedVideo(ctx, source, nil); err != nil || inserted {
		t.Fatalf("initial decision: inserted=%v err=%v", inserted, err)
	}
	readRecord := func() string {
		t.Helper()
		var record string
		if err := cat.db.QueryRow(`SELECT json_array(record_key,origin,outcome,reason,
source_snapshot,canonical_snapshot,matched_snapshot,selected_snapshot,evidence,
first_seen_at,last_seen_at,occurrences) FROM duplicate_records WHERE video_id='skipped'`).Scan(&record); err != nil {
			t.Fatal(err)
		}
		return record
	}
	before := readRecord()
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	cat, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	assertDuplicateRecordCount(t, cat, 1)
	if after := readRecord(); after != before {
		t.Fatalf("restart changed audit record:\nbefore=%s\nafter=%s", before, after)
	}
	if inserted, err := cat.InsertScannedVideo(ctx, source, nil); err != nil || inserted {
		t.Fatalf("repeated decision: inserted=%v err=%v", inserted, err)
	}
	assertDuplicateRecordCount(t, cat, 1)
	var occurrences int
	if err := cat.db.QueryRow(`SELECT occurrences FROM duplicate_records WHERE video_id='skipped'`).Scan(&occurrences); err != nil || occurrences != 2 {
		t.Fatalf("repeated observation not coalesced: count=%d err=%v", occurrences, err)
	}
}

func assertDuplicateRecordCount(t *testing.T, cat *Catalog, want int) {
	t.Helper()
	var count int
	if err := cat.db.QueryRow(`SELECT COUNT(*) FROM duplicate_records`).Scan(&count); err != nil || count != want {
		t.Fatalf("duplicate records=%d err=%v, want %d", count, err, want)
	}
}
