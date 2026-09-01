//go:build windows

package filelock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Windows byte-range locks are mandatory: an exclusive lock over a region
// makes ReadFile on that region fail for every other handle. Callers store
// ownership metadata at the start of the lock file and expect to read it after
// losing the race, so the lock is placed on a single byte far past any content
// instead of over the whole file. The byte is never written, which makes the
// region a pure mutex and leaves the metadata readable, matching the advisory
// semantics flock provides on Unix.
const (
	lockRegionOffsetLow  = 0
	lockRegionOffsetHigh = 1 << 30
	lockRegionLengthLow  = 1
	lockRegionLengthHigh = 0
)

func tryLock(file *os.File) error {
	overlapped := new(windows.Overlapped)
	overlapped.Offset = lockRegionOffsetLow
	overlapped.OffsetHigh = lockRegionOffsetHigh
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		lockRegionLengthLow,
		lockRegionLengthHigh,
		overlapped,
	)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return ErrLocked
	}
	return err
}

func unlock(file *os.File) error {
	overlapped := new(windows.Overlapped)
	overlapped.Offset = lockRegionOffsetLow
	overlapped.OffsetHigh = lockRegionOffsetHigh
	return windows.UnlockFileEx(
		windows.Handle(file.Fd()),
		0,
		lockRegionLengthLow,
		lockRegionLengthHigh,
		overlapped,
	)
}
