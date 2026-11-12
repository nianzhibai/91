package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/nianzhibai/91/backend/internal/auth"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/httpapi"
)

func TestOpenAPICoversClientRoutes(t *testing.T) {
	var document struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Version string `json:"version"`
		} `json:"info"`
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(openAPIDocument, &document); err != nil {
		t.Fatal(err)
	}
	if document.OpenAPI != "3.1.0" || document.Info.Version != httpapi.Version {
		t.Fatal("contract version mismatch")
	}
	router := chi.NewRouter()
	authenticator := &auth.Authenticator{}
	server := &Server{}
	server.RegisterRoutes(router, authenticator)
	(&AdminServer{Auth: authenticator}).Register(router, server.RegisterAdminRoutes)
	actual := map[string]bool{}
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasPrefix(route, "/peer/") {
			return nil
		}
		if strings.HasSuffix(route, "*") {
			route = strings.TrimSuffix(route, "*") + "{filePath}"
		}
		method = strings.ToLower(method)
		actual[method+" "+route] = true
		if _, ok := document.Paths[route][method]; !ok {
			t.Errorf("undocumented route: %s %s", method, route)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for route, operations := range document.Paths {
		for method := range operations {
			if !actual[method+" "+route] {
				t.Errorf("contract has no implementation: %s %s", method, route)
			}
		}
	}
}

func TestIndependentClientLifecycle(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	uploads := t.TempDir()
	if err := os.WriteFile(filepath.Join(uploads, "demo.mp4"), []byte("0123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID: "demo", DriveID: localUploadDriveID, FileID: "demo.mp4", FileName: "demo.mp4", Title: "Demo",
		Ext: "mp4", DurationSeconds: 754, Size: 10, PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	authenticator := &auth.Authenticator{Catalog: cat}
	currentTheme := "dark"
	router := chi.NewRouter()
	library := &Server{Catalog: cat, UploadDir: uploads}
	library.RegisterRoutes(router, authenticator)
	(&AdminServer{Catalog: cat, Auth: authenticator, ImageVersion: "test",
		GetTheme: func() string { return currentTheme },
		SetTheme: func(value string) error { currentTheme = value; return nil },
	}).Register(router, library.RegisterAdminRoutes)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	request := func(method, path, body string, status int) []byte {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		if response.StatusCode != status {
			t.Fatalf("%s %s: %d %s", method, path, response.StatusCode, data)
		}
		return data
	}
	request("GET", "/api/server", "", 200)
	request("GET", "/api/openapi.json", "", 200)
	request("GET", "/api/list", "", 401)
	request("GET", "/p/upload/demo", "", 401)
	request("POST", "/api/auth/setup", `{"username":"owner","password":"secret123"}`, 200)
	request("POST", "/api/auth/setup", `{"username":"other","password":"secret123"}`, 409)
	request("GET", "/api/auth/me", "", 200)
	request("GET", "/api/list?page=1&size=12", "", 200)
	var detail VideoDetailDTO
	if err := json.Unmarshal(request("GET", "/api/video/demo", "", 200), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.DurationSeconds != 754 || detail.PlaybackURL == "" {
		t.Fatalf("detail: %+v", detail)
	}
	var manifest PlaybackManifestDTO
	if err := json.Unmarshal(request("GET", detail.PlaybackURL, "", 200), &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Sources) != 1 || manifest.Sources[0].URL != "/p/upload/demo" {
		t.Fatalf("manifest: %+v", manifest)
	}
	if string(request("GET", manifest.Sources[0].URL, "", 200)) != "0123456789" {
		t.Fatal("media body mismatch")
	}
	request("PUT", "/admin/api/settings", `{"theme":"light"}`, 200)
	if currentTheme != "light" {
		t.Fatal("management mutation did not reach the backend")
	}
	var share createVideoShareResponse
	if err := json.Unmarshal(request("POST", "/api/video/demo/share", "", 201), &share); err != nil {
		t.Fatal(err)
	}
	if share.Token == "" || !strings.HasSuffix(share.URL, "#"+share.Token) {
		t.Fatal("missing frontend-independent share token")
	}
	request("POST", "/api/auth/logout", "", 200)
	request("PUT", "/admin/api/settings", `{"theme":"dark"}`, 401)
	request("GET", "/p/upload/demo", "", 401)
	request("POST", "/api/auth/login", `{"username":"owner","password":"secret123"}`, 200)
	request("POST", "/admin/api/users", `{"username":"viewer","password":"viewer123","role":"user"}`, 201)
	request("POST", "/api/auth/logout", "", 200)
	request("POST", "/api/auth/login", `{"username":"viewer","password":"viewer123"}`, 200)
	request("GET", "/api/list", "", 200)
	data := request("PUT", "/admin/api/settings", `{"theme":"dark"}`, 403)
	var failure map[string]string
	if err := json.Unmarshal(data, &failure); err != nil || failure["code"] != "forbidden" {
		t.Fatalf("error contract: %s", data)
	}
	if currentTheme != "light" {
		t.Fatal("ordinary user changed management settings")
	}
}

func TestEveryDocumentedManagementOperationRejectsRegularUsers(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	userID, err := cat.CreateUser(context.Background(), "viewer", "unused", "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.CreateSession(context.Background(), "viewer-session", time.Hour, userID); err != nil {
		t.Fatal(err)
	}
	authenticator := &auth.Authenticator{Catalog: cat}
	router := chi.NewRouter()
	server := &Server{Catalog: cat}
	server.RegisterRoutes(router, authenticator)
	(&AdminServer{Catalog: cat, Auth: authenticator}).Register(router, server.RegisterAdminRoutes)
	var document struct {
		Paths map[string]map[string]struct {
			Role string `json:"x-role"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(openAPIDocument, &document); err != nil {
		t.Fatal(err)
	}
	for path, operations := range document.Paths {
		for method, operation := range operations {
			if operation.Role != "admin" {
				continue
			}
			t.Run(method+" "+path, func(t *testing.T) {
				requestPath := path
				for strings.Contains(requestPath, "{") {
					start := strings.Index(requestPath, "{")
					end := strings.Index(requestPath, "}")
					requestPath = requestPath[:start] + "test" + requestPath[end+1:]
				}
				for _, loggedIn := range []bool{false, true} {
					req := httptest.NewRequest(strings.ToUpper(method), requestPath, strings.NewReader(`{}`))
					want := http.StatusUnauthorized
					if loggedIn {
						req.AddCookie(&http.Cookie{Name: "vs_admin", Value: "viewer-session"})
						want = http.StatusForbidden
					}
					res := httptest.NewRecorder()
					router.ServeHTTP(res, req)
					if res.Code != want {
						t.Fatalf("status=%d want=%d body=%s", res.Code, want, res.Body.String())
					}
				}
			})
		}
	}
}
