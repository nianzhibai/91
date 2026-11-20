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
	if err := cat.SetVideoSignatureForSource(ctx, stored, signature); err != nil {
		t.Fatal(err)
	}
}

func TestNearDuplicateSkipsSignatureSamplingWhilePreviewsDisabled(t *testing.T) {
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
	seedSignedCandidate(t, cat, "candidate", 600, &mediasig.Signature{Duration: 600 * time.Second, Frames: []mediasig.Frame{{Informative: true}}})
	source := &catalog.Video{ID: "source", Title: "New title", DurationSeconds: 600}
	for _, enabled := range []bool{false, true} {
		_ = os.Remove(marker)
		importer := &Importer{cfg: ImporterConfig{
			Driver: New(Config{ID: "crawler", RootDir: dir}), Catalog: cat, FFmpegPath: ffmpegPath,
			PreviewEnabled: func() bool { return enabled },
		}}
		match, err := importer.findNearDuplicateVideo(ctx, source, "", sourcePath)
		_, statErr := os.Stat(marker)
		if err != nil || match != nil || (statErr == nil) != enabled {
			t.Fatalf("enabled=%v match=%v err=%v sampled=%v", enabled, match, err, statErr == nil)
		}
	}
}

func TestNearDuplicateMatchesReencodedDownloadBySignature(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ctx := context.Background()
	dir := t.TempDir()
	const duration = 60
	render := func(name, filter string, startX float64) string {
		path := filepath.Join(dir, name)
		args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
			"-f", "lavfi", "-i", fmt.Sprintf("mandelbrot=size=240x180:rate=10:maxiter=150:end_pts=60:start_x=%g", startX),
			"-t", "60"}
		if filter != "" {
			args = append(args, "-vf", filter)
		}
		args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-crf", "30", path)
		if out, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("render %s: %v: %s", name, err, out)
		}
		return path
	}
	library := render("library.mp4", "", -0.743643887037151)
	download := render("download.mp4", "scale=240:136,pad=240:180:0:22:black", -0.743643887037151)
	unrelated := render("unrelated.mp4", "", -1.25)

	windows := preview.TeaserSourceWindows(duration)
	clips := make([]mediasig.Clip, 0, len(windows))
	for _, window := range windows {
		clips = append(clips, mediasig.Clip{Path: library, Offset: window.Start, Window: window})
	}
	signature, err := mediasig.Extract(ctx, ffmpeg, duration*time.Second, clips)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Open(filepath.Join(dir, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	seedSignedCandidate(t, cat, "library", duration+1, signature)

	importer := &Importer{cfg: ImporterConfig{Driver: New(Config{ID: "crawler", RootDir: dir}), Catalog: cat, FFmpegPath: ffmpeg}}
	source := &catalog.Video{ID: "source", Title: "Another title", DurationSeconds: duration}
	match, err := importer.findNearDuplicateVideo(ctx, source, "", download)
	if err != nil || match == nil || match.video.ID != "library" || match.match.Reason != dedupe.ReasonSignature {
		t.Fatalf("re-encoded download match = %+v err = %v, want the library video", match, err)
	}
	if match, err := importer.findNearDuplicateVideo(ctx, source, "", unrelated); err != nil || match != nil {
		t.Fatalf("unrelated download match = %+v err = %v, want none", match, err)
	}
}
