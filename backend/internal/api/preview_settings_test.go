package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/nianzhibai/91/backend/internal/auth"
)

func TestPublicPreviewSettingsExposeOnlyLiveGlobalSwitch(t *testing.T) {
	admin, _ := newConfigAPIForTest(t, "preview: {enabled: true}\n")
	server := &Server{GetPreviewEnabled: func() bool {
		return admin.ConfigManager.LiveSettings().PreviewEnabled
	}}
	router := chi.NewRouter()
	server.RegisterRoutes(router, &auth.Authenticator{})
	for _, enabled := range []bool{true, false, true} {
		source := "preview: {enabled: false}\n"
		if enabled {
			source = "preview: {enabled: true}\n"
		}
		if _, err := admin.ConfigManager.ReplaceYAML([]byte(source), ""); err != nil {
			t.Fatal(err)
		}
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/settings/preview", nil))
		if rr.Code != http.StatusOK || rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("settings status=%d headers=%v body=%s", rr.Code, rr.Header(), rr.Body.String())
		}
		var result map[string]bool
		if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result) != 1 || result["previewEnabled"] != enabled {
			t.Fatalf("unexpected public settings: %v", result)
		}
	}
}
