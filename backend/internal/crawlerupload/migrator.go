// Package crawlerupload uploads videos saved by script crawlers to a configured
// target drive. Each crawler drive chooses its own upload target.
//
//   - 改写 catalog 行：drive_id / file_id / content_hash 改成目标盘的；
//     视频自身的 id 不变，video_tags、收藏、点赞、views 等关联数据全部保留
//   - 删除爬虫本地 mp4 和源 thumb；公共 /p/thumb/<videoID> 副本会保留
//
// 之后回放时，videoSource() 自动落到 /p/stream/<target>/<file_id>，
// proxy 层走对应盘的直链 / 302 直连。
//
// 下次目标盘扫盘时，scanner 通过 (content_hash) / (file_name+size)
// 已有的 findDuplicate 兜底逻辑，不会为同一物理文件再建一行。
package crawlerupload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/drives/googledrive"
	"github.com/nianzhibai/91/backend/internal/drives/guangyapan"
	"github.com/nianzhibai/91/backend/internal/drives/onedrive"
	"github.com/nianzhibai/91/backend/internal/drives/p115"
	"github.com/nianzhibai/91/backend/internal/drives/p123"
	"github.com/nianzhibai/91/backend/internal/drives/pikpak"
	"github.com/nianzhibai/91/backend/internal/drives/quark"
	"github.com/nianzhibai/91/backend/internal/drives/scriptcrawler"
	"github.com/nianzhibai/91/backend/internal/drives/webdav"
	"github.com/nianzhibai/91/backend/internal/drives/wopan"
	"github.com/nianzhibai/91/backend/internal/drives/yun139"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
	"github.com/nianzhibai/91/backend/internal/persistence"
	"github.com/nianzhibai/91/backend/internal/videoname"
)

// uploadTarget 是 migrator 调用目标 drive 的最小接口。任何一种"接收爬虫上传"的
// 网盘都要实现它；当前 PikPak、115、123、OneDrive、Google Drive、联通网盘、光鸭网盘和 WebDAV 各自通过适配器满足。
//
// 这一层抽象把"迁移调用方"和"具体盘的 SDK 协议"解耦：
//   - PikPak 走 GCID + OSS PutObject（pikpak.UploadResult）
//   - 115   走 SHA1   + 秒传 / OSS / 分片（p115.UploadResult）
//   - 123   走 MD5    + 秒传 / S3 预签名分片（p123.UploadResult）
//   - OneDrive 走 SHA1 + 小文件 PUT / 大文件 upload session
//   - Google Drive 走 MD5 + resumable upload session
//   - 联通网盘 走 SDK Upload2C，当前上游不返回内容 hash
//   - 光鸭网盘 走 OSS 分片上传，当前上游不返回内容 hash
//   - WebDAV 走标准 MKCOL + PUT，协议不提供稳定内容 hash
//
// 各家返回值都被归一成本地的 UploadResult，并在 catalog 改写阶段统一处理。
type uploadTarget interface {
	ID() string
	Kind() string
	RootID() string
	EnsureDir(ctx context.Context, pathFromRoot string) (string, error)
	UploadAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error)
	Rename(ctx context.Context, fileID, newName string) error
}

// existingUploadFinder reconciles the deterministic destination name before a
// new upload. It closes the unavoidable remote-write/catalog-write crash
// window: after a restart the worker binds the local row to the already-created
// remote object instead of uploading another copy.
type existingUploadFinder interface {
	FindExisting(ctx context.Context, parentID, name string, size int64) (*UploadResult, error)
}

// LocalSource is the local source interface used by the migration
// worker. scriptcrawler.Driver satisfies it when mounted for a crawler that
// keeps videos in local storage before uploading them to a target drive.
type LocalSource interface {
	drives.Drive
	VideosDir() string
	ThumbsDir() string
	VideoPath(fileID string) (string, error)
	ThumbPath(fileID string) (string, error)
}

// UploadResult 是 uploadTarget.UploadAndReportHash 的归一返回。
//
// FileID  目标盘上的新文件 ID；
// Hash    GCID（PikPak）、MD5 HEX（123 / Google Drive）或 SHA1 HEX（115 / OneDrive），写入 catalog.content_hash 用于跨盘去重；联通网盘和光鸭网盘暂为空；
// Size    实际上传字节数。
type UploadResult struct {
	FileID string
	Hash   string
	Size   int64
}

type UploadProgress struct {
	DriveID      string
	State        string
	CurrentTitle string
	QueueLength  int
	DoneCount    int
	TotalCount   int
}

const scriptCrawlerUploadRootDirName = "Script Crawlers"

type migrationPlan struct {
	source         LocalSource
	row            *catalog.Drive
	targetDriveID  string
	target         uploadTarget
	uploadDir      string
	uploadProxyURL string
}

