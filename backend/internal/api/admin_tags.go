package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/video-site/backend/internal/tagging"
)

func (a *AdminServer) handleListTags(w http.ResponseWriter, r *http.Request) {
	tags, err := a.Catalog.ListTags(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, tags)
}

type createTagReq struct {
	Label string `json:"label"`
}

type updateTagReq struct {
	MatchRules tagging.Rule `json:"matchRules"`
}

func (a *AdminServer) handleCreateTag(w http.ResponseWriter, r *http.Request) {
	var body createTagReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	classified, err := a.Catalog.CreateTagAndClassify(r.Context(), body.Label, nil, "user")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if a.OnTagsChanged != nil {
		a.OnTagsChanged()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"label":      body.Label,
		"classified": classified,
	})
}

func (a *AdminServer) handleUpdateTag(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, errors.New("invalid tag id"))
		return
	}
	var body updateTagReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	tag, _, err := a.Catalog.UpdateTagAndReconcile(r.Context(), id, body.MatchRules)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, err)
		} else {
			writeErr(w, http.StatusBadRequest, err)
		}
		return
	}
	if a.OnTagsChanged != nil {
		a.OnTagsChanged()
	}
	writeJSON(w, http.StatusOK, map[string]any{"tag": tag})
}

func (a *AdminServer) handleStartTagRetag(w http.ResponseWriter, _ *http.Request) {
	if a.OnStartTagRetag == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("tag maintenance is not configured"))
		return
	}
	if !a.OnStartTagRetag() {
		writeJSON(w, http.StatusConflict, map[string]any{"accepted": false})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true})
}

func (a *AdminServer) handleTagJobStatus(w http.ResponseWriter, _ *http.Request) {
	status := TagJobStatus{State: "idle"}
	if a.GetTagJobStatus != nil {
		status = a.GetTagJobStatus()
	}
	writeJSON(w, http.StatusOK, status)
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
		default:
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	if a.OnTagsChanged != nil {
		a.OnTagsChanged()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removedVideos": removedVideos})
}
