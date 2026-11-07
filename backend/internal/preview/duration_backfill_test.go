package preview

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
)

func existingDurationTestVideo(t *testing.T, id string) (*catalog.Catalog, *catalog.Video) {
	t.Helper()
	cat, video := seedPreviewTestVideo(t, id)
	video.ThumbnailURL = "/p/thumb/" + video.ID
	if err := cat.UpsertVideo(context.Background(), video); err != nil {
		t.Fatal(err)
	}
	return cat, video
}

func TestDurationFailureWaitsForNextExplicitSweep(t *testing.T) {
	ctx := context.Background()
	cat, video := existingDurationTestVideo(t, "duration-timeout")
	original, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatal(err)
	}
	gen := &fakeThumbGenerator{probeErr: context.DeadlineExceeded}
	worker := NewThumbWorker(gen, cat, &previewFakeDrive{})
	if !worker.EnqueueDurationBlocking(ctx, video) {
		t.Fatal("backfill enqueue failed")
	}
	task := <-worker.durationCh
	worker.processQueuedTask(task.ctx, task.video, true)
	if gen.probeCalls != 1 || worker.Status().QueueLength != 0 {
		t.Fatal("failed duration was automatically requeued")
	}
	if err := worker.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	worker.process(ctx, video)
	if gen.probeCalls != 1 {
		t.Fatal("ordinary cover work repeated metadata failure")
	}
	current, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.DurationSeconds != 0 || !current.ThumbnailUpdatedAt.Equal(original.ThumbnailUpdatedAt) {
		t.Fatal("failed probe changed existing assets")
	}
	gen.probeErr, gen.probeDuration = nil, 19
	// The next round can try immediately; there is no timer or attempt budget.
	worker.processDuration(ctx, video)
	current, err = cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.DurationSeconds != 19 || gen.probeCalls != 2 || gen.generateCalls != 0 {
		t.Fatalf("next round did not recover: %+v", current)
	}
}

func TestStoreProbedDurationRequiresSuccessfulPersistence(t *testing.T) {
	cat, video := seedPreviewTestVideo(t, "duration-save-failed")
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	duration, err := storeProbedDuration(context.Background(), cat, video, 42)
	if err == nil || !strings.Contains(err.Error(), "save duration") || duration != 0 {
		t.Fatalf("failed save reported success: duration=%v err=%v", duration, err)
	}
	if video.DurationSeconds != 0 {
		t.Fatal("unsaved duration published in memory")
	}
}