// pikpakAdapter / p115Adapter / p123Adapter / onedriveAdapter / googledriveAdapter / webdavAdapter / wopanAdapter / guangyapanAdapter 把具体 driver 包装成 uploadTarget。
//
// 之所以不让 driver 直接实现 uploadTarget：
//
//  1. 各 driver 的 UploadAndReportXxx 返回的是各自包内的 UploadResult 类型，
//     直接共用同名同签名方法会引入循环依赖；
//  2. driver 包不应该感知 crawlerupload 这一层业务定义。
type pikpakAdapter struct {
	d *pikpak.Driver
	existingUploadCache
}

func (a *pikpakAdapter) ID() string     { return a.d.ID() }
func (a *pikpakAdapter) Kind() string   { return a.d.Kind() }
func (a *pikpakAdapter) RootID() string { return a.d.RootID() }
func (a *pikpakAdapter) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	return a.d.EnsureDir(ctx, pathFromRoot)
}
func (a *pikpakAdapter) UploadAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error) {
	res, err := a.d.UploadAndReportHash(ctx, parentID, name, r, size)
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{FileID: res.FileID, Hash: res.Hash, Size: res.Size}, nil
}
func (a *pikpakAdapter) Rename(ctx context.Context, fileID, newName string) error {
	return a.d.Rename(ctx, fileID, newName)
}

type p115Adapter struct {
	d *p115.Driver
	existingUploadCache
}

func (a *p115Adapter) ID() string     { return a.d.ID() }
func (a *p115Adapter) Kind() string   { return a.d.Kind() }
func (a *p115Adapter) RootID() string { return a.d.RootID() }
func (a *p115Adapter) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	return a.d.EnsureDir(ctx, pathFromRoot)
}
func (a *p115Adapter) UploadAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error) {
	res, err := a.d.UploadAndReportSha1(ctx, parentID, name, r, size)
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{FileID: res.FileID, Hash: res.Sha1, Size: res.Size}, nil
}
func (a *p115Adapter) Rename(ctx context.Context, fileID, newName string) error {
	return a.d.Rename(ctx, fileID, newName)
}

type p123Adapter struct {
	d *p123.Driver
	existingUploadCache
}

func (a *p123Adapter) ID() string     { return a.d.ID() }
func (a *p123Adapter) Kind() string   { return a.d.Kind() }
func (a *p123Adapter) RootID() string { return a.d.RootID() }
func (a *p123Adapter) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	return a.d.EnsureDir(ctx, pathFromRoot)
}
func (a *p123Adapter) UploadAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error) {
	res, err := a.d.UploadAndReportHash(ctx, parentID, name, r, size)
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{FileID: res.FileID, Hash: res.Hash, Size: res.Size}, nil
}
func (a *p123Adapter) Rename(ctx context.Context, fileID, newName string) error {
	return a.d.Rename(ctx, fileID, newName)
}

type onedriveAdapter struct {
	d *onedrive.Driver
	existingUploadCache
}

func (a *onedriveAdapter) ID() string     { return a.d.ID() }
func (a *onedriveAdapter) Kind() string   { return a.d.Kind() }
func (a *onedriveAdapter) RootID() string { return a.d.RootID() }
func (a *onedriveAdapter) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	return a.d.EnsureDir(ctx, pathFromRoot)
}
func (a *onedriveAdapter) UploadAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error) {
	res, err := a.d.UploadAndReportHash(ctx, parentID, name, r, size)
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{FileID: res.FileID, Hash: res.Hash, Size: res.Size}, nil
}
func (a *onedriveAdapter) Rename(ctx context.Context, fileID, newName string) error {
	return a.d.Rename(ctx, fileID, newName)
}

type googledriveAdapter struct {
	d *googledrive.Driver
	existingUploadCache
}

func (a *googledriveAdapter) ID() string     { return a.d.ID() }
func (a *googledriveAdapter) Kind() string   { return a.d.Kind() }
func (a *googledriveAdapter) RootID() string { return a.d.RootID() }
func (a *googledriveAdapter) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	return a.d.EnsureDir(ctx, pathFromRoot)
}
func (a *googledriveAdapter) UploadAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error) {
	res, err := a.d.UploadAndReportHash(ctx, parentID, name, r, size)
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{FileID: res.FileID, Hash: res.Hash, Size: res.Size}, nil
}
func (a *googledriveAdapter) Rename(ctx context.Context, fileID, newName string) error {
	return a.d.Rename(ctx, fileID, newName)
}

type wopanAdapter struct {
	d *wopan.Driver
	existingUploadCache
}

