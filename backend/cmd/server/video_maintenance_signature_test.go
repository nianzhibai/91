package main

import (
	"context"
	"database/sql"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
	"github.com/nianzhibai/91/backend/internal/mediasig"
	"github.com/nianzhibai/91/backend/internal/preview"
)

// contentSignature samples the teaser windows of a video whose pictures are
// determined by seed; noise flips a few bits per frame like a re-encode.
func contentSignature(durationSeconds int, seed int64, noise int) *mediasig.Signature {
	signature := &mediasig.Signature{Duration: time.Duration(durationSeconds) * time.Second}
	for window, span := range preview.TeaserSourceWindows(float64(durationSeconds)) {
		for i := range 6 {
			at := span.Start + time.Duration(i)*mediasig.SampleInterval
			second := int64(at / time.Second)
			hash := rand.New(rand.NewSource(seed*1_000_003 + second)).Uint64()
			rng := rand.New(rand.NewSource(seed + int64(at)))
			for _, bit := range rng.Perm(64)[:noise] {
				hash ^= 1 << uint(bit)
			}
			signature.Frames = append(signature.Frames, mediasig.Frame{At: at, Window: window, Hash: hash, Informative: true})
		}
	}
	return signature
}

type signedVideo struct {
	video     *catalog.Video
	signature *mediasig.Signature
}

func seedSignedVideos(t *testing.T, cat *catalog.Catalog, localDir string, seeds []signedVideo) {
	t.Helper()
	ctx := context.Background()
	for _, seed := range seeds {
		video := seed.video
		video.PreviewLocal = filepath.Join(localDir, video.ID+".mp4")
		video.PreviewStatus = "ready"
		if err := os.WriteFile(video.PreviewLocal, []byte("teaser"), 0o644); err != nil {
			t.Fatalf("write teaser: %v", err)
		}
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatalf("seed %s: %v", video.ID, err)
		}
		if seed.signature == nil {
			continue
		}
		stored, err := cat.GetVideo(ctx, video.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := cat.SetVideoSignatureForSource(ctx, stored, seed.signature); err != nil {
			t.Fatalf("signature %s: %v", video.ID, err)
		}
	}
}

func openMaintenanceTestApp(t *testing.T) (*App, *catalog.Catalog, string) {
	t.Helper()
	localDir := t.TempDir()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	return &App{cfg: &config.Config{Storage: config.Storage{LocalPreviewDir: localDir}}, cat: cat}, cat, localDir
}

