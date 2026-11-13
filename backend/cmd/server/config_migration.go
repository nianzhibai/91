package main

import (
	"context"
	"fmt"
	"log"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
)

// Import legacy values before applying the template, while no runtime consumer
// or config watcher is running. Explicit YAML values always take precedence.
func migrateApplicationConfig(ctx context.Context, cat *catalog.Catalog, manager *config.Manager) error {
	if _, err := migrateLegacyAdmin(ctx, cat, manager); err != nil {
		return fmt.Errorf("migrate administrator configuration: %w", err)
	}
	legacy, err := loadLegacyRuntimeSettings(ctx, cat)
	if err != nil {
		return fmt.Errorf("load legacy runtime settings: %w", err)
	}
	if _, err := manager.MigrateLegacyRuntimeSettings(legacy); err != nil {
		return fmt.Errorf("migrate runtime settings: %w", err)
	}
	if err := cat.DeleteSettings(ctx, legacyNightlyStartTimeSetting); err != nil {
		return fmt.Errorf("remove migrated SQLite configuration: %w", err)
	}
	if err := migrateTelegramConfig(ctx, cat, manager); err != nil {
		return fmt.Errorf("migrate Telegram settings: %w", err)
	}
	data, _, err := manager.ReadYAML()
	if err != nil {
		return err
	}
	legacyTelegram, err := config.IsLegacyBotAPITelegram(data)
	if err != nil {
		return err
	}
	changed, err := manager.SyncTemplate()
	if err != nil {
		return fmt.Errorf("update configuration from template: %w", err)
	}
	if changed {
		log.Printf("[config] updated config.yaml from the release template, preserving existing settings")
	}
	if legacyTelegram {
		log.Printf("[config] Telegram now connects without the Bot API service and was disabled; enter the API ID and API Hash in the admin panel, then enable it again")
	}
	return nil
}
