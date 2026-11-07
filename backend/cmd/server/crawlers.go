package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/crawljob"
	"github.com/nianzhibai/91/backend/internal/drives/scriptcrawler"
)

func (a *App) scheduleScriptCrawlerCrawl(ctx context.Context, driveID string) bool {
	_, err := a.startScriptCrawlerCrawl(ctx, driveID)
	if err != nil {
		log.Printf("[scriptcrawler] drive=%s rejected: %v", driveID, err)
	}
	return err == nil
}

// Admission and snapshot capture finish before returning an accepted task ID.
func (a *App) acceptCrawlerTask(ctx context.Context, driveID string) (context.Context, *scriptcrawler.Crawler, *scriptcrawler.Task, func(), error) {
	if a.driveHasActiveWork(driveID) {
		return nil, nil, nil, nil, fmt.Errorf("当前爬虫有正在进行的任务")
	}
	taskCtx, done, admitted := a.registerDriveTaskContext(ctx, driveID, driveTaskScopeScan)
	if !admitted {
		return nil, nil, nil, nil, fmt.Errorf("爬虫配置正在更新")
	}
	if !a.beginDriveScanOrCrawl(driveID) {
		done()
		return nil, nil, nil, nil, fmt.Errorf("爬虫已排队或正在运行")
	}
	release := func() { a.endDriveScanOrCrawl(driveID); done() }
	fail := func(err error) (context.Context, *scriptcrawler.Crawler, *scriptcrawler.Task, func(), error) {
		release()
		return nil, nil, nil, nil, err
	}
	if err := a.ensureDriveAttached(taskCtx, driveID); err != nil {
		return fail(err)
	}
	d, err := a.activeDriveConfig(taskCtx, driveID)
	if err != nil {
		return fail(err)
	}
	if d == nil {
		return fail(fmt.Errorf("爬虫不存在"))
	}
	// Hold both configurations throughout crawl, generation and upload.
	if target := strings.TrimSpace(d.Credentials["upload_drive_id"]); target != "" {
		targetCtx, targetDone, ok := a.registerDriveTaskContext(taskCtx, target, 0)
		if !ok {
			return fail(fmt.Errorf("上传目标配置正在更新"))
		}
		sourceRelease := release
		release = func() { targetDone(); sourceRelease() }
		taskCtx = targetCtx
	}
	a.mu.Lock()
	crawler := a.scriptCrawlers[driveID]
	a.mu.Unlock()
	if crawler == nil {
		return fail(fmt.Errorf("爬虫未加载"))
	}
	task, err := crawler.Prepare(taskCtx, crawlerIntCred(d, "target_new", scriptcrawler.DefaultTargetNew), applog.ContextFields(ctx).TaskID)
	if err != nil {
		return fail(err)
	}
	return taskCtx, crawler, task, release, nil
}
func (a *App) startScriptCrawlerCrawl(ctx context.Context, driveID string) (string, error) {
	taskCtx, crawler, task, release, err := a.acceptCrawlerTask(ctx, driveID)
	if err != nil {
		return "", err
	}
	go func() {
		defer release()
		if _, err := a.executeCrawlerTask(taskCtx, driveID, crawler, task); err != nil {
			log.Printf("[scriptcrawler] task=%s: %v", task.Result.TaskID, err)
		}
	}()
	return task.Result.TaskID, nil
}
func (a *App) runScriptCrawlerCrawl(ctx context.Context, driveID string) error {
	taskCtx, crawler, task, release, err := a.acceptCrawlerTask(ctx, driveID)
	if err != nil {
		return err
	}
	defer release()
	_, err = a.executeCrawlerTask(taskCtx, driveID, crawler, task)
	return err
}
func (a *App) executeCrawlerTask(ctx context.Context, driveID string, crawler *scriptcrawler.Crawler, task *scriptcrawler.Task) (*crawljob.Result, error) {
	result, runErr := crawler.RunTask(ctx, task, func(ctx context.Context, task *scriptcrawler.Task) error {
		if err := task.RunStage(ctx, "generation", func(ctx context.Context) error {
			finishEnqueue := a.beginDriveResourceEnqueue(driveID)
			a.mu.Lock()
			worker, thumbWorker, fingerprintWorker := a.workers[driveID], a.thumbWorkers[driveID], a.fingerprintWorkers[driveID]
			a.mu.Unlock()
			a.enqueueFingerprintBackfill(ctx, driveID, fingerprintWorker)
			a.enqueueDriveGeneration(ctx, driveID, worker, thumbWorker)
			finishEnqueue()
			return a.waitDriveResourcesIdle(ctx, driveID)
		}); err != nil {
			return err
		}
		return task.RunStage(ctx, "upload", func(ctx context.Context) error { return a.finishCrawlerProcessing(ctx, driveID) })
	})
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return result, errors.Join(runErr, a.updateScriptCrawlerRunState(saveCtx, driveID, runErr))
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

	_, localCount, err := a.cat.CrawlerUploadScope(taskCtx, driveID)
	if err != nil {
		log.Printf("[scriptcrawler] drive=%s manual upload candidates: %v", driveID, err)
		return false, "读取待上传视频失败"
	}
	if localCount == 0 {
		return false, "没有待上传的本地视频"
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

func crawlerCatalogVideoIDPrefixes(d *catalog.Drive) []string {
	if d == nil {
		return nil
	}
	return []string{
		scriptcrawler.Kind + "-" + d.ID + "-",
	}
}

func (a *App) finishCrawlerProcessing(ctx context.Context, driveID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d, err := a.activeDriveConfig(ctx, driveID)
	if err != nil {
		return err
	}
	if d == nil {
		return fmt.Errorf("crawler configuration missing")
	}
	if err := a.waitDriveResourcesIdle(ctx, driveID); err != nil {
		return err
	}
	var uploadErr error
	if target := strings.TrimSpace(d.Credentials["upload_drive_id"]); target != "" {
		uploadErr = a.migrateCrawlerVideos(ctx, driveID, target)
	}
	// Restoring retained local videos is independent of the destination's
	// availability. Preserve both outcomes if upload and restoration fail.
	return errors.Join(uploadErr, a.restoreScriptCrawlerVideos(ctx, driveID))
}

func (a *App) migrateCrawlerVideos(ctx context.Context, driveID, targetDriveID string) error {
	if a.crawlerUploader == nil {
		return fmt.Errorf("上传迁移器未初始化")
	}
	targetCtx, targetDone, ok := a.registerDriveTaskContext(ctx, targetDriveID, 0)
	if !ok {
		return fmt.Errorf("上传目标配置正在更新")
	}
	defer targetDone()
	return a.crawlerUploader.RunDrive(targetCtx, driveID)
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
		finishEnqueue := a.beginDriveResourceEnqueue(driveID)
		defer finishEnqueue()
		a.mu.Lock()
		worker := a.workers[driveID]
		thumbWorker := a.thumbWorkers[driveID]
		fingerprintWorker := a.fingerprintWorkers[driveID]
		a.mu.Unlock()
		a.enqueueFingerprintBackfill(ctx, driveID, fingerprintWorker)
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
