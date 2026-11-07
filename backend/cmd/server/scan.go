package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/drives/localupload"
	"github.com/nianzhibai/91/backend/internal/drives/scriptcrawler"
	"github.com/nianzhibai/91/backend/internal/fingerprint"
	"github.com/nianzhibai/91/backend/internal/persistence"
	"github.com/nianzhibai/91/backend/internal/preview"
	"github.com/nianzhibai/91/backend/internal/scanjob"
	"github.com/nianzhibai/91/backend/internal/scanner"
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
func (a *App) runScan(ctx context.Context, driveID string) scanjob.Result {
	taskCtx, done, admitted := a.registerDriveTaskContext(ctx, driveID, driveTaskScopeScan)
	if !admitted {
		log.Printf("[scan] drive=%s configuration update in progress, reject direct scan", driveID)
		return skippedScanResult(driveID, "配置正在更新，本次扫描已跳过")
	}
	defer done()
	if !a.beginDriveScanOrCrawl(driveID) {
		log.Printf("[scan] drive=%s already queued or running, skip direct scan", driveID)
		return skippedScanResult(driveID, "该网盘已有扫描任务，本次扫描已跳过")
	}
	defer a.endDriveScanOrCrawl(driveID)
	return a.runScanWithTaskContext(taskCtx, driveID)
}

func skippedScanResult(driveID, message string) scanjob.Result {
	now := time.Now()
	return scanjob.Result{DriveID: driveID, State: scanjob.Skipped, Message: message, StartedAt: now, FinishedAt: now}
}

