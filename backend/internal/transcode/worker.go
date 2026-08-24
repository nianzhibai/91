package transcode

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/streamhttp"
)

// DefaultTargetDirName 是转码产物在网盘上的存放目录（相对根目录）。
// worker 第一次上传前会 EnsureDir 并把该目录加进 drive 的扫描跳过列表，
// 避免 scanner 把转码产物当成新视频重复入库。
const DefaultTargetDirName = "91转码"

type Config struct {
	FFmpegPath  string
	FFprobePath string
	// WorkDir 是下载原始文件 / 写转码产物的本地临时目录。
	WorkDir string
	// TargetDirName 为空时用 DefaultTargetDirName。
	TargetDirName string
}

// TaskStatus 与 preview/fingerprint worker 的状态结构对齐，供 admin 展示。
type TaskStatus struct {
	State        string
	CurrentTitle string
	QueueLength  int
	DoneCount    int
	TotalCount   int
}

// Worker 串行处理一个 drive 的转码任务。生命周期与一次"开始转码"对应：
// Run 处理完整个候选列表（或 ctx 被取消）后即结束，不常驻。
type Worker struct {
	cfg      Config
	cat      *catalog.Catalog
	drv      drives.Drive
	uploader drives.Uploader
	hc       *http.Client

	mu           sync.Mutex
	state        string
	currentTitle string
	done         int
	total        int

	targetDirOnce sync.Once
	targetDirID   string
	targetDirErr  error
	targetFiles   map[string]drives.Entry
	targetLoaded  bool
}

func NewWorker(cfg Config, cat *catalog.Catalog, drv drives.Drive) *Worker {
	if cfg.FFmpegPath == "" {
		cfg.FFmpegPath = "ffmpeg"
	}
	if cfg.FFprobePath == "" {
		cfg.FFprobePath = "ffprobe"
	}
	if cfg.TargetDirName == "" {
		cfg.TargetDirName = DefaultTargetDirName
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = os.TempDir()
	}
	uploader, _ := drv.(drives.Uploader)
	return &Worker{
		cfg:      cfg,
		cat:      cat,
		drv:      drv,
		uploader: uploader,
		hc:       streamhttp.NewClient(0),
		state:    "idle",
	}
}

func (w *Worker) Status() TaskStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	queueLen := w.total - w.done
	if w.state == "generating" && queueLen > 0 {
		// 正在处理的那条不算"排队中"
		queueLen--
	}
	if queueLen < 0 {
		queueLen = 0
	}
	return TaskStatus{
		State:        w.state,
		CurrentTitle: w.currentTitle,
		QueueLength:  queueLen,
		DoneCount:    w.done,
		TotalCount:   w.total,
	}
}

// Run 串行转码整个候选列表。ctx 取消时停在当前条目边界（正在跑的 ffmpeg
// 会被 CommandContext 杀掉），未处理的候选保持原状态，下次开始时继续。
func (w *Worker) Run(ctx context.Context, videos []*catalog.Video) {
	w.mu.Lock()
	w.state = "generating"
	w.total = len(videos)
	w.done = 0
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		w.state = "idle"
		w.currentTitle = ""
		w.mu.Unlock()
	}()

	for _, v := range videos {
		if ctx.Err() != nil {
			log.Printf("[transcode] drive=%s canceled after %d/%d", w.drv.ID(), w.doneCount(), len(videos))
			return
		}
		w.mu.Lock()
		w.currentTitle = v.Title
		w.mu.Unlock()

		if err := w.process(ctx, v); err != nil {
			if ctx.Err() != nil {
				// 取消导致的失败不要写 failed，保持候选状态便于下次继续
				log.Printf("[transcode] drive=%s canceled while processing %s", w.drv.ID(), v.ID)
				return
			}
			log.Printf("[transcode] drive=%s video=%s failed: %v", w.drv.ID(), v.ID, err)
			if uerr := w.cat.UpdateVideoTranscode(context.WithoutCancel(ctx), v.ID, "failed", err.Error(), "", 0); uerr != nil {
				log.Printf("[transcode] mark failed %s: %v", v.ID, uerr)
			}
			if wait, rateLimited := drives.RateLimitRetryAfter(err); rateLimited {
				log.Printf("[transcode] drive=%s rate limited, stop batch (retry after %s)", w.drv.ID(), wait)
				return
			}
		}
		w.mu.Lock()
		w.done++
		w.mu.Unlock()
	}
	log.Printf("[transcode] drive=%s finished %d videos", w.drv.ID(), len(videos))
}

