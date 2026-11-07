package mediaimport

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives/localupload"
	"github.com/nianzhibai/91/backend/internal/drives/telegramstorage"
	"github.com/nianzhibai/91/backend/internal/persistence"
	"github.com/nianzhibai/91/backend/internal/videoname"
)

const (
	defaultDiskReserve = int64(1 << 30)
	defaultIdleTimeout = 120 * time.Second
	retentionPeriod    = 7 * 24 * time.Hour
)

type ValidationError struct {
	message string
}

func (e *ValidationError) Error() string { return e.message }

func validationError(message string) error {
	return &ValidationError{message: message}
}

func IsValidationError(err error) bool {
	var target *ValidationError
	return errors.As(err, &target)
}

type CreateInput struct {
	URL   string
	Title string
	Tags  []string
}

type Config struct {
	Catalog         *catalog.Catalog
	UploadDir       string
	FFprobePath     string
	DiskReserve     int64
	IdleTimeout     time.Duration
	OnVideoUploaded func(*catalog.Video)
	TelegramSource  FileSource
}

type Manager struct {
	telegramSource  FileSource
	catalog         *catalog.Catalog
	uploadDir       string
	ffprobePath     string
	diskReserve     int64
	idleTimeout     time.Duration
	onVideoUploaded func(*catalog.Video)

	policy         *urlPolicy
	client         *http.Client
	validateURL    func(context.Context, string) (*url.URL, error)
	availableBytes func(string) (int64, error)
	probeFile      func(context.Context, string, string) (mediaInfo, error)

	startMu sync.Mutex
	started bool
	runCtx  context.Context
	cancel  context.CancelFunc
	wake    chan struct{}
	done    chan struct{}

	currentMu     sync.Mutex
	currentID     string
	currentCancel context.CancelFunc
}

func New(cfg Config) (*Manager, error) {
	if cfg.Catalog == nil {
		return nil, errors.New("remote upload: catalog is required")
	}
	cfg.UploadDir = strings.TrimSpace(cfg.UploadDir)
	if cfg.UploadDir == "" {
		return nil, errors.New("remote upload: upload directory is required")
	}
	if err := os.MkdirAll(cfg.UploadDir, 0o755); err != nil {
		return nil, fmt.Errorf("remote upload: create upload directory: %w", err)
	}
	if cfg.FFprobePath == "" {
		cfg.FFprobePath = "ffprobe"
	}
	if cfg.DiskReserve <= 0 {
		cfg.DiskReserve = defaultDiskReserve
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = defaultIdleTimeout
	}

	policy := newURLPolicy()
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           policy.dialContext,
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   1,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	m := &Manager{
		catalog:         cfg.Catalog,
		telegramSource:  cfg.TelegramSource,
		uploadDir:       cfg.UploadDir,
		ffprobePath:     cfg.FFprobePath,
		diskReserve:     cfg.DiskReserve,
		idleTimeout:     cfg.IdleTimeout,
		onVideoUploaded: cfg.OnVideoUploaded,
		policy:          policy,
		wake:            make(chan struct{}, 1),
		done:            make(chan struct{}),
		availableBytes:  diskAvailableBytes,
		probeFile:       probeMediaFile,
	}
	m.validateURL = policy.validate
	m.client = &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return validationError("视频直链重定向次数过多")
			}
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			req.Header.Del("Referer")
			return policy.validateParsed(req.Context(), req.URL)
		},
	}
	return m, nil
}

func (m *Manager) Start(parent context.Context) error {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	if m.started {
		return nil
	}

	if err := m.cleanupAbandonedTelegram(parent); err != nil {
		return err
	}
	refs, err := m.catalog.ListInterruptedRemoteUploadArtifacts(parent)
	if err != nil {
		return fmt.Errorf("remote upload: inspect interrupted jobs: %w", err)
	}
	for _, ref := range refs {
		if err := m.cleanupArtifactRef(ref); err != nil {
			return fmt.Errorf("remote upload: clean interrupted job %s: %w", ref.JobID, err)
		}
	}
	if _, err := m.catalog.RecoverRemoteUploadJobs(parent); err != nil {
		return fmt.Errorf("remote upload: recover jobs: %w", err)
	}
	if err := m.cleanupStrayParts(); err != nil {
		return fmt.Errorf("remote upload: clean stale part files: %w", err)
	}
	if _, err := m.catalog.DeleteExpiredRemoteUploadJobs(parent, time.Now().Add(-retentionPeriod)); err != nil {
		return fmt.Errorf("remote upload: clean expired jobs: %w", err)
	}

	m.runCtx, m.cancel = context.WithCancel(parent)
	m.started = true
	go m.run()
	m.signal()
	return nil
}

