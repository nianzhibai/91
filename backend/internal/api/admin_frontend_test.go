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
	"strings"
	"testing"

	"github.com/nianzhibai/91/backend/internal/frontend"
	"github.com/nianzhibai/91/backend/internal/httpapi"
)

type frontendTransport func(*http.Request) (*http.Response, error)

func (f frontendTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func frontendArchive(t *testing.T, name, version string) []byte {
	t.Helper()
	manifest := frontend.Manifest{FormatVersion: 1, Name: name, Version: version, APIMin: "1.0.0", APIMax: "2.0.0"}
	metadata, _ := json.Marshal(manifest)
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for file, body := range map[string]string{"frontend.json": string(metadata), "index.html": `<script src="` + manifest.BaseURL() + `assets/app.js"></script>`, "assets/app.js": name + version} {
		if err := tw.WriteHeader(&tar.Header{Name: file, Mode: 0644, Size: int64(len(body))}); err != nil {
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
	return archive.Bytes()
}

// frontendRequest runs one handler and decodes the frontend list it returns.
func frontendRequest(t *testing.T, handler http.HandlerFunc, method, path string, body io.Reader) (int, []frontend.Frontend, string) {
	t.Helper()
	response := httptest.NewRecorder()
	handler(response, httptest.NewRequest(method, path, body))
	var list FrontendList
	if response.Code == http.StatusOK && path != "/admin/api/frontend/updates" {
		if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s %s: %v %s", method, path, err, response.Body.String())
		}
	}
	return response.Code, list.Frontends, response.Body.String()
}

func activeFrontend(frontends []frontend.Frontend) string {
	for _, item := range frontends {
		if item.Active {
			return item.Name + "@" + item.Version
		}
	}
	return ""
}

func TestFrontendManagementAddsSwitchesUploadsAndUpdates(t *testing.T) {
	versions := map[string][]byte{"1.2.0": frontendArchive(t, "test-web", "1.2.0"), "1.3.0": frontendArchive(t, "test-web", "1.3.0")}
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	latest, failDownload := "1.2.0", false
	requests := []string{}
	http.DefaultTransport = frontendTransport(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.URL.String())
		var body string
		switch r.URL.String() {
		case "https://api.github.com/repos/community/web/releases/latest":
			body = `{"tag_name":"v` + latest + `","assets":[{"name":"frontend.tar.gz","browser_download_url":"https://github.com/archive"},{"name":"frontend.tar.gz.sha256","browser_download_url":"https://github.com/checksum"}]}`
		case "https://github.com/checksum":
			body = fmt.Sprintf("%x  %s", sha256.Sum256(versions[latest]), frontend.ArchiveName)
		case "https://github.com/archive":
			if failDownload {
				return nil, fmt.Errorf("download failed")
			}
			body = string(versions[latest])
		default:
			t.Errorf("unexpected release request: %s", r.URL)
			body = "{}"
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	manager, err := frontend.New(t.TempDir(), "", httpapi.Version)
	if err != nil {
		t.Fatal(err)
	}
	server := &AdminServer{Frontend: manager}

	code, frontends, body := frontendRequest(t, server.handleFrontendInstall, http.MethodPost, "/admin/api/frontend/install", strings.NewReader(`{"repository":"https://github.com/community/web.git"}`))
	if code != http.StatusOK || activeFrontend(frontends) != "test-web@1.2.0" || frontends[0].Repository != "https://github.com/community/web" {
		t.Fatalf("install: %d %s", code, body)
	}
	for _, invalid := range []string{`{"repository":"community/web"}`, `{"repository":"https://github.com/community/web","version":"v1.0.0"}`, `{}`} {
		if code, _, body := frontendRequest(t, server.handleFrontendInstall, http.MethodPost, "/admin/api/frontend/install", strings.NewReader(invalid)); code != http.StatusBadRequest {
			t.Fatalf("accepted %s: %d %s", invalid, code, body)
		}
	}

	code, frontends, body = frontendRequest(t, server.handleFrontendUpload, http.MethodPost, "/admin/api/frontend/upload", bytes.NewReader(frontendArchive(t, "local", "0.1.0")))
	if code != http.StatusOK || activeFrontend(frontends) != "local@0.1.0" || len(frontends) != 2 {
		t.Fatalf("upload: %d %s", code, body)
	}
	if code, _, _ := frontendRequest(t, server.handleFrontendUpload, http.MethodPost, "/admin/api/frontend/upload", strings.NewReader("not an archive")); code != http.StatusBadRequest {
		t.Fatalf("accepted invalid upload: %d", code)
	}

	before := len(requests)
	code, frontends, body = frontendRequest(t, server.handleFrontendActivate, http.MethodPost, "/admin/api/frontend/activate", strings.NewReader(`{"id":"test-web@1.2.0"}`))
	if code != http.StatusOK || activeFrontend(frontends) != "test-web@1.2.0" || len(requests) != before {
		t.Fatalf("offline switch: %d %s %v", code, body, requests[before:])
	}
	if code, _, _ := frontendRequest(t, server.handleFrontendActivate, http.MethodPost, "/admin/api/frontend/activate", strings.NewReader(`{"id":"missing@1.0.0"}`)); code != http.StatusBadRequest {
		t.Fatalf("activated a missing frontend: %d", code)
	}

	latest = "1.3.0"
	before = len(requests)
	response := httptest.NewRecorder()
	server.handleFrontendUpdates(response, httptest.NewRequest(http.MethodGet, "/admin/api/frontend/updates", nil))
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"updates":[{"name":"test-web","version":"1.3.0"}]}` || len(requests) != before+1 {
		t.Fatalf("updates: %d %s %v", response.Code, response.Body.String(), requests[before:])
	}

	failDownload = true
	if code, _, _ := frontendRequest(t, server.handleFrontendInstall, http.MethodPost, "/admin/api/frontend/install", strings.NewReader(`{"repository":"https://github.com/community/web"}`)); code != http.StatusBadRequest {
		t.Fatalf("failed download: %d", code)
	}
	if code, frontends, _ = frontendRequest(t, server.handleFrontendList, http.MethodGet, "/admin/api/frontend", nil); code != http.StatusOK || activeFrontend(frontends) != "test-web@1.2.0" {
		t.Fatalf("failed update changed the active frontend: %+v", frontends)
	}
	failDownload = false
	if code, frontends, body = frontendRequest(t, server.handleFrontendInstall, http.MethodPost, "/admin/api/frontend/install", strings.NewReader(`{"repository":"https://github.com/community/web"}`)); code != http.StatusOK || activeFrontend(frontends) != "test-web@1.3.0" {
		t.Fatalf("update: %d %s", code, body)
	}
}
