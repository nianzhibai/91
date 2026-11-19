package catalog

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nianzhibai/91/backend/internal/driveevents"
)

// A nonzero hidden state excludes the row from public and duplicate lookups.
// Legacy hides (1) predate tracking the reason and retain their blacklist policy.
const hiddenForInvalidSize = 2

func (v *Video) hiddenState() int {
	if v.Hidden && v.SizeUnavailable {
		return hiddenForInvalidSize
	}
	return boolToInt(v.Hidden)
}

// MarkInvalidSizeSources keeps the last usable metadata for files the provider
// still lists. An unknown file has no row to update and is never admitted here.
func (c *Catalog) MarkInvalidSizeSources(ctx context.Context, driveID string, fileIDs []string) error {
	if len(fileIDs) == 0 {
		return nil
	}
	ids, err := json.Marshal(fileIDs)
	if err != nil {
		return err
	}
	result, err := c.db.ExecContext(ctx, `UPDATE videos SET hidden = ?, updated_at = ?
		WHERE drive_id = ? AND file_id IN (SELECT value FROM json_each(?))
		AND COALESCE(hidden, 0) = 0`, hiddenForInvalidSize, time.Now().UnixMilli(), driveID, string(ids))
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if changed > 0 {
		c.driveEvents.Notify("", false, driveevents.MediaChanged)
	}
	return err
}
