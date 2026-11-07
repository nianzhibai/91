package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
)

func TestTelegramDatabaseMigrationClearsOnlyAfterSuccessfulYAMLWrite(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "valid", false: "invalid"}[valid], func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte("# preserved\n{}\n"), 0600); err != nil {
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
			db, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			token := "123:migration_test"
			if !valid {
				token = "invalid"
			}
			_, err = db.Exec(`INSERT INTO telegram_settings(id, config, bot_token, api_id, api_hash, version) VALUES(1, ?, ?, 1234, ?, 'legacy')`, `{"enabled":true,"allowedUserIds":[42],"uploadDriveId":"cloud"}`, token, "0123456789abcdef0123456789abcdef")
			if err != nil {
				t.Fatal(err)
			}
			err = migrateTelegramConfig(ctx, cat, manager)
			stored, readErr := cat.GetTelegramSettings(ctx)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !valid {
				if err == nil || stored.Version == "" {
					t.Fatal("failed migration discarded legacy settings")
				}
				return
			}
			if err != nil || stored.Version != "" {
				t.Fatal("successful migration did not clear legacy settings", err)
			}
			cfg := manager.TelegramSettings()
			if cfg.BotToken != token || cfg.AllowedUserIDs[0] != 42 || cfg.UploadDriveID != "cloud" {
				t.Fatal("migration lost settings")
			}
			if err = migrateTelegramConfig(ctx, cat, manager); err != nil {
				t.Fatal("repeated migration failed", err)
			}
		})
	}
}
