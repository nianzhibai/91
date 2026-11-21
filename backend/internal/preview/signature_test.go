package preview

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/mediasig"
)

func TestGenerateSamplesSignatureFromSegmentWindows(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp4")
	const duration = 90
	if out, err := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-f", "lavfi", "-i", "mandelbrot=size=240x180:rate=10:maxiter=150:end_pts=90", "-t", "90",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "20", source).CombinedOutput(); err != nil {
		t.Fatalf("make source: %v: %s", err, out)
	}

	gen := New(Config{FFmpegPath: ffmpeg, FFprobePath: ffprobe, Width: 160, LocalDir: filepath.Join(dir, "output")})
	media, err := gen.Generate(ctx, &drives.StreamLink{URL: source}, duration, MediaRequest{Teaser: true, Thumbnail: true})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(media.TeaserPath); _ = os.Remove(media.ThumbnailPath) })
	if media.Signature == nil || media.TeaserPath == "" || media.ThumbnailPath == "" {
		t.Fatalf("media = %+v, want teaser, thumbnail and signature from one pass", media)
	}
	if info, err := os.Stat(media.ThumbnailPath); err != nil || info.Size() == 0 {
		t.Fatalf("thumbnail missing: %v", err)
	}
	if err := gen.validateGeneratedTeaser(ctx, media.TeaserPath); err != nil {
		t.Fatalf("teaser invalid: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "output"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("output files = %v, want only the teaser and the thumbnail", entries)
	}

	// Frames come from the decoded source, before the teaser's fades: no
	// window loses its first frame to a fade-in. (The fractal's last window is
	// a flat deep zoom, so a whole window may be uninformative by content.)
	if len(media.Signature.Frames) != 24 || !media.SignatureComplete {
		t.Fatalf("frames = %d complete = %v, want six frames in each of four windows", len(media.Signature.Frames), media.SignatureComplete)
	}
	informative := map[int]int{}
	for _, frame := range media.Signature.Frames {
		if frame.Informative {
			informative[frame.Window]++
		}
	}
	for window, count := range informative {
		if count != 6 {
			t.Fatalf("window %d has %d informative frames, want all six", window, count)
		}
	}
	if len(informative) < 3 {
		t.Fatalf("informative windows = %v", informative)
	}
	windows := TeaserSourceWindows(duration)
	for _, frame := range media.Signature.Frames {
		window := windows[frame.Window]
		if frame.At < window.Start || frame.At >= window.Start+window.Duration {
			t.Fatalf("frame at %v lies outside its window %+v", frame.At, window)
		}
	}
	clips := make([]mediasig.Clip, 0, len(windows))
	for _, window := range windows {
		clips = append(clips, mediasig.Clip{Path: source, Offset: window.Start, Window: window})
	}
	direct, err := mediasig.Extract(ctx, ffmpeg, duration*time.Second, clips)
	if err != nil {
		t.Fatalf("sample source: %v", err)
	}
	if got := mediasig.Compare(media.Signature, direct); got.Verdict != mediasig.VerdictStrong {
		t.Fatalf("teaser signature vs source = %+v, want strong", got)
	}
}

func TestGenerateWithoutTeaserKeepsOnlyThumbnailAndSignature(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp4")
	if out, err := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-f", "lavfi", "-i", "mandelbrot=size=240x180:rate=10:maxiter=150:end_pts=60", "-t", "60",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "20", source).CombinedOutput(); err != nil {
		t.Fatalf("make source: %v: %s", err, out)
	}
	gen := New(Config{FFmpegPath: ffmpeg, FFprobePath: ffprobe, Width: 160, LocalDir: filepath.Join(dir, "output")})
	media, err := gen.Generate(ctx, &drives.StreamLink{URL: source}, 60, MediaRequest{Thumbnail: true})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if media.TeaserPath != "" || media.ThumbnailPath == "" || media.Signature == nil {
		t.Fatalf("media = %+v, want thumbnail and signature only", media)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "output"))
	if len(entries) != 1 {
		t.Fatalf("output files = %v, want only the thumbnail", entries)
	}
}

func TestPlannedSignatureLeavesOutFallbackSegments(t *testing.T) {
	dir := t.TempDir()
	// Raw frames as mediasig.SampleFilter writes them: six per 3-second window.
	writeFrames := func(name string, seed byte) string {
		raw := make([]byte, 6*mediasig.GridSize*mediasig.GridSize)
		for i := range raw {
			raw[i] = byte(i/7) ^ seed
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	segment := func(start float64, signature string) teaserSegmentResult {
		return teaserSegmentResult{teaserSegmentCandidate: teaserSegmentCandidate{start: start}, signature: signature}
	}
	planned := []float64{13.5, 34.2}

	// The second planned segment fell back to 22.5 seconds, which samples no
	// signature frames.
	signature, complete := plannedSignature(90, planned, []teaserSegmentResult{
		segment(13.5, writeFrames("planned.gray", 1)), segment(22.5, ""),
	}, 3)
	if signature == nil || complete || len(signature.Frames) != 6 {
		t.Fatalf("signature = %+v complete = %v, want six frames of one window", signature, complete)
	}
	for _, frame := range signature.Frames {
		if frame.At < 13500*time.Millisecond || frame.At >= 16500*time.Millisecond || frame.Window != 0 {
			t.Fatalf("frame %+v comes from outside the first planned window", frame)
		}
	}

	signature, complete = plannedSignature(90, planned, []teaserSegmentResult{
		segment(13.5, writeFrames("first.gray", 1)), segment(34.2, writeFrames("second.gray", 2)),
	}, 3)
	if signature == nil || !complete || signature.SampledWindows() != 2 {
		t.Fatalf("signature = %+v complete = %v, want both planned windows", signature, complete)
	}
	if signature, _ := plannedSignature(20, planned, nil, 3); signature != nil {
		t.Fatal("short video got a signature")
	}
}
