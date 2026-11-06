package backup

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives/telegramstorage"
)

// Backups export TG library videos as portable uploads. Restore therefore never
// writes into active Telegram storage or depends on its original host path.
func normalizeTelegramSnapshot(ctx context.Context, tx *sql.Tx, state *snapshotSelectionState) error {
	state.TelegramUploadFiles = make(map[string]catalog.TelegramLocalFile)
	if state.Selection.UploadStorage {
		rows, err := tx.QueryContext(ctx, `SELECT v.id,v.file_id,COALESCE(f.job_id,''),COALESCE(f.directory,'') FROM videos v LEFT JOIN telegram_local_files f ON f.file_id=v.file_id WHERE v.drive_id='telegram-local'`)
		if err != nil {
			return err
		}
		type item struct {
			id   string
			file catalog.TelegramLocalFile
		}
		var files []item
		for rows.Next() {
			var v item
			if err := rows.Scan(&v.id, &v.file.FileID, &v.file.JobID, &v.file.Directory); err != nil {
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
			if _, err := telegramstorage.RelativePath(v.file); err != nil || v.file.JobID == "" {
				return fmt.Errorf("backup: TG video %s has unavailable storage", v.id)
			}
			// Generated TG IDs are opaque basenames, so this name cannot reveal tokens.
			target := "telegram-" + v.file.FileID
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
			state.TelegramUploadFiles[target] = v.file
			if _, err := tx.ExecContext(ctx, `UPDATE videos SET drive_id='local-upload',file_id=? WHERE id=?`, target, v.id); err != nil {
				return err
			}
		}
	}
	// Tombstones carry no exported video bytes, as with ordinary upload storage.
	// Preserve their identity without retaining a reference to the source host.
	for _, statement := range []string{
		`DELETE FROM telegram_upload_cleanups`,
		`UPDATE deleted_videos SET drive_id='local-upload',file_id='telegram-'||file_id,restore_payload=CASE WHEN json_valid(restore_payload) THEN json_set(restore_payload,'$.video.driveId','local-upload','$.video.fileId','telegram-'||file_id) ELSE restore_payload END WHERE drive_id='telegram-local'`,
		`DELETE FROM telegram_local_files`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}