func TestCleanupMergesSignatureDuplicatesWhilePreviewsDisabled(t *testing.T) {
	ctx := context.Background()
	app, cat, localDir := openMaintenanceTestApp(t)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	video := func(id string, size int64, duration, order int) *catalog.Video {
		at := now.Add(time.Duration(order) * time.Second)
		return &catalog.Video{ID: id, DriveID: "115", FileID: "file-" + id, Title: "标题 " + id, Size: size,
			DurationSeconds: duration, PublishedAt: at, CreatedAt: at, UpdatedAt: at}
	}
	seedSignedVideos(t, cat, localDir, []signedVideo{
		{video("video-original", 977_000_000, 1129, 0), contentSignature(1129, 1, 0)},
		{video("video-compressed", 129_000_000, 1130, 1), contentSignature(1130, 1, 4)},
		{video("video-other", 500_000_000, 1129, 2), contentSignature(1129, 2, 0)},
		// Same pictures, but too short for a signature verdict.
		{video("video-short", 400_000_000, 20, 3), contentSignature(20, 1, 0)},
		{video("video-short-copy", 300_000_000, 20, 4), contentSignature(20, 1, 0)},
	})
	app.previewGate.Set(false)

	if err := app.cleanupDuplicateVideoAssets(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := cat.GetVideo(ctx, "video-compressed"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("compressed copy lookup = %v, want merged", err)
	}
	if merged, err := cat.GetMergedVideo(ctx, "video-compressed"); err != nil || merged.CanonicalVideoID != "video-original" {
		t.Fatalf("merge = %+v err = %v, want the larger original kept", merged, err)
	}
	for _, id := range []string{"video-original", "video-other", "video-short", "video-short-copy"} {
		if _, err := cat.GetVideo(ctx, id); err != nil {
			t.Fatalf("%s should survive: %v", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(localDir, "video-compressed.mp4")); !os.IsNotExist(err) {
		t.Fatalf("merged teaser still exists, err = %v", err)
	}
}

func TestCleanupDuplicateVideoAssetsResolvesCanonicalAcrossChannels(t *testing.T) {
	ctx := context.Background()
	app, cat, localDir := openMaintenanceTestApp(t)
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	videos := []*catalog.Video{
		{ID: "exact-loser", DriveID: "drive-a", FileID: "a", Title: "A", Size: 100, DurationSeconds: 300, PublishedAt: now, CreatedAt: now},
		{ID: "exact-winner", DriveID: "drive-b", FileID: "b", Title: "B", Size: 100, DurationSeconds: 300, PublishedAt: now, CreatedAt: now.Add(time.Second)},
		{ID: "final-signature-winner", DriveID: "drive-c", FileID: "c", Title: "C", Size: 300, DurationSeconds: 300, PublishedAt: now, CreatedAt: now.Add(2 * time.Second)},
	}
	seeds := make([]signedVideo, 0, len(videos))
	for _, video := range videos {
		seeds = append(seeds, signedVideo{video, contentSignature(300, 91, 0)})
	}
	seedSignedVideos(t, cat, localDir, seeds)
	for i, video := range videos {
		stored, err := cat.GetVideo(ctx, video.ID)
		if err != nil {
			t.Fatal(err)
		}
		fingerprint := "different"
		if i < 2 {
			fingerprint = "same-exact-fingerprint"
		}
		if _, err := cat.UpdateVideoFingerprintForSource(ctx, stored, fingerprint, "ready", ""); err != nil {
			t.Fatalf("fingerprint %s: %v", video.ID, err)
		}
	}

	if err := app.cleanupDuplicateVideoAssets(ctx); err != nil {
		t.Fatalf("cleanup duplicate assets: %v", err)
	}
	if _, err := cat.GetVideo(ctx, "final-signature-winner"); err != nil {
		t.Fatalf("final canonical missing: %v", err)
	}
	for _, id := range []string{"exact-loser", "exact-winner"} {
		if _, err := cat.GetVideo(ctx, id); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("deleted video %s lookup = %v", id, err)
		}
		if _, err := os.Stat(filepath.Join(localDir, id+".mp4")); !os.IsNotExist(err) {
			t.Fatalf("deleted video %s teaser remains: %v", id, err)
		}
	}
	list, err := cat.ListMergedVideos(ctx, catalog.MergedVideoFilter{CanonicalVideoID: "final-signature-winner"})
	if err != nil || list.Total != 2 {
		t.Fatalf("merged list = %+v err = %v", list, err)
	}
	jobs, err := cat.ListDuplicateAssetCleanupJobs(ctx, 10)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("asset cleanup jobs = %#v, err = %v", jobs, err)
	}
}

func TestCleanupBackfillsSignaturesOnlyWhilePreviewsEnabled(t *testing.T) {
	ctx := context.Background()
	app, cat, localDir := openMaintenanceTestApp(t)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	seedSignedVideos(t, cat, localDir, []signedVideo{
		{video: &catalog.Video{ID: "video-a", DriveID: "115", FileID: "a", Title: "unrelated a", Size: 200, DurationSeconds: 600, PublishedAt: now, CreatedAt: now}},
		{video: &catalog.Video{ID: "video-b", DriveID: "115", FileID: "b", Title: "unrelated b", Size: 100, DurationSeconds: 600, PublishedAt: now, CreatedAt: now.Add(time.Second)}},
	})
	var calls atomic.Int32
	restore := signatureFromTeaser
	signatureFromTeaser = func(context.Context, string, string, time.Duration, []mediasig.Window) (*mediasig.Signature, error) {
		calls.Add(1)
		return contentSignature(600, 5, 0), nil
	}
	t.Cleanup(func() { signatureFromTeaser = restore })

	app.previewGate.Set(false)
	if err := app.cleanupDuplicateVideoAssets(ctx); err != nil {
		t.Fatalf("disabled maintenance: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("disabled previews decoded %d teasers", calls.Load())
	}
	app.previewGate.Set(true)
	if err := app.cleanupDuplicateVideoAssets(ctx); err != nil {
		t.Fatalf("enabled maintenance: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("decoded %d teasers, want both missing signatures", calls.Load())
	}
	if merged, err := cat.GetMergedVideo(ctx, "video-b"); err != nil || merged.CanonicalVideoID != "video-a" {
		t.Fatalf("merge = %+v err = %v, want backfilled signatures matched", merged, err)
	}
}
