package fingerprint

import (
	"context"
	"errors"
	"net/http"
	"syscall"
	"time"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/dedupe"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/generation"
	"github.com/nianzhibai/91/backend/internal/streamhttp"
	"github.com/nianzhibai/91/backend/internal/tasklimit"
)

type Config struct {
	// RateLimitCooldown is the pause when a throttling error gives no retry time.
	RateLimitCooldown time.Duration
	// StallTimeout ends a remote read that receives no data for this long.
	StallTimeout time.Duration
	HTTPClient   *http.Client
}

// Job is the fingerprint step of a drive: it samples a video's source and
// records the fingerprint, or a failure that only a retry admits again.
type Job struct {
	Catalog *catalog.Catalog
	Drive   drives.Drive
	Config  Config
	// Cooldown is the drive's provider cooldown, shared with its other steps.
	Cooldown *tasklimit.Cooldown
	// OnCooldown runs after a provider error paused the drive.
	OnCooldown func()

	http *http.Client
}

func NewJob(cat *catalog.Catalog, drv drives.Drive, cfg Config) *Job {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = streamhttp.NewClient(0)
	}
	if cfg.RateLimitCooldown <= 0 {
		cfg.RateLimitCooldown = drives.DefaultCooldown
	}
	return &Job{Catalog: cat, Drive: drv, Config: cfg, Cooldown: &tasklimit.Cooldown{}, http: hc}
}

// Run fingerprints v. It reports StepPaused when v stays pending because the
// drive is cooling down or the task was interrupted, and StepSettled once the
// fingerprint outcome has been written or no work was needed.
func (j *Job) Run(ctx context.Context, v *catalog.Video) generation.StepOutcome {
	if v == nil || v.Hidden || dedupe.IsCurrentFingerprint(v.SampledSHA256) {
		return generation.StepSettled
	}
	ctx = applog.WithFields(applog.NewTask(ctx, "fingerprint", j.Drive.ID()), applog.Fields{VideoID: v.ID, FileID: v.FileID})
	sum, err := Compute(ctx, j.Drive, v, j.Config, j.http)
	if ctx.Err() != nil {
		return generation.StepPaused
	}
	if err != nil {
		if j.pauseForProviderError(ctx, err) {
			return generation.StepPaused
		}
		applog.Error(ctx, "Fingerprint computation failed", err, "stage", "compute")
		_, _ = j.Catalog.UpdateVideoFingerprintForSource(ctx, v, "", "failed", err.Error())
		return generation.StepSettled
	}
	replaced, err := j.Catalog.UpdateVideoFingerprintForSource(ctx, v, sum, "ready", "")
	if err != nil {
		applog.Error(ctx, "Save fingerprint failed", err, "stage", "save")
		return generation.StepUnsettled
	}
	if replaced {
		// The catalog cleared the assets made from the old bytes; the runner
		// regenerates them from the row's state.
		applog.Info(ctx, "Source replaced in place; regenerating its assets")
		return generation.StepSettled
	}
	applog.Info(ctx, "Fingerprint ready")
	return generation.StepSettled
}

// pauseForProviderError pauses the drive's provider work and reports true when
// err says nothing about the file itself: the provider is throttling or briefly
// unavailable, or the read timed out or lost its connection.
func (j *Job) pauseForProviderError(ctx context.Context, err error) bool {
	wait, limited := drives.RateLimitRetryAfter(err)
	if !limited && !drives.ShouldCooldown(j.Drive.Kind(), err) && !transientTransportError(err) {
		return false
	}
	if wait <= 0 {
		wait = j.Config.RateLimitCooldown
	}
	until := j.Cooldown.Pause(time.Now(), wait)
	if j.OnCooldown != nil {
		j.OnCooldown()
	}
	applog.Warn(ctx, "Fingerprint paused; drive cooling down until "+until.Format(time.RFC3339), err, "stage", "compute")
	return true
}

func transientTransportError(err error) bool {
	var timeout interface{ Timeout() bool }
	return (errors.As(err, &timeout) && timeout.Timeout()) || errors.Is(err, syscall.ECONNRESET)
}
