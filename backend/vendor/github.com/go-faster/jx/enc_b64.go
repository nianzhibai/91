package jx

// Base64 encodes data as standard base64 encoded string.
//
// Same as encoding/json, base64.StdEncoding or RFC 4648.
func (e *Encoder) Base64(data []byte) bool {
	return e.comma() ||
		e.w.Base64(data)
}
