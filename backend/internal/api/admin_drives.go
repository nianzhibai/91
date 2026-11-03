package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/driveevents"
	drivepkg "github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/drives/scriptcrawler"
	"github.com/video-site/backend/internal/driveview"
	"github.com/video-site/backend/internal/scanjob"
)

type driveConfigDTO struct {
	ID                   string   `json:"id"`
	Kind                 string   `json:"kind"`
	Name                 string   `json:"name"`
	RootID               string   `json:"rootId"`
	ScanRootID           string   `json:"scanRootId"`
	Status               string   `json:"status"`
	LastError            string   `json:"lastError,omitempty"`
	HasCredential        bool     `json:"hasCredential"`
	CanUpload            bool     `json:"canUpload"`
	SkipDirIDs           []string `json:"skipDirIds"`
	DirectoryRevision    uint64   `json:"directoryRevision"`
	LastCrawlAt          int64    `json:"lastCrawlAt,omitempty"`
	STRMAllowOutsideRoot *bool    `json:"strmAllowOutsideRoot,omitempty"`
}

type driveRuntimeDTO struct {
	ScanGenerationStatus        GenerationStatus  `json:"scanGenerationStatus"`
	ThumbnailGenerationStatus   GenerationStatus  `json:"thumbnailGenerationStatus"`
	PreviewGenerationStatus     GenerationStatus  `json:"previewGenerationStatus"`
	FingerprintGenerationStatus GenerationStatus  `json:"fingerprintGenerationStatus"`
	MaintenanceStatus           *NightlyJobStatus `json:"maintenanceStatus,omitempty"`
}

type driveStatsDTO struct {
	ThumbnailReadyCount           int `json:"thumbnailReadyCount"`
	ThumbnailPendingCount         int `json:"thumbnailPendingCount"`
	ThumbnailFailedCount          int `json:"thumbnailFailedCount"`
	ThumbnailDurationPendingCount int `json:"thumbnailDurationPendingCount"`
	TeaserReadyCount              int `json:"teaserReadyCount"`
	TeaserPendingCount            int `json:"teaserPendingCount"`
	TeaserFailedCount             int `json:"teaserFailedCount"`
	FingerprintReadyCount         int `json:"fingerprintReadyCount"`
	FingerprintPendingCount       int `json:"fingerprintPendingCount"`
	FingerprintFailedCount        int `json:"fingerprintFailedCount"`
}

type driveDTO struct {
	driveConfigDTO
	driveRuntimeDTO
	driveStatsDTO
}

func (a *AdminServer) driveConfig(d *catalog.Drive) driveConfigDTO {
	hasCredential := false
	for key := range d.Credentials {
		if key != "last_crawl_at" {
			hasCredential = true
			break
		}
	}
	lastCrawlAt, _ := strconv.ParseInt(d.Credentials["last_crawl_at"], 10, 64)
	return driveConfigDTO{
		ID: d.ID, Kind: d.Kind, Name: d.Name, RootID: d.RootID, ScanRootID: d.ScanRootID,
		Status: d.Status, LastError: d.LastError, HasCredential: hasCredential,
		CanUpload:  drivepkg.CapabilitiesForKind(d.Kind).Upload,
		SkipDirIDs: append([]string{}, d.SkipDirIDs...), LastCrawlAt: lastCrawlAt,
		DirectoryRevision:    a.Catalog.DriveEvents().Version(d.ID, driveevents.DriveRuntimeChanged),
		STRMAllowOutsideRoot: strmAllowOutsideRootForDrive(d),
	}
}

func driveRuntime(generation DriveGenerationStatuses, result *scanjob.Result) driveRuntimeDTO {
	if result != nil && (generation.Scan.State == "" || generation.Scan.State == "idle") {
		generation.Scan.State = string(result.State)
		generation.Scan.ScannedCount = result.ScannedCount
		generation.Scan.AddedCount = result.AddedCount
		generation.Scan.Result = result
	}
	for _, status := range []*GenerationStatus{&generation.Scan, &generation.Thumbnail, &generation.Preview, &generation.Fingerprint} {
		if status.State == "" {
			status.State = "idle"
		}
	}
	return driveRuntimeDTO{ScanGenerationStatus: generation.Scan, ThumbnailGenerationStatus: generation.Thumbnail,
		PreviewGenerationStatus: generation.Preview, FingerprintGenerationStatus: generation.Fingerprint}
}

