package ige

import (
	"crypto/cipher"

	"github.com/go-faster/xor"
)

// NewIGEEncrypter returns an IGE cipher.BlockMode which encrypts using IGE and
// the given cipher.Block.
//
// Note: iv must contain two iv values for IGE (concatenated), otherwise this
// function will panic. See ErrInvalidIV for more information.
func NewIGEEncrypter(b cipher.Block, iv []byte) IGE {
	if err := checkIV(b, iv); err != nil {
		panic(err.Error())
	}

	return (*igeEncrypter)(newIGE(b, iv))
}

type igeEncrypter ige

func (i *igeEncrypter) BlockSize() int {
	return i.block.BlockSize()
}

func (i *igeEncrypter) CryptBlocks(dst, src []byte) {
	EncryptBlocks(i.block, i.iv, dst, src)
}

// EncryptBlocks is a simple shorthand for IGE encrypting.
// Note: unlike NewIGEEncrypter, EncryptBlocks does NOT COPY iv.
// So you must not modify passed iv.
func EncryptBlocks(block cipher.Block, iv, dst, src []byte) {
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
		xor.Bytes(dst[o:o+b:o+b], src[o:o+b:o+b], c)
		block.Encrypt(dst[o:o+b:o+b], dst[o:o+b:o+b])
		xor.Bytes(dst[o:o+b:o+b], dst[o:o+b:o+b], m)

		c = dst[o : o+b : o+b]
		m = src[o : o+b : o+b]
	}
}
