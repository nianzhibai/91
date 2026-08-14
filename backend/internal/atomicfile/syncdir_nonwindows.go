//go:build !windows

package atomicfile

import "os"

func syncDirectory(directory string) error {
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return err
	}
	syncErr := directoryHandle.Sync()
	closeErr := directoryHandle.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
