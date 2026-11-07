package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/preview"
)

func TestInitialGenerationContinuesWithFullFingerprintQueue(t *testing.T) {
	app := newGenerationRequestApp(t)
	video := seedGenerationRequestVideo(t, app, "needs-assets", "pending")
	duration := seedGenerationRequestVideo(t, app, "needs-duration", "ready")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	if err := app.cat.UpdateVideoMeta(ctx, duration.ID, catalog.VideoMetaPatch{DurationSecondsSet: true}); err != nil {
		cancel()
		t.Fatal(err)
	}

	fingerprints := app.fingerprintWorkers[video.DriveID]
	capacity := 0
	// Keep the fingerprint worker stopped so its producer cannot regain space.
	for fingerprints.Enqueue(&catalog.Video{ID: fmt.Sprintf("buffered-%d", capacity)}) {
		capacity++
	}
	gen := &durationBackfillGenerator{calls: make(map[string]int)}
	drv := &durationBackfillDrive{}
	thumbnails := preview.NewThumbWorker(gen, app.cat, drv)
	previews := preview.NewWorker(gen, app.cat, drv)
	var workers sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		workers.Wait()
		waitGenerationRequestProducer(t, app)
	})
	for _, run := range []func(context.Context){thumbnails.Run, previews.Run} {
		workers.Add(1)
		go func(run func(context.Context)) {
			defer workers.Done()
			run(ctx)
		}(run)
	}
	app.registerPreviewWorkers(ctx, video.DriveID, previews, thumbnails, fingerprints, cancel)

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, err := app.cat.GetVideo(ctx, video.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.ThumbnailURL != "" && current.PreviewStatus == "ready" && fingerprints.Status().QueueLength > capacity {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("fingerprint backpressure blocked initial thumbnail or preview generation")
		case <-ticker.C:
		}
	}
	if !app.fingerprintQueueingBusy(video.DriveID) {
		t.Fatal("fingerprint producer finished despite its full queue")
	}

	waitCtx, stopWait := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stopWait()
	if err := app.waitDurationBackfillsIdle(waitCtx, video.DriveID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("duration completion did not wait for blocked resource admission: %v", err)
	}
	gen.mu.Lock()
	probes := gen.calls["https://video.example/"+duration.FileID]
	gen.mu.Unlock()
	if probes != 0 {
		t.Fatal("duration backfill began before fingerprint admission finished")
	}

	// Cancellation must release both producers and the pending duration sweep.
	cancel()
	stopCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := app.waitDurationBackfillsIdle(stopCtx, video.DriveID); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled duration sweep did not finish: %v", err)
	}
	waitGenerationRequestProducer(t, app)
	app.mu.Lock()
	backfill := app.durationBackfills[video.DriveID]
	app.mu.Unlock()
	if backfill.busy() || app.fingerprintQueueingBusy(video.DriveID) {
		t.Fatal("canceled initial generation left resource admission active")
	}
}
