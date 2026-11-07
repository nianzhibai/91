package scriptcrawler

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/crawljob"
)

type importRoundTripper func(*http.Request) (*http.Response, error)

func (f importRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type importResponseBody struct {
	io.Reader
	onClose func()
}

func (b importResponseBody) Close() error {
	if b.onClose != nil {
		b.onClose()
	}
	return nil
}

func TestCrawlerCancellationFinishesDownloadedVideo(t *testing.T) {
	for _, mode := range []string{"url", "duplicate", "invalid", "thumbnail", "task_id"} {
		t.Run(mode, func(t *testing.T) {
			body := `c=read(); send(c,"page",items=[dict(discovery_key="one",locator={}),dict(discovery_key="two",locator={})],next_cursor="next")
c=read(); assert c["candidate"]["discovery_key"]=="one"
media=dict(type="url",url="http://download.test/video.mp4")
`
			body += `send(c,"item",discovery_key="one",source_id="one",title="Complete video",media=media,thumbnail=dict(type="url",url="http://download.test/thumb.webp"))
time.sleep(30)
`
			c := newRuntimeTestCrawler(t, body, ProtocolV3, func(cfg *CrawlerConfig) {
				cfg.CommonThumbDir = t.TempDir()
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "duplicate" {
				seedURL := serveScriptCrawlerMedia(t, "complete-video")
				outcome, err := c.Import(ctx, ctx, Item{DiscoveryKey: "seed", SourceID: "seed", Title: "Existing", Media: MediaRef{Type: "url", URL: seedURL}})
				if err != nil || outcome != ImportAdded {
					t.Fatalf("seed import: %s %v", outcome, err)
				}
			}
			if mode == "invalid" {
				c.Importer.cfg.FFprobePath = writeScriptCrawlerFFprobeStub(t, t.TempDir(), false)
			}
			task, err := c.Prepare(ctx, 2, "")
			if err != nil {
				t.Fatal(err)
			}
			stop := func() {
				if !c.CancelTask(task.Result.TaskID) {
					t.Error("task was released before import completed")
				}
				stored, err := c.cfg.Catalog.GetCrawlerTask(context.Background(), task.Result.DriveID, task.Result.TaskID)
				if err != nil || stored.State != "running" || !stored.FinishedAt.IsZero() {
					t.Errorf("task finished before import: %+v %v", stored, err)
				}
				if _, err := os.Stat(task.dir); err != nil {
					t.Errorf("workspace removed before import: %v", err)
				}
			}
			thumb, err := base64.StdEncoding.DecodeString(scriptCrawlerWebPBase64)
			if err != nil {
				t.Fatal(err)
			}
			c.Importer.cfg.HTTPClient = &http.Client{Transport: importRoundTripper(func(req *http.Request) (*http.Response, error) {
				payload := "complete-video"
				var onClose func()
				if req.URL.Path == "/thumb.webp" {
					payload = string(thumb)
					if mode == "thumbnail" {
						stop()
					}
				} else if mode != "thumbnail" {
					// downloadAtomic closes the body after renaming the complete
					// file, just before validation starts.
					onClose = func() {
						stop()
						<-req.Context().Done()
					}
				}
				if err := req.Context().Err(); err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: importResponseBody{Reader: strings.NewReader(payload), onClose: onClose}}, nil
			})}
			var stages []string
			result, err := c.RunTask(ctx, task, func(ctx context.Context, task *Task) error {
				for _, stage := range []string{"generation", "upload"} {
					if err := task.RunStage(ctx, stage, func(ctx context.Context) error {
						stages = append(stages, stage)
						// Repeated pauses must not interrupt completion either.
						c.StopTasks()
						return ctx.Err()
					}); err != nil {
						return err
					}
				}
				return nil
			})
			if !errors.Is(err, context.Canceled) || result.State != "canceled" || result.StopReason != "user_canceled" || result.ResolveCalls != 1 || result.DiscoverCalls != 1 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			wantNew, wantDuplicates := 1, 0
			if mode == "duplicate" {
				wantNew, wantDuplicates = 0, 1
			} else if mode == "invalid" {
				wantNew = 0
			}
			if (wantNew == 1 && strings.Join(stages, ",") != "generation,upload") || (wantNew == 0 && len(stages) != 0) {
				t.Fatalf("completion stages=%v new=%d", stages, wantNew)
			}
			stored, err := c.cfg.Catalog.GetCrawlerTask(context.Background(), result.DriveID, result.TaskID)
			if err != nil || stored.NewVideos != wantNew || stored.Duplicates != wantDuplicates || stored.State != "canceled" || len(stored.ImportedVideoIDs) != wantNew {
				t.Fatalf("persisted result=%+v err=%v", stored, err)
			}
			videoID := importVideoID(result.DriveID, "one")
			videoPath := filepath.Join(c.cfg.Driver.VideosDir(), mediaFileStem("one")+".mp4")
			if wantNew == 1 {
				v, err := c.cfg.Catalog.GetVideo(context.Background(), videoID)
				if err != nil || v.FingerprintStatus != "ready" || v.SampledSHA256 == "" || v.ThumbnailURL == "" || !strings.Contains(strings.Join(v.Tags, ","), result.DriveID) {
					t.Fatalf("incomplete video publication: %+v %v", v, err)
				}
				data, err := os.ReadFile(videoPath)
				if err != nil || string(data) != "complete-video" {
					t.Fatalf("downloaded video lost: %q %v", data, err)
				}
			} else {
				if _, err := os.Stat(videoPath); !os.IsNotExist(err) {
					t.Fatalf("rejected video retained: %v", err)
				}
				if _, err := c.cfg.Catalog.GetVideo(context.Background(), videoID); err == nil {
					t.Fatal("rejected video published")
				}
			}
			known, err := c.cfg.Catalog.KnownCrawlerCandidates(context.Background(), result.DriveID, []catalog.CrawlerIdentity{{DiscoveryKey: "one"}, {DiscoveryKey: "two"}})
			if err != nil || known["one"] != (mode != "invalid") || known["two"] {
				t.Fatalf("source history=%v err=%v", known, err)
			}
			if mode == "duplicate" {
				data, err := os.ReadFile(filepath.Join(c.cfg.Driver.VideosDir(), mediaFileStem("seed")+".mp4"))
				if err != nil || string(data) != "complete-video" {
					t.Fatalf("canonical video changed: %q %v", data, err)
				}
			}
			if mode == "invalid" {
				found := false
				for _, issue := range stored.Issues {
					found = found || issue.Code == "import_failed"
				}
				if !found {
					t.Fatal("cancellation hid the validation failure")
				}
			}
			if _, err := os.Stat(task.dir); !os.IsNotExist(err) {
				t.Fatalf("workspace retained after import: %v", err)
			}
		})
	}
}

func TestCrawlerCancellationRemovesPartialDownload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Length", "10000")
		_, _ = io.WriteString(w, "partial-video")
		w.(http.Flusher).Flush()
		<-req.Context().Done()
	}))
	defer server.Close()
	c := newRuntimeTestCrawler(t, fmt.Sprintf(`c=read(); send(c,"page",items=[dict(discovery_key="one",locator={}),dict(discovery_key="two",locator={})],next_cursor=None)
c=read(); send(c,"item",discovery_key="one",source_id="one",title="Partial",media=dict(type="url",url=%q))
time.sleep(30)
`, server.URL+"/video.mp4"), ProtocolV3, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	task, err := c.Prepare(ctx, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var result *crawljob.Result
	var runErr error
	go func() {
		result, runErr = c.RunTask(ctx, task, nil)
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	path := filepath.Join(c.cfg.Driver.VideosDir(), mediaFileStem("one")+".mp4")
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	waiting := true
	for waiting {
		select {
		case <-done:
			t.Fatalf("download finished early: %+v %v", result, runErr)
		case <-deadline.C:
			t.Fatal("download did not create a partial file")
		case <-ticker.C:
			info, err := os.Stat(path + ".part")
			waiting = err != nil || info.Size() == 0
		}
	}
	if !c.CancelTask(task.Result.TaskID) {
		t.Fatal("could not stop the partial download")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("partial download did not stop promptly")
	}
	if !errors.Is(runErr, context.Canceled) || result.State != "canceled" || result.NewVideos != 0 || result.ResolveCalls != 1 {
		t.Fatalf("result=%+v err=%v", result, runErr)
	}
	for _, file := range []string{path, path + ".part", task.dir} {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatalf("incomplete file retained: %s %v", file, err)
		}
	}
	known, err := c.cfg.Catalog.KnownCrawlerCandidates(context.Background(), result.DriveID, []catalog.CrawlerIdentity{{DiscoveryKey: "one"}})
	if err != nil || known["one"] {
		t.Fatalf("partial download marked as imported: %v %v", known, err)
	}
}

func TestCrawlerCompletedDownloadSurvivesAcquisitionDeadline(t *testing.T) {
	c := newRuntimeTestCrawler(t, `c=read(); send(c,"page",items=[dict(discovery_key="one",locator={})],next_cursor=None)
c=read(); send(c,"item",discovery_key="one",source_id="one",title="Complete",media=dict(type="url",url="http://download.test/video.mp4"))
time.sleep(30)
`, ProtocolV3, func(cfg *CrawlerConfig) { cfg.RunTimeout = 500 * time.Millisecond })
	c.Importer.cfg.HTTPClient = &http.Client{Transport: importRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: importResponseBody{
			Reader: strings.NewReader("complete-video"), onClose: func() { <-req.Context().Done() },
		}}, nil
	})}
	task, err := c.Prepare(context.Background(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	completed := false
	r, err := c.RunTask(context.Background(), task, func(ctx context.Context, task *Task) error {
		completed = true
		if _, hasDeadline := ctx.Deadline(); hasDeadline || ctx.Err() != nil {
			t.Fatal("completion inherited the expired acquisition budget")
		}
		return nil
	})
	if err != nil || r.NewVideos != 1 || r.StopReason != "time_limit" || r.Failed != 0 || len(r.Issues) != 0 || !completed {
		t.Fatalf("result=%+v completed=%v err=%v", r, completed, err)
	}
	stored, err := c.cfg.Catalog.GetCrawlerTask(context.Background(), r.DriveID, r.TaskID)
	if err != nil || stored.NewVideos != 1 || len(stored.ImportedVideoIDs) != 1 || stored.StopReason != "time_limit" || stored.Failed != 0 {
		t.Fatalf("completed download lost at time limit: %+v %v", stored, err)
	}
}

func TestCrawlerPauseDuringCompletionPreservesFailuresAndHardCancellation(t *testing.T) {
	for _, mode := range []string{"pause", "upload_failure", "hard_cancel"} {
		t.Run(mode, func(t *testing.T) {
			mediaURL := serveScriptCrawlerMedia(t, "new video")
			c := newRuntimeTestCrawler(t, fmt.Sprintf(`c=read();send(c,"page",items=[dict(discovery_key="one",locator={})],next_cursor=None)
c=read();send(c,"item",discovery_key="one",source_id="one",title="New",media=dict(type="url",url=%q));stop()
`, mediaURL), ProtocolV3, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			task, err := c.Prepare(ctx, 1, "")
			if err != nil {
				t.Fatal(err)
			}
			uploadFailure := errors.New("upload unavailable")
			r, err := c.RunTask(ctx, task, func(ctx context.Context, task *Task) error {
				return task.RunStage(ctx, "upload", func(ctx context.Context) error {
					c.CancelTask(task.Result.TaskID)
					if mode == "hard_cancel" {
						cancel()
						if !errors.Is(ctx.Err(), context.Canceled) {
							t.Error("hard cancellation did not reach completion")
						}
						return ctx.Err()
					}
					if ctx.Err() != nil {
						t.Fatal("pause interrupted upload")
					}
					if mode == "upload_failure" {
						return uploadFailure
					}
					return nil
				})
			})
			if !errors.Is(err, context.Canceled) || r.State != "canceled" || r.NewVideos != 1 || !r.StopRequested {
				t.Fatalf("result=%+v err=%v", r, err)
			}
			if mode == "upload_failure" {
				if !errors.Is(err, uploadFailure) || len(r.Issues) < 2 || r.Issues[0].Stage != "upload" || r.Issues[0].Message != uploadFailure.Error() {
					t.Fatalf("lost completion failure: %+v %v", r, err)
				}
			}
		})
	}
}

func TestCrawlerCompletionCanOutlastAcquisitionBudget(t *testing.T) {
	c := newRuntimeTestCrawler(t, `c=read();send(c,"page",items=[],next_cursor=None);stop()`, ProtocolV3,
		func(cfg *CrawlerConfig) { cfg.RunTimeout = 500 * time.Millisecond })
	task, err := c.Prepare(context.Background(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.RunTask(context.Background(), task, func(ctx context.Context, task *Task) error {
		return task.RunStage(ctx, "upload", func(ctx context.Context) error {
			timer := time.NewTimer(600 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		})
	})
	if err != nil || r.State != "completed" || r.StopReason != "source_exhausted" {
		t.Fatalf("completion was cut short or relabeled as timed out: %+v %v", r, err)
	}
}
