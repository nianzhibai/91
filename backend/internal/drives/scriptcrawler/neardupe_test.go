package scriptcrawler

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/dedupe"
	"github.com/nianzhibai/91/backend/internal/mediasig"
	"github.com/nianzhibai/91/backend/internal/preview"
)

func seedSignedCandidate(t *testing.T, cat *catalog.Catalog, id string, duration int, signature *mediasig.Signature) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	video := &catalog.Video{
		ID: id, DriveID: "storage", FileID: id + "-file", Title: "Unrelated title " + id, Size: 1000,
		DurationSeconds: duration, PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatal(err)
	}
	stored, err := cat.GetVideo(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.CompleteMediaForSource(ctx, stored, signature); err != nil {
		t.Fatal(err)
	}
}

func TestNearDuplicateSamplesOnlyWhenSignaturesExist(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cat, err := catalog.Open(filepath.Join(dir, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	sourcePath := filepath.Join(dir, "source.mp4")
	if err := os.WriteFile(sourcePath, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The fake ffmpeg records that sampling started, then fails it.
	marker := filepath.Join(dir, "ffmpeg-ran")
	ffmpegPath := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(ffmpegPath, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	importer := &Importer{cfg: ImporterConfig{Driver: New(Config{ID: "crawler", RootDir: dir}), Catalog: cat, FFmpegPath: ffmpegPath}}
	source := &catalog.Video{ID: "source", Title: "New title", DurationSeconds: 600}

	// Without stored signatures to compare, the download is not sampled.
	seedSignedCandidate(t, cat, "unsigned", 600, nil)
	if match, err := importer.findNearDuplicateVideo(ctx, source, sourcePath); err != nil || match != nil {
		t.Fatalf("match = %v err = %v", match, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("sampled the download without signatures to compare")
	}
	seedSignedCandidate(t, cat, "candidate", 600, &mediasig.Signature{Duration: 600 * time.Second, Frames: []mediasig.Frame{{Informative: true}}})
	if match, err := importer.findNearDuplicateVideo(ctx, source, sourcePath); err != nil || match != nil {
		t.Fatalf("match = %v err = %v", match, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("did not sample the download against a stored signature")
	}
}

// renderClip renders a fractal clip whose picture keeps changing; startX
// selects the content and filter re-frames it like a re-encoded copy. The zoom
// rate is fixed, so short clips do not zoom into flat black.
func renderClip(t *testing.T, ffmpeg, path string, duration int, startX float64, filter string) {
	t.Helper()
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("mandelbrot=size=240x180:rate=10:maxiter=150:end_pts=60:start_x=%g", startX),
		"-t", fmt.Sprint(duration)}
	if filter != "" {
		args = append(args, "-vf", filter)
	}
	args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-crf", "30", path)
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("render %s: %v: %s", filepath.Base(path), err, out)
	}
}

// sampleClip samples a file at the teaser windows, as a media job does.
func sampleClip(t *testing.T, ffmpeg, path string, duration int) *mediasig.Signature {
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

func requireFFmpeg(t *testing.T) string {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	return ffmpeg
}

const letterbox = "scale=240:136,pad=240:180:0:22:black"

func TestNearDuplicateMatchesReencodedDownloadBySignature(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()
	for _, duration := range []int{60, 12} {
		library := filepath.Join(dir, fmt.Sprintf("library-%d.mp4", duration))
		download := filepath.Join(dir, fmt.Sprintf("download-%d.mp4", duration))
		unrelated := filepath.Join(dir, fmt.Sprintf("unrelated-%d.mp4", duration))
		renderClip(t, ffmpeg, library, duration, -0.743643887037151, "")
		renderClip(t, ffmpeg, download, duration, -0.743643887037151, letterbox)
		renderClip(t, ffmpeg, unrelated, duration, -1.25, "")

		cat, err := catalog.Open(filepath.Join(dir, fmt.Sprintf("catalog-%d.db", duration)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cat.Close() })
		seedSignedCandidate(t, cat, "library", duration+1, sampleClip(t, ffmpeg, library, duration))

		importer := &Importer{cfg: ImporterConfig{Driver: New(Config{ID: "crawler", RootDir: dir}), Catalog: cat, FFmpegPath: ffmpeg}}
		source := &catalog.Video{ID: "source", Title: "Another title", DurationSeconds: duration}
		match, err := importer.findNearDuplicateVideo(ctx, source, download)
		if err != nil || match == nil || match.video.ID != "library" || match.match.Reason != dedupe.ReasonSignature {
			t.Fatalf("%ds: re-encoded download match = %+v err = %v, want the library video", duration, match, err)
		}
		if match, err := importer.findNearDuplicateVideo(ctx, source, unrelated); err != nil || match != nil {
			t.Fatalf("%ds: unrelated download match = %+v err = %v, want none", duration, match, err)
		}
	}
}
