package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/nianzhibai/91/backend/internal/catalog"
)

func (a *AdminServer) handleMergedVideos(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := catalog.MergedVideoFilter{DriveID: q.Get("driveId"), CanonicalVideoID: q.Get("canonicalVideoId"), SourceState: q.Get("sourceState")}
	f.Page, _ = strconv.Atoi(q.Get("page"))
	f.Size, _ = strconv.Atoi(q.Get("size"))
	switch f.SourceState {
	case "", catalog.MergedSourceRecorded, catalog.MergedSourceGone, catalog.MergedSourceReplaced:
	default:
		writeErr(w, r, http.StatusBadRequest, errors.New("invalid sourceState"))
		return
	}
	list, err := a.Catalog.ListMergedVideos(r.Context(), f)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *AdminServer) handleMergedVideo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	m, err := a.Catalog.GetMergedVideo(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeCatalogLookupError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

type mergedSourceDeleteRequest struct {
	SourceRevision string `json:"sourceRevision"`
}

func (a *AdminServer) handleDeleteMergedSource(w http.ResponseWriter, r *http.Request) {
	var body mergedSourceDeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.SourceRevision == "" {
		writeErr(w, r, http.StatusBadRequest, errors.New("sourceRevision is required"))
		return
	}
	if a.OnDeleteMergedSource == nil {
		writeServiceUnavailable(w, r, "delete merged source", errors.New("source removal is unavailable"))
		return
	}
	id := chi.URLParam(r, "id")
	err := a.OnDeleteMergedSource(r.Context(), id, body.SourceRevision)
	if err != nil {
		for _, conflict := range []struct {
			err  error
			code string
		}{
			{catalog.ErrMergedRevisionChanged, "source_revision_changed"}, {catalog.ErrMergedSourceShared, "source_shared"},
			{catalog.ErrMergedSourceReplaced, "source_replaced"}, {catalog.ErrMergedSourceUnverified, "source_unverified"},
			{catalog.ErrVideoSourceInUse, "source_removal_in_progress"},
		} {
			if errors.Is(err, conflict.err) {
				writeJSON(w, http.StatusConflict, map[string]string{"code": conflict.code, "error": err.Error()})
				return
			}
		}
		if errors.Is(err, sql.ErrNoRows) {
			writeCatalogLookupError(w, r, err)
			return
		}
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"videoId": id, "sourceState": catalog.MergedSourceGone})
}
