package catalog

import (
	"context"
	"crypto/rand"
	"database/sql"
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
CREATE TRIGGER IF NOT EXISTS guard_source_removal_delete
BEFORE DELETE ON videos
WHEN EXISTS (SELECT 1 FROM video_source_removals
             WHERE drive_id = OLD.drive_id AND file_id = OLD.file_id AND expires_at > unixepoch())
BEGIN SELECT RAISE(ABORT, 'video source removal in progress'); END;
DELETE FROM video_source_removals WHERE expires_at <= unixepoch();
`)
	return err
}

// WithBlacklistedSourceRemoval rechecks the snapshot under the writer lock
// before reserving a physical file. Other aliases may have finished it already.
func (c *Catalog) WithBlacklistedSourceRemoval(ctx context.Context, item *DeletedVideo, remove func(context.Context) error) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var pending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deleted_videos WHERE id=? AND drive_id=? AND file_id=? AND deleted_at=? AND `+pendingSourceDeletionWhere+`)`, item.ID, item.DriveID, item.FileID, item.DeletedAt).Scan(&pending); err != nil {
		return err
	}
	if !pending {
		return ErrVideoSourceInUse
	}
	token := rand.Text()
	deadline, _ := ctx.Deadline()
	if err := reserveDeletionFile(ctx, tx, item.DriveID, item.FileID, token, deadline); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, c.releaseSourceReservations(ctx, token)) }()
	return remove(ctx)
}

func (c *Catalog) releaseSourceReservations(ctx context.Context, token string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := c.db.ExecContext(ctx, "DELETE FROM video_source_removals WHERE token=?", token)
	if err != nil {
		return fmt.Errorf("release source reservations: %w", err)
	}
	return nil
}

// RecordSourceDeleted finalizes one physical removal, including aliases that
// were blacklisted separately. Pending local assets remain durably scheduled.
func (c *Catalog) RecordSourceDeleted(ctx context.Context, source *Video) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := recordSourceDeletedTx(ctx, tx, source); err != nil {
		return err
	}
	return tx.Commit()
}

// recordSourceDeletedTx keeps the blacklist entries of a removed file. They
// still block their content; with the source gone they can only be lifted,
// never restored. A pending crawler restore has nothing left to restore.
func recordSourceDeletedTx(ctx context.Context, tx *sql.Tx, source *Video) error {
	// Other generations recorded at this provider ID must never inherit a
	// successful removal of different bytes.
	if err := markReplacedMergedSources(ctx, tx, source); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE merged_videos SET source_state='gone'
		WHERE drive_id=? AND file_id=? AND source_state='recorded'`, source.DriveID, source.FileID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO duplicate_asset_cleanup_jobs(video_id,preview_local,created_at,updated_at)
		SELECT d.id, CASE WHEN json_valid(d.restore_payload) THEN COALESCE(json_extract(d.restore_payload,'$.video.previewLocal'),'') ELSE '' END, ?, ?
		FROM deleted_videos d WHERE d.drive_id=? AND d.file_id=? AND d.asset_cleanup_after>0
		AND NOT EXISTS (SELECT 1 FROM videos WHERE id=d.id)
		ON CONFLICT(video_id) DO NOTHING`, time.Now().UnixMilli(), time.Now().UnixMilli(), source.DriveID, source.FileID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM deleted_videos WHERE drive_id=? AND file_id=? AND restore_requested=1`, source.DriveID, source.FileID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE deleted_videos SET source_deleted=1, restore_payload='', asset_cleanup_after=0
		WHERE drive_id=? AND file_id=?`, source.DriveID, source.FileID)
	return err
}
