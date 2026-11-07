package preview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
)

func TestThumbWorkerUpdatesThumbnailAndDurationWithoutChangingPreviewStatus(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "thumb-worker-video")

	gen := &fakeThumbGenerator{probeDuration: 42}
	drv := &previewFakeDrive{}
	worker := NewThumbWorker(gen, cat, drv)

	worker.process(ctx, video)

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.ThumbnailURL != "/p/thumb/"+video.ID {
		t.Fatalf("thumbnail = %q, want generated thumb URL", got.ThumbnailURL)
	}
	if got.PreviewStatus != "pending" {
		t.Fatalf("preview status = %q, want pending", got.PreviewStatus)
	}
	if got.DurationSeconds != 42 {
		t.Fatalf("duration = %d, want probed duration", got.DurationSeconds)
	}
	if gen.thumbnailVideoID != video.ID {
		t.Fatalf("thumbnail video id = %q, want %q", gen.thumbnailVideoID, video.ID)
	}
	if gen.thumbnailDuration != 42 {
		t.Fatalf("thumbnail duration = %.1f, want probed duration", gen.thumbnailDuration)
	}
	if gen.probeCalls != 1 {
		t.Fatalf("probe calls = %d, want 1 for thumbnail generation", gen.probeCalls)
	}
	if drv.streamFileID != video.FileID {
		t.Fatalf("stream file id = %q, want %q", drv.streamFileID, video.FileID)
	}
}

func TestThumbWorkerDefersDurationUntilFinalBackfill(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "thumb-worker-existing-thumbnail")
	video.ThumbnailURL = "/p/thumb/" + video.ID
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("update video: %v", err)
	}

	gen := &fakeThumbGenerator{probeDuration: 19}
	drv := &previewFakeDrive{}
	worker := NewThumbWorker(gen, cat, drv)

	worker.process(ctx, video)
	if gen.probeCalls != 0 {
		t.Fatal("ordinary cover work probed existing thumbnail duration")
	}
	worker.processDuration(ctx, video)

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.DurationSeconds != 19 {
		t.Fatalf("duration = %d, want probed duration", got.DurationSeconds)
	}
	if got.ThumbnailURL != "/p/thumb/"+video.ID {
		t.Fatalf("thumbnail = %q, want unchanged existing thumbnail", got.ThumbnailURL)
	}
	ready, err := cat.ListVideosByThumbnailStatus(ctx, video.DriveID, "ready", 0)
	if err != nil {
		t.Fatalf("list ready thumbnails: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != video.ID {
		t.Fatalf("ready thumbnails = %#v, want only %s", ready, video.ID)
	}
	if gen.probeCalls != 1 {
		t.Fatalf("probe calls = %d, want 1", gen.probeCalls)
	}
	if gen.thumbnailVideoID != "" {
		t.Fatalf("thumbnail generation video id = %q, want no regeneration", gen.thumbnailVideoID)
	}
}

func TestThumbWorkerGeneratesThumbnailForCrawlerLikeVideoID(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "scriptcrawler-crawler-main-source001")

	gen := &fakeThumbGenerator{probeDuration: 42}
	drv := &previewFakeDrive{kind: "pikpak"}
	worker := NewThumbWorker(gen, cat, drv)

	worker.process(ctx, video)

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.ThumbnailURL != "/p/thumb/"+video.ID {
		t.Fatalf("thumbnail = %q, want generated thumb URL", got.ThumbnailURL)
	}
	ready, err := cat.ListVideosByThumbnailStatus(ctx, video.DriveID, "ready", 0)
	if err != nil {
		t.Fatalf("list ready thumbnails: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != video.ID {
		t.Fatalf("ready thumbnails = %#v, want only %s", ready, video.ID)
	}
	if gen.probeCalls != 1 || gen.generateCalls != 1 {
		t.Fatalf("generator calls probe=%d generate=%d, want one thumbnail generation", gen.probeCalls, gen.generateCalls)
	}
}

func TestDurationBackfillFailurePreservesReadyThumbnail(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "thumb-worker-existing-thumbnail-probe-fails")
	video.ThumbnailURL = "/p/thumb/" + video.ID
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("update video: %v", err)
	}

	gen := &fakeThumbGenerator{probeErr: errors.New("invalid data found when processing input")}
	drv := &previewFakeDrive{}
	worker := NewThumbWorker(gen, cat, drv)

	worker.processDuration(ctx, video)

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.ThumbnailURL != "/p/thumb/"+video.ID {
		t.Fatalf("thumbnail = %q, want unchanged existing thumbnail", got.ThumbnailURL)
	}
	if got.DurationSeconds != 0 {
		t.Fatalf("duration = %d, want still unknown", got.DurationSeconds)
	}
	ready, err := cat.ListVideosByThumbnailStatus(ctx, video.DriveID, "ready", 0)
	if err != nil {
		t.Fatalf("list ready thumbnails: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != video.ID {
		t.Fatalf("ready thumbnails = %#v, want only %s", ready, video.ID)
	}
	worker.process(ctx, video)
	if gen.probeCalls != 1 {
		t.Fatalf("ordinary cover work repeated duration probe %d times", gen.probeCalls)
	}
	missing, err := cat.CountVideosNeedingThumbnail(ctx, video.DriveID)
	if err != nil {
		t.Fatalf("count videos needing thumbnail: %v", err)
	}
	if missing != 0 {
		t.Fatalf("missing thumbnails = %d, want 0 for an existing cover", missing)
	}
}

func TestThumbWorkerUsesOriginalVideoWhenLocalPreviewExists(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "thumb-worker-original-with-local-preview")
	localPreview := filepath.Join(t.TempDir(), "preview.mp4")
	if err := os.WriteFile(localPreview, []byte("preview"), 0o644); err != nil {
		t.Fatalf("write local preview: %v", err)
	}
	video.PreviewLocal = localPreview
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("update video: %v", err)
	}

	gen := &fakeThumbGenerator{probeDuration: 42}
	drv := &previewFakeDrive{}
	worker := NewThumbWorker(gen, cat, drv)

	worker.process(ctx, video)

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.ThumbnailURL != "/p/thumb/"+video.ID {
		t.Fatalf("thumbnail = %q, want generated thumb URL", got.ThumbnailURL)
	}
	if gen.thumbnailURL != "https://video.example/clip.mp4" {
		t.Fatalf("thumbnail source = %q, want original video stream", gen.thumbnailURL)
	}
	if gen.thumbnailDuration != 42 {
		t.Fatalf("thumbnail duration = %.1f, want original video duration", gen.thumbnailDuration)
	}
	if gen.probeCalls != 1 || drv.streamCalls != 1 {
		t.Fatalf("remote work probe=%d stream=%d, want original-source probe and generation", gen.probeCalls, drv.streamCalls)
	}
}

