package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nianzhibai/91/backend/internal/catalog"
)

func TestCrawlerImportEditMoveAndDeleteUsePortableScriptReference(t *testing.T) {
	root := t.TempDir()
	cat, err := catalog.Open(filepath.Join(root, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	server := &AdminServer{Catalog: cat, LocalPreviewDir: filepath.Join(root, "data", "previews")}
	script, err := server.saveCrawlerScript(context.Background(), "demo.py", strings.NewReader("CRAWLER_NAME = 'Portable crawler'\nCRAWLER_PROTOCOL = 'crawler.v3'\n"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := filepath.Rel(filepath.Join(root, "data", "crawler-scripts"), script)
	if err != nil {
		t.Fatal(err)
	}
	reference = filepath.ToSlash(reference)
	save := func(path string) *catalog.Drive {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"id": "crawler", "scriptPath": path})
		response := httptest.NewRecorder()
		server.handleUpsertCrawler(response, httptest.NewRequest(http.MethodPost, "/admin/api/crawlers", bytes.NewReader(body)))
		if response.Code != http.StatusOK {
			t.Fatalf("save crawler: %d %s", response.Code, response.Body.String())
		}
		drive, err := cat.GetDrive(context.Background(), "crawler")
		if err != nil {
			t.Fatal(err)
		}
		return drive
	}
	for _, path := range []string{script, ""} {
		drive := save(path)
		if drive.Credentials["script_file"] != reference || drive.Credentials["script_path"] != "" {
			t.Fatalf("stored imported script as an absolute path: %+v", drive.Credentials)
		}
	}
	moved := filepath.Join(root, "moved")
	if err := os.Rename(filepath.Join(root, "data"), moved); err != nil {
		t.Fatal(err)
	}
	server.LocalPreviewDir = filepath.Join(moved, "previews")
	drive := save("")
	dto := server.crawlerDTOForDrive(drive, catalog.CrawlerAssetCounts{}, DriveGenerationStatuses{})
	movedScript := filepath.Join(moved, "crawler-scripts", filepath.FromSlash(reference))
	if dto.ScriptPath != movedScript || dto.Name != "Portable crawler" {
		t.Fatalf("crawler after move = %+v", dto)
	}
	external := filepath.Join(root, "external.py")
	if err := os.WriteFile(external, []byte("CRAWLER_NAME = 'External crawler'\nCRAWLER_PROTOCOL = 'crawler.v3'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	drive = save(external)
	if drive.Credentials["script_file"] != "" || drive.Credentials["script_path"] != external {
		t.Fatalf("switch to external retained imported reference: %+v", drive.Credentials)
	}
	drive = save(movedScript)
	if drive.Credentials["script_file"] != reference || drive.Credentials["script_path"] != "" {
		t.Fatalf("switch to imported retained external reference: %+v", drive.Credentials)
	}
	if removed, err := server.removeImportedCrawlerScript(drive); err != nil || !removed {
		t.Fatalf("remove imported script = %v, %v", removed, err)
	}
	if _, err := os.Stat(external); err != nil {
		t.Fatalf("external script was removed: %v", err)
	}
}
