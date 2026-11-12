package main

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/nianzhibai/91/backend/internal/applog"
)

const slowQueryLogThreshold = time.Second

// Only known read-only queries are quiet. Commands, downloads and ordinary
// public requests retain their access records, even when they share a path.
var routineQueryLogRoutes = map[string]struct{}{
	"GET /api/settings/preview":                     {},
	"GET /admin/api/upload/remote":                  {},
	"GET /api/auth/me":                              {},
	"GET /admin/api/drives":                         {},
	"GET /admin/api/drives/storage":                 {},
	"GET /admin/api/drives/{id}":                    {},
	"GET /admin/api/drives/{id}/config":             {},
	"GET /admin/api/drives/{id}/runtime":            {},
	"GET /admin/api/drives/{id}/stats":              {},
	"GET /admin/api/drives/{id}/storage":            {},
	"GET /admin/api/drives/{id}/dirtree":            {},
	"POST /admin/api/drives/quark/qr/status":        {},
	"POST /admin/api/drives/p115/qr/status":         {},
	"POST /admin/api/drives/yun139/qr/status":       {},
	"GET /admin/api/drives/p123/qr/{uniID}":         {},
	"GET /admin/api/drives/wopan/qr/{uuid}":         {},
	"GET /admin/api/drives/guangyapan/qr/status":    {},
	"GET /admin/api/crawlers":                       {},
	"GET /admin/api/crawlers/{id}/tasks":            {},
	"GET /admin/api/crawlers/{id}/tasks/{taskID}":   {},
	"GET /admin/api/videos":                         {},
	"GET /admin/api/videos/stats":                   {},
	"GET /admin/api/blacklist":                      {},
	"GET /admin/api/blacklist/source-delete/status": {},
	"GET /admin/api/tags":                           {},
	"GET /admin/api/tags/jobs/status":               {},
	"GET /admin/api/users":                          {},
	"GET /admin/api/banned-ips":                     {},
	"GET /admin/api/telegram/availability":          {},
	"GET /admin/api/telegram/status":                {},
	"GET /admin/api/import-jobs":                    {},
	"GET /admin/api/logs":                           {},
	"GET /admin/api/backups":                        {},
	"GET /admin/api/backup-uploads/{id}":            {},
	"GET /admin/api/backup-transfers":               {},
	"GET /admin/api/backup-receives":                {},
	"GET /admin/api/jobs/scan-all/status":           {},
}

func requestLogRoute(r *http.Request) string {
	if routeContext := chi.RouteContext(r.Context()); routeContext != nil {
		if pattern := routeContext.RoutePattern(); pattern != "" {
			return pattern
		}
	}
	return r.URL.Path
}

// An empty level suppresses a routine successful response in both log outputs.
func requestLogLevel(r *http.Request, status int, elapsed time.Duration) applog.Level {
	if status >= http.StatusInternalServerError {
		return applog.LevelError
	}
	if status >= http.StatusBadRequest {
		return applog.LevelWarning
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return applog.LevelInfo
	}
	if r == nil {
		return applog.LevelInfo
	}
	if r.Method == http.MethodGet && r.URL.Path == "/admin/api/logs" && r.URL.Query().Get("download") == "1" {
		return applog.LevelInfo
	}
	route := r.Method + " " + requestLogRoute(r)
	// A stream's elapsed time measures its connection lifetime, not query latency.
	if route == "GET /admin/api/drives/{id}/events" {
		return ""
	}
	if _, routine := routineQueryLogRoutes[route]; !routine {
		return applog.LevelInfo
	}
	if elapsed >= slowQueryLogThreshold {
		return applog.LevelWarning
	}
	return ""
}
