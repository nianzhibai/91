package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
	"github.com/nianzhibai/91/backend/internal/dedupe"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/tasklimit"
)

// Blocking at the provider boundary proves that waiting drives do not obtain
// URLs, probe media or start FFmpeg before a slot becomes available.
type budgetTestDrive struct {
	drives.Drive
	id      string
	kind    string
	started chan string
}

func (d *budgetTestDrive) ID() string { return d.id }
func (d *budgetTestDrive) Kind() string {
	if d.kind == "" {
		return "test"
	}
	return d.kind
}
func (d *budgetTestDrive) StreamURL(ctx context.Context, fileID string) (*drives.StreamLink, error) {
	d.started <- d.id
	<-ctx.Done()
	return nil, ctx.Err()
}

func assertBudgetAvailable(t *testing.T, limiter *tasklimit.Limiter, count int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := 0; i < count; i++ {
		release, err := limiter.Acquire(ctx, "test")
		if err != nil {
			t.Fatalf("acquire %d/%d: %v", i+1, count, err)
		}
		defer release()
	}
	blocked, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if release, err := limiter.Acquire(blocked, "test"); err == nil {
		release()
		t.Fatal("drive budget exceeded")
	}
}

func TestCloudDrivesShareTheLiveGenerationConcurrency(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a := &App{cat: cat}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan string, 20)
	limiter := a.generationLimiter()
	for _, id := range []string{"drive-a", "drive-b", "drive-c"} {
		for _, file := range []string{"one", "two"} {
			// The fingerprint is settled: only asset steps take a slot.
			v := &catalog.Video{ID: id + file, DriveID: id, FileID: file, Title: file, Size: 10,
				SampledSHA256: dedupe.SampledFingerprintPrefix + id + file, FingerprintStatus: "ready"}
			if err := cat.UpsertVideo(ctx, v); err != nil {
				t.Fatal(err)
			}
		}
		drv := &budgetTestDrive{id: id, started: started}
		a.registerDriveGeneration(ctx, id, a.newDriveGenerationRunner(drv, &serverFakeTeaserGenerator{}, &serverFakeTeaserGenerator{}, false))
	}
	expectDrives := func(count int, reason string) map[string]bool {
		t.Helper()
		working := map[string]bool{}
		for range count {
			select {
			case id := <-started:
				if working[id] {
					t.Fatalf("drive %s took a second slot", id)
				}
				working[id] = true
			case <-time.After(time.Second):
				t.Fatalf("%s: only %d drives started", reason, len(working))
			}
		}
		select {
		case id := <-started:
			t.Fatalf("%s: extra drive %s started", reason, id)
		case <-time.After(40 * time.Millisecond):
		}
		return working
	}
	first := expectDrives(1, "default budget")
	settings := config.DefaultLiveSettings()
	settings.GenerationConcurrency = 2
	if err := a.applyLiveConfig(ctx, settings); err != nil {
		t.Fatal(err)
	}
	second := expectDrives(1, "live increase")
	for id := range second {
		if first[id] {
			t.Fatalf("drive %s started twice", id)
		}
	}
	if a.generationLimiter() != limiter {
		t.Fatal("live update replaced the shared budget")
	}
	cancel()
	for _, runner := range a.generationRunners() {
		waitCtx, stop := context.WithTimeout(context.Background(), time.Second)
		if err := runner.WaitIdle(waitCtx); err != nil {
			t.Fatal(err)
		}
		stop()
	}
	assertBudgetAvailable(t, limiter, 2)
}

func TestFingerprintsTakeNoGenerationSlot(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a := &App{cat: cat}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan string, 20)
	ids := []string{"drive-a", "drive-b", "drive-c"}
	for _, id := range ids {
		v := &catalog.Video{ID: id + "-new", DriveID: id, FileID: "new", Title: "new", Size: 10, FingerprintStatus: "pending"}
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatal(err)
		}
		drv := &budgetTestDrive{id: id, started: started}
		a.registerDriveGeneration(ctx, id, a.newDriveGenerationRunner(drv, &serverFakeTeaserGenerator{}, &serverFakeTeaserGenerator{}, false))
	}
	// The default budget is one video, yet every drive samples its source.
	working := map[string]bool{}
	for range ids {
		select {
		case id := <-started:
			working[id] = true
		case <-time.After(time.Second):
			t.Fatalf("only %d of %d drives started fingerprinting", len(working), len(ids))
		}
	}
	if len(working) != len(ids) {
		t.Fatalf("fingerprinting drives = %v", working)
	}
}