func (m *Manager) Shutdown(ctx context.Context) error {
	m.startMu.Lock()
	if !m.started {
		m.startMu.Unlock()
		return nil
	}
	cancel := m.cancel
	done := m.done
	m.startMu.Unlock()

	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) Create(ctx context.Context, input CreateInput) (*catalog.RemoteUploadJob, error) {
	u, err := m.validateURL(ctx, input.URL)
	if err != nil {
		return nil, err
	}
	title := strings.TrimSpace(input.Title)
	if title != "" {
		if err := videoname.ValidateUploadTitle(title, ".webm"); err != nil {
			return nil, validationError(err.Error())
		}
	}
	id, err := randomID("remote")
	if err != nil {
		return nil, err
	}
	job, err := m.catalog.CreateRemoteUploadJob(
		ctx,
		id,
		u.String(),
		sourceLabel(u),
		title,
		input.Tags,
	)
	if err != nil {
		return nil, err
	}
	m.signal()
	return job, nil
}

func (m *Manager) List(ctx context.Context, limit int) ([]*catalog.RemoteUploadJob, error) {
	_, _ = m.catalog.DeleteExpiredRemoteUploadJobs(ctx, time.Now().Add(-retentionPeriod))
	return m.catalog.ListRemoteUploadJobs(ctx, limit)
}

func (m *Manager) Cancel(ctx context.Context, id string) (*catalog.RemoteUploadJob, error) {
	job, err := m.catalog.CancelRemoteUploadJob(ctx, id)
	if err != nil {
		return nil, err
	}
	if job.State == catalog.RemoteUploadCanceled {
		if err := m.discardTelegram(ctx, job); err != nil {
			return nil, err
		}
	}
	if job.CancelRequested {
		m.currentMu.Lock()
		if m.currentID == job.ID && m.currentCancel != nil {
			m.currentCancel()
		}
		m.currentMu.Unlock()
	}
	m.signal()
	return job, nil
}

func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) run() {
	defer close(m.done)
	cleanupTicker := time.NewTicker(24 * time.Hour)
	defer cleanupTicker.Stop()

	for {
		if err := m.runCtx.Err(); err != nil {
			return
		}
		select {
		case <-cleanupTicker.C:
			_ = m.cleanupAbandonedTelegram(m.runCtx)
			_, _ = m.catalog.DeleteExpiredRemoteUploadJobs(
				context.Background(),
				time.Now().Add(-retentionPeriod),
			)
		default:
		}
		job, err := m.catalog.ClaimNextImportJob(m.runCtx, m.telegramSource != nil && m.telegramSource.Available())
		if err == nil {
			m.process(job)
			continue
		}
		retryDelay := 30 * time.Second
		if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, context.Canceled) {
			applog.Error(m.runCtx, "Remote upload queue lookup failed", err, applog.Fields{Component: "remote-upload", Stage: "queue"})
			retryDelay = time.Second
		}
		retryTimer := time.NewTimer(retryDelay)
		select {
		case <-m.runCtx.Done():
			retryTimer.Stop()
			return
		case <-m.wake:
			retryTimer.Stop()
		case <-cleanupTicker.C:
			_ = m.cleanupAbandonedTelegram(m.runCtx)
			retryTimer.Stop()
			_, _ = m.catalog.DeleteExpiredRemoteUploadJobs(
				context.Background(),
				time.Now().Add(-retentionPeriod),
			)
		case <-retryTimer.C:
		}
	}
}

