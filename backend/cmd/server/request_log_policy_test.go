package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/nianzhibai/91/backend/internal/applog"
)

func TestRequestLogLevelPolicy(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range []struct {
		name    string
		req     *http.Request
		mode    applog.RequestLogMode
		status  int
		elapsed time.Duration
		want    applog.Level
	}{
		{name: "default 200", mode: applog.RequestLogDefault, status: 200, elapsed: 10 * time.Millisecond, want: applog.LevelInfo},
		{name: "quiet fast 200", mode: applog.RequestLogQuiet, status: 200, elapsed: time.Second - time.Nanosecond, want: ""},
		{name: "quiet slow 200", mode: applog.RequestLogQuiet, status: 200, elapsed: time.Second, want: applog.LevelWarn},
		{name: "quiet 401", mode: applog.RequestLogQuiet, status: 401, elapsed: 2 * time.Millisecond, want: applog.LevelWarn},
		{name: "quiet 503", mode: applog.RequestLogQuiet, status: 503, elapsed: 2 * time.Millisecond, want: applog.LevelError},
		{name: "stream long 200", mode: applog.RequestLogSkipSuccess, status: 200, elapsed: time.Hour, want: ""},
		{name: "stream 503", mode: applog.RequestLogSkipSuccess, status: 503, elapsed: time.Second, want: applog.LevelError},
		{name: "client canceled", req: httptest.NewRequest(http.MethodGet, "/api/list", nil).WithContext(canceledCtx), mode: applog.RequestLogDefault, status: 500, want: applog.LevelWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestLogLevel(tc.req, tc.mode, tc.status, tc.elapsed); got != tc.want {
				t.Fatalf("requestLogLevel() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRequestLogMiddlewareDeclarativeGroups(t *testing.T) {
	rt, console := installTestLogRuntime(t)

	router := chi.NewRouter()
	router.Use(requestLogMiddleware())
	router.Route("/admin/api", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(applog.QuietRequestLog)
			r.Get("/drives/{id}/runtime", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
			r.Get("/logs", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
		})
		r.With(applog.SkipSuccessRequestLog).Get("/logs/stream", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		r.Post("/drives/{id}/tasks/stop", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	})

	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/admin/api/drives/one/runtime", nil))
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/admin/api/logs/stream", nil))
	if console.Len() != 0 {
		t.Fatalf("quiet/stream 200 requests were logged: %q", console.String())
	}

	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/admin/api/drives/one/tasks/stop", nil))
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/admin/api/logs", nil))

	result, err := rt.Store().Query(context.Background(), applog.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 2 || result.Entries[0].Level != applog.LevelInfo || result.Entries[1].Level != applog.LevelWarn {
		t.Fatalf("entries = %+v", result.Entries)
	}
	if result.Entries[0].HTTP == nil || result.Entries[0].HTTP.Route != "/admin/api/drives/{id}/tasks/stop" {
		t.Fatalf("route pattern not captured: %+v", result.Entries[0].HTTP)
	}
}
