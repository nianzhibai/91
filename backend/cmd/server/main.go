package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/video-site/backend/internal/api"
	"github.com/video-site/backend/internal/applog"
	"github.com/video-site/backend/internal/auth"
	"github.com/video-site/backend/internal/backup"
	"github.com/video-site/backend/internal/backuptransfer"
	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/config"
	"github.com/video-site/backend/internal/crawlerupload"
	"github.com/video-site/backend/internal/driveevents"
	"github.com/video-site/backend/internal/drives/scriptcrawler"
	"github.com/video-site/backend/internal/fingerprint"
	"github.com/video-site/backend/internal/mediaimport"
	"github.com/video-site/backend/internal/nightly"
	"github.com/video-site/backend/internal/preview"
	"github.com/video-site/backend/internal/proxy"
	"github.com/video-site/backend/internal/subtitles"
	"github.com/video-site/backend/internal/telegram"
)

const (
	fingerprintReconcileInterval = time.Minute

	// 近重复阈值统一定义在 mediasim（NearDuplicate* / ContentDuplicate*），
	// 爬虫导入与夜间维护共用同一组常量。

	blacklistSourceDeletePace            = 250 * time.Millisecond
	blacklistSourceDeleteDefaultCooldown = 30 * time.Second
	blacklistSourceDeleteMaxAttempts     = 4
)

