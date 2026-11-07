package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
)

func TestTelegramImportsUseTheSameDirectoryCollectionRules(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	for i, directory := range []string{"海边旅行-a1b2", "海边旅行-a1b2", "另一组-c3d4", "", ""} {
		id := "episode-" + strconv.Itoa(i+1)
		if err := cat.AcceptTelegramUpdate(ctx, catalog.TelegramReceipt{BotID: 123, ChatID: 42, SenderID: 42, MessageID: int64(i + 1), UpdateID: int64(i + 1)}, &catalog.TelegramSource{BotID: 123, SenderID: 42, FileID: id, UniqueID: id, Size: 5}, id, id, 100); err != nil {
			t.Fatal(err)
		}
		if _, err := cat.ReserveTelegramLocalFile(ctx, catalog.TelegramLocalFile{FileID: id + ".media", JobID: id, Directory: directory}); err != nil {
			t.Fatal(err)
		}
		if err := cat.TransitionRemoteUploadJob(ctx, id, catalog.RemoteUploadQueued, catalog.RemoteUploadSaving); err != nil {
			t.Fatal(err)
		}
		if err := cat.FinalizeRemoteUpload(ctx, id, &catalog.Video{ID: id, DriveID: catalog.TelegramLocalDriveID, FileID: id + ".media", FileName: id + ".mp4", Title: id, Size: 5}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{Catalog: cat}
	request := requestWithVideoID(http.MethodGet, "/api/video/episode-1/collection", "episode-1", strings.NewReader(""))
	response := httptest.NewRecorder()
	server.handleVideoCollection(response, request)
	var collection VideoCollectionDTO
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &collection) != nil {
		t.Fatalf("collection response: %d %s", response.Code, response.Body.String())
	}
	if collection.Name != "海边旅行-a1b2" || collection.Total != 2 || len(collection.Items) != 2 || collection.Items[0].ID != "episode-1" || collection.Items[1].ID != "episode-2" {
		t.Fatalf("Telegram directories were not respected by the collection API: %+v", collection)
	}
	request = requestWithVideoID(http.MethodGet, "/api/video/episode-1/collection/summary", "episode-1", strings.NewReader(""))
	response = httptest.NewRecorder()
	server.handleVideoCollectionSummary(response, request)
	var summary VideoCollectionSummary
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &summary) != nil || summary.Total != 2 || summary.Name != collection.Name {
		t.Fatalf("Telegram collection summary differs from the full collection: %s", response.Body.String())
	}
	request = requestWithVideoID(http.MethodGet, "/api/video/episode-4/collection", "episode-4", strings.NewReader(""))
	response = httptest.NewRecorder()
	server.handleVideoCollection(response, request)
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &collection) != nil || collection.Name != "library" || collection.Total != 2 || collection.Items[0].ID != "episode-4" || collection.Items[1].ID != "episode-5" {
		t.Fatalf("Telegram root directory did not form a collection: %s", response.Body.String())
	}
}

func TestVideoDetailDefersCollectionAndSummaryMatchesNaturalDirectoryOrder(t *testing.T) {
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
	if !detail.CollectionCandidate {
		t.Fatal("detail collection candidate is false")
	}

	summaryRequest := requestWithVideoID(
		http.MethodGet,
		"/api/video/episode-2/collection/summary",
		"episode-2",
		strings.NewReader(""),
	)
	summaryRecorder := httptest.NewRecorder()
	server.handleVideoCollectionSummary(summaryRecorder, summaryRequest)
	if summaryRecorder.Code != http.StatusOK {
		t.Fatalf("summary status = %d, body = %s", summaryRecorder.Code, summaryRecorder.Body.String())
	}
	var summary VideoCollectionSummary
	if err := json.NewDecoder(summaryRecorder.Body).Decode(&summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if summary.Name != "Data Structures" || summary.Total != 3 || summary.CurrentIndex != 2 {
		t.Fatalf("collection summary = %#v", summary)
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
	if detail.CollectionCandidate {
		t.Fatal("empty parent IDs must not be collection candidates")
	}

	summaryRequest := requestWithVideoID(http.MethodGet, "/api/video/loose-1/collection/summary", "loose-1", strings.NewReader(""))
	summaryRecorder := httptest.NewRecorder()
	(&Server{Catalog: cat}).handleVideoCollectionSummary(summaryRecorder, summaryRequest)
	if summaryRecorder.Code != http.StatusOK {
		t.Fatalf("summary status = %d, body = %s", summaryRecorder.Code, summaryRecorder.Body.String())
	}
	var summary VideoCollectionSummary
	if err := json.NewDecoder(summaryRecorder.Body).Decode(&summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if summary.Total != 0 || summary.CurrentIndex != 0 || summary.Name != "" {
		t.Fatalf("empty-directory summary = %#v", summary)
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