func (a *App) runScanWithTaskContext(ctx context.Context, driveID string) (report scanjob.Result) {
	ctx = applog.NewTask(ctx, "scan", driveID)
	applog.Info(ctx, "Scan started", applog.Fields{})
	report = scanjob.Result{DriveID: driveID, State: scanjob.Succeeded, StartedAt: time.Now()}
	defer func() {
		report.FinishedAt = time.Now()
		if report.State == scanjob.Succeeded && report.ErrorCount > 0 {
			report.State = scanjob.Partial
		}
		if err := ctx.Err(); err != nil {
			report.State = scanjob.Canceled
			report.Message = err.Error()
		}
		// Cancellation must not erase the outcome. This bounded write finishes
		// before the drive task admission is released, including during stop.
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := persistence.RLockContext(saveCtx); err != nil {
			report.AddIssue("save_result", err)
		} else {
			err := a.cat.SaveScanResult(saveCtx, report)
			persistence.RUnlock()
			if err != nil {
				report.AddIssue("save_result", err)
				log.Printf("[scan] save result drive=%s: %v", driveID, err)
			}
		}
		if report.State == scanjob.Succeeded && report.ErrorCount > 0 {
			report.State = scanjob.Partial
		}
		message := fmt.Sprintf("Scan finished state=%s scanned=%d added=%d cleaned=%d errors=%d", report.State, report.ScannedCount, report.AddedCount, report.CleanedCount, report.ErrorCount)
		switch report.State {
		case scanjob.Failed:
			applog.Error(ctx, message, nil, applog.Fields{})
		case scanjob.Partial, scanjob.Canceled:
			applog.Warn(ctx, message, nil, applog.Fields{})
		default:
			applog.Info(ctx, message, applog.Fields{})
		}
	}()
	fail := func(stage string, err error) {
		applog.Error(ctx, "Scan failed", err, applog.Fields{Stage: stage})
		report.State = scanjob.Failed
		report.Message = err.Error()
		if ctx.Err() == nil {
			report.AddIssue(stage, err)
		}
	}
	if err := ctx.Err(); err != nil {
		log.Printf("[scan] drive=%s canceled before start: %v", driveID, err)
		return report
	}
	if err := a.ensureDriveAttached(ctx, driveID); err != nil {
		log.Printf("[scan] drive=%s attach failed: %v", driveID, err)
		fail("attach", err)
		return report
	}
	drv, ok := a.registry.Get(driveID)
	if !ok {
		log.Printf("[scan] drive=%s not attached", driveID)
		fail("attach", errors.New("网盘未挂载"))
		return report
	}
	driveConfig, err := a.activeDriveConfig(ctx, driveID)
	if err != nil {
		log.Printf("[scan] get active drive config %s: %v", driveID, err)
		fail("config", err)
		return report
	}
	if resetter, ok := drv.(drives.GenerationStreamScanResetter); ok {
		resetter.ResetGenerationStreamForScan()
	}
	rateLimitBudget := scanner.NewRateLimitBudget()
	result, err := a.scanDrive(ctx, drv, driveConfig, rateLimitBudget)
	report.ScannedCount = result.Stats.Scanned
	report.AddedCount = result.Stats.Added
	report.UpdatedCount = result.Updated
	report.DuplicateCount = result.Duplicates
	report.TombstonedCount = result.Tombstoned
	for _, issue := range result.Issues {
		report.AddIssue(string(issue.Stage), issue)
	}
	if err != nil {
		fail("scan", err)
		if ctxErr := ctx.Err(); ctxErr != nil {
			log.Printf("[scan] drive=%s canceled: %v", driveID, ctxErr)
		} else if errors.Is(err, scanner.ErrRateLimitBudgetExhausted) {
			log.Printf("[scan] drive=%s rate-limit retry budget exhausted: %v", driveID, err)
		} else {
			log.Printf("[scan] drive=%s error: %v", driveID, err)
		}
		return report
	}
	if err := ctx.Err(); err != nil {
		log.Printf("[scan] drive=%s canceled after reconciliation: %v", driveID, err)
		return report
	}

	stats := result.Stats
	reconciliationIssues := len(result.Issues) - len(result.Snapshot.Issues)
	if reconciliationIssues < 0 {
		reconciliationIssues = 0
	}
	log.Printf(
		"[scan] drive=%s done scanned=%d added=%d updated=%d duplicates=%d tombstoned=%d enumerated_dirs=%d failed_dirs=%d excluded_dirs=%d discovery_issues=%d reconciliation_issues=%d errors=%d",
		driveID, stats.Scanned, stats.Added, result.Updated, result.Duplicates,
		result.Tombstoned, len(result.Snapshot.EnumeratedDirIDs), len(result.Snapshot.FailedDirIDs),
		len(result.Snapshot.ExcludedDirIDs), len(result.Snapshot.Issues), reconciliationIssues, stats.Errors,
	)

	// Reconciliation refreshes ancestry for every discovered catalog row before
	// skip-policy cleanup evaluates persisted chains. SeenFileIDs is an
	// additional guard for rows whose metadata update failed.
	skipCleanupResult, skipCleanupErr := a.cleanupSkippedDriveVideos(
		ctx, drv, driveConfig, result.Snapshot.SeenFileIDs, rateLimitBudget,
	)
	report.CleanedCount += skipCleanupResult.RemovedCount
	if skipCleanupErr != nil {
		applog.Error(ctx, "Skipped-directory cleanup failed", skipCleanupErr, applog.Fields{Stage: "skip_cleanup"})
		report.AddIssue("skip_cleanup", skipCleanupErr)
		if ctxErr := ctx.Err(); ctxErr != nil {
			log.Printf("[skip-cleanup] drive=%s canceled: %v", driveID, ctxErr)
			return report
		}
		if errors.Is(skipCleanupErr, scanner.ErrRateLimitBudgetExhausted) {
			log.Printf("[skip-cleanup] drive=%s rate-limit retry budget exhausted; continuing cleanup: %v", driveID, skipCleanupErr)
		} else {
			log.Printf("[skip-cleanup] drive=%s error; continuing cleanup: %v", driveID, skipCleanupErr)
		}
	}
	removed, err := a.cleanupScanResult(ctx, drv, result, skipCleanupResult.ProtectUnlocated)
	report.CleanedCount += removed
	if err != nil {
		applog.Error(ctx, "Missing-file cleanup failed", err, applog.Fields{Stage: "presence_cleanup"})
		report.AddIssue("presence_cleanup", err)
		if ctxErr := ctx.Err(); ctxErr != nil {
			log.Printf("[cleanup] canceled stale cleanup drive=%s kind=%s: %v", drv.ID(), drv.Kind(), ctxErr)
			return report
		}
		log.Printf("[cleanup] stale cleanup drive=%s kind=%s error: %v", drv.ID(), drv.Kind(), err)
	}
	if err := ctx.Err(); err != nil {
		log.Printf("[scan] drive=%s canceled before derived-task dispatch: %v", driveID, err)
		return report
	}

	a.mu.Lock()
	previewWorker := a.workers[driveID]
	thumbnailWorker := a.thumbWorkers[driveID]
	fingerprintWorker := a.fingerprintWorkers[driveID]
	a.mu.Unlock()
	// Reset failures once, after stale-source cleanup and before admitting new
	// generation. Recurring queue admission only picks up pending work, so a
	// retry that fails again remains failed until the next scan or manual retry.
	if _, err := a.resetFailedGeneration(ctx, driveID, catalog.GenerationKinds{
		Thumbnails:   thumbnailWorker != nil,
		Previews:     previewWorker != nil && a.previewEnabled(),
		Fingerprints: fingerprintWorker != nil,
	}); err != nil {
		report.AddIssue("generation_retry", err)
		log.Printf("[scan] reset failed generation drive=%s: %v", driveID, err)
		if ctx.Err() != nil {
			return report
		}
	}
	finishEnqueue := a.beginDriveResourceEnqueue(driveID)
	defer finishEnqueue()
	enqueueNewScanVideos(result.NewVideos, thumbnailWorker, fingerprintWorker)
	a.enqueueFingerprintBackfill(ctx, driveID, fingerprintWorker)
	a.enqueueDriveGeneration(ctx, driveID, previewWorker, thumbnailWorker)
	return report
}

