package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/nianzhibai/91/backend/internal/applog"
)

func installTestLogRuntime(t *testing.T) (*applog.Runtime, *bytes.Buffer) {
	t.Helper()
	var console bytes.Buffer
	rt, err := applog.OpenRuntime(applog.RuntimeConfig{
		Directory:         t.TempDir(),
		Level:             applog.LevelInfo,
		ConsoleFormat:     applog.ConsoleFormatText,
		ConsoleWriter:     &console,
		FileEnabled:       true,
		MaxTotalSizeBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	prev := slog.Default()
	slog.SetDefault(rt.Logger())
	t.Cleanup(func() { slog.SetDefault(prev) })
	return rt, &console
}

func TestRequestLogMiddlewareCapturesRequestsAndHonorsQuietRoutes(t *testing.T) {
	rt, console := installTestLogRuntime(t)

	router := chi.NewRouter()
	router.Use(requestLogMiddleware())
	router.Use(middleware.Recoverer)
	router.Get("/api/list", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	router.With(applog.QuietRequestLog).Get("/admin/api/logs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	router.Get("/api/panic", func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	req := httptest.NewRequest(http.MethodGet, "/api/list", nil)
	req.RemoteAddr = "127.0.0.1:43210"
	req.Header.Set("X-Forwarded-For", "203.0.113.25")
	router.ServeHTTP(httptest.NewRecorder(), req)

	if got := console.String(); !strings.Contains(got, "GET /api/list 204") || !strings.Contains(got, "remote=203.0.113.25") || strings.Contains(got, "127.0.0.1") {
		t.Fatalf("console access log = %q", got)
	}
	snap, err := rt.Store().Query(context.Background(), applog.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Entries) != 1 || snap.Entries[0].Source != applog.SourceHTTP || snap.Entries[0].HTTP == nil {
		t.Fatalf("stored access logs = %+v", snap.Entries)
	}
	if h := snap.Entries[0].HTTP; h.Method != applog.MethodGET || h.Status != http.StatusNoContent || h.Path != "/api/list" || h.Remote != "203.0.113.25" {
		t.Fatalf("structured HTTP record = %+v", h)
	}

	console.Reset()
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/admin/api/logs?limit=200", nil))
	if console.Len() != 0 {
		t.Fatalf("quiet viewer request was logged: %q", console.String())
	}
	afterPoll, err := rt.Store().Query(context.Background(), applog.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(afterPoll.Entries) != 1 {
		t.Fatalf("quiet viewer request entered store: %+v", afterPoll.Entries)
	}

	panicRR := httptest.NewRecorder()
	router.ServeHTTP(panicRR, httptest.NewRequest(http.MethodGet, "/api/panic", nil))
	if panicRR.Code != http.StatusInternalServerError {
		t.Fatalf("panic status = %d", panicRR.Code)
	}
	panicLogs, err := rt.Store().Query(context.Background(), applog.Query{Source: applog.SourceApp, Levels: []applog.Level{applog.LevelError}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(panicLogs.Entries) != 1 || !strings.Contains(panicLogs.Entries[0].Message, "HTTP panic: boom") ||
		!strings.Contains(panicLogs.Entries[0].Stack, "goroutine") || panicLogs.Entries[0].RequestID != panicRR.Header().Get("X-Request-ID") {
		t.Fatalf("stored panic logs = %+v", panicLogs.Entries)
	}
}

func TestRequestLogsCarryGeneratedIDsAndRedactCredentials(t *testing.T) {
	rt, console := installTestLogRuntime(t)
	var requestID string
	handler := requestLogMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	result, err := rt.Store().Query(context.Background(), applog.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 1 || result.Entries[0].RequestID != requestID || result.Entries[0].HTTP == nil {
		t.Fatalf("result=%+v", result)
	}
	if strings.Contains(console.String()+result.Entries[0].Message+result.Entries[0].HTTP.Path, "private-") {
		t.Fatal("credential leaked")
	}
}