func (a *wopanAdapter) ID() string     { return a.d.ID() }
func (a *wopanAdapter) Kind() string   { return a.d.Kind() }
func (a *wopanAdapter) RootID() string { return a.d.RootID() }
func (a *wopanAdapter) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	return a.d.EnsureDir(ctx, pathFromRoot)
}
func (a *wopanAdapter) UploadAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error) {
	fileID, err := a.d.Upload(ctx, parentID, name, r, size)
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{FileID: fileID, Size: size}, nil
}
func (a *wopanAdapter) Rename(ctx context.Context, fileID, newName string) error {
	return a.d.Rename(ctx, fileID, newName)
}

type yun139Adapter struct {
	d *yun139.Driver
	existingUploadCache
}

func (a *yun139Adapter) ID() string     { return a.d.ID() }
func (a *yun139Adapter) Kind() string   { return a.d.Kind() }
func (a *yun139Adapter) RootID() string { return a.d.RootID() }
func (a *yun139Adapter) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	return a.d.EnsureDir(ctx, pathFromRoot)
}
func (a *yun139Adapter) UploadAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error) {
	fileID, err := a.d.Upload(ctx, parentID, name, r, size)
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{FileID: fileID, Size: size}, nil
}
func (a *yun139Adapter) Rename(ctx context.Context, fileID, name string) error {
	return a.d.Rename(ctx, fileID, name)
}
func (a *yun139Adapter) FindExisting(ctx context.Context, parentID, name string, size int64) (*UploadResult, error) {
	return findExistingDriveUpload(ctx, a.d, &a.existingUploadCache, parentID, name, size)
}

type guangyapanAdapter struct {
	d *guangyapan.Driver
	existingUploadCache
}

func (a *guangyapanAdapter) ID() string     { return a.d.ID() }
func (a *guangyapanAdapter) Kind() string   { return a.d.Kind() }
func (a *guangyapanAdapter) RootID() string { return a.d.RootID() }
func (a *guangyapanAdapter) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	return a.d.EnsureDir(ctx, pathFromRoot)
}
func (a *guangyapanAdapter) UploadAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error) {
	fileID, err := a.d.Upload(ctx, parentID, name, r, size)
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{FileID: fileID, Size: size}, nil
}
func (a *guangyapanAdapter) Rename(ctx context.Context, fileID, newName string) error {
	return a.d.Rename(ctx, fileID, newName)
}

type webdavAdapter struct {
	d *webdav.Driver
	existingUploadCache
}

func (a *webdavAdapter) ID() string     { return a.d.ID() }
func (a *webdavAdapter) Kind() string   { return a.d.Kind() }
func (a *webdavAdapter) RootID() string { return a.d.RootID() }
func (a *webdavAdapter) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	return a.d.EnsureDir(ctx, pathFromRoot)
}
func (a *webdavAdapter) UploadAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error) {
	fileID, err := a.d.Upload(ctx, parentID, name, r, size)
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{FileID: fileID, Size: size}, nil
}
func (a *webdavAdapter) Rename(ctx context.Context, fileID, newName string) error {
	return a.d.Rename(ctx, fileID, newName)
}

type quarkAdapter struct {
	d *quark.Driver
	existingUploadCache
}

func (a *quarkAdapter) ID() string     { return a.d.ID() }
func (a *quarkAdapter) Kind() string   { return a.d.Kind() }
func (a *quarkAdapter) RootID() string { return a.d.RootID() }
func (a *quarkAdapter) EnsureDir(ctx context.Context, pathFromRoot string) (string, error) {
	return a.d.EnsureDir(ctx, pathFromRoot)
}
func (a *quarkAdapter) UploadAndReportHash(ctx context.Context, parentID, name string, r io.Reader, size int64) (UploadResult, error) {
	res, err := a.d.UploadAndReportHash(ctx, parentID, name, r, size)
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{FileID: res.FileID, Hash: res.Hash, Size: res.Size}, nil
}
func (a *quarkAdapter) Rename(ctx context.Context, fileID, newName string) error {
	return a.d.Rename(ctx, fileID, newName)
}

type existingUploadCache struct {
	byParent map[string]map[string]UploadResult
}

func (c *existingUploadCache) invalidateExisting(parent string) {
	delete(c.byParent, parent)
}

func findExistingDriveUpload(ctx context.Context, d drives.Drive, cache *existingUploadCache, parentID, name string, size int64) (*UploadResult, error) {
	if cache == nil {
		cache = &existingUploadCache{}
	}
	if cache.byParent == nil {
		cache.byParent = make(map[string]map[string]UploadResult)
	}
	files, loaded := cache.byParent[parentID]
	if !loaded {
		entries, err := d.List(ctx, parentID)
		if err != nil {
			return nil, err
		}
		files = make(map[string]UploadResult)
		for _, entry := range entries {
			if entry.IsDir || strings.TrimSpace(entry.ID) == "" {
				continue
			}
			files[uploadDestinationSignature(entry.Name, entry.Size)] = UploadResult{
				FileID: entry.ID, Hash: entry.Hash, Size: entry.Size,
			}
		}
		cache.byParent[parentID] = files
	}
	result, ok := files[uploadDestinationSignature(name, size)]
	if !ok {
		return nil, nil
	}
	return &result, nil
}