func main() {
	if handled, err := runServerCommand(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	// stderr is always kept as the operational log sink. The durable admin log
	// file is attached after configuration paths have been resolved.
	log.SetOutput(applog.Output(os.Stderr, nil))

	cfgPath := applicationConfigPath()
	workingDir, err := os.Getwd()
	if err != nil {
		log.Fatalf("resolve startup directory: %v", err)
	}
	fileConfig, cfg, err := loadApplicationConfig(cfgPath, workingDir)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// A restore is activated before SQLite is opened. The switch itself only
	// uses same-directory renames; opening and migrating the restored catalog
	// below is the commit check. If that check fails, every switched path is
	// returned to its pre-restore value.
	dataRoot := cfg.Storage.DataDir
	_, pendingRestoreStatErr := os.Stat(backup.PendingMarkerPath(dataRoot))
	pendingRestoreAtStartup := pendingRestoreStatErr == nil
	appliedRestore, err := backup.ApplyPendingRestore(dataRoot)
	if err != nil {
		log.Fatalf("apply pending restore: %v", err)
	}
	// Reload after either applying a restore or resuming an interrupted
	// rollback. The config read before ApplyPendingRestore may have belonged to
	// the opposite side of the directory switch.
	if appliedRestore != nil || pendingRestoreAtStartup {
		fileConfig, cfg, err = loadApplicationConfig(cfgPath, workingDir)
		if err != nil {
			if appliedRestore != nil {
				if rollbackErr := backup.RollbackAppliedRestore(appliedRestore, err); rollbackErr != nil {
					log.Fatalf("reload restored config: %v (rollback failed: %v)", err, rollbackErr)
				}
				log.Fatalf("reload restored config: %v; old data restored", err)
			}
			log.Fatalf("reload config after restore rollback: %v", err)
		}
	}

	logStore := openRuntimeLogs(cfg.Logging)
	if logStore != nil {
		defer func() {
			if err := logStore.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "close runtime log: %s\n", applog.Redact(err.Error()))
			}
		}()
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Storage.DBPath), 0o755); err != nil {
		log.Fatalf("mkdir db dir: %v", err)
	}
	if err := os.MkdirAll(cfg.Storage.LocalPreviewDir, 0o755); err != nil {
		log.Fatalf("mkdir preview dir: %v", err)
	}
	assetLease, err := acquireAssetDirectoryLease(cfg.Storage.LocalPreviewDir, cfg.Storage.DBPath)
	if err != nil {
		log.Fatalf("acquire generated asset directory lease: %v", err)
	}
	defer func() {
		if closeErr := assetLease.Close(); closeErr != nil {
			log.Printf("release generated asset directory lease: %v", closeErr)
		}
	}()

	cat, err := openApplicationCatalog(cfg.Storage)
	if err != nil {
		if appliedRestore != nil {
			if rollbackErr := backup.RollbackAppliedRestore(appliedRestore, err); rollbackErr != nil {
				log.Fatalf("open restored catalog: %v (rollback failed: %v)", err, rollbackErr)
			}
			log.Fatalf("open restored catalog: %v; old data restored and will be used after restart", err)
		}
		log.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	if err := cat.InterruptCrawlerTasks(context.Background()); err != nil {
		log.Fatalf("recover crawler tasks: %v", err)
	}
	if err := cat.ResetLoginProtection(context.Background()); err != nil {
		log.Fatalf("reset login protection at startup: %v", err)
	}
	if appliedRestore != nil {
		if err := backup.CommitAppliedRestore(appliedRestore); err != nil {
			log.Printf("[restore] restored catalog opened, but rollback cleanup/report write failed: %v", err)
		} else {
			log.Printf("[restore] backup restored successfully; previous sessions were cleared")
		}
	}
	configManager, err := config.NewManager(cfgPath)
	if err != nil {
		log.Fatalf("configure config manager: %v", err)
	}
	if err := migrateApplicationConfig(context.Background(), cat, configManager); err != nil {
		log.Fatalf("migrate config.yaml: %v", err)
	}
	fileConfig, cfg, err = loadApplicationConfig(cfgPath, workingDir)
	if err != nil {
		log.Fatalf("reload migrated config: %v", err)
	}

	app := &App{
		cfg:                cfg,
		cat:                cat,
		configManager:      configManager,
		registry:           proxy.NewRegistry(),
		workers:            make(map[string]*preview.Worker),
		thumbWorkers:       make(map[string]*preview.ThumbWorker),
		fingerprintWorkers: make(map[string]*fingerprint.Worker),
		scriptCrawlers:     make(map[string]*scriptcrawler.Crawler),
	}
	app.proxy = proxy.New(app.registry)
	app.proxy.SetAllowForcedRelay(cfg.Proxy.AllowsForcedRelay())
	app.proxy.SetStreamStatusReporter(app.recordPlaybackDriveStatus)
	app.crawlerUploader = crawlerupload.New(crawlerupload.Config{
		Catalog:          cat,
		Registry:         app.registry,
		GetDrive:         app.activeDriveConfig,
		PreviewEnabled:   app.previewEnabled,
		CommonThumbDir:   app.commonThumbsDir(),
		OnUploadProgress: app.updateCrawlerUploadProgress,
	})

	// 初始化本地内置盘；外部云盘放到 HTTP 服务启动后异步挂载，避免上游
	// 登录态校验拖慢端口监听。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := configManager.SetApply(func(settings config.LiveSettings) error {
		return app.applyLiveConfig(ctx, settings)
	}); err != nil {
		log.Fatalf("apply initial live configuration: %v", err)
	}

	legacyCrawlerStats, err := app.cleanupLegacyDeletedCrawlers(ctx)
	if err != nil {
		log.Printf(
			"[scriptcrawler-maintenance] cleanup incomplete removed_crawlers=%d removed_videos=%d: %v",
			legacyCrawlerStats.RemovedCrawlers,
			legacyCrawlerStats.RemovedVideos,
			err,
		)
	} else if legacyCrawlerStats.RemovedCrawlers > 0 {
		log.Printf(
			"[scriptcrawler-maintenance] removed legacy deleted crawlers=%d videos=%d",
			legacyCrawlerStats.RemovedCrawlers,
			legacyCrawlerStats.RemovedVideos,
		)
	}
	app.loadTheme(ctx)
	if orphans, err := app.cat.ListVideosWithMissingDrive(ctx); err != nil {
		log.Printf("[catalog-maintenance] inspect orphan drive videos: %v", err)
	} else if len(orphans) > 0 {
		log.Printf(
			"[catalog-maintenance] preserved %d videos with missing drive metadata; assets are removed only by an explicit drive deletion",
			len(orphans),
		)
	}
	if err := app.attachLocalUpload(ctx); err != nil {
		log.Printf("[local-upload] attach failed: %v", err)
	}
	if err := app.cleanupTelegramUploadSources(ctx); err != nil {
		log.Printf("[telegram-upload] startup source cleanup: %v", err)
	}
	go app.runFingerprintReconciler(ctx)
	go app.runDeletedVideoAssetCleanup(ctx)

	telegramService := telegram.NewIntegration(cat, configManager, app.localUploadDir(), cfg.RemoteUpload.DiskReserveBytes)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = telegramService.Shutdown(shutdownCtx)
	}()

	remoteUploader, err := mediaimport.New(mediaimport.Config{
		TelegramSource: telegramService,
		Catalog:        cat,
		UploadDir:      app.localUploadDir(),
		FFprobePath:    cfg.Preview.FFprobePath,
		DiskReserve:    cfg.RemoteUpload.DiskReserveBytes,
		IdleTimeout:    time.Duration(cfg.RemoteUpload.IdleTimeoutSeconds) * time.Second,
		OnVideoUploaded: func(v *catalog.Video) {
			app.enqueueUploadedVideo(ctx, v)
		},
	})
	if err != nil {
		log.Fatalf("configure remote upload: %v", err)
	}
	if err := remoteUploader.Start(ctx); err != nil {
		log.Fatalf("start remote upload: %v", err)
	}
	telegramService.SetWake(remoteUploader.Wake)
	telegramService.Start(ctx)

	authr := &auth.Authenticator{Catalog: cat}
	versionFilePath := strings.TrimSpace(os.Getenv("VIDEO_VERSION_FILE"))
	if versionFilePath == "" {
		versionFilePath = filepath.Join(filepath.Dir(cfgPath), ".version")
	}
	imageVersion := strings.TrimSpace(os.Getenv("VIDEO_IMAGE_VERSION"))
	appVersion := imageVersion
	if appVersion == "" {
		appVersion = readVersionFile(versionFilePath)
	}
	githubRepo := strings.TrimSpace(os.Getenv("VIDEO_GITHUB_REPO"))
	if githubRepo == "" {
		githubRepo = strings.TrimSpace(os.Getenv("GITHUB_REPO"))
	}
	backupManager, err := backup.NewManager(backup.Config{
		Catalog:           cat,
		TelegramFilesRoot: func() string { return configManager.TelegramSettings().LocalFilesRoot },
		AppConfig:         fileConfig,
		RuntimeStorage:    cfg.Storage,
		ConfigPath:        cfgPath,
		AppVersion:        appVersion,
		RestartManaged:    restartIsManaged(),
	})
	if err != nil {
		log.Fatalf("configure backup service: %v", err)
	}
	backupManager.Start(ctx)
	defer backupManager.Close()
	backupTransferManager, err := backuptransfer.New(backuptransfer.Config{
		Backups: backupManager,
		RootDir: filepath.Join(cfg.Storage.DataDir, "backups", ".peer-transfer"),
	})
	if err != nil {
		log.Fatalf("configure backup transfer service: %v", err)
	}
	backupTransferManager.Start(ctx)
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := backupTransferManager.Shutdown(shutdownCtx); err != nil {
			log.Printf("[backup-transfer] shutdown: %v", err)
		}
	}()

	apiServer := &api.Server{
		Catalog:        cat,
		Proxy:          app.proxy,
		SubtitleClient: subtitles.NewGuangYaPanClient(subtitles.GuangYaPanConfig{}),
		LocalDir:       cfg.Storage.LocalPreviewDir,
		UploadDir:      app.localUploadDir(),
		OnVideoUploaded: func(v *catalog.Video) {
			app.enqueueUploadedVideo(ctx, v)
		},
		RemoteUploads: remoteUploader,
		// 前台「不再展示」走拉黑逻辑：删记录 + 删本地封面/预览 + 写墓碑，
		// 保留网盘源文件（deleteSource=false）。后续任务不再入库；可重新发现的
		// 普通网盘/爬虫来源可在后台解除墓碑，操作本身不会立即触发扫盘或爬取。
		OnHideVideo: func(reqCtx context.Context, videoID string) error {
			_, err := app.deleteVideo(reqCtx, videoID, false)
			return err
		},
		GetTheme:          func() string { return app.Theme() },
		GetPreviewEnabled: app.previewEnabled,
	}
	app.onTagsChanged = apiServer.InvalidateTagCache

	adminServer := &api.AdminServer{
		Telegram:               telegramService,
		Imports:                remoteUploader,
		Catalog:                cat,
		Auth:                   authr,
		Backups:                backupManager,
		BackupTransfers:        backupTransferManager,
		Logs:                   logStore,
		ConfigManager:          configManager,
		GPU:                    app.generationHardware(),
		VersionFilePath:        versionFilePath,
		ImageVersion:           imageVersion,
		GitHubRepo:             githubRepo,
		LocalPreviewDir:        cfg.Storage.LocalPreviewDir,
		BeginDriveConfigUpdate: app.beginDriveConfigUpdate,
		OnDriveRuntimeConfigChanged: func(driveID string) error {
			return app.reloadDriveRuntime(ctx, driveID)
		},
		OnPrepareDriveDelete: func(deleteCtx context.Context, driveID string) error {
			app.cancelDriveTasks(ctx, driveID)
			return app.waitDriveTasksStopped(deleteCtx, driveID)
		},
		OnDriveDeleteCleanup: func(cleanupCtx context.Context, driveID string) (int, error) {
			return app.cleanupDriveVideosForDelete(cleanupCtx, driveID)
		},
		OnDriveRemoved: func(driveID string) {
			app.detachDrive(driveID)
		},
		OnScanRequested: func(requestCtx context.Context, driveID string) bool {
			taskCtx := applog.WithFields(ctx, applog.ContextFields(requestCtx))
			// 爬虫类 drive 的"重扫"等同于手动触发一次爬取；其它 drive 走标准 scan
			isScriptCrawler := false
			if d, err := app.cat.GetDrive(ctx, driveID); err == nil && d != nil {
				isScriptCrawler = d.Kind == scriptcrawler.Kind
			}
			if isScriptCrawler {
				return app.scheduleScriptCrawlerCrawl(taskCtx, driveID)
			}
			return app.scheduleScan(taskCtx, driveID)
		},
		OnCrawlerRunRequested: func(requestCtx context.Context, driveID string) (string, error) {
			return app.startScriptCrawlerCrawl(applog.WithFields(ctx, applog.ContextFields(requestCtx)), driveID)
		},
		OnCrawlerTaskCancel: func(driveID, taskID string) bool {
			app.mu.Lock()
			crawler := app.scriptCrawlers[driveID]
			app.mu.Unlock()
			return crawler != nil && crawler.CancelTask(taskID)
		},
		OnCrawlerUploadRequested: func(driveID string) (bool, string) {
			return app.scheduleManualCrawlerUploadMigration(ctx, driveID)
		},
		OnStopDriveTasks: func(driveID string) bool {
			return app.stopDriveTasks(ctx, driveID)
		},
		OnStopAllTasks: func() int {
			return app.stopAllDriveTasks(ctx)
		},
		OnRegenPreview: func(videoID string) {
			go app.regenPreview(ctx, videoID)
		},
		OnRegenAllPreviews: func() {
			go app.regenAllPreviews(ctx)
		},
		OnDriveGenerationRequested: func(reqCtx context.Context, driveID string, kind api.DriveGenerationKind) (api.DriveGenerationResult, error) {
			return app.requestDriveGeneration(reqCtx, ctx, driveID, kind)
		},
		OnDeleteVideo: func(reqCtx context.Context, videoID string, deleteSource bool) (api.DeleteVideoResult, error) {
			return app.deleteVideo(reqCtx, videoID, deleteSource)
		},
		OnStartBlacklistSourceDelete: func(req api.BlacklistSourceDeleteRequest) bool {
			return app.startBlacklistSourceDelete(ctx, req)
		},
		GetBlacklistSourceDeleteStatus: func() api.BlacklistSourceDeleteStatus {
			return app.blacklistSourceDeleteStatus()
		},
		OnRemoveBlacklist: func(reqCtx context.Context, videoID string) error {
			return app.restoreDeletedVideo(reqCtx, videoID)
		},
		OnStartTagRetag: func() bool {
			return app.startTagRetag(ctx)
		},
		OnTagsChanged: apiServer.InvalidateTagCache,
		GetTagJobStatus: func() api.TagJobStatus {
			return app.tagJobStatus()
		},
		GetDriveGenerationStatus: app.driveGenerationStatus,
		GetDriveGenerationStatuses: func() map[string]api.DriveGenerationStatuses {
			return app.driveGenerationStatuses()
		},
		GetPreviewGenerationVideoIDs: func() map[string]bool {
			return app.previewGenerationVideoIDs()
		},
		GetTheme: func() string { return app.Theme() },
		SetTheme: func(theme string) error {
			return app.SetTheme(ctx, theme)
		},
		OnRunScanAllJob: func() bool {
			if app.nightlyRunner != nil {
				return app.nightlyRunner.TriggerScanAll()
			}
			return false
		},
		GetNightlyJobStatus: func() api.NightlyJobStatus {
			return app.nightlyJobStatus()
		},
		ListDriveDirChildren: func(reqCtx context.Context, driveID, parentID string) ([]api.DriveDirEntry, error) {
			return app.listDriveDirChildren(reqCtx, driveID, parentID)
		},
	}

	r := chi.NewRouter()
	accessLogger := log.New(
		os.Stdout,
		"",
		log.LstdFlags,
	)
	r.Use(requestLogMiddleware(accessLogger, log.Default(), logStore))
	r.Use(responseCompressionMiddleware)
	r.Use(middleware.Recoverer)
	r.Use(corsMiddleware(cfg.Server.AllowedOrigins))

	apiServer.RegisterRoutes(r, authr)
	adminServer.Register(r)
	mountFrontend(r)

	// 凌晨流水线：每天按后台可热更新的 HH:mm + IANA 时区触发一次，依次运行以下阶段：
	//   Phase 1 并行扫所有非爬虫 / localupload 网盘 + 跳过策略/缺失确认清理 + 入队封面/预览视频
	//   Phase 1b 对账本地封面/预览文件 + 将丢失资产重置入队并等待补生成
	//   Phase 2 脚本爬虫 + 入队预览视频
	//   Phase 3 爬虫本地视频 → 云盘上传
	//   Phase 4 扫描爬虫本地目录并恢复已取消拉黑的视频
	//   Phase 5 全库重复视频维护：精确指纹去重 + 标题/时长/封面近似去重
	// 每个存储在自身资源生成完成后补全缺失时长，各存储独立推进。
	// 标签匹配不在夜间流水线中全库重算；新视频入库和管理员修改标签规则时按事件刷新。
	// admin "扫描所有网盘" 使用同一个 Runner 的独立 scan-all 模式，只运行
	// 云盘扫描、本地资产对账和全库重复维护，不触发爬虫、迁移或恢复，也不占用当天的定时执行标记。
	liveSettings := app.liveConfigSettings()
	app.nightlyRunner = nightly.New(nightly.Config{
		OnStatusChanged:             func() { cat.DriveEvents().Notify("", true, driveevents.ActivityChanged) },
		Settings:                    cat,
		Disabled:                    liveSettings.NightlyDisabled,
		StartTime:                   liveSettings.NightlyStartTime,
		Timezone:                    liveSettings.NightlyTimezone,
		ListScanTargets:             app.listScanTargetIDs,
		RunScan:                     app.runScan,
		ListCrawlerDrives:           app.listCrawlerDriveIDs,
		RunCrawlerCrawl:             app.runScriptCrawlerCrawl,
		WaitPreviewQueuesIdle:       app.waitAllPreviewQueuesIdle,
		RunLocalAssetReconciliation: app.reconcileLocalGeneratedAssets,

		RunTelegramUpload: app.runTelegramUploadMigration,

		RunDedupeAssetCleanup: app.cleanupDuplicateVideoAssets,
	})
	go configManager.Watch(ctx)
	go app.nightlyRunner.Run(ctx)

	srv := &http.Server{
		Addr:    cfg.Server.Listen,
		Handler: r,
	}
	listener, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		log.Fatalf("listen %s: %v", cfg.Server.Listen, err)
	}
	go func() {
		log.Printf("video-site backend listening on %s", cfg.Server.Listen)
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()
	go app.attachExistingDrives(ctx)
	go app.runStartupThumbnailNormalization(ctx)
	go app.migrateHiddenVideosToTombstone(ctx)

	// 等待退出信号或恢复任务要求的受控重启。
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	restoreRestart := false
	select {
	case <-sigs:
	case <-backupManager.RestartRequested():
		restoreRestart = true
	}
	signal.Stop(sigs)
	log.Println("shutting down...")
	cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
	remoteShutCtx, remoteShutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer remoteShutCancel()
	if err := remoteUploader.Shutdown(remoteShutCtx); err != nil {
		log.Printf("[remote-upload] shutdown: %v", err)
	}
	if restoreRestart {
		// Keep the restore maintenance barrier held until process exit. Releasing
		// it here would let canceled background workers write after the target
		// snapshot that is about to replace the live database.
		log.Printf("[restore] pending restore staged; exiting with restart code %d", backup.RestartExitCode)
		os.Exit(backup.RestartExitCode)
	}
	backupManager.Close()
}

