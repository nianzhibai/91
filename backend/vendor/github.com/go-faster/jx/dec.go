package jx

import (
	"io"
)

// Type of json value.
type Type int

func (t Type) String() string {
	switch t {
	case Invalid:
		return "invalid"
	case String:
		return "string"
	case Number:
		return "number"
	case Null:
		return "null"
	case Bool:
		return "bool"
	case Array:
		return "array"
	case Object:
		return "object"
	default:
		return "unknown"
	}
}

const (
	// Invalid json value.
	Invalid Type = iota
	// String json value, like "foo".
	String
	// Number json value, like 100 or 1.01.
	Number
	// Null json value.
	Null
	// Bool json value, true or false.
	Bool
	// Array json value, like [1, 2, 3].
	Array
	// Object json value, like {"foo": 1}.
	Object
)

var types []Type

func init() {
	types = make([]Type, 256)
	for i := range types {
		types[i] = Invalid
	}
	types['"'] = String
	types['-'] = Number
	types['0'] = Number
	types['1'] = Number
	types['2'] = Number
	types['3'] = Number
	types['4'] = Number
	types['5'] = Number
	types['6'] = Number
	types['7'] = Number
	types['8'] = Number
	types['9'] = Number
	types['t'] = Bool
	types['f'] = Bool
	types['n'] = Null
	types['['] = Array
	types['{'] = Object
}

// Decoder decodes json.
//
// Can decode from io.Reader or byte slice directly.
type Decoder struct {
	reader io.Reader

	// buf is current buffer.
	//
	// Contains full json if reader is nil or used as a read buffer
	// otherwise.
	buf  []byte
	head int // offset in buf to start of current json stream
	tail int // offset in buf to end of current json stream

	streamOffset int // for reader, offset in stream to start of current buf contents
	depth        int
}

const defaultBuf = 512

// Decode creates a Decoder that reads json from io.Reader.
func Decode(reader io.Reader, bufSize int) *Decoder {
	if bufSize <= 0 {
		bufSize = defaultBuf
	}
	return &Decoder{
		reader: reader,
		buf:    make([]byte, bufSize),
	}
}

// DecodeBytes creates a Decoder that reads json from byte slice.
func DecodeBytes(input []byte) *Decoder {
	return &Decoder{
		buf:  input,
		tail: len(input),
	}
}

// DecodeStr creates a Decoder that reads string as json.
func DecodeStr(input string) *Decoder {
	return DecodeBytes([]byte(input))
}

func (d *Decoder) offset() int {
	return d.streamOffset + d.head
}

// Reset resets reader and underlying state, next reads will use provided io.Reader.
func (d *Decoder) Reset(reader io.Reader) {
	d.reader = reader
	d.head = 0
	d.tail = 0
	d.depth = 0

	// Reads from reader need buffer.
	if cap(d.buf) == 0 {
		// Allocate new buffer if none.
		d.buf = make([]byte, defaultBuf)
	}
	if len(d.buf) == 0 {
		// Set buffer to full capacity if needed.
		d.buf = d.buf[:cap(d.buf)]
	}
}

// ResetBytes resets underlying state, next reads will use provided buffer.
func (d *Decoder) ResetBytes(input []byte) {
	d.reader = nil
	d.head = 0
	d.tail = len(input)
	d.depth = 0

	d.buf = input
}
