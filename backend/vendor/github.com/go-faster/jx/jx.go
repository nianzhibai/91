// Package jx implements RFC 7159 json encoding and decoding.
package jx

import (
	"sync"
)

// Valid reports whether data is valid json.
func Valid(data []byte) bool {
	d := GetDecoder()
	defer PutDecoder(d)
	d.ResetBytes(data)
	return d.Validate() == nil
}

var (
	encPool = &sync.Pool{
		New: func() interface{} {
			return &Encoder{}
		},
	}
	writerPool = &sync.Pool{
		New: func() interface{} {
			return &Writer{}
		},
	}
	decPool = &sync.Pool{
		New: func() interface{} {
			return &Decoder{}
		},
	}
)

// GetDecoder gets *Decoder from pool.
func GetDecoder() *Decoder {
	return decPool.Get().(*Decoder)
}

// PutDecoder puts *Decoder into pool.
func PutDecoder(d *Decoder) {
	d.Reset(nil)
	decPool.Put(d)
}

// GetEncoder returns *Encoder from pool.
func GetEncoder() *Encoder {
	return encPool.Get().(*Encoder)
}

// PutEncoder puts *Encoder to pool
func PutEncoder(e *Encoder) {
	e.Reset()
	e.SetIdent(0)
	encPool.Put(e)
}

// GetWriter returns *Writer from pool.
func GetWriter() *Writer {
	return writerPool.Get().(*Writer)
}

// PutWriter puts *Writer to pool
func PutWriter(e *Writer) {
	e.Reset()
	writerPool.Put(e)
}