func openRuntimeLogs(cfg config.Logging) *applog.Store {
	if !cfg.IsFileEnabled() {
		return nil
	}
	store, err := applog.Open(applog.Config{
		Directory: cfg.Directory, MaxLineBytes: applog.DefaultMaxLineBytes,
		MaxFileSizeBytes:  int64(cfg.MaxFileSizeMB) * 1024 * 1024,
		MaxTotalSizeBytes: int64(cfg.MaxTotalSizeMB) * 1024 * 1024,
	})
	if err != nil {
		log.Printf("[logging] file logging unavailable: %v", err)
		return nil
	}
	log.SetOutput(applog.Output(os.Stderr, store))
	log.Printf("[logging] durable runtime log enabled dir=%s max_file_mb=%d max_total_mb=%d", store.Directory(), cfg.MaxFileSizeMB, cfg.MaxTotalSizeMB)
	return store
}

func loadApplicationConfig(path, workingDir string) (*config.Config, *config.Config, error) {
	fileConfig, err := config.Load(path)
	if err != nil {
		return nil, nil, err
	}
	runtimeStorage, err := config.ResolveStoragePaths(fileConfig.Storage, workingDir)
	if err != nil {
		return nil, nil, err
	}
	runtimeConfig := *fileConfig
	runtimeConfig.Storage = runtimeStorage
	runtimeLogging, err := config.ResolveLoggingPaths(fileConfig.Logging, workingDir)
	if err != nil {
		return nil, nil, err
	}
	runtimeConfig.Logging = runtimeLogging
	return fileConfig, &runtimeConfig, nil
}

