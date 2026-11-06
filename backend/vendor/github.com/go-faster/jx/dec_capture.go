package jx

import (
	"bytes"
	"io"
)

// Capture calls f and then rolls back to state before call.
func (d *Decoder) Capture(f func(d *Decoder) error) error {
	if f == nil {
		return nil
	}

	if d.reader != nil {
		// TODO(tdakkota): May it be more efficient?
		var (
			buf          bytes.Buffer
			streamOffset = d.streamOffset
		)
		reader := io.TeeReader(d.reader, &buf)
		defer func() {
			d.reader = io.MultiReader(&buf, d.reader)
			d.streamOffset = streamOffset
		}()
		d.reader = reader
	}
	head, tail, depth := d.head, d.tail, d.depth
	err := f(d)
	d.head, d.tail, d.depth = head, tail, depth
	return err
}
