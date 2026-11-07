package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/nianzhibai/91/backend/internal/backup"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
)

func restoredCatalogStorage(storage config.Storage, applied *backup.AppliedRestore) (config.Storage, error) {
	path := applied.DatabasePath()
	if path == "" || path == storage.DBPath {
		return storage, nil
	}
	legacy := filepath.Join(filepath.Dir(storage.DBPath), "video-site.db")
	if path != legacy {
		return config.Storage{}, fmt.Errorf("restored database %s does not match configured database %s", path, storage.DBPath)
	}
	if _, err := os.Lstat(storage.DBPath); err == nil {
		return config.Storage{}, fmt.Errorf("both legacy restored database and %s exist", storage.DBPath)
	} else if !os.IsNotExist(err) {
		return config.Storage{}, err
	}
	// Commit at the original filename before renaming: pending restore markers
	// must remain valid for rollback, including after an interrupted startup.
	storage.DBPath = path
	return storage, nil
}

// openStartupCatalog finishes a pending restore at its original path before
// migrating the filename. Catalog.Open itself only opens the requested file.
func openStartupCatalog(ctx context.Context, storage config.Storage, applied *backup.AppliedRestore) (*catalog.Catalog, error) {
	restored, err := restoredCatalogStorage(storage, applied)
	if err == nil && restored.DBPath == storage.DBPath {
		err = migrateDatabaseFilename(ctx, storage.DBPath)
	}
	var cat *catalog.Catalog
	if err == nil {
		cat, err = openApplicationCatalog(restored)
	}
	if err == nil {
		err = cat.InterruptCrawlerTasks(ctx)
	}
	if err == nil {
		err = cat.ResetLoginProtection(ctx)
	}
	if err != nil {
		if cat != nil {
			_ = cat.Close()
		}
		if applied != nil {
			if rollbackErr := backup.RollbackAppliedRestore(applied, err); rollbackErr != nil {
				return nil, fmt.Errorf("open restored catalog: %v (rollback failed: %w)", err, rollbackErr)
			}
			return nil, fmt.Errorf("open restored catalog: %w; old data restored and will be used after restart", err)
		}
		return nil, err
	}
	if applied != nil {
		if err := backup.CommitAppliedRestore(applied); err != nil {
			if restored.DBPath != storage.DBPath {
				_ = cat.Close()
				return nil, fmt.Errorf("finish restore before migrating database filename: %w", err)
			}
			log.Printf("[restore] restored catalog opened, but rollback cleanup/report write failed: %v", err)
		} else {
			log.Printf("[restore] backup restored successfully; previous sessions were cleared")
		}
	}
	if restored.DBPath == storage.DBPath {
		return cat, nil
	}
	if err := cat.Close(); err != nil {
		return nil, fmt.Errorf("close restored catalog before database migration: %w", err)
	}
	if err := migrateDatabaseFilename(ctx, storage.DBPath); err != nil {
		return nil, fmt.Errorf("migrate restored database filename: %w", err)
	}
	return openApplicationCatalog(storage)
}
