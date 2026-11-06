package jx

// StrEscape encodes string with html special characters escaping.
func (e *Encoder) StrEscape(v string) bool {
	return e.comma() ||
		e.w.StrEscape(v)
}

// ByteStrEscape encodes string with html special characters escaping.
func (e *Encoder) ByteStrEscape(v []byte) bool {
	return e.comma() ||
		e.w.ByteStrEscape(v)
}
