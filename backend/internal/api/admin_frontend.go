package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/nianzhibai/91/backend/internal/frontend"
	"github.com/nianzhibai/91/backend/internal/githubrepo"
	"github.com/nianzhibai/91/backend/internal/httpapi"
)

type FrontendList struct {
	Frontends []frontend.Frontend `json:"frontends"`
}

type FrontendUpdates struct {
	Updates []frontend.Update `json:"updates"`
}

type frontendActivateRequest struct {
	ID string `json:"id"`
}

type frontendInstallRequest struct {
	Repository string `json:"repository"`
}

func (a *AdminServer) requireFrontend(w http.ResponseWriter) bool {
	if a.Frontend == nil {
		httpapi.Error(w, "frontend management is unavailable", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// writeFrontends answers every frontend request with the resulting list, so a
// client never has to combine an operation result with a separate reload.
func (a *AdminServer) writeFrontends(w http.ResponseWriter, r *http.Request) {
	frontends, err := a.Frontend.Frontends()
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, FrontendList{Frontends: frontends})
}

func decodeFrontendRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeErr(w, r, http.StatusBadRequest, errors.New("request body must contain one JSON object"))
		return false
	}
	return true
}

func (a *AdminServer) handleFrontendList(w http.ResponseWriter, r *http.Request) {
	if a.requireFrontend(w) {
		a.writeFrontends(w, r)
	}
}

// handleFrontendActivate switches between installed frontends without network access.
func (a *AdminServer) handleFrontendActivate(w http.ResponseWriter, r *http.Request) {
	var body frontendActivateRequest
	if !a.requireFrontend(w) || !decodeFrontendRequest(w, r, &body) {
		return
	}
	if err := a.Frontend.Activate(body.ID); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	a.writeFrontends(w, r)
}

// handleFrontendInstall adds or updates a frontend from its latest GitHub
// release and switches to it.
func (a *AdminServer) handleFrontendInstall(w http.ResponseWriter, r *http.Request) {
	var body frontendInstallRequest
	if !a.requireFrontend(w) || !decodeFrontendRequest(w, r, &body) {
		return
	}
	name, err := githubrepo.Name(body.Repository)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	release, err := frontend.NewReleaseSource().Install(r.Context(), a.Frontend, frontend.ReleaseRequest{Repository: "https://github.com/" + name})
	if err == nil {
		err = a.Frontend.Activate(release.ID())
	}
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	a.writeFrontends(w, r)
}

// handleFrontendUpload installs a locally built frontend.tar.gz and switches to it.
func (a *AdminServer) handleFrontendUpload(w http.ResponseWriter, r *http.Request) {
	if !a.requireFrontend(w) {
		return
	}
	// The manager reports oversized archives; one extra byte lets it see them.
	release, err := a.Frontend.Install(http.MaxBytesReader(w, r.Body, frontend.MaxArchiveBytes+1), frontend.InstallOptions{})
	if err == nil {
		err = a.Frontend.Activate(release.ID())
	}
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	a.writeFrontends(w, r)
}

func (a *AdminServer) handleFrontendUpdates(w http.ResponseWriter, r *http.Request) {
	if !a.requireFrontend(w) {
		return
	}
	updates, err := frontend.NewReleaseSource().Updates(r.Context(), a.Frontend)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, FrontendUpdates{Updates: updates})
}
