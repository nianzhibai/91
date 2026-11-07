package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nianzhibai/91/backend/internal/applog"
)

const (
	defaultLogQueryLimit = 500
	maxLogQueryLimit     = 10000
	maxLogSearchRunes    = 200
)

func (a *AdminServer) handleLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.Logs == nil {
		writeErr(w, r, http.StatusServiceUnavailable, errors.New("log viewer is unavailable"))
		return
	}

	query, err := parseLogQuery(r)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	if r.URL.Query().Get("download") == "1" {
		a.downloadLogs(w, r, query)
		return
	}
	snapshot, err := a.Logs.Query(r.Context(), query)
	if r.Context().Err() != nil {
		return
	}
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, fmt.Errorf("read runtime logs: %w", err))
		return
	}
	responseWriter, closeCompression := compressLogResponse(w, r)
	defer closeCompression()
	writeJSON(responseWriter, http.StatusOK, snapshot)
}

func (a *AdminServer) handleClearLogs(w http.ResponseWriter, r *http.Request) {
	if a.Logs == nil {
		writeErr(w, r, http.StatusServiceUnavailable, errors.New("log viewer is unavailable"))
		return
	}
	if err := a.Logs.Clear(); err != nil {
		writeErr(w, r, http.StatusInternalServerError, fmt.Errorf("clear runtime logs: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func parseLogQuery(r *http.Request) (applog.Query, error) {
	values := r.URL.Query()
	query := applog.Query{Limit: defaultLogQueryLimit}
	if raw := strings.TrimSpace(values.Get("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxLogQueryLimit {
			return query, errors.New("limit must be between 1 and 10000")
		}
		query.Limit = limit
	}
	query.Cursor = strings.TrimSpace(values.Get("cursor"))
	if len(query.Cursor) > 4096 {
		return query, errors.New("log cursor is too long")
	}
	if raw := values.Get("before"); raw != "" {
		before, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || before == 0 {
			return query, errors.New("before must be a positive log ID")
		}
		query.Before = before
		if query.Cursor != "" {
			return query, errors.New("before and cursor cannot be combined")
		}
	}
	for _, field := range []struct {
		name   string
		target *time.Time
	}{{"from", &query.From}, {"to", &query.To}} {
		if raw := values.Get(field.name); raw != "" {
			parsed, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return query, fmt.Errorf("invalid %s timestamp", field.name)
			}
			*field.target = parsed
		}
	}
	if !query.From.IsZero() && !query.To.IsZero() && query.From.After(query.To) {
		return query, errors.New("from must not be after to")
	}

	switch source := strings.TrimSpace(values.Get("source")); source {
	case "", "all":
	case string(applog.SourceApplication):
		query.Source = applog.SourceApplication
	case string(applog.SourceHTTP):
		query.Source = applog.SourceHTTP
	default:
		return query, errors.New("invalid log source")
	}

	switch level := strings.TrimSpace(values.Get("level")); level {
	case "", "all":
	case string(applog.LevelInfo):
		query.Level = applog.LevelInfo
	case string(applog.LevelWarning):
		query.Level = applog.LevelWarning
	case string(applog.LevelError):
		query.Level = applog.LevelError
	default:
		return query, errors.New("invalid log level")
	}

	switch method := strings.TrimSpace(values.Get("method")); method {
	case "", "all":
	case string(applog.MethodGET):
		query.Method = applog.MethodGET
	case string(applog.MethodPOST):
		query.Method = applog.MethodPOST
	case string(applog.MethodPUT):
		query.Method = applog.MethodPUT
	case string(applog.MethodPATCH):
		query.Method = applog.MethodPATCH
	case string(applog.MethodDELETE):
		query.Method = applog.MethodDELETE
	case string(applog.MethodOPTIONS):
		query.Method = applog.MethodOPTIONS
	case string(applog.MethodHEAD):
		query.Method = applog.MethodHEAD
	default:
		return query, errors.New("invalid HTTP method")
	}

	query.Search = strings.TrimSpace(values.Get("q"))
	if utf8.RuneCountInString(query.Search) > maxLogSearchRunes {
		return query, errors.New("log search must not exceed 200 characters")
	}
	return query, nil
}

func (a *AdminServer) downloadLogs(w http.ResponseWriter, r *http.Request, query applog.Query) {
	file, err := os.CreateTemp("", "91-logs-*.log")
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	defer os.Remove(file.Name())
	defer file.Close()
	query.Before = 0
	query.Cursor = ""
	if err := a.Logs.Export(r.Context(), query, file); err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	info, err := file.Stat()
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="runtime-logs.log"`)
	http.ServeContent(w, r, "runtime-logs.log", info.ModTime(), file)
}
