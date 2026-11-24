package main

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
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
		name      string
		enabled   bool
		scanFails bool
		canceled  bool
		wantState scanjob.State
		wantRetry bool
	}{
		// The retried media job also samples the signature while previews
		// are disabled.
		{"enabled", true, false, false, scanjob.Succeeded, true},
		{"preview disabled", false, false, false, scanjob.Succeeded, true},
		{"discovery failed", true, true, false, scanjob.Failed, false},
		{"canceled", true, false, true, scanjob.Canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, drv := failedGenerationScanApp(t, tc.enabled)
			// Hold the only slot and every source read, so the woken drive
			// leaves the retried work due: fingerprints take no slot.
			release, err := app.generationLimiter().Acquire(context.Background(), "test")
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			runCtx, stop := context.WithCancel(context.Background())
			defer stop()
			gen := &serverFakeTeaserGenerator{}
			runner := startPausedTestGeneration(t, runCtx, app, gatedStreamDrive{Drive: drv}, gen, gen)
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
			if woken := !runner.Status().Paused; woken != tc.wantRetry {
				t.Fatalf("drive woken = %v, want %v", woken, tc.wantRetry)
			}
			video, err := app.cat.GetVideo(context.Background(), "failed")
			if err != nil {
				t.Fatal(err)
			}
			want := "failed"
			if tc.wantRetry {
				want = "pending"
			}
			if video.PreviewStatus != want || video.FingerprintStatus != want {
				t.Fatalf("preview/fingerprint state = %s/%s, want %s", video.PreviewStatus, video.FingerprintStatus, want)
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

func (g *failingRetryTeaserGenerator) Generate(context.Context, *drives.StreamLink, float64, preview.MediaRequest) (preview.Media, error) {
	g.record("attempt")
	return preview.Media{}, errors.New("invalid media")
}

func (g *failingRetryTeaserGenerator) attempts() int {
	attempts := 0
	for _, event := range g.Events() {
		if event == "attempt" {
			attempts++
		}
	}
	return attempts
}

func TestFailedGenerationWaitsForNextScanBeforeRetryingAgain(t *testing.T) {
	app, drv := failedGenerationScanApp(t, true)
	gen := &failingRetryTeaserGenerator{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	startPausedTestGeneration(t, ctx, app, drv, gen, gen)

	for round := 1; round <= 2; round++ {
		if result := app.runScan(ctx, drv.ID()); result.State != scanjob.Succeeded {
			t.Fatalf("scan = %+v", result)
		}
		waitGenerationIdle(t, app, drv.ID())
		video, err := app.cat.GetVideo(ctx, "failed")
		if err != nil || video.PreviewStatus != "failed" {
			t.Fatalf("failed attempt did not retain its state: %+v, %v", video, err)
		}
		// Waking the drive again, as maintenance does after a scan, must not
		// turn a terminal failure into an endless retry loop.
		app.wakeDriveGeneration(drv.ID())
		waitGenerationIdle(t, app, drv.ID())
		if attempts := gen.attempts(); attempts != round {
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
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			generation := startPausedTestGeneration(t, ctx, app, drv, gen, gen)
			runner := nightly.New(nightly.Config{
				Settings: app.cat, StartTime: "05:00", Timezone: "Etc/UTC",
				Now:             func() time.Time { return time.Date(2026, 9, 6, 6, 0, 0, 0, time.UTC) },
				ListScanTargets: app.listScanTargetIDs, RunScan: app.runScan,
				WaitGenerationIdle: app.waitAllGenerationIdle,
				RunDedupeAssetCleanup: func(context.Context) error {
					if gen.attempts() != 1 || generation.Busy() {
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
			if ctx.Err() != nil || status.Outcome != scanjob.Succeeded || len(status.ScanResults) != 1 || gen.attempts() != 1 {
				t.Fatalf("maintenance = %+v, attempts = %v, err = %v", status, gen.Events(), ctx.Err())
			}
		})
	}
}
