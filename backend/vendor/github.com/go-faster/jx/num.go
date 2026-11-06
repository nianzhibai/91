package jx

import (
	"bytes"
	"fmt"
	"math/big"

	"github.com/go-faster/errors"
)

// Num represents number, which can be raw json number or number string.
//
// Same as Raw, but with number invariants.
//
// Examples:
//
//	123.45   // Str: false, IsInt: false
//	"123.45" // Str: true,  IsInt: false
//	"12345"  // Str: true,  IsInt: true
//	12345    // Str: false, IsInt: true
type Num []byte

func (n Num) dec() Decoder {
	head := 0
	tail := len(n)
	if n.Str() {
		head = 1
		tail--
	}
	return Decoder{
		buf:  n,
		head: head,
		tail: tail,
	}
}

// Str reports whether Num is string number.
func (n Num) Str() bool {
	return len(n) > 0 && n[0] == '"'
}

func (n Num) floatAsInt() (dotIdx int, _ error) {
	// Allow decoding floats with zero fractional, like 1.0 as 1.
	dotIdx = -1
	for i, c := range n {
		if c == '.' {
			dotIdx = i
			continue
		}
		if dotIdx == -1 {
			continue
		}
		switch c {
		case '0', '"': // ok
		default:
			return dotIdx, errors.Errorf("non-zero fractional part %q at %d", c, i)
		}
	}
	return dotIdx, nil
}

// Int64 decodes number as a signed 64-bit integer.
// Works on floats with zero fractional part.
func (n Num) Int64() (int64, error) {
	dotIdx, err := n.floatAsInt()
	if err != nil {
		return 0, errors.Wrap(err, "float as int")
	}
	d := n.dec()
	if dotIdx != -1 {
		d.tail = dotIdx
	}
	return d.Int64()
}

// IsInt reports whether number is integer.
func (n Num) IsInt() bool {
	if len(n) == 0 {
		return false
	}
	b := n
	if b[0] == '"' {
		b = b[1 : len(b)-1]
	}
	if b[0] == '-' {
		b = b[1:]
	}
	for _, c := range b {
		switch c {
		case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9': // ok
		default:
			return false
		}
	}
	return true
}

// Uint64 decodes number as an unsigned 64-bit integer.
// Works on floats with zero fractional part.
func (n Num) Uint64() (uint64, error) {
	dotIdx, err := n.floatAsInt()
	if err != nil {
		return 0, errors.Wrap(err, "float as int")
	}
	d := n.dec()
	if dotIdx != -1 {
		d.tail = dotIdx
	}
	return d.UInt64()
}

// Float64 decodes number as 64-bit floating point.
func (n Num) Float64() (float64, error) {
	d := n.dec()
	return d.Float64()
}

// Equal reports whether numbers are strictly equal, including their formats.
func (n Num) Equal(v Num) bool {
	return bytes.Equal(n, v)
}

func (n Num) String() string {
	if len(n) == 0 {
		return "<invalid>"
	}
	return string(n)
}

// Format implements fmt.Formatter.
func (n Num) Format(f fmt.State, verb rune) {
	switch verb {
	case 's', 'v':
		_, _ = f.Write(n)
	case 'd':
		d, err := n.Int64()
		if err != nil {
			_, _ = fmt.Fprintf(f, "%%!invalid(Num=%s)", n.String())
			return
		}
		v := big.NewInt(d)
		v.Format(f, verb)
	case 'f':
		d, err := n.Float64()
		if err != nil {
			_, _ = fmt.Fprintf(f, "%%!invalid(Num=%s)", n.String())
			return
		}
		v := big.NewFloat(d)
		v.Format(f, verb)
	}
}

// Sign reports sign of number.
//
// 0 is zero, 1 is positive, -1 is negative.
func (n Num) Sign() int {
	if len(n) == 0 {
		return 0
	}
	c := n[0]
	if c == '"' {
		if len(n) < 2 {
			return 0
		}
		c = n[1]
	}
	switch c {
	case '-':
		return -1
	case '0':
		return 0
	default:
		return 1
	}
}

// Positive reports whether number is positive.
func (n Num) Positive() bool { return n.Sign() > 0 }

// Negative reports whether number is negative.
func (n Num) Negative() bool { return n.Sign() < 0 }

// Zero reports whether number is zero.
func (n Num) Zero() bool {
	if len(n) == 0 {
		return false
	}
	if len(n) == 1 {
		return n[0] == '0'
	}
	for _, c := range n {
		switch c {
		case '.', '0', '-':
			continue
		default:
			return false
		}
	}
	return true
}