func uploadDestinationSignature(name string, size int64) string {
	return name + "\x00" + strconv.FormatInt(size, 10)
}

func (a *pikpakAdapter) FindExisting(ctx context.Context, parentID, name string, size int64) (*UploadResult, error) {
	return findExistingDriveUpload(ctx, a.d, &a.existingUploadCache, parentID, name, size)
}
func (a *p115Adapter) FindExisting(ctx context.Context, parentID, name string, size int64) (*UploadResult, error) {
	return findExistingDriveUpload(ctx, a.d, &a.existingUploadCache, parentID, name, size)
}
func (a *p123Adapter) FindExisting(ctx context.Context, parentID, name string, size int64) (*UploadResult, error) {
	return findExistingDriveUpload(ctx, a.d, &a.existingUploadCache, parentID, name, size)
}
func (a *onedriveAdapter) FindExisting(ctx context.Context, parentID, name string, size int64) (*UploadResult, error) {
	return findExistingDriveUpload(ctx, a.d, &a.existingUploadCache, parentID, name, size)
}
func (a *googledriveAdapter) FindExisting(ctx context.Context, parentID, name string, size int64) (*UploadResult, error) {
	return findExistingDriveUpload(ctx, a.d, &a.existingUploadCache, parentID, name, size)
}
func (a *wopanAdapter) FindExisting(ctx context.Context, parentID, name string, size int64) (*UploadResult, error) {
	return findExistingDriveUpload(ctx, a.d, &a.existingUploadCache, parentID, name, size)
}
func (a *guangyapanAdapter) FindExisting(ctx context.Context, parentID, name string, size int64) (*UploadResult, error) {
	return findExistingDriveUpload(ctx, a.d, &a.existingUploadCache, parentID, name, size)
}
func (a *webdavAdapter) FindExisting(ctx context.Context, parentID, name string, size int64) (*UploadResult, error) {
	return findExistingDriveUpload(ctx, a.d, &a.existingUploadCache, parentID, name, size)
}
func (a *quarkAdapter) FindExisting(ctx context.Context, parentID, name string, size int64) (*UploadResult, error) {
	return findExistingDriveUpload(ctx, a.d, &a.existingUploadCache, parentID, name, size)
}

// adaptUploadTarget 把通用 drive 包装成 uploadTarget。
// 不支持的盘 kind 返回 error。
func adaptUploadTarget(d drives.Drive) (uploadTarget, error) {
	switch v := d.(type) {
	case *pikpak.Driver:
		return &pikpakAdapter{d: v}, nil
	case *p115.Driver:
		return &p115Adapter{d: v}, nil
	case *p123.Driver:
		return &p123Adapter{d: v}, nil
	case *onedrive.Driver:
		return &onedriveAdapter{d: v}, nil
	case *googledrive.Driver:
		return &googledriveAdapter{d: v}, nil
	case *wopan.Driver:
		return &wopanAdapter{d: v}, nil
	case *yun139.Driver:
		return &yun139Adapter{d: v}, nil
	case *guangyapan.Driver:
		return &guangyapanAdapter{d: v}, nil
	case *webdav.Driver:
		return &webdavAdapter{d: v}, nil
	case *quark.Driver:
		return &quarkAdapter{d: v}, nil
	case uploadTarget:
		// 测试或自定义实现可以直接传入；优先使用具体类型分支以拿到适配器。
		return v, nil
	default:
		return nil, fmt.Errorf("drive %q kind=%s does not support crawler upload", d.ID(), d.Kind())
	}
}

// Registry 是 worker 用来按 driveID 取 driver 的最小依赖。
type Registry interface {
	Get(id string) (drives.Drive, bool)
	All() []drives.Drive
}

type Config struct {
	// PreviewEnabled reads the global policy before admitting each upload.
	// Nil requires ready previews for standalone callers.
	PreviewEnabled func() bool
	Catalog        *catalog.Catalog
	Registry       Registry
	// GetDrive returns the task-generation configuration snapshot. Production
	// supplies this while deferred admin edits are pending; tests and standalone
	// users may omit it to read Catalog directly.
	GetDrive func(context.Context, string) (*catalog.Drive, error)
	// PageSize is the internal page size, not a cap on a completed sweep.
	PageSize int
	// CaptchaCooldown is the destination cooldown after PikPak challenges.
	// Zero defaults to five minutes; negative disables it in tests.
	CaptchaCooldown  time.Duration
	CommonThumbDir   string
	OnMigrated       func(videoID string)
	OnUploadProgress func(UploadProgress)
}

