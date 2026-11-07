package main

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/dedupe"
	"github.com/nianzhibai/91/backend/internal/localpath"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
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

	log.Printf("[dedupe-maintenance] videos=%d exact_groups=%d exact_deleted=%d near_candidates=%d near_ssim_comparisons=%d near_groups=%d near_deleted=%d content_candidates=%d content_comparisons=%d content_cross_matched=%d content_groups=%d content_deleted=%d",
		stats.VideosScanned, stats.ExactGroups, stats.ExactDeleted, stats.NearCandidates, stats.NearSSIMComparisons, stats.NearGroups, stats.NearDeleted,
		stats.ContentCandidates, stats.ContentComparisons, stats.ContentCrossMatched, stats.ContentGroups, stats.ContentDeleted)
	return nil
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
	clean, ok := localpath.Managed(localDir, v.PreviewLocal)
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
