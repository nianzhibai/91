package preview

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/drives"
)

func capturePreviewLogs(t *testing.T) (*applog.Store, *strings.Builder) {
	t.Helper()
	store, err := applog.Open(applog.Config{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var output strings.Builder
	previous := log.Writer()
	log.SetOutput(applog.Output(&output, store))
	t.Cleanup(func() { log.SetOutput(previous) })
	return store, &output
}

func TestGenerateLogsDegradationAsWarningWithTaskContext(t *testing.T) {
	store, _ := capturePreviewLogs(t)
	gen := newTeaserStubGenerator(t, 2)
	fields := applog.Fields{Component: "preview", TaskID: "degraded-task", DriveID: "drive-1", VideoID: "video-1", FileID: "file-1"}
	ctx := applog.WithFields(context.Background(), fields)
	_, err := gen.Generate(ctx, &drives.StreamLink{URL: filepath.Join(t.TempDir(), "source.mp4")}, 204)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Query(ctx, applog.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Entries) != 1 {
		t.Fatalf("entries = %+v, want one degradation warning", snapshot.Entries)
	}
	entry := snapshot.Entries[0]
	fields.Stage = "segments"
	if entry.Level != applog.LevelWarning || entry.Fields != fields ||
		!strings.Contains(entry.Message, "degraded teaser accepted segments=2/4") ||
		!strings.Contains(entry.Error, "produced empty file") {
		t.Fatalf("degradation event = %+v", entry)
	}
}

func TestPreviewWorkerLogsFinalOutcomeAtItsOwnLevel(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cause   error
		level   applog.Level
		status  string
		message string
	}{
		{name: "ready", level: applog.LevelInfo, status: "ready", message: "Preview ready"},
		{name: "failed", cause: fmt.Errorf("only generated 1/4 teaser segments: %w", context.DeadlineExceeded), level: applog.LevelError, status: "failed", message: "Preview generation failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := capturePreviewLogs(t)
			cat, video := seedPreviewTestVideo(t, "preview-log-"+tc.name)
			video.DurationSeconds = 204
			if err := cat.UpsertVideo(context.Background(), video); err != nil {
				t.Fatal(err)
			}
			worker := NewWorker(&fakeTeaserGenerator{generateErr: tc.cause}, cat, &previewFakeDrive{})
			ctx := context.Background()
			if !worker.Enqueue(video) {
				t.Fatal("enqueue preview failed")
			}
			worker.processQueued(ctx, <-worker.ch)
			snapshot, err := store.Query(ctx, applog.Query{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Entries) != 1 {
				t.Fatalf("entries = %+v, want one final outcome", snapshot.Entries)
			}
			entry := snapshot.Entries[0]
			if entry.Level != tc.level || !strings.Contains(entry.Message, tc.message) ||
				entry.Component != "preview" || entry.DriveID != video.DriveID || entry.VideoID != video.ID || entry.TaskID == "" {
				t.Fatalf("final outcome = %+v", entry)
			}
			if tc.cause != nil && !strings.Contains(entry.Error, "context deadline exceeded") {
				t.Fatalf("failure lost its cause: %+v", entry)
			}
			got, err := cat.GetVideo(ctx, video.ID)
			if err != nil || got.PreviewStatus != tc.status {
				t.Fatalf("preview status = %+v, err=%v, want %s", got, err, tc.status)
			}
		})
	}
}
