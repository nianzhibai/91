package main

import (
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
	"github.com/nianzhibai/91/backend/internal/preview"
)

func TestNormalizeLegacyThumbnailFilesConvertsAndMarksMigration(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	previewDir := filepath.Join(root, "previews")
	thumbDir := filepath.Join(previewDir, "thumbs")
	if err := os.MkdirAll(thumbDir, 0o755); err != nil {
		t.Fatalf("mkdir thumbnails: %v", err)
	}
	legacyPath := filepath.Join(thumbDir, "legacy.jpg")
	legacy, err := os.Create(legacyPath)
	if err != nil {
		t.Fatalf("create legacy thumbnail: %v", err)
	}
	frame := image.NewRGBA(image.Rect(0, 0, 24, 16))
	for y := 0; y < frame.Bounds().Dy(); y++ {
		for x := 0; x < frame.Bounds().Dx(); x++ {
			frame.SetRGBA(x, y, color.RGBA{R: 20, G: 140, B: 220, A: 255})
		}
	}
	if err := png.Encode(legacy, frame); err != nil {
		_ = legacy.Close()
		t.Fatalf("encode legacy PNG: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy thumbnail: %v", err)
	}

	cat, err := catalog.Open(filepath.Join(root, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	app := &App{cat: cat, cfg: &config.Config{Storage: config.Storage{LocalPreviewDir: previewDir}}}
	stats, err := app.normalizeLegacyThumbnailFiles(ctx)
	if err != nil {
		t.Fatalf("normalize legacy thumbnails: %v", err)
	}
	if stats.Scanned != 1 || stats.Normalized != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want scanned=1 normalized=1 failed=0", stats)
	}
	normalized, err := os.Open(legacyPath)
	if err != nil {
		t.Fatalf("open normalized thumbnail: %v", err)
	}
	if _, err := jpeg.Decode(normalized); err != nil {
		_ = normalized.Close()
		t.Fatalf("decode normalized JPEG: %v", err)
	}
	_ = normalized.Close()
	marker, err := cat.GetSetting(ctx, thumbnailJPEGNormalizationSetting, "")
	if err != nil || marker != "1" {
		t.Fatalf("migration marker = %q err=%v, want 1", marker, err)
	}

	if err := os.WriteFile(legacyPath, []byte("marker prevents a second pass"), 0o644); err != nil {
		t.Fatalf("replace normalized thumbnail: %v", err)
	}
	second, err := app.normalizeLegacyThumbnailFiles(ctx)
	if err != nil {
		t.Fatalf("second normalization: %v", err)
	}
	if second.Scanned != 0 || second.Normalized != 0 || second.Failed != 0 {
		t.Fatalf("second stats = %+v, want zero-value marker skip", second)
	}
}

func TestNormalizeLegacyThumbnailFilesDoesNotMarkFailedMigration(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	previewDir := filepath.Join(root, "previews")
	thumbDir := filepath.Join(previewDir, "thumbs")
	if err := os.MkdirAll(thumbDir, 0o755); err != nil {
		t.Fatalf("mkdir thumbnails: %v", err)
	}
	if err := os.WriteFile(filepath.Join(thumbDir, "invalid.jpg"), []byte("invalid"), 0o644); err != nil {
		t.Fatalf("write invalid thumbnail: %v", err)
	}
	cat, err := catalog.Open(filepath.Join(root, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	app := &App{cat: cat, cfg: &config.Config{Storage: config.Storage{LocalPreviewDir: previewDir}}}
	stats, err := app.normalizeLegacyThumbnailFiles(ctx)
	if err == nil {
		t.Fatal("normalization succeeded despite invalid thumbnail")
	}
	if stats.Failed != 1 {
		t.Fatalf("stats = %+v, want one failure", stats)
	}
	marker, markerErr := cat.GetSetting(ctx, thumbnailJPEGNormalizationSetting, "")
	if markerErr != nil || marker != "" {
		t.Fatalf("migration marker = %q err=%v, want unset", marker, markerErr)
	}
}

func TestReconcileMissingLocalThumbnailFilesRepairsOnlyMissingGeneratedAssets(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	previewDir := filepath.Join(root, "previews")
	thumbDir := filepath.Join(previewDir, "thumbs")
	if err := os.MkdirAll(thumbDir, 0o755); err != nil {
		t.Fatalf("mkdir thumbnails: %v", err)
	}
	if err := os.WriteFile(filepath.Join(thumbDir, "present.jpg"), []byte("jpeg"), 0o644); err != nil {
		t.Fatalf("write present thumbnail: %v", err)
	}

	cat, err := catalog.Open(filepath.Join(root, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	now := time.Now()
	for _, video := range []*catalog.Video{
		{
			ID: "present", DriveID: "drive", FileID: "present-file", Title: "Present",
			ThumbnailURL: "/p/thumb/present", PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "missing", DriveID: "drive", FileID: "missing-file", Title: "Missing",
			ThumbnailURL: "/p/thumb/missing", PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "remote", DriveID: "drive", FileID: "remote-file", Title: "Remote",
			ThumbnailURL: "https://example.test/remote.jpg", PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		},
	} {
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatalf("seed %s: %v", video.ID, err)
		}
	}

	app := &App{cat: cat, cfg: &config.Config{Storage: config.Storage{LocalPreviewDir: previewDir}}}
	stats, err := app.reconcileMissingLocalThumbnailFiles(ctx)
	if err != nil {
		t.Fatalf("reconcile local thumbnails: %v", err)
	}
	if stats != (localAssetReconciliationStats{Scanned: 2, Present: 1, Missing: 1, Reset: 1}) {
		t.Fatalf("stats = %+v", stats)
	}

	present, err := cat.GetVideo(ctx, "present")
	if err != nil {
		t.Fatalf("get present video: %v", err)
	}
	if present.ThumbnailURL != "/p/thumb/present" {
		t.Fatalf("present thumbnail = %q", present.ThumbnailURL)
	}
	missing, err := cat.GetVideo(ctx, "missing")
	if err != nil {
		t.Fatalf("get missing video: %v", err)
	}
	if missing.ThumbnailURL != "" || !missing.ThumbnailUpdatedAt.IsZero() {
		t.Fatalf("missing thumbnail = %q updated=%v", missing.ThumbnailURL, missing.ThumbnailUpdatedAt)
	}
	remote, err := cat.GetVideo(ctx, "remote")
	if err != nil {
		t.Fatalf("get remote video: %v", err)
	}
	if remote.ThumbnailURL != "https://example.test/remote.jpg" {
		t.Fatalf("remote thumbnail = %q", remote.ThumbnailURL)
	}
	pending, err := cat.ListVideosByThumbnailStatus(ctx, "drive", "pending", 0)
	if err != nil {
		t.Fatalf("list pending thumbnails: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "missing" {
		t.Fatalf("pending thumbnails = %#v, want missing only", pending)
	}

	second, err := app.reconcileMissingLocalThumbnailFiles(ctx)
	if err != nil {
		t.Fatalf("second reconciliation: %v", err)
	}
	if second != (localAssetReconciliationStats{Scanned: 1, Present: 1}) {
		t.Fatalf("second stats = %+v", second)
	}
}

func TestReconcileMissingLocalPreviewFilesQueuesOnlyInvalidReadyReferences(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	previewDir := filepath.Join(root, "previews")
	if err := os.MkdirAll(previewDir, 0o755); err != nil {
		t.Fatalf("mkdir previews: %v", err)
	}
	presentPath := filepath.Join(previewDir, "present.mp4")
	if err := os.WriteFile(presentPath, []byte("preview"), 0o644); err != nil {
		t.Fatalf("write present preview: %v", err)
	}
	zeroPath := filepath.Join(previewDir, "zero.mp4")
	if err := os.WriteFile(zeroPath, nil, 0o644); err != nil {
		t.Fatalf("write zero preview: %v", err)
	}

	cat, err := catalog.Open(filepath.Join(root, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	now := time.Now()
	for _, video := range []*catalog.Video{
		{
			ID: "present", DriveID: "drive", FileID: "present-file", Title: "Present",
			PreviewLocal: presentPath, PreviewStatus: "ready", PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "missing", DriveID: "drive", FileID: "missing-file", Title: "Missing",
			PreviewLocal: filepath.Join(previewDir, "missing.mp4"), PreviewStatus: "ready", PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "zero", DriveID: "drive", FileID: "zero-file", Title: "Zero",
			PreviewLocal: zeroPath, PreviewStatus: "ready", PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "outside", DriveID: "drive", FileID: "outside-file", Title: "Outside",
			PreviewLocal: filepath.Join(root, "outside.mp4"), PreviewStatus: "ready", PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "already-pending", DriveID: "drive", FileID: "pending-file", Title: "Pending",
			PreviewStatus: "pending", PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		},
	} {
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatalf("seed %s: %v", video.ID, err)
		}
	}

	app := &App{cat: cat, cfg: &config.Config{Storage: config.Storage{LocalPreviewDir: previewDir}}}
	stats, err := app.reconcileMissingLocalPreviewFiles(ctx)
	if err != nil {
		t.Fatalf("reconcile local previews: %v", err)
	}
	if stats != (localAssetReconciliationStats{Scanned: 4, Present: 1, Missing: 3, Reset: 3}) {
		t.Fatalf("stats = %+v", stats)
	}

	present, err := cat.GetVideo(ctx, "present")
	if err != nil {
		t.Fatalf("get present: %v", err)
	}
	if present.PreviewStatus != "ready" || present.PreviewLocal != presentPath {
		t.Fatalf("present preview status=%q local=%q", present.PreviewStatus, present.PreviewLocal)
	}
	for _, videoID := range []string{"missing", "zero", "outside"} {
		video, err := cat.GetVideo(ctx, videoID)
		if err != nil {
			t.Fatalf("get %s: %v", videoID, err)
		}
		if video.PreviewStatus != "pending" || video.PreviewLocal != "" || !video.PreviewUpdatedAt.IsZero() {
			t.Fatalf("%s preview status=%q local=%q updated=%v", videoID, video.PreviewStatus, video.PreviewLocal, video.PreviewUpdatedAt)
		}
	}

	second, err := app.reconcileMissingLocalPreviewFiles(ctx)
	if err != nil {
		t.Fatalf("second reconciliation: %v", err)
	}
	if second != (localAssetReconciliationStats{Scanned: 1, Present: 1}) {
		t.Fatalf("second stats = %+v", second)
	}
}

func TestReconcileLocalGeneratedAssetsAdmitsRepairedWorkBeforeReturning(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	previewDir := filepath.Join(root, "previews")
	cat, err := catalog.Open(filepath.Join(root, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	seedGenerationDrive(t, cat, "drive-id")

	now := time.Now()
	video := &catalog.Video{
		ID:            "missing-assets",
		DriveID:       "drive-id",
		FileID:        "missing-file",
		Title:         "Missing Assets",
		ThumbnailURL:  "/p/thumb/missing-assets",
		PreviewLocal:  filepath.Join(previewDir, "missing-assets.mp4"),
		PreviewStatus: "ready",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	drive := &serverFakeDrive{}
	generator := &serverFakeTeaserGenerator{}
	previewWorker := preview.NewWorker(generator, cat, drive)
	thumbnailWorker := preview.NewThumbWorker(generator, cat, drive)
	app := &App{
		cat:          cat,
		cfg:          &config.Config{Storage: config.Storage{LocalPreviewDir: previewDir}},
		workers:      map[string]*preview.Worker{"drive-id": previewWorker},
		thumbWorkers: map[string]*preview.ThumbWorker{"drive-id": thumbnailWorker},
	}

	gate := app.driveOperationGate("drive-id")
	gate.mu.Lock()
	gate.beginBlockedLocked()
	gate.mu.Unlock()
	gateBlocked := true
	defer func() {
		if !gateBlocked {
			return
		}
		gate.mu.Lock()
		gate.endBlockedLocked()
		gate.mu.Unlock()
	}()

	type reconciliationResult struct {
		resets int
		err    error
	}
	resultCh := make(chan reconciliationResult, 1)
	go func() {
		resets, err := app.reconcileLocalGeneratedAssets(ctx)
		resultCh <- reconciliationResult{resets: resets, err: err}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		stored, getErr := cat.GetVideo(ctx, video.ID)
		if getErr != nil {
			t.Fatalf("get reconciled video: %v", getErr)
		}
		if stored.ThumbnailURL == "" && stored.PreviewStatus == "pending" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("assets were not reset before queue admission: thumbnail=%q preview_status=%q", stored.ThumbnailURL, stored.PreviewStatus)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case result := <-resultCh:
		t.Fatalf("reconciliation returned before blocked queue admission completed: %+v", result)
	default:
	}

	gate.mu.Lock()
	gate.endBlockedLocked()
	gate.mu.Unlock()
	gateBlocked = false

	var result reconciliationResult
	select {
	case result = <-resultCh:
	case <-time.After(2 * time.Second):
		t.Fatal("reconciliation did not return after queue admission unblocked")
	}
	if result.err != nil {
		t.Fatalf("reconcile generated assets: %v", result.err)
	}
	if result.resets != 2 {
		t.Fatalf("reset assets = %d, want thumbnail + preview", result.resets)
	}
	stored, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get reconciled video: %v", err)
	}
	if stored.ThumbnailURL != "" || stored.PreviewStatus != "pending" || stored.PreviewLocal != "" {
		t.Fatalf(
			"reconciled assets thumbnail=%q preview_status=%q preview_local=%q",
			stored.ThumbnailURL,
			stored.PreviewStatus,
			stored.PreviewLocal,
		)
	}
	if got := thumbnailWorker.Status().QueueLength; got != 1 {
		t.Fatalf("thumbnail queue length on return = %d, want admitted 1", got)
	}
	if got := previewWorker.Status().QueueLength; got != 1 {
		t.Fatalf("preview queue length on return = %d, want admitted 1", got)
	}
}

func TestStartupThumbnailNormalizationDoesNotReconcileGeneratedAssets(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	previewDir := filepath.Join(root, "previews")
	if err := os.MkdirAll(filepath.Join(previewDir, "thumbs"), 0o755); err != nil {
		t.Fatalf("mkdir thumbnails: %v", err)
	}
	cat, err := catalog.Open(filepath.Join(root, "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()

	now := time.Now()
	video := &catalog.Video{
		ID:            "missing-assets",
		DriveID:       "drive-id",
		FileID:        "missing-file",
		Title:         "Missing Assets",
		ThumbnailURL:  "/p/thumb/missing-assets",
		PreviewLocal:  filepath.Join(previewDir, "missing-assets.mp4"),
		PreviewStatus: "ready",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	app := &App{cat: cat, cfg: &config.Config{Storage: config.Storage{LocalPreviewDir: previewDir}}}

	app.runStartupThumbnailNormalization(ctx)

	stored, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video after startup normalization: %v", err)
	}
	if stored.ThumbnailURL != video.ThumbnailURL || stored.PreviewStatus != "ready" || stored.PreviewLocal != video.PreviewLocal {
		t.Fatalf(
			"startup normalization changed generated assets thumbnail=%q preview_status=%q preview_local=%q",
			stored.ThumbnailURL,
			stored.PreviewStatus,
			stored.PreviewLocal,
		)
	}
}
