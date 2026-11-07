package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/api"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/fingerprint"
	"github.com/nianzhibai/91/backend/internal/preview"
)

type durationBackfillDrive struct{ serverFakeDrive }

func (d *durationBackfillDrive) StreamURL(_ context.Context, fileID string) (*drives.StreamLink, error) {
	return &drives.StreamLink{URL: "https://video.example/" + fileID}, nil
}

type durationBackfillGenerator struct {
	serverFakeTeaserGenerator
	mu    sync.Mutex
	calls map[string]int
	fail  bool
}

func (g *durationBackfillGenerator) Probe(_ context.Context, link *drives.StreamLink) (float64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls[link.URL]++
	if g.fail && strings.HasSuffix(link.URL, "/failed") {
		return 0, errors.New("temporary read error")
	}
	return 19, nil
}

func TestDriveDurationSweepTriesEachVideoOnceAndRecoversNextRound(t *testing.T) {
	app := newGenerationRequestApp(t)
	now := time.Now()
	originals := make(map[string]*catalog.Video)
	for _, id := range []string{"failed", "good", "known"} {
		video := &catalog.Video{ID: id, DriveID: "drive-id", FileID: id, Title: id, Size: 100,
			ThumbnailURL: "/p/thumb/" + id, CreatedAt: now, PublishedAt: now, UpdatedAt: now}
		if id == "known" {
			video.DurationSeconds = 42
		}
		if err := app.cat.UpsertVideo(context.Background(), video); err != nil {
			t.Fatal(err)
		}
		original, err := app.cat.GetVideo(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		originals[id] = original
	}
	gen := &durationBackfillGenerator{calls: make(map[string]int), fail: true}
	worker := preview.NewThumbWorker(gen, app.cat, &durationBackfillDrive{})
	runCtx, cancel := context.WithCancel(context.Background())
	app.registerPreviewWorkersWithOptions(runCtx, "drive-id", nil, worker, nil, cancel, false)
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-done })
	ctx, stop := context.WithTimeout(runCtx, 3*time.Second)
	defer stop()

	app.enqueueDriveGeneration(ctx, "drive-id", nil, worker)
	if err := app.waitDriveResourcesIdle(ctx, "drive-id"); err != nil {
		t.Fatal(err)
	}
	gen.mu.Lock()
	if gen.calls["https://video.example/failed"] != 1 || gen.calls["https://video.example/good"] != 1 || gen.calls["https://video.example/known"] != 0 {
		t.Errorf("first round probes=%v", gen.calls)
	}
	gen.fail = false
	gen.mu.Unlock()
	app.enqueueDriveGeneration(ctx, "drive-id", nil, worker)
	if err := app.waitDriveResourcesIdle(ctx, "drive-id"); err != nil {
		t.Fatal(err)
	}
	gen.mu.Lock()
	if gen.calls["https://video.example/failed"] != 2 || gen.calls["https://video.example/good"] != 1 {
		t.Errorf("second round probes=%v", gen.calls)
	}
	gen.mu.Unlock()
	for _, id := range []string{"failed", "good"} {
		current, err := app.cat.GetVideo(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if current.DurationSeconds != 19 || current.ThumbnailURL != originals[id].ThumbnailURL || !current.ThumbnailUpdatedAt.Equal(originals[id].ThumbnailUpdatedAt) {
			t.Fatalf("duration sweep changed cover or did not recover %s: %+v", id, current)
		}
	}
	ready, err := app.cat.ListVideosByThumbnailStatus(ctx, "drive-id", "ready", 100)
	if err != nil || len(ready) != 3 {
		t.Fatalf("cover state changed: %v, %v", ready, err)
	}
}

func runDurationTestWorker(t *testing.T, ctx context.Context, run func(context.Context)) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); run(ctx) }()
	t.Cleanup(func() { <-done })
}

