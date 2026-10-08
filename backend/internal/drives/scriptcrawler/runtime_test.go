package scriptcrawler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/crawljob"
)

const pythonSessionPrelude = `import json, sys, os, time
job = json.load(open(sys.argv[sys.argv.index("--job")+1]))
def read(): return json.loads(sys.stdin.readline())
def send(command, kind, **fields):
    print(json.dumps(dict(type=kind, request_id=command["request_id"], **fields)), flush=True)
def stop():
    command=read()
    assert command["type"] == "stop"
    send(command, "stopped")
`

func newRuntimeTestCrawler(t *testing.T, body, protocol string, mutate func(*CrawlerConfig)) *Crawler {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("Python is required for protocol tests")
	}
	dir := t.TempDir()
	cat, err := catalog.Open(filepath.Join(dir, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	script := filepath.Join(dir, "crawler.py")
	if err := os.WriteFile(script, []byte("CRAWLER_NAME = 'Runtime Test'\nCRAWLER_PROTOCOL = '"+protocol+"'\n"+pythonSessionPrelude+body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := CrawlerConfig{Driver: New(Config{ID: "runtime-test", RootDir: filepath.Join(dir, "crawler")}), Catalog: cat, ScriptPath: script, RunTimeout: 3 * time.Second, OperationTimeout: time.Second, StopGrace: 200 * time.Millisecond, FFprobePath: writeScriptCrawlerFFprobeStub(t, dir, true)}
	if mutate != nil {
		mutate(&cfg)
	}
	return NewCrawler(cfg)
}
func TestDefaultOperationDeadlines(t *testing.T) {
	body := `import datetime
def check(c, maximum):
    deadline=datetime.datetime.fromisoformat(c["deadline_at"].replace("Z","+00:00"))
    remaining=(deadline-datetime.datetime.now(datetime.timezone.utc)).total_seconds()
    assert 0 < remaining <= maximum and remaining > maximum-10, (c["type"], remaining)
c=read();assert c["type"]=="discover";check(c,300)
send(c,"page",items=[dict(discovery_key="one",locator={})],next_cursor=None)
c=read();assert c["type"]=="resolve";check(c,300)
send(c,"item",discovery_key="one",source_id="known",title="Known",media=dict(type="url",url="https://example.com/video.mp4"))
c=read();assert c["type"]=="stop";check(c,1)
send(c,"stopped")
`
	c := newRuntimeTestCrawler(t, body, ProtocolV3, func(cfg *CrawlerConfig) {
		cfg.OperationTimeout = 0
		cfg.RunTimeout = 10 * time.Minute
		cfg.StopGrace = 0
	})
	ctx := context.Background()
	if err := c.cfg.Catalog.MarkCrawlerSourceSeen(ctx, Kind, c.cfg.Driver.ID(), "known", "imported", "existing", "", 1); err != nil {
		t.Fatal(err)
	}
	if result, err := c.RunOnce(ctx, 1); err != nil || result.Known != 1 {
		t.Fatalf("production default deadline: %+v %v", result, err)
	}
	result := DryRun(ctx, DryRunConfig{ScriptPath: c.cfg.ScriptPath, Timeout: 10 * time.Minute, SkipMediaProbe: true})
	if !result.OK {
		t.Fatalf("session default deadline: %+v", result)
	}
}
func TestSessionRejectsInvalidMessagesAndExits(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{"no_response", `read()`, "without page"},
		{"plain_stdout", `read(); print("diagnostic", flush=True)`, "JSON objects"},
		{"unknown_field", `c=read(); send(c,"page",items=[],next_cursor=None,extra=1)`, "unknown field"},
		{"wrong_id", `c=read(); c["request_id"]="wrong"; send(c,"page",items=[],next_cursor=None)`, "request_id"},
		{"missing_cursor", `c=read(); send(c,"page",items=[])`, "next_cursor"},
		{"null_items", `c=read(); send(c,"page",items=None,next_cursor=None)`, "null"},
		{"candidate_overflow", `c=read(); send(c,"page",items=[dict(discovery_key=str(i),locator={}) for i in range(c["limit"]+1)],next_cursor=None)`, "at most"},
		{"duplicate_keys", `c=read(); print('{"type":"page","type":"page","request_id":'+json.dumps(c["request_id"])+',"items":[],"next_cursor":null}',flush=True)`, "duplicate field"},
		{"unflushed_line", `c=read(); sys.stdout.write(json.dumps(dict(type="page",request_id=c["request_id"],items=[],next_cursor=None)))`, "incomplete"},
		{"extra_response", `c=read(); send(c,"page",items=[],next_cursor=None); send(c,"page",items=[],next_cursor=None); stop()`, "request_id"},
		{"after_stopped", `c=read(); send(c,"page",items=[],next_cursor=None); stop(); print("unexpected",flush=True)`, "after stopped"},
		{"nonzero_exit", `c=read(); send(c,"page",items=[],next_cursor=None); stop(); sys.exit(7)`, "process exit"},
		{"stop_hang", `c=read(); send(c,"page",items=[],next_cursor=None); stop(); time.sleep(30)`, "did not exit"},
		{"repeat_cursor", `c=read(); send(c,"page",items=[],next_cursor="next"); c=read(); send(c,"page",items=[],next_cursor="next")`, "repeated next_cursor"},
		{"repeat_page", `c=read(); send(c,"page",items=[dict(discovery_key="known",source_id="known",locator={})],next_cursor="next"); c=read(); send(c,"page",items=[dict(discovery_key="known",source_id="known",locator={})],next_cursor=None)`, "repeated discovery page"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newRuntimeTestCrawler(t, tt.body, ProtocolV3, nil)
			if err := c.cfg.Catalog.MarkCrawlerSourceSeen(context.Background(), Kind, c.cfg.Driver.ID(), "known", "imported", "old", "", 1); err != nil {
				t.Fatal(err)
			}
			result, err := c.RunOnce(context.Background(), 1)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("result=%+v err=%v want %s", result, err, tt.want)
			}
			if result.StopReason != "protocol_error" {
				t.Fatalf("stop reason: %s", result.StopReason)
			}
		})
	}
}
func TestHeartbeatCannotExtendOperationDeadline(t *testing.T) {
	c := newRuntimeTestCrawler(t, `c=read()
while True:
    send(c,"heartbeat")
    time.sleep(.005)
`, ProtocolV3, func(cfg *CrawlerConfig) { cfg.OperationTimeout = 80 * time.Millisecond })
	started := time.Now()
	result, err := c.RunOnce(context.Background(), 1)
	if !errors.Is(err, context.DeadlineExceeded) || result.StopReason != "operation_timeout" || time.Since(started) > time.Second {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
func TestKnownSourcesAndDiscoveryAliasesSkipResolve(t *testing.T) {
	c := newRuntimeTestCrawler(t, `c=read()
send(c,"page",items=[dict(discovery_key="direct",source_id="known",locator={}),dict(discovery_key="alias",locator={})],next_cursor=None)
stop()
`, ProtocolV3, nil)
	ctx := context.Background()
	cat := c.cfg.Catalog
	if err := cat.MarkCrawlerSourceSeen(ctx, Kind, c.cfg.Driver.ID(), "known", "imported", "video", "", 1); err != nil {
		t.Fatal(err)
	}
	if err := cat.BindCrawlerDiscovery(ctx, c.cfg.Driver.ID(), "alias", "known"); err != nil {
		t.Fatal(err)
	}
	result, err := c.RunOnce(ctx, 1)
	if err != nil || result.Known != 2 || result.ResolveCalls != 0 || result.NewVideos != 0 || result.StopReason != "source_exhausted" {
		t.Fatalf("%+v %v", result, err)
	}
}
func TestResolveKnownSourceCreatesAlias(t *testing.T) {
	c := newRuntimeTestCrawler(t, `c=read(); send(c,"page",items=[dict(discovery_key="new-alias",locator={})],next_cursor=None)
c=read()
if c["type"]=="resolve":
    send(c,"item",discovery_key="new-alias",source_id="known",title="Known",media=dict(type="url",url="https://unused.example/video.mp4"))
    stop()
else:
    assert c["type"]=="stop"
    send(c,"stopped")
`, ProtocolV3, nil)
	if err := c.cfg.Catalog.MarkCrawlerSourceSeen(context.Background(), Kind, c.cfg.Driver.ID(), "known", "imported", "existing", "", 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		r, err := c.RunOnce(context.Background(), 1)
		if err != nil || r.Known != 1 || r.Resolved != 1-i {
			t.Fatalf("run=%d %+v %v", i, r, err)
		}
	}
}
func TestQueuedCancellationPersistsBeforeProgressAndRestartRecovers(t *testing.T) {
	c := newRuntimeTestCrawler(t, `raise Exception("must not execute")`, ProtocolV3, nil)
	ctx, cancel := context.WithCancel(context.Background())
	task, err := c.Prepare(ctx, 2, "parent")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := c.cfg.Catalog.GetCrawlerTask(ctx, c.cfg.Driver.ID(), task.Result.TaskID)
	if err != nil || stored.State != "queued" {
		t.Fatalf("%+v %v", stored, err)
	}
	c.cfg.OnProgress = func(r crawljob.Result) {
		stored, err := c.cfg.Catalog.GetCrawlerTask(context.Background(), r.DriveID, r.TaskID)
		if err != nil || stored.State != r.State {
			t.Errorf("progress preceded persistence: %+v %v", stored, err)
		}
	}
	cancel()
	r, err := c.RunTask(ctx, task, nil)
	if !errors.Is(err, context.Canceled) || r.State != "canceled" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := os.Stat(task.dir); !os.IsNotExist(err) {
		t.Fatalf("workspace not removed: %v", err)
	}
	task, err = c.Prepare(context.Background(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.cfg.Catalog.InterruptCrawlerTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err = c.cfg.Catalog.GetCrawlerTask(context.Background(), r.DriveID, task.Result.TaskID)
	if err != nil || stored.State != "interrupted" || stored.StopReason != "service_restart" {
		t.Fatalf("%+v %v", stored, err)
	}
}
func TestAcceptedTaskUsesScriptSnapshot(t *testing.T) {
	c := newRuntimeTestCrawler(t, `c=read(); send(c,"page",items=[],next_cursor=None); stop()`, ProtocolV3, nil)
	task, err := c.Prepare(context.Background(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.cfg.ScriptPath, []byte("broken replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := c.RunTask(context.Background(), task, nil)
	if err != nil || r.StopReason != "source_exhausted" || r.Pages != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := c.Prepare(context.Background(), 1, ""); err == nil {
		t.Fatal("new run accepted invalid script")
	}
}
func TestScopeAndRetryPolicies(t *testing.T) {
	for _, code := range []string{"not_found", "auth_required", "rate_limited"} {
		t.Run(code, func(t *testing.T) {
			body := `c=read(); send(c,"page",items=[dict(discovery_key="one",locator={})],next_cursor=None)
c=read(); send(c,"error",scope="item",code="` + code + `",message="test",retryable=True,retry_after_seconds=1)
`
			if code == "not_found" {
				body += "stop()\n"
			}
			if code == "rate_limited" {
				body += `c=read(); send(c,"item",discovery_key="one",source_id="known",title="known",media=dict(type="url",url="https://unused.example/video.mp4")); stop()`
			}
			c := newRuntimeTestCrawler(t, body, ProtocolV3, nil)
			_ = c.cfg.Catalog.MarkCrawlerSourceSeen(context.Background(), Kind, c.cfg.Driver.ID(), "known", "imported", "video", "", 1)
			r, err := c.RunOnce(context.Background(), 1)
			switch code {
			case "not_found":
				if err == nil || r.Failed != 1 || r.Retries != 0 || r.State != "failed" {
					t.Fatalf("%+v %v", r, err)
				}
			case "auth_required":
				if err == nil || r.StopReason != "source_error" || r.Retries != 0 {
					t.Fatalf("%+v %v", r, err)
				}
			case "rate_limited":
				if err != nil || r.Retries != 1 || r.ResolveCalls != 2 || r.Known != 1 {
					t.Fatalf("%+v %v", r, err)
				}
			}
		})
	}
}
func TestScanScriptOutputLimits(t *testing.T) {
	for _, limit := range []struct {
		line  int
		total int64
	}{{16, 100}, {100, 16}} {
		var got error
		for output := range scanScriptOutput(context.Background(), strings.NewReader(strings.Repeat("x", 32)+"\n"), limit.line, limit.total) {
			got = output.err
		}
		if got == nil {
			t.Fatal("missing output limit error")
		}
	}
}
func TestStrictItemValidation(t *testing.T) {
	base := Item{DiscoveryKey: "detail", SourceID: "源站:123/视频", Title: "Title", Media: MediaRef{Type: "url", URL: "https://example.com/video.mp4"}}
	if err := validateItem(base, Candidate{DiscoveryKey: "detail"}); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Item){func(i *Item) { i.SourceID = "" }, func(i *Item) { i.DiscoveryKey = "wrong" }, func(i *Item) { i.Media.Type = "file" }, func(i *Item) { i.Media.URL = "" }, func(i *Item) { i.Media = MediaRef{Type: "url", URL: "file:///etc/passwd"} }, func(i *Item) {
		i.Media = MediaRef{Type: "url", URL: "https://example.com", Headers: map[string]string{"X-Test": "a\r\nb"}}
	}, func(i *Item) {
		i.Thumbnail = &MediaRef{Type: "file", URL: "https://example.com/thumb.jpg"}
	}} {
		item := base
		mutate(&item)
		if err := validateItem(item, Candidate{DiscoveryKey: "detail"}); err == nil {
			t.Fatalf("accepted %+v", item)
		}
	}
	var response itemResponse
	for _, raw := range []string{
		`{"type":"item","request_id":"1","media_url":"https://example.com"}`,
		`{"type":"item","request_id":"1","media":{"type":"file","path":"/tmp/video.mp4"}}`,
		`{"type":"item","request_id":"1","media":{"type":"url","url":"https://example.com/video.mp4","path":"/tmp/video.mp4"}}`,
		`{"type":"item","request_id":"1","thumbnail":{"type":"file","path":"/tmp/thumb.jpg"}}`,
	} {
		if err := strictDecode([]byte(raw), &response); err == nil {
			t.Fatalf("accepted removed media field: %s", raw)
		}
	}
}

func TestCrawlerImportsItemsWithIgnoredDescriptions(t *testing.T) {
	mediaURL := serveScriptCrawlerMedia(t, "video with ignored description")
	c := newRuntimeTestCrawler(t, fmt.Sprintf(`c=read(); send(c,"page",items=[dict(discovery_key="one",locator={})],next_cursor=None)
c=read(); send(c,"item",discovery_key="one",source_id="source",title="Video title",author="Video author",duration_seconds=42,description="Legacy description",media=dict(type="url",url=%q))
stop()
`, mediaURL), ProtocolV3, nil)
	ctx := context.Background()
	result, err := c.RunOnce(ctx, 1)
	if err != nil || result.NewVideos != 1 || result.Failed != 0 {
		t.Fatalf("import with description failed: result=%+v error=%v", result, err)
	}
	videos, err := c.cfg.Catalog.ListVideosByDrive(ctx, result.DriveID)
	if err != nil || len(videos) != 1 {
		t.Fatalf("imported videos=%#v error=%v", videos, err)
	}
	video := videos[0]
	if video.Title != "Video title" || video.Author != "Video author" || video.DurationSeconds != 42 {
		t.Fatalf("retained metadata changed: %#v", video)
	}
	encoded, err := json.Marshal(video)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"description"`) {
		t.Fatalf("import retained description: %s", encoded)
	}
}

func TestRunningCancellationPersistsAndReleasesTask(t *testing.T) {
	c := newRuntimeTestCrawler(t, `c=read(); send(c,"heartbeat"); time.sleep(30)`, ProtocolV3, nil)
	ctx, cancel := context.WithCancel(context.Background())
	task, err := c.Prepare(ctx, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	c.cfg.OnProgress = func(r crawljob.Result) {
		if r.State == "running" {
			cancel()
		}
	}
	result, err := c.RunTask(ctx, task, nil)
	if !errors.Is(err, context.Canceled) || result.State != "canceled" {
		t.Fatalf("%+v %v", result, err)
	}
	stored, err := c.cfg.Catalog.GetCrawlerTask(context.Background(), result.DriveID, result.TaskID)
	if err != nil || stored.State != "canceled" {
		t.Fatalf("%+v %v", stored, err)
	}
	// The active-task uniqueness guard must be released by the terminal commit.
	if _, err := c.Prepare(context.Background(), 1, ""); err != nil {
		t.Fatal(err)
	}
}

func TestActualImportTargetStopsResolveAndFailedCandidatesRemainRetryable(t *testing.T) {
	mediaURL := serveScriptCrawlerMedia(t, "video")
	c := newRuntimeTestCrawler(t, fmt.Sprintf(`c=read(); send(c,"page",items=[dict(discovery_key="bad",locator={}),dict(discovery_key="good",locator={}),dict(discovery_key="extra",locator={})],next_cursor=None)
c=read(); assert c["candidate"]["discovery_key"]=="bad"
send(c,"item",discovery_key="bad",source_id="bad",title="Bad",media=dict(type="url",url="http://127.0.0.1:1/not-found"))
c=read(); assert c["candidate"]["discovery_key"]=="good"
send(c,"item",discovery_key="good",source_id="源站:123/视频",title="Good",media=dict(type="url",url=%q))
stop()
`, mediaURL), ProtocolV3, nil)
	r, err := c.RunOnce(context.Background(), 1)
	if err == nil || r.NewVideos != 1 || r.Failed != 1 || r.ResolveCalls != 2 || r.Checked != 3 || !r.TargetReached || r.StopReason != "target_reached" {
		t.Fatalf("%+v %v", r, err)
	}
	known, err := c.cfg.Catalog.KnownCrawlerCandidates(context.Background(), r.DriveID, []catalog.CrawlerIdentity{{DiscoveryKey: "bad"}, {DiscoveryKey: "good"}})
	if err != nil || known["bad"] || !known["good"] {
		t.Fatalf("history=%v %v", known, err)
	}
	videos, err := c.cfg.Catalog.ListVideosByDrive(context.Background(), r.DriveID)
	if err != nil || len(videos) != 1 || filepath.Base(videos[0].FileID) != videos[0].FileID {
		t.Fatalf("%+v %v", videos, err)
	}
	sources, err := c.cfg.Catalog.ListCrawlerSourceIDs(context.Background(), Kind, r.DriveID)
	if err != nil || len(sources) != 1 || sources[0] != "源站:123/视频" {
		t.Fatalf("sources=%v %v", sources, err)
	}
}

func TestGenerationFailureIsPartOfPersistedOutcome(t *testing.T) {
	mediaURL := serveScriptCrawlerMedia(t, "new video")
	c := newRuntimeTestCrawler(t, fmt.Sprintf(`c=read();send(c,"page",items=[dict(discovery_key="one",locator={})],next_cursor=None)
c=read();send(c,"item",discovery_key="one",source_id="source",title="New",media=dict(type="url",url=%q));stop()
`, mediaURL), ProtocolV3, nil)
	task, err := c.Prepare(context.Background(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.RunTask(context.Background(), task, func(ctx context.Context, task *Task) error {
		return task.RunStage(ctx, "generation", func(ctx context.Context) error {
			return c.cfg.Catalog.UpdatePreview(ctx, task.Result.ImportedVideoIDs[0], "", "failed")
		})
	})
	if err == nil || result.State != "partial" || result.NewVideos != 1 || result.Failed != 1 || len(result.Issues) != 1 || result.Issues[0].Stage != "generation" || result.Issues[0].VideoID == "" {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestAcquisitionFailuresStillRunCompletion(t *testing.T) {
	for _, tc := range []struct{ name, body, reason string }{
		{"source", `c=read();send(c,"error",scope="source",code="source_unavailable",message="site unavailable",retryable=False)`, "source_error"},
		{"protocol", `read();print("invalid protocol output",flush=True)`, "protocol_error"},
		{"timeout", `read();time.sleep(30)`, "operation_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newRuntimeTestCrawler(t, tc.body, ProtocolV3, func(cfg *CrawlerConfig) {
				if tc.name == "timeout" {
					cfg.OperationTimeout = 100 * time.Millisecond
				}
			})
			task, err := c.Prepare(context.Background(), 1, "")
			if err != nil {
				t.Fatal(err)
			}
			var stages []string
			result, err := c.RunTask(context.Background(), task, func(ctx context.Context, task *Task) error {
				if ctx.Err() != nil {
					t.Errorf("acquisition failure canceled completion: %v", ctx.Err())
				}
				for _, stage := range []string{"generation", "upload"} {
					if err := task.RunStage(ctx, stage, func(context.Context) error {
						stages = append(stages, stage)
						return nil
					}); err != nil {
						return err
					}
				}
				return nil
			})
			if err == nil || result.State != "failed" || result.StopReason != tc.reason || result.NewVideos != 0 || strings.Join(stages, ",") != "generation,upload" {
				t.Fatalf("acquisition failure prevented completion or lost its outcome: result=%+v stages=%v error=%v", result, stages, err)
			}
			if len(result.Issues) != 1 || result.Issues[0].Stage != "discover" {
				t.Fatalf("acquisition failure attributed to completion: %+v", result.Issues)
			}
		})
	}
}
