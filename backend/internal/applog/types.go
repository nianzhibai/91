package applog

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	DefaultMaxTotalSizeBytes = 30 * 1024 * 1024
	DefaultRetentionDays     = 14
	DefaultQueryLimit        = 200
	MaxQueryLimit            = 5000

	maxMessageBytes = 8 * 1024
	maxErrorBytes   = 8 * 1024
	maxStackBytes   = 32 * 1024
	maxPathBytes    = 4 * 1024
	maxIDBytes      = 256
	maxShortBytes   = 128
	maxAttrsBytes   = 16 * 1024
)

type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

func ParseLevel(raw string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return LevelDebug, nil
	case "info":
		return LevelInfo, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "error":
		return LevelError, nil
	default:
		return "", fmt.Errorf("invalid log level %q", raw)
	}
}

func (l Level) Slog() slog.Level {
	switch l {
	case LevelDebug:
		return slog.LevelDebug
	case LevelWarn:
		return slog.LevelWarn
	case LevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func levelFromSlog(l slog.Level) Level {
	switch {
	case l < slog.LevelInfo:
		return LevelDebug
	case l < slog.LevelWarn:
		return LevelInfo
	case l < slog.LevelError:
		return LevelWarn
	default:
		return LevelError
	}
}

type Source string

const (
	SourceApp  Source = "app"
	SourceHTTP Source = "http"
)

func ParseSource(raw string) (Source, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "app", "application":
		return SourceApp, nil
	case "http":
		return SourceHTTP, nil
	default:
		return "", fmt.Errorf("invalid log source %q", raw)
	}
}

type ConsoleFormat string

const (
	ConsoleFormatText ConsoleFormat = "text"
	ConsoleFormatJSON ConsoleFormat = "json"
)

func ParseConsoleFormat(raw string) (ConsoleFormat, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "text":
		return ConsoleFormatText, nil
	case "json":
		return ConsoleFormatJSON, nil
	default:
		return "", fmt.Errorf("invalid console log format %q", raw)
	}
}

type Method string

const (
	MethodGET     Method = "GET"
	MethodPOST    Method = "POST"
	MethodPUT     Method = "PUT"
	MethodPATCH   Method = "PATCH"
	MethodDELETE  Method = "DELETE"
	MethodOPTIONS Method = "OPTIONS"
	MethodHEAD    Method = "HEAD"
)

func validMethod(method Method) bool {
	switch method {
	case MethodGET, MethodPOST, MethodPUT, MethodPATCH, MethodDELETE, MethodOPTIONS, MethodHEAD:
		return true
	default:
		return false
	}
}

// Fields identify the operation independently of its human-readable message.
type Fields struct {
	Component string `json:"component,omitempty"`
	RequestID string `json:"requestId,omitempty"`
	TaskID    string `json:"taskId,omitempty"`
	DriveID   string `json:"driveId,omitempty"`
	VideoID   string `json:"videoId,omitempty"`
	FileID    string `json:"fileId,omitempty"`
	Stage     string `json:"stage,omitempty"`
}

// HTTPRecord holds structured HTTP access metrics without duplicating them into Message.
type HTTPRecord struct {
	Method     Method  `json:"method"`
	Path       string  `json:"path"`
	Route      string  `json:"route,omitempty"`
	Status     int     `json:"status"`
	Bytes      int     `json:"bytes"`
	DurationMs float64 `json:"durationMs"`
	Remote     string  `json:"remote,omitempty"`
}

// Record is one structured runtime log entry stored in SQLite and streamed to operators.
type Record struct {
	ID        int64          `json:"id"`
	Timestamp time.Time      `json:"timestamp"`
	Level     Level          `json:"level"`
	Source    Source         `json:"source"`
	Component string         `json:"component,omitempty"`
	Message   string         `json:"message"`
	Error     string         `json:"error,omitempty"`
	Stack     string         `json:"stack,omitempty"`
	RequestID string         `json:"requestId,omitempty"`
	TaskID    string         `json:"taskId,omitempty"`
	DriveID   string         `json:"driveId,omitempty"`
	VideoID   string         `json:"videoId,omitempty"`
	FileID    string         `json:"fileId,omitempty"`
	Stage     string         `json:"stage,omitempty"`
	HTTP      *HTTPRecord    `json:"http,omitempty"`
	Attrs     map[string]any `json:"attrs,omitempty"`
}

type Query struct {
	Limit         int
	Before        int64
	After         int64
	From          time.Time
	To            time.Time
	Source        Source
	Levels        []Level
	Component     string
	Method        Method
	StatusMin     int
	StatusMax     int
	MinDurationMs float64
	RequestID     string
	TaskID        string
	DriveID       string
	VideoID       string
	Search        string
}

type Snapshot struct {
	Entries    []Record `json:"entries"`
	HasMore    bool     `json:"hasMore"`
	NextBefore int64    `json:"nextBefore,omitempty"`
	NextAfter  int64    `json:"nextAfter,omitempty"`
}

type Meta struct {
	StorageBytes    int64       `json:"storageBytes"`
	MaxStorageBytes int64       `json:"maxStorageBytes"`
	TotalCount      int64       `json:"totalCount"`
	Level           Level       `json:"level"`
	RetentionDays   int         `json:"retentionDays"`
	Components      []string    `json:"components"`
	WriteHealth     WriteHealth `json:"writeHealth"`
}

