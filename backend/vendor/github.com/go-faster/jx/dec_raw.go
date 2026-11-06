package jx

import (
	"io"

	"github.com/go-faster/errors"
)

type rawReader struct {
	// internal buffer, may be reference to *Decoder.buf.
	buf []byte
	// if true, buf is reference to  *Decoder.buf.
	captured bool
	orig     io.Reader
}

func (r *rawReader) Read(p []byte) (n int, err error) {
	if r.captured {
		// Make a copy.
		r.buf = append([]byte(nil), r.buf...)
		r.captured = false
	}
	n, err = r.orig.Read(p)
	if n > 0 {
		r.buf = append(r.buf, p[:n]...)
	}
	return n, err
}

// Raw is like Skip(), but saves and returns skipped value as raw json.
//
// Do not retain returned value, it references underlying buffer.
func (d *Decoder) Raw() (Raw, error) {
	start := d.head
	if orig := d.reader; orig != nil {
		rr := &rawReader{
			buf:      d.buf[start:d.tail],
			captured: true,
			orig:     orig,
		}
		d.reader = rr
		defer func() {
			d.reader = orig
		}()

		if err := d.Skip(); err != nil {
			return nil, errors.Wrap(err, "skip")
		}

		unread := d.tail - d.head
		raw := rr.buf
		raw = raw[:len(raw)-unread]
		return raw, nil
	}

	if err := d.Skip(); err != nil {
		return nil, errors.Wrap(err, "skip")
	}

	return d.buf[start:d.head], nil
}

// RawAppend is Raw that appends saved raw json value to buf.
func (d *Decoder) RawAppend(buf Raw) (Raw, error) {
	raw, err := d.Raw()
	if err != nil {
		return nil, err
	}
	return append(buf, raw...), err
}

// Raw json value.
type Raw []byte

// Type of Raw json value.
func (r Raw) Type() Type {
	d := Decoder{buf: r, tail: len(r)}
	return d.Next()
}

func (r Raw) String() string { return string(r) }
