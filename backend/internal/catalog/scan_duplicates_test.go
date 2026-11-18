package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/driveevents"
)

func TestInsertScannedVideoCoordinatesConcurrentCatalogs(t *testing.T) {
	for _, match := range []string{"hash", "file name and size", "distinct"} {
		t.Run(match, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			path := filepath.Join(t.TempDir(), "catalog.db")
			catalogs := make([]*Catalog, 2)
			for i := range catalogs {
				cat, err := Open(path)
				if err != nil {
					t.Fatal(err)
				}
				catalogs[i] = cat
				t.Cleanup(func() { _ = cat.Close() })
			}
			const scans = 8
			type admission struct {
				inserted bool
				err      error
			}
			start := make(chan struct{})
			finished := make(chan admission, scans)
			for i := range scans {
				go func() {
					<-start
					v := &Video{
						ID: fmt.Sprintf("video-%d", i), DriveID: fmt.Sprintf("drive-%d", i),
						FileID: "file", FileName: "shared.mp4", Title: "Shared", Size: 123,
					}
					switch match {
					case "hash":
						v.FileName = fmt.Sprintf("copy-%d.mp4", i)
						v.ContentHash = "shared-hash"
						if i%2 == 0 {
							v.ContentHash = " SHARED-HASH "
						}
					case "distinct":
						v.Size += int64(i)
					}
					inserted, err := catalogs[i%len(catalogs)].InsertScannedVideo(ctx, v, map[string]struct{}{"file": {}})
					finished <- admission{inserted, err}
				}()
			}
			close(start)
			inserted := 0
			for range scans {
				got := <-finished
				if got.err != nil {
					t.Errorf("admit scan: %v", got.err)
				}
				if got.inserted {
					inserted++
				}
			}
			want := 1
			if match == "distinct" {
				want = scans
			}
			var stored int
			if err := catalogs[0].db.QueryRowContext(ctx, `SELECT COUNT(*) FROM videos`).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if inserted != want || stored != want {
				t.Fatalf("inserted=%d stored=%d, want %d of each", inserted, stored, want)
			}
		})
	}
}

func TestScannedVideoDuplicatePolicyPreservesLiveSources(t *testing.T) {
	for _, tc := range []struct {
		name     string
		seed     []*Video
		seen     map[string]struct{}
		wantID   string
		inserted bool
	}{
		{
			name: "stale same-drive source permits replacement",
			seed: []*Video{{ID: "old", DriveID: "drive", FileID: "old"}},
			seen: map[string]struct{}{"new": {}}, inserted: true,
		},
		{
			name: "live same-drive source prevents duplicate",
			seed: []*Video{{ID: "old", DriveID: "drive", FileID: "old"}},
			seen: map[string]struct{}{"new": {}, "old": {}}, wantID: "old",
		},
		{
			name: "live candidate after stale candidate prevents duplicate",
			seed: []*Video{{ID: "a-stale", DriveID: "drive", FileID: "old"}, {ID: "b-live", DriveID: "drive", FileID: "live"}},
			seen: map[string]struct{}{"new": {}, "live": {}}, wantID: "b-live",
		},
		{
			name: "other drive does not depend on this scan's seen files",
			seed: []*Video{{ID: "other", DriveID: "other-drive", FileID: "other"}},
			seen: map[string]struct{}{"new": {}}, wantID: "other",
		},
	} {
		for _, hash := range []string{"shared-hash", ""} {
			t.Run(tc.name+"/hash="+hash, func(t *testing.T) {
				cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = cat.Close() })
				ctx := context.Background()
				for _, seed := range tc.seed {
					v := *seed
					v.Title, v.FileName, v.Size, v.ContentHash = "Old", "shared.mp4", 123, hash
					if err := cat.UpsertVideo(ctx, &v); err != nil {
						t.Fatal(err)
					}
				}
				v := &Video{ID: "new", DriveID: "drive", FileID: "new", Title: "New", FileName: "shared.mp4", Size: 123, ContentHash: hash}
				if hash != "" {
					v.FileName = "renamed.mp4"
				}
				duplicate, err := cat.FindScannedVideoDuplicate(ctx, v, tc.seen)
				if err != nil {
					t.Fatal(err)
				}
				if tc.wantID == "" && duplicate != nil || tc.wantID != "" && (duplicate == nil || duplicate.ID != tc.wantID) {
					t.Fatalf("duplicate=%+v, want %q", duplicate, tc.wantID)
				}
				inserted, err := cat.InsertScannedVideo(ctx, v, tc.seen)
				if err != nil || inserted != tc.inserted {
					t.Fatalf("inserted=%t error=%v, want inserted=%t", inserted, err, tc.inserted)
				}
			})
		}
	}
}

