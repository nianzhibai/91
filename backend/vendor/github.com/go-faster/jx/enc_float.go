package jx

// Float32 encodes float32.
//
// NB: Infinities and NaN are represented as null.
func (e *Encoder) Float32(v float32) bool {
	return e.comma() ||
		e.w.Float32(v)
}

// Float64 encodes float64.
//
// NB: Infinities and NaN are represented as null.
func (e *Encoder) Float64(v float64) bool {
	return e.comma() ||
		e.w.Float64(v)
}