func (m *Manager) process(job *catalog.RemoteUploadJob) {
	jobCtx, cancel := context.WithCancel(applog.WithFields(m.runCtx, applog.Fields{Component: "remote-upload", TaskID: job.ID}))
	m.currentMu.Lock()
	m.currentID = job.ID
	m.currentCancel = cancel
	m.currentMu.Unlock()

	err := m.processJob(jobCtx, job)
	cancel()
	m.currentMu.Lock()
	if m.currentID == job.ID {
		m.currentID = ""
		m.currentCancel = nil
	}
	m.currentMu.Unlock()
	if err == nil {
		return
	}

	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cleanupCancel()
	current, getErr := m.catalog.GetRemoteUploadJob(cleanupCtx, job.ID)
	if getErr == nil {
		if cleanupErr := m.cleanupArtifactRef(catalog.RemoteUploadCleanupRef{JobID: current.ID, TempFile: current.TempFile, FinalFile: current.FinalFile}); cleanupErr != nil {
			applog.Error(jobCtx, "Import cleanup failed; artifacts retained for recovery", cleanupErr, applog.Fields{Stage: "cleanup"})
			return
		}
	}

	if m.runCtx.Err() != nil {
		if requeueErr := m.catalog.RequeueRemoteUploadOnShutdown(cleanupCtx, job.ID); requeueErr != nil {
			applog.Error(jobCtx, "Shutdown requeue failed", requeueErr, applog.Fields{Stage: "requeue"})
		}
		return
	}
	if errors.Is(err, catalog.ErrRemoteUploadCanceled) ||
		errors.Is(err, context.Canceled) ||
		(current != nil && current.CancelRequested) {
		_ = m.discardTelegram(cleanupCtx, job)
		if cancelErr := m.catalog.MarkRemoteUploadCanceled(cleanupCtx, job.ID); cancelErr != nil &&
			!errors.Is(cancelErr, catalog.ErrRemoteUploadTerminal) {
			applog.Error(jobCtx, "Cancellation cleanup failed", cancelErr, applog.Fields{Stage: "cancel"})
		}
		return
	}

	var sourceErr *SourceError
	if errors.As(err, &sourceErr) && (sourceErr.WaitForAvailability || (sourceErr.RetryAfter > 0 && job.RetryCount < 3)) {
		delay := sourceErr.RetryAfter
		if delay <= 0 {
			delay = time.Minute
		}
		backoff := []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute}
		if job.RetryCount < 3 && delay < backoff[job.RetryCount] {
			delay = backoff[job.RetryCount]
		}
		if delayErr := m.catalog.DelayImport(cleanupCtx, job.ID, sourceErr.Message, delay, !sourceErr.WaitForAvailability); delayErr != nil {
			if errors.Is(delayErr, catalog.ErrRemoteUploadCanceled) || (current != nil && current.CancelRequested) {
				_ = m.catalog.MarkRemoteUploadCanceled(cleanupCtx, job.ID)
			}
		}
		return
	}
	_ = m.discardTelegram(cleanupCtx, job)
	message := publicJobError(err)
	if failErr := m.catalog.FailRemoteUploadJob(cleanupCtx, job.ID, message); failErr != nil &&
		!errors.Is(failErr, catalog.ErrRemoteUploadTerminal) {
		applog.Error(jobCtx, "Failure state update failed", failErr, applog.Fields{Stage: "save"})
	}
	applog.Error(jobCtx, message, err, applog.Fields{Stage: "complete"})
}

