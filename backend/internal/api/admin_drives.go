package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/video-site/backend/internal/catalog"
	drivepkg "github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/drives/scriptcrawler"
)

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
	transcodeCounts, err := a.Catalog.CountTranscodesByDrive(r.Context())
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
		CanUpload     bool   `json:"canUpload"`
		// TeaserEnabled 控制是否给本盘生成预览视频；封面生成不受影响。
		// 前端用它在网盘列表/编辑表单展示开关状态。
		TeaserEnabled bool `json:"teaserEnabled"`
		// SkipDirIDs 是用户在 admin 配置的"扫描跳过目录"集合（drive 侧目录 fileID）。
		// 前端用它在"设置跳过目录"弹窗里回显已选项；JSON 字段名 camelCase 与
		// catalog.Drive 保持一致。
		SkipDirIDs  []string `json:"skipDirIds"`
		LastCrawlAt int64    `json:"lastCrawlAt,omitempty"`
		// STRMAllowOutsideRoot 是 localstorage 的 .strm 越root开关；其它 kind 省略。
		STRMAllowOutsideRoot          *bool            `json:"strmAllowOutsideRoot,omitempty"`
		ScanGenerationStatus          GenerationStatus `json:"scanGenerationStatus"`
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
		TranscodeGenerationStatus     GenerationStatus `json:"transcodeGenerationStatus"`
		TranscodePendingCount         int              `json:"transcodePendingCount"`
		TranscodeReadyCount           int              `json:"transcodeReadyCount"`
		TranscodeFailedCount          int              `json:"transcodeFailedCount"`
		TranscodeSkippedCount         int              `json:"transcodeSkippedCount"`
	}
	list := make([]out, 0, len(drives))
	for _, d := range drives {
		if isCrawlerDriveKind(d.Kind) {
			continue
		}
		counts := teaserCounts[d.ID]
		thumbCounts := thumbnailCounts[d.ID]
		fingerprintCount := fingerprintCounts[d.ID]
		transcodeCount := transcodeCounts[d.ID]
		generation := generationStatuses[d.ID]
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
		if generation.Transcode.State == "" {
			generation.Transcode.State = "idle"
		}
		// last_crawl_at 是后端自动写入的运行状态字段，不计入 hasCredential 判定。
		hasCred := false
		userCredKeys := 0
		for k := range d.Credentials {
			if k == "last_crawl_at" {
				continue
			}
			userCredKeys++
		}
		hasCred = userCredKeys > 0

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
			CanUpload:                     drivepkg.CapabilitiesForKind(d.Kind).Upload,
			TeaserEnabled:                 d.TeaserEnabled,
			SkipDirIDs:                    append([]string{}, d.SkipDirIDs...),
			LastCrawlAt:                   lastCrawlAt,
			STRMAllowOutsideRoot:          strmAllowOutsideRootForDrive(d),
			ScanGenerationStatus:          generation.Scan,
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
			TranscodeGenerationStatus:     generation.Transcode,
			TranscodePendingCount:         transcodeCount.Pending,
			TranscodeReadyCount:           transcodeCount.Ready,
			TranscodeFailedCount:          transcodeCount.Failed,
			TranscodeSkippedCount:         transcodeCount.Skipped,
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
	// TeaserEnabled 是 per-drive 预览视频生成开关；封面生成不受影响。
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
	if !isSupportedDriveKind(body.Kind) {
		http.Error(w, "unsupported drive kind", http.StatusBadRequest)
		return
	}
	configLease, ok := a.beginDriveConfigUpdate(w, body.ID)
	if !ok {
		return
	}
	if configLease != nil {
		defer configLease.Release()
	}

	// 凭证 / TeaserEnabled 都支持 "未传 = 沿用旧值"：先把现存 drive 拉出来一次。
	// 只有确实不存在才进入新建路径；读取失败时继续保存会误判变更类型，甚至把
	// 同 ID 的现有凭证当成新网盘整组替换。
	var existing *catalog.Drive
	existingDrive, err := a.Catalog.GetDrive(r.Context(), body.ID)
	switch {
	case err == nil:
		existing = existingDrive
	case errors.Is(err, sql.ErrNoRows):
		// New drive.
	default:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// Existing drives use credential-patch semantics. Access/refresh tokens and
	// cookies may rotate after an edit form was opened; replacing the form's
	// complete snapshot would roll those runtime updates back. A kind change is
	// intentionally a replacement so credentials never leak across providers.
	patchCredentials := existing != nil && existing.Kind == body.Kind && body.Kind != scriptcrawler.Kind
	if body.Kind == scriptcrawler.Kind {
		credentials, err := mergeScriptCrawlerCredentials(existing, body.Credentials)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		body.Credentials = credentials
	} else if body.Kind == "googledrive" {
		if patchCredentials {
			body.Credentials = googleDriveCredentialPatch(body.Credentials)
		} else {
			body.Credentials = mergeGoogleDriveCredentials(nil, body.Credentials)
		}
	} else if body.Kind == "onedrive" && patchCredentials {
		body.Credentials = oneDriveCredentialPatch(body.Credentials)
	} else {
		body.Credentials = nonEmptyCredentials(body.Credentials)
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
	//   2. 请求没带 → 在 catalog 的冲突更新 SQL 中保留当前值，避免这里先读到
	//      旧值后与跳过目录自动保存发生丢失更新；新建 drive 仍写入空数组。
	var skipDirIDs []string
	if body.SkipDirIDs != nil {
		skipDirIDs = *body.SkipDirIDs
	}

	// Status is only the initial value for a new row. Catalog configuration
	// upserts preserve status/last_error on conflicts; the mounted runtime owns
	// subsequent connection-state updates.
	d := &catalog.Drive{
		ID: body.ID, Kind: body.Kind, Name: body.Name,
		RootID:        catalog.NormalizeDriveRootID(body.Kind, body.RootID),
		Credentials:   body.Credentials,
		Status:        "disconnected",
		TeaserEnabled: teaserEnabled,
		SkipDirIDs:    skipDirIDs,
	}
	runtimeReload := driveRuntimeReloadRequired(existing, d)
	teaserChanged := existing != nil && existing.TeaserEnabled != teaserEnabled
	updateScope := DriveConfigUpdateScope(0)
	if runtimeReload {
		updateScope |= DriveConfigUpdateRuntime
	}
	if teaserChanged {
		updateScope |= DriveConfigUpdatePreview
	}
	if body.SkipDirIDs != nil {
		updateScope |= DriveConfigUpdateScan
	}
	if !authorizeDriveConfigUpdate(w, configLease, updateScope) {
		return
	}

	saveErr := a.Catalog.UpsertDriveWithOptions(r.Context(), d, catalog.DriveUpsertOptions{
		ReplaceSkipDirIDs:    body.SkipDirIDs != nil,
		ReplaceTeaserEnabled: body.TeaserEnabled != nil,
		PatchCredentials:     patchCredentials,
	})
	if saveErr != nil {
		writeErr(w, http.StatusInternalServerError, saveErr)
		return
	}
	deferred := false
	var runtimeErr error
	if runtimeReload {
		wasDeferred, applyErr := commitDriveConfigUpdate(configLease, DriveConfigUpdateRuntime, func() error {
			if a.OnDriveRuntimeConfigChanged == nil {
				return nil
			}
			return a.OnDriveRuntimeConfigChanged(body.ID)
		})
		deferred = deferred || wasDeferred
		runtimeErr = applyErr
	}
	if teaserChanged {
		wasDeferred, applyErr := commitDriveConfigUpdate(configLease, DriveConfigUpdatePreview, func() error {
			if a.OnTeaserEnabledChanged != nil {
				a.OnTeaserEnabledChanged(body.ID, teaserEnabled)
			}
			return nil
		})
		deferred = deferred || wasDeferred
		if runtimeErr == nil {
			runtimeErr = applyErr
		}
	}
	if body.SkipDirIDs != nil {
		wasDeferred, applyErr := commitDriveConfigUpdate(configLease, DriveConfigUpdateScan, nil)
		deferred = deferred || wasDeferred
		if runtimeErr == nil {
			runtimeErr = applyErr
		}
	}
	resp := map[string]any{"ok": true}
	if deferred {
		resp["deferred"] = true
		resp["message"] = driveConfigDeferredMessage
	}
	if runtimeErr != nil {
		resp["warning"] = runtimeErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// driveRuntimeReloadRequired separates persisted metadata from the fields
// captured by a mounted Driver. For an existing non-crawler drive Credentials
// contains only the explicit JSON patch, so an empty map means the current
// runtime credentials must be left untouched. Script files can be overwritten
// in place without changing their path; crawler saves therefore remain
// conservatively reload-on-save.
func driveRuntimeReloadRequired(existing, next *catalog.Drive) bool {
	if existing == nil || next == nil {
		return true
	}
	if existing.Kind != next.Kind || existing.RootID != next.RootID {
		return true
	}
	if next.Kind == scriptcrawler.Kind {
		return true
	}
	return len(next.Credentials) > 0
}

func (a *AdminServer) beginDriveConfigUpdate(w http.ResponseWriter, driveID string) (DriveConfigUpdateLease, bool) {
	if a.BeginDriveConfigUpdate == nil {
		return nil, true
	}
	lease, reason := a.BeginDriveConfigUpdate(driveID)
	if lease != nil {
		return lease, true
	}
	if reason == "" {
		reason = "当前网盘配置正在更新，请稍后重试"
	}
	http.Error(w, reason, http.StatusConflict)
	return nil, false
}

func authorizeDriveConfigUpdate(w http.ResponseWriter, lease DriveConfigUpdateLease, scope DriveConfigUpdateScope) bool {
	if lease == nil {
		return true
	}
	if reason := lease.Authorize(scope); reason != "" {
		http.Error(w, reason, http.StatusConflict)
		return false
	}
	return true
}

const driveConfigDeferredMessage = "配置已保存，将在当前网盘任务结束后生效"

func commitDriveConfigUpdate(lease DriveConfigUpdateLease, scope DriveConfigUpdateScope, apply func() error) (bool, error) {
	if lease != nil {
		return lease.Commit(scope, apply)
	}
	if apply == nil {
		return false, nil
	}
	return false, apply()
}
