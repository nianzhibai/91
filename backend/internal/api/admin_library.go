package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/video-site/backend/internal/catalog"
)

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
	createdAtFrom, createdAtBefore, err := parseAdminVideoDateRange(
		q.Get("createdFrom"),
		q.Get("createdTo"),
		"入库时间",
	)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	durationSecondsMin, durationSecondsMax, err := parseAdminVideoDurationRange(
		q.Get("durationMinMinutes"),
		q.Get("durationMaxMinutes"),
	)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	items, total, err := a.Catalog.ListVideos(r.Context(), catalog.ListParams{
		Keyword:            q.Get("keyword"),
		DriveID:            q.Get("driveId"),
		CrawlerID:          strings.TrimSpace(q.Get("crawlerId")),
		CreatedAtFrom:      createdAtFrom,
		CreatedAtBefore:    createdAtBefore,
		DurationSecondsMin: durationSecondsMin,
		DurationSecondsMax: durationSecondsMax,
		Page:               page,
		PageSize:           size,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if a.GetPreviewGenerationVideoIDs != nil {
		generating := a.GetPreviewGenerationVideoIDs()
		for _, item := range items {
			if item != nil && generating[item.ID] {
				item.PreviewStatus = "generating"
			}
		}
	}
	videoIDs := make([]string, 0, len(items))
	for _, item := range items {
		if item != nil {
			videoIDs = append(videoIDs, item.ID)
		}
	}
	tagMetadata, err := a.Catalog.ListVideoTagMetadata(r.Context(), videoIDs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	mappedItems := mapAdminVideos(items)
	for i := range mappedItems {
		metadata := tagMetadata[mappedItems[i].ID]
		if len(metadata) == 0 {
			continue
		}
		mappedItems[i].TagSources = make(map[string]string, len(metadata))
		mappedItems[i].TagEvidence = make(map[string]string, len(metadata))
		for label, item := range metadata {
			mappedItems[i].TagSources[label] = item.Source
			if item.Evidence != "" {
				mappedItems[i].TagEvidence[label] = item.Evidence
			}
		}
		if len(mappedItems[i].TagEvidence) == 0 {
			mappedItems[i].TagEvidence = nil
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": mappedItems,
		"total": total,
		"page":  page,
		"size":  size,
	})
}

const adminVideoDateLayout = "2006-01-02"

// parseAdminVideoDateRange converts inclusive date inputs into an inclusive
// start and exclusive next-day boundary in the server's local timezone.
func parseAdminVideoDateRange(fromRaw, toRaw, label string) (from, before int64, err error) {
	fromRaw = strings.TrimSpace(fromRaw)
	toRaw = strings.TrimSpace(toRaw)
	now := time.Now().In(time.Local)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	if fromRaw != "" {
		parsed, parseErr := time.ParseInLocation(adminVideoDateLayout, fromRaw, time.Local)
		if parseErr != nil {
			return 0, 0, fmt.Errorf("%s开始日期格式无效，应为 YYYY-MM-DD", label)
		}
		if parsed.After(today) {
			return 0, 0, fmt.Errorf("%s开始日期不能超过当天", label)
		}
		from = parsed.UnixMilli()
	}
	if toRaw != "" {
		parsed, parseErr := time.ParseInLocation(adminVideoDateLayout, toRaw, time.Local)
		if parseErr != nil {
			return 0, 0, fmt.Errorf("%s结束日期格式无效，应为 YYYY-MM-DD", label)
		}
		if parsed.After(today) {
			return 0, 0, fmt.Errorf("%s结束日期不能超过当天", label)
		}
		before = parsed.AddDate(0, 0, 1).UnixMilli()
	}
	if from > 0 && before > 0 && from >= before {
		return 0, 0, fmt.Errorf("%s开始日期不能晚于结束日期", label)
	}
	return from, before, nil
}

const adminVideoMaxDurationMinutes = 365 * 24 * 60

// parseAdminVideoDurationRange converts an inclusive whole-minute range into
// seconds, matching the catalog's duration_seconds storage unit.
func parseAdminVideoDurationRange(minRaw, maxRaw string) (minSeconds, maxSeconds int, err error) {
	parseMinutes := func(raw, boundary string) (int, error) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return 0, nil
		}
		minutes, parseErr := strconv.Atoi(raw)
		if parseErr != nil || minutes <= 0 || minutes > adminVideoMaxDurationMinutes {
			return 0, fmt.Errorf("视频时长%s必须为大于 0 的整数分钟", boundary)
		}
		return minutes, nil
	}

	minMinutes, err := parseMinutes(minRaw, "最短值")
	if err != nil {
		return 0, 0, err
	}
	maxMinutes, err := parseMinutes(maxRaw, "最长值")
	if err != nil {
		return 0, 0, err
	}
	if minMinutes > 0 && maxMinutes > 0 && minMinutes > maxMinutes {
		return 0, 0, fmt.Errorf("视频时长最短值不能大于最长值")
	}
	return minMinutes * 60, maxMinutes * 60, nil
}

