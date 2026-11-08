package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestFrontendModesPreserveBackendRoutes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("custom frontend"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VIDEO_FRONTEND_DIR", dir)
	for _, mode := range []string{"auto", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("VIDEO_FRONTEND_MODE", mode)
			router := chi.NewRouter()
			router.Get("/api/server", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("backend")) })
			if err := mountFrontend(router, nil); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"/", "/admin", "/api/server", "/api/missing", "/admin/api/missing", "/p/missing"} {
				res := httptest.NewRecorder()
				router.ServeHTTP(res, httptest.NewRequest("GET", path, nil))
				want := 404
				if path == "/api/server" || mode == "auto" && (path == "/" || path == "/admin") {
					want = 200
				}
				if res.Code != want {
					t.Errorf("%s status=%d want=%d", path, res.Code, want)
				}
				if path == "/admin" && mode == "auto" && res.Body.String() != "custom frontend" {
					t.Fatal("management page was not replaced")
				}
			}
		})
	}
}

func TestInvalidFrontendConfigurationFailsExplicitly(t *testing.T) {
	t.Setenv("VIDEO_FRONTEND_MODE", "auto")
	t.Setenv("VIDEO_FRONTEND_DIR", t.TempDir())
	if err := mountFrontend(chi.NewRouter(), nil); err == nil {
		t.Fatal("missing custom index.html accepted")
	}
	t.Setenv("VIDEO_FRONTEND_MODE", "typo")
	if err := mountFrontend(chi.NewRouter(), nil); err == nil {
		t.Fatal("invalid mode accepted")
	}
}

func TestFrontendHandlerServesStaticAsset(t *testing.T) {
	dir := t.TempDir()
	assets := filepath.Join(dir, "assets")
	if err := os.MkdirAll(assets, 0o755); err != nil {
		t.Fatalf("mkdir assets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>app</html>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(assets, "app.js"), []byte("console.log('ok')"), 0o644); err != nil {
		t.Fatalf("write asset: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	rr := httptest.NewRecorder()
	frontendHandler(dir).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "console.log") {
		t.Fatalf("body = %q, want asset content", rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != frontendHashedAssetCacheControl {
		t.Fatalf("Cache-Control = %q, want %q", got, frontendHashedAssetCacheControl)
	}
}

func TestFrontendHandlerServesUnversionedAssetsWithRevalidation(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"index.html":           "<html>app</html>",
		"manifest.webmanifest": `{"display":"standalone"}`,
		"icon.png":             "icon content",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	for target, content := range map[string]string{
		"/":                     files["index.html"],
		"/admin":                files["index.html"],
		"/index.html":           files["index.html"],
		"/manifest.webmanifest": files["manifest.webmanifest"],
		"/icon.png":             files["icon.png"],
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rr := httptest.NewRecorder()
		frontendHandler(dir).ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", target, rr.Code)
		}
		if rr.Body.String() != content {
			t.Fatalf("%s body = %q, want %q", target, rr.Body.String(), content)
		}
		if got := rr.Header().Get("Cache-Control"); got != frontendUnversionedAssetCacheControl {
			t.Fatalf("%s Cache-Control = %q, want %q", target, got, frontendUnversionedAssetCacheControl)
		}
		if rr.Header().Get("ETag") == "" || rr.Header().Get("Last-Modified") != "" {
			t.Fatalf("%s must use content validation: %v", target, rr.Header())
		}

		conditionalReq := httptest.NewRequest(http.MethodGet, target, nil)
		conditionalReq.Header.Set("If-None-Match", rr.Header().Get("ETag"))
		conditionalRR := httptest.NewRecorder()
		frontendHandler(dir).ServeHTTP(conditionalRR, conditionalReq)
		if conditionalRR.Code != http.StatusNotModified {
			t.Fatalf("%s conditional status = %d, want 304", target, conditionalRR.Code)
		}
		if got := conditionalRR.Header().Get("Cache-Control"); got != frontendUnversionedAssetCacheControl {
			t.Fatalf("%s conditional Cache-Control = %q, want %q", target, got, frontendUnversionedAssetCacheControl)
		}
	}
}

func TestFrontendHandlerDoesNotSwallowBackendRoutes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>app</html>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	for _, target := range []string{
		"/api/missing",
		"/admin/api/missing",
		"/p/missing",
		"/peer/backups/missing",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rr := httptest.NewRecorder()
		frontendHandler(dir).ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", target, rr.Code)
		}
	}
}

func TestResolveFrontendDirFallsBackToParentDist(t *testing.T) {
	workspace := t.TempDir()
	backendDir := filepath.Join(workspace, "backend")
	distDir := filepath.Join(workspace, "dist")
	if err := os.MkdirAll(backendDir, 0o755); err != nil {
		t.Fatalf("mkdir backend: %v", err)
	}
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		t.Fatalf("mkdir dist: %v", err)
	}
	if err := os.WriteFile(filepath.Join(distDir, "index.html"), []byte("<html>app</html>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWD); err != nil {
			t.Fatalf("restore wd: %v", err)
		}
	})
	t.Setenv("VIDEO_FRONTEND_DIR", "")
	if err := os.Chdir(backendDir); err != nil {
		t.Fatalf("chdir backend: %v", err)
	}

	got, ok := resolveFrontendDir()
	if !ok {
		t.Fatal("resolveFrontendDir ok = false, want true")
	}
	if got != "../dist" {
		t.Fatalf("frontend dir = %q, want ../dist", got)
	}
}
