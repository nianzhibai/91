package main

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/drives/localupload"
	"github.com/video-site/backend/internal/drives/scriptcrawler"
	"github.com/video-site/backend/internal/fingerprint"
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
	a.mu.Lock()
	worker := a.workers[v.DriveID]
	thumbWorker := a.thumbWorkers[v.DriveID]
	fingerprintWorker := a.fingerprintWorkers[v.DriveID]
	a.mu.Unlock()

	if thumbWorker != nil && v.ThumbnailURL == "" {
		thumbWorker.Enqueue(v)
	}
	if worker != nil && a.teaserEnabledForDrive(ctx, v.DriveID) {
		worker.Enqueue(v)
	}
	if fingerprintWorker != nil {
		fingerprintWorker.Enqueue(v)
	}
}

func (a *App) regenPreview(ctx context.Context, videoID string) {
	v, err := a.cat.GetVideo(ctx, videoID)
	if err != nil {
		return
	}
	taskCtx, done, admitted := a.registerDriveTaskContext(ctx, v.DriveID, driveTaskScopePreview)
	if !admitted {
		return
	}
	defer done()
	a.mu.Lock()
	worker := a.workers[v.DriveID]
	a.mu.Unlock()
	if worker != nil {
		worker.EnqueueBlocking(taskCtx, v)
	}
}

