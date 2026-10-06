package api

import (
	"context"
	"encoding/json"
	"fmt"
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
		"hot":    {"most-liked", "recent-liked", "older-liked", "a-tie", "b-tie"},
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
				{ID: "a-tie", PublishedAt: now.Add(-2 * time.Hour), Likes: 1},
				{ID: "b-tie", PublishedAt: now.Add(-2 * time.Hour), Likes: 1},
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

func TestShortsLatestRoundsUseNewest100VisibleVideos(t *testing.T) {
	for _, videoCount := range []int{0, 6, 100, 125} {
		t.Run(strconv.Itoa(videoCount), func(t *testing.T) {
			ctx := context.Background()
			cat, err := catalog.Open(t.TempDir() + "/catalog.db")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cat.Close() })
			now := time.Now()
			allIDs := make([]string, 0, videoCount)
			for index := 0; index < videoCount; index++ {
				id := fmt.Sprintf("video-%03d", index)
				video := &catalog.Video{
					ID: id, DriveID: "drive", FileID: id, Title: id,
					// Tied timestamps must retain deterministic ID ordering.
					PublishedAt: now.Add(-time.Duration(index/2) * time.Minute),
					CreatedAt:   now, UpdatedAt: now,
				}
				if index >= 100 {
					// Older videos with ready thumbnails must not displace newer ones.
					video.ThumbnailURL = "/p/thumb/" + id
				}
				if err := cat.UpsertVideo(ctx, video); err != nil {
					t.Fatal(err)
				}
				allIDs = append(allIDs, id)
			}
			if err := cat.UpsertVideo(ctx, &catalog.Video{
				ID: "hidden-newest", DriveID: "drive", FileID: "hidden-newest", Title: "hidden",
				Hidden: true, PublishedAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}

			server := &Server{Catalog: cat}
			request := func(path string) shortsFeedResponse {
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
			readRound := func(first shortsFeedResponse) []string {
				t.Helper()
				var ids []string
				current := first
				for {
					if current.Total != first.Total || current.FeedToken != first.FeedToken {
						t.Fatalf("snapshot changed within a round: %#v", current)
					}
					for _, item := range current.Items {
						ids = append(ids, item.ID)
					}
					if current.RoundComplete {
						if current.NextCursor != first.Total {
							t.Fatalf("completed cursor = %d, want %d", current.NextCursor, first.Total)
						}
						return ids
					}
					cursor := current.NextCursor
					current = request("/api/shorts/next?mode=latest&count=20&feedToken=" + first.FeedToken + "&cursor=" + strconv.Itoa(cursor))
					if current.NextCursor <= cursor {
						t.Fatal("round did not advance")
					}
				}
			}

			first := request("/api/shorts/next?mode=latest&count=2")
			want := allIDs[:min(videoCount, 100)]
			if first.Total != len(want) {
				t.Fatalf("total = %d, want %d", first.Total, len(want))
			}
			if videoCount == 0 {
				if len(first.Items) != 0 || !first.RoundComplete || first.FeedToken != "" {
					t.Fatalf("invalid empty feed: %#v", first)
				}
				return
			}
			if err := cat.UpsertVideo(ctx, &catalog.Video{
				ID: "inserted-later", DriveID: "drive", FileID: "inserted-later", Title: "new",
				PublishedAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			// A restart must restore the same bounded snapshot and ordering.
			server = &Server{Catalog: cat}
			if got := readRound(first); !slices.Equal(got, want) {
				t.Fatalf("first round = %v, want %v", got, want)
			}

			next := request("/api/shorts/next?mode=latest&count=2")
			wantNext := append([]string{"inserted-later"}, allIDs...)
			wantNext = wantNext[:min(len(wantNext), 100)]
			if next.FeedToken == first.FeedToken || next.Total != len(wantNext) {
				t.Fatalf("invalid next round: %#v", next)
			}
			if got := readRound(next); !slices.Equal(got, wantNext) {
				t.Fatalf("next round = %v, want %v", got, wantNext)
			}
			if other := request("/api/shorts/next?mode=recommend&count=2"); other.Total != videoCount+1 {
				t.Fatalf("recommend total = %d, want %d", other.Total, videoCount+1)
			}
			if hot := request("/api/shorts/next?mode=hot&count=2"); hot.Total != 0 || len(hot.Items) != 0 || !hot.RoundComplete {
				t.Fatalf("unliked library returned hot videos: %#v", hot)
			}
		})
	}
}

func TestShortsLatestExpiresFullLibrarySnapshots(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	server := &Server{Catalog: cat}
	ids := make([]string, 125)
	for index := range ids {
		ids[index] = fmt.Sprintf("video-%03d", index)
	}
	server.storeShortsFeed("old-latest", ids)
	for _, source := range []string{"memory", "persisted"} {
		t.Run(source, func(t *testing.T) {
			if source == "persisted" {
				server = &Server{Catalog: cat}
			}
			for _, cursor := range []int{0, 100} {
				rr := httptest.NewRecorder()
				path := "/api/shorts/next?mode=latest&feedToken=old-latest&cursor=" + strconv.Itoa(cursor)
				server.handleShortsNext(rr, httptest.NewRequest(http.MethodGet, path, nil))
				if rr.Code != http.StatusGone {
					t.Fatalf("cursor %d: status = %d, want 410; body = %s", cursor, rr.Code, rr.Body.String())
				}
			}
		})
	}
}

func TestShortsHotIncludesAllLikedVisibleVideos(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	now := time.Now()
	var want []string
	for index := 0; index < 125; index++ {
		id := fmt.Sprintf("video-%03d", index)
		likes := 0
		if index < 120 {
			likes = 1
			if index != 20 {
				want = append(want, id)
			}
		}
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID: id, DriveID: "drive", FileID: id, Title: id, Likes: likes,
			PublishedAt: now.Add(-time.Duration(index) * time.Minute), CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID: "hidden-liked", DriveID: "drive", FileID: "hidden-liked", Title: "hidden",
		Hidden: true, Likes: 200, PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	server := &Server{Catalog: cat}
	request := func(path string) shortsFeedResponse {
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
	first := request("/api/shorts/next?mode=hot&count=20")
	if first.Total != 120 || first.NextCursor != 20 || first.RoundComplete {
		t.Fatalf("invalid liked snapshot: %#v", first)
	}
	if likes, err := cat.DecrementLike(ctx, "video-020"); err != nil || likes != 0 {
		t.Fatalf("remove last like: likes=%d, err=%v", likes, err)
	}
	server = &Server{Catalog: cat}
	var got []string
	current := first
	for {
		for _, item := range current.Items {
			if item.Likes <= 0 {
				t.Fatalf("hot feed returned an unliked video: %#v", item)
			}
			got = append(got, item.ID)
		}
		if current.RoundComplete {
			break
		}
		cursor := current.NextCursor
		current = request("/api/shorts/next?mode=hot&count=20&feedToken=" + first.FeedToken + "&cursor=" + strconv.Itoa(cursor))
		if current.NextCursor <= cursor {
			t.Fatal("round did not advance")
		}
	}
	if current.NextCursor != 120 || !slices.Equal(got, want) {
		t.Fatalf("completed cursor = %d; round = %v, want %v", current.NextCursor, got, want)
	}
	if next := request("/api/shorts/next?mode=hot&count=2"); next.Total != 119 || next.FeedToken == first.FeedToken {
		t.Fatalf("next round did not reflect removed like: %#v", next)
	}
}

func TestShortsHotSkipsUnlikedVideosInOlderSnapshots(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	now := time.Now()
	var ids []string
	for index := 0; index < 42; index++ {
		id := fmt.Sprintf("video-%03d", index)
		likes := 0
		if index >= 40 {
			likes = 1
		}
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID: id, DriveID: "drive", FileID: id, Title: id, Likes: likes,
			PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	server := &Server{Catalog: cat}
	server.storeShortsFeed("old-hot", ids)
	for _, source := range []string{"memory", "persisted"} {
		t.Run(source, func(t *testing.T) {
			if source == "persisted" {
				server = &Server{Catalog: cat}
			}
			rr := httptest.NewRecorder()
			server.handleShortsNext(rr, httptest.NewRequest(http.MethodGet, "/api/shorts/next?mode=hot&feedToken=old-hot&count=2", nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
			}
			var response shortsFeedResponse
			if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
				t.Fatal(err)
			}
			if len(response.Items) != 2 || response.NextCursor != 42 || !response.RoundComplete {
				t.Fatalf("invalid resumed response: %#v", response)
			}
			for index, item := range response.Items {
				if item.ID != ids[40+index] || item.Likes <= 0 || item.FeedCursor != 41+index {
					t.Fatalf("resumed item %d = %#v", index, item)
				}
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