func driveStats(id string, stats catalog.DriveAssetStats) driveStatsDTO {
	thumb, teaser, fingerprint := stats.Thumbnails[id], stats.Teasers[id], stats.Fingerprints[id]
	return driveStatsDTO{
		ThumbnailReadyCount: thumb.Ready, ThumbnailPendingCount: thumb.Pending, ThumbnailFailedCount: thumb.Failed,
		ThumbnailDurationPendingCount: thumb.DurationPending, TeaserReadyCount: teaser.Ready, TeaserPendingCount: teaser.Pending,
		TeaserFailedCount: teaser.Failed, FingerprintReadyCount: fingerprint.Ready,
		FingerprintPendingCount: fingerprint.Pending, FingerprintFailedCount: fingerprint.Failed,
	}
}

func (a *AdminServer) handleListDrives(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	drives, err := a.Catalog.ListDrives(r.Context())
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	assetStats, err := a.Catalog.CountDriveAssetStats(r.Context())
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	scanResults, err := a.Catalog.LatestScanResults(r.Context())
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	generationStatuses := map[string]DriveGenerationStatuses{}
	if a.GetDriveGenerationStatuses != nil {
		generationStatuses = a.GetDriveGenerationStatuses()
	}
	list := make([]driveDTO, 0, len(drives))
	for _, d := range drives {
		if isCrawlerDriveKind(d.Kind) {
			continue
		}
		var result *scanjob.Result
		if saved, ok := scanResults[d.ID]; ok {
			result = &saved
		}
		list = append(list, driveDTO{a.driveConfig(d), driveRuntime(generationStatuses[d.ID], result), driveStats(d.ID, assetStats)})
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
	// SkipDirIDs 同样用指针区分 "未传"（沿用旧值）/ "传了空数组"（清空）。
	// 推荐前端"设置跳过目录"走专用 POST /drives/{id}/skip-dirs；
	// 这里支持是为了允许批量编辑场景一次性提交。
	SkipDirIDs *[]string `json:"skipDirIds,omitempty"`
}

func (a *AdminServer) handleUpsertDrive(w http.ResponseWriter, r *http.Request) {
	var body upsertDriveReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
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

	// 凭证支持 "未传 = 沿用旧值"：先把现存 drive 拉出来一次。
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
		writeErr(w, r, http.StatusInternalServerError, err)
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
		if err := a.normalizeCrawlerScriptReference(body.Credentials); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	} else if body.Kind == "googledrive" {
		body.Credentials, err = googleDriveCredentialPatch(body.Credentials)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	} else if body.Kind == "onedrive" && patchCredentials {
		body.Credentials = oneDriveCredentialPatch(body.Credentials)
	} else {
		body.Credentials = nonEmptyCredentials(body.Credentials)
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
		RootID:      catalog.NormalizeDriveRootID(body.Kind, body.RootID),
		Credentials: body.Credentials,
		Status:      "disconnected",
		SkipDirIDs:  skipDirIDs,
	}
	runtimeReload := driveRuntimeReloadRequired(existing, d)
	updateScope := DriveConfigUpdateScope(0)
	if runtimeReload {
		updateScope |= DriveConfigUpdateRuntime
	}
	if body.SkipDirIDs != nil {
		updateScope |= DriveConfigUpdateScan
	}
	if !authorizeDriveConfigUpdate(w, configLease, updateScope) {
		return
	}

	saveErr := a.Catalog.UpsertDriveWithOptions(r.Context(), d, catalog.DriveUpsertOptions{
		ReplaceSkipDirIDs: body.SkipDirIDs != nil,
		PatchCredentials:  patchCredentials,
	})
	if saveErr != nil {
		writeErr(w, r, http.StatusInternalServerError, saveErr)
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
	if body.SkipDirIDs != nil {
		wasDeferred, applyErr := commitDriveConfigUpdate(configLease, DriveConfigUpdateScan, nil)
		deferred = deferred || wasDeferred
		if runtimeErr == nil {
			runtimeErr = applyErr
		}
	}
	resp := map[string]any{"ok": true}
	a.addDriveSnapshot(r.Context(), resp, body.ID, driveview.Config)
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
