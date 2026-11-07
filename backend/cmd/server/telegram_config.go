package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
)

func migrateTelegramConfig(ctx context.Context, cat *catalog.Catalog, manager *config.Manager) error {
	stored, err := cat.GetTelegramSettings(ctx)
	if err != nil || stored.Version == "" {
		return err
	}
	var legacy config.Telegram
	if stored.Config != "" {
		if err = json.Unmarshal([]byte(stored.Config), &legacy); err != nil {
			return errors.New("无法解析待迁移的 Telegram 配置")
		}
	}
	legacy.BotToken = stored.BotToken
	legacy.APIID, legacy.APIHash = int(stored.APIID), stored.APIHash
	if err = manager.MigrateTelegramSettings(legacy); err != nil {
		return err
	}
	return cat.DeleteTelegramSettings(ctx)
}
