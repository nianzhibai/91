package main

import (
	"context"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

type dependentCoverGenerator struct {
	serverFakeTeaserGenerator
	started chan struct{}
	release chan struct{}
}

func (g *dependentCoverGenerator) GenerateThumbnail(ctx context.Context, _ *drives.StreamLink, id string, _ float64) (string, error) {
	close(g.started)
	select {
	case <-g.release:
		return "/tmp/" + id + ".jpg", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// The media job leaves this video without a cover; waiting for the drive
// includes the cover step that follows it.
func TestGenerationWaitIncludesCoverAfterMediaJob(t *testing.T) {
	for _, allDrives := range []bool{false, true} {
		name := "single drive"
		if allDrives {
			name = "all drives"
		}
		t.Run(name, func(t *testing.T) {
			app, release := newGenerationRequestApp(t)
			video := seedGenerationRequestVideo(t, app, "dependent-cover", "pending")
			coverGen := &dependentCoverGenerator{started: make(chan struct{}), release: make(chan struct{})}
			runCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			startTestGeneration(t, runCtx, app, &serverFakeDrive{}, &serverFakeTeaserGenerator{}, coverGen)
			release()
			waited := make(chan error, 1)
			go func() {
				if allDrives {
					waited <- app.waitAllGenerationIdle(runCtx)
				} else {
					waited <- app.waitDriveGenerationIdle(runCtx, video.DriveID)
				}
			}()
			select {
			case <-coverGen.started:
			case <-runCtx.Done():
				t.Fatal("the drive did not reach the cover step")
			}
			select {
			case err := <-waited:
				t.Fatalf("generation wait returned before the cover completed: %v", err)
			case <-time.After(300 * time.Millisecond):
			}
			close(coverGen.release)
			select {
			case err := <-waited:
				if err != nil {
					t.Fatal(err)
				}
			case <-runCtx.Done():
				t.Fatal("generation wait did not finish")
			}
		})
	}
}
