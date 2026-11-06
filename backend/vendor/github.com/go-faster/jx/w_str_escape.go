package jx

import (
	"unicode/utf8"

	"github.com/go-faster/jx/internal/byteseq"
)

// htmlSafeSet holds the value true if the ASCII character with the given
// array position can be safely represented inside a JSON string, embedded
// inside of HTML <script> tags, without any additional escaping.
//
// All values are true except for the ASCII control characters (0-31), the
// double quote ("), the backslash character ("\"), HTML opening and closing
// tags ("<" and ">"), and the ampersand ("&").
var htmlSafeSet = [utf8.RuneSelf]bool{
	' ':      true,
	'!':      true,
	'"':      false,
	'#':      true,
	'$':      true,
	'%':      true,
	'&':      false,
	'\'':     true,
	'(':      true,
	')':      true,
	'*':      true,
	'+':      true,
	',':      true,
	'-':      true,
	'.':      true,
	'/':      true,
	'0':      true,
	'1':      true,
	'2':      true,
	'3':      true,
	'4':      true,
	'5':      true,
	'6':      true,
	'7':      true,
	'8':      true,
	'9':      true,
	':':      true,
	';':      true,
	'<':      false,
	'=':      true,
	'>':      false,
	'?':      true,
	'@':      true,
	'A':      true,
	'B':      true,
	'C':      true,
	'D':      true,
	'E':      true,
	'F':      true,
	'G':      true,
	'H':      true,
	'I':      true,
	'J':      true,
	'K':      true,
	'L':      true,
	'M':      true,
	'N':      true,
	'O':      true,
	'P':      true,
	'Q':      true,
	'R':      true,
	'S':      true,
	'T':      true,
	'U':      true,
	'V':      true,
	'W':      true,
	'X':      true,
	'Y':      true,
	'Z':      true,
	'[':      true,
	'\\':     false,
	']':      true,
	'^':      true,
	'_':      true,
	'`':      true,
	'a':      true,
	'b':      true,
	'c':      true,
	'd':      true,
	'e':      true,
	'f':      true,
	'g':      true,
	'h':      true,
	'i':      true,
	'j':      true,
	'k':      true,
	'l':      true,
	'm':      true,
	'n':      true,
	'o':      true,
	'p':      true,
	'q':      true,
	'r':      true,
	's':      true,
	't':      true,
	'u':      true,
	'v':      true,
	'w':      true,
	'x':      true,
	'y':      true,
	'z':      true,
	'{':      true,
	'|':      true,
	'}':      true,
	'~':      true,
	'\u007f': true,
}

// StrEscape encodes string with html special characters escaping.
func (w *Writer) StrEscape(v string) bool {
	return strEscape(w, v)
}

// ByteStrEscape encodes string with html special characters escaping.
func (w *Writer) ByteStrEscape(v []byte) bool {
	return strEscape(w, v)
}

func strEscape[S byteseq.Byteseq](w *Writer, v S) (fail bool) {
	fail = w.byte('"')

	// Fast path, probably does not require escaping.
	var (
		i      = 0
		length = len(v)
	)
	for ; i < length && !fail; i++ {
		c := v[i]
		if c >= utf8.RuneSelf || !(htmlSafeSet[c]) {
			break
		}
	}
	fail = fail || writeStreamByteseq(w, v[:i])
	if i == length {
		return fail || w.byte('"')
	}
	return fail || strEscapeSlow[S](w, i, v, length)
}

func strEscapeSlow[S byteseq.Byteseq](w *Writer, i int, v S, valLen int) (fail bool) {
	start := i
	// for the remaining parts, we process them char by char
	for i < valLen && !fail {
		if b := v[i]; b < utf8.RuneSelf {
			if htmlSafeSet[b] {
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
		c, size := byteseq.DecodeRuneInByteseq(v[i:])
		if c == utf8.RuneError && size == 1 {
			if start < i {
				fail = fail || writeStreamByteseq(w, v[start:i])
			}
			fail = fail || w.rawStr(`\ufffd`)
			i++
			start = i
			continue
		}
		// U+2028 is LINE SEPARATOR.
		// U+2029 is PARAGRAPH SEPARATOR.
		// They are both technically valid characters in JSON strings,
		// but don't work in JSONP, which has to be evaluated as JavaScript,
		// and can lead to security holes there. It is valid JSON to
		// escape them, so we do so unconditionally.
		// See http://timelessrepo.com/json-isnt-a-javascript-subset for discussion.
		if c == '\u2028' || c == '\u2029' {
			if start < i {
				fail = fail || writeStreamByteseq(w, v[start:i])
			}
			fail = fail || w.rawStr(`\u202`) || w.byte(hexChars[c&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	if start < len(v) {
		fail = fail || writeStreamByteseq(w, v[start:])
	}
	return fail || w.byte('"')
}
