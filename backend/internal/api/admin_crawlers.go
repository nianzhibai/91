package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/crawljob"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/drives/scriptcrawler"
	"github.com/nianzhibai/91/backend/internal/persistence"
	"github.com/nianzhibai/91/backend/internal/uploadjob"
)

type crawlerDTO struct {
	ID                          string               `json:"id"`
	Name                        string               `json:"name"`
	Protocol                    string               `json:"protocol"`
	Feeds                       []scriptcrawler.Feed `json:"feeds"`
	SelectedFeedID              string               `json:"selectedFeedId"`
	Kind                        string               `json:"kind"`
	Status                      string               `json:"status"`
	LastError                   string               `json:"lastError,omitempty"`
	ScriptPath                  string               `json:"scriptPath"`
	ScriptSourceURL             string               `json:"scriptSourceUrl,omitempty"`
	Proxy                       string               `json:"proxy,omitempty"`
	UploadProxy                 string               `json:"uploadProxy,omitempty"`
	TargetNew                   string               `json:"targetNew,omitempty"`
	UploadDriveID               string               `json:"uploadDriveId,omitempty"`
	Paused                      bool                 `json:"paused"`
	LastCrawlAt                 int64                `json:"lastCrawlAt,omitempty"`
	ScanGenerationStatus        GenerationStatus     `json:"scanGenerationStatus"`
	ThumbnailGenerationStatus   GenerationStatus     `json:"thumbnailGenerationStatus"`
	PreviewGenerationStatus     GenerationStatus     `json:"previewGenerationStatus"`
	FingerprintGenerationStatus GenerationStatus     `json:"fingerprintGenerationStatus"`
	UploadGenerationStatus      GenerationStatus     `json:"uploadGenerationStatus"`
	ThumbnailReadyCount         int                  `json:"thumbnailReadyCount"`
	ThumbnailPendingCount       int                  `json:"thumbnailPendingCount"`
	ThumbnailFailedCount        int                  `json:"thumbnailFailedCount"`
	TeaserReadyCount            int                  `json:"teaserReadyCount"`
	TeaserPendingCount          int                  `json:"teaserPendingCount"`
	TeaserFailedCount           int                  `json:"teaserFailedCount"`
	FingerprintReadyCount       int                  `json:"fingerprintReadyCount"`
	FingerprintPendingCount     int                  `json:"fingerprintPendingCount"`
	FingerprintFailedCount      int                  `json:"fingerprintFailedCount"`
	TotalCrawledCount           int                  `json:"totalCrawledCount"`
	LocalVideoCount             int                  `json:"localVideoCount"`
	MigratedVideoCount          int                  `json:"migratedVideoCount"`

	CurrentTask      *crawljob.Result  `json:"currentTask,omitempty"`
	LastCrawlResult  *crawljob.Result  `json:"lastCrawlResult,omitempty"`
	ScriptError      string            `json:"scriptError,omitempty"`
	LastUploadResult *uploadjob.Result `json:"lastUploadResult,omitempty"`
}

type upsertCrawlerReq struct {
	SelectedFeedID  *string `json:"selectedFeedId"`
	ID              string  `json:"id"`
	ScriptPath      string  `json:"scriptPath"`
	ScriptSourceURL string  `json:"scriptSourceUrl"`
	Proxy           string  `json:"proxy"`
	UploadProxy     *string `json:"uploadProxy"`
	TargetNew       string  `json:"targetNew"`
	UploadDriveID   string  `json:"uploadDriveId"`
}

type crawlerPausedReq struct {
	Paused bool `json:"paused"`
}

