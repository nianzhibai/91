package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/api"
	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/drives/scriptcrawler"
	"github.com/nianzhibai/91/backend/internal/fingerprint"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
	"github.com/nianzhibai/91/backend/internal/persistence"
)

// migrateHiddenVideosToTombstone 把历史「隐藏」视频一次性迁移为黑名单墓碑。
// 隐藏机制已废弃——前台「不再展示」改走拉黑逻辑。迁移＝删库记录 + 删本地
// 封面/预览 + 写墓碑，保留网盘源文件。等待有效大小的记录保留在视频库，供复扫恢复。
func (a *App) migrateHiddenVideosToTombstone(ctx context.Context) {
	if a == nil || a.cat == nil {
		return
	}
	hidden, err := a.cat.ListLegacyHiddenVideos(ctx)
	if err != nil {
		applog.Error(ctx, "list legacy hidden videos failed", err, "component", "migrate")
		return
	}
	if len(hidden) == 0 {
		return
	}
	applog.Info(ctx, "converting legacy hidden videos to blacklist tombstones", "component", "migrate", "count", len(hidden))
	migrated := 0
	for _, v := range hidden {
		if err := a.cat.DeleteVideoWithTombstoneOptions(ctx, v.ID, catalog.DeleteVideoTombstoneOptions{AssetCleanupAfter: time.Now().Add(videoDeletionGracePeriod)}); err != nil {
			applog.Warn(ctx, "convert hidden video to tombstone failed", err, "component", "migrate", "videoId", v.ID)
			continue
		}
		migrated++
	}
	applog.Info(ctx, "legacy hidden video migration completed", "component", "migrate", "migrated", migrated, "total", len(hidden))
}

func (a *App) deleteVideo(ctx context.Context, videoID string, options catalog.VideoDeletionOptions) (api.DeleteVideoResult, error) {
	if a == nil || a.cat == nil {
		return api.DeleteVideoResult{}, sql.ErrNoRows
	}
	unlock := a.blacklistVideoLocks.lock(videoID)
	defer unlock()
	if err := persistence.RLockContext(ctx); err != nil {
		return api.DeleteVideoResult{}, err
	}
	defer persistence.RUnlock()
	localDir := ""
	if a.cfg != nil {
		localDir = a.cfg.Storage.LocalPreviewDir
	}
	clean := func(ctx context.Context, v *catalog.Video) error {
		if localDir == "" {
			return nil
		}
		return a.cleanupRetiredVideoAssets(ctx, localDir, v.ID)
	}
	err := a.cat.DeleteVideoGroup(ctx, videoID, options, time.Now().Add(videoDeletionGracePeriod), a.removeVideoSource, clean)
	return api.DeleteVideoResult{OK: err == nil, DeletedSource: err == nil && options.DeleteSource}, err
}

func (a *App) startBlacklistSourceDelete(ctx context.Context, req api.BlacklistSourceDeleteRequest) bool {
	if a == nil || a.cat == nil {
		return false
	}
	req = normalizeBlacklistSourceDeleteRequest(req)
	a.blacklistSourceDeleteMu.Lock()
	if a.blacklistSourceDeleteState.Running {
		a.blacklistSourceDeleteMu.Unlock()
		return false
	}
	a.blacklistSourceDeleteState = api.BlacklistSourceDeleteStatus{
		State:     "running",
		Running:   true,
		StartedAt: time.Now().Format(time.RFC3339),
	}
	a.blacklistSourceDeleteMu.Unlock()

	go a.runBlacklistSourceDelete(ctx, req)
	return true
}

func normalizeBlacklistSourceDeleteRequest(req api.BlacklistSourceDeleteRequest) api.BlacklistSourceDeleteRequest {
	if req.DeleteAllSources {
		return api.BlacklistSourceDeleteRequest{DeleteAllSources: true}
	}
	seen := make(map[string]bool, len(req.IDs))
	ids := make([]string, 0, len(req.IDs))
	for _, id := range req.IDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return api.BlacklistSourceDeleteRequest{DeleteAllSources: true}
	}
	return api.BlacklistSourceDeleteRequest{IDs: ids}
}

func (a *App) blacklistSourceDeleteStatus() api.BlacklistSourceDeleteStatus {
	if a == nil {
		return api.BlacklistSourceDeleteStatus{State: "idle"}
	}
	a.blacklistSourceDeleteMu.Lock()
	defer a.blacklistSourceDeleteMu.Unlock()
	status := a.blacklistSourceDeleteState
	if status.State == "" {
		status.State = "idle"
	}
	return status
}

