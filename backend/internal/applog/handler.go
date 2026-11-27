package applog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// RuntimeConfig configures the unified slog pipeline (console + optional SQLite store).
type RuntimeConfig struct {
	Directory         string
	Level             Level
	ConsoleFormat     ConsoleFormat
	ConsoleWriter     io.Writer
	FileEnabled       bool
	MaxTotalSizeBytes int64
	RetentionDays     int
}

// Runtime owns the live LevelVar, slog.Handler, and optional durable Store.
type Runtime struct {
	level   *slog.LevelVar
	handler *Handler
	logger  *slog.Logger
	store   *Store
}

// OpenRuntime initializes the unified logging runtime. If FileEnabled is true
// and opening the SQLite log store fails, it falls back to console-only logging
// and returns the error alongside the usable console Runtime so startup is never blocked.
func OpenRuntime(cfg RuntimeConfig) (*Runtime, error) {
	levelVar := new(slog.LevelVar)
	lvl := cfg.Level
	if lvl == "" {
		lvl = LevelInfo
	}
	levelVar.Set(lvl.Slog())

	format := cfg.ConsoleFormat
	if format == "" {
		format = ConsoleFormatText
	}
	console := cfg.ConsoleWriter
	if console == nil {
		console = os.Stderr
	}

	var store *Store
	var storeErr error
	if cfg.FileEnabled && strings.TrimSpace(cfg.Directory) != "" {
		store, storeErr = Open(StoreConfig{
			Directory:         cfg.Directory,
			MaxTotalSizeBytes: cfg.MaxTotalSizeBytes,
			RetentionDays:     cfg.RetentionDays,
			LevelFunc:         func() Level { return levelFromSlog(levelVar.Level()) },
		})
	}

	handler := NewHandler(HandlerOptions{
		Level:         levelVar,
		ConsoleWriter: console,
		ConsoleFormat: format,
		Store:         store,
	})
	rt := &Runtime{
		level:   levelVar,
		handler: handler,
		logger:  slog.New(handler),
		store:   store,
	}
	return rt, storeErr
}

func (r *Runtime) Logger() *slog.Logger {
	if r == nil {
		return slog.Default()
	}
	return r.logger
}

func (r *Runtime) Handler() *Handler {
	if r == nil {
		return nil
	}
	return r.handler
}

func (r *Runtime) Store() *Store {
	if r == nil {
		return nil
	}
	return r.store
}

func (r *Runtime) Level() Level {
	if r == nil || r.level == nil {
		return LevelInfo
	}
	return levelFromSlog(r.level.Level())
}

func (r *Runtime) SetLevel(level Level) {
	if r == nil || r.level == nil {
		return
	}
	r.level.Set(level.Slog())
}

func (r *Runtime) SetLimits(maxTotalBytes int64, retentionDays int) {
	if r == nil || r.store == nil {
		return
	}
	r.store.SetLimits(maxTotalBytes, retentionDays)
}

// InstallDefault installs this runtime as the global slog default and bridges
// the legacy standard library `log` package into it (for third-party libraries).
func (r *Runtime) InstallDefault() {
	if r == nil {
		return
	}
	slog.SetDefault(r.logger)
	log.SetFlags(0)
	log.SetPrefix("")
	log.SetOutput(stdLogBridge{logger: r.logger})
}

func (r *Runtime) Close() error {
	if r == nil || r.store == nil {
		return nil
	}
	return r.store.Close()
}

type stdLogBridge struct {
	logger *slog.Logger
}

func (b stdLogBridge) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	if msg == "" {
		return len(p), nil
	}
	b.logger.Info(msg)
	return len(p), nil
}

// HandlerOptions configures a slog.Handler that redacts once and fans out to
// both the console writer and durable Store.
type HandlerOptions struct {
	Level         slog.Leveler
	ConsoleWriter io.Writer
	ConsoleFormat ConsoleFormat
	Store         *Store
}

// Handler is an slog.Handler that extracts ContextFields, redacts once, writes
// to console, and submits structured Records to Store + SSE Live Hub.
type Handler struct {
	level         slog.Leveler
	console       io.Writer
	consoleFormat ConsoleFormat
	consoleMu     *sync.Mutex
	store         *Store
	attrs         []slog.Attr
	group         string
}

func NewHandler(opts HandlerOptions) *Handler {
	lvl := opts.Level
	if lvl == nil {
		lvl = slog.LevelInfo
	}
	format := opts.ConsoleFormat
	if format == "" {
		format = ConsoleFormatText
	}
	return &Handler{
		level:         lvl,
		console:       opts.ConsoleWriter,
		consoleFormat: format,
		consoleMu:     &sync.Mutex{},
		store:         opts.Store,
	}
}

