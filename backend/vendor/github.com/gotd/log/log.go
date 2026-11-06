// Package log defines a minimal, dependency-free logging port for gotd
// libraries.
//
// A library writes structured records to a [Logger] and never logs through a
// global or imports a concrete logging framework. The consumer chooses the
// backend by passing an adapter: see the logslog and logzap subpackages for
// log/slog and go.uber.org/zap. When no logger is supplied, [Nop] discards
// every record.
//
// The attribute model ([Attr], [Value]) stores scalar values without boxing
// them into an interface, and the [Logger.Enabled] gate lets callers skip
// building attributes on hot paths when a level is disabled.
package log

import "context"

// Level is the severity of a log record.
//
// Values match log/slog (Debug=-4, Info=0, Warn=4, Error=8) so adapters convert
// by casting; this is an interop convenience, the values are defined here.
type Level int

// Severity levels.
const (
	LevelDebug Level = -4
	LevelInfo  Level = 0
	LevelWarn  Level = 4
	LevelError Level = 8
)

// String returns the canonical name of the level.
func (l Level) String() string {
	switch {
	case l < LevelInfo:
		return "DEBUG"
	case l < LevelWarn:
		return "INFO"
	case l < LevelError:
		return "WARN"
	default:
		return "ERROR"
	}
}

// Logger is the logging port a library writes structured records to.
//
// Implementations must be safe for concurrent use by multiple goroutines.
type Logger interface {
	// Enabled reports whether a record at level would be recorded. Callers use
	// it to avoid building attributes on hot paths when logging is off.
	Enabled(ctx context.Context, level Level) bool
	// Log records one structured message. The attrs slice and its contents must
	// not be retained after Log returns.
	Log(ctx context.Context, level Level, msg string, attrs ...Attr)
}

// Nop is a Logger that discards every record. Its Enabled always returns false.
var Nop Logger = nop{}

type nop struct{}

func (nop) Enabled(context.Context, Level) bool         { return false }
func (nop) Log(context.Context, Level, string, ...Attr) {}

// OrNop returns l, or Nop if l is nil. Callers store the result so no log site
// needs a nil check.
func OrNop(l Logger) Logger {
	if l == nil {
		return Nop
	}
	return l
}
