package scriptcrawler

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

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