func TestSubsecondBackfillDoesNotRemainUnknown(t *testing.T) {
	cat, video := existingDurationTestVideo(t, "duration-subsecond")
	worker := NewThumbWorker(&fakeThumbGenerator{probeDuration: 0.7}, cat, &previewFakeDrive{})
	worker.processDuration(context.Background(), video)
	current, err := cat.GetVideo(context.Background(), video.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.DurationSeconds != 1 {
		t.Fatalf("subsecond duration lost: %+v", current)
	}
}

func TestDurationBackfillDoesNotRetryFailedThumbnail(t *testing.T) {
	cat, video := seedPreviewTestVideo(t, "duration-failed-thumbnail")
	if err := cat.UpdateVideoMeta(context.Background(), video.ID, catalog.VideoMetaPatch{ThumbnailStatus: "failed"}); err != nil {
		t.Fatal(err)
	}
	gen := &fakeThumbGenerator{probeDuration: 19}
	worker := NewThumbWorker(gen, cat, &previewFakeDrive{})
	worker.processDuration(context.Background(), video)
	current, err := cat.GetVideo(context.Background(), video.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.DurationSeconds != 19 || gen.generateCalls != 0 {
		t.Fatalf("metadata work generated assets: %+v", current)
	}
	failed, err := cat.ListVideosByThumbnailStatus(context.Background(), video.DriveID, "failed", 100)
	if err != nil || len(failed) != 1 {
		t.Fatalf("duration success changed thumbnail failure: %v, %v", failed, err)
	}
}

func TestDurationRateLimitDoesNotRequeueTheVideo(t *testing.T) {
	cat, video := existingDurationTestVideo(t, "duration-rate-limit")
	gen := &fakeThumbGenerator{probeErr: &drives.RateLimitError{RetryAfter: time.Hour, Err: errors.New("429")}}
	worker := NewThumbWorker(gen, cat, &previewFakeDrive{})
	if !worker.EnqueueDurationBlocking(context.Background(), video) {
		t.Fatal("enqueue failed")
	}
	task := <-worker.durationCh
	worker.processQueuedTask(task.ctx, task.video, true)
	if gen.probeCalls != 1 || worker.Status().QueueLength != 0 {
		t.Fatal("rate-limited metadata was requeued")
	}
	ready, err := cat.ListVideosByThumbnailStatus(context.Background(), video.DriveID, "ready", 100)
	if err != nil || len(ready) != 1 {
		t.Fatalf("rate limit changed cover state: %v, %v", ready, err)
	}
}

func TestDurationBackfillProviderErrorsCooldownWithoutThumbnailRetries(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		streamErr  bool
		cause      error
		cooldown   time.Duration
		want       time.Duration
	}{
		{
			name: "PikPak probe unavailable", kind: "pikpak",
			cause: ffmpegCommandError("ffprobe", errors.New("exit status 1"), []byte("Server returned 503 Service Unavailable")),
			want:  defaultTransientMediaCooldown,
		},
		{
			name: "Wopan probe forbidden", kind: "wopan",
			cause:    ffmpegCommandError("ffprobe", errors.New("exit status 1"), []byte("Server returned 403 Forbidden")),
			cooldown: 10 * time.Minute, want: 10 * time.Minute,
		},
		{
			name: "PikPak stream unavailable", kind: "pikpak", streamErr: true,
			cause:    errors.New("HTTP 503 Service Unavailable"),
			cooldown: 10 * time.Minute, want: 10 * time.Minute,
		},
		{
			name: "Explicit retry after", kind: "pikpak",
			cause:    &drives.RateLimitError{RetryAfter: time.Hour, Err: errors.New("HTTP 429 Too Many Requests")},
			cooldown: 10 * time.Minute, want: time.Hour,
		},
		{
			name: "Invalid media", kind: "pikpak",
			cause: errors.New("invalid data found when processing input"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cat, video := existingDurationTestVideo(t, "duration-provider-error")
			gen := &fakeThumbGenerator{}
			drv := &previewFakeDrive{kind: tc.kind}
			if tc.streamErr {
				drv.streamErr = tc.cause
			} else {
				gen.probeErr = tc.cause
			}
			worker := NewThumbWorker(gen, cat, drv)
			worker.RateLimitCooldown = tc.cooldown
			if !worker.EnqueueDurationBlocking(ctx, video) {
				t.Fatal("duration enqueue failed")
			}
			task := <-worker.durationCh
			before := time.Now()
			worker.processQueuedTask(task.ctx, task.video, true)
			if tc.want > 0 {
				assertCooldownAround(t, worker.Status().CooldownUntil, before, tc.want)
			} else if !worker.Status().CooldownUntil.IsZero() {
				t.Fatal("permanent media error cooled down the provider")
			}
			if worker.Status().QueueLength != 0 || gen.generateCalls != 0 {
				t.Fatal("duration failure requeued work or generated a thumbnail")
			}
			ready, err := cat.ListVideosByThumbnailStatus(ctx, video.DriveID, "ready", 100)
			if err != nil || len(ready) != 1 || ready[0].DurationSeconds != 0 {
				t.Fatalf("duration failure changed cover state or metadata: %v, %v", ready, err)
			}
			failures, err := cat.IncrementThumbnailFailures(ctx, video.ID)
			if err != nil || failures != 1 {
				t.Fatalf("duration failure consumed thumbnail retries: failures=%d err=%v", failures, err)
			}
		})
	}
}

type blockingDurationGenerator struct {
	fakeThumbGenerator
	started chan struct{}
}

func (g *blockingDurationGenerator) Probe(ctx context.Context, _ *drives.StreamLink) (float64, error) {
	close(g.started)
	<-ctx.Done()
	return 0, ctx.Err()
}

func TestStoppingRoundCancelsDurationProbeWithoutStoppingWorker(t *testing.T) {
	cat, video := existingDurationTestVideo(t, "duration-canceled")
	gen := &blockingDurationGenerator{started: make(chan struct{})}
	worker := NewThumbWorker(gen, cat, &previewFakeDrive{})
	runCtx, stopWorker := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(runCtx) }()
	t.Cleanup(func() { stopWorker(); <-done })
	phaseCtx, stopPhase := context.WithCancel(runCtx)
	defer stopPhase()
	if !worker.EnqueueDurationBlocking(phaseCtx, video) {
		t.Fatal("enqueue failed")
	}
	select {
	case <-gen.started:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	stopPhase()
	waitCtx, cancel := context.WithTimeout(runCtx, time.Second)
	defer cancel()
	if err := worker.WaitIdle(waitCtx); err != nil {
		t.Fatalf("canceled probe did not drain: %v", err)
	}
	current, err := cat.GetVideo(runCtx, video.ID)
	if err != nil || current.DurationSeconds != 0 {
		t.Fatalf("canceled probe changed duration: %v, %v", current, err)
	}
	fresh := *video
	fresh.ID, fresh.FileID, fresh.ThumbnailURL, fresh.DurationSeconds = "new-cover", "new-file", "", 42
	if err := cat.UpsertVideo(runCtx, &fresh); err != nil {
		t.Fatal(err)
	}
	if !worker.Enqueue(&fresh) {
		t.Fatal("cover enqueue failed after canceled backfill")
	}
	if err := worker.WaitIdle(waitCtx); err != nil {
		t.Fatal(err)
	}
	current, err = cat.GetVideo(runCtx, fresh.ID)
	if err != nil || current.ThumbnailURL == "" {
		t.Fatalf("worker stopped with the round: %v, %v", current, err)
	}
}

func TestPreviewReadyFollowUpSurvivesCanceledDurationBackfill(t *testing.T) {
	cat, video := seedPreviewTestVideo(t, "duration-cover-follow-up")
	if err := cat.UpdateVideoMeta(context.Background(), video.ID, catalog.VideoMetaPatch{ThumbnailStatus: "failed"}); err != nil {
		t.Fatal(err)
	}
	gen := &blockingDurationGenerator{started: make(chan struct{})}
	worker := NewThumbWorker(gen, cat, &previewFakeDrive{})
	runCtx, stopWorker := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(runCtx) }()
	t.Cleanup(func() { stopWorker(); <-done })
	phaseCtx, stopPhase := context.WithCancel(runCtx)
	defer stopPhase()
	if !worker.EnqueueDurationBlocking(phaseCtx, video) {
		t.Fatal("duration enqueue failed")
	}
	select {
	case <-gen.started:
	case <-time.After(time.Second):
		t.Fatal("duration probe did not start")
	}
	// A concurrently completed preview saves its duration and grants the failed
	// cover another attempt while the metadata probe is still running.
	if err := cat.UpdateVideoMeta(runCtx, video.ID, catalog.VideoMetaPatch{DurationSeconds: 42}); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdatePreview(runCtx, video.ID, "preview.mp4", "ready"); err != nil {
		t.Fatal(err)
	}
	ready, err := cat.GetVideo(runCtx, video.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !worker.EnqueueFollowUp(ready) || !worker.EnqueueFollowUp(ready) {
		t.Fatal("preview-ready cover enqueue failed")
	}
	stopPhase()
	waitCtx, cancel := context.WithTimeout(runCtx, 2*time.Second)
	defer cancel()
	if err := worker.WaitIdle(waitCtx); err != nil {
		t.Fatal(err)
	}
	current, err := cat.GetVideo(runCtx, video.ID)
	if err != nil || current.ThumbnailURL == "" || current.DurationSeconds != 42 || current.PreviewStatus != "ready" {
		t.Fatalf("canceling metadata discarded the cover follow-up: %v, %v", current, err)
	}
	if gen.generateCalls != 1 {
		t.Fatalf("thumbnail generation calls = %d, want one deduplicated follow-up", gen.generateCalls)
	}
}
