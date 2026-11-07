package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives/scriptcrawler"
)

func importCrawlerRevision(t *testing.T, server *AdminServer, mode, source string) string {
	t.Helper()
	response := httptest.NewRecorder()
	if mode == "file" {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		file, err := writer.CreateFormFile("file", "91Porn.py")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(file, source)
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/admin/api/crawlers/import-file", &body)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		server.handleImportCrawlerScriptFile(response, request)
	} else {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, source) }))
		defer upstream.Close()
		body, _ := json.Marshal(map[string]string{"url": upstream.URL + "/91Porn.py"})
		server.handleImportCrawlerScriptURL(response, httptest.NewRequest(http.MethodPost, "/admin/api/crawlers/import-url", bytes.NewReader(body)))
	}
	if response.Code != http.StatusOK {
		t.Fatalf("import %s: %d %s", mode, response.Code, response.Body)
	}
	var result struct {
		ScriptPath string `json:"scriptPath"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.ScriptPath == "" {
		t.Fatalf("import response: %s %v", response.Body, err)
	}
	return result.ScriptPath
}

func TestCrawlerScriptReplacementOnlyTakesEffectAfterConfirmation(t *testing.T) {
	for _, mode := range []string{"file", "url"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			catalogPath := filepath.Join(root, "catalog.db")
			cat, err := catalog.Open(catalogPath)
			if err != nil {
				t.Fatal(err)
			}
			defer cat.Close()
			applied := 0
			server := &AdminServer{Catalog: cat, LocalPreviewDir: filepath.Join(root, "previews"), OnDriveRuntimeConfigChanged: func(string) error { applied++; return nil }}
			importDir, err := server.crawlerScriptImportDir()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(importDir, 0o755); err != nil {
				t.Fatal(err)
			}
			original := filepath.Join(importDir, "91Porn.py")
			oldSource := "CRAWLER_NAME = 'Original crawler'\nCRAWLER_PROTOCOL = 'crawler.v3'\n# original behavior\n"
			if err := os.WriteFile(original, []byte(oldSource), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"crawler", "other"} {
				if err := cat.UpsertDrive(context.Background(), &catalog.Drive{ID: id, Kind: scriptcrawler.Kind, Name: "Original crawler", Credentials: map[string]string{"script_file": "91Porn.py", "feed_id": "default"}}); err != nil {
					t.Fatal(err)
				}
			}
			assertOriginal := func(id string) {
				t.Helper()
				drive, err := cat.GetDrive(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				resolved, err := server.crawlerScriptPath(drive.Credentials)
				if err != nil || resolved != original || drive.Name != "Original crawler" {
					t.Fatalf("unconfirmed edit changed crawler: %+v %q %v", drive, resolved, err)
				}
				data, err := os.ReadFile(original)
				if err != nil || string(data) != oldSource {
					t.Fatalf("unconfirmed edit changed original script: %q %v", data, err)
				}
			}
			firstSource := "CRAWLER_NAME = 'First candidate'\nCRAWLER_PROTOCOL = 'crawler.v3'\n# first candidate\n"
			first := importCrawlerRevision(t, server, mode, firstSource)
			// Cancel closes the editor without calling the configuration save.
			assertOriginal("crawler")
			assertOriginal("other")
			newSource := "CRAWLER_NAME = 'Confirmed crawler'\nCRAWLER_PROTOCOL = 'crawler.v3'\n# new behavior\n"
			replacement := importCrawlerRevision(t, server, mode, newSource)
			if replacement == original || replacement == first {
				t.Fatal("same filename reused an existing script revision")
			}
			if data, err := os.ReadFile(first); err != nil || string(data) != firstSource {
				t.Fatalf("a second import overwrote the first editor's candidate: %q %v", data, err)
			}
			save := func(feed string) *httptest.ResponseRecorder {
				t.Helper()
				body, _ := json.Marshal(map[string]string{"id": "crawler", "scriptPath": replacement, "selectedFeedId": feed, "targetNew": "10"})
				response := httptest.NewRecorder()
				server.handleUpsertCrawler(response, httptest.NewRequest(http.MethodPost, "/admin/api/crawlers", bytes.NewReader(body)))
				return response
			}
			if response := save("removed"); response.Code != http.StatusBadRequest {
				t.Fatalf("invalid selection: %d %s", response.Code, response.Body)
			}
			assertOriginal("crawler")
			// Failure after validation must also leave the active revision intact.
			db, err := sql.Open("sqlite", catalogPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`CREATE TRIGGER reject_crawler_save BEFORE UPDATE ON drives WHEN OLD.id = 'crawler' BEGIN SELECT RAISE(ABORT, 'save rejected'); END`); err != nil {
				t.Fatal(err)
			}
			if response := save("default"); response.Code != http.StatusInternalServerError {
				t.Fatalf("failed persistence: %d %s", response.Code, response.Body)
			}
			assertOriginal("crawler")
			if applied != 0 {
				t.Fatal("unconfirmed or failed edits reconfigured the crawler")
			}
			if _, err := db.Exec(`DROP TRIGGER reject_crawler_save`); err != nil {
				t.Fatal(err)
			}
			if response := save("default"); response.Code != http.StatusOK {
				t.Fatalf("confirm replacement: %d %s", response.Code, response.Body)
			}
			drive, err := cat.GetDrive(context.Background(), "crawler")
			if err != nil {
				t.Fatal(err)
			}
			active, err := server.crawlerScriptPath(drive.Credentials)
			if err != nil || active != replacement || drive.Name != "Confirmed crawler" || applied != 1 {
				t.Fatalf("confirmed revision did not become active: %+v %q %v applies=%d", drive, active, err, applied)
			}
			if data, err := os.ReadFile(active); err != nil || string(data) != newSource {
				t.Fatalf("active revision contents: %q %v", data, err)
			}
			assertOriginal("other")
		})
	}
}

func TestConcurrentCrawlerImportsKeepIndependentScriptContents(t *testing.T) {
	server := &AdminServer{LocalPreviewDir: filepath.Join(t.TempDir(), "previews")}
	const count = 16
	paths := make([]string, count)
	sources := make([]string, count)
	var workers sync.WaitGroup
	for i := range paths {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			sources[i] = fmt.Sprintf("CRAWLER_NAME = 'Candidate %d'\nCRAWLER_PROTOCOL = 'crawler.v3'\n", i)
			path, err := server.saveCrawlerScript(context.Background(), "91Porn.py", strings.NewReader(sources[i]), maxCrawlerScriptBytes)
			if err != nil {
				t.Errorf("import %d: %v", i, err)
				return
			}
			paths[i] = path
		}(i)
	}
	workers.Wait()
	seen := make(map[string]bool)
	for i, path := range paths {
		if path == "" || seen[path] {
			t.Fatalf("imports shared a storage identity: %q", path)
		}
		seen[path] = true
		if data, err := os.ReadFile(path); err != nil || string(data) != sources[i] {
			t.Errorf("import %d contents: %q %v", i, data, err)
		}
	}
}
