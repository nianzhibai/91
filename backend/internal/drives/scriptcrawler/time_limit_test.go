package scriptcrawler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
)

func TestCrawlerContinuesBeyondFormerCountLimits(t *testing.T) {
	for _, mode := range []string{"known", "duplicates"} {
		t.Run(mode, func(t *testing.T) {
			duplicateURL := serveScriptCrawlerMedia(t, "duplicate-video")
			newURL := serveScriptCrawlerMedia(t, "different-new-video")
			body := fmt.Sprintf(`for i in range(125):
    c=read()
    assert c["type"]=="discover" and c["limit"]==1
    key="candidate-"+str(i)
    send(c,"page",items=[dict(discovery_key=key,source_id=key,locator={})],next_cursor=str(i+1))
    if %q=="duplicates":
        c=read()
        assert c["type"]=="resolve"
        send(c,"item",discovery_key=key,source_id=key,title="Duplicate",media=dict(type="url",url=%q))
c=read()
assert c["type"]=="discover" and c["limit"]==1
send(c,"page",items=[dict(discovery_key="new",source_id="new",locator={})],next_cursor="more")
c=read()
assert c["type"]=="resolve"
send(c,"item",discovery_key="new",source_id="new",title="New video",media=dict(type="url",url=%q))
stop()
`, mode, duplicateURL, newURL)
			c := newRuntimeTestCrawler(t, body, ProtocolV3, func(cfg *CrawlerConfig) {
				cfg.PageSize = 1
				cfg.RunTimeout = 10 * time.Second
			})
			ctx := context.Background()
			if mode == "known" {
				for i := 0; i < 125; i++ {
					if err := c.cfg.Catalog.MarkCrawlerSourceSeen(ctx, Kind, c.cfg.Driver.ID(), fmt.Sprintf("candidate-%d", i), "imported", "old", "", 1); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				if err := c.cfg.Driver.Init(ctx); err != nil {
					t.Fatal(err)
				}
				outcome, err := c.Import(ctx, ctx, Item{DiscoveryKey: "seed", SourceID: "seed", Title: "Seed", Media: MediaRef{Type: "url", URL: duplicateURL}})
				if err != nil || outcome != ImportAdded {
					t.Fatalf("seed import: %s %v", outcome, err)
				}
			}
			r, err := c.RunOnce(ctx, 1)
			if err != nil || r.StopReason != "target_reached" || r.State != "completed" || r.NewVideos != 1 || r.Checked != 126 || r.Pages != 126 || r.Failed != 0 {
				t.Fatalf("count limit prevented the new video: %+v %v", r, err)
			}
			if mode == "known" && (r.Known != 125 || r.ResolveCalls != 1) {
				t.Fatalf("known candidates were resolved: %+v", r)
			}
			if mode == "duplicates" && (r.Duplicates != 125 || r.ResolveCalls != 126) {
				t.Fatalf("processing stopped before the new video: %+v", r)
			}
			if _, err := c.cfg.Catalog.GetVideo(ctx, importVideoID(r.DriveID, "new")); err != nil {
				t.Fatalf("new video was not committed: %v", err)
			}
		})
	}
}

func TestCrawlerTimeLimitStopsAcquisitionAndLogsOutcome(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"discover", `c=read()
while True:
    send(c,"heartbeat")
    time.sleep(.005)
`},
		{"resolve", `c=read();send(c,"page",items=[dict(discovery_key="one",locator={})],next_cursor=None)
c=read()
while True:
    send(c,"heartbeat")
    time.sleep(.005)
`},
		{"retry_wait", `c=read();send(c,"error",scope="source",code="rate_limited",message="wait",retryable=True,retry_after_seconds=60)
read()
`},
		{"known_pages", `i=0
while True:
    c=read()
    assert c["type"]=="discover"
    send(c,"page",items=[dict(discovery_key="alias-"+str(i),source_id="known",locator={})],next_cursor=str(i+1))
    i+=1
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newRuntimeTestCrawler(t, tc.body, ProtocolV3, func(cfg *CrawlerConfig) {
				cfg.RunTimeout = 300 * time.Millisecond
			})
			if err := c.cfg.Catalog.MarkCrawlerSourceSeen(context.Background(), Kind, c.cfg.Driver.ID(), "known", "imported", "old", "", 1); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(previous) })
			task, err := c.Prepare(context.Background(), 10, "")
			if err != nil {
				t.Fatal(err)
			}
			var stages []string
			started := time.Now()
			r, err := c.RunTask(context.Background(), task, func(ctx context.Context, task *Task) error {
				if ctx.Err() != nil {
					t.Fatalf("time limit canceled completion: %v", ctx.Err())
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
			if err != nil || r.StopReason != "time_limit" || r.NewVideos != 0 || r.Failed != 0 || len(r.Issues) != 0 || r.TargetReached || time.Since(started) > 2*time.Second {
				t.Fatalf("time limit was treated as a failure or extended: %+v %v", r, err)
			}
			if strings.Join(stages, ",") != "generation,upload" {
				t.Fatalf("time limit skipped completion: %v", stages)
			}
			stored, err := c.cfg.Catalog.GetCrawlerTask(context.Background(), r.DriveID, r.TaskID)
			if err != nil || stored.State == "running" || stored.StopReason != "time_limit" || stored.Failed != 0 {
				t.Fatalf("time limit not persisted: %+v %v", stored, err)
			}
			if _, err := os.Stat(task.dir); !os.IsNotExist(err) {
				t.Fatalf("time limit retained workspace: %v", err)
			}
			for _, message := range []string{"Crawler time limit reached", "reason=time_limit target=10", "crawl_elapsed=", "total_elapsed="} {
				if !strings.Contains(logs.String(), message) {
					t.Fatalf("missing time limit log %q: %s", message, logs.String())
				}
			}
		})
	}
}

func TestCrawlerDeadlinesRemovePartialDownload(t *testing.T) {
	for _, mode := range []string{"time_limit", "download_timeout"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Length", "10000")
				_, _ = io.WriteString(w, "partial-video")
				w.(http.Flusher).Flush()
				<-req.Context().Done()
			}))
			defer server.Close()
			c := newRuntimeTestCrawler(t, fmt.Sprintf(`c=read();send(c,"page",items=[dict(discovery_key="one",source_id="one",locator={})],next_cursor=None)
c=read();send(c,"item",discovery_key="one",source_id="one",title="Partial",media=dict(type="url",url=%q))
stop()
`, server.URL+"/video.mp4"), ProtocolV3, func(cfg *CrawlerConfig) {
				if mode == "time_limit" {
					cfg.RunTimeout = 400 * time.Millisecond
				} else {
					cfg.DownloadTimeout = 150 * time.Millisecond
				}
			})
			task, err := c.Prepare(context.Background(), 1, "")
			if err != nil {
				t.Fatal(err)
			}
			r, err := c.RunTask(context.Background(), task, nil)
			if r.NewVideos != 0 || r.ResolveCalls != 1 {
				t.Fatalf("download was not attempted: %+v %v", r, err)
			}
			if mode == "time_limit" {
				if err != nil || r.StopReason != "time_limit" || r.Failed != 0 || len(r.Issues) != 0 {
					t.Fatalf("time limit counted a download failure: %+v %v", r, err)
				}
			} else if err == nil || r.Failed != 1 || len(r.Issues) != 1 || r.Issues[0].Code != "operation_timeout" || r.Issues[0].Stage != "import" || r.StopReason == "time_limit" {
				t.Fatalf("download timeout mislabeled as time limit: %+v %v", r, err)
			}
			path := filepath.Join(c.cfg.Driver.VideosDir(), mediaFileStem("one")+".mp4")
			for _, file := range []string{path, path + ".part", task.dir} {
				if _, err := os.Stat(file); !os.IsNotExist(err) {
					t.Fatalf("incomplete file retained: %s %v", file, err)
				}
			}
			known, err := c.cfg.Catalog.KnownCrawlerCandidates(context.Background(), r.DriveID, []catalog.CrawlerIdentity{{DiscoveryKey: "one"}})
			if err != nil || known["one"] {
				t.Fatalf("partial download recorded as known: %v %v", known, err)
			}
		})
	}
}

func TestCrawlerTimeLimitDoesNotHideCompletionFailure(t *testing.T) {
	c := newRuntimeTestCrawler(t, `read();time.sleep(30)`, ProtocolV3, func(cfg *CrawlerConfig) {
		cfg.RunTimeout = 200 * time.Millisecond
	})
	task, err := c.Prepare(context.Background(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	uploadFailure := errors.New("upload unavailable")
	r, err := c.RunTask(context.Background(), task, func(ctx context.Context, task *Task) error {
		return task.RunStage(ctx, "upload", func(context.Context) error { return uploadFailure })
	})
	if !errors.Is(err, uploadFailure) || r.State != "failed" || r.Failed != 1 || r.StopReason != "backend_error" || len(r.Issues) != 1 || r.Issues[0].Stage != "upload" {
		t.Fatalf("time limit hid upload failure: %+v %v", r, err)
	}
}

func TestCrawlerParentDeadlineIsNotTimeLimit(t *testing.T) {
	c := newRuntimeTestCrawler(t, `read();time.sleep(30)`, ProtocolV3, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r, err := c.RunOnce(ctx, 1)
	if !errors.Is(err, context.DeadlineExceeded) || r.StopReason != "timeout" || r.Failed != 1 {
		t.Fatalf("parent deadline treated as configured time limit: %+v %v", r, err)
	}
}

func TestCrawlerTimeLimitDoesNotHideEarlierOperationTimeout(t *testing.T) {
	c := newRuntimeTestCrawler(t, `import signal
signal.signal(signal.SIGTERM,signal.SIG_IGN)
read();time.sleep(30)
`, ProtocolV3, func(cfg *CrawlerConfig) {
		cfg.OperationTimeout = 150 * time.Millisecond
		cfg.RunTimeout = 250 * time.Millisecond
		cfg.StopGrace = 300 * time.Millisecond
	})
	r, err := c.RunOnce(context.Background(), 1)
	if !errors.Is(err, errOperationTimeout) || !errors.Is(err, context.DeadlineExceeded) || r.StopReason != "operation_timeout" || r.Failed != 1 {
		t.Fatalf("shutdown time hid the earlier operation timeout: %+v %v", r, err)
	}
}
