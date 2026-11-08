package scriptcrawler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func documentedPython(t *testing.T, marker string) string {
	t.Helper()
	data, err := os.ReadFile("../../../../docs/CRAWLER_PROTOCOL.md")
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(data), "<!-- "+marker+" -->")
	if !ok {
		t.Fatal("missing code marker")
	}
	_, after, ok = strings.Cut(after, "```python\n")
	if !ok {
		t.Fatal("missing Python code")
	}
	code, _, ok := strings.Cut(after, "\n```")
	if !ok {
		t.Fatal("missing code fence")
	}
	return code + "\n"
}
func documentedSiteTemplate(t *testing.T) string {
	t.Helper()
	source := documentedPython(t, "crawler-v3-template")
	functions := documentedPython(t, "crawler-v3-site-functions")
	start := strings.Index(source, "def discover(")
	end := strings.Index(source, "def main():")
	if start < 0 || end < start {
		t.Fatal("missing template functions")
	}
	return source[:start] + functions + "\n" + source[end:]
}

func startDocumentSession(t *testing.T, source string, config any) (*scriptSession, string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "crawler.py")
	if _, err := ExtractMetadata(source); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	job := map[string]any{"protocol": ProtocolV3, "task_id": "doc-test", "crawler_id": "doc", "feed_id": "hot", "work_dir": dir}
	if config != nil {
		job["config"] = config
	}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	jobPath := filepath.Join(dir, "job.json")
	if err := os.WriteFile(jobPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := startSession(context.Background(), sessionConfig{ScriptPath: script, JobPath: jobPath, WorkDir: dir, OperationTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Close)
	return session, dir
}
func TestDocumentTemplateDiscoverResolveErrorAndStop(t *testing.T) {
	source := documentedPython(t, "crawler-v3-template")
	rows := []map[string]string{{"id": "ignored", "feed": "latest"}, {"id": "first", "feed": "hot", "title": "First", "media_url": "https://example.com/one.mp4"}, {"id": "second", "feed": "hot", "title": "Second", "media_url": "https://example.com/two.mp4"}}
	session, _ := startDocumentSession(t, source, map[string]any{"items": rows})
	ctx := context.Background()
	page, err := session.Discover(ctx, nil, 1)
	if err != nil || len(page.Items) != 1 || page.NextCursor == nil {
		t.Fatalf("%+v %v", page, err)
	}
	again, err := session.Discover(ctx, nil, 1)
	if err != nil || again.Items[0].SourceID != "first" {
		t.Fatalf("retry changed cursor: %+v %v", again, err)
	}
	if item, err := session.Resolve(ctx, page.Items[0]); err != nil || item.SourceID != "first" {
		t.Fatalf("%+v %v", item, err)
	}
	last, err := session.Discover(ctx, page.NextCursor, 1)
	if err != nil || last.NextCursor != nil || last.Items[0].SourceID != "second" {
		t.Fatalf("%+v %v", last, err)
	}
	_, err = session.Resolve(ctx, Candidate{DiscoveryKey: "missing", Locator: json.RawMessage(`{"id":"missing"}`)})
	if failure, ok := err.(*ScriptError); !ok || failure.Code != "not_found" || failure.Scope != "item" {
		t.Fatalf("error=%v", err)
	}
	if err := session.Stop(ctx, "source_exhausted"); err != nil {
		t.Fatal(err)
	}
}
func TestDocumentSiteFunctionsPreservePageRemainder(t *testing.T) {
	var base string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/videos" {
			if r.URL.Query().Get("sort") != "hot" {
				t.Error("site example did not request the selected feed")
			}
			fmt.Fprintf(w, `{"items":[{"id":"1","detail_url":%q},{"id":"2","detail_url":%q}],"has_more":false}`, base+"/detail/1", base+"/detail/2")
			return
		}
		if r.URL.Path == "/api/videos/1" {
			fmt.Fprintf(w, `{"id":"1","title":"Example","video_url":%q}`, base+"/media.mp4")
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	base = server.URL
	source := documentedSiteTemplate(t)
	session, _ := startDocumentSession(t, source, map[string]any{"base_url": base})
	ctx := context.Background()
	first, err := session.Discover(ctx, nil, 1)
	if err != nil || len(first.Items) != 1 || first.NextCursor == nil || *first.NextCursor != "1:1" {
		t.Fatalf("%+v %v", first, err)
	}
	item, err := session.Resolve(ctx, first.Items[0])
	if err != nil || item.Media.Headers["Referer"] != base+"/detail/1" {
		t.Fatalf("%+v %v", item, err)
	}
	second, err := session.Discover(ctx, first.NextCursor, 1)
	if err != nil || second.NextCursor != nil || second.Items[0].SourceID != "2" {
		t.Fatalf("%+v %v", second, err)
	}
	if err := session.Stop(ctx, "target_reached"); err != nil {
		t.Fatal(err)
	}
}

func TestDocumentTemplateDefaultConfigAndUTF8Streams(t *testing.T) {
	t.Setenv("PYTHONIOENCODING", "ascii")
	session, _ := startDocumentSession(t, documentedPython(t, "crawler-v3-template"), nil)
	ctx := context.Background()
	page, err := session.Discover(ctx, nil, 1)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("default config discovery: %+v %v", page, err)
	}
	item, err := session.Resolve(ctx, page.Items[0])
	if err != nil || item.Title != "最热示例视频" {
		t.Fatalf("UTF-8 response: %+v %v", item, err)
	}
	if err := session.Stop(ctx, "source_exhausted"); err != nil {
		t.Fatal(err)
	}
}

