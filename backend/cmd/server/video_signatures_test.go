package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
	"github.com/nianzhibai/91/backend/internal/mediasig"
)

func TestBackfillVideoSignaturesDerivesFromSavedTeasers(t *testing.T) {
	ctx := context.Background()
	localDir := t.TempDir()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	for _, id := range []string{"teaser", "broken", "no-file"} {
		video := &catalog.Video{
			ID: id, DriveID: "drive", FileID: id, Title: id, Size: 100, DurationSeconds: 300,
			PreviewLocal: id + ".mp4", PreviewStatus: "ready",
		}
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatal(err)
		}
		if id != "no-file" {
			if err := os.WriteFile(filepath.Join(localDir, id+".mp4"), []byte("teaser"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	var mu sync.Mutex
	var decoded []string
	restore := signatureFromTeaser
	signatureFromTeaser = func(_ context.Context, _, teaserPath string, duration time.Duration, windows []mediasig.Window) (*mediasig.Signature, error) {
		mu.Lock()
		decoded = append(decoded, filepath.Base(teaserPath))
		mu.Unlock()
		if filepath.Base(teaserPath) == "broken.mp4" {
			return nil, errors.New("corrupt teaser")
		}
		if duration != 300*time.Second || len(windows) != 4 {
			t.Errorf("duration = %v windows = %+v, want the teaser plan of a 300s video", duration, windows)
		}
		return &mediasig.Signature{Duration: duration, Frames: []mediasig.Frame{{At: windows[0].Start, Hash: 7, Informative: true}}}, nil
	}
	t.Cleanup(func() { signatureFromTeaser = restore })

	app := &App{cfg: &config.Config{Storage: config.Storage{LocalPreviewDir: localDir}}, cat: cat}
	if err := app.backfillVideoSignatures(ctx, localDir); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	stored, err := cat.LoadVideoSignatures(ctx, []string{"teaser", "broken", "no-file"})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored["teaser"] == nil {
		t.Fatalf("stored = %+v, want only the decodable teaser", stored)
	}
	if len(decoded) != 2 {
		t.Fatalf("decoded = %v, want the two teasers present on disk", decoded)
	}
}
