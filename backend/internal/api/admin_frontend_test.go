package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nianzhibai/91/backend/internal/config"
	"github.com/nianzhibai/91/backend/internal/frontend"
	"github.com/nianzhibai/91/backend/internal/httpapi"
)

type frontendTransport func(*http.Request) (*http.Response, error)

func (f frontendTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFrontendInstallUsesSavedRepositoryAndActivatesLatest(t *testing.T) {
	manifest := frontend.Manifest{FormatVersion: 1, Name: "test-web", Version: "1.2.0", APIMin: "1.0.0", APIMax: "2.0.0"}
	metadata, _ := json.Marshal(manifest)
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"frontend.json": string(metadata), "index.html": `<script src="` + manifest.BaseURL() + `assets/app.js"></script>`, "assets/app.js": "latest frontend"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	checksum := fmt.Sprintf("%x  %s", sha256.Sum256(archive.Bytes()), frontend.ArchiveName)
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	failDownload := false
	latestTag := "v1.2.0"
	requests := []string{}
	http.DefaultTransport = frontendTransport(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.URL.String())
		var body string
		switch r.URL.String() {
		case "https://api.github.com/repos/community/web/releases/latest":
			body = `{"tag_name":"` + latestTag + `","assets":[{"name":"frontend.tar.gz","browser_download_url":"https://github.com/archive"},{"name":"frontend.tar.gz.sha256","browser_download_url":"https://github.com/checksum"}]}`
		case "https://github.com/checksum":
			body = checksum
		case "https://github.com/archive":
			if failDownload {
				return nil, fmt.Errorf("download failed")
			}
			body = archive.String()
		default:
			t.Errorf("unexpected release request: %s", r.URL)
			body = "{}"
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("frontend: {repository: 'https://github.com/community/web'}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.NewManager(configPath)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := frontend.New(t.TempDir(), "", httpapi.Version)
	if err != nil {
		t.Fatal(err)
	}
	server := &AdminServer{ConfigManager: cfg, Frontend: manager}
	response := httptest.NewRecorder()
	server.handleFrontendInstall(response, httptest.NewRequest(http.MethodPost, "/admin/api/frontend/install", nil))
	if response.Code != http.StatusCreated {
		t.Fatalf("install: %d %s", response.Code, response.Body.String())
	}
	status, err := manager.Status()
	if err != nil || status.Active != "test-web@1.2.0" {
		t.Fatalf("install did not activate: %+v %v", status, err)
	}
	if status.Current == nil || status.Current.Repository != "https://github.com/community/web" {
		t.Fatalf("source missing after install: %+v", status)
	}
	for _, tc := range []struct {
		tag       string
		available bool
	}{{"v1.2.0", false}, {"v1.3.0", true}} {
		latestTag = tc.tag
		before := len(requests)
		response = httptest.NewRecorder()
		server.handleFrontendUpdate(response, httptest.NewRequest(http.MethodGet, "/admin/api/frontend/update", nil))
		var result frontend.UpdateCheck
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.UpdateAvailable != tc.available {
			t.Fatalf("check: %d %s", response.Code, response.Body.String())
		}
		if response.Header().Get("Cache-Control") != "no-store" || len(requests) != before+1 || requests[before] != "https://api.github.com/repos/community/web/releases/latest" {
			t.Fatalf("update check downloaded a package: %v", requests[before:])
		}
	}
	latestTag = "v1.1.0"
	response = httptest.NewRecorder()
	server.handleFrontendInstall(response, httptest.NewRequest(http.MethodPost, "/admin/api/frontend/install", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("downgrade allowed: %d %s", response.Code, response.Body.String())
	}
	latestTag = "v1.3.0"
	failDownload = true
	response = httptest.NewRecorder()
	server.handleFrontendInstall(response, httptest.NewRequest(http.MethodPost, "/admin/api/frontend/install", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("failed download: %d", response.Code)
	}
	status, err = manager.Status()
	if err != nil || status.Active != "test-web@1.2.0" {
		t.Fatalf("failed install changed active frontend: %+v %v", status, err)
	}
}
