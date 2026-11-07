package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/uploadjob"
)

func TestManualCrawlerUploadDoesNotGateOnHistoricalAssets(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if err := cat.UpsertDrive(ctx, &catalog.Drive{ID: "crawler", Kind: "scriptcrawler", Credentials: map[string]string{"script_path": "/tmp/example.py", "upload_drive_id": "target"}}); err != nil {
		t.Fatal(err)
	}
	for _, driveID := range []string{"crawler", "old-target"} {
		if err := cat.UpsertVideo(ctx, &catalog.Video{ID: "scriptcrawler-crawler-" + driveID, DriveID: driveID, FileID: driveID + ".mp4", Size: 10, FingerprintStatus: "failed", PreviewStatus: "failed"}); err != nil {
			t.Fatal(err)
		}
	}
	called := 0
	a := &AdminServer{Catalog: cat, OnCrawlerUploadRequested: func(id string) (bool, string) {
		called++
		if id != "crawler" {
			t.Fatalf("id=%s", id)
		}
		return true, ""
	}}
	request := httptest.NewRequest(http.MethodPost, "/admin/api/crawlers/crawler/upload", nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("id", "crawler")
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
	w := httptest.NewRecorder()
	a.handleUploadCrawlerVideos(w, request)
	var response struct {
		Accepted bool `json:"accepted"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusAccepted || !response.Accepted || called != 1 {
		t.Fatalf("code=%d body=%s called=%d", w.Code, w.Body, called)
	}
	// Admission failures remain visible to the caller even though no upload
	// task exists; file-level diagnostics are supplied by the admitted worker.
	a.OnCrawlerUploadRequested = func(string) (bool, string) { return false, "上传目标不可用" }
	w = httptest.NewRecorder()
	a.handleUploadCrawlerVideos(w, request)
	var rejected struct {
		Accepted bool   `json:"accepted"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rejected); err != nil {
		t.Fatal(err)
	}
	if rejected.Accepted || rejected.Message != "上传目标不可用" {
		t.Fatalf("body=%s", w.Body)
	}
}

func TestCrawlerListExposesPersistedUploadOutcome(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if err := cat.UpsertDrive(ctx, &catalog.Drive{ID: "crawler", Kind: "scriptcrawler", Credentials: map[string]string{"script_path": "/tmp/example.py", "upload_drive_id": "target"}}); err != nil {
		t.Fatal(err)
	}
	r := uploadjob.Result{DriveID: "crawler", State: "failed", FailedCount: 1, FinishedAt: time.Now()}
	r.AddIssue(uploadjob.Issue{VideoID: "video", Reason: "missing_file", Message: "本地视频文件不存在"})
	if err := cat.SaveCrawlerUploadResult(ctx, r); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	(&AdminServer{Catalog: cat}).handleListCrawlers(w, httptest.NewRequest(http.MethodGet, "/admin/api/crawlers", nil))
	var response []crawlerDTO
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || len(response) != 1 || response[0].LastUploadResult == nil || response[0].LastUploadResult.Issues[0].Reason != "missing_file" {
		t.Fatalf("body=%s", w.Body)
	}
}