func TestPreviewWorkerGeneratesTeaserWithoutReplacingExistingThumbnail(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "preview-worker-video")
	video.ThumbnailURL = "https://thumbnail.example/original.jpg"
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("update video: %v", err)
	}

	gen := &fakeTeaserGenerator{}
	drv := &previewFakeDrive{}
	worker := NewWorker(gen, cat, drv)

	worker.process(ctx, video)

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.ThumbnailURL != "https://thumbnail.example/original.jpg" {
		t.Fatalf("thumbnail = %q, want existing thumbnail unchanged", got.ThumbnailURL)
	}
	if got.PreviewStatus != "ready" {
		t.Fatalf("preview status = %q, want ready", got.PreviewStatus)
	}
	if got.PreviewLocal != video.ID+".mp4" {
		t.Fatalf("preview local = %q, want portable teaser filename", got.PreviewLocal)
	}
}

func TestPreviewWorkerNotifiesDependentThumbnailAfterReady(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "preview-ready-notifies-thumbnail")
	worker := NewWorker(&fakeTeaserGenerator{}, cat, &previewFakeDrive{})
	var notified *catalog.Video
	worker.OnPreviewReady = func(video *catalog.Video) {
		notified = video
	}

	worker.process(ctx, video)

	if notified == nil || notified.ID != video.ID {
		t.Fatalf("notified video = %#v, want %s", notified, video.ID)
	}
	if notified.PreviewStatus != "ready" || notified.PreviewLocal == "" {
		t.Fatalf("notified preview status=%q local=%q", notified.PreviewStatus, notified.PreviewLocal)
	}
}

func TestPreviewReadyFollowUpSurvivesRunningThumbnailAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cat, video := seedPreviewTestVideo(t, "preview-ready-running-thumbnail")
	gen := &followUpThumbGenerator{
		started:      make(chan int, 2),
		releaseFirst: make(chan struct{}),
	}
	worker := NewThumbWorker(gen, cat, &previewFakeDrive{})
	go worker.Run(ctx)
	if !worker.Enqueue(video) {
		t.Fatal("enqueue initial thumbnail returned false")
	}

	select {
	case call := <-gen.started:
		if call != 1 {
			t.Fatalf("first thumbnail call = %d, want 1", call)
		}
	case <-time.After(time.Second):
		t.Fatal("initial thumbnail generation did not start")
	}
	if err := cat.UpdatePreview(ctx, video.ID, "/tmp/preview.mp4", "ready"); err != nil {
		t.Fatalf("mark preview ready: %v", err)
	}
	ready := *video
	ready.PreviewLocal = "/tmp/preview.mp4"
	ready.PreviewStatus = "ready"
	if !worker.EnqueueFollowUp(&ready) {
		t.Fatal("enqueue preview-ready follow-up returned false")
	}
	close(gen.releaseFirst)

	select {
	case call := <-gen.started:
		if call != 2 {
			t.Fatalf("follow-up thumbnail call = %d, want 2", call)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("preview-ready follow-up was swallowed by queue deduplication")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := cat.GetVideo(ctx, video.ID)
		if err != nil {
			t.Fatalf("get video: %v", err)
		}
		if got.ThumbnailURL == "/p/thumb/"+video.ID {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("follow-up thumbnail did not become ready")
}

func TestPreviewWorkerDeduplicatesQueuedVideos(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "preview-dedupe-video")

	gen := &fakeTeaserGenerator{}
	drv := &previewFakeDrive{}
	worker := NewWorker(gen, cat, drv)

	if !worker.EnqueueBlocking(ctx, video) {
		t.Fatal("first enqueue returned false, want true")
	}
	if !worker.EnqueueBlocking(ctx, video) {
		t.Fatal("duplicate enqueue returned false, want idempotent success")
	}
	if got := worker.Status().QueueLength; got != 1 {
		t.Fatalf("queue length = %d, want 1 unique video", got)
	}

	queued := <-worker.ch
	if !worker.Enqueue(video) {
		t.Fatal("enqueue while the same video is reserved returned false, want idempotent success")
	}
	select {
	case <-worker.ch:
		t.Fatal("duplicate enqueue added another queued video")
	default:
	}

	worker.processQueued(ctx, queued)
	if !worker.Enqueue(video) {
		t.Fatal("enqueue after processing returned false, want true")
	}
}

func TestSingleDriveUsesGlobalPreviewConcurrency(t *testing.T) {
	ctx := context.Background()
	cat, first := seedPreviewTestVideo(t, "preview-concurrent-1")
	videos := []*catalog.Video{first}
	for i := 2; i <= 3; i++ {
		video := *first
		video.ID = fmt.Sprintf("preview-concurrent-%d", i)
		video.FileID = fmt.Sprintf("file-id-%d", i)
		if err := cat.UpsertVideo(ctx, &video); err != nil {
			t.Fatalf("seed video %d: %v", i, err)
		}
		videos = append(videos, &video)
	}

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseGenerator := func() {
		releaseOnce.Do(func() { close(release) })
	}
	gen := &blockingTeaserGenerator{
		started: make(chan struct{}, len(videos)),
		release: release,
	}
	worker := NewWorker(gen, cat, &concurrentPreviewDrive{})
	worker.Limiter.SetLimit(3)
	runCtx, cancel := context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() {
		worker.Run(runCtx)
		close(runDone)
	}()
	// seedPreviewTestVideo registered the catalog cleanup first. Test cleanups
	// run in reverse order, so this always stops the worker before the catalog
	// is closed, including assertion-failure paths.
	t.Cleanup(func() {
		cancel()
		releaseGenerator()
		select {
		case <-runDone:
		case <-time.After(time.Second):
			t.Error("preview worker did not stop during test cleanup")
		}
	})

	for _, video := range videos {
		if !worker.EnqueueBlocking(runCtx, video) {
			t.Fatalf("enqueue %s returned false", video.ID)
		}
	}
	for range videos {
		select {
		case <-gen.started:
		case <-time.After(2 * time.Second):
			cancel()
			t.Fatal("three preview tasks did not start concurrently")
		}
	}
	status := worker.Status()
	if status.State != "generating" || status.QueueLength != 0 {
		t.Fatalf("status while blocked = %#v, want three active tasks and no queued tasks", status)
	}

	releaseGenerator()
	waitCtx, waitCancel := context.WithTimeout(ctx, 2*time.Second)
	defer waitCancel()
	if err := worker.WaitIdle(waitCtx); err != nil {
		t.Fatalf("wait for concurrent worker: %v", err)
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
	for _, video := range videos {
		stored, err := cat.GetVideo(ctx, video.ID)
		if err != nil {
			t.Fatalf("get %s: %v", video.ID, err)
		}
		if stored.PreviewStatus != "ready" {
			t.Fatalf("preview status for %s = %q, want ready", video.ID, stored.PreviewStatus)
		}
	}
}

func TestPreviewWorkerAppliesGlobalConcurrencyUpdatesWhileRunning(t *testing.T) {
	ctx := context.Background()
	cat, first := seedPreviewTestVideo(t, "preview-resize-1")
	second := *first
	second.ID = "preview-resize-2"
	second.FileID = "preview-resize-file-2"
	if err := cat.UpsertVideo(ctx, &second); err != nil {
		t.Fatalf("seed second video: %v", err)
	}

	release := make(chan struct{})
	gen := &blockingTeaserGenerator{
		started: make(chan struct{}, 2),
		release: release,
	}
	worker := NewWorker(gen, cat, &concurrentPreviewDrive{})
	worker.Limiter.SetLimit(1)
	runCtx, cancel := context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() {
		worker.Run(runCtx)
		close(runDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(time.Second):
			t.Error("preview worker did not stop during test cleanup")
		}
	})

	if !worker.EnqueueBlocking(runCtx, first) || !worker.EnqueueBlocking(runCtx, &second) {
		t.Fatal("enqueue preview resize videos failed")
	}
	select {
	case <-gen.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first preview task did not start")
	}
	select {
	case <-gen.started:
		t.Fatal("second preview task started before concurrency increased")
	case <-time.After(100 * time.Millisecond):
	}

	worker.Limiter.SetLimit(2)
	select {
	case <-gen.started:
	case <-time.After(2 * time.Second):
		t.Fatal("second preview task did not start after concurrency increased")
	}
	close(release)

	waitCtx, waitCancel := context.WithTimeout(ctx, 2*time.Second)
	defer waitCancel()
	if err := worker.WaitIdle(waitCtx); err != nil {
		t.Fatalf("wait for resized worker: %v", err)
	}
}

func TestThumbWorkerDeduplicatesQueuedVideos(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "thumb-dedupe-video")

	gen := &fakeThumbGenerator{}
	drv := &previewFakeDrive{}
	worker := NewThumbWorker(gen, cat, drv)

	if !worker.Enqueue(video) {
		t.Fatal("first enqueue returned false, want true")
	}
	if !worker.Enqueue(video) {
		t.Fatal("duplicate enqueue returned false, want idempotent success")
	}
	if got := worker.Status().QueueLength; got != 1 {
		t.Fatalf("queue length = %d, want 1 unique video", got)
	}

	queued := <-worker.ch
	if !worker.Enqueue(video) {
		t.Fatal("enqueue while the same thumbnail is reserved returned false, want idempotent success")
	}
	select {
	case <-worker.ch:
		t.Fatal("duplicate enqueue added another queued thumbnail")
	default:
	}

	worker.processQueued(ctx, queued)
	if !worker.Enqueue(video) {
		t.Fatal("enqueue after release returned false, want true")
	}
}

