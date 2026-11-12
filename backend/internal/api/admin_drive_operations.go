package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/driveevents"
	"github.com/nianzhibai/91/backend/internal/drives/googledrive"
	"github.com/nianzhibai/91/backend/internal/drives/scriptcrawler"
	"github.com/nianzhibai/91/backend/internal/driveview"
	"github.com/nianzhibai/91/backend/internal/httpapi"
	"github.com/nianzhibai/91/backend/internal/scopedproxy"
)

func isCrawlerDriveKind(kind string) bool {
	return kind == scriptcrawler.Kind
}

func isSupportedDriveKind(kind string) bool {
	switch kind {
	case "quark", "p115", "p123", "pikpak", "wopan", "yun139", "guangyapan", "onedrive", "googledrive", "webdav", "localstorage", scriptcrawler.Kind:
		return true
	default:
		return false
	}
}

func isConfiguredCrawlerDrive(d *catalog.Drive) bool {
	return d != nil &&
		isCrawlerDriveKind(d.Kind) &&
		scriptcrawler.IsConfigured(d.Credentials)
}

func (a *AdminServer) removeImportedCrawlerScript(d *catalog.Drive) (bool, error) {
	if d == nil || d.Credentials == nil {
		return false, nil
	}
	if !scriptcrawler.IsConfigured(d.Credentials) {
		return false, nil
	}
	scriptAbs, err := a.crawlerScriptPath(d.Credentials)
	if err != nil {
		return false, err
	}
	rootAbs, err := a.crawlerScriptImportDir()
	if err != nil {
		return false, err
	}
	if scriptAbs == rootAbs || !strings.HasPrefix(scriptAbs, rootAbs+string(os.PathSeparator)) {
		return false, nil
	}
	if err := os.Remove(scriptAbs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func googleDriveCredentialPatch(incoming map[string]string) (map[string]string, error) {
	cleaned := nonEmptyCredentials(incoming)
	// The optional web login can be removed without touching OAuth credentials.
	if cookie, ok := incoming["playback_cookie"]; ok {
		cleaned["playback_cookie"] = strings.TrimSpace(cookie)
	}
	if value, ok := incoming["playback_auth_user"]; ok {
		authUser, err := googledrive.NormalizePlaybackAuthUser(value)
		if err != nil {
			return nil, err
		}
		cleaned["playback_auth_user"] = authUser
	}
	delete(cleaned, "use_online_api")
	delete(cleaned, "api_url_address")
	return cleaned, nil
}

// Preserve explicit empty custom-client values in an edit patch so switching
// an existing OneDrive mount back to OpenList API also clears the old secret.
func oneDriveCredentialPatch(incoming map[string]string) map[string]string {
	cleaned := nonEmptyCredentials(incoming)
	for _, key := range []string{"client_id", "client_secret"} {
		if value, ok := incoming[key]; ok {
			cleaned[key] = strings.TrimSpace(value)
		}
	}
	return cleaned
}

// mergeNonEmptyCredentials 逐键合并凭证：incoming 里非空的键覆盖旧值，
// 空值/缺失的键沿用旧值。quark、googledrive、webdav、localstorage 和 guangyapan 的编辑表单都依赖
// 这个语义（留空 = 不修改）。
func mergeNonEmptyCredentials(existing *catalog.Drive, incoming map[string]string) map[string]string {
	merged := map[string]string{}
	if existing != nil {
		for k, v := range existing.Credentials {
			merged[k] = v
		}
	}
	for k, v := range nonEmptyCredentials(incoming) {
		merged[k] = v
	}
	return merged
}

func nonEmptyCredentials(incoming map[string]string) map[string]string {
	cleaned := map[string]string{}
	for k, v := range incoming {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		value := strings.TrimSpace(v)
		if value == "" {
			continue
		}
		cleaned[key] = value
	}
	return cleaned
}

func mergeScriptCrawlerCredentials(existing *catalog.Drive, incoming map[string]string) (map[string]string, error) {
	merged := map[string]string{}
	if existing != nil {
		for k, v := range existing.Credentials {
			merged[k] = v
		}
	}
	for k, v := range incoming {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		value := strings.TrimSpace(v)
		switch key {
		case "proxy", "upload_proxy":
			label := "脚本爬虫"
			if key == "upload_proxy" {
				label = "脚本爬虫上传"
			}
			proxy, err := normalizeCrawlerProxyURL(value, label)
			if err != nil {
				return nil, err
			}
			if proxy == "" {
				delete(merged, key)
			} else {
				merged[key] = proxy
			}
		case "target_new":
			if value == "" {
				delete(merged, key)
				continue
			}
			n, err := strconv.Atoi(value)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("脚本爬虫 target_new 必须是正整数")
			}
			merged[key] = strconv.Itoa(n)
		case "script_path":
			if value == "" {
				if existing == nil {
					delete(merged, key)
				}
				continue
			}
			merged[key] = value
		case "builtin", "python_path", "config_json":
			delete(merged, key)
		default:
			if value == "" {
				delete(merged, key)
			} else {
				merged[key] = value
			}
		}
	}
	if strings.TrimSpace(incoming["script_path"]) != "" {
		delete(merged, "script_file")
	}
	if !scriptcrawler.IsConfigured(merged) {
		return nil, fmt.Errorf("脚本爬虫必须填写 script_path")
	}
	delete(merged, "builtin")
	delete(merged, "python_path")
	delete(merged, "config_json")
	return merged, nil
}

func normalizeCrawlerProxyURL(raw, label string) (string, error) {
	proxy, err := scopedproxy.Normalize(raw)
	if err == nil {
		return proxy, nil
	}
	if errors.Is(err, scopedproxy.ErrUnsupportedScheme) {
		return "", fmt.Errorf("%s 代理地址仅支持 http://、https://、socks5:// 或 socks5h://", label)
	}
	return "", fmt.Errorf("%s 代理地址格式无效，请填写类似 http://127.0.0.1:7890 的地址", label)
}

// handleGetDriveCredentials returns the stored values only when an authenticated
// administrator opens the edit form. Keeping this separate from handleListDrives
// avoids sending secrets with the page's periodic drive-list polling.
func (a *AdminServer) handleGetDriveCredentials(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		writeErr(w, r, http.StatusBadRequest, errors.New("invalid drive id"))
		return
	}
	drive, err := a.Catalog.GetDrive(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, r, http.StatusNotFound, err)
			return
		}
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	credentials := make(map[string]string, len(drive.Credentials))
	for key, value := range drive.Credentials {
		credentials[key] = value
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"credentials": credentials})
}

