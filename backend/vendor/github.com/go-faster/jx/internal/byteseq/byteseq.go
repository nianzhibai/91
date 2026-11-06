// Package byteseq provides a Byteseq type that can be used to represent a sequence of bytes.
package byteseq

import "unicode/utf8"

// Byteseq is common interface for byte slices and strings.
type Byteseq interface {
	string | []byte
}

// DecodeRuneInByteseq decodes the first UTF-8 encoded rune in val and returns the rune and its size in bytes.
func DecodeRuneInByteseq[T Byteseq](val T) (r rune, size int) {
	var tmp [4]byte
	n := copy(tmp[:], val)
	return utf8.DecodeRune(tmp[:n])
}
