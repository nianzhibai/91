package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/mediasig"
	"github.com/nianzhibai/91/backend/internal/persistence"
	"github.com/nianzhibai/91/backend/internal/preview"
)

const signatureBackfillWorkers = 2

// signatureFromTeaser allows tests to inject signatures; production decodes
// the saved teaser with ffmpeg.
var signatureFromTeaser = mediasig.FromTeaser

// backfillVideoSignatures derives missing signatures from saved teasers. New
// teasers are generated with a signature; this covers teasers made before
// signatures existed and those whose sampling failed. It only decodes local
// files, so it never contacts a drive.
func (a *App) backfillVideoSignatures(ctx context.Context, localDir string) error {
	videos, err := a.cat.ListVideosMissingSignature(ctx, int(mediasig.MinDuration/time.Second))
	if err != nil || len(videos) == 0 {
		return err
	}
	ffmpegPath := ""
	if a.cfg != nil {
		ffmpegPath = a.cfg.Preview.FFmpegPath
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	stored, failed := 0, 0
	for range signatureBackfillWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				video := videos[i]
				teaser, ok := localGeneratedPreviewPath(localDir, video)
				if !ok {
					continue
				}
				duration := float64(video.DurationSeconds)
				signature, err := signatureFromTeaser(ctx, ffmpegPath, teaser,
					time.Duration(video.DurationSeconds)*time.Second, preview.TeaserSourceWindows(duration))
				if err == nil {
					err = a.storeVideoSignature(ctx, video, signature)
				}
				mu.Lock()
				if err != nil {
					failed++
					if ctx.Err() == nil {
						log.Printf("[signature-backfill] video=%s: %v", video.ID, err)
					}
				} else {
					stored++
				}
				mu.Unlock()
			}
		}()
	}
	for i := range videos {
		select {
		case jobs <- i:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()
	log.Printf("[signature-backfill] missing=%d stored=%d failed=%d", len(videos), stored, failed)
	return ctx.Err()
}

func (a *App) storeVideoSignature(ctx context.Context, video *catalog.Video, signature *mediasig.Signature) error {
	if err := persistence.RLockContext(ctx); err != nil {
		return err
	}
	defer persistence.RUnlock()
	return a.cat.SetVideoSignatureForSource(ctx, video, signature)
}

// removeRetiredFrameSignatureCache deletes the teaser frame cache that content
// matching kept before signatures were stored in the catalog.
func removeRetiredFrameSignatureCache(previewDir string) {
	if err := os.RemoveAll(filepath.Join(previewDir, "framesigs")); err != nil {
		log.Printf("[startup] remove retired frame signature cache: %v", err)
	}
}