func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	if h == nil {
		return false
	}
	return level >= h.level.Level()
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if h == nil {
		return nil
	}
	ts := r.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	cf := ContextFields(ctx)
	rec := Record{
		Timestamp: ts.UTC(),
		Level:     levelFromSlog(r.Level),
		Source:    SourceApp,
		Component: cf.Component,
		Message:   r.Message,
		RequestID: cf.RequestID,
		TaskID:    cf.TaskID,
		DriveID:   cf.DriveID,
		VideoID:   cf.VideoID,
		FileID:    cf.FileID,
		Stage:     cf.Stage,
	}

	applyAttr := func(prefix string, attr slog.Attr) {
		attr.Value = attr.Value.Resolve()
		if attr.Equal(slog.Attr{}) {
			return
		}
		key := attr.Key
		if prefix != "" {
			if key != "" {
				key = prefix + "." + key
			} else {
				key = prefix
			}
		}
		assignRecordAttr(&rec, key, attr.Value)
	}

	for _, a := range h.attrs {
		applyAttr("", a)
	}
	r.Attrs(func(a slog.Attr) bool {
		applyAttr(h.group, a)
		return true
	})

	h.emit(rec)
	return nil
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	cp := *h
	cp.attrs = make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	cp.attrs = append(cp.attrs, h.attrs...)
	for _, a := range attrs {
		if h.group != "" && a.Key != "" {
			a.Key = h.group + "." + a.Key
		}
		cp.attrs = append(cp.attrs, a)
	}
	return &cp
}

func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	cp := *h
	if cp.group == "" {
		cp.group = name
	} else {
		cp.group = cp.group + "." + name
	}
	return &cp
}

// emit normalizes, redacts once, writes console output, and submits to Store.
func (h *Handler) emit(rec Record) {
	rec = normalizeRecord(rec)
	if h.store != nil {
		rec.ID = h.store.Submit(rec)
	}
	if h.console != nil {
		var line []byte
		if h.consoleFormat == ConsoleFormatJSON {
			if encoded, err := json.Marshal(rec); err == nil {
				line = append(encoded, '\n')
			}
		} else {
			line = []byte(FormatText(rec) + "\n")
		}
		if len(line) > 0 {
			h.consoleMu.Lock()
			_, _ = h.console.Write(line)
			h.consoleMu.Unlock()
		}
	}
}

func assignRecordAttr(rec *Record, key string, val slog.Value) {
	if val.Kind() == slog.KindGroup {
		for _, child := range val.Group() {
			childKey := child.Key
			if key != "" && childKey != "" {
				childKey = key + "." + childKey
			} else if key != "" {
				childKey = key
			}
			assignRecordAttr(rec, childKey, child.Value.Resolve())
		}
		return
	}

	switch key {
	case "component":
		if s := val.String(); s != "" {
			rec.Component = s
		}
		return
	case "requestId", "request_id":
		if s := val.String(); s != "" {
			rec.RequestID = s
		}
		return
	case "taskId", "task_id":
		if s := val.String(); s != "" {
			rec.TaskID = s
		}
		return
	case "driveId", "drive_id":
		if s := val.String(); s != "" {
			rec.DriveID = s
		}
		return
	case "videoId", "video_id":
		if s := val.String(); s != "" {
			rec.VideoID = s
		}
		return
	case "fileId", "file_id":
		if s := val.String(); s != "" {
			rec.FileID = s
		}
		return
	case "stage":
		if s := val.String(); s != "" {
			rec.Stage = s
		}
		return
	case "source":
		if parsed, err := ParseSource(val.String()); err == nil {
			rec.Source = parsed
		}
		return
	case "error", "err":
		if val.Kind() == slog.KindAny {
			if errVal, ok := val.Any().(error); ok {
				if errVal != nil {
					rec.Error = errVal.Error()
				}
				return
			}
		}
		if s := val.String(); s != "" && s != "<nil>" {
			rec.Error = s
		}
		return
	case "stack":
		if s := val.String(); s != "" {
			rec.Stack = s
		}
		return
	case "http":
		if val.Kind() == slog.KindAny {
			switch h := val.Any().(type) {
			case *HTTPRecord:
				if h != nil {
					cp := *h
					rec.HTTP = &cp
				}
				return
			case HTTPRecord:
				cp := h
				rec.HTTP = &cp
				return
			}
		}
	}

	if key == "" {
		return
	}
	if rec.Attrs == nil {
		rec.Attrs = make(map[string]any)
	}
	rec.Attrs[key] = slogValueToAny(val)
}

