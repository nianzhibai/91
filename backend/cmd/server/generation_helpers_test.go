package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/generation"
	"github.com/nianzhibai/91/backend/internal/preview"
)

// startTestGeneration runs the drive's generation runner with fake
// generators. A nil generator leaves its steps out.
func startTestGeneration(t *testing.T, ctx context.Context, app *App, drv drives.Drive, media preview.MediaGenerator, thumbnails preview.ThumbnailGenerator) *generation.Runner {
	t.Helper()
	runner := app.newDriveGenerationRunner(drv, media, thumbnails, false)
	app.registerDriveGeneration(ctx, drv.ID(), runner)
	return runner
}

// waitGenerationIdle waits until the drive's runner has nothing due.
func waitGenerationIdle(t *testing.T, app *App, driveID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.waitDriveGenerationIdle(ctx, driveID); err != nil {
		status := generation.Status{}
		if runner := app.generationRunner(driveID); runner != nil {
			status = runner.Status()
		}
		t.Fatalf("drive %s generation did not become idle: %v (status %+v)", driveID, err, status)
	}
}

// serverBlockingProbeGenerator holds each duration probe until release and
// then fails it, leaving the duration missing.
type serverBlockingProbeGenerator struct {
	started chan string
	release chan struct{}
}

func (g *serverBlockingProbeGenerator) Probe(ctx context.Context, link *drives.StreamLink) (float64, error) {
	g.started <- link.URL
	select {
	case <-g.release:
		return 0, errors.New("probe failed")
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (g *serverBlockingProbeGenerator) GenerateThumbnail(context.Context, *drives.StreamLink, string, float64) (string, error) {
	return "", errors.New("unexpected thumbnail generation")
}

// serverObservingMediaGenerator reports each media job before producing its
// teaser and cover.
type serverObservingMediaGenerator struct {
	serverFakeTeaserGenerator
	onGenerate func()
}

func (g *serverObservingMediaGenerator) Generate(ctx context.Context, link *drives.StreamLink, duration float64, request preview.MediaRequest) (preview.Media, error) {
	if g.onGenerate != nil {
		g.onGenerate()
	}
	return g.serverFakeTeaserGenerator.Generate(ctx, link, duration, request)
}

// startPausedTestGeneration registers a stopped runner that takes work only
// after a wake, as one that a task is about to wake.
func startPausedTestGeneration(t *testing.T, ctx context.Context, app *App, drv drives.Drive, media preview.MediaGenerator, thumbnails preview.ThumbnailGenerator) *generation.Runner {
	t.Helper()
	runner := app.newDriveGenerationRunner(drv, media, thumbnails, true)
	app.registerDriveGeneration(ctx, drv.ID(), runner)
	return runner
}

// gatedStreamDrive holds every source read until open is closed or the
// caller gives up. Fingerprints take no slot, so a test that holds the slot
// to keep work due holds the source reads too. A nil open never opens.
type gatedStreamDrive struct {
	drives.Drive
	open <-chan struct{}
}

func (d gatedStreamDrive) StreamURL(ctx context.Context, fileID string) (*drives.StreamLink, error) {
	select {
	case <-d.open:
		return d.Drive.StreamURL(ctx, fileID)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
