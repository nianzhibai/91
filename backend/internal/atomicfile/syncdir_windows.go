//go:build windows

package atomicfile

func syncDirectory(string) error {
	// os.File.Sync uses FlushFileBuffers on Windows. Directory handles opened
	// through os.Open cannot be flushed and return ERROR_ACCESS_DENIED, even
	// though the preceding rename has already completed.
	return nil
}
