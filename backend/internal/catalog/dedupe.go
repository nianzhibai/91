package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/dedupe"
	"github.com/nianzhibai/91/backend/internal/driveevents"
)

var ErrDuplicatePlanStale = errors.New("catalog: duplicate plan is stale")

// DuplicateVideoDeletion is one finalized action from the read-only dedupe
// planner. CanonicalVideoID must identify a surviving videos row, never another
// deletion in the same plan. Expected revisions come from DuplicateRevision;
// an empty revision skips that row's staleness check.
type DuplicateVideoDeletion struct {
	VideoID                   string
	CanonicalVideoID          string
	ExpectedRevision          string
	CanonicalExpectedRevision string
	Origin                    string
	Evidence                  dedupe.Evidence
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

// ApplyDuplicateVideoDeletions atomically applies a finalized hard-dedupe plan.
// User state and durable references are merged into each canonical row before
// duplicate rows are retired. Generated files are intentionally not touched
// here; cleanup jobs are committed with the aliases and processed later.
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
	canonicalRevisions := make(map[string]string)
	for _, deletion := range normalized {
		video, err := loadDuplicateRevisionTx(ctx, tx, "duplicate", deletion.VideoID, deletion.ExpectedRevision)
		if err != nil {
			return err
		}
		videos[deletion.VideoID] = video
		if expected, exists := canonicalRevisions[deletion.CanonicalVideoID]; exists && expected != deletion.CanonicalExpectedRevision {
			return fmt.Errorf("catalog: canonical video %s has conflicting expected revisions", deletion.CanonicalVideoID)
		}
		canonicalRevisions[deletion.CanonicalVideoID] = deletion.CanonicalExpectedRevision
	}
	for canonicalID, expectedRevision := range canonicalRevisions {
		if _, err := loadDuplicateRevisionTx(ctx, tx, "canonical", canonicalID, expectedRevision); err != nil {
			return err
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
		if err := mergeAndRetireDuplicateVideoTx(ctx, tx, videos[deletion.VideoID], deletion.CanonicalVideoID, deletion.Evidence.Reason); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// loadDuplicateRevisionTx reads a row a duplicate decision depends on and
// rejects the plan when that row is gone or its DuplicateRevision changed.
func loadDuplicateRevisionTx(ctx context.Context, tx *sql.Tx, role, id, expectedRevision string) (*Video, error) {
	video, err := scanVideo(tx.QueryRowContext(ctx, `SELECT `+allVideoCols+` FROM videos WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s video %s no longer exists", ErrDuplicatePlanStale, role, id)
	}
	if err != nil {
		return nil, err
	}
	if expectedRevision != "" && DuplicateRevision(video) != expectedRevision {
		return nil, fmt.Errorf("%w: %s video %s changed since planning", ErrDuplicatePlanStale, role, id)
	}
	return video, nil
}

// ResolveVideoID maps a public video ID through merged video aliases to the
// current live row. Catalog.GetVideo deliberately keeps its strict row lookup
// semantics; callers opt into alias resolution only at public read boundaries.
func (c *Catalog) ResolveVideoID(ctx context.Context, id string) (string, error) {
	return resolveVideoID(ctx, c.db, id)
}

func resolveVideoID(ctx context.Context, query deletedVideoRestoreQuerier, id string) (string, error) {
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
		err := query.QueryRowContext(ctx, `SELECT id FROM videos WHERE id = ?`, current).Scan(&liveID)
		if err == nil {
			return liveID, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}

		var next string
		err = query.QueryRowContext(ctx, `
SELECT canonical_video_id
  FROM merged_videos
 WHERE video_id = ?`, current).Scan(&next)
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

func mergeAndRetireDuplicateVideoTx(ctx context.Context, tx *sql.Tx, duplicate *Video, canonicalID, reason string) error {
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO merged_videos
		(video_id,canonical_video_id,drive_id,file_id,parent_id,file_name,size_bytes,content_hash,sampled_sha256,source_mod_time,merge_reason,merged_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, duplicate.ID, canonicalID, duplicate.DriveID, duplicate.FileID, duplicate.ParentID,
		duplicate.FileName, duplicate.Size, duplicate.ContentHash, duplicate.SampledSHA256, duplicate.SourceModTime, reason, time.Now().UnixMilli()); err != nil {
		return err
	}
	if err := deleteVideoRowTx(ctx, tx, duplicate.ID); err != nil {
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
UPDATE merged_videos SET canonical_video_id = ? WHERE canonical_video_id = ?`, canonicalID, duplicateID); err != nil {
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

// A cleanup job belongs to a retired video ID. When the ID becomes live again,
// as when a backup restore merges the row back, its assets belong to the live
// row and the job is dropped in the same statement. Install after legacy
// migrations which may rebuild the videos table.
func (c *Catalog) ensureDuplicateAssetCleanupGuard(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx, `
CREATE TRIGGER IF NOT EXISTS drop_duplicate_asset_cleanup_on_revive
AFTER INSERT ON videos
BEGIN DELETE FROM duplicate_asset_cleanup_jobs WHERE video_id = NEW.id; END;
DELETE FROM duplicate_asset_cleanup_jobs WHERE video_id IN (SELECT id FROM videos);
`)
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

// CleanupRetiredVideoAssets rechecks the job under the SQLite writer lock.
// Restoring the video cancels the job and cannot race the filesystem removal.
func (c *Catalog) CleanupRetiredVideoAssets(ctx context.Context, id string, remove func(string) error) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var preview string
	err = tx.QueryRowContext(ctx, "SELECT preview_local FROM duplicate_asset_cleanup_jobs WHERE video_id = ?", id).Scan(&preview)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	cleanupErr := remove(preview)
	if cleanupErr == nil {
		_, err = tx.ExecContext(ctx, "DELETE FROM duplicate_asset_cleanup_jobs WHERE video_id = ?", id)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE duplicate_asset_cleanup_jobs SET attempts = attempts + 1,
			last_error = substr(?,1,500), updated_at = ? WHERE video_id = ?`, cleanupErr.Error(), time.Now().UnixMilli(), id)
	}
	if err != nil {
		return errors.Join(cleanupErr, err)
	}
	return errors.Join(cleanupErr, tx.Commit())
}
