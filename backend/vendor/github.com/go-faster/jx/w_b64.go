package jx

import (
	stdbase64 "encoding/base64"

	"github.com/segmentio/asm/base64"
)

// Base64 encodes data as standard base64 encoded string.
//
// Same as encoding/json, base64.StdEncoding or RFC 4648.
func (w *Writer) Base64(data []byte) bool {
	if data == nil {
		return w.Null()
	}

	if w.byte('"') {
		return true
	}

	encodedLen := base64.StdEncoding.EncodedLen(len(data))
	switch {
	case w.stream == nil || len(w.Buf)+encodedLen <= cap(w.Buf):
		start := len(w.Buf)
		w.Buf = append(w.Buf, make([]byte, encodedLen)...)
		base64.StdEncoding.Encode(w.Buf[start:], data)
	default:
		s := w.stream

		var fail bool
		w.Buf, fail = s.flush(w.Buf)
		if fail {
			return true
		}
		e := stdbase64.NewEncoder(stdbase64.StdEncoding, s.writer)
		if _, err := e.Write(data); err != nil {
			s.setError(err)
			return true
		}
		if err := e.Close(); err != nil {
			s.setError(err)
			return true
		}
	}

	return w.byte('"')
}