func (a *AdminServer) handleListCrawlers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	all, err := a.Catalog.ListDrives(r.Context())
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	generationStatuses := map[string]DriveGenerationStatuses{}
	if a.GetDriveGenerationStatuses != nil {
		generationStatuses = a.GetDriveGenerationStatuses()
	}

	uploadResults, err := a.Catalog.LatestCrawlerUploadResults(r.Context())
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	crawlResults, err := a.Catalog.LatestCrawlerTasks(r.Context())
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	out := make([]crawlerDTO, 0, len(all))
	for _, d := range all {
		if d == nil || !isConfiguredCrawlerDrive(d) {
			continue
		}
		assets, err := a.Catalog.CountCrawlerAssets(r.Context(), d.ID, crawlerVideoIDPrefixes(d))
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, err)
			return
		}
		item := a.crawlerDTOForDrive(d, assets, generationStatuses[d.ID])
		if result, ok := uploadResults[d.ID]; ok {
			item.LastUploadResult = &result
		}
		if result, ok := crawlResults[d.ID]; ok {
			if result.State == "queued" || result.State == "running" {
				item.CurrentTask = &result
				history, err := a.Catalog.ListCrawlerTasks(r.Context(), d.ID, 2)
				if err != nil {
					writeErr(w, r, http.StatusInternalServerError, err)
					return
				}
				for _, previous := range history {
					if previous.TaskID != result.TaskID {
						item.LastCrawlResult = &previous
						break
					}
				}
			} else {
				item.LastCrawlResult = &result
			}
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *AdminServer) crawlerDTOForDrive(d *catalog.Drive, assets catalog.CrawlerAssetCounts, generation DriveGenerationStatuses) crawlerDTO {
	if generation.Scan.State == "" {
		generation.Scan.State = "idle"
	}
	if generation.Thumbnail.State == "" {
		generation.Thumbnail.State = "idle"
	}
	if generation.Preview.State == "" {
		generation.Preview.State = "idle"
	}
	if generation.Fingerprint.State == "" {
		generation.Fingerprint.State = "idle"
	}
	if generation.Upload.State == "" {
		generation.Upload.State = "idle"
	}
	lastCrawlAt := int64(0)
	if raw := strings.TrimSpace(d.Credentials["last_crawl_at"]); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil {
			lastCrawlAt = v
		}
	}
	meta := scriptcrawler.Metadata{Name: strings.TrimSpace(d.Name), Feeds: []scriptcrawler.Feed{}}
	selectedFeedID := d.Credentials["feed_id"]
	scriptPath, scriptErr := a.crawlerScriptPath(d.Credentials)
	if scriptErr == nil {
		var parsed scriptcrawler.Metadata
		parsed, scriptErr = scriptcrawler.ReadMetadata(scriptPath)
		if scriptErr == nil {
			meta = parsed
			var feed scriptcrawler.Feed
			feed, scriptErr = meta.ResolveFeed(selectedFeedID)
			if scriptErr == nil {
				selectedFeedID = feed.ID
			}
		}
	}
	scriptError := ""
	if scriptErr != nil {
		scriptError = scriptErr.Error()
	}
	return crawlerDTO{
		ID:                          d.ID,
		Name:                        meta.Name,
		Protocol:                    meta.Protocol,
		Feeds:                       meta.Feeds,
		SelectedFeedID:              selectedFeedID,
		ScriptError:                 scriptError,
		Kind:                        d.Kind,
		Status:                      d.Status,
		LastError:                   d.LastError,
		ScriptPath:                  scriptPath,
		ScriptSourceURL:             strings.TrimSpace(d.Credentials["script_source_url"]),
		Proxy:                       strings.TrimSpace(d.Credentials["proxy"]),
		UploadProxy:                 strings.TrimSpace(d.Credentials["upload_proxy"]),
		TargetNew:                   strings.TrimSpace(d.Credentials["target_new"]),
		UploadDriveID:               strings.TrimSpace(d.Credentials["upload_drive_id"]),
		Paused:                      crawlerPaused(d),
		LastCrawlAt:                 lastCrawlAt,
		ScanGenerationStatus:        generation.Scan,
		ThumbnailGenerationStatus:   generation.Thumbnail,
		PreviewGenerationStatus:     generation.Preview,
		FingerprintGenerationStatus: generation.Fingerprint,
		UploadGenerationStatus:      generation.Upload,
		ThumbnailReadyCount:         assets.Thumbnail.Ready,
		ThumbnailPendingCount:       assets.Thumbnail.Pending,
		ThumbnailFailedCount:        assets.Thumbnail.Failed,
		TeaserReadyCount:            assets.Teaser.Ready,
		TeaserPendingCount:          assets.Teaser.Pending,
		TeaserFailedCount:           assets.Teaser.Failed,
		FingerprintReadyCount:       assets.Fingerprint.Ready,
		FingerprintPendingCount:     assets.Fingerprint.Pending,
		FingerprintFailedCount:      assets.Fingerprint.Failed,
		TotalCrawledCount:           assets.Total,
		LocalVideoCount:             assets.Local,
		MigratedVideoCount:          assets.Migrated,
	}
}

