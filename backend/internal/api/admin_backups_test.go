package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/nianzhibai/91/backend/internal/backup"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
)

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (r *flushRecorder) Flush() {
	r.flushed = true
}

func TestWriteRestoreAcceptedCompactsLargeManifestBeforeRestart(t *testing.T) {
	response := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	report := backup.ValidationReport{
		Manifest: backup.Manifest{
			Files: []backup.ManifestFile{
				{Path: "payload/very-large-file-1.mp4", Size: 1},
				{Path: "payload/very-large-file-2.mp4", Size: 2},
			},
		},
		Warnings: make([]string, maxRestoreResponseMessages+1),
	}

	writeRestoreAccepted(response, true, report)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if !response.flushed {
		t.Fatal("accepted restore response was not flushed before restart scheduling")
	}
	if strings.Contains(response.Body.String(), "very-large-file") {
		t.Fatalf("restore response leaked full manifest: %q", response.Body.String())
	}
	var result backup.RestoreResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode accepted restore response: %v", err)
	}
	if len(result.Report.Manifest.Files) != 0 {
		t.Fatalf("manifest files = %d, want 0", len(result.Report.Manifest.Files))
	}
	if len(result.Report.Warnings) != maxRestoreResponseMessages {
		t.Fatalf("warning count = %d, want %d", len(result.Report.Warnings), maxRestoreResponseMessages)
	}
}

func TestBackupDownloadSupportsRangeAndSecurityHeaders(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{
		Storage: config.Storage{
			DataDir:         root,
			DBPath:          filepath.Join(root, "91.db"),
			LocalPreviewDir: filepath.Join(root, "previews"),
		},
	}
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("storage: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Open(cfg.Storage.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	manager, err := backup.NewManager(backup.Config{
		Catalog:    cat,
		AppConfig:  cfg,
		ConfigPath: configPath,
		AppVersion: "v1.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	name := "video-site-91-full-20260730-120000.zip"
	body := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	if err := os.WriteFile(filepath.Join(root, "backups", name), body, 0o600); err != nil {
		t.Fatal(err)
	}
	server := &AdminServer{Backups: manager}
	router := chi.NewRouter()
	router.Get("/admin/api/backups/{id}/download", server.handleDownloadBackup)
	request := httptest.NewRequest(
		http.MethodGet,
		"/admin/api/backups/video-site-91-full-20260730-120000/download",
		nil,
	)
	request.Header.Set("Range", "bytes=5-9")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if response.Body.String() != "56789" {
		t.Fatalf("range body = %q", response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", response.Header().Get("Cache-Control"))
	}
	if response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", response.Header().Get("X-Content-Type-Options"))
	}
	if response.Header().Get("Content-Disposition") != `attachment; filename="`+name+`"` {
		t.Fatalf("Content-Disposition = %q", response.Header().Get("Content-Disposition"))
	}
}

func TestCreateBackupRejectsInvalidSelection(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{
		Storage: config.Storage{
			DataDir:         root,
			DBPath:          filepath.Join(root, "91.db"),
			LocalPreviewDir: filepath.Join(root, "previews"),
		},
	}
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("storage: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Open(cfg.Storage.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	manager, err := backup.NewManager(backup.Config{
		Catalog:    cat,
		AppConfig:  cfg,
		ConfigPath: configPath,
		AppVersion: "v1.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	server := &AdminServer{Backups: manager}
	router := chi.NewRouter()
	router.Post("/admin/api/backups", server.handleCreateBackup)
	for _, body := range []string{
		`{}`,
		`{"userConfig":true}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/admin/api/backups", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()

		router.ServeHTTP(response, request)

		if response.Code != http.StatusBadRequest {
			t.Fatalf("body = %s, status = %d, response = %q", body, response.Code, response.Body.String())
		}
	}
}

func TestBackupRestoreRequiresConfirmationWithoutPassword(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{
		Storage: config.Storage{
			DataDir:         root,
			DBPath:          filepath.Join(root, "91.db"),
			LocalPreviewDir: filepath.Join(root, "previews"),
		},
	}
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("storage: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Open(cfg.Storage.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	manager, err := backup.NewManager(backup.Config{
		Catalog:    cat,
		AppConfig:  cfg,
		ConfigPath: configPath,
		AppVersion: "v1.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	server := &AdminServer{Backups: manager}
	router := chi.NewRouter()
	router.Post("/admin/api/backups/{id}/restore", server.handleRestoreBackup)
	request := httptest.NewRequest(
		http.MethodPost,
		"/admin/api/backups/missing/restore",
		strings.NewReader(`{"confirmation":"确认恢复"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	// Reaching the backup lookup proves confirmation alone passed request
	// validation. The missing demo ID is expected to return 404.
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
}
