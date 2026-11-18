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

	"github.com/nianzhibai/91/backend/internal/catalog"
)

type updateVideoReq struct {
	Title       json.RawMessage `json:"title"`
	Author      json.RawMessage `json:"author"`
	Tags        []string        `json:"tags"`
	Badges      []string        `json:"badges"`
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
	Views             int               `json:"views"`
	LastViewedAt      time.Time         `json:"lastViewedAt"`
	Favorites         int               `json:"favorites"`
	Comments          int               `json:"comments"`
	Likes             int               `json:"likes"`
	Dislikes          int               `json:"dislikes"`
	Hidden            bool              `json:"hidden"`
	Badges            []string          `json:"badges"`
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
		Views:             v.Views,
		LastViewedAt:      v.LastViewedAt,
		Favorites:         v.Favorites,
		Comments:          v.Comments,
		Likes:             v.Likes,
		Dislikes:          v.Dislikes,
		Hidden:            v.Hidden,
		Badges:            v.Badges,
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
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	if len(body.Title) > 0 || len(body.Author) > 0 {
		writeErr(w, r, http.StatusBadRequest, errors.New("video title and author are read-only"))
		return
	}
	v, err := a.Catalog.GetVideo(r.Context(), id)
	if err != nil {
		writeErr(w, r, http.StatusNotFound, err)
		return
	}
	if body.Badges != nil {
		v.Badges = body.Badges
	}
	if body.Thumbnail != "" {
		v.ThumbnailURL = body.Thumbnail
	}
	if body.DurationSec > 0 {
		v.DurationSeconds = body.DurationSec
	}
	if err := a.Catalog.UpsertVideo(r.Context(), v); err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	if body.Tags != nil {
		if err := a.Catalog.SetManualVideoTags(r.Context(), id, body.Tags); err != nil {
			if errors.Is(err, catalog.ErrUnknownTag) {
				writeErr(w, r, http.StatusBadRequest, err)
				return
			}
			writeErr(w, r, http.StatusInternalServerError, err)
			return
		}
		v, err = a.Catalog.GetVideo(r.Context(), id)
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, mapAdminVideo(v))
}

func (a *AdminServer) handleVideoDeletionPreview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	preview, err := a.Catalog.PreviewVideoDeletion(r.Context(), strings.TrimSpace(chi.URLParam(r, "id")))
	if err != nil {
		writeCatalogLookupError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

func (a *AdminServer) handleDeleteVideo(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		writeErr(w, r, http.StatusBadRequest, errors.New("invalid video id"))
		return
	}
	var body catalog.VideoDeletionOptions
	if r.Body != nil {
		defer r.Body.Close()
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, r, http.StatusBadRequest, err)
			return
		}
	}
	var (
		result DeleteVideoResult
		err    error
	)
	if a.OnDeleteVideo != nil {
		result, err = a.OnDeleteVideo(r.Context(), id, body)
	} else if body.DeleteSource {
		writeServiceUnavailable(w, r, "delete video source", errors.New("source removal is unavailable"))
		return
	} else {
		err = a.Catalog.DeleteVideoGroup(r.Context(), id, body, time.Time{}, nil, nil)
		result = DeleteVideoResult{OK: err == nil}
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, r, http.StatusNotFound, err)
			return
		}
		if errors.Is(err, catalog.ErrDeletionScopeIncomplete) {
			writeErr(w, r, http.StatusBadRequest, err)
			return
		}
		if errors.Is(err, catalog.ErrDeletionScopeChanged) {
			writeJSON(w, http.StatusConflict, map[string]string{"code": "deletion_scope_changed", "error": err.Error()})
			return
		}
		if errors.Is(err, catalog.ErrVideoSourceInUse) {
			writeErr(w, r, http.StatusConflict, errors.New("源文件删除正在进行，请稍后重试"))
			return
		}
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	if !result.OK {
		result.OK = true
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *AdminServer) handleRegenPreview(w http.ResponseWriter, r *http.Request) {
	if !a.requirePreviewEnabled(w) {
		return
	}
	id := chi.URLParam(r, "id")
	if a.OnRegenPreview != nil {
		a.OnRegenPreview(id)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (a *AdminServer) handleRegenAllPreviews(w http.ResponseWriter, r *http.Request) {
	if !a.requirePreviewEnabled(w) {
		return
	}
	if a.OnRegenAllPreviews != nil {
		a.OnRegenAllPreviews()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}