func crawlerPaused(d *catalog.Drive) bool {
	if d == nil || d.Credentials == nil {
		return false
	}
	raw := strings.TrimSpace(d.Credentials["paused"])
	if raw == "" {
		return false
	}
	v, err := strconv.ParseBool(raw)
	return err == nil && v
}

func crawlerVideoIDPrefixes(d *catalog.Drive) []string {
	if d == nil {
		return nil
	}
	return []string{
		scriptcrawler.Kind + "-" + d.ID + "-",
	}
}

func (a *AdminServer) handleUpsertCrawler(w http.ResponseWriter, r *http.Request) {
	var body upsertCrawlerReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	id := strings.TrimSpace(body.ID)
	creds := map[string]string{}
	var existing *catalog.Drive
	var configLease DriveConfigUpdateLease
	if id != "" {
		var ok bool
		configLease, ok = a.beginDriveConfigUpdate(w, id)
		if !ok {
			return
		}
		if configLease != nil {
			defer configLease.Release()
		}
		existingDrive, err := a.Catalog.GetDrive(r.Context(), id)
		switch {
		case err == nil:
			existing = existingDrive
		case errors.Is(err, sql.ErrNoRows):
			// Explicit ID for a new crawler.
		default:
			writeErr(w, r, http.StatusInternalServerError, err)
			return
		}
	}
	if existing != nil {
		for k, v := range existing.Credentials {
			creds[k] = v
		}
	}
	scriptPath := strings.TrimSpace(body.ScriptPath)
	incoming := map[string]string{
		"script_path":       scriptPath,
		"script_source_url": strings.TrimSpace(body.ScriptSourceURL),
		"proxy":             strings.TrimSpace(body.Proxy),
		"target_new":        strings.TrimSpace(body.TargetNew),
		"upload_drive_id":   strings.TrimSpace(body.UploadDriveID),
	}
	// Keep the new field backward-compatible with older admin clients: omitted
	// means preserve, while an explicit empty string clears the upload proxy.
	if body.UploadProxy != nil {
		incoming["upload_proxy"] = strings.TrimSpace(*body.UploadProxy)
	}
	if body.SelectedFeedID != nil {
		if strings.TrimSpace(*body.SelectedFeedID) == "" {
			http.Error(w, "请选择抓取栏目", http.StatusBadRequest)
			return
		}
		incoming["feed_id"] = *body.SelectedFeedID
	}
	for k, v := range incoming {
		creds[k] = v
	}
	if err := a.validateCrawlerUploadDrive(r.Context(), creds["upload_drive_id"]); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	merged, err := mergeScriptCrawlerCredentials(existing, creds)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.normalizeCrawlerScriptReference(merged); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Existing crawler saves patch only user-owned configuration keys. Runtime
	// state such as last_crawl_at may be updated by the still-running old task
	// after this request loaded its snapshot and must never be replaced wholesale.
	persistedCredentials := merged
	if existing != nil {
		persistedCredentials = map[string]string{
			"script_path":       merged["script_path"],
			"script_file":       merged["script_file"],
			"script_source_url": merged["script_source_url"],
			"proxy":             merged["proxy"],
			"target_new":        merged["target_new"],
			"upload_drive_id":   merged["upload_drive_id"],
		}
		if body.UploadProxy != nil {
			persistedCredentials["upload_proxy"] = merged["upload_proxy"]
		}
	}
	resolvedScript, err := a.crawlerScriptPath(merged)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	meta, err := scriptcrawler.ReadMetadata(resolvedScript)
	if err != nil {
		http.Error(w, "脚本元信息无效："+err.Error(), http.StatusBadRequest)
		return
	}
	feed, err := meta.ResolveFeed(merged["feed_id"])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	persistedCredentials["feed_id"] = feed.ID
	name := meta.Name
	if id == "" {
		generatedID, err := a.generateCrawlerID(r.Context(), name)
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, err)
			return
		}
		id = generatedID
		var ok bool
		configLease, ok = a.beginDriveConfigUpdate(w, id)
		if !ok {
			return
		}
		if configLease != nil {
			defer configLease.Release()
		}
	}
	updateScope := DriveConfigUpdateRuntime
	if !authorizeDriveConfigUpdate(w, configLease, updateScope) {
		return
	}
	d := &catalog.Drive{
		ID:          id,
		Kind:        scriptcrawler.Kind,
		Name:        name,
		RootID:      "/",
		Credentials: persistedCredentials,
		Status:      "disconnected",
	}
	if err := a.Catalog.UpsertDriveWithOptions(r.Context(), d, catalog.DriveUpsertOptions{
		PatchCredentials: existing != nil,
	}); err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	// Confirmation commits the selected script revision and settings together.
	// Rebuild the runtime snapshot even when an external script kept its path.
	deferred, runtimeErr := commitDriveConfigUpdate(configLease, DriveConfigUpdateRuntime, func() error {
		if a.OnDriveRuntimeConfigChanged == nil {
			return nil
		}
		return a.OnDriveRuntimeConfigChanged(id)
	})
	resp := map[string]any{"ok": true, "id": id}
	if deferred {
		resp["deferred"] = true
		resp["message"] = driveConfigDeferredMessage
	}
	if runtimeErr != nil {
		resp["warning"] = runtimeErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *AdminServer) generateCrawlerID(ctx context.Context, name string) (string, error) {
	all, err := a.Catalog.ListDrives(ctx)
	if err != nil {
		return "", err
	}
	used := map[string]bool{}
	for _, d := range all {
		if d == nil {
			continue
		}
		if isCrawlerDriveKind(d.Kind) && !scriptcrawler.IsConfigured(d.Credentials) {
			continue
		}
		used[d.ID] = true
	}
	slug := crawlerIDSlug(name)
	base := "crawler"
	if slug != "" {
		base += "-" + slug
	}
	candidate := base
	for suffix := 2; used[candidate]; suffix++ {
		candidate = fmt.Sprintf("%s-%d", base, suffix)
	}
	return candidate, nil
}

