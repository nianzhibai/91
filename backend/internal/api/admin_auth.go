package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/video-site/backend/internal/auth"
	"github.com/video-site/backend/internal/catalog"
)

type updateCheckDTO struct {
	CurrentVersion string `json:"currentVersion"`
	LatestVersion  string `json:"latestVersion"`
	HasUpdate      bool   `json:"hasUpdate"`
	ReleaseURL     string `json:"releaseUrl,omitempty"`
	ReleaseNotes   string `json:"releaseNotes,omitempty"`
	CheckedAt      string `json:"checkedAt"`
}

type githubReleaseDTO struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
}

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type setupReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *AdminServer) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	required, err := a.Catalog.AdminSetupRequired(r.Context())
	if err != nil {
		writeServiceUnavailable(w, r, "check administrator setup", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"required": required})
}

func (a *AdminServer) handleSetup(w http.ResponseWriter, r *http.Request) {
	required, err := a.Catalog.AdminSetupRequired(r.Context())
	if err != nil {
		writeServiceUnavailable(w, r, "check administrator setup", err)
		return
	}
	if !required {
		http.Error(w, "setup already completed", http.StatusConflict)
		return
	}
	if a.Auth == nil {
		http.Error(w, "setup is not available", http.StatusInternalServerError)
		return
	}
	var body setupReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	username := strings.TrimSpace(body.Username)
	password := body.Password
	if username == "" {
		http.Error(w, "username is required", http.StatusBadRequest)
		return
	}
	if len(password) < 6 || len(password) > 72 {
		http.Error(w, "password must be between 6 and 72 bytes", http.StatusBadRequest)
		return
	}
	hashed, err := auth.HashPassword(password)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	if err := a.Catalog.InitializeAdmin(r.Context(), username, hashed); err != nil {
		if errors.Is(err, catalog.ErrAdminAlreadyInitialized) {
			http.Error(w, "setup already completed", http.StatusConflict)
			return
		}
		writeServiceUnavailable(w, r, "initialize administrator", err)
		return
	}
	role, err := a.Auth.UserLogin(w, r, username, password)
	if err != nil {
		if errors.Is(err, auth.ErrLoginIPBanned) {
			http.Error(w, "ip banned", http.StatusForbidden)
			return
		}
		writeServiceUnavailable(w, r, "login after administrator setup", err)
		return
	}
	if role != "admin" {
		http.Error(w, "setup completed but login failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *AdminServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	required, err := a.Catalog.AdminSetupRequired(r.Context())
	if err != nil {
		writeServiceUnavailable(w, r, "check administrator setup", err)
		return
	}
	if required {
		http.Error(w, "setup required", http.StatusPreconditionRequired)
		return
	}
	var body loginReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}

	role, err := a.Auth.UserLogin(w, r, body.Username, body.Password)
	if err != nil {
		if errors.Is(err, auth.ErrLoginIPBanned) {
			http.Error(w, "ip banned", http.StatusForbidden)
			return
		}
		if errors.Is(err, auth.ErrUserBanned) {
			http.Error(w, "user banned", http.StatusForbidden)
			return
		}
		writeServiceUnavailable(w, r, "login", err)
		return
	}
	if role == "" {
		http.Error(w, "invalid credentials", http.StatusBadRequest)
		return
	}
	response := map[string]any{"ok": true, "role": role}
	if role == "admin" && a.ConfigManager != nil {
		response["telegramEnabled"] = a.ConfigManager.LiveSettings().TelegramEnabled
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *AdminServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	a.Auth.Logout(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *AdminServer) handleMe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.Auth == nil {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	ok, userID, err := a.Auth.ValidateRequest(w, r)
	if err != nil {
		writeServiceUnavailable(w, r, "validate current session", err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}

	u, err := a.Catalog.GetUserByID(r.Context(), userID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && u.Banned) {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	if err != nil {
		writeServiceUnavailable(w, r, "load current user", err)
		return
	}
	response := map[string]any{"authenticated": true, "role": u.Role}
	if u.Role == "admin" && a.ConfigManager != nil {
		response["telegramEnabled"] = a.ConfigManager.LiveSettings().TelegramEnabled
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *AdminServer) handleCheckUpdate(w http.ResponseWriter, r *http.Request) {
	info, err := a.checkUpdate(r.Context())
	if err != nil {
		writeErr(w, r, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, info)
}

func (a *AdminServer) checkUpdate(ctx context.Context) (updateCheckDTO, error) {
	current := a.installedVersion()
	if current == "" {
		current = "unknown"
	}
	release, err := a.latestRelease(ctx)
	if err != nil {
		return updateCheckDTO{
			CurrentVersion: current,
			CheckedAt:      time.Now().Format(time.RFC3339),
		}, err
	}
	latest := strings.TrimSpace(release.TagName)
	return updateCheckDTO{
		CurrentVersion: current,
		LatestVersion:  latest,
		HasUpdate:      current != "unknown" && latest != "" && current != latest,
		ReleaseURL:     release.HTMLURL,
		ReleaseNotes:   strings.TrimSpace(release.Body),
		CheckedAt:      time.Now().Format(time.RFC3339),
	}, nil
}

func (a *AdminServer) installedVersion() string {
	if version := strings.TrimSpace(a.ImageVersion); version != "" {
		return version
	}
	path := strings.TrimSpace(a.VersionFilePath)
	if path == "" {
		path = ".version"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[0])
}

func (a *AdminServer) latestRelease(ctx context.Context) (githubReleaseDTO, error) {
	url := strings.TrimSpace(a.ReleaseAPIURL)
	if url == "" {
		repo := strings.TrimSpace(a.GitHubRepo)
		if repo == "" {
			repo = "nianzhibai/91"
		}
		url = "https://api.github.com/repos/" + repo + "/releases/latest"
	}
	client := a.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return githubReleaseDTO{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "video-site-91")
	res, err := client.Do(req)
	if err != nil {
		return githubReleaseDTO{}, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return githubReleaseDTO{}, fmt.Errorf("github release check failed: HTTP %d", res.StatusCode)
	}
	var release githubReleaseDTO
	if err := json.NewDecoder(res.Body).Decode(&release); err != nil {
		return githubReleaseDTO{}, err
	}
	if strings.TrimSpace(release.TagName) == "" {
		return githubReleaseDTO{}, errors.New("github release check returned empty tag")
	}
	return release, nil
}
