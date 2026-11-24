package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/api"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
	"github.com/nianzhibai/91/backend/internal/drives/localupload"
)

func teaserCount(gen *serverFakeTeaserGenerator) int {
	count := 0
	for _, event := range gen.Events() {
		if strings.HasPrefix(event, "preview") {
			count++
		}
	}
	return count
}

func TestGlobalPreviewHotReloadBackfillsEverySource(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("preview: {enabled: false}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewManager(path)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{cat: cat, configManager: manager}
	if err := manager.SetApply(func(s config.LiveSettings) error { return app.applyLiveConfig(ctx, s) }); err != nil {
		t.Fatal(err)
	}
	if app.previewEnabled() {
		t.Fatal("startup ignored disabled global switch")
	}
	generators := map[string]*serverFakeTeaserGenerator{}
	for _, driveID := range []string{"storage", "crawler", localupload.DriveID} {
		if driveID != localupload.DriveID {
			seedGenerationDrive(t, cat, driveID)
		}
		now := time.Now()
		video := &catalog.Video{
			ID: driveID + "-video", DriveID: driveID, FileID: "source", Title: "Pending", DurationSeconds: 30,
			ThumbnailURL: "/p/thumb/" + driveID, PreviewStatus: "pending", CreatedAt: now, UpdatedAt: now, PublishedAt: now,
		}
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatal(err)
		}
		// Only the teaser is outstanding; signature jobs run regardless of
		// the switch.
		markMediaDone(t, cat, video.ID)
		gen := &serverFakeTeaserGenerator{}
		generators[driveID] = gen
		startTestGeneration(t, ctx, app, &serverFakeKindDrive{id: driveID, kind: "fake"}, gen, gen)
		app.wakeDriveGeneration(driveID)
		app.regenPreview(ctx, video.ID)
		if _, err := app.requestDriveGeneration(ctx, driveID, api.DriveGenerationPreviews); err == nil && driveID != localupload.DriveID {
			t.Fatalf("disabled source %s accepted a preview request", driveID)
		}
		waitGenerationIdle(t, app, driveID)
		if teaserCount(gen) != 0 {
			t.Fatalf("disabled source %s generated a teaser", driveID)
		}
	}
	result, err := manager.ReplaceYAML([]byte("preview: {enabled: true}\n"), "")
	if err != nil || result.RestartRequired || !app.previewEnabled() {
		t.Fatalf("hot enable: %+v, %v", result, err)
	}
	for driveID, gen := range generators {
		for teaserCount(gen) == 0 && ctx.Err() == nil {
			time.Sleep(time.Millisecond)
		}
		waitGenerationIdle(t, app, driveID)
		if got, err := cat.GetVideo(ctx, driveID+"-video"); err != nil || got.PreviewStatus != "ready" {
			t.Fatalf("source %s was not backfilled: %+v, %v", driveID, got, err)
		}
	}
}

func TestPreviewSwitchDefersLocalPreviewReconciliationUntilEnabled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	cat, err := catalog.Open(filepath.Join(root, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	seedGenerationDrive(t, cat, "storage")
	now := time.Now()
	if err := cat.UpsertVideo(ctx, &catalog.Video{
		ID: "missing", DriveID: "storage", FileID: "source", Title: "Missing", DurationSeconds: 30,
		ThumbnailURL: "/p/thumb/missing", PreviewLocal: filepath.Join(root, "missing.mp4"), PreviewStatus: "ready",
		PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	markMediaDone(t, cat, "missing")
	app := &App{cat: cat, cfg: &config.Config{Storage: config.Storage{LocalPreviewDir: root}}}
	app.applyPreviewEnabled(ctx, false)
	gen := &serverFakeTeaserGenerator{}
	startTestGeneration(t, ctx, app, &serverFakeKindDrive{id: "storage", kind: "fake"}, gen, gen)

	if _, err := app.reconcileLocalGeneratedAssets(ctx); err != nil {
		t.Fatal(err)
	}
	waitGenerationIdle(t, app, "storage")
	if got, err := cat.GetVideo(ctx, "missing"); err != nil || got.PreviewStatus != "ready" || teaserCount(gen) != 0 {
		t.Fatalf("disabled maintenance touched previews: %+v, %v", got, err)
	}

	app.applyPreviewEnabled(ctx, true)
	for teaserCount(gen) == 0 && ctx.Err() == nil {
		time.Sleep(time.Millisecond)
	}
	waitGenerationIdle(t, app, "storage")
	got, err := cat.GetVideo(ctx, "missing")
	if err != nil || got.PreviewStatus != "ready" || got.PreviewLocal != "missing.mp4" {
		t.Fatalf("enabling did not repair and regenerate the preview: %+v, %v", got, err)
	}
}
