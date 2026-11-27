package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/nianzhibai/91/backend/internal/applog"
)

const slowQueryLogThreshold = time.Second

func requestLogRoute(r *http.Request) string {
	if routeContext := chi.RouteContext(r.Context()); routeContext != nil {
		if pattern := routeContext.RoutePattern(); pattern != "" {
			return pattern
		}
	}
	return r.URL.Path
}

// requestLogLevel determines the access log severity for a completed request.
// An empty level suppresses the record from both console and durable storage.
func requestLogLevel(r *http.Request, mode applog.RequestLogMode, status int, elapsed time.Duration) applog.Level {
	if r != nil && errors.Is(r.Context().Err(), context.Canceled) {
		if status == 0 || status >= http.StatusInternalServerError {
			return applog.LevelWarn
		}
	}
	if status >= http.StatusInternalServerError {
		return applog.LevelError
	}
	if status >= http.StatusBadRequest {
		return applog.LevelWarn
	}
	if status > 0 && (status < http.StatusOK || status >= http.StatusMultipleChoices) {
		return applog.LevelInfo
	}
	switch mode {
	case applog.RequestLogSkipSuccess:
		return ""
	case applog.RequestLogQuiet:
		if elapsed >= slowQueryLogThreshold {
			return applog.LevelWarn
		}
		return ""
	default:
		return applog.LevelInfo
	}
}
