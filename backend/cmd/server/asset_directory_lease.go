package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/video-site/backend/internal/filelock"
)

// assetDirectoryLease gives one server process exclusive write ownership of a
// generated-asset directory. The lock file lives beside the directory so a
// restore can atomically replace the asset directory without replacing the
// inode that carries the process lease.
type assetDirectoryLease struct {
	file *os.File
}

func acquireAssetDirectoryLease(assetDir, databasePath string) (*assetDirectoryLease, error) {
	assetDir = strings.TrimSpace(assetDir)
	if assetDir == "" {
		return nil, errors.New("asset directory lease requires a configured directory")
	}
	absoluteAssetDir, err := filepath.Abs(assetDir)
	if err != nil {
		return nil, fmt.Errorf("resolve asset directory: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absoluteAssetDir); resolveErr == nil {
		absoluteAssetDir = resolved
	} else if !errors.Is(resolveErr, os.ErrNotExist) {
		return nil, fmt.Errorf("resolve asset directory symlinks: %w", resolveErr)
	}

	digest := sha256.Sum256([]byte(filepath.Clean(absoluteAssetDir)))
	lockPath := filepath.Join(
		filepath.Dir(absoluteAssetDir),
		fmt.Sprintf(".video-site-assets-%x.lock", digest[:8]),
	)
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open asset directory lease %s: %w", lockPath, err)
	}
	if err := filelock.TryLock(file); err != nil {
		_, _ = file.Seek(0, 0)
		owner, _ := io.ReadAll(io.LimitReader(file, 2048))
		_ = file.Close()
		if errors.Is(err, filelock.ErrLocked) {
			details := strings.TrimSpace(string(owner))
			if details != "" {
				details = "; current owner: " + strings.ReplaceAll(details, "\n", ", ")
			}
			return nil, fmt.Errorf("generated asset directory %s is already owned by another server%s", absoluteAssetDir, details)
		}
		return nil, fmt.Errorf("lock generated asset directory %s: %w", absoluteAssetDir, err)
	}

	lease := &assetDirectoryLease{file: file}
	metadata := fmt.Sprintf(
		"pid=%d\ndatabase=%s\nassets=%s\nstarted_at=%s\n",
		os.Getpid(),
		singleLinePath(databasePath),
		singleLinePath(absoluteAssetDir),
		time.Now().UTC().Format(time.RFC3339),
	)
	if err := file.Truncate(0); err != nil {
		_ = lease.Close()
		return nil, fmt.Errorf("truncate asset directory lease metadata: %w", err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		_ = lease.Close()
		return nil, fmt.Errorf("seek asset directory lease metadata: %w", err)
	}
	if _, err := file.WriteString(metadata); err != nil {
		_ = lease.Close()
		return nil, fmt.Errorf("write asset directory lease metadata: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = lease.Close()
		return nil, fmt.Errorf("sync asset directory lease metadata: %w", err)
	}
	return lease, nil
}

func singleLinePath(path string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(strings.TrimSpace(path))
}

func (lease *assetDirectoryLease) Close() error {
	if lease == nil || lease.file == nil {
		return nil
	}
	file := lease.file
	lease.file = nil
	unlockErr := filelock.Unlock(file)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}
