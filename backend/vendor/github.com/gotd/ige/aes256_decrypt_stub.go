//go:build !arm || !cgo || (!linux && !android)
// +build !arm !cgo !linux,!android

package ige

func decryptAES256BlocksHardware(key, iv, dst, src []byte) bool {
	return false
}
