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
)

var generationTestKinds = []api.DriveGenerationKind{api.DriveGenerationThumbnails, api.DriveGenerationPreviews, api.DriveGenerationFingerprints}

// newGenerationRequestApp runs the drive's generation with fake generators
// while the test holds the only slot and the source reads, so requests leave
// their work due. The returned release lets the runner take it.
func newGenerationRequestApp(t *testing.T) (*App, func()) {
	t.Helper()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	seedGenerationDrive(t, cat, "drive-id")
	app := &App{cat: cat}
	releaseSlot, err := app.generationLimiter().Acquire(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	open := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(open); releaseSlot() }) }
	t.Cleanup(release)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	gen := &serverFakeTeaserGenerator{}
	startTestGeneration(t, ctx, app, gatedStreamDrive{Drive: &serverFakeDrive{}, open: open}, gen, gen)
	return app, release
}

func seedGenerationRequestVideo(t *testing.T, app *App, id, state string) *catalog.Video {
	t.Helper()
	now := time.Now()
	v := &catalog.Video{ID: id, DriveID: "drive-id", FileID: id, FileName: id + ".mp4", Title: id, Size: 123,
		PreviewStatus: state, FingerprintStatus: state, DurationSeconds: 30, CreatedAt: now, PublishedAt: now}
	if state == "ready" {
		v.PreviewLocal = id + ".mp4"
		v.ThumbnailURL = "/p/thumb/" + id
		v.SampledSHA256 = "s2:" + strings.Repeat("a", 64)
	}
	if err := app.cat.UpsertVideo(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	markMediaDone(t, app.cat, id)
	if state == "failed" {
		if err := app.cat.UpdateVideoMeta(context.Background(), id, catalog.VideoMetaPatch{ThumbnailStatus: "failed"}); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := app.cat.GetVideo(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func dueCounts(t *testing.T, app *App) catalog.GenerationCounts {
	t.Helper()
	counts, err := app.cat.CountDueGeneration(context.Background(), "drive-id", app.previewEnabled(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestDriveGenerationRequestReportsReadyWithoutReopeningCompletedResources(t *testing.T) {
	for _, kind := range generationTestKinds {
		t.Run(string(kind), func(t *testing.T) {
			app, _ := newGenerationRequestApp(t)
			seedGenerationRequestVideo(t, app, "ready", "ready")
			result, err := app.requestDriveGeneration(context.Background(), "drive-id", kind)
			if err != nil || result.State != "ready" || result.Message != generationLabel(kind)+"已全部就绪" {
				t.Fatalf("result = %+v, err=%v", result, err)
			}
			if counts := dueCounts(t, app); counts.Any() {
				t.Fatalf("completed resources became due: %+v", counts)
			}
		})
	}
}

func TestDriveGenerationRequestKeepsExistingThumbnailReadyWithMissingDuration(t *testing.T) {
	ctx := context.Background()
	app, _ := newGenerationRequestApp(t)
	video := seedGenerationRequestVideo(t, app, "duration-only", "ready")
	if err := app.cat.UpdateVideoMeta(ctx, video.ID, catalog.VideoMetaPatch{DurationSecondsSet: true}); err != nil {
		t.Fatal(err)
	}
	result, err := app.requestDriveGeneration(ctx, video.DriveID, api.DriveGenerationThumbnails)
	if err != nil || result.State != "ready" {
		t.Fatalf("missing duration made ready covers fail: %+v, %v", result, err)
	}
	if counts := dueCounts(t, app); counts.Thumbnails != 0 || counts.Durations != 1 {
		t.Fatalf("counts = %+v, want only the duration due", counts)
	}
}

func TestDriveGenerationRequestRetriesFailedResourcesOfOnlyItsKind(t *testing.T) {
	for _, kind := range generationTestKinds {
		t.Run(string(kind), func(t *testing.T) {
			app, _ := newGenerationRequestApp(t)
			seedGenerationRequestVideo(t, app, "pending", "pending")
			seedGenerationRequestVideo(t, app, "failed", "failed")
			seedGenerationRequestVideo(t, app, "ready", "ready")
			ctx := context.Background()
			result, err := app.requestDriveGeneration(ctx, "drive-id", kind)
			if err != nil || result.State != "started" {
				t.Fatalf("result = %+v, err=%v", result, err)
			}
			if due := dueOfKind(dueCounts(t, app), kind); due != 2 {
				t.Fatalf("%s due = %d, want the pending and the retried video", kind, due)
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

func TestDriveGenerationRequestsAllowEveryKindWhileScanning(t *testing.T) {
	app, _ := newGenerationRequestApp(t)
	seedGenerationRequestVideo(t, app, "failed", "failed")
	app.scanQueued = map[string]bool{"drive-id": true}
	for _, kind := range generationTestKinds {
		result, err := app.requestDriveGeneration(context.Background(), "drive-id", kind)
		if err != nil || result.State != "started" {
			t.Fatalf("%s result=%+v, err=%v", kind, result, err)
		}
	}
}

func TestRepeatedDriveGenerationRequestsShareTheDueWork(t *testing.T) {
	app, _ := newGenerationRequestApp(t)
	seedGenerationRequestVideo(t, app, "pending", "pending")
	var requests sync.WaitGroup
	failures := make(chan error, 20)
	for range 20 {
		requests.Add(1)
		go func() {
			defer requests.Done()
			result, err := app.requestDriveGeneration(context.Background(), "drive-id", api.DriveGenerationPreviews)
			if err == nil && result.State != "started" {
				err = errors.New("request was not started: " + result.State)
			}
			failures <- err
		}()
	}
	requests.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if counts := dueCounts(t, app); counts.Media != 1 {
		t.Fatalf("media due = %d, want the one pending video", counts.Media)
	}
}

func TestDriveGenerationRequestRejectsMissingUnavailableAndBlockedDrives(t *testing.T) {
	app, _ := newGenerationRequestApp(t)
	seedGenerationRequestVideo(t, app, "failed", "failed")
	ctx := context.Background()
	if _, err := app.requestDriveGeneration(ctx, "missing", api.DriveGenerationThumbnails); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing drive error = %v", err)
	}
	seedGenerationDrive(t, app.cat, "detached")
	if _, err := app.requestDriveGeneration(ctx, "detached", api.DriveGenerationThumbnails); err == nil {
		t.Fatal("a drive without generation was acknowledged")
	}
	gate := app.driveOperationGate("drive-id")
	gate.mu.Lock()
	gate.beginBlockedLocked()
	gate.pending = true
	gate.mu.Unlock()
	result, err := app.requestDriveGeneration(ctx, "drive-id", api.DriveGenerationFingerprints)
	if err != nil || result.State != "busy" {
		t.Fatalf("blocked result=%+v, err=%v", result, err)
	}
	video, _ := app.cat.GetVideo(ctx, "failed")
	if video.FingerprintStatus != "failed" {
		t.Fatal("blocked request reset failed work")
	}
}

func TestDriveGenerationRequestDoesNotCallIneligibleFailuresReady(t *testing.T) {
	app, _ := newGenerationRequestApp(t)
	video := seedGenerationRequestVideo(t, app, "invalid-size", "failed")
	video.Size = 0
	if err := app.cat.UpsertVideo(context.Background(), video); err != nil {
		t.Fatal(err)
	}
	result, err := app.requestDriveGeneration(context.Background(), "drive-id", api.DriveGenerationFingerprints)
	if err == nil || result.State == "ready" {
		t.Fatalf("ineligible failure was acknowledged as ready: %+v, %v", result, err)
	}
}

func TestAcceptedDriveGenerationSurvivesHTTPContextCancellation(t *testing.T) {
	app, release := newGenerationRequestApp(t)
	seedGenerationRequestVideo(t, app, "failed", "failed")
	reqCtx, cancelRequest := context.WithCancel(context.Background())
	result, err := app.requestDriveGeneration(reqCtx, "drive-id", api.DriveGenerationThumbnails)
	cancelRequest()
	if err != nil || result.State != "started" {
		t.Fatalf("result=%+v, err=%v", result, err)
	}
	release()
	waitGenerationIdle(t, app, "drive-id")
	if got, err := app.cat.GetVideo(context.Background(), "failed"); err != nil || got.ThumbnailURL == "" {
		t.Fatalf("video = %+v, %v; want the retried cover generated", got, err)
	}
}
