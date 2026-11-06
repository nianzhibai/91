package log

import "context"

// Helper wraps a Logger with leveled convenience methods for call sites. It is a
// thin value type; adapters implement Logger, not Helper.
//
// Build one with For; the zero Helper logs to Nop.
type Helper struct {
	l Logger // underlying logger, returned by Logger()
	// leveled is l with one extra caller-skip frame, so the leveled methods
	// below report their own caller rather than Helper. It is built once by For
	// to avoid adjusting caller depth on every call.
	leveled Logger
}

// For returns a Helper writing to l, or to Nop if l is nil.
func For(l Logger) Helper {
	l = OrNop(l)
	// The leveled methods add one frame between the call site and the logger;
	// skip it so caller-computing adapters report the caller, not Helper.
	return Helper{l: l, leveled: AddCallerSkip(l, 1)}
}

func (h Helper) logger() Logger {
	if h.l == nil {
		return Nop
	}
	return h.l
}

func (h Helper) leveledLogger() Logger {
	if h.leveled == nil {
		return Nop
	}
	return h.leveled
}

// Logger returns the underlying Logger, never nil.
func (h Helper) Logger() Logger {
	return h.logger()
}

// With returns a Helper whose logger attaches attrs to every record. See With.
func (h Helper) With(attrs ...Attr) Helper {
	return For(With(h.logger(), attrs...))
}

// Named returns a Helper whose logger is tagged with name. See Named.
func (h Helper) Named(name string) Helper {
	return For(Named(h.logger(), name))
}

// Enabled reports whether a record at level would be recorded.
func (h Helper) Enabled(ctx context.Context, level Level) bool {
	return h.logger().Enabled(ctx, level)
}

// Debug logs at LevelDebug.
func (h Helper) Debug(ctx context.Context, msg string, attrs ...Attr) {
	h.leveledLogger().Log(ctx, LevelDebug, msg, attrs...)
}

// Info logs at LevelInfo.
func (h Helper) Info(ctx context.Context, msg string, attrs ...Attr) {
	h.leveledLogger().Log(ctx, LevelInfo, msg, attrs...)
}

// Warn logs at LevelWarn.
func (h Helper) Warn(ctx context.Context, msg string, attrs ...Attr) {
	h.leveledLogger().Log(ctx, LevelWarn, msg, attrs...)
}

// Error logs at LevelError.
func (h Helper) Error(ctx context.Context, msg string, attrs ...Attr) {
	h.leveledLogger().Log(ctx, LevelError, msg, attrs...)
}
