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
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/drives/googledrive"
	"github.com/video-site/backend/internal/drives/guangyapan"
	"github.com/video-site/backend/internal/drives/onedrive"
	"github.com/video-site/backend/internal/drives/p115"
	"github.com/video-site/backend/internal/drives/p123"
	"github.com/video-site/backend/internal/drives/pikpak"
	"github.com/video-site/backend/internal/drives/quark"
	"github.com/video-site/backend/internal/drives/scriptcrawler"
	"github.com/video-site/backend/internal/drives/webdav"
	"github.com/video-site/backend/internal/drives/wopan"
	"github.com/video-site/backend/internal/mediaasset"
	"github.com/video-site/backend/internal/persistence"
	"github.com/video-site/backend/internal/scopedproxy"
	"github.com/video-site/backend/internal/videoname"
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
	source              LocalSource
	row                 *catalog.Drive
	targetDriveID       string
	target              uploadTarget
	uploadDir           string
	uploadProxyURL      string
	keepLatestN         int
	requireAssetsReady  bool
	requirePreviewReady bool
}

// pikpakAdapter / p115Adapter / p123Adapter / onedriveAdapter / googledriveAdapter / webdavAdapter / wopanAdapter / guangyapanAdapter 把具体 driver 包装成 uploadTarget。
//
// 之所以不让 driver 直接实现 uploadTarget：
//
//  1. 各 driver 的 UploadAndReportXxx 返回的是各自包内的 UploadResult 类型，
//     直接共用同名同签名方法会引入循环依赖；
//  2. driver 包不应该感知 crawlerupload 这一层业务定义。
type pikpakAdapter struct {
	d        *pikpak.Driver
	existing existingUploadCache
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
	d        *p115.Driver
	existing existingUploadCache
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
	d        *p123.Driver
	existing existingUploadCache
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
	d        *onedrive.Driver
	existing existingUploadCache
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
	d        *googledrive.Driver
	existing existingUploadCache
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
	d        *wopan.Driver
	existing existingUploadCache
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

type guangyapanAdapter struct {
	d        *guangyapan.Driver
	existing existingUploadCache
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
	d        *webdav.Driver
	existing existingUploadCache
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
	d        *quark.Driver
	existing existingUploadCache
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
	return findExistingDriveUpload(ctx, a.d, &a.existing, parentID, name, size)
}
func (a *p115Adapter) FindExisting(ctx context.Context, parentID, name string, size int64) (*UploadResult, error) {
	return findExistingDriveUpload(ctx, a.d, &a.existing, parentID, name, size)
}
func (a *p123Adapter) FindExisting(ctx context.Context, parentID, name string, size int64) (*UploadResult, error) {
	return findExistingDriveUpload(ctx, a.d, &a.existing, parentID, name, size)
}
func (a *onedriveAdapter) FindExisting(ctx context.Context, parentID, name string, size int64) (*UploadResult, error) {
	return findExistingDriveUpload(ctx, a.d, &a.existing, parentID, name, size)
}
func (a *googledriveAdapter) FindExisting(ctx context.Context, parentID, name string, size int64) (*UploadResult, error) {
	return findExistingDriveUpload(ctx, a.d, &a.existing, parentID, name, size)
}
func (a *wopanAdapter) FindExisting(ctx context.Context, parentID, name string, size int64) (*UploadResult, error) {
	return findExistingDriveUpload(ctx, a.d, &a.existing, parentID, name, size)
}
func (a *guangyapanAdapter) FindExisting(ctx context.Context, parentID, name string, size int64) (*UploadResult, error) {
	return findExistingDriveUpload(ctx, a.d, &a.existing, parentID, name, size)
}
func (a *webdavAdapter) FindExisting(ctx context.Context, parentID, name string, size int64) (*UploadResult, error) {
	return findExistingDriveUpload(ctx, a.d, &a.existing, parentID, name, size)
}
func (a *quarkAdapter) FindExisting(ctx context.Context, parentID, name string, size int64) (*UploadResult, error) {
	return findExistingDriveUpload(ctx, a.d, &a.existing, parentID, name, size)
}

// adaptUploadTarget 把通用 drive 包装成 uploadTarget。
// 不支持的盘 kind 返回 error；调用方静默跳过。
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
	Catalog  *catalog.Catalog
	Registry Registry
	// GetDrive returns the task-generation configuration snapshot. Production
	// supplies this while deferred admin edits are pending; tests and standalone
	// users may omit it to read Catalog directly.
	GetDrive func(context.Context, string) (*catalog.Drive, error)
	// Interval 已废弃 —— 旧版迁移 worker 是周期 ticker，新版只通过 nightly
	// pipeline 调用 RunOnce，不再有内置定时器。保留字段不删是为了兼容外
	// 部 yaml / 测试代码里仍传值的场景。
	Interval   time.Duration
	BatchLimit int // 单轮最多迁多少个，0 时默认 50
	// KeepLatestN is deprecated. Script crawler uploads use 0 internally so all
	// local videos that satisfy asset requirements are eligible for upload.
	KeepLatestN int
	// CaptchaCooldown 是迁移 worker 在遇到 PikPak captcha 错误（error_code
	// 4002 / 9）后整体进入冷却的时长。冷却期间 runOnce 直接返回，不再发起任何
	// PikPak API 请求，避免被进一步风控。0 时默认 5 分钟；< 0 关闭冷却（仅用于测试）。
	CaptchaCooldown  time.Duration
	CommonThumbDir   string
	OnMigrated       func(videoID string)
	OnUploadProgress func(UploadProgress)
}

