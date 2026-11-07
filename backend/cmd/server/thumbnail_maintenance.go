package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/localpath"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
)

const thumbnailJPEGNormalizationSetting = "media.thumbnails.jpeg_normalized.v1"

type localAssetReconciliationStats struct {
	Scanned int
	Present int
	Missing int
	Reset   int
}

// runStartupThumbnailNormalization keeps the one-time legacy file migration
// independent from recurring catalog/filesystem reconciliation. Once its
// persisted marker exists, startup performs no thumbnail directory scan.
func (a *App) runStartupThumbnailNormalization(ctx context.Context) {
	if _, err := a.normalizeLegacyThumbnailFiles(ctx); err != nil {
		log.Printf("[thumbnail-maintenance] migration failed: %v", err)
	}
}

// reconcileLocalGeneratedAssets repairs local thumbnail and preview
// references, then waits for every currently registered drive to finish
// admitting the resulting pending work. Generation itself remains asynchronous;
// the nightly runner establishes the queue-idle boundary after this method
// returns.
func (a *App) reconcileLocalGeneratedAssets(ctx context.Context) (int, error) {
	thumbnailStats, thumbnailErr := a.reconcileMissingLocalThumbnailFiles(ctx)
	previewStats, previewErr := a.reconcileMissingLocalPreviewFiles(ctx)
	resets := thumbnailStats.Reset + previewStats.Reset
	reconcileErr := errors.Join(thumbnailErr, previewErr)
	if resets == 0 {
		return 0, errors.Join(reconcileErr, ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return resets, errors.Join(reconcileErr, err)
	}
	log.Printf(
		"[asset-reconciliation] admitting repaired assets thumbnail_resets=%d preview_resets=%d",
		thumbnailStats.Reset,
		previewStats.Reset,
	)
	if err := a.enqueueRegisteredDriveGenerationAndWait(ctx); err != nil {
		return resets, errors.Join(reconcileErr, err)
	}
	return resets, reconcileErr
}

// normalizeLegacyThumbnailFiles upgrades the old cache contract where a .jpg
// path could contain WebP bytes. The catalog marker makes the directory scan a
// one-time migration while still allowing a restored pre-migration database to
// trigger it again.
func (a *App) normalizeLegacyThumbnailFiles(ctx context.Context) (mediaasset.ThumbnailNormalizationStats, error) {
	stats := mediaasset.ThumbnailNormalizationStats{}
	if a == nil || a.cfg == nil || a.cat == nil {
		return stats, errors.New("thumbnail normalization dependencies are unavailable")
	}
	marker, err := a.cat.GetSetting(ctx, thumbnailJPEGNormalizationSetting, "")
	if err != nil {
		return stats, fmt.Errorf("read migration marker: %w", err)
	}
	if strings.TrimSpace(marker) == "1" {
		return stats, nil
	}

	directory := filepath.Join(a.cfg.Storage.LocalPreviewDir, "thumbs")
	stats, err = mediaasset.NormalizeThumbnailDirectoryJPEG(directory)
	if err != nil {
		return stats, fmt.Errorf(
			"normalize legacy thumbnails scanned=%d normalized=%d failed=%d: %w",
			stats.Scanned,
			stats.Normalized,
			stats.Failed,
			err,
		)
	}
	if err := a.cat.SetSetting(ctx, thumbnailJPEGNormalizationSetting, "1"); err != nil {
		return stats, fmt.Errorf("write migration marker: %w", err)
	}
	log.Printf(
		"[thumbnail-maintenance] scanned=%d normalized=%d",
		stats.Scanned,
		stats.Normalized,
	)
	return stats, nil
}

// reconcileMissingLocalThumbnailFiles repairs the persisted catalog/filesystem
// invariant. Keeping this check in App avoids coupling the catalog package to
// local storage while ensuring API queries can continue to treat a non-empty
// thumbnail_url as actually ready.
func (a *App) reconcileMissingLocalThumbnailFiles(ctx context.Context) (localAssetReconciliationStats, error) {
	stats := localAssetReconciliationStats{}
	if a == nil || a.cfg == nil || a.cat == nil {
		return stats, errors.New("thumbnail reconciliation dependencies are unavailable")
	}
	localDir := strings.TrimSpace(a.cfg.Storage.LocalPreviewDir)
	if localDir == "" {
		return stats, errors.New("thumbnail reconciliation local preview directory is empty")
	}

	references, err := a.cat.ListCanonicalLocalThumbnailReferences(ctx)
	if err != nil {
		return stats, fmt.Errorf("list local thumbnail references: %w", err)
	}
	missing := make([]catalog.LocalThumbnailReference, 0)
	for _, reference := range references {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		stats.Scanned++
		exists, err := localThumbnailFileExists(localDir, reference.VideoID)
		if err != nil {
			return stats, fmt.Errorf("inspect thumbnail for %q: %w", reference.VideoID, err)
		}
		if exists {
			stats.Present++
			continue
		}
		stats.Missing++
		missing = append(missing, reference)
	}

	stats.Reset, err = a.cat.ResetMissingLocalThumbnails(ctx, missing)
	if err != nil {
		return stats, fmt.Errorf("reset missing local thumbnails: %w", err)
	}
	log.Printf(
		"[thumbnail-maintenance] reconciled scanned=%d present=%d missing=%d reset=%d",
		stats.Scanned,
		stats.Present,
		stats.Missing,
		stats.Reset,
	)
	return stats, nil
}

func localThumbnailFileExists(localDir, videoID string) (bool, error) {
	for _, candidate := range mediaasset.ThumbnailPathCandidates(localDir, videoID) {
		info, err := os.Stat(candidate)
		if err == nil {
			if info.Mode().IsRegular() && info.Size() > 0 {
				return true, nil
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

// reconcileMissingLocalPreviewFiles applies the same persisted-reference
// invariant to generated teaser videos. Missing ready files return to the
// normal pending queue and are picked up by scheduled queue admission.
func (a *App) reconcileMissingLocalPreviewFiles(ctx context.Context) (localAssetReconciliationStats, error) {
	stats := localAssetReconciliationStats{}
	if a == nil || a.cfg == nil || a.cat == nil {
		return stats, errors.New("preview reconciliation dependencies are unavailable")
	}
	localDir := strings.TrimSpace(a.cfg.Storage.LocalPreviewDir)
	if localDir == "" {
		return stats, errors.New("preview reconciliation local preview directory is empty")
	}

	references, err := a.cat.ListReadyLocalPreviewReferences(ctx)
	if err != nil {
		return stats, fmt.Errorf("list local preview references: %w", err)
	}
	missing := make([]catalog.LocalPreviewReference, 0)
	for _, reference := range references {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		stats.Scanned++
		exists, err := localPreviewFileExists(localDir, reference.PreviewLocal)
		if err != nil {
			return stats, fmt.Errorf("inspect preview for %q: %w", reference.VideoID, err)
		}
		if exists {
			stats.Present++
			continue
		}
		stats.Missing++
		missing = append(missing, reference)
	}

	stats.Reset, err = a.cat.ResetMissingLocalPreviews(ctx, missing)
	if err != nil {
		return stats, fmt.Errorf("reset missing local previews: %w", err)
	}
	log.Printf(
		"[preview-maintenance] reconciled scanned=%d present=%d missing=%d reset=%d",
		stats.Scanned,
		stats.Present,
		stats.Missing,
		stats.Reset,
	)
	return stats, nil
}

func localPreviewFileExists(localDir, previewLocal string) (bool, error) {
	clean, ok := localpath.Managed(localDir, previewLocal)
	if !ok {
		return false, nil
	}
	info, err := os.Stat(clean)
	if err == nil {
		return info.Mode().IsRegular() && info.Size() > 0, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}
