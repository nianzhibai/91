package api

import (
	"encoding/json"
	"net/http"

	"github.com/nianzhibai/91/backend/internal/frontend"
	"github.com/nianzhibai/91/backend/internal/httpapi"
)

func (a *AdminServer) requireFrontend(w http.ResponseWriter) bool {
	if a.Frontend == nil {
		httpapi.Error(w, "frontend management is unavailable", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (a *AdminServer) handleFrontendStatus(w http.ResponseWriter, r *http.Request) {
	if !a.requireFrontend(w) {
		return
	}
	status, err := a.Frontend.Status()
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, status)
}

func (a *AdminServer) handleFrontendInstall(w http.ResponseWriter, r *http.Request) {
	if !a.requireFrontend(w) {
		return
	}
	var request frontend.ReleaseRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&request); err != nil {
		httpapi.Error(w, "invalid frontend release request", http.StatusBadRequest)
		return
	}
	if err := request.Validate(); err != nil {
		httpapi.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	release, err := frontend.NewReleaseSource().Install(r.Context(), a.Frontend, request)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	// Installation never changes the active site. Activation is a separate,
	// explicit action after the administrator has reviewed the installed version.
	writeJSON(w, http.StatusCreated, release)
}

func (a *AdminServer) handleFrontendActivate(w http.ResponseWriter, r *http.Request) {
	if !a.requireFrontend(w) {
		return
	}
	var request struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&request); err != nil || request.ID == "" {
		httpapi.Error(w, "frontend id is required", http.StatusBadRequest)
		return
	}
	if err := a.Frontend.Activate(request.ID); err != nil {
		writeErr(w, r, http.StatusConflict, err)
		return
	}
	a.handleFrontendStatus(w, r)
}

func (a *AdminServer) handleFrontendRollback(w http.ResponseWriter, r *http.Request) {
	if !a.requireFrontend(w) {
		return
	}
	if err := a.Frontend.Rollback(); err != nil {
		writeErr(w, r, http.StatusConflict, err)
		return
	}
	a.handleFrontendStatus(w, r)
}

func (a *AdminServer) handleFrontendReset(w http.ResponseWriter, r *http.Request) {
	if !a.requireFrontend(w) {
		return
	}
	if err := a.Frontend.Activate(""); err != nil {
		writeErr(w, r, http.StatusConflict, err)
		return
	}
	a.handleFrontendStatus(w, r)
}
