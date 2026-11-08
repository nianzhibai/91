package api

import (
	_ "embed"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/nianzhibai/91/backend/internal/httpapi"
)

//go:embed openapi.json
var openAPIDocument []byte

func (a *AdminServer) registerAuthRoutes(r chi.Router) {
	r.Get("/setup", a.handleSetupStatus)
	r.Post("/setup", a.handleSetup)
	r.Post("/login", a.handleLogin)
	r.Post("/logout", a.handleLogout)
	r.Get("/me", a.handleMe)
}

func (a *AdminServer) handleServerInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"name": "91", "version": a.installedVersion(), "apiVersion": httpapi.Version,
		"auth":      map[string]string{"type": "cookie", "login": "/api/auth/login", "session": "/api/auth/me"},
		"endpoints": map[string]string{"openapi": "/api/openapi.json", "media": "/p/", "admin": "/admin/api"},
	})
}

func handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(openAPIDocument)
}
