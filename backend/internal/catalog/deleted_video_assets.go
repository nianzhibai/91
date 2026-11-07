package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/nianzhibai/91/backend/internal/driveevents"
)

var ErrDeletedVideoAssetCleanupRequired = errors.New("deleted video asset cleanup must complete before restore")

// ListExpiredDeletedVideoAssets returns durable cleanup work. The tombstone
// owns the deadline, so restoring or deleting it also cancels its cleanup.
func (c *Catalog) ListExpiredDeletedVideoAssets(ctx context.Context, now time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT id FROM deleted_videos
 WHERE asset_cleanup_after > 0 AND asset_cleanup_after <= ?
   AND restore_requested = 0
 ORDER BY asset_cleanup_after, id LIMIT ?`, now.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CleanupDeletedVideoAssets rechecks the deadline while holding the SQLite
// writer reservation. Restore cannot publish the same assets during removal.
// Explicit source deletion uses force to bypass the undo period. A failed
// removal or commit keeps the work pending for an idempotent retry.
func (c *Catalog) CleanupDeletedVideoAssets(ctx context.Context, id string, now time.Time, force bool, remove func(*Video) error) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var encoded string
	var deadline int64
	err = tx.QueryRowContext(ctx, `
SELECT restore_payload, asset_cleanup_after FROM deleted_videos
 WHERE id = ? AND asset_cleanup_after > 0 AND restore_requested = 0`, id).Scan(&encoded, &deadline)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !force && now.UnixMilli() < deadline {
		return nil
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM videos WHERE id = ?)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		payload, err := decodeDeletedVideoRestorePayload(id, encoded)
		if err != nil {
			return err
		}
		video := payload.Video
		if video == nil {
			video = &Video{}
		}
		video.ID = id
		if err := remove(video); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE deleted_videos SET asset_cleanup_after = 0 WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// undoVideoDeletion restores the original row and resource references together
// when cleanup has not become due. This works for every source without a scan
// or provider request: ordinary deletion has kept the original source intact.
func (c *Catalog) undoVideoDeletion(ctx context.Context, id string) (result DeletedVideoRestoreResult, restored bool, resultErr error) {
	defer func() {
		if restored {
			c.notifyDriveWrite(&resultErr, "", driveevents.MediaChanged)
		}
	}()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return result, false, err
	}
	defer tx.Rollback()
	deleted, _, encoded, err := c.deletedVideoForRestore(ctx, tx, id)
	if err != nil {
		return result, false, err
	}
	if deleted.SourceDeleted || deleted.Reason == DeletedVideoReasonDuplicate ||
		deleted.AssetCleanupAfter <= time.Now().UnixMilli() {
		return result, false, nil
	}
	payload, err := decodeDeletedVideoRestorePayload(id, encoded)
	if err != nil {
		return result, false, err
	}
	video, err := scanVideo(tx.QueryRowContext(ctx, `SELECT `+allVideoCols+` FROM videos WHERE id = ?`, id))
	switch {
	case err == nil:
		if video.DriveID != deleted.DriveID || video.FileID != deleted.FileID {
			return result, false, fmt.Errorf("%w: existing video source does not match tombstone", ErrDeletedVideoNotRestorable)
		}
	case errors.Is(err, sql.ErrNoRows):
		if payload.Video == nil {
			return result, false, errors.New("catalog: undo deletion requires a video snapshot")
		}
		video = payload.Video
		video.ID, video.DriveID, video.FileID = id, deleted.DriveID, deleted.FileID
		video.Hidden = false
		if _, err := upsertVideoRow(ctx, tx, video); err != nil {
			return result, false, err
		}
		if err := restoreDeletedVideoTagsTx(ctx, tx, video, payload); err != nil {
			return result, false, err
		}
	default:
		return result, false, err
	}
	video, err = scanVideo(tx.QueryRowContext(ctx, `SELECT `+allVideoCols+` FROM videos WHERE id = ?`, id))
	if err != nil {
		return result, false, err
	}
	if err := deletePendingDeletedVideoTx(ctx, tx, id); err != nil {
		return result, false, err
	}
	if err := tx.Commit(); err != nil {
		return result, false, err
	}
	return DeletedVideoRestoreResult{RestorePolicy: DeletedVideoRestorePolicyDirect, Video: video}, true, nil
}
