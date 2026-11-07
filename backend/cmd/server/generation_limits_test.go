package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/tasklimit"
)

// Blocking at the provider boundary proves that waiting tasks do not obtain
// URLs, probe media or start FFmpeg before a global slot becomes available.
type budgetTestDrive struct {
	drives.Drive
	id      string
	started chan string
}

func (d *budgetTestDrive) ID() string   { return d.id }
func (d *budgetTestDrive) Kind() string { return "test" }
func (d *budgetTestDrive) StreamURL(ctx context.Context, fileID string) (*drives.StreamLink, error) {
	d.started <- fileID
	<-ctx.Done()
	return nil, ctx.Err()
}

func assertBudgetAvailable(t *testing.T, limiter *tasklimit.Limiter, count int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := 0; i < count; i++ {
		release, err := limiter.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire %d/%d: %v", i+1, count, err)
		}
		defer release()
	}
	blocked, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if release, err := limiter.Acquire(blocked); err == nil {
		release()
		t.Fatal("global budget exceeded")
	}
}

func TestGenerationWorkersShareLiveLimitsAcrossDrivesAndRemounts(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a := &App{cat: cat}
	ctx, cancel := context.WithCancel(context.Background())
	var tasks sync.WaitGroup
	defer func() { cancel(); tasks.Wait() }()
	started := make(chan string, 20)
	thumbnails, previews, fingerprints := a.generationLimits()
	for _, id := range []string{"drive-a", "drive-b"} {
		drv := &budgetTestDrive{id: id, started: started}
		pw, tw, fw := a.newDriveGenerationWorkers(drv)
		if pw.Limiter != previews || tw.Limiter != thumbnails || fw.Config.Limiter != fingerprints {
			t.Fatal("worker did not receive shared budgets")
		}
		for kind, enqueue := range map[string]func(*catalog.Video) bool{"preview": pw.Enqueue, "thumb": tw.Enqueue, "fingerprint": fw.Enqueue} {
			v := &catalog.Video{ID: id + kind, DriveID: id, FileID: kind, Title: kind, Size: 10, PreviewStatus: "pending", FingerprintStatus: "pending"}
			if err := cat.UpsertVideo(ctx, v); err != nil {
				t.Fatal(err)
			}
			if !enqueue(v) {
				t.Fatal("enqueue failed")
			}
		}
		for _, run := range []func(context.Context){pw.Run, tw.Run, fw.Run} {
			tasks.Add(1)
			go func() { defer tasks.Done(); run(ctx) }()
		}
	}
	for range 3 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("default budgets did not start thumbnail, preview and fingerprint work")
		}
	}
	select {
	case kind := <-started:
		t.Fatalf("extra %s task started with default budgets", kind)
	case <-time.After(40 * time.Millisecond):
	}
	settings := config.DefaultLiveSettings()
	settings.ThumbnailConcurrency = 2
	settings.PreviewConcurrency = 2
	settings.FingerprintConcurrency = 2
	if err := a.applyLiveConfig(ctx, settings); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("live increase did not unblock waiting workers")
		}
	}
	select {
	case kind := <-started:
		t.Fatalf("extra %s task exceeded resized budgets", kind)
	case <-time.After(40 * time.Millisecond):
	}
	for _, id := range []string{"drive-a", "late-drive"} {
		pw, tw, fw := a.newDriveGenerationWorkers(&budgetTestDrive{id: id, started: started})
		if pw.Limiter != previews || tw.Limiter != thumbnails || fw.Config.Limiter != fingerprints {
			t.Fatal("remount or late attachment replaced shared budget")
		}
	}
	cancel()
	tasks.Wait()
	assertBudgetAvailable(t, thumbnails, 2)
	assertBudgetAvailable(t, previews, 2)
	assertBudgetAvailable(t, fingerprints, 2)
}
