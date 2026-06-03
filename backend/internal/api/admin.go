package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/video-site/backend/internal/auth"
	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives/p123"
)

type AdminServer struct {
	Catalog *catalog.Catalog
	Auth    *auth.Authenticator
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
	OnDriveSaved               func(driveID string) error
	OnDriveDeleteCleanup       func(ctx context.Context, driveID string) (int, error)
	OnDriveRemoved             func(driveID string)
	OnScanRequested            func(driveID string)
	OnRegenPreview             func(videoID string)
	OnRegenAllPreviews         func()
	OnRegenFailedPreviews      func(driveID string)
	OnRegenFailedThumbnails    func(driveID string)
	OnRegenFailedFingerprints  func(driveID string)
	GetDriveGenerationStatuses func() map[string]DriveGenerationStatuses
	// OnTeaserEnabledChanged 在 per-drive 预览视频开关被切换后调用。
	// enabled=true 时上层应该重新把 pending 预览视频入队（类似旧的全局开关从关到开）；
	// enabled=false 时通常不用做事 —— worker 入队前会再次查 catalog，自然停止。
	OnTeaserEnabledChanged func(driveID string, enabled bool)
	// Theme 读写（"dark" | "pink"）
	GetTheme func() string
	SetTheme func(theme string) error
	// Spider91 → 115/PikPak 上传目标 drive ID 读写
	GetSpider91UploadDriveID func() string
	SetSpider91UploadDriveID func(driveID string) error
	// OnRunNightlyJob 触发一次完整的凌晨流水线（Phase1 扫盘 + Phase2 91 爬虫 +
	// Phase3 迁移）。立即返回 —— 实际任务在后台跑，admin 在日志或下次状态查询里
	// 看进度。若流水线正在跑或已排队，Runner 会拒绝重复触发。
	OnRunNightlyJob func() bool
	// GetNightlyJobStatus 返回凌晨流水线当前状态，用于前端禁用重复触发按钮。
	GetNightlyJobStatus func() NightlyJobStatus
	// ListDriveDirChildren 列出某个 drive 在 parentID 目录下的直接子目录。
	// parentID 为空时使用 drive 的 RootID。返回 (子目录列表, error)。
	// 用于"设置跳过目录"弹窗按需展开浏览网盘目录树；只返回目录条目，文件忽略。
	// 调用方应当处理 error 并以 5xx 返回前端。
	ListDriveDirChildren func(ctx context.Context, driveID, parentID string) ([]DriveDirEntry, error)
	// 123 云盘扫码登录接口测试注入；生产留空走官方 user.123pan.cn。
	P123UserAPIBaseURL string
	P123HTTPClient     *http.Client
}

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
}

type DriveGenerationStatuses struct {
	Thumbnail   GenerationStatus `json:"thumbnail"`
	Preview     GenerationStatus `json:"preview"`
	Fingerprint GenerationStatus `json:"fingerprint"`
}

type NightlyJobStatus struct {
	State          string `json:"state"`
	Running        bool   `json:"running"`
	Queued         bool   `json:"queued"`
	StartedAt      string `json:"startedAt,omitempty"`
	LastFinishedAt string `json:"lastFinishedAt,omitempty"`
}

