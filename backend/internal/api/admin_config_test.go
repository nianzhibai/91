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

	"github.com/video-site/backend/internal/config"
)

func newConfigAPIForTest(t *testing.T, source string) (*AdminServer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatalf("write config: %v", err)
	}
	manager, err := config.NewManager(path)
	if err != nil {
		t.Fatalf("new config manager: %v", err)
	}
	return &AdminServer{ConfigManager: manager}, path
}

func TestConfigYAMLGetReturnsRealFileAndETag(t *testing.T) {
	server, _ := newConfigAPIForTest(t, "# keep\nnightly:\n  start_time: \"01:00\"\n")
	recorder := httptest.NewRecorder()
	server.handleGetConfigYAML(recorder, httptest.NewRequest(http.MethodGet, "/admin/api/config.yaml", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "# keep") {
		t.Fatalf("response is not the source file: %s", recorder.Body.String())
	}
	if recorder.Header().Get("ETag") == "" {
		t.Fatal("missing config version ETag")
	}
}

func TestConfigYAMLPutValidatesPersistsAndPublishes(t *testing.T) {
	server, path := newConfigAPIForTest(t, "# keep\nnightly:\n  start_time: \"01:00\"\ntags:\n  builtin_pack_enabled: true\nfuture:\n  value: keep\n")
	get := httptest.NewRecorder()
	server.handleGetConfigYAML(get, httptest.NewRequest(http.MethodGet, "/admin/api/config.yaml", nil))

	candidate := "# keep\nnightly:\n  disabled: true\n  start_time: \"00:45\"\n  timezone: Asia/Shanghai\ntags:\n  builtin_pack_enabled: false\ngeneration:\n  thumbnail_concurrency: 2\n  preview_concurrency: 3\n  fingerprint_concurrency: 4\nfuture:\n  value: keep\n"
	request := httptest.NewRequest(http.MethodPut, "/admin/api/config.yaml", strings.NewReader(candidate))
	request.Header.Set("If-Match", get.Header().Get("ETag"))
	recorder := httptest.NewRecorder()
	server.handlePutConfigYAML(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response config.SaveResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.RestartRequired {
		t.Fatal("live-only change should not require restart")
	}
	if response.Settings.NightlyStartTime != "00:45" {
		t.Fatalf("published settings = %#v", response.Settings)
	}
	if response.Settings.NightlyTimezone != "Asia/Shanghai" {
		t.Fatalf("published settings = %#v", response.Settings)
	}
	if !response.Settings.NightlyDisabled {
		t.Fatalf("published settings = %#v, want nightly disabled", response.Settings)
	}
	if response.Settings.PreviewConcurrency != 3 || response.Settings.ThumbnailConcurrency != 2 || response.Settings.FingerprintConcurrency != 4 {
		t.Fatalf("published settings = %#v, want preview concurrency 3", response.Settings)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != candidate {
		t.Fatalf("written config differs:\n%s", written)
	}
}

func TestConfigYAMLPutRejectsStaleAndInvalidWrites(t *testing.T) {
	server, path := newConfigAPIForTest(t, "nightly:\n  start_time: \"01:00\"\n")
	original, _ := os.ReadFile(path)

	staleRequest := httptest.NewRequest(http.MethodPut, "/admin/api/config.yaml", strings.NewReader("nightly:\n  start_time: \"02:00\"\n"))
	staleRequest.Header.Set("If-Match", `"stale"`)
	stale := httptest.NewRecorder()
	server.handlePutConfigYAML(stale, staleRequest)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale status = %d, body=%s", stale.Code, stale.Body.String())
	}

	invalid := httptest.NewRecorder()
	server.handlePutConfigYAML(invalid, httptest.NewRequest(http.MethodPut, "/admin/api/config.yaml", strings.NewReader("nightly:\n  start_time: \"25:00\"\n")))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid status = %d, body=%s", invalid.Code, invalid.Body.String())
	}
	written, _ := os.ReadFile(path)
	if string(written) != string(original) {
		t.Fatalf("rejected writes changed config:\n%s", written)
	}
}

func TestConfigYAMLPutRejectsAdministratorCredentials(t *testing.T) {
	server, path := newConfigAPIForTest(t, "server:\n  listen: ':8080'\n")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	server.handlePutConfigYAML(res, httptest.NewRequest(http.MethodPut, "/admin/api/config.yaml", strings.NewReader("server:\n  admin:\n    username: owner\n    password: secret-password\n")))
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "server.admin") {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "secret-password") {
		t.Fatal("response exposed submitted password")
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != string(original) {
		t.Fatal("rejected administrator config was persisted")
	}
}

func TestConfigYAMLPutReportsRestartForNonLiveFields(t *testing.T) {
	server, _ := newConfigAPIForTest(t, "server:\n  listen: \":8080\"\nnightly:\n  start_time: \"01:00\"\n")
	request := httptest.NewRequest(http.MethodPut, "/admin/api/config.yaml", strings.NewReader("server:\n  listen: \":9090\"\nnightly:\n  start_time: \"01:00\"\n"))
	recorder := httptest.NewRecorder()
	server.handlePutConfigYAML(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response config.SaveResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.RestartRequired {
		t.Fatal("server.listen change should require restart")
	}
}

func TestGlobalPreviewSwitchControlsRegenerationEndpoints(t *testing.T) {
	server, _ := newConfigAPIForTest(t, "preview: {enabled: false}\n")
	calls := 0
	server.OnRegenPreview = func(string) { calls++ }
	server.OnRegenAllPreviews = func() { calls++ }
	server.OnDriveGenerationRequested = func(context.Context, string, DriveGenerationKind) (DriveGenerationResult, error) {
		calls++
		return DriveGenerationResult{State: "started", Message: "started"}, nil
	}
	handlers := []http.HandlerFunc{
		server.handleRegenPreview,
		server.handleRegenAllPreviews,
		server.handleGenerateDrivePreviews,
	}
	for _, handler := range handlers {
		rr := httptest.NewRecorder()
		handler(rr, httptest.NewRequest(http.MethodPost, "/", nil))
		if rr.Code != http.StatusConflict {
			t.Fatalf("disabled regeneration returned %d: %s", rr.Code, rr.Body.String())
		}
	}
	if calls != 0 {
		t.Fatal("disabled switch triggered generation")
	}
	rr := httptest.NewRecorder()
	server.handlePutConfigYAML(rr, httptest.NewRequest(http.MethodPut, "/admin/api/config.yaml", strings.NewReader("preview: {enabled: true}\n")))
	var result config.SaveResult
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil || rr.Code != http.StatusOK || result.RestartRequired || !result.Settings.PreviewEnabled {
		t.Fatalf("enable failed: status=%d, body=%s, err=%v", rr.Code, rr.Body.String(), err)
	}
	for _, handler := range handlers {
		rr := httptest.NewRecorder()
		handler(rr, httptest.NewRequest(http.MethodPost, "/", nil))
		if rr.Code != http.StatusAccepted {
			t.Fatalf("enabled regeneration returned %d", rr.Code)
		}
	}
	if calls != len(handlers) {
		t.Fatalf("generation callbacks = %d, want %d", calls, len(handlers))
	}
}
