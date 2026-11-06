package jx

// Num encodes number.
func (e *Encoder) Num(v Num) bool {
	return e.comma() ||
		e.w.Num(v)
}
