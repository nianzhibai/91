package main

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/video-site/backend/internal/api"
	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/drives/localupload"
	"github.com/video-site/backend/internal/drives/scriptcrawler"
	"github.com/video-site/backend/internal/fingerprint"
	"github.com/video-site/backend/internal/persistence"
	"github.com/video-site/backend/internal/preview"
)

func (a *App) enqueueUploadedVideo(ctx context.Context, v *catalog.Video) {
	if v == nil {
		return
	}
	release, _, admitted := a.driveOperationGate(v.DriveID).beginTask(ctx, driveTaskScopePreview)
	if !admitted {
		return
	}
	defer release()
	finishEnqueue := a.beginDriveResourceEnqueue(v.DriveID)
	defer finishEnqueue()
	a.mu.Lock()
	worker := a.workers[v.DriveID]
	thumbWorker := a.thumbWorkers[v.DriveID]
	fingerprintWorker := a.fingerprintWorkers[v.DriveID]
	a.mu.Unlock()

	if thumbWorker != nil && v.ThumbnailURL == "" {
		thumbWorker.Enqueue(v)
	}
	if worker != nil && a.previewEnabled() && v.PreviewStatus != "ready" {
		worker.Enqueue(v)
	}
	if fingerprintWorker != nil && v.FingerprintStatus != "ready" {
		fingerprintWorker.Enqueue(v)
	}
}

func (a *App) regenPreview(ctx context.Context, videoID string) {
	if !a.previewEnabled() {
		return
	}
	v, err := a.cat.GetVideo(ctx, videoID)
	if err != nil {
		return
	}
	taskCtx, done, admitted := a.registerDriveTaskContext(ctx, v.DriveID, driveTaskScopePreview)
	if !admitted {
		return
	}
	defer done()
	finishEnqueue := a.beginDriveResourceEnqueue(v.DriveID)
	defer finishEnqueue()
	a.mu.Lock()
	worker := a.workers[v.DriveID]
	a.mu.Unlock()
	if worker != nil {
		worker.EnqueueBlocking(taskCtx, v)
	}
}

func (a *App) regenAllPreviews(ctx context.Context) {
	if !a.previewEnabled() {
		return
	}
	items, total, err := a.cat.ListVideos(ctx, catalog.ListParams{Page: 1, PageSize: 1000000})
	if err != nil {
		log.Printf("[preview] list all videos for regen: %v", err)
		return
	}
	log.Printf("[preview] enqueue all visible videos for regen count=%d total=%d", len(items), total)
	queued := 0
	type driveAdmission struct {
		ctx  context.Context
		done func()
	}
	admissions := make(map[string]driveAdmission)
	rejected := make(map[string]bool)
	defer func() {
		for _, admission := range admissions {
			admission.done()
		}
	}()
	for _, v := range items {
		if err := ctx.Err(); err != nil {
			log.Printf("[preview] enqueue all canceled after %d videos: %v", queued, err)
			return
		}
		if rejected[v.DriveID] {
			continue
		}
		admission, ok := admissions[v.DriveID]
		if !ok {
			taskCtx, done, taskAdmitted := a.registerDriveTaskContext(ctx, v.DriveID, driveTaskScopePreview)
			if !taskAdmitted {
				rejected[v.DriveID] = true
				continue
			}
			finishEnqueue := a.beginDriveResourceEnqueue(v.DriveID)
			admission = driveAdmission{ctx: taskCtx, done: func() { finishEnqueue(); done() }}
			admissions[v.DriveID] = admission
		}
		a.mu.Lock()
		worker := a.workers[v.DriveID]
		a.mu.Unlock()
		if worker == nil {
			continue
		}
		if !worker.EnqueueBlocking(admission.ctx, v) {
			log.Printf("[preview] enqueue all canceled for drive=%s after %d videos", v.DriveID, queued)
			admission.done()
			delete(admissions, v.DriveID)
			rejected[v.DriveID] = true
			continue
		}
		queued++
	}
	log.Printf("[preview] enqueued all visible videos for regen queued=%d", queued)
}

func (a *App) resetFailedGeneration(ctx context.Context, driveID string, kinds catalog.GenerationKinds) (catalog.GenerationRetryCounts, error) {
	if err := persistence.RLockContext(ctx); err != nil {
		return catalog.GenerationRetryCounts{}, err
	}
	defer persistence.RUnlock()
	counts, err := a.cat.ResetFailedGeneration(ctx, driveID, kinds)
	if err == nil && (counts.Thumbnails > 0 || counts.Previews > 0 || counts.Fingerprints > 0) {
		log.Printf("[generation-retry] drive=%s reset_failed thumbnails=%d previews=%d fingerprints=%d", driveID, counts.Thumbnails, counts.Previews, counts.Fingerprints)
	}
	return counts, err
}

func (a *App) regenFailedPreviews(ctx context.Context, driveID string) {
	if !a.previewEnabled() {
		return
	}
	a.regenerateDriveResources(ctx, driveID, api.DriveGenerationPreviews)
}

