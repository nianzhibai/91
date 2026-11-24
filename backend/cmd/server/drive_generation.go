package main

import (
	"context"
	"fmt"
	"time"

	"github.com/nianzhibai/91/backend/internal/api"
	"github.com/nianzhibai/91/backend/internal/catalog"
)

func generationLabel(kind api.DriveGenerationKind) string {
	switch kind {
	case api.DriveGenerationThumbnails:
		return "封面"
	case api.DriveGenerationPreviews:
		return "预览"
	case api.DriveGenerationFingerprints:
		return "指纹"
	default:
		return string(kind)
	}
}

// generationRetryKinds selects the failed work a manual request retries.
// Previews and signatures come from one media job.
func generationRetryKinds(kind api.DriveGenerationKind) (catalog.GenerationKinds, error) {
	switch kind {
	case api.DriveGenerationThumbnails:
		return catalog.GenerationKinds{Thumbnails: true}, nil
	case api.DriveGenerationPreviews:
		return catalog.GenerationKinds{Previews: true}, nil
	case api.DriveGenerationFingerprints:
		return catalog.GenerationKinds{Fingerprints: true}, nil
	}
	return catalog.GenerationKinds{}, fmt.Errorf("unknown generation kind: %s", kind)
}

func dueOfKind(counts catalog.GenerationCounts, kind api.DriveGenerationKind) int {
	switch kind {
	case api.DriveGenerationThumbnails:
		return counts.Thumbnails
	case api.DriveGenerationPreviews:
		return counts.Media
	case api.DriveGenerationFingerprints:
		return counts.Fingerprints
	}
	return 0
}

// requestDriveGeneration retries the drive's failed work of one kind and
// wakes its runner, which takes everything due in its own order.
func (a *App) requestDriveGeneration(ctx context.Context, driveID string, kind api.DriveGenerationKind) (api.DriveGenerationResult, error) {
	if _, err := a.cat.GetDrive(ctx, driveID); err != nil {
		return api.DriveGenerationResult{}, err
	}
	kinds, err := generationRetryKinds(kind)
	if err != nil {
		return api.DriveGenerationResult{}, err
	}
	label := generationLabel(kind)
	if kind == api.DriveGenerationPreviews && !a.previewEnabled() {
		return api.DriveGenerationResult{}, fmt.Errorf("预览生成已关闭")
	}
	taskCtx, done, admitted := a.registerDriveTaskContext(ctx, driveID, 0)
	if !admitted {
		return api.DriveGenerationResult{State: "busy", Message: "当前网盘配置正在切换或正在删除，请稍后重试"}, nil
	}
	defer done()
	if a.generationRunner(driveID) == nil {
		return api.DriveGenerationResult{}, fmt.Errorf("%s生成服务尚未就绪，请稍后重试", label)
	}
	if _, err := a.resetFailedGeneration(taskCtx, driveID, kinds); err != nil {
		return api.DriveGenerationResult{}, err
	}
	counts, err := a.cat.CountDueGeneration(taskCtx, driveID, a.previewEnabled(), time.Now())
	if err != nil {
		return api.DriveGenerationResult{}, err
	}
	if dueOfKind(counts, kind) > 0 {
		a.wakeDriveGeneration(driveID)
		return api.DriveGenerationResult{State: "started", Message: "已触发" + label + "生成"}, nil
	}
	stats, err := a.cat.CountDriveAssetStatsForDrive(taskCtx, driveID)
	if err != nil {
		return api.DriveGenerationResult{}, err
	}
	remaining := 0
	switch kind {
	case api.DriveGenerationThumbnails:
		remaining = stats.Thumbnails[driveID].Pending + stats.Thumbnails[driveID].Failed
	case api.DriveGenerationPreviews:
		remaining = stats.Teasers[driveID].Pending + stats.Teasers[driveID].Failed
	case api.DriveGenerationFingerprints:
		remaining = stats.Fingerprints[driveID].Pending + stats.Fingerprints[driveID].Failed
	}
	if remaining > 0 {
		return api.DriveGenerationResult{}, fmt.Errorf("%s资源尚未就绪，但当前没有可生成的项目", label)
	}
	// Durations complete after the other steps; a drive with nothing else
	// due may still have some to probe.
	if counts.Any() {
		a.wakeDriveGeneration(driveID)
	}
	return api.DriveGenerationResult{State: "ready", Message: label + "已全部就绪"}, nil
}
