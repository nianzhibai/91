package filelock_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/video-site/backend/internal/filelock"
)

func openLockFile(t *testing.T, path string) *os.File {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func TestTryLockExcludesASecondHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resource.lock")
	first := openLockFile(t, path)
	if err := filelock.TryLock(first); err != nil {
		t.Fatalf("lock first handle: %v", err)
	}

	second := openLockFile(t, path)
	err := filelock.TryLock(second)
	if !errors.Is(err, filelock.ErrLocked) {
		t.Fatalf("second TryLock = %v, want ErrLocked", err)
	}
}

func TestUnlockLetsAnotherHandleTakeTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resource.lock")
	first := openLockFile(t, path)
	if err := filelock.TryLock(first); err != nil {
		t.Fatalf("lock first handle: %v", err)
	}
	if err := filelock.Unlock(first); err != nil {
		t.Fatalf("unlock first handle: %v", err)
	}

	second := openLockFile(t, path)
	if err := filelock.TryLock(second); err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	if err := filelock.Unlock(second); err != nil {
		t.Fatalf("unlock second handle: %v", err)
	}
}

func TestClosingTheFileReleasesTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resource.lock")
	first, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open first handle: %v", err)
	}
	if err := filelock.TryLock(first); err != nil {
		t.Fatalf("lock first handle: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first handle: %v", err)
	}

	second := openLockFile(t, path)
	if err := filelock.TryLock(second); err != nil {
		t.Fatalf("lock after close: %v", err)
	}
	if err := filelock.Unlock(second); err != nil {
		t.Fatalf("unlock second handle: %v", err)
	}
}

// A caller that loses the race reads ownership metadata out of the lock file,
// so holding the lock must not make the file's own bytes unreadable. Windows
// byte-range locks are mandatory and would break this if the lock covered the
// content instead of a byte past it.
func TestHoldingTheLockLeavesFileContentReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resource.lock")
	owner := openLockFile(t, path)
	if err := filelock.TryLock(owner); err != nil {
		t.Fatalf("lock owner handle: %v", err)
	}
	if _, err := owner.WriteString("pid=4242\n"); err != nil {
		t.Fatalf("write owner metadata: %v", err)
	}
	if err := owner.Sync(); err != nil {
		t.Fatalf("sync owner metadata: %v", err)
	}

	reader := openLockFile(t, path)
	if err := filelock.TryLock(reader); !errors.Is(err, filelock.ErrLocked) {
		t.Fatalf("reader TryLock = %v, want ErrLocked", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read locked file: %v", err)
	}
	if !strings.Contains(string(contents), "pid=4242") {
		t.Fatalf("locked file contents = %q, want the owner metadata", contents)
	}
}
