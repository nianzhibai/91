package api

import (
	"encoding/json"
	"errors"
	"net/http"
)

// settingsDTO 是 GET/PUT /admin/api/settings 的入参/出参。
//
// Application configuration, including preview.enabled, is owned by config.yaml.
// This endpoint only retains database-backed UI preferences.
type settingsDTO struct {
	Theme string `json:"theme"`
}

func (a *AdminServer) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	theme := "dark"
	if a.GetTheme != nil {
		if v := a.GetTheme(); v != "" {
			theme = v
		}
	}
	writeJSON(w, http.StatusOK, settingsDTO{
		Theme: theme,
	})
}

func (a *AdminServer) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	if _, ok := raw["builtinTagsEnabled"]; ok {
		writeErr(w, r, http.StatusBadRequest, errors.New("built-in tags have been retired"))
		return
	}

	if v, ok := raw["theme"]; ok && a.SetTheme != nil {
		var theme string
		if err := json.Unmarshal(v, &theme); err != nil {
			writeErr(w, r, http.StatusBadRequest, err)
			return
		}
		if theme != "" {
			if err := a.SetTheme(theme); err != nil {
				writeErr(w, r, http.StatusBadRequest, err)
				return
			}
		}
	}
	// 回显当前值
	resp := settingsDTO{}
	if a.GetTheme != nil {
		resp.Theme = a.GetTheme()
	}
	writeJSON(w, http.StatusOK, resp)
}
