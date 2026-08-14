package atomicfile

import "testing"

func TestSyncDirectoryAcceptsExistingDirectory(t *testing.T) {
	if err := SyncDirectory(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