type Migrator struct {
	cfg      Config
	mu       sync.Mutex
	running  bool
	finished chan struct{}

	// Cooldowns belong to destination accounts, so throttling one provider
	// cannot block uploads to an unrelated target.
	cooldownMu sync.Mutex
	cooldowns  map[string]time.Time
}

func New(cfg Config) *Migrator {
	if cfg.PageSize <= 0 {
		cfg.PageSize = 50
	}
	if cfg.CaptchaCooldown == 0 {
		cfg.CaptchaCooldown = 5 * time.Minute
	}
	return &Migrator{
		cfg: cfg,
	}
}

func (m *Migrator) inCooldown(targetID string) (bool, time.Time) {
	m.cooldownMu.Lock()
	defer m.cooldownMu.Unlock()
	until := m.cooldowns[targetID]
	if !time.Now().Before(until) {
		delete(m.cooldowns, targetID)
		return false, time.Time{}
	}
	return true, until
}

func (m *Migrator) setCooldown(targetID string, delay time.Duration) {
	if delay <= 0 {
		return
	}
	m.cooldownMu.Lock()
	defer m.cooldownMu.Unlock()
	if m.cooldowns == nil {
		m.cooldowns = make(map[string]time.Time)
	}
	m.cooldowns[targetID] = time.Now().Add(delay)
}

// RunOnce executes one sweep of configured crawler destinations. Completed
// outcomes are persisted per crawler; failures are returned to orchestration.
func (m *Migrator) RunOnce(ctx context.Context) error {
	if !m.tryBeginRun() {
		return errors.New("已有爬虫上传任务正在运行")
	}
	defer m.finishRun()
	return m.run(ctx, nil)
}

// RunDrives migrates exactly the supplied crawler IDs. The application uses
// this after admitting the same source set, so a crawler attached concurrently
// cannot escape task/configuration coordination.
func (m *Migrator) RunDrives(ctx context.Context, driveIDs []string) error {
	seen := make(map[string]struct{}, len(driveIDs))
	cleaned := make([]string, 0, len(driveIDs))
	for _, driveID := range driveIDs {
		driveID = strings.TrimSpace(driveID)
		if driveID == "" {
			continue
		}
		if _, exists := seen[driveID]; exists {
			continue
		}
		seen[driveID] = struct{}{}
		cleaned = append(cleaned, driveID)
	}
	if len(cleaned) == 0 {
		return nil
	}
	if !m.tryBeginRun() {
		return errors.New("已有爬虫上传任务正在运行")
	}
	defer m.finishRun()
	return m.run(ctx, cleaned)
}

// RunDrive completes an admitted crawl's upload stage. Unlike manual upload
// admission, this waits for the shared migration slot instead of discarding
// the upload stage when another crawler is finishing at the same time.
func (m *Migrator) RunDrive(ctx context.Context, driveID string) error {
	driveID = strings.TrimSpace(driveID)
	if driveID == "" {
		return errors.New("missing crawler drive ID")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		m.mu.Lock()
		if !m.running {
			m.running = true
			m.finished = make(chan struct{})
			m.mu.Unlock()
			break
		}
		finished := m.finished
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-finished:
		}
	}
	defer m.finishRun()
	return m.run(ctx, []string{driveID})
}

// StartDrive 原子地占用迁移器并异步迁移指定的单个爬虫。返回 false 表示
// 此时已有全量或单爬虫迁移在运行；调用方必须把它作为“任务忙”反馈给用户，
// 不能把这次请求报告成已接受。完成通道只会返回一个结果并随后关闭。
func (m *Migrator) StartDrive(ctx context.Context, driveID string) (<-chan error, bool) {
	driveID = strings.TrimSpace(driveID)
	if driveID == "" || !m.tryBeginRun() {
		return nil, false
	}
	result, err := m.acceptDrive(ctx, driveID)
	if err != nil {
		m.finishRun()
		log.Printf("[crawlerupload] admission: %v", err)
		return nil, false
	}
	done := make(chan error, 1)
	go func() {
		err := func() error {
			defer m.finishRun()
			return m.runDrive(ctx, driveID, make(map[string]error), result)
		}()
		done <- err
		close(done)
	}()
	return done, true
}

// tryBeginRun synchronously reserves the one global migration slot. The
// reservation happens before StartDrive reports success, eliminating the old
// race where the HTTP API returned accepted and the background RunOnce then
// silently discovered that another migration was already running.
func (m *Migrator) tryBeginRun() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return false
	}
	m.running = true
	m.finished = make(chan struct{})
	return true
}

func (m *Migrator) finishRun() {
	m.mu.Lock()
	m.running = false
	close(m.finished)
	m.finished = nil
	m.mu.Unlock()
}

func (m *Migrator) reportUploadProgress(progress UploadProgress) {
	if m == nil || m.cfg.OnUploadProgress == nil {
		return
	}
	progress.DriveID = strings.TrimSpace(progress.DriveID)
	if progress.DriveID == "" {
		return
	}
	if progress.State == "" {
		progress.State = "idle"
	}
	m.cfg.OnUploadProgress(progress)
}