func (a *App) runBlacklistSourceDelete(ctx context.Context, reqs ...api.BlacklistSourceDeleteRequest) {
	req := api.BlacklistSourceDeleteRequest{DeleteAllSources: true}
	if len(reqs) > 0 {
		req = normalizeBlacklistSourceDeleteRequest(reqs[0])
	}

	var (
		items []*catalog.DeletedVideo
		err   error
	)
	if req.DeleteAllSources {
		items, err = a.cat.ListDeletedVideosPendingSourceDeletion(ctx)
	} else {
		items, err = a.cat.ListDeletedVideosPendingSourceDeletionByIDs(ctx, req.IDs)
	}
	if err != nil {
		a.finishBlacklistSourceDelete("failed", err)
		return
	}

	a.blacklistSourceDeleteMu.Lock()
	a.blacklistSourceDeleteState.Total = len(items)
	a.blacklistSourceDeleteMu.Unlock()

	for index, item := range items {
		if err := ctx.Err(); err != nil {
			a.finishBlacklistSourceDelete("canceled", err)
			return
		}
		if item == nil {
			continue
		}

		a.blacklistSourceDeleteMu.Lock()
		a.blacklistSourceDeleteState.CurrentFile = item.FileName
		if a.blacklistSourceDeleteState.CurrentFile == "" {
			a.blacklistSourceDeleteState.CurrentFile = item.ID
		}
		a.blacklistSourceDeleteMu.Unlock()

		skipped, deleteErr := a.deleteBlacklistedVideoSource(ctx, item)

		a.blacklistSourceDeleteMu.Lock()
		a.blacklistSourceDeleteState.Processed++
		if deleteErr != nil {
			a.blacklistSourceDeleteState.Failed++
			a.blacklistSourceDeleteState.LastError = deleteErr.Error()
		} else if skipped {
			a.blacklistSourceDeleteState.Skipped++
		} else {
			a.blacklistSourceDeleteState.Deleted++
		}
		a.blacklistSourceDeleteMu.Unlock()

		if deleteErr != nil {
			applog.Error(ctx, "delete blacklisted video source failed", deleteErr,
				"component", "blacklist-source-delete",
				"videoId", item.ID,
				"driveId", item.DriveID,
				"fileId", item.FileID,
			)
		} else if skipped {
			applog.Info(ctx, "skipped blacklisted video source deletion; tombstone changed or no longer pending",
				"component", "blacklist-source-delete",
				"videoId", item.ID,
			)
		} else {
			applog.Info(ctx, "deleted blacklisted video source",
				"component", "blacklist-source-delete",
				"videoId", item.ID,
				"driveId", item.DriveID,
				"fileId", item.FileID,
			)
		}

		if index+1 < len(items) {
			if err := waitForBlacklistSourceDelete(ctx, blacklistSourceDeletePace); err != nil {
				a.finishBlacklistSourceDelete("canceled", err)
				return
			}
		}
	}

	a.finishBlacklistSourceDelete("completed", nil)
}