// regenFailedThumbnails retries missing failed covers. Duration-only work belongs
// after this drive's resource generation and never changes existing thumbnail state.
func (a *App) regenFailedThumbnails(ctx context.Context, driveID string) {
	a.regenerateDriveResources(ctx, driveID, api.DriveGenerationThumbnails)
}

// listScanTargetIDs 返回 nightly Phase 1 应扫描的所有 drive ID
// （非爬虫、非 localupload）。它直接读 catalog，而不是 registry，这样
// 进程刚启动、云盘还在后台挂载时，nightly 也不会漏掉配置过的 drive。
func (a *App) listScanTargetIDs(ctx context.Context) ([]string, error) {
	all, err := a.cat.ListDrives(ctx)
	if err != nil {
		log.Printf("[nightly] list scan target drives: %v", err)
		return nil, err
	}
	out := make([]string, 0, len(all))
	for _, d := range all {
		if d == nil || d.ID == localupload.DriveID || d.Kind == scriptcrawler.Kind {
			continue
		}
		out = append(out, d.ID)
	}
	return out, nil
}

// listCrawlerDriveIDs 返回 nightly Phase 2 应触发爬取的爬虫 drive ID 列表。
func (a *App) listCrawlerDriveIDs(ctx context.Context) []string {
	all, err := a.cat.ListDrives(ctx)
	if err != nil {
		log.Printf("[nightly] list crawler drives: %v", err)
		return nil
	}
	out := make([]string, 0, len(all))
	for _, d := range all {
		if d == nil || d.Kind != scriptcrawler.Kind || !scriptcrawler.IsConfigured(d.Credentials) {
			continue
		}
		if parseBoolDefault(strings.TrimSpace(d.Credentials["paused"]), false) {
			continue
		}
		out = append(out, d.ID)
	}
	return out
}

// waitAllPreviewQueuesIdle 阻塞直到所有 drive 的封面、预览视频和指纹 worker
// 队列都为空且无 in-flight 任务。
//
// 顺序：先等预览视频，再等封面（预览完成会追加封面任务），最后等指纹。队列生成时互不等待；
// nightly 只在 phase 边界统一等待它们都 drain，保证爬虫视频迁移前本地资产已产出。
// 若 ctx 在等待中被取消（shutdown / 管理员停止），立即返回 ctx.Err。
func (a *App) waitAllPreviewQueuesIdle(ctx context.Context) error {
	a.mu.Lock()
	thumbWorkers := make([]*preview.ThumbWorker, 0, len(a.thumbWorkers))
	previewWorkers := make([]*preview.Worker, 0, len(a.workers))
	fingerprintWorkers := make([]*fingerprint.Worker, 0, len(a.fingerprintWorkers))
	for _, w := range a.thumbWorkers {
		thumbWorkers = append(thumbWorkers, w)
	}
	for _, w := range a.workers {
		previewWorkers = append(previewWorkers, w)
	}
	for _, w := range a.fingerprintWorkers {
		fingerprintWorkers = append(fingerprintWorkers, w)
	}
	a.mu.Unlock()

	for _, w := range previewWorkers {
		if err := w.WaitIdle(ctx); err != nil {
			return err
		}
	}
	for _, w := range thumbWorkers {
		if err := w.WaitIdle(ctx); err != nil {
			return err
		}
	}
	if err := a.waitFingerprintQueueingIdle(ctx, ""); err != nil {
		return err
	}
	for _, w := range fingerprintWorkers {
		if err := w.WaitIdle(ctx); err != nil {
			return err
		}
	}
	return a.waitDurationBackfillsIdle(ctx, "")
}

func (a *App) waitDriveGenerationQueuesIdle(ctx context.Context, driveID string) error {
	a.mu.Lock()
	thumbWorker := a.thumbWorkers[driveID]
	previewWorker := a.workers[driveID]
	fingerprintWorker := a.fingerprintWorkers[driveID]
	a.mu.Unlock()
	if err := previewWorker.WaitIdle(ctx); err != nil {
		return err
	}
	if err := thumbWorker.WaitIdle(ctx); err != nil {
		return err
	}
	if err := a.waitFingerprintQueueingIdle(ctx, driveID); err != nil {
		return err
	}
	if err := fingerprintWorker.WaitIdle(ctx); err != nil {
		return err
	}
	return nil
}

func (a *App) waitFingerprintQueueingIdle(ctx context.Context, driveID string) error {
	if !a.fingerprintQueueingBusy(driveID) {
		return nil
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if !a.fingerprintQueueingBusy(driveID) {
				return nil
			}
		}
	}
}

func (a *App) fingerprintQueueingBusy(driveID string) bool {
	a.fingerprintQueueMu.Lock()
	defer a.fingerprintQueueMu.Unlock()
	if driveID != "" {
		return a.fingerprintQueueing[driveID]
	}
	return len(a.fingerprintQueueing) > 0
}

func shouldScanDrive(d drives.Drive) bool {
	if d == nil || (d.ID() == localupload.DriveID || d.ID() == catalog.TelegramLocalDriveID) {
		return false
	}
	// 爬虫类 drive 由专用 crawl 阶段触发，不参与普通 scan
	if d.Kind() == scriptcrawler.Kind {
		return false
	}
	return true
}
