package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/video-site/backend"
)

func TestServerListenDefaultsMatchDeploymentTemplate(t *testing.T) {
	for _, data := range [][]byte{[]byte("{}"), []byte("server: {listen: ''}"), backend.ConfigTemplate()} {
		cfg, err := Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server.Listen != "0.0.0.0:9191" {
			t.Fatalf("default listener = %q", cfg.Server.Listen)
		}
	}
	for _, address := range []string{"127.0.0.1:9192", "0.0.0.0:9999", ":8080"} {
		cfg, err := Parse([]byte("server: {listen: '" + address + "'}"))
		if err != nil || cfg.Server.Listen != address {
			t.Fatalf("explicit listener %q was not preserved: %+v %v", address, cfg, err)
		}
	}
}

func TestLoadDefaultScannerVideoExtensionsIncludeSTRM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if !hasVideoExtension(cfg.Scanner.VideoExtensions, ".strm") {
		t.Fatalf("video extensions = %#v, want .strm", cfg.Scanner.VideoExtensions)
	}
}

func TestExampleConfigOmitsDatabaseManagedDriveDefinitions(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	text := string(data)
	for _, obsolete := range []string{"drives:", "# 盘列表", "my-onedrive", "my-webdav"} {
		if strings.Contains(text, obsolete) {
			t.Fatalf("example config still exposes database-managed drive definition %q", obsolete)
		}
	}
}

func TestResolveStoragePathsUsesStartupDirectoryWithoutMutatingConfig(t *testing.T) {
	baseDir := t.TempDir()
	storage := Storage{
		DataDir: "./data",
	}

	resolved, err := ResolveStoragePaths(storage, baseDir)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.DBPath != filepath.Join(baseDir, "data", "video-site.db") {
		t.Fatalf("resolved db path = %q", resolved.DBPath)
	}
	if resolved.LocalPreviewDir != filepath.Join(baseDir, "data", "previews") {
		t.Fatalf("resolved preview path = %q", resolved.LocalPreviewDir)
	}
	if storage.DataDir != "./data" || storage.DBPath != "" || storage.LocalPreviewDir != "" {
		t.Fatalf("source storage config was mutated: %+v", storage)
	}
}