func (a *AdminServer) validateCrawlerUploadDrive(ctx context.Context, driveID string) error {
	driveID = strings.TrimSpace(driveID)
	if driveID == "" {
		return nil
	}
	if a == nil || a.Catalog == nil {
		return errors.New("crawler upload target validation unavailable")
	}
	d, err := a.Catalog.GetDrive(ctx, driveID)
	if err != nil || d == nil {
		return fmt.Errorf("上传目标网盘 %q 不存在", driveID)
	}
	if !drives.CapabilitiesForKind(d.Kind).Upload {
		return fmt.Errorf("上传目标网盘 %q 类型为 %s，不支持上传", driveID, d.Kind)
	}
	return nil
}

func isCrawlerUploadTargetKind(kind string) bool {
	return drives.CapabilitiesForKind(kind).Upload
}

func crawlerIDSlug(raw string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(raw) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if b.Len() > 0 && !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

type importCrawlerScriptURLReq struct {
	URL      string `json:"url"`
	FileName string `json:"fileName"`
}

type testCrawlerScriptReq struct {
	ScriptPath     string `json:"scriptPath"`
	Proxy          string `json:"proxy"`
	SelectedFeedID string `json:"selectedFeedId"`
}

// handleTestCrawlerScript 试跑一个爬虫脚本：不入库，抓到第一条视频
// （并探测直链可达）即返回，让用户在保存前确认脚本能爬到视频。
func (a *AdminServer) handleTestCrawlerScript(w http.ResponseWriter, r *http.Request) {
	var body testCrawlerScriptReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	scriptPath := strings.TrimSpace(body.ScriptPath)
	if scriptPath == "" {
		http.Error(w, "请先导入爬虫脚本", http.StatusBadRequest)
		return
	}
	proxyURL, err := normalizeCrawlerProxyURL(body.Proxy, "脚本爬虫")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	result := scriptcrawler.DryRun(r.Context(), scriptcrawler.DryRunConfig{
		ScriptPath: scriptPath,
		ProxyURL:   proxyURL,
		FeedID:     body.SelectedFeedID,
	})
	writeJSON(w, http.StatusOK, result)
}

func (a *AdminServer) handleImportCrawlerScriptFile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCrawlerScriptBytes+1024*1024)
	if err := r.ParseMultipartForm(maxCrawlerScriptBytes + 1024*1024); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, errors.New("file is required"))
		return
	}
	defer file.Close()

	name := "crawler.py"
	if header != nil && strings.TrimSpace(header.Filename) != "" {
		name = header.Filename
	}
	if _, err := safeCrawlerScriptFileName(name); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	// Validate before publishing an independent revision for the editor.
	data, meta, err := readCrawlerScript(file, maxCrawlerScriptBytes)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	scriptPath, err := a.saveCrawlerScript(r.Context(), name, bytes.NewReader(data), maxCrawlerScriptBytes)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scriptPath": scriptPath, "name": meta.Name, "protocol": meta.Protocol, "feeds": meta.Feeds})
}

