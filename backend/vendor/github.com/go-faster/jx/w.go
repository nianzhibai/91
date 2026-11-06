package jx

import (
	"bytes"
	"io"
)

// Writer writes json tokens to underlying buffer.
//
// Zero value is valid.
type Writer struct {
	Buf    []byte // underlying buffer
	stream *streamState
}

// Write implements io.Writer.
func (w *Writer) Write(p []byte) (n int, err error) {
	if w.stream != nil {
		if len(w.Buf) > 0 {
			if w.Flush() {
				return 0, w.stream.writeErr
			}
		}
		return w.stream.writer.Write(p)
	}
	w.Buf = append(w.Buf, p...)
	return len(p), nil
}

// WriteTo implements io.WriterTo.
func (w *Writer) WriteTo(t io.Writer) (n int64, err error) {
	if w.stream != nil {
		return 0, errStreaming
	}
	wrote, err := t.Write(w.Buf)
	return int64(wrote), err
}

// String returns string of underlying buffer.
func (w Writer) String() string {
	w.stream.mustNotBeStreaming()
	return string(w.Buf)
}

// Reset resets underlying buffer.
//
// If w is in streaming mode, it is reset to non-streaming mode.
func (w *Writer) Reset() {
	w.Buf = w.Buf[:0]
	w.stream = nil
}

// ResetWriter resets underlying buffer and sets output writer.
func (w *Writer) ResetWriter(out io.Writer) {
	w.Buf = w.Buf[:0]
	if w.stream == nil {
		w.stream = newStreamState(out)
	}
	w.stream.Reset(out)
}

// Grow grows the underlying buffer.
//
// Calls (*bytes.Buffer).Grow(n int) on w.Buf.
func (w *Writer) Grow(n int) {
	buf := bytes.NewBuffer(w.Buf)
	buf.Grow(n)
	w.Buf = buf.Bytes()
}

// Flush flushes the stream. It does nothing if not in streaming mode
func (w *Writer) Flush() (fail bool) {
	if w.stream != nil {
		w.Buf, fail = w.stream.flush(w.Buf)
		if fail {
			return true
		}
	}
	return false
}

// byte writes a single byte.
func (w *Writer) byte(c byte) (fail bool) {
	if w.stream == nil {
		w.Buf = append(w.Buf, c)
		return false
	}
	return writeStreamBytes(w, c)
}

func (w *Writer) twoBytes(c1, c2 byte) bool {
	if w.stream == nil {
		w.Buf = append(w.Buf, c1, c2)
		return false
	}
	return writeStreamBytes(w, c1, c2)
}

// RawStr writes string as raw json.
func (w *Writer) RawStr(v string) bool {
	return w.rawStr(v)
}

func (w *Writer) rawStr(v string) bool {
	return writeStreamByteseq(w, v)
}

// Raw writes byte slice as raw json.
func (w *Writer) Raw(b []byte) bool {
	return writeStreamByteseq(w, b)
}

// Null writes null.
func (w *Writer) Null() bool {
	return writeStreamByteseq(w, "null")
}

// True writes true.
func (w *Writer) True() bool {
	return writeStreamByteseq(w, "true")
}

// False writes false.
func (w *Writer) False() bool {
	return writeStreamByteseq(w, "false")
}

// Bool encodes boolean.
func (w *Writer) Bool(v bool) bool {
	if v {
		return w.True()
	}
	return w.False()
}

// ObjStart writes object start.
func (w *Writer) ObjStart() bool {
	return w.byte('{')
}

// FieldStart encodes field name and writes colon.
func (w *Writer) FieldStart(field string) bool {
	return w.Str(field) ||
		w.byte(':')
}

// ObjEnd writes end of object token.
func (w *Writer) ObjEnd() bool {
	return w.byte('}')
}

// ArrStart writes start of array.
func (w *Writer) ArrStart() bool {
	return w.byte('[')
}

// ArrEnd writes end of array.
func (w *Writer) ArrEnd() bool {
	return w.byte(']')
}

// Comma writes comma.
func (w *Writer) Comma() bool {
	return w.byte(',')
}