type Migrator struct {
	cfg     Config
	mu      sync.Mutex
	running bool

	// cooldownMu 保护 cooldownUntil。captcha 冷却的语义：
	//   - migrateDrive 遇到上传失败且 pikpak.IsCaptchaError(err) == true 时
	//     调 setCooldown，未来 cfg.CaptchaCooldown 内 runOnce 直接 noop
	//   - 一次冷却期内只打印一行进入日志和一行恢复日志，避免之前那种
	//     "每秒一条 4002" 的刷屏
	cooldownMu     sync.Mutex
	cooldownUntil  time.Time
	cooldownLogged bool
}

func New(cfg Config) *Migrator {
	if cfg.BatchLimit == 0 {
		cfg.BatchLimit = 50
	}
	if cfg.KeepLatestN == 0 {
		cfg.KeepLatestN = 15
	}
	if cfg.CaptchaCooldown == 0 {
		cfg.CaptchaCooldown = 5 * time.Minute
	}
	return &Migrator{
		cfg: cfg,
	}
}

// inCooldown 返回当前是否处于 captcha 冷却期，以及冷却结束时间。
// 冷却期间应该跳过整个 runOnce —— 不要列盘、不要尝试上传，
// 让 PikPak 喘口气。
func (m *Migrator) inCooldown() (bool, time.Time) {
	m.cooldownMu.Lock()
	defer m.cooldownMu.Unlock()
	return time.Now().Before(m.cooldownUntil), m.cooldownUntil
}

// cooldownState 返回当前冷却状态。若发现冷却已经过期，会清掉状态并让
// 调用方打印一次恢复日志。
func (m *Migrator) cooldownState() (active bool, until time.Time, resumed bool) {
	m.cooldownMu.Lock()
	defer m.cooldownMu.Unlock()
	if m.cooldownUntil.IsZero() {
		return false, time.Time{}, false
	}
	until = m.cooldownUntil
	if time.Now().Before(until) {
		return true, until, false
	}
	m.cooldownUntil = time.Time{}
	m.cooldownLogged = false
	return false, until, true
}

// setCooldown 把冷却结束时间往后推 cfg.CaptchaCooldown，并返回结束时间。
// 当 cfg.CaptchaCooldown < 0（仅测试用）时不改任何状态、返回零值。
func (m *Migrator) setCooldown() time.Time {
	if m.cfg.CaptchaCooldown < 0 {
		return time.Time{}
	}
	m.cooldownMu.Lock()
	defer m.cooldownMu.Unlock()
	m.cooldownUntil = time.Now().Add(m.cfg.CaptchaCooldown)
	m.cooldownLogged = false
	return m.cooldownUntil
}

// markCooldownLogged 是 runOnce 用来只打一次"在冷却中"日志的小工具。
// 第一次返回 false（应该打），第二次起返回 true（不再打），冷却到期 / 重新设置时复位。
func (m *Migrator) markCooldownLogged() bool {
	m.cooldownMu.Lock()
	defer m.cooldownMu.Unlock()
	if m.cooldownLogged {
		return true
	}
	m.cooldownLogged = true
	return false
}

