package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/dedupe"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
	"github.com/nianzhibai/91/backend/internal/persistence"
)

func (a *App) buildDuplicateMaintenancePlan(ctx context.Context, localDir string, videos []*catalog.Video, channels dedupe.Channels) (dedupe.Plan, error) {
	ids := make([]string, 0, len(videos))
	for _, video := range videos {
		if video != nil && strings.TrimSpace(video.ID) != "" {
			ids = append(ids, video.ID)
		}
	}
	signatures, err := a.cat.LoadVideoSignatures(ctx, ids)
	if err != nil {
		return dedupe.Plan{}, err
	}
	candidates := make([]dedupe.Candidate, 0, len(videos))
	for _, video := range videos {
		if video == nil || strings.TrimSpace(video.ID) == "" {
			continue
		}
		candidates = append(candidates, dedupe.Candidate{
			ID:              video.ID,
			DurationSeconds: video.DurationSeconds,
			Size:            video.Size,
			ContentHash:     video.ContentHash,
			SampledSHA256:   video.SampledSHA256,
			AssetScore:      videoAssetCompletenessScore(localDir, video),
			CreatedAt:       video.CreatedAt,
			Revision:        catalog.DuplicateRevision(video),
			Signature:       signatures[video.ID],
		})
	}

	distinct, err := a.cat.DismissedDuplicatePairs(ctx)
	if err != nil {
		return dedupe.Plan{}, err
	}
	plan, err := dedupe.Build(ctx, candidates, dedupe.Options{Channels: channels, Distinct: distinct})
	if err != nil {
		return dedupe.Plan{}, err
	}
	for _, match := range plan.Matches {
		if c := match.Signature; c != nil {
			applog.Info(ctx, "signature duplicate matched",
				"component", "dedupe-maintenance",
				"leftId", match.LeftID,
				"rightId", match.RightID,
				"matchedFrames", c.MatchedFrames,
				"frames", c.Frames,
				"matchedWindows", c.MatchedWindows,
				"windows", c.Windows,
				"medianDistance", c.MedianDistance,
				"static", c.Static,
			)
		}
	}
	return plan, nil
}

func (a *App) applyDuplicateMaintenancePlan(ctx context.Context, localDir string, plan dedupe.Plan) error {
	if err := plan.Validate(); err != nil {
		return err
	}
	deletions := make([]catalog.DuplicateVideoDeletion, 0, len(plan.Actions))
	for _, action := range plan.Actions {
		deletions = append(deletions, catalog.DuplicateVideoDeletion{
			VideoID:                   action.VideoID,
			CanonicalVideoID:          action.CanonicalVideoID,
			ExpectedRevision:          action.ExpectedRevision,
			CanonicalExpectedRevision: action.CanonicalExpectedRevision,
			Origin:                    catalog.DuplicateOriginMaintenance,
			Evidence:                  action.Evidence,
		})
	}
	if err := persistence.RLockContext(ctx); err != nil {
		return err
	}
	defer persistence.RUnlock()

	var applyErr error
	for attempt := 0; attempt < 12; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		applyErr = a.cat.ApplyDuplicateVideoDeletions(ctx, deletions)
		if applyErr == nil {
			break
		}
		if !isSQLiteBusyError(applyErr) {
			return applyErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 250 * time.Millisecond):
		}
	}
	if applyErr != nil {
		return fmt.Errorf("apply duplicate plan after retries: %w", applyErr)
	}
	for _, action := range plan.Actions {
		applog.Info(ctx, "duplicate merged",
			"component", "dedupe-maintenance",
			"stage", action.Stage,
			"videoId", action.VideoID,
			"canonicalVideoId", action.CanonicalVideoID,
		)
	}
	return a.cleanupPendingDuplicateAssetsLocked(ctx, localDir)
}

func (a *App) cleanupPendingDuplicateAssets(ctx context.Context, localDir string) error {
	if err := persistence.RLockContext(ctx); err != nil {
		return err
	}
	defer persistence.RUnlock()
	return a.cleanupPendingDuplicateAssetsLocked(ctx, localDir)
}

func (a *App) cleanupPendingDuplicateAssetsLocked(ctx context.Context, localDir string) error {
	jobs, err := a.cat.ListDuplicateAssetCleanupJobs(ctx, 10000)
	if err != nil {
		return err
	}
	var cleanupErrors []error
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(cleanupErrors, err)...)
		}
		if err := a.cleanupRetiredVideoAssets(ctx, localDir, job.VideoID); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove generated assets for %s: %w", job.VideoID, err))
		}
	}
	return errors.Join(cleanupErrors...)
}

func (a *App) cleanupRetiredVideoAssets(ctx context.Context, localDir, id string) error {
	return a.cat.CleanupRetiredVideoAssets(ctx, id, func(preview string) error {
		return mediaasset.RemoveGeneratedVideoAssets(localDir, id, preview)
	})
}

// replaceDuplicateReviews publishes the pairs of the committed plan that a
// person should judge.
func (a *App) replaceDuplicateReviews(ctx context.Context, reviews []dedupe.Review) error {
	if err := persistence.RLockContext(ctx); err != nil {
		return err
	}
	defer persistence.RUnlock()
	return a.cat.ReplaceDuplicateReviews(ctx, reviews)
}

// mergeDuplicateReview merges a confirmed review pair and removes the merged
// video's generated assets, like an automatic merge.
func (a *App) mergeDuplicateReview(ctx context.Context, keepID, mergeID string) error {
	if err := persistence.RLockContext(ctx); err != nil {
		return err
	}
	defer persistence.RUnlock()
	if err := a.cat.MergeDuplicateReview(ctx, keepID, mergeID); err != nil {
		return err
	}
	localDir := ""
	if a.cfg != nil {
		localDir = strings.TrimSpace(a.cfg.Storage.LocalPreviewDir)
	}
	if err := a.cleanupPendingDuplicateAssetsLocked(ctx, localDir); err != nil {
		// The merge is committed; maintenance retries the cleanup jobs.
		applog.Warn(ctx, "asset cleanup after duplicate review merge failed", err,
			"component", "duplicate-review",
			"videoId", mergeID,
			"canonicalVideoId", keepID,
		)
	}
	return nil
}
