package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/video-site/backend/internal/auth"
	"github.com/video-site/backend/internal/catalog"
)

func openShortsSignalCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	return cat
}

func insertShortsSignalVideo(t *testing.T, cat *catalog.Catalog, id, parentID string, publishedAt time.Time) {
	t.Helper()
	if err := cat.UpsertVideo(context.Background(), &catalog.Video{
		ID:          id,
		DriveID:     "drive",
		FileID:      id,
		FileName:    id + ".mp4",
		ParentID:    parentID,
		Title:       id,
		PublishedAt: publishedAt,
		CreatedAt:   publishedAt,
		UpdatedAt:   publishedAt,
	}); err != nil {
		t.Fatal(err)
	}
}

func postShortsEvents(t *testing.T, server *Server, userID int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/shorts/events", strings.NewReader(body))
	if userID > 0 {
		request = request.WithContext(auth.WithUserID(request.Context(), userID))
	}
	recorder := httptest.NewRecorder()
	server.handleShortsEvents(recorder, request)
	return recorder
}

func requestShortsFeed(t *testing.T, server *Server, path string) shortsFeedResponse {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.handleShortsNext(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}
	var response shortsFeedResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	return response
}

func shortsFeedIDs(response shortsFeedResponse) []string {
	ids := make([]string, 0, len(response.Items))
	for _, item := range response.Items {
		ids = append(ids, item.ID)
	}
	return ids
}

func TestShortsEventsStoresSettledWatches(t *testing.T) {
	cat := openShortsSignalCatalog(t)
	insertShortsSignalVideo(t, cat, "v1", "dir", time.Now())
	server := &Server{Catalog: cat}

	recorder := postShortsEvents(t, server, 7,
		`{"events":[{"videoId":"v1","kind":"complete","watchMs":1200,"loopCount":1,"position":0}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Stored int `json:"stored"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Stored != 1 {
		t.Fatalf("stored = %d, want 1", response.Stored)
	}
}

func TestShortsEventsRequireAnAccount(t *testing.T) {
	cat := openShortsSignalCatalog(t)
	insertShortsSignalVideo(t, cat, "v1", "dir", time.Now())
	server := &Server{Catalog: cat}

	recorder := postShortsEvents(t, server, 0,
		`{"events":[{"videoId":"v1","kind":"complete"}]}`)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
}

func TestShortsEventsRejectUnknownKind(t *testing.T) {
	cat := openShortsSignalCatalog(t)
	insertShortsSignalVideo(t, cat, "v1", "dir", time.Now())
	server := &Server{Catalog: cat}

	recorder := postShortsEvents(t, server, 7,
		`{"events":[{"videoId":"v1","kind":"watched"}]}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

func TestShortsRecommendExtendsTheLastWatchedDirectory(t *testing.T) {
	cat := openShortsSignalCatalog(t)
	now := time.Now()
	for index := 0; index < 6; index++ {
		insertShortsSignalVideo(t, cat, fmt.Sprintf("a-%d", index), "dir-a", now.Add(-time.Duration(index)*time.Minute))
		insertShortsSignalVideo(t, cat, fmt.Sprintf("b-%d", index), "dir-b", now.Add(-time.Duration(index)*time.Minute))
	}
	server := &Server{Catalog: cat}

	response := requestShortsFeed(t, server, "/api/shorts/next?mode=recommend&count=5&after=a-1")
	ids := shortsFeedIDs(response)
	if len(ids) != 5 {
		t.Fatalf("batch = %v, want 5 items", ids)
	}
	fromDirectory := 0
	for _, id := range ids {
		if id == "a-1" {
			t.Fatalf("the video being watched came back in the same batch: %v", ids)
		}
		if strings.HasPrefix(id, "a-") {
			fromDirectory++
		}
	}
	if fromDirectory == 0 {
		t.Fatalf("no same-directory recommendation: %v", ids)
	}
	// 一批 5 条最多买 2 条同目录，否则推荐流会退化成目录连播。
	if fromDirectory > 2 {
		t.Fatalf("same-directory picks = %d, want at most 2: %v", fromDirectory, ids)
	}
}

func TestShortsRecommendReplaysTheSameCursor(t *testing.T) {
	cat := openShortsSignalCatalog(t)
	now := time.Now()
	for index := 0; index < 12; index++ {
		insertShortsSignalVideo(t, cat, fmt.Sprintf("video-%02d", index), "dir", now.Add(-time.Duration(index)*time.Minute))
	}
	server := &Server{Catalog: cat}

	first := requestShortsFeed(t, server, "/api/shorts/next?mode=recommend&count=3")
	if first.FeedToken == "" || first.NextCursor != 3 {
		t.Fatalf("invalid first batch: %#v", first)
	}
	replay := requestShortsFeed(t, server,
		"/api/shorts/next?mode=recommend&count=3&cursor=0&feedToken="+first.FeedToken)
	// 同一个 cursor 的重试必须重放同一批：重算会让前端的去重键错位。
	if !slices.Equal(shortsFeedIDs(first), shortsFeedIDs(replay)) {
		t.Fatalf("replay changed the batch: %v vs %v", shortsFeedIDs(first), shortsFeedIDs(replay))
	}

	next := requestShortsFeed(t, server, fmt.Sprintf(
		"/api/shorts/next?mode=recommend&count=3&cursor=%d&feedToken=%s",
		first.NextCursor, first.FeedToken,
	))
	if next.NextCursor <= first.NextCursor {
		t.Fatalf("cursor did not advance: %d", next.NextCursor)
	}
	for _, id := range shortsFeedIDs(next) {
		if slices.Contains(shortsFeedIDs(first), id) {
			t.Fatalf("video %q was served twice in one session", id)
		}
	}
}

func TestShortsRecommendReportsEveryVisibleVideoAsTotal(t *testing.T) {
	cat := openShortsSignalCatalog(t)
	now := time.Now()
	for index := 0; index < 7; index++ {
		insertShortsSignalVideo(t, cat, fmt.Sprintf("video-%02d", index), "dir", now.Add(-time.Duration(index)*time.Minute))
	}
	hidden := &catalog.Video{
		ID: "hidden", DriveID: "drive", FileID: "hidden", FileName: "hidden.mp4",
		Title: "hidden", Hidden: true, PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := cat.UpsertVideo(context.Background(), hidden); err != nil {
		t.Fatal(err)
	}
	server := &Server{Catalog: cat}

	response := requestShortsFeed(t, server, "/api/shorts/next?mode=recommend&count=3")
	if response.Total != 7 {
		t.Fatalf("total = %d, want 7 visible videos", response.Total)
	}
	if response.RoundComplete {
		t.Fatal("a recommend batch is never the end of a round")
	}
}
