package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/video-site/backend/internal/api"
	"github.com/video-site/backend/internal/crawlerupload"
	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/drives/localupload"
	"github.com/video-site/backend/internal/fingerprint"
	"github.com/video-site/backend/internal/preview"
	"github.com/video-site/backend/internal/transcode"
)

// teaserEnabledForDrive 查询某个 drive 当前的 per-drive 预览视频开关。
//
// 预览视频生成不再由全局 setting 控制，而是由 catalog.drives.teaser_enabled
// 决定。任何"是否入队 preview worker"的判断都应通过这个方法读，避免把状态
// 散落到 App 内存里和 DB 不一致。
//
// local-upload 是内置盘，不一定有 catalog.drives 行；缺省按开启处理。
//
// 其它 drive 读 catalog 失败时退化成 false（不生成）：比 "默认开" 更安全 —— 读不到
// 状态时倾向不消耗 ffmpeg；调用方会记日志，运维能立刻看到问题。
func (a *App) teaserEnabledForDrive(ctx context.Context, driveID string) bool {
	d, err := a.activeDriveConfig(ctx, driveID)
	if err != nil {
		if driveID == localupload.DriveID && errors.Is(err, sql.ErrNoRows) {
			return true
		}
		log.Printf("[preview] read teaser_enabled drive=%s: %v (treating as disabled)", driveID, err)
		return false
	}
	return d.TeaserEnabled
}

// Theme 线程安全读当前主题。
func (a *App) Theme() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.theme == "" {
		return "dark"
	}
	return a.theme
}

// SetTheme 切换并持久化主题；未知值会返回错误。
func (a *App) SetTheme(ctx context.Context, theme string) error {
	if theme != "dark" && theme != "pink" && theme != "sky" {
		return fmt.Errorf("unsupported theme %q", theme)
	}
	a.mu.Lock()
	a.theme = theme
	a.mu.Unlock()
	return a.cat.SetSetting(ctx, "ui.theme", theme)
}

// loadTheme 从 DB 读全站主题；找不到时回退到 "dark"。
func (a *App) loadTheme(ctx context.Context) {
	v, err := a.cat.GetSetting(ctx, "ui.theme", "dark")
	if err != nil {
		log.Printf("[theme] load setting: %v (fallback to dark)", err)
		a.mu.Lock()
		a.theme = "dark"
		a.mu.Unlock()
		return
	}
	if v != "pink" && v != "dark" && v != "sky" {
		v = "dark"
	}
	a.mu.Lock()
	a.theme = v
	a.mu.Unlock()
}

func (a *App) nightlyJobStatus() api.NightlyJobStatus {
	if a.nightlyRunner == nil {
		return api.NightlyJobStatus{State: "idle"}
	}
	status := a.nightlyRunner.Status()
	return api.NightlyJobStatus{
		State:          status.State,
		Running:        status.Running,
		Queued:         status.Queued,
		StartedAt:      formatOptionalRFC3339(status.StartedAt),
		LastFinishedAt: formatOptionalRFC3339(status.LastFinishedAt),
	}
}

