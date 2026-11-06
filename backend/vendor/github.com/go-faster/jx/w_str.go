package jx

import (
	"github.com/go-faster/jx/internal/byteseq"
)

const hexChars = "0123456789abcdef"

// safeSet holds the value true if the ASCII character with the given array
// position can be represented inside a JSON string without any further
// escaping.
//
// All values are true except for the ASCII control characters (0-31), the
// double quote ("), and the backslash character ("\").
var safeSet = [256]byte{
	// First 31 characters.
	1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1,
	'"':  1,
	'\\': 1,
}

// Str encodes string without html escaping.
//
// Use StrEscape to escape html, this is default for encoding/json and
// should be used by default for untrusted strings.
func (w *Writer) Str(v string) bool {
	return writeStr(w, v)
}

// ByteStr encodes string without html escaping.
//
// Use ByteStrEscape to escape html, this is default for encoding/json and
// should be used by default for untrusted strings.
func (w *Writer) ByteStr(v []byte) bool {
	return writeStr(w, v)
}

func writeStr[S byteseq.Byteseq](w *Writer, v S) (fail bool) {
	fail = w.byte('"')

	// Fast path, without utf8 and escape support.
	var (
		i      = 0
		length = len(v)
	)
	for ; i < length && !fail; i++ {
		c := v[i]
		if safeSet[c] != 0 {
			break
		}
	}
	fail = fail || writeStreamByteseq(w, v[:i])
	if i == length {
		return fail || w.byte('"')
	}
	return fail || strSlow[S](w, v[i:])
}

func strSlow[S byteseq.Byteseq](w *Writer, v S) (fail bool) {
	var i, start int
	// for the remaining parts, we process them char by char
	for i < len(v) && !fail {
		b := v[i]
		if safeSet[b] == 0 {
			i++
			continue
		}
		if start < i {
			fail = fail || writeStreamByteseq(w, v[start:i])
		}

		switch b {
		case '\\', '"':
			fail = fail || w.twoBytes('\\', b)
		case '\n':
			fail = fail || w.twoBytes('\\', 'n')
		case '\r':
			fail = fail || w.twoBytes('\\', 'r')
		case '\t':
			fail = fail || w.twoBytes('\\', 't')
		default:
			// This encodes bytes < 0x20 except for \t, \n and \r.
			// If escapeHTML is set, it also escapes <, >, and &
			// because they can lead to security holes when
			// user-controlled strings are rendered into JSON
			// and served to some browsers.
			fail = fail || w.rawStr(`\u00`) || w.twoBytes(hexChars[b>>4], hexChars[b&0xF])
		}
		i++
		start = i
		continue
	}
	if start < len(v) {
		fail = fail || writeStreamByteseq(w, v[start:])
	}
	return fail || w.byte('"')
}
