package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/drives/localupload"
	"github.com/video-site/backend/internal/drives/scriptcrawler"
	"github.com/video-site/backend/internal/fingerprint"
	"github.com/video-site/backend/internal/persistence"
	"github.com/video-site/backend/internal/preview"
	"github.com/video-site/backend/internal/scanner"
)

// scheduleScan admits an asynchronous scan for one drive. Different drives can
// scan concurrently, while each drive shares one operation gate with its
// generation and configuration tasks.
func (a *App) scheduleScan(ctx context.Context, driveID string) bool {
	if a.driveHasActiveWork(driveID) {
		log.Printf("[scan] drive=%s has active work, skip duplicate request", driveID)
		return false
	}
	taskCtx, done, admitted := a.registerDriveTaskContext(ctx, driveID, driveTaskScopeScan)
	if !admitted {
		log.Printf("[scan] drive=%s configuration update in progress, reject scan", driveID)
		return false
	}
	if !a.beginDriveScanOrCrawl(driveID) {
		done()
		log.Printf("[scan] drive=%s already queued or running, skip duplicate request", driveID)
		return false
	}

	go func() {
		defer func() {
			a.endDriveScanOrCrawl(driveID)
			done()
		}()
		a.runScanWithTaskContext(taskCtx, driveID)
	}()
	return true
}

// runScan is the synchronous entry point used by the nightly pipeline.
func (a *App) runScan(ctx context.Context, driveID string) {
	taskCtx, done, admitted := a.registerDriveTaskContext(ctx, driveID, driveTaskScopeScan)
	if !admitted {
		log.Printf("[scan] drive=%s configuration update in progress, reject direct scan", driveID)
		return
	}
	defer done()
	if !a.beginDriveScanOrCrawl(driveID) {
		log.Printf("[scan] drive=%s already queued or running, skip direct scan", driveID)
		return
	}
	defer a.endDriveScanOrCrawl(driveID)
	a.runScanWithTaskContext(taskCtx, driveID)
}

func (a *App) runScanWithTaskContext(ctx context.Context, driveID string) {
	if err := ctx.Err(); err != nil {
		log.Printf("[scan] drive=%s canceled before start: %v", driveID, err)
		return
	}
	if err := a.ensureDriveAttached(ctx, driveID); err != nil {
		log.Printf("[scan] drive=%s attach failed: %v", driveID, err)
		return
	}
	drv, ok := a.registry.Get(driveID)
	if !ok {
		log.Printf("[scan] drive=%s not attached", driveID)
		return
	}
	driveConfig, err := a.activeDriveConfig(ctx, driveID)
	if err != nil {
		log.Printf("[scan] get active drive config %s: %v", driveID, err)
		return
	}

	result, err := a.scanDrive(ctx, drv, driveConfig)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			log.Printf("[scan] drive=%s canceled: %v", driveID, err)
		} else if !a.pauseDriveScanForRateLimit(ctx, driveID, drv, err) {
			log.Printf("[scan] drive=%s error: %v", driveID, err)
		}
		return
	}
	if err := ctx.Err(); err != nil {
		log.Printf("[scan] drive=%s canceled after reconciliation: %v", driveID, err)
		return
	}

	stats := result.Stats
	log.Printf(
		"[scan] drive=%s done scanned=%d added=%d updated=%d duplicates=%d tombstoned=%d excluded_dirs=%d errors=%d",
		driveID, stats.Scanned, stats.Added, result.Updated, result.Duplicates,
		result.Tombstoned, len(result.Snapshot.ExcludedDirIDs), stats.Errors,
	)
	if err := a.cleanupScanSnapshot(ctx, drv, result.Snapshot); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			log.Printf("[cleanup] canceled stale cleanup drive=%s kind=%s: %v", drv.ID(), drv.Kind(), ctxErr)
			return
		}
		log.Printf("[cleanup] stale cleanup drive=%s kind=%s error: %v", drv.ID(), drv.Kind(), err)
	}
	if err := ctx.Err(); err != nil {
		log.Printf("[scan] drive=%s canceled before derived-task dispatch: %v", driveID, err)
		return
	}

	a.mu.Lock()
	previewWorker := a.workers[driveID]
	thumbnailWorker := a.thumbWorkers[driveID]
	fingerprintWorker := a.fingerprintWorkers[driveID]
	a.mu.Unlock()
	enqueueNewScanVideos(result.NewVideos, thumbnailWorker, fingerprintWorker)
	a.scheduleFingerprintBackfill(ctx, driveID, fingerprintWorker)
	a.enqueueDriveGeneration(ctx, driveID, previewWorker, thumbnailWorker)
}

