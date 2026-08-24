package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives/scriptcrawler"
)

func (a *App) scheduleScriptCrawlerCrawl(ctx context.Context, driveID string) bool {
	if a.driveHasActiveWork(driveID) {
		log.Printf("[scriptcrawler] drive=%s has active work, skip duplicate crawl request", driveID)
		return false
	}
	taskCtx, done, admitted := a.registerDriveTaskContext(ctx, driveID, driveTaskScopeScan)
	if !admitted {
		log.Printf("[scriptcrawler] drive=%s configuration update in progress, reject crawl", driveID)
		return false
	}
	if !a.beginDriveScanOrCrawl(driveID) {
		done()
		log.Printf("[scriptcrawler] drive=%s already queued or running, skip duplicate crawl request", driveID)
		return false
	}

	go func() {
		defer func() {
			a.endDriveScanOrCrawl(driveID)
			done()
		}()
		if a.runScriptCrawlerCrawlWithTaskContext(taskCtx, driveID) {
			a.runCrawlerMigrationAfterManualCrawl(taskCtx, driveID)
		}
	}()
	return true
}

func (a *App) runScriptCrawlerCrawl(ctx context.Context, driveID string) {
	taskCtx, done, admitted := a.registerDriveTaskContext(ctx, driveID, driveTaskScopeScan)
	if !admitted {
		log.Printf("[scriptcrawler] drive=%s configuration update in progress, reject direct crawl", driveID)
		return
	}
	defer done()
	if !a.beginDriveScanOrCrawl(driveID) {
		log.Printf("[scriptcrawler] drive=%s already queued or running, skip direct crawl", driveID)
		return
	}
	defer a.endDriveScanOrCrawl(driveID)
	a.runScriptCrawlerCrawlWithTaskContext(taskCtx, driveID)
}

func (a *App) runScriptCrawlerCrawlWithTaskContext(ctx context.Context, driveID string) bool {
	if err := ctx.Err(); err != nil {
		log.Printf("[scriptcrawler] drive=%s crawl canceled before start: %v", driveID, err)
		return false
	}
	a.mu.Lock()
	c := a.scriptCrawlers[driveID]
	a.mu.Unlock()
	if c == nil {
		if err := a.ensureDriveAttached(ctx, driveID); err != nil {
			log.Printf("[scriptcrawler] drive=%s attach failed: %v", driveID, err)
			return false
		}
		a.mu.Lock()
		c = a.scriptCrawlers[driveID]
		a.mu.Unlock()
		if c == nil {
			log.Printf("[scriptcrawler] drive=%s crawler not attached", driveID)
			return false
		}
	}

	d, err := a.activeDriveConfig(ctx, driveID)
	if err != nil || d == nil {
		log.Printf("[scriptcrawler] drive=%s active configuration lookup failed: %v", driveID, err)
		return false
	}
	targetNew := crawlerIntCred(d, "target_new", scriptcrawler.DefaultTargetNew)
	if targetNew <= 0 {
		targetNew = scriptcrawler.DefaultTargetNew
	}

	log.Printf("[scriptcrawler] drive=%s start crawl target_new=%d", driveID, targetNew)
	res, runErr := c.RunOnce(ctx, targetNew)
	if runErr != nil {
		log.Printf("[scriptcrawler] drive=%s crawl failed: %v", driveID, runErr)
	} else if res != nil {
		log.Printf("[scriptcrawler] drive=%s crawl done target=%d candidate_budget=%d total=%d new=%d skipped=%d failed=%d seen_snapshot=%d",
			driveID, res.TargetNew, res.CandidateBudget, res.TotalEntries, res.NewVideos, res.Skipped, res.Failed, res.SeenSnapshot)
	}

	if err := a.updateScriptCrawlerRunState(ctx, driveID, runErr); err != nil {
		log.Printf("[scriptcrawler] drive=%s update last_crawl_at: %v", driveID, err)
	}
	if err := ctx.Err(); err != nil {
		log.Printf("[scriptcrawler] drive=%s crawl canceled after run: %v", driveID, err)
		return false
	}

	a.mu.Lock()
	worker := a.workers[driveID]
	thumbWorker := a.thumbWorkers[driveID]
	fingerprintWorker := a.fingerprintWorkers[driveID]
	a.mu.Unlock()
	a.scheduleFingerprintBackfill(ctx, driveID, fingerprintWorker)
	a.enqueueDriveGeneration(ctx, driveID, worker, thumbWorker)
	return runErr == nil
}

