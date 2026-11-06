package ige

import (
	"crypto/cipher"

	"github.com/go-faster/xor"
)

// NewIGEDecrypter returns an IGE cipher.BlockMode which decrypts using IGE and
// the given cipher.Block.
//
// Note: iv must contain two iv values for IGE (concatenated), otherwise this
// function will panic. See ErrInvalidIV for more information.
func NewIGEDecrypter(b cipher.Block, iv []byte) IGE {
	if err := checkIV(b, iv); err != nil {
		panic(err.Error())
	}

	return (*igeDecrypter)(newIGE(b, iv))
}

type igeDecrypter ige

func (i *igeDecrypter) BlockSize() int {
	return i.block.BlockSize()
}

func (i *igeDecrypter) CryptBlocks(dst, src []byte) {
	DecryptBlocks(i.block, i.iv, dst, src)
}

// DecryptBlocks is a simple shorthand for IGE decrypting.
// Note: unlike NewIGEDecrypter, DecryptBlocks does NOT COPY iv.
// So you must not modify passed iv.
func DecryptBlocks(block cipher.Block, iv, dst, src []byte) {
	if err := checkIV(block, iv); err != nil {
		panic(err.Error())
	}
	if len(src)%block.BlockSize() != 0 {
		panic("src not full blocks")
	}
	if len(dst) < len(src) {
		panic("len(dst) < len(src)")
	}

	b := block.BlockSize()
	c := iv[:b]
	m := iv[b:]

	for o := 0; o < len(src); o += b {
		t := src[o : o+b : o+b]

		xor.Bytes(dst[o:o+b:o+b], src[o:o+b:o+b], m)
		block.Decrypt(dst[o:o+b:o+b], dst[o:o+b:o+b])
		xor.Bytes(dst[o:o+b:o+b], dst[o:o+b:o+b], c)

		m = dst[o : o+b]
		c = t
	}
}
