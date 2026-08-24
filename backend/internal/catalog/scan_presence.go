package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type scanPresenceVideo struct {
	fileID   string
	parentID string
}

// ConfirmMissingDriveFiles advances the durable missing counter for files that
// were eligible for this successful scan but were not observed. A live file
// clears its counter immediately. Only file IDs reaching threshold in this
// snapshot are returned to the caller for destructive cleanup. For a full-drive
// scan every catalog row is eligible, including rows below directories that the
// current scan policy excludes; excluding a directory therefore removes its old
// rows from management after the normal confirmation threshold.
func (c *Catalog) ConfirmMissingDriveFiles(
	ctx context.Context,
	driveID string,
	liveFileIDs map[string]struct{},
	visitedDirIDs map[string]struct{},
	fullDriveScan bool,
	threshold int,
) (map[string]struct{}, error) {
	if c == nil || c.db == nil {
		return nil, errors.New("catalog: database is not open")
	}
	driveID = strings.TrimSpace(driveID)
	if driveID == "" {
		return nil, errors.New("catalog: empty drive id")
	}
	if threshold < 2 {
		return nil, fmt.Errorf("catalog: unsafe missing-file confirmation threshold %d", threshold)
	}

	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT file_id, COALESCE(parent_id, '') FROM videos WHERE drive_id = ?`, driveID)
	if err != nil {
		return nil, err
	}
	var videos []scanPresenceVideo
	for rows.Next() {
		var video scanPresenceVideo
		if err := rows.Scan(&video.fileID, &video.parentID); err != nil {
			rows.Close()
			return nil, err
		}
		videos = append(videos, video)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	confirmed := make(map[string]struct{})
	now := time.Now().UnixMilli()
	for _, video := range videos {
		fileID := strings.TrimSpace(video.fileID)
		if fileID == "" {
			continue
		}
		if _, live := liveFileIDs[fileID]; live {
			if _, err := tx.ExecContext(ctx, `DELETE FROM drive_scan_misses WHERE drive_id = ? AND file_id = ?`, driveID, fileID); err != nil {
				return nil, err
			}
			continue
		}
		if !fullDriveScan {
			if _, eligible := visitedDirIDs[video.parentID]; !eligible {
				continue
			}
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO drive_scan_misses (drive_id, file_id, consecutive_misses, last_missing_at)
VALUES (?, ?, 1, ?)
ON CONFLICT(drive_id, file_id) DO UPDATE SET
  consecutive_misses = drive_scan_misses.consecutive_misses + 1,
  last_missing_at = excluded.last_missing_at`, driveID, fileID, now); err != nil {
			return nil, err
		}
		var misses int
		if err := tx.QueryRowContext(ctx,
			`SELECT consecutive_misses FROM drive_scan_misses WHERE drive_id = ? AND file_id = ?`,
			driveID, fileID).Scan(&misses); err != nil {
			return nil, err
		}
		if misses >= threshold {
			confirmed[fileID] = struct{}{}
		}
	}

	// Keep the auxiliary table bounded when videos are removed through another
	// lifecycle (admin delete, dedupe, crawler migration, and so on).
	if _, err := tx.ExecContext(ctx, `
DELETE FROM drive_scan_misses
WHERE drive_id = ?
  AND NOT EXISTS (
    SELECT 1 FROM videos
    WHERE videos.drive_id = drive_scan_misses.drive_id
      AND videos.file_id = drive_scan_misses.file_id
  )`, driveID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return confirmed, nil
}