func (a *App) scanDrive(
	ctx context.Context,
	drv drives.Drive,
	driveConfig *catalog.Drive,
	rateLimitBudget *scanner.RateLimitBudget,
) (scanner.Result, error) {
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
	a.configureScannerRetries(scan, drv, rateLimitBudget)
	scan.OnProgress = func(stats scanner.Stats) {
		a.updateDriveScanProgress(drv.ID(), stats.Scanned, stats.Added)
	}
	log.Printf("[scan] drive=%s start=%s skip_dirs=%d", drv.ID(), driveConfig.RootID, len(driveConfig.SkipDirIDs))
	return scan.Scan(ctx, driveConfig.RootID)
}

// cleanupScanResult derives a safe cleanup decision for each catalog row from
// the discovery directory sets. Complete discovery of the configured scope is
// authoritative enough for immediate cleanup. Reconciliation and skip-policy
// errors do not weaken the already-built presence snapshot. Incomplete
// discovery retains two-scan confirmation, while E/F/X classification protects
// failed and excluded subtrees.
func (a *App) cleanupScanResult(
	ctx context.Context,
	drv drives.Drive,
	result scanner.Result,
	protectUnlocated bool,
) (int, error) {
	if drv.Kind() == scriptcrawler.Kind || (drv.ID() == localupload.DriveID || drv.ID() == catalog.TelegramLocalDriveID) {
		return 0, nil
	}
	mode := missingFileCleanupMode(result.Snapshot)
	if mode == catalog.MissingFileCleanupConfirmTwice {
		log.Printf(
			"[cleanup] guarded stale cleanup drive=%s kind=%s enumerated_dirs=%d failed_dirs=%d discovery_issues=%d",
			drv.ID(), drv.Kind(), len(result.Snapshot.EnumeratedDirIDs),
			len(result.Snapshot.FailedDirIDs), len(result.Snapshot.Issues),
		)
	}
	if protectUnlocated {
		log.Printf(
			"[cleanup] protecting unlocated videos drive=%s kind=%s reason=incomplete_skip_policy_backfill",
			drv.ID(), drv.Kind(),
		)
	}
	presenceAuthoritative := result.Snapshot.PresenceAuthoritative()
	removed, err := a.cleanupMissingDriveVideos(
		ctx,
		drv.ID(),
		result.Snapshot.SeenFileIDs,
		catalog.ScanPresenceScope{
			EnumeratedDirIDs:      result.Snapshot.EnumeratedDirIDs,
			FailedDirIDs:          result.Snapshot.FailedDirIDs,
			ExcludedDirIDs:        result.Snapshot.ExcludedDirIDs,
			PresenceAuthoritative: presenceAuthoritative,
			ProtectUnlocated:      protectUnlocated,
		},
		mode,
	)
	if err != nil {
		return removed, err
	}
	if removed > 0 {
		modeName := "confirmed-twice"
		if mode == catalog.MissingFileCleanupImmediate {
			modeName = "immediate"
		}
		log.Printf("[cleanup] removed %d stale videos for drive=%s kind=%s mode=%s", removed, drv.ID(), drv.Kind(), modeName)
	}
	return removed, nil
}

