package catalog

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"
)

// A and alias share one file. A/B match by provider hash, B/C by sampled
// fingerprint, C/D by the catalog's filename/size rule. E has an unrelated file.
func seedDeletionGroup(t *testing.T) *Catalog {
	t.Helper()
	cat := openCanonicalTestCatalog(t)
	for _, v := range []*Video{
		{ID: "A", DriveID: "one", FileID: "shared", ContentHash: "ab", FileName: "a.mp4", Size: 10},
		{ID: "alias", DriveID: "one", FileID: "shared", FileName: "alias.mp4", Size: 10},
		{ID: "B", DriveID: "two", FileID: "shared", ContentHash: "ab", SampledSHA256: "bc", FileName: "b.mp4", Size: 10},
		{ID: "C", DriveID: "two", FileID: "c", SampledSHA256: "bc", FileName: "same.mp4", Size: 10},
		{ID: "D", DriveID: "two", FileID: "d", FileName: "same.mp4", Size: 10},
		{ID: "E", DriveID: "two", FileID: "e", FileName: "same.mp4", Size: 11},
	} {
		if err := cat.UpsertVideo(context.Background(), v); err != nil {
			t.Fatal(err)
		}
	}
	return cat
}

func TestRecordOnlyDeletionIncludesDuplicateGroup(t *testing.T) {
	ctx := context.Background()
	cat := seedDeletionGroup(t)
	if err := cat.DeleteVideoGroup(ctx, "alias", VideoDeletionOptions{}, time.Now().Add(time.Minute), nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"A", "alias", "B", "C", "D"} {
		if _, err := cat.GetVideo(ctx, id); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s remains: %v", id, err)
		}
		if ok, err := cat.IsVideoDeleted(ctx, id); err != nil || !ok {
			t.Fatalf("%s tombstone: %t %v", id, ok, err)
		}
	}
	if _, err := cat.GetVideo(ctx, "E"); err != nil {
		t.Fatal(err)
	}
}

func TestPhysicalDeletionChoiceAndSharedFiles(t *testing.T) {
	ctx := context.Background()
	for _, all := range []bool{false, true} {
		t.Run(map[bool]string{false: "selected", true: "all"}[all], func(t *testing.T) {
			cat := seedDeletionGroup(t)
			var calls []string
			remove := func(_ context.Context, v *Video, _ bool) error {
				calls = append(calls, v.DriveID+"/"+v.FileID)
				return nil
			}
			preview, err := cat.PreviewVideoDeletion(ctx, "A")
			if err != nil || len(preview.Sources) != 4 || !preview.RequiresConfirmation || preview.GroupRevision == "" {
				t.Fatalf("preview=%+v err=%v", preview, err)
			}
			options := VideoDeletionOptions{DeleteSource: true, IncludeDuplicates: &all, GroupRevision: preview.GroupRevision}
			if err := cat.DeleteVideoGroup(ctx, "A", options, time.Time{}, remove, nil); err != nil {
				t.Fatal(err)
			}
			want := []string{"one/shared"}
			if all {
				want = []string{"two/d", "two/c", "two/shared", "one/shared"}
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("provider calls=%v want=%v", calls, want)
			}
			for _, id := range []string{"A", "alias", "B", "C", "D", "E"} {
				_, err := cat.GetVideo(ctx, id)
				deleted := id == "A" || id == "alias" || (all && id != "E")
				if (deleted && !errors.Is(err, sql.ErrNoRows)) || (!deleted && err != nil) {
					t.Fatalf("%s deleted=%t, err=%v", id, deleted, err)
				}
			}
		})
	}
}

