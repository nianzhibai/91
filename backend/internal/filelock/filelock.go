// Package filelock contains the platform-specific advisory file locking used
// to give a single process exclusive ownership of a resource identified by a
// lock file. Callers open the lock file themselves so they stay free to store
// ownership metadata in it.
package filelock

import (
	"errors"
	"os"
)

// ErrLocked reports that another handle already holds the lock. It is returned
// instead of the platform error so callers can branch on ownership without
// importing platform packages.
var ErrLocked = errors.New("file is locked by another owner")

// TryLock takes an exclusive lock on the file without blocking. It returns
// ErrLocked when another handle already owns it.
//
// The lock is advisory between cooperating processes: it excludes other
// TryLock callers, not arbitrary readers and writers of the same path. Both
// implementations deliberately leave the file's own bytes readable, so a
// caller that loses the race can still read ownership metadata out of it.
func TryLock(file *os.File) error {
	return tryLock(file)
}

// Unlock releases a lock taken by TryLock. Closing the file releases the lock
// as well; Unlock exists so callers can report a release failure separately
// from a close failure.
func Unlock(file *os.File) error {
	return unlock(file)
}
