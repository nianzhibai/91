package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/driveevents"
)

// TelegramUploadCleanup owns a source left behind after a committed transfer.
// FileIdentity identifies the original filesystem object, not its reusable name.
// These host-local tasks are excluded from portable backups.
type TelegramUploadCleanup struct {
	VideoID       string
	SourceDriveID string
	SourceFileID  string
	Size          int64
	ModTimeNS     int64
	FileIdentity  string
	Attempts      int
	LastError     string
}

// MigrateTelegramVideoToDrive commits the new source and its cleanup obligation
// together, so a crash cannot leave an untracked local copy.
func (c *Catalog) MigrateTelegramVideoToDrive(ctx context.Context, videoID string, target VideoDriveMigration, cleanup TelegramUploadCleanup) (resultErr error) {
	defer c.notifyDriveWrite(&resultErr, "", driveevents.MediaChanged)
	if cleanup.VideoID != videoID || cleanup.SourceDriveID != target.SourceDriveID || cleanup.SourceFileID != target.SourceFileID ||
		(cleanup.SourceDriveID != "local-upload" && cleanup.SourceDriveID != TelegramLocalDriveID) ||
		cleanup.SourceFileID == "" || filepath.Base(cleanup.SourceFileID) != cleanup.SourceFileID || strings.ContainsAny(cleanup.SourceFileID, "/\\\x00") ||
		cleanup.Size <= 0 || cleanup.FileIdentity == "" {
		return errors.New("catalog: invalid Telegram upload cleanup source")
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := migrateVideoToDrive(ctx, tx, videoID, target); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO telegram_upload_cleanups
 (video_id,source_drive_id,source_file_id,size_bytes,mod_time_ns,file_identity,created_at,updated_at)
 VALUES(?,?,?,?,?,?,?,?)`, videoID, cleanup.SourceDriveID, cleanup.SourceFileID, cleanup.Size, cleanup.ModTimeNS, cleanup.FileIdentity, now, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (c *Catalog) ListTelegramUploadCleanups(ctx context.Context, afterID string, limit int) ([]TelegramUploadCleanup, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := c.db.QueryContext(ctx, `SELECT video_id,source_drive_id,source_file_id,size_bytes,mod_time_ns,file_identity,attempts,last_error
 FROM telegram_upload_cleanups WHERE video_id>? ORDER BY video_id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []TelegramUploadCleanup
	for rows.Next() {
		var job TelegramUploadCleanup
		if err := rows.Scan(&job.VideoID, &job.SourceDriveID, &job.SourceFileID, &job.Size, &job.ModTimeNS, &job.FileIdentity, &job.Attempts, &job.LastError); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (c *Catalog) TelegramUploadSourceReferenced(ctx context.Context, driveID, fileID string) (bool, error) {
	var referenced bool
	err := c.db.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM videos WHERE drive_id=? AND file_id=?
 UNION ALL SELECT 1 FROM deleted_videos WHERE drive_id=? AND file_id=?)`, driveID, fileID, driveID, fileID).Scan(&referenced)
	return referenced, err
}

func (c *Catalog) CompleteTelegramUploadCleanup(ctx context.Context, videoID string) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM telegram_upload_cleanups WHERE video_id=?`, videoID)
	return err
}

func (c *Catalog) FailTelegramUploadCleanup(ctx context.Context, videoID, message string) error {
	_, err := c.db.ExecContext(ctx, `UPDATE telegram_upload_cleanups SET attempts=attempts+1,last_error=?,updated_at=? WHERE video_id=?`, message, time.Now().UnixMilli(), videoID)
	return err
}