func (m *Migrator) resolveTargetID(id string) (string, uploadTarget, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", nil, errors.New("target drive not configured")
	}
	if m.cfg.Registry == nil {
		return "", nil, errors.New("registry not configured")
	}
	d, ok := m.cfg.Registry.Get(id)
	if !ok {
		return "", nil, fmt.Errorf("target drive %q not in registry", id)
	}
	t, err := adaptUploadTarget(d)
	if err != nil {
		return "", nil, err
	}
	return id, t, nil
}

func (m *Migrator) getDrive(ctx context.Context, driveID string) (*catalog.Drive, error) {
	if m != nil && m.cfg.GetDrive != nil {
		return m.cfg.GetDrive(ctx, driveID)
	}
	if m == nil || m.cfg.Catalog == nil {
		return nil, errors.New("catalog not configured")
	}
	return m.cfg.Catalog.GetDrive(ctx, driveID)
}

func (m *Migrator) migrationPlan(ctx context.Context, driveID string) (migrationPlan, error) {
	d, ok := m.cfg.Registry.Get(driveID)
	if !ok {
		return migrationPlan{}, errors.New("爬虫本地存储未挂载")
	}
	src, ok := d.(LocalSource)
	if !ok {
		return migrationPlan{}, errors.New("爬虫不支持本地文件读取")
	}
	row, err := m.getDrive(ctx, driveID)
	if err != nil {
		return migrationPlan{}, err
	}
	if row == nil || row.Kind != scriptcrawler.Kind || !scriptcrawler.IsConfigured(row.Credentials) {
		return migrationPlan{}, errors.New("爬虫配置不可用")
	}
	targetID := strings.TrimSpace(row.Credentials["upload_drive_id"])
	if targetID == "" {
		return migrationPlan{}, errors.New("请先配置上传网盘")
	}
	resolvedID, target, err := m.resolveTargetID(targetID)
	if err != nil {
		return migrationPlan{}, fmt.Errorf("上传目标不可用: %w", err)
	}
	return migrationPlan{
		source: src, row: row, targetDriveID: resolvedID, target: target,
		uploadDir:      scriptCrawlerUploadDir(row.ID),
		uploadProxyURL: strings.TrimSpace(row.Credentials["upload_proxy"]),
	}, nil
}

func scriptCrawlerUploadDir(driveID string) string {
	driveID = sanitizeUploadDirSegment(driveID)
	if driveID == "" {
		driveID = "crawler"
	}
	return scriptCrawlerUploadRootDirName + "/" + driveID
}

func sanitizeUploadDirSegment(raw string) string {
	clean := sanitizeTitle(raw)
	clean = strings.Trim(clean, "/")
	if clean == "." || clean == ".." {
		return ""
	}
	return clean
}

func (m *Migrator) findVideoForLocalFile(ctx context.Context, plan migrationPlan, localFile string) *catalog.Video {
	sourceID := stripExt(localFile)
	driveID := ""
	if plan.source != nil {
		driveID = plan.source.ID()
	}
	id := scriptcrawler.BuildVideoID(driveID, sourceID)
	v, err := m.cfg.Catalog.GetVideo(ctx, id)
	if err == nil && v != nil {
		return v
	}
	return nil
}

