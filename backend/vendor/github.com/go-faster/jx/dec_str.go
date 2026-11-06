package jx

import (
	"io"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/go-faster/errors"
)

// StrAppend reads string and appends it to byte slice.
func (d *Decoder) StrAppend(b []byte) ([]byte, error) {
	v := value{
		buf: b,
		raw: false,
	}
	var err error
	if v, err = d.str(v); err != nil {
		return b, err
	}
	return v.buf, nil
}

type value struct {
	buf []byte
	raw bool // false forces buf reuse
}

func (v value) rune(r rune) value {
	return value{
		buf: appendRune(v.buf, r),
		raw: v.raw,
	}
}

func (d *Decoder) str(v value) (value, error) {
	if err := d.consume('"'); err != nil {
		return value{}, err
	}
	var (
		c byte
		i int
	)
	for {
		buf := d.buf[d.head:d.tail]
		for len(buf) >= 8 {
			c = buf[0]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			c = buf[1]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			c = buf[2]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			c = buf[3]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			c = buf[4]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			c = buf[5]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			c = buf[6]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			c = buf[7]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			buf = buf[8:]
		}
		var n int
		for n, c = range buf {
			if safeSet[c] != 0 {
				i += n
				goto readTok
			}
		}
		return d.strSlow(v)
	}
readTok:
	buf := d.buf[d.head:d.tail]
	str := buf[:i]

	switch {
	case c == '"':
		// Skip string + last quote.
		d.head += i + 1
		if v.raw {
			return value{buf: str, raw: true}, nil
		}
		return value{buf: append(v.buf, str...)}, nil
	case c == '\\':
		// Skip only string, keep quote in buffer.
		d.head += i
		// We need a copy anyway, because string is escaped.
		return d.strSlow(value{buf: append(v.buf, str...)})
	default:
		return v, badToken(c, d.offset()+i)
	}
}

func (d *Decoder) strSlow(v value) (value, error) {
	var (
		c byte
		i int
	)
readStr:
	for {
		i = 0
		buf := d.buf[d.head:d.tail]
		for len(buf) >= 8 {
			c = buf[0]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			c = buf[1]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			c = buf[2]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			c = buf[3]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			c = buf[4]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			c = buf[5]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			c = buf[6]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			c = buf[7]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			buf = buf[8:]
		}
		for _, c = range buf {
			if safeSet[c] != 0 {
				goto readTok
			}
			i++
		}

		v.buf = append(v.buf, d.buf[d.head:d.head+i]...)
		if err := d.read(); err != nil {
			if err == io.EOF {
				return value{}, io.ErrUnexpectedEOF
			}
			return value{}, err
		}
	}
readTok:
	buf := d.buf[d.head:d.tail]
	str := buf[:i]
	d.head += i + 1

	switch {
	case c == '"':
		return value{buf: append(v.buf, str...)}, nil
	case c == '\\':
		v.buf = append(v.buf, str...)
		c, err := d.byte()
		if err != nil {
			return value{}, err
		}
		v, err = d.escapedChar(v, c)
		if err != nil {
			return v, errors.Wrap(err, "escape")
		}
	default:
		return v, badToken(c, d.offset()-1)
	}
	goto readStr
}

// StrBytes returns string value as sub-slice of internal buffer.
//
// Bytes are valid only until next call to any Decoder method.
func (d *Decoder) StrBytes() ([]byte, error) {
	v, err := d.str(value{raw: true})
	if err != nil {
		return nil, err
	}
	return v.buf, nil
}

// Str reads string.
func (d *Decoder) Str() (string, error) {
	s, err := d.StrBytes()
	if err != nil {
		return "", err
	}
	return string(s), nil
}

func (d *Decoder) escapedChar(v value, c byte) (value, error) {
	switch val := escapedStrSet[c]; val {
	default:
		v.buf = append(v.buf, val)
	case 'u':
		r1, err := d.readU4()
		if err != nil {
			return value{}, errors.Wrap(err, "read u4")
		}
		if utf16.IsSurrogate(r1) {
			c, err := d.byte()
			if err != nil {
				return value{}, err
			}
			if c != '\\' {
				d.unread()
				return v.rune(r1), nil
			}
			c, err = d.byte()
			if err != nil {
				return value{}, err
			}
			if c != 'u' {
				return d.escapedChar(v.rune(r1), c)
			}
			r2, err := d.readU4()
			if err != nil {
				return value{}, err
			}
			combined := utf16.DecodeRune(r1, r2)
			if combined == '\uFFFD' {
				v = v.rune(r1).rune(r2)
			} else {
				v = v.rune(combined)
			}
		} else {
			v = v.rune(r1)
		}
	case 0:
		err := badToken(c, d.offset()-1)
		return v, errors.Wrap(err, "bad escape")
	}
	return v, nil
}

func (d *Decoder) readU4() (v rune, _ error) {
	var (
		offset = d.offset()
		b      [4]byte
	)
	if err := d.readExact4(&b); err != nil {
		return 0, err
	}
	for i, c := range b {
		val := hexSet[c]
		if val == 0 {
			return 0, badToken(c, offset+i)
		}
		v = v*16 + rune(val-1)
	}
	return v, nil
}

func appendRune(p []byte, r rune) []byte {
	buf := make([]byte, 4)
	n := utf8.EncodeRune(buf, r)
	return append(p, buf[:n]...)
}