// deleteBlacklistedVideoSource revalidates a snapshot item while holding the
// same per-video lock used by direct restore. A restored, claimed, or newly
// recreated tombstone is a skip, not permission to delete the stale snapshot's
// source file.
func (a *App) deleteBlacklistedVideoSource(ctx context.Context, snapshot *catalog.DeletedVideo) (bool, error) {
	if snapshot == nil {
		return false, errors.New("remove blacklisted source: empty tombstone")
	}
	unlock := a.blacklistVideoLocks.lock(snapshot.ID)
	defer unlock()
	if err := persistence.RLockContext(ctx); err != nil {
		return false, err
	}
	defer persistence.RUnlock()

	items, err := a.cat.ListDeletedVideosPendingSourceDeletionByIDs(ctx, []string{snapshot.ID})
	if err != nil {
		return false, err
	}
	if len(items) == 0 {
		return true, nil
	}
	current := items[0]
	if current.DeletedAt != snapshot.DeletedAt ||
		current.DriveID != snapshot.DriveID ||
		current.FileID != snapshot.FileID ||
		current.ParentID != snapshot.ParentID ||
		current.FileName != snapshot.FileName ||
		current.Size != snapshot.Size ||
		current.Reason != snapshot.Reason {
		return true, nil
	}
	// Finish any retained-asset cleanup before removing the source. Otherwise
	// an asset cleanup failure could leave an undoable tombstone whose source
	// has already been deleted.
	if err := a.cleanupDeletedVideoAssets(ctx, current.ID, time.Now(), true); err != nil {
		return false, err
	}
	if err := a.removeDeletedVideoSourceFile(ctx, current); err != nil {
		if errors.Is(err, catalog.ErrVideoSourceInUse) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func (a *App) removeDeletedVideoSourceFile(ctx context.Context, item *catalog.DeletedVideo) error {
	if item == nil {
		return errors.New("remove blacklisted source: empty tombstone")
	}
	if strings.TrimSpace(item.FileID) == "" {
		return fmt.Errorf("remove blacklisted source %s: empty file id", item.ID)
	}
	gate := a.driveOperationGate(item.DriveID)
	taskRelease, generation, admitted := gate.beginTask(ctx, 0)
	if !admitted {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fmt.Errorf("remove blacklisted source %s: drive configuration changed", item.ID)
	}
	defer taskRelease()
	ctx = withDriveTaskAdmission(ctx, gate, generation)
	video := &catalog.Video{
		ID:       item.ID,
		DriveID:  item.DriveID,
		FileID:   item.FileID,
		ParentID: item.ParentID,
		FileName: item.FileName,
		Size:     item.Size,
	}
	var lastErr error
	for attempt := 0; attempt < blacklistSourceDeleteMaxAttempts; attempt++ {
		err := a.cat.WithBlacklistedSourceRemoval(ctx, item, func(ctx context.Context) error {
			merged, err := a.cat.GetMergedVideo(ctx, item.ID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			check := liveSource
			if merged != nil {
				check = mergedSource
				if merged.SourceState != catalog.MergedSourceRecorded {
					return catalog.ErrMergedSourceReplaced
				}
				video.ContentHash, video.SampledSHA256, video.SourceModTime = merged.ContentHash, merged.SampledSHA256, merged.SourceModTime
			}
			if err := a.removeProviderSource(ctx, video, check); err != nil {
				return err
			}
			// The remote deletion has succeeded. Finish the catalog write even
			// if the task was canceled while the provider was responding.
			finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			return a.recordDeletedSource(finishCtx, video)
		})
		if err == nil {
			return nil
		}
		lastErr = err
		wait, rateLimited := drives.RateLimitRetryAfter(err)
		if !rateLimited && drives.TextMentionsHTTPStatus(err.Error(), http.StatusTooManyRequests) {
			rateLimited = true
		}
		if !rateLimited || attempt+1 >= blacklistSourceDeleteMaxAttempts {
			return err
		}
		if wait <= 0 {
			wait = blacklistSourceDeleteDefaultCooldown
		}
		a.blacklistSourceDeleteMu.Lock()
		a.blacklistSourceDeleteState.LastError = fmt.Sprintf("%s 限流，等待 %s 后重试", item.FileName, wait)
		a.blacklistSourceDeleteMu.Unlock()
		applog.Warn(ctx, "blacklisted video source deletion rate limited; retrying", err,
			"component", "blacklist-source-delete",
			"videoId", item.ID,
			"driveId", item.DriveID,
			"retryIn", wait,
			"attempt", attempt+1,
		)
		if err := waitForBlacklistSourceDelete(ctx, wait); err != nil {
			return err
		}
	}
	return lastErr
}

func waitForBlacklistSourceDelete(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *App) recordDeletedSource(ctx context.Context, item *catalog.Video) error {
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := a.cat.RecordSourceDeleted(ctx, item); err != nil {
			if !isSQLiteBusyError(err) {
				return err
			}
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
			continue
		}
		return nil
	}
	return fmt.Errorf("record deleted blacklisted source after retries: %w", lastErr)
}

func (a *App) finishBlacklistSourceDelete(state string, err error) {
	a.blacklistSourceDeleteMu.Lock()
	defer a.blacklistSourceDeleteMu.Unlock()
	a.blacklistSourceDeleteState.State = state
	a.blacklistSourceDeleteState.Running = false
	a.blacklistSourceDeleteState.CurrentFile = ""
	a.blacklistSourceDeleteState.LastFinished = time.Now().Format(time.RFC3339)
	if err != nil {
		a.blacklistSourceDeleteState.LastError = err.Error()
	}
}

func (a *App) deleteMergedSource(ctx context.Context, id, revision string) error {
	if err := persistence.RLockContext(ctx); err != nil {
		return err
	}
	defer persistence.RUnlock()
	return a.cat.RemoveMergedVideoSource(ctx, id, revision, func(ctx context.Context, v *catalog.Video) error {
		return a.removeProviderSource(ctx, v, mergedSource)
	})
}

func (a *App) removeVideoSource(ctx context.Context, v *catalog.Video, historical bool) error {
	check := liveSource
	if historical {
		_, err := a.cat.GetMergedVideo(ctx, v.ID)
		switch {
		case err == nil:
			check = mergedSource
		case errors.Is(err, sql.ErrNoRows):
			check = skippedScanSource
		default:
			return err
		}
	}
	return a.removeProviderSource(ctx, v, check)
}

type sourceIdentityCheck int

const (
	liveSource sourceIdentityCheck = iota
	skippedScanSource
	mergedSource
)

func (a *App) removeProviderSource(ctx context.Context, v *catalog.Video, check sourceIdentityCheck) error {
	if v == nil {
		return errors.New("remove video source: empty video")
	}
	if a == nil {
		return fmt.Errorf("remove video source %s: app unavailable: %w", v.ID, drives.ErrNotSupported)
	}
	fileID := v.FileID
	if strings.TrimSpace(fileID) == "" {
		return fmt.Errorf("remove video source %s: empty file id", v.ID)
	}
	gate := a.driveOperationGate(v.DriveID)
	taskRelease, generation, admitted := gate.tryBeginTask(ctx, 0)
	if !admitted {
		return fmt.Errorf("remove video source %s: drive %s configuration update in progress", v.ID, v.DriveID)
	}
	defer taskRelease()
	ctx = withDriveTaskAdmission(ctx, gate, generation)
	if a.registry == nil {
		return fmt.Errorf("remove video source %s: drive registry unavailable: %w", v.ID, drives.ErrNotSupported)
	}
	if _, ok := a.registry.Get(v.DriveID); !ok {
		if err := a.ensureDriveAttached(ctx, v.DriveID); err != nil {
			return fmt.Errorf("remove video source %s: attach drive %s: %w", v.ID, v.DriveID, err)
		}
	}
	drv, ok := a.registry.Get(v.DriveID)
	if !ok {
		return fmt.Errorf("remove video source %s: drive %s not attached: %w", v.ID, v.DriveID, drives.ErrNotSupported)
	}
	if check != liveSource {
		entry, err := historicalSourceEntry(ctx, drv, v)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry == nil {
			return catalog.ErrMergedSourceUnverified
		}
		if entry.IsDir || entry.ID != fileID || entry.Size != v.Size {
			return catalog.ErrMergedSourceReplaced
		}
		switch {
		case v.ContentHash != "" && entry.Hash != "":
			if !strings.EqualFold(v.ContentHash, entry.Hash) {
				return catalog.ErrMergedSourceReplaced
			}
		case v.SourceModTime != "" && !entry.ModTime.IsZero():
			if catalog.FormatSourceModTime(entry.ModTime) != v.SourceModTime {
				return catalog.ErrMergedSourceReplaced
			}
		case check == mergedSource && v.SampledSHA256 != "":
			sum, err := fingerprint.Compute(ctx, drv, v, fingerprint.Config{}, nil)
			if err != nil {
				return fmt.Errorf("%w: %v", catalog.ErrMergedSourceUnverified, err)
			}
			if !strings.EqualFold(sum, v.SampledSHA256) {
				return catalog.ErrMergedSourceReplaced
			}
		case check == mergedSource:
			return catalog.ErrMergedSourceUnverified
		case entry.Name != v.FileName:
			return catalog.ErrMergedSourceReplaced
		}
		// Providers may require up-to-date directory and filename metadata.
		current := *v
		current.FileName, current.Size = entry.Name, entry.Size
		if entry.ParentID != "" {
			current.ParentID = entry.ParentID
		}
		v = &current
	}
	if sourceRemover, ok := drv.(drives.SourceRemover); ok {
		if err := sourceRemover.RemoveSource(ctx, drives.SourceFile{
			FileID:   fileID,
			ParentID: strings.TrimSpace(v.ParentID),
			Name:     strings.TrimSpace(v.FileName),
			Size:     v.Size,
		}); err != nil {
			return fmt.Errorf("remove video source %s from drive %s: %w", v.ID, v.DriveID, err)
		}
		return nil
	}
	remover, ok := drv.(drives.Remover)
	if !ok {
		return fmt.Errorf("remove video source %s: drive %s (%s) does not support source deletion: %w", v.ID, v.DriveID, drv.Kind(), drives.ErrNotSupported)
	}
	if err := remover.Remove(ctx, fileID); err != nil {
		return fmt.Errorf("remove video source %s from drive %s: %w", v.ID, v.DriveID, err)
	}
	return nil
}

// Some providers expose file metadata only through the parent listing.
func historicalSourceEntry(ctx context.Context, drive drives.Drive, v *catalog.Video) (*drives.Entry, error) {
	entry, err := drive.Stat(ctx, v.FileID)
	if !errors.Is(err, drives.ErrNotSupported) {
		return entry, err
	}
	if v.ParentID == "" {
		return nil, catalog.ErrMergedSourceUnverified
	}
	entries, err := drive.List(ctx, v.ParentID)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, drives.ErrNotSupported) {
		return nil, catalog.ErrMergedSourceUnverified
	}
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if entries[i].ID == v.FileID {
			return &entries[i], nil
		}
	}
	return nil, fmt.Errorf("%w: 原目录中找不到文件 %s", catalog.ErrMergedSourceUnverified, v.FileID)
}

