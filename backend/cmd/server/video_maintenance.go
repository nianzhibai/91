package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/dedupe"
	"github.com/video-site/backend/internal/drives/localupload"
	"github.com/video-site/backend/internal/localpath"
	"github.com/video-site/backend/internal/mediaasset"
	"github.com/video-site/backend/internal/videoname"
)

type duplicateVideoMaintenanceStats struct {
	VideosScanned       int
	ExactGroups         int
	ExactDeleted        int
	NearCandidates      int
	NearSSIMComparisons int
	NearGroups          int
	NearDeleted         int
	ContentCandidates   int
	ContentComparisons  int
	ContentCrossMatched int
	ContentGroups       int
	ContentDeleted      int
}

func (a *App) cleanupDuplicateVideoAssets(ctx context.Context) error {
	if a == nil || a.cat == nil {
		return nil
	}
	a.maintainLocalUploadFileNames(ctx)
	localDir := ""
	if a.cfg != nil {
		localDir = strings.TrimSpace(a.cfg.Storage.LocalPreviewDir)
	}
	if err := a.cleanupPendingDuplicateAssets(ctx, localDir); err != nil {
		log.Printf("[dedupe-maintenance] retry pending asset cleanup: %v", err)
	}
	const maxPlanAttempts = 3
	var (
		plan  dedupe.Plan
		stats duplicateVideoMaintenanceStats
	)
	for attempt := 1; attempt <= maxPlanAttempts; attempt++ {
		videos, err := a.cat.ListVideoMaintenanceCandidates(ctx)
		if err != nil {
			return err
		}
		stats = duplicateVideoMaintenanceStats{VideosScanned: len(videos)}
		if len(videos) == 0 {
			if err := a.retireLegacyDuplicateReviewTable(ctx); err != nil {
				return err
			}
			log.Printf("[dedupe-maintenance] no videos to maintain")
			return nil
		}

		plan, err = a.buildDuplicateMaintenancePlan(ctx, localDir, videos, dedupe.AllChannels)
		if err != nil {
			return err
		}
		if err = a.applyDuplicateMaintenancePlan(ctx, localDir, plan); err == nil {
			break
		}
		if !errors.Is(err, catalog.ErrDuplicatePlanStale) || attempt == maxPlanAttempts {
			return err
		}
		log.Printf("[dedupe-maintenance] plan changed before commit; rebuilding attempt=%d/%d: %v", attempt+1, maxPlanAttempts, err)
	}
	stats.ExactGroups = plan.Stats.Exact.Groups
	stats.ExactDeleted = plan.Stats.Exact.Deleted
	stats.NearCandidates = plan.Stats.Near.Candidates
	stats.NearSSIMComparisons = plan.Stats.Near.Comparisons
	stats.NearGroups = plan.Stats.Near.Groups
	stats.NearDeleted = plan.Stats.Near.Deleted
	stats.ContentCandidates = plan.Stats.Content.Candidates
	stats.ContentComparisons = plan.Stats.Content.Comparisons
	stats.ContentCrossMatched = plan.Stats.Content.CrossMatched
	stats.ContentGroups = plan.Stats.Content.Groups
	stats.ContentDeleted = plan.Stats.Content.Deleted
	if err := a.retireLegacyDuplicateReviewTable(ctx); err != nil {
		return err
	}

	log.Printf("[dedupe-maintenance] videos=%d exact_groups=%d exact_deleted=%d near_candidates=%d near_ssim_comparisons=%d near_groups=%d near_deleted=%d content_candidates=%d content_comparisons=%d content_cross_matched=%d content_groups=%d content_deleted=%d",
		stats.VideosScanned, stats.ExactGroups, stats.ExactDeleted, stats.NearCandidates, stats.NearSSIMComparisons, stats.NearGroups, stats.NearDeleted,
		stats.ContentCandidates, stats.ContentComparisons, stats.ContentCrossMatched, stats.ContentGroups, stats.ContentDeleted)
	return nil
}

func (a *App) retireLegacyDuplicateReviewTable(ctx context.Context) error {
	dropped, err := a.cat.DropLegacyDuplicateReviewTable(ctx)
	if err != nil {
		return fmt.Errorf("retire legacy duplicate review queue: %w", err)
	}
	if dropped {
		log.Printf("[dedupe-maintenance] retired legacy duplicate review queue after automatic content dedupe")
	}
	return nil
}

