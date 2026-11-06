package telegram

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives/telegramstorage"
	"github.com/video-site/backend/internal/mediaimport"
	"github.com/video-site/backend/internal/persistence"
)

// Check both new messages and persisted jobs before starting a download.
func validateVideoSize(size, limit int64) error {
	if size <= 0 {
		return errors.New("无法确认视频大小，已拒绝下载，请重新发送视频")
	}
	if size > limit {
		return errors.New("视频超过配置的单文件大小限制")
	}
	return nil
}

func mediaGroupStorage(source catalog.TelegramSource) (directory, key string) {
	if source.MediaGroupID == "" {
		return "", ""
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%s", source.BotID, source.SenderID, source.MediaGroupID)))
	// Directory labels keep the group's first nonempty line; video titles use
	// the full caption independently of the stable storage directory name.
	directoryCaption := ""
	for _, line := range strings.Split(source.MediaGroupTitle, "\n") {
		if strings.TrimSpace(line) != "" {
			directoryCaption = strings.TrimSpace(line)
			break
		}
	}
	title := videoTitle(directoryCaption, "", source.MediaGroupDate)
	if strings.TrimSpace(source.MediaGroupTitle) == "" {
		title = time.Unix(source.MediaGroupDate, 0).UTC().Format("2006-01-02_15-04-05")
	}
	return fmt.Sprintf("%s-%x", title, digest[:8]), fmt.Sprintf("%x", digest[:])
}

func (s *Service) Fetch(ctx context.Context, j *catalog.RemoteUploadJob, progress func(string, int64, int64) error) (mediaimport.SourceFile, error) {
	// A configuration change cancels downloads using the old bot identity.
	if s.runCtx != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		stop := context.AfterFunc(s.runCtx, cancel)
		defer stop()
		defer cancel()
	}
	cfg := s.settings()
	var out mediaimport.SourceFile
	source, err := catalog.DecodeTelegramSource(j)
	if err != nil {
		return out, &mediaimport.SourceError{Message: "TG 文件信息无效"}
	}
	if source.BotID != s.BotID() {
		return out, &mediaimport.SourceError{Message: "请连接提交任务的机器人", RetryAfter: time.Minute, WaitForAvailability: true}
	}
	if !s.Allowed(source.SenderID) {
		return out, &mediaimport.SourceError{Message: "发送者已不在允许用户列表中"}
	}
	size := source.Size
	if err := validateVideoSize(size, cfg.MaxFileSizeBytes); err != nil {
		return out, &mediaimport.SourceError{Message: err.Error()}
	}
	// Persist the destination before moving bytes, including across restarts.
	if err = persistence.RLockContext(ctx); err != nil {
		return out, err
	}
	local, localErr := s.cat.TelegramLocalFile(ctx, j.ID+".media")
	if errors.Is(localErr, sql.ErrNoRows) {
		directory, key := mediaGroupStorage(source)
		local, err = s.cat.ReserveTelegramLocalFile(ctx, catalog.TelegramLocalFile{FileID: j.ID + ".media", JobID: j.ID, Directory: directory, GroupKey: key})
	} else {
		err = localErr
	}
	var destination string
	if err == nil {
		destination, err = telegramstorage.Prepare(cfg.LocalFilesRoot, local)
	}
	persistence.RUnlock()
	if err != nil {
		return out, &mediaimport.SourceError{Message: "无法准备 TG 视频存储目录", WaitForAvailability: true}
	}
	if info, e := os.Stat(destination); e == nil {
		if info.Size() <= 0 || info.Size() > cfg.MaxFileSizeBytes || source.Size != info.Size() {
			return out, &mediaimport.SourceError{Message: "TG 视频大小无效"}
		}
		if err = syncLibrary(destination, filepath.Dir(destination), filepath.Join(cfg.LocalFilesRoot, "library")); err != nil {
			return out, &mediaimport.SourceError{Message: "无法确认 TG 视频已落盘", WaitForAvailability: true}
		}
		return acquiredFile(source, local, destination, info.Size()), nil
	}
	free, err := mediaimport.AvailableBytes(cfg.LocalFilesRoot)
	if err != nil || free < s.reserve || size > free-s.reserve {
		return out, &mediaimport.SourceError{Message: "TG 视频目录空间不足或不可用", WaitForAvailability: true}
	}
	if err = progress("telegram_download", 0, source.Size); err != nil {
		return out, err
	}
	fileID, err := s.cat.LatestTelegramFileID(ctx, source)
	if err != nil {
		return out, err
	}
	s.mu.RLock()
	c := s.client
	s.mu.RUnlock()
	if c == nil {
		return out, &mediaimport.SourceError{Message: "Telegram 尚未连接", RetryAfter: time.Minute}
	}
	fetchCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.FetchTimeoutSeconds)*time.Second)
	defer cancel()
	partPath := destination + ".part"
	// Interrupted downloads are retried from zero at this deterministic path.
	if err := os.Remove(partPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return out, &mediaimport.SourceError{Message: "无法清理未完成的 Telegram 下载", WaitForAvailability: true}
	}
	part, err := os.OpenFile(partPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return out, &mediaimport.SourceError{Message: "无法创建 Telegram 下载文件", WaitForAvailability: true}
	}
	defer os.Remove(part.Name())
	defer part.Close()
	s.downloadProgress.start(j.ID, time.Now())
	writer := &downloadWriter{file: part, limit: source.Size, progress: func(stage string, done, total int64) error {
		if !s.Allowed(source.SenderID) {
			return &mediaimport.SourceError{Message: "发送者已不在允许用户列表中"}
		}
		if err := progress(stage, done, total); err != nil {
			return err
		}
		s.downloadProgress.update(j.ID, done, time.Now())
		return nil
	}}
	if err = c.Download(fetchCtx, fileID, writer); err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if errors.Is(err, catalog.ErrRemoteUploadCanceled) {
			return out, catalog.ErrRemoteUploadCanceled
		}
		var sourceErr *mediaimport.SourceError
		if errors.As(err, &sourceErr) {
			return out, sourceErr
		}
		var outputErr *localError
		if errors.As(err, &outputErr) {
			return out, &mediaimport.SourceError{Message: outputErr.message, WaitForAvailability: outputErr.retry}
		}
		var connectionErr *connectionError
		if errors.As(err, &connectionErr) {
			return out, &mediaimport.SourceError{Message: connectionErr.Error(), RetryAfter: 10 * time.Second}
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return out, &mediaimport.SourceError{Message: apiErr.Error(), RetryAfter: apiErr.RetryAfter}
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return out, &mediaimport.SourceError{Message: "Telegram 视频下载超时", RetryAfter: 10 * time.Second}
		}
		return out, &mediaimport.SourceError{Message: "Telegram 视频下载失败，请检查原消息或重新发送"}
	}
	if !s.Allowed(source.SenderID) {
		return out, &mediaimport.SourceError{Message: "发送者已不在允许用户列表中"}
	}
	if writer.done != source.Size {
		return out, &mediaimport.SourceError{Message: "Telegram 视频大小不一致", RetryAfter: 10 * time.Second}
	}
	if err = part.Sync(); err != nil {
		return out, &mediaimport.SourceError{Message: "无法保存 Telegram 视频", WaitForAvailability: true}
	}
	info, err := part.Stat()
	if err != nil || info.Size() != source.Size {
		return out, &mediaimport.SourceError{Message: "Telegram 视频文件大小无效"}
	}
	if err = part.Close(); err != nil {
		return out, err
	}
	if err = persistence.RLockContext(ctx); err != nil {
		return out, err
	}
	defer persistence.RUnlock()
	if err = os.Rename(part.Name(), destination); err != nil {
		return out, &mediaimport.SourceError{Message: "无法保存 Telegram 视频，请检查目录写入权限", WaitForAvailability: true}
	}
	if err = syncLibrary(destination, filepath.Dir(destination), filepath.Join(cfg.LocalFilesRoot, "library")); err != nil {
		return out, &mediaimport.SourceError{Message: "无法确认 Telegram 视频已落盘", WaitForAvailability: true}
	}
	if err = progress("telegram_download", source.Size, source.Size); err != nil {
		return out, err
	}
	return acquiredFile(source, local, destination, info.Size()), nil
}

