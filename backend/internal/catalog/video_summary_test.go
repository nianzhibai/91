package catalog

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestVisibleVideoSummariesByIDsReturnsOnlyVisibleCardsInSnapshotOrder(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	base := time.UnixMilli(1_800_000_000_000)
	for index, id := range []string{"first", "second", "hidden"} {
		publishedAt := base.Add(time.Duration(index) * time.Minute)
		video := &Video{
			ID:                 id,
			DriveID:            "drive",
			FileID:             "file-" + id,
			FileName:           id + ".mp4",
			Title:              "Title " + id,
			Author:             "Author " + id,
			DurationSeconds:    120 + index,
			ThumbnailURL:       "/p/thumb/" + id,
			ThumbnailUpdatedAt: publishedAt,
			PreviewLocal:       "previews/" + id + ".mp4",
			PreviewStatus:      "ready",
			PreviewUpdatedAt:   publishedAt,
			Views:              100 + index,
			Badges:             []string{"badge-" + id},
			Description:        "detail-only description",
			PublishedAt:        publishedAt,
			CreatedAt:          publishedAt,
			UpdatedAt:          publishedAt,
		}
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	if err := cat.HideVideo(ctx, "hidden"); err != nil {
		t.Fatalf("hide video: %v", err)
	}

	summaries, err := cat.VisibleVideoSummariesByIDs(
		ctx,
		[]string{"second", "missing", "hidden", "first"},
	)
	if err != nil {
		t.Fatalf("load summaries: %v", err)
	}
	if len(summaries) != 2 || summaries[0].ID != "second" || summaries[1].ID != "first" {
		t.Fatalf("summary order = %#v, want second then first", summaries)
	}

	second := summaries[0]
	if second.Title != "Title second" || second.Author != "Author second" ||
		second.DurationSeconds != 121 || second.ThumbnailURL != "/p/thumb/second" ||
		second.Views != 101 || !second.PublishedAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("second summary = %#v", second)
	}
	if !second.ThumbnailUpdatedAt.Equal(base.Add(time.Minute)) ||
		!second.PreviewUpdatedAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("summary asset revisions = %#v", second)
	}
	if !reflect.DeepEqual(second.Badges, []string{"badge-second"}) {
		t.Fatalf("summary badges = %v", second.Badges)
	}
}
