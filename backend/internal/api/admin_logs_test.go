package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/auth"
	"github.com/nianzhibai/91/backend/internal/catalog"
)

func newAdminLogsTestStore(t *testing.T) *applog.Store {
	t.Helper()
	store, err := applog.Open(applog.StoreConfig{
		Directory:         t.TempDir(),
		MaxTotalSizeBytes: 4 << 20,
		RetentionDays:     14,
		LevelFunc:         func() applog.Level { return applog.LevelInfo },
	})
	if err != nil {
		t.Fatalf("open log store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func appendAdminLog(t *testing.T, store *applog.Store, rec applog.Record) int64 {
	t.Helper()
	id, err := store.Append(context.Background(), rec)
	if err != nil {
		t.Fatalf("append log record: %v", err)
	}
	return id
}

func TestHandleLogsSupportsMultiFieldFiltersAndPagination(t *testing.T) {
	store := newAdminLogsTestStore(t)
	appendAdminLog(t, store, applog.Record{Source: applog.SourceApp, Level: applog.LevelInfo, Component: "scan", DriveID: "d1", Message: "worker ready"})
	appendAdminLog(t, store, applog.Record{Source: applog.SourceApp, Level: applog.LevelError, Component: "scan", DriveID: "d1", Message: "worker failed: first"})
	appendAdminLog(t, store, applog.Record{
		Source: applog.SourceHTTP,
		Level:  applog.LevelError,
		HTTP: &applog.HTTPRecord{
			Method:     applog.MethodPOST,
			Path:       "/api/drives",
			Status:     502,
			Bytes:      9,
			DurationMs: 1200,
			Remote:     "127.0.0.1",
		},
	})
	appendAdminLog(t, store, applog.Record{Source: applog.SourceApp, Level: applog.LevelError, Component: "scan", DriveID: "d1", Message: "worker failed: latest"})

	req := httptest.NewRequest(http.MethodGet, "/admin/api/logs?source=app&level=error&component=scan&driveId=d1&q=worker&limit=1", nil)
	rr := httptest.NewRecorder()
	(&AdminServer{Logs: store}).handleLogs(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	var snap applog.Snapshot
	if err := json.NewDecoder(rr.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if !snap.HasMore || len(snap.Entries) != 1 || snap.Entries[0].Message != "worker failed: latest" || snap.NextBefore == 0 {
		t.Fatalf("snapshot = %+v", snap)
	}

	// Filter HTTP by status class and minDurationMs.
	httpReq := httptest.NewRequest(http.MethodGet, "/admin/api/logs?source=http&method=POST&status=5xx&minDurationMs=1000", nil)
	httpRR := httptest.NewRecorder()
	(&AdminServer{Logs: store}).handleLogs(httpRR, httpReq)
	if httpRR.Code != http.StatusOK {
		t.Fatalf("http filter status = %d body=%s", httpRR.Code, httpRR.Body.String())
	}
	var httpSnap applog.Snapshot
	if err := json.NewDecoder(httpRR.Body).Decode(&httpSnap); err != nil {
		t.Fatal(err)
	}
	if len(httpSnap.Entries) != 1 || httpSnap.Entries[0].HTTP == nil || httpSnap.Entries[0].HTTP.Status != 502 {
		t.Fatalf("http snapshot = %+v", httpSnap)
	}
}

func TestHandleLogsMetaAndClear(t *testing.T) {
	store := newAdminLogsTestStore(t)
	appendAdminLog(t, store, applog.Record{Level: applog.LevelInfo, Component: "preview", Message: "ready"})

	metaRR := httptest.NewRecorder()
	(&AdminServer{Logs: store}).handleLogsMeta(metaRR, httptest.NewRequest(http.MethodGet, "/admin/api/logs/meta", nil))
	if metaRR.Code != http.StatusOK {
		t.Fatalf("meta status = %d body=%s", metaRR.Code, metaRR.Body.String())
	}
	var meta applog.Meta
	if err := json.NewDecoder(metaRR.Body).Decode(&meta); err != nil {
		t.Fatal(err)
	}
	if meta.TotalCount != 1 || meta.Level != applog.LevelInfo || len(meta.Components) != 1 || meta.Components[0] != "preview" {
		t.Fatalf("meta = %+v", meta)
	}

	clearRR := httptest.NewRecorder()
	(&AdminServer{Logs: store}).handleClearLogs(clearRR, httptest.NewRequest(http.MethodDelete, "/admin/api/logs", nil))
	if clearRR.Code != http.StatusOK {
		t.Fatalf("clear status = %d body=%s", clearRR.Code, clearRR.Body.String())
	}
	after, err := store.Query(context.Background(), applog.Query{Limit: 10})
	if err != nil || len(after.Entries) != 0 {
		t.Fatalf("after clear = %+v, err=%v", after, err)
	}
}

func TestHandleExportLogsSupportsTextAndJSONL(t *testing.T) {
	store := newAdminLogsTestStore(t)
	for i := 0; i < 5; i++ {
		appendAdminLog(t, store, applog.Record{Level: applog.LevelError, Component: "scan", Message: "retained error"})
	}
	server := &AdminServer{Logs: store}

	textRR := httptest.NewRecorder()
	server.handleExportLogs(textRR, httptest.NewRequest(http.MethodGet, "/admin/api/logs/export?level=error&format=text", nil))
	if textRR.Code != http.StatusOK || strings.Count(textRR.Body.String(), "retained error") != 5 {
		t.Fatalf("text export status=%d body=%s", textRR.Code, textRR.Body.String())
	}
	if !strings.Contains(textRR.Header().Get("Content-Disposition"), "runtime-logs.log") {
		t.Fatalf("content-disposition = %q", textRR.Header().Get("Content-Disposition"))
	}

	jsonlRR := httptest.NewRecorder()
	server.handleExportLogs(jsonlRR, httptest.NewRequest(http.MethodGet, "/admin/api/logs/export?format=jsonl", nil))
	if jsonlRR.Code != http.StatusOK || !strings.Contains(jsonlRR.Header().Get("Content-Disposition"), "runtime-logs.jsonl") {
		t.Fatalf("jsonl export status=%d hdr=%q", jsonlRR.Code, jsonlRR.Header().Get("Content-Disposition"))
	}
	lines := strings.Split(strings.TrimSpace(jsonlRR.Body.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("jsonl lines = %d, want 5", len(lines))
	}
}

func TestHandleLogStreamResumesFromLastEventIDAndStreamsLive(t *testing.T) {
	store := newAdminLogsTestStore(t)
	id1 := appendAdminLog(t, store, applog.Record{Level: applog.LevelError, Component: "scan", Message: "first"})
	id2 := appendAdminLog(t, store, applog.Record{Level: applog.LevelError, Component: "scan", Message: "catch-up"})

	server := httptest.NewServer(http.HandlerFunc((&AdminServer{Logs: store}).handleLogStream))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"?level=error&component=scan", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Last-Event-ID", strings.TrimSpace(json.Number(string(rune('0'+id1))).String()))
	// Use explicit strconv formatting for clarity.
	req.Header.Set("Last-Event-ID", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") || resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("stream response status=%d headers=%v", resp.StatusCode, resp.Header)
	}

	// Emit a live event after connecting.
	go func() {
		time.Sleep(50 * time.Millisecond)
		store.Submit(applog.Record{Level: applog.LevelInfo, Component: "scan", Message: "filtered out"})
		store.Submit(applog.Record{Level: applog.LevelError, Component: "scan", Message: "live-event"})
	}()

	scanner := bufio.NewScanner(resp.Body)
	var received []applog.Record
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			var rec applog.Record
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &rec); err != nil {
				t.Fatal(err)
			}
			received = append(received, rec)
			if len(received) == 2 {
				break
			}
		}
	}
	if len(received) != 2 || received[0].ID != id2 || received[0].Message != "catch-up" || received[1].Message != "live-event" {
		t.Fatalf("received SSE events = %+v", received)
	}
}

func TestHandleLogStreamFlushesInitialCommentImmediately(t *testing.T) {
	store := newAdminLogsTestStore(t)
	server := httptest.NewServer(http.HandlerFunc((&AdminServer{Logs: store}).handleLogStream))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	if !scanner.Scan() {
		t.Fatalf("expected initial SSE comment frame, err=%v", scanner.Err())
	}
	if got := scanner.Text(); got != ": connected" {
		t.Fatalf("first SSE line = %q, want %q", got, ": connected")
	}
}

func TestHandleLogsValidatesQueryAndUnavailableStore(t *testing.T) {
	rr := httptest.NewRecorder()
	(&AdminServer{}).handleLogs(rr, httptest.NewRequest(http.MethodGet, "/admin/api/logs", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable status = %d", rr.Code)
	}

	store := newAdminLogsTestStore(t)
	for _, suffix := range []string{
		"?limit=0",
		"?limit=5001",
		"?before=0",
		"?after=-1",
		"?before=5&after=1",
		"?source=filesystem",
		"?level=verbose",
		"?method=TRACE",
		"?status=999",
		"?minDurationMs=-1",
		"?from=invalid",
		"?from=2026-09-10T00:00:00Z&to=2026-09-09T00:00:00Z",
		"?q=" + strings.Repeat("x", 201),
	} {
		t.Run(suffix, func(t *testing.T) {
			rec := httptest.NewRecorder()
			(&AdminServer{Logs: store}).handleLogs(rec, httptest.NewRequest(http.MethodGet, "/admin/api/logs"+suffix, nil))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRegisteredLogsRoutesRequireAdministratorSession(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	router := chi.NewRouter()
	(&AdminServer{
		Catalog: cat,
		Auth:    &auth.Authenticator{Catalog: cat},
		Logs:    newAdminLogsTestStore(t),
	}).Register(router)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/admin/api/logs"},
		{http.MethodGet, "/admin/api/logs/meta"},
		{http.MethodGet, "/admin/api/logs/export"},
		{http.MethodGet, "/admin/api/logs/stream"},
		{http.MethodDelete, "/admin/api/logs"},
	} {
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d, want 401", tc.method, tc.path, rr.Code)
		}
	}

	adminID, err := cat.CreateUser(context.Background(), "operator", "unused-hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.CreateSession(context.Background(), "admin-session", time.Hour, adminID); err != nil {
		t.Fatal(err)
	}
	adminReq := httptest.NewRequest(http.MethodGet, "/admin/api/logs", nil)
	adminReq.AddCookie(&http.Cookie{Name: "vs_admin", Value: "admin-session"})
	adminRR := httptest.NewRecorder()
	router.ServeHTTP(adminRR, adminReq)
	if adminRR.Code != http.StatusOK {
		t.Fatalf("admin status = %d body=%s", adminRR.Code, adminRR.Body.String())
	}
}

func TestWriteErrRecordsCauseAndRequestIdentity(t *testing.T) {
	rt, err := applog.OpenRuntime(applog.RuntimeConfig{
		Directory:     t.TempDir(),
		Level:         applog.LevelInfo,
		ConsoleWriter: &bytes.Buffer{},
		FileEnabled:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	prev := slog.Default()
	slog.SetDefault(rt.Logger())
	defer slog.SetDefault(prev)

	r := httptest.NewRequest(http.MethodGet, "/admin/api/drives", nil)
	r = r.WithContext(applog.WithFields(r.Context(), applog.Fields{RequestID: "request-1"}))
	writeErr(httptest.NewRecorder(), r, http.StatusInternalServerError, errors.New("database is locked"))
	result, err := rt.Store().Query(context.Background(), applog.Query{Levels: []applog.Level{applog.LevelError}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 1 || result.Entries[0].Error != "database is locked" || result.Entries[0].RequestID != "request-1" {
		t.Fatalf("result=%+v", result)
	}
}
