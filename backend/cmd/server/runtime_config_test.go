package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/config"
	"github.com/video-site/backend/internal/nightly"
)

func TestConfigSavePersistsAndHotUpdatesRuntimeSettings(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("nightly:\n  start_time: \"01:00\"\n  timezone: Etc/UTC\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewManager(path)
	if err != nil {
		t.Fatal(err)
	}
	tagCacheInvalidations := 0
	app := &App{
		cat:           cat,
		configManager: manager,
		onTagsChanged: func() { tagCacheInvalidations++ },
	}
	app.nightlyRunner = nightly.New(nightly.Config{
		Settings:  cat,
		Disabled:  manager.LiveSettings().NightlyDisabled,
		StartTime: manager.LiveSettings().NightlyStartTime,
		Timezone:  manager.LiveSettings().NightlyTimezone,
	})
	if err := manager.SetApply(func(settings config.LiveSettings) error {
		return app.applyLiveConfig(context.Background(), settings)
	}); err != nil {
		t.Fatal(err)
	}

	_, version, err := manager.ReadYAML()
	if err != nil {
		t.Fatal(err)
	}
	next, err := manager.ReplaceYAML([]byte("nightly:\n  disabled: true\n  start_time: \"00:45\"\n  timezone: Asia/Shanghai\ntags:\n  builtin_pack_enabled: false\ngeneration:\n  preview_concurrency: 4\n  thumbnail_concurrency: 2\n  fingerprint_concurrency: 3\n"), version)
	if err != nil {
		t.Fatalf("replace config: %v", err)
	}
	if next.Settings.NightlyStartTime != "00:45" {
		t.Fatalf("updated settings = %#v", next.Settings)
	}
	if got := app.nightlyRunner.StartTime(); got != "00:45" {
		t.Fatalf("live scheduler start time = %q, want 00:45", got)
	}
	if next.Settings.NightlyTimezone != "Asia/Shanghai" {
		t.Fatalf("updated settings = %#v", next.Settings)
	}
	if got := app.nightlyRunner.Timezone(); got != "Asia/Shanghai" {
		t.Fatalf("live scheduler timezone = %q, want Asia/Shanghai", got)
	}
	if !next.Settings.NightlyDisabled || !app.nightlyRunner.Disabled() {
		t.Fatalf("live scheduler disabled state was not applied: %#v", next.Settings)
	}
	if next.RestartRequired {
		t.Fatal("preview concurrency should hot update without a restart")
	}
	if next.Settings.PreviewConcurrency != 4 || next.Settings.ThumbnailConcurrency != 2 || next.Settings.FingerprintConcurrency != 3 {
		t.Fatalf("global settings not applied: %+v", next.Settings)
	}
	thumbnails, previews, fingerprints := app.generationLimits()
	assertBudgetAvailable(t, thumbnails, 2)
	assertBudgetAvailable(t, previews, 4)
	assertBudgetAvailable(t, fingerprints, 3)
	latePreview, lateThumb, lateFingerprint := app.newDriveGenerationWorkers(&serverFakeDrive{})
	if latePreview.Limiter != previews || lateThumb.Limiter != thumbnails || lateFingerprint.Config.Limiter != fingerprints {
		t.Fatal("late attached drive did not receive live global limits")
	}
	if tagCacheInvalidations != 0 {
		t.Fatalf("configuration changes invalidated tags: %d", tagCacheInvalidations)
	}
	reloaded, err := config.NewManager(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.LiveSettings(); got != next.Settings {
		t.Fatalf("reloaded settings = %#v, want %#v", got, next.Settings)
	}
}

func TestLoadLegacyRuntimeSettingsIgnoresInvalidValues(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	if err := cat.SetSetting(ctx, legacyNightlyStartTimeSetting, "25:00"); err != nil {
		t.Fatal(err)
	}
	legacy, err := loadLegacyRuntimeSettings(ctx, cat)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.NightlyStartTime != nil {
		t.Fatalf("invalid legacy values should be ignored: %#v", legacy)
	}
}

func TestMigratedRuntimeSettingsCanBeRemovedFromSQLite(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	if err := cat.SetSetting(ctx, legacyNightlyStartTimeSetting, "03:20"); err != nil {
		t.Fatal(err)
	}
	if err := cat.DeleteSettings(ctx, legacyNightlyStartTimeSetting); err != nil {
		t.Fatal(err)
	}
	legacy, err := loadLegacyRuntimeSettings(ctx, cat)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.NightlyStartTime != nil {
		t.Fatalf("deleted SQLite settings returned: %#v", legacy)
	}
}
