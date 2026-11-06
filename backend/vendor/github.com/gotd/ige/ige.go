package ige

import (
	"crypto/cipher"
	"errors"
)

// ErrInvalidIV is displayed as the panic message if the initialization vector
// passed to NewIGEEncrypter or NewIGEDecrypter doesn't fulfill the length
// requirements for IGE.
//
// IGE uses a two step xor process, so the first initialization vector is the
// first half, and the second initialization vector is the second half. This
// requires the initialization vector to be twice as long as the block size.
var ErrInvalidIV = errors.New("iv length must be: (block size * 2)")

// IGE satisfies the cipher.BlockMode interface from the crypto/cipher package.
type IGE interface {
	// BlockSize returns the mode's block size.
	BlockSize() int
	// CryptBlocks encrypts or decrypts a number of blocks based on the
	// underlying cipher.Block passed to NewIGEEncrypter or NewIGEDecrypter
	// (usually from crypto/aes).
	CryptBlocks(dst, src []byte)
}

type ige struct {
	block cipher.Block
	iv    []byte
}

func newIGE(b cipher.Block, iv []byte) *ige {
	i := &ige{b, make([]byte, len(iv))}
	copy(i.iv, iv)
	return i
}

func checkIV(b cipher.Block, iv []byte) error {
	// the initialization vector needs to contain b.Blocksize()*2 bytes because
	// ige uses a two step xor process, and iv[:16] corresponds to the first iv
	// while iv[16:] corresponds to the second iv
	//
	// the original ige paper described the first iv as a random block and the
	// second iv as its encrypted counterpart, however, we're copying the
	// openssl implementation and therefore both ivs are supplied by the user
	if len(iv) != b.BlockSize()*2 {
		return ErrInvalidIV
	}

	return nil
}
