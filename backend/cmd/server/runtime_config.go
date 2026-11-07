package main

import (
	"context"
	"fmt"

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
	return !a.previewDisabled.Load()
}

// Publish before queue admission: Manager invokes live callbacks before exposing
// its new snapshot. Workers and crawler uploads share this atomic projection.
func (a *App) applyPreviewEnabled(ctx context.Context, enabled bool) {
	wasDisabled := a.previewDisabled.Swap(!enabled)
	if !enabled || !wasDisabled {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for driveID, worker := range a.workers {
		a.scheduleDriveGenerationEnqueue(ctx, driveID, worker, nil)
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
