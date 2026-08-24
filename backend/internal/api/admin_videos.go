package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/video-site/backend/internal/catalog"
)

type updateVideoReq struct {
	Title       json.RawMessage `json:"title"`
	Author      json.RawMessage `json:"author"`
	Tags        []string        `json:"tags"`
	Badges      []string        `json:"badges"`
	Description string          `json:"description"`
	Thumbnail   string          `json:"thumbnail"`
	DurationSec int             `json:"durationSeconds"`
}

type adminVideoDTO struct {
	ID                string            `json:"id"`
	DriveID           string            `json:"driveId"`
	FileID            string            `json:"fileId"`
	FileName          string            `json:"fileName"`
	ContentHash       string            `json:"contentHash"`
	SampledSHA256     string            `json:"sampledSha256"`
	FingerprintStatus string            `json:"fingerprintStatus"`
	FingerprintError  string            `json:"fingerprintError"`
	ParentID          string            `json:"parentId"`
	Title             string            `json:"title"`
	Author            string            `json:"author"`
	Tags              []string          `json:"tags"`
	TagSources        map[string]string `json:"tagSources,omitempty"`
	TagEvidence       map[string]string `json:"tagEvidence,omitempty"`
	DurationSeconds   int               `json:"durationSeconds"`
	Size              int64             `json:"size"`
	Ext               string            `json:"ext"`
	ThumbnailURL      string            `json:"thumbnailUrl"`
	PreviewFileID     string            `json:"previewFileId"`
	PreviewLocal      string            `json:"previewLocal"`
	PreviewStatus     string            `json:"previewStatus"`
	TranscodeStatus   string            `json:"transcodeStatus"`
	TranscodeError    string            `json:"transcodeError"`
	TranscodedFileID  string            `json:"transcodedFileId"`
	TranscodedSize    int64             `json:"transcodedSize"`
	Views             int               `json:"views"`
	LastViewedAt      time.Time         `json:"lastViewedAt"`
	Favorites         int               `json:"favorites"`
	Comments          int               `json:"comments"`
	Likes             int               `json:"likes"`
	Dislikes          int               `json:"dislikes"`
	Hidden            bool              `json:"hidden"`
	Badges            []string          `json:"badges"`
	Description       string            `json:"description"`
	PublishedAt       time.Time         `json:"publishedAt"`
	CreatedAt         time.Time         `json:"createdAt"`
	UpdatedAt         time.Time         `json:"updatedAt"`
}

func mapAdminVideo(v *catalog.Video) adminVideoDTO {
	if v == nil {
		return adminVideoDTO{}
	}
	return adminVideoDTO{
		ID:                v.ID,
		DriveID:           v.DriveID,
		FileID:            v.FileID,
		FileName:          v.FileName,
		ContentHash:       v.ContentHash,
		SampledSHA256:     v.SampledSHA256,
		FingerprintStatus: v.FingerprintStatus,
		FingerprintError:  v.FingerprintError,
		ParentID:          v.ParentID,
		Title:             v.Title,
		Author:            v.Author,
		Tags:              v.Tags,
		DurationSeconds:   v.DurationSeconds,
		Size:              v.Size,
		Ext:               v.Ext,
		ThumbnailURL:      v.ThumbnailURL,
		PreviewFileID:     v.PreviewFileID,
		PreviewLocal:      v.PreviewLocal,
		PreviewStatus:     v.PreviewStatus,
		TranscodeStatus:   v.TranscodeStatus,
		TranscodeError:    v.TranscodeError,
		TranscodedFileID:  v.TranscodedFileID,
		TranscodedSize:    v.TranscodedSize,
		Views:             v.Views,
		LastViewedAt:      v.LastViewedAt,
		Favorites:         v.Favorites,
		Comments:          v.Comments,
		Likes:             v.Likes,
		Dislikes:          v.Dislikes,
		Hidden:            v.Hidden,
		Badges:            v.Badges,
		Description:       v.Description,
		PublishedAt:       v.PublishedAt,
		CreatedAt:         v.CreatedAt,
		UpdatedAt:         v.UpdatedAt,
	}
}

func mapAdminVideos(vs []*catalog.Video) []adminVideoDTO {
	out := make([]adminVideoDTO, 0, len(vs))
	for _, v := range vs {
		out = append(out, mapAdminVideo(v))
	}
	return out
}

func (a *AdminServer) handleUpdateVideo(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body updateVideoReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(body.Title) > 0 || len(body.Author) > 0 {
		writeErr(w, http.StatusBadRequest, errors.New("video title and author are read-only"))
		return
	}
	v, err := a.Catalog.GetVideo(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
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
	writeJSON(w, http.StatusOK, mapAdminVideo(v))
}

func (a *AdminServer) handleDeleteVideo(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, errors.New("invalid video id"))
		return
	}
	var body deleteVideoReq
	if r.Body != nil {
		defer r.Body.Close()
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	var (
		result DeleteVideoResult
		err    error
	)
	if a.OnDeleteVideo != nil {
		result, err = a.OnDeleteVideo(r.Context(), id, body.DeleteSource)
	} else {
		err = a.Catalog.DeleteVideoWithTombstone(r.Context(), id)
		result = DeleteVideoResult{OK: err == nil}
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !result.OK {
		result.OK = true
	}
	writeJSON(w, http.StatusOK, result)
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
