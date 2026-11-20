package scriptcrawler

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
	"github.com/nianzhibai/91/backend/internal/mediasim"
)

func TestLoadCandidateTeaserSignatureUsesDiskCache(t *testing.T) {
	localDir := t.TempDir()
	teaserPath := filepath.Join(localDir, "candidate.mp4")
	if err := os.WriteFile(teaserPath, []byte("teaser"), 0o644); err != nil {
		t.Fatalf("write teaser: %v", err)
	}
	frame := bytes.Repeat([]byte{17}, mediasim.FrameSignatureGridSize*mediasim.FrameSignatureGridSize)
	want := &mediasim.FrameSignature{Frames: [][]byte{frame}}
	cachePath := mediaasset.FrameSignaturePath(localDir, "candidate")
	if err := mediasim.StoreCachedTeaserSignature(cachePath, teaserPath, want); err != nil {
		t.Fatalf("store cache: %v", err)
	}

	crawler := NewCrawler(CrawlerConfig{
		LocalPreviewDir: localDir,
		FFmpegPath:      filepath.Join(localDir, "ffmpeg-must-not-run"),
	})
	got, err := crawler.loadCandidateTeaserSignature(context.Background(), "candidate", teaserPath)
	if err != nil {
		t.Fatalf("load candidate signature: %v", err)
	}
	if got == nil || len(got.Frames) != 1 || !bytes.Equal(got.Frames[0], frame) {
		t.Fatalf("loaded signature = %#v, want cached frame", got)
	}
}

func TestNearDuplicateSkipsTeaserComparisonWhilePreviewsDisabled(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cat, err := catalog.Open(filepath.Join(dir, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	teaserPath := filepath.Join(dir, "candidate.mp4")
	sourcePath := filepath.Join(dir, "source.mp4")
	for _, path := range []string{teaserPath, sourcePath} {
		if err := os.WriteFile(path, []byte("video"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The fake ffmpeg records that content comparison started, then fails it.
	marker := filepath.Join(dir, "ffmpeg-ran")
	ffmpegPath := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(ffmpegPath, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID: "candidate", DriveID: "storage", FileID: "candidate-file", Title: "Unrelated title",
		DurationSeconds: 600, ThumbnailURL: "/p/thumb/candidate", PreviewStatus: "ready", PreviewLocal: teaserPath,
		PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	source := &catalog.Video{ID: "source", Title: "New title", DurationSeconds: 600}
	for _, enabled := range []bool{false, true} {
		_ = os.Remove(marker)
		importer := &Importer{cfg: ImporterConfig{
			Driver: New(Config{ID: "crawler", RootDir: dir}), Catalog: cat, FFmpegPath: ffmpegPath,
			LocalPreviewDir: dir, PreviewEnabled: func() bool { return enabled },
		}}
		match, err := importer.findNearDuplicateVideo(ctx, source, "", sourcePath)
		_, statErr := os.Stat(marker)
		if err != nil || match != nil || (statErr == nil) != enabled {
			t.Fatalf("previews=%v: match=%v err=%v ffmpeg ran=%v", enabled, match, err, statErr == nil)
		}
	}
}
