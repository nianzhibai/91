package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/nianzhibai/91/backend/internal/frontend"
	"github.com/nianzhibai/91/backend/internal/httpapi"
)

func frontendFixture(t *testing.T, version string) []byte {
	t.Helper()
	manifest := frontend.Manifest{FormatVersion: 1, Name: "fixture", Version: version, APIMin: "1.0.0", APIMax: "2.0.0"}
	metadata, _ := json.Marshal(manifest)
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for _, file := range []struct{ name, body string }{{"frontend.json", string(metadata)}, {"index.html", `<script src="` + manifest.BaseURL() + `assets/app.js"></script>`}, {"assets/app.js", version}} {
		if err := tw.WriteHeader(&tar.Header{Name: file.name, Mode: 0644, Size: int64(len(file.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(file.body)); err != nil {
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

func TestFrontendCommandsAndHTTPObserveTheSameSelection(t *testing.T) {
	data := t.TempDir()
	builtin := t.TempDir()
	if err := os.WriteFile(filepath.Join(builtin, "index.html"), []byte("bundled frontend"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VIDEO_FRONTEND_DIR", builtin)
	t.Setenv("VIDEO_FRONTEND_MODE", "auto")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configBody := []byte(fmt.Sprintf("storage:\n  data_dir: %q\n", data))
	if err := os.WriteFile(configPath, configBody, 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := frontend.New(data, builtin, httpapi.Version)
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	if err := mountFrontend(router, manager); err != nil {
		t.Fatal(err)
	}
	command := func(args ...string) frontend.Status {
		t.Helper()
		var output bytes.Buffer
		args = append(args, "--config", configPath)
		if err := runFrontendCommand(args, &output, &output); err != nil {
			t.Fatal(err)
		}
		var status frontend.Status
		if err := json.Unmarshal(output.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		return status
	}
	request := func(path string, want int) string {
		t.Helper()
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != want {
			t.Fatalf("%s: status %d, body %s", path, response.Code, response.Body.String())
		}
		return response.Body.String()
	}
	packagePath := filepath.Join(t.TempDir(), "frontend.tar.gz")
	if err := os.WriteFile(packagePath, frontendFixture(t, "1.0.0"), 0644); err != nil {
		t.Fatal(err)
	}
	if status := command("install", "--package", packagePath); status.Active != "fixture@1.0.0" {
		t.Fatalf("status: %+v", status)
	}
	request("/", 200)
	if got := request("/_frontend/fixture/1.0.0/assets/app.js", 200); got != "1.0.0" {
		t.Fatal(got)
	}
	request("/_frontend/fixture/1.0.0/missing", 404)
	request("/api/missing", 404)
	command("reset")
	if got := request("/", 200); got != "bundled frontend" {
		t.Fatal(got)
	}
	request("/_frontend/fixture/1.0.0/assets/app.js", 200)
	command("rollback")
	if got := command("status"); got.Active != "fixture@1.0.0" {
		t.Fatal(got)
	}
	actual, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(actual, configBody) {
		t.Fatal("frontend command modified configuration")
	}
}

func TestFrontendSwitchRevalidatesPageContent(t *testing.T) {
	builtin := t.TempDir()
	if err := os.WriteFile(filepath.Join(builtin, "index.html"), []byte("bundled frontend"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VIDEO_FRONTEND_DIR", builtin)
	t.Setenv("VIDEO_FRONTEND_MODE", "auto")
	manager, err := frontend.New(t.TempDir(), builtin, httpapi.Version)
	if err != nil {
		t.Fatal(err)
	}
	for i, version := range []string{"1.0.0", "1.1.0"} {
		release, err := manager.Install(bytes.NewReader(frontendFixture(t, version)), frontend.InstallOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.Activate(release.ID()); err != nil {
			t.Fatal(err)
		}
		dir, err := manager.ActiveDirectory()
		if err != nil {
			t.Fatal(err)
		}
		modified := time.Date(2026, 1, i+1, 12, 0, 0, 0, time.UTC)
		if err := os.Chtimes(filepath.Join(dir, "index.html"), modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	router := chi.NewRouter()
	if err := mountFrontend(router, manager); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/", "/admin/settings", "/index.html"} {
		t.Run(target, func(t *testing.T) {
			if err := manager.Activate("fixture@1.1.0"); err != nil {
				t.Fatal(err)
			}
			newer := httptest.NewRecorder()
			router.ServeHTTP(newer, httptest.NewRequest(http.MethodGet, target, nil))
			if newer.Code != http.StatusOK || newer.Header().Get("ETag") == "" {
				t.Fatalf("newer frontend: %d %v", newer.Code, newer.Header())
			}
			if err := manager.Activate("fixture@1.0.0"); err != nil {
				t.Fatal(err)
			}
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				for _, useETag := range []bool{false, true} {
					request := httptest.NewRequest(method, target, nil)
					// Also cover browsers with a cached page from before ETags were added.
					request.Header.Set("If-Modified-Since", "Fri, 02 Jan 2026 12:00:00 GMT")
					if useETag {
						request.Header.Set("If-None-Match", newer.Header().Get("ETag"))
					}
					response := httptest.NewRecorder()
					router.ServeHTTP(response, request)
					if response.Code != http.StatusOK || response.Header().Get("ETag") == newer.Header().Get("ETag") {
						t.Fatalf("%s after switch (ETag=%v): %d %v", method, useETag, response.Code, response.Header())
					}
					if method == http.MethodGet && !bytes.Contains(response.Body.Bytes(), []byte("/fixture/1.0.0/")) {
						t.Fatalf("served wrong version: %s", response.Body.String())
					}
				}
			}
			if err := manager.Rollback(); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, target, nil)
			request.Header.Set("If-None-Match", newer.Header().Get("ETag"))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != http.StatusNotModified {
				t.Fatalf("unchanged content after rollback: %d", response.Code)
			}
		})
	}
}

func TestManagedFrontendCanBeInstalledAfterServerStartsWithoutBuiltin(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("VIDEO_FRONTEND_DIR", "")
	// An absent frontend must never serve arbitrary files from the working directory.
	if err := os.WriteFile("index.html", []byte("not a frontend"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"auto", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("VIDEO_FRONTEND_MODE", mode)
			manager, err := frontend.New(t.TempDir(), "", httpapi.Version)
			if err != nil {
				t.Fatal(err)
			}
			router := chi.NewRouter()
			if err := mountFrontend(router, manager); err != nil {
				t.Fatal(err)
			}
			initial := httptest.NewRecorder()
			router.ServeHTTP(initial, httptest.NewRequest(http.MethodGet, "/", nil))
			if initial.Code != http.StatusNotFound {
				t.Fatalf("no frontend installed: %d", initial.Code)
			}
			release, err := manager.Install(bytes.NewReader(frontendFixture(t, "1.0.0")), frontend.InstallOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Activate(release.ID()); err != nil {
				t.Fatal(err)
			}
			for _, target := range []string{"/", "/admin", "/_frontend/fixture/1.0.0/assets/app.js", "/api/missing", "/_frontend/fixture/1.0.0/missing"} {
				response := httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
				want := http.StatusNotFound
				if mode == "auto" && target != "/api/missing" && target != "/_frontend/fixture/1.0.0/missing" {
					want = http.StatusOK
				}
				if response.Code != want {
					t.Errorf("%s: got %d, want %d", target, response.Code, want)
				}
			}
		})
	}
}