func (w *Worker) doneCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.done
}

func (w *Worker) process(ctx context.Context, v *catalog.Video) error {
	if w.uploader == nil {
		return fmt.Errorf("transcode output: %w", drives.ErrNotSupported)
	}
	// 候选列表是任务开始时的快照，条目排队期间可能已被其它途径处理
	// （如单条工具、上一次被打断的任务）。以库里当前状态为准防止重复
	// 转码；读失败不阻塞，继续按快照处理。
	if cur, err := w.cat.GetVideo(ctx, v.ID); err == nil &&
		(cur.TranscodeStatus == "ready" || cur.TranscodeStatus == "skipped") {
		return nil
	}

	link, err := w.drv.StreamURL(ctx, v.FileID)
	if err != nil {
		return fmt.Errorf("resolve source: %w", err)
	}

	if path, ok := localSourcePath(link); ok {
		info, err := ProbeFile(ctx, w.cfg.FFprobePath, path)
		if err != nil {
			return err
		}
		return w.finish(ctx, v, info, path)
	}

	// 云盘文件先远程探测（只读容器元数据，不整文件下载）。编码本就兼容
	// 的直接标 skipped——绝大多数 mp4 在这一步零下载跳过。
	info, probeErr := ProbeURL(ctx, w.cfg.FFprobePath, link.URL, link.Headers)
	if probeErr == nil && !NeedsTranscode(info, v.Ext) {
		log.Printf("[transcode] drive=%s video=%s compatible (%s), skip", w.drv.ID(), v.ID, info.FormatName)
		return w.cat.UpdateVideoTranscode(ctx, v.ID, "skipped", "", "", 0)
	}
	if probeErr != nil && extAssumedPlayable(v.Ext) {
		// mp4/m4v 只为甄别编码才进候选：远程探测失败时不能整文件下载兜
		// 底，否则一次系统性探测失败会把全库 mp4 拉一遍。标 failed 留在
		// 候选里，下次开始转码时自动重试。
		return fmt.Errorf("remote probe: %w", probeErr)
	}

	// 走到这里要么确认需要转码，要么是容器本就不兼容的老候选（avi/mov/
	// mkv…）探测失败——都下载整文件，以本地探测结果为准。
	localPath, cleanup, err := w.download(ctx, v, link)
	if err != nil {
		return fmt.Errorf("fetch source: %w", err)
	}
	defer cleanup()
	info, err = ProbeFile(ctx, w.cfg.FFprobePath, localPath)
	if err != nil {
		return err
	}
	return w.finish(ctx, v, info, localPath)
}

