package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func newManagerForTest(t *testing.T, source string) (*Manager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatalf("write config: %v", err)
	}
	manager, err := NewManager(path)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	return manager, path
}

func TestGPUSettingDefaultsToCPUAndHotReloads(t *testing.T) {
	manager, path := newManagerForTest(t, "generation: { use_gpu: false }\n")
	if manager.LiveSettings().UseGPU || DefaultLiveSettings().UseGPU {
		t.Fatal("GPU must be opt-in")
	}
	var applied bool
	if err := manager.SetApply(func(settings LiveSettings) error {
		applied = settings.UseGPU
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, version, _ := manager.ReadYAML()
	result, err := manager.ReplaceYAML([]byte("generation: { use_gpu: true }\n"), version)
	if err != nil || result.RestartRequired || !result.Settings.UseGPU || !applied {
		t.Fatalf("GPU hot update = %+v applied=%v err=%v", result, applied, err)
	}
	if err := os.WriteFile(path, []byte("generation: { use_gpu: false }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := manager.Reload(); !changed || err != nil || applied {
		t.Fatalf("external disable: changed=%v applied=%v err=%v", changed, applied, err)
	}
	if _, err := Parse([]byte("generation: { use_gpu: invalid }\n")); err == nil {
		t.Fatal("invalid GPU boolean accepted")
	}
}

func TestManagerMigratesLegacyValuesIntoYAMLWithoutDiscardingUnknownNodes(t *testing.T) {
	manager, path := newManagerForTest(t, `# retained root comment
nightly:
  # legacy schedule comment
  cron_hour: 3
# obsolete template placeholder
drives: []
future_section:
  keep_me: true
`)
	start := "04:25"
	changed, err := manager.MigrateLegacyRuntimeSettings(LegacyRuntimeSettings{
		NightlyStartTime: &start,
	})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !changed {
		t.Fatal("legacy document was not migrated")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, retained := range []string{"# retained root comment", "future_section:", "keep_me: true"} {
		if !strings.Contains(text, retained) {
			t.Fatalf("migration discarded %q:\n%s", retained, text)
		}
	}
	if strings.Contains(text, "cron_hour:") || !strings.Contains(text, "start_time: 04:25") {
		t.Fatalf("nightly schema was not migrated:\n%s", text)
	}
	if !strings.Contains(text, "timezone: Asia/Shanghai") {
		t.Fatalf("nightly timezone was not made explicit:\n%s", text)
	}
	if strings.Contains(text, "drives:") || strings.Contains(text, "obsolete template placeholder") {
		t.Fatalf("retired empty drive placeholder remains:\n%s", text)
	}
	if strings.Contains(text, "builtin_pack_enabled") {
		t.Fatalf("retired tag setting remains:\n%s", text)
	}
	want := LiveSettings{PreviewEnabled: true, NightlyStartTime: "04:25", NightlyTimezone: "Asia/Shanghai", GenerationConcurrency: DefaultGenerationConcurrency}
	if got := manager.LiveSettings(); got != want {
		t.Fatalf("live settings = %#v, want %#v", got, want)
	}
}

func TestManagerYAMLValuesWinOverLegacySQLiteValues(t *testing.T) {
	manager, path := newManagerForTest(t, "nightly:\n  start_time: \"02:10\"\n  cron_hour: 7\ntags:\n  builtin_pack_enabled: true\n")
	start := "22:45"
	_, err := manager.MigrateLegacyRuntimeSettings(LegacyRuntimeSettings{
		NightlyStartTime: &start,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := LiveSettings{PreviewEnabled: true, NightlyStartTime: "02:10", NightlyTimezone: "Asia/Shanghai", GenerationConcurrency: DefaultGenerationConcurrency}
	if got := manager.LiveSettings(); got != want {
		t.Fatalf("live settings = %#v, want YAML %#v", got, want)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "cron_hour:") {
		t.Fatalf("retired fields remain:\n%s", data)
	}
}

func TestManagerRemovesNonEmptyLegacyDriveDefinitions(t *testing.T) {
	source := `nightly:
  start_time: "01:00"
  timezone: "Etc/UTC"
tags:
  builtin_pack_enabled: true
drives:
  - id: "operator-copy"
    kind: "webdav"
    params:
      base_url: "https://example.com/dav"
`
	manager, path := newManagerForTest(t, source)
	changed, err := manager.MigrateLegacyRuntimeSettings(LegacyRuntimeSettings{})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("non-empty legacy drive data was not removed")
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(written)
	for _, removed := range []string{"drives:", "operator-copy", "base_url", "example.com/dav"} {
		if strings.Contains(text, removed) {
			t.Fatalf("retired drive data %q remains:\n%s", removed, written)
		}
	}
}

func TestManagerRejectsStaleVersionWithoutChangingFile(t *testing.T) {
	manager, path := newManagerForTest(t, "nightly:\n  start_time: \"01:00\"\n")
	original, _ := os.ReadFile(path)
	_, err := manager.ReplaceYAML([]byte("nightly:\n  start_time: \"02:00\"\n"), "stale")
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("error = %v, want ErrVersionConflict", err)
	}
	written, _ := os.ReadFile(path)
	if string(written) != string(original) {
		t.Fatalf("stale write changed file:\n%s", written)
	}
}

func TestManagerReloadPublishesExternalValidChangeAndKeepsLastGoodOnError(t *testing.T) {
	manager, path := newManagerForTest(t, "nightly:\n  start_time: \"01:00\"\n")
	var applied []LiveSettings
	if err := manager.SetApply(func(settings LiveSettings) error {
		applied = append(applied, settings)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("nightly:\n  disabled: true\n  start_time: \"06:30\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	changed, err := manager.Reload()
	if err != nil || !changed {
		t.Fatalf("reload changed=%v err=%v", changed, err)
	}
	want := LiveSettings{PreviewEnabled: true, NightlyDisabled: true, NightlyStartTime: "06:30", NightlyTimezone: "Asia/Shanghai", GenerationConcurrency: DefaultGenerationConcurrency}
	if got := manager.LiveSettings(); got != want {
		t.Fatalf("settings = %#v, want %#v", got, want)
	}
	if err := os.WriteFile(path, []byte("nightly:\n  start_time: \"99:00\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if changed, err := manager.Reload(); err == nil || changed {
		t.Fatalf("invalid reload changed=%v err=%v", changed, err)
	}
	if got := manager.LiveSettings(); got != want {
		t.Fatalf("invalid reload replaced last good settings: %#v", got)
	}
	if len(applied) != 2 {
		t.Fatalf("apply callbacks = %d, want initial + valid reload", len(applied))
	}
}

func TestRestartRequiredComparisonIgnoresOnlyLivePaths(t *testing.T) {
	before := []byte("nightly:\n  start_time: \"01:00\"\nfuture:\n  value: one\n")
	liveOnly := []byte("nightly:\n  disabled: true\n  start_time: \"03:15\"\n  timezone: Asia/Shanghai\ntags:\n  builtin_pack_enabled: false\ngeneration:\n  concurrency: 3\nfuture:\n  value: one\n")
	if hasRestartRequiredChange(before, liveOnly) {
		t.Fatal("live-only values were reported as restart-required")
	}
	nonLive := []byte("nightly:\n  start_time: \"03:15\"\nfuture:\n  value: two\n")
	if !hasRestartRequiredChange(before, nonLive) {
		t.Fatal("unknown non-live value should require restart")
	}
}

func TestManagerRestoresYAMLAndLiveSnapshotWhenLiveApplyFails(t *testing.T) {
	manager, path := newManagerForTest(t, "nightly:\n  start_time: \"01:00\"\npreview:\n  enabled: true\n")
	var applied []LiveSettings
	if err := manager.SetApply(func(settings LiveSettings) error {
		applied = append(applied, settings)
		if !settings.PreviewEnabled {
			return errors.New("catalog unavailable")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	original, version, err := manager.ReadYAML()
	if err != nil {
		t.Fatal(err)
	}

	_, err = manager.ReplaceYAML([]byte("nightly:\n  start_time: \"01:00\"\npreview:\n  enabled: false\n"), version)
	if err == nil || !strings.Contains(err.Error(), "catalog unavailable") {
		t.Fatalf("replace error = %v, want live apply failure", err)
	}
	written, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(written) != string(original) {
		t.Fatalf("failed live apply changed YAML:\n%s", written)
	}
	if got := manager.LiveSettings(); !got.PreviewEnabled {
		t.Fatalf("failed live apply changed snapshot: %#v", got)
	}
	if len(applied) != 3 || !applied[0].PreviewEnabled || applied[1].PreviewEnabled || !applied[2].PreviewEnabled {
		t.Fatalf("apply sequence = %#v, want current, rejected candidate, rollback", applied)
	}
}

func TestManagerReplacesRetiredGenerationLimitsWithConcurrency(t *testing.T) {
	manager, path := newManagerForTest(t, `nightly:
  start_time: "01:00"
  timezone: Asia/Shanghai
tags:
  builtin_pack_enabled: true
preview:
  # keep ffmpeg settings
  ffmpeg_path: /usr/bin/ffmpeg
  concurrency: 5
generation:
  media_concurrency: 4
  thumbnail_concurrency: 2
  preview_concurrency: 3
  fingerprint_concurrency: 4
  future_option: keep
`)
	changed, err := manager.MigrateLegacyRuntimeSettings(LegacyRuntimeSettings{})
	if err != nil || !changed {
		t.Fatalf("migration changed=%v err=%v", changed, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if _, exists := document["preview"]["concurrency"]; exists {
		t.Fatal("retained obsolete per-drive limit")
	}
	for _, key := range []string{"media_concurrency", "thumbnail_concurrency", "preview_concurrency", "fingerprint_concurrency"} {
		if _, exists := document["generation"][key]; exists {
			t.Fatalf("retained obsolete limit %s", key)
		}
	}
	if document["generation"]["concurrency"] != 3 {
		t.Fatalf("generation concurrency = %v, want the former media budget", document["generation"]["concurrency"])
	}
	if document["generation"]["future_option"] != "keep" || !strings.Contains(string(data), "# keep ffmpeg settings") {
		t.Fatal("discarded unrelated config")
	}
	settings := manager.LiveSettings()
	if settings.GenerationConcurrency != 3 {
		t.Fatalf("live settings = %+v, want three generation concurrency", settings)
	}
	changed, err = manager.MigrateLegacyRuntimeSettings(LegacyRuntimeSettings{})
	if err != nil || changed {
		t.Fatalf("migration was not idempotent: changed=%v err=%v", changed, err)
	}
}
func TestPreviewEnabledDefaultsAndHotReload(t *testing.T) {
	for _, source := range []string{"", "preview: {}", "preview: {enabled: true}"} {
		cfg, err := Parse([]byte(source))
		if err != nil || !cfg.Preview.Enabled {
			t.Fatalf("source %q: cfg=%+v, err=%v", source, cfg, err)
		}
	}
	manager, path := newManagerForTest(t, "preview: {enabled: false}\n")
	var applied []bool
	if err := manager.SetApply(func(settings LiveSettings) error {
		applied = append(applied, settings.PreviewEnabled)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	result, err := manager.ReplaceYAML([]byte("preview: {enabled: true}\n"), "")
	if err != nil || result.RestartRequired || !result.Settings.PreviewEnabled {
		t.Fatalf("enable: %+v, %v", result, err)
	}
	if err := os.WriteFile(path, []byte("preview: {enabled: false}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if changed, err := manager.Reload(); err != nil || !changed || manager.LiveSettings().PreviewEnabled {
		t.Fatalf("reload: changed=%v, err=%v", changed, err)
	}
	if len(applied) != 3 || applied[0] || !applied[1] || applied[2] {
		t.Fatalf("applied = %v", applied)
	}
	if _, err := manager.ReplaceYAML([]byte("preview: {enabled: invalid}\n"), ""); err == nil {
		t.Fatal("invalid boolean accepted")
	}
	if manager.LiveSettings().PreviewEnabled {
		t.Fatal("invalid update changed the live switch")
	}
	reloaded, err := NewManager(path)
	if err != nil || reloaded.LiveSettings().PreviewEnabled {
		t.Fatalf("disabled switch was not persisted: %v", err)
	}
}
