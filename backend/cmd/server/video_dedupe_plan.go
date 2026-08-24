package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/dedupe"
	"github.com/video-site/backend/internal/mediaasset"
	"github.com/video-site/backend/internal/mediasim"
	"github.com/video-site/backend/internal/persistence"
)

func (a *App) buildDuplicateMaintenancePlan(ctx context.Context, localDir string, videos []*catalog.Video, channels dedupe.Channels) (dedupe.Plan, error) {
	candidates := make([]dedupe.Candidate, 0, len(videos))
	for _, video := range videos {
		if video == nil || strings.TrimSpace(video.ID) == "" {
			continue
		}
		thumbnailPath, _ := localGeneratedThumbnailPath(localDir, video)
		teaserPath, _ := localGeneratedPreviewPath(localDir, video)
		candidates = append(candidates, dedupe.Candidate{
			ID:                video.ID,
			Title:             video.Title,
			DurationSeconds:   video.DurationSeconds,
			Size:              video.Size,
			SampledSHA256:     video.SampledSHA256,
			AssetScore:        videoAssetCompletenessScore(localDir, video),
			CreatedAt:         video.CreatedAt,
			ExpectedUpdatedAt: video.UpdatedAt.UnixMilli(),
			ThumbnailPath:     thumbnailPath,
			TeaserPath:        teaserPath,
		})
	}

	ffmpegPath := ""
	if a != nil && a.cfg != nil {
		ffmpegPath = a.cfg.Preview.FFmpegPath
	}
	loadSignature := func(ctx context.Context, candidate dedupe.Candidate) (*mediasim.FrameSignature, error) {
		cachePath := mediaasset.FrameSignaturePath(localDir, candidate.ID)
		if signature, cached := mediasim.LoadCachedTeaserSignature(cachePath, candidate.TeaserPath); cached {
			return signature, nil
		}
		signature, err := contentSignatureExtractor(ctx, ffmpegPath, candidate.TeaserPath)
		if err != nil {
			return nil, err
		}
		if signature != nil && signature.InformativeFrames() >= mediasim.ContentDuplicateMinComparisons {
			if err := mediasim.StoreCachedTeaserSignature(cachePath, candidate.TeaserPath, signature); err != nil {
				log.Printf("[dedupe-maintenance] content signature cache write id=%s: %v", candidate.ID, err)
			}
		}
		return signature, nil
	}

	plan, err := dedupe.Build(ctx, candidates, dedupe.Options{
		Channels:             channels,
		LoadContentSignature: loadSignature,
	})
	if err != nil {
		return dedupe.Plan{}, err
	}
	for _, issue := range plan.Issues {
		switch issue.Stage {
		case dedupe.StageNear:
			log.Printf("[dedupe-maintenance] thumbnail ssim failed left=%s right=%s: %v", issue.LeftID, issue.RightID, issue.Err)
		case dedupe.StageContent:
			log.Printf("[dedupe-maintenance] content signature failed id=%s: %v", issue.VideoID, issue.Err)
		}
	}
	for _, match := range plan.Matches {
		if match.Stage != dedupe.StageContent {
			continue
		}
		if match.Cross {
			log.Printf("[dedupe-maintenance] content duplicate matched (cross) left=%s right=%s median_best=%.3f comparisons=%d", match.LeftID, match.RightID, match.Score, match.Comparisons)
			continue
		}
		log.Printf("[dedupe-maintenance] content duplicate matched left=%s right=%s median_ssim=%.3f comparisons=%d", match.LeftID, match.RightID, match.Score, match.Comparisons)
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
			VideoID:                    action.VideoID,
			CanonicalVideoID:           action.CanonicalVideoID,
			ExpectedUpdatedAt:          action.ExpectedUpdatedAt,
			CanonicalExpectedUpdatedAt: action.CanonicalExpectedUpdatedAt,
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
		time.Sleep(time.Duration(attempt+1) * 250 * time.Millisecond)
	}
	if applyErr != nil {
		return fmt.Errorf("apply duplicate plan after retries: %w", applyErr)
	}
	for _, action := range plan.Actions {
		log.Printf("[dedupe-maintenance] %s duplicate deleted id=%s canonical=%s", action.Stage, action.VideoID, action.CanonicalVideoID)
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
		if err := mediaasset.RemoveGeneratedVideoAssets(localDir, job.VideoID, job.PreviewLocal); err != nil {
			if markErr := a.cat.FailDuplicateAssetCleanupJob(ctx, job.VideoID, err); markErr != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("record asset cleanup failure for %s: %w", job.VideoID, markErr))
			}
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove duplicate assets for %s: %w", job.VideoID, err))
			continue
		}
		if err := a.cat.CompleteDuplicateAssetCleanupJob(ctx, job.VideoID); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("complete asset cleanup for %s: %w", job.VideoID, err))
		}
	}
	return errors.Join(cleanupErrors...)
}