func slogValueToAny(val slog.Value) any {
	switch val.Kind() {
	case slog.KindBool:
		return val.Bool()
	case slog.KindInt64:
		return val.Int64()
	case slog.KindUint64:
		return val.Uint64()
	case slog.KindFloat64:
		return val.Float64()
	case slog.KindString:
		return val.String()
	case slog.KindDuration:
		return val.Duration().String()
	case slog.KindTime:
		return val.Time().UTC().Format(time.RFC3339Nano)
	case slog.KindAny:
		anyVal := val.Any()
		if err, ok := anyVal.(error); ok {
			if err == nil {
				return nil
			}
			return err.Error()
		}
		if stringer, ok := anyVal.(fmt.Stringer); ok {
			return stringer.String()
		}
		return anyVal
	default:
		return val.String()
	}
}

func normalizeRecord(rec Record) Record {
	redactRecord(&rec)
	if rec.Timestamp.IsZero() {
		rec.Timestamp = time.Now().UTC()
	} else {
		rec.Timestamp = rec.Timestamp.UTC()
	}
	if rec.Source != SourceHTTP {
		rec.Source = SourceApp
	}
	if rec.Level == "" {
		rec.Level = LevelInfo
	}
	rec.Message = strings.Clone(truncateUTF8(strings.TrimSpace(rec.Message), maxMessageBytes))
	rec.Error = strings.Clone(truncateUTF8(strings.TrimSpace(rec.Error), maxErrorBytes))
	rec.Stack = strings.Clone(truncateUTF8(rec.Stack, maxStackBytes))
	rec.Component = strings.Clone(truncateUTF8(strings.TrimSpace(rec.Component), maxShortBytes))
	rec.RequestID = strings.Clone(truncateUTF8(strings.TrimSpace(rec.RequestID), maxIDBytes))
	rec.TaskID = strings.Clone(truncateUTF8(strings.TrimSpace(rec.TaskID), maxIDBytes))
	rec.DriveID = strings.Clone(truncateUTF8(strings.TrimSpace(rec.DriveID), maxPathBytes))
	rec.VideoID = strings.Clone(truncateUTF8(strings.TrimSpace(rec.VideoID), maxPathBytes))
	rec.FileID = strings.Clone(truncateUTF8(strings.TrimSpace(rec.FileID), maxPathBytes))
	rec.Stage = strings.Clone(truncateUTF8(strings.TrimSpace(rec.Stage), maxShortBytes))
	if rec.HTTP != nil {
		cp := *rec.HTTP
		if !validMethod(cp.Method) {
			cp.Method = ""
		}
		if cp.Status < 100 || cp.Status > 599 {
			cp.Status = 0
		}
		if cp.Bytes < 0 {
			cp.Bytes = 0
		}
		if cp.DurationMs < 0 {
			cp.DurationMs = 0
		}
		cp.Path = strings.Clone(truncateUTF8(strings.TrimSpace(cp.Path), maxPathBytes))
		cp.Route = strings.Clone(truncateUTF8(strings.TrimSpace(cp.Route), maxPathBytes))
		cp.Remote = strings.Clone(truncateUTF8(strings.TrimSpace(cp.Remote), maxShortBytes))
		rec.HTTP = &cp
	}
	if rec.Error != "" && rec.Level == LevelInfo {
		rec.Level = LevelError
	}
	return rec
}

// Package-level ergonomic helpers built directly on slog.Default().

func Debug(ctx context.Context, msg string, args ...any) {
	slog.Default().DebugContext(ctx, msg, args...)
}

func Info(ctx context.Context, msg string, args ...any) {
	slog.Default().InfoContext(ctx, msg, args...)
}

func Warn(ctx context.Context, msg string, err error, args ...any) {
	if err != nil {
		args = append(args, slog.Any("error", err))
	}
	slog.Default().WarnContext(ctx, msg, args...)
}

func Error(ctx context.Context, msg string, err error, args ...any) {
	if err != nil {
		args = append(args, slog.Any("error", err))
		if errors.Is(err, context.Canceled) {
			slog.Default().WarnContext(ctx, msg, args...)
			return
		}
	}
	slog.Default().ErrorContext(ctx, msg, args...)
}

// LogHTTP emits a structured HTTP access record through the active slog pipeline.
func LogHTTP(ctx context.Context, level Level, httpRec HTTPRecord, msg string) {
	slog.Default().LogAttrs(ctx, level.Slog(), msg,
		slog.String("source", string(SourceHTTP)),
		slog.Any("http", httpRec),
	)
}

// LogPanic emits a single error record containing the recovered panic and stack trace.
func LogPanic(ctx context.Context, httpRec *HTTPRecord, value any, stack []byte) {
	attrs := []slog.Attr{
		slog.String("component", "http"),
		slog.String("stack", string(stack)),
	}
	if httpRec != nil {
		attrs = append(attrs, slog.Any("http", *httpRec))
	}
	slog.Default().LogAttrs(ctx, slog.LevelError, fmt.Sprintf("HTTP panic: %v", value), attrs...)
}