func TestPreviewWorkerRemovesPreviousLocalTeaserAfterNewTeaserIsReady(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "preview-cleanup-video")
	oldPath := filepath.Join(t.TempDir(), "old-teaser.mp4")
	if err := os.WriteFile(oldPath, []byte("old teaser"), 0o644); err != nil {
		t.Fatalf("write old teaser: %v", err)
	}
	video.PreviewLocal = filepath.Base(oldPath)
	video.PreviewStatus = "ready"
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("update video: %v", err)
	}

	gen := &fakeTeaserGenerator{
		localPath: filepath.Join(filepath.Dir(oldPath), "new-teaser.mp4"),
	}
	drv := &previewFakeDrive{}
	worker := NewWorker(gen, cat, drv)

	worker.process(ctx, video)

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old teaser still exists or stat failed with unexpected error: %v", err)
	}
	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.PreviewLocal != filepath.Base(gen.localPath) {
		t.Fatalf("preview local = %q, want %q", got.PreviewLocal, filepath.Base(gen.localPath))
	}
}

func TestPreviewWorkerNeverCallsDriveUploadOrEnsureDir(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "preview-local-only-video")
	localPath := filepath.Join(t.TempDir(), "local-only-teaser.mp4")
	gen := &fakeTeaserGenerator{localPath: localPath}
	drv := &previewFakeDrive{}
	worker := NewWorker(gen, cat, drv)

	worker.process(ctx, video)

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.PreviewStatus != "ready" {
		t.Fatalf("preview status = %q, want ready", got.PreviewStatus)
	}
	if got.PreviewLocal != filepath.Base(localPath) {
		t.Fatalf("preview local = %q, want %q", got.PreviewLocal, filepath.Base(localPath))
	}
	if got.PreviewFileID != "" {
		t.Fatalf("preview file id = %q, want empty for local-only teaser", got.PreviewFileID)
	}
	if drv.ensureDirCalls != 0 {
		t.Fatalf("ensure dir calls = %d, want 0 (teaser/cover must not write back to drive)", drv.ensureDirCalls)
	}
	if drv.uploadCalls != 0 {
		t.Fatalf("upload calls = %d, want 0 (teaser/cover must not write back to drive)", drv.uploadCalls)
	}
}

func TestPreviewWorkerGeneratesTeaserForLargeVideo(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "preview-large-video")
	video.Size = 6 * 1024 * 1024 * 1024
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("update video: %v", err)
	}

	gen := &fakeTeaserGenerator{}
	drv := &previewFakeDrive{}
	worker := NewWorker(gen, cat, drv)

	worker.process(ctx, video)

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.PreviewStatus != "ready" {
		t.Fatalf("preview status = %q, want ready", got.PreviewStatus)
	}
	if drv.streamCalls != 1 {
		t.Fatalf("stream calls = %d, want 1", drv.streamCalls)
	}
	if gen.generateCalls != 1 {
		t.Fatalf("generate calls = %d, want 1", gen.generateCalls)
	}
}

