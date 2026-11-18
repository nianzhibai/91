package catalog

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestMergedSourceDeletionKeepsAliasesAndCanRetry(t *testing.T) {
	ctx := context.Background()
	cat := openCanonicalTestCatalog(t)
	for _, v := range []*Video{
		{ID: "A", DriveID: "one", FileID: "a", Size: 10},
		{ID: "B", DriveID: "two", FileID: "b", ContentHash: "h", Size: 10},
		{ID: "C", DriveID: "two", FileID: "b", ContentHash: "h", Size: 10},
		{ID: "D", DriveID: "two", FileID: "b", ContentHash: "older", Size: 10},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.ApplyDuplicateVideoDeletions(ctx, []DuplicateVideoDeletion{{VideoID: "B", CanonicalVideoID: "A"}, {VideoID: "D", CanonicalVideoID: "A"}}); err != nil {
		t.Fatal(err)
	}
	list, err := cat.ListMergedVideos(ctx, MergedVideoFilter{CanonicalVideoID: "A", DriveID: "two", SourceState: MergedSourceRecorded, Size: 1})
	if err != nil || list.Total != 2 || len(list.Items) != 1 {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	b, err := cat.GetMergedVideo(ctx, "B")
	if err != nil || !b.SharedSource {
		t.Fatalf("B=%+v err=%v", b, err)
	}
	calls := 0
	remove := func(ctx context.Context, v *Video) error {
		calls++
		if v.ID != "B" || v.DriveID != "two" || v.FileID != "b" {
			t.Fatalf("unexpected removal: %+v", v)
		}
		if err := cat.UpsertVideo(ctx, &Video{ID: "another", DriveID: "two", FileID: "b"}); err == nil {
			t.Fatal("bound source during removal")
		}
		return nil
	}
	if err := cat.RemoveMergedVideoSource(ctx, "B", "stale", remove); !errors.Is(err, ErrMergedRevisionChanged) {
		t.Fatal(err)
	}
	if err := cat.RemoveMergedVideoSource(ctx, "B", b.SourceRevision, remove); !errors.Is(err, ErrMergedSourceShared) {
		t.Fatal(err)
	}
	if err := cat.ApplyDuplicateVideoDeletions(ctx, []DuplicateVideoDeletion{{VideoID: "C", CanonicalVideoID: "A"}}); err != nil {
		t.Fatal(err)
	}
	if pending, err := cat.CountDeletedVideosPendingSourceDeletion(ctx); err != nil || pending != 0 {
		t.Fatalf("blacklist pending=%d err=%v", pending, err)
	}
	offline := errors.New("offline")
	if err := cat.RemoveMergedVideoSource(ctx, "B", b.SourceRevision, func(context.Context, *Video) error { return offline }); !errors.Is(err, offline) {
		t.Fatal(err)
	}
	for range 2 {
		if err := cat.RemoveMergedVideoSource(ctx, "B", b.SourceRevision, remove); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("provider calls=%d", calls)
	}
	for id, state := range map[string]string{"B": MergedSourceGone, "C": MergedSourceGone, "D": MergedSourceReplaced} {
		m, err := cat.GetMergedVideo(ctx, id)
		if err != nil || m.SourceState != state {
			t.Fatalf("%s=%+v err=%v", id, m, err)
		}
		if got, err := cat.ResolveVideoID(ctx, id); err != nil || got != "A" {
			t.Fatalf("%s resolved to %s, err=%v", id, got, err)
		}
	}
}

func TestMergedScanPreservesPublicIdentity(t *testing.T) {
	ctx := context.Background()
	for _, scenario := range []string{"unchanged", "replacement", "absent canonical", "blacklisted"} {
		t.Run(scenario, func(t *testing.T) {
			cat := openCanonicalTestCatalog(t)
			a := &Video{ID: "A", DriveID: "one", FileID: "a", Size: 12}
			b := &Video{ID: "B", DriveID: "two", FileID: "b", FileName: "old.mp4", Size: 10, ContentHash: "h"}
			for _, v := range []*Video{a, b} {
				if err := cat.UpsertVideo(ctx, v); err != nil {
					t.Fatal(err)
				}
			}
			if err := cat.ApplyDuplicateVideoDeletions(ctx, []DuplicateVideoDeletion{{VideoID: "B", CanonicalVideoID: "A"}}); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "replacement":
				b.ContentHash = "changed"
			case "absent canonical":
				if err := cat.DeleteVideo(ctx, "A"); err != nil {
					t.Fatal(err)
				}
			case "blacklisted":
				if err := cat.DeleteVideoGroup(ctx, "A", VideoDeletionOptions{}, time.Now().Add(time.Minute), nil, nil); err != nil {
					t.Fatal(err)
				}
				if _, err := cat.ResolveVideoID(ctx, "B"); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("blacklisted alias resolved: %v", err)
				}
			}
			added, err := cat.InsertScannedVideo(ctx, b, nil)
			want := scenario == "replacement" || scenario == "absent canonical"
			if err != nil || added != want {
				t.Fatalf("admission=%t, err=%v", added, err)
			}
			if added && b.ID == "B" {
				t.Fatal("reused a retired public ID")
			}
			m, err := cat.GetMergedVideo(ctx, "B")
			if err != nil {
				t.Fatal(err)
			}
			if (m.SourceState == MergedSourceReplaced) != (scenario == "replacement") {
				t.Fatalf("state %s", m.SourceState)
			}
			if scenario == "blacklisted" {
				if ok, err := cat.IsVideoDeleted(ctx, "B"); err != nil || !ok {
					t.Fatalf("group not blacklisted: %t %v", ok, err)
				}
				if err := cat.RemoveDeletedVideo(ctx, "A"); err != nil {
					t.Fatal(err)
				}
				if err := cat.RemoveDeletedVideo(ctx, "B"); err != nil {
					t.Fatal(err)
				}
				if _, err := cat.GetVideo(ctx, "B"); !errors.Is(err, sql.ErrNoRows) {
					t.Fatal("unblocking revived the merged row")
				}
			}
			if scenario != "absent canonical" {
				if got, err := cat.ResolveVideoID(ctx, "B"); err != nil || got != "A" {
					t.Fatalf("old link: %q, %v", got, err)
				}
			}
		})
	}
}

