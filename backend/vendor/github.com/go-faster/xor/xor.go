package xor

// Bytes xors the bytes in a and b. The destination should have enough
// space, otherwise Bytes will panic. Returns the number of bytes xor'd.
func Bytes(dst, a, b []byte) int {
	return xorBytes(dst, a, b)
}
