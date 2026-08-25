package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/video-site/backend/internal/catalog"
)

func TestVideoFeedUsesAnIdempotentSnapshotCursor(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	baseTime := time.Now().Add(-time.Hour)
	for index := 0; index < 7; index++ {
		id := "listing-" + strconv.Itoa(index)
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID:          id,
			DriveID:     "drive",
			FileID:      "file-" + id,
			Title:       id,
			PublishedAt: baseTime.Add(time.Duration(index) * time.Minute),
			CreatedAt:   baseTime,
			UpdatedAt:   baseTime,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	server := &Server{Catalog: cat}
	request := func(path string) videoFeedResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		server.handleVideoFeed(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
		var response videoFeedResponse
		if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return response
	}

	first := request("/api/feed?kind=listing&sort=latest&count=3")
	if first.FeedToken == "" || first.Total != 7 || first.NextCursor != 3 || first.Exhausted {
		t.Fatalf("first response = %#v", first)
	}
	shared := request("/api/feed?kind=latest&count=3")
	if shared.FeedToken != first.FeedToken {
		t.Fatalf("identical deterministic feed token = %q, want shared %q", shared.FeedToken, first.FeedToken)
	}
	if len(server.videoFeeds) != 1 {
		t.Fatalf("identical deterministic feeds stored %d snapshots, want 1", len(server.videoFeeds))
	}
	repeated := request("/api/feed?kind=listing&feedToken=" + first.FeedToken + "&cursor=0&count=3")
	if got, want := videoFeedIDs(repeated.Items), videoFeedIDs(first.Items); !equalStrings(got, want) {
		t.Fatalf("repeated batch = %v, want %v", got, want)
	}

	// A new video would move every live OFFSET page, but cannot enter the frozen
	// snapshot or displace the next cursor range.
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID:          "new-after-snapshot",
		DriveID:     "drive",
		FileID:      "file-new",
		Title:       "new",
		PublishedAt: time.Now(),
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("insert after snapshot: %v", err)
	}
	fresh := request("/api/feed?kind=latest&count=3")
	if fresh.FeedToken == first.FeedToken || fresh.Total != 8 {
		t.Fatalf("changed deterministic feed reused stale snapshot: %#v", fresh)
	}

	stored := append([]string(nil), server.videoFeeds[first.FeedToken].videoIDs...)
	next := request("/api/feed?kind=listing&feedToken=" + first.FeedToken + "&cursor=3&count=3")
	if got, want := videoFeedIDs(next.Items), stored[3:6]; !equalStrings(got, want) {
		t.Fatalf("next batch = %v, want frozen range %v", got, want)
	}
	for _, item := range next.Items {
		if item.ID == "new-after-snapshot" {
			t.Fatal("post-snapshot video leaked into an existing feed")
		}
	}
}

