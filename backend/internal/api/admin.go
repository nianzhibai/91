package api

import (
	"context"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/auth"
	"github.com/nianzhibai/91/backend/internal/backup"
	"github.com/nianzhibai/91/backend/internal/backuptransfer"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
	"github.com/nianzhibai/91/backend/internal/drivelogin"
	"github.com/nianzhibai/91/backend/internal/driveview"
	"github.com/nianzhibai/91/backend/internal/frontend"
	"github.com/nianzhibai/91/backend/internal/mediaimport"
	"github.com/nianzhibai/91/backend/internal/preview"
	"github.com/nianzhibai/91/backend/internal/scanjob"
	"github.com/nianzhibai/91/backend/internal/telegram"
)

type DriveConfigUpdateScope uint8

const (
	// DriveConfigUpdateRuntime covers kind, root, credentials, and provider
	// options captured by a mounted Driver.
	DriveConfigUpdateRuntime DriveConfigUpdateScope = 1 << iota
	// DriveConfigUpdateScan covers skip-directory settings.
	DriveConfigUpdateScan
	// DriveConfigUpdateDestructive reserves deletion. It blocks admissions while
	// the API cancels and drains existing work; it is never coalesced as a normal
	// deferred configuration callback.
	DriveConfigUpdateDestructive
)

// DriveConfigUpdateLease serializes configuration writers for one drive.
// Authorize reserves the transition before persistence. For configuration,
// Commit applies the runtime callback immediately or coalesces it until the
// current task generation drains. For deletion, Commit permanently retires the
// old task gate after cleanup and row deletion succeed. Release is mandatory.
type DriveConfigUpdateLease interface {
	Authorize(scope DriveConfigUpdateScope) (busyReason string)
	Commit(scope DriveConfigUpdateScope, apply func() error) (deferred bool, err error)
	Release()
}

