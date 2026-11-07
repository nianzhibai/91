// Package telegramupload moves already-imported Telegram videos to cloud
// storage without changing their logical video identity.
package telegramupload

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/drives/telegramstorage"
	"github.com/nianzhibai/91/backend/internal/persistence"
	"github.com/nianzhibai/91/backend/internal/scopedproxy"
)

type localSource interface {
	drives.LocalFileProvider
	drives.Remover
}

type Config struct {
	Catalog           *catalog.Catalog
	Target            drives.Drive
	LocalDirectory    string
	TelegramDirectory string
	TargetDirectory   string
	UploadProxy       string
	OnMigrated        func(*catalog.Video)
}

// Run makes one cancellable sweep. Failed videos remain local and are retried
// by the next scheduled run. Only durable Telegram imports are selected.
func Run(ctx context.Context, cfg Config) (runErr error) {
	defer func() {
		if runErr != nil {
			driveID := ""
			if cfg.Target != nil {
				driveID = cfg.Target.ID()
			}
			logFailure(ctx, "", driveID, runErr)
		}
	}()
	if cfg.Catalog == nil || cfg.Target == nil {
		return failure("configuration", "TG 转存服务未配置", nil)
	}
	cleanupErr := Cleanup(ctx, cfg)
	if err := ctx.Err(); err != nil {
		return err
	}
	uploader, ok := cfg.Target.(drives.Uploader)
	if !ok {
		return errors.Join(cleanupErr, failure("configuration", "目标网盘不支持上传", drives.ErrNotSupported))
	}
	uploadCtx, err := scopedproxy.WithURL(ctx, cfg.UploadProxy)
	if err != nil {
		return errors.Join(cleanupErr, failure("proxy", "TG 转存代理地址无效，请检查 telegram.upload_proxy 配置", err))
	}
	type targetFolder struct {
		parent   string
		existing map[string][]drives.Entry
	}
	folders := make(map[string]targetFolder)
	after := ""
	failed, completed := 0, 0
	var failures []error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		videos, err := cfg.Catalog.ListLocalTelegramVideos(ctx, after, 100)
		if err != nil {
			return errors.Join(cleanupErr, failure("listing", "无法读取待转存的 TG 视频", err))
		}
		if len(videos) == 0 {
			break
		}
		for _, v := range videos {
			after = v.ID
			if err := ctx.Err(); err != nil {
				return err
			}
			transferCfg := cfg
			if v.DriveID == telegramstorage.DriveID {
				local, err := cfg.Catalog.TelegramLocalFile(ctx, v.FileID)
				if err != nil {
					return errors.Join(cleanupErr, failure("source", "无法读取 TG 视频目录", err))
				}
				if _, err := telegramstorage.RelativePath(local); err != nil {
					return errors.Join(cleanupErr, failure("source", "TG 视频目录无效", err))
				}
				transferCfg.TargetDirectory = path.Join(cfg.TargetDirectory, local.Directory)
			}
			folder, exists := folders[transferCfg.TargetDirectory]
			if !exists {
				parent, err := uploader.EnsureDir(uploadCtx, transferCfg.TargetDirectory)
				if err != nil || strings.TrimSpace(parent) == "" {
					return errors.Join(cleanupErr, failure("directory", "无法创建或读取 TG 转存目标目录", err))
				}
				entries, err := cfg.Target.List(uploadCtx, parent)
				if err != nil {
					return errors.Join(cleanupErr, failure("listing", "无法检查目标目录中已有的 TG 文件", err))
				}
				folder = targetFolder{parent: parent, existing: make(map[string][]drives.Entry)}
				for _, e := range entries {
					folder.existing[e.Name] = append(folder.existing[e.Name], e)
				}
				folders[transferCfg.TargetDirectory] = folder
			}
			if err := transferOne(uploadCtx, transferCfg, uploader, folder.parent, folder.existing, v); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				failed++
				failures = append(failures, err)
				logFailure(ctx, v.ID, cfg.Target.ID(), err)
				// Provider rate limits should stop this sweep rather than send
				// the rest of the library into the same failing service.
				if _, limited := drives.RateLimitRetryAfter(err); limited {
					return errors.Join(cleanupErr, failure("rate_limit", "TG 转存受到网盘限流，本轮已停止，下次定时任务重试", err))
				}
				continue
			}
			completed++
			log.Printf("[telegram-upload] video=%s stored on drive=%s", v.ID, cfg.Target.ID())
		}
	}
	log.Printf("[telegram-upload] finished: completed=%d failed=%d", completed, failed)
	if failed > 0 {
		return errors.Join(cleanupErr, failure("sweep", fmt.Sprintf("%d 个 TG 视频转存失败，请检查网盘连接和本地文件；下次定时任务会重试", failed), errors.Join(failures...)))
	}
	return cleanupErr
}

