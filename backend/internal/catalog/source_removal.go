package catalog

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"
)

var ErrVideoSourceInUse = errors.New("video source is in use")

// Install after legacy migrations which may rebuild the videos table.
func (c *Catalog) ensureSourceRemovalGuards(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx, `
CREATE TRIGGER IF NOT EXISTS guard_source_removal_insert
BEFORE INSERT ON videos
WHEN EXISTS (SELECT 1 FROM video_source_removals
             WHERE drive_id = NEW.drive_id AND file_id = NEW.file_id AND expires_at > unixepoch())
BEGIN SELECT RAISE(ABORT, 'video source removal in progress'); END;
CREATE TRIGGER IF NOT EXISTS guard_source_removal_move
BEFORE UPDATE OF drive_id, file_id ON videos
WHEN (OLD.drive_id != NEW.drive_id OR OLD.file_id != NEW.file_id)
 AND EXISTS (SELECT 1 FROM video_source_removals
             WHERE (drive_id, file_id) IN ((OLD.drive_id, OLD.file_id), (NEW.drive_id, NEW.file_id))
               AND expires_at > unixepoch())
BEGIN SELECT RAISE(ABORT, 'video source removal in progress'); END;
DELETE FROM video_source_removals WHERE expires_at <= unixepoch();
`)
	return err
}

// WithVideoSourceRemoval reserves the physical file through the provider call.
// exceptID may name the live row being removed. A tombstone has no exemption.
// SQLite serializes the ownership check and reservation, but other files and
// provider credential writes remain available during the network request.
func (c *Catalog) WithVideoSourceRemoval(ctx context.Context, driveID, fileID, exceptID string, remove func(context.Context) error) (err error) {
	// Bound a single provider attempt. A crashed process leaves a reservation
	// only until that deadline and a short grace period for cancellation.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	deadline, _ := ctx.Deadline()
	token := rand.Text()
	res, err := c.db.ExecContext(ctx, `
INSERT INTO video_source_removals (drive_id, file_id, token, expires_at)
SELECT ?, ?, ?, ? WHERE NOT EXISTS (
	SELECT 1 FROM videos WHERE drive_id = ? AND file_id = ? AND id != ?
)
AND (? = '' OR EXISTS (SELECT 1 FROM videos WHERE id = ? AND drive_id = ? AND file_id = ?))
ON CONFLICT(drive_id, file_id) DO UPDATE SET token = excluded.token, expires_at = excluded.expires_at
WHERE video_source_removals.expires_at <= unixepoch()`,
		driveID, fileID, token, deadline.Add(time.Minute).Unix(), driveID, fileID, exceptID,
		exceptID, exceptID, driveID, fileID)
	if err != nil {
		return err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrVideoSourceInUse
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, releaseErr := c.db.ExecContext(releaseCtx, `DELETE FROM video_source_removals
			WHERE drive_id = ? AND file_id = ? AND token = ?`, driveID, fileID, token); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release video source removal: %w", releaseErr))
		}
	}()
	return remove(ctx)
}
