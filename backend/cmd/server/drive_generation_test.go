package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/api"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/fingerprint"
	"github.com/nianzhibai/91/backend/internal/preview"
)

var generationTestKinds = []api.DriveGenerationKind{api.DriveGenerationThumbnails, api.DriveGenerationPreviews, api.DriveGenerationFingerprints}

func newGenerationRequestApp(t *testing.T) *App {
	t.Helper()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	seedGenerationDrive(t, cat, "drive-id")
	gen, drv := &serverFakeTeaserGenerator{}, &serverFakeDrive{}
	return &App{
		cat:                cat,
		workers:            map[string]*preview.Worker{"drive-id": preview.NewWorker(gen, cat, drv)},
		thumbWorkers:       map[string]*preview.ThumbWorker{"drive-id": preview.NewThumbWorker(gen, cat, drv)},
		fingerprintWorkers: map[string]*fingerprint.Worker{"drive-id": fingerprint.NewWorker(cat, drv, fingerprint.Config{})},
	}
}

func seedGenerationRequestVideo(t *testing.T, app *App, id, state string) *catalog.Video {
	t.Helper()
	now := time.Now()
	v := &catalog.Video{ID: id, DriveID: "drive-id", FileID: id, FileName: id + ".mp4", Title: id, Size: 123,
		PreviewStatus: state, FingerprintStatus: state, DurationSeconds: 30, CreatedAt: now, PublishedAt: now}
	if state == "ready" {
		v.PreviewLocal = id + ".mp4"
		v.ThumbnailURL = "/p/thumb/" + id
		v.SampledSHA256 = strings.Repeat("a", 64)
	}
	if err := app.cat.UpsertVideo(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	if state == "failed" {
		if err := app.cat.UpdateVideoMeta(context.Background(), id, catalog.VideoMetaPatch{ThumbnailStatus: "failed"}); err != nil {
			t.Fatal(err)
		}
	}
	return v
}

func waitGenerationRequestProducer(t *testing.T, app *App) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		app.taskCancelMu.Lock()
		active := len(app.driveTaskCancels["drive-id"])
		app.taskCancelMu.Unlock()
		if active == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("generation producer did not finish")
}

func TestDriveGenerationRequestReportsReadyWithoutRequeueingCompletedResources(t *testing.T) {
	for _, kind := range generationTestKinds {
		t.Run(string(kind), func(t *testing.T) {
			app := newGenerationRequestApp(t)
			seedGenerationRequestVideo(t, app, "ready", "ready")
			result, err := app.requestDriveGeneration(context.Background(), context.Background(), "drive-id", kind)
			if err != nil || result.State != "ready" || result.Message != generationLabel(kind)+"已全部就绪" {
				t.Fatalf("result = %+v, err=%v", result, err)
			}
			work, _ := app.driveGenerationWork("drive-id", kind)
			if work.busy {
				t.Fatal("completed resources were requeued")
			}
		})
	}
}

func TestDriveGenerationRequestKeepsExistingThumbnailReadyWithMissingDuration(t *testing.T) {
	ctx := context.Background()
	app := newGenerationRequestApp(t)
	video := seedGenerationRequestVideo(t, app, "duration-only", "ready")
	if err := app.cat.UpdateVideoMeta(ctx, video.ID, catalog.VideoMetaPatch{DurationSecondsSet: true}); err != nil {
		t.Fatal(err)
	}
	result, err := app.requestDriveGeneration(ctx, ctx, video.DriveID, api.DriveGenerationThumbnails)
	if err != nil || result.State != "ready" {
		t.Fatalf("missing duration made ready covers fail: %+v, %v", result, err)
	}
	if app.thumbWorkers[video.DriveID].Status().QueueLength != 0 {
		t.Fatal("cover generation admitted duration-only work")
	}
}

