package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/config"
	"gopkg.in/yaml.v3"
)

func TestStartupConfigMigrationImportsLegacyValuesBeforeAddingDefaults(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "explicit_yaml"}[explicit], func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			source := "nightly: {cron_hour: 6}\ngeneration: {preview_concurrency: 4}\n"
			if explicit {
				source += "telegram: {enabled: false, bot_token: '', allowed_user_ids: []}\ntags: {builtin_pack_enabled: true}\n"
			}
			if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			manager, err := config.NewManager(path)
			if err != nil {
				t.Fatal(err)
			}
			dbPath := filepath.Join(dir, "catalog.db")
			cat, err := catalog.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer cat.Close()
			if err := cat.SetSetting(ctx, legacyNightlyStartTimeSetting, "03:25"); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			_, err = db.Exec(`INSERT INTO telegram_settings(id, config, bot_token, api_id, api_hash, version) VALUES(1, ?, ?, 1234, ?, 'legacy')`, `{"enabled":true,"allowedUserIds":[42],"uploadDriveId":"cloud"}`, "123:migration_test", "0123456789abcdef0123456789abcdef")
			if err != nil {
				t.Fatal(err)
			}
			if err := migrateApplicationConfig(ctx, cat, manager); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Nightly.StartTime != "03:25" || cfg.Generation.PreviewConcurrency != 4 {
				t.Fatal("startup defaults overwrote migrated or explicit runtime settings")
			}
			if explicit {
				if cfg.Telegram.Enabled || cfg.Telegram.BotToken != "" || len(cfg.Telegram.AllowedUserIDs) != 0 {
					t.Fatal("startup migration overwrote explicit false/empty YAML values")
				}
			} else if !cfg.Telegram.Enabled || cfg.Telegram.BotToken != "123:migration_test" || len(cfg.Telegram.AllowedUserIDs) != 1 || cfg.Telegram.AllowedUserIDs[0] != 42 {
				t.Fatal("startup defaults blocked legacy Telegram import")
			}
			if cfg.Telegram.UploadDriveID != "cloud" {
				t.Fatal("startup migration lost Telegram settings")
			}
			stored, err := cat.GetTelegramSettings(ctx)
			if err != nil || stored.Version != "" {
				t.Fatalf("legacy Telegram settings were not cleared: %v", err)
			}
			for _, key := range []string{legacyNightlyStartTimeSetting, "tags.builtin_pack_enabled"} {
				if value, err := cat.GetSetting(ctx, key, "missing"); err != nil || value != "missing" {
					t.Fatalf("legacy runtime setting %s was not cleared: %v", key, err)
				}
			}
			data, _, _ := manager.ReadYAML()
			var document map[string]map[string]any
			if err := yaml.Unmarshal(data, &document); err != nil {
				t.Fatal(err)
			}
			if _, exists := document["telegram"]["upload_proxy"]; !exists {
				t.Fatal("startup did not fill the new Telegram field")
			}
			if _, exists := document["nightly"]["cron_hour"]; exists {
				t.Fatal("startup retained the legacy schedule field")
			}
			if err := migrateApplicationConfig(ctx, cat, manager); err != nil {
				t.Fatal(err)
			}
			again, _, _ := manager.ReadYAML()
			if !bytes.Equal(data, again) {
				t.Fatal("repeated startup rewrote the configuration")
			}
		})
	}
}
