package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/video-site/backend/internal/catalog"
)

func TestShortsOrderedModesFreezeSortingAndResumeAfterRestart(t *testing.T) {
	for mode, want := range map[string][]string{
		"latest": {"latest", "older-liked", "recent-liked", "a-tie", "b-tie", "most-liked"},
		"hot":    {"most-liked", "recent-liked", "older-liked", "latest", "a-tie", "b-tie"},
	} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			cat, err := catalog.Open(t.TempDir() + "/catalog.db")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cat.Close() })
			now := time.Now()
			for _, video := range []*catalog.Video{
				{ID: "latest", PublishedAt: now},
				{ID: "older-liked", PublishedAt: now.Add(-30 * time.Minute), Likes: 5, LastLikedAt: now.Add(-time.Hour)},
				{ID: "recent-liked", PublishedAt: now.Add(-time.Hour), Likes: 5, LastLikedAt: now},
				{ID: "a-tie", PublishedAt: now.Add(-2 * time.Hour)},
				{ID: "b-tie", PublishedAt: now.Add(-2 * time.Hour)},
				{ID: "most-liked", PublishedAt: now.Add(-3 * time.Hour), Likes: 9},
				{ID: "hidden", PublishedAt: now.Add(time.Hour), Likes: 100},
			} {
				video.DriveID, video.FileID, video.Title = "drive", video.ID, video.ID
				video.CreatedAt, video.UpdatedAt = now, now
				if err := cat.UpsertVideo(ctx, video); err != nil {
					t.Fatal(err)
				}
			}
			if err := cat.HideVideo(ctx, "hidden"); err != nil {
				t.Fatal(err)
			}
			request := func(server *Server, path string) shortsFeedResponse {
				t.Helper()
				rr := httptest.NewRecorder()
				server.handleShortsNext(rr, httptest.NewRequest(http.MethodGet, path, nil))
				if rr.Code != http.StatusOK {
					t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
				}
				var response shortsFeedResponse
				if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
					t.Fatal(err)
				}
				return response
			}
			first := request(&Server{Catalog: cat}, "/api/shorts/next?mode="+mode+"&count=2")
			if first.Total != len(want) || first.RoundComplete || first.NextCursor != 2 {
				t.Fatalf("invalid initial response: %#v", first)
			}
			if err := cat.UpsertVideo(ctx, &catalog.Video{
				ID: "inserted-later", DriveID: "drive", FileID: "inserted-later", Title: "new",
				PublishedAt: now.Add(time.Hour), Likes: 99, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := cat.IncrementLike(ctx, "latest"); err != nil {
				t.Fatal(err)
			}
			server := &Server{Catalog: cat}
			next := request(server, "/api/shorts/next?mode="+mode+"&count=20&feedToken="+first.FeedToken+"&cursor="+strconv.Itoa(first.NextCursor))
			var got []string
			for _, item := range append(first.Items, next.Items...) {
				got = append(got, item.ID)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("frozen %s order = %v, want %v", mode, got, want)
			}
			if !next.RoundComplete || next.NextCursor != len(want) {
				t.Fatalf("invalid final response: %#v", next)
			}
			replayed := request(server, "/api/shorts/next?mode="+mode+"&count=2&feedToken="+first.FeedToken)
			if replayed.Items[0].ID != first.Items[0].ID || replayed.Items[1].ID != first.Items[1].ID {
				t.Fatal("replaying the same cursor changed the order")
			}
		})
	}
}

func TestShortsRejectsUnsupportedMode(t *testing.T) {
	rr := httptest.NewRecorder()
	(&Server{}).handleShortsNext(rr, httptest.NewRequest(http.MethodGet, "/api/shorts/next?mode=unknown", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}
