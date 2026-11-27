package preview

import (
	"context"
	"errors"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/generation"
)

var errDurationUnavailable = errors.New("source duration is unavailable")

type durationProber interface {
	Probe(context.Context, *drives.StreamLink) (float64, error)
}

// Asset generation and the subsequent backfill share validation and persistence.
func probeSourceDuration(ctx context.Context, cat *catalog.Catalog, video *catalog.Video, gen durationProber, link *drives.StreamLink) (float64, error) {
	duration, err := normalizedStoredDuration(ctx, cat, video)
	if err != nil || duration > 0 {
		return duration, err
	}
	duration, err = gen.Probe(ctx, link)
	if err != nil {
		return 0, err
	}
	return storeProbedDuration(ctx, cat, video, duration)
}

// ProbeDuration stores the duration of a video the other steps left without
// one. It reports StepPaused when the drive is cooling down or the task was
// interrupted, StepUnsettled when the probe failed without cooling down, and
// StepSettled once the duration is stored. It never changes thumbnail state.
func (w *ThumbnailJob) ProbeDuration(ctx context.Context, video *catalog.Video) generation.StepOutcome {
	if w.Catalog == nil || video == nil || video.Hidden {
		return generation.StepSettled
	}
	if w.skipIfRateLimited() {
		return generation.StepPaused
	}
	ctx = applog.WithFields(applog.NewTask(ctx, "media-metadata", generationDriveID(w.Drive)), applog.Fields{VideoID: video.ID, FileID: video.FileID})
	duration, err := normalizedStoredDuration(ctx, w.Catalog, video)
	if err == nil && duration > 0 {
		return generation.StepSettled
	}
	if err == nil {
		var input *generationInput
		input, err = resolveGenerationInput(ctx, w.Drive, video.FileID)
		if err == nil {
			defer input.Close()
			_, err = input.probe(ctx, func(ctx context.Context, link *drives.StreamLink) (float64, error) {
				return probeSourceDuration(ctx, w.Catalog, video, w.Gen, link)
			})
		}
	}
	if err == nil {
		return generation.StepSettled
	}
	if ctx.Err() != nil {
		return generation.StepPaused
	}
	applog.Error(ctx, "Duration backfill failed: "+video.Title, err,
		"component", "media-metadata", "driveId", video.DriveID, "videoId", video.ID, "stage", "duration")
	if w.pauseForProviderError(err, "duration", video.Title) {
		return generation.StepPaused
	}
	return generation.StepUnsettled
}

// A probe needed for cover generation keeps the cover job's provider-error
// handling; it does not introduce a metadata retry policy.
func (w *ThumbnailJob) probeDuration(ctx context.Context, video *catalog.Video, input *generationInput) generation.StepOutcome {
	_, err := input.probe(ctx, func(ctx context.Context, link *drives.StreamLink) (float64, error) {
		return probeSourceDuration(ctx, w.Catalog, video, w.Gen, link)
	})
	if err == nil {
		return generation.StepSettled
	}
	if ctx.Err() != nil {
		return generation.StepPaused
	}
	if w.pauseForRecoverableError(ctx, video, err, "probe") {
		return generation.StepPaused
	}
	applog.Error(ctx, "Thumbnail duration probe failed: "+video.Title, err,
		"component", "thumb", "driveId", generationDriveID(w.Drive), "videoId", video.ID, "stage", "probe")
	return generation.StepUnsettled
}
