package api

import (
	"errors"
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
	request := frontend.ReleaseRequest{Repository: a.ConfigManager.FrontendRepository()}

	release, err := frontend.NewReleaseSource().Install(r.Context(), a.Frontend, request)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, frontend.ErrNoNewerRelease) {
			code = http.StatusConflict
		}
		writeErr(w, r, code, err)
		return
	}
	if err := a.Frontend.Activate(release.Name + "@" + release.Version); err != nil {
		writeErr(w, r, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusCreated, release)
}

func (a *AdminServer) handleFrontendUpdate(w http.ResponseWriter, r *http.Request) {
	if !a.requireFrontend(w) {
		return
	}
	repository := a.ConfigManager.FrontendRepository()
	update, err := frontend.NewReleaseSource().CheckUpdate(r.Context(), a.Frontend, repository)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, frontend.ErrRepositoryNotActive) {
			code = http.StatusConflict
		}
		writeErr(w, r, code, err)
		return
	}
	if a.ConfigManager.FrontendRepository() != repository {
		httpapi.Error(w, "frontend repository changed; reload configuration and check again", http.StatusConflict)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, update)
}
