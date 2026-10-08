package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/video-site/backend/internal/catalog"
)

func TestVideoAPIsOmitRetiredDescription(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	now := time.Now()
	video := &catalog.Video{
		ID: "video-1", DriveID: localUploadDriveID, FileID: "original.mp4",
		Title: "Original title", PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatal(err)
	}
	server := &Server{Catalog: cat}

	t.Run("public detail", func(t *testing.T) {
		request := requestWithVideoID(http.MethodGet, "/api/video/video-1", video.ID, strings.NewReader(""))
		response := httptest.NewRecorder()
		server.handleVideoDetail(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		assertVideoHasNoDescription(t, response.Body.Bytes())
	})
	t.Run("admin update", func(t *testing.T) {
		request := requestWithVideoID(http.MethodPut, "/admin/api/videos/video-1", video.ID,
			strings.NewReader(`{"description":"Retired description","badges":["featured"]}`))
		response := httptest.NewRecorder()
		(&AdminServer{Catalog: cat}).handleUpdateVideo(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		assertVideoHasNoDescription(t, response.Body.Bytes())
		saved, err := cat.GetVideo(ctx, video.ID)
		if err != nil || len(saved.Badges) != 1 || saved.Badges[0] != "featured" {
			t.Fatalf("retained update failed: video=%#v error=%v", saved, err)
		}
	})
	t.Run("shared detail", func(t *testing.T) {
		encoded, err := json.Marshal(server.mapSharedVideoDetail(ctx, video, "share-1"))
		if err != nil {
			t.Fatal(err)
		}
		assertVideoHasNoDescription(t, encoded)
	})
}

func assertVideoHasNoDescription(t *testing.T, data []byte) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["description"]; exists {
		t.Fatalf("response contains retired description: %s", data)
	}
	if string(fields["title"]) != `"Original title"` {
		t.Fatalf("response lost video title: %s", data)
	}
}
