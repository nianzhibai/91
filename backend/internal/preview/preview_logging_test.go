package preview

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/drives"
)

func capturePreviewLogs(t *testing.T) (*applog.Store, *strings.Builder) {
	t.Helper()
	var output strings.Builder
	rt, err := applog.OpenRuntime(applog.RuntimeConfig{
		Directory:     t.TempDir(),
		Level:         applog.LevelInfo,
		ConsoleWriter: &output,
		FileEnabled:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	prev := slog.Default()
	slog.SetDefault(rt.Logger())
	t.Cleanup(func() { slog.SetDefault(prev) })
	return rt.Store(), &output
}

func recordFields(r applog.Record) applog.Fields {
	return applog.Fields{
		Component: r.Component,
		RequestID: r.RequestID,
		TaskID:    r.TaskID,
		DriveID:   r.DriveID,
		VideoID:   r.VideoID,
		FileID:    r.FileID,
		Stage:     r.Stage,
	}
}

func TestGenerateLogsDegradationAsWarningWithTaskContext(t *testing.T) {
	store, _ := capturePreviewLogs(t)
	gen := newTeaserStubGenerator(t, 2)
	fields := applog.Fields{Component: "preview", TaskID: "degraded-task", DriveID: "drive-1", VideoID: "video-1", FileID: "file-1"}
	ctx := applog.WithFields(context.Background(), fields)
	_, err := gen.generatePath(ctx, &drives.StreamLink{URL: filepath.Join(t.TempDir(), "source.mp4")}, 204)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Query(ctx, applog.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	fields.Stage = "segments"
	var degradations []applog.Record
	for _, entry := range snapshot.Entries {
		if entry.Stage == fields.Stage {
			degradations = append(degradations, entry)
		}
	}
	if len(degradations) != 1 {
		t.Fatalf("entries = %+v, want one degradation warning", snapshot.Entries)
	}
	entry := degradations[0]
	if entry.Level != applog.LevelWarn || recordFields(entry) != fields ||
		!strings.Contains(entry.Message, "degraded teaser accepted segments=2/4") ||
		!strings.Contains(entry.Error, "produced empty file") {
		t.Fatalf("degradation event = %+v", entry)
	}
}

func TestMediaJobLogsFinalOutcomeAtItsOwnLevel(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cause   error
		level   applog.Level
		status  string
		message string
	}{
		{name: "ready", level: applog.LevelInfo, status: "ready", message: "Media ready"},
		{name: "failed", cause: fmt.Errorf("only generated 1/4 teaser segments: %w", context.DeadlineExceeded), level: applog.LevelError, status: "failed", message: "Media generation failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := capturePreviewLogs(t)
			cat, video := seedPreviewTestVideo(t, "preview-log-"+tc.name)
			video.DurationSeconds = 204
			if err := cat.UpsertVideo(context.Background(), video); err != nil {
				t.Fatal(err)
			}
			worker := NewMediaJob(&fakeTeaserGenerator{generateErr: tc.cause}, cat, &previewFakeDrive{})
			ctx := context.Background()
			worker.Run(ctx, video)
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