type AdminServer struct {
	Frontend           *frontend.Manager
	Telegram           *telegram.Integration
	Imports            *mediaimport.Manager
	Catalog            *catalog.Catalog
	driveSnapshotsOnce sync.Once
	driveSnapshots     *driveview.Service
	Auth               *auth.Authenticator
	Backups            *backup.Manager
	BackupTransfers    *backuptransfer.Manager
	// Logs is the durable runtime log store exposed only through the
	// administrator-authenticated routes below.
	Logs *applog.Store
	// ConfigManager owns the real config.yaml management surface. Both visual
	// and source editors read and write this same document.
	ConfigManager *config.Manager
	// PreviewEnabled reads the global preview switch, the same runtime source
	// used by resource generation. Nil enables preview endpoints.
	PreviewEnabled func() bool
	GPU            *preview.Hardware
	// VersionFilePath points to the installer-written .version file.
	VersionFilePath string
	// ImageVersion is the Docker image version injected at build/runtime.
	// It takes precedence over VersionFilePath because Docker data volumes can
	// keep an older .version file across image upgrades.
	ImageVersion string
	// GitHubRepo is the owner/name repo used for update checks.
	GitHubRepo string
	// ReleaseAPIURL and HTTPClient are injectable for tests. Production code leaves them empty.
	ReleaseAPIURL string
	HTTPClient    *http.Client
	// LocalPreviewDir is the local directory that stores generated preview videos and thumbs.
	LocalPreviewDir string
	// Hooks：外层注入实际执行者。
	// BeginDriveConfigUpdate reserves the per-drive writer slot before the API
	// reads its current row. Active tasks keep their old snapshot; Commit applies
	// immediately when idle or coalesces the latest update until those tasks drain.
	BeginDriveConfigUpdate func(driveID string) (lease DriveConfigUpdateLease, busyReason string)
	// OnDriveRuntimeConfigChanged 只在新建网盘，或 kind/root/credentials 等
	// Driver 构造参数发生变化时调用。名称、跳过目录等纯元数据保存不得触发重挂载。
	OnDriveRuntimeConfigChanged func(driveID string) error
	// OnDriveGenerationParallelismChanged applies a saved generation
	// parallelism to the drive's running generation.
	OnDriveGenerationParallelismChanged func(driveID string) error
	// OnPrepareDriveDelete cancels all work for the drive and returns only after
	// every tracked task has exited. The destructive lease is already blocking
	// new admissions when this hook runs.
	OnPrepareDriveDelete       func(ctx context.Context, driveID string) error
	OnDriveDeleteCleanup       func(ctx context.Context, driveID string) (int, error)
	OnDriveRemoved             func(driveID string)
	OnScanRequested            func(context.Context, string) bool
	OnCrawlerRunRequested      func(context.Context, string) (string, error)
	OnCrawlerTaskCancel        func(string, string) bool
	OnCrawlerUploadRequested   func(driveID string) (bool, string)
	OnStopDriveTasks           func(driveID string) bool
	OnStopAllTasks             func() int
	OnRegenPreview             func(videoID string)
	OnRegenAllPreviews         func()
	OnDriveGenerationRequested func(context.Context, string, DriveGenerationKind) (DriveGenerationResult, error)
	OnDeleteMergedSource       func(context.Context, string, string) error
	// OnMergeDuplicateReview merges a confirmed review pair (keep, merge) and
	// cleans up the merged video's generated assets.
	OnMergeDuplicateReview         func(context.Context, string, string) error
	OnDeleteVideo                  func(ctx context.Context, videoID string, options catalog.VideoDeletionOptions) (DeleteVideoResult, error)
	OnStartBlacklistSourceDelete   func(BlacklistSourceDeleteRequest) bool
	GetBlacklistSourceDeleteStatus func() BlacklistSourceDeleteStatus
	// OnRemoveBlacklist owns the complete application-level restore operation:
	// provider inspection, catalog mutation, source-delete coordination, and any
	// post-restore generation/cache work. Tests may omit it for non-direct paths.
	OnRemoveBlacklist func(ctx context.Context, videoID string) error
	OnStartTagRetag   func() bool
	// OnTagsChanged invalidates read-side tag caches after a catalog mutation.
	OnTagsChanged                func()
	GetTagJobStatus              func() TagJobStatus
	GetDriveGenerationStatuses   func() map[string]DriveGenerationStatuses
	GetDriveGenerationStatus     func(string) DriveGenerationStatuses
	GetPreviewGenerationVideoIDs func() map[string]bool
	// Theme 读写（"dark" | "light"）
	GetTheme func() string
	SetTheme func(theme string) error
	// OnRunScanAllJob 触发一次手动全量扫盘：只扫描配置的真实云盘、等待新视频
	// 资产生成并运行视频去重。爬虫、迁移和恢复只属于定时 nightly 流水线。
	// 立即返回；若共享 Runner 正在跑或已排队，会拒绝重复触发。
	OnRunScanAllJob func() bool
	// GetNightlyJobStatus 返回共享维护 Runner 的状态，用于避免手动扫盘、单盘任务
	// 和定时 nightly 流水线互相重叠。
	GetNightlyJobStatus func() NightlyJobStatus
	// ListDriveDirChildren 列出某个 drive 在 parentID 目录下的直接子目录。
	// parentID 为空时使用 drive 的 RootID。返回 (子目录列表, error)。
	// 用于"设置跳过目录"弹窗按需展开浏览网盘目录树；只返回目录条目，文件忽略。
	// 调用方应当处理 error 并以 5xx 返回前端。
	ListDriveDirChildren func(ctx context.Context, driveID, parentID string) ([]DriveDirEntry, error)
	// 扫码登录的上游地址与 HTTP 客户端只供测试注入；生产留空使用各网盘官方接口。
	P123UserAPIBaseURL       string
	P123HTTPClient           *http.Client
	P115QRHTTPClient         *http.Client
	QuarkQRUOPBaseURL        string
	QuarkQRPanBaseURL        string
	QuarkQRHTTPClient        *http.Client
	Yun139QRHTTPClient       *http.Client
	WopanQRAPIBaseURL        string
	WopanQRHTTPClient        *http.Client
	GuangYaPanAccountBaseURL string
	GuangYaPanHTTPClient     *http.Client
	driveLoginOnce           sync.Once
	driveLoginManager        *drivelogin.Manager
}

const (
	driveTaskBusyMessage = "当前存储有正在进行的任务，请稍后重试"
	fullScanBusyMessage  = "当前有全量扫描任务正在进行，请稍后重试"
)

// DriveDirEntry 是 dirtree 接口的一条返回项：网盘上的一个目录节点。
type DriveDirEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type GenerationStatus struct {
	Result        *scanjob.Result `json:"result,omitempty"`
	State         string          `json:"state"`
	CurrentTitle  string          `json:"currentTitle,omitempty"`
	QueueLength   int             `json:"queueLength"`
	CooldownUntil string          `json:"cooldownUntil,omitempty"`
	ScannedCount  int             `json:"scannedCount"`
	AddedCount    int             `json:"addedCount"`
	DoneCount     int             `json:"doneCount"`
	TotalCount    int             `json:"totalCount"`
}

