package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nianzhibai/91/backend/internal/atomicfile"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/filelock"
)

// Only the application's fixed filename is migrated. Explicit paths used by
// backup staging and diagnostic tools remain untouched.
func migrateDatabaseFilename(ctx context.Context, target string) error {
	if filepath.Base(target) != "91.db" {
		return nil
	}
	directory := filepath.Dir(target)
	legacy := filepath.Join(directory, "video-site.db")
	if _, err := os.Lstat(legacy); errors.Is(err, os.ErrNotExist) {
		return requireNoDatabaseSidecars(legacy)
	} else if err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(directory, ".91-database-migration.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := filelock.TryLock(lock); err != nil {
		return fmt.Errorf("database filename migration is already running: %w", err)
	}
	defer filelock.Unlock(lock)

	oldInfo, err := os.Lstat(legacy)
	if errors.Is(err, os.ErrNotExist) {
		return requireNoDatabaseSidecars(legacy)
	}
	if err != nil {
		return err
	}
	if !oldInfo.Mode().IsRegular() {
		return fmt.Errorf("legacy database must be a regular file: %s", legacy)
	}
	newInfo, err := os.Lstat(target)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Two names for the same inode mean publication was interrupted before the
	// old name was removed. Distinct databases must never be chosen implicitly.
	if newInfo != nil && (!newInfo.Mode().IsRegular() || !os.SameFile(oldInfo, newInfo)) {
		return fmt.Errorf("both %s and %s exist; resolve the database conflict before starting", legacy, target)
	}
	if err := requireNoDatabaseSidecars(target); err != nil {
		return err
	}
	if err := catalog.PrepareDatabaseRename(ctx, legacy); err != nil {
		return fmt.Errorf("prepare legacy database migration (stop other processes using it): %w", err)
	}
	if err := requireNoDatabaseSidecars(legacy); err != nil {
		return err
	}
	// A hard link publishes the complete database without overwriting a file
	// created concurrently. Both names are in the same directory/filesystem.
	if newInfo == nil {
		if err := os.Link(legacy, target); err != nil {
			return fmt.Errorf("publish renamed database: %w", err)
		}
	}
	if err := atomicfile.SyncDirectory(directory); err != nil {
		return err
	}
	if err := os.Remove(legacy); err != nil {
		return err
	}
	return atomicfile.SyncDirectory(directory)
}

func requireNoDatabaseSidecars(path string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); err == nil {
			return fmt.Errorf("database migration found unresolved SQLite file %s; stop other database processes and recover it before retrying", path+suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
