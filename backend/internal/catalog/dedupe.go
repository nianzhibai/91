package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/video-site/backend/internal/dedupe"
	"github.com/video-site/backend/internal/driveevents"
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
	Origin                     string
	Evidence                   dedupe.Evidence
}

type DuplicateAssetCleanupJob struct {
	VideoID      string
	PreviewLocal string
	Attempts     int
	LastError    string
}

type CrawlerSourceSeen struct {
	DiscoveryKey  string
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
	Evidence                  dedupe.Evidence
}

// ApplyDuplicateVideoDeletions atomically applies a finalized hard-dedupe plan.
// User state and durable references are merged into each canonical row before
// duplicate rows are retired. Generated files are intentionally not touched
// here; cleanup jobs are committed with the tombstones and processed later.
func (c *Catalog) ApplyDuplicateVideoDeletions(ctx context.Context, deletions []DuplicateVideoDeletion) (resultErr error) {
	defer c.notifyDriveWrite(&resultErr, "", driveevents.MediaChanged)
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

	// Snapshot all matching and intermediate winner rows before retiring any of
	// them. A later failure rolls these records back with the entire merge.
	for _, deletion := range normalized {
		origin := deletion.Origin
		if origin == "" {
			origin = DuplicateOriginMaintenance
		}
		if err := recordDuplicateMerge(ctx, tx, origin, DuplicateOutcomeMerged, videos[deletion.VideoID], deletion.CanonicalVideoID, deletion.Evidence); err != nil {
			return err
		}
	}
	for _, deletion := range normalized {
		if err := mergeAndRetireDuplicateVideoTx(ctx, tx, videos[deletion.VideoID], deletion.CanonicalVideoID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReplaceDuplicateVideo atomically publishes a new canonical row and
// tombstones the smaller row it supersedes. It is used by crawler ingress,
// where deleting the old row before inserting the downloaded replacement
// would otherwise leave the library without either video on failure.
func (c *Catalog) ReplaceDuplicateVideo(ctx context.Context, replacement DuplicateVideoReplacement) (resultErr error) {
	defer c.notifyDriveWrite(&resultErr, "", driveevents.MediaChanged)
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
	if err := recordDuplicateMerge(ctx, tx, DuplicateOriginCrawler, DuplicateOutcomeReplaced, oldVideo, newID, replacement.Evidence); err != nil {
		return err
	}
	if err := mergeAndRetireDuplicateVideoTx(ctx, tx, oldVideo, newID); err != nil {
		return err
	}
	if source := replacement.CrawlerSource; source != nil {
		if err := markCrawlerSourceSeen(ctx, tx, source.Kind, source.DriveID, source.SourceID, source.Status, newID, source.SampledSHA256, source.Size); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ResolveVideoID maps a public video ID through duplicate tombstones to the
// current live row. Catalog.GetVideo deliberately keeps its strict row lookup
// semantics; callers opt into alias resolution only at public read boundaries.
func (c *Catalog) ResolveVideoID(ctx context.Context, id string) (string, error) {
	current := strings.TrimSpace(id)
	if current == "" {
		return "", sql.ErrNoRows
	}
	seen := make(map[string]struct{})
	for depth := 0; depth < 64; depth++ {
		if _, duplicate := seen[current]; duplicate {
			return "", sql.ErrNoRows
		}
		seen[current] = struct{}{}

		var liveID string
		err := c.db.QueryRowContext(ctx, `SELECT id FROM videos WHERE id = ?`, current).Scan(&liveID)
		if err == nil {
			return liveID, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}

		var next string
		err = c.db.QueryRowContext(ctx, `
SELECT canonical_video_id
  FROM deleted_videos
 WHERE id = ?
   AND reason = ?`, current, DeletedVideoReasonDuplicate).Scan(&next)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", sql.ErrNoRows
			}
			return "", err
		}
		current = strings.TrimSpace(next)
		if current == "" {
			return "", sql.ErrNoRows
		}
	}
	return "", sql.ErrNoRows
}

func mergeAndRetireDuplicateVideoTx(ctx context.Context, tx *sql.Tx, duplicate *Video, canonicalID string) error {
	if duplicate == nil {
		return sql.ErrNoRows
	}
	canonicalID = strings.TrimSpace(canonicalID)
	if canonicalID == "" || canonicalID == duplicate.ID {
		return errors.New("catalog: duplicate merge requires distinct video IDs")
	}
	if err := mergeDuplicateVideoStateTx(ctx, tx, duplicate, canonicalID); err != nil {
		return err
	}
	if err := redirectDuplicateVideoReferencesTx(ctx, tx, duplicate.ID, canonicalID); err != nil {
		return err
	}
	if err := deleteVideoWithTombstoneTx(ctx, tx, duplicate, DeleteVideoTombstoneOptions{
		Reason:           DeletedVideoReasonDuplicate,
		CanonicalVideoID: canonicalID,
	}); err != nil {
		return err
	}
	return enqueueDuplicateAssetCleanupTx(ctx, tx, duplicate)
}

func mergeDuplicateVideoStateTx(ctx context.Context, tx *sql.Tx, duplicate *Video, canonicalID string) error {
	if err := mergeDuplicateVideoTagsTx(ctx, tx, duplicate.ID, canonicalID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO video_reaction_visits (video_id, visit_id, reaction, created_at, updated_at)
SELECT ?, visit_id, reaction, created_at, updated_at
  FROM video_reaction_visits
 WHERE video_id = ?
ON CONFLICT(video_id, visit_id) DO UPDATE SET
  reaction = CASE
    WHEN excluded.updated_at > video_reaction_visits.updated_at THEN excluded.reaction
    ELSE video_reaction_visits.reaction
  END,
  created_at = MIN(video_reaction_visits.created_at, excluded.created_at),
  updated_at = MAX(video_reaction_visits.updated_at, excluded.updated_at)`, canonicalID, duplicate.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE video_shares SET video_id = ? WHERE video_id = ?`, canonicalID, duplicate.ID); err != nil {
		return err
	}
	// 收藏按 (user_id, video_id) 唯一：同一账号可能同时收藏了副本和保留项，
	// 所以先补写再删源行，不能直接 UPDATE 主键。
	if _, err := tx.ExecContext(ctx, `
INSERT INTO video_favorites (user_id, video_id, created_at)
SELECT user_id, ?, created_at
  FROM video_favorites
 WHERE video_id = ?
ON CONFLICT(user_id, video_id) DO NOTHING`, canonicalID, duplicate.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM video_favorites WHERE video_id = ?`, duplicate.ID); err != nil {
		return err
	}

	now := time.Now().UnixMilli()
	res, err := tx.ExecContext(ctx, `
UPDATE videos
   SET views = COALESCE(views, 0) + ?,
       last_viewed_at = MAX(COALESCE(last_viewed_at, 0), ?),
       favorites = COALESCE(favorites, 0) + ?,
       comments = COALESCE(comments, 0) + ?,
       likes = COALESCE(likes, 0) + ?,
       last_liked_at = MAX(COALESCE(last_liked_at, 0), ?),
       dislikes = COALESCE(dislikes, 0) + ?,
       updated_at = ?
 WHERE id = ?`,
		duplicate.Views,
		unixMilliOrZero(duplicate.LastViewedAt),
		duplicate.Favorites,
		duplicate.Comments,
		duplicate.Likes,
		unixMilliOrZero(duplicate.LastLikedAt),
		duplicate.Dislikes,
		now,
		canonicalID,
	)
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func mergeDuplicateVideoTagsTx(ctx context.Context, tx *sql.Tx, duplicateID, canonicalID string) error {
	type assignment struct {
		tagID     int64
		source    string
		evidence  string
		createdAt int64
	}
	rows, err := tx.QueryContext(ctx, `
SELECT tag_id, COALESCE(source, ''), COALESCE(evidence, ''), created_at
  FROM video_tags
 WHERE video_id = ?`, duplicateID)
	if err != nil {
		return err
	}
	assignments := make([]assignment, 0)
	for rows.Next() {
		var item assignment
		if err := rows.Scan(&item.tagID, &item.source, &item.evidence, &item.createdAt); err != nil {
			rows.Close()
			return err
		}
		assignments = append(assignments, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, incoming := range assignments {
		var existingSource, existingEvidence string
		err := tx.QueryRowContext(ctx, `
SELECT COALESCE(source, ''), COALESCE(evidence, '')
  FROM video_tags
 WHERE video_id = ? AND tag_id = ?`, canonicalID, incoming.tagID).Scan(&existingSource, &existingEvidence)
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO video_tags (video_id, tag_id, source, evidence, created_at)
VALUES (?, ?, ?, ?, ?)`, canonicalID, incoming.tagID, normalizeVideoTagSource(incoming.source), incoming.evidence, incoming.createdAt); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		// The canonical assignment wins ties; only a stronger source (for
		// example manual over auto) may replace its metadata during a merge.
		if videoTagAssignmentPriority(incoming.source) <= videoTagAssignmentPriority(existingSource) {
			continue
		}
		evidence := incoming.evidence
		if evidence == "" {
			evidence = existingEvidence
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE video_tags
   SET source = ?, evidence = ?
 WHERE video_id = ? AND tag_id = ?`, normalizeVideoTagSource(incoming.source), evidence, canonicalID, incoming.tagID); err != nil {
			return err
		}
	}

	var duplicateManual, canonicalManual int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(tags_manual, 0) FROM videos WHERE id = ?`, duplicateID).Scan(&duplicateManual); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(tags_manual, 0) FROM videos WHERE id = ?`, canonicalID).Scan(&canonicalManual); err != nil {
		return err
	}
	return syncVideoTagsJSONTx(ctx, tx, canonicalID, duplicateManual != 0 || canonicalManual != 0)
}

func redirectDuplicateVideoReferencesTx(ctx context.Context, tx *sql.Tx, duplicateID, canonicalID string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE telegram_files SET video_id=? WHERE video_id=?`, canonicalID, duplicateID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE deleted_videos
   SET canonical_video_id = ?
 WHERE reason = ?
   AND canonical_video_id = ?`, canonicalID, DeletedVideoReasonDuplicate, duplicateID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE crawler_seen_sources
   SET canonical_video_id = ?
 WHERE canonical_video_id = ?`, canonicalID, duplicateID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE remote_upload_jobs
   SET completed_video_id = ?
 WHERE completed_video_id = ?`, canonicalID, duplicateID); err != nil {
		return err
	}
	return nil
}

func enqueueDuplicateAssetCleanupTx(ctx context.Context, tx *sql.Tx, video *Video) error {
	now := time.Now().UnixMilli()
	_, err := tx.ExecContext(ctx, `
INSERT INTO duplicate_asset_cleanup_jobs (
  video_id, preview_local, attempts, last_error, created_at, updated_at
) VALUES (?, ?, 0, '', ?, ?)
ON CONFLICT(video_id) DO UPDATE SET
  preview_local = excluded.preview_local,
  attempts = 0,
  last_error = '',
  updated_at = excluded.updated_at`, video.ID, strings.TrimSpace(video.PreviewLocal), now, now)
	return err
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
