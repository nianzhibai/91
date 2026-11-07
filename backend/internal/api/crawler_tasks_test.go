package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/crawljob"
)

func crawlerTaskRequest(method, path string) *http.Request {
	request := httptest.NewRequest(method, path, nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("id", "crawler")
	route.URLParams.Add("taskID", "accepted-task")
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
}
func TestCrawlerAcceptanceListAndDetailUseDurableTask(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	ctx := context.Background()
	if err := cat.UpsertDrive(ctx, &catalog.Drive{ID: "crawler", Kind: "scriptcrawler", Credentials: map[string]string{"script_path": "/unused.py"}}); err != nil {
		t.Fatal(err)
	}
	previous := crawljob.Result{TaskID: "previous", DriveID: "crawler", State: "completed", NewVideos: 2, AcceptedAt: time.Now().Add(-time.Minute)}
	if err := cat.CreateCrawlerTask(ctx, previous); err != nil {
		t.Fatal(err)
	}
	task := crawljob.Result{TaskID: "accepted-task", DriveID: "crawler", State: "queued", TargetNew: 3, AcceptedAt: time.Now()}
	server := &AdminServer{Catalog: cat, OnCrawlerRunRequested: func(ctx context.Context, id string) (string, error) {
		if id != "crawler" {
			t.Error(id)
		}
		return task.TaskID, cat.CreateCrawlerTask(ctx, task)
	}}
	response := httptest.NewRecorder()
	server.handleRunCrawler(response, crawlerTaskRequest(http.MethodPost, "/admin/api/crawlers/crawler/run"))
	if response.Code != http.StatusAccepted {
		t.Fatalf("%d %s", response.Code, response.Body)
	}
	var accepted struct {
		Accepted bool   `json:"accepted"`
		TaskID   string `json:"taskId"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if !accepted.Accepted || accepted.TaskID != task.TaskID {
		t.Fatalf("%+v", accepted)
	}
	if stored, err := cat.GetCrawlerTask(ctx, "crawler", accepted.TaskID); err != nil || stored.State != "queued" {
		t.Fatalf("acceptance without record: %+v %v", stored, err)
	}
	response = httptest.NewRecorder()
	server.handleListCrawlers(response, httptest.NewRequest(http.MethodGet, "/admin/api/crawlers", nil))
	var list []crawlerDTO
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].CurrentTask == nil || list[0].CurrentTask.TaskID != task.TaskID || list[0].LastCrawlResult == nil || list[0].LastCrawlResult.TaskID != "previous" {
		t.Fatalf("%s", response.Body)
	}
	response = httptest.NewRecorder()
	server.handleGetCrawlerTask(response, crawlerTaskRequest(http.MethodGet, "/admin/api/crawlers/crawler/tasks/accepted-task"))
	var detail struct {
		Task crawljob.Result `json:"task"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Task.TaskID != list[0].CurrentTask.TaskID || detail.Task.TargetNew != list[0].CurrentTask.TargetNew {
		t.Fatalf("list/detail mismatch: %+v", detail)
	}
	server.OnCrawlerRunRequested = func(context.Context, string) (string, error) { return "", errors.New("persistence failed") }
	response = httptest.NewRecorder()
	server.handleRunCrawler(response, crawlerTaskRequest(http.MethodPost, "/admin/api/crawlers/crawler/run"))
	if response.Code != http.StatusConflict {
		t.Fatalf("failed admission reported accepted: %d", response.Code)
	}
}
func TestCancelCrawlerTaskRejectsTerminalResult(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	task := crawljob.Result{TaskID: "accepted-task", DriveID: "crawler", State: "queued", AcceptedAt: time.Now()}
	if err := cat.CreateCrawlerTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := &AdminServer{Catalog: cat, OnCrawlerTaskCancel: func(string, string) bool { calls++; return true }}
	response := httptest.NewRecorder()
	server.handleCancelCrawlerTask(response, crawlerTaskRequest(http.MethodPost, "/cancel"))
	if response.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("%d calls=%d", response.Code, calls)
	}
	task.State = "canceled"
	if err := cat.SaveCrawlerTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	server.handleCancelCrawlerTask(response, crawlerTaskRequest(http.MethodPost, "/cancel"))
	if response.Code != http.StatusConflict || calls != 1 {
		t.Fatalf("terminal cancellation affected next task: %d calls=%d", response.Code, calls)
	}
}