// seedSettledVideos adds count videos whose fingerprint is settled, so only
// asset steps are due.
func seedSettledVideos(t *testing.T, cat *catalog.Catalog, driveID string, count int) {
	t.Helper()
	for i := range count {
		id := fmt.Sprintf("%s-%d", driveID, i)
		v := &catalog.Video{ID: id, DriveID: driveID, FileID: id, Title: id, Size: 10,
			SampledSHA256: dedupe.SampledFingerprintPrefix + id, FingerprintStatus: "ready"}
		if err := cat.UpsertVideo(context.Background(), v); err != nil {
			t.Fatal(err)
		}
	}
}

// expectStarts waits for count reads of the source and no more.
func expectStarts(t *testing.T, started <-chan string, count int, reason string) {
	t.Helper()
	for i := range count {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("%s: only %d of %d videos started", reason, i, count)
		}
	}
	select {
	case <-started:
		t.Fatalf("%s: more than %d videos started", reason, count)
	case <-time.After(40 * time.Millisecond):
	}
}

func newBudgetTestApp(t *testing.T, concurrency int) (*App, context.Context) {
	t.Helper()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	a := &App{cat: cat}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	settings := config.DefaultLiveSettings()
	settings.GenerationConcurrency = concurrency
	if err := a.applyLiveConfig(ctx, settings); err != nil {
		t.Fatal(err)
	}
	return a, ctx
}

func TestLocalSourceTakesEverySlotOfTheBudget(t *testing.T) {
	a, ctx := newBudgetTestApp(t, 2)
	seedSettledVideos(t, a.cat, "local", 4)
	started := make(chan string, 20)
	drv := &budgetTestDrive{id: "local", kind: "localstorage", started: started}
	a.registerDriveGeneration(ctx, "local", a.newDriveGenerationRunner(drv, &serverFakeTeaserGenerator{}, &serverFakeTeaserGenerator{}, false))
	expectStarts(t, started, 2, "local source")
}

func TestDriveGenerationParallelismAppliesLive(t *testing.T) {
	a, ctx := newBudgetTestApp(t, 3)
	if err := a.cat.UpsertDrive(ctx, &catalog.Drive{ID: "cloud", Kind: "webdav", Name: "cloud", RootID: "/"}); err != nil {
		t.Fatal(err)
	}
	seedSettledVideos(t, a.cat, "cloud", 4)
	started := make(chan string, 20)
	drv := &budgetTestDrive{id: "cloud", kind: "webdav", started: started}
	a.registerDriveGeneration(ctx, "cloud", a.newDriveGenerationRunner(drv, &serverFakeTeaserGenerator{}, &serverFakeTeaserGenerator{}, false))
	expectStarts(t, started, 1, "cloud default")

	if err := a.cat.SetDriveGenerationParallelism(ctx, "cloud", 2); err != nil {
		t.Fatal(err)
	}
	if err := a.applyDriveGenerationParallelism(ctx, "cloud"); err != nil {
		t.Fatal(err)
	}
	expectStarts(t, started, 1, "override of two")
}

func TestLiveGenerationConcurrencyIncreaseExpandsWaitingLocalSource(t *testing.T) {
	a, ctx := newBudgetTestApp(t, 1)
	seedSettledVideos(t, a.cat, "local", 4)
	started := make(chan string, 20)
	drv := &budgetTestDrive{id: "local", kind: "localstorage", started: started}
	a.registerDriveGeneration(ctx, "local", a.newDriveGenerationRunner(drv, &serverFakeTeaserGenerator{}, &serverFakeTeaserGenerator{}, false))
	expectStarts(t, started, 1, "initial concurrency of one")

	settings := config.DefaultLiveSettings()
	settings.GenerationConcurrency = 3
	if err := a.applyLiveConfig(ctx, settings); err != nil {
		t.Fatal(err)
	}
	expectStarts(t, started, 2, "live concurrency increase to three")
}
