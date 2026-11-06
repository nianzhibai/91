package ige

import "crypto/aes"

// DecryptAES256Blocks decrypts src using AES-256 in IGE mode and writes the
// result to dst. It uses an architecture-specific implementation when one is
// available and otherwise falls back to crypto/aes and DecryptBlocks.
//
// The key must be exactly 32 bytes. The IV must contain two AES blocks. Src
// must contain whole AES blocks and dst must be at least as long as src. Dst
// and src must not overlap.
func DecryptAES256Blocks(key, iv, dst, src []byte) {
	if len(key) != 32 {
		panic(aes.KeySizeError(len(key)))
	}
	if len(iv) != aes.BlockSize*2 {
		panic(ErrInvalidIV)
	}
	if len(src)%aes.BlockSize != 0 {
		panic("src not full blocks")
	}
	if len(dst) < len(src) {
		panic("len(dst) < len(src)")
	}

	if decryptAES256BlocksHardware(key, iv, dst, src) {
		return
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	DecryptBlocks(block, iv, dst, src)
}
