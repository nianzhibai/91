package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nianzhibai/91/backend"
	"gopkg.in/yaml.v3"
)

func TestDataDirectoryControlsStorageAndLogging(t *testing.T) {
	for _, root := range []string{"./data", "./custom data", filepath.Join(t.TempDir(), "moved data")} {
		t.Run(root, func(t *testing.T) {
			data, err := yaml.Marshal(map[string]any{"storage": map[string]string{"data_dir": root}})
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Storage.DataDir != root || cfg.Storage.DBPath != filepath.Join(root, "91.db") ||
				cfg.Storage.LocalPreviewDir != filepath.Join(root, "previews") || cfg.Logging.Directory != filepath.Join(root, "logs") {
				t.Fatalf("paths do not follow data directory: %+v, %+v", cfg.Storage, cfg.Logging)
			}
			baseDir := t.TempDir()
			runtime, err := ResolveStoragePaths(cfg.Storage, baseDir)
			if err != nil {
				t.Fatal(err)
			}
			wantRoot := root
			if !filepath.IsAbs(wantRoot) {
				wantRoot = filepath.Join(baseDir, root)
			}
			if runtime.DataDir != wantRoot || runtime.DBPath != filepath.Join(wantRoot, "91.db") || runtime.LocalPreviewDir != filepath.Join(wantRoot, "previews") {
				t.Fatalf("runtime paths = %+v", runtime)
			}
			logging, err := ResolveLoggingPaths(cfg.Logging, baseDir)
			if err != nil || logging.Directory != filepath.Join(wantRoot, "logs") {
				t.Fatalf("runtime logging = %+v, err=%v", logging, err)
			}
			encoded, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			for _, old := range []string{"db_path:", "local_preview_dir:", "\n    directory:"} {
				if strings.Contains(string(encoded), old) {
					t.Fatalf("derived path leaked into YAML: %s", encoded)
				}
			}
			roundTrip, err := Parse(encoded)
			if err != nil || cfg.Storage != roundTrip.Storage || !reflect.DeepEqual(cfg.Logging, roundTrip.Logging) {
				t.Fatalf("config changed after serialization: err=%v", err)
			}
		})
	}
	defaults, err := Parse(backend.ConfigTemplate())
	if err != nil || defaults.Storage.DataDir != "./data" {
		t.Fatalf("template data directory = %+v, err=%v", defaults, err)
	}
}

func TestTemplateMigrationConvertsUnifiedLegacyDataDirectory(t *testing.T) {
	for _, root := range []string{"./data", filepath.Join(t.TempDir(), "existing data")} {
		t.Run(root, func(t *testing.T) {
			data, err := yaml.Marshal(map[string]any{
				"storage": map[string]string{"db_path": filepath.Join(root, "video-site.db"), "local_preview_dir": filepath.Join(root, "previews")},
				"logging": map[string]any{"directory": filepath.Join(root, "logs"), "max_file_size_mb": 10},
			})
			if err != nil {
				t.Fatal(err)
			}
			manager, _ := newManagerForTest(t, string(data))
			before, err := Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			if changed, err := manager.SyncTemplate(); err != nil || !changed {
				t.Fatalf("migrate legacy config: changed=%v err=%v", changed, err)
			}
			written, _, err := manager.ReadYAML()
			if err != nil {
				t.Fatal(err)
			}
			assertTemplateStructure(t, written)
			after, err := Parse(written)
			if err != nil || before.Storage != after.Storage || before.Logging.Directory != after.Logging.Directory || after.Logging.MaxFileSizeMB != 10 {
				t.Fatalf("migration changed effective paths: before=%+v after=%+v err=%v", before, after, err)
			}
			if changed, err := manager.SyncTemplate(); err != nil || changed {
				t.Fatalf("migration not idempotent: changed=%v err=%v", changed, err)
			}
		})
	}
}