func (a *AdminServer) handleImportCrawlerScriptURL(w http.ResponseWriter, r *http.Request) {
	var body importCrawlerScriptURLReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	rawURL := strings.TrimSpace(body.URL)
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		writeErr(w, r, http.StatusBadRequest, errors.New("脚本链接格式无效"))
		return
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		writeErr(w, r, http.StatusBadRequest, errors.New("脚本链接仅支持 http:// 或 https://"))
		return
	}
	downloadURL := crawlerScriptDownloadURL(u)

	client := a.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, downloadURL.String(), nil)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	req.Header.Set("User-Agent", "91-crawler-import/1.0")
	resp, err := client.Do(req)
	if err != nil {
		writeErr(w, r, http.StatusBadGateway, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeErr(w, r, http.StatusBadGateway, fmt.Errorf("下载脚本失败: HTTP %d", resp.StatusCode))
		return
	}
	if resp.ContentLength > maxCrawlerScriptBytes {
		writeErr(w, r, http.StatusBadRequest, fmt.Errorf("脚本文件不能超过 %d KiB", maxCrawlerScriptBytes/1024))
		return
	}

	name := strings.TrimSpace(body.FileName)
	if name == "" {
		name = path.Base(downloadURL.Path)
	}
	if name == "." || name == "/" || name == "" {
		name = "crawler.py"
	}
	if _, err := safeCrawlerScriptFileName(name); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	// Link updates also create a revision; they never mutate the active file.
	data, meta, err := readCrawlerScript(resp.Body, maxCrawlerScriptBytes)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	scriptPath, err := a.saveCrawlerScript(r.Context(), name, bytes.NewReader(data), maxCrawlerScriptBytes)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scriptPath": scriptPath, "name": meta.Name, "protocol": meta.Protocol, "feeds": meta.Feeds, "sourceUrl": downloadURL.String()})
}

func crawlerScriptDownloadURL(u *url.URL) *url.URL {
	if raw, ok := githubRawCrawlerScriptURL(u); ok {
		return raw
	}
	return u
}

func githubRawCrawlerScriptURL(u *url.URL) (*url.URL, bool) {
	host := strings.ToLower(u.Hostname())
	if host != "github.com" && host != "www.github.com" {
		return nil, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 5 {
		return nil, false
	}
	if parts[0] == "" || parts[1] == "" || parts[3] == "" || (parts[2] != "blob" && parts[2] != "raw") {
		return nil, false
	}
	rawParts := append([]string{parts[0], parts[1], parts[3]}, parts[4:]...)
	return &url.URL{
		Scheme: "https",
		Host:   "raw.githubusercontent.com",
		Path:   "/" + strings.Join(rawParts, "/"),
	}, true
}

// readCrawlerScript 把脚本内容读入内存并校验大小和元信息，返回内容和元信息。
func readCrawlerScript(r io.Reader, maxBytes int64) ([]byte, scriptcrawler.Metadata, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, scriptcrawler.Metadata{}, err
	}
	if len(data) == 0 {
		return nil, scriptcrawler.Metadata{}, errors.New("脚本文件为空")
	}
	if int64(len(data)) > maxBytes {
		return nil, scriptcrawler.Metadata{}, fmt.Errorf("脚本文件不能超过 %d KiB", maxBytes/1024)
	}
	meta, err := scriptcrawler.ExtractMetadata(string(data))
	if err != nil {
		return nil, scriptcrawler.Metadata{}, fmt.Errorf("脚本元信息无效: %w", err)
	}
	return data, meta, nil
}