// Writer and progress errors cross the downloader boundary without becoming
// network failures. Error() is safe for users; Unwrap preserves cancellation.
type localError struct {
	message string
	cause   error
	retry   bool
}

func (e *localError) Error() string { return e.message }
func (e *localError) Unwrap() error { return e.cause }

// Enforce advertised size while writing and report measured byte progress.
type downloadWriter struct {
	mu          sync.Mutex
	file        io.WriterAt
	done, limit int64
	progress    func(string, int64, int64) error
	last        time.Time
}

func (w *downloadWriter) WriteAt(data []byte, offset int64) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if offset < 0 || offset > w.limit || int64(len(data)) > w.limit-offset || int64(len(data)) > w.limit-w.done {
		return 0, &localError{message: "Telegram 视频超过声明的文件大小"}
	}
	n, err := w.file.WriteAt(data, offset)
	w.done += int64(n)
	if err == nil && n < len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return n, &localError{message: "无法写入 Telegram 视频，请检查磁盘空间和目录权限", cause: err, retry: true}
	}
	if w.last.IsZero() || time.Since(w.last) >= time.Second {
		err = w.progress("telegram_download", w.done, w.limit)
		w.last = time.Now()
		if err != nil {
			return n, &localError{message: "无法保存 Telegram 下载进度，请检查数据库状态", cause: err, retry: true}
		}
	}
	return n, err
}

func acquiredFile(source catalog.TelegramSource, local catalog.TelegramLocalFile, path string, size int64) mediaimport.SourceFile {
	name := source.FileName
	if name == "" {
		name = "telegram-video.mp4"
	}
	return mediaimport.SourceFile{Name: name, MIME: source.MIME, Size: size, Path: path, DriveID: telegramstorage.DriveID, FileID: local.FileID}
}
func (s *Service) Discard(ctx context.Context, j *catalog.RemoteUploadJob) error {
	cfg := s.settings()
	return telegramstorage.New(s.cat, func() string { return cfg.LocalFilesRoot }).Remove(ctx, j.ID+".media")
}