func TestPreviewWorkerDiscardsImplausibleStoredDuration(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "preview-implausible-duration")
	video.Size = 24_184_736
	video.DurationSeconds = 2_481_536
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("update video: %v", err)
	}

	gen := &fakeTeaserGenerator{}
	worker := NewWorker(gen, cat, &previewFakeDrive{})
	worker.process(ctx, video)

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.DurationSeconds != 0 {
		t.Fatalf("duration = %d, want corrupt metadata cleared", got.DurationSeconds)
	}
	if gen.generatedDuration != 0 {
		t.Fatalf("generation duration = %.1f, want unknown-duration safe plan", gen.generatedDuration)
	}
}

func TestPreviewWorkerRateLimitLeavesCurrentPendingAndSkipsNextVideo(t *testing.T) {
	ctx := context.Background()
	cat, first := seedPreviewTestVideo(t, "preview-rate-limit-1")
	second := *first
	second.ID = "preview-rate-limit-2"
	second.FileID = "file-id-2"
	if err := cat.UpsertVideo(ctx, &second); err != nil {
		t.Fatalf("seed second video: %v", err)
	}

	gen := &fakeTeaserGenerator{
		generateErr: &drives.RateLimitError{
			Provider:   "onedrive",
			RetryAfter: 2 * time.Hour,
			Err:        errors.New("429 Too Many Requests"),
		},
	}
	drv := &previewFakeDrive{}
	worker := NewWorker(gen, cat, drv)

	before := time.Now()
	worker.process(ctx, first)
	gotFirst, err := cat.GetVideo(ctx, first.ID)
	if err != nil {
		t.Fatalf("get first video: %v", err)
	}
	if gotFirst.PreviewStatus != "pending" {
		t.Fatalf("first preview status = %q, want pending after rate limit", gotFirst.PreviewStatus)
	}
	if gen.generateCalls != 1 {
		t.Fatalf("generate calls = %d, want 1", gen.generateCalls)
	}
	assertCooldownAround(t, worker.Status().CooldownUntil, before, 2*time.Hour)

	gen.generateErr = nil
	worker.process(ctx, &second)
	gotSecond, err := cat.GetVideo(ctx, second.ID)
	if err != nil {
		t.Fatalf("get second video: %v", err)
	}
	if gotSecond.PreviewStatus != "pending" {
		t.Fatalf("second preview status = %q, want pending while drive is cooling down", gotSecond.PreviewStatus)
	}
	if gen.generateCalls != 1 {
		t.Fatalf("generate calls = %d, want second video skipped during cooldown", gen.generateCalls)
	}
}

func TestPreviewWorkerRequeuesRecoverableFailureAfterCooldown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cat, video := seedPreviewTestVideo(t, "preview-rate-limit-requeue")
	gen := &fakeTeaserGenerator{generateErrs: []error{
		&drives.RateLimitError{
			Provider:   "p115",
			RetryAfter: time.Millisecond,
			Err:        errors.New("429 Too Many Requests"),
		},
		nil,
	}}
	worker := NewWorker(gen, cat, &previewFakeDrive{kind: "p115"})

	if !worker.EnqueueBlocking(ctx, video) {
		t.Fatal("enqueue returned false")
	}
	first := <-worker.ch
	worker.processQueued(ctx, first)

	var retry *catalog.Video
	select {
	case retry = <-worker.ch:
	case <-ctx.Done():
		t.Fatal("recoverable preview failure was not requeued")
	}
	worker.processQueued(ctx, retry)

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.PreviewStatus != "ready" {
		t.Fatalf("preview status = %q, want ready after retry", got.PreviewStatus)
	}
	if gen.generateCalls != 2 {
		t.Fatalf("generate calls = %d, want initial attempt and one retry", gen.generateCalls)
	}
	if got := worker.Status().QueueLength; got != 0 {
		t.Fatalf("queue length = %d, want empty after successful retry", got)
	}
}

func TestThumbWorkerRateLimitHonorsRetryAfter(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "thumb-rate-limit")

	gen := &fakeThumbGenerator{
		generateErr: &drives.RateLimitError{
			Provider:   "media source",
			RetryAfter: 2 * time.Hour,
			Err:        errors.New("429 Too Many Requests"),
		},
	}
	drv := &previewFakeDrive{}
	worker := NewThumbWorker(gen, cat, drv)

	before := time.Now()
	worker.process(ctx, video)

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.ThumbnailURL != "" {
		t.Fatalf("thumbnail = %q, want unchanged after rate limit", got.ThumbnailURL)
	}
	assertCooldownAround(t, worker.Status().CooldownUntil, before, 2*time.Hour)
}

func TestThumbWorkerProviderErrorsRespectThumbnailRetryLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cat, video := seedPreviewTestVideo(t, "thumb-provider-retry-limit")
	gen := &fakeThumbGenerator{
		probeDuration: 42,
		generateErr:   errors.New("Server returned 503 Service Unavailable"),
	}
	worker := NewThumbWorker(gen, cat, &previewFakeDrive{kind: "pikpak"})
	worker.RateLimitCooldown = time.Millisecond
	if !worker.EnqueueBlocking(ctx, video) {
		t.Fatal("thumbnail enqueue failed")
	}
	for attempt := 0; attempt < defaultThumbTransientMediaMaxFailures; attempt++ {
		select {
		case queued := <-worker.ch:
			worker.processQueued(ctx, queued)
		case <-ctx.Done():
			t.Fatal("thumbnail retry was not queued")
		}
	}
	if worker.Status().QueueLength != 0 || gen.generateCalls != defaultThumbTransientMediaMaxFailures {
		t.Fatal("thumbnail retry limit was not enforced")
	}
	failed, err := cat.ListVideosByThumbnailStatus(ctx, video.DriveID, "failed", 100)
	if err != nil || len(failed) != 1 {
		t.Fatalf("exhausted thumbnail did not fail: %v, %v", failed, err)
	}
}

func TestP115WAFStreamRateLimitKeepsPreviewPending(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "preview-p115-waf-405")
	drv := &previewFakeDrive{
		kind: "p115",
		streamErr: &drives.RateLimitError{
			Provider:   "p115",
			RetryAfter: time.Hour,
			Err:        errors.New(`<!doctypehtml><html><title>405</title><p>blocked</p>`),
		},
	}
	worker := NewWorker(&fakeTeaserGenerator{}, cat, drv)

	worker.process(ctx, video)
	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.PreviewStatus != "pending" {
		t.Fatalf("preview status = %q, want pending", got.PreviewStatus)
	}
	if worker.Status().CooldownUntil.IsZero() {
		t.Fatal("preview worker should enter cooldown")
	}
}