// migrateOne uploads or reconciles one validated local video. The boolean
// identifies reuse of an existing remote file, rather than a new upload.
func (m *Migrator) migrateOne(ctx context.Context, v *catalog.Video, plan migrationPlan, parent string) (bool, error) {
	src := plan.source
	pp := plan.target
	path, err := src.VideoPath(v.FileID)
	if err != nil {
		return false, fmt.Errorf("resolve local path: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return false, fmt.Errorf("stat local: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() != v.Size {
		return false, fmt.Errorf("local file invalid: dir=%v size=%d", info.IsDir(), info.Size())
	}

	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open local: %w", err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !os.SameFile(info, opened) || opened.Size() != info.Size() {
		return false, errors.New("本地视频在读取期间发生变化")
	}

	uploadName := desiredUploadName(v.Title, sourceIDForUploadName(v, plan), v.Ext)
	if finder, ok := pp.(existingUploadFinder); ok {
		existing, err := finder.FindExisting(ctx, parent, uploadName, info.Size())
		if err != nil {
			return false, fmt.Errorf("%s reconcile destination: %w", pp.Kind(), err)
		}
		if existing != nil {
			if strings.TrimSpace(existing.FileID) == "" {
				return false, fmt.Errorf("%s reconcile destination returned empty file id", pp.Kind())
			}
			if err := m.completeMigration(ctx, v, plan, *existing, uploadName, parent); err != nil {
				return false, err
			}
			log.Printf("[crawlerupload] %s reconciled existing drive=%s(kind=%s) file=%s name=%q", v.ID, plan.targetDriveID, pp.Kind(), existing.FileID, uploadName)
			return true, nil
		}
	}
	res, err := pp.UploadAndReportHash(ctx, parent, uploadName, io.NewSectionReader(f, 0, info.Size()), info.Size())
	if err != nil {
		return false, fmt.Errorf("%s upload: %w", pp.Kind(), err)
	}
	if res.FileID == "" {
		return false, fmt.Errorf("%s returned empty file id", pp.Kind())
	}

	if err := m.completeMigration(ctx, v, plan, res, uploadName, parent); err != nil {
		return false, err
	}

	log.Printf("[crawlerupload] %s migrated to drive=%s(kind=%s) file=%s name=%q", v.ID, plan.targetDriveID, pp.Kind(), res.FileID, uploadName)
	return false, nil
}

func (m *Migrator) completeMigration(ctx context.Context, v *catalog.Video, plan migrationPlan, res UploadResult, uploadName, parentID string) error {
	// The catalog rewrite is one atomic UPDATE. If the process stops after the
	// remote write but before this point, FindExisting reconciles it next run.
	// Persist the known upload directory now instead of waiting for a later
	// destination-drive scan to repair an incomplete storage identity.
	persistence.RLock()
	defer persistence.RUnlock()
	title := firstNonEmpty(videoname.TitleFromFileName(uploadName), v.Title)
	if err := m.cfg.Catalog.MigrateVideoToDrive(ctx, v.ID, catalog.VideoDriveMigration{
		SourceDriveID: v.DriveID,
		SourceFileID:  v.FileID,
		DriveID:       plan.targetDriveID,
		FileID:        res.FileID,
		ContentHash:   res.Hash,
		ParentID:      strings.TrimSpace(parentID),
		DirName:       uploadDirectoryLabel(plan),
		FileName:      uploadName,
		Title:         title,
	}); err != nil {
		return fmt.Errorf("catalog migrate: %w", err)
	}
	m.preserveCrawledThumbnail(ctx, plan.source, v)

	// 删除本地 mp4 和源 thumb（公共 /p/thumb 副本已在 preserveCrawledThumbnail 中保留）。
	CleanupLocal(plan.source, v.FileID)
	return nil
}

func uploadDirectoryLabel(plan migrationPlan) string {
	clean := strings.Trim(strings.TrimSpace(plan.uploadDir), "/")
	label := strings.TrimSpace(path.Base(clean))
	if label != "" && label != "." {
		return label
	}
	if plan.row != nil {
		return strings.TrimSpace(plan.row.ID)
	}
	return ""
}

func (m *Migrator) bindToExistingTarget(ctx context.Context, v, target *catalog.Video, plan migrationPlan) (bool, error) {
	if v == nil || target == nil || plan.source == nil {
		return false, nil
	}
	if plan.targetDriveID == "" || target.FileID == "" {
		return false, nil
	}
	persistence.RLock()
	defer persistence.RUnlock()
	fileName := firstNonEmpty(target.FileName, v.FileName)
	title := v.Title
	if target.FileName != "" {
		title = firstNonEmpty(videoname.TitleFromFileName(target.FileName), title)
	}
	if err := m.cfg.Catalog.MigrateVideoToDrive(ctx, v.ID, catalog.VideoDriveMigration{
		SourceDriveID:    v.DriveID,
		SourceFileID:     v.FileID,
		DriveID:          plan.targetDriveID,
		FileID:           target.FileID,
		ContentHash:      firstNonEmpty(target.ContentHash, v.ContentHash),
		ParentID:         target.ParentID,
		DirName:          firstNonEmpty(target.DirName, v.DirName),
		AncestorDirNames: target.AncestorDirNames,
		FileName:         fileName,
		Title:            title,
	}); err != nil {
		return false, fmt.Errorf("catalog bind existing target: %w", err)
	}
	m.preserveCrawledThumbnail(ctx, plan.source, v)
	CleanupLocal(plan.source, v.FileID)
	log.Printf("[crawlerupload] %s bound to existing drive=%s(kind=%s) file=%s duplicate=%s", v.ID, plan.targetDriveID, plan.target.Kind(), target.FileID, target.ID)
	return true, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func sourceIDForUploadName(v *catalog.Video, plan migrationPlan) string {
	if v == nil {
		return ""
	}
	prefix := scriptcrawler.Kind + "-" + plan.source.ID() + "-"
	if strings.HasPrefix(v.ID, prefix) {
		return strings.TrimPrefix(v.ID, prefix)
	}
	if v.FileID != "" {
		return stripExt(v.FileID)
	}
	return extractSourceID(v.ID)
}

func (m *Migrator) preserveCrawledThumbnail(ctx context.Context, src LocalSource, v *catalog.Video) {
	if m == nil || m.cfg.Catalog == nil || src == nil || v == nil || v.ID == "" || v.FileID == "" {
		return
	}
	commonDir := strings.TrimSpace(m.cfg.CommonThumbDir)
	if commonDir == "" {
		return
	}
	thumbPath, ok := findCrawlerThumbPath(src, v.FileID)
	if !ok {
		if v.ThumbnailURL == "" {
			log.Printf("[crawlerupload] %s crawled thumbnail missing before migration cleanup", v.ID)
		}
		return
	}
	if err := os.MkdirAll(commonDir, 0o755); err != nil {
		log.Printf("[crawlerupload] %s mkdir common thumbs: %v", v.ID, err)
		return
	}
	dst := mediaasset.ThumbnailPathInDir(commonDir, v.ID)
	if _, err := os.Stat(dst); err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[crawlerupload] %s stat common thumb: %v", v.ID, err)
			return
		}
		if err := mediaasset.NormalizeThumbnailJPEG(thumbPath, dst); err != nil {
			log.Printf("[crawlerupload] %s preserve crawled thumbnail: %v", v.ID, err)
			return
		}
	}
	if err := m.cfg.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{
		ThumbnailURL: "/p/thumb/" + v.ID,
	}); err != nil {
		log.Printf("[crawlerupload] %s update crawled thumbnail url: %v", v.ID, err)
		return
	}
	v.ThumbnailURL = "/p/thumb/" + v.ID
}

