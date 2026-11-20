package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/dedupe"
	"github.com/nianzhibai/91/backend/internal/mediasig"
	"github.com/nianzhibai/91/backend/internal/preview"
)

const testDurationSeconds = 130

func runFFmpeg(t *testing.T, ffmpeg string, args ...string) {
	t.Helper()
	args = append([]string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}, args...)
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func fractal(t *testing.T, ffmpeg, path string, startX float64) {
	t.Helper()
	source := fmt.Sprintf("mandelbrot=size=240x180:rate=5:maxiter=150:start_x=%g:end_pts=%d", startX, testDurationSeconds)
	runFFmpeg(t, ffmpeg, "-f", "lavfi", "-i", source, "-t", fmt.Sprint(testDurationSeconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "20", path)
}

// sampleSource samples a file at the teaser windows, as a media job does.
func sampleSource(t *testing.T, ffmpeg, path string, duration int) *mediasig.Signature {
	t.Helper()
	windows := preview.TeaserSourceWindows(float64(duration))
	clips := make([]mediasig.Clip, 0, len(windows))
	for _, window := range windows {
		clips = append(clips, mediasig.Clip{Path: path, Offset: window.Start, Window: window})
	}
	signature, err := mediasig.Extract(context.Background(), ffmpeg, time.Duration(duration)*time.Second, clips)
	if err != nil {
		t.Fatal(err)
	}
	return signature
}

func TestDryRunPlansFromStoredSignatures(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp4")
	fractal(t, ffmpeg, source, -0.743643887037151)
	letterboxed := filepath.Join(dir, "letterboxed.mp4")
	runFFmpeg(t, ffmpeg, "-i", source, "-vf", "scale=240:136,pad=240:180:0:22:black",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "34", letterboxed)
	other := filepath.Join(dir, "other.mp4")
	fractal(t, ffmpeg, other, -1.25)

	cat, err := catalog.Open(filepath.Join(dir, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	for i, input := range []struct{ id, path string }{
		{"original", source}, {"copy", letterboxed}, {"other", other}, {"unsampled", source},
	} {
		duration := testDurationSeconds + i%2
		video := &catalog.Video{ID: input.id, DriveID: "drive", FileID: input.id, Title: input.id,
			Size: int64(100 * (4 - i)), DurationSeconds: duration}
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatal(err)
		}
		if input.id == "unsampled" {
			continue
		}
		stored, err := cat.GetVideo(ctx, input.id)
		if err != nil {
			t.Fatal(err)
		}
		if err := cat.CompleteMediaForSource(ctx, stored, sampleSource(t, ffmpeg, input.path, duration)); err != nil {
			t.Fatal(err)
		}
	}

	videos, err := cat.ListVideoMaintenanceCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	candidates, _ := signatureCandidates(dir, videos)
	if err := attachSignatures(ctx, cat, candidates); err != nil {
		t.Fatalf("attach signatures: %v", err)
	}
	plan, err := dedupe.Build(ctx, candidates, dedupe.Options{Channels: dedupe.ChannelSignature})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].VideoID != "copy" || plan.Actions[0].CanonicalVideoID != "original" {
		t.Fatalf("actions = %+v reviews = %+v, want the copy merged into the original", plan.Actions, plan.Reviews)
	}
	if len(plan.Reviews) != 0 {
		t.Fatalf("reviews = %+v, want the other video left alone", plan.Reviews)
	}
}
