package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/nianzhibai/91/backend/internal/auth"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/httpapi"
)

type createUserReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

type userDTO struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	Banned    bool   `json:"banned"`
	CreatedAt int64  `json:"createdAt"`
}

func (a *AdminServer) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.Catalog.ListUsers(r.Context())
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	out := make([]userDTO, 0, len(users))
	for _, u := range users {
		out = append(out, userDTO{
			ID: u.ID, Username: u.Username, Role: u.Role,
			Banned: u.Banned, CreatedAt: u.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *AdminServer) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var body createUserReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	username := strings.TrimSpace(body.Username)
	if username == "" {
		httpapi.Error(w, "username is required", http.StatusBadRequest)
		return
	}
	if len(body.Password) < 6 {
		httpapi.Error(w, "password must be at least 6 characters", http.StatusBadRequest)
		return
	}
	role := body.Role
	if role == "" {
		role = "user"
	}
	if role != "admin" && role != "user" {
		httpapi.Error(w, "role must be admin or user", http.StatusBadRequest)
		return
	}

	hashed, err := auth.HashPassword(body.Password)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	id, err := a.Catalog.CreateUser(r.Context(), username, hashed, role)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			httpapi.Error(w, "username already exists", http.StatusConflict)
			return
		}
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "id": id})
}

func (a *AdminServer) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		httpapi.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	target, err := a.Catalog.GetUserByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpapi.Error(w, "user not found", http.StatusNotFound)
			return
		}
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	if currentSessionUserID(r.Context(), a.Catalog, r) == id {
		httpapi.Error(w, "cannot delete yourself", http.StatusBadRequest)
		return
	}
	if target.Role == "admin" {
		admins, err := a.Catalog.CountAdmins(r.Context())
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, err)
			return
		}
		if admins <= 1 {
			httpapi.Error(w, "cannot delete the last admin", http.StatusBadRequest)
			return
		}
	}
	if err := a.Catalog.DeleteUser(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpapi.Error(w, "user not found", http.StatusNotFound)
			return
		}
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	if err := a.Catalog.DeleteSessionsForUser(r.Context(), id); err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *AdminServer) handleBanUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		httpapi.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	target, err := a.Catalog.GetUserByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpapi.Error(w, "user not found", http.StatusNotFound)
			return
		}
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	if currentSessionUserID(r.Context(), a.Catalog, r) == id {
		httpapi.Error(w, "cannot ban yourself", http.StatusBadRequest)
		return
	}
	if target.Role == "admin" && !target.Banned {
		activeAdmins, err := a.Catalog.CountActiveAdmins(r.Context())
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, err)
			return
		}
		if activeAdmins <= 1 {
			httpapi.Error(w, "cannot ban the last active admin", http.StatusBadRequest)
			return
		}
	}
	if err := a.Catalog.SetUserBanned(r.Context(), id, true); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpapi.Error(w, "user not found", http.StatusNotFound)
			return
		}
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	if err := a.Catalog.DeleteSessionsForUser(r.Context(), id); err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *AdminServer) handleUnbanUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		httpapi.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	if err := a.Catalog.SetUserBanned(r.Context(), id, false); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpapi.Error(w, "user not found", http.StatusNotFound)
			return
		}
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *AdminServer) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		httpapi.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	if len(body.Password) < 6 {
		httpapi.Error(w, "password must be at least 6 characters", http.StatusBadRequest)
		return
	}
	hashed, err := auth.HashPassword(body.Password)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	if err := a.Catalog.UpdateUserPassword(r.Context(), id, hashed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpapi.Error(w, "user not found", http.StatusNotFound)
			return
		}
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func currentSessionUserID(ctx context.Context, cat *catalog.Catalog, r *http.Request) int64 {
	if cat == nil || r == nil {
		return 0
	}
	c, err := r.Cookie("vs_admin")
	if err != nil {
		return 0
	}
	ok, userID, err := cat.ValidateSession(ctx, c.Value)
	if err != nil || !ok {
		return 0
	}
	return userID
}

// ---------- IP Ban Management ----------

type bannedIPDTO struct {
	IP        string `json:"ip"`
	Reason    string `json:"reason"`
	CreatedAt int64  `json:"createdAt"`
}

func (a *AdminServer) handleListBannedIPs(w http.ResponseWriter, r *http.Request) {
	ips, err := a.Catalog.ListBannedLoginIPs(r.Context())
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	out := make([]bannedIPDTO, 0, len(ips))
	for _, ip := range ips {
		out = append(out, bannedIPDTO{IP: ip.IP, Reason: ip.Reason, CreatedAt: ip.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *AdminServer) handleUnbanIP(w http.ResponseWriter, r *http.Request) {
	ip := chi.URLParam(r, "ip")
	if err := a.Catalog.UnbanLoginIP(r.Context(), ip); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpapi.Error(w, "IP not found", http.StatusNotFound)
			return
		}
		writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