func (a *App) updateScriptCrawlerRunState(ctx context.Context, driveID string, runErr error) error {
	status, lastError := "ok", ""
	if runErr != nil {
		status, lastError = "error", runErr.Error()
	}
	return a.cat.UpdateDriveRuntimeState(ctx, driveID, scriptcrawler.Kind, status, lastError, map[string]string{
		"last_crawl_at": strconv.FormatInt(time.Now().Unix(), 10),
	})
}

func (a *App) runCrawlerUploadMigration(ctx context.Context) error {
	if a == nil || a.cat == nil || a.crawlerUploader == nil {
		return nil
	}
	drives, err := a.cat.ListDrives(ctx)
	if err != nil {
		return err
	}
	sourceIDs := make([]string, 0)
	for _, drive := range drives {
		if drive != nil && drive.Kind == scriptcrawler.Kind {
			sourceIDs = append(sourceIDs, drive.ID)
		}
	}
	sort.Strings(sourceIDs)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type registeredTask struct {
		ctx  context.Context
		done func()
	}
	registered := make([]registeredTask, 0, len(sourceIDs))
	registeredIDs := make(map[string]struct{}, len(sourceIDs))
	register := func(id string) bool {
		id = strings.TrimSpace(id)
		if id == "" {
			return true
		}
		if _, exists := registeredIDs[id]; exists {
			return true
		}
		taskCtx, done := a.registerDriveTaskContextWaiting(runCtx, id, 0)
		if err := taskCtx.Err(); err != nil {
			done()
			return false
		}
		registeredIDs[id] = struct{}{}
		registered = append(registered, registeredTask{ctx: taskCtx, done: done})
		return true
	}
	defer func() {
		for i := len(registered) - 1; i >= 0; i-- {
			registered[i].done()
		}
	}()

	// Lock sources first. Once admitted, their active configuration snapshots
	// cannot change, so the target set derived below is exactly the set used by
	// the migrator rather than a stale pre-admission catalog snapshot.
	for _, sourceID := range sourceIDs {
		if !register(sourceID) {
			return runCtx.Err()
		}
	}
	effectiveSources := make([]string, 0, len(sourceIDs))
	targetIDs := make([]string, 0, len(sourceIDs))
	for _, sourceID := range sourceIDs {
		drive, getErr := a.activeDriveConfig(runCtx, sourceID)
		if getErr != nil || drive == nil || drive.Kind != scriptcrawler.Kind {
			continue
		}
		effectiveSources = append(effectiveSources, sourceID)
		if targetID := strings.TrimSpace(drive.Credentials["upload_drive_id"]); targetID != "" {
			targetIDs = append(targetIDs, targetID)
		}
	}
	sort.Strings(targetIDs)
	for _, targetID := range targetIDs {
		if !register(targetID) {
			return runCtx.Err()
		}
	}
	for _, task := range registered {
		go func(taskCtx context.Context) {
			select {
			case <-taskCtx.Done():
				cancel()
			case <-runCtx.Done():
			}
		}(task.ctx)
	}
	return a.crawlerUploader.RunDrives(runCtx, effectiveSources)
}