func (m *Manager) processJob(ctx context.Context, job *catalog.RemoteUploadJob) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var metadata downloadMetadata
	var source SourceFile
	var partPath, partName string
	var err error
	if job.SourceKind == "telegram" {
		if m.telegramSource == nil {
			return taskError("Telegram 未启用")
		}
		source, err = m.telegramSource.Fetch(ctx, job, func(stage string, done, total int64) error {
			if err := m.catalog.SetImportStage(ctx, job.ID, stage); err != nil {
				return err
			}
			return m.catalog.UpdateRemoteUploadProgress(ctx, job.ID, done, total)
		})
		if err != nil {
			return err
		}
		if source.DriveID != telegramstorage.DriveID || source.FileID == "" || source.Path == "" {
			return taskError("TG 视频存储信息无效")
		}
		partPath = source.Path
		metadata = downloadMetadata{Size: source.Size, Total: source.Size, ContentType: source.MIME, ContentDisposition: mime.FormatMediaType("attachment", map[string]string{"filename": source.Name})}
	} else {
		part, e := os.CreateTemp(m.uploadDir, ".remote-"+job.ID+"-*.part")
		if e != nil {
			return taskError("无法创建下载临时文件")
		}
		partPath, partName = part.Name(), filepath.Base(part.Name())
		if err = m.catalog.SetRemoteUploadTempFile(ctx, job.ID, partName); err != nil {
			part.Close()
			os.Remove(partPath)
			return err
		}
		metadata, err = m.download(ctx, job, part)
		closeErr := part.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return taskError("无法保存下载文件")
		}
		if err = os.Chmod(partPath, 0644); err != nil {
			return taskError("无法设置下载文件权限")
		}
	}
	if metadata.Size <= 0 {
		return taskError("远程视频为空文件")
	}
	if err := m.catalog.UpdateRemoteUploadProgress(ctx, job.ID, metadata.Size, metadata.Total); err != nil {
		return err
	}
	if err := m.catalog.TransitionRemoteUploadJob(
		ctx,
		job.ID,
		catalog.RemoteUploadDownloading,
		catalog.RemoteUploadValidating,
	); err != nil {
		return err
	}

	info, err := m.probeFile(ctx, m.ffprobePath, partPath)
	if err != nil || len(info.VideoCodecs) == 0 {
		applog.Error(ctx, "Validate downloaded video failed", err, applog.Fields{Stage: "probe"})
		return taskError("下载内容不是有效的视频文件")
	}
	ext, err := supportedExtension(info, metadata)
	if err != nil {
		return err
	}
	title, err := resolveTitle(job, metadata, ext)
	if err != nil {
		return err
	}
	if err := m.catalog.TransitionRemoteUploadJob(
		ctx,
		job.ID,
		catalog.RemoteUploadValidating,
		catalog.RemoteUploadSaving,
	); err != nil {
		return err
	}

	if err := persistence.RLockContext(ctx); err != nil {
		return err
	}
	mutationLocked := true
	defer func() {
		if mutationLocked {
			persistence.RUnlock()
		}
	}()
	var video *catalog.Video
	if job.SourceKind == "telegram" {
		info, statErr := os.Stat(source.Path)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() != source.Size {
			return taskError("TG 视频文件已变化")
		}
		video = &catalog.Video{
			ID:            telegramstorage.DriveID + "-" + job.ID,
			DriveID:       source.DriveID,
			FileID:        source.FileID,
			FileName:      videoname.UploadFileName(title, ext, job.ID, false),
			Title:         title,
			Size:          source.Size,
			Ext:           strings.TrimPrefix(ext, "."),
			PreviewStatus: "pending",
		}
	} else {
		video, err = m.publishFile(ctx, job.ID, partName, title, ext, metadata.Size)
		if err != nil {
			return err
		}
	}
	autoTags, err := m.catalog.MatchTagAssignments(
		ctx,
		video.Title,
		video.FileName,
		video.Author,
		"",
	)
	if err != nil {
		return taskError("无法匹配视频标签")
	}
	if err := m.catalog.FinalizeRemoteUpload(ctx, job.ID, video, job.Tags, autoTags); err != nil {
		return err
	}
	persistence.RUnlock()
	mutationLocked = false
	saved, err := m.catalog.GetVideo(ctx, video.ID)
	if err == nil {
		video = saved
	}
	if m.onVideoUploaded != nil {
		m.onVideoUploaded(video)
	}
	return nil
}

func (m *Manager) ensureDiskSpace(nextWrite int64) error {
	available, err := m.availableBytes(m.uploadDir)
	if err != nil {
		return taskError("无法检查上传目录可用空间")
	}
	if available < m.diskReserve || nextWrite > available-m.diskReserve {
		return taskError("磁盘空间不足，无法在保留配置的安全余量后继续下载")
	}
	return nil
}