func TestVideoFeedAvoidsCompletedSnapshotsAndKeepsRecommendationsPrivate(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	for index := 0; index < 5; index++ {
		id := "video-" + strconv.Itoa(index)
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID: id, DriveID: "drive", FileID: "file-" + id, Title: id,
			ThumbnailURL: "/p/thumb/" + id,
			Badges:       []string{"new"},
			PublishedAt:  now.Add(time.Duration(index) * time.Minute),
			CreatedAt:    now,
			UpdatedAt:    now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	server := &Server{Catalog: cat}
	completed := requestVideoFeed(t, server, "/api/feed?kind=listing&count=5")
	if completed.FeedToken != "" || !completed.Exhausted || completed.NextCursor != 5 {
		t.Fatalf("completed first batch = %#v", completed)
	}
	if len(server.videoFeeds) != 0 {
		t.Fatalf("completed first batch retained %d snapshots, want 0", len(server.videoFeeds))
	}
	encoded, err := json.Marshal(completed.Items[0])
	if err != nil {
		t.Fatalf("encode compact item: %v", err)
	}
	for _, unused := range []string{"tags", "likes", "comments", "description", "fileId"} {
		if strings.Contains(string(encoded), `"`+unused+`"`) {
			t.Fatalf("compact feed item still contains %q: %s", unused, encoded)
		}
	}

	firstRandom := requestVideoFeed(t, server, "/api/feed?kind=recommend&count=2")
	secondRandom := requestVideoFeed(t, server, "/api/feed?kind=recommend&count=2")
	if firstRandom.FeedToken == "" || secondRandom.FeedToken == "" || firstRandom.FeedToken == secondRandom.FeedToken {
		t.Fatalf("random recommendation tokens must stay private: %q / %q", firstRandom.FeedToken, secondRandom.FeedToken)
	}
	if len(server.videoFeeds) != 2 {
		t.Fatalf("random recommendations stored %d snapshots, want 2", len(server.videoFeeds))
	}
}

func TestVideoFeedSharesDeterministicSnapshotConcurrently(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	for index := 0; index < 10; index++ {
		id := "shared-" + strconv.Itoa(index)
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID: id, DriveID: "drive", FileID: "file-" + id, Title: id,
			PublishedAt: now.Add(time.Duration(index) * time.Minute),
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	server := &Server{Catalog: cat}
	const clients = 16
	tokens := make(chan string, clients)
	errors := make(chan error, clients)
	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(clients)
	for range clients {
		go func() {
			defer wait.Done()
			<-start
			req := httptest.NewRequest(http.MethodGet, "/api/feed?kind=latest&count=2", nil)
			rr := httptest.NewRecorder()
			server.handleVideoFeed(rr, req)
			if rr.Code != http.StatusOK {
				errors <- fmt.Errorf("status = %d, body = %s", rr.Code, rr.Body.String())
				return
			}
			var response videoFeedResponse
			if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
				errors <- err
				return
			}
			tokens <- response.FeedToken
		}()
	}
	close(start)
	wait.Wait()
	close(tokens)
	close(errors)
	for err := range errors {
		t.Fatalf("concurrent feed request: %v", err)
	}

	var sharedToken string
	for token := range tokens {
		if token == "" {
			t.Fatal("concurrent feed returned an empty continuation token")
		}
		if sharedToken == "" {
			sharedToken = token
			continue
		}
		if token != sharedToken {
			t.Fatalf("concurrent deterministic token = %q, want %q", token, sharedToken)
		}
	}
	if len(server.videoFeeds) != 1 {
		t.Fatalf("concurrent deterministic feeds stored %d snapshots, want 1", len(server.videoFeeds))
	}
}

func TestVideoFeedLookupSizeCoversDeepRestoreInOneChunk(t *testing.T) {
	if got := videoFeedLookupSize(maxVideoFeedBatchSize); got != maxVideoFeedBatchSize {
		t.Fatalf("deep restore lookup size = %d, want %d", got, maxVideoFeedBatchSize)
	}
	if got := videoFeedLookupSize(12); got != videoFeedLookupChunk {
		t.Fatalf("small batch lookup size = %d, want %d", got, videoFeedLookupChunk)
	}
}

func TestVideoFeedSkipsRemovedSnapshotRowsAndExpires(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	for index := 0; index < 3; index++ {
		id := "visible-" + strconv.Itoa(index)
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID: id, DriveID: "drive", FileID: "file-" + id, Title: id,
			PublishedAt: now.Add(time.Duration(index) * time.Minute),
			CreatedAt:   now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	clock := now
	server := &Server{Catalog: cat, videoFeedNow: func() time.Time { return clock }}
	firstReq := httptest.NewRequest(http.MethodGet, "/api/feed?kind=latest&count=1", nil)
	firstRR := httptest.NewRecorder()
	server.handleVideoFeed(firstRR, firstReq)
	if firstRR.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", firstRR.Code, firstRR.Body.String())
	}
	var first videoFeedResponse
	if err := json.NewDecoder(firstRR.Body).Decode(&first); err != nil {
		t.Fatalf("decode first: %v", err)
	}

	removedID := server.videoFeeds[first.FeedToken].videoIDs[first.NextCursor]
	if err := cat.HideVideo(ctx, removedID); err != nil {
		t.Fatalf("hide snapshot row: %v", err)
	}
	nextReq := httptest.NewRequest(
		http.MethodGet,
		"/api/feed?feedToken="+first.FeedToken+"&cursor="+strconv.Itoa(first.NextCursor)+"&count=1",
		nil,
	)
	nextRR := httptest.NewRecorder()
	server.handleVideoFeed(nextRR, nextReq)
	if nextRR.Code != http.StatusOK {
		t.Fatalf("next status = %d, body = %s", nextRR.Code, nextRR.Body.String())
	}
	var next videoFeedResponse
	if err := json.NewDecoder(nextRR.Body).Decode(&next); err != nil {
		t.Fatalf("decode next: %v", err)
	}
	if len(next.Items) != 1 || next.Items[0].ID == removedID || next.NextCursor <= first.NextCursor+1 {
		t.Fatalf("next response did not skip removed row: %#v", next)
	}

	clock = clock.Add(videoFeedTTL)
	expiredReq := httptest.NewRequest(
		http.MethodGet,
		"/api/feed?feedToken="+first.FeedToken+"&cursor="+strconv.Itoa(next.NextCursor)+"&count=1",
		nil,
	)
	expiredRR := httptest.NewRecorder()
	server.handleVideoFeed(expiredRR, expiredReq)
	if expiredRR.Code != http.StatusGone {
		t.Fatalf("expired status = %d, want %d", expiredRR.Code, http.StatusGone)
	}
}

func requestVideoFeed(t *testing.T, server *Server, path string) videoFeedResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	server.handleVideoFeed(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var response videoFeedResponse
	if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return response
}

func videoFeedIDs(items []VideoCardDTO) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