func (a *AdminServer) Register(r chi.Router) {
	r.Route("/admin/api", func(r chi.Router) {
		// 登录、登出和首次部署初始化不需要鉴权
		r.Get("/setup", a.handleSetupStatus)
		r.Post("/setup", a.handleSetup)
		r.Post("/login", a.handleLogin)
		r.Post("/logout", a.handleLogout)
		r.Get("/me", a.handleMe)

		// 其余路由需鉴权
		r.Group(func(r chi.Router) {
			r.Use(a.Auth.Required)

			// 网盘
			r.Get("/drives", a.handleListDrives)
			r.Get("/drives/storage", a.handleDriveStorage)
			r.Post("/drives", a.handleUpsertDrive)
			r.Post("/drives/p123/qr", a.handleP123QRStart)
			r.Get("/drives/p123/qr/{uniID}", a.handleP123QRStatus)
			r.Delete("/drives/{id}", a.handleDeleteDrive)
			r.Post("/drives/{id}/rescan", a.handleRescan)
			r.Post("/drives/{id}/teaser-enabled", a.handleSetDriveTeaserEnabled)
			r.Post("/drives/{id}/skip-dirs", a.handleSetDriveSkipDirs)
			r.Get("/drives/{id}/dirtree", a.handleListDriveDirTree)
			r.Post("/drives/{id}/previews/failed/regenerate", a.handleRegenFailedPreviews)
			r.Post("/drives/{id}/thumbnails/failed/regenerate", a.handleRegenFailedThumbnails)
			r.Post("/drives/{id}/fingerprints/failed/regenerate", a.handleRegenFailedFingerprints)

			// 视频
			r.Get("/videos", a.handleAdminListVideos)
			r.Put("/videos/{id}", a.handleUpdateVideo)
			r.Post("/videos/regen-preview", a.handleRegenAllPreviews)
			r.Post("/videos/{id}/frontend-selected", a.handleSetVideoFrontendSelected)
			r.Post("/videos/{id}/regen-preview", a.handleRegenPreview)

			// 标签
			r.Get("/tags", a.handleListTags)
			r.Post("/tags", a.handleCreateTag)
			r.Post("/tags/{id}/frontend-access", a.handleSetTagFrontendAccess)
			r.Delete("/tags/{id}", a.handleDeleteTag)

			// 运行时设置
			r.Get("/settings", a.handleGetSettings)
			r.Put("/settings", a.handlePutSettings)

			// 运维任务
			r.Get("/update/check", a.handleCheckUpdate)
			r.Get("/jobs/nightly/status", a.handleNightlyJobStatus)
			r.Post("/jobs/nightly/run", a.handleRunNightlyJob)
		})
	})
}

type updateCheckDTO struct {
	CurrentVersion string `json:"currentVersion"`
	LatestVersion  string `json:"latestVersion"`
	HasUpdate      bool   `json:"hasUpdate"`
	ReleaseURL     string `json:"releaseUrl,omitempty"`
	CheckedAt      string `json:"checkedAt"`
}

type githubReleaseDTO struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type setupReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *AdminServer) setupRequired() bool {
	return a.SetupRequired != nil && a.SetupRequired()
}

func (a *AdminServer) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"required": a.setupRequired()})
}

