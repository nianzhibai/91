//go:build arm && cgo && (linux || android)
// +build arm
// +build cgo
// +build linux android

package ige

/*
#cgo CFLAGS: -O3 -march=armv8-a+crypto -mfpu=crypto-neon-fp-armv8
#cgo LDFLAGS: -ldl
#include <stddef.h>
#include <stdint.h>

int aes256_ige_decrypt(const uint8_t *key, const uint8_t *iv,
                       uint8_t *dst, const uint8_t *src, size_t len);
*/
import "C"
import "unsafe"

func decryptAES256BlocksHardware(key, iv, dst, src []byte) bool {
	if len(src) == 0 {
		return false
	}

	rc := C.aes256_ige_decrypt(
		(*C.uint8_t)(unsafe.Pointer(&key[0])),
		(*C.uint8_t)(unsafe.Pointer(&iv[0])),
		(*C.uint8_t)(unsafe.Pointer(&dst[0])),
		(*C.uint8_t)(unsafe.Pointer(&src[0])),
		C.size_t(len(src)),
	)
	return rc == 0
}