func TestDataDirectoryRejectsSplitOrConflictingLegacySettings(t *testing.T) {
	for _, source := range []string{
		"storage: {db_path: /nzb2/video-site.db, local_preview_dir: /nzb1/previews}\nlogging: {directory: /nzb1/logs}\n",
		"storage: {db_path: /nzb/custom.db, local_preview_dir: /nzb/previews}\nlogging: {directory: /nzb/logs}\n",
		"storage: {data_dir: /nzb, db_path: /nzb/video-site.db}\n",
		"storage: {data_dir: /nzb, local_preview_dir: /elsewhere/previews}\n",
		"storage: {data_dir: /nzb}\nlogging: {directory: /elsewhere/logs}\n",
		"logging: {directory: /elsewhere/logs}\n",
	} {
		t.Run(source, func(t *testing.T) {
			if _, err := Parse([]byte(source)); err == nil || !strings.Contains(err.Error(), "storage.data_dir") {
				t.Fatalf("accepted legacy paths or missing guidance: %v", err)
			}
			manager, path := newManagerForTest(t, "{}\n")
			if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			if changed, err := manager.SyncTemplate(); err == nil || changed {
				t.Fatalf("invalid legacy layout was migrated: changed=%v err=%v", changed, err)
			}
			written, err := os.ReadFile(path)
			if err != nil || string(written) != source {
				t.Fatalf("rejected legacy config was overwritten: %v", err)
			}
		})
	}
}

func TestChangingDataDirectoryRequiresRestart(t *testing.T) {
	for _, source := range []string{"storage: {data_dir: ./moved}\n", "storage: {data_dir: ./data, db_dir: ./ssd}\n"} {
		manager, _ := newManagerForTest(t, "storage: {data_dir: ./data}\n")
		result, err := manager.ReplaceYAML([]byte(source), "")
		if err != nil || !result.RestartRequired {
			t.Fatalf("directory change: result=%+v err=%v", result, err)
		}
	}
}

func TestDatabaseDirectoryOverridesOnlyDatabaseLocation(t *testing.T) {
	baseDir := t.TempDir()
	for _, dbDir := range []string{"", "  ", "./ssd", "../database", filepath.Join(t.TempDir(), "fast database")} {
		t.Run(dbDir, func(t *testing.T) {
			data, err := yaml.Marshal(map[string]any{"storage": map[string]string{"data_dir": "./media", "db_dir": dbDir}})
			if err != nil {
				t.Fatal(err)
			}
			manager, _ := newManagerForTest(t, string(data))
			if _, err := manager.SyncTemplate(); err != nil {
				t.Fatal(err)
			}
			written, _, err := manager.ReadYAML()
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := Parse(written)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Storage.DBDir != strings.TrimSpace(dbDir) {
				t.Fatalf("template migration lost database override: %+v", cfg.Storage)
			}
			wantDBDir := strings.TrimSpace(dbDir)
			if wantDBDir == "" {
				wantDBDir = "./media"
			}
			if cfg.Storage.DBPath != filepath.Join(wantDBDir, "91.db") ||
				cfg.Storage.LocalPreviewDir != filepath.Join("media", "previews") || cfg.Logging.Directory != filepath.Join("media", "logs") {
				t.Fatalf("database override changed unrelated paths: storage=%+v logging=%+v", cfg.Storage, cfg.Logging)
			}
			runtime, err := ResolveStoragePaths(cfg.Storage, baseDir)
			if err != nil {
				t.Fatal(err)
			}
			if !filepath.IsAbs(wantDBDir) {
				wantDBDir = filepath.Join(baseDir, wantDBDir)
			}
			if runtime.DBDir != wantDBDir || runtime.DBPath != filepath.Join(wantDBDir, "91.db") ||
				runtime.DataDir != filepath.Join(baseDir, "media") || runtime.LocalPreviewDir != filepath.Join(baseDir, "media", "previews") {
				t.Fatalf("runtime database override = %+v", runtime)
			}
		})
	}
}