func (a *AdminServer) saveCrawlerScript(ctx context.Context, name string, r io.Reader, maxBytes int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fileName, err := safeCrawlerScriptFileName(name)
	if err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", errors.New("脚本文件为空")
	}
	if int64(len(data)) > maxBytes {
		return "", fmt.Errorf("脚本文件不能超过 %d KiB", maxBytes/1024)
	}
	root, err := a.crawlerScriptImportDir()
	if err != nil {
		return "", err
	}
	if err := persistence.RLockContext(ctx); err != nil {
		return "", err
	}
	defer persistence.RUnlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	// A filename is a display/import detail, not a mutable storage identity.
	// Only handleUpsertCrawler changes which revision a crawler uses. Imports
	// remain isolated across canceled edits, failed saves and other crawlers.
	revisionDir, err := os.MkdirTemp(root, "import-")
	if err != nil {
		return "", err
	}
	dst := filepath.Join(revisionDir, fileName)
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		_ = os.RemoveAll(revisionDir)
		return "", err
	}
	return dst, nil
}

func (a *AdminServer) crawlerScriptImportDir() (string, error) {
	base := strings.TrimSpace(a.LocalPreviewDir)
	if base == "" {
		base = filepath.Join(".", "data", "previews")
	}
	root := filepath.Join(filepath.Dir(base), "crawler-scripts")
	return filepath.Abs(root)
}

func (a *AdminServer) crawlerScriptPath(credentials map[string]string) (string, error) {
	root, err := a.crawlerScriptImportDir()
	if err != nil {
		return "", err
	}
	return scriptcrawler.ScriptPath(credentials, root)
}

func (a *AdminServer) normalizeCrawlerScriptReference(credentials map[string]string) error {
	root, err := a.crawlerScriptImportDir()
	if err != nil {
		return err
	}
	path, err := scriptcrawler.ScriptPath(credentials, root)
	if err != nil {
		return err
	}
	return scriptcrawler.SetScriptPath(credentials, root, path)
}

func safeCrawlerScriptFileName(raw string) (string, error) {
	name := strings.TrimSpace(filepath.Base(raw))
	if name == "" || name == "." || name == string(os.PathSeparator) {
		name = "crawler.py"
	}
	ext := strings.ToLower(filepath.Ext(name))
	if ext != ".py" {
		return "", errors.New("目前只支持导入 .py 爬虫脚本")
	}
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	var b strings.Builder
	for _, r := range stem {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	cleanStem := strings.Trim(b.String(), "._-")
	if cleanStem == "" {
		cleanStem = "crawler"
	}
	return cleanStem + ".py", nil
}

func (a *AdminServer) handleRunCrawler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	d, err := a.Catalog.GetDrive(r.Context(), id)
	if err != nil || d == nil || !isCrawlerDriveKind(d.Kind) || !scriptcrawler.IsConfigured(d.Credentials) {
		http.Error(w, "crawler not found", http.StatusNotFound)
		return
	}
	status := a.nightlyJobStatus()
	if status.Running || status.Queued {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"ok":       true,
			"accepted": false,
			"message":  fullScanBusyMessage,
			"status":   status,
		})
		return
	}
	if a.OnCrawlerRunRequested == nil {
		writeErr(w, r, http.StatusServiceUnavailable, errors.New("爬虫执行器未配置"))
		return
	}
	taskID, err := a.OnCrawlerRunRequested(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "accepted": false, "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "accepted": true, "taskId": taskID})
}

func (a *AdminServer) handleSetCrawlerPaused(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	d, err := a.Catalog.GetDrive(r.Context(), id)
	if err != nil || d == nil || !isConfiguredCrawlerDrive(d) {
		http.Error(w, "crawler not found", http.StatusNotFound)
		return
	}
	var body crawlerPausedReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	configLease, ok := a.beginDriveConfigUpdate(w, id)
	if !ok {
		return
	}
	if configLease != nil {
		defer configLease.Release()
	}
	if !authorizeDriveConfigUpdate(w, configLease, 0) {
		return
	}
	if err := a.Catalog.PatchDriveCredentials(r.Context(), id, map[string]string{
		"paused": strconv.FormatBool(body.Paused),
	}); err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "paused": body.Paused})
}