func TestLoggingDefaultsAndCanBeDisabled(t *testing.T) {
	defaults, err := Parse([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if !defaults.Logging.IsFileEnabled() || defaults.Logging.Directory != filepath.Join("data", "logs") ||
		defaults.Logging.MaxFileSizeMB != 5 || defaults.Logging.MaxTotalSizeMB != 30 {
		t.Fatalf("logging defaults = %+v", defaults.Logging)
	}

	disabled, err := Parse([]byte("logging:\n  file_enabled: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Logging.IsFileEnabled() {
		t.Fatal("file logging should be disabled explicitly")
	}
}

func TestResolveLoggingPathsUsesStartupDirectoryWithoutMutatingConfig(t *testing.T) {
	baseDir := t.TempDir()
	logging := Logging{Directory: "./data/logs", MaxFileSizeMB: 10, MaxTotalSizeMB: 200}

	resolved, err := ResolveLoggingPaths(logging, baseDir)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Directory != filepath.Join(baseDir, "data", "logs") {
		t.Fatalf("resolved logging path = %q", resolved.Directory)
	}
	if logging.Directory != "./data/logs" {
		t.Fatalf("source logging config was mutated: %+v", logging)
	}
}

func TestParseRejectsInvalidLoggingSizeLimits(t *testing.T) {
	tests := []string{
		"logging:\n  max_file_size_mb: -1\n",
		"logging:\n  max_file_size_mb: 20\n  max_total_size_mb: 10\n",
		"logging:\n  max_total_size_mb: 10241\n",
	}
	for _, data := range tests {
		if _, err := Parse([]byte(data)); err == nil {
			t.Fatalf("Parse(%q) succeeded, want validation error", data)
		}
	}
}

func TestLoadLegacyDefaultScannerVideoExtensionsIncludeSTRM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
scanner:
  video_extensions: [".mp4", ".mkv", ".mov", ".webm", ".avi"]
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if !hasVideoExtension(cfg.Scanner.VideoExtensions, ".strm") {
		t.Fatalf("video extensions = %#v, want .strm appended for legacy default list", cfg.Scanner.VideoExtensions)
	}
}

func TestLoadCustomScannerVideoExtensionsArePreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
scanner:
  video_extensions: [".mp4"]
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if len(cfg.Scanner.VideoExtensions) != 1 || cfg.Scanner.VideoExtensions[0] != ".mp4" {
		t.Fatalf("video extensions = %#v, want custom list preserved", cfg.Scanner.VideoExtensions)
	}
}

func TestGlobalPreviewConcurrency(t *testing.T) {
	cfg, err := Parse([]byte(`
generation:
  preview_concurrency: 3
`))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if got := cfg.Generation.PreviewConcurrency; got != 3 {
		t.Fatalf("preview concurrency = %d, want 3", got)
	}
	maximum, err := Parse([]byte("generation:\n  preview_concurrency: 5\n"))
	if err != nil {
		t.Fatalf("parse maximum preview concurrency: %v", err)
	}
	if got := maximum.Generation.PreviewConcurrency; got != MaxGenerationConcurrency {
		t.Fatalf("maximum preview concurrency = %d, want %d", got, MaxGenerationConcurrency)
	}

	defaults, err := Parse([]byte(`{}`))
	if err != nil {
		t.Fatalf("parse default config: %v", err)
	}
	if got := defaults.Generation.PreviewConcurrency; got != DefaultGenerationConcurrency {
		t.Fatalf("default preview concurrency = %d, want %d", got, DefaultGenerationConcurrency)
	}
}

func TestGlobalPreviewConcurrencyRejectsInvalidValue(t *testing.T) {
	_, err := Parse([]byte(`
generation:
  preview_concurrency: 6
`))
	if err == nil || !strings.Contains(err.Error(), "must be between 1 and 5") {
		t.Fatalf("parse error = %v, want concurrency validation error", err)
	}
}

func TestLoadDefaultNightlyCronHour(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Nightly.CronHour != 1 {
		t.Fatalf("nightly cron hour = %d, want 1", cfg.Nightly.CronHour)
	}
	if cfg.Nightly.StartTime != DefaultNightlyStartTime {
		t.Fatalf("nightly start time = %q, want %q", cfg.Nightly.StartTime, DefaultNightlyStartTime)
	}
	if cfg.Nightly.Timezone != DefaultNightlyTimezone {
		t.Fatalf("nightly timezone = %q, want %q", cfg.Nightly.Timezone, DefaultNightlyTimezone)
	}
	if cfg.Nightly.Disabled != DefaultNightlyDisabled {
		t.Fatalf("nightly disabled = %v, want %v", cfg.Nightly.Disabled, DefaultNightlyDisabled)
	}
}

func TestParseNightlyStartTime(t *testing.T) {
	cfg, err := Parse([]byte("nightly:\n  start_time: \"00:15\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Nightly.StartTime != "00:15" {
		t.Fatalf("start time = %q", cfg.Nightly.StartTime)
	}
	if cfg.Nightly.Timezone != DefaultNightlyTimezone {
		t.Fatalf("default timezone = %q", cfg.Nightly.Timezone)
	}
}

func TestParseNightlyTimezone(t *testing.T) {
	cfg, err := Parse([]byte("nightly:\n  timezone: Asia/Shanghai\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Nightly.Timezone != "Asia/Shanghai" {
		t.Fatalf("timezone = %q", cfg.Nightly.Timezone)
	}
}

func TestParseNightlyDisabled(t *testing.T) {
	disabled, err := Parse([]byte("nightly:\n  disabled: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !disabled.Nightly.Disabled {
		t.Fatal("explicitly disabled nightly schedule was enabled")
	}

	enabled, err := Parse([]byte("nightly:\n  disabled: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if enabled.Nightly.Disabled {
		t.Fatal("explicitly enabled nightly schedule was disabled")
	}

	if _, err := Parse([]byte("nightly:\n  disabled: not-a-boolean\n")); err == nil {
		t.Fatal("non-boolean nightly.disabled was accepted")
	}
}

func TestParseRejectsInvalidNightlyStartTime(t *testing.T) {
	_, err := Parse([]byte("nightly:\n  start_time: \"24:00\"\n"))
	if !errors.Is(err, ErrInvalidNightlyStartTime) {
		t.Fatalf("error = %v, want ErrInvalidNightlyStartTime", err)
	}
}

func TestParseRejectsInvalidNightlyTimezone(t *testing.T) {
	for _, timezone := range []string{"Local", "Mars/Olympus"} {
		t.Run(timezone, func(t *testing.T) {
			_, err := Parse([]byte("nightly:\n  timezone: \"" + timezone + "\"\n"))
			if !errors.Is(err, ErrInvalidNightlyTimezone) {
				t.Fatalf("error = %v, want ErrInvalidNightlyTimezone", err)
			}
		})
	}
}

func TestLoadForcedRelayDefaultsEnabledAndCanBeDisabled(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		if !cfg.Proxy.AllowsForcedRelay() {
			t.Fatal("forced relay should remain enabled by default")
		}
	})

	t.Run("disabled", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("proxy:\n  allow_forced_relay: false\n"), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		if cfg.Proxy.AllowsForcedRelay() {
			t.Fatal("forced relay should honor an explicit false value")
		}
	})
}

func TestLoadInvalidNightlyCronHourFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
nightly:
  cron_hour: 25
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Nightly.CronHour != 1 {
		t.Fatalf("nightly cron hour = %d, want fallback 1", cfg.Nightly.CronHour)
	}
}

func TestLoadRemoteUploadDefaultsAndOverrides(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		if cfg.RemoteUpload.DiskReserveBytes != 1073741824 {
			t.Fatalf("disk reserve = %d", cfg.RemoteUpload.DiskReserveBytes)
		}
		if cfg.RemoteUpload.IdleTimeoutSeconds != 120 {
			t.Fatalf("idle timeout = %d", cfg.RemoteUpload.IdleTimeoutSeconds)
		}
	})

	t.Run("overrides", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(`
remote_upload:
  disk_reserve_bytes: 2147483648
  idle_timeout_seconds: 240
`), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		if cfg.RemoteUpload.DiskReserveBytes != 2147483648 ||
			cfg.RemoteUpload.IdleTimeoutSeconds != 240 {
			t.Fatalf("remote upload config = %#v", cfg.RemoteUpload)
		}
	})
}