type DriveGenerationStatuses struct {
	Scan        GenerationStatus `json:"scan"`
	Thumbnail   GenerationStatus `json:"thumbnail"`
	Preview     GenerationStatus `json:"preview"`
	Fingerprint GenerationStatus `json:"fingerprint"`
	Upload      GenerationStatus `json:"upload"`
}

type NightlyJobStatus struct {
	Outcome        scanjob.State    `json:"outcome,omitempty"`
	ScanResults    []scanjob.Result `json:"scanResults,omitempty"`
	Issues         []scanjob.Issue  `json:"issues,omitempty"`
	State          string           `json:"state"`
	Running        bool             `json:"running"`
	Queued         bool             `json:"queued"`
	StartedAt      string           `json:"startedAt,omitempty"`
	LastFinishedAt string           `json:"lastFinishedAt,omitempty"`
}

type TagJobStatus struct {
	State          string `json:"state"`
	Running        bool   `json:"running"`
	Kind           string `json:"kind,omitempty"`
	Total          int    `json:"total"`
	Processed      int    `json:"processed"`
	LastError      string `json:"lastError,omitempty"`
	StartedAt      string `json:"startedAt,omitempty"`
	LastFinishedAt string `json:"lastFinishedAt,omitempty"`
}

type BlacklistSourceDeleteStatus struct {
	State        string `json:"state"`
	Running      bool   `json:"running"`
	Pending      int    `json:"pending"`
	Total        int    `json:"total"`
	Processed    int    `json:"processed"`
	Deleted      int    `json:"deleted"`
	Skipped      int    `json:"skipped"`
	Failed       int    `json:"failed"`
	CurrentFile  string `json:"currentFile,omitempty"`
	LastError    string `json:"lastError,omitempty"`
	StartedAt    string `json:"startedAt,omitempty"`
	LastFinished string `json:"lastFinishedAt,omitempty"`
}

type BlacklistSourceDeleteRequest struct {
	DeleteAllSources bool     `json:"deleteAllSources,omitempty"`
	IDs              []string `json:"ids,omitempty"`
}

const maxCrawlerScriptBytes = 2 * 1024 * 1024

type DeleteVideoResult struct {
	OK            bool `json:"ok"`
	DeletedSource bool `json:"deletedSource"`
}

