package catalog

import (
	"context"
	"fmt"
	"time"

	"github.com/nianzhibai/91/backend/internal/driveevents"
)

// GenerationKinds selects the queues whose failed work may be retried.
// The caller decides which generators are available and enabled.
type GenerationKinds struct {
	Thumbnails   bool
	Previews     bool
	Fingerprints bool
}

type GenerationRetryCounts struct {
	Thumbnails   int64
	Previews     int64
	Fingerprints int64
}

// ResetFailedGeneration admits one retry batch by moving eligible failed work
// back to pending. Callers invoke it once before dispatching generation, never
// from a recurring pending-queue poll. Conditional updates preserve work that
// another worker has already completed; the transaction resets all selected
// kinds before any of them can fail again.
func (c *Catalog) ResetFailedGeneration(ctx context.Context, driveID string, kinds GenerationKinds) (resultValue GenerationRetryCounts, resultErr error) {
	defer c.notifyDriveWrite(&resultErr, driveID, driveevents.GenerationChanged)
	counts := GenerationRetryCounts{}
	if !kinds.Thumbnails && !kinds.Previews && !kinds.Fingerprints {
		return counts, ctx.Err()
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return counts, err
	}
	defer tx.Rollback()

	updates := []struct {
		name    string
		enabled bool
		query   string
		count   *int64
	}{
		{
			name: "thumbnails", enabled: kinds.Thumbnails, count: &counts.Thumbnails,
			query: `UPDATE videos
			   SET thumbnail_status = 'pending', thumbnail_failures = 0, updated_at = ?
			 WHERE drive_id = ? AND thumbnail_status = 'failed'
			   AND COALESCE(thumbnail_url, '') = ''
			   AND COALESCE(hidden, 0) = 0 AND ` + uniqueVideoWhereSQL,
		},
		{
			name: "previews", enabled: kinds.Previews, count: &counts.Previews,
			query: `UPDATE videos
			   SET preview_status = 'pending', preview_file_id = '', preview_local = '',
			       preview_updated_at = 0, updated_at = ?
			 WHERE drive_id = ? AND preview_status = 'failed'
			   AND COALESCE(hidden, 0) = 0 AND ` + uniqueVideoWhereSQL,
		},
		{
			name: "fingerprints", enabled: kinds.Fingerprints, count: &counts.Fingerprints,
			query: `UPDATE videos
			   SET fingerprint_status = 'pending', fingerprint_error = '', updated_at = ?
			 WHERE drive_id = ? AND fingerprint_status = 'failed'
			   AND size_bytes > 0 AND COALESCE(sampled_sha256, '') = ''
			   AND COALESCE(hidden, 0) = 0`,
		},
	}
	now := time.Now().UnixMilli()
	for _, update := range updates {
		if !update.enabled {
			continue
		}
		result, err := tx.ExecContext(ctx, update.query, now, driveID)
		if err != nil {
			return GenerationRetryCounts{}, fmt.Errorf("reset failed %s: %w", update.name, err)
		}
		*update.count, err = result.RowsAffected()
		if err != nil {
			return GenerationRetryCounts{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return GenerationRetryCounts{}, err
	}
	return counts, nil
}