func transferOne(ctx context.Context, cfg Config, uploader drives.Uploader, parent string, existing map[string][]drives.Entry, v *catalog.Video) error {
	if v.FileID == "" || filepath.Base(v.FileID) != v.FileID || strings.ContainsAny(v.FileID, "\\\x00") {
		return failure("source", "本地视频路径无效", nil)
	}
	storage := sourceStorage(cfg, v.DriveID)
	localPath, err := storage.LocalPath(ctx, v.FileID)
	if err != nil {
		return failure("source", "无法读取本地视频位置", err)
	}
	info, err := os.Lstat(localPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() != v.Size {
		return failure("source", "本地视频不存在或大小不一致", err)
	}
	f, err := os.Open(localPath)
	if err != nil {
		return failure("source", "无法读取本地视频", err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Size() != info.Size() || !opened.ModTime().Equal(info.ModTime()) {
		return failure("source", "本地视频在读取期间发生变化", err)
	}
	identity, err := sourceFileIdentity(f)
	if err != nil {
		return failure("source", "无法记录本地视频身份", err)
	}
	cleanup := catalog.TelegramUploadCleanup{
		VideoID: v.ID, SourceDriveID: v.DriveID, SourceFileID: v.FileID,
		Size: info.Size(), ModTimeNS: info.ModTime().UnixNano(), FileIdentity: identity,
	}
	// Local filenames can be reused after migration. Include the immutable
	// video ID so distinct same-title imports never bind to each other's file.
	name := destinationName(v)
	fileID := ""
	if matches := existing[name]; len(matches) > 0 {
		if len(matches) != 1 || matches[0].IsDir || matches[0].Size != info.Size() {
			return failure("reconcile", "目标目录存在冲突的同名文件，请先处理后重试", nil)
		}
		fileID = matches[0].ID
		if fileID == "" {
			return failure("reconcile", "目标文件标识无效", nil)
		}
	} else {
		// Retain ownership of f: HTTP clients close request bodies that implement
		// io.Closer. A section reader preserves seek/parallel reads for drivers
		// without allowing them to close the local file before we do.
		fileID, err = uploader.Upload(ctx, parent, name, io.NewSectionReader(f, 0, info.Size()), info.Size())
		if err != nil {
			return failure("upload", "网盘上传失败", err)
		}
		if strings.TrimSpace(fileID) == "" {
			return failure("upload", "网盘上传未返回有效的文件结果", nil)
		}
		// Remember the remote write even if its verification/catalog commit
		// fails. A subsequent sweep rebuilds this index from the cloud drive.
		existing[name] = []drives.Entry{{ID: fileID, Name: name, Size: info.Size()}}
	}
	if err := f.Close(); err != nil {
		return failure("source", "无法关闭本地视频文件", err)
	}
	remote, err := cfg.Target.Stat(ctx, fileID)
	if err != nil || remote == nil || remote.IsDir || remote.Size != info.Size() {
		return failure("verify", "无法确认网盘文件完整，本地来源保持不变", err)
	}
	link, err := cfg.Target.StreamURL(ctx, fileID)
	if err != nil || link == nil || strings.TrimSpace(link.URL) == "" {
		return failure("playback", "无法取得网盘播放地址，本地来源保持不变", err)
	}
	if err := persistence.RLockContext(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		persistence.RUnlock()
		return err
	}
	err = cfg.Catalog.MigrateTelegramVideoToDrive(ctx, v.ID, catalog.VideoDriveMigration{
		SourceDriveID: v.DriveID, SourceFileID: v.FileID,
		DriveID: cfg.Target.ID(), FileID: fileID, ContentHash: remote.Hash,
		ParentID: parent, DirName: path.Base(cfg.TargetDirectory), FileName: name,
	}, cleanup)
	persistence.RUnlock()
	if err != nil {
		return failure("migration", "视频记录已变化或保存失败，本地文件已保留", err)
	}
	if cfg.OnMigrated != nil {
		if saved, err := cfg.Catalog.GetVideo(ctx, v.ID); err == nil {
			cfg.OnMigrated(saved)
		} else {
			applog.Warn(ctx, "TG 视频已转存，但读取生成任务所需的视频记录失败", err, applog.Fields{Component: "telegram-upload", VideoID: v.ID, Stage: "generation"})
		}
	}
	return cleanupSource(ctx, cfg, cleanup)
}

func destinationName(v *catalog.Video) string {
	name := v.FileName
	if name == "" {
		name = v.FileID
	}
	ext := filepath.Ext(name)
	title := strings.TrimSuffix(name, ext)
	id := sha256.Sum256([]byte(v.ID))
	suffix := fmt.Sprintf("-%x", id[:8])
	for len(title)+len(suffix)+len(ext) > 240 && len(title) > 0 {
		_, size := utf8.DecodeLastRuneInString(title)
		title = title[:len(title)-size]
	}
	return title + suffix + ext
}