func TestSharedFileAloneNeedsNoChoice(t *testing.T) {
	ctx := context.Background()
	cat := openCanonicalTestCatalog(t)
	for _, id := range []string{"a", "b"} {
		if err := cat.UpsertVideo(ctx, &Video{ID: id, DriveID: "one", FileID: "file"}); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	err := cat.DeleteVideoGroup(ctx, "a", VideoDeletionOptions{DeleteSource: true}, time.Time{}, func(ctx context.Context, _ *Video, _ bool) error {
		calls++
		// A competing delete, new binding or migration cannot invalidate the
		// provider operation. Unrelated catalog writes remain available.
		if err := cat.DeleteVideo(ctx, "b"); err == nil {
			t.Fatal("deleted reserved owner")
		}
		if err := cat.UpsertVideo(ctx, &Video{ID: "c", DriveID: "one", FileID: "file"}); err == nil {
			t.Fatal("bound reserved file")
		}
		return cat.SetSetting(ctx, "unrelated", "value")
	}, nil)
	if err != nil || calls != 1 {
		t.Fatalf("calls=%d,err=%v", calls, err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := cat.GetVideo(ctx, id); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s: %v", id, err)
		}
	}
}

func TestDeletionGroupStaleChoiceAndPartialRetry(t *testing.T) {
	ctx := context.Background()
	cat := seedDeletionGroup(t)
	preview, err := cat.PreviewVideoDeletion(ctx, "A")
	if err != nil {
		t.Fatal(err)
	}
	all := true
	options := VideoDeletionOptions{DeleteSource: true, IncludeDuplicates: &all, GroupRevision: preview.GroupRevision}
	if err := cat.UpsertVideo(ctx, &Video{ID: "new", DriveID: "three", FileID: "new", ContentHash: "ab"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.DeleteVideoGroup(ctx, "A", options, time.Time{}, nil, nil); !errors.Is(err, ErrDeletionScopeChanged) {
		t.Fatalf("stale preview: %v", err)
	}
	preview, err = cat.PreviewVideoDeletion(ctx, "A")
	if err != nil || preview.GroupRevision == options.GroupRevision {
		t.Fatalf("new preview: %+v %v", preview, err)
	}
	options.GroupRevision = preview.GroupRevision
	var calls []string
	fail := true
	remove := func(_ context.Context, v *Video, _ bool) error {
		if fail && v.ID == "C" {
			return errors.New("offline")
		}
		calls = append(calls, v.ID)
		return nil
	}
	if err := cat.DeleteVideoGroup(ctx, "A", options, time.Time{}, remove, nil); err == nil {
		t.Fatal("expected provider failure")
	}
	if _, err := cat.GetVideo(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.GetVideo(ctx, "C"); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.GetVideo(ctx, "D"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("completed file: %v", err)
	}
	fail = false
	preview, err = cat.PreviewVideoDeletion(ctx, "A")
	if err != nil {
		t.Fatal(err)
	}
	options.GroupRevision = preview.GroupRevision
	if err := cat.DeleteVideoGroup(ctx, "A", options, time.Time{}, remove, nil); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, id := range calls {
		if seen[id] {
			t.Fatalf("removed %s twice", id)
		}
		seen[id] = true
	}
	if len(seen) != 5 {
		t.Fatalf("removed %v", calls)
	}
}

func TestDeletionIncludesMergedAndScanSkippedFiles(t *testing.T) {
	ctx := context.Background()
	for _, physical := range []bool{false, true} {
		t.Run(map[bool]string{false: "blacklist", true: "remove"}[physical], func(t *testing.T) {
			cat := openCanonicalTestCatalog(t)
			root := &Video{ID: "root", DriveID: "one", FileID: "root-file", FileName: "root.mp4", ContentHash: "hash", Size: 100}
			retired := &Video{ID: "retired", DriveID: "two", FileID: "retired-file", FileName: "other.mp4", Size: 90}
			for _, v := range []*Video{root, retired} {
				if err := cat.UpsertVideo(ctx, v); err != nil {
					t.Fatal(err)
				}
			}
			if err := cat.ApplyDuplicateVideoDeletions(ctx, []DuplicateVideoDeletion{{VideoID: "retired", CanonicalVideoID: "root"}}); err != nil {
				t.Fatal(err)
			}
			skipped := &Video{ID: "skipped", DriveID: "three", FileID: "skipped-file", FileName: "skip.mp4", ContentHash: "hash", Size: 100}
			if added, err := cat.InsertScannedVideo(ctx, skipped, nil); err != nil || added {
				t.Fatalf("skip: %t, %v", added, err)
			}
			var history []string
			remove := func(_ context.Context, v *Video, historical bool) error {
				if historical {
					history = append(history, v.ID)
				}
				return nil
			}
			preview, err := cat.PreviewVideoDeletion(ctx, "root")
			if err != nil {
				t.Fatal(err)
			}
			options := VideoDeletionOptions{DeleteSource: physical, IncludeDuplicates: &physical, GroupRevision: preview.GroupRevision}
			if err := cat.DeleteVideoGroup(ctx, "root", options, time.Time{}, remove, nil); err != nil {
				t.Fatal(err)
			}
			if physical && !reflect.DeepEqual(history, []string{"skipped", "retired"}) {
				t.Fatalf("historical files: %v", history)
			}
			// Removing the whole group keeps its content blacklisted.
			for _, id := range []string{"root", "retired", "skipped"} {
				if entry := deletedEntry(t, cat, id); entry == nil || entry.SourceDeleted != physical {
					t.Fatalf("tombstone %s=%+v", id, entry)
				}
			}
		})
	}
}

func deletedEntry(t *testing.T, cat *Catalog, id string) *DeletedVideo {
	t.Helper()
	items, _, err := cat.ListDeletedVideos(context.Background(), ListParams{Page: 1, PageSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	return nil
}

func TestRemovingGroupSourcesKeepsContentBlocked(t *testing.T) {
	ctx := context.Background()
	remove := func(context.Context, *Video, bool) error { return nil }
	for _, all := range []bool{false, true} {
		t.Run(map[bool]string{false: "selected", true: "all"}[all], func(t *testing.T) {
			cat := openCanonicalTestCatalog(t)
			seedVideos(t, cat,
				&Video{ID: "A", DriveID: "one", FileID: "a", ContentHash: "hash", SampledSHA256: "s", FileName: "a.mp4", Size: 10},
				&Video{ID: "B", DriveID: "two", FileID: "b", SampledSHA256: "s", FileName: "b.mp4", Size: 10},
			)
			preview, err := cat.PreviewVideoDeletion(ctx, "A")
			if err != nil {
				t.Fatal(err)
			}
			options := VideoDeletionOptions{DeleteSource: true, IncludeDuplicates: &all, GroupRevision: preview.GroupRevision}
			if err := cat.DeleteVideoGroup(ctx, "A", options, time.Time{}, remove, nil); err != nil {
				t.Fatal(err)
			}
			if entry := deletedEntry(t, cat, "A"); (entry != nil) != all {
				t.Fatalf("entry A=%+v, want present=%t", entry, all)
			}
			if !all {
				// B keeps the content, so a new copy is an ordinary duplicate of B
				// and nothing was blacklisted.
				if entry := deletedEntry(t, cat, "B"); entry != nil {
					t.Fatalf("kept video blacklisted: %+v", entry)
				}
				if admit(t, cat, &Video{ID: "N", DriveID: "three", FileID: "n", SampledSHA256: "s", FileName: "n.mp4", Size: 10}) {
					t.Fatal("copy of a kept video was admitted")
				}
				return
			}
			entry := deletedEntry(t, cat, "A")
			if !entry.SourceDeleted || entry.RestorePolicy != DeletedVideoRestorePolicyScan {
				t.Fatalf("entry A=%+v", entry)
			}
			if admit(t, cat, &Video{ID: "N", DriveID: "three", FileID: "n", ContentHash: "hash", FileName: "n.mp4", Size: 10}) {
				t.Fatal("copy of removed content was admitted")
			}
			if admit(t, cat, &Video{ID: "S", DriveID: "one", FileID: "s", FileName: "a.mp4", Size: 10}) {
				t.Fatal("same name and size on the removed file's drive was admitted")
			}
			// The provider may reuse the removed file ID for other content.
			reused := &Video{ID: "A", DriveID: "one", FileID: "a", ContentHash: "other", FileName: "other.mp4", Size: 20}
			if !admit(t, cat, reused) || reused.ID == "A" {
				t.Fatalf("reused file ID: admitted as %q", reused.ID)
			}
			// Lifting one entry releases the content held by the whole group.
			if err := cat.RemoveDeletedVideo(ctx, "A"); err != nil {
				t.Fatal(err)
			}
			if !admit(t, cat, &Video{ID: "M", DriveID: "three", FileID: "m", ContentHash: "hash", FileName: "m.mp4", Size: 10}) {
				t.Fatal("released content stayed blocked")
			}
		})
	}
}
