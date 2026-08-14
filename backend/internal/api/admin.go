package api

import (
	"context"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/video-site/backend/internal/applog"
	"github.com/video-site/backend/internal/auth"
	"github.com/video-site/backend/internal/backup"
	"github.com/video-site/backend/internal/backuptransfer"
	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/config"
	"github.com/video-site/backend/internal/drives/quark"
)

type AdminServer struct {
	Catalog         *catalog.Catalog
	Auth            *auth.Authenticator
	Backups         *backup.Manager
	BackupTransfers *backuptransfer.Manager
	// Logs is the durable runtime log store exposed only through the
	// administrator-authenticated routes below.
	Logs *applog.Store
	// ConfigManager owns the real config.yaml management surface. Both visual
	// and source editors read and write this same document.
	ConfigManager *config.Manager
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
	// SetupRequired 表示当前是否仍处于首次部署初始化状态。
	SetupRequired func() bool
	// OnSetup 持久化首次部署时设置的管理员账号密码，并更新运行中认证器。
	OnSetup func(username, password string) error
	// LocalPreviewDir is the local directory that stores generated preview videos and thumbs.
	LocalPreviewDir string
	// Hooks：外层注入实际执行者
	OnDriveSaved              func(driveID string) error
	OnDriveDeleteCleanup      func(ctx context.Context, driveID string) (int, error)
	OnDriveRemoved            func(driveID string)
	OnScanRequested           func(driveID string) bool
	OnCrawlerUploadRequested  func(driveID string) (bool, string)
	OnStopDriveTasks          func(driveID string) bool
	OnStopAllTasks            func() int
	OnRegenPreview            func(videoID string)
	OnRegenAllPreviews        func()
	OnRegenFailedPreviews     func(driveID string)
	OnRegenFailedThumbnails   func(driveID string)
	OnRegenFailedFingerprints func(driveID string)
	// OnStartDriveTranscode 手动开启某盘的浏览器兼容性转码任务。
	// 返回 (是否接受, 拒绝原因)。转码从不自动运行，只能在这里手动触发；
	// 处理完候选列表后任务自然结束。
	OnStartDriveTranscode func(driveID string) (bool, string)
	// OnStopDriveTranscode 手动停止某盘正在进行的转码任务。返回是否有任务被停。
	OnStopDriveTranscode           func(driveID string) bool
	OnDeleteVideo                  func(ctx context.Context, videoID string, deleteSource bool) (DeleteVideoResult, error)
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
	GetPreviewGenerationVideoIDs func() map[string]bool
	// OnTeaserEnabledChanged 在 per-drive 预览视频开关被切换后调用。
	// enabled=true 时上层应该重新把 pending 预览视频入队（类似旧的全局开关从关到开）；
	// enabled=false 时通常不用做事 —— worker 入队前会再次查 catalog，自然停止。
	OnTeaserEnabledChanged func(driveID string, enabled bool)
	// Theme 读写（"dark" | "pink" | "sky"）
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
	// 123网盘扫码登录接口测试注入；生产留空走官方 user.123pan.cn。
	P123UserAPIBaseURL string
	P123HTTPClient     *http.Client
	// 115网盘扫码登录接口测试注入；生产留空使用带超时的默认客户端。
	P115QRHTTPClient *http.Client
	// 夸克网盘扫码登录接口测试注入；生产留空走官方 uop.quark.cn 和 pan.quark.cn。
	QuarkQRUOPBaseURL string
	QuarkQRPanBaseURL string
	QuarkQRHTTPClient *http.Client
	quarkQRMu         sync.Mutex
	quarkQRClient     *quark.QRClient
	// 联通网盘扫码登录接口测试注入；生产留空走官方 panservice.mail.wo.cn。
	WopanQRAPIBaseURL string
	WopanQRHTTPClient *http.Client
	// 光鸭网盘扫码登录接口测试注入；生产留空走官方 account.guangyapan.com。
	GuangYaPanAccountBaseURL string
	GuangYaPanHTTPClient     *http.Client
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
	State         string `json:"state"`
	CurrentTitle  string `json:"currentTitle,omitempty"`
	QueueLength   int    `json:"queueLength"`
	CooldownUntil string `json:"cooldownUntil,omitempty"`
	ScannedCount  int    `json:"scannedCount"`
	AddedCount    int    `json:"addedCount"`
	DoneCount     int    `json:"doneCount"`
	TotalCount    int    `json:"totalCount"`
}

