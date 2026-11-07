package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/preview"
)

// Observe the waiter's first blocking queue wait without relying on a sleep to
// race generation dispatch. The initially empty cover queue does not call Done.
type generationWaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *generationWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

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

func TestGenerationWaitIncludesCoverScheduledByPreviewCompletion(t *testing.T) {
	for _, allDrives := range []bool{false, true} {
		name := "single drive"
		if allDrives {
			name = "all drives"
		}
		t.Run(name, func(t *testing.T) {
			app := newGenerationRequestApp(t)
			video := seedGenerationRequestVideo(t, app, "dependent-cover", "pending")
			drv := &serverFakeDrive{}
			coverGen := &dependentCoverGenerator{started: make(chan struct{}), release: make(chan struct{})}
			cover := preview.NewThumbWorker(coverGen, app.cat, drv)
			teaser := preview.NewWorker(&serverFakeTeaserGenerator{}, app.cat, drv)
			teaser.OnPreviewReady = func(ready *catalog.Video) {
				if !cover.EnqueueFollowUp(ready) {
					t.Error("dependent cover enqueue failed")
				}
			}
			app.thumbWorkers[video.DriveID], app.workers[video.DriveID] = cover, teaser
			if !teaser.Enqueue(video) {
				t.Fatal("preview enqueue failed")
			}
			runCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			waitCtx := &generationWaitContext{Context: runCtx, entered: make(chan struct{})}
			waited := make(chan error, 1)
			go func() {
				if allDrives {
					waited <- app.waitAllPreviewQueuesIdle(waitCtx)
				} else {
					waited <- app.waitDriveGenerationQueuesIdle(waitCtx, video.DriveID)
				}
			}()
			select {
			case <-waitCtx.entered:
			case <-runCtx.Done():
				t.Fatal("queue wait did not start")
			}
			var workers sync.WaitGroup
			for _, run := range []func(context.Context){cover.Run, teaser.Run} {
				workers.Add(1)
				go func(run func(context.Context)) { defer workers.Done(); run(runCtx) }(run)
			}
			t.Cleanup(func() { cancel(); workers.Wait() })
			select {
			case <-coverGen.started:
			case <-runCtx.Done():
				t.Fatal("preview did not schedule a dependent cover")
			}
			select {
			case err := <-waited:
				t.Fatalf("generation wait returned before dependent cover completed: %v", err)
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