func TestDriveGenerationRequestResumesPendingAndFailedResourcesOfOnlyItsKind(t *testing.T) {
	for _, kind := range generationTestKinds {
		t.Run(string(kind), func(t *testing.T) {
			app := newGenerationRequestApp(t)
			seedGenerationRequestVideo(t, app, "pending", "pending")
			seedGenerationRequestVideo(t, app, "failed", "failed")
			seedGenerationRequestVideo(t, app, "ready", "ready")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result, err := app.requestDriveGeneration(ctx, ctx, "drive-id", kind)
			if err != nil || result.State != "started" {
				t.Fatalf("result = %+v, err=%v", result, err)
			}
			waitGenerationRequestProducer(t, app)
			queues := map[api.DriveGenerationKind]int{
				api.DriveGenerationThumbnails:   app.thumbWorkers["drive-id"].Status().QueueLength,
				api.DriveGenerationPreviews:     app.workers["drive-id"].Status().QueueLength,
				api.DriveGenerationFingerprints: app.fingerprintWorkers["drive-id"].Status().QueueLength,
			}
			for queuedKind, length := range queues {
				want := 0
				if queuedKind == kind {
					want = 2
				}
				if length != want {
					t.Fatalf("%s queue=%d, want %d", queuedKind, length, want)
				}
			}
			failed, err := app.cat.GetVideo(ctx, "failed")
			if err != nil {
				t.Fatal(err)
			}
			states := map[api.DriveGenerationKind]string{api.DriveGenerationPreviews: failed.PreviewStatus, api.DriveGenerationFingerprints: failed.FingerprintStatus}
			for stateKind, state := range states {
				want := "failed"
				if stateKind == kind {
					want = "pending"
				}
				if state != want {
					t.Fatalf("%s state=%s, want %s", stateKind, state, want)
				}
			}
			stats, err := app.cat.CountDriveAssetStatsForDrive(ctx, "drive-id")
			if err != nil {
				t.Fatal(err)
			}
			wantFailed := 1
			if kind == api.DriveGenerationThumbnails {
				wantFailed = 0
			}
			if stats.Thumbnails["drive-id"].Failed != wantFailed {
				t.Fatalf("thumbnail failures = %d, want %d", stats.Thumbnails["drive-id"].Failed, wantFailed)
			}
		})
	}
}

func TestDriveGenerationRequestReportsBusyWithoutResettingFailures(t *testing.T) {
	for _, kind := range generationTestKinds {
		t.Run(string(kind), func(t *testing.T) {
			app := newGenerationRequestApp(t)
			video := seedGenerationRequestVideo(t, app, "failed", "failed")
			work, _ := app.driveGenerationWork("drive-id", kind)
			work.enqueue(context.Background(), video)
			result, err := app.requestDriveGeneration(context.Background(), context.Background(), "drive-id", kind)
			if err != nil || result.State != "busy" || result.Message != generationLabel(kind)+"生成任务正在进行" {
				t.Fatalf("result = %+v, err=%v", result, err)
			}
			failed, err := app.cat.GetVideo(context.Background(), "failed")
			if err != nil || failed.PreviewStatus != "failed" || failed.FingerprintStatus != "failed" {
				t.Fatalf("busy request changed failures: %+v, %v", failed, err)
			}
			stats, err := app.cat.CountDriveAssetStatsForDrive(context.Background(), "drive-id")
			if err != nil || stats.Thumbnails["drive-id"].Failed != 1 {
				t.Fatalf("busy request changed thumbnail failures: %+v, %v", stats, err)
			}
		})
	}
}

func TestDriveGenerationRequestsAllowIndependentKindsWhileScanning(t *testing.T) {
	app := newGenerationRequestApp(t)
	seedGenerationRequestVideo(t, app, "pending", "pending")
	app.scanQueued = map[string]bool{"drive-id": true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, kind := range generationTestKinds {
		result, err := app.requestDriveGeneration(ctx, ctx, "drive-id", kind)
		if err != nil || result.State != "started" {
			t.Fatalf("%s result=%+v, err=%v", kind, result, err)
		}
	}
	waitGenerationRequestProducer(t, app)
}

func TestConcurrentDriveGenerationClicksStartOnlyOneBatch(t *testing.T) {
	for _, kind := range generationTestKinds {
		t.Run(string(kind), func(t *testing.T) {
			app := newGenerationRequestApp(t)
			seedGenerationRequestVideo(t, app, "pending", "pending")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var requests sync.WaitGroup
			results := make(chan api.DriveGenerationResult, 20)
			errors := make(chan error, 20)
			for range 20 {
				requests.Add(1)
				go func() {
					defer requests.Done()
					result, err := app.requestDriveGeneration(ctx, ctx, "drive-id", kind)
					results <- result
					errors <- err
				}()
			}
			requests.Wait()
			close(results)
			close(errors)
			for err := range errors {
				if err != nil {
					t.Fatal(err)
				}
			}
			started, busy := 0, 0
			for result := range results {
				if result.State == "started" {
					started++
				}
				if result.State == "busy" {
					busy++
				}
			}
			if started != 1 || busy != 19 {
				t.Fatalf("started=%d, busy=%d", started, busy)
			}
			waitGenerationRequestProducer(t, app)
		})
	}
}

func TestDriveGenerationRequestRejectsMissingUnavailableAndBlockedDrives(t *testing.T) {
	app := newGenerationRequestApp(t)
	seedGenerationRequestVideo(t, app, "failed", "failed")
	ctx := context.Background()
	if _, err := app.requestDriveGeneration(ctx, ctx, "missing", api.DriveGenerationThumbnails); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing drive error = %v", err)
	}
	delete(app.thumbWorkers, "drive-id")
	if _, err := app.requestDriveGeneration(ctx, ctx, "drive-id", api.DriveGenerationThumbnails); err == nil {
		t.Fatal("missing worker was acknowledged")
	}
	gate := app.driveOperationGate("drive-id")
	gate.mu.Lock()
	gate.beginBlockedLocked()
	gate.pending = true
	gate.mu.Unlock()
	result, err := app.requestDriveGeneration(ctx, ctx, "drive-id", api.DriveGenerationFingerprints)
	if err != nil || result.State != "busy" {
		t.Fatalf("blocked result=%+v, err=%v", result, err)
	}
	video, _ := app.cat.GetVideo(ctx, "failed")
	if video.FingerprintStatus != "failed" {
		t.Fatal("blocked request reset failed work")
	}
}

