package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/api"
	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/crawlerupload"
	"github.com/nianzhibai/91/backend/internal/driveevents"
	"github.com/nianzhibai/91/backend/internal/generation"
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
	if theme != "dark" && theme != "light" {
		return fmt.Errorf("unsupported theme %q", theme)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.cat.SetSetting(ctx, "ui.theme", theme); err != nil {
		return err
	}
	a.theme = theme
	return nil
}

// loadTheme 从 DB 读全站主题；找不到时回退到 "dark"。
func (a *App) loadTheme(ctx context.Context) {
	v, err := a.cat.GetSetting(ctx, "ui.theme", "dark")
	if err != nil {
		applog.Warn(ctx, "load theme setting failed; falling back to dark", err, "component", "theme")
		a.mu.Lock()
		a.theme = "dark"
		a.mu.Unlock()
		return
	}
	if v == "pink" || v == "sky" {
		v = "light"
		if err := a.cat.SetSetting(ctx, "ui.theme", v); err != nil {
			applog.Warn(ctx, "migrate light theme setting failed", err, "component", "theme")
		}
	}
	if v != "light" && v != "dark" {
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
	runners := make(map[string]*generation.Runner, len(a.runners))
	for id, runner := range a.runners {
		runners[id] = runner
	}
	a.mu.Unlock()

	out := make(map[string]api.DriveGenerationStatuses, len(scanningDrives)+len(runners)+len(uploadProgresses))
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
	for id, runner := range runners {
		status := out[id]
		status.Thumbnail, status.Preview, status.Fingerprint = runnerGenerationStatuses(runner.Status())
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
	status := api.DriveGenerationStatuses{}
	if scanning {
		status.Scan = api.GenerationStatus{State: "scanning", ScannedCount: progress.Scanned, AddedCount: progress.Added}
		if progress.CooldownUntil.After(time.Now()) {
			status.Scan.State = "cooling"
			status.Scan.CooldownUntil = formatOptionalRFC3339(progress.CooldownUntil)
		}
	}
	runnerStatus := generation.Status{State: "idle"}
	if runner := a.generationRunner(id); runner != nil {
		runnerStatus = runner.Status()
	}
	status.Thumbnail, status.Preview, status.Fingerprint = runnerGenerationStatuses(runnerStatus)
	return status
}

func (a *App) notifyDriveRuntime(id string, immediate bool) {
	if a.cat != nil {
		a.cat.DriveEvents().Notify(id, immediate, driveevents.ActivityChanged)
	}
}

// previewGenerationVideoIDs lists the videos whose media job is running.
func (a *App) previewGenerationVideoIDs() map[string]bool {
	out := make(map[string]bool)
	for _, runner := range a.generationRunners() {
		for _, activity := range runner.Status().Active {
			if activity.Kind == generation.KindMedia {
				out[activity.ID] = true
			}
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

// runnerGenerationStatuses projects a drive's runner onto the API's per-step
// statuses. Duration probes count with covers, which own them; a video
// missing both counts once.
func runnerGenerationStatuses(status generation.Status) (thumbnail, preview, fingerprint api.GenerationStatus) {
	project := func(due int, kinds ...generation.Kind) api.GenerationStatus {
		out := api.GenerationStatus{State: "idle"}
		if status.Paused || status.State == "idle" {
			due = 0
		}
		var running []generation.Activity
		for _, activity := range status.Active {
			if slices.Contains(kinds, activity.Kind) {
				running = append(running, activity)
			}
		}
		switch {
		case !status.CooldownUntil.IsZero():
			out.State = "cooling"
			out.CooldownUntil = status.CooldownUntil.Format(time.RFC3339)
		case len(running) > 0:
			out.State = "generating"
			out.CurrentTitle = running[0].Title
			due -= len(running)
		case due > 0:
			out.State = "queued"
		}
		out.QueueLength = max(due, 0)
		return out
	}
	counts := status.Counts
	thumbnail = project(counts.Covers, generation.KindThumbnail, generation.KindDuration)
	preview = project(counts.Media, generation.KindMedia)
	fingerprint = project(counts.Fingerprints, generation.KindFingerprint)
	return thumbnail, preview, fingerprint
}
