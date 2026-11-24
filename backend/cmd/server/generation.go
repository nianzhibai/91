package main

import (
	"context"
	"log"
	"strings"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/drives/localupload"
	"github.com/nianzhibai/91/backend/internal/drives/scriptcrawler"
	"github.com/nianzhibai/91/backend/internal/fingerprint"
	"github.com/nianzhibai/91/backend/internal/generation"
	"github.com/nianzhibai/91/backend/internal/persistence"
	"github.com/nianzhibai/91/backend/internal/preview"
	"github.com/nianzhibai/91/backend/internal/tasklimit"
)

// generationLimiter is the application-wide budget of videos running asset
// steps at once, shared fairly among sources; fingerprints take none.
func (a *App) generationLimiter() *tasklimit.Limiter {
	a.generationLimiterOnce.Do(func() {
		a.driveGenerationLimiter = tasklimit.New(a.liveConfigSettings().GenerationConcurrency)
	})
	return a.driveGenerationLimiter
}

func (a *App) newPreviewGenerator() *preview.Generator {
	previewCfg := preview.Config{}
	if a.cfg != nil {
		previewCfg = preview.Config{
			FFmpegPath:      a.cfg.Preview.FFmpegPath,
			FFprobePath:     a.cfg.Preview.FFprobePath,
			FFmpegThreads:   a.cfg.Preview.FFmpegThreads,
			DurationSeconds: a.cfg.Preview.DurationSeconds,
			Width:           a.cfg.Preview.Width,
			Segments:        a.cfg.Preview.Segments,
			LocalDir:        a.cfg.Storage.LocalPreviewDir,
		}
	}
	previewCfg.Hardware = a.generationHardware()
	return preview.New(previewCfg)
}

// newDriveGenerationRunner wires the drive's steps to one runner. A nil
// generator leaves its steps out.
func (a *App) newDriveGenerationRunner(drv drives.Drive, media preview.MediaGenerator, thumbnails preview.ThumbnailGenerator, paused bool) *generation.Runner {
	driveID := drv.ID()
	// Every step reading this drive pauses together when the provider throttles.
	cooldown := &tasklimit.Cooldown{}
	cooldownWait := drives.CooldownFor(drv.Kind())
	onCooldown := func() { a.notifyDriveRuntime(driveID, true) }
	fingerprints := fingerprint.NewJob(a.cat, drv, fingerprint.Config{RateLimitCooldown: cooldownWait})
	fingerprints.Cooldown, fingerprints.OnCooldown = cooldown, onCooldown
	steps := generation.Steps{Fingerprint: fingerprints}
	if media != nil {
		job := preview.NewMediaJob(media, a.cat, drv)
		job.Gate = &a.previewGate
		job.RateLimitCooldown, job.Cooldown, job.OnCooldown = cooldownWait, cooldown, onCooldown
		steps.Media = job
	}
	if thumbnails != nil {
		job := preview.NewThumbnailJob(thumbnails, a.cat, drv)
		job.RateLimitCooldown, job.Cooldown, job.OnCooldown = cooldownWait, cooldown, onCooldown
		steps.Thumbnail, steps.Duration = job, generation.StepFunc(job.ProbeDuration)
	}

	// The runner belongs to the runtime generation that created it: it may
	// finish its work while a configuration change is pending, but never runs
	// after a remount has replaced it.
	gate := a.driveOperationGate(driveID)
	runtimeGeneration := gate.currentGeneration()
	return generation.New(generation.Config{
		DriveID:         driveID,
		Store:           a.cat,
		Steps:           steps,
		Limiter:         a.generationLimiter(),
		Parallelism:     a.driveGenerationParallelism(context.Background(), driveID, drv.Kind()),
		Cooldown:        cooldown,
		PreviewsEnabled: a.previewEnabled,
		Admit: func() func() {
			release, admitted := gate.tryBeginTaskGeneration(runtimeGeneration)
			if !admitted {
				return nil
			}
			return release
		},
		Paused:          paused,
		OnStatusChanged: func(immediate bool) { a.notifyDriveRuntime(driveID, immediate) },
	})
}