func missingFileCleanupMode(snapshot scanner.Snapshot) catalog.MissingFileCleanupMode {
	if snapshot.PresenceAuthoritative() {
		return catalog.MissingFileCleanupImmediate
	}
	return catalog.MissingFileCleanupConfirmTwice
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
	scope catalog.ScanPresenceScope,
	mode catalog.MissingFileCleanupMode,
) (int, error) {
	fileIDsToRemove, err := func() (map[string]struct{}, error) {
		if err := persistence.RLockContext(ctx); err != nil {
			return nil, err
		}
		defer persistence.RUnlock()
		return a.cat.EvaluateMissingDriveFiles(
			ctx, driveID, liveFileIDs, scope, mode,
		)
	}()
	if err != nil {
		return 0, fmt.Errorf("evaluate missing drive files: %w", err)
	}
	if len(fileIDsToRemove) == 0 {
		return 0, nil
	}
	items, err := a.cat.ListVideosByDrive(ctx, driveID)
	if err != nil {
		return 0, err
	}

	missing := make([]*catalog.Video, 0, len(fileIDsToRemove))
	for _, video := range items {
		if _, ok := fileIDsToRemove[video.FileID]; ok {
			missing = append(missing, video)
		}
	}
	return a.deleteScanCleanupVideos(ctx, missing)
}

type skipCleanupResult struct {
	ProtectUnlocated bool
	RemovedCount     int
}

