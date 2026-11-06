package log

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// Kind enumerates the dynamic type stored in a [Value].
type Kind uint8

// Value kinds.
const (
	KindAny Kind = iota
	KindString
	KindInt64
	KindUint64
	KindFloat64
	KindBool
	KindDuration
	KindTime
	KindError
	KindGroup
)

// Attr is a key-value pair attached to a log record.
type Attr struct {
	Key   string
	Value Value
}

// Value holds an attribute value without boxing scalar kinds into an interface.
//
// num carries int64/uint64/float64/bool/duration bits; s carries strings; a
// carries the boxed value for KindAny, KindTime and KindError. It is modeled on
// slog.Value but kept deliberately small and explicit.
type Value struct {
	kind Kind
	num  uint64
	s    string
	a    any
}

// Kind returns the value's kind.
func (v Value) Kind() Kind { return v.kind }

// Int64 returns the value as int64. Valid for KindInt64 and KindDuration.
func (v Value) Int64() int64 { return int64(v.num) }

// Uint64 returns the value as uint64. Valid for KindUint64.
func (v Value) Uint64() uint64 { return v.num }

// Float64 returns the value as float64. Valid for KindFloat64.
func (v Value) Float64() float64 { return math.Float64frombits(v.num) }

// Bool returns the value as bool. Valid for KindBool.
func (v Value) Bool() bool { return v.num != 0 }

// Duration returns the value as time.Duration. Valid for KindDuration.
func (v Value) Duration() time.Duration { return time.Duration(int64(v.num)) }

// Time returns the value as time.Time, or the zero time. Valid for KindTime.
func (v Value) Time() time.Time {
	t, _ := v.a.(time.Time)
	return t
}

// Error returns the value as error, or nil. Valid for KindError.
func (v Value) Error() error {
	e, _ := v.a.(error)
	return e
}

// Group returns the value's child attributes, or nil. Valid for KindGroup.
func (v Value) Group() []Attr {
	g, _ := v.a.([]Attr)
	return g
}

// Any returns the value boxed into an interface, regardless of kind.
func (v Value) Any() any {
	switch v.kind {
	case KindString:
		return v.s
	case KindInt64:
		return int64(v.num)
	case KindUint64:
		return v.num
	case KindFloat64:
		return math.Float64frombits(v.num)
	case KindBool:
		return v.num != 0
	case KindDuration:
		return time.Duration(int64(v.num))
	case KindTime:
		return v.Time()
	case KindError:
		return v.Error()
	case KindGroup:
		return v.Group()
	default:
		return v.a
	}
}

// String renders the value to a string. For KindString it returns the string
// unchanged, so Value also satisfies fmt.Stringer.
func (v Value) String() string {
	switch v.kind {
	case KindString:
		return v.s
	case KindInt64:
		return strconv.FormatInt(int64(v.num), 10)
	case KindUint64:
		return strconv.FormatUint(v.num, 10)
	case KindFloat64:
		return strconv.FormatFloat(math.Float64frombits(v.num), 'g', -1, 64)
	case KindBool:
		return strconv.FormatBool(v.num != 0)
	case KindDuration:
		return time.Duration(int64(v.num)).String()
	case KindTime:
		return v.Time().String()
	case KindError:
		if e := v.Error(); e != nil {
			return e.Error()
		}
		return "<nil>"
	case KindGroup:
		return fmt.Sprint(v.Group())
	default:
		return fmt.Sprint(v.a)
	}
}

// String returns a string Attr.
func String(key, value string) Attr {
	return Attr{Key: key, Value: Value{kind: KindString, s: value}}
}

// Int returns an int Attr, stored as int64.
func Int(key string, value int) Attr {
	return Int64(key, int64(value))
}

// Int64 returns an int64 Attr.
func Int64(key string, value int64) Attr {
	return Attr{Key: key, Value: Value{kind: KindInt64, num: uint64(value)}}
}

// Uint64 returns a uint64 Attr.
func Uint64(key string, value uint64) Attr {
	return Attr{Key: key, Value: Value{kind: KindUint64, num: value}}
}

// Float64 returns a float64 Attr.
func Float64(key string, value float64) Attr {
	return Attr{Key: key, Value: Value{kind: KindFloat64, num: math.Float64bits(value)}}
}

// Bool returns a bool Attr.
func Bool(key string, value bool) Attr {
	var n uint64
	if value {
		n = 1
	}
	return Attr{Key: key, Value: Value{kind: KindBool, num: n}}
}

// Duration returns a time.Duration Attr.
func Duration(key string, value time.Duration) Attr {
	return Attr{Key: key, Value: Value{kind: KindDuration, num: uint64(value)}}
}

// Time returns a time.Time Attr.
func Time(key string, value time.Time) Attr {
	return Attr{Key: key, Value: Value{kind: KindTime, a: value}}
}

// Error returns an Attr with the conventional key "error". A nil err is kept as
// a nil-valued error Attr.
func Error(err error) Attr {
	return Attr{Key: "error", Value: Value{kind: KindError, a: err}}
}

// Int32 returns an int32 Attr, stored as int64.
func Int32(key string, value int32) Attr {
	return Int64(key, int64(value))
}

// NamedError returns an error Attr under an explicit key. Unlike Error, which
// uses the conventional "error" key, this lets callers name the field.
func NamedError(key string, err error) Attr {
	return Attr{Key: key, Value: Value{kind: KindError, a: err}}
}

// Stringer returns an Attr holding value.String(). A nil Stringer yields the
// string "<nil>".
func Stringer(key string, value fmt.Stringer) Attr {
	if value == nil {
		return String(key, "<nil>")
	}
	return String(key, value.String())
}

// Group returns an Attr whose value is the given child attributes. Adapters
// render it as a nested object. As a special case, an empty key inlines the
// children into the parent (like log/slog), matching zap's Inline.
func Group(key string, attrs ...Attr) Attr {
	return Attr{Key: key, Value: Value{kind: KindGroup, a: attrs}}
}

// Any returns an Attr holding an arbitrary value. Prefer the typed constructors
// for scalars: Any boxes the value into an interface.
func Any(key string, value any) Attr {
	return Attr{Key: key, Value: Value{kind: KindAny, a: value}}
}
