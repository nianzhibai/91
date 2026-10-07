package catalog

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"

	"github.com/video-site/backend/internal/tagging"
)

const retiredGeneratedTagIDsSQL = `
SELECT t.id
  FROM tags t
 WHERE lower(trim(COALESCE(t.source, ''))) = 'generated'
   AND lower(trim(COALESCE(t.origin, ''))) NOT IN ('crawler', 'telegram')
   AND NOT EXISTS (
     SELECT 1
       FROM video_tags vt_source
      WHERE vt_source.tag_id = t.id
        AND lower(trim(COALESCE(vt_source.source, ''))) IN ('crawler', 'telegram')
   )`

// removeAutomaticTaggingArtifacts removes the retired "create new labels from
// content" model. It preserves user tag definitions plus crawler and Telegram
// provenance tags, and leaves engine assignments that point at preserved tags
// for the subsequent existing-tag retag pass to refresh.
func (c *Catalog) removeAutomaticTaggingArtifacts(ctx context.Context) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	affectedRows, err := tx.QueryContext(ctx, `
SELECT DISTINCT vt.video_id
  FROM video_tags vt
  LEFT JOIN tags t ON t.id = vt.tag_id
 WHERE lower(trim(COALESCE(vt.source, ''))) IN ('series', 'propagated')
    OR vt.tag_id IN (`+retiredGeneratedTagIDsSQL+`)`)
	if err != nil {
		return err
	}
	var videoIDs []string
	for affectedRows.Next() {
		var videoID string
		if err := affectedRows.Scan(&videoID); err != nil {
			affectedRows.Close()
			return err
		}
		videoIDs = append(videoIDs, videoID)
	}
	if err := affectedRows.Err(); err != nil {
		affectedRows.Close()
		return err
	}
	if err := affectedRows.Close(); err != nil {
		return err
	}

	if err := mergeRetiredAVSeriesAssignmentsTx(ctx, tx); err != nil {
		return err
	}

	removedAssignments := int64(0)
	res, err := tx.ExecContext(ctx, `
DELETE FROM video_tags
 WHERE lower(trim(COALESCE(source, ''))) IN ('series', 'propagated')`)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil {
		removedAssignments += n
	}
	res, err = tx.ExecContext(ctx, `DELETE FROM video_tags WHERE tag_id IN (`+retiredGeneratedTagIDsSQL+`)`)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil {
		removedAssignments += n
	}

	res, err = tx.ExecContext(ctx, `DELETE FROM tags WHERE id IN (`+retiredGeneratedTagIDsSQL+`)`)
	if err != nil {
		return err
	}
	removedTags, _ := res.RowsAffected()

	staleRows, err := tx.QueryContext(ctx, `
SELECT id
  FROM videos
 WHERE COALESCE(tags_manual, 0) = 0
   AND COALESCE(tags, '') NOT IN ('', '[]', 'null')
   AND NOT EXISTS (
     SELECT 1
       FROM video_tags vt
      WHERE vt.video_id = videos.id
   )`)
	if err != nil {
		return err
	}
	for staleRows.Next() {
		var videoID string
		if err := staleRows.Scan(&videoID); err != nil {
			staleRows.Close()
			return err
		}
		videoIDs = append(videoIDs, videoID)
	}
	if err := staleRows.Err(); err != nil {
		staleRows.Close()
		return err
	}
	if err := staleRows.Close(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE videos
   SET tags = '[]'
 WHERE COALESCE(tags_manual, 0) = 0
   AND COALESCE(tags, '') NOT IN ('', '[]', 'null')
   AND NOT EXISTS (
     SELECT 1
       FROM video_tags vt
      WHERE vt.video_id = videos.id
   )`); err != nil {
		return err
	}

	for _, videoID := range uniqueStrings(videoIDs) {
		manual := hasManualTagsTx(ctx, tx, videoID)
		if err := syncVideoTagsJSONTx(ctx, tx, videoID, manual); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `
DELETE FROM settings WHERE key IN (
  'tags.auto_generate_enabled', 'tags.retag.v2_done', 'tags.maintenance.last_run_ms'
)`); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	if removedAssignments > 0 || removedTags > 0 {
		log.Printf("[catalog] removed retired automatic tagging artifacts: assignments=%d tags=%d", removedAssignments, removedTags)
		if removedTags > 0 {
			if err := c.bumpTagRulesVersion(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

// Preserve the AV classification of legacy series-only videos, including
// manually curated videos. User-defined and crawler-owned labels are excluded.
// A deliberately disabled or deleted AV rule must not be restored by migration.
func mergeRetiredAVSeriesAssignmentsTx(ctx context.Context, tx *sql.Tx) error {
	av, err := getTagByLabelTxRaw(ctx, tx, avTagLabel)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	allowedPrefixes := make(map[string]bool)
	for _, prefix := range effectiveRule(av.Label, av.MatchRules).AVCodePrefixes {
		allowedPrefixes[prefix] = true
	}
	if len(allowedPrefixes) == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `
SELECT vt.video_id, t.label, COALESCE(vt.source, ''), COALESCE(vt.evidence, ''), vt.created_at
  FROM video_tags vt
  JOIN tags t ON t.id = vt.tag_id
 WHERE lower(trim(COALESCE(t.origin, ''))) = 'av_series'
   AND t.id IN (`+retiredGeneratedTagIDsSQL+`)
 ORDER BY vt.video_id, t.id`)
	if err != nil {
		return err
	}
	type legacyAssignment struct {
		videoID, source, evidence string
		createdAt                 int64
	}
	var assignments []legacyAssignment
	for rows.Next() {
		var assignment legacyAssignment
		var label string
		if err := rows.Scan(&assignment.videoID, &label, &assignment.source, &assignment.evidence, &assignment.createdAt); err != nil {
			rows.Close()
			return err
		}
		if allowedPrefixes[tagging.NormalizeAVCodePrefix(label)] {
			assignments = append(assignments, assignment)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, assignment := range assignments {
		source := normalizeVideoTagSource(assignment.source)
		var existingSource, existingEvidence string
		err := tx.QueryRowContext(ctx, `
SELECT COALESCE(source, ''), COALESCE(evidence, '')
  FROM video_tags WHERE video_id = ? AND tag_id = ?`, assignment.videoID, av.ID).Scan(&existingSource, &existingEvidence)
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO video_tags (video_id, tag_id, source, evidence, created_at)
VALUES (?, ?, ?, ?, ?)`, assignment.videoID, av.ID, source, assignment.evidence, assignment.createdAt); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		// An assignment scheduled for cleanup cannot block a surviving one,
		// even when normalization gives it the same or a higher priority.
		existingSource = strings.ToLower(strings.TrimSpace(existingSource))
		existingSourceRetired := existingSource == "series" || existingSource == "propagated"
		if !existingSourceRetired && (!shouldReplaceVideoTagAssignment(existingSource, source) || normalizeVideoTagSource(existingSource) == source) {
			continue
		}
		evidence := assignment.evidence
		if strings.TrimSpace(evidence) == "" {
			evidence = existingEvidence
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE video_tags SET source = ?, evidence = ? WHERE video_id = ? AND tag_id = ?`,
			source, evidence, assignment.videoID, av.ID); err != nil {
			return err
		}
	}
	return nil
}