type DriveGenerationStatuses struct {
	Scan        GenerationStatus `json:"scan"`
	Thumbnail   GenerationStatus `json:"thumbnail"`
	Preview     GenerationStatus `json:"preview"`
	Fingerprint GenerationStatus `json:"fingerprint"`
	Upload      GenerationStatus `json:"upload"`
	Transcode   GenerationStatus `json:"transcode"`
}

type NightlyJobStatus struct {
	State          string `json:"state"`
	Running        bool   `json:"running"`
	Queued         bool   `json:"queued"`
	StartedAt      string `json:"startedAt,omitempty"`
	LastFinishedAt string `json:"lastFinishedAt,omitempty"`
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

type deleteVideoReq struct {
	DeleteSource bool `json:"deleteSource"`
}

func (a *AdminServer) Register(r chi.Router) {
	r.Route("/admin/api", func(r chi.Router) {
		// 登录、登出和首次部署初始化不需要鉴权
		r.Get("/setup", a.handleSetupStatus)
		r.Post("/setup", a.handleSetup)
		r.Post("/login", a.handleLogin)
		r.Post("/logout", a.handleLogout)
		r.Get("/me", a.handleMe)

		// 其余路由需管理员鉴权
		r.Group(func(r chi.Router) {
			r.Use(a.Auth.AdminRequired)

			// 网盘
			r.Get("/drives", a.handleListDrives)
			r.Get("/drives/storage", a.handleDriveStorage)
			r.Post("/drives", a.handleUpsertDrive)
			r.Post("/drives/quark/qr", a.handleQuarkQRStart)
			r.Post("/drives/quark/qr/status", a.handleQuarkQRStatus)
			r.Post("/drives/p115/qr", a.handleP115QRStart)
			r.Post("/drives/p115/qr/status", a.handleP115QRStatus)
			r.Post("/drives/p123/qr", a.handleP123QRStart)
			r.Get("/drives/p123/qr/{uniID}", a.handleP123QRStatus)
			r.Post("/drives/wopan/qr", a.handleWopanQRStart)
			r.Get("/drives/wopan/qr/{uuid}", a.handleWopanQRStatus)
			r.Post("/drives/guangyapan/qr", a.handleGuangYaPanQRStart)
			r.Get("/drives/guangyapan/qr/status", a.handleGuangYaPanQRStatus)
			r.Get("/drives/{id}/credentials", a.handleGetDriveCredentials)
			r.Delete("/drives/{id}", a.handleDeleteDrive)
			r.Post("/drives/{id}/rescan", a.handleRescan)
			r.Post("/drives/{id}/tasks/stop", a.handleStopDriveTasks)
			r.Post("/drives/{id}/teaser-enabled", a.handleSetDriveTeaserEnabled)
			r.Post("/drives/{id}/skip-dirs", a.handleSetDriveSkipDirs)
			r.Get("/drives/{id}/dirtree", a.handleListDriveDirTree)
			r.Post("/drives/{id}/previews/failed/regenerate", a.handleRegenFailedPreviews)
			r.Post("/drives/{id}/thumbnails/failed/regenerate", a.handleRegenFailedThumbnails)
			r.Post("/drives/{id}/fingerprints/failed/regenerate", a.handleRegenFailedFingerprints)
			r.Post("/drives/{id}/transcode/start", a.handleStartDriveTranscode)
			r.Post("/drives/{id}/transcode/stop", a.handleStopDriveTranscode)

			// 爬虫
			r.Get("/crawlers", a.handleListCrawlers)
			r.Post("/crawlers", a.handleUpsertCrawler)
			r.Post("/crawlers/import-file", a.handleImportCrawlerScriptFile)
			r.Post("/crawlers/import-url", a.handleImportCrawlerScriptURL)
			r.Post("/crawlers/test-script", a.handleTestCrawlerScript)
			r.Delete("/crawlers/{id}", a.handleDeleteCrawler)
			r.Post("/crawlers/{id}/run", a.handleRunCrawler)
			r.Post("/crawlers/{id}/upload", a.handleUploadCrawlerVideos)
			r.Post("/crawlers/{id}/paused", a.handleSetCrawlerPaused)
			r.Post("/crawlers/{id}/tasks/stop", a.handleStopCrawlerTasks)

			// 视频
			r.Get("/videos", a.handleAdminListVideos)
			r.Get("/videos/stats", a.handleVideoStats)
			r.Put("/videos/{id}", a.handleUpdateVideo)
			r.Delete("/videos/{id}", a.handleDeleteVideo)
			r.Post("/videos/regen-preview", a.handleRegenAllPreviews)
			r.Post("/videos/{id}/regen-preview", a.handleRegenPreview)
			// 黑名单（被拉黑/手动删除、扫盘不再入库的视频）
			r.Get("/blacklist", a.handleListBlacklist)
			r.Get("/blacklist/source-delete/status", a.handleBlacklistSourceDeleteStatus)
			r.Post("/blacklist/source-delete", a.handleStartBlacklistSourceDelete)
			r.Delete("/blacklist/{id}", a.handleRemoveBlacklist)

			// 标签
			r.Get("/tags", a.handleListTags)
			r.Post("/tags", a.handleCreateTag)
			r.Put("/tags/{id}", a.handleUpdateTag)
			r.Delete("/tags/{id}", a.handleDeleteTag)
			r.Post("/tags/retag", a.handleStartTagRetag)
			r.Get("/tags/jobs/status", a.handleTagJobStatus)

			// 用户管理
			r.Get("/users", a.handleListUsers)
			r.Post("/users", a.handleCreateUser)
			r.Delete("/users/{id}", a.handleDeleteUser)
			r.Post("/users/{id}/ban", a.handleBanUser)
			r.Post("/users/{id}/unban", a.handleUnbanUser)
			r.Put("/users/{id}/password", a.handleResetPassword)

			// IP 封禁管理
			r.Get("/banned-ips", a.handleListBannedIPs)
			r.Delete("/banned-ips/{ip}", a.handleUnbanIP)

			// 配置文件与其它独立设置
			r.Get("/config.yaml", a.handleGetConfigYAML)
			r.Put("/config.yaml", a.handlePutConfigYAML)
			r.Get("/settings", a.handleGetSettings)
			r.Put("/settings", a.handlePutSettings)

			// 运维任务
			r.Get("/update/check", a.handleCheckUpdate)
			r.Get("/logs", a.handleLogs)
			r.Delete("/logs", a.handleClearLogs)
			r.Get("/backups", a.handleListBackups)
			r.Post("/backups", a.handleCreateBackup)
			r.Post("/backups/current/cancel", a.handleCancelBackup)
			r.Get("/backups/{id}/download", a.handleDownloadBackup)
			r.Delete("/backups/{id}", a.handleDeleteBackup)
			r.Post("/backups/{id}/restore", a.handleRestoreBackup)
			r.Post("/backups/{id}/transfers", a.handleCreateBackupTransfer)
			r.Post("/backup-uploads", a.handleBeginBackupUpload)
			r.Get("/backup-uploads/{id}", a.handleBackupUploadStatus)
			r.Put("/backup-uploads/{id}/chunks/{index}", a.handleBackupUploadChunk)
			r.Post("/backup-uploads/{id}/finalize", a.handleFinalizeBackupUpload)
			r.Delete("/backup-uploads/{id}", a.handleCancelBackupUpload)
			r.Get("/backup-transfers", a.handleListBackupTransfers)
			r.Get("/backup-receives", a.handleListBackupReceiveTransfers)
			r.Delete("/backup-receives/{id}", a.handleCancelBackupReceiveTransfer)
			r.Delete("/backup-transfers/{id}", a.handleCancelBackupTransfer)
			r.Post("/backup-transfers/{id}/retry", a.handleRetryBackupTransfer)
			r.Get("/backup-receive-tokens", a.handleListBackupReceiveTokens)
			r.Post("/backup-receive-tokens", a.handleCreateBackupReceiveToken)
			r.Delete("/backup-receive-tokens/{id}", a.handleRevokeBackupReceiveToken)
			r.Get("/jobs/scan-all/status", a.handleScanAllJobStatus)
			r.Post("/jobs/scan-all/run", a.handleRunScanAllJob)
			// 兼容旧前端缓存；旧路径现在也遵循“只扫盘 + 去重”的语义。
			r.Get("/jobs/nightly/status", a.handleNightlyJobStatus)
			r.Post("/jobs/nightly/run", a.handleRunNightlyJob)
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