func (m *Manager) publishFile(
	ctx context.Context,
	jobID, partName, title, ext string,
	size int64,
) (*catalog.Video, error) {
	partPath, err := m.artifactPath(partName)
	if err != nil {
		return nil, taskError("下载临时文件路径无效")
	}
	uploadID, err := randomID("upload")
	if err != nil {
		return nil, taskError("无法生成视频编号")
	}

	for attempt := 0; attempt < 20; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		collision := attempt > 0
		if attempt > 1 {
			uploadID, err = randomID("upload")
			if err != nil {
				return nil, taskError("无法生成视频编号")
			}
		}
		storedName := videoname.UploadFileName(title, ext, uploadID, collision)
		resolvedTitle := videoname.TitleFromFileName(storedName)
		videoID := localupload.DriveID + "-" + uploadID
		if err := m.catalog.PrepareRemoteUploadSaving(
			ctx,
			jobID,
			partName,
			storedName,
			videoID,
			resolvedTitle,
		); err != nil {
			return nil, err
		}
		finalPath, err := m.artifactPath(storedName)
		if err != nil {
			return nil, taskError("最终视频路径无效")
		}
		if err := os.Link(partPath, finalPath); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return nil, taskError("无法保存最终视频文件")
		}
		if err := os.Remove(partPath); err != nil {
			_ = os.Remove(finalPath)
			return nil, taskError("无法完成视频文件落盘")
		}
		if err := ctx.Err(); err != nil {
			_ = os.Remove(finalPath)
			return nil, err
		}
		now := time.Now()
		return &catalog.Video{
			ID:            videoID,
			DriveID:       localupload.DriveID,
			FileID:        storedName,
			FileName:      storedName,
			Title:         resolvedTitle,
			Size:          size,
			Ext:           strings.TrimPrefix(ext, "."),
			PreviewStatus: "pending",
			PublishedAt:   now,
			CreatedAt:     now,
			UpdatedAt:     now,
		}, nil
	}
	return nil, taskError("同名视频文件过多，无法生成可用文件名")
}

func supportedExtension(info mediaInfo, metadata downloadMetadata) (string, error) {
	formatNames := make(map[string]bool)
	for _, name := range strings.Split(strings.ToLower(info.FormatName), ",") {
		formatNames[strings.TrimSpace(name)] = true
	}
	candidate := preferredSourceExtension(metadata)
	switch {
	case formatNames["avi"]:
		return ".avi", nil
	case formatNames["matroska"] || formatNames["webm"]:
		if candidate == ".webm" || metadata.ContentType == "video/webm" {
			return ".webm", nil
		}
		return ".mkv", nil
	case formatNames["mov"] || formatNames["mp4"]:
		if candidate == ".mov" || metadata.ContentType == "video/quicktime" {
			return ".mov", nil
		}
		return ".mp4", nil
	default:
		return "", taskError("无法确认下载视频的受支持格式")
	}
}

func preferredSourceExtension(metadata downloadMetadata) string {
	for _, name := range []string{
		contentDispositionFileName(metadata.ContentDisposition),
		urlFileName(metadata.FinalURL),
	} {
		ext := strings.ToLower(filepath.Ext(name))
		switch ext {
		case ".avi", ".mkv", ".mov", ".mp4", ".webm":
			return ext
		}
	}
	switch metadata.ContentType {
	case "video/x-msvideo", "video/avi":
		return ".avi"
	case "video/x-matroska":
		return ".mkv"
	case "video/quicktime":
		return ".mov"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	default:
		return ""
	}
}

func resolveTitle(
	job *catalog.RemoteUploadJob,
	metadata downloadMetadata,
	ext string,
) (string, error) {
	candidates := []string{
		strings.TrimSpace(job.RequestedTitle),
		titleFromRemoteFileName(contentDispositionFileName(metadata.ContentDisposition)),
		titleFromRemoteFileName(urlFileName(metadata.FinalURL)),
		titleFromRemoteFileName(urlFileName(metadata.OriginalURL)),
	}
	for _, title := range candidates {
		if title == "" {
			continue
		}
		if err := videoname.ValidateUploadTitle(title, ext); err != nil {
			return "", taskError(err.Error())
		}
		return title, nil
	}
	return "", taskError("无法从直链生成视频名，请手动填写视频名")
}

