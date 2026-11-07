package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/telegramupload"
)

func (a *App) runTelegramUploadMigration(ctx context.Context) error {
	if a == nil || a.cat == nil {
		return nil
	}
	cfg := a.configManager.TelegramSettings()
	if cfg.UploadDriveID == "" {
		return a.cleanupTelegramUploadSources(ctx)
	}
	var migrated []*catalog.Video
	err := func() error {
		// Target drive edits/removal participate in the same admission and
		// cancellation mechanism as scanning and crawler uploads.
		taskCtx, done := a.registerDriveTaskContextWaiting(ctx, cfg.UploadDriveID, 0)
		defer done()
		if err := taskCtx.Err(); err != nil {
			return err
		}
		row, err := a.activeDriveConfig(taskCtx, cfg.UploadDriveID)
		if err != nil {
			return errors.Join(fmt.Errorf("读取 TG 转存目标失败: %w", err), a.cleanupTelegramUploadSources(taskCtx))
		}
		if row == nil || !drives.CapabilitiesForKind(row.Kind).Upload {
			return errors.Join(errors.New("TG 转存目标不存在或不支持上传，请检查 Telegram 设置"), a.cleanupTelegramUploadSources(taskCtx))
		}
		target, ok := a.registry.Get(cfg.UploadDriveID)
		if !ok {
			return errors.Join(errors.New("TG 转存目标网盘尚未连接"), a.cleanupTelegramUploadSources(taskCtx))
		}
		return telegramupload.Run(taskCtx, telegramupload.Config{
			Catalog: a.cat, Target: target, LocalDirectory: a.localUploadDir(),
			TelegramDirectory: cfg.LocalFilesRoot,
			TargetDirectory:   cfg.UploadDirectory,
			UploadProxy:       cfg.UploadProxy,
			OnMigrated:        func(v *catalog.Video) { migrated = append(migrated, v) },
		})
	}()
	// Release the upload's drive admission before admitting generation work;
	// an intervening drive edit must not deadlock behind its own upload task.
	for _, v := range migrated {
		a.enqueueUploadedVideo(ctx, v)
	}
	return err
}

func (a *App) cleanupTelegramUploadSources(ctx context.Context) error {
	if a == nil || a.cat == nil {
		return nil
	}
	return telegramupload.Cleanup(ctx, telegramupload.Config{
		Catalog: a.cat, LocalDirectory: a.localUploadDir(),
		TelegramDirectory: a.configManager.TelegramSettings().LocalFilesRoot,
	})
}