// handleVideoStats 返回后台视频管理两个标签页的计数（当前/拉黑）。
func (a *AdminServer) handleVideoStats(w http.ResponseWriter, r *http.Request) {
	current, blacklisted, err := a.Catalog.VideoManagementCounts(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current":     current,
		"blacklisted": blacklisted,
	})
}

// handleListBlacklist 分页返回黑名单（墓碑）视频。
func (a *AdminServer) handleListBlacklist(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	size, _ := strconv.Atoi(q.Get("size"))
	if page <= 0 {
		page = 1
	}
	if size <= 0 || size > 100 {
		size = 100
	}
	items, total, err := a.Catalog.ListDeletedVideos(r.Context(), catalog.ListParams{
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

func (a *AdminServer) handleStartBlacklistSourceDelete(w http.ResponseWriter, r *http.Request) {
	var body BlacklistSourceDeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := normalizeBlacklistSourceDeleteRequest(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	accepted := false
	if a.OnStartBlacklistSourceDelete != nil {
		accepted = a.OnStartBlacklistSourceDelete(body)
	}
	resp := map[string]any{
		"ok":       true,
		"accepted": accepted,
		"status":   a.blacklistSourceDeleteStatus(r.Context()),
	}
	if !accepted {
		resp["message"] = "黑名单源文件删除任务已在运行"
	}
	writeJSON(w, http.StatusAccepted, resp)
}

func normalizeBlacklistSourceDeleteRequest(req *BlacklistSourceDeleteRequest) error {
	if req == nil {
		return errors.New("blacklist source delete request is required")
	}
	seen := make(map[string]bool, len(req.IDs))
	ids := req.IDs[:0]
	for _, id := range req.IDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	req.IDs = ids

	hasIDs := len(req.IDs) > 0
	switch {
	case req.DeleteAllSources && hasIDs:
		return errors.New("deleteAllSources and ids cannot be used together")
	case !req.DeleteAllSources && !hasIDs:
		return errors.New("deleteAllSources=true or ids is required")
	default:
		return nil
	}
}

func (a *AdminServer) handleBlacklistSourceDeleteStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.blacklistSourceDeleteStatus(r.Context()))
}

func (a *AdminServer) blacklistSourceDeleteStatus(ctx context.Context) BlacklistSourceDeleteStatus {
	var status BlacklistSourceDeleteStatus
	if a.GetBlacklistSourceDeleteStatus == nil {
		status.State = "idle"
	} else {
		status = a.GetBlacklistSourceDeleteStatus()
	}
	if status.State == "" {
		status.State = "idle"
	}
	if a.Catalog != nil {
		if pending, err := a.Catalog.CountDeletedVideosPendingSourceDeletion(ctx); err == nil {
			status.Pending = pending
		}
	}
	return status
}

// handleRemoveBlacklist 允许视频在后续手动/定时任务中重新入库，不会立即触发
// 扫盘或爬取。direct 策略的来源（本地上传）没有任何重新发现的途径，改为当场
// 重建记录，因此先校验保留下来的源文件还在。不可恢复的来源仍然返回 409。
func (a *AdminServer) handleRemoveBlacklist(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var err error
	if a.OnRemoveBlacklist != nil {
		err = a.OnRemoveBlacklist(r.Context(), id)
	} else {
		err = a.Catalog.RemoveDeletedVideo(r.Context(), id)
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		if errors.Is(err, catalog.ErrDeletedVideoNotRestorable) ||
			errors.Is(err, catalog.ErrDeletedVideoSourceCheckRequired) ||
			errors.Is(err, catalog.ErrDeletedVideoSourceMissing) {
			writeErr(w, http.StatusConflict, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
