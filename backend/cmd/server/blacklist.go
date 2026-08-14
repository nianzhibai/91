package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/video-site/backend/internal/api"
	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/drives/scriptcrawler"
	"github.com/video-site/backend/internal/mediaasset"
	"github.com/video-site/backend/internal/persistence"
)

// migrateHiddenVideosToTombstone 把历史「隐藏」视频一次性迁移为黑名单墓碑。
// 隐藏机制已废弃——前台「不再展示」改走拉黑逻辑。迁移＝删库记录 + 删本地
// 封面/预览 + 写墓碑，保留网盘源文件。迁移后无 hidden=1 记录，重复执行为空操作。
func (a *App) migrateHiddenVideosToTombstone(ctx context.Context) {
	if a == nil || a.cat == nil {
		return
	}
	hidden, err := a.cat.ListHiddenVideos(ctx)
	if err != nil {
		log.Printf("[migrate] list hidden videos: %v", err)
		return
	}
	if len(hidden) == 0 {
		return
	}
	log.Printf("[migrate] converting %d hidden video(s) to blacklist tombstones", len(hidden))
	migrated := 0
	for _, v := range hidden {
		if _, err := a.deleteVideo(ctx, v.ID, false); err != nil {
			log.Printf("[migrate] hidden->tombstone %s: %v", v.ID, err)
			continue
		}
		migrated++
	}
	log.Printf("[migrate] hidden->tombstone done: %d/%d", migrated, len(hidden))
}

