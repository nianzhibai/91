package jx

// Str encodes string without html escaping.
//
// Use StrEscape to escape html, this is default for encoding/json and
// should be used by default for untrusted strings.
func (e *Encoder) Str(v string) bool {
	return e.comma() ||
		e.w.Str(v)
}

// ByteStr encodes byte slice without html escaping.
//
// Use ByteStrEscape to escape html, this is default for encoding/json and
// should be used by default for untrusted strings.
func (e *Encoder) ByteStr(v []byte) bool {
	return e.comma() ||
		e.w.ByteStr(v)
}
