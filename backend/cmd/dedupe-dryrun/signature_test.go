package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/dedupe"
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

// makeTeaser cuts faded segments at the primary teaser plan, like the
// preview generator does.
func makeTeaser(t *testing.T, ffmpeg, source, teaser string) {
	t.Helper()
	var args, filters []string
	concat := ""
	for i, window := range preview.TeaserSourceWindows(testDurationSeconds) {
		args = append(args, "-ss", fmt.Sprintf("%.3f", window.Start.Seconds()), "-t", "3", "-i", source)
		filters = append(filters, fmt.Sprintf("[%d:v]setpts=PTS-STARTPTS,fade=t=in:st=0:d=0.2,fade=t=out:st=2.8:d=0.2[v%d]", i, i))
		concat += fmt.Sprintf("[v%d]", i)
	}
	filters = append(filters, concat+fmt.Sprintf("concat=n=%d:v=1:a=0[out]", len(filters)))
	args = append(args, "-filter_complex", strings.Join(filters, ";"), "-map", "[out]",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "28", teaser)
	runFFmpeg(t, ffmpeg, args...)
}

func fractal(t *testing.T, ffmpeg, path string, startX float64) {
	t.Helper()
	source := fmt.Sprintf("mandelbrot=size=240x180:rate=5:maxiter=150:start_x=%g:end_pts=%d", startX, testDurationSeconds)
	runFFmpeg(t, ffmpeg, "-f", "lavfi", "-i", source, "-t", fmt.Sprint(testDurationSeconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "20", path)
}

func TestSignatureChannelFindsReencodedTeaserCopies(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp4")
	fractal(t, ffmpeg, source, -0.743643887037151)
	letterboxed := filepath.Join(dir, "letterboxed.mp4")
	runFFmpeg(t, ffmpeg, "-i", source, "-vf", "scale=240:136,pad=240:180:0:22:black",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "34", letterboxed)
	other := filepath.Join(dir, "other.mp4")
	fractal(t, ffmpeg, other, -1.25)

	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	var candidates []dedupe.Candidate
	for i, input := range []struct{ id, path string }{
		{"original", source}, {"copy", letterboxed}, {"other", other},
	} {
		teaser := filepath.Join(dir, input.id+"-teaser.mp4")
		makeTeaser(t, ffmpeg, input.path, teaser)
		candidates = append(candidates, dedupe.Candidate{
			ID: input.id, DurationSeconds: testDurationSeconds + i%2, Size: int64(100 * (3 - i)),
			CreatedAt: now, Revision: input.id, TeaserPath: teaser,
		})
	}

	attachSignatures(context.Background(), ffmpeg, 2, candidates, pairedCandidates(candidates))
	plan, err := dedupe.Build(context.Background(), candidates, dedupe.Options{Channels: dedupe.ChannelSignature})
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