func TestP115WAFStreamRateLimitKeepsThumbnailPending(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "thumb-p115-waf-405")
	drv := &previewFakeDrive{
		kind: "p115",
		streamErr: &drives.RateLimitError{
			Provider:   "p115",
			RetryAfter: time.Hour,
			Err:        errors.New(`<!doctypehtml><html><title>405</title><p>blocked</p>`),
		},
	}
	worker := NewThumbWorker(&fakeThumbGenerator{}, cat, drv)

	retry := worker.process(ctx, video)
	pending, err := cat.ListVideosByThumbnailStatus(ctx, video.DriveID, "pending", 0)
	if err != nil {
		t.Fatalf("list pending thumbnails: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != video.ID {
		t.Fatalf("pending thumbnails = %#v, want only %s", pending, video.ID)
	}
	if !retry {
		t.Fatal("thumbnail should be requeued after cooldown")
	}
	if worker.Status().CooldownUntil.IsZero() {
		t.Fatal("thumbnail worker should enter cooldown")
	}
}

func TestThumbWorkerP115MessageOnlyErrorFailsWithoutCooldown(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "thumb-p115-message-only")

	gen := &fakeThumbGenerator{
		generateErr: errors.New("ffmpeg thumb: exit status 183, stderr: partial file Cannot determine format of input 0:0 after EOF"),
	}
	drv := &previewFakeDrive{kind: "p115"}
	worker := NewThumbWorker(gen, cat, drv)

	worker.process(ctx, video)

	failed, err := cat.ListVideosByThumbnailStatus(ctx, video.DriveID, "failed", 0)
	if err != nil {
		t.Fatalf("list failed thumbnails: %v", err)
	}
	if len(failed) != 1 || failed[0].ID != video.ID {
		t.Fatalf("failed thumbnails = %#v, want only %s", failed, video.ID)
	}
	if !worker.Status().CooldownUntil.IsZero() {
		t.Fatalf("cooldown until = %s, want no cooldown for message-only media error", worker.Status().CooldownUntil)
	}
	if gen.generateCalls != 2 || drv.streamCalls != 2 {
		t.Fatalf("calls generate=%d stream=%d, want one failure-driven link refresh before the terminal failure", gen.generateCalls, drv.streamCalls)
	}
}

func TestThumbWorkerDoesNotRequeueP115MessageOnlyError(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "thumb-p115-no-requeue")

	gen := &fakeThumbGenerator{
		generateErr: errors.New("ffmpeg thumb: partial file Cannot determine format of input 0:0 after EOF"),
	}
	drv := &previewFakeDrive{kind: "p115"}
	worker := NewThumbWorker(gen, cat, drv)

	worker.processQueued(ctx, video)

	select {
	case queued := <-worker.ch:
		t.Fatalf("unexpected requeued video id = %q", queued.ID)
	default:
	}

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.ThumbnailURL != "" {
		t.Fatalf("thumbnail = %q, want empty after message-only failure", got.ThumbnailURL)
	}
	failed, err := cat.ListVideosByThumbnailStatus(ctx, video.DriveID, "failed", 0)
	if err != nil {
		t.Fatalf("list failed thumbnails: %v", err)
	}
	if len(failed) != 1 || failed[0].ID != video.ID {
		t.Fatalf("failed thumbnails = %#v, want only %s", failed, video.ID)
	}
}

func TestThumbWorkerPikPakMoovAtomErrorFailsWithoutCooldown(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "thumb-pikpak-missing-moov")

	mediaErr := errors.New("ffprobe: exit status 1, stderr: moov atom not found Invalid data found when processing input")
	gen := &fakeThumbGenerator{
		probeErr:    mediaErr,
		generateErr: mediaErr,
	}
	drv := &previewFakeDrive{kind: "pikpak"}
	worker := NewThumbWorker(gen, cat, drv)

	worker.process(ctx, video)

	failed, err := cat.ListVideosByThumbnailStatus(ctx, video.DriveID, "failed", 0)
	if err != nil {
		t.Fatalf("list failed thumbnails: %v", err)
	}
	if len(failed) != 1 || failed[0].ID != video.ID {
		t.Fatalf("failed thumbnails = %#v, want only %s", failed, video.ID)
	}
	if !worker.Status().CooldownUntil.IsZero() {
		t.Fatalf("cooldown until = %s, want no cooldown for invalid PikPak MP4", worker.Status().CooldownUntil)
	}
	if gen.generateCalls != 1 {
		t.Fatalf("generate calls = %d, want 1", gen.generateCalls)
	}
}

func TestPreviewWorkerP115ForbiddenFailsOnlyThisVideo(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "preview-p115-transient")

	gen := &fakeTeaserGenerator{
		generateErr: errors.New("ffmpeg: exit status 1, stderr: Server returned 403 Forbidden"),
	}
	drv := &previewFakeDrive{kind: "p115"}
	worker := NewWorker(gen, cat, drv)

	worker.process(ctx, video)

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.PreviewStatus != "failed" {
		t.Fatalf("preview status = %q, want failed after rejected original source", got.PreviewStatus)
	}
	if !worker.Status().CooldownUntil.IsZero() {
		t.Fatal("one rejected file must not cool down the drive")
	}
	if gen.generateCalls != 1 {
		t.Fatalf("generate calls = %d, want 1", gen.generateCalls)
	}
}

func TestP123TransientErrorsShouldCooldown(t *testing.T) {
	drv := &previewFakeDrive{kind: "p123"}
	for _, err := range []error{
		errors.New("Server returned 403 Forbidden"),
		errors.New("http 503 service unavailable"),
	} {
		if !driveErrorShouldCooldown(drv, err) {
			t.Fatalf("driveErrorShouldCooldown(%v) = false, want true", err)
		}
	}
	if driveErrorShouldCooldown(drv, errors.New("请求太频繁")) {
		t.Fatal("message-only throttling text should not trigger p123 cooldown")
	}
	if driveErrorShouldCooldown(drv, errors.New("invalid credential")) {
		t.Fatal("invalid credential should not trigger p123 cooldown")
	}
}

func TestWopanTransientErrorsShouldCooldown(t *testing.T) {
	drv := &previewFakeDrive{kind: "wopan"}
	for _, err := range []error{
		errors.New("ffmpeg: Server returned 403 Forbidden"),
		errors.New("wopan download url: request failed with status: 429 Too Many Requests"),
		errors.New("http 503 service unavailable"),
	} {
		if !driveErrorShouldCooldown(drv, err) {
			t.Fatalf("driveErrorShouldCooldown(%v) = false, want true", err)
		}
	}
	if driveErrorShouldCooldown(drv, errors.New("操作频繁，请稍后重试")) {
		t.Fatal("message-only throttling text should not trigger wopan cooldown")
	}
	if driveErrorShouldCooldown(drv, errors.New("invalid access token")) {
		t.Fatal("invalid access token should not trigger wopan cooldown")
	}
}