// Register mounts identity routes and the administrator API. Each of
// adminRoutes adds routes relative to /admin/api behind the administrator check.
func (a *AdminServer) Register(r chi.Router, adminRoutes ...func(chi.Router)) {
	// Identity belongs to every client, independently of the management UI.
	r.Route("/api/auth", a.registerAuthRoutes)
	r.Get("/api/server", a.handleServerInfo)
	r.Get("/api/openapi.json", handleOpenAPI)
	r.Route("/admin/api", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(a.Auth.AdminRequired)
			for _, register := range adminRoutes {
				register(r)
			}
			// 常规只读管理查询：正常快速 2xx 静默，慢查询（>=1s）和 4xx/5xx 照常记录。
			r.Group(func(r chi.Router) {
				r.Use(applog.QuietRequestLog)
				r.Get("/frontend", a.handleFrontendList)
				r.Get("/frontend/updates", a.handleFrontendUpdates)
				r.Get("/drives", a.handleListDrives)
				r.Get("/drives/storage", a.handleDriveStorage)
				r.Get("/drive-logins/{id}", a.handleDriveLoginStatus)
				r.Get("/drives/{id}", a.handleDriveConfigSnapshot)
				r.Get("/drives/{id}/config", a.handleDriveConfigSnapshot)
				r.Get("/drives/{id}/runtime", a.handleDriveRuntimeSnapshot)
				r.Get("/drives/{id}/stats", a.handleDriveStatsSnapshot)
				r.Get("/drives/{id}/storage", a.handleDriveStorageSnapshot)
				r.Get("/drives/{id}/dirtree", a.handleListDriveDirTree)
				r.Get("/crawlers", a.handleListCrawlers)
				r.Get("/crawlers/{id}/tasks", a.handleListCrawlerTasks)
				r.Get("/crawlers/{id}/tasks/{taskID}", a.handleGetCrawlerTask)
				r.Get("/videos", a.handleAdminListVideos)
				r.Get("/videos/stats", a.handleVideoStats)
				r.Get("/videos/{id}/deletion-preview", a.handleVideoDeletionPreview)
				r.Get("/merged-videos", a.handleMergedVideos)
				r.Get("/merged-videos/{id}", a.handleMergedVideo)
				r.Get("/duplicate-reviews", a.handleDuplicateReviews)
				r.Get("/blacklist", a.handleListBlacklist)
				r.Get("/blacklist/source-delete/status", a.handleBlacklistSourceDeleteStatus)
				r.Get("/tags", a.handleListTags)
				r.Get("/tags/jobs/status", a.handleTagJobStatus)
				r.Get("/users", a.handleListUsers)
				r.Get("/banned-ips", a.handleListBannedIPs)
				r.Get("/telegram/availability", a.handleTelegramAvailability)
				r.Get("/telegram/status", a.handleTelegramStatus)
				r.Get("/import-jobs", a.handleImportList)
				r.Get("/config.yaml", a.handleGetConfigYAML)
				r.Get("/generation/gpu", a.handleGPUStatus)
				r.Get("/settings", a.handleGetSettings)
				r.Get("/update/check", a.handleCheckUpdate)
				r.Get("/logs", a.handleLogs)
				r.Get("/logs/meta", a.handleLogsMeta)
				r.Get("/backups", a.handleListBackups)
				r.Get("/backup-uploads/{id}", a.handleBackupUploadStatus)
				r.Get("/backup-transfers", a.handleListBackupTransfers)
				r.Get("/backup-receives", a.handleListBackupReceiveTransfers)
				r.Get("/backup-receive-tokens", a.handleListBackupReceiveTokens)
				r.Get("/jobs/scan-all/status", a.handleScanAllJobStatus)
			})

			// 长连接 SSE 流：正常 2xx 断开不记慢请求，错误照常记录。
			r.Group(func(r chi.Router) {
				r.Use(applog.SkipSuccessRequestLog)
				r.Get("/drives/{id}/events", a.handleDriveEvents)
				r.Get("/logs/stream", a.handleLogStream)
			})

			// 前端管理变更
			r.Post("/frontend/activate", a.handleFrontendActivate)
			r.Post("/frontend/install", a.handleFrontendInstall)
			r.Post("/frontend/upload", a.handleFrontendUpload)

			// 网盘变更与敏感读取（credentials 保留审计日志）
			r.Post("/drives", a.handleUpsertDrive)
			r.Post("/drive-logins", a.handleStartDriveLogin)
			r.Delete("/drive-logins/{id}", a.handleCancelDriveLogin)
			r.Get("/drives/{id}/credentials", a.handleGetDriveCredentials)
			r.Delete("/drives/{id}", a.handleDeleteDrive)
			r.Post("/drives/{id}/rescan", a.handleRescan)
			r.Post("/drives/{id}/tasks/stop", a.handleStopDriveTasks)
			r.Post("/drives/{id}/skip-dirs", a.handleSetDriveSkipDirs)
			r.Post("/drives/{id}/generation-parallelism", a.handleSetDriveGenerationParallelism)
			r.Post("/drives/{id}/previews/generate", a.handleGenerateDrivePreviews)
			r.Post("/drives/{id}/thumbnails/generate", a.handleGenerateDriveThumbnails)
			r.Post("/drives/{id}/fingerprints/generate", a.handleGenerateDriveFingerprints)

			// 爬虫变更
			r.Post("/crawlers", a.handleUpsertCrawler)
			r.Post("/crawlers/import-file", a.handleImportCrawlerScriptFile)
			r.Post("/crawlers/import-url", a.handleImportCrawlerScriptURL)
			r.Post("/crawlers/test-script", a.handleTestCrawlerScript)
			r.Delete("/crawlers/{id}", a.handleDeleteCrawler)
			r.Post("/crawlers/{id}/run", a.handleRunCrawler)
			r.Post("/crawlers/{id}/tasks/{taskID}/cancel", a.handleCancelCrawlerTask)
			r.Post("/crawlers/{id}/upload", a.handleUploadCrawlerVideos)
			r.Post("/crawlers/{id}/paused", a.handleSetCrawlerPaused)
			r.Post("/crawlers/{id}/tasks/stop", a.handleStopCrawlerTasks)

			// 视频变更
			r.Put("/videos/{id}", a.handleUpdateVideo)
			r.Delete("/videos/{id}", a.handleDeleteVideo)
			r.Post("/videos/regen-preview", a.handleRegenAllPreviews)
			r.Post("/videos/{id}/regen-preview", a.handleRegenPreview)
			r.Delete("/merged-videos/{id}/source", a.handleDeleteMergedSource)
			r.Post("/duplicate-reviews/merge", a.handleMergeDuplicateReview)
			r.Post("/duplicate-reviews/dismiss", a.handleDismissDuplicateReview)
			r.Post("/blacklist/source-delete", a.handleStartBlacklistSourceDelete)
			r.Delete("/blacklist/{id}", a.handleRemoveBlacklist)

			// 标签变更
			r.Post("/tags", a.handleCreateTag)
			r.Put("/tags/{id}", a.handleUpdateTag)
			r.Delete("/tags/{id}", a.handleDeleteTag)
			r.Post("/tags/retag", a.handleStartTagRetag)

			// 用户与封禁变更
			r.Post("/users", a.handleCreateUser)
			r.Delete("/users/{id}", a.handleDeleteUser)
			r.Post("/users/{id}/ban", a.handleBanUser)
			r.Post("/users/{id}/unban", a.handleUnbanUser)
			r.Put("/users/{id}/password", a.handleResetPassword)
			r.Delete("/banned-ips/{ip}", a.handleUnbanIP)

			// 配置与外部集成变更
			r.Post("/telegram/test", a.handleTelegramTest)
			r.Post("/telegram/resume", a.handleTelegramResume)
			r.Post("/import-jobs/{jobId}/cancel", a.handleImportCancel)
			r.Post("/import-jobs/{jobId}/retry", a.handleImportRetry)
			r.Put("/config.yaml", a.handlePutConfigYAML)
			r.Post("/generation/gpu/probe", a.handleGPUProbe)
			r.Put("/settings", a.handlePutSettings)

			// 运维变更与导出（保留审计日志）
			r.Get("/logs/export", a.handleExportLogs)
			r.Delete("/logs", a.handleClearLogs)
			r.Post("/backups", a.handleCreateBackup)
			r.Post("/backups/current/cancel", a.handleCancelBackup)
			r.Get("/backups/{id}/download", a.handleDownloadBackup)
			r.Delete("/backups/{id}", a.handleDeleteBackup)
			r.Post("/backups/{id}/restore", a.handleRestoreBackup)
			r.Post("/backups/{id}/transfers", a.handleCreateBackupTransfer)
			r.Post("/backup-uploads", a.handleBeginBackupUpload)
			r.Put("/backup-uploads/{id}/chunks/{index}", a.handleBackupUploadChunk)
			r.Post("/backup-uploads/{id}/finalize", a.handleFinalizeBackupUpload)
			r.Delete("/backup-uploads/{id}", a.handleCancelBackupUpload)
			r.Delete("/backup-receives/{id}", a.handleCancelBackupReceiveTransfer)
			r.Delete("/backup-transfers/{id}", a.handleCancelBackupTransfer)
			r.Post("/backup-transfers/{id}/retry", a.handleRetryBackupTransfer)
			r.Post("/backup-receive-tokens", a.handleCreateBackupReceiveToken)
			r.Delete("/backup-receive-tokens/{id}", a.handleRevokeBackupReceiveToken)
			r.Post("/jobs/scan-all/run", a.handleRunScanAllJob)
			r.Post("/tasks/stop", a.handleStopAllTasks)
		})
	})

	// Peer routes use narrowly scoped receive tokens rather than browser login
	// sessions. Keeping them outside /admin/api prevents machine credentials
	// from acquiring unrelated administrator capabilities.
	r.Route(backuptransfer.PeerBackupPath, func(r chi.Router) {
		r.Get("/capabilities", a.handlePeerBackupCapabilities)
		r.Post("/imports", a.handlePeerBeginBackupImport)
		r.Get("/imports/{id}", a.handlePeerBackupImportStatus)
		r.Put("/imports/{id}/ranges/{index}", a.handlePeerBackupImportRange)
		r.Post("/imports/{id}/finalize", a.handlePeerFinalizeBackupImport)
		r.Delete("/imports/{id}", a.handlePeerCancelBackupImport)
	})
}
