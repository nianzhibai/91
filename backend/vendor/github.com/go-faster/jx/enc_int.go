package jx

// Int encodes int.
func (e *Encoder) Int(v int) bool {
	return e.comma() ||
		e.w.Int(v)
}

// UInt encodes uint.
func (e *Encoder) UInt(v uint) bool {
	return e.comma() ||
		e.w.UInt(v)
}

// UInt8 encodes uint8.
func (e *Encoder) UInt8(v uint8) bool {
	return e.comma() ||
		e.w.UInt8(v)
}

// Int8 encodes int8.
func (e *Encoder) Int8(v int8) bool {
	return e.comma() ||
		e.w.Int8(v)
}
