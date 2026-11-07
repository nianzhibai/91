package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
	"github.com/nianzhibai/91/backend/internal/drives/localupload"
	"github.com/nianzhibai/91/backend/internal/preview"
)

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
	app := &App{cat: cat, configManager: manager, workers: make(map[string]*preview.Worker)}
	if err := manager.SetApply(func(s config.LiveSettings) error { return app.applyLiveConfig(ctx, s) }); err != nil {
		t.Fatal(err)
	}
	if app.previewEnabled() {
		t.Fatal("startup ignored disabled global switch")
	}
	for _, driveID := range []string{"storage", "crawler", localupload.DriveID} {
		if driveID != localupload.DriveID {
			seedGenerationDrive(t, cat, driveID)
		}
		now := time.Now()
		video := &catalog.Video{
			ID: driveID + "-video", DriveID: driveID, FileID: "source", Title: "Pending",
			PreviewStatus: "pending", CreatedAt: now, UpdatedAt: now, PublishedAt: now,
		}
		if err := cat.UpsertVideo(ctx, video); err != nil {
			t.Fatal(err)
		}
		worker := preview.NewWorker(&serverFakeTeaserGenerator{}, cat, &serverFakeDrive{})
		worker.Enabled = app.previewEnabled
		app.workers[driveID] = worker
		app.enqueueDriveGeneration(ctx, driveID, worker, nil)
		app.enqueueUploadedVideo(ctx, video)
		app.regenPreview(ctx, video.ID)
		app.regenFailedPreviews(ctx, driveID)
		if worker.Status().QueueLength != 0 {
			t.Fatalf("disabled source %s admitted preview work", driveID)
		}
	}
	result, err := manager.ReplaceYAML([]byte("preview: {enabled: true}\n"), "")
	if err != nil || result.RestartRequired || !app.previewEnabled() {
		t.Fatalf("hot enable: %+v, %v", result, err)
	}
	for driveID, worker := range app.workers {
		for worker.Status().QueueLength != 1 && ctx.Err() == nil {
			time.Sleep(time.Millisecond)
		}
		if worker.Status().QueueLength != 1 {
			t.Fatalf("source %s was not backfilled", driveID)
		}
	}
	if _, err := manager.ReplaceYAML([]byte("preview: {enabled: false}\n"), ""); err != nil {
		t.Fatal(err)
	}
	late, _, _ := app.newDriveGenerationWorkers(&serverFakeDrive{})
	if late.Enabled == nil || late.Enabled() {
		t.Fatal("late attached worker ignored global switch")
	}
}
