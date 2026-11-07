package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/api"
	"github.com/nianzhibai/91/backend/internal/crawlerupload"
	"github.com/nianzhibai/91/backend/internal/driveevents"
	"github.com/nianzhibai/91/backend/internal/fingerprint"
	"github.com/nianzhibai/91/backend/internal/preview"
)

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
		Outcome:        status.Outcome,
		ScanResults:    status.ScanResults,
		Issues:         status.Issues,
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

	a.crawlerUploadMu.Lock()
	crawlerUploads := make(map[string]bool, len(a.crawlerUploadRunning))
	for id, running := range a.crawlerUploadRunning {
		crawlerUploads[id] = running
	}
	a.crawlerUploadMu.Unlock()

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

	out := make(map[string]api.DriveGenerationStatuses, len(scanningDrives)+len(previewWorkers)+len(thumbWorkers)+len(fingerprintWorkers)+len(uploadProgresses))
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
	for id, running := range crawlerUploads {
		if !running {
			continue
		}
		status := out[id]
		if status.Upload.State == "" || status.Upload.State == "idle" {
			status.Upload.State = "queued"
			out[id] = status
		}
	}
	return out
}

func (a *App) driveGenerationStatus(id string) api.DriveGenerationStatuses {
	a.scanQueueMu.Lock()
	scanning, progress := a.scanQueued[id], a.scanProgress[id]
	a.scanQueueMu.Unlock()
	a.mu.Lock()
	previewWorker, thumbWorker, fingerprintWorker := a.workers[id], a.thumbWorkers[id], a.fingerprintWorkers[id]
	a.mu.Unlock()
	status := api.DriveGenerationStatuses{}
	if scanning {
		status.Scan = api.GenerationStatus{State: "scanning", ScannedCount: progress.Scanned, AddedCount: progress.Added}
		if progress.CooldownUntil.After(time.Now()) {
			status.Scan.State = "cooling"
			status.Scan.CooldownUntil = formatOptionalRFC3339(progress.CooldownUntil)
		}
	}
	status.Preview = generationStatusFromPreview(previewWorker.Status())
	status.Thumbnail = generationStatusFromPreview(thumbWorker.Status())
	status.Fingerprint = generationStatusFromFingerprint(fingerprintWorker.Status())
	return status
}

func (a *App) notifyDriveRuntime(id string, immediate bool) {
	if a.cat != nil {
		a.cat.DriveEvents().Notify(id, immediate, driveevents.ActivityChanged)
	}
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