func formatOptionalRFC3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func (a *App) driveGenerationStatuses() map[string]api.DriveGenerationStatuses {
	a.scanQueueMu.Lock()
	scanningDrives := make(map[string]bool, len(a.scanQueued))
	for id, running := range a.scanQueued {
		scanningDrives[id] = running
	}
	scanProgresses := make(map[string]driveScanProgress, len(a.scanProgress))
	for id, progress := range a.scanProgress {
		scanProgresses[id] = progress
	}
	a.scanQueueMu.Unlock()

	a.uploadProgressMu.Lock()
	uploadProgresses := make(map[string]driveUploadProgress, len(a.uploadProgress))
	for id, progress := range a.uploadProgress {
		uploadProgresses[id] = progress
	}
	a.uploadProgressMu.Unlock()

	a.mu.Lock()
	previewWorkers := make(map[string]*preview.Worker, len(a.workers))
	for id, worker := range a.workers {
		previewWorkers[id] = worker
	}
	thumbWorkers := make(map[string]*preview.ThumbWorker, len(a.thumbWorkers))
	for id, worker := range a.thumbWorkers {
		thumbWorkers[id] = worker
	}
	fingerprintWorkers := make(map[string]*fingerprint.Worker, len(a.fingerprintWorkers))
	for id, worker := range a.fingerprintWorkers {
		fingerprintWorkers[id] = worker
	}
	a.mu.Unlock()

	a.transcodeMu.Lock()
	transcodeWorkers := make(map[string]*transcode.Worker, len(a.transcodeWorkers))
	for id, worker := range a.transcodeWorkers {
		transcodeWorkers[id] = worker
	}
	a.transcodeMu.Unlock()

	out := make(map[string]api.DriveGenerationStatuses, len(scanningDrives)+len(previewWorkers)+len(thumbWorkers)+len(fingerprintWorkers)+len(uploadProgresses)+len(transcodeWorkers))
	now := time.Now()
	for id, running := range scanningDrives {
		if !running {
			continue
		}
		progress := scanProgresses[id]
		state := "scanning"
		if progress.CooldownUntil.After(now) {
			state = "cooling"
		}
		status := out[id]
		status.Scan = api.GenerationStatus{
			State:        state,
			ScannedCount: progress.Scanned,
			AddedCount:   progress.Added,
		}
		if !progress.CooldownUntil.IsZero() {
			status.Scan.CooldownUntil = progress.CooldownUntil.Format(time.RFC3339)
		}
		out[id] = status
	}
	for id, worker := range previewWorkers {
		status := out[id]
		status.Preview = generationStatusFromPreview(worker.Status())
		out[id] = status
	}
	for id, worker := range thumbWorkers {
		status := out[id]
		status.Thumbnail = generationStatusFromPreview(worker.Status())
		out[id] = status
	}
	for id, worker := range fingerprintWorkers {
		status := out[id]
		status.Fingerprint = generationStatusFromFingerprint(worker.Status())
		out[id] = status
	}
	for id, progress := range uploadProgresses {
		state := progress.State
		if state == "" {
			state = "idle"
		}
		status := out[id]
		status.Upload = api.GenerationStatus{
			State:        state,
			CurrentTitle: progress.CurrentTitle,
			QueueLength:  progress.QueueLength,
			DoneCount:    progress.DoneCount,
			TotalCount:   progress.TotalCount,
		}
		out[id] = status
	}
	for id, worker := range transcodeWorkers {
		status := out[id]
		status.Transcode = generationStatusFromTranscode(worker.Status())
		out[id] = status
	}
	return out
}

func (a *App) previewGenerationVideoIDs() map[string]bool {
	a.mu.Lock()
	previewWorkers := make([]*preview.Worker, 0, len(a.workers))
	for _, worker := range a.workers {
		previewWorkers = append(previewWorkers, worker)
	}
	a.mu.Unlock()

	out := make(map[string]bool)
	for _, worker := range previewWorkers {
		for _, id := range worker.ActiveVideoIDs() {
			out[id] = true
		}
	}
	return out
}

func (a *App) updateCrawlerUploadProgress(progress crawlerupload.UploadProgress) {
	driveID := strings.TrimSpace(progress.DriveID)
	if driveID == "" {
		return
	}
	state := strings.TrimSpace(progress.State)
	if state == "" {
		state = "idle"
	}
	a.uploadProgressMu.Lock()
	if a.uploadProgress == nil {
		a.uploadProgress = make(map[string]driveUploadProgress)
	}
	if state == "idle" {
		delete(a.uploadProgress, driveID)
		a.uploadProgressMu.Unlock()
		return
	}
	a.uploadProgress[driveID] = driveUploadProgress{
		State:        state,
		CurrentTitle: strings.TrimSpace(progress.CurrentTitle),
		QueueLength:  progress.QueueLength,
		DoneCount:    progress.DoneCount,
		TotalCount:   progress.TotalCount,
	}
	a.uploadProgressMu.Unlock()
}

func (a *App) clearCrawlerUploadProgress(driveID string) bool {
	driveID = strings.TrimSpace(driveID)
	if driveID == "" {
		return false
	}
	a.uploadProgressMu.Lock()
	_, ok := a.uploadProgress[driveID]
	delete(a.uploadProgress, driveID)
	a.uploadProgressMu.Unlock()
	return ok
}

func generationStatusFromPreview(status preview.TaskStatus) api.GenerationStatus {
	state := status.State
	if state == "" {
		state = "idle"
	}
	out := api.GenerationStatus{
		State:        state,
		CurrentTitle: status.CurrentTitle,
		QueueLength:  status.QueueLength,
	}
	if !status.CooldownUntil.IsZero() {
		out.CooldownUntil = status.CooldownUntil.Format(time.RFC3339)
	}
	return out
}

func generationStatusFromFingerprint(status fingerprint.TaskStatus) api.GenerationStatus {
	state := status.State
	if state == "" {
		state = "idle"
	}
	out := api.GenerationStatus{
		State:        state,
		CurrentTitle: status.CurrentTitle,
		QueueLength:  status.QueueLength,
	}
	if !status.CooldownUntil.IsZero() {
		out.CooldownUntil = status.CooldownUntil.Format(time.RFC3339)
	}
	return out
}