func (a *AdminServer) handleDeleteDrive(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body deleteDriveReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	if !body.DeleteVideos {
		httpapi.Error(w, "deleteVideos=true is required when deleting a drive", http.StatusBadRequest)
		return
	}

	deletedVideos := 0
	if a.OnDriveDeleteCleanup == nil {
		httpapi.Error(w, "drive video cleanup is not available", http.StatusInternalServerError)
		return
	}
	configLease, ok := a.beginDriveConfigUpdate(w, id)
	if !ok {
		return
	}
	if configLease != nil {
		defer configLease.Release()
	}
	if !authorizeDriveConfigUpdate(w, configLease, DriveConfigUpdateDestructive) {
		return
	}
	if err := a.prepareDriveDelete(r.Context(), id); err != nil {
		writeErr(w, r, http.StatusInternalServerError, fmt.Errorf("stop drive tasks before delete: %w", err))
		return
	}
	removed, err := a.OnDriveDeleteCleanup(r.Context(), id)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	deletedVideos = removed

	if err := a.Catalog.DeleteDrive(r.Context(), id); err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	if a.OnDriveRemoved != nil {
		a.OnDriveRemoved(id)
	}
	if err := completeDriveDelete(configLease); err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deletedVideos": deletedVideos})
}

func (a *AdminServer) prepareDriveDelete(ctx context.Context, driveID string) error {
	if a.OnPrepareDriveDelete != nil {
		return a.OnPrepareDriveDelete(ctx, driveID)
	}
	// Keep lightweight AdminServer users compatible while production always
	// supplies the stop-and-drain hook.
	if a.OnStopDriveTasks != nil {
		a.OnStopDriveTasks(driveID)
	}
	return nil
}

func completeDriveDelete(lease DriveConfigUpdateLease) error {
	deferred, err := commitDriveConfigUpdate(lease, DriveConfigUpdateDestructive, nil)
	if err != nil {
		return fmt.Errorf("complete drive deletion: %w", err)
	}
	if deferred {
		return errors.New("complete drive deletion: destructive operation was unexpectedly deferred")
	}
	return nil
}

type deleteDriveReq struct {
	DeleteVideos bool `json:"deleteVideos"`
}

