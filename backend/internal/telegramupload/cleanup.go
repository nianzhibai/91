package telegramupload

import (
	"context"
	"database/sql"
	"errors"
	"os"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives/localupload"
	"github.com/nianzhibai/91/backend/internal/drives/telegramstorage"
	"github.com/nianzhibai/91/backend/internal/persistence"
)

func sourceStorage(cfg Config, driveID string) localSource {
	if driveID == telegramstorage.DriveID {
		return telegramstorage.New(cfg.Catalog, func() string { return cfg.TelegramDirectory })
	}
	return localupload.New(cfg.LocalDirectory)
}

// Cleanup retries only committed transfers. It needs no live target drive and
// can run at startup even when Telegram receiving or cloud uploads are disabled.
func Cleanup(ctx context.Context, cfg Config) error {
	if cfg.Catalog == nil {
		return failure("cleanup", "TG 转存清理服务未配置", nil)
	}
	var failures []error
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		jobs, err := cfg.Catalog.ListTelegramUploadCleanups(ctx, after, 100)
		if err != nil {
			wrapped := failure("cleanup", "无法读取 TG 转存待清理记录", err)
			logFailure(ctx, "", "", wrapped)
			return wrapped
		}
		if len(jobs) == 0 {
			break
		}
		for _, job := range jobs {
			after = job.VideoID
			if err := cleanupSource(ctx, cfg, job); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				logFailure(ctx, job.VideoID, job.SourceDriveID, err)
				failures = append(failures, err)
			}
		}
	}
	if len(failures) > 0 {
		return failure("cleanup", "TG 转存本地副本清理失败，后续维护会重试", errors.Join(failures...))
	}
	return nil
}

func cleanupSource(ctx context.Context, cfg Config, job catalog.TelegramUploadCleanup) error {
	if err := persistence.RLockContext(ctx); err != nil {
		return err
	}
	defer persistence.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	err := removeSource(ctx, cfg, job)
	if err == nil {
		if completeErr := cfg.Catalog.CompleteTelegramUploadCleanup(ctx, job.VideoID); completeErr != nil {
			err = failure("cleanup", "本地副本已删除，但无法完成清理记录，后续维护会重试", completeErr)
		}
	}
	if err != nil {
		message := diagnosticText(err)
		if saveErr := cfg.Catalog.FailTelegramUploadCleanup(ctx, job.VideoID, applog.Redact(message)); saveErr != nil {
			return failure("cleanup", "无法记录 TG 本地副本清理失败", errors.Join(err, saveErr))
		}
		return err
	}
	return nil
}

func removeSource(ctx context.Context, cfg Config, job catalog.TelegramUploadCleanup) error {
	referenced, err := cfg.Catalog.TelegramUploadSourceReferenced(ctx, job.SourceDriveID, job.SourceFileID)
	if err != nil {
		return failure("cleanup", "无法确认本地副本是否仍被视频引用", err)
	}
	if referenced {
		return failure("cleanup", "本地文件已被其他视频使用，已保留供检查", nil)
	}
	storage := sourceStorage(cfg, job.SourceDriveID)
	path, err := storage.LocalPath(ctx, job.SourceFileID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) && job.SourceDriveID == telegramstorage.DriveID {
			return nil
		}
		return failure("cleanup", "无法读取待清理本地副本位置", err)
	}
	info, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return failure("cleanup", "无法检查待清理本地副本", err)
	}
	if err == nil {
		if !info.Mode().IsRegular() || info.Size() != job.Size || info.ModTime().UnixNano() != job.ModTimeNS {
			return failure("cleanup", "本地副本发生变化，已保留供检查", nil)
		}
		file, err := os.Open(path)
		if err != nil {
			return failure("cleanup", "无法确认本地副本身份", err)
		}
		identity, identityErr := sourceFileIdentity(file)
		opened, statErr := file.Stat()
		closeErr := file.Close()
		if err := errors.Join(identityErr, statErr, closeErr); err != nil {
			return failure("cleanup", "无法确认本地副本身份", err)
		}
		if !os.SameFile(info, opened) || identity != job.FileIdentity || opened.Size() != job.Size || opened.ModTime().UnixNano() != job.ModTimeNS {
			return failure("cleanup", "本地副本已被替换，已保留供检查", nil)
		}
	}
	if err := storage.Remove(ctx, job.SourceFileID); err != nil {
		return failure("cleanup", "视频已转存，但无法删除本地副本，后续维护会重试", err)
	}
	return nil
}
