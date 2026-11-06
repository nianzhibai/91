package jx

// Num encodes number.
func (w *Writer) Num(v Num) bool {
	if len(v) == 0 {
		return w.Null()
	}
	return w.Raw(v)
}
