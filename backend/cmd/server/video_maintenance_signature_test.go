package main

import (
	"context"
	"database/sql"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
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
		if err := cat.CompleteMediaForSource(ctx, stored, seed.signature); err != nil {
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
		{video("video-short", 400_000_000, 5, 3), contentSignature(5, 1, 0)},
		{video("video-short-copy", 300_000_000, 5, 4), contentSignature(5, 1, 0)},
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

func TestCleanupPublishesProbablePairsAndHonoursDismissals(t *testing.T) {
	ctx := context.Background()
	app, cat, localDir := openMaintenanceTestApp(t)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	// b shares only the first two teaser windows with a: probable, not strong.
	partial := contentSignature(600, 1, 0)
	other := contentSignature(600, 2, 0)
	for i := range partial.Frames {
		if partial.Frames[i].Window >= 2 {
			partial.Frames[i] = other.Frames[i]
		}
	}
	seedSignedVideos(t, cat, localDir, []signedVideo{
		{&catalog.Video{ID: "video-a", DriveID: "115", FileID: "a", Title: "a", Size: 200, DurationSeconds: 600, PublishedAt: now, CreatedAt: now}, contentSignature(600, 1, 0)},
		{&catalog.Video{ID: "video-b", DriveID: "115", FileID: "b", Title: "b", Size: 100, DurationSeconds: 600, PublishedAt: now, CreatedAt: now.Add(time.Second)}, partial},
	})

	if err := app.cleanupDuplicateVideoAssets(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	reviews, err := cat.ListDuplicateReviews(ctx, 1, 10)
	if err != nil || reviews.Total != 1 || reviews.Items[0].Left.ID != "video-a" || reviews.Items[0].Right.ID != "video-b" {
		t.Fatalf("reviews = %+v err = %v, want the probable pair", reviews, err)
	}
	if err := cat.DismissDuplicateReview(ctx, "video-a", "video-b"); err != nil {
		t.Fatal(err)
	}
	if err := app.cleanupDuplicateVideoAssets(ctx); err != nil {
		t.Fatalf("cleanup after dismissal: %v", err)
	}
	if reviews, _ := cat.ListDuplicateReviews(ctx, 1, 10); reviews.Total != 0 {
		t.Fatalf("reviews = %+v, want the dismissed pair gone", reviews)
	}
	for _, id := range []string{"video-a", "video-b"} {
		if _, err := cat.GetVideo(ctx, id); err != nil {
			t.Fatalf("%s should survive: %v", id, err)
		}
	}
}

func TestCleanupWaitsForCurrentFingerprintsBeforeMerging(t *testing.T) {
	ctx := context.Background()
	app, cat, _ := openMaintenanceTestApp(t)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	// The old scheme gave a complete file and a truncated download of the
	// same size the same fingerprint; both await a new one.
	for i, id := range []string{"complete", "truncated"} {
		at := now.Add(time.Duration(i) * time.Second)
		if err := cat.UpsertVideo(ctx, &catalog.Video{ID: id, DriveID: "drive", FileID: id, FileName: id + ".mp4", Title: id,
			Size: 1000, SampledSHA256: "legacy", FingerprintStatus: "pending", PublishedAt: at, CreatedAt: at, UpdatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.cleanupDuplicateVideoAssets(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	for _, id := range []string{"complete", "truncated"} {
		if _, err := cat.GetVideo(ctx, id); err != nil {
			t.Fatalf("%s merged on an old fingerprint: %v", id, err)
		}
	}

	// Current fingerprints decide: equal values merge.
	for _, id := range []string{"complete", "truncated"} {
		stored, err := cat.GetVideo(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cat.UpdateVideoFingerprintForSource(ctx, stored, "s2:same", "ready", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.cleanupDuplicateVideoAssets(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if merged, err := cat.GetMergedVideo(ctx, "truncated"); err != nil || merged.CanonicalVideoID != "complete" {
		t.Fatalf("merge = %+v err = %v, want the copy merged once current fingerprints agree", merged, err)
	}
}