func generationStatusFromTranscode(status transcode.TaskStatus) api.GenerationStatus {
	state := status.State
	if state == "" {
		state = "idle"
	}
	return api.GenerationStatus{
		State:        state,
		CurrentTitle: status.CurrentTitle,
		QueueLength:  status.QueueLength,
		DoneCount:    status.DoneCount,
		TotalCount:   status.TotalCount,
	}
}

// transcodeWorkDir 返回转码用的本地临时目录（下载原片 / 写产物），与
// localUploadDir 一样挂在数据目录下，避免 /tmp 空间不足。
func (a *App) transcodeWorkDir() string {
	return filepath.Join(filepath.Dir(a.cfg.Storage.LocalPreviewDir), "transcode-tmp")
}

// startDriveTranscode 手动开启某盘的浏览器兼容性转码。
// 转码从不自动运行：扫盘、夜间流水线都不会触发，这里是唯一入口。
// 任务跑完候选列表后自然结束；中途可用 stopDriveTranscode / 停止所有任务中断。
func (a *App) startDriveTranscode(ctx context.Context, driveID string) (bool, string) {
	driveID = strings.TrimSpace(driveID)
	if driveID == "" {
		return false, "缺少存储 ID"
	}
	// Admission must precede every runtime/registry snapshot. Otherwise a
	// concurrent save can remount the drive between Get and task registration,
	// leaving this worker uploading through a retired provider instance.
	taskCtx, done, admitted := a.registerDriveTaskContext(ctx, driveID, 0)
	if !admitted {
		return false, "该存储有配置等待生效，请稍后重试"
	}
	taskOwned := true
	defer func() {
		if taskOwned {
			done()
		}
	}()

	drv, ok := a.registry.Get(driveID)
	if !ok {
		return false, "存储未挂载或不可用"
	}
	if _, ok := drv.(drives.Uploader); !ok {
		return false, "该存储不支持上传转码产物"
	}
	workDir := a.transcodeWorkDir()
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return false, "创建转码临时目录失败: " + err.Error()
	}

	a.transcodeMu.Lock()
	if a.transcodeWorkers == nil {
		a.transcodeWorkers = make(map[string]*transcode.Worker)
		a.transcodeCancels = make(map[string]context.CancelFunc)
	}
	if existing := a.transcodeWorkers[driveID]; existing != nil {
		a.transcodeMu.Unlock()
		return false, "该存储的转码任务已在运行"
	}
	worker := transcode.NewWorker(transcode.Config{
		FFmpegPath:  a.cfg.Preview.FFmpegPath,
		FFprobePath: a.cfg.Preview.FFprobePath,
		WorkDir:     workDir,
	}, a.cat, drv)
	runCtx, cancel := context.WithCancel(taskCtx)
	a.transcodeWorkers[driveID] = worker
	a.transcodeCancels[driveID] = cancel
	a.transcodeMu.Unlock()
	taskOwned = false

	go func() {
		defer func() {
			cancel()
			a.transcodeMu.Lock()
			if a.transcodeWorkers[driveID] == worker {
				delete(a.transcodeWorkers, driveID)
				delete(a.transcodeCancels, driveID)
			}
			a.transcodeMu.Unlock()
			done()
			// Transcode may atomically add its output directory to skip_dir_ids.
			// Refresh only after releasing this task, and recheck the gate after
			// reading the catalog so a concurrent config transition keeps its old
			// generation snapshot immutable.
			a.refreshActiveDriveConfigIfStable(driveID)
		}()
		candidates, err := a.cat.ListTranscodeCandidates(runCtx, driveID, 0)
		if err != nil {
			log.Printf("[transcode] list candidates drive=%s: %v", driveID, err)
			return
		}
		if len(candidates) == 0 {
			log.Printf("[transcode] drive=%s no candidates", driveID)
			return
		}
		log.Printf("[transcode] drive=%s start, %d candidates", driveID, len(candidates))
		worker.Run(runCtx, candidates)
	}()
	return true, ""
}

// stopDriveTranscode 手动停止某盘的转码任务。返回是否有任务被停。
func (a *App) stopDriveTranscode(driveID string) bool {
	driveID = strings.TrimSpace(driveID)
	a.transcodeMu.Lock()
	cancel := a.transcodeCancels[driveID]
	delete(a.transcodeCancels, driveID)
	delete(a.transcodeWorkers, driveID)
	a.transcodeMu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	log.Printf("[transcode] stop drive=%s", driveID)
	return true
}