func TestInsertScannedVideoDoesNotOverwriteExistingSource(t *testing.T) {
	for _, sameID := range []bool{true, false} {
		t.Run(fmt.Sprintf("same-id=%t", sameID), func(t *testing.T) {
			cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cat.Close() })
			ctx := context.Background()
			old := &Video{
				ID: "existing", DriveID: "drive", FileID: "file", Title: "Curated title",
				FileName: "old.mp4", Size: 123, ThumbnailURL: "/p/thumb/existing", Views: 42,
			}
			if err := cat.UpsertVideo(ctx, old); err != nil {
				t.Fatal(err)
			}
			incoming := &Video{ID: "new", DriveID: "drive", FileID: "file", Title: "New title", FileName: "new.mp4", Size: 456}
			if sameID {
				incoming.ID = old.ID
			}
			if inserted, err := cat.InsertScannedVideo(ctx, incoming, nil); err != nil || inserted {
				t.Fatalf("existing source admitted again: inserted=%t error=%v", inserted, err)
			}
			stored, err := cat.GetVideo(ctx, old.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Title != old.Title || stored.ThumbnailURL != old.ThumbnailURL || stored.Views != old.Views {
				t.Fatalf("existing source was overwritten: %+v", stored)
			}
		})
	}
}

func TestInsertScannedVideoRollsBackFailedAdmission(t *testing.T) {
	cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	cat.db.SetMaxOpenConns(1)
	ctx := context.Background()
	if _, err := cat.db.ExecContext(ctx, `CREATE TRIGGER reject_scan_insert AFTER INSERT ON videos
WHEN NEW.id = 'rejected' BEGIN SELECT RAISE(ABORT, 'test insert failure'); END`); err != nil {
		t.Fatal(err)
	}
	v := &Video{ID: "rejected", DriveID: "drive-a", FileID: "file", Title: "Shared", FileName: "shared.mp4", Size: 123}
	if inserted, err := cat.InsertScannedVideo(ctx, v, nil); err == nil || inserted {
		t.Fatalf("failed insert accepted: inserted=%t error=%v", inserted, err)
	}
	v.ID, v.DriveID = "accepted", "drive-b"
	if inserted, err := cat.InsertScannedVideo(ctx, v, nil); err != nil || !inserted {
		t.Fatalf("failed admission blocked another scan: inserted=%t error=%v", inserted, err)
	}
	var stored int
	if err := cat.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM videos`).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("rows after failed admission=%d error=%v", stored, err)
	}
}

func TestInsertScannedVideoCancellationReleasesConnection(t *testing.T) {
	cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	barrier, err := cat.BeginWriteBarrier(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	v := &Video{ID: "canceled", DriveID: "drive-a", FileID: "file", Title: "Shared", FileName: "shared.mp4", Size: 123}
	if inserted, err := cat.InsertScannedVideo(ctx, v, nil); err == nil || inserted || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("canceled admission: inserted=%t error=%v context=%v", inserted, err, ctx.Err())
	}
	if err := barrier.Close(); err != nil {
		t.Fatal(err)
	}
	v.ID, v.DriveID = "accepted", "drive-b"
	if inserted, err := cat.InsertScannedVideo(context.Background(), v, nil); err != nil || !inserted {
		t.Fatalf("admission after cancellation: inserted=%t error=%v", inserted, err)
	}
}

func TestInsertScannedVideoIgnoresHiddenDuplicatePartners(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	if err := cat.UpsertVideo(ctx, &Video{ID: "hidden", DriveID: "other", FileID: "old", FileName: "a.mp4", ContentHash: "hash", Size: 10, Hidden: true}); err != nil {
		t.Fatal(err)
	}
	inserted, err := cat.InsertScannedVideo(ctx, &Video{ID: "new", DriveID: "drive", FileID: "new", FileName: "a.mp4", ContentHash: "hash", Size: 10}, nil)
	if err != nil || !inserted {
		t.Fatalf("hidden row blocked admission: inserted=%v err=%v", inserted, err)
	}
	version := cat.DriveEvents().Version("drive", driveevents.MediaChanged)
	inserted, err = cat.InsertScannedVideo(ctx, &Video{ID: "copy", DriveID: "third", FileID: "copy", FileName: "b.mp4", ContentHash: "HASH", Size: 10}, nil)
	if err != nil || inserted {
		t.Fatalf("visible hash match admitted: inserted=%v err=%v", inserted, err)
	}
	if got := cat.DriveEvents().Version("drive", driveevents.MediaChanged); got != version {
		t.Fatalf("audit-only skip changed media version from %d to %d", version, got)
	}
}

func TestFindScannedVideoDuplicateUsesIndexes(t *testing.T) {
	cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	cases := map[string]*Video{
		"name and size":        {ID: "v", FileName: "a.mp4", Size: 10},
		"hash":                 {ID: "v", ContentHash: "hash"},
		"hash, name and size":  {ID: "v", ContentHash: "hash", FileName: "a.mp4", Size: 10},
		"fingerprint":          {ID: "v", SampledSHA256: "sampled", Size: 10},
		"hash and fingerprint": {ID: "v", ContentHash: "hash", SampledSHA256: "sampled", Size: 10},
	}
	for name, video := range cases {
		t.Run(name, func(t *testing.T) {
			plan := &queryPlanRecorder{db: cat.db}
			if _, err := findScannedVideoDuplicate(context.Background(), plan, video, nil); err != nil {
				t.Fatal(err)
			}
			for _, detail := range plan.details {
				if strings.HasPrefix(detail, "SCAN videos") {
					t.Fatalf("lookup scans the table: %q", plan.details)
				}
			}
		})
	}
}

// queryPlanRecorder runs EXPLAIN QUERY PLAN for the lookup's exact SQL.
type queryPlanRecorder struct {
	db      *sql.DB
	details []string
}

func (r *queryPlanRecorder) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	rows, err := r.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			rows.Close()
			return nil, err
		}
		r.details = append(r.details, detail)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	// The lookup itself only needs an empty result.
	return r.db.QueryContext(ctx, "SELECT "+allVideoCols+" FROM videos WHERE 0")
}

func TestDuplicateTombstoneAllowsOnlyItsLiveCanonicalSource(t *testing.T) {
	ctx := context.Background()
	for _, sharedFile := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared-file-%t", sharedFile), func(t *testing.T) {
			cat := duplicateRecordTestCatalog(t)
			duplicate := &Video{ID: "scan-id", DriveID: "drive", FileID: "duplicate-file", FileName: "duplicate.mp4", ContentHash: "hash", Size: 100}
			kept := &Video{ID: "migrated-id", DriveID: "drive", FileID: "kept-file", FileName: "kept.mp4", ContentHash: "hash", Size: 100}
			if sharedFile {
				duplicate.FileID = kept.FileID
			}
			for _, v := range []*Video{duplicate, kept} {
				if err := cat.UpsertVideo(ctx, v); err != nil {
					t.Fatal(err)
				}
			}
			if err := cat.ApplyDuplicateVideoDeletions(ctx, []DuplicateVideoDeletion{{VideoID: duplicate.ID, CanonicalVideoID: kept.ID}}); err != nil {
				t.Fatal(err)
			}
			// The scanner's generated ID can even belong to the retired row.
			if blocked, err := cat.IsDeletedVideoCandidate(ctx, duplicate.ID, kept.DriveID, kept.FileID, kept.ContentHash, "renamed.mp4", 100); err != nil || blocked {
				t.Fatalf("survivor blocked=%t err=%v", blocked, err)
			}
			if !sharedFile {
				if blocked, err := cat.IsDeletedVideoCandidate(ctx, duplicate.ID, duplicate.DriveID, duplicate.FileID, duplicate.ContentHash, duplicate.FileName, 100); err != nil || !blocked {
					t.Fatalf("retired source blocked=%t err=%v", blocked, err)
				}
			}
		})
	}
}
