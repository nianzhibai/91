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
