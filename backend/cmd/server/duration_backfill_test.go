package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
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

func (g *durationBackfillGenerator) probes(fileID string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls["https://video.example/"+fileID]
}

func TestDriveDurationProbeFailureWaitsForTheNextScan(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	seedGenerationDrive(t, cat, "drive-id")
	app := &App{cat: cat}
	ctx := context.Background()
	now := time.Now()
	originals := make(map[string]*catalog.Video)
	for _, id := range []string{"failed", "good", "known"} {
		video := &catalog.Video{ID: id, DriveID: "drive-id", FileID: id, Title: id, Size: 100,
			ThumbnailURL: "/p/thumb/" + id, CreatedAt: now, PublishedAt: now, UpdatedAt: now}
		if id == "known" {
			video.DurationSeconds = 42
		}
		if err := app.cat.UpsertVideo(ctx, video); err != nil {
			t.Fatal(err)
		}
		markMediaDone(t, app.cat, id)
		if _, err := app.cat.UpdateVideoFingerprintForSource(ctx, video, "s2:"+id, "ready", ""); err != nil {
			t.Fatal(err)
		}
		original, err := app.cat.GetVideo(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		originals[id] = original
	}
	gen := &durationBackfillGenerator{calls: make(map[string]int), fail: true}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	startTestGeneration(t, runCtx, app, &durationBackfillDrive{}, nil, gen)
	waitGenerationIdle(t, app, "drive-id")
	if gen.probes("failed") != 1 || gen.probes("good") != 1 || gen.probes("known") != 0 {
		t.Fatalf("first probes = %v", gen.calls)
	}

	gen.mu.Lock()
	gen.fail = false
	gen.mu.Unlock()
	app.wakeDriveGeneration("drive-id")
	waitGenerationIdle(t, app, "drive-id")
	if gen.probes("failed") != 1 {
		t.Fatal("a failed probe was retried before the next scan")
	}

	// A scan retries failed work, which lifts the deferral.
	if _, err := app.resetFailedGeneration(ctx, "drive-id", catalog.GenerationKinds{Thumbnails: true, Previews: true, Fingerprints: true}); err != nil {
		t.Fatal(err)
	}
	app.wakeDriveGeneration("drive-id")
	waitGenerationIdle(t, app, "drive-id")
	if gen.probes("failed") != 2 || gen.probes("good") != 1 {
		t.Fatalf("probes after the scan = %v", gen.calls)
	}
	for _, id := range []string{"failed", "good"} {
		current, err := app.cat.GetVideo(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if current.DurationSeconds != 19 || current.ThumbnailURL != originals[id].ThumbnailURL || !current.ThumbnailUpdatedAt.Equal(originals[id].ThumbnailUpdatedAt) {
			t.Fatalf("duration probe changed the cover or did not recover %s: %+v", id, current)
		}
	}
}