func (a *AdminServer) handleRescan(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
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

	accepted := true
	if a.OnScanRequested != nil {
		accepted = a.OnScanRequested(r.Context(), id)
	}
	resp := map[string]any{"ok": true, "accepted": accepted}
	if a.Catalog != nil {
		a.Catalog.DriveEvents().Notify(id, true, driveevents.ActivityChanged)
	}
	a.addDriveSnapshot(r.Context(), resp, id, driveview.Runtime)
	if !accepted {
		resp["message"] = driveTaskBusyMessage
	}
	writeJSON(w, http.StatusAccepted, resp)
}

func (a *AdminServer) handleStopDriveTasks(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	stopped := false
	if a.OnStopDriveTasks != nil {
		stopped = a.OnStopDriveTasks(id)
	}
	resp := map[string]any{"ok": true, "stopped": stopped}
	if a.Catalog != nil {
		a.Catalog.DriveEvents().Notify(id, true, driveevents.ActivityChanged)
	}
	a.addDriveSnapshot(r.Context(), resp, id, driveview.Runtime)
	writeJSON(w, http.StatusAccepted, resp)
}

// handleRunScanAllJob 触发手动全量扫盘：真实云盘扫描、新视频资产处理和视频去重。
// 它不会运行脚本爬虫、爬虫上传或恢复阶段。任务已在跑或排队时拒绝重复触发。
func (a *AdminServer) handleRunScanAllJob(w http.ResponseWriter, r *http.Request) {
	accepted := false
	if a.OnRunScanAllJob != nil {
		accepted = a.OnRunScanAllJob()
	}
	resp := map[string]any{
		"ok":       true,
		"accepted": accepted,
		"status":   a.nightlyJobStatus(),
	}
	if !accepted {
		resp["message"] = fullScanBusyMessage
	}
	writeJSON(w, http.StatusAccepted, resp)
}

// handleRunNightlyJob 保留旧接口路径的兼容性；其行为与扫描所有网盘一致。
func (a *AdminServer) handleScanAllJobStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.nightlyJobStatus())
}

func (a *AdminServer) handleStopAllTasks(w http.ResponseWriter, r *http.Request) {
	stoppedDrives := 0
	if a.OnStopAllTasks != nil {
		stoppedDrives = a.OnStopAllTasks()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok":            true,
		"stoppedDrives": stoppedDrives,
		"status":        a.nightlyJobStatus(),
	})
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
//   - 立即保存 catalog.drives.skip_dir_ids（整体覆盖）
//   - 当前网盘有任务时，旧任务继续使用原快照，结束后再切换
//   - 不排清理任务、不重新触发扫描；下一次扫描开始时使用新列表执行策略清理
//   - 下一次扫描前再次保存可取消刚设置的跳过目录，期间不会删除媒体库记录
//   - 返回保存后的列表，方便前端乐观更新但又能以服务端为准
func (a *AdminServer) handleSetDriveSkipDirs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		httpapi.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	var body skipDirsReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	// 去重 + trim 空白；前端理论上保证清洁，这里再防一道。
	seen := map[string]struct{}{}
	cleaned := make([]string, 0, len(body.DirIDs))
	for _, raw := range body.DirIDs {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		cleaned = append(cleaned, s)
	}
	configLease, ok := a.beginDriveConfigUpdate(w, id)
	if !ok {
		return
	}
	if configLease != nil {
		defer configLease.Release()
	}
	if !authorizeDriveConfigUpdate(w, configLease, DriveConfigUpdateScan) {
		return
	}
	if err := a.Catalog.SetDriveSkipDirIDs(r.Context(), id, cleaned); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpapi.Error(w, "drive not found", http.StatusNotFound)
			return
		}
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	deferred, applyErr := commitDriveConfigUpdate(configLease, DriveConfigUpdateScan, nil)
	if applyErr != nil {
		writeErr(w, r, http.StatusInternalServerError, applyErr)
		return
	}
	resp := map[string]any{"ok": true, "skipDirIds": cleaned}
	a.addDriveSnapshot(r.Context(), resp, id, driveview.Config)
	if deferred {
		resp["deferred"] = true
		resp["message"] = driveConfigDeferredMessage
	}
	writeJSON(w, http.StatusOK, resp)
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
		httpapi.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	if a.ListDriveDirChildren == nil {
		writeErr(w, r, http.StatusInternalServerError, errors.New("dirtree not configured"))
		return
	}
	parent := r.URL.Query().Get("parent")
	entries, err := a.ListDriveDirChildren(r.Context(), id, parent)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	if entries == nil {
		entries = []DriveDirEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}
