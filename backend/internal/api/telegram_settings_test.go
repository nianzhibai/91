package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/video-site/backend/internal/auth"
	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/config"
	"github.com/video-site/backend/internal/telegram"
)

func TestAuthenticationInitializesTelegramAvailabilityForAdmins(t *testing.T) {
	hash, err := auth.HashPassword("secret123")
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range []struct {
		name    string
		yaml    string
		enabled bool
	}{
		{"enabled", "telegram: {enabled: true, bot_token: '123:private_token'}\n", true},
		{"disabled", "telegram: {enabled: false}\n", false},
	} {
		for _, role := range []string{"admin", "user"} {
			t.Run(setting.name+"/"+role, func(t *testing.T) {
				server, _ := newConfigAPIForTest(t, setting.yaml)
				cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = cat.Close() })
				server.Catalog = cat
				server.Auth = &auth.Authenticator{Catalog: cat}
				if err := cat.InitializeAdmin(context.Background(), "owner", hash); err != nil {
					t.Fatal(err)
				}
				username := "owner"
				if role == "user" {
					username = "viewer"
					if _, err := cat.CreateUser(context.Background(), username, hash, role); err != nil {
						t.Fatal(err)
					}
				}
				login := httptest.NewRecorder()
				server.handleLogin(login, httptest.NewRequest(http.MethodPost, "/admin/api/login",
					strings.NewReader(`{"username":"`+username+`","password":"secret123"}`)))
				if login.Code != http.StatusOK {
					t.Fatalf("login = %d %s", login.Code, login.Body.String())
				}
				request := httptest.NewRequest(http.MethodGet, "/admin/api/me", nil)
				for _, cookie := range login.Result().Cookies() {
					request.AddCookie(cookie)
				}
				me := httptest.NewRecorder()
				server.handleMe(me, request)
				guest := httptest.NewRecorder()
				server.handleMe(guest, httptest.NewRequest(http.MethodGet, "/admin/api/me", nil))
				for name, response := range map[string]*httptest.ResponseRecorder{"login": login, "me": me, "guest": guest} {
					if response.Code != http.StatusOK {
						t.Fatalf("%s = %d %s", name, response.Code, response.Body.String())
					}
					var body map[string]any
					if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
						t.Fatal(err)
					}
					if name == "guest" || role != "admin" {
						if _, present := body["telegramEnabled"]; present {
							t.Fatalf("%s exposes admin configuration", name)
						}
					} else if body["telegramEnabled"] != setting.enabled {
						t.Fatalf("%s availability = %v, want %v", name, body["telegramEnabled"], setting.enabled)
					}
					if strings.Contains(response.Body.String(), "private_token") {
						t.Fatalf("%s exposes Telegram credentials", name)
					}
				}
			})
		}
	}
}

func TestTelegramAvailabilityReadsSavedConfigWithoutRuntimeService(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewManager(path)
	if err != nil {
		t.Fatal(err)
	}
	server := &AdminServer{ConfigManager: manager}
	for _, enabled := range []bool{true, false} {
		body := "telegram: {enabled: false}\n"
		if enabled {
			body = "telegram: {enabled: true, bot_token: '123:private_token'}\n"
		}
		if _, err := manager.ReplaceYAML([]byte(body), ""); err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		server.handleTelegramAvailability(response, httptest.NewRequest(http.MethodGet, "/admin/api/telegram/availability", nil))
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("response = %d, headers=%v", response.Code, response.Header())
		}
		var availability map[string]bool
		if err := json.Unmarshal(response.Body.Bytes(), &availability); err != nil {
			t.Fatal(err)
		}
		if len(availability) != 1 || availability["enabled"] != enabled {
			t.Fatalf("availability = %v, want enabled=%v only", availability, enabled)
		}
	}
}

func TestTelegramSettingsUseSharedYAMLAPI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewManager(path)
	if err != nil {
		t.Fatal(err)
	}
	cat := openRemoteUploadAPICatalog(t)
	integration := telegram.NewIntegration(cat, manager, t.TempDir(), 1)
	server := &AdminServer{Catalog: cat, ConfigManager: manager, Telegram: integration}
	token := "123:private_token"
	body := "telegram:\n  enabled: true\n  bot_token: " + token + "\n"
	response := httptest.NewRecorder()
	server.handlePutConfigYAML(response, httptest.NewRequest("PUT", "/admin/api/config.yaml", strings.NewReader(body)))
	if response.Code != 200 || strings.Contains(response.Body.String(), `"restartRequired":true`) {
		t.Fatal("save failed", response.Code)
	}
	var saved config.SaveResult
	if err := json.Unmarshal(response.Body.Bytes(), &saved); err != nil || !saved.Settings.TelegramEnabled {
		t.Fatal("save response must expose the persisted enable flag", err)
	}
	response = httptest.NewRecorder()
	server.handleGetConfigYAML(response, httptest.NewRequest("GET", "/admin/api/config.yaml", nil))
	if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" || response.Body.String() != body {
		t.Fatal("YAML did not return saved values")
	}
	response = httptest.NewRecorder()
	server.handleTelegramStatus(response, httptest.NewRequest("GET", "/admin/api/telegram/status", nil))
	var status struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil || !status.Enabled {
		t.Fatal("navigation must follow saved config even before the integration connects", err)
	}
	for _, hidden := range []string{token, "apiFilesRoot", "localFilesRoot"} {
		if strings.Contains(response.Body.String(), hidden) {
			t.Fatal("status exposes credentials or storage paths")
		}
	}
	response = httptest.NewRecorder()
	server.handlePutConfigYAML(response, httptest.NewRequest("PUT", "/admin/api/config.yaml", strings.NewReader("telegram: { enabled: false }\n")))
	if response.Code != 200 {
		t.Fatal("disable failed", response.Code)
	}
	response = httptest.NewRecorder()
	server.handleTelegramStatus(response, httptest.NewRequest("GET", "/admin/api/telegram/status", nil))
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil || status.Enabled {
		t.Fatal("disabled Telegram must hide navigation", err)
	}
}
