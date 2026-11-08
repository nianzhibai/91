package api

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/nianzhibai/91/backend/internal/httpapi"
)

type DriveGenerationKind string

const (
	DriveGenerationThumbnails   DriveGenerationKind = "thumbnails"
	DriveGenerationPreviews     DriveGenerationKind = "previews"
	DriveGenerationFingerprints DriveGenerationKind = "fingerprints"
)

type DriveGenerationResult struct {
	State   string `json:"state"` // started, busy, ready
	Message string `json:"message"`
}

func (a *AdminServer) handleGenerateDrivePreviews(w http.ResponseWriter, r *http.Request) {
	if !a.requirePreviewEnabled(w) {
		return
	}
	a.handleDriveGeneration(w, r, DriveGenerationPreviews)
}

func (a *AdminServer) handleGenerateDriveThumbnails(w http.ResponseWriter, r *http.Request) {
	a.handleDriveGeneration(w, r, DriveGenerationThumbnails)
}

func (a *AdminServer) handleGenerateDriveFingerprints(w http.ResponseWriter, r *http.Request) {
	a.handleDriveGeneration(w, r, DriveGenerationFingerprints)
}

func (a *AdminServer) handleDriveGeneration(w http.ResponseWriter, r *http.Request, kind DriveGenerationKind) {
	if a.OnDriveGenerationRequested == nil {
		httpapi.Error(w, "生成服务不可用", http.StatusServiceUnavailable)
		return
	}
	result, err := a.OnDriveGenerationRequested(r.Context(), chi.URLParam(r, "id"), kind)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, sql.ErrNoRows) {
			status = http.StatusNotFound
		}
		writeErr(w, r, status, err)
		return
	}
	status := http.StatusOK
	if result.State == "started" {
		status = http.StatusAccepted
	}
	writeJSON(w, status, result)
}
