package jx

import "io"

// Encoder encodes json to underlying buffer.
//
// Zero value is valid.
type Encoder struct {
	w      Writer // underlying writer
	indent int    // count of spaces for single indentation level

	// first handles state for comma and indentation writing.
	//
	// New Object or Array appends new level to this slice, and
	// last element of this slice denotes whether first element was written.
	//
	// We write commas only before non-first element of Array or Object.
	//
	// See comma, begin, end and FieldStart for implementation details.
	//
	// Note: probably, this can be optimized as bit set to ease memory
	// consumption.
	//
	// See https://yourbasic.org/algorithms/your-basic-int/#simple-sets
	first []bool
}

// Write implements io.Writer.
func (e *Encoder) Write(p []byte) (n int, err error) {
	return e.w.Write(p)
}

// WriteTo implements io.WriterTo.
func (e *Encoder) WriteTo(w io.Writer) (n int64, err error) {
	return e.w.WriteTo(w)
}

// SetIdent sets length of single indentation step.
func (e *Encoder) SetIdent(n int) {
	e.indent = n
}

// String returns string of underlying buffer.
func (e Encoder) String() string {
	return e.w.String()
}

// Reset resets underlying buffer.
//
// If e is in streaming mode, it is reset to non-streaming mode.
func (e *Encoder) Reset() {
	e.w.Reset()
	e.first = e.first[:0]
}

// ResetWriter resets underlying buffer and sets output writer.
func (e *Encoder) ResetWriter(out io.Writer) {
	e.w.ResetWriter(out)
	e.first = e.first[:0]
}

// Grow grows the underlying buffer
func (e *Encoder) Grow(n int) {
	e.w.Grow(n)
}

// Bytes returns underlying buffer.
func (e Encoder) Bytes() []byte { return e.w.Buf }

// SetBytes sets underlying buffer.
func (e *Encoder) SetBytes(buf []byte) { e.w.Buf = buf }

// byte writes a single byte.
func (e *Encoder) byte(c byte) bool {
	return e.w.byte(c)
}

// RawStr writes string as raw json.
func (e *Encoder) RawStr(v string) bool {
	return e.comma() ||
		e.w.RawStr(v)
}

// Raw writes byte slice as raw json.
func (e *Encoder) Raw(b []byte) bool {
	return e.comma() ||
		e.w.Raw(b)
}

// Null writes null.
func (e *Encoder) Null() bool {
	return e.comma() ||
		e.w.Null()
}

// Bool encodes boolean.
func (e *Encoder) Bool(v bool) bool {
	return e.comma() ||
		e.w.Bool(v)
}

// ObjStart writes object start, performing indentation if needed.
//
// Use Obj as convenience helper for writing objects.
func (e *Encoder) ObjStart() (fail bool) {
	fail = e.comma() || e.w.ObjStart()
	e.begin()
	return fail || e.writeIndent()
}

// FieldStart encodes field name and writes colon.
//
// For non-zero indentation also writes single space after colon.
//
// Use Field as convenience helper for encoding fields.
func (e *Encoder) FieldStart(field string) (fail bool) {
	fail = e.comma() || e.w.FieldStart(field)
	if e.indent > 0 {
		fail = fail || e.byte(' ')
	}
	if len(e.first) > 0 {
		e.first[e.current()] = true
	}
	return fail
}

// Field encodes field start and then invokes callback.
//
// Has ~5ns overhead over FieldStart.
func (e *Encoder) Field(name string, f func(e *Encoder)) (fail bool) {
	fail = e.FieldStart(name)
	// TODO(tdakkota): return bool from f?
	f(e)
	return fail
}

// ObjEnd writes end of object token, performing indentation if needed.
//
// Use Obj as convenience helper for writing objects.
func (e *Encoder) ObjEnd() bool {
	e.end()
	return e.writeIndent() || e.w.ObjEnd()
}

// ObjEmpty writes empty object.
func (e *Encoder) ObjEmpty() bool {
	return e.comma() ||
		e.w.ObjStart() ||
		e.w.ObjEnd()
}

// Obj writes start of object, invokes callback and writes end of object.
//
// If callback is nil, writes empty object.
func (e *Encoder) Obj(f func(e *Encoder)) (fail bool) {
	if f == nil {
		return e.ObjEmpty()
	}
	fail = e.ObjStart()
	// TODO(tdakkota): return bool from f?
	f(e)
	return fail || e.ObjEnd()
}

// ArrStart writes start of array, performing indentation if needed.
//
// Use Arr as convenience helper for writing arrays.
func (e *Encoder) ArrStart() (fail bool) {
	fail = e.comma() || e.w.ArrStart()
	e.begin()
	return fail || e.writeIndent()
}

// ArrEmpty writes empty array.
func (e *Encoder) ArrEmpty() bool {
	return e.comma() ||
		e.w.ArrStart() ||
		e.w.ArrEnd()
}

// ArrEnd writes end of array, performing indentation if needed.
//
// Use Arr as convenience helper for writing arrays.
func (e *Encoder) ArrEnd() bool {
	e.end()
	return e.writeIndent() ||
		e.w.ArrEnd()
}

// Arr writes start of array, invokes callback and writes end of array.
//
// If callback is nil, writes empty array.
func (e *Encoder) Arr(f func(e *Encoder)) (fail bool) {
	if f == nil {
		return e.ArrEmpty()
	}
	fail = e.ArrStart()
	// TODO(tdakkota): return bool from f?
	f(e)
	return fail || e.ArrEnd()
}

func (e *Encoder) writeIndent() (fail bool) {
	if e.indent == 0 {
		return false
	}
	fail = e.byte('\n')
	for i := 0; i < len(e.first)*e.indent && !fail; i++ {
		fail = fail || e.byte(' ')
	}
	return fail
}