// restoreDeletedVideo coordinates expired asset cleanup, provider inspection,
// and restoration under the same per-video lock used by deletion. Undoing a
// deletion retains ready assets; missing assets enter the normal queues.
func (a *App) restoreDeletedVideo(ctx context.Context, videoID string) error {
	if a == nil || a.cat == nil {
		return sql.ErrNoRows
	}
	videoID = strings.TrimSpace(videoID)
	if videoID == "" {
		return sql.ErrNoRows
	}
	unlock := a.blacklistVideoLocks.lock(videoID)
	defer unlock()
	if err := persistence.RLockContext(ctx); err != nil {
		return err
	}
	if err := a.cleanupDeletedVideoAssets(ctx, videoID, time.Now(), false); err != nil {
		persistence.RUnlock()
		return err
	}

	inspectSource := func(driveID, fileID string) (catalog.DeletedVideoSourceInfo, error) {
		return a.inspectRestorableSource(ctx, driveID, fileID)
	}
	result, err := a.cat.RestoreDeletedVideo(ctx, videoID, inspectSource)
	if errors.Is(err, catalog.ErrDeletedVideoAssetCleanupRequired) {
		// The deadline may have passed between the initial cleanup check and
		// the restore transaction. Finish cleanup before removing its owner.
		err = a.cleanupDeletedVideoAssets(ctx, videoID, time.Now(), false)
		if err == nil {
			result, err = a.cat.RestoreDeletedVideo(ctx, videoID, inspectSource)
		}
	}
	persistence.RUnlock()
	if err != nil {
		return err
	}
	if result.Video != nil {
		a.wakeDriveGeneration(result.Video.DriveID)
		if a.onTagsChanged != nil {
			a.onTagsChanged()
		}
	}
	return nil
}