type WriteHealth struct {
	LastError     string     `json:"lastError,omitempty"`
	FailedWrites  uint64     `json:"failedWrites"`
	DroppedWrites uint64     `json:"droppedWrites"`
	LastFailureAt *time.Time `json:"lastFailureAt,omitempty"`
	LastSuccessAt *time.Time `json:"lastSuccessAt,omitempty"`
}

// Matches evaluates whether record satisfies query (used for live SSE filtering).
func (r Record) Matches(q Query) bool {
	if q.Before > 0 && r.ID >= q.Before {
		return false
	}
	if q.After > 0 && r.ID <= q.After {
		return false
	}
	if !q.From.IsZero() && r.Timestamp.Before(q.From) {
		return false
	}
	if !q.To.IsZero() && r.Timestamp.After(q.To) {
		return false
	}
	if q.Source != "" && r.Source != q.Source {
		return false
	}
	if len(q.Levels) > 0 && !slices.Contains(q.Levels, r.Level) {
		return false
	}
	if q.Component != "" && !strings.EqualFold(r.Component, q.Component) {
		return false
	}
	if q.RequestID != "" && r.RequestID != q.RequestID {
		return false
	}
	if q.TaskID != "" && r.TaskID != q.TaskID {
		return false
	}
	if q.DriveID != "" && r.DriveID != q.DriveID {
		return false
	}
	if q.VideoID != "" && r.VideoID != q.VideoID {
		return false
	}
	if q.Method != "" {
		if r.HTTP == nil || r.HTTP.Method != q.Method {
			return false
		}
	}
	if q.StatusMin > 0 || q.StatusMax > 0 {
		if r.HTTP == nil {
			return false
		}
		if q.StatusMin > 0 && r.HTTP.Status < q.StatusMin {
			return false
		}
		if q.StatusMax > 0 && r.HTTP.Status > q.StatusMax {
			return false
		}
	}
	if q.MinDurationMs > 0 {
		if r.HTTP == nil || r.HTTP.DurationMs < q.MinDurationMs {
			return false
		}
	}
	if needle := strings.ToLower(strings.TrimSpace(q.Search)); needle != "" {
		if !recordMatchesSearch(r, needle) {
			return false
		}
	}
	return true
}

func recordMatchesSearch(r Record, needle string) bool {
	fields := []string{
		r.Message, r.Error, r.Stack, r.Component,
		r.RequestID, r.TaskID, r.DriveID, r.VideoID, r.FileID, r.Stage,
	}
	if r.HTTP != nil {
		fields = append(fields, string(r.HTTP.Method), r.HTTP.Path, r.HTTP.Route, r.HTTP.Remote, strconv.Itoa(r.HTTP.Status))
	}
	for _, value := range fields {
		if value != "" && strings.Contains(strings.ToLower(value), needle) {
			return true
		}
	}
	if len(r.Attrs) > 0 {
		if raw, err := json.Marshal(r.Attrs); err == nil && strings.Contains(strings.ToLower(string(raw)), needle) {
			return true
		}
	}
	return false
}

// FormatText renders one record into a human-readable line for console output or text export.
func FormatText(r Record) string {
	var b strings.Builder
	b.WriteString(r.Timestamp.UTC().Format(time.RFC3339Nano))
	b.WriteByte(' ')
	b.WriteString(strings.ToUpper(string(r.Level)))
	if r.Component != "" {
		b.WriteString(" [")
		b.WriteString(r.Component)
		b.WriteByte(']')
	} else if r.Source == SourceHTTP {
		b.WriteString(" [http]")
	}
	if r.HTTP != nil {
		fmt.Fprintf(&b, " %s %s %d %dB %.2fms", r.HTTP.Method, r.HTTP.Path, r.HTTP.Status, r.HTTP.Bytes, r.HTTP.DurationMs)
		if r.HTTP.Remote != "" {
			b.WriteString(" remote=")
			b.WriteString(r.HTTP.Remote)
		}
	}
	if r.Message != "" {
		b.WriteByte(' ')
		b.WriteString(r.Message)
	}
	writeKV := func(k, v string) {
		if v == "" {
			return
		}
		b.WriteByte(' ')
		b.WriteString(k)
		b.WriteByte('=')
		if strings.ContainsAny(v, " \t\r\n\"") {
			b.WriteString(strconv.Quote(v))
		} else {
			b.WriteString(v)
		}
	}
	writeKV("requestId", r.RequestID)
	writeKV("taskId", r.TaskID)
	writeKV("driveId", r.DriveID)
	writeKV("videoId", r.VideoID)
	writeKV("fileId", r.FileID)
	writeKV("stage", r.Stage)
	if len(r.Attrs) > 0 {
		keys := make([]string, 0, len(r.Attrs))
		for k := range r.Attrs {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			writeKV(k, fmt.Sprint(r.Attrs[k]))
		}
	}
	writeKV("error", r.Error)
	if r.Stack != "" {
		b.WriteByte('\n')
		b.WriteString(r.Stack)
	}
	return b.String()
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	cut := maxBytes
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return value[:cut] + "… [truncated]"
}
