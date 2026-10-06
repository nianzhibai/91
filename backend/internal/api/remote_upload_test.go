package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/video-site/backend/internal/auth"
	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/mediaimport"
)

type fakeRemoteUploadService struct {
	createInput mediaimport.CreateInput
	createJob   *catalog.RemoteUploadJob
	createErr   error
	listLimit   int
	listJobs    []*catalog.RemoteUploadJob
	listErr     error
	cancelID    string
	cancelJob   *catalog.RemoteUploadJob
	cancelErr   error
}

func (f *fakeRemoteUploadService) Create(
	_ context.Context,
	input mediaimport.CreateInput,
) (*catalog.RemoteUploadJob, error) {
	f.createInput = input
	return f.createJob, f.createErr
}

func (f *fakeRemoteUploadService) List(
	_ context.Context,
	limit int,
) ([]*catalog.RemoteUploadJob, error) {
	f.listLimit = limit
	return f.listJobs, f.listErr
}

func (f *fakeRemoteUploadService) Cancel(
	_ context.Context,
	id string,
) (*catalog.RemoteUploadJob, error) {
	f.cancelID = id
	return f.cancelJob, f.cancelErr
}

func TestCreateRemoteUploadReturnsAcceptedRedactedJob(t *testing.T) {
	cat := openRemoteUploadAPICatalog(t)
	if _, err := cat.CreateTagAndClassify(context.Background(), "奶子", "user"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	service := &fakeRemoteUploadService{
		createJob: &catalog.RemoteUploadJob{
			ID:              "remote-1",
			SourceURL:       "https://cdn.example/video.mp4?token=secret",
			SourceLabel:     "cdn.example/video.mp4",
			RequestedTitle:  "标题",
			Tags:            []string{"奶子"},
			State:           catalog.RemoteUploadQueued,
			CreatedAt:       now,
			UpdatedAt:       now,
			CancelRequested: false,
		},
	}
	server := &Server{Catalog: cat, RemoteUploads: service}
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/upload/remote",
		strings.NewReader(`{
			"url":"https://cdn.example/video.mp4?token=secret",
			"title":"标题",
			"tags":["奶子"]
		}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	server.handleCreateRemoteUpload(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if service.createInput.URL != "https://cdn.example/video.mp4?token=secret" ||
		service.createInput.Title != "标题" ||
		len(service.createInput.Tags) != 1 ||
		service.createInput.Tags[0] != "奶子" {
		t.Fatalf("create input = %#v", service.createInput)
	}
	body := rr.Body.String()
	if strings.Contains(body, "token") || strings.Contains(body, "secret") ||
		strings.Contains(body, "sourceURL") {
		t.Fatalf("response leaked source URL: %s", body)
	}
	var dto RemoteUploadJobDTO
	if err := json.Unmarshal(rr.Body.Bytes(), &dto); err != nil {
		t.Fatal(err)
	}
	if dto.ID != "remote-1" ||
		dto.State != catalog.RemoteUploadQueued ||
		dto.SourceLabel != "cdn.example/video.mp4" ||
		!dto.CanCancel {
		t.Fatalf("dto = %#v", dto)
	}
}

func TestCreateRemoteUploadRejectsInvalidJSONTagsAndOversizedBody(t *testing.T) {
	cat := openRemoteUploadAPICatalog(t)
	service := &fakeRemoteUploadService{}
	server := &Server{Catalog: cat, RemoteUploads: service}

	for name, body := range map[string]string{
		"unknown field":   `{"url":"https://example.com/a.mp4","extra":true}`,
		"unsupported tag": `{"url":"https://example.com/a.mp4","tags":["不存在"]}`,
		"trailing JSON":   `{"url":"https://example.com/a.mp4"} {}`,
		"oversized":       `{"url":"https://example.com/` + strings.Repeat("a", maxRemoteUploadRequestBytes) + `.mp4"}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/upload/remote", strings.NewReader(body))
			rr := httptest.NewRecorder()
			server.handleCreateRemoteUpload(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestListAndCancelRemoteUploads(t *testing.T) {
	now := time.Now()
	service := &fakeRemoteUploadService{
		listJobs: []*catalog.RemoteUploadJob{{
			ID:          "remote-1",
			SourceLabel: "cdn.example/video.mp4",
			State:       catalog.RemoteUploadDownloading,
			CreatedAt:   now,
			UpdatedAt:   now,
		}},
		cancelJob: &catalog.RemoteUploadJob{
			ID:          "remote-1",
			SourceLabel: "cdn.example/video.mp4",
			State:       catalog.RemoteUploadDownloading,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
	}
	server := &Server{RemoteUploads: service}

	listReq := httptest.NewRequest(http.MethodGet, "/api/upload/remote?limit=7", nil)
	listRR := httptest.NewRecorder()
	server.handleListRemoteUploads(listRR, listReq)
	if listRR.Code != http.StatusOK || service.listLimit != 7 {
		t.Fatalf("list status/limit = %d/%d body=%s", listRR.Code, service.listLimit, listRR.Body.String())
	}

	cancelReq := requestWithRouteParam(
		http.MethodPost,
		"/api/upload/remote/remote-1/cancel",
		"jobId",
		"remote-1",
		strings.NewReader(""),
	)
	cancelRR := httptest.NewRecorder()
	server.handleCancelRemoteUpload(cancelRR, cancelReq)
	if cancelRR.Code != http.StatusOK || service.cancelID != "remote-1" {
		t.Fatalf("cancel status/id = %d/%q body=%s", cancelRR.Code, service.cancelID, cancelRR.Body.String())
	}

	service.cancelErr = catalog.ErrRemoteUploadTerminal
	conflictRR := httptest.NewRecorder()
	server.handleCancelRemoteUpload(conflictRR, cancelReq)
	if conflictRR.Code != http.StatusConflict {
		t.Fatalf("terminal cancel status = %d body=%s", conflictRR.Code, conflictRR.Body.String())
	}
	service.cancelErr = sql.ErrNoRows
	missingRR := httptest.NewRecorder()
	server.handleCancelRemoteUpload(missingRR, cancelReq)
	if missingRR.Code != http.StatusNotFound {
		t.Fatalf("missing cancel status = %d body=%s", missingRR.Code, missingRR.Body.String())
	}
}

func TestUploadRoutesRequireAdminAuthentication(t *testing.T) {
	cat := openRemoteUploadAPICatalog(t)
	service := &fakeRemoteUploadService{}
	authenticator := &auth.Authenticator{Catalog: cat}
	router := chi.NewRouter()
	(&Server{Catalog: cat, RemoteUploads: service}).RegisterRoutes(router, authenticator)

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/upload/tags", nil),
		httptest.NewRequest(http.MethodPost, "/api/upload/remote", strings.NewReader(`{"url":"https://example.com/video.mp4"}`)),
		httptest.NewRequest(http.MethodGet, "/api/upload/remote", nil),
		httptest.NewRequest(http.MethodPost, "/api/upload/remote/remote-1/cancel", nil),
	} {
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, request)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d", request.Method, request.URL.Path, rr.Code)
		}
	}

	passwordHash, err := auth.HashPassword("viewer-secret")
	if err != nil {
		t.Fatal(err)
	}
	viewerID, err := cat.CreateUser(context.Background(), "viewer", passwordHash, "user")
	if err != nil {
		t.Fatal(err)
	}
	const token = "viewer-remote-upload-session"
	if err := cat.CreateSessionUntil(
		context.Background(),
		token,
		time.Now().Add(time.Hour),
		viewerID,
	); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/upload/tags", "/api/upload/remote"} {
		userRequest := httptest.NewRequest(http.MethodGet, path, nil)
		userRequest.AddCookie(&http.Cookie{Name: "vs_admin", Value: token})
		userRR := httptest.NewRecorder()
		router.ServeHTTP(userRR, userRequest)
		if userRR.Code != http.StatusForbidden {
			t.Fatalf("ordinary user %s status = %d, want 403", path, userRR.Code)
		}
	}
}

func TestRemoteUploadHandlerDoesNotExposeServiceErrors(t *testing.T) {
	cat := openRemoteUploadAPICatalog(t)
	service := &fakeRemoteUploadService{
		createErr: errors.New(`Get "https://cdn.example/video.mp4?token=secret": dial failed`),
	}
	server := &Server{Catalog: cat, RemoteUploads: service}
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/upload/remote",
		strings.NewReader(`{"url":"https://cdn.example/video.mp4?token=secret"}`),
	)
	rr := httptest.NewRecorder()
	server.handleCreateRemoteUpload(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "token") || strings.Contains(rr.Body.String(), "secret") {
		t.Fatalf("response leaked service error: %s", rr.Body.String())
	}
}

func openRemoteUploadAPICatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Errorf("close catalog: %v", err)
		}
	})
	return cat
}
