package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/video-site/backend/internal/drives/scriptcrawler"
	"github.com/video-site/backend/internal/persistence"
)

type legacyDeletedCrawlerCleanupStats struct {
	RemovedCrawlers int
	RemovedVideos   int
}

// cleanupLegacyDeletedCrawlers removes rows left by the legacy soft-delete
// flow. That flow cleared script_path but retained the drive, source history,
// local storage, and unrelated credentials such as upload_drive_id.
//
// This runs before drive attachment and uses the current destructive-delete
// semantics: local crawler videos and storage are removed, while videos that
// were already migrated to another drive remain owned by that destination.
func (a *App) cleanupLegacyDeletedCrawlers(ctx context.Context) (legacyDeletedCrawlerCleanupStats, error) {
	stats := legacyDeletedCrawlerCleanupStats{}
	if a == nil || a.cat == nil || a.cfg == nil {
		return stats, errors.New("legacy crawler cleanup dependencies are unavailable")
	}

	driveRows, err := a.cat.ListDrives(ctx)
	if err != nil {
		return stats, fmt.Errorf("list drives: %w", err)
	}

	var cleanupErrors []error
	for _, drive := range driveRows {
		if drive == nil || drive.Kind != scriptcrawler.Kind || scriptcrawler.IsConfigured(drive.Credentials) {
			continue
		}
		if err := ctx.Err(); err != nil {
			cleanupErrors = append(cleanupErrors, err)
			break
		}

		removedVideos, err := a.cleanupDriveVideosForDelete(ctx, drive.ID)
		if err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("cleanup crawler %s storage and videos: %w", drive.ID, err))
			continue
		}
		stats.RemovedVideos += removedVideos

		persistence.RLock()
		err = a.cat.DeleteDrive(ctx, drive.ID)
		persistence.RUnlock()
		if err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("delete crawler %s state: %w", drive.ID, err))
			continue
		}
		stats.RemovedCrawlers++
	}

	return stats, errors.Join(cleanupErrors...)
}
