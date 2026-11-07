package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives/scriptcrawler"
)

const feedScriptHeader = `CRAWLER_NAME = "Feed crawler"
CRAWLER_PROTOCOL = "crawler.v3"
CRAWLER_FEEDS = '[{"id":"latest","label":"最新","default":true},{"id":"hot","label":"最热"}]'
`

func TestCrawlerSaveAndListFeedSelection(t *testing.T) {
	root := t.TempDir()
	cat, err := catalog.Open(filepath.Join(root, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	server := &AdminServer{Catalog: cat, LocalPreviewDir: filepath.Join(root, "previews")}
	script := filepath.Join(root, "crawler.py")
	if err := os.WriteFile(script, []byte(feedScriptHeader), 0o600); err != nil {
		t.Fatal(err)
	}
	save := func(feed *string, status int) {
		t.Helper()
		body := map[string]any{"id": "crawler", "scriptPath": script, "targetNew": "10"}
		if feed != nil {
			body["selectedFeedId"] = *feed
		}
		data, _ := json.Marshal(body)
		rr := httptest.NewRecorder()
		server.handleUpsertCrawler(rr, httptest.NewRequest(http.MethodPost, "/admin/api/crawlers", bytes.NewReader(data)))
		if rr.Code != status {
			t.Fatalf("save=%d want=%d body=%s", rr.Code, status, rr.Body)
		}
	}
	list := func() crawlerDTO {
		t.Helper()
		rr := httptest.NewRecorder()
		server.handleListCrawlers(rr, httptest.NewRequest(http.MethodGet, "/admin/api/crawlers", nil))
		var crawlers []crawlerDTO
		if err := json.Unmarshal(rr.Body.Bytes(), &crawlers); err != nil || len(crawlers) != 1 {
			t.Fatalf("list=%s err=%v", rr.Body, err)
		}
		return crawlers[0]
	}
	save(nil, http.StatusOK)
	if dto := list(); dto.SelectedFeedID != "latest" || len(dto.Feeds) != 2 || dto.ScriptError != "" {
		t.Fatalf("default selection=%+v", dto)
	}
	hot := "hot"
	save(&hot, http.StatusOK)
	save(nil, http.StatusOK)
	if dto := list(); dto.SelectedFeedID != "hot" {
		t.Fatalf("selection not preserved=%+v", dto)
	}
	for _, invalid := range []string{"", "removed"} {
		save(&invalid, http.StatusBadRequest)
	}
	if err := os.WriteFile(script, []byte(strings.Replace(feedScriptHeader, `,{"id":"hot","label":"最热"}`, "", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if dto := list(); dto.SelectedFeedID != "hot" || !strings.Contains(dto.ScriptError, "不支持抓取栏目") || len(dto.Feeds) != 1 {
		t.Fatalf("removed selection not exposed=%+v", dto)
	}
	save(nil, http.StatusBadRequest)
	drive, err := cat.GetDrive(context.Background(), "crawler")
	if err != nil || drive.Credentials["feed_id"] != "hot" {
		t.Fatalf("failed save changed selection: %+v %v", drive, err)
	}
	latest := "latest"
	save(&latest, http.StatusOK)
	if dto := list(); dto.SelectedFeedID != "latest" || dto.ScriptError != "" {
		t.Fatalf("reselected feed=%+v", dto)
	}
}

func TestCrawlerImportsExposeFeedsWithoutExecutingScript(t *testing.T) {
	for _, mode := range []string{"file", "url"} {
		t.Run(mode, func(t *testing.T) {
			server := &AdminServer{LocalPreviewDir: filepath.Join(t.TempDir(), "previews")}
			source := feedScriptHeader + "raise RuntimeError('metadata must not execute')\n"
			rr := httptest.NewRecorder()
			if mode == "file" {
				var body bytes.Buffer
				writer := multipart.NewWriter(&body)
				file, err := writer.CreateFormFile("file", "crawler.py")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.Write([]byte(source)); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, "/admin/api/crawlers/import-file", &body)
				request.Header.Set("Content-Type", writer.FormDataContentType())
				server.handleImportCrawlerScriptFile(rr, request)
			} else {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, source) }))
				defer upstream.Close()
				body, _ := json.Marshal(map[string]string{"url": upstream.URL + "/crawler.py"})
				server.handleImportCrawlerScriptURL(rr, httptest.NewRequest(http.MethodPost, "/admin/api/crawlers/import-url", bytes.NewReader(body)))
			}
			var imported struct {
				Feeds []scriptcrawler.Feed `json:"feeds"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &imported); err != nil || rr.Code != http.StatusOK || len(imported.Feeds) != 2 || imported.Feeds[0].ID != "latest" || !imported.Feeds[0].Default {
				t.Fatalf("import=%d %s err=%v", rr.Code, rr.Body, err)
			}
		})
	}
}

func TestCrawlerTestEndpointUsesSelectedFeed(t *testing.T) {
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusPartialContent)
		fmt.Fprint(w, "v")
	}))
	defer media.Close()
	source := feedScriptHeader + fmt.Sprintf(`import json, sys
job = json.load(open(sys.argv[sys.argv.index("--job") + 1]))
for line in sys.stdin:
    command = json.loads(line)
    response = dict(request_id=command["request_id"])
    if command["type"] == "discover":
        response.update(type="page",items=[dict(discovery_key="one",locator={})],next_cursor=None)
    elif command["type"] == "resolve":
        response.update(type="item",discovery_key="one",source_id="one",title=job["feed_id"],media=dict(type="url",url=%q))
    else:
        response.update(type="stopped")
    print(json.dumps(response),flush=True)
    if response["type"] == "stopped":
        break
`, media.URL)
	script := filepath.Join(t.TempDir(), "crawler.py")
	if err := os.WriteFile(script, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, selection := range []string{"hot", "removed"} {
		data, _ := json.Marshal(map[string]string{"scriptPath": script, "selectedFeedId": selection})
		rr := httptest.NewRecorder()
		(&AdminServer{}).handleTestCrawlerScript(rr, httptest.NewRequest(http.MethodPost, "/admin/api/crawlers/test-script", bytes.NewReader(data)))
		var result scriptcrawler.DryRunResult
		if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if selection == "hot" {
			if !result.OK || result.FeedID != "hot" || len(result.Items) != 1 || result.Items[0].Title != "hot" {
				t.Fatalf("selected feed was not tested: %+v", result)
			}
		} else if result.OK || !strings.Contains(result.Error, "不支持抓取栏目") {
			t.Fatalf("invalid selection accepted: %+v", result)
		}
	}
}