// Convert legacy file references before workers, asset maintenance or HTTP
// handlers can observe paths tied to the previous deployment directory.
func openApplicationCatalog(storage config.Storage) (*catalog.Catalog, error) {
	cat, err := catalog.Open(storage.DBPath)
	if err != nil {
		return nil, err
	}
	changed, err := cat.MigrateManagedPaths(context.Background(), storage.LocalPreviewDir)
	if err != nil {
		_ = cat.Close()
		return nil, fmt.Errorf("convert managed file references: %w", err)
	}
	if changed > 0 {
		log.Printf("[storage] converted %d catalog records to portable file references", changed)
	}
	return cat, nil
}

func readVersionFile(path string) string {
	data, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return ""
	}
	line := strings.SplitN(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n", 2)[0]
	return strings.TrimSpace(line)
}

func restartIsManaged() bool {
	if raw := strings.TrimSpace(os.Getenv("VIDEO_RESTART_MANAGED")); raw != "" {
		return parseBoolDefault(raw, false)
	}
	if strings.TrimSpace(os.Getenv("INVOCATION_ID")) != "" {
		return true
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	return false
}

func runHashPasswordCommand(r io.Reader, w io.Writer) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read password: %w", err)
	}
	password := strings.TrimRight(string(data), "\r\n")
	if len(password) < 6 {
		return fmt.Errorf("password must be at least 6 characters")
	}
	hashed, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	_, err = fmt.Fprintln(w, hashed)
	return err
}
