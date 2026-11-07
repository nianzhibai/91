package main

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/nianzhibai/91/backend/internal/applog"
)

func TestRequestLogPolicyFiltersBothOutputs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		method  string
		target  string
		pattern string
		status  int
		elapsed time.Duration
		level   applog.Level
	}{
		{name: "status query", method: "GET", target: "/admin/api/telegram/status", status: 200},
		{name: "Telegram configuration query", method: "GET", target: "/admin/api/telegram/availability", status: 200},
		{name: "public settings query", method: "GET", target: "/api/settings/preview", status: 200},
		{name: "import list query", method: "GET", target: "/admin/api/import-jobs?source=telegram", status: 200},
		{name: "backup list query", method: "GET", target: "/admin/api/backups", status: 200},
		{name: "POST scan status query", method: "POST", target: "/admin/api/drives/quark/qr/status", status: 200},
		{name: "mobile QR status query", method: "POST", target: "/admin/api/drives/yun139/qr/status", status: 200},
		{name: "viewer query", method: "GET", target: "/admin/api/logs?q=error", status: 200},
		{name: "query below slow threshold", method: "GET", target: "/admin/api/drives", status: 200, elapsed: time.Second - time.Nanosecond},
		{name: "slow query", method: "GET", target: "/admin/api/drives", status: 200, elapsed: time.Second, level: applog.LevelWarning},
		{name: "slow viewer query", method: "GET", target: "/admin/api/logs", status: 200, elapsed: 2 * time.Second, level: applog.LevelWarning},
		{name: "unauthorized viewer", method: "GET", target: "/admin/api/logs", status: 401, level: applog.LevelWarning},
		{name: "invalid query", method: "GET", target: "/admin/api/backups?limit=bad", status: 400, level: applog.LevelWarning},
		{name: "failed viewer", method: "GET", target: "/admin/api/logs", status: 503, level: applog.LevelError},
		{name: "failed POST query", method: "POST", target: "/admin/api/drives/p115/qr/status", status: 502, level: applog.LevelError},
		{name: "backup creation", method: "POST", target: "/admin/api/backups", status: 202, level: applog.LevelInfo},
		{name: "log deletion", method: "DELETE", target: "/admin/api/logs", status: 200, level: applog.LevelInfo},
		{name: "log download", method: "GET", target: "/admin/api/logs?download=1", status: 200, level: applog.LevelInfo},
		{name: "backup download", method: "GET", target: "/admin/api/backups/one/download", pattern: "/admin/api/backups/{id}/download", status: 200, elapsed: time.Minute, level: applog.LevelInfo},
		{name: "public browsing", method: "GET", target: "/api/videos", status: 200, level: applog.LevelInfo},
		{name: "unknown status endpoint", method: "GET", target: "/admin/api/example/status", status: 200, level: applog.LevelInfo},
		{name: "credential access", method: "GET", target: "/admin/api/drives/one/credentials", pattern: "/admin/api/drives/{id}/credentials", status: 200, level: applog.LevelInfo},
		{name: "long-lived event stream", method: "GET", target: "/admin/api/drives/one/events", pattern: "/admin/api/drives/{id}/events", status: 200, elapsed: time.Hour},
		{name: "failed event stream", method: "GET", target: "/admin/api/drives/one/events", pattern: "/admin/api/drives/{id}/events", status: 503, level: applog.LevelError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := applog.Open(applog.Config{Directory: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			var output bytes.Buffer
			formatter := capturedLogFormatter{
				access: &middleware.DefaultLogFormatter{Logger: log.New(&output, "", 0), NoColor: true},
				logs:   store,
			}
			request := httptest.NewRequest(tc.method, tc.target, nil)
			if tc.pattern != "" {
				routeContext := chi.NewRouteContext()
				routeContext.RoutePatterns = []string{tc.pattern}
				request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
			}
			formatter.NewLogEntry(request).Write(tc.status, 10, http.Header{}, tc.elapsed, nil)
			result, err := store.Query(context.Background(), applog.Query{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if tc.level == "" {
				if output.Len() != 0 || len(result.Entries) != 0 {
					t.Fatalf("routine query was logged: stdout=%q entries=%+v", output.String(), result.Entries)
				}
				return
			}
			if output.Len() == 0 || len(result.Entries) != 1 {
				t.Fatalf("request not logged in both outputs: stdout=%q entries=%+v", output.String(), result.Entries)
			}
			entry := result.Entries[0]
			if entry.Level != tc.level || entry.Status != tc.status || entry.Method != applog.Method(tc.method) {
				t.Fatalf("unexpected entry: %+v", entry)
			}
		})
	}
}

func TestRequestLogMiddlewareUsesMatchedNestedRoutes(t *testing.T) {
	store, err := applog.Open(applog.Config{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var output bytes.Buffer
	logger := log.New(&output, "", 0)
	router := chi.NewRouter()
	router.Use(requestLogMiddleware(logger, logger, store))
	router.Route("/admin/api", func(router chi.Router) {
		router.Get("/drives/{id}/runtime", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		router.Get("/drives/wopan/qr/{uuid}", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		router.Post("/drives/{id}/tasks/stop", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		router.Get("/logs", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
	})
	for _, target := range []string{"/admin/api/drives/one/runtime", "/admin/api/drives/wopan/qr/session"} {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
	}
	if output.Len() != 0 {
		t.Fatalf("successful matched queries were logged: %q", output.String())
	}
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/admin/api/drives/one/tasks/stop", nil))
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/admin/api/logs", nil))
	result, err := store.Query(context.Background(), applog.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 2 || result.Entries[0].Level != applog.LevelInfo || result.Entries[1].Level != applog.LevelWarning {
		t.Fatalf("operation and viewer failure logs = %+v", result.Entries)
	}
}