// inspectRestorableSource confirms that a direct-restore source is a playable
// regular file and returns provider-owned metadata used to repair stale size,
// timestamps, and fingerprints.
func (a *App) inspectRestorableSource(ctx context.Context, driveID, fileID string) (catalog.DeletedVideoSourceInfo, error) {
	driveID = strings.TrimSpace(driveID)
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return catalog.DeletedVideoSourceInfo{}, fmt.Errorf("restore from drive %s: empty file id", driveID)
	}
	if a == nil || a.registry == nil {
		return catalog.DeletedVideoSourceInfo{}, fmt.Errorf("restore from drive %s: drive registry unavailable: %w", driveID, drives.ErrNotSupported)
	}
	gate := a.driveOperationGate(driveID)
	taskRelease, generation, admitted := gate.tryBeginTask(ctx, 0)
	if !admitted {
		return catalog.DeletedVideoSourceInfo{}, fmt.Errorf("restore from drive %s: configuration update in progress", driveID)
	}
	defer taskRelease()
	ctx = withDriveTaskAdmission(ctx, gate, generation)
	if _, ok := a.registry.Get(driveID); !ok {
		if err := a.ensureDriveAttached(ctx, driveID); err != nil {
			return catalog.DeletedVideoSourceInfo{}, fmt.Errorf("restore from drive %s: attach drive: %w", driveID, err)
		}
	}
	drv, ok := a.registry.Get(driveID)
	if !ok {
		return catalog.DeletedVideoSourceInfo{}, fmt.Errorf("restore from drive %s: drive not attached: %w", driveID, drives.ErrNotSupported)
	}
	entry, err := drv.Stat(ctx, fileID)
	if err != nil {
		return catalog.DeletedVideoSourceInfo{}, fmt.Errorf("restore from drive %s: stat %s: %w", driveID, fileID, err)
	}
	if entry == nil || entry.IsDir {
		return catalog.DeletedVideoSourceInfo{}, fmt.Errorf("restore from drive %s: %s is not a regular file", driveID, fileID)
	}
	if entry.Size <= 0 {
		return catalog.DeletedVideoSourceInfo{}, fmt.Errorf("restore from drive %s: %s is empty", driveID, fileID)
	}
	return catalog.DeletedVideoSourceInfo{Size: entry.Size, ModTime: entry.ModTime}, nil
}

