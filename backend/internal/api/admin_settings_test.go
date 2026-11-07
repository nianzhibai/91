package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nianzhibai/91/backend/internal/catalog"
)

func TestGetSettingsReturnsDatabasePreferencesOnly(t *testing.T) {
	server := &AdminServer{GetTheme: func() string { return "light" }}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin/api/settings", nil)
	server.handleGetSettings(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", recorder.Header().Get("Cache-Control"))
	}
	var response settingsDTO
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Theme != "light" {
		t.Fatalf("theme = %q, want light", response.Theme)
	}
	for _, configField := range []string{"nightlyDisabled", "nightlyStartTime", "builtinTagsEnabled", "autoGenerateTagsEnabled"} {
		if strings.Contains(recorder.Body.String(), configField) {
			t.Fatalf("config.yaml field %q leaked into settings endpoint: %s", configField, recorder.Body.String())
		}
	}
}

func TestPutSettingsRejectsBuiltinTagConfigOutsideConfigYAML(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	tagChanges := 0
	server := &AdminServer{
		Catalog:       cat,
		OnTagsChanged: func() { tagChanges++ },
	}
	for _, body := range []string{
		`{"builtinTagsEnabled":false}`,
		`{"builtinTagsEnabled":null}`,
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPut, "/admin/api/settings", strings.NewReader(body))
		server.handlePutSettings(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, response=%s; want 400", body, recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "retired") {
			t.Fatalf("body %s: response does not identify retired tag configuration: %s", body, recorder.Body.String())
		}
	}

	if tagChanges != 0 {
		t.Fatalf("tag cache invalidations = %d, want 0", tagChanges)
	}
}

func TestPutSettingsStillSupportsPartialThemeUpdate(t *testing.T) {
	theme := "dark"
	server := &AdminServer{
		GetTheme: func() string { return theme },
		SetTheme: func(next string) error {
			theme = next
			return nil
		},
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPut,
		"/admin/api/settings",
		strings.NewReader(`{"theme":"light"}`),
	)
	server.handlePutSettings(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if theme != "light" {
		t.Fatalf("theme = %q, want light", theme)
	}
}

func TestPutSettingsRejectsUnsupportedThemes(t *testing.T) {
	for _, value := range []string{`"pink"`, `"sky"`, `"orange"`, `"auto"`, `""`, `null`, `42`} {
		t.Run(value, func(t *testing.T) {
			called := false
			server := &AdminServer{SetTheme: func(string) error { called = true; return nil }}
			recorder := httptest.NewRecorder()
			server.handlePutSettings(recorder, httptest.NewRequest(
				http.MethodPut, "/admin/api/settings", strings.NewReader(`{"theme":`+value+`}`),
			))
			if recorder.Code != http.StatusBadRequest || called {
				t.Fatalf("unsupported theme reached persistence: status=%d called=%v", recorder.Code, called)
			}
		})
	}
}

func TestPublicThemeReturnsOnlyDarkOrLight(t *testing.T) {
	for _, value := range []string{"dark", "light", "invalid", ""} {
		t.Run(value, func(t *testing.T) {
			server := &Server{GetTheme: func() string { return value }}
			recorder := httptest.NewRecorder()
			server.handleGetTheme(recorder, httptest.NewRequest(http.MethodGet, "/api/settings/theme", nil))
			var response settingsDTO
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			want := "dark"
			if value == "light" {
				want = "light"
			}
			if recorder.Code != http.StatusOK || response.Theme != want {
				t.Fatalf("theme=%q status=%d, want %q", response.Theme, recorder.Code, want)
			}
		})
	}
}