func (a *App) cleanupSkippedDriveVideos(
	ctx context.Context,
	drv drives.Drive,
	driveConfig *catalog.Drive,
	seenFileIDs map[string]struct{},
	rateLimitBudget *scanner.RateLimitBudget,
) (skipCleanupResult, error) {
	if drv == nil || driveConfig == nil || drv.Kind() == scriptcrawler.Kind || (drv.ID() == localupload.DriveID || drv.ID() == catalog.TelegramLocalDriveID) {
		return skipCleanupResult{}, nil
	}
	currentDirIDs := normalizedDirIDSet(driveConfig.SkipDirIDs)
	result := skipCleanupResult{ProtectUnlocated: len(currentDirIDs) > 0}
	state, err := a.cat.GetDriveSkipCleanupState(ctx, drv.ID())
	if err != nil {
		return result, fmt.Errorf("read cleanup progress: %w", err)
	}
	pendingLegacyDirIDs := pendingDirIDs(currentDirIDs, state.LegacyDoneDirIDs)
	result.ProtectUnlocated = len(pendingLegacyDirIDs) > 0
	if state.Initialized && len(pendingLegacyDirIDs) == 0 && equalDirIDSets(state.DirIDs, currentDirIDs) {
		return result, nil
	}

	exactItems, err := a.cat.ListVideosInAncestorDirs(ctx, drv.ID(), currentDirIDs)
	if err != nil {
		return result, fmt.Errorf("list videos selected by skip policy: %w", err)
	}
	exactItems = videosNotSeenInCurrentScan(exactItems, seenFileIDs)
	exactRemoved, err := a.deleteScanCleanupVideos(ctx, exactItems)
	result.RemovedCount += exactRemoved
	if err != nil {
		return result, fmt.Errorf("exact cleanup: %w", err)
	}
	if exactRemoved > 0 {
		log.Printf("[skip-cleanup] drive=%s removed=%d mode=exact", drv.ID(), exactRemoved)
	}

	var cleanupErrors []error
	if len(pendingLegacyDirIDs) > 0 {
		hasLegacyVideos, legacyQueryErr := a.cat.DriveHasVideosWithoutAncestorDirIDs(ctx, drv.ID())
		switch {
		case legacyQueryErr != nil:
			cleanupErrors = append(cleanupErrors,
				fmt.Errorf("check videos without ancestor directories: %w", legacyQueryErr))
		case !hasLegacyVideos:
			result.ProtectUnlocated = false
			for _, skippedDirID := range pendingLegacyDirIDs {
				if err := ctx.Err(); err != nil {
					return result, err
				}
				if err := a.cat.MarkDriveSkipCleanupLegacyDirDone(ctx, drv.ID(), skippedDirID); err != nil {
					cleanupErrors = append(cleanupErrors,
						fmt.Errorf("mark legacy cleanup complete for directory %s: %w", skippedDirID, err))
				}
			}
		default:
			legacyCleanupComplete := true
			for _, skippedDirID := range pendingLegacyDirIDs {
				removed, complete, cleanupErr := a.cleanupLegacySkippedDirectory(
					ctx, drv, currentDirIDs, skippedDirID, seenFileIDs, rateLimitBudget,
				)
				result.RemovedCount += removed
				if ctxErr := ctx.Err(); ctxErr != nil {
					return result, ctxErr
				}
				if cleanupErr != nil {
					legacyCleanupComplete = false
					if errors.Is(cleanupErr, scanner.ErrRateLimitBudgetExhausted) {
						return result, cleanupErr
					}
					cleanupErrors = append(cleanupErrors, cleanupErr)
					continue
				}
				if !complete {
					legacyCleanupComplete = false
					continue
				}
				if err := a.cat.MarkDriveSkipCleanupLegacyDirDone(ctx, drv.ID(), skippedDirID); err != nil {
					cleanupErrors = append(cleanupErrors,
						fmt.Errorf("mark legacy cleanup complete for directory %s: %w", skippedDirID, err))
				}
			}
			result.ProtectUnlocated = !legacyCleanupComplete
		}
	}

	// Exact cleanup completion is independent from per-directory legacy work.
	// Failed legacy directories stay pending without forcing completed siblings
	// to be traversed again.
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := a.cat.SetDriveSkipCleanupDirIDs(ctx, drv.ID(), currentDirIDs); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("record skip cleanup directory IDs: %w", err))
	}
	return result, errors.Join(cleanupErrors...)
}

func (a *App) cleanupLegacySkippedDirectory(
	ctx context.Context,
	drv drives.Drive,
	currentDirIDs []string,
	skippedDirID string,
	seenFileIDs map[string]struct{},
	rateLimitBudget *scanner.RateLimitBudget,
) (int, bool, error) {
	videoExtensions := []string(nil)
	if a.cfg != nil {
		videoExtensions = a.cfg.Scanner.VideoExtensions
	}
	scan := scanner.New(a.cat, drv, videoExtensions, currentDirIDs, nil)
	scan.LogPrefix = "skip-cleanup"
	a.configureScannerRetries(scan, drv, rateLimitBudget)
	snapshot, _, discoverErr := scan.Discover(ctx, skippedDirID)
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}

	legacyDirIDs := make([]string, 0, len(snapshot.EnumeratedDirIDs)+1)
	legacyDirIDs = append(legacyDirIDs, skippedDirID)
	for dirID := range snapshot.EnumeratedDirIDs {
		legacyDirIDs = append(legacyDirIDs, dirID)
	}
	legacyItems, err := a.cat.ListVideosByParentDirIDs(ctx, drv.ID(), legacyDirIDs)
	if err != nil {
		return 0, false, fmt.Errorf("list legacy videos under skipped directory %s: %w", skippedDirID, err)
	}
	legacyItems = videosNotSeenInCurrentScan(legacyItems, seenFileIDs)
	legacyRemoved, err := a.deleteScanCleanupVideos(ctx, legacyItems)
	if err != nil {
		return legacyRemoved, false, fmt.Errorf("legacy cleanup under skipped directory %s: %w", skippedDirID, err)
	}
	if legacyRemoved > 0 {
		log.Printf("[skip-cleanup] drive=%s dir=%s removed=%d mode=legacy", drv.ID(), skippedDirID, legacyRemoved)
	}
	if discoverErr != nil {
		if errors.Is(discoverErr, scanner.ErrRateLimitBudgetExhausted) {
			return legacyRemoved, false, discoverErr
		}
		log.Printf("[skip-cleanup] drive=%s dir=%s legacy discovery incomplete: %v", drv.ID(), skippedDirID, discoverErr)
		return legacyRemoved, false, nil
	}
	if !snapshot.Complete() {
		log.Printf(
			"[skip-cleanup] drive=%s dir=%s legacy discovery incomplete failed_dirs=%d",
			drv.ID(), skippedDirID, len(snapshot.FailedDirIDs),
		)
		return legacyRemoved, false, nil
	}
	return legacyRemoved, true, nil
}