func (a *App) cleanupDriveVideosForDelete(ctx context.Context, driveID string) (int, error) {
	if a == nil || a.cat == nil {
		return 0, nil
	}
	d, err := a.cat.GetDrive(ctx, driveID)
	if err != nil {
		return 0, err
	}

	// The delete coordinator has already blocked admissions and waited for all
	// generation/crawl workers to exit. Keep the runtime attached until cleanup
	// succeeds so an I/O failure does not leave an otherwise existing drive
	// detached; the successful-delete hook retires it after the catalog row goes.
	items, err := a.videosForDriveDelete(ctx, d)
	if err != nil {
		return 0, err
	}

	localDir := ""
	if a.cfg != nil {
		localDir = a.cfg.Storage.LocalPreviewDir
	}
	persistence.RLock()
	defer persistence.RUnlock()
	for _, v := range items {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if err := removeLocalVideoAssets(localDir, v); err != nil {
			return 0, fmt.Errorf("remove local assets for %s: %w", v.ID, err)
		}
	}
	if d.Kind == scriptcrawler.Kind {
		if err := a.removeScriptCrawlerStorageForDelete(d.ID); err != nil {
			return 0, fmt.Errorf("remove crawler storage for %s: %w", d.ID, err)
		}
	}

	removed := 0
	for _, v := range items {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if err := a.cat.DeleteVideo(ctx, v.ID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return removed, fmt.Errorf("delete catalog video %s: %w", v.ID, err)
		}
		removed++
	}
	return removed, nil
}

// removeScriptCrawlerStorageForDelete removes the application-owned source
// tree for a deleted crawler. The containment check is deliberately repeated
// here instead of trusting the drive ID: this is a recursive destructive
// operation and must never be able to target the shared scriptcrawlers root or
// a path outside it.
func (a *App) removeScriptCrawlerStorageForDelete(driveID string) error {
	if a == nil || a.cfg == nil {
		return errors.New("crawler storage root is unavailable")
	}
	root := a.scriptCrawlerRootDir()
	rootPath, rootOK := localPathWithin(root, root)
	targetPath, targetOK := localPathWithin(root, a.scriptCrawlerDriveDir(driveID))
	if !rootOK || !targetOK || targetPath == rootPath {
		return fmt.Errorf("unsafe crawler storage path for drive %q", driveID)
	}
	if err := os.RemoveAll(targetPath); err != nil {
		return err
	}
	return nil
}

func (a *App) videosForDriveDelete(ctx context.Context, d *catalog.Drive) ([]*catalog.Video, error) {
	if d == nil {
		return nil, nil
	}
	items, err := a.cat.ListVideosByDrive(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*catalog.Video, len(items))
	for _, v := range items {
		byID[v.ID] = v
	}

	out := make([]*catalog.Video, 0, len(byID))
	for _, v := range byID {
		out = append(out, v)
	}
	return out, nil
}

func removeLocalVideoAssets(localDir string, v *catalog.Video) error {
	if localDir == "" || v == nil || v.ID == "" {
		return nil
	}
	return mediaasset.RemoveGeneratedVideoAssets(localDir, v.ID, v.PreviewLocal)
}
