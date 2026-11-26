package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
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
			if err := cat.MarkInvalidSizeSources(ctx, "drive", []string{"hidden"}); err != nil {
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

func TestShortsRejectsCrossModeTokenReuse(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID: "v1", DriveID: "drive", FileID: "v1", Title: "v1", Likes: 3,
		PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	server := &Server{Catalog: cat}
	rr := httptest.NewRecorder()
	server.handleShortsNext(rr, httptest.NewRequest(http.MethodGet, "/api/shorts/next?mode=latest&count=1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("initial status = %d: %s", rr.Code, rr.Body.String())
	}
	var first shortsFeedResponse
	if err := json.NewDecoder(rr.Body).Decode(&first); err != nil {
		t.Fatal(err)
	}

	for _, otherMode := range []string{"hot", "recommend"} {
		for _, source := range []string{"memory", "persisted"} {
			target := server
			if source == "persisted" {
				target = &Server{Catalog: cat}
			}
			mismatch := httptest.NewRecorder()
			target.handleShortsNext(
				mismatch,
				httptest.NewRequest(http.MethodGet, "/api/shorts/next?mode="+otherMode+"&feedToken="+first.FeedToken, nil),
			)
			if mismatch.Code != http.StatusGone {
				t.Fatalf("%s/%s: status = %d, want 410", otherMode, source, mismatch.Code)
			}
		}
	}
}

func TestShortsRecommendLightweightSnapshotResumesAndExcludesInserts(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	now := time.Now()
	const count = 25
	expected := make(map[string]bool, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("rec-%03d", i)
		expected[id] = true
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID: id, DriveID: "drive", FileID: id, Title: id,
			PublishedAt: now.Add(-time.Duration(i) * time.Minute), CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}

	request := func(server *Server, path string) shortsFeedResponse {
		t.Helper()
		rr := httptest.NewRecorder()
		server.handleShortsNext(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
		}
		var resp shortsFeedResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	first := request(&Server{Catalog: cat}, "/api/shorts/next?mode=recommend&count=5")
	if first.Total != count || first.FeedToken == "" || len(first.Items) != 5 || first.RoundComplete {
		t.Fatalf("invalid first recommend batch: %#v", first)
	}

	// Verify the persisted snapshot uses the compact binary rowid blob (4 bytes/video + 8-byte header).
	persisted, _, err := cat.LoadShortsFeedSnapshot(ctx, first.FeedToken)
	if err != nil || persisted == nil || persisted.RowIDs == nil || len(persisted.VideoIDs) != 0 {
		t.Fatalf("expected compact binary rowid snapshot, got %#v (err=%v)", persisted, err)
	}
	if wantBytes := 8 + count*4; len(persisted.RowIDs.Blob) != wantBytes {
		t.Fatalf("binary blob size = %d, want %d", len(persisted.RowIDs.Blob), wantBytes)
	}

	// Insert a new video mid-round; it must not leak into the frozen round.
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID: "late-insert", DriveID: "drive", FileID: "late-insert", Title: "late",
		PublishedAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// Hide an already-viewed video from the first batch before resuming.
	// With slot-based rowid pagination (not SQL OFFSET), hiding an earlier item
	// must NEVER shift or skip the next unseen video.
	if err := cat.MarkInvalidSizeSources(ctx, "drive", []string{first.Items[1].ID}); err != nil {
		t.Fatalf("hide earlier viewed video: %v", err)
	}
	delete(expected, first.Items[1].ID)

	// Simulate restart with a fresh Server instance.
	restarted := &Server{Catalog: cat}
	seen := make(map[string]bool, count)
	var roundOrder []string
	current := first
	for {
		for _, item := range current.Items {
			if item.ID == "late-insert" {
				t.Fatal("newly inserted video leaked into active recommend round")
			}
			if seen[item.ID] {
				t.Fatalf("duplicate video %q in recommend round", item.ID)
			}
			seen[item.ID] = true
			roundOrder = append(roundOrder, item.ID)
		}
		if current.RoundComplete {
			break
		}
		current = request(
			restarted,
			"/api/shorts/next?mode=recommend&count=7&feedToken="+first.FeedToken+"&cursor="+strconv.Itoa(current.NextCursor),
		)
	}
	// All remaining visible videos (including the already-seen hidden one from first batch)
	// must be accounted for with ZERO skipped neighbors.
	if len(seen) != count {
		t.Fatalf("visited %d unique videos after hiding an earlier item, want %d (no video skipped)", len(seen), count)
	}
	for id := range expected {
		if !seen[id] {
			t.Fatalf("missing video %q from recommend round", id)
		}
	}

	// Replaying cursor=0 with the same token skips the newly hidden item cleanly
	// while keeping all remaining items in the exact same shuffled order.
	replayed := request(restarted, "/api/shorts/next?mode=recommend&count=4&feedToken="+first.FeedToken)
	wantReplay := []string{roundOrder[0], roundOrder[2], roundOrder[3], roundOrder[4]}
	for i, item := range replayed.Items {
		if item.ID != wantReplay[i] {
			t.Fatalf("replay[%d] = %q, want %q", i, item.ID, wantReplay[i])
		}
	}
}

func TestShortsRecommendShuffleAvoidsThreeGapPattern(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	now := time.Now()

	// Seed 2,000 sequential videos and verify Fisher-Yates produces a genuinely
	// varied distribution of adjacent rowid strides (never collapsing to <= 3 gaps).
	const total = 2000
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("seq-%04d", i)
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID: id, DriveID: "drive", FileID: id, Title: id,
			PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}

	snap, err := cat.CreateShortsRecommendSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Total() != total || snap.RowIDs == nil {
		t.Fatalf("unexpected snapshot: %#v", snap)
	}
	distinctGaps := make(map[int64]struct{})
	prev := snap.RowIDs.RowIDAt(0)
	for i := 1; i < snap.RowIDs.Count; i++ {
		curr := snap.RowIDs.RowIDAt(i)
		distinctGaps[curr-prev] = struct{}{}
		prev = curr
	}
	if len(distinctGaps) < 500 {
		t.Fatalf("distinct adjacent rowid gaps = %d, want genuine random spread (>= 500)", len(distinctGaps))
	}
}

func TestShortsHydratesVisitReactionsAndPrewarmValidatesPayload(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	now := time.Now()
	for _, id := range []string{"v1", "v2"} {
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID: id, DriveID: "drive", FileID: id, Title: id,
			PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	visitID := "shorts-visit-id-0123456789abcdef"
	if _, err := cat.SetVisitReaction(ctx, "v1", visitID, catalog.VideoReactionLike); err != nil {
		t.Fatal(err)
	}

	server := &Server{Catalog: cat}
	rr := httptest.NewRecorder()
	server.handleShortsNext(rr, httptest.NewRequest(http.MethodGet, "/api/shorts/next?mode=latest&count=5&visitId="+visitID, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var resp shortsFeedResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	reactions := map[string]catalog.VideoReaction{}
	for _, item := range resp.Items {
		reactions[item.ID] = item.Reaction
	}
	if reactions["v1"] != catalog.VideoReactionLike || reactions["v2"] != "" {
		t.Fatalf("hydrated reactions = %#v", reactions)
	}

	// Prewarm endpoint accepts valid payload and rejects malformed JSON.
	bad := httptest.NewRecorder()
	server.handleShortsPrewarm(bad, httptest.NewRequest(http.MethodPost, "/api/shorts/prewarm", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("empty body status = %d, want 400", bad.Code)
	}
	okRR := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/shorts/prewarm", strings.NewReader(`{"videoIds":["v1","v2"]}`))
	server.handleShortsPrewarm(okRR, req)
	if okRR.Code != http.StatusOK {
		t.Fatalf("prewarm status = %d: %s", okRR.Code, okRR.Body.String())
	}
}
