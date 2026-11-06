package log

import "context"

// Wither is an optional Logger capability: returning a child Logger with attrs
// permanently attached to every record. Adapters implement it to map onto a
// native mechanism (e.g. zap.Logger.With); With falls back to a wrapper for
// loggers that do not.
type Wither interface {
	With(attrs ...Attr) Logger
}

// Namer is an optional Logger capability: returning a child Logger tagged with a
// name. Adapters implement it to map onto a native mechanism (e.g.
// zap.Logger.Named); Named is a no-op for loggers that do not, since the name is
// advisory.
type Namer interface {
	Named(name string) Logger
}

// CallerSkipper is an optional Logger capability: returning a child Logger that
// attributes records to a caller skip frames further up the stack. Wrappers
// that sit between the call site and the adapter (such as Helper) use it so the
// adapter still reports the real caller. Adapters that compute the caller
// implement it by mapping onto a native mechanism (e.g. zap.AddCallerSkip);
// loggers that do not are returned unchanged by AddCallerSkip.
type CallerSkipper interface {
	WithCallerSkip(skip int) Logger
}

// AddCallerSkip returns a Logger that attributes records skip frames further up
// the stack, using the logger's native mechanism when it implements
// CallerSkipper. It returns l unchanged when skip is 0 or l does not track
// caller depth.
func AddCallerSkip(l Logger, skip int) Logger {
	l = OrNop(l)
	if skip == 0 {
		return l
	}
	if s, ok := l.(CallerSkipper); ok {
		return s.WithCallerSkip(skip)
	}
	return l
}

// With returns a child Logger that attaches attrs to every record. It uses the
// logger's native With when it implements Wither, otherwise it wraps l. With no
// attrs it returns l unchanged.
func With(l Logger, attrs ...Attr) Logger {
	l = OrNop(l)
	if len(attrs) == 0 {
		return l
	}
	if w, ok := l.(Wither); ok {
		return w.With(attrs...)
	}
	return &withLogger{l: l, attrs: append([]Attr(nil), attrs...)}
}

// Named returns a child Logger tagged with name. It uses the logger's native
// Named when it implements Namer, otherwise it returns l unchanged (the name is
// advisory for backends without a naming concept). With an empty name it returns
// l unchanged.
func Named(l Logger, name string) Logger {
	l = OrNop(l)
	if name == "" {
		return l
	}
	if n, ok := l.(Namer); ok {
		return n.Named(name)
	}
	return l
}

type withLogger struct {
	l     Logger
	attrs []Attr
}

func (w *withLogger) Enabled(ctx context.Context, level Level) bool {
	return w.l.Enabled(ctx, level)
}

func (w *withLogger) Log(ctx context.Context, level Level, msg string, attrs ...Attr) {
	// Prepend the attached attrs; cap the stored slice so append never mutates it.
	merged := append(w.attrs[:len(w.attrs):len(w.attrs)], attrs...)
	w.l.Log(ctx, level, msg, merged...)
}

// With implements Wither so chained With calls accumulate without re-wrapping.
func (w *withLogger) With(attrs ...Attr) Logger {
	return With(w.l, append(w.attrs[:len(w.attrs):len(w.attrs)], attrs...)...)
}
