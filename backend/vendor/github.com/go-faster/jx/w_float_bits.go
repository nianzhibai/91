// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jx

import (
	"math"
	"strconv"
)

// Float writes float value to buffer.
func (w *Writer) Float(v float64, bits int) bool {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		// Like in ECMA:
		// NaN and Infinity regardless of sign are represented
		// as the String null.
		//
		// JSON.stringify({"foo":NaN}) -> {"foo":null}
		return w.Null()
	}

	switch s := w.stream; {
	case s == nil:
		w.Buf = floatAppend(w.Buf, v, bits)
		return false
	case s.fail():
		return true
	default:
		tmp := make([]byte, 0, 32)
		tmp = floatAppend(tmp, v, bits)
		return writeStreamByteseq(w, tmp)
	}
}

func floatAppend(b []byte, v float64, bits int) []byte {
	// From go std sources, strconv/ftoa.go:

	// Convert as if by ES6 number to string conversion.
	// This matches most other JSON generators.
	// See golang.org/issue/6384 and golang.org/issue/14135.
	// Like fmt %g, but the exponent cutoffs are different
	// and exponents themselves are not padded to two digits.
	abs := math.Abs(v)
	fmt := byte('f')
	// Note: Must use float32 comparisons for underlying float32 value to get precise cutoffs right.
	if abs != 0 {
		if bits == 64 && (abs < 1e-6 || abs >= 1e21) || bits == 32 && (float32(abs) < 1e-6 || float32(abs) >= 1e21) {
			fmt = 'e'
		}
	}
	b = strconv.AppendFloat(b, v, fmt, -1, bits)
	if fmt == 'e' {
		// clean up e-09 to e-9
		n := len(b)
		if n >= 4 && b[n-4] == 'e' && b[n-3] == '-' && b[n-2] == '0' {
			b[n-2] = b[n-1]
			b = b[:n-1]
		}
	}
	return b
}