func TestDurationBackfillWaitsForOwnResourcesWithoutWaitingForOtherDrives(t *testing.T) {
	app := newGenerationRequestApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	t.Cleanup(cancel)
	duration := seedGenerationRequestVideo(t, app, "duration", "ready")
	if err := app.cat.UpdateVideoMeta(ctx, duration.ID, catalog.VideoMetaPatch{DurationSecondsSet: true}); err != nil {
		t.Fatal(err)
	}
	cover := seedGenerationRequestVideo(t, app, "cover", "pending")
	teaser := seedGenerationRequestVideo(t, app, "teaser", "pending")
	finger := seedGenerationRequestVideo(t, app, "fingerprint", "pending")
	finger.Size = 0 // Drain a failed fingerprint without making an HTTP request.
	if err := app.cat.UpsertVideo(ctx, finger); err != nil {
		t.Fatal(err)
	}
	gen := &durationBackfillGenerator{calls: make(map[string]int)}
	drv := &durationBackfillDrive{}
	thumbWorker := preview.NewThumbWorker(gen, app.cat, drv)
	previewWorker := preview.NewWorker(gen, app.cat, drv)
	fingerprintWorker := fingerprint.NewWorker(app.cat, drv, fingerprint.Config{})
	app.registerPreviewWorkersWithOptions(ctx, "drive-id", previewWorker, thumbWorker, fingerprintWorker, cancel, false)
	if !thumbWorker.Enqueue(cover) || !previewWorker.Enqueue(teaser) || !fingerprintWorker.Enqueue(finger) {
		t.Fatal("resource enqueue failed")
	}
	other := preview.NewWorker(gen, app.cat, drv)
	if !other.Enqueue(teaser) {
		t.Fatal("other drive enqueue failed")
	}
	app.workers["other-drive"] = other
	app.scheduleDriveDurationBackfill("drive-id")
	runDurationTestWorker(t, ctx, thumbWorker.Run)
	if err := thumbWorker.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	runDurationTestWorker(t, ctx, previewWorker.Run)
	if err := previewWorker.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	// Cover and teaser work has finished, but the fingerprint is still queued.
	waitCtx, stopWait := context.WithTimeout(ctx, 100*time.Millisecond)
	if err := app.waitDurationBackfillsIdle(waitCtx, "drive-id"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("backfill did not wait for fingerprint generation: %v", err)
	}
	stopWait()
	gen.mu.Lock()
	probes := gen.calls["https://video.example/duration"]
	gen.mu.Unlock()
	if probes != 0 {
		t.Fatal("missing duration was probed before fingerprint generation drained")
	}
	runDurationTestWorker(t, ctx, fingerprintWorker.Run)
	if err := app.waitDriveResourcesIdle(ctx, "drive-id"); err != nil {
		t.Fatal(err)
	}
	current, err := app.cat.GetVideo(ctx, duration.ID)
	if err != nil || current.DurationSeconds != 19 {
		t.Fatalf("own drive did not complete backfill: %v, %v", current, err)
	}
	if other.Status().QueueLength != 1 {
		t.Fatal("test did not retain another drive's pending generation")
	}
	cancel()
}

func TestDurationBackfillCoalescesRequestsUntilResourceAdmissionFinishes(t *testing.T) {
	app := newGenerationRequestApp(t)
	video := seedGenerationRequestVideo(t, app, "failed", "ready")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := app.cat.UpdateVideoMeta(ctx, video.ID, catalog.VideoMetaPatch{DurationSecondsSet: true}); err != nil {
		t.Fatal(err)
	}
	gen := &durationBackfillGenerator{calls: make(map[string]int), fail: true}
	worker := preview.NewThumbWorker(gen, app.cat, &durationBackfillDrive{})
	app.registerPreviewWorkersWithOptions(ctx, "drive-id", nil, worker, nil, cancel, false)
	runDurationTestWorker(t, ctx, worker.Run)
	finishEnqueue := app.beginDriveResourceEnqueue("drive-id")
	for i := 0; i < 10; i++ {
		app.scheduleDriveDurationBackfill("drive-id")
	}
	waitCtx, stopWait := context.WithTimeout(ctx, 100*time.Millisecond)
	if err := app.waitDurationBackfillsIdle(waitCtx, "drive-id"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("backfill ignored unfinished resource admission: %v", err)
	}
	stopWait()
	gen.mu.Lock()
	probes := gen.calls["https://video.example/failed"]
	gen.mu.Unlock()
	if probes != 0 {
		t.Fatal("duration was probed while resource admission was still open")
	}
	finishEnqueue()
	if err := app.waitDriveResourcesIdle(ctx, "drive-id"); err != nil {
		t.Fatal(err)
	}
	gen.mu.Lock()
	probes = gen.calls["https://video.example/failed"]
	gen.mu.Unlock()
	if probes != 1 {
		t.Fatalf("same round retried a failed probe %d times", probes)
	}
	cancel()
}