func (a *AdminServer) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !a.setupRequired() {
		http.Error(w, "setup already completed", http.StatusConflict)
		return
	}
	if a.OnSetup == nil || a.Auth == nil {
		http.Error(w, "setup is not available", http.StatusInternalServerError)
		return
	}
	var body setupReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	username := strings.TrimSpace(body.Username)
	password := body.Password
	if username == "" {
		http.Error(w, "username is required", http.StatusBadRequest)
		return
	}
	if len(password) < 6 {
		http.Error(w, "password must be at least 6 characters", http.StatusBadRequest)
		return
	}
	if err := a.OnSetup(username, password); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	ok, err := a.Auth.Login(w, r, username, password)
	if err != nil {
		if errors.Is(err, auth.ErrLoginIPBanned) {
			http.Error(w, "ip banned", http.StatusForbidden)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		http.Error(w, "setup completed but login failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *AdminServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	if a.setupRequired() {
		http.Error(w, "setup required", http.StatusPreconditionRequired)
		return
	}
	var body loginReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ok, err := a.Auth.Login(w, r, body.Username, body.Password)
	if err != nil {
		if errors.Is(err, auth.ErrLoginIPBanned) {
			http.Error(w, "ip banned", http.StatusForbidden)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *AdminServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	a.Auth.Logout(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *AdminServer) handleMe(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("vs_admin")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	ok, _ := a.Catalog.ValidateSession(r.Context(), c.Value)
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": ok})
}

func (a *AdminServer) handleCheckUpdate(w http.ResponseWriter, r *http.Request) {
	info, err := a.checkUpdate(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, info)
}

func (a *AdminServer) checkUpdate(ctx context.Context) (updateCheckDTO, error) {
	current := a.installedVersion()
	if current == "" {
		current = "unknown"
	}
	release, err := a.latestRelease(ctx)
	if err != nil {
		return updateCheckDTO{
			CurrentVersion: current,
			CheckedAt:      time.Now().Format(time.RFC3339),
		}, err
	}
	latest := strings.TrimSpace(release.TagName)
	return updateCheckDTO{
		CurrentVersion: current,
		LatestVersion:  latest,
		HasUpdate:      current != "unknown" && latest != "" && current != latest,
		ReleaseURL:     release.HTMLURL,
		CheckedAt:      time.Now().Format(time.RFC3339),
	}, nil
}

func (a *AdminServer) installedVersion() string {
	if version := strings.TrimSpace(a.ImageVersion); version != "" {
		return version
	}
	path := strings.TrimSpace(a.VersionFilePath)
	if path == "" {
		path = ".version"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[0])
}

func (a *AdminServer) latestRelease(ctx context.Context) (githubReleaseDTO, error) {
	url := strings.TrimSpace(a.ReleaseAPIURL)
	if url == "" {
		repo := strings.TrimSpace(a.GitHubRepo)
		if repo == "" {
			repo = "nianzhibai/91"
		}
		url = "https://api.github.com/repos/" + repo + "/releases/latest"
	}
	client := a.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return githubReleaseDTO{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "video-site-91")
	res, err := client.Do(req)
	if err != nil {
		return githubReleaseDTO{}, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return githubReleaseDTO{}, fmt.Errorf("github release check failed: HTTP %d", res.StatusCode)
	}
	var release githubReleaseDTO
	if err := json.NewDecoder(res.Body).Decode(&release); err != nil {
		return githubReleaseDTO{}, err
	}
	if strings.TrimSpace(release.TagName) == "" {
		return githubReleaseDTO{}, errors.New("github release check returned empty tag")
	}
	return release, nil
}

func (a *AdminServer) handleListDrives(w http.ResponseWriter, r *http.Request) {
	drives, err := a.Catalog.ListDrives(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	teaserCounts, err := a.Catalog.CountTeasersByDrive(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	thumbnailCounts, err := a.Catalog.CountThumbnailsByDrive(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	fingerprintCounts, err := a.Catalog.CountFingerprintsByDrive(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	generationStatuses := map[string]DriveGenerationStatuses{}
	if a.GetDriveGenerationStatuses != nil {
		generationStatuses = a.GetDriveGenerationStatuses()
	}
	// 出参不返回凭证明文，只告诉前端是否已配置
	type out struct {
		ID            string `json:"id"`
		Kind          string `json:"kind"`
		Name          string `json:"name"`
		RootID        string `json:"rootId"`
		ScanRootID    string `json:"scanRootId"`
		Status        string `json:"status"`
		LastError     string `json:"lastError,omitempty"`
		HasCredential bool   `json:"hasCredential"`
		// TeaserEnabled 控制是否给本盘生成预览视频/封面。前端用它在网盘列表/编辑表单展示开关状态。
		TeaserEnabled bool `json:"teaserEnabled"`
		// SkipDirIDs 是用户在 admin 配置的"扫描跳过目录"集合（drive 侧目录 fileID）。
		// 前端用它在"设置跳过目录"弹窗里回显已选项；JSON 字段名 camelCase 与
		// catalog.Drive 保持一致。
		SkipDirIDs []string `json:"skipDirIds"`
		// LastCrawlAt 是 spider91 上次成功爬取的 unix 秒（来自 credentials.last_crawl_at）。
		// 其它 kind 留 0；前端用它显示"上次抓取: N 小时前"。
		Spider91Proxy                 string           `json:"spider91Proxy,omitempty"`
		LastCrawlAt                   int64            `json:"lastCrawlAt,omitempty"`
		ThumbnailGenerationStatus     GenerationStatus `json:"thumbnailGenerationStatus"`
		PreviewGenerationStatus       GenerationStatus `json:"previewGenerationStatus"`
		FingerprintGenerationStatus   GenerationStatus `json:"fingerprintGenerationStatus"`
		ThumbnailReadyCount           int              `json:"thumbnailReadyCount"`
		ThumbnailPendingCount         int              `json:"thumbnailPendingCount"`
		ThumbnailFailedCount          int              `json:"thumbnailFailedCount"`
		ThumbnailDurationPendingCount int              `json:"thumbnailDurationPendingCount"`
		TeaserReadyCount              int              `json:"teaserReadyCount"`
		TeaserPendingCount            int              `json:"teaserPendingCount"`
		TeaserFailedCount             int              `json:"teaserFailedCount"`
		FingerprintReadyCount         int              `json:"fingerprintReadyCount"`
		FingerprintPendingCount       int              `json:"fingerprintPendingCount"`
		FingerprintFailedCount        int              `json:"fingerprintFailedCount"`
	}
	list := make([]out, 0, len(drives))
	for _, d := range drives {
		counts := teaserCounts[d.ID]
		thumbCounts := thumbnailCounts[d.ID]
		fingerprintCount := fingerprintCounts[d.ID]
		generation := generationStatuses[d.ID]
		if generation.Thumbnail.State == "" {
			generation.Thumbnail.State = "idle"
		}
		if generation.Preview.State == "" {
			generation.Preview.State = "idle"
		}
		if generation.Fingerprint.State == "" {
			generation.Fingerprint.State = "idle"
		}
		// spider91 没有用户凭证概念；只要存在 drive 行就视为"已配置"。
		// last_crawl_at 是后端自动写入的运行状态字段，不计入 hasCredential 判定。
		hasCred := false
		userCredKeys := 0
		for k := range d.Credentials {
			if k == "last_crawl_at" {
				continue
			}
			userCredKeys++
		}
		hasCred = userCredKeys > 0 || d.Kind == "spider91"

		var lastCrawlAt int64
		if d.Credentials != nil {
			if raw, ok := d.Credentials["last_crawl_at"]; ok && raw != "" {
				if v, err := strconv.ParseInt(raw, 10, 64); err == nil {
					lastCrawlAt = v
				}
			}
		}

		list = append(list, out{
			ID: d.ID, Kind: d.Kind, Name: d.Name,
			RootID: d.RootID, ScanRootID: d.ScanRootID,
			Status: d.Status, LastError: d.LastError,
			HasCredential:                 hasCred,
			TeaserEnabled:                 d.TeaserEnabled,
			SkipDirIDs:                    append([]string{}, d.SkipDirIDs...),
			Spider91Proxy:                 spider91ProxyForDrive(d),
			LastCrawlAt:                   lastCrawlAt,
			ThumbnailGenerationStatus:     generation.Thumbnail,
			PreviewGenerationStatus:       generation.Preview,
			FingerprintGenerationStatus:   generation.Fingerprint,
			ThumbnailReadyCount:           thumbCounts.Ready,
			ThumbnailPendingCount:         thumbCounts.Pending,
			ThumbnailFailedCount:          thumbCounts.Failed,
			ThumbnailDurationPendingCount: thumbCounts.DurationPending,
			TeaserReadyCount:              counts.Ready,
			TeaserPendingCount:            counts.Pending,
			TeaserFailedCount:             counts.Failed,
			FingerprintReadyCount:         fingerprintCount.Ready,
			FingerprintPendingCount:       fingerprintCount.Pending,
			FingerprintFailedCount:        fingerprintCount.Failed,
		})
	}
	writeJSON(w, http.StatusOK, list)
}

type upsertDriveReq struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	RootID string `json:"rootId"`
	// Deprecated: 扫描起点已固定为 rootId；保留字段只为兼容旧客户端请求体。
	ScanRootID  string            `json:"scanRootId"`
	Credentials map[string]string `json:"credentials"`
	// TeaserEnabled 是 per-drive 预览视频/封面生成开关。
	// 用 *bool 区分 "未传" / "传了 false"：未传时表示客户端不打算改这个字段，
	// 沿用 catalog 现有值；新建时未传一律默认开启（true）。
	TeaserEnabled *bool `json:"teaserEnabled,omitempty"`
	// SkipDirIDs 同样用指针区分 "未传"（沿用旧值）/ "传了空数组"（清空）。
	// 推荐前端"设置跳过目录"走专用 POST /drives/{id}/skip-dirs；
	// 这里支持是为了允许批量编辑场景一次性提交。
	SkipDirIDs *[]string `json:"skipDirIds,omitempty"`
}

func (a *AdminServer) handleUpsertDrive(w http.ResponseWriter, r *http.Request) {
	var body upsertDriveReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if body.ID == "" || body.Kind == "" {
		http.Error(w, "id and kind are required", http.StatusBadRequest)
		return
	}
	// 凭证 / TeaserEnabled 都支持 "未传 = 沿用旧值"：先把现存 drive 拉出来一次。
	var existing *catalog.Drive
	if existingDrive, err := a.Catalog.GetDrive(r.Context(), body.ID); err == nil {
		existing = existingDrive
	}
	if body.Kind == "spider91" {
		credentials, err := mergeSpider91Credentials(existing, body.Credentials)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		body.Credentials = credentials
	} else if len(body.Credentials) == 0 && existing != nil && len(existing.Credentials) > 0 {
		body.Credentials = existing.Credentials
	}

	// teaserEnabled 解析顺序：
	//   1. 请求显式带了 → 用请求值
	//   2. 请求没带 + 编辑现有 drive → 沿用旧值
	//   3. 请求没带 + 新建 drive → 默认 true（用户没特别说就生成）
	teaserEnabled := true
	switch {
	case body.TeaserEnabled != nil:
		teaserEnabled = *body.TeaserEnabled
	case existing != nil:
		teaserEnabled = existing.TeaserEnabled
	}

	// skipDirIds 解析顺序：
	//   1. 请求显式带了（包括空数组）→ 用请求值（空数组 = 清空）
	//   2. 请求没带 + 编辑现有 drive → 沿用旧值
	//   3. 请求没带 + 新建 drive → nil（不跳过任何目录）
	var skipDirIDs []string
	switch {
	case body.SkipDirIDs != nil:
		skipDirIDs = *body.SkipDirIDs
	case existing != nil:
		skipDirIDs = existing.SkipDirIDs
	}

	d := &catalog.Drive{
		ID: body.ID, Kind: body.Kind, Name: body.Name,
		RootID:        body.RootID,
		Credentials:   body.Credentials,
		Status:        "disconnected",
		TeaserEnabled: teaserEnabled,
		SkipDirIDs:    skipDirIDs,
	}
	if err := a.Catalog.UpsertDrive(r.Context(), d); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if a.OnDriveSaved != nil {
		if err := a.OnDriveSaved(body.ID); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func spider91ProxyForDrive(d *catalog.Drive) string {
	if d == nil || d.Kind != "spider91" || d.Credentials == nil {
		return ""
	}
	return strings.TrimSpace(d.Credentials["proxy"])
}

func mergeSpider91Credentials(existing *catalog.Drive, incoming map[string]string) (map[string]string, error) {
	merged := map[string]string{}
	if existing != nil {
		for k, v := range existing.Credentials {
			merged[k] = v
		}
	}
	for k, v := range incoming {
		if strings.TrimSpace(k) == "" {
			continue
		}
		if k == "proxy" {
			proxy, err := normalizeSpider91ProxyURL(v)
			if err != nil {
				return nil, err
			}
			if proxy == "" {
				delete(merged, "proxy")
			} else {
				merged["proxy"] = proxy
			}
			continue
		}
		merged[k] = v
	}
	return merged, nil
}

func normalizeSpider91ProxyURL(raw string) (string, error) {
	proxy := strings.TrimSpace(raw)
	if proxy == "" {
		return "", nil
	}
	u, err := url.Parse(proxy)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("91Spider 代理地址格式无效，请填写类似 http://127.0.0.1:7890 的地址")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
		return proxy, nil
	default:
		return "", fmt.Errorf("91Spider 代理地址仅支持 http://、https://、socks5:// 或 socks5h://")
	}
}

func (a *AdminServer) handleDeleteDrive(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body deleteDriveReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !body.DeleteVideos {
		http.Error(w, "deleteVideos=true is required when deleting a drive", http.StatusBadRequest)
		return
	}

	deletedVideos := 0
	if a.OnDriveDeleteCleanup == nil {
		http.Error(w, "drive video cleanup is not available", http.StatusInternalServerError)
		return
	}
	removed, err := a.OnDriveDeleteCleanup(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	deletedVideos = removed

	if err := a.Catalog.DeleteDrive(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if a.OnDriveRemoved != nil {
		a.OnDriveRemoved(id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deletedVideos": deletedVideos})
}

type deleteDriveReq struct {
	DeleteVideos bool `json:"deleteVideos"`
}

func (a *AdminServer) handleRescan(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if a.OnScanRequested != nil {
		a.OnScanRequested(id)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (a *AdminServer) p123QRClient() *p123.QRClient {
	return p123.NewQRClient(p123.QRConfig{
		UserAPIBaseURL: a.P123UserAPIBaseURL,
		HTTPClient:     a.P123HTTPClient,
	})
}

func (a *AdminServer) handleP123QRStart(w http.ResponseWriter, r *http.Request) {
	session, err := a.p123QRClient().Generate(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, session)
}

func (a *AdminServer) handleP123QRStatus(w http.ResponseWriter, r *http.Request) {
	uniID := chi.URLParam(r, "uniID")
	loginUUID := r.URL.Query().Get("loginUuid")
	if strings.TrimSpace(uniID) == "" || strings.TrimSpace(loginUUID) == "" {
		http.Error(w, "uniID and loginUuid are required", http.StatusBadRequest)
		return
	}
	status, err := a.p123QRClient().Poll(r.Context(), loginUUID, uniID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, status)
}

// handleRunNightlyJob 触发一次完整的凌晨流水线（不论当前时间，不论今日是否已跑）。
// 立即返回 202；进度通过 backend 日志和下次 GET /admin/api/drives 的状态变化观察。
// 流水线已在跑或已排队时，Runner 会拒绝重复触发。
func (a *AdminServer) handleRunNightlyJob(w http.ResponseWriter, r *http.Request) {
	accepted := false
	if a.OnRunNightlyJob != nil {
		accepted = a.OnRunNightlyJob()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok":       true,
		"accepted": accepted,
		"status":   a.nightlyJobStatus(),
	})
}

func (a *AdminServer) handleNightlyJobStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.nightlyJobStatus())
}

func (a *AdminServer) nightlyJobStatus() NightlyJobStatus {
	if a.GetNightlyJobStatus == nil {
		return NightlyJobStatus{State: "idle"}
	}
	status := a.GetNightlyJobStatus()
	if status.State == "" {
		status.State = "idle"
	}
	return status
}

// teaserEnabledReq 是 POST /admin/api/drives/{id}/teaser-enabled 的入参。
type teaserEnabledReq struct {
	Enabled bool `json:"enabled"`
}

// handleSetDriveTeaserEnabled 切换某盘的预览视频生成开关。
//
// 行为：
//   - 写 catalog.drives.teaser_enabled
//   - 调 OnTeaserEnabledChanged（main 注入；从关到开时会重新入队 pending 预览视频）
//   - 返回切换后的新值，方便前端乐观更新但又能以服务端为准
//
// 与 upsertDrive 的区别：那条接口要重传 kind / name / rootId 等，开关切换不该
// 牵连这些字段（顺手覆盖凭证或 rootID 容易出 bug）。所以单独走一条。
func (a *AdminServer) handleSetDriveTeaserEnabled(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	var body teaserEnabledReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.Catalog.SetDriveTeaserEnabled(r.Context(), id, body.Enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "drive not found", http.StatusNotFound)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if a.OnTeaserEnabledChanged != nil {
		a.OnTeaserEnabledChanged(id, body.Enabled)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "teaserEnabled": body.Enabled})
}

// skipDirsReq 是 POST /admin/api/drives/{id}/skip-dirs 的入参。
//
// 整体覆盖语义：传啥就保存啥（不是增量合并）。dirIds 可以是 nil/空数组 表示
// 清空跳过列表。
type skipDirsReq struct {
	DirIDs []string `json:"dirIds"`
}

// handleSetDriveSkipDirs 更新某盘的"扫描跳过目录"集合。
//
// 与 upsertDrive 的区别：那条接口要重传 kind / name / rootId / credentials 等字段，
// 用户保存跳过目录时不该牵连这些。所以单独走一条 PUT 风格接口。
//
// 行为：
//   - 写 catalog.drives.skip_dir_ids（整体覆盖）
//   - 不重新触发扫描；下次 nightly Phase 1 或 admin 手动重扫时生效
//   - 返回保存后的列表，方便前端乐观更新但又能以服务端为准
func (a *AdminServer) handleSetDriveSkipDirs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	var body skipDirsReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// 去重 + trim 空白；前端理论上保证清洁，这里再防一道。
	seen := map[string]struct{}{}
	cleaned := make([]string, 0, len(body.DirIDs))
	for _, raw := range body.DirIDs {
		s := raw
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		cleaned = append(cleaned, s)
	}
	if err := a.Catalog.SetDriveSkipDirIDs(r.Context(), id, cleaned); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "drive not found", http.StatusNotFound)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "skipDirIds": cleaned})
}

// handleListDriveDirTree 列出某 drive 在指定父目录下的直接子目录。
//
// 查询参数 ?parent=<dirID>：留空 = drive 的 RootID。前端按需展开调用 ——
// 每展开一层调一次，避免一次性递归整个网盘（115 限频会很难受）。
//
// 错误：drive 未挂载 / List 失败 → 500，body 是错误文案；前端展示给用户。
func (a *AdminServer) handleListDriveDirTree(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	if a.ListDriveDirChildren == nil {
		writeErr(w, http.StatusInternalServerError, errors.New("dirtree not configured"))
		return
	}
	parent := r.URL.Query().Get("parent")
	entries, err := a.ListDriveDirChildren(r.Context(), id, parent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if entries == nil {
		entries = []DriveDirEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

func (a *AdminServer) handleAdminListVideos(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	size, _ := strconv.Atoi(q.Get("size"))
	if page <= 0 {
		page = 1
	}
	if size <= 0 || size > 100 {
		size = 100
	}
	items, total, err := a.Catalog.ListVideos(r.Context(), catalog.ListParams{
		Keyword:  q.Get("keyword"),
		DriveID:  q.Get("driveId"),
		Page:     page,
		PageSize: size,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"total": total,
		"page":  page,
		"size":  size,
	})
}

func (a *AdminServer) handleListTags(w http.ResponseWriter, r *http.Request) {
	tags, err := a.Catalog.ListTags(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, tags)
}

type createTagReq struct {
	Label   string   `json:"label"`
	Aliases []string `json:"aliases"`
}

func (a *AdminServer) handleCreateTag(w http.ResponseWriter, r *http.Request) {
	var body createTagReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	classified, err := a.Catalog.CreateTagAndClassify(r.Context(), body.Label, body.Aliases, "user")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"label":      body.Label,
		"classified": classified,
	})
}

func (a *AdminServer) handleDeleteTag(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, errors.New("invalid tag id"))
		return
	}
	removedVideos, err := a.Catalog.DeleteTag(r.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			writeErr(w, http.StatusNotFound, err)
		case errors.Is(err, catalog.ErrSystemTag):
			writeErr(w, http.StatusBadRequest, err)
		default:
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removedVideos": removedVideos})
}

type updateVideoReq struct {
	Title       string   `json:"title"`
	Author      string   `json:"author"`
	Tags        []string `json:"tags"`
	Category    string   `json:"category"`
	Badges      []string `json:"badges"`
	Description string   `json:"description"`
	Thumbnail   string   `json:"thumbnail"`
	Quality     string   `json:"quality"`
	DurationSec int      `json:"durationSeconds"`
}

func (a *AdminServer) handleUpdateVideo(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body updateVideoReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	v, err := a.Catalog.GetVideo(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if body.Title != "" {
		v.Title = body.Title
	}
	if body.Author != "" {
		v.Author = body.Author
	}
	if body.Category != "" {
		v.Category = body.Category
	}
	if body.Badges != nil {
		v.Badges = body.Badges
	}
	if body.Description != "" {
		v.Description = body.Description
	}
	if body.Thumbnail != "" {
		v.ThumbnailURL = body.Thumbnail
	}
	if body.Quality != "" {
		v.Quality = body.Quality
	}
	if body.DurationSec > 0 {
		v.DurationSeconds = body.DurationSec
	}
	if err := a.Catalog.UpsertVideo(r.Context(), v); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if body.Tags != nil {
		if err := a.Catalog.SetManualVideoTags(r.Context(), id, body.Tags); err != nil {
			if errors.Is(err, catalog.ErrUnknownTag) {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		v, err = a.Catalog.GetVideo(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, v)
}

type setEnabledReq struct {
	Enabled bool `json:"enabled"`
}

func (a *AdminServer) handleSetVideoFrontendSelected(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body setEnabledReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.Catalog.SetVideoFrontendSelected(r.Context(), id, body.Enabled); err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			writeErr(w, http.StatusNotFound, err)
		case errors.Is(err, catalog.ErrVideoOutsideFrontendTags):
			writeErr(w, http.StatusBadRequest, err)
		default:
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	v, err := a.Catalog.GetVideo(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *AdminServer) handleSetTagFrontendAccess(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, errors.New("invalid tag id"))
		return
	}
	var body setEnabledReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.Catalog.SetTagFrontendAccess(r.Context(), id, body.Enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "frontendAccess": body.Enabled})
}

func (a *AdminServer) handleRegenPreview(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if a.OnRegenPreview != nil {
		a.OnRegenPreview(id)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (a *AdminServer) handleRegenAllPreviews(w http.ResponseWriter, r *http.Request) {
	if a.OnRegenAllPreviews != nil {
		a.OnRegenAllPreviews()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (a *AdminServer) handleRegenFailedPreviews(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if a.OnRegenFailedPreviews != nil {
		a.OnRegenFailedPreviews(id)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

// handleRegenFailedThumbnails 触发某 drive 下所有 thumbnail_status=failed 的封面
// 重新入队生成。和 handleRegenFailedPreviews 行为对称（一个管预览视频，一个管封面）。
//
// 立即返回 202；实际执行在后台 goroutine 跑，状态可在下次 GET /admin/api/drives
// 的 thumbnailFailedCount / thumbnailGenerationStatus 看变化。
func (a *AdminServer) handleRegenFailedThumbnails(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if a.OnRegenFailedThumbnails != nil {
		a.OnRegenFailedThumbnails(id)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

// handleRegenFailedFingerprints triggers regeneration for all failed sampled
// fingerprints on a drive. It mirrors the failed preview-video/thumbnail retry endpoints.
func (a *AdminServer) handleRegenFailedFingerprints(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if a.OnRegenFailedFingerprints != nil {
		a.OnRegenFailedFingerprints(id)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

// ---------- Settings ----------

// settingsDTO 是 GET/PUT /admin/api/settings 的入参/出参。
//
// 注意：早期的全局 previewEnabled 字段已经下沉为每盘 teaser_enabled，
// 不再出现在这里；前端要切换某个盘的预览视频生成请用 POST /admin/api/drives 上传
// teaserEnabled 字段。保留 settings 用作主题、spider91 上传目标这类全局配置。
type settingsDTO struct {
	Theme                 string `json:"theme"`
	Spider91UploadDriveID string `json:"spider91UploadDriveId"`
}

func (a *AdminServer) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	theme := "dark"
	if a.GetTheme != nil {
		if v := a.GetTheme(); v != "" {
			theme = v
		}
	}
	spider91UploadID := ""
	if a.GetSpider91UploadDriveID != nil {
		spider91UploadID = a.GetSpider91UploadDriveID()
	}
	writeJSON(w, http.StatusOK, settingsDTO{
		Theme:                 theme,
		Spider91UploadDriveID: spider91UploadID,
	})
}

func (a *AdminServer) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	// 用 map 区分"没传"和"传了空字符串"两种语义；空 spider91 上传 ID 表示
	// 本地保存不上传。
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	if v, ok := raw["theme"]; ok && a.SetTheme != nil {
		var theme string
		if err := json.Unmarshal(v, &theme); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if theme != "" {
			if err := a.SetTheme(theme); err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
		}
	}

	if v, ok := raw["spider91UploadDriveId"]; ok && a.SetSpider91UploadDriveID != nil {
		var driveID string
		if err := json.Unmarshal(v, &driveID); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if err := a.SetSpider91UploadDriveID(driveID); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}

	// 回显当前值
	resp := settingsDTO{}
	if a.GetTheme != nil {
		resp.Theme = a.GetTheme()
	}
	if a.GetSpider91UploadDriveID != nil {
		resp.Spider91UploadDriveID = a.GetSpider91UploadDriveID()
	}
	writeJSON(w, http.StatusOK, resp)
}