func TestGuangYaPanTransientErrorsShouldCooldown(t *testing.T) {
	drv := &previewFakeDrive{kind: "guangyapan"}
	for _, err := range []error{
		errors.New("ffmpeg: Server returned 403 Forbidden"),
		errors.New("guangyapan api rate limited: status=429 msg=操作频繁，请稍后重试"),
		errors.New("http 503 service unavailable"),
	} {
		if !driveErrorShouldCooldown(drv, err) {
			t.Fatalf("driveErrorShouldCooldown(%v) = false, want true", err)
		}
	}
	if driveErrorShouldCooldown(drv, errors.New("操作频繁，请稍后重试")) {
		t.Fatal("message-only throttling text should not trigger guangyapan cooldown")
	}
	if driveErrorShouldCooldown(drv, errors.New("invalid access token")) {
		t.Fatal("invalid access token should not trigger guangyapan cooldown")
	}
}

func TestGoogleDriveMediaErrorsShouldCooldown(t *testing.T) {
	drv := &previewFakeDrive{kind: "googledrive"}
	for _, err := range []error{
		errors.New("ffmpeg: Server returned 403 Forbidden"),
		errors.New("http 503 service unavailable"),
	} {
		if !driveErrorShouldCooldown(drv, err) {
			t.Fatalf("driveErrorShouldCooldown(%v) = false, want true", err)
		}
	}
	for _, err := range []error{
		errors.New("google drive api error: usageLimits userRateLimitExceeded"),
		errors.New("downloadQuotaExceeded: The download quota for this file has been exceeded"),
		errors.New("sharingRateLimitExceeded"),
	} {
		if driveErrorShouldCooldown(drv, err) {
			t.Fatalf("message-only google drive error %v should not trigger cooldown", err)
		}
	}
	if driveErrorShouldCooldown(drv, errors.New("invalid credentials")) {
		t.Fatal("invalid credentials should not trigger googledrive cooldown")
	}
}

func TestThumbWorkerPrefersGenerationStream(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "thumb-generation-stream")
	gen := &fakeThumbGenerator{}
	drv := &generationPreviewDrive{previewFakeDrive: &previewFakeDrive{kind: "p115"}}
	worker := NewThumbWorker(gen, cat, drv)

	worker.process(ctx, video)

	if gen.thumbnailURL != "https://hls.example/initial.m3u8" {
		t.Fatalf("thumbnail source = %q", gen.thumbnailURL)
	}
	if drv.streamCalls != 0 {
		t.Fatalf("original stream calls = %d, want 0", drv.streamCalls)
	}
	if len(drv.generationRefreshes) != 1 || drv.generationRefreshes[0] {
		t.Fatalf("generation refresh flags = %#v", drv.generationRefreshes)
	}
}

func TestThumbWorkerFallsBackToOriginalAfterGenerationDecodeError(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "thumb-generation-fallback")
	gen := &fakeThumbGenerator{generateErrs: []error{errors.New("invalid HLS data"), nil}}
	drv := &generationPreviewDrive{previewFakeDrive: &previewFakeDrive{kind: "p115"}}
	worker := NewThumbWorker(gen, cat, drv)

	worker.process(ctx, video)

	if len(gen.thumbnailURLs) != 2 || gen.thumbnailURLs[0] != "https://hls.example/initial.m3u8" || gen.thumbnailURLs[1] != "https://video.example/clip.mp4" {
		t.Fatalf("thumbnail sources = %#v", gen.thumbnailURLs)
	}
	if drv.streamCalls != 1 {
		t.Fatalf("original stream calls = %d, want 1", drv.streamCalls)
	}
}

func TestThumbWorkerRefreshesRejectedGenerationStreamOnce(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "thumb-generation-refresh")
	forbidden := &drives.RateLimitError{
		Provider: "p115",
		Err:      errors.New("ffmpeg thumb: Server returned 403 Forbidden"),
	}
	gen := &fakeThumbGenerator{generateErrs: []error{forbidden, nil}}
	drv := &generationPreviewDrive{previewFakeDrive: &previewFakeDrive{kind: "p115"}}
	worker := NewThumbWorker(gen, cat, drv)

	worker.process(ctx, video)

	if len(gen.thumbnailURLs) != 2 || gen.thumbnailURLs[1] != "https://hls.example/refreshed.m3u8" {
		t.Fatalf("thumbnail sources = %#v", gen.thumbnailURLs)
	}
	if len(drv.generationRefreshes) != 2 || drv.generationRefreshes[0] || !drv.generationRefreshes[1] {
		t.Fatalf("generation refresh flags = %#v", drv.generationRefreshes)
	}
	if drv.streamCalls != 0 {
		t.Fatalf("original stream calls = %d, want 0", drv.streamCalls)
	}
}

func TestThumbWorkerRefreshesP115OriginalOnlyAfterReadFailure(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "thumb-original-on-demand-refresh")
	gen := &fakeThumbGenerator{generateErrs: []error{
		errors.New("ffmpeg thumb: partial file after EOF"),
		nil,
	}}
	drv := &previewFakeDrive{kind: "p115"}
	worker := NewThumbWorker(gen, cat, drv)

	worker.process(ctx, video)

	if drv.streamCalls != 2 {
		t.Fatalf("stream calls = %d, want initial resolution plus one failure-driven refresh", drv.streamCalls)
	}
	if gen.generateCalls != 2 {
		t.Fatalf("generate calls = %d, want one retry with the refreshed link", gen.generateCalls)
	}
	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.ThumbnailURL != "/p/thumb/"+video.ID {
		t.Fatalf("thumbnail = %q, want recovered local thumbnail", got.ThumbnailURL)
	}
}

func TestPreviewWorkerPrefersGenerationStreamWithoutOriginalRefreshes(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "preview-generation-stream")
	gen := &fakeTeaserGenerator{}
	drv := &generationPreviewDrive{previewFakeDrive: &previewFakeDrive{kind: "p115"}}
	worker := NewWorker(gen, cat, drv)

	worker.process(ctx, video)

	if len(gen.generatedURLs) != 1 || gen.generatedURLs[0] != "https://hls.example/initial.m3u8" {
		t.Fatalf("preview sources = %#v", gen.generatedURLs)
	}
	if drv.streamCalls != 0 {
		t.Fatalf("original stream calls = %d, want none for a healthy generation stream", drv.streamCalls)
	}
}

