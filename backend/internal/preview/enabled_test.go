package preview

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/mediasig"
)

func TestGateReportsStateChanges(t *testing.T) {
	var gate Gate
	if !gate.Enabled() || gate.Set(true) {
		t.Fatal("zero gate must be enabled and setting it enabled must not change it")
	}
	if !gate.Set(false) || gate.Enabled() || gate.Set(false) {
		t.Fatal("disabling must change the gate once")
	}
	var missing *Gate
	if !missing.Enabled() {
		t.Fatal("nil gate must be enabled")
	}
}

func testMediaSignature() *mediasig.Signature {
	return &mediasig.Signature{Duration: 90 * time.Second, Frames: []mediasig.Frame{{At: 13 * time.Second, Hash: 7, Informative: true}}}
}

func TestDisabledPreviewsStillSampleSignatureWithoutTeaser(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "previews-off")
	gen := &fakeTeaserGenerator{signature: testMediaSignature()}
	worker := NewWorker(gen, cat, &previewFakeDrive{})
	worker.Gate = &Gate{}
	worker.Gate.Set(false)

	if retry := worker.process(ctx, video); retry {
		t.Fatal("job asked for a retry")
	}
	if len(gen.requests) != 1 || gen.requests[0].Teaser {
		t.Fatalf("requests = %+v, want one job without a teaser", gen.requests)
	}
	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PreviewStatus != "pending" || got.MediaStatus != "ready" {
		t.Fatalf("preview = %q media = %q, want the teaser left pending and the job done", got.PreviewStatus, got.MediaStatus)
	}
	if signatures, _ := cat.LoadVideoSignatures(ctx, []string{video.ID}); signatures[video.ID] == nil {
		t.Fatal("signature not stored")
	}

	// Enabling previews later generates the teaser that is still pending.
	worker.Gate.Set(true)
	if retry := worker.process(ctx, got); retry {
		t.Fatal("job asked for a retry")
	}
	if len(gen.requests) != 2 || !gen.requests[1].Teaser {
		t.Fatalf("requests = %+v, want a teaser job after enabling", gen.requests)
	}
	if got, _ := cat.GetVideo(ctx, video.ID); got.PreviewStatus != "ready" {
		t.Fatalf("preview = %q, want ready", got.PreviewStatus)
	}
}

func TestSettledVideoIsNotProcessedAgain(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "settled")
	gen := &fakeTeaserGenerator{signature: testMediaSignature()}
	worker := NewWorker(gen, cat, &previewFakeDrive{})
	worker.process(ctx, video)
	settled, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatal(err)
	}
	worker.process(ctx, settled)
	if gen.generateCalls != 1 {
		t.Fatalf("generate calls = %d, want one job for a settled video", gen.generateCalls)
	}
	if err := cat.RequestMediaRenewal(ctx, []string{video.ID}); err != nil {
		t.Fatal(err)
	}
	renewed, _ := cat.GetVideo(ctx, video.ID)
	worker.process(ctx, renewed)
	if gen.generateCalls != 2 || !gen.requests[1].Teaser {
		t.Fatalf("requests = %+v, want a renewal to regenerate the teaser", gen.requests)
	}
}

func TestFailedRenewalKeepsTheExistingTeaser(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "renewal-fails")
	if err := cat.UpdatePreviewForSource(ctx, video, "renewal-fails.mp4", "ready"); err != nil {
		t.Fatal(err)
	}
	renewing, err := cat.GetVideo(ctx, video.ID)
	if err != nil || renewing.MediaStatus != "pending" {
		t.Fatalf("video = %+v err = %v, want a pending renewal", renewing, err)
	}
	worker := NewWorker(&fakeTeaserGenerator{generateErr: errors.New("ffmpeg segment: invalid data")}, cat, &previewFakeDrive{})

	worker.process(ctx, renewing)

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PreviewStatus != "ready" || got.PreviewLocal != "renewal-fails.mp4" || got.MediaStatus != "failed" {
		t.Fatalf("preview = %q/%q media = %q, want the teaser kept and the job failed", got.PreviewStatus, got.PreviewLocal, got.MediaStatus)
	}
}