func TestDriveDurationBackfillReportsInfrastructureFailure(t *testing.T) {
	app := newGenerationRequestApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	worker := app.thumbWorkers["drive-id"]
	app.registerPreviewWorkersWithOptions(ctx, "drive-id", nil, worker, nil, cancel, false)
	if err := app.cat.Close(); err != nil {
		t.Fatal(err)
	}
	app.scheduleDriveDurationBackfill("drive-id")
	if err := app.waitDurationBackfillsIdle(ctx, "drive-id"); err == nil {
		t.Fatal("database failure was not reported by the resource completion wait")
	}
}

func TestDriveGenerationEntrypointsScheduleDurationBackfill(t *testing.T) {
	for _, entrypoint := range []string{"scan generation", "manual generation", "uploaded video"} {
		t.Run(entrypoint, func(t *testing.T) {
			app := newGenerationRequestApp(t)
			video := seedGenerationRequestVideo(t, app, "duration-entrypoint", "ready")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := app.cat.UpdateVideoMeta(ctx, video.ID, catalog.VideoMetaPatch{DurationSecondsSet: true}); err != nil {
				t.Fatal(err)
			}
			worker := preview.NewThumbWorker(&durationBackfillGenerator{calls: make(map[string]int)}, app.cat, &durationBackfillDrive{})
			app.registerPreviewWorkersWithOptions(ctx, "drive-id", nil, worker, nil, cancel, false)
			runDurationTestWorker(t, ctx, worker.Run)
			switch entrypoint {
			case "scan generation":
				app.enqueueDriveGeneration(ctx, "drive-id", nil, worker)
			case "manual generation":
				result, err := app.requestDriveGeneration(ctx, ctx, "drive-id", api.DriveGenerationThumbnails)
				if err != nil || result.State != "ready" {
					t.Fatalf("request = %+v, %v", result, err)
				}
			case "uploaded video":
				app.enqueueUploadedVideo(ctx, video)
			}
			if err := app.waitDriveResourcesIdle(ctx, "drive-id"); err != nil {
				t.Fatal(err)
			}
			current, err := app.cat.GetVideo(ctx, video.ID)
			if err != nil || current.DurationSeconds != 19 {
				t.Fatalf("generation entrypoint omitted duration backfill: %v, %v", current, err)
			}
			cancel()
		})
	}
}

func TestAdmittedDurationBackfillCanFinishDuringPendingConfigurationChange(t *testing.T) {
	app := newGenerationRequestApp(t)
	video := seedGenerationRequestVideo(t, app, "duration-config-change", "ready")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := app.cat.UpdateVideoMeta(ctx, video.ID, catalog.VideoMetaPatch{DurationSecondsSet: true}); err != nil {
		t.Fatal(err)
	}
	worker := preview.NewThumbWorker(&durationBackfillGenerator{calls: make(map[string]int)}, app.cat, &durationBackfillDrive{})
	app.registerPreviewWorkersWithOptions(ctx, "drive-id", nil, worker, nil, cancel, false)
	runDurationTestWorker(t, ctx, worker.Run)
	finishEnqueue := app.beginDriveResourceEnqueue("drive-id")
	gate := app.driveOperationGate("drive-id")
	gate.mu.Lock()
	gate.beginBlockedLocked()
	gate.mu.Unlock()
	defer func() { gate.mu.Lock(); gate.endBlockedLocked(); gate.mu.Unlock() }()
	finishEnqueue()
	if err := app.waitDriveResourcesIdle(ctx, "drive-id"); err != nil {
		t.Fatalf("pending change prevented admitted generation from completing: %v", err)
	}
	current, err := app.cat.GetVideo(ctx, video.ID)
	if err != nil || current.DurationSeconds != 19 {
		t.Fatalf("admitted duration was not completed: %v, %v", current, err)
	}
	cancel()
}