func TestLegacyMergedSourceMigration(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", t.TempDir()+"/old.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Restore may encounter an archive with neither audit history nor restore_payload.
	if _, err := db.Exec(`CREATE TABLE deleted_videos(id TEXT PRIMARY KEY, drive_id TEXT,file_id TEXT,file_name TEXT,size_bytes INTEGER,content_hash TEXT,
		reason TEXT,canonical_video_id TEXT,source_deleted INTEGER,deleted_at INTEGER);
		INSERT INTO deleted_videos VALUES('B','drive','file','old.mp4',10,'hash','duplicate','A',1,123),
		('manual','drive','other','manual.mp4',20,'','','',0,100)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := MigrateMergedVideos(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	var canonical, state, reason string
	var mergedAt int64
	if err := db.QueryRow("SELECT canonical_video_id,source_state,merge_reason,merged_at FROM merged_videos WHERE video_id='B'").Scan(&canonical, &state, &reason, &mergedAt); err != nil ||
		canonical != "A" || state != MergedSourceGone || reason != "" || mergedAt != 123 {
		t.Fatalf("migrated alias=%s/%s/%s/%d err=%v", canonical, state, reason, mergedAt, err)
	}
	var remaining string
	if err := db.QueryRow("SELECT group_concat(id) FROM deleted_videos").Scan(&remaining); err != nil || remaining != "manual" {
		t.Fatalf("blacklist=%s err=%v", remaining, err)
	}
}

func TestSharedMergedFileRefreshInvalidatesOldSnapshot(t *testing.T) {
	ctx := context.Background()
	cat := openCanonicalTestCatalog(t)
	for _, id := range []string{"A", "B"} {
		if err := cat.UpsertVideo(ctx, &Video{ID: id, DriveID: "drive", FileID: "shared", ContentHash: "old", Size: 10}); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.ApplyDuplicateVideoDeletions(ctx, []DuplicateVideoDeletion{{VideoID: "B", CanonicalVideoID: "A"}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateVideoMeta(ctx, "A", VideoMetaPatch{ResetContent: true, Size: 10, ContentHash: "new"}); err != nil {
		t.Fatal(err)
	}
	m, err := cat.GetMergedVideo(ctx, "B")
	if err != nil || m.SourceState != MergedSourceReplaced {
		t.Fatalf("old file=%+v err=%v", m, err)
	}
}
