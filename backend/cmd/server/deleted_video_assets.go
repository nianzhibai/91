package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
	"github.com/nianzhibai/91/backend/internal/persistence"
)

const videoDeletionGracePeriod = 3 * time.Minute
const deletedVideoAssetCleanupInterval = time.Second

func (a *App) runDeletedVideoAssetCleanup(ctx context.Context) {
	ticker := time.NewTicker(deletedVideoAssetCleanupInterval)
	defer ticker.Stop()
	for {
		if err := a.cleanupExpiredDeletedVideoAssets(ctx, time.Now()); err != nil && ctx.Err() == nil {
			log.Printf("[deleted-video-assets] cleanup: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) cleanupExpiredDeletedVideoAssets(ctx context.Context, now time.Time) error {
	ids, err := a.cat.ListExpiredDeletedVideoAssets(ctx, now, 1000)
	if err != nil {
		return err
	}
	var cleanupErrors []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(cleanupErrors, err)...)
		}
		unlock := a.blacklistVideoLocks.lock(id)
		err := persistence.RLockContext(ctx)
		if err == nil {
			err = a.cleanupDeletedVideoAssets(ctx, id, now, false)
			persistence.RUnlock()
		}
		unlock()
		if err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("video %s: %w", id, err))
		}
	}
	return errors.Join(cleanupErrors...)
}

// The caller holds the per-video lock and the persistence snapshot gate.
func (a *App) cleanupDeletedVideoAssets(ctx context.Context, id string, now time.Time, force bool) error {
	return a.cat.CleanupDeletedVideoAssets(ctx, id, now, force, func(video *catalog.Video) error {
		localDir := ""
		if a.cfg != nil {
			localDir = a.cfg.Storage.LocalPreviewDir
		}
		return mediaasset.RemoveGeneratedVideoAssets(localDir, video.ID, video.PreviewLocal)
	})
}
