//go:build !windows

package api

import (
	"golang.org/x/sys/unix"

	"github.com/nianzhibai/91/backend/internal/storageusage"
)

func localDiskStats(path string) (storageusage.DiskStats, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return storageusage.DiskStats{}, err
	}
	blockSize := uint64(stat.Bsize)
	return storageusage.DiskStats{
		AvailableBytes: int64(uint64(stat.Bavail) * blockSize),
		CapacityBytes:  int64(uint64(stat.Blocks) * blockSize),
	}, nil
}
