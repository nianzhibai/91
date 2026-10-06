package mediaimport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/video-site/backend/internal/catalog"
)

func TestManagerDownloadsValidatesAndFinalizesVideo(t *testing.T) {
	uploaded := make(chan *catalog.Video, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Disposition", `attachment; filename="remote-clip.mp4"`)
		_, _ = w.Write([]byte("downloaded-video-bytes"))
	}))
	defer server.Close()

	manager, cat, uploadDir := newTestManager(t, Config{
		OnVideoUploaded: func(video *catalog.Video) {
			uploaded <- video
		},
	})
	if _, err := cat.CreateTagAndClassify(context.Background(), "奶子", "user"); err != nil {
		t.Fatal(err)
	}
	manager.client = server.Client()
	startTestManager(t, manager)

	job, err := manager.Create(context.Background(), CreateInput{
		URL:  server.URL + "/asset?signature=private-token",
		Tags: []string{"奶子"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	completed := waitForRemoteJob(t, cat, job.ID, catalog.RemoteUploadCompleted)
	if completed.SourceURL != "" || completed.ResolvedTitle != "remote-clip" {
		t.Fatalf("completed job = %#v", completed)
	}
	if completed.BytesDownloaded != int64(len("downloaded-video-bytes")) ||
		completed.TotalBytes != completed.BytesDownloaded {
		t.Fatalf("progress = %d/%d", completed.BytesDownloaded, completed.TotalBytes)
	}

	video, err := cat.GetVideo(context.Background(), completed.CompletedVideoID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if video.Title != "remote-clip" ||
		video.Author != "" ||
		video.Ext != "mp4" ||
		len(video.Tags) != 1 ||
		video.Tags[0] != "奶子" {
		t.Fatalf("video = %#v", video)
	}
	body, err := os.ReadFile(filepath.Join(uploadDir, video.FileID))
	if err != nil {
		t.Fatalf("read final file: %v", err)
	}
	if string(body) != "downloaded-video-bytes" {
		t.Fatalf("final body = %q", body)
	}
	select {
	case callbackVideo := <-uploaded:
		if callbackVideo.ID != video.ID {
			t.Fatalf("callback video = %#v", callbackVideo)
		}
	case <-time.After(time.Second):
		t.Fatal("OnVideoUploaded was not called")
	}
	assertNoRemoteParts(t, uploadDir)
}

func TestManagerRunsOneFIFORequestAtATime(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	firstStarted := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		defer active.Add(-1)
		if strings.Contains(r.URL.Path, "first") {
			firstStarted <- struct{}{}
			select {
			case <-releaseFirst:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("video"))
	}))
	defer server.Close()

	manager, cat, _ := newTestManager(t, Config{})
	manager.client = server.Client()
	startTestManager(t, manager)
	first, err := manager.Create(context.Background(), CreateInput{
		URL:   server.URL + "/first.mp4",
		Title: "first",
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first request did not start")
	}
	second, err := manager.Create(context.Background(), CreateInput{
		URL:   server.URL + "/second.mp4",
		Title: "second",
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := maximum.Load(); got != 1 {
		t.Fatalf("maximum concurrent downloads = %d, want 1", got)
	}
	close(releaseFirst)
	waitForRemoteJob(t, cat, first.ID, catalog.RemoteUploadCompleted)
	waitForRemoteJob(t, cat, second.ID, catalog.RemoteUploadCompleted)
	if got := maximum.Load(); got != 1 {
		t.Fatalf("maximum concurrent downloads = %d, want 1", got)
	}
}

func TestManagerCancellationAbortsRequestAndCleansPart(t *testing.T) {
	requestStarted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		requestStarted <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()

	manager, cat, uploadDir := newTestManager(t, Config{
		IdleTimeout: 5 * time.Second,
	})
	manager.client = server.Client()
	startTestManager(t, manager)
	job, err := manager.Create(context.Background(), CreateInput{
		URL:   server.URL + "/slow.mp4?token=secret",
		Title: "slow",
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("download did not start")
	}
	if _, err := manager.Cancel(context.Background(), job.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	canceled := waitForRemoteJob(t, cat, job.ID, catalog.RemoteUploadCanceled)
	if canceled.SourceURL != "" || canceled.CanCancel() {
		t.Fatalf("canceled job = %#v", canceled)
	}
	assertNoRemoteParts(t, uploadDir)
}

func TestManagerFailsSafelyWhenDiskReserveWouldBeConsumed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("video-data"))
	}))
	defer server.Close()

	manager, cat, uploadDir := newTestManager(t, Config{})
	manager.client = server.Client()
	manager.availableBytes = func(string) (int64, error) {
		return manager.diskReserve, nil
	}
	startTestManager(t, manager)
	job, err := manager.Create(context.Background(), CreateInput{
		URL:   server.URL + "/video.mp4",
		Title: "disk-full",
	})
	if err != nil {
		t.Fatal(err)
	}
	failed := waitForRemoteJob(t, cat, job.ID, catalog.RemoteUploadFailed)
	if !strings.Contains(failed.ErrorMessage, "磁盘空间不足") ||
		failed.SourceURL != "" {
		t.Fatalf("failed job = %#v", failed)
	}
	assertNoRemoteParts(t, uploadDir)
}

func TestManagerFailsAfterBodyIdleTimeoutWithoutLeakingURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	manager, cat, _ := newTestManager(t, Config{IdleTimeout: 40 * time.Millisecond})
	manager.client = server.Client()
	startTestManager(t, manager)
	job, err := manager.Create(context.Background(), CreateInput{
		URL:   server.URL + "/idle.mp4?token=should-never-appear",
		Title: "idle",
	})
	if err != nil {
		t.Fatal(err)
	}
	failed := waitForRemoteJob(t, cat, job.ID, catalog.RemoteUploadFailed)
	if !strings.Contains(failed.ErrorMessage, "未发送数据") {
		t.Fatalf("failure = %q", failed.ErrorMessage)
	}
	if strings.Contains(failed.ErrorMessage, "token") ||
		strings.Contains(failed.ErrorMessage, "should-never-appear") ||
		failed.SourceURL != "" {
		t.Fatalf("job leaked URL data: %#v", failed)
	}
}