func (a *App) deleteVideo(ctx context.Context, videoID string, deleteSource bool) (api.DeleteVideoResult, error) {
	if a == nil || a.cat == nil {
		return api.DeleteVideoResult{}, sql.ErrNoRows
	}
	v, err := a.cat.GetVideo(ctx, videoID)
	if err != nil {
		return api.DeleteVideoResult{}, err
	}

	// Source deletion can remove a local-upload or crawler file that belongs in
	// a full backup. Keep source, derived assets, and the catalog tombstone on
	// the same side of the persistence snapshot gate.
	persistence.RLock()
	defer persistence.RUnlock()
	deletedSource := false
	if deleteSource {
		deletedSource, err = a.removeVideoSourceFile(ctx, v)
		if err != nil {
			return api.DeleteVideoResult{}, err
		}
	}

	localDir := ""
	if a.cfg != nil {
		localDir = a.cfg.Storage.LocalPreviewDir
	}
	if err := removeLocalVideoAssets(localDir, v); err != nil {
		return api.DeleteVideoResult{}, fmt.Errorf("remove local assets for %s: %w", v.ID, err)
	}
	if err := a.cat.DeleteVideoWithTombstoneOptions(ctx, v.ID, catalog.DeleteVideoTombstoneOptions{
		SourceDeleted: deletedSource,
	}); err != nil {
		return api.DeleteVideoResult{}, err
	}
	return api.DeleteVideoResult{OK: true, DeletedSource: deletedSource}, nil
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
			log.Printf("[blacklist-source-delete] id=%s drive=%s file=%s failed: %v", item.ID, item.DriveID, item.FileID, deleteErr)
		} else if skipped {
			log.Printf("[blacklist-source-delete] id=%s skipped: tombstone changed or is no longer pending", item.ID)
		} else {
			log.Printf("[blacklist-source-delete] id=%s drive=%s file=%s deleted", item.ID, item.DriveID, item.FileID)
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
	if err := a.removeDeletedVideoSourceFile(ctx, current); err != nil {
		return false, err
	}
	if err := a.purgeDeletedVideoTombstone(ctx, current.ID); err != nil {
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
		_, err := a.removeVideoSourceFile(ctx, video)
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
		log.Printf("[blacklist-source-delete] id=%s drive=%s rate limited; retry_in=%s attempt=%d", item.ID, item.DriveID, wait, attempt+1)
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

func (a *App) purgeDeletedVideoTombstone(ctx context.Context, videoID string) error {
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := a.cat.PurgeDeletedVideo(ctx, videoID); err != nil {
			if !isSQLiteBusyError(err) {
				return err
			}
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
			continue
		}
		return nil
	}
	return fmt.Errorf("purge blacklisted tombstone after retries: %w", lastErr)
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

func (a *App) removeVideoSourceFile(ctx context.Context, v *catalog.Video) (bool, error) {
	if v == nil {
		return false, errors.New("remove video source: empty video")
	}
	if a == nil {
		return false, fmt.Errorf("remove video source %s: app unavailable: %w", v.ID, drives.ErrNotSupported)
	}
	fileID := strings.TrimSpace(v.FileID)
	if fileID == "" {
		return false, fmt.Errorf("remove video source %s: empty file id", v.ID)
	}
	if a == nil || a.registry == nil {
		return false, fmt.Errorf("remove video source %s: drive registry unavailable: %w", v.ID, drives.ErrNotSupported)
	}
	if _, ok := a.registry.Get(v.DriveID); !ok {
		if a.cat == nil {
			return false, fmt.Errorf("remove video source %s: drive %s not attached: %w", v.ID, v.DriveID, drives.ErrNotSupported)
		}
		if err := a.ensureDriveAttached(ctx, v.DriveID); err != nil {
			return false, fmt.Errorf("remove video source %s: attach drive %s: %w", v.ID, v.DriveID, err)
		}
	}
	drv, ok := a.registry.Get(v.DriveID)
	if !ok {
		return false, fmt.Errorf("remove video source %s: drive %s not attached: %w", v.ID, v.DriveID, drives.ErrNotSupported)
	}
	if sourceRemover, ok := drv.(drives.SourceRemover); ok {
		if err := sourceRemover.RemoveSource(ctx, drives.SourceFile{
			FileID:   fileID,
			ParentID: strings.TrimSpace(v.ParentID),
			Name:     strings.TrimSpace(v.FileName),
			Size:     v.Size,
		}); err != nil {
			return false, fmt.Errorf("remove video source %s from drive %s: %w", v.ID, v.DriveID, err)
		}
		return true, nil
	}
	remover, ok := drv.(drives.Remover)
	if !ok {
		return false, fmt.Errorf("remove video source %s: drive %s (%s) does not support source deletion: %w", v.ID, v.DriveID, drv.Kind(), drives.ErrNotSupported)
	}
	if err := remover.Remove(ctx, fileID); err != nil {
		return false, fmt.Errorf("remove video source %s from drive %s: %w", v.ID, v.DriveID, err)
	}
	return true, nil
}

// restoreDeletedVideo coordinates provider inspection, the catalog state
// transition, and application side effects under the same per-video lock used
// by source deletion. Catalog returns the restored row so direct restores enter
// the same generation queues as newly uploaded videos.
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

	result, err := a.cat.RestoreDeletedVideo(ctx, videoID, func(driveID, fileID string) (catalog.DeletedVideoSourceInfo, error) {
		return a.inspectRestorableSource(ctx, driveID, fileID)
	})
	persistence.RUnlock()
	if err != nil {
		return err
	}
	if result.Video != nil {
		// The catalog commit is already durable. A client disconnect at this point
		// must not strand the restored row in pending derived-asset state.
		postRestoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		a.enqueueUploadedVideo(postRestoreCtx, result.Video)
		cancel()
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

	// Stop generation/crawl workers before deleting assets so they do not keep
	// writing files for a drive that is being removed.
	a.detachDrive(driveID)

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

func (a *App) cleanupOrphanDriveVideos(ctx context.Context) (int, error) {
	if a == nil || a.cat == nil {
		return 0, nil
	}
	items, err := a.cat.ListVideosWithMissingDrive(ctx)
	if err != nil {
		return 0, err
	}
	if len(items) == 0 {
		return 0, nil
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
			return 0, fmt.Errorf("remove local assets for orphan %s: %w", v.ID, err)
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
			return removed, fmt.Errorf("delete orphan catalog video %s: %w", v.ID, err)
		}
		removed++
	}
	return removed, nil
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
	candidates := []string{
		v.PreviewLocal,
	}
	candidates = append(candidates, mediaasset.PreviewPathCandidates(localDir, v.ID)...)
	candidates = append(candidates, mediaasset.ThumbnailAssetPathCandidates(localDir, v.ID)...)
	candidates = append(candidates, mediaasset.FrameSignaturePath(localDir, v.ID))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		clean, ok := localPathWithin(localDir, candidate)
		if !ok {
			continue
		}
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		info, err := os.Stat(clean)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(clean); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
