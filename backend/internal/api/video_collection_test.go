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

func TestVideoDetailAndCollectionUseSameNaturalDirectoryOrder(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	for _, item := range []struct {
		id       string
		fileName string
		title    string
	}{
		{id: "episode-10", fileName: "Lesson 10.mp4", title: "Lesson 10"},
		{id: "episode-2", fileName: "Lesson 2.mp4", title: "Lesson 2"},
		{id: "episode-1", fileName: "Lesson 1.mp4", title: "Lesson 1"},
	} {
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID:          item.id,
			DriveID:     "drive-a",
			FileID:      item.id,
			FileName:    item.fileName,
			ParentID:    "folder-a",
			DirName:     "Data Structures",
			Title:       item.title,
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatalf("seed %s: %v", item.id, err)
		}
	}

	server := &Server{Catalog: cat}
	detailRequest := requestWithVideoID(
		http.MethodGet,
		"/api/video/episode-2",
		"episode-2",
		strings.NewReader(""),
	)
	detailRecorder := httptest.NewRecorder()
	server.handleVideoDetail(detailRecorder, detailRequest)
	if detailRecorder.Code != http.StatusOK {
		t.Fatalf("detail status = %d, body = %s", detailRecorder.Code, detailRecorder.Body.String())
	}
	var detail VideoDetailDTO
	if err := json.NewDecoder(detailRecorder.Body).Decode(&detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.Collection == nil {
		t.Fatal("detail collection is nil")
	}
	if detail.Collection.Name != "Data Structures" || detail.Collection.Total != 3 || detail.Collection.CurrentIndex != 2 {
		t.Fatalf("detail collection = %#v", detail.Collection)
	}

	collectionRequest := requestWithVideoID(
		http.MethodGet,
		"/api/video/episode-2/collection",
		"episode-2",
		strings.NewReader(""),
	)
	collectionRecorder := httptest.NewRecorder()
	server.handleVideoCollection(collectionRecorder, collectionRequest)
	if collectionRecorder.Code != http.StatusOK {
		t.Fatalf("collection status = %d, body = %s", collectionRecorder.Code, collectionRecorder.Body.String())
	}
	if strings.Contains(collectionRecorder.Body.String(), `"previewSrc"`) {
		t.Fatalf("compact collection unexpectedly contains previewSrc: %s", collectionRecorder.Body.String())
	}
	var collection VideoCollectionDTO
	if err := json.NewDecoder(collectionRecorder.Body).Decode(&collection); err != nil {
		t.Fatalf("decode collection: %v", err)
	}
	if collection.Total != 3 || collection.CurrentIndex != 2 || len(collection.Items) != 3 {
		t.Fatalf("collection summary = total %d current %d items %d", collection.Total, collection.CurrentIndex, len(collection.Items))
	}
	want := []string{"episode-1", "episode-2", "episode-10"}
	for index, id := range want {
		if collection.Items[index].ID != id {
			t.Fatalf("collection item %d = %q, want %q", index, collection.Items[index].ID, id)
		}
	}

	previewRequest := requestWithVideoID(
		http.MethodGet,
		"/api/video/episode-2/collection?preview=1",
		"episode-2",
		strings.NewReader(""),
	)
	previewRecorder := httptest.NewRecorder()
	server.handleVideoCollection(previewRecorder, previewRequest)
	if previewRecorder.Code != http.StatusOK {
		t.Fatalf("preview collection status = %d, body = %s", previewRecorder.Code, previewRecorder.Body.String())
	}
	var previewCollection VideoCollectionDTO
	if err := json.NewDecoder(previewRecorder.Body).Decode(&previewCollection); err != nil {
		t.Fatalf("decode preview collection: %v", err)
	}
	for index, id := range want {
		if got := previewCollection.Items[index].PreviewSrc; got != "/p/preview/"+id {
			t.Fatalf("preview collection item %d preview = %q, want %q", index, got, "/p/preview/"+id)
		}
	}
}

func TestVideoDetailOmitsCollectionForVideoWithoutDirectory(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	for _, id := range []string{"loose-1", "loose-2"} {
		if err := cat.UpsertVideo(ctx, &catalog.Video{
			ID: id, DriveID: "drive-a", FileID: id, FileName: id + ".mp4",
			Title: id, PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	request := requestWithVideoID(http.MethodGet, "/api/video/loose-1", "loose-1", strings.NewReader(""))
	recorder := httptest.NewRecorder()
	(&Server{Catalog: cat}).handleVideoDetail(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var detail VideoDetailDTO
	if err := json.NewDecoder(recorder.Body).Decode(&detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.Collection != nil {
		t.Fatalf("collection = %#v, empty parent IDs must not be grouped", detail.Collection)
	}
}

func TestNaturalCompareOrdersEpisodeNumbers(t *testing.T) {
	for _, test := range []struct {
		left  string
		right string
		want  int
	}{
		{left: "episode 2", right: "episode 10", want: -1},
		{left: "episode 02", right: "episode 2", want: 1},
		{left: "Episode 10", right: "episode 10", want: 0},
		{left: "part 12a", right: "part 12b", want: -1},
	} {
		got := naturalCompare(test.left, test.right)
		if got < 0 {
			got = -1
		} else if got > 0 {
			got = 1
		}
		if got != test.want {
			t.Fatalf("naturalCompare(%q, %q) = %d, want %d", test.left, test.right, got, test.want)
		}
	}
}