func videosNotSeenInCurrentScan(videos []*catalog.Video, seenFileIDs map[string]struct{}) []*catalog.Video {
	if len(videos) == 0 || len(seenFileIDs) == 0 {
		return videos
	}
	filtered := make([]*catalog.Video, 0, len(videos))
	for _, video := range videos {
		if video != nil {
			if _, seen := seenFileIDs[video.FileID]; seen {
				continue
			}
		}
		filtered = append(filtered, video)
	}
	return filtered
}

func (a *App) deleteScanCleanupVideos(ctx context.Context, videos []*catalog.Video) (int, error) {
	if len(videos) == 0 {
		return 0, nil
	}
	if err := persistence.RLockContext(ctx); err != nil {
		return 0, err
	}
	defer persistence.RUnlock()

	localDir := ""
	if a.cfg != nil {
		localDir = a.cfg.Storage.LocalPreviewDir
	}
	removed := 0
	for _, video := range videos {
		if video == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if err := removeLocalVideoAssets(localDir, video); err != nil {
			return removed, fmt.Errorf("remove local assets for %s: %w", video.ID, err)
		}
		if err := a.cat.DeleteVideo(ctx, video.ID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return removed, fmt.Errorf("delete catalog video %s: %w", video.ID, err)
		}
		removed++
	}
	return removed, nil
}

func normalizedDirIDSet(dirIDs []string) []string {
	seen := make(map[string]struct{}, len(dirIDs))
	out := make([]string, 0, len(dirIDs))
	for _, dirID := range dirIDs {
		dirID = strings.TrimSpace(dirID)
		if dirID == "" {
			continue
		}
		if _, exists := seen[dirID]; exists {
			continue
		}
		seen[dirID] = struct{}{}
		out = append(out, dirID)
	}
	return out
}

func pendingDirIDs(current, completed []string) []string {
	completedSet := make(map[string]struct{}, len(completed))
	for _, dirID := range normalizedDirIDSet(completed) {
		completedSet[dirID] = struct{}{}
	}
	pending := make([]string, 0, len(current))
	for _, dirID := range normalizedDirIDSet(current) {
		if _, done := completedSet[dirID]; !done {
			pending = append(pending, dirID)
		}
	}
	return pending
}

func equalDirIDSets(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	rightSet := make(map[string]struct{}, len(right))
	for _, dirID := range right {
		rightSet[dirID] = struct{}{}
	}
	for _, dirID := range left {
		if _, exists := rightSet[dirID]; !exists {
			return false
		}
	}
	return true
}

func (a *App) configureScannerRetries(
	scan *scanner.Scanner,
	drv drives.Drive,
	rateLimitBudget *scanner.RateLimitBudget,
) {
	if scan == nil || drv == nil {
		return
	}
	if rateLimitBudget != nil {
		scan.RateLimitBudget = rateLimitBudget
	}
	scan.OnCooldown = func(until time.Time) {
		a.updateDriveScanCooldown(drv.ID(), until)
	}
}
