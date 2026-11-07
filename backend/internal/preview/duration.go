package preview

import (
	"context"
	"errors"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
)

var errDurationUnavailable = errors.New("source duration is unavailable")

type durationProber interface {
	Probe(context.Context, *drives.StreamLink) (float64, error)
}

type durationTask struct {
	ctx   context.Context
	video *catalog.Video
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

// EnqueueDurationBlocking is called after the current drive's resource queues
// have drained. This queue never admits retries on its own.
func (w *ThumbWorker) EnqueueDurationBlocking(ctx context.Context, video *catalog.Video) bool {
	defer w.notifyStatus(false)
	if video == nil || ctx.Err() != nil {
		return false
	}
	if !w.durationQueue.reserve(video) {
		return true
	}
	select {
	case w.durationCh <- durationTask{ctx: ctx, video: video}:
		return true
	case <-ctx.Done():
		w.durationQueue.release(video)
		return false
	}
}

// processDuration performs one best-effort attempt. Missing duration remains the
// selection criterion for the next round; thumbnail state is never modified.
func (w *ThumbWorker) processDuration(ctx context.Context, video *catalog.Video) {
	if w.Catalog == nil || video == nil {
		return
	}
	current, err := w.Catalog.GetVideo(ctx, video.ID)
	if err != nil || current.Hidden {
		return
	}
	duration, err := normalizedStoredDuration(ctx, w.Catalog, current)
	if err == nil && duration > 0 {
		return
	}
	if err == nil {
		var input *generationInput
		input, err = resolveGenerationInput(ctx, w.Drive, current.FileID)
		if err == nil {
			defer input.Close()
			_, err = input.probe(ctx, func(ctx context.Context, link *drives.StreamLink) (float64, error) {
				return probeSourceDuration(ctx, w.Catalog, current, w.Gen, link)
			})
		}
	}
	if err != nil && ctx.Err() == nil {
		applog.Error(ctx, "Duration backfill failed: "+current.Title, err,
			applog.Fields{Component: "media-metadata", DriveID: current.DriveID, VideoID: current.ID, Stage: "duration"})
		w.pauseForProviderError(err, "duration", current.Title)
	}
}

// A probe needed for cover generation keeps the cover worker's existing
// provider-error handling; it does not introduce a metadata retry policy.
func (w *ThumbWorker) probeDuration(ctx context.Context, video *catalog.Video, input *generationInput) bool {
	_, err := input.probe(ctx, func(ctx context.Context, link *drives.StreamLink) (float64, error) {
		return probeSourceDuration(ctx, w.Catalog, video, w.Gen, link)
	})
	if err == nil {
		return false
	}
	if w.pauseForRecoverableError(ctx, video, err, "probe") {
		return true
	}
	applog.Error(ctx, "Thumbnail duration probe failed: "+video.Title, err,
		applog.Fields{Component: "thumb", DriveID: generationDriveID(w.Drive), VideoID: video.ID, Stage: "probe"})
	return false
}
