package main

import (
	"context"
	"sync"
	"time"

	"github.com/video-site/backend/internal/api"
	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/config"
	"github.com/video-site/backend/internal/drives/scriptcrawler"
	"github.com/video-site/backend/internal/fingerprint"
	"github.com/video-site/backend/internal/nightly"
	"github.com/video-site/backend/internal/preview"
	"github.com/video-site/backend/internal/proxy"
	"github.com/video-site/backend/internal/transcode"
)

type App struct {
	cfg      *config.Config
	cat      *catalog.Catalog
	registry *proxy.Registry
	proxy    *proxy.Proxy

	mu                 sync.Mutex
	workers            map[string]*preview.Worker
	thumbWorkers       map[string]*preview.ThumbWorker
	fingerprintWorkers map[string]*fingerprint.Worker
	cancels            map[string]context.CancelFunc
	// scriptCrawlers 按 driveID 索引，每个脚本爬虫 drive 独立一个 Crawler。
	scriptCrawlers map[string]*scriptcrawler.Crawler

	// driveAttachMu 串行化云盘挂载/重挂载。挂载会访问上游服务，可能较慢；
	// 串行化可以避免启动后台挂载和手动扫盘按需挂载同一个 drive 时重复创建 worker。
	driveAttachMu sync.Mutex

	// 全站主题（"dark" | "pink" | "sky"），从 DB 读
	theme string

	// configManager 将 config.yaml 的已校验热更新字段发布给运行时消费者；
	// 文件本身是唯一持久化事实来源。
	configManager *config.Manager
	// onTagsChanged invalidates the API read cache after a config-driven tag
	// catalog mutation. It is installed before the HTTP server starts serving.
	onTagsChanged func()

	// crawlerUploader 把脚本爬虫保存在本地的视频上传到每个爬虫配置的目标 drive。
	crawlerUploader crawlerUploadRunner

	// nightlyRunner 协调两种互斥任务：定时完整流水线，以及 admin 手动触发的
	// “扫所有真实网盘 → 等新视频资产 → 去重”精简流水线。
	nightlyRunner *nightly.Runner

	// scanQueueMu 保护 scanQueued 和 scanProgress。
	scanQueueMu sync.Mutex
	// scanQueued 跟踪哪些 driveID 已经排队或正在跑扫盘/爬取，去重后续重复点击。
	// 不同 drive 互不等待，可以并行扫；同一个 drive 只能有一个扫盘/抓取任务。
	scanQueued map[string]bool
	// scanProgress 跟踪每个正在扫盘/抓取的 drive 当前进度。
	scanProgress map[string]driveScanProgress

	// taskCancelMu 保护 driveTaskCancels。这里登记的是可被"停止任务"按钮中断
	// 的 drive 级任务上下文：扫盘、91 爬取、指纹补队列、失败生成重试等。
	taskCancelMu       sync.Mutex
	driveTaskCancelSeq uint64
	driveTaskCancels   map[string]map[uint64]context.CancelFunc

	// fingerprintQueueing 去重每个 drive 的 pending 指纹补队列任务，避免定时
	// reconcile 和扫盘结束同时为同一批 pending 视频启动多个长时间入队 goroutine。
	fingerprintQueueMu  sync.Mutex
	fingerprintQueueing map[string]bool

	// crawlerUploadRunning 去重管理员手动发起的单爬虫上传任务。
	crawlerUploadMu      sync.Mutex
	crawlerUploadRunning map[string]bool

	// uploadProgress 跟踪脚本爬虫迁移到云盘时的实时上传状态。
	uploadProgressMu sync.Mutex
	uploadProgress   map[string]driveUploadProgress

	// transcodeMu 保护 transcodeWorkers / transcodeCancels。
	// 浏览器兼容性转码每盘最多一个任务，且只能由管理员手动开启
	// （不随扫盘/夜间流水线自动运行），手动停止或处理完即从 map 清除。
	transcodeMu      sync.Mutex
	transcodeWorkers map[string]*transcode.Worker
	transcodeCancels map[string]context.CancelFunc

	// blacklistSourceDeleteMu protects the one-at-a-time background job that
	// removes source files for tombstoned videos. The job reads tombstones from
	// the catalog and purges each one after a successful provider delete.
	blacklistSourceDeleteMu    sync.Mutex
	blacklistSourceDeleteState api.BlacklistSourceDeleteStatus
	// blacklistVideoLocks serializes restore and source deletion for the same
	// tombstone without making unrelated videos wait on a slow provider call.
	blacklistVideoLocks videoOperationLocks

	// tagJobMu protects the admin-visible tag job status. tagMaintenanceMu
	// serializes bulk writes to video_tags across startup, manual, and nightly
	// maintenance paths.
	tagJobMu         sync.Mutex
	tagMaintenanceMu sync.Mutex
	tagJobState      api.TagJobStatus
}

type videoOperationLocks struct {
	mu    sync.Mutex
	items map[string]*videoOperationLock
}

type videoOperationLock struct {
	mu   sync.Mutex
	refs int
}

func (locks *videoOperationLocks) lock(videoID string) func() {
	locks.mu.Lock()
	if locks.items == nil {
		locks.items = make(map[string]*videoOperationLock)
	}
	item := locks.items[videoID]
	if item == nil {
		item = &videoOperationLock{}
		locks.items[videoID] = item
	}
	item.refs++
	locks.mu.Unlock()

	item.mu.Lock()
	return func() {
		item.mu.Unlock()
		locks.mu.Lock()
		item.refs--
		if item.refs == 0 {
			delete(locks.items, videoID)
		}
		locks.mu.Unlock()
	}
}

type driveScanProgress struct {
	Scanned       int
	Added         int
	CooldownUntil time.Time
}

type driveUploadProgress struct {
	State        string
	CurrentTitle string
	QueueLength  int
	DoneCount    int
	TotalCount   int
}

type crawlerUploadRunner interface {
	RunOnce(ctx context.Context) error
	StartDrive(ctx context.Context, driveID string) (<-chan error, bool)
}
