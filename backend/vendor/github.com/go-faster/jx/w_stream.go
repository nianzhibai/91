package jx

import (
	"errors"
	"io"

	"github.com/go-faster/jx/internal/byteseq"
)

// Close flushes underlying buffer to writer in streaming mode.
// Otherwise, it does nothing.
func (w *Writer) Close() error {
	if w.stream == nil {
		return nil
	}
	_, fail := w.stream.flush(w.Buf)
	if fail {
		return w.stream.writeErr
	}
	return nil
}

var errStreaming = errors.New("unexpected call in streaming mode")

type streamState struct {
	writer   io.Writer
	writeErr error
}

func newStreamState(w io.Writer) *streamState {
	return &streamState{
		writer: w,
	}
}

func (s *streamState) mustNotBeStreaming() {
	if s != nil {
		panic(errStreaming)
	}
}

func (s *streamState) Reset(w io.Writer) {
	s.writer = w
	s.writeErr = nil
}

func (s *streamState) setError(err error) {
	s.writeErr = err
}

func (s *streamState) fail() bool {
	return s.writeErr != nil
}

func (s *streamState) flush(buf []byte) ([]byte, bool) {
	if s.fail() {
		return nil, true
	}

	n, err := s.writer.Write(buf)
	switch {
	case err != nil:
		s.setError(err)
		return nil, true
	case n != len(buf):
		s.setError(io.ErrShortWrite)
		return nil, true
	default:
		buf = buf[:0]
		return buf, false
	}
}

func writeStreamBytes(w *Writer, s ...byte) bool {
	return writeStreamByteseq(w, s)
}

func writeStreamByteseq[S byteseq.Byteseq](w *Writer, s S) bool {
	if w.stream == nil {
		w.Buf = append(w.Buf, s...)
		return false
	}
	return writeStreamByteseqSlow(w, s)
}

func writeStreamByteseqSlow[S byteseq.Byteseq](w *Writer, s S) bool {
	if w.stream.fail() {
		return true
	}

	for len(w.Buf)+len(s) > cap(w.Buf) {
		var fail bool
		w.Buf, fail = w.stream.flush(w.Buf)
		if fail {
			return true
		}
		n := copy(w.Buf[len(w.Buf):cap(w.Buf)], s)
		s = s[n:]
		w.Buf = w.Buf[:len(w.Buf)+n]
	}
	w.Buf = append(w.Buf, s...)
	return false
}