func (a *App) scheduleManualCrawlerUploadMigration(ctx context.Context, driveID string) (bool, string) {
	driveID = strings.TrimSpace(driveID)
	if driveID == "" || a == nil || a.cat == nil {
		return false, "爬虫不存在"
	}
	if a.crawlerUploader == nil {
		return false, "上传迁移器未初始化"
	}
	if a.driveHasActiveWork(driveID) {
		return false, "当前爬虫有正在进行的任务，请稍后重试"
	}

	// Admit the source before reading upload_drive_id or capturing a Driver.
	// This makes the source snapshot and the target lock describe one generation.
	taskCtx, done, admitted := a.registerDriveTaskContext(ctx, driveID, 0)
	if !admitted {
		return false, "当前爬虫有配置等待生效，请稍后重试"
	}
	sourceOwned := true
	defer func() {
		if sourceOwned {
			done()
		}
	}()
	d, err := a.activeDriveConfig(taskCtx, driveID)
	if err != nil || d == nil || d.Kind != scriptcrawler.Kind {
		return false, "爬虫不存在"
	}
	targetDriveID := strings.TrimSpace(d.Credentials["upload_drive_id"])
	if targetDriveID == "" {
		return false, "请先配置上传网盘"
	}
	targetCtx, targetDone, targetAdmitted := a.registerDriveTaskContext(taskCtx, targetDriveID, 0)
	if !targetAdmitted {
		return false, "上传目标网盘有配置等待生效，请稍后重试"
	}
	targetOwned := true
	defer func() {
		if targetOwned {
			targetDone()
		}
	}()

	assets, err := a.cat.CountCrawlerAssets(taskCtx, driveID, crawlerCatalogVideoIDPrefixes(d))
	if err != nil {
		log.Printf("[scriptcrawler] drive=%s manual upload count assets: %v", driveID, err)
		return false, "读取待上传视频失败"
	}
	if reason := crawlerUploadAssetBlockReason(d, assets); reason != "" {
		return false, reason
	}
	if err := a.ensureDriveAttached(taskCtx, driveID); err != nil {
		log.Printf("[scriptcrawler] drive=%s manual upload source attach: %v", driveID, err)
		return false, "爬虫本地存储不可用"
	}
	if err := a.ensureDriveAttached(targetCtx, targetDriveID); err != nil {
		log.Printf("[scriptcrawler] drive=%s manual upload target=%s attach: %v", driveID, targetDriveID, err)
		return false, "上传网盘不可用：" + err.Error()
	}

	a.crawlerUploadMu.Lock()
	if a.crawlerUploadRunning == nil {
		a.crawlerUploadRunning = make(map[string]bool)
	}
	if a.crawlerUploadRunning[driveID] {
		a.crawlerUploadMu.Unlock()
		return false, "当前爬虫已有上传任务正在运行"
	}
	a.crawlerUploadRunning[driveID] = true
	a.crawlerUploadMu.Unlock()

	runCtx, runCancel := context.WithCancel(taskCtx)
	go func() {
		select {
		case <-targetCtx.Done():
			runCancel()
		case <-runCtx.Done():
		}
	}()
	runDone, accepted := a.crawlerUploader.StartDrive(runCtx, driveID)
	if !accepted {
		runCancel()
		a.crawlerUploadMu.Lock()
		delete(a.crawlerUploadRunning, driveID)
		a.crawlerUploadMu.Unlock()
		return false, "已有其他爬虫上传任务正在运行，请稍后重试"
	}
	sourceOwned = false
	targetOwned = false
	log.Printf("[scriptcrawler] drive=%s running manual upload migration target=%s", driveID, targetDriveID)
	go func() {
		defer func() {
			runCancel()
			targetDone()
			done()
			a.crawlerUploadMu.Lock()
			delete(a.crawlerUploadRunning, driveID)
			a.crawlerUploadMu.Unlock()
		}()
		if err := <-runDone; err != nil {
			log.Printf("[scriptcrawler] drive=%s manual upload migration: %v", driveID, err)
		}
	}()
	return true, ""
}

func crawlerUploadAssetBlockReason(d *catalog.Drive, assets catalog.CrawlerAssetCounts) string {
	if assets.Local <= 0 {
		return "没有待上传的本地视频"
	}
	if assets.Fingerprint.Pending > 0 {
		return "还有待生成的视频指纹"
	}
	if assets.Fingerprint.Failed > 0 {
		return "存在指纹生成失败的视频，请先重试或处理失败项"
	}
	if d != nil && d.TeaserEnabled {
		if assets.Teaser.Pending > 0 {
			return "还有待生成的预览视频"
		}
		if assets.Teaser.Failed > 0 {
			return "存在预览视频生成失败的视频，请先重试或处理失败项"
		}
	}
	return ""
}

func crawlerCatalogVideoIDPrefixes(d *catalog.Drive) []string {
	if d == nil {
		return nil
	}
	return []string{
		scriptcrawler.Kind + "-" + d.ID + "-",
	}
}