func findCrawlerThumbPath(src LocalSource, fileID string) (string, bool) {
	thumbBase := stripExt(fileID)
	for _, ext := range []string{".jpg", ".jpeg", ".png", ".webp"} {
		thumbPath, err := src.ThumbPath(thumbBase + ext)
		if err != nil {
			continue
		}
		info, statErr := os.Stat(thumbPath)
		if statErr == nil && info.Mode().IsRegular() && info.Size() > 0 {
			return thumbPath, true
		}
	}
	return "", false
}

// CleanupLocal 删除已上传视频的本地 mp4 和 thumb。
//
// thumb 删除是 best-effort —— 找不到就算了；逐个尝试常见后缀。
//
// 暴露成包级函数方便 cleanup 模块复用。
func CleanupLocal(src LocalSource, fileID string) {
	videoPath, err := src.VideoPath(fileID)
	if err == nil {
		if err := os.Remove(videoPath); err != nil && !os.IsNotExist(err) {
			log.Printf("[crawlerupload] remove local mp4 %s: %v", videoPath, err)
		}
	}
	// thumb 文件名是 <sourceID>.<ext>；fileID 是 <sourceID>.<videoExt>，
	// 不一定相同。尝试用 fileID 去掉视频扩展名后拼 thumb 常见后缀。
	thumbBase := stripExt(fileID)
	for _, ext := range []string{".jpg", ".jpeg", ".png", ".webp"} {
		thumbPath, err := src.ThumbPath(thumbBase + ext)
		if err != nil {
			continue
		}
		_ = os.Remove(thumbPath) // 忽略错误：找不到很正常
	}
}

func stripExt(name string) string {
	ext := filepath.Ext(name)
	return name[:len(name)-len(ext)]
}

// cleanupOldLocalVideos 是防御性兜底：扫爬虫本地 videos/ 目录，
// 删除所有 catalog 中已经迁移到别处（drive_id != src.ID()）的本地残留。
//
// 与 migrateDrive 的区别：
//   - 不上传任何东西
//   - 只看 catalog 状态，不看 mtime
//
// 正常路径下迁移成功后立刻 CleanupLocal，所以这里
// 应该不会有任何工作。极端情况（手工改 catalog、迁移过程中 crash）才会
// 找到孤儿。
//
// 返回实际删除的文件个数。
func (m *Migrator) cleanupOldLocalVideos(ctx context.Context, plan migrationPlan) (int, error) {
	src := plan.source
	if src == nil {
		return 0, nil
	}
	entries, err := os.ReadDir(src.VideosDir())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	deleted := 0
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		if e.IsDir() {
			continue
		}
		v := m.findVideoForLocalFile(ctx, plan, e.Name())
		if v == nil {
			continue
		}
		if v.DriveID == src.ID() {
			continue
		}
		path, perr := src.VideoPath(e.Name())
		if perr != nil {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("[crawlerupload] cleanup remove %s: %v", path, err)
			continue
		}
		// thumb 一并删（best-effort）
		thumbBase := stripExt(e.Name())
		for _, ext := range []string{".jpg", ".jpeg", ".png", ".webp"} {
			tp, terr := src.ThumbPath(thumbBase + ext)
			if terr != nil {
				continue
			}
			_ = os.Remove(tp)
		}
		deleted++
	}
	return deleted, nil
}
