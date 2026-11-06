package jx

import "io"

const (
	encoderBufSize    = 512
	minEncoderBufSize = 32
)

// NewStreamingEncoder creates new streaming encoder.
func NewStreamingEncoder(w io.Writer, bufSize int) *Encoder {
	switch {
	case bufSize < 0:
		bufSize = encoderBufSize
	case bufSize < minEncoderBufSize:
		bufSize = minEncoderBufSize
	}
	return &Encoder{
		w: Writer{
			Buf:    make([]byte, 0, bufSize),
			stream: newStreamState(w),
		},
	}
}

// Close flushes underlying buffer to writer in streaming mode.
// Otherwise, it does nothing.
func (e *Encoder) Close() error {
	return e.w.Close()
}