func (a *App) regenAllPreviews(ctx context.Context) {
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
			admission = driveAdmission{ctx: taskCtx, done: done}
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

func (a *App) regenFailedPreviews(ctx context.Context, driveID string) {
	taskCtx, done, admitted := a.registerDriveTaskContext(ctx, driveID, driveTaskScopePreview)
	if !admitted {
		return
	}
	defer done()
	failed, err := a.cat.ListVideosByPreviewStatus(taskCtx, driveID, "failed", 0)
	if err != nil {
		log.Printf("[preview] list failed videos for regen drive=%s: %v", driveID, err)
		return
	}
	a.mu.Lock()
	worker := a.workers[driveID]
	a.mu.Unlock()
	if worker == nil {
		log.Printf("[preview] regen failed drive=%s skipped: worker not found", driveID)
		return
	}
	reset := 0
	for _, v := range failed {
		if err := taskCtx.Err(); err != nil {
			log.Printf("[preview] reset failed canceled drive=%s reset=%d: %v", driveID, reset, err)
			return
		}
		if err := a.cat.UpdatePreview(taskCtx, v.ID, "", "pending"); err != nil {
			log.Printf("[preview] reset failed video %s drive=%s: %v", v.ID, driveID, err)
			continue
		}
		reset++
	}
	items, err := a.cat.ListVideosByPreviewStatus(taskCtx, driveID, "pending", 0)
	if err != nil {
		log.Printf("[preview] list pending videos for regen drive=%s: %v", driveID, err)
		return
	}
	log.Printf("[preview] enqueue pending videos for regen drive=%s count=%d reset_failed=%d", driveID, len(items), reset)
	queued := 0
	for _, v := range items {
		if err := taskCtx.Err(); err != nil {
			log.Printf("[preview] enqueue pending canceled drive=%s queued=%d: %v", driveID, queued, err)
			return
		}
		if !worker.EnqueueBlocking(taskCtx, v) {
			log.Printf("[preview] enqueue pending canceled drive=%s queued=%d", driveID, queued)
			return
		}
		queued++
	}
	log.Printf("[preview] enqueued pending videos for regen drive=%s queued=%d reset_failed=%d", driveID, queued, reset)
}

// regenFailedThumbnails 把某 drive 下 thumbnail_status=failed 的视频全部重置为
// pending 并重新入队封面 worker。与 regenFailedPreviews 行为对称：那条管预览视频，
// 这条管封面图（两个 worker 是独立队列）。
//
// 操作不会触发已生成失败的视频重新去网盘取流 —— 只是把 catalog 的状态翻到 pending
// 并入队；真正的取链 / ffmpeg 在 thumb worker 里执行。
func (a *App) regenFailedThumbnails(ctx context.Context, driveID string) {
	taskCtx, done, admitted := a.registerDriveTaskContext(ctx, driveID, 0)
	if !admitted {
		return
	}
	defer done()
	failed, err := a.cat.ListVideosByThumbnailStatus(taskCtx, driveID, "failed", 0)
	if err != nil {
		log.Printf("[thumb] list failed videos for regen drive=%s: %v", driveID, err)
		return
	}
	a.mu.Lock()
	thumbWorker := a.thumbWorkers[driveID]
	a.mu.Unlock()
	if thumbWorker == nil {
		log.Printf("[thumb] regen failed drive=%s skipped: thumb worker not found", driveID)
		return
	}
	reset := 0
	for _, v := range failed {
		if err := taskCtx.Err(); err != nil {
			log.Printf("[thumb] reset failed canceled drive=%s reset=%d: %v", driveID, reset, err)
			return
		}
		// 状态翻 pending；保留 thumbnail_url 字段（thumb worker 先看 url 是否已写
		// 来判断是否真的要再生）。但既然之前是 failed 说明 url 没写过，所以这里
		// 把 url 一并清空更稳。
		if err := a.cat.UpdateVideoMeta(taskCtx, v.ID, catalog.VideoMetaPatch{
			ThumbnailURL:           "",
			ThumbnailStatus:        "pending",
			ResetThumbnailFailures: true,
		}); err != nil {
			log.Printf("[thumb] reset failed video %s drive=%s: %v", v.ID, driveID, err)
			continue
		}
		reset++
	}
	items, err := a.cat.ListVideosNeedingThumbnail(taskCtx, driveID, 0)
	if err != nil {
		log.Printf("[thumb] list pending thumbnails for regen drive=%s: %v", driveID, err)
		return
	}
	log.Printf("[thumb] enqueue pending thumbnails for regen drive=%s count=%d reset_failed=%d", driveID, len(items), reset)
	queued := 0
	for _, v := range items {
		if err := taskCtx.Err(); err != nil {
			log.Printf("[thumb] enqueue pending canceled drive=%s queued=%d: %v", driveID, queued, err)
			return
		}
		if !thumbWorker.EnqueueBlocking(taskCtx, v) {
			log.Printf("[thumb] enqueue pending canceled drive=%s queued=%d", driveID, queued)
			return
		}
		queued++
	}
	log.Printf("[thumb] enqueued pending thumbnails for regen drive=%s queued=%d reset_failed=%d", driveID, queued, reset)
}

func (a *App) regenFailedFingerprints(ctx context.Context, driveID string) {
	taskCtx, done, admitted := a.registerDriveTaskContext(ctx, driveID, 0)
	if !admitted {
		return
	}
	defer done()
	failed, err := a.cat.ListVideosByFingerprintStatus(taskCtx, driveID, "failed", 0)
	if err != nil {
		log.Printf("[fingerprint] list failed videos for regen drive=%s: %v", driveID, err)
		return
	}
	a.mu.Lock()
	fingerprintWorker := a.fingerprintWorkers[driveID]
	a.mu.Unlock()
	if fingerprintWorker == nil {
		log.Printf("[fingerprint] regen failed drive=%s skipped: fingerprint worker not found", driveID)
		return
	}
	reset := 0
	for _, v := range failed {
		if err := taskCtx.Err(); err != nil {
			log.Printf("[fingerprint] reset failed canceled drive=%s reset=%d: %v", driveID, reset, err)
			return
		}
		if err := a.cat.UpdateVideoFingerprint(taskCtx, v.ID, "", "pending", ""); err != nil {
			log.Printf("[fingerprint] reset failed video %s drive=%s: %v", v.ID, driveID, err)
			continue
		}
		reset++
	}
	items, err := a.cat.ListVideosNeedingFingerprint(taskCtx, driveID, 0)
	if err != nil {
		log.Printf("[fingerprint] list pending videos for regen drive=%s: %v", driveID, err)
		return
	}
	log.Printf("[fingerprint] enqueue pending videos for regen drive=%s count=%d reset_failed=%d", driveID, len(items), reset)
	queued := 0
	for _, v := range items {
		if err := taskCtx.Err(); err != nil {
			log.Printf("[fingerprint] enqueue pending canceled drive=%s queued=%d: %v", driveID, queued, err)
			return
		}
		if !fingerprintWorker.EnqueueBlocking(taskCtx, v) {
			log.Printf("[fingerprint] enqueue pending canceled drive=%s queued=%d", driveID, queued)
			return
		}
		queued++
	}
	log.Printf("[fingerprint] enqueued pending videos for regen drive=%s queued=%d reset_failed=%d", driveID, queued, reset)
}

// listScanTargetIDs 返回 nightly Phase 1 应扫描的所有 drive ID
// （非爬虫、非 localupload）。它直接读 catalog，而不是 registry，这样
// 进程刚启动、云盘还在后台挂载时，nightly 也不会漏掉配置过的 drive。
func (a *App) listScanTargetIDs(ctx context.Context) []string {
	all, err := a.cat.ListDrives(ctx)
	if err != nil {
		log.Printf("[nightly] list scan target drives: %v", err)
		return nil
	}
	out := make([]string, 0, len(all))
	for _, d := range all {
		if d == nil || d.ID == localupload.DriveID || d.Kind == scriptcrawler.Kind {
			continue
		}
		out = append(out, d.ID)
	}
	return out
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
// 顺序：先等所有 thumb worker，再等预览视频，最后等指纹。队列生成时互不等待；
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

	for _, w := range thumbWorkers {
		if err := w.WaitIdle(ctx); err != nil {
			return err
		}
	}
	for _, w := range previewWorkers {
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
	return nil
}

func (a *App) waitDriveGenerationQueuesIdle(ctx context.Context, driveID string) error {
	a.mu.Lock()
	thumbWorker := a.thumbWorkers[driveID]
	previewWorker := a.workers[driveID]
	fingerprintWorker := a.fingerprintWorkers[driveID]
	a.mu.Unlock()
	if err := thumbWorker.WaitIdle(ctx); err != nil {
		return err
	}
	if err := previewWorker.WaitIdle(ctx); err != nil {
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
	if d == nil || d.ID() == localupload.DriveID {
		return false
	}
	// 爬虫类 drive 由专用 crawl 阶段触发，不参与普通 scan
	if d.Kind() == scriptcrawler.Kind {
		return false
	}
	return true
}
