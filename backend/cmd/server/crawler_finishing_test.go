package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
	"github.com/nianzhibai/91/backend/internal/crawlerupload"
	"github.com/nianzhibai/91/backend/internal/crawljob"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/drives/scriptcrawler"
	"github.com/nianzhibai/91/backend/internal/fingerprint"
	"github.com/nianzhibai/91/backend/internal/preview"
	"github.com/nianzhibai/91/backend/internal/proxy"
)

type finishingRoundTripper func(*http.Request) (*http.Response, error)

func (f finishingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type finishingResponseBody struct {
	io.ReadCloser
	afterClose func()
}

func (b finishingResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.afterClose()
	return err
}

type finishingUploadDrive struct {
	serverFakeKindDrive
	beforeUpload func(context.Context) error
	payload      []byte
}

func (d *finishingUploadDrive) EnsureDir(context.Context, string) (string, error) {
	return "uploads", nil
}
func (d *finishingUploadDrive) Rename(context.Context, string, string) error { return nil }
func (d *finishingUploadDrive) UploadAndReportHash(ctx context.Context, _, _ string, r io.Reader, size int64) (crawlerupload.UploadResult, error) {
	if err := d.beforeUpload(ctx); err != nil {
		return crawlerupload.UploadResult{}, err
	}
	var err error
	d.payload, err = io.ReadAll(r)
	return crawlerupload.UploadResult{FileID: "uploaded-video", Size: size}, err
}

type finishingGenerator struct {
	serverFakeTeaserGenerator
	dir           string
	beforePreview func(context.Context) error
}

func (g *finishingGenerator) Generate(ctx context.Context, link *drives.StreamLink, duration float64) (string, error) {
	if err := g.beforePreview(ctx); err != nil {
		return "", err
	}
	return g.serverFakeTeaserGenerator.Generate(ctx, link, duration)
}
func (g *finishingGenerator) MoveToLocal(_ string, id string) (string, error) {
	path := filepath.Join(g.dir, id+".mp4")
	return path, os.WriteFile(path, []byte("preview"), 0o600)
}
func (g *finishingGenerator) GenerateThumbnail(ctx context.Context, _ *drives.StreamLink, id string, _ float64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	g.record("thumbnail:" + id)
	path := filepath.Join(g.dir, id+".jpg")
	return path, os.WriteFile(path, []byte("thumbnail"), 0o600)
}

func TestPausedCrawlerFinishesGenerationAndUpload(t *testing.T) {
	// Build a native probe stub once so every scenario works without a shell.
	probe := filepath.Join(t.TempDir(), "ffprobe")
	if runtime.GOOS == "windows" {
		probe += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", probe, "./testdata/ffprobe").CombinedOutput(); err != nil {
		t.Fatalf("build ffprobe stub: %v\n%s", err, out)
	}

	for _, mode := range []string{"import", "generation", "upload", "upload_failure", "local_only", "preview_disabled", "pending_config"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			root := t.TempDir()
			cat, err := catalog.Open(filepath.Join(root, "catalog.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cat.Close() })
			script := filepath.Join(root, "crawler.py")
			if err := os.WriteFile(script, []byte(`CRAWLER_NAME = "Finishing test"
CRAWLER_PROTOCOL = "crawler.v3"
import json, os, sys, time
job=json.load(open(sys.argv[sys.argv.index("--job")+1]))
def read(): return json.loads(sys.stdin.readline())
def send(c, kind, **fields): print(json.dumps(dict(type=kind,request_id=c["request_id"],**fields)),flush=True)
c=read();send(c,"page",items=[dict(discovery_key="one",locator={}),dict(discovery_key="two",locator={})],next_cursor=None)
c=read();send(c,"item",discovery_key="one",source_id="one",title="Complete",media=dict(type="url",url="http://download.test/video.mp4"))
c=read()
assert c["type"]=="stop", "must not resolve the next video"
send(c,"stopped")
`), 0o600); err != nil {
				t.Fatal(err)
			}
			source := scriptcrawler.New(scriptcrawler.Config{ID: "crawler", RootDir: filepath.Join(root, "source")})
			if err := source.Init(ctx); err != nil {
				t.Fatal(err)
			}
			target := &finishingUploadDrive{serverFakeKindDrive: serverFakeKindDrive{id: "target", kind: "test-upload"}}
			creds := map[string]string{"script_path": script, "target_new": "1"}
			if mode != "local_only" {
				creds["upload_drive_id"] = target.ID()
			}
			for _, d := range []*catalog.Drive{
				{ID: source.ID(), Kind: scriptcrawler.Kind, Name: "Crawler", RootID: "/", Credentials: creds},
				{ID: target.ID(), Kind: target.Kind(), RootID: "/"},
			} {
				if err := cat.UpsertDrive(ctx, d); err != nil {
					t.Fatal(err)
				}
			}
			registry := proxy.NewRegistry()
			registry.Set(source.ID(), source)
			registry.Set(target.ID(), target)
			app := &App{cat: cat, registry: registry, cfg: &config.Config{Storage: config.Storage{LocalPreviewDir: root}}}
			app.previewDisabled.Store(mode == "preview_disabled")
			workerCtx, workerCancel := context.WithCancel(ctx)
			gen := &finishingGenerator{dir: root}
			worker := preview.NewWorker(gen, cat, source)
			thumb := preview.NewThumbWorker(gen, cat, source)
			app.workers = map[string]*preview.Worker{source.ID(): worker}
			app.thumbWorkers = map[string]*preview.ThumbWorker{source.ID(): thumb}
			app.cancels = map[string]context.CancelFunc{source.ID(): workerCancel}
			var workers sync.WaitGroup
			workers.Add(2)
			go func() { defer workers.Done(); worker.Run(workerCtx) }()
			go func() { defer workers.Done(); thumb.Run(workerCtx) }()
			t.Cleanup(func() { workerCancel(); workers.Wait() })
			pause := func() error {
				if !app.stopDriveTasks(ctx, source.ID()) || !app.stopDriveTasks(ctx, source.ID()) {
					return errors.New("pause lost the active crawler task")
				}
				if !app.driveHasActiveWork(source.ID()) || workerCtx.Err() != nil {
					return errors.New("pause released task ownership or canceled workers")
				}
				return nil
			}
			gen.beforePreview = func(ctx context.Context) error {
				if mode == "generation" {
					return pause()
				}
				return ctx.Err()
			}
			var taskID, localFile string
			target.beforeUpload = func(uploadCtx context.Context) error {
				if mode == "upload" {
					if err := pause(); err != nil {
						return err
					}
				}
				if uploadCtx.Err() != nil {
					return uploadCtx.Err()
				}
				result, err := cat.GetCrawlerTask(uploadCtx, source.ID(), taskID)
				if err != nil {
					return err
				}
				if result.State != "running" || result.Stage != "upload" {
					return fmt.Errorf("task ended before upload: %+v", result)
				}
				v, err := cat.GetVideo(uploadCtx, result.ImportedVideoIDs[0])
				if err != nil {
					return err
				}
				localFile = v.FileID
				if v.ThumbnailURL == "" || (mode != "preview_disabled" && v.PreviewStatus != "ready") {
					return fmt.Errorf("upload preceded generation: %+v", v)
				}
				if mode == "upload_failure" {
					return errors.New("destination is unavailable")
				}
				return nil
			}
			crawler := scriptcrawler.NewCrawler(scriptcrawler.CrawlerConfig{
				Driver: source, Catalog: cat, ScriptPath: script, FFprobePath: probe,
				OnProgress: func(r crawljob.Result) {
					app.updateDriveScanProgress(source.ID(), r.Checked, r.NewVideos)
				},
				HTTPClient: &http.Client{Transport: finishingRoundTripper(func(req *http.Request) (*http.Response, error) {
					if err := req.Context().Err(); err != nil {
						return nil, err
					}
					body := finishingResponseBody{ReadCloser: io.NopCloser(strings.NewReader("complete-video")), afterClose: func() {
						// Closing the body happens after the complete download is committed.
						if mode == "generation" || mode == "upload" {
							return
						}
						if err := pause(); err != nil {
							t.Error(err)
						}
						if mode == "pending_config" {
							for _, id := range []string{source.ID(), target.ID()} {
								if _, err := app.activeDriveConfig(ctx, id); err != nil {
									t.Error(err)
								}
								gate := app.driveOperationGate(id)
								gate.mu.Lock()
								gate.pending = true
								gate.beginBlockedLocked()
								gate.mu.Unlock()
							}
						}
					}}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
				})},
			})
			app.scriptCrawlers = map[string]*scriptcrawler.Crawler{source.ID(): crawler}
			app.crawlerUploader = crawlerupload.New(crawlerupload.Config{Catalog: cat, Registry: registry, GetDrive: app.activeDriveConfig, PreviewEnabled: app.previewEnabled})
			taskCtx, accepted, task, release, err := app.acceptCrawlerTask(ctx, source.ID())
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			taskID = task.Result.TaskID
			result, runErr := app.executeCrawlerTask(taskCtx, source.ID(), accepted, task)
			if !errors.Is(runErr, context.Canceled) || result.State != "canceled" || result.NewVideos != 1 || !result.StopRequested {
				t.Fatalf("result=%+v err=%v", result, runErr)
			}
			v, err := cat.GetVideo(context.Background(), result.ImportedVideoIDs[0])
			if err != nil {
				t.Fatal(err)
			}
			if v.ThumbnailURL == "" || (mode != "preview_disabled" && v.PreviewStatus != "ready") {
				t.Fatalf("assets not completed: %+v", v)
			}
			if _, err := os.Stat(filepath.Join(root, v.ID+".jpg")); err != nil {
				t.Fatal(err)
			}
			if mode != "preview_disabled" {
				if _, err := os.Stat(filepath.Join(root, v.PreviewLocal)); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "upload_failure" || mode == "local_only" {
				if v.DriveID != source.ID() {
					t.Fatalf("failed/local video migrated: %+v", v)
				}
				data, err := os.ReadFile(filepath.Join(source.VideosDir(), v.FileID))
				if err != nil || string(data) != "complete-video" {
					t.Fatalf("local file lost: %q %v", data, err)
				}
			} else {
				if v.DriveID != target.ID() || string(target.payload) != "complete-video" {
					t.Fatalf("video not uploaded: %+v payload=%q", v, target.payload)
				}
				if _, err := os.Stat(filepath.Join(source.VideosDir(), localFile)); !os.IsNotExist(err) {
					t.Fatalf("migrated local file retained: %v", err)
				}
			}
			if mode == "upload_failure" {
				results, err := cat.LatestCrawlerUploadResults(context.Background())
				if err != nil || results[source.ID()].State != "failed" || results[source.ID()].FailedCount != 1 {
					t.Fatalf("upload failure missing: %+v %v", results, err)
				}
				if len(result.Issues) < 2 || result.Issues[0].Stage != "upload" {
					t.Fatalf("crawl hid upload failure: %+v", result)
				}
			}
			release()
			if app.driveHasActiveWork(source.ID()) {
				t.Fatal("finished task still owns source drive")
			}
		})
	}
}

func TestFailedCrawlerProcessesBacklogAndRestoreRequests(t *testing.T) {
	for _, uploadFails := range []bool{false, true} {
		t.Run(fmt.Sprint(uploadFails), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			root := t.TempDir()
			cat, err := catalog.Open(filepath.Join(root, "catalog.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cat.Close() })
			script := filepath.Join(root, "crawler.py")
			if err := os.WriteFile(script, []byte(`CRAWLER_NAME = "Backlog test"
CRAWLER_PROTOCOL = "crawler.v3"
import json, sys
c=json.loads(sys.stdin.readline())
print(json.dumps(dict(type="error", request_id=c["request_id"], scope="source", code="source_unavailable", message="site unavailable", retryable=False)),flush=True)
`), 0o600); err != nil {
				t.Fatal(err)
			}
			source := scriptcrawler.New(scriptcrawler.Config{ID: "crawler", RootDir: filepath.Join(root, "source")})
			if err := source.Init(ctx); err != nil {
				t.Fatal(err)
			}
			target := &finishingUploadDrive{serverFakeKindDrive: serverFakeKindDrive{id: "target", kind: "test-upload"}}
			for _, d := range []*catalog.Drive{
				{ID: source.ID(), Kind: scriptcrawler.Kind, Name: "Crawler", RootID: "/", Credentials: map[string]string{"script_path": script, "upload_drive_id": target.ID()}},
				{ID: target.ID(), Kind: target.Kind(), RootID: "/"},
			} {
				if err := cat.UpsertDrive(ctx, d); err != nil {
					t.Fatal(err)
				}
			}
			for _, id := range []string{"backlog", "restore"} {
				payload := []byte(id + "-video")
				if err := os.WriteFile(filepath.Join(source.VideosDir(), id+".mp4"), payload, 0o600); err != nil {
					t.Fatal(err)
				}
				v := &catalog.Video{ID: scriptcrawler.BuildVideoID(source.ID(), id), DriveID: source.ID(), FileID: id + ".mp4", FileName: id + ".mp4", Title: id, Size: int64(len(payload)), Ext: "mp4", PreviewStatus: "pending", FingerprintStatus: "pending"}
				if err := cat.UpsertVideo(ctx, v); err != nil {
					t.Fatal(err)
				}
			}
			restoreID := scriptcrawler.BuildVideoID(source.ID(), "restore")
			if err := cat.DeleteVideoWithTombstone(ctx, restoreID); err != nil {
				t.Fatal(err)
			}
			if err := cat.RemoveDeletedVideo(ctx, restoreID); err != nil {
				t.Fatal(err)
			}
			registry := proxy.NewRegistry()
			registry.Set(source.ID(), source)
			registry.Set(target.ID(), target)
			app := &App{cat: cat, registry: registry, cfg: &config.Config{Storage: config.Storage{LocalPreviewDir: root}}}
			gen := &finishingGenerator{dir: root, beforePreview: func(ctx context.Context) error { return ctx.Err() }}
			worker := preview.NewWorker(gen, cat, source)
			thumb := preview.NewThumbWorker(gen, cat, source)
			fp := fingerprint.NewWorker(cat, source, fingerprint.Config{})
			app.workers = map[string]*preview.Worker{source.ID(): worker}
			app.thumbWorkers = map[string]*preview.ThumbWorker{source.ID(): thumb}
			app.fingerprintWorkers = map[string]*fingerprint.Worker{source.ID(): fp}
			workerCtx, workerCancel := context.WithCancel(ctx)
			var workers sync.WaitGroup
			for _, run := range []func(context.Context){worker.Run, thumb.Run, fp.Run} {
				workers.Add(1)
				go func(run func(context.Context)) { defer workers.Done(); run(workerCtx) }(run)
			}
			t.Cleanup(func() { workerCancel(); workers.Wait() })
			var taskID string
			backlogID := scriptcrawler.BuildVideoID(source.ID(), "backlog")
			target.beforeUpload = func(ctx context.Context) error {
				task, err := cat.GetCrawlerTask(ctx, source.ID(), taskID)
				if err != nil {
					return err
				}
				v, err := cat.GetVideo(ctx, backlogID)
				if err != nil {
					return err
				}
				if task.State != "running" || task.Stage != "upload" || task.NewVideos != 0 || v.PreviewStatus != "ready" || v.ThumbnailURL == "" || v.FingerprintStatus != "ready" {
					t.Errorf("backlog upload preceded generation or changed crawl counts: task=%+v video=%+v", task, v)
				}
				if uploadFails {
					return errors.New("destination unavailable")
				}
				return ctx.Err()
			}
			crawler := scriptcrawler.NewCrawler(scriptcrawler.CrawlerConfig{Driver: source, Catalog: cat, ScriptPath: script})
			app.scriptCrawlers = map[string]*scriptcrawler.Crawler{source.ID(): crawler}
			app.crawlerUploader = crawlerupload.New(crawlerupload.Config{Catalog: cat, Registry: registry, GetDrive: app.activeDriveConfig, PreviewEnabled: app.previewEnabled})
			taskCtx, accepted, task, release, err := app.acceptCrawlerTask(ctx, source.ID())
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			taskID = task.Result.TaskID
			result, runErr := app.executeCrawlerTask(taskCtx, source.ID(), accepted, task)
			var sourceErr *scriptcrawler.ScriptError
			if !errors.As(runErr, &sourceErr) || result.State != "failed" || result.StopReason != "source_error" || result.NewVideos != 0 {
				t.Fatalf("completion lost the source failure: %+v %v", result, runErr)
			}
			uploads, err := cat.CrawlerTaskUploads(ctx, taskID)
			if err != nil || len(uploads) != 1 {
				t.Fatalf("backlog upload not tracked: %+v %v", uploads, err)
			}
			if uploadFails {
				if uploads[0].State != "failed" || result.Failed != 2 || result.Issues[0].Stage != "upload" {
					t.Fatalf("completion hid upload failure: result=%+v uploads=%+v", result, uploads)
				}
			} else if uploads[0].UploadedCount != 1 || string(target.payload) != "backlog-video" {
				t.Fatalf("backlog was not uploaded: %+v payload=%q", uploads, target.payload)
			}
			if _, err := cat.GetVideo(ctx, restoreID); err != nil {
				t.Fatalf("source/upload failure prevented restoration: %v", err)
			}
			requests, err := cat.ListCrawlerRestoreRequests(ctx, source.ID())
			if err != nil || len(requests) != 0 {
				t.Fatalf("restoration was not completed: %+v %v", requests, err)
			}
			if err := app.waitDriveGenerationQueuesIdle(ctx, source.ID()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
