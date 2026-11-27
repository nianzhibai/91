package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nianzhibai/91/backend/internal/applog"
)

const (
	defaultLogQueryLimit = applog.DefaultQueryLimit
	maxLogQueryLimit     = applog.MaxQueryLimit
	maxLogSearchRunes    = 200
	maxLogFieldBytes     = 256
	logStreamCatchUpMax  = 1000
	logStreamPingEvery   = 25 * time.Second
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
	snapshot, err := a.Logs.Query(r.Context(), query)
	if r.Context().Err() != nil {
		return
	}
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, fmt.Errorf("read runtime logs: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (a *AdminServer) handleLogsMeta(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.Logs == nil {
		writeErr(w, r, http.StatusServiceUnavailable, errors.New("log viewer is unavailable"))
		return
	}
	meta, err := a.Logs.Meta(r.Context())
	if r.Context().Err() != nil {
		return
	}
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, fmt.Errorf("read runtime log metadata: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

func (a *AdminServer) handleExportLogs(w http.ResponseWriter, r *http.Request) {
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
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	switch format {
	case "", "text":
		format = "text"
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="runtime-logs.log"`)
	case "jsonl":
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="runtime-logs.jsonl"`)
	default:
		writeErr(w, r, http.StatusBadRequest, errors.New("format must be text or jsonl"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_ = a.Logs.Export(r.Context(), query, format, w)
}

func (a *AdminServer) handleLogStream(w http.ResponseWriter, r *http.Request) {
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
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, r, http.StatusInternalServerError, errors.New("streaming is not supported"))
		return
	}

	lastID := query.After
	if headerID := strings.TrimSpace(r.Header.Get("Last-Event-ID")); headerID != "" {
		parsed, parseErr := strconv.ParseInt(headerID, 10, 64)
		if parseErr != nil || parsed < 0 {
			writeErr(w, r, http.StatusBadRequest, errors.New("invalid Last-Event-ID header"))
			return
		}
		if parsed > lastID {
			lastID = parsed
		}
	}

	// Subscribe before querying catch-up so records arriving during catch-up are never missed.
	subQuery := query
	subQuery.After = lastID
	subQuery.Before = 0
	liveCh, cancel := a.Logs.Subscribe(subQuery)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	writeSSE := func(rec applog.Record) error {
		payload, marshalErr := json.Marshal(rec)
		if marshalErr != nil {
			return marshalErr
		}
		if _, err := fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", rec.ID, payload); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	if lastID > 0 {
		catchUpQuery := query
		catchUpQuery.After = lastID
		catchUpQuery.Before = 0
		catchUpQuery.Limit = logStreamCatchUpMax
		snap, catchErr := a.Logs.Query(r.Context(), catchUpQuery)
		if catchErr == nil {
			for _, rec := range snap.Entries {
				if rec.ID <= lastID {
					continue
				}
				if err := writeSSE(rec); err != nil {
					return
				}
				lastID = rec.ID
			}
		}
	}

	ping := time.NewTicker(logStreamPingEvery)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case rec, open := <-liveCh:
			if !open {
				return
			}
			if rec.ID <= lastID {
				continue
			}
			if err := writeSSE(rec); err != nil {
				return
			}
			lastID = rec.ID
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (a *AdminServer) handleClearLogs(w http.ResponseWriter, r *http.Request) {
	if a.Logs == nil {
		writeErr(w, r, http.StatusServiceUnavailable, errors.New("log viewer is unavailable"))
		return
	}
	if err := a.Logs.Clear(r.Context()); err != nil {
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
			return query, fmt.Errorf("limit must be between 1 and %d", maxLogQueryLimit)
		}
		query.Limit = limit
	}
	if raw := strings.TrimSpace(values.Get("before")); raw != "" {
		before, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || before <= 0 {
			return query, errors.New("before must be a positive log ID")
		}
		query.Before = before
	}
	if raw := strings.TrimSpace(values.Get("after")); raw != "" {
		after, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || after <= 0 {
			return query, errors.New("after must be a positive log ID")
		}
		if query.Before > 0 {
			return query, errors.New("before and after cannot be combined")
		}
		query.After = after
	}

	for _, field := range []struct {
		name   string
		target *time.Time
	}{{"from", &query.From}, {"to", &query.To}} {
		if raw := strings.TrimSpace(values.Get(field.name)); raw != "" {
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

	if raw := strings.TrimSpace(values.Get("source")); raw != "" && !strings.EqualFold(raw, "all") {
		src, err := applog.ParseSource(raw)
		if err != nil {
			return query, errors.New("invalid log source")
		}
		query.Source = src
	}

	if raw := strings.TrimSpace(values.Get("level")); raw != "" && !strings.EqualFold(raw, "all") {
		seen := make(map[applog.Level]bool, 4)
		for _, part := range strings.Split(raw, ",") {
			token := strings.TrimSpace(part)
			if token == "" || strings.EqualFold(token, "all") {
				continue
			}
			lvl, err := applog.ParseLevel(token)
			if err != nil {
				return query, errors.New("invalid log level")
			}
			if !seen[lvl] {
				seen[lvl] = true
				query.Levels = append(query.Levels, lvl)
			}
		}
	}

	switch method := strings.ToUpper(strings.TrimSpace(values.Get("method"))); method {
	case "", "ALL":
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

	if raw := strings.ToLower(strings.TrimSpace(values.Get("status"))); raw != "" && raw != "all" {
		switch raw {
		case "2xx":
			query.StatusMin, query.StatusMax = 200, 299
		case "3xx":
			query.StatusMin, query.StatusMax = 300, 399
		case "4xx":
			query.StatusMin, query.StatusMax = 400, 499
		case "5xx":
			query.StatusMin, query.StatusMax = 500, 599
		default:
			code, err := strconv.Atoi(raw)
			if err != nil || code < 100 || code > 599 {
				return query, errors.New("status must be 2xx, 3xx, 4xx, 5xx, or an HTTP status code")
			}
			query.StatusMin, query.StatusMax = code, code
		}
	}

	if raw := strings.TrimSpace(values.Get("minDurationMs")); raw != "" {
		dur, err := strconv.ParseFloat(raw, 64)
		if err != nil || dur < 0 || dur > 86_400_000 {
			return query, errors.New("minDurationMs must be a non-negative number")
		}
		query.MinDurationMs = dur
	}

	for _, field := range []struct {
		name   string
		target *string
	}{
		{"component", &query.Component},
		{"requestId", &query.RequestID},
		{"taskId", &query.TaskID},
		{"driveId", &query.DriveID},
		{"videoId", &query.VideoID},
	} {
		val := strings.TrimSpace(values.Get(field.name))
		if len(val) > maxLogFieldBytes {
			return query, fmt.Errorf("%s exceeds maximum length", field.name)
		}
		*field.target = val
	}

	query.Search = strings.TrimSpace(values.Get("q"))
	if utf8.RuneCountInString(query.Search) > maxLogSearchRunes {
		return query, errors.New("log search must not exceed 200 characters")
	}
	return query, nil
}