func TestPreviewWorkerFallsBackWhenGenerationStreamUnavailable(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "preview-generation-unavailable")
	gen := &fakeTeaserGenerator{}
	drv := &generationPreviewDrive{
		previewFakeDrive: &previewFakeDrive{kind: "p115"},
		generationErrs:   []error{drives.ErrGenerationStreamUnavailable},
	}
	worker := NewWorker(gen, cat, drv)

	worker.process(ctx, video)

	if len(gen.generatedURLs) != 1 || gen.generatedURLs[0] != "https://video.example/clip.mp4" {
		t.Fatalf("preview sources = %#v", gen.generatedURLs)
	}
	if drv.streamCalls != 1 {
		t.Fatalf("original stream calls = %d, want one on the healthy fallback path", drv.streamCalls)
	}
}

func assertCooldownAround(t *testing.T, until time.Time, before time.Time, want time.Duration) {
	t.Helper()
	if until.IsZero() {
		t.Fatal("cooldown is zero, want active cooldown")
	}
	min := before.Add(want - time.Second)
	max := time.Now().Add(want + time.Second)
	if until.Before(min) || until.After(max) {
		t.Fatalf("cooldown until = %s, want around %s from now", until.Format(time.RFC3339Nano), want)
	}
}

func TestPreviewWorkerReusesP115LinkAcrossTeaserInputs(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "preview-p115-refresh")
	video.DurationSeconds = 81
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("update video: %v", err)
	}

	gen := &fakeTeaserGenerator{}
	drv := &previewFakeDrive{kind: "p115"}
	worker := NewWorker(gen, cat, drv)

	worker.process(ctx, video)

	if drv.streamCalls != 1 {
		t.Fatalf("stream calls = %d, want one task-scoped original link", drv.streamCalls)
	}
}