// maintainLocalUploadFileNames migrates legacy local uploads from random
// physical names to the user-visible title. It is intentionally best-effort:
// one invalid or missing legacy file must not block the nightly maintenance
// pipeline for every other video.
func (a *App) maintainLocalUploadFileNames(ctx context.Context) {
	if a == nil || a.cat == nil {
		return
	}
	root := a.localUploadDir()
	videos, err := a.cat.ListVideosByDrive(ctx, localupload.DriveID)
	if err != nil {
		log.Printf("[local-upload-maintenance] list videos: %v", err)
		return
	}
	renamed, skipped := 0, 0
	for _, v := range videos {
		if err := ctx.Err(); err != nil {
			return
		}
		if v == nil || filepath.Base(v.FileID) != v.FileID {
			skipped++
			continue
		}
		title := strings.TrimSpace(v.Title)
		if title == "" {
			title = videoname.TitleFromFileName(v.FileName)
		}
		ext := filepath.Ext(v.FileID)
		if ext == "" && strings.TrimSpace(v.Ext) != "" {
			ext = "." + strings.TrimPrefix(strings.TrimSpace(v.Ext), ".")
		}
		if err := videoname.ValidateUploadTitle(title, ext); err != nil {
			log.Printf("[local-upload-maintenance] skip id=%s invalid title=%q: %v", v.ID, title, err)
			skipped++
			continue
		}

		newName := videoname.UploadFileName(title, ext, v.FileID, false)
		oldPath := filepath.Join(root, v.FileID)
		newPath := filepath.Join(root, newName)
		if newName != v.FileID {
			if _, err := os.Stat(oldPath); err != nil {
				log.Printf("[local-upload-maintenance] skip id=%s source=%q: %v", v.ID, v.FileID, err)
				skipped++
				continue
			}
			if _, err := os.Stat(newPath); err == nil {
				uploadID := strings.TrimSuffix(v.FileID, filepath.Ext(v.FileID))
				newName = videoname.UploadFileName(title, ext, uploadID, true)
				newPath = filepath.Join(root, newName)
				if _, err := os.Stat(newPath); err == nil || !os.IsNotExist(err) {
					log.Printf("[local-upload-maintenance] skip id=%s collision=%q", v.ID, newName)
					skipped++
					continue
				}
			} else if !os.IsNotExist(err) {
				log.Printf("[local-upload-maintenance] skip id=%s destination=%q: %v", v.ID, newName, err)
				skipped++
				continue
			}
			if err := os.Rename(oldPath, newPath); err != nil {
				log.Printf("[local-upload-maintenance] rename id=%s %q -> %q: %v", v.ID, v.FileID, newName, err)
				skipped++
				continue
			}
		}

		finalTitle := videoname.TitleFromFileName(newName)
		if err := a.cat.UpdateVideoFileIdentity(ctx, v.ID, newName, newName, finalTitle); err != nil {
			if newName != v.FileID {
				if rollbackErr := os.Rename(newPath, oldPath); rollbackErr != nil {
					log.Printf("[local-upload-maintenance] rollback id=%s: %v", v.ID, rollbackErr)
				}
			}
			log.Printf("[local-upload-maintenance] catalog id=%s: %v", v.ID, err)
			skipped++
			continue
		}
		if newName != v.FileID || v.FileName != newName || v.Title != finalTitle {
			renamed++
		}
	}
	log.Printf("[local-upload-maintenance] videos=%d updated=%d skipped=%d", len(videos), renamed, skipped)
}

func videoAssetCompletenessScore(localDir string, v *catalog.Video) int {
	if v == nil {
		return 0
	}
	score := 0
	if localGeneratedPreviewReady(localDir, v) {
		score++
	}
	if _, ok := localGeneratedThumbnailPath(localDir, v); ok {
		score++
	}
	if strings.TrimSpace(v.SampledSHA256) != "" && strings.TrimSpace(v.FingerprintStatus) == "ready" {
		score++
	}
	return score
}

func localGeneratedPreviewReady(localDir string, v *catalog.Video) bool {
	if v == nil || strings.TrimSpace(v.PreviewStatus) != "ready" || strings.TrimSpace(v.PreviewLocal) == "" {
		return false
	}
	localDir = strings.TrimSpace(localDir)
	if localDir == "" {
		return true
	}
	_, ok := localGeneratedPreviewPath(localDir, v)
	return ok
}

// localGeneratedPreviewPath 返回本地 teaser 的实际路径；仅当预览就绪、
// 路径落在 localDir 内且文件存在时才可用。
func localGeneratedPreviewPath(localDir string, v *catalog.Video) (string, bool) {
	if v == nil || strings.TrimSpace(v.PreviewStatus) != "ready" || strings.TrimSpace(v.PreviewLocal) == "" {
		return "", false
	}
	if strings.TrimSpace(localDir) == "" {
		return "", false
	}
	clean, ok := localPathWithin(localDir, v.PreviewLocal)
	if !ok {
		return "", false
	}
	if !regularFileExists(clean) {
		return "", false
	}
	return clean, true
}

func localGeneratedThumbnailPath(localDir string, v *catalog.Video) (string, bool) {
	if v == nil || strings.TrimSpace(localDir) == "" || strings.TrimSpace(v.ID) == "" {
		return "", false
	}
	if strings.TrimSpace(v.ThumbnailURL) != "/p/thumb/"+v.ID {
		return "", false
	}
	for _, candidate := range mediaasset.ThumbnailPathCandidates(localDir, v.ID) {
		clean, ok := localPathWithin(localDir, candidate)
		if !ok {
			continue
		}
		if regularFileExists(clean) {
			return clean, true
		}
	}
	return "", false
}

func regularFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func isSQLiteBusyError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "sqlite_locked") ||
		strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked")
}

func localPathWithin(root, path string) (string, bool) {
	return localpath.Within(root, path)
}