func (a *AdminServer) handleUploadCrawlerVideos(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	d, err := a.Catalog.GetDrive(r.Context(), id)
	if err != nil || d == nil || !isConfiguredCrawlerDrive(d) {
		http.Error(w, "crawler not found", http.StatusNotFound)
		return
	}
	reject := func(reason string) {
		applog.Warn(r.Context(), "手动上传未启动: "+reason, nil, applog.Fields{Component: "crawlerupload", DriveID: id, Stage: "admission"})
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "accepted": false, "message": reason})
	}
	status := a.nightlyJobStatus()
	if status.Running || status.Queued {
		reject(fullScanBusyMessage)
		return
	}
	if strings.TrimSpace(d.Credentials["upload_drive_id"]) == "" {
		reject("请先配置上传网盘")
		return
	}
	if a.GetDriveGenerationStatuses != nil && driveGenerationBusy(a.GetDriveGenerationStatuses()[d.ID]) {
		reject("当前爬虫有正在进行的任务，请稍后重试")
		return
	}
	// Per-video eligibility belongs to the worker. A failed historical asset
	// or one blocked local video must not reject the entire sweep.
	if a.OnCrawlerUploadRequested == nil {
		reject("上传服务未初始化")
		return
	}
	accepted, message := a.OnCrawlerUploadRequested(id)
	if !accepted {
		if strings.TrimSpace(message) == "" {
			message = driveTaskBusyMessage
		}
		reject(message)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "accepted": true})
}

func driveGenerationBusy(g DriveGenerationStatuses) bool {
	return generationBusy(g.Scan) ||
		generationBusy(g.Thumbnail) ||
		generationBusy(g.Preview) ||
		generationBusy(g.Fingerprint) ||
		generationBusy(g.Upload)
}

func generationBusy(g GenerationStatus) bool {
	switch strings.TrimSpace(g.State) {
	case "", "idle":
		return false
	default:
		return true
	}
}

func (a *AdminServer) handleStopCrawlerTasks(w http.ResponseWriter, r *http.Request) {
	a.handleStopDriveTasks(w, r)
}

func (a *AdminServer) handleDeleteCrawler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	configLease, ok := a.beginDriveConfigUpdate(w, id)
	if !ok {
		return
	}
	if configLease != nil {
		defer configLease.Release()
	}
	d, err := a.Catalog.GetDrive(r.Context(), id)
	if err != nil {
		writeErr(w, r, http.StatusNotFound, err)
		return
	}
	if !isCrawlerDriveKind(d.Kind) {
		http.Error(w, "crawler not found", http.StatusNotFound)
		return
	}
	if !authorizeDriveConfigUpdate(w, configLease, DriveConfigUpdateDestructive) {
		return
	}
	if err := a.prepareDriveDelete(r.Context(), id); err != nil {
		writeErr(w, r, http.StatusInternalServerError, fmt.Errorf("stop crawler tasks before delete: %w", err))
		return
	}
	if a.OnDriveDeleteCleanup == nil {
		http.Error(w, "crawler video cleanup is not available", http.StatusInternalServerError)
		return
	}
	deletedVideos, err := a.OnDriveDeleteCleanup(r.Context(), id)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}

	persistence.RLock()
	deletedScript, scriptErr := a.removeImportedCrawlerScript(d)
	deleteErr := a.Catalog.DeleteDrive(r.Context(), id)
	persistence.RUnlock()
	if deleteErr != nil {
		writeErr(w, r, http.StatusInternalServerError, deleteErr)
		return
	}
	if a.OnDriveRemoved != nil {
		a.OnDriveRemoved(id)
	}
	if err := completeDriveDelete(configLease); err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	resp := map[string]any{
		"ok":            true,
		"deletedVideos": deletedVideos,
		"deletedScript": deletedScript,
	}
	if scriptErr != nil {
		resp["warning"] = scriptErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *AdminServer) handleListCrawlerTasks(w http.ResponseWriter, r *http.Request) {
	results, err := a.Catalog.ListCrawlerTasks(r.Context(), chi.URLParam(r, "id"), 100)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, results)
}
func (a *AdminServer) handleGetCrawlerTask(w http.ResponseWriter, r *http.Request) {
	result, err := a.Catalog.GetCrawlerTask(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "taskID"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	uploads, err := a.Catalog.CrawlerTaskUploads(r.Context(), result.TaskID)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": result, "uploads": uploads})
}
func (a *AdminServer) handleCancelCrawlerTask(w http.ResponseWriter, r *http.Request) {
	result, err := a.Catalog.GetCrawlerTask(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "taskID"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	if result.State != "queued" && result.State != "running" {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "message": "任务已结束"})
		return
	}
	if a.OnCrawlerTaskCancel == nil {
		writeErr(w, r, http.StatusServiceUnavailable, errors.New("任务取消不可用"))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": a.OnCrawlerTaskCancel(result.DriveID, result.TaskID), "taskId": result.TaskID})
}
