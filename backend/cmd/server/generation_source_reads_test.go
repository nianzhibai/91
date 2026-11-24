package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/dedupe"
	"github.com/nianzhibai/91/backend/internal/drives"
)

// unreadableDrive refuses every stream link with an error about the file,
// one that starts no provider cooldown.
type unreadableDrive struct {
	drives.Drive
	reads atomic.Int32
}

func (d *unreadableDrive) ID() string   { return "broken" }
func (d *unreadableDrive) Kind() string { return "test" }
func (d *unreadableDrive) StreamURL(context.Context, string) (*drives.StreamLink, error) {
	d.reads.Add(1)
	return nil, errors.New("file is gone")
}

func TestUnreadableSourceIsReadOncePerTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	// Media job, cover and duration are all due.
	v := &catalog.Video{ID: "broken-video", DriveID: "broken", FileID: "file", Title: "broken", Size: 10,
		SampledSHA256: dedupe.SampledFingerprintPrefix + "broken", FingerprintStatus: "ready"}
	if err := cat.UpsertVideo(ctx, v); err != nil {
		t.Fatal(err)
	}
	app := &App{cat: cat}
	drv := &unreadableDrive{}
	gen := &serverFakeTeaserGenerator{}
	startTestGeneration(t, ctx, app, drv, gen, gen)
	waitGenerationIdle(t, app, "broken")

	if reads := drv.reads.Load(); reads != 1 {
		t.Fatalf("stream links = %d, want one for the whole turn", reads)
	}
	_, steps, err := cat.GenerationWork(ctx, v.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !steps.Thumbnail || !steps.Duration {
		t.Fatalf("steps = %+v, want the cover and duration left for a later turn", steps)
	}
	if due, err := cat.ListDueAssets(ctx, "broken", true, time.Now(), 10); err != nil || len(due) != 0 {
		t.Fatalf("due = %v, err = %v; want the video deferred", due, err)
	}
}

func TestEnablingPreviewsTakesDeferredTeasersAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	seedGenerationDrive(t, cat, "drive")
	now := time.Now()
	v := &catalog.Video{ID: "clip", DriveID: "drive", FileID: "clip", Title: "clip", DurationSeconds: 30,
		ThumbnailURL: "/p/thumb/clip", PreviewStatus: "pending", CreatedAt: now, UpdatedAt: now, PublishedAt: now}
	if err := cat.UpsertVideo(ctx, v); err != nil {
		t.Fatal(err)
	}
	markMediaDone(t, cat, v.ID)
	// Deferred around the switch: its teaser was skipped while previews were off.
	if err := cat.DeferGeneration(ctx, v.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	app := &App{cat: cat}
	app.previewGate.Set(false)
	gen := &serverFakeTeaserGenerator{}
	startTestGeneration(t, ctx, app, &serverFakeKindDrive{id: "drive", kind: "fake"}, gen, gen)
	waitGenerationIdle(t, app, "drive")

	app.applyPreviewEnabled(ctx, true)
	deadline := time.Now().Add(3 * time.Second)
	for teaserCount(gen) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if teaserCount(gen) != 1 {
		t.Fatalf("teasers = %d, want the deferred teaser right after enabling", teaserCount(gen))
	}
}
