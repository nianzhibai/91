package jx

// Int encodes int.
func (w *Writer) Int(v int) bool {
	return w.Int64(int64(v))
}

// UInt encodes uint.
func (w *Writer) UInt(v uint) bool {
	return w.UInt64(uint64(v))
}

// UInt8 encodes uint8.
func (w *Writer) UInt8(v uint8) bool {
	// v is always smaller than digits size (1000)
	return writeFirstBuf(w, digits[v])
}

// Int8 encodes int8.
func (w *Writer) Int8(v int8) (fail bool) {
	var val uint8
	if v < 0 {
		val = uint8(-v)
		fail = w.byte('-')
	} else {
		val = uint8(v)
	}
	return fail || w.UInt8(val)
}