func hasVideoExtension(exts []string, want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	for _, ext := range exts {
		if strings.ToLower(strings.TrimSpace(ext)) == want {
			return true
		}
	}
	return false
}

func TestGenerationConcurrencyDefaultsAndValidation(t *testing.T) {
	cfg, err := Parse([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Generation.ThumbnailConcurrency != 1 || cfg.Generation.PreviewConcurrency != 1 || cfg.Generation.FingerprintConcurrency != 1 || cfg.Preview.FFmpegThreads != 1 {
		t.Fatalf("unsafe generation defaults: generation=%+v threads=%d", cfg.Generation, cfg.Preview.FFmpegThreads)
	}
	for _, key := range []string{"thumbnail_concurrency", "preview_concurrency", "fingerprint_concurrency"} {
		for _, value := range []string{"-1", "6", "abc"} {
			if _, err := Parse([]byte("generation:\n  " + key + ": " + value + "\n")); err == nil {
				t.Fatalf("accepted %s=%s", key, value)
			}
		}
		if _, err := Parse([]byte("generation:\n  " + key + ": 5\n")); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{"-1", "17"} {
		if _, err := Parse([]byte("preview:\n  ffmpeg_threads: " + value + "\n")); err == nil {
			t.Fatalf("accepted threads=%s", value)
		}
	}
}

func TestParseIgnoresRetiredTagConfiguration(t *testing.T) {
	for _, source := range []string{"{}", "tags: {builtin_pack_enabled: false}", "tags: {builtin_pack_enabled: legacy-value}", "tags: false"} {
		if _, err := Parse([]byte(source)); err != nil {
			t.Fatalf("retired configuration blocked parsing: %v", err)
		}
	}
}
