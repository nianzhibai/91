package applog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

type fieldsKey struct{}
type requestLogHintKey struct{}

var fallbackSequence atomic.Uint64

func WithFields(ctx context.Context, fields Fields) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, fieldsKey{}, mergeFields(ContextFields(ctx), fields))
}

func WithComponent(ctx context.Context, component string) context.Context {
	return WithFields(ctx, Fields{Component: component})
}

func ContextFields(ctx context.Context) Fields {
	if ctx == nil {
		return Fields{}
	}
	fields, _ := ctx.Value(fieldsKey{}).(Fields)
	return fields
}

func NewTask(ctx context.Context, component, driveID string) context.Context {
	return WithFields(ctx, Fields{Component: component, DriveID: driveID, TaskID: NewID()})
}

func NewID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return fmt.Sprintf("local-%x-%x", time.Now().UnixNano(), fallbackSequence.Add(1))
	}
	return hex.EncodeToString(id[:])
}

func mergeFields(base, next Fields) Fields {
	if next.Component != "" {
		base.Component = next.Component
	}
	if next.RequestID != "" {
		base.RequestID = next.RequestID
	}
	if next.TaskID != "" {
		base.TaskID = next.TaskID
	}
	if next.DriveID != "" {
		base.DriveID = next.DriveID
	}
	if next.VideoID != "" {
		base.VideoID = next.VideoID
	}
	if next.FileID != "" {
		base.FileID = next.FileID
	}
	if next.Stage != "" {
		base.Stage = next.Stage
	}
	return base
}

// RequestLogMode controls how the HTTP access middleware records a completed route.
type RequestLogMode uint8

const (
	// RequestLogDefault records all completed requests.
	RequestLogDefault RequestLogMode = iota
	// RequestLogQuiet suppresses 2xx responses below the slow threshold;
	// 4xx/5xx and slow responses remain logged.
	RequestLogQuiet
	// RequestLogSkipSuccess suppresses 2xx responses regardless of duration
	// (intended for long-lived SSE event streams).
	RequestLogSkipSuccess
)

// RequestLogHint is attached by the outer HTTP logging middleware so inner route
// groups can declare their logging policy without maintaining a separate route table.
type RequestLogHint struct {
	Mode RequestLogMode
}

func WithRequestLogHint(ctx context.Context) (context.Context, *RequestLogHint) {
	hint := &RequestLogHint{}
	return context.WithValue(ctx, requestLogHintKey{}, hint), hint
}

func SetRequestLogMode(ctx context.Context, mode RequestLogMode) {
	if ctx == nil {
		return
	}
	if hint, ok := ctx.Value(requestLogHintKey{}).(*RequestLogHint); ok && hint != nil {
		hint.Mode = mode
	}
}

// QuietRequestLog marks a route group as routine read-only queries whose fast
// 2xx responses are omitted from access logs.
func QuietRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetRequestLogMode(r.Context(), RequestLogQuiet)
		next.ServeHTTP(w, r)
	})
}

// SkipSuccessRequestLog marks a long-lived stream route whose 2xx completion is
// omitted from access logs (errors still log normally).
func SkipSuccessRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetRequestLogMode(r.Context(), RequestLogSkipSuccess)
		next.ServeHTTP(w, r)
	})
}
