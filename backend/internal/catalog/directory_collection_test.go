package catalog

import (
	"context"
	"testing"
	"time"
)

func TestListVisibleVideosByDirectoryScopesAndFiltersCollection(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	seed := func(id, driveID, parentID, fileName string) {
		t.Helper()
		if err := cat.UpsertVideo(ctx, &Video{
			ID:          id,
			DriveID:     driveID,
			FileID:      id,
			FileName:    fileName,
			ParentID:    parentID,
			DirName:     "Series",
			Title:       fileName,
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	seed("episode-1", "drive-a", "folder-a", "episode 1.mp4")
	seed("episode-2", "drive-a", "folder-a", "episode 2.mp4")
	seed("other-folder", "drive-a", "folder-b", "other.mp4")
	seed("other-drive", "drive-b", "folder-a", "other-drive.mp4")
	seed("no-directory", "drive-a", "", "loose.mp4")
	seed("hidden", "drive-a", "folder-a", "hidden.mp4")
	if err := cat.HideVideo(ctx, "hidden"); err != nil {
		t.Fatalf("hide video: %v", err)
	}

	items, err := cat.ListVisibleVideosByDirectory(ctx, "drive-a", "folder-a")
	if err != nil {
		t.Fatalf("list directory: %v", err)
	}
	if len(items) != 2 || items[0].ID != "episode-1" || items[1].ID != "episode-2" {
		t.Fatalf("directory items = %#v, want episode-1 and episode-2", videoIDs(items))
	}

	empty, err := cat.ListVisibleVideosByDirectory(ctx, "drive-a", "")
	if err != nil {
		t.Fatalf("list empty directory: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty parent items = %#v, want no pseudo collection", videoIDs(empty))
	}
}

func videoIDs(items []*Video) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if item != nil {
			ids = append(ids, item.ID)
		}
	}
	return ids
}
