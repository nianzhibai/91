package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/nianzhibai/91/backend/internal/applog"
)

func TestRequestLogMiddlewareCapturesRequestsButOmitsViewerPolling(t *testing.T) {
	var output bytes.Buffer
	var panicOutput bytes.Buffer
	store, err := applog.Open(applog.Config{
		Directory:         t.TempDir(),
		MaxLineBytes:      1024,
		MaxFileSizeBytes:  1 << 20,
		MaxTotalSizeBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	logger := log.New(&output, "", 0)
	panicLogger := log.New(io.MultiWriter(&panicOutput, store.Writer(applog.SourceApplication)), "", 0)
	handler := requestLogMiddleware(logger, panicLogger, store)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodGet, "/api/list", nil)
	request.RemoteAddr = "127.0.0.1:43210"
	request.Header.Set("X-Forwarded-For", "203.0.113.25")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if got := output.String(); !strings.Contains(got, `"GET http://example.com/api/list HTTP/1.1"`) {
		t.Fatalf("access log = %q", got)
	}
	if got := output.String(); !strings.Contains(got, "from 203.0.113.25 -") || strings.Contains(got, "127.0.0.1") {
		t.Fatalf("access log did not use forwarded client IP: %q", got)
	}
	beforePoll, err := store.Query(context.Background(), applog.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(beforePoll.Entries) != 1 || beforePoll.Entries[0].Source != applog.SourceHTTP {
		t.Fatalf("stored access logs = %#v", beforePoll.Entries)
	}
	if entry := beforePoll.Entries[0]; entry.Method != applog.MethodGET || entry.Status != http.StatusNoContent || entry.Path != "/api/list" || entry.Remote != "203.0.113.25" {
		t.Fatalf("structured access log = %#v", entry)
	}

	output.Reset()
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/admin/api/logs?limit=500", nil))
	if output.Len() != 0 {
		t.Fatalf("viewer request was logged: %q", output.String())
	}
	afterPoll, err := store.Query(context.Background(), applog.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(afterPoll.Entries) != len(beforePoll.Entries) || afterPoll.Entries[len(afterPoll.Entries)-1].ID != beforePoll.Entries[len(beforePoll.Entries)-1].ID {
		t.Fatalf("viewer request entered the store: before=%#v after=%#v", beforePoll.Entries, afterPoll.Entries)
	}

	panicHandler := requestLogMiddleware(logger, panicLogger, store)(middleware.Recoverer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) { panic("boom") },
	)))
	panicRR := httptest.NewRecorder()
	panicHandler.ServeHTTP(panicRR, httptest.NewRequest(http.MethodGet, "/api/panic", nil))
	if panicRR.Code != http.StatusInternalServerError {
		t.Fatalf("panic status = %d", panicRR.Code)
	}
	if got := panicOutput.String(); !strings.Contains(got, "[http] panic: boom") || !strings.Contains(got, "goroutine") {
		t.Fatalf("panic log = %q", got)
	}
	panicLogs, err := store.Query(context.Background(), applog.Query{Source: applog.SourceApplication, Search: "panic: boom", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(panicLogs.Entries) == 0 || !strings.Contains(panicLogs.Entries[0].Message, "[http] panic: boom") {
		t.Fatalf("stored panic logs = %#v", panicLogs.Entries)
	}
}

func TestRequestLogsCarryGeneratedIDsAndRedactCredentials(t *testing.T) {
	store, err := applog.Open(applog.Config{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var output bytes.Buffer
	logger := log.New(&output, "", 0)
	var requestID string
	handler := requestLogMiddleware(logger, logger, store)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID = applog.ContextFields(r.Context()).RequestID
		if r.URL.Query().Get("deviceCode") != "private-code" {
			t.Error("request parameters were modified")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/status?deviceCode=private-code&loginUuid=private-uuid&dir=42", nil)
	req.Header.Set("X-Request-ID", "untrusted-client-value")
	handler.ServeHTTP(rr, req)
	if requestID == "" || requestID == "untrusted-client-value" || rr.Header().Get("X-Request-ID") != requestID {
		t.Fatal("request identity was not generated and propagated")
	}
	result, err := store.Query(context.Background(), applog.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 1 || result.Entries[0].RequestID != requestID {
		t.Fatalf("result=%+v", result)
	}
	if strings.Contains(output.String()+result.Entries[0].Message+result.Entries[0].Path, "private-") {
		t.Fatal("credential leaked")
	}
}

func TestPanicStackIsOneErrorEventWithRequestContext(t *testing.T) {
	store, err := applog.Open(applog.Config{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	logger := log.New(store.Writer(applog.SourceApplication), "", 0)
	handler := requestLogMiddleware(log.New(io.Discard, "", 0), logger, store)(middleware.Recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("test panic") })))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/panic", nil))
	result, err := store.Query(context.Background(), applog.Query{Source: applog.SourceApplication, Level: applog.LevelError, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 1 || !strings.Contains(result.Entries[0].Stack, "goroutine") || result.Entries[0].RequestID != rr.Header().Get("X-Request-ID") {
		t.Fatalf("result=%+v", result)
	}
}