func (a *App) scanDrive(ctx context.Context, drv drives.Drive, driveConfig *catalog.Drive) (scanner.Result, error) {
	if a == nil || a.cfg == nil {
		return scanner.Result{}, errors.New("scan configuration is unavailable")
	}
	if driveConfig == nil {
		return scanner.Result{}, errors.New("drive scan configuration is unavailable")
	}
	scan := scanner.New(
		a.cat,
		drv,
		a.cfg.Scanner.VideoExtensions,
		driveConfig.SkipDirIDs,
		nil,
	)
	scan.OnProgress = func(stats scanner.Stats) {
		a.updateDriveScanProgress(drv.ID(), stats.Scanned, stats.Added)
	}
	log.Printf("[scan] drive=%s start=%s skip_dirs=%d", drv.ID(), driveConfig.RootID, len(driveConfig.SkipDirIDs))
	return scan.Scan(ctx, driveConfig.RootID)
}

// cleanupScanSnapshot performs the destructive phase only after complete
// discovery. Reconciliation issues do not make a live file look missing because
// presence comes from the snapshot, independently of catalog writes.
func (a *App) cleanupScanSnapshot(ctx context.Context, drv drives.Drive, snapshot scanner.Snapshot) error {
	if drv.Kind() == scriptcrawler.Kind || drv.ID() == localupload.DriveID {
		return nil
	}
	if !snapshot.Complete() {
		log.Printf(
			"[cleanup] skip stale cleanup for drive=%s kind=%s: discovery had %d directory errors",
			drv.ID(), drv.Kind(), len(snapshot.Issues),
		)
		return nil
	}
	removed, err := a.cleanupMissingDriveVideos(
		ctx,
		drv.ID(),
		snapshot.SeenFileIDs,
		snapshot.VisitedDirIDs,
		snapshot.FullDriveScan,
	)
	if err != nil {
		return err
	}
	if removed > 0 {
		log.Printf("[cleanup] removed %d stale videos for drive=%s kind=%s", removed, drv.ID(), drv.Kind())
	}
	return nil
}

func enqueueNewScanVideos(
	videos []*catalog.Video,
	thumbnailWorker *preview.ThumbWorker,
	fingerprintWorker *fingerprint.Worker,
) {
	for _, video := range videos {
		if video == nil {
			continue
		}
		if fingerprintWorker != nil {
			fingerprintWorker.Enqueue(video)
		}
		if thumbnailWorker != nil && video.ThumbnailURL == "" {
			thumbnailWorker.Enqueue(video)
		}
	}
}

func (a *App) cleanupMissingDriveVideos(
	ctx context.Context,
	driveID string,
	liveFileIDs map[string]struct{},
	visitedDirIDs map[string]struct{},
	fullDriveScan bool,
) (int, error) {
	if err := persistence.RLockContext(ctx); err != nil {
		return 0, err
	}
	defer persistence.RUnlock()

	const confirmationThreshold = 2
	confirmedMissing, err := a.cat.ConfirmMissingDriveFiles(
		ctx, driveID, liveFileIDs, visitedDirIDs, fullDriveScan, confirmationThreshold,
	)
	if err != nil {
		return 0, fmt.Errorf("confirm missing drive files: %w", err)
	}
	if len(confirmedMissing) == 0 {
		return 0, nil
	}
	items, err := a.cat.ListVideosByDrive(ctx, driveID)
	if err != nil {
		return 0, err
	}

	localDir := ""
	if a.cfg != nil {
		localDir = a.cfg.Storage.LocalPreviewDir
	}
	removed := 0
	for _, video := range items {
		if _, ok := confirmedMissing[video.FileID]; !ok {
			continue
		}
		if err := removeLocalVideoAssets(localDir, video); err != nil {
			return removed, fmt.Errorf("remove local assets for %s: %w", video.ID, err)
		}
		if err := a.cat.DeleteVideo(ctx, video.ID); err != nil {
			return removed, fmt.Errorf("delete catalog video %s: %w", video.ID, err)
		}
		removed++
	}
	return removed, nil
}

func (a *App) pauseDriveScanForRateLimit(ctx context.Context, driveID string, drv drives.Drive, err error) bool {
	wait, ok := drives.RateLimitRetryAfter(err)
	if !ok {
		return false
	}
	if wait <= 0 {
		wait = scanCooldownForDrive(drv)
	}
	if wait <= 0 {
		wait = 5 * time.Minute
	}
	until := time.Now().Add(wait)
	a.updateDriveScanCooldown(driveID, until)
	log.Printf("[scan] drive=%s rate limited; cooling until=%s wait=%s: %v", driveID, until.Format(time.RFC3339), wait, err)
	if !sleepDriveScanCooldown(ctx, wait) {
		log.Printf("[scan] drive=%s cooldown canceled: %v", driveID, ctx.Err())
	}
	return true
}

func scanCooldownForDrive(drv drives.Drive) time.Duration {
	if drv == nil {
		return 5 * time.Minute
	}
	switch strings.ToLower(drv.Kind()) {
	case "guangyapan":
		return 10 * time.Minute
	default:
		return 5 * time.Minute
	}
}

func sleepDriveScanCooldown(ctx context.Context, duration time.Duration) bool {
	if duration <= 0 {
		return true
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