// driveGenerationParallelism is how many videos the source generates at once:
// the administrator's number for the drive, or its kind's default. Local
// sources other than directories have no drive row and take the default.
func (a *App) driveGenerationParallelism(ctx context.Context, driveID, kind string) int {
	override := 0
	if d, err := a.cat.GetDrive(ctx, driveID); err == nil {
		override = d.GenerationParallelism
	}
	return generationParallelism(override, kind)
}

func generationParallelism(override int, kind string) int {
	if override > 0 {
		return override
	}
	return drives.DefaultGenerationParallelism(kind)
}

// applyDriveGenerationParallelism hands the drive's saved parallelism to its
// runner, which applies it from the next video on.
func (a *App) applyDriveGenerationParallelism(ctx context.Context, driveID string) error {
	d, err := a.cat.GetDrive(ctx, driveID)
	if err != nil {
		return err
	}
	if runner := a.generationRunner(driveID); runner != nil {
		runner.SetParallelism(generationParallelism(d.GenerationParallelism, d.Kind))
	}
	return nil
}

// startDriveGeneration replaces the drive's runner. A paused runner waits for
// the next wake, as after an explicit stop.
func (a *App) startDriveGeneration(ctx context.Context, driveID string, drv drives.Drive, active bool) {
	gen := a.newPreviewGenerator()
	a.registerDriveGeneration(ctx, driveID, a.newDriveGenerationRunner(drv, gen, gen, !active))
}

// registerDriveGeneration runs the runner as the drive's only one.
func (a *App) registerDriveGeneration(ctx context.Context, driveID string, runner *generation.Runner) {
	runCtx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	if a.runners == nil {
		a.runners = make(map[string]*generation.Runner)
	}
	if a.cancels == nil {
		a.cancels = make(map[string]context.CancelFunc)
	}
	if old := a.cancels[driveID]; old != nil {
		old()
	}
	a.runners[driveID] = runner
	a.cancels[driveID] = cancel
	a.mu.Unlock()
	go runner.Run(runCtx)
}

func (a *App) generationRunner(driveID string) *generation.Runner {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runners[driveID]
}

func (a *App) generationRunners() []*generation.Runner {
	a.mu.Lock()
	defer a.mu.Unlock()
	runners := make([]*generation.Runner, 0, len(a.runners))
	for _, runner := range a.runners {
		runners = append(runners, runner)
	}
	return runners
}

// wakeDriveGeneration asks the drive to take the work just written to the
// catalog. It also resumes a drive whose generation was stopped.
func (a *App) wakeDriveGeneration(driveID string) {
	if runner := a.generationRunner(driveID); runner != nil {
		runner.Wake()
	}
}

func (a *App) wakeAllDriveGeneration() {
	for _, runner := range a.generationRunners() {
		runner.Wake()
	}
}

// waitDriveGenerationIdle blocks until the drive has no due work left.
func (a *App) waitDriveGenerationIdle(ctx context.Context, driveID string) error {
	if runner := a.generationRunner(driveID); runner != nil {
		return runner.WaitIdle(ctx)
	}
	return ctx.Err()
}

// waitAllGenerationIdle blocks until every attached drive has no due work.
// Drives attached later take their work when they start.
func (a *App) waitAllGenerationIdle(ctx context.Context) error {
	for _, runner := range a.generationRunners() {
		if err := runner.WaitIdle(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) regenPreview(ctx context.Context, videoID string) {
	if !a.previewEnabled() {
		return
	}
	if err := a.cat.RequestMediaRenewal(ctx, []string{videoID}); err != nil {
		log.Printf("[preview] request renewal video=%s: %v", videoID, err)
		return
	}
	v, err := a.cat.GetVideo(ctx, videoID)
	if err != nil {
		return
	}
	a.wakeDriveGeneration(v.DriveID)
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
	ids := make([]string, 0, len(items))
	for _, v := range items {
		ids = append(ids, v.ID)
	}
	if err := a.cat.RequestMediaRenewal(ctx, ids); err != nil {
		log.Printf("[preview] request renewal of all videos: %v", err)
		return
	}
	log.Printf("[preview] requested renewal of all visible videos count=%d total=%d", len(items), total)
	a.wakeAllDriveGeneration()
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