func TestDriveGenerationRequestDoesNotCallIneligibleFailuresReady(t *testing.T) {
	app := newGenerationRequestApp(t)
	video := seedGenerationRequestVideo(t, app, "invalid-size", "failed")
	video.Size = 0
	if err := app.cat.UpsertVideo(context.Background(), video); err != nil {
		t.Fatal(err)
	}
	result, err := app.requestDriveGeneration(context.Background(), context.Background(), "drive-id", api.DriveGenerationFingerprints)
	if err == nil || result.State == "ready" {
		t.Fatalf("ineligible failure was acknowledged as ready: %+v, %v", result, err)
	}
}

func TestAcceptedDriveGenerationSurvivesHTTPContextCancellation(t *testing.T) {
	app := newGenerationRequestApp(t)
	seedGenerationRequestVideo(t, app, "pending", "pending")
	reqCtx, cancelRequest := context.WithCancel(context.Background())
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	result, err := app.requestDriveGeneration(reqCtx, runCtx, "drive-id", api.DriveGenerationThumbnails)
	cancelRequest()
	if err != nil || result.State != "started" {
		t.Fatalf("result=%+v, err=%v", result, err)
	}
	waitGenerationRequestProducer(t, app)
	if app.thumbWorkers["drive-id"].Status().QueueLength != 1 {
		t.Fatal("HTTP context cancellation discarded accepted work")
	}
}

func TestDriveGenerationRequestReportsBusyWhileAWorkerIsProcessing(t *testing.T) {
	app := newGenerationRequestApp(t)
	video := seedGenerationRequestVideo(t, app, "active", "pending")
	drv := &budgetTestDrive{id: "drive-id", started: make(chan string, 1)}
	worker := preview.NewThumbWorker(&serverFakeTeaserGenerator{}, app.cat, drv)
	app.thumbWorkers["drive-id"] = worker
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	worker.Enqueue(video)
	select {
	case <-drv.started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	result, err := app.requestDriveGeneration(ctx, ctx, "drive-id", api.DriveGenerationThumbnails)
	if err != nil || result.State != "busy" {
		t.Fatalf("active worker result=%+v, err=%v", result, err)
	}
}

func TestStopCancelsPreparingGenerationAndAllowsAnotherRequest(t *testing.T) {
	app := newGenerationRequestApp(t)
	seedGenerationRequestVideo(t, app, "pending", "pending")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	barrier, err := app.cat.BeginWriteBarrier(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Close()
	type response struct {
		result api.DriveGenerationResult
		err    error
	}
	out := make(chan response, 1)
	go func() {
		result, err := app.requestDriveGeneration(ctx, ctx, "drive-id", api.DriveGenerationThumbnails)
		out <- response{result, err}
	}()
	gate := app.driveOperationGate("drive-id")
	deadline := time.Now().Add(time.Second)
	for {
		gate.mu.Lock()
		preparing := gate.generationRequests[api.DriveGenerationThumbnails]
		gate.mu.Unlock()
		if preparing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request did not enter preparation")
		}
		time.Sleep(time.Millisecond)
	}
	if !app.stopDriveTasks(ctx, "drive-id") {
		t.Fatal("preparing generation was not stopped")
	}
	if err := barrier.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case stopped := <-out:
		if stopped.err == nil || stopped.result.State == "started" {
			t.Fatalf("canceled preparation was acknowledged: %+v", stopped)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stopped preparation did not release its task")
	}
	app.thumbWorkers["drive-id"] = preview.NewThumbWorker(&serverFakeTeaserGenerator{}, app.cat, &serverFakeDrive{})
	result, err := app.requestDriveGeneration(ctx, ctx, "drive-id", api.DriveGenerationThumbnails)
	if err != nil || result.State != "started" {
		t.Fatalf("request after stop=%+v, err=%v", result, err)
	}
	waitGenerationRequestProducer(t, app)
	if app.thumbWorkers["drive-id"].Status().QueueLength != 1 {
		t.Fatal("request after stop did not enqueue pending work")
	}
}