func TestManagerStartupRemovesOldPartAndRestartsInterruptedJob(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("fresh-download"))
	}))
	defer server.Close()

	manager, cat, uploadDir := newTestManager(t, Config{})
	manager.client = server.Client()
	ctx := context.Background()
	job, err := cat.CreateRemoteUploadJob(
		ctx,
		"remote-recovery",
		server.URL+"/recovery.mp4",
		"public.example/recovery.mp4",
		"recovered",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ClaimNextRemoteUploadJob(ctx); err != nil {
		t.Fatal(err)
	}
	oldPart := ".remote-recovery-old.part"
	if err := os.WriteFile(filepath.Join(uploadDir, oldPart), []byte("stale-prefix"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cat.SetRemoteUploadTempFile(ctx, job.ID, oldPart); err != nil {
		t.Fatal(err)
	}

	startTestManager(t, manager)
	completed := waitForRemoteJob(t, cat, job.ID, catalog.RemoteUploadCompleted)
	if _, err := os.Stat(filepath.Join(uploadDir, oldPart)); !os.IsNotExist(err) {
		t.Fatalf("old part still exists: %v", err)
	}
	video, err := cat.GetVideo(ctx, completed.CompletedVideoID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(uploadDir, video.FileID))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "fresh-download" {
		t.Fatalf("recovered download body = %q", body)
	}
}

func TestManagerShutdownRequeuesRunningJobFromByteZero(t *testing.T) {
	requestStarted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		requestStarted <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()

	manager, cat, uploadDir := newTestManager(t, Config{IdleTimeout: 5 * time.Second})
	manager.client = server.Client()
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, err := manager.Create(context.Background(), CreateInput{
		URL:   server.URL + "/restart.mp4?token=retained-in-db-only",
		Title: "restart",
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	requeued, err := cat.GetRemoteUploadJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if requeued.State != catalog.RemoteUploadQueued ||
		requeued.BytesDownloaded != 0 ||
		requeued.SourceURL == "" ||
		!requeued.StartedAt.IsZero() {
		t.Fatalf("requeued job = %#v", requeued)
	}
	assertNoRemoteParts(t, uploadDir)
}

func newTestManager(
	t *testing.T,
	cfg Config,
) (*Manager, *catalog.Catalog, string) {
	t.Helper()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	uploadDir := t.TempDir()
	cfg.Catalog = cat
	cfg.UploadDir = uploadDir
	manager, err := New(cfg)
	if err != nil {
		cat.Close()
		t.Fatalf("new manager: %v", err)
	}
	manager.validateURL = testURLValidator
	manager.availableBytes = func(string) (int64, error) {
		return 1 << 40, nil
	}
	manager.probeFile = func(
		context.Context,
		string,
		string,
	) (mediaInfo, error) {
		return mediaInfo{
			FormatName:  "mov,mp4,m4a,3gp,3g2,mj2",
			VideoCodecs: []string{"h264"},
		}, nil
	}
	t.Cleanup(func() {
		_ = cat.Close()
	})
	return manager, cat, uploadDir
}

func startTestManager(t *testing.T, manager *Manager) {
	t.Helper()
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	})
}

func testURLValidator(_ context.Context, raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, validationError("invalid test URL")
	}
	if err := validateURLShape(u); err != nil {
		return nil, err
	}
	u.Fragment = ""
	return u, nil
}

func waitForRemoteJob(
	t *testing.T,
	cat *catalog.Catalog,
	id, state string,
) *catalog.RemoteUploadJob {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, err := cat.GetRemoteUploadJob(context.Background(), id)
		if err == nil && job.State == state {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, err := cat.GetRemoteUploadJob(context.Background(), id)
	t.Fatalf("job %s did not reach %s: job=%#v err=%v", id, state, job, err)
	return nil
}

func assertNoRemoteParts(t *testing.T, uploadDir string) {
	t.Helper()
	entries, err := os.ReadDir(uploadDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".remote-") &&
			strings.HasSuffix(entry.Name(), ".part") {
			t.Fatalf("stale part file: %s", entry.Name())
		}
	}
}
