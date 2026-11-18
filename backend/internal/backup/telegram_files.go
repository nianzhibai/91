package backup

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives/telegramstorage"
)

// Backups export TG library videos as portable uploads. Restore therefore never
// writes into active Telegram storage or depends on its original host path.
func normalizeTelegramSnapshot(ctx context.Context, tx *sql.Tx, state *snapshotSelectionState) error {
	// telegram_local_files is read here, before secret rows are cleared.
	state.TelegramUploadFiles = make(map[string]catalog.TelegramLocalFile)
	if state.Selection.UploadStorage {
		rows, err := tx.QueryContext(ctx, `SELECT v.file_id,COALESCE(f.job_id,''),COALESCE(f.directory,'') FROM (
			SELECT file_id FROM videos WHERE drive_id='telegram-local'
			UNION SELECT file_id FROM merged_videos WHERE drive_id='telegram-local' AND source_state='recorded'
			AND file_id IN (SELECT file_id FROM telegram_local_files)
		) v LEFT JOIN telegram_local_files f ON f.file_id=v.file_id`)
		if err != nil {
			return err
		}
		var files []catalog.TelegramLocalFile
		for rows.Next() {
			var v catalog.TelegramLocalFile
			if err := rows.Scan(&v.FileID, &v.JobID, &v.Directory); err != nil {
				rows.Close()
				return err
			}
			files = append(files, v)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, v := range files {
			if _, err := telegramstorage.RelativePath(v); err != nil || v.JobID == "" {
				return fmt.Errorf("backup: TG video %s has unavailable storage", v.FileID)
			}
			// Generated TG IDs are opaque basenames, so this name cannot reveal tokens.
			target := "telegram-" + v.FileID
			if filepath.Base(target) != target {
				return fmt.Errorf("backup: invalid TG video identity")
			}
			var conflicts int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM videos WHERE drive_id='local-upload' AND file_id=?`, target).Scan(&conflicts); err != nil {
				return err
			}
			if conflicts != 0 {
				return fmt.Errorf("backup: TG video filename conflicts with upload storage")
			}
			state.TelegramUploadFiles[target] = v
			if _, err := tx.ExecContext(ctx, `UPDATE videos SET drive_id='local-upload',file_id=? WHERE drive_id='telegram-local' AND file_id=?`, target, v.FileID); err != nil {
				return err
			}
		}
	}
	// Tombstones carry no exported video bytes, as with ordinary upload storage.
	// Preserve their identity without retaining a reference to the source host.
	// The host-specific Telegram tables are cleared with the other secret rows.
	if _, err := tx.ExecContext(ctx, `UPDATE merged_videos SET drive_id='local-upload',file_id='telegram-'||file_id WHERE drive_id='telegram-local'`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE deleted_videos SET drive_id='local-upload',file_id='telegram-'||file_id,restore_payload=CASE WHEN json_valid(restore_payload) THEN json_set(restore_payload,'$.video.driveId','local-upload','$.video.fileId','telegram-'||file_id) ELSE restore_payload END WHERE drive_id='telegram-local'`)
	return err
}