// RunOnce 跑一次完整迁移：列出所有配置了 upload_drive_id 的 scriptcrawler
// drive，把本地视频上传到目标 drive，事务性改写 catalog 行，删本地文件。
//
// 这是上层 nightly 流水线 Phase 3 的入口；不再有周期 ticker / Trigger 通道。
// captcha cooldown 状态在单次 RunOnce 内仍生效（多 drive 时遇到 4002 立即停整轮）；
// 跨调用持久 5 分钟，下次 RunOnce 命中冷却期会直接 noop。
//
// 当前实现不会向调用方返回 error —— 单条迁移失败已在内部记日志并跳过；
// 整轮被 cooldown / context 取消时也通过日志可观测。保留 error 返回签名是为
// 给未来需要把 nightly 失败状态展示给 admin 用。
func (m *Migrator) RunOnce(ctx context.Context) error {
	if !m.tryBeginRun() {
		return nil
	}
	defer m.finishRun()
	m.run(ctx, nil)
	return nil
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
	if len(cleaned) == 0 || !m.tryBeginRun() {
		return nil
	}
	defer m.finishRun()
	m.run(ctx, cleaned)
	return nil
}

// StartDrive 原子地占用迁移器并异步迁移指定的单个爬虫。返回 false 表示
// 此时已有全量或单爬虫迁移在运行；调用方必须把它作为“任务忙”反馈给用户，
// 不能把这次请求报告成已接受。完成通道只会返回一个结果并随后关闭。
func (m *Migrator) StartDrive(ctx context.Context, driveID string) (<-chan error, bool) {
	driveID = strings.TrimSpace(driveID)
	if driveID == "" || !m.tryBeginRun() {
		return nil, false
	}
	done := make(chan error, 1)
	go func() {
		func() {
			defer m.finishRun()
			m.run(ctx, []string{driveID})
		}()
		done <- nil
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
	return true
}

func (m *Migrator) finishRun() {
	m.mu.Lock()
	m.running = false
	m.mu.Unlock()
}

// run executes either the global nightly migration (driveIDs nil) or exactly
// the explicitly selected crawler set.
func (m *Migrator) run(ctx context.Context, driveIDs []string) {

	// captcha 冷却期间整轮跳过 —— 不做任何 PikPak API 调用、不做本地清理，
	// 等冷却结束。这样从用户视角看：进入冷却 → 一行日志 → 完全静默 → 冷却
	// 结束自然恢复。避免之前每秒一条 4002 的日志雪崩。
	if active, until, resumed := m.cooldownState(); active {
		if !m.markCooldownLogged() {
			log.Printf("[crawlerupload] captcha cooldown active until %s, skipping run", until.Format(time.RFC3339))
		}
		return
	} else if resumed {
		log.Printf("[crawlerupload] captcha cooldown ended at %s, resuming migration", until.Format(time.RFC3339))
	}

	var plans []migrationPlan
	if driveIDs == nil {
		plans = m.migrationPlans(ctx)
	} else {
		plans = make([]migrationPlan, 0, len(driveIDs))
		for _, driveID := range driveIDs {
			plans = append(plans, m.migrationPlansForDrive(ctx, driveID)...)
		}
	}
	if len(plans) == 0 {
		// 没目标就静默 —— 用户选择了本地保存，或目标盘还没挂载。
		return
	}

	migrated := 0
	for _, plan := range plans {
		if err := ctx.Err(); err != nil {
			return
		}
		n, err := m.migrateDrive(ctx, plan)
		if err != nil {
			log.Printf("[crawlerupload] drive=%s migrate batch error: %v", plan.source.ID(), err)
			if _, rateLimited := drives.RateLimitRetryAfter(err); rateLimited {
				migrated += n
				if migrated > 0 {
					log.Printf("[crawlerupload] migrated %d video(s)", migrated)
				}
				return
			}
		}
		migrated += n
		if active, _ := m.inCooldown(); active {
			if migrated > 0 {
				log.Printf("[crawlerupload] migrated %d video(s)", migrated)
			}
			return
		}
	}
	if migrated > 0 {
		log.Printf("[crawlerupload] migrated %d video(s)", migrated)
	}

	// 收尾：扫每个本地爬虫 drive 的 videos 目录，把 catalog 已经迁到别处但本地
	// 仍有残留的孤儿文件清掉。这是纯防御性兜底——正常路径下 migrateDrive
	// 已经在迁移成功后立刻 CleanupLocal，不会留孤儿。
	for _, plan := range plans {
		if err := ctx.Err(); err != nil {
			return
		}
		deleted, err := m.cleanupOldLocalVideos(ctx, plan)
		if err != nil {
			log.Printf("[crawlerupload] cleanup drive=%s: %v", plan.source.ID(), err)
		}
		if deleted > 0 {
			log.Printf("[crawlerupload] cleanup drive=%s deleted %d orphan local file(s)", plan.source.ID(), deleted)
		}
	}
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

func (m *Migrator) migrationPlans(ctx context.Context) []migrationPlan {
	if m == nil || m.cfg.Catalog == nil || m.cfg.Registry == nil {
		return nil
	}
	all := m.cfg.Registry.All()
	out := make([]migrationPlan, 0, len(all))
	for _, d := range all {
		if plan, ok := m.migrationPlan(ctx, d); ok {
			out = append(out, plan)
		}
	}
	return out
}

func (m *Migrator) migrationPlansForDrive(ctx context.Context, driveID string) []migrationPlan {
	if m == nil || m.cfg.Catalog == nil || m.cfg.Registry == nil {
		return nil
	}
	d, ok := m.cfg.Registry.Get(strings.TrimSpace(driveID))
	if !ok {
		return nil
	}
	plan, ok := m.migrationPlan(ctx, d)
	if !ok {
		return nil
	}
	return []migrationPlan{plan}
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

func (m *Migrator) migrationPlan(ctx context.Context, d drives.Drive) (migrationPlan, bool) {
	if d == nil {
		return migrationPlan{}, false
	}
	src, ok := d.(LocalSource)
	if !ok {
		return migrationPlan{}, false
	}
	row, err := m.getDrive(ctx, d.ID())
	if err != nil || row == nil || row.Kind != scriptcrawler.Kind || !scriptcrawler.IsConfigured(row.Credentials) {
		return migrationPlan{}, false
	}
	targetID := strings.TrimSpace(row.Credentials["upload_drive_id"])
	if targetID == "" {
		return migrationPlan{}, false
	}
	resolvedID, target, err := m.resolveTargetID(targetID)
	if err != nil {
		log.Printf("[crawlerupload] crawler=%s upload target=%q unavailable: %v", row.ID, targetID, err)
		return migrationPlan{}, false
	}
	return migrationPlan{
		source:              src,
		row:                 row,
		targetDriveID:       resolvedID,
		target:              target,
		uploadDir:           scriptCrawlerUploadDir(row.ID),
		uploadProxyURL:      strings.TrimSpace(row.Credentials["upload_proxy"]),
		keepLatestN:         0,
		requireAssetsReady:  true,
		requirePreviewReady: row.TeaserEnabled,
	}, true
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

// migrateDrive 对单个本地爬虫 drive 跑一批迁移；返回成功迁移的条数。
func (m *Migrator) migrateDrive(ctx context.Context, plan migrationPlan) (int, error) {
	src := plan.source
	if src == nil || plan.target == nil || plan.targetDriveID == "" {
		return 0, nil
	}
	uploadCtx, err := scopedproxy.WithURL(ctx, plan.uploadProxyURL)
	if err != nil {
		return 0, fmt.Errorf("invalid crawler upload proxy: %w", err)
	}
	keepN := plan.keepLatestN
	if keepN < 0 {
		keepN = 0
	}

	type localFile struct {
		name    string
		modTime time.Time
	}

	entries, err := os.ReadDir(src.VideosDir())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read videos dir: %w", err)
	}

	files := make([]localFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, localFile{name: e.Name(), modTime: info.ModTime()})
	}

	if plan.keepLatestN >= 0 && len(files) <= keepN {
		return 0, nil
	}

	sort.Slice(files, func(i, j int) bool { return files[i].modTime.After(files[j].modTime) })

	skip := keepN
	if plan.keepLatestN < 0 {
		skip = 0
	}
	candidates := files
	if skip < len(files) {
		candidates = files[skip:]
	} else {
		m.reportUploadProgress(UploadProgress{DriveID: src.ID(), State: "idle"})
		return 0, nil
	}
	totalCandidates := len(candidates)
	m.reportUploadProgress(UploadProgress{
		DriveID:     src.ID(),
		State:       "uploading",
		QueueLength: totalCandidates,
		TotalCount:  totalCandidates,
	})
	defer m.reportUploadProgress(UploadProgress{DriveID: src.ID(), State: "idle"})

	localVideos, err := m.cfg.Catalog.ListVideosByDriveID(ctx, src.ID(), 100000)
	if err != nil {
		return 0, fmt.Errorf("list local catalog videos: %w", err)
	}
	byFileID := make(map[string]*catalog.Video, len(localVideos))
	for _, v := range localVideos {
		if v != nil && strings.TrimSpace(v.FileID) != "" {
			byFileID[v.FileID] = v
		}
	}

	migrated := 0
	processed := 0
	uploadParentID := ""
	for index, f := range candidates {
		if err := ctx.Err(); err != nil {
			return migrated, err
		}
		if migrated >= m.cfg.BatchLimit {
			break
		}

		v := m.findVideoForLocalFile(ctx, plan, f.name, byFileID)
		if v == nil {
			processed++
			m.reportUploadProgress(UploadProgress{
				DriveID:     src.ID(),
				State:       "uploading",
				QueueLength: maxInt(totalCandidates-processed, 0),
				DoneCount:   processed,
				TotalCount:  totalCandidates,
			})
			continue
		}
		m.reportUploadProgress(UploadProgress{
			DriveID:      src.ID(),
			State:        "uploading",
			CurrentTitle: v.Title,
			QueueLength:  maxInt(totalCandidates-index-1, 0),
			DoneCount:    processed,
			TotalCount:   totalCandidates,
		})

		if v.DriveID != src.ID() {
			CleanupLocal(src, f.name)
			processed++
			m.reportUploadProgress(UploadProgress{
				DriveID:     src.ID(),
				State:       "uploading",
				QueueLength: maxInt(totalCandidates-processed, 0),
				DoneCount:   processed,
				TotalCount:  totalCandidates,
			})
			continue
		}

		if targetDuplicate, err := m.cfg.Catalog.FindEquivalentVideoOnDrive(ctx, v, plan.targetDriveID); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				log.Printf("[crawlerupload] %s find target duplicate: %v", v.ID, err)
			}
		} else if targetDuplicate != nil {
			ok, err := m.bindToExistingTarget(ctx, v, targetDuplicate, plan)
			if err != nil {
				log.Printf("[crawlerupload] %s: %v", v.ID, err)
				continue
			}
			if ok {
				migrated++
				if m.cfg.OnMigrated != nil {
					m.cfg.OnMigrated(v.ID)
				}
			}
			processed++
			m.reportUploadProgress(UploadProgress{
				DriveID:     src.ID(),
				State:       "uploading",
				QueueLength: maxInt(totalCandidates-processed, 0),
				DoneCount:   processed,
				TotalCount:  totalCandidates,
			})
			continue
		}

		if plan.requireAssetsReady {
			ready, err := m.crawlerVideoAssetsReady(ctx, v, plan.requirePreviewReady)
			if err != nil {
				log.Printf("[crawlerupload] %s check generated assets: %v", v.ID, err)
				continue
			}
			if !ready {
				processed++
				m.reportUploadProgress(UploadProgress{
					DriveID:     src.ID(),
					State:       "uploading",
					QueueLength: maxInt(totalCandidates-processed, 0),
					DoneCount:   processed,
					TotalCount:  totalCandidates,
				})
				continue
			}
		}

		if uploadParentID == "" {
			uploadParentID, err = plan.target.EnsureDir(uploadCtx, plan.uploadDir)
			if err != nil {
				return migrated, fmt.Errorf("%s ensure %q dir: %w", plan.target.Kind(), plan.uploadDir, err)
			}
			if strings.TrimSpace(uploadParentID) == "" {
				return migrated, fmt.Errorf("%s ensure %q dir returned empty id", plan.target.Kind(), plan.uploadDir)
			}
		}
		ok, err := m.migrateOne(uploadCtx, v, plan, uploadParentID)
		if err != nil {
			log.Printf("[crawlerupload] %s: %v", v.ID, err)
			if _, rateLimited := drives.RateLimitRetryAfter(err); rateLimited {
				// Provider throttling applies to the batch. Retrying the same
				// operation for every remaining video only deepens the throttle.
				return migrated, err
			}
			// captcha 错误（4002 / 9）说明 PikPak 当前正拒绝我们；继续在
			// 同一轮里尝试其它文件大概率会拿到同样的 4002，并且每多一次
			// 失败就多一份"被风控加深"的风险。立即中止当前 batch 并
			// 打开冷却窗口，等 cfg.CaptchaCooldown 之后再重试。
			if pikpak.IsCaptchaError(err) {
				until := m.setCooldown()
				log.Printf("[crawlerupload] drive=%s captcha-blocked, cooling down until %s", src.ID(), until.Format(time.RFC3339))
				return migrated, nil
			}
			continue
		}
		if ok {
			migrated++
			if m.cfg.OnMigrated != nil {
				m.cfg.OnMigrated(v.ID)
			}
		}
		processed++
		m.reportUploadProgress(UploadProgress{
			DriveID:     src.ID(),
			State:       "uploading",
			QueueLength: maxInt(totalCandidates-processed, 0),
			DoneCount:   processed,
			TotalCount:  totalCandidates,
		})
	}
	return migrated, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (m *Migrator) findVideoForLocalFile(ctx context.Context, plan migrationPlan, localFile string, byFileID map[string]*catalog.Video) *catalog.Video {
	if v := byFileID[localFile]; v != nil {
		return v
	}
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

func (m *Migrator) crawlerVideoAssetsReady(ctx context.Context, v *catalog.Video, requirePreview bool) (bool, error) {
	if v == nil {
		return false, nil
	}
	fingerprintReady := strings.EqualFold(strings.TrimSpace(v.FingerprintStatus), "ready") || strings.TrimSpace(v.SampledSHA256) != ""
	if !fingerprintReady {
		return false, nil
	}
	if !requirePreview {
		return true, nil
	}
	if strings.EqualFold(strings.TrimSpace(v.PreviewStatus), "ready") {
		return true, nil
	}
	return m.cfg.Catalog.HasReadyEquivalentPreview(ctx, v)
}

// migrateOne 把单条本地爬虫视频上传到目标盘并改写 catalog。
// 返回 (true, nil) 表示真的迁了一条；(false, nil) 表示跳过（本地文件已不在等）；
// (false, err) 表示真出错。
func (m *Migrator) migrateOne(ctx context.Context, v *catalog.Video, plan migrationPlan, parent string) (bool, error) {
	src := plan.source
	pp := plan.target
	path, err := src.VideoPath(v.FileID)
	if err != nil {
		return false, fmt.Errorf("resolve local path: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 本地文件被人手动删了，但 catalog 还指向该爬虫；
			// 这种状态没法上传。跳过即可（保留行让管理员可见，避免数据丢失）。
			return false, nil
		}
		return false, fmt.Errorf("stat local: %w", err)
	}
	if info.IsDir() || info.Size() == 0 {
		return false, fmt.Errorf("local file invalid: dir=%v size=%d", info.IsDir(), info.Size())
	}

	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open local: %w", err)
	}
	defer f.Close()

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
	res, err := pp.UploadAndReportHash(ctx, parent, uploadName, f, info.Size())
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
	return true, nil
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
		DriveID:     plan.targetDriveID,
		FileID:      res.FileID,
		ContentHash: res.Hash,
		ParentID:    strings.TrimSpace(parentID),
		DirName:     uploadDirectoryLabel(plan),
		FileName:    uploadName,
		Title:       title,
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
		DriveID:     plan.targetDriveID,
		FileID:      target.FileID,
		ContentHash: firstNonEmpty(target.ContentHash, v.ContentHash),
		ParentID:    target.ParentID,
		DirName:     firstNonEmpty(target.DirName, v.DirName),
		FileName:    fileName,
		Title:       title,
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
//   - 不依赖 KeepLatestN —— 哪怕这个孤儿在"最新 N"窗口内，已迁移就该删
//   - 只看 catalog 状态，不看 mtime
//
// 正常路径下 migrateDrive 迁移成功后立刻 CleanupLocal，所以这里
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
		v := m.findVideoForLocalFile(ctx, plan, e.Name(), nil)
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
