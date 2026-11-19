package catalog

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/nianzhibai/91/backend/internal/dedupe"
	"github.com/nianzhibai/91/backend/internal/driveevents"
)

// InsertScannedVideo admits a new scan row only if neither its source identity
// nor a live duplicate is already stored. The caller owns metadata matching and
// attaches automatic tags after admission; only an inserted row needs generation.
// seenFileIDs identifies live files on v.DriveID so stale rows on that same drive
// cannot prevent replacement files from being admitted before presence cleanup.
func (c *Catalog) InsertScannedVideo(ctx context.Context, v *Video, seenFileIDs map[string]struct{}) (bool, error) {
	if v == nil || v.ID == "" || v.DriveID == "" || v.FileID == "" {
		return false, errors.New("catalog: scanned video requires video, drive, and file IDs")
	}
	// Catalog opens SQLite with _txlock=immediate, serializing the decision
	// with concurrent scans and source deletions.
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var removing bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM video_source_removals WHERE drive_id=? AND file_id=? AND expires_at>unixepoch())`, v.DriveID, v.FileID).Scan(&removing); err != nil {
		return false, err
	}
	if removing {
		return false, ErrVideoSourceInUse
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM videos WHERE id=? OR (drive_id=? AND file_id=?))", v.ID, v.DriveID, v.FileID).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	blocked, err := isDeletedVideoCandidate(ctx, tx, v.ID, v.DriveID, v.FileID, v.ContentHash, v.FileName, v.Size)
	if err != nil || blocked {
		return false, err
	}
	skip, err := reconcileMergedScan(ctx, tx, v)
	if err != nil {
		return false, err
	}
	if skip {
		return false, tx.Commit()
	}
	duplicate, err := findScannedVideoDuplicate(ctx, tx, v, seenFileIDs)
	if err != nil {
		return false, err
	}
	if duplicate != nil {
		if err := recordScannedDuplicate(ctx, tx, v, duplicate, DuplicateOutcomeSkipped); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if _, err := upsertVideoRow(ctx, tx, v); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	c.driveEvents.Notify("", false, driveevents.MediaChanged)
	return true, nil
}

// FindScannedVideoDuplicate applies the same duplicate policy to an existing
// scan row without treating the row itself as a duplicate. New rows must use
// InsertScannedVideo so the decision and insertion share one transaction.
func (c *Catalog) FindScannedVideoDuplicate(ctx context.Context, v *Video, seenFileIDs map[string]struct{}) (*Video, error) {
	return findScannedVideoDuplicate(ctx, c.db, v, seenFileIDs)
}

// RecordScannedDuplicate records the observed match for an already admitted
// row. It does not tombstone the file or change later admission decisions,
// so it writes only audit history and publishes no media change.
func (c *Catalog) RecordScannedDuplicate(ctx context.Context, source, duplicate *Video) error {
	return recordScannedDuplicate(ctx, c.db, source, duplicate, DuplicateOutcomeExisting)
}

func recordScannedDuplicate(ctx context.Context, exec videoRowExecer, source, duplicate *Video, outcome string) error {
	if source == nil || duplicate == nil {
		return errors.New("catalog: scan duplicate requires both videos")
	}
	reason := exactDuplicateReason(source, duplicate)
	if reason == "" && weakDuplicate(source, duplicate) {
		reason = dedupe.ReasonFileNameSize
	}
	if reason == "" {
		return errors.New("catalog: scan duplicate snapshots do not match the scan policy")
	}
	evidence := dedupe.NewEvidence(reason, duplicate.ID, duplicate.ID, "earliest_matching_candidate")
	return recordDuplicateDecision(ctx, exec, DuplicateOriginScan, outcome, source, duplicate, duplicate, duplicate, evidence)
}

type scanDuplicateQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// findScannedVideoDuplicate applies the exact identity rule shared with crawler
// ingress and maintenance. Scan additionally treats a matching file name and
// size as a provisional signal before a fingerprint exists: the file is only
// skipped, never tombstoned, and is admitted once the matched row disappears.
func findScannedVideoDuplicate(ctx context.Context, query scanDuplicateQuerier, v *Video, seenFileIDs map[string]struct{}) (*Video, error) {
	// Only active conditions enter the predicate: a constant branch in the OR
	// stops SQLite from using the hash, fingerprint and name/size indexes.
	var clauses []string
	var args []any
	exactWhere, exactArgs, hasExact := equivalentVideoLookupWhere(v)
	if hasExact {
		clauses = append(clauses, exactWhere)
		args = append(args, exactArgs...)
	}
	hasName := v.FileName != "" && v.Size > 0
	if hasName {
		// The SQL form of weakDuplicate against the incoming observation.
		sampled, hash := normalizeContentHash(v.SampledSHA256), normalizeContentHash(v.ContentHash)
		clauses = append(clauses, `(file_name = ? AND size_bytes = ?
			AND (COALESCE(sampled_sha256, '') = '' OR ? = '' OR sampled_sha256 = ?)
			AND (drive_id != ? OR COALESCE(content_hash, '') = '' OR ? = '' OR content_hash = ?))`)
		args = append(args, v.FileName, v.Size, sampled, sampled, v.DriveID, hash, hash)
	}
	if len(clauses) == 0 {
		return nil, nil
	}
	order := "created_at ASC, id ASC"
	if hasExact && hasName {
		// Exact matches sort before provisional name/size matches.
		order = "CASE WHEN " + exactWhere + " THEN 0 ELSE 1 END, " + order
		args = append(args, exactArgs...)
	}
	rows, err := query.QueryContext(ctx, `SELECT `+allVideoCols+` FROM videos
WHERE `+duplicateLookupScope+`
  AND (`+strings.Join(clauses, " OR ")+`)
ORDER BY `+order, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		candidate, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		if candidate.ID == v.ID {
			continue
		}
		if candidate.DriveID == v.DriveID {
			// Keep looking after stale sources: a later candidate can still be live.
			if _, seen := seenFileIDs[candidate.FileID]; !seen {
				continue
			}
		}
		return candidate, nil
	}
	return nil, rows.Err()
}
