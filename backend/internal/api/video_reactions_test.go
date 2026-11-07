package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/nianzhibai/91/backend/internal/catalog"
)

func TestHandleSetVideoReactionReturnsAuthoritativeState(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	seedAPIReactionVideo(t, cat, "video-1")

	srv := &Server{Catalog: cat}
	router := chi.NewRouter()
	router.Put("/api/video/{id}/reaction", srv.handleSetVideoReaction)

	call := func(reaction string) VideoReactionResponse {
		t.Helper()
		body, err := json.Marshal(map[string]string{
			"visitId":  "visit-0000000000000001",
			"reaction": reaction,
		})
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		req := httptest.NewRequest(
			http.MethodPut,
			"/api/video/video-1/reaction",
			bytes.NewReader(body),
		)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("reaction %s status = %d, body = %s", reaction, rr.Code, rr.Body.String())
		}
		var got VideoReactionResponse
		if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return got
	}

	liked := call("like")
	if liked.Reaction != "like" || liked.Likes != 1 || liked.Dislikes != 0 {
		t.Fatalf("liked response = %#v", liked)
	}
	disliked := call("dislike")
	if disliked.Reaction != "dislike" || disliked.Likes != 0 || disliked.Dislikes != 1 {
		t.Fatalf("disliked response = %#v", disliked)
	}
	cleared := call("none")
	if cleared.Reaction != "none" || cleared.Likes != 0 || cleared.Dislikes != 0 {
		t.Fatalf("cleared response = %#v", cleared)
	}
}

func TestHandleSetVideoReactionValidatesRequest(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	seedAPIReactionVideo(t, cat, "video-1")

	srv := &Server{Catalog: cat}
	router := chi.NewRouter()
	router.Put("/api/video/{id}/reaction", srv.handleSetVideoReaction)

	tests := []struct {
		name string
		id   string
		body string
		want int
	}{
		{
			name: "malformed json",
			id:   "video-1",
			body: `{`,
			want: http.StatusBadRequest,
		},
		{
			name: "invalid visit id",
			id:   "video-1",
			body: `{"visitId":"short","reaction":"like"}`,
			want: http.StatusBadRequest,
		},
		{
			name: "invalid reaction",
			id:   "video-1",
			body: `{"visitId":"visit-0000000000000001","reaction":"love"}`,
			want: http.StatusBadRequest,
		},
		{
			name: "trailing json object",
			id:   "video-1",
			body: `{"visitId":"visit-0000000000000001","reaction":"like"}{}`,
			want: http.StatusBadRequest,
		},
		{
			name: "missing video",
			id:   "missing-video",
			body: `{"visitId":"visit-0000000000000001","reaction":"like"}`,
			want: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(
				http.MethodPut,
				"/api/video/"+tt.id+"/reaction",
				bytes.NewBufferString(tt.body),
			)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)
			if rr.Code != tt.want {
				t.Fatalf("status = %d, want %d; body = %s", rr.Code, tt.want, rr.Body.String())
			}
		})
	}
}

type VideoReactionResponse struct {
	Reaction string `json:"reaction"`
	Likes    int    `json:"likes"`
	Dislikes int    `json:"dislikes"`
}

func seedAPIReactionVideo(t *testing.T, cat *catalog.Catalog, id string) {
	t.Helper()
	now := time.Now()
	if err := cat.UpsertVideo(context.Background(), &catalog.Video{
		ID:          id,
		DriveID:     "drive-1",
		FileID:      "file-1",
		Title:       "API reaction test",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
}
