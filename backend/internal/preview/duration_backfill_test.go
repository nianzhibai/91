package preview

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/generation"
)

func existingDurationTestVideo(t *testing.T, id string) (*catalog.Catalog, *catalog.Video) {
	t.Helper()
	cat, video := seedPreviewTestVideo(t, id)
	video.ThumbnailURL = "/p/thumb/" + video.ID
	if err := cat.UpsertVideo(context.Background(), video); err != nil {
		t.Fatal(err)
	}
	return cat, video
}

func TestDurationFailureLeavesAssetsForALaterAttempt(t *testing.T) {
	ctx := context.Background()
	cat, video := existingDurationTestVideo(t, "duration-timeout")
	original, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatal(err)
	}
	gen := &fakeThumbGenerator{probeErr: context.DeadlineExceeded}
	worker := NewThumbnailJob(gen, cat, &previewFakeDrive{})
	if got := worker.ProbeDuration(ctx, video); got != generation.StepUnsettled || gen.probeCalls != 1 {
		t.Fatalf("outcome = %v calls = %d, want unsettled after one probe", got, gen.probeCalls)
	}
	worker.Run(ctx, video)
	if gen.probeCalls != 1 {
		t.Fatal("ordinary cover work repeated metadata failure")
	}
	current, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.DurationSeconds != 0 || !current.ThumbnailUpdatedAt.Equal(original.ThumbnailUpdatedAt) {
		t.Fatal("failed probe changed existing assets")
	}
	gen.probeErr, gen.probeDuration = nil, 19
	worker.ProbeDuration(ctx, video)
	current, err = cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.DurationSeconds != 19 || gen.probeCalls != 2 || gen.generateCalls != 0 {
		t.Fatalf("a later attempt did not recover: %+v", current)
	}
}

func TestStoreProbedDurationRequiresSuccessfulPersistence(t *testing.T) {
	cat, video := seedPreviewTestVideo(t, "duration-save-failed")
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	duration, err := storeProbedDuration(context.Background(), cat, video, 42)
	if err == nil || !strings.Contains(err.Error(), "save duration") || duration != 0 {
		t.Fatalf("failed save reported success: duration=%v err=%v", duration, err)
	}
	if video.DurationSeconds != 0 {
		t.Fatal("unsaved duration published in memory")
	}
}

func TestSubsecondBackfillDoesNotRemainUnknown(t *testing.T) {
	cat, video := existingDurationTestVideo(t, "duration-subsecond")
	worker := NewThumbnailJob(&fakeThumbGenerator{probeDuration: 0.7}, cat, &previewFakeDrive{})
	worker.ProbeDuration(context.Background(), video)
	current, err := cat.GetVideo(context.Background(), video.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.DurationSeconds != 1 {
		t.Fatalf("subsecond duration lost: %+v", current)
	}
}

func TestDurationBackfillDoesNotRetryFailedThumbnail(t *testing.T) {
	cat, video := seedPreviewTestVideo(t, "duration-failed-thumbnail")
	if err := cat.UpdateVideoMeta(context.Background(), video.ID, catalog.VideoMetaPatch{ThumbnailStatus: "failed"}); err != nil {
		t.Fatal(err)
	}
	gen := &fakeThumbGenerator{probeDuration: 19}
	worker := NewThumbnailJob(gen, cat, &previewFakeDrive{})
	worker.ProbeDuration(context.Background(), video)
	current, err := cat.GetVideo(context.Background(), video.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.DurationSeconds != 19 || gen.generateCalls != 0 {
		t.Fatalf("metadata work generated assets: %+v", current)
	}
	failed, err := cat.ListVideosByThumbnailStatus(context.Background(), video.DriveID, "failed", 100)
	if err != nil || len(failed) != 1 {
		t.Fatalf("duration success changed thumbnail failure: %v, %v", failed, err)
	}
}

func TestDurationRateLimitCoolsTheDriveWithoutChangingTheCover(t *testing.T) {
	cat, video := existingDurationTestVideo(t, "duration-rate-limit")
	gen := &fakeThumbGenerator{probeErr: &drives.RateLimitError{RetryAfter: time.Hour, Err: errors.New("429")}}
	worker := NewThumbnailJob(gen, cat, &previewFakeDrive{})
	if got := worker.ProbeDuration(context.Background(), video); got != generation.StepPaused || gen.probeCalls != 1 {
		t.Fatalf("outcome = %v calls = %d, want paused behind the cooldown", got, gen.probeCalls)
	}
	ready, err := cat.ListVideosByThumbnailStatus(context.Background(), video.DriveID, "ready", 100)
	if err != nil || len(ready) != 1 {
		t.Fatalf("rate limit changed cover state: %v, %v", ready, err)
	}
}

func TestDurationBackfillProviderErrorsCooldownWithoutThumbnailRetries(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		streamErr  bool
		cause      error
		cooldown   time.Duration
		want       time.Duration
	}{
		{
			name: "PikPak probe unavailable", kind: "pikpak",
			cause: ffmpegCommandError("ffprobe", errors.New("exit status 1"), []byte("Server returned 503 Service Unavailable")),
			want:  drives.DefaultCooldown,
		},
		{
			name: "Wopan probe forbidden", kind: "wopan",
			cause:    ffmpegCommandError("ffprobe", errors.New("exit status 1"), []byte("Server returned 403 Forbidden")),
			cooldown: 10 * time.Minute, want: 10 * time.Minute,
		},
		{
			name: "PikPak stream unavailable", kind: "pikpak", streamErr: true,
			cause:    errors.New("HTTP 503 Service Unavailable"),
			cooldown: 10 * time.Minute, want: 10 * time.Minute,
		},
		{
			name: "Explicit retry after", kind: "pikpak",
			cause:    &drives.RateLimitError{RetryAfter: time.Hour, Err: errors.New("HTTP 429 Too Many Requests")},
			cooldown: 10 * time.Minute, want: time.Hour,
		},
		{
			name: "Invalid media", kind: "pikpak",
			cause: errors.New("invalid data found when processing input"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cat, video := existingDurationTestVideo(t, "duration-provider-error")
			gen := &fakeThumbGenerator{}
			drv := &previewFakeDrive{kind: tc.kind}
			if tc.streamErr {
				drv.streamErr = tc.cause
			} else {
				gen.probeErr = tc.cause
			}
			worker := NewThumbnailJob(gen, cat, drv)
			worker.RateLimitCooldown = tc.cooldown
			before := time.Now()
			worker.ProbeDuration(ctx, video)
			if tc.want > 0 {
				assertCooldownAround(t, cooldownUntil(worker.Cooldown), before, tc.want)
			} else if !cooldownUntil(worker.Cooldown).IsZero() {
				t.Fatal("permanent media error cooled down the provider")
			}
			if gen.generateCalls != 0 {
				t.Fatal("duration failure generated a thumbnail")
			}
			ready, err := cat.ListVideosByThumbnailStatus(ctx, video.DriveID, "ready", 100)
			if err != nil || len(ready) != 1 || ready[0].DurationSeconds != 0 {
				t.Fatalf("duration failure changed cover state or metadata: %v, %v", ready, err)
			}
			failures, err := cat.IncrementThumbnailFailuresForSource(ctx, video)
			if err != nil || failures != 1 {
				t.Fatalf("duration failure consumed thumbnail retries: failures=%d err=%v", failures, err)
			}
		})
	}
}
