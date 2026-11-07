//go:build windows

package api

import (
	"fmt"
	"path/filepath"
	"syscall"
	"unsafe"

	"github.com/nianzhibai/91/backend/internal/storageusage"
)

func localDiskStats(path string) (storageusage.DiskStats, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return storageusage.DiskStats{}, err
	}
	h, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return storageusage.DiskStats{}, err
	}
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("GetDiskFreeSpaceExW")
	var freeBytesAvailable, totalBytes uint64
	r, _, _ := proc.Call(uintptr(unsafe.Pointer(h)), uintptr(unsafe.Pointer(&freeBytesAvailable)), uintptr(unsafe.Pointer(&totalBytes)), 0)
	if r == 0 {
		return storageusage.DiskStats{}, fmt.Errorf("GetDiskFreeSpaceEx failed")
	}
	return storageusage.DiskStats{
		AvailableBytes: int64(freeBytesAvailable),
		CapacityBytes:  int64(totalBytes),
	}, nil
}
