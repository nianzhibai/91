package main

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/fingerprint"
	"github.com/nianzhibai/91/backend/internal/nightly"
	"github.com/nianzhibai/91/backend/internal/preview"
	"github.com/nianzhibai/91/backend/internal/scanjob"
)

func failedGenerationScanApp(t *testing.T, enabled bool) (*App, *serverTreeScanDrive) {
	t.Helper()
	app, drv := scanResultTestApp(t)
	ctx := context.Background()
	app.applyPreviewEnabled(ctx, enabled)
	now := time.Now()
	for _, id := range []string{"failed", "removed"} {
		if err := app.cat.UpsertVideo(ctx, &catalog.Video{
			ID: id, DriveID: drv.ID(), FileID: id, FileName: id + ".mp4", Title: id,
			ParentID: "root", AncestorDirIDs: []string{"root"}, Size: 123,
			PreviewStatus: "failed", FingerprintStatus: "failed", FingerprintError: "previous failure",
			CreatedAt: now, PublishedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		if err := app.cat.UpdateVideoMeta(ctx, id, catalog.VideoMetaPatch{ThumbnailStatus: "failed"}); err != nil {
			t.Fatal(err)
		}
	}
	drv.entries["root"] = []drives.Entry{{ID: "failed", Name: "failed.mp4", Size: 123}}
	return app, drv
}

func TestScanRetriesFailedGenerationAfterCleanup(t *testing.T) {
	for _, tc := range []struct {
		name       string
		enabled    bool
		workers    bool
		scanFails  bool
		canceled   bool
		wantState  scanjob.State
		wantQueues [3]int
	}{
		{"enabled", true, true, false, false, scanjob.Succeeded, [3]int{1, 1, 1}},
		{"preview disabled", false, true, false, false, scanjob.Succeeded, [3]int{1, 0, 1}},
		{"workers unavailable", true, false, false, false, scanjob.Succeeded, [3]int{}},
		{"discovery failed", true, true, true, false, scanjob.Failed, [3]int{}},
		{"canceled", true, true, false, true, scanjob.Canceled, [3]int{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, drv := failedGenerationScanApp(t, tc.enabled)
			gen := &serverFakeTeaserGenerator{}
			thumb := preview.NewThumbWorker(gen, app.cat, drv)
			teaser := preview.NewWorker(gen, app.cat, drv)
			fp := fingerprint.NewWorker(app.cat, drv, fingerprint.Config{})
			if tc.workers {
				app.thumbWorkers = map[string]*preview.ThumbWorker{drv.ID(): thumb}
				app.workers = map[string]*preview.Worker{drv.ID(): teaser}
				app.fingerprintWorkers = map[string]*fingerprint.Worker{drv.ID(): fp}
			}
			if tc.scanFails {
				drv.listErrors = map[string]error{"root": errors.New("discovery unavailable")}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			result := app.runScan(ctx, drv.ID())
			if result.State != tc.wantState {
				t.Fatalf("scan = %+v", result)
			}
			queues := [3]int{thumb.Status().QueueLength, teaser.Status().QueueLength, fp.Status().QueueLength}
			if queues != tc.wantQueues {
				t.Fatalf("queue lengths = %v, want %v", queues, tc.wantQueues)
			}
			video, err := app.cat.GetVideo(context.Background(), "failed")
			if err != nil {
				t.Fatal(err)
			}
			wantPreview := "failed"
			if tc.wantQueues[1] > 0 {
				wantPreview = "pending"
			}
			if video.PreviewStatus != wantPreview {
				t.Fatalf("preview state = %s, want %s", video.PreviewStatus, wantPreview)
			}
			if !tc.scanFails && !tc.canceled {
				if _, err := app.cat.GetVideo(context.Background(), "removed"); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("stale source must be removed before generation retry: %v", err)
				}
			}
		})
	}
}

type failingRetryTeaserGenerator struct {
	serverFakeTeaserGenerator
}

func (g *failingRetryTeaserGenerator) Generate(context.Context, *drives.StreamLink, float64) (string, error) {
	g.record("attempt")
	return "", errors.New("invalid media")
}

func TestFailedGenerationWaitsForNextScanBeforeRetryingAgain(t *testing.T) {
	app, drv := failedGenerationScanApp(t, true)
	gen := &failingRetryTeaserGenerator{}
	worker := preview.NewWorker(gen, app.cat, drv)
	app.workers = map[string]*preview.Worker{drv.ID(): worker}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	for round := 1; round <= 2; round++ {
		if result := app.runScan(ctx, drv.ID()); result.State != scanjob.Succeeded {
			t.Fatalf("scan = %+v", result)
		}
		if err := worker.WaitIdle(ctx); err != nil {
			t.Fatal(err)
		}
		video, err := app.cat.GetVideo(ctx, "failed")
		if err != nil || video.PreviewStatus != "failed" {
			t.Fatalf("failed attempt did not retain its state: %+v, %v", video, err)
		}
		// Normal queue refill, including maintenance after a scan, must not
		// turn a terminal failure into an endless retry loop.
		app.enqueueDriveGeneration(ctx, drv.ID(), worker, nil)
		if err := worker.WaitIdle(ctx); err != nil {
			t.Fatal(err)
		}
		if attempts := len(gen.Events()); attempts != round {
			t.Fatalf("round %d generated %d attempts", round, attempts)
		}
	}
}

func TestMaintenanceRetriesFailedGenerationAndWaitsForCompletion(t *testing.T) {
	for _, scheduled := range []bool{false, true} {
		name := "manual scan all"
		if scheduled {
			name = "scheduled"
		}
		t.Run(name, func(t *testing.T) {
			app, drv := failedGenerationScanApp(t, true)
			gen := &failingRetryTeaserGenerator{}
			worker := preview.NewWorker(gen, app.cat, drv)
			app.workers = map[string]*preview.Worker{drv.ID(): worker}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			workerDone := make(chan struct{})
			go func() { defer close(workerDone); worker.Run(ctx) }()
			t.Cleanup(func() { cancel(); <-workerDone })
			runner := nightly.New(nightly.Config{
				Settings: app.cat, StartTime: "05:00", Timezone: "Etc/UTC",
				Now:             func() time.Time { return time.Date(2026, 9, 6, 6, 0, 0, 0, time.UTC) },
				ListScanTargets: app.listScanTargetIDs, RunScan: app.runScan,
				WaitPreviewQueuesIdle: app.waitAllPreviewQueuesIdle,
				RunDedupeAssetCleanup: func(context.Context) error {
					if len(gen.Events()) != 1 || worker.Status().State != "idle" {
						return errors.New("maintenance started before retried generation finished")
					}
					return nil
				},
			})
			if scheduled {
				if err := runner.UpdateStartTime("06:00"); err != nil {
					t.Fatal(err)
				}
			} else if !runner.TriggerScanAll() {
				t.Fatal("manual trigger rejected")
			}
			runnerDone := make(chan struct{})
			go func() { defer close(runnerDone); runner.Run(ctx) }()
			t.Cleanup(func() { cancel(); <-runnerDone })
			for runner.Status().LastFinishedAt.IsZero() && ctx.Err() == nil {
				time.Sleep(10 * time.Millisecond)
			}
			status := runner.Status()
			if ctx.Err() != nil || status.Outcome != scanjob.Succeeded || len(status.ScanResults) != 1 || len(gen.Events()) != 1 {
				t.Fatalf("maintenance = %+v, attempts = %v, err = %v", status, gen.Events(), ctx.Err())
			}
		})
	}
}