func (a *App) runCrawlerMigrationAfterManualCrawl(ctx context.Context, driveID string) {
	if err := ctx.Err(); err != nil {
		log.Printf("[scriptcrawler] drive=%s skip post-crawl migration: %v", driveID, err)
		return
	}
	d, err := a.activeDriveConfig(ctx, driveID)
	if err != nil || d == nil {
		log.Printf("[scriptcrawler] drive=%s skip post-crawl migration active configuration lookup: %v", driveID, err)
		return
	}
	targetDriveID := strings.TrimSpace(d.Credentials["upload_drive_id"])
	log.Printf("[scriptcrawler] drive=%s waiting for generation queues before post-crawl completion", driveID)
	if err := a.waitDriveGenerationQueuesIdle(ctx, driveID); err != nil {
		log.Printf("[scriptcrawler] drive=%s post-crawl migration wait canceled: %v", driveID, err)
		return
	}
	if err := ctx.Err(); err != nil {
		log.Printf("[scriptcrawler] drive=%s skip post-crawl migration after wait: %v", driveID, err)
		return
	}
	if targetDriveID != "" {
		if a.crawlerUploader == nil {
			log.Printf("[scriptcrawler] drive=%s skip post-crawl migration: migrator not configured", driveID)
		} else {
			targetCtx, targetDone, admitted := a.registerDriveTaskContext(ctx, targetDriveID, 0)
			if !admitted {
				log.Printf("[scriptcrawler] drive=%s skip post-crawl migration: target=%s configuration update in progress", driveID, targetDriveID)
			} else {
				func() {
					defer targetDone()
					runCtx, cancel := context.WithCancel(ctx)
					defer cancel()
					go func() {
						select {
						case <-targetCtx.Done():
							cancel()
						case <-runCtx.Done():
						}
					}()
					log.Printf("[scriptcrawler] drive=%s running post-crawl migration target=%s", driveID, targetDriveID)
					runDone, accepted := a.crawlerUploader.StartDrive(runCtx, driveID)
					if !accepted {
						log.Printf("[scriptcrawler] drive=%s skip post-crawl migration: another crawler upload is running", driveID)
					} else if err := <-runDone; err != nil {
						log.Printf("[scriptcrawler] drive=%s post-crawl migration: %v", driveID, err)
					}
				}()
			}
		}
	}
	if err := a.restoreScriptCrawlerVideos(ctx, driveID); err != nil {
		log.Printf("[scriptcrawler] drive=%s post-crawl restore: %v", driveID, err)
	}
}

func (a *App) restoreScriptCrawlerVideos(ctx context.Context, driveID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	taskCtx, done, admitted := a.registerDriveTaskContext(ctx, driveID, 0)
	if !admitted {
		return fmt.Errorf("restore crawler drive %s: configuration update in progress", driveID)
	}
	defer done()
	ctx = taskCtx
	requests, err := a.cat.ListCrawlerRestoreRequests(ctx, driveID)
	if err != nil || len(requests) == 0 {
		return err
	}
	if err := a.ensureDriveAttached(ctx, driveID); err != nil {
		return err
	}
	a.mu.Lock()
	crawler := a.scriptCrawlers[driveID]
	a.mu.Unlock()
	if crawler == nil {
		return nil
	}
	restored, err := crawler.RestoreRequestedVideos(ctx)
	if restored > 0 {
		a.mu.Lock()
		worker := a.workers[driveID]
		thumbWorker := a.thumbWorkers[driveID]
		fingerprintWorker := a.fingerprintWorkers[driveID]
		a.mu.Unlock()
		a.scheduleFingerprintBackfill(ctx, driveID, fingerprintWorker)
		a.enqueueDriveGeneration(ctx, driveID, worker, thumbWorker)
	}
	return err
}

// crawlerIntCred 解析 credentials 中的整数字段，缺省时返回 def。
func crawlerIntCred(d *catalog.Drive, key string, def int) int {
	if d == nil || d.Credentials == nil {
		return def
	}
	raw := strings.TrimSpace(d.Credentials[key])
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return v
}