// finish 拿到权威探测结果和本地可读路径后完成剩余流程：跳过或转码+上传。
func (w *Worker) finish(ctx context.Context, v *catalog.Video, info MediaInfo, localPath string) error {
	if !NeedsTranscode(info, v.Ext) {
		log.Printf("[transcode] drive=%s video=%s compatible (%s), skip", w.drv.ID(), v.ID, info.FormatName)
		return w.cat.UpdateVideoTranscode(ctx, v.ID, "skipped", "", "", 0)
	}

	dirID, err := w.ensureTargetDir(ctx)
	if err != nil {
		return fmt.Errorf("ensure target dir: %w", err)
	}
	name := transcodedName(v)
	if existing, ok, err := w.findTargetFile(ctx, dirID, name, -1, false); err != nil {
		return fmt.Errorf("reconcile transcoded file: %w", err)
	} else if ok {
		log.Printf("[transcode] drive=%s video=%s reconciled existing file=%s size=%d", w.drv.ID(), v.ID, existing.ID, existing.Size)
		return w.cat.UpdateVideoTranscode(ctx, v.ID, "ready", "", existing.ID, existing.Size)
	}

	outPath := filepath.Join(w.cfg.WorkDir, sanitizeFileName(v.ID)+".transcoding.mp4")
	defer os.Remove(outPath)
	if err := TranscodeFile(ctx, w.cfg.FFmpegPath, info, localPath, outPath); err != nil {
		return err
	}
	stat, err := os.Stat(outPath)
	if err != nil {
		return fmt.Errorf("stat transcoded output: %w", err)
	}

	fileID, err := w.uploadTranscodedFile(ctx, dirID, name, outPath, stat.Size())
	if err != nil {
		return fmt.Errorf("upload transcoded file: %w", err)
	}
	log.Printf("[transcode] drive=%s video=%s ready: file=%s size=%d", w.drv.ID(), v.ID, fileID, stat.Size())
	return w.cat.UpdateVideoTranscode(ctx, v.ID, "ready", "", fileID, stat.Size())
}

// uploadTranscodedFile closes the remote-write/catalog-write crash window.
// The destination name is deterministic, so a preflight list can bind a prior
// successful attempt. If Upload returns an ambiguous error, a forced refresh
// also recovers the object when the remote commit actually succeeded.
func (w *Worker) uploadTranscodedFile(ctx context.Context, dirID, name, path string, size int64) (string, error) {
	if existing, ok, err := w.findTargetFile(ctx, dirID, name, size, true); err != nil {
		return "", fmt.Errorf("preflight destination: %w", err)
	} else if ok {
		return existing.ID, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open transcoded output: %w", err)
	}
	fileID, uploadErr := w.uploader.Upload(ctx, dirID, name, f, size)
	_ = f.Close()
	if uploadErr == nil && strings.TrimSpace(fileID) == "" {
		uploadErr = errors.New("transcode uploader returned an empty file id")
	}
	if uploadErr == nil {
		w.rememberTargetFile(name, drives.Entry{ID: fileID, Name: name, Size: size})
		return fileID, nil
	}
	if ctx.Err() != nil {
		return "", uploadErr
	}
	if _, rateLimited := drives.RateLimitRetryAfter(uploadErr); rateLimited {
		// The next run will reconcile after the provider cooldown. Issuing List
		// immediately would only add another throttled request.
		return "", uploadErr
	}

	existing, ok, reconcileErr := w.findTargetFile(ctx, dirID, name, size, true)
	if reconcileErr != nil {
		return "", errors.Join(uploadErr, fmt.Errorf("reconcile destination after upload error: %w", reconcileErr))
	}
	if ok {
		return existing.ID, nil
	}
	return "", uploadErr
}

func (w *Worker) findTargetFile(ctx context.Context, dirID, name string, size int64, force bool) (drives.Entry, bool, error) {
	if force || !w.targetLoaded {
		entries, err := w.drv.List(ctx, dirID)
		if err != nil {
			return drives.Entry{}, false, err
		}
		files := make(map[string]drives.Entry)
		for _, entry := range entries {
			if entry.IsDir || strings.TrimSpace(entry.ID) == "" {
				continue
			}
			files[entry.Name] = entry
		}
		w.targetFiles = files
		w.targetLoaded = true
	}
	entry, ok := w.targetFiles[name]
	if !ok || entry.Size <= 0 || (size >= 0 && entry.Size != size) {
		return drives.Entry{}, false, nil
	}
	return entry, true, nil
}

func (w *Worker) rememberTargetFile(name string, entry drives.Entry) {
	if w.targetFiles == nil {
		w.targetFiles = make(map[string]drives.Entry)
	}
	w.targetFiles[name] = entry
	w.targetLoaded = true
}

