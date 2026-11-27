package main

import (
	"context"
	"fmt"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
	"github.com/nianzhibai/91/backend/internal/preview"
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
	a.generationLimiter().SetLimit(settings.GenerationConcurrency)
	for _, runner := range a.generationRunners() {
		runner.NotifyCapacityChanged()
	}

	if a.logRuntime != nil {
		if lvl, err := applog.ParseLevel(settings.LogLevel); err == nil {
			a.logRuntime.SetLevel(lvl)
		}
		a.logRuntime.SetLimits(int64(settings.LogMaxTotalSizeMB)*1024*1024, settings.LogRetentionDays)
	}
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

// applyPreviewEnabled runs before Manager publishes the new snapshot, so
// runners observe the switch first. Disabling the gate stops new teasers from
// being generated while letting signatures and in-flight jobs finish; enabling
// catches up on the work skipped while off.
func (a *App) applyPreviewEnabled(ctx context.Context, enabled bool) {
	if a.previewGate.Set(enabled) && enabled {
		go a.resumePreviewGeneration(ctx)
	}
}

// resumePreviewGeneration repairs local preview references, which nightly
// maintenance skips while previews are off, lifts the deferrals of videos
// whose teaser is pending, then wakes every drive to take the previews that
// became due.
func (a *App) resumePreviewGeneration(ctx context.Context) {
	if a.cat != nil {
		if a.cfg != nil {
			if _, err := a.reconcileMissingLocalPreviewFiles(ctx); err != nil && ctx.Err() == nil {
				applog.Error(ctx, "reconcile local previews after enabling failed", err, "component", "preview")
			}
		}
		if _, err := a.cat.LiftPreviewDeferrals(ctx); err != nil && ctx.Err() == nil {
			applog.Error(ctx, "lift preview deferrals after enabling failed", err, "component", "preview")
		}
	}
	a.wakeAllDriveGeneration()
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
