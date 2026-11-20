package main

import (
	"context"
	"fmt"
	"log"
	"maps"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
	"github.com/nianzhibai/91/backend/internal/preview"
	"github.com/nianzhibai/91/backend/internal/tasklimit"
)

const (
	legacyNightlyStartTimeSetting = "automation.nightly_start_time"
	legacySettingMissing          = "\x0091-config-setting-missing\x00"
)

func (a *App) liveConfigSettings() config.LiveSettings {
	if a == nil || a.configManager == nil {
		return config.DefaultLiveSettings()
	}
	return a.configManager.LiveSettings()
}

func (a *App) applyLiveConfig(ctx context.Context, settings config.LiveSettings) error {
	if a == nil {
		return nil
	}
	a.generationHardware().SetEnabled(ctx, settings.UseGPU)
	if a.nightlyRunner != nil {
		// The configuration parser already validated and normalized these values.
		// Runner validates again and swaps the schedule state atomically at its boundary.
		if err := a.nightlyRunner.UpdateSchedule(
			settings.NightlyStartTime,
			settings.NightlyTimezone,
			settings.NightlyDisabled,
		); err != nil {
			return fmt.Errorf("update nightly schedule: %w", err)
		}
	}
	thumbnails, previews, fingerprints := a.generationLimits()
	thumbnails.SetLimit(settings.ThumbnailConcurrency)
	previews.SetLimit(settings.PreviewConcurrency)
	fingerprints.SetLimit(settings.FingerprintConcurrency)

	a.applyPreviewEnabled(ctx, settings.PreviewEnabled)
	return nil
}

func (a *App) generationHardware() *preview.Hardware {
	a.generationHardwareOnce.Do(func() {
		cfg := preview.Config{}
		if a.cfg != nil {
			cfg.FFmpegPath = a.cfg.Preview.FFmpegPath
			cfg.FFprobePath = a.cfg.Preview.FFprobePath
			cfg.FFmpegThreads = a.cfg.Preview.FFmpegThreads
		}
		a.hardware = preview.NewHardware(cfg)
	})
	return a.hardware
}

func (a *App) previewEnabled() bool {
	return a.previewGate.Enabled()
}

// applyPreviewEnabled runs before Manager publishes the new snapshot, so queue
// admission observes the switch first. Disabling the gate cancels running
// previews; enabling catches up on the work skipped while off.
func (a *App) applyPreviewEnabled(ctx context.Context, enabled bool) {
	if a.previewGate.Set(enabled) && enabled {
		go a.resumePreviewGeneration(ctx)
	}
}

// resumePreviewGeneration repairs local preview references, which nightly
// maintenance skips while previews are off, then admits pending previews for
// every attached drive. Drives attached after this snapshot run their own
// initial pending scan with the switch already on.
func (a *App) resumePreviewGeneration(ctx context.Context) {
	if a.cfg != nil && a.cat != nil {
		if _, err := a.reconcileMissingLocalPreviewFiles(ctx); err != nil && ctx.Err() == nil {
			log.Printf("[preview] reconcile local previews after enabling: %v", err)
		}
	}
	a.mu.Lock()
	workers := maps.Clone(a.workers)
	a.mu.Unlock()
	for driveID, worker := range workers {
		go func() {
			taskCtx, done := a.registerDriveTaskContextWaiting(ctx, driveID, driveTaskScopePreview)
			defer done()
			if taskCtx.Err() != nil {
				return
			}
			a.enqueueDriveGeneration(taskCtx, driveID, worker, nil)
		}()
	}
}

func loadLegacyRuntimeSettings(ctx context.Context, cat *catalog.Catalog) (config.LegacyRuntimeSettings, error) {
	var legacy config.LegacyRuntimeSettings
	startTime, err := cat.GetSetting(ctx, legacyNightlyStartTimeSetting, legacySettingMissing)
	if err != nil {
		return legacy, err
	}
	if startTime != legacySettingMissing {
		if normalized, normalizeErr := config.NormalizeNightlyStartTime(startTime); normalizeErr == nil {
			legacy.NightlyStartTime = &normalized
		}
	}
	return legacy, nil
}

// generationLimits initializes shared budgets once. Live updates resize these
// objects so active, newly attached and replaced workers all observe the limit.
func (a *App) generationLimits() (*tasklimit.Limiter, *tasklimit.Limiter, *tasklimit.Limiter) {
	a.generationLimitsOnce.Do(func() {
		limits := config.Generation{
			ThumbnailConcurrency:   config.DefaultGenerationConcurrency,
			PreviewConcurrency:     config.DefaultGenerationConcurrency,
			FingerprintConcurrency: config.DefaultGenerationConcurrency,
		}
		if a.cfg != nil {
			limits = a.cfg.Generation
		}
		a.thumbnailLimiter = tasklimit.New(limits.ThumbnailConcurrency)
		a.previewLimiter = tasklimit.New(limits.PreviewConcurrency)
		a.fingerprintLimiter = tasklimit.New(limits.FingerprintConcurrency)
	})
	return a.thumbnailLimiter, a.previewLimiter, a.fingerprintLimiter
}