func contentDispositionFileName(value string) string {
	_, params, err := mime.ParseMediaType(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	return baseRemoteFileName(params["filename"])
}

func urlFileName(u *url.URL) string {
	if u == nil {
		return ""
	}
	name := path.Base(u.EscapedPath())
	if decoded, err := url.PathUnescape(name); err == nil {
		name = decoded
	}
	return baseRemoteFileName(name)
}

func baseRemoteFileName(name string) string {
	name = strings.TrimSpace(strings.ReplaceAll(name, `\`, "/"))
	name = path.Base(name)
	if name == "." || name == "/" {
		return ""
	}
	return strings.TrimSpace(name)
}

func titleFromRemoteFileName(name string) string {
	name = baseRemoteFileName(name)
	if name == "" {
		return ""
	}
	ext := filepath.Ext(name)
	if ext != "" {
		name = strings.TrimSuffix(name, ext)
	}
	return strings.TrimSpace(name)
}

func isHLSContentType(contentType string) bool {
	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case "application/vnd.apple.mpegurl",
		"application/x-mpegurl",
		"audio/mpegurl",
		"audio/x-mpegurl":
		return true
	default:
		return false
	}
}

func (m *Manager) cleanupArtifactRef(ref catalog.RemoteUploadCleanupRef) error {
	tempPath, tempErr := m.artifactPath(ref.TempFile)
	finalPath, finalErr := m.artifactPath(ref.FinalFile)

	tempInfo, tempStatErr := os.Stat(tempPath)
	finalInfo, finalStatErr := os.Stat(finalPath)
	tempExists := tempErr == nil && tempStatErr == nil
	finalExists := finalErr == nil && finalStatErr == nil

	if finalExists {
		removeFinal := !tempExists
		if tempExists && os.SameFile(tempInfo, finalInfo) {
			removeFinal = true
		}
		if removeFinal {
			if err := os.Remove(finalPath); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	if tempExists {
		if err := os.Remove(tempPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (m *Manager) cleanupStrayParts() error {
	entries, err := os.ReadDir(m.uploadDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() ||
			!strings.HasPrefix(entry.Name(), ".remote-") ||
			!strings.HasSuffix(entry.Name(), ".part") {
			continue
		}
		path, err := m.artifactPath(entry.Name())
		if err != nil {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (m *Manager) artifactPath(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("empty artifact name")
	}
	if filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return "", errors.New("invalid artifact name")
	}
	root, err := filepath.Abs(m.uploadDir)
	if err != nil {
		return "", err
	}
	target, err := filepath.Abs(filepath.Join(root, name))
	if err != nil {
		return "", err
	}
	if target == root || !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return "", errors.New("artifact escapes upload directory")
	}
	return target, nil
}

type taskError string

func (e taskError) Error() string { return string(e) }

func publicJobError(err error) string {
	var source *SourceError
	if errors.As(err, &source) {
		return source.Message
	}
	var safe taskError
	if errors.As(err, &safe) {
		return safe.Error()
	}
	var validation *ValidationError
	if errors.As(err, &validation) {
		return validation.Error()
	}
	return "后台任务处理失败"
}

func randomID(prefix string) (string, error) {
	var suffix [6]byte
	if _, err := crand.Read(suffix[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"%s-%d-%s",
		prefix,
		time.Now().UnixNano(),
		hex.EncodeToString(suffix[:]),
	), nil
}

func (m *Manager) discardTelegram(ctx context.Context, job *catalog.RemoteUploadJob) error {
	if job.SourceKind != "telegram" || m.telegramSource == nil {
		return nil
	}
	// A queued cancellation may race a retry. Do not release an acquisition
	// already claimed by the worker; it will resolve its own terminal state.
	m.currentMu.Lock()
	defer m.currentMu.Unlock()
	if m.currentID == job.ID {
		return nil
	}
	if err := persistence.RLockContext(ctx); err != nil {
		return err
	}
	defer persistence.RUnlock()
	err := m.telegramSource.Discard(ctx, job)
	if err != nil {
		applog.Error(ctx, "TG 视频清理失败，将在后续维护中重试", err, applog.Fields{Component: "telegram", Stage: "cleanup"})
	}
	return err
}
func (m *Manager) cleanupAbandonedTelegram(ctx context.Context) error {
	if m.telegramSource == nil {
		return nil
	}
	m.currentMu.Lock()
	defer m.currentMu.Unlock()
	if err := persistence.RLockContext(ctx); err != nil {
		return err
	}
	defer persistence.RUnlock()
	files, err := m.catalog.AbandonedTelegramLocalFiles(ctx)
	if err != nil {
		return err
	}
	for _, file := range files {
		if file.JobID == m.currentID {
			continue
		}
		if err := m.telegramSource.Discard(ctx, &catalog.RemoteUploadJob{ID: file.JobID, SourceKind: "telegram"}); err != nil {
			applog.Error(ctx, "TG 视频清理失败，将在后续维护中重试", err, applog.Fields{Component: "telegram", Stage: "cleanup"})
		}
	}
	return nil
}
