package catalog

import (
	"context"
	"testing"
	"time"
)

func TestListRecommendationCandidatesBoundsAndCombinesTagFilters(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	videos := []*Video{
		{
			ID: "current", DriveID: "drive", FileID: "current", Title: "Current",
			Tags: []string{"shared", "second"}, ThumbnailURL: "/thumb/current",
			PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "shared-ready-new", DriveID: "drive", FileID: "shared-ready-new", Title: "Shared ready new",
			Tags: []string{"shared"}, ThumbnailURL: "/thumb/shared-ready-new",
			PublishedAt: now.Add(-time.Minute), CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "second-ready", DriveID: "drive", FileID: "second-ready", Title: "Second ready",
			Tags: []string{"second"}, ThumbnailURL: "/thumb/second-ready",
			PublishedAt: now.Add(-2 * time.Minute), CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "shared-pending", DriveID: "drive", FileID: "shared-pending", Title: "Shared pending",
			Tags:        []string{"shared"},
			PublishedAt: now.Add(-3 * time.Minute), CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "unrelated-ready", DriveID: "drive", FileID: "unrelated-ready", Title: "Unrelated ready",
			Tags: []string{"other"}, ThumbnailURL: "/thumb/unrelated-ready",
			PublishedAt: now.Add(time.Minute), CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "hidden-shared", DriveID: "drive", FileID: "hidden-shared", Title: "Hidden shared",
			Tags: []string{"shared"}, ThumbnailURL: "/thumb/hidden-shared",
			PublishedAt: now.Add(2 * time.Minute), CreatedAt: now, UpdatedAt: now,
		},
	}
	for _, video := range videos {
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatalf("seed %s: %v", video.ID, err)
		}
	}
	if err := cat.MarkInvalidSizeSources(ctx, "drive", []string{"hidden-shared"}); err != nil {
		t.Fatalf("hide tagged candidate: %v", err)
	}

	ready, err := cat.ListRecommendationCandidates(ctx, RecommendationCandidateParams{
		Tags:               []string{"shared", "SECOND", "shared", ""},
		ExcludeIDs:         []string{"current", "current", ""},
		ThumbnailReadyOnly: true,
		Limit:              10,
	})
	if err != nil {
		t.Fatalf("list ready tag candidates: %v", err)
	}
	if len(ready) != 2 || ready[0].ID != "shared-ready-new" || ready[1].ID != "second-ready" {
		t.Fatalf("ready tag candidates = %#v", recommendationSummaryIDs(ready))
	}

	allTagged, err := cat.ListRecommendationCandidates(ctx, RecommendationCandidateParams{
		Tags:       []string{"shared", "second"},
		ExcludeIDs: []string{"current"},
		Limit:      2,
	})
	if err != nil {
		t.Fatalf("list bounded tag candidates: %v", err)
	}
	if len(allTagged) != 2 || allTagged[0].ID != "shared-ready-new" || allTagged[1].ID != "second-ready" {
		t.Fatalf("bounded tag candidates = %#v", recommendationSummaryIDs(allTagged))
	}

	global, err := cat.ListRecommendationCandidates(ctx, RecommendationCandidateParams{
		ExcludeIDs: []string{"current"},
		Limit:      1,
	})
	if err != nil {
		t.Fatalf("list global candidates: %v", err)
	}
	if len(global) != 1 || global[0].ID != "unrelated-ready" {
		t.Fatalf("global candidates = %#v", recommendationSummaryIDs(global))
	}

	none, err := cat.ListRecommendationCandidates(ctx, RecommendationCandidateParams{Limit: 0})
	if err != nil {
		t.Fatalf("list zero candidates: %v", err)
	}
	if none != nil {
		t.Fatalf("zero limit candidates = %#v, want nil", none)
	}
}

func recommendationSummaryIDs(videos []*VideoSummary) []string {
	ids := make([]string, 0, len(videos))
	for _, video := range videos {
		if video != nil {
			ids = append(ids, video.ID)
		}
	}
	return ids
}