// localSourcePath 判断 StreamLink 是否指向本地文件（本地存储盘），是则
// 返回本地路径。
func localSourcePath(link *drives.StreamLink) (string, bool) {
	u, err := url.Parse(link.URL)
	if err != nil || u.Scheme == "http" || u.Scheme == "https" {
		return "", false
	}
	if u.Scheme == "file" {
		return u.Path, true
	}
	return link.URL, true
}

// extAssumedPlayable 是"容器本身浏览器可播、只为甄别里面编码才进转码
// 候选"的扩展名。这些文件不做整文件下载兜底（见 process）。
func extAssumedPlayable(ext string) bool {
	switch strings.ToLower(strings.TrimSpace(ext)) {
	case "mp4", "m4v":
		return true
	}
	return false
}

// download 把云盘文件整文件下载到 WorkDir，返回本地路径和清理函数。
func (w *Worker) download(ctx context.Context, v *catalog.Video, link *drives.StreamLink) (string, func(), error) {
	tmpPath := filepath.Join(w.cfg.WorkDir, sanitizeFileName(v.ID)+".src.tmp")
	cleanup := func() { os.Remove(tmpPath) }
	if err := w.downloadTo(ctx, link, tmpPath); err != nil {
		cleanup()
		return "", nil, err
	}
	return tmpPath, cleanup, nil
}

func (w *Worker) downloadTo(ctx context.Context, link *drives.StreamLink, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.URL, nil)
	if err != nil {
		return err
	}
	for k, vals := range link.Headers {
		for _, val := range vals {
			req.Header.Add(k, val)
		}
	}
	res, err := w.hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("download source: HTTP %d", res.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, res.Body); err != nil {
		return fmt.Errorf("download source: %w", err)
	}
	return f.Sync()
}

// ensureTargetDir 确保网盘上的转码产物目录存在，并把它写进 drive 的扫描
// 跳过列表（幂等），避免 scanner 把产物再当新视频收进库。
func (w *Worker) ensureTargetDir(ctx context.Context) (string, error) {
	w.targetDirOnce.Do(func() {
		dirID, err := w.uploader.EnsureDir(ctx, w.cfg.TargetDirName)
		if err != nil {
			w.targetDirErr = err
			return
		}
		w.targetDirID = dirID
		if err := w.addDirToSkipList(ctx, dirID); err != nil {
			// 跳过列表更新失败不阻塞转码，只记日志（最坏情况是 scanner
			// 之后把产物扫成新视频，可手动加跳过目录修复）。
			log.Printf("[transcode] drive=%s add skip dir %s: %v", w.drv.ID(), dirID, err)
		}
	})
	return w.targetDirID, w.targetDirErr
}

func (w *Worker) addDirToSkipList(ctx context.Context, dirID string) error {
	return w.cat.EnsureDriveSkipDirID(ctx, w.drv.ID(), dirID)
}

// transcodedName includes a stable video-ID suffix. Basename alone is not a
// unique key in the shared target directory and previously caused unrelated
// videos named e.g. video.avi to overwrite or reconcile to each other.
func transcodedName(v *catalog.Video) string {
	base := strings.TrimSpace(v.FileName)
	if base == "" {
		base = v.Title
	}
	if base == "" {
		base = v.ID
	}
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	identity := strings.TrimSpace(v.ID)
	if identity == "" {
		identity = base
	}
	sum := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%s-%x.mp4", sanitizeFileName(base), sum[:6])
}

// sanitizeFileName 把路径分隔符等危险字符替换掉，避免拼出意外路径。
func sanitizeFileName(name string) string {
	replacer := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_", "?", "_",
		"\"", "_", "<", "_", ">", "_", "|", "_", "\x00", "_",
	)
	out := strings.TrimSpace(replacer.Replace(name))
	if out == "" {
		out = fmt.Sprintf("transcoded-%d", time.Now().UnixMilli())
	}
	return out
}
