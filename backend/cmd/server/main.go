package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
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
	"github.com/video-site/backend/internal/drives/scriptcrawler"
	"github.com/video-site/backend/internal/fingerprint"
	"github.com/video-site/backend/internal/nightly"
	"github.com/video-site/backend/internal/preview"
	"github.com/video-site/backend/internal/proxy"
	"github.com/video-site/backend/internal/remoteupload"
	"github.com/video-site/backend/internal/subtitles"
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
	if len(os.Args) > 1 && os.Args[1] == "hash-password" {
		if err := runHashPasswordCommand(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	// stderr is always kept as the operational log sink. The durable admin log
	// file is attached after configuration paths have been resolved.
	log.SetOutput(os.Stderr)

	cfgPath := "./config.yaml"
	if v := os.Getenv("VIDEO_CONFIG"); v != "" {
		cfgPath = v
	}
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
	dataRoot := filepath.Dir(cfg.Storage.DBPath)
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

	if err := os.MkdirAll(filepath.Dir(cfg.Storage.DBPath), 0o755); err != nil {
		log.Fatalf("mkdir db dir: %v", err)
	}
	if err := os.MkdirAll(cfg.Storage.LocalPreviewDir, 0o755); err != nil {
		log.Fatalf("mkdir preview dir: %v", err)
	}

	cat, err := catalog.Open(cfg.Storage.DBPath)
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
	legacyRuntimeSettings, err := loadLegacyRuntimeSettings(context.Background(), cat)
	if err != nil {
		log.Fatalf("load legacy runtime settings: %v", err)
	}
	configMigrated, err := configManager.MigrateLegacyRuntimeSettings(legacyRuntimeSettings)
	if err != nil {
		log.Fatalf("migrate config.yaml runtime settings: %v", err)
	}
	if err := cat.DeleteSettings(
		context.Background(),
		legacyNightlyStartTimeSetting,
		legacyBuiltinTagsEnabledSetting,
		obsoleteDuplicateReviewEnabledSetting,
	); err != nil {
		log.Fatalf("remove migrated SQLite configuration: %v", err)
	}
	if configMigrated {
		fileConfig, cfg, err = loadApplicationConfig(cfgPath, workingDir)
		if err != nil {
			log.Fatalf("reload migrated config: %v", err)
		}
		log.Printf("[config] migrated runtime settings into config.yaml")
	}

	var logStore *applog.Store
	if cfg.Logging.IsFileEnabled() {
		logStore, err = applog.Open(applog.Config{
			Directory:         cfg.Logging.Directory,
			MaxLineBytes:      applog.DefaultMaxLineBytes,
			MaxFileSizeBytes:  int64(cfg.Logging.MaxFileSizeMB) * 1024 * 1024,
			MaxTotalSizeBytes: int64(cfg.Logging.MaxTotalSizeMB) * 1024 * 1024,
		})
		if err != nil {
			log.Printf("[logging] file logging unavailable: %v", err)
			logStore = nil
		} else {
			defer func() {
				if closeErr := logStore.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "close runtime log: %v\n", closeErr)
				}
			}()
			log.SetOutput(io.MultiWriter(os.Stderr, logStore.Writer(applog.SourceApplication)))
			log.Printf("[logging] durable runtime log enabled dir=%s max_file_mb=%d max_total_mb=%d",
				logStore.Directory(), cfg.Logging.MaxFileSizeMB, cfg.Logging.MaxTotalSizeMB)
		}
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

	if _, err := app.normalizeLegacyThumbnailFiles(ctx); err != nil {
		log.Printf("[thumbnail-maintenance] migration failed: %v", err)
	}
	app.loadTheme(ctx)
	if removed, err := app.cleanupOrphanDriveVideos(ctx); err != nil {
		log.Printf("[cleanup] orphan drive videos: %v", err)
	} else if removed > 0 {
		log.Printf("[cleanup] removed %d orphan drive videos", removed)
	}
	if err := app.attachLocalUpload(ctx); err != nil {
		log.Printf("[local-upload] attach failed: %v", err)
	}
	go app.runFingerprintReconciler(ctx)

	remoteUploader, err := remoteupload.New(remoteupload.Config{
		Catalog:     cat,
		UploadDir:   app.localUploadDir(),
		FFprobePath: cfg.Preview.FFprobePath,
		DiskReserve: cfg.RemoteUpload.DiskReserveBytes,
		IdleTimeout: time.Duration(cfg.RemoteUpload.IdleTimeoutSeconds) * time.Second,
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

	authr := &auth.Authenticator{
		Username: cfg.Server.Admin.Username,
		Password: cfg.Server.Admin.Password,
		Catalog:  cat,
	}
	setupRequired := config.RequiresAdminSetup(cfg)
	if !setupRequired {
		if err := ensureConfigAdminUser(ctx, cat, cfg); err != nil {
			log.Printf("[auth] migrate config admin: %v", err)
		}
	}
	var setupMu sync.Mutex
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
		Catalog:        cat,
		AppConfig:      fileConfig,
		RuntimeStorage: cfg.Storage,
		ConfigPath:     cfgPath,
		AppVersion:     appVersion,
		RestartManaged: restartIsManaged(),
	})
	if err != nil {
		log.Fatalf("configure backup service: %v", err)
	}
	backupManager.Start(ctx)
	defer backupManager.Close()
	backupTransferManager, err := backuptransfer.New(backuptransfer.Config{
		Backups: backupManager,
		RootDir: filepath.Join(filepath.Dir(cfg.Storage.DBPath), "backups", ".peer-transfer"),
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
		GetTheme: func() string { return app.Theme() },
	}
	app.onTagsChanged = apiServer.InvalidateTagCache

	adminServer := &api.AdminServer{
		Catalog:         cat,
		Auth:            authr,
		Backups:         backupManager,
		BackupTransfers: backupTransferManager,
		Logs:            logStore,
		ConfigManager:   configManager,
		VersionFilePath: versionFilePath,
		ImageVersion:    imageVersion,
		GitHubRepo:      githubRepo,
		SetupRequired: func() bool {
			setupMu.Lock()
			defer setupMu.Unlock()
			return setupRequired
		},
		OnSetup: func(username, password string) error {
			setupMu.Lock()
			defer setupMu.Unlock()
			if !setupRequired {
				return nil
			}
			if err := configManager.UpdateAdminCredentials(username, password); err != nil {
				return err
			}
			hashed, err := auth.HashPassword(password)
			if err != nil {
				return err
			}
			if _, err := cat.CreateUser(ctx, username, hashed, "admin"); err != nil {
				return err
			}
			fileConfig.Server.Admin.Username = username
			fileConfig.Server.Admin.Password = password
			cfg.Server.Admin.Username = username
			cfg.Server.Admin.Password = password
			authr.SetCredentials(username, password)
			setupRequired = false
			return nil
		},
		LocalPreviewDir: cfg.Storage.LocalPreviewDir,
		OnDriveSaved: func(driveID string) error {
			return app.reloadSavedDrive(ctx, driveID)
		},
		OnDriveDeleteCleanup: func(cleanupCtx context.Context, driveID string) (int, error) {
			return app.cleanupDriveVideosForDelete(cleanupCtx, driveID)
		},
		OnDriveRemoved: func(driveID string) {
			app.detachDrive(driveID)
		},
		OnScanRequested: func(driveID string) bool {
			// 爬虫类 drive 的"重扫"等同于手动触发一次爬取；其它 drive 走标准 scan
			isScriptCrawler := false
			if d, err := app.cat.GetDrive(ctx, driveID); err == nil && d != nil {
				isScriptCrawler = d.Kind == scriptcrawler.Kind
			}
			if isScriptCrawler {
				return app.scheduleScriptCrawlerCrawl(ctx, driveID)
			}
			return app.scheduleScan(ctx, driveID)
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
		OnRegenFailedPreviews: func(driveID string) {
			go app.regenFailedPreviews(ctx, driveID)
		},
		OnRegenFailedThumbnails: func(driveID string) {
			go app.regenFailedThumbnails(ctx, driveID)
		},
		OnRegenFailedFingerprints: func(driveID string) {
			go app.regenFailedFingerprints(ctx, driveID)
		},
		OnStartDriveTranscode: func(driveID string) (bool, string) {
			return app.startDriveTranscode(ctx, driveID)
		},
		OnStopDriveTranscode: func(driveID string) bool {
			return app.stopDriveTranscode(driveID)
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
		GetDriveGenerationStatuses: func() map[string]api.DriveGenerationStatuses {
			return app.driveGenerationStatuses()
		},
		GetPreviewGenerationVideoIDs: func() map[string]bool {
			return app.previewGenerationVideoIDs()
		},
		OnTeaserEnabledChanged: func(driveID string, enabled bool) {
			// 从关到开时立刻补扫该盘 pending 预览视频，行为对齐旧的"全局开关从关到开"。
			// 关闭分支不需要做事 —— 入队前会重新查 catalog，新的 enqueue 自然停。
			if !enabled {
				return
			}
			app.mu.Lock()
			worker := app.workers[driveID]
			thumbWorker := app.thumbWorkers[driveID]
			app.mu.Unlock()
			go app.enqueueDriveGeneration(ctx, driveID, worker, thumbWorker)
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

	// 凌晨流水线：每天按后台可热更新的 HH:mm + IANA 时区触发一次，串行跑
	//   Phase 1 扫所有非爬虫 / localupload 网盘 + 删除检测 + 入队封面/预览视频
	//   Phase 2 脚本爬虫 + 入队预览视频
	//   Phase 3 爬虫本地视频 → 云盘上传
	//   Phase 4 扫描爬虫本地目录并恢复已取消拉黑的视频
	//   Phase 5 全库重复视频维护：精确指纹去重 + 标题/时长/封面近似去重
	// 标签匹配不在夜间流水线中全库重算；新视频入库和管理员修改标签规则时按事件刷新。
	// admin "扫描所有网盘" 使用同一个 Runner 的独立 scan-all 模式，只运行
	// Phase 1 和 Phase 5，不触发爬虫、迁移或恢复，也不占用当天的定时执行标记。
	liveSettings := app.liveConfigSettings()
	app.nightlyRunner = nightly.New(nightly.Config{
		Settings:              cat,
		StartTime:             liveSettings.NightlyStartTime,
		Timezone:              liveSettings.NightlyTimezone,
		ListScanTargets:       app.listScanTargetIDs,
		RunScan:               app.runScan,
		ListCrawlerDrives:     app.listCrawlerDriveIDs,
		RunCrawlerCrawl:       app.runScriptCrawlerCrawl,
		WaitPreviewQueuesIdle: app.waitAllPreviewQueuesIdle,
		RunMigration:          app.crawlerUploader.RunOnce,
		RestoreCrawlerVideos:  app.restoreScriptCrawlerVideos,
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

func ensureConfigAdminUser(ctx context.Context, cat *catalog.Catalog, cfg *config.Config) error {
	if cat == nil || cfg == nil {
		return nil
	}
	username := strings.TrimSpace(cfg.Server.Admin.Username)
	password := cfg.Server.Admin.Password
	if username == "" || password == "" {
		return nil
	}
	if _, err := cat.GetUserByUsername(ctx, username); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	hashed, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	_, err = cat.CreateUser(ctx, username, hashed, "admin")
	return err
}
