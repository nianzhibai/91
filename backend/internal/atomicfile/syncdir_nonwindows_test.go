//go:build !windows

package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSyncDirectoryReturnsOpenErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if err := SyncDirectory(missing); !os.IsNotExist(err) {
		t.Fatalf("SyncDirectory error = %v, want not-exist error", err)
	}
}