func seedPreviewTestVideo(t *testing.T, id string) (*catalog.Catalog, *catalog.Video) {
	t.Helper()
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	video := &catalog.Video{
		ID:            id,
		DriveID:       "drive-id",
		FileID:        "file-id",
		Title:         "Clip",
		PreviewStatus: "pending",
		PublishedAt:   time.Now(),
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	return cat, video
}

type fakeThumbGenerator struct {
	thumbnailVideoID  string
	thumbnailDuration float64
	thumbnailURL      string
	probeCalls        int
	generateCalls     int
	probeDuration     float64
	probeErr          error
	generateErr       error
	generateErrs      []error
	thumbnailURLs     []string
}

type followUpThumbGenerator struct {
	mu           sync.Mutex
	calls        int
	started      chan int
	releaseFirst chan struct{}
}

func (g *followUpThumbGenerator) Probe(context.Context, *drives.StreamLink) (float64, error) {
	return 42, nil
}

func (g *followUpThumbGenerator) GenerateThumbnail(ctx context.Context, _ *drives.StreamLink, videoID string, _ float64) (string, error) {
	g.mu.Lock()
	g.calls++
	call := g.calls
	g.mu.Unlock()
	select {
	case g.started <- call:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if call == 1 {
		select {
		case <-g.releaseFirst:
			return "", errors.New("invalid media")
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "/tmp/" + videoID + ".jpg", nil
}

func (g *fakeThumbGenerator) Probe(context.Context, *drives.StreamLink) (float64, error) {
	g.probeCalls++
	if g.probeErr != nil {
		return 0, g.probeErr
	}
	return g.probeDuration, nil
}

func (g *fakeThumbGenerator) GenerateThumbnail(_ context.Context, link *drives.StreamLink, videoID string, duration float64) (string, error) {
	g.generateCalls++
	g.thumbnailVideoID = videoID
	g.thumbnailDuration = duration
	if link != nil {
		g.thumbnailURL = link.URL
		g.thumbnailURLs = append(g.thumbnailURLs, link.URL)
	}
	if len(g.generateErrs) > 0 {
		err := g.generateErrs[0]
		g.generateErrs = g.generateErrs[1:]
		if err != nil {
			return "", err
		}
	}
	if g.generateErr != nil {
		return "", g.generateErr
	}
	return "/tmp/" + videoID + ".jpg", nil
}

type fakeTeaserGenerator struct {
	localPath         string
	generateErr       error
	generateCalls     int
	generateErrs      []error
	generatedURLs     []string
	generatedDuration float64
}

type blockingTeaserGenerator struct {
	started chan struct{}
	release <-chan struct{}
}

func (g *blockingTeaserGenerator) Probe(context.Context, *drives.StreamLink) (float64, error) {
	return 60, nil
}

func (g *blockingTeaserGenerator) Generate(ctx context.Context, _ *drives.StreamLink, _ float64) (string, error) {
	select {
	case g.started <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case <-g.release:
		return "/tmp/source-teaser.mp4", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (g *blockingTeaserGenerator) MoveToLocal(_ string, videoID string) (string, error) {
	return "/tmp/" + videoID + ".mp4", nil
}

type concurrentPreviewDrive struct {
	previewFakeDrive
}

func (d *concurrentPreviewDrive) StreamURL(_ context.Context, fileID string) (*drives.StreamLink, error) {
	return &drives.StreamLink{URL: "https://video.example/" + fileID}, nil
}

func (g *fakeTeaserGenerator) Probe(context.Context, *drives.StreamLink) (float64, error) {
	return 0, nil
}

func (g *fakeTeaserGenerator) Generate(_ context.Context, link *drives.StreamLink, duration float64) (string, error) {
	g.generateCalls++
	g.generatedDuration = duration
	if link != nil {
		g.generatedURLs = append(g.generatedURLs, link.URL)
	}
	if len(g.generateErrs) > 0 {
		err := g.generateErrs[0]
		g.generateErrs = g.generateErrs[1:]
		if err != nil {
			return "", err
		}
	}
	if g.generateErr != nil {
		return "", g.generateErr
	}
	return "/tmp/source-teaser.mp4", nil
}

func (g *fakeTeaserGenerator) GenerateWithLinkRefresh(ctx context.Context, first *drives.StreamLink, duration float64, _ func(context.Context) (*drives.StreamLink, error)) (string, error) {
	return g.Generate(ctx, first, duration)
}

func (g *fakeTeaserGenerator) MoveToLocal(_ string, videoID string) (string, error) {
	if g.localPath != "" {
		return g.localPath, nil
	}
	return "/tmp/" + videoID + ".mp4", nil
}

type previewFakeDrive struct {
	kind           string
	streamFileID   string
	streamCalls    int
	streamErr      error
	ensureDirCalls int
	uploadCalls    int
}

type generationPreviewDrive struct {
	*previewFakeDrive
	generationErrs      []error
	generationRefreshes []bool
}

func (d *generationPreviewDrive) GenerationStreamURL(_ context.Context, _ string, forceRefresh bool) (*drives.StreamLink, error) {
	d.generationRefreshes = append(d.generationRefreshes, forceRefresh)
	if len(d.generationErrs) > 0 {
		err := d.generationErrs[0]
		d.generationErrs = d.generationErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	name := "initial"
	if forceRefresh {
		name = "refreshed"
	}
	return &drives.StreamLink{URL: "https://hls.example/" + name + ".m3u8"}, nil
}

func (d *previewFakeDrive) Kind() string {
	if d.kind != "" {
		return d.kind
	}
	return "fake"
}
func (d *previewFakeDrive) ID() string { return "drive-id" }
func (d *previewFakeDrive) Init(context.Context) error {
	return nil
}
func (d *previewFakeDrive) List(context.Context, string) ([]drives.Entry, error) {
	return nil, nil
}
func (d *previewFakeDrive) Stat(context.Context, string) (*drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *previewFakeDrive) StreamURL(_ context.Context, fileID string) (*drives.StreamLink, error) {
	d.streamCalls++
	d.streamFileID = fileID
	if d.streamErr != nil {
		return nil, d.streamErr
	}
	return &drives.StreamLink{URL: "https://video.example/clip.mp4"}, nil
}
func (d *previewFakeDrive) Upload(context.Context, string, string, io.Reader, int64) (string, error) {
	d.uploadCalls++
	return "", drives.ErrNotSupported
}
func (d *previewFakeDrive) EnsureDir(context.Context, string) (string, error) {
	d.ensureDirCalls++
	return "", drives.ErrNotSupported
}
func (d *previewFakeDrive) RootID() string { return "root" }

func TestWorkerWaitIdleReturnsImmediatelyWhenQueueEmpty(t *testing.T) {
	worker := NewWorker(&fakeTeaserGenerator{}, nil, &previewFakeDrive{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	start := time.Now()
	if err := worker.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle on empty queue: %v", err)
	}
	if took := time.Since(start); took > 50*time.Millisecond {
		t.Fatalf("WaitIdle on empty queue took %s, want immediate return", took)
	}
}

func TestWorkerWaitIdleBlocksUntilQueueDrains(t *testing.T) {
	worker := NewWorker(&fakeTeaserGenerator{}, nil, &previewFakeDrive{})
	v := &catalog.Video{ID: "wait-idle-vid"}
	if !worker.queue.reserve(v) {
		t.Fatalf("reserve should succeed on fresh queue")
	}

	go func() {
		time.Sleep(120 * time.Millisecond)
		worker.queue.release(v)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	if err := worker.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	took := time.Since(start)
	if took < 100*time.Millisecond {
		t.Fatalf("WaitIdle returned in %s, expected to wait until release", took)
	}
	if took > time.Second {
		t.Fatalf("WaitIdle took %s, expected to return shortly after release", took)
	}
}

func TestWorkerWaitIdleHonoursContextCancel(t *testing.T) {
	worker := NewWorker(&fakeTeaserGenerator{}, nil, &previewFakeDrive{})
	v := &catalog.Video{ID: "ctx-cancel"}
	if !worker.queue.reserve(v) {
		t.Fatalf("reserve should succeed")
	}
	t.Cleanup(func() { worker.queue.release(v) })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if err := worker.WaitIdle(ctx); err == nil {
		t.Fatalf("WaitIdle expected ctx.Err, got nil")
	}
}

func TestThumbWorkerWaitIdleBlocksUntilQueueDrains(t *testing.T) {
	worker := NewThumbWorker(&fakeThumbGenerator{}, nil, &previewFakeDrive{})
	v := &catalog.Video{ID: "thumb-wait-idle"}
	if !worker.queue.reserve(v) {
		t.Fatalf("reserve should succeed")
	}

	go func() {
		time.Sleep(80 * time.Millisecond)
		worker.queue.release(v)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := worker.WaitIdle(ctx); err != nil {
		t.Fatalf("ThumbWorker.WaitIdle: %v", err)
	}
}

func TestRejectedRefreshedHLSFallsBackToOriginal(t *testing.T) {
	for _, kind := range []string{"thumbnail", "preview"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			cat, v := seedPreviewTestVideo(t, "hls-rejected-"+kind)
			rejected := errors.New("Server returned 403 Forbidden")
			drv := &generationPreviewDrive{previewFakeDrive: &previewFakeDrive{kind: "p115"}}
			var sources []string
			if kind == "thumbnail" {
				gen := &fakeThumbGenerator{generateErrs: []error{rejected, rejected, nil}}
				w := NewThumbWorker(gen, cat, drv)
				w.process(ctx, v)
				sources = gen.thumbnailURLs
				if !w.Status().CooldownUntil.IsZero() {
					t.Fatal("HLS failure cooled down healthy original")
				}
			} else {
				gen := &fakeTeaserGenerator{generateErrs: []error{rejected, rejected, nil}}
				w := NewWorker(gen, cat, drv)
				w.process(ctx, v)
				sources = gen.generatedURLs
				if !w.Status().CooldownUntil.IsZero() {
					t.Fatal("HLS failure cooled down healthy original")
				}
			}
			if len(sources) != 3 || sources[2] != "https://video.example/clip.mp4" || drv.streamCalls != 1 {
				t.Fatalf("sources=%v direct=%d", sources, drv.streamCalls)
			}
		})
	}
}

func TestHLS429DoesNotFallBackAndMultiplyRequests(t *testing.T) {
	ctx := context.Background()
	cat, v := seedPreviewTestVideo(t, "hls-limited")
	err := &drives.RateLimitError{Provider: "p115", Err: errors.New("HTTP status=429")}
	drv := &generationPreviewDrive{previewFakeDrive: &previewFakeDrive{kind: "p115"}}
	gen := &fakeTeaserGenerator{generateErr: err}
	worker := NewWorker(gen, cat, drv)
	if !worker.process(ctx, v) || drv.streamCalls != 0 {
		t.Fatal("explicit limit should pause instead of requesting original")
	}
}
