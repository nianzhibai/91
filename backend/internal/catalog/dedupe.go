package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrDuplicatePlanStale = errors.New("catalog: duplicate plan is stale")

// DuplicateVideoDeletion is one finalized action from the read-only dedupe
// planner. CanonicalVideoID must identify a surviving videos row, never another
// deletion in the same plan.
type DuplicateVideoDeletion struct {
	VideoID                    string
	CanonicalVideoID           string
	ExpectedUpdatedAt          int64
	CanonicalExpectedUpdatedAt int64
}

type DuplicateAssetCleanupJob struct {
	VideoID      string
	PreviewLocal string
	Attempts     int
	LastError    string
}

type CrawlerSourceSeen struct {
	Kind          string
	DriveID       string
	SourceID      string
	Status        string
	SampledSHA256 string
	Size          int64
}

type DuplicateVideoReplacement struct {
	NewVideo                  *Video
	ReplacedVideoID           string
	ExpectedReplacedUpdatedAt int64
	CrawlerSource             *CrawlerSourceSeen
}

// ApplyDuplicateVideoDeletions atomically applies a finalized hard-dedupe plan.
// Generated files are intentionally not touched here; durable cleanup jobs are
// committed alongside the tombstones and processed after this method returns.
func (c *Catalog) ApplyDuplicateVideoDeletions(ctx context.Context, deletions []DuplicateVideoDeletion) error {
	if len(deletions) == 0 {
		return nil
	}
	normalized := make([]DuplicateVideoDeletion, 0, len(deletions))
	deletedIDs := make(map[string]struct{}, len(deletions))
	for _, deletion := range deletions {
		deletion.VideoID = strings.TrimSpace(deletion.VideoID)
		deletion.CanonicalVideoID = strings.TrimSpace(deletion.CanonicalVideoID)
		if deletion.VideoID == "" || deletion.CanonicalVideoID == "" {
			return errors.New("catalog: duplicate deletion requires video and canonical IDs")
		}
		if deletion.VideoID == deletion.CanonicalVideoID {
			return fmt.Errorf("catalog: duplicate video %s points at itself", deletion.VideoID)
		}
		if _, exists := deletedIDs[deletion.VideoID]; exists {
			return fmt.Errorf("catalog: duplicate video %s appears more than once", deletion.VideoID)
		}
		deletedIDs[deletion.VideoID] = struct{}{}
		normalized = append(normalized, deletion)
	}
	for _, deletion := range normalized {
		if _, deleted := deletedIDs[deletion.CanonicalVideoID]; deleted {
			return fmt.Errorf("catalog: canonical video %s is also scheduled for deletion", deletion.CanonicalVideoID)
		}
	}

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	videos := make(map[string]*Video, len(normalized))
	canonicalRevisions := make(map[string]int64)
	for _, deletion := range normalized {
		video, err := scanVideo(tx.QueryRowContext(ctx,
			`SELECT `+allVideoCols+` FROM videos WHERE id = ?`, deletion.VideoID))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: duplicate video %s no longer exists", ErrDuplicatePlanStale, deletion.VideoID)
		}
		if err != nil {
			return err
		}
		if deletion.ExpectedUpdatedAt > 0 && video.UpdatedAt.UnixMilli() != deletion.ExpectedUpdatedAt {
			return fmt.Errorf("%w: duplicate video %s changed from revision %d to %d", ErrDuplicatePlanStale, deletion.VideoID, deletion.ExpectedUpdatedAt, video.UpdatedAt.UnixMilli())
		}
		videos[deletion.VideoID] = video
		if expected, exists := canonicalRevisions[deletion.CanonicalVideoID]; exists && expected != deletion.CanonicalExpectedUpdatedAt {
			return fmt.Errorf("catalog: canonical video %s has conflicting expected revisions", deletion.CanonicalVideoID)
		}
		canonicalRevisions[deletion.CanonicalVideoID] = deletion.CanonicalExpectedUpdatedAt
	}
	for canonicalID, expectedUpdatedAt := range canonicalRevisions {
		var updatedAt int64
		if err := tx.QueryRowContext(ctx, `SELECT updated_at FROM videos WHERE id = ?`, canonicalID).Scan(&updatedAt); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: canonical video %s no longer exists", ErrDuplicatePlanStale, canonicalID)
		} else if err != nil {
			return err
		}
		if expectedUpdatedAt > 0 && updatedAt != expectedUpdatedAt {
			return fmt.Errorf("%w: canonical video %s changed from revision %d to %d", ErrDuplicatePlanStale, canonicalID, expectedUpdatedAt, updatedAt)
		}
	}

	// Redirect historical references before inserting this plan's tombstones.
	// Every target is final, so chains cannot be created by this transaction.
	for _, deletion := range normalized {
		if _, err := tx.ExecContext(ctx, `
UPDATE deleted_videos
   SET canonical_video_id = ?
 WHERE reason = ?
   AND canonical_video_id = ?`, deletion.CanonicalVideoID, DeletedVideoReasonDuplicate, deletion.VideoID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE crawler_seen_sources
   SET canonical_video_id = ?
 WHERE canonical_video_id = ?`, deletion.CanonicalVideoID, deletion.VideoID); err != nil {
			return err
		}
	}

	now := time.Now().UnixMilli()
	for _, deletion := range normalized {
		video := videos[deletion.VideoID]
		if err := deleteVideoWithTombstoneTx(ctx, tx, video, DeleteVideoTombstoneOptions{
			Reason:           DeletedVideoReasonDuplicate,
			CanonicalVideoID: deletion.CanonicalVideoID,
		}); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO duplicate_asset_cleanup_jobs (
  video_id, preview_local, attempts, last_error, created_at, updated_at
) VALUES (?, ?, 0, '', ?, ?)
ON CONFLICT(video_id) DO UPDATE SET
  preview_local = excluded.preview_local,
  attempts = 0,
  last_error = '',
  updated_at = excluded.updated_at`, video.ID, strings.TrimSpace(video.PreviewLocal), now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReplaceDuplicateVideo atomically publishes a new canonical row and
// tombstones the smaller row it supersedes. It is used by crawler ingress,
// where deleting the old row before inserting the downloaded replacement
// would otherwise leave the library without either video on failure.
func (c *Catalog) ReplaceDuplicateVideo(ctx context.Context, replacement DuplicateVideoReplacement) error {
	if replacement.NewVideo == nil {
		return errors.New("catalog: duplicate replacement requires a new video")
	}
	newID := strings.TrimSpace(replacement.NewVideo.ID)
	oldID := strings.TrimSpace(replacement.ReplacedVideoID)
	if newID == "" || oldID == "" || newID == oldID {
		return errors.New("catalog: duplicate replacement requires distinct video IDs")
	}
	if len(replacement.NewVideo.Tags) > 0 {
		return errors.New("catalog: duplicate replacement tags must be attached after the row transaction")
	}

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	oldVideo, err := scanVideo(tx.QueryRowContext(ctx,
		`SELECT `+allVideoCols+` FROM videos WHERE id = ?`, oldID))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: replaced video %s no longer exists", ErrDuplicatePlanStale, oldID)
	}
	if err != nil {
		return err
	}
	if replacement.ExpectedReplacedUpdatedAt > 0 && oldVideo.UpdatedAt.UnixMilli() != replacement.ExpectedReplacedUpdatedAt {
		return fmt.Errorf("%w: replaced video %s changed from revision %d to %d", ErrDuplicatePlanStale, oldID, replacement.ExpectedReplacedUpdatedAt, oldVideo.UpdatedAt.UnixMilli())
	}
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM videos WHERE id = ?`, newID).Scan(&existing); err == nil {
		return fmt.Errorf("catalog: replacement video %s already exists", newID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := upsertVideoRow(ctx, tx, replacement.NewVideo); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE deleted_videos
   SET canonical_video_id = ?
 WHERE reason = ?
   AND canonical_video_id = ?`, newID, DeletedVideoReasonDuplicate, oldID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE crawler_seen_sources
   SET canonical_video_id = ?
 WHERE canonical_video_id = ?`, newID, oldID); err != nil {
		return err
	}
	if err := deleteVideoWithTombstoneTx(ctx, tx, oldVideo, DeleteVideoTombstoneOptions{
		Reason:           DeletedVideoReasonDuplicate,
		CanonicalVideoID: newID,
	}); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO duplicate_asset_cleanup_jobs (
  video_id, preview_local, attempts, last_error, created_at, updated_at
) VALUES (?, ?, 0, '', ?, ?)
ON CONFLICT(video_id) DO UPDATE SET
  preview_local = excluded.preview_local,
  attempts = 0,
  last_error = '',
  updated_at = excluded.updated_at`, oldVideo.ID, strings.TrimSpace(oldVideo.PreviewLocal), now, now); err != nil {
		return err
	}
	if source := replacement.CrawlerSource; source != nil {
		if err := markCrawlerSourceSeen(ctx, tx, source.Kind, source.DriveID, source.SourceID, source.Status, newID, source.SampledSHA256, source.Size); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (c *Catalog) ListDuplicateAssetCleanupJobs(ctx context.Context, limit int) ([]DuplicateAssetCleanupJob, error) {
	if limit <= 0 {
		limit = 10000
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT video_id, COALESCE(preview_local, ''), attempts, COALESCE(last_error, '')
  FROM duplicate_asset_cleanup_jobs
 ORDER BY updated_at, video_id
 LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := make([]DuplicateAssetCleanupJob, 0)
	for rows.Next() {
		var job DuplicateAssetCleanupJob
		if err := rows.Scan(&job.VideoID, &job.PreviewLocal, &job.Attempts, &job.LastError); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (c *Catalog) CompleteDuplicateAssetCleanupJob(ctx context.Context, videoID string) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM duplicate_asset_cleanup_jobs WHERE video_id = ?`, strings.TrimSpace(videoID))
	return err
}

func (c *Catalog) FailDuplicateAssetCleanupJob(ctx context.Context, videoID string, cleanupErr error) error {
	message := ""
	if cleanupErr != nil {
		message = cleanupErr.Error()
		if len(message) > 500 {
			message = message[:500]
		}
	}
	_, err := c.db.ExecContext(ctx, `
UPDATE duplicate_asset_cleanup_jobs
   SET attempts = attempts + 1,
       last_error = ?,
       updated_at = ?
 WHERE video_id = ?`, message, time.Now().UnixMilli(), strings.TrimSpace(videoID))
	return err
}