func TestDocumentTemplateRejectsInvalidConfig(t *testing.T) {
	for _, config := range []any{"invalid", 42, false, []string{}, json.RawMessage(`null`)} {
		t.Run(fmt.Sprintf("%T", config), func(t *testing.T) {
			session, _ := startDocumentSession(t, documentedPython(t, "crawler-v3-template"), config)
			if _, err := session.Discover(context.Background(), nil, 1); err == nil {
				t.Fatal("accepted a config that is not an object")
			}
			session.Close()
			if !strings.Contains(strings.Join(session.logs.snapshot(), "\n"), "config 必须是对象") {
				t.Fatalf("missing config diagnostic: %v", session.logs.snapshot())
			}
		})
	}
}

func TestDocumentTemplatePreservesCandidateIdentity(t *testing.T) {
	for _, sourceID := range []string{"stable-video-1", ""} {
		t.Run(sourceID, func(t *testing.T) {
			rows := []map[string]string{{"id": "1", "feed": "hot", "title": "Video", "media_url": "https://example.com/video.mp4"}}
			session, _ := startDocumentSession(t, documentedPython(t, "crawler-v3-template"), map[string]any{"items": rows})
			candidate := Candidate{DiscoveryKey: "detail:1", SourceID: sourceID, Locator: json.RawMessage(`{"id":1}`)}
			item, err := session.Resolve(context.Background(), candidate)
			wantID := sourceID
			if wantID == "" {
				wantID = "1"
			}
			if err != nil || item.SourceID != wantID {
				t.Fatalf("candidate identity: %+v %v", item, err)
			}
			if err := session.Stop(context.Background(), "source_exhausted"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDocumentTemplateErrorMessagesRespectByteLimit(t *testing.T) {
	longMessage, err := json.Marshal("解析错误：" + strings.Repeat("错", 4000))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, expression string }{
		{"structured", `SourceError("source", "parse_failed", ` + string(longMessage) + `)`},
		{"unexpected", `ValueError(` + string(longMessage) + `)`},
		{"empty", `SourceError("source", "parse_failed", "")`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := documentedPython(t, "crawler-v3-template")
			override := "\ndef discover(job, cursor, limit, deadline_at):\n    raise " + tc.expression + "\n"
			source = strings.Replace(source, "\nif __name__ == \"__main__\":", override+"\nif __name__ == \"__main__\":", 1)
			session, _ := startDocumentSession(t, source, map[string]any{})
			_, err := session.Discover(context.Background(), nil, 1)
			var failure *ScriptError
			if !errors.As(err, &failure) || failure.Code != "parse_failed" || failure.Message == "" || len(failure.Message) > 8192 || !utf8.ValidString(failure.Message) {
				t.Fatalf("invalid error response: %v", err)
			}
			if tc.name != "empty" && !strings.HasPrefix(failure.Message, "解析错误：") {
				t.Fatalf("error context lost: %q", failure.Message)
			}
			if err := session.Stop(context.Background(), "source_exhausted"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDocumentSiteFunctionsPreserveCandidateIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/videos/locator-1" {
			t.Errorf("used video identity as locator: %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"id":"locator-1","title":"Video","video_url":"https://example.com/video.mp4"}`)
	}))
	defer server.Close()
	session, _ := startDocumentSession(t, documentedSiteTemplate(t), map[string]any{"base_url": server.URL})
	candidate := Candidate{DiscoveryKey: "detail:1", SourceID: "stable-video-1", Locator: json.RawMessage(`{"id":"locator-1","detail_url":"https://example.com/detail/1"}`)}
	item, err := session.Resolve(context.Background(), candidate)
	if err != nil || item.SourceID != candidate.SourceID {
		t.Fatalf("site candidate identity: %+v %v", item, err)
	}
	if err := session.Stop(context.Background(), "source_exhausted"); err != nil {
		t.Fatal(err)
	}
}

func TestDocumentSiteFunctionsClassifyErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body, code, scope string
		discover, network       bool
	}{
		{name: "list_json", body: "invalid-json", code: "parse_failed", scope: "source", discover: true},
		{name: "detail_json", body: "invalid-json", code: "parse_failed", scope: "item"},
		{name: "wrong_video", body: `{"id":"other","title":"Other video","video_url":"https://example.com/video.mp4"}`, code: "parse_failed", scope: "item"},
		{name: "network", code: "source_unavailable", scope: "item", network: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			if tc.network {
				server.Close()
			}
			session, _ := startDocumentSession(t, documentedSiteTemplate(t), map[string]any{"base_url": server.URL})
			var err error
			if tc.discover {
				_, err = session.Discover(context.Background(), nil, 1)
			} else {
				_, err = session.Resolve(context.Background(), Candidate{DiscoveryKey: "detail:1", SourceID: "1", Locator: json.RawMessage(`{"id":"1","detail_url":"https://example.com/detail/1"}`)})
			}
			var failure *ScriptError
			if !errors.As(err, &failure) || failure.Code != tc.code || failure.Scope != tc.scope || failure.Retryable != tc.network {
				t.Fatalf("error classification: %v", err)
			}
			if err := session.Stop(context.Background(), "source_exhausted"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
