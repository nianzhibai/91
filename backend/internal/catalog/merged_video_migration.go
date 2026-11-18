package catalog

import (
	"context"
	"database/sql"
	"errors"
)

const mergedVideoSchema = `
-- A public ID retired by a merge keeps pointing to the survivor even after
-- its provider file is removed or replaced.
CREATE TABLE IF NOT EXISTS merged_videos (
    video_id           TEXT PRIMARY KEY,
    canonical_video_id TEXT NOT NULL,
    drive_id           TEXT NOT NULL DEFAULT '',
    file_id            TEXT NOT NULL DEFAULT '',
    parent_id          TEXT NOT NULL DEFAULT '',
    file_name          TEXT NOT NULL DEFAULT '',
    size_bytes         INTEGER NOT NULL DEFAULT 0,
    content_hash       TEXT NOT NULL DEFAULT '',
    sampled_sha256     TEXT NOT NULL DEFAULT '',
    source_mod_time    TEXT NOT NULL DEFAULT '',
    source_state       TEXT NOT NULL DEFAULT 'recorded' CHECK (source_state IN ('recorded','gone','replaced')),
    merge_reason       TEXT NOT NULL DEFAULT '',
    merged_at          INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_merged_videos_canonical ON merged_videos(canonical_video_id);
CREATE INDEX IF NOT EXISTS idx_merged_videos_file ON merged_videos(drive_id,file_id);
CREATE INDEX IF NOT EXISTS idx_merged_videos_recent ON merged_videos(merged_at DESC,video_id);
`

// MigrateMergedVideos upgrades the live catalog and staged backup databases.
// An archive can predate both audit history and the restore payload.
func MigrateMergedVideos(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, mergedVideoSchema); err != nil {
		return err
	}
	var hasReason bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pragma_table_info('deleted_videos') WHERE name='reason')").Scan(&hasReason); err != nil {
		return err
	}
	if !hasReason {
		return tx.Commit()
	}
	var hasLegacy bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM deleted_videos WHERE reason='duplicate')").Scan(&hasLegacy); err != nil {
		return err
	}
	if !hasLegacy {
		return tx.Commit()
	}
	// Supply defaults for optional columns from older archive schemas.
	columns := map[string]string{"parent_id": "''", "canonical_video_id": "''", "source_deleted": "0", "restore_payload": "''"}
	rows, err := tx.QueryContext(ctx, "SELECT name FROM pragma_table_info('deleted_videos')")
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		if _, ok := columns[name]; ok {
			columns[name] = "d." + name
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var hasHistory bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='duplicate_records')").Scan(&hasHistory); err != nil {
		return err
	}
	reason := "''"
	if hasHistory {
		reason = "COALESCE((SELECT reason FROM duplicate_records WHERE video_id=d.id AND outcome='merged' ORDER BY last_seen_at DESC,record_key LIMIT 1),'')"
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO merged_videos(video_id, canonical_video_id, drive_id, file_id, parent_id,
		file_name, size_bytes, content_hash, sampled_sha256, source_mod_time, source_state, merge_reason, merged_at)
		SELECT id,COALESCE(`+columns["canonical_video_id"]+`,''),COALESCE(drive_id,''),COALESCE(file_id,''),COALESCE(`+columns["parent_id"]+`,''),COALESCE(file_name,''),COALESCE(size_bytes,0),COALESCE(content_hash,''),
		CASE WHEN json_valid(`+columns["restore_payload"]+`) THEN COALESCE(json_extract(`+columns["restore_payload"]+`,'$.video.sampledSha256'),'') ELSE '' END,
		CASE WHEN json_valid(`+columns["restore_payload"]+`) THEN COALESCE(json_extract(`+columns["restore_payload"]+`,'$.video.sourceModTime'),'') ELSE '' END,
		CASE WHEN `+columns["source_deleted"]+`!=0 THEN 'gone' ELSE 'recorded' END,`+reason+`,
		deleted_at FROM deleted_videos d WHERE reason='duplicate' ON CONFLICT(video_id) DO NOTHING`)
	if err != nil {
		return err
	}
	var conflict bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deleted_videos d JOIN merged_videos m ON m.video_id=d.id
		WHERE d.reason='duplicate' AND (m.drive_id!=COALESCE(d.drive_id,'') OR m.file_id!=COALESCE(d.file_id,'')
		OR m.canonical_video_id!=COALESCE(`+columns["canonical_video_id"]+`,'')))`).Scan(&conflict); err != nil {
		return err
	}
	if conflict {
		return errors.New("legacy duplicate conflicts with an existing merged video")
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM deleted_videos WHERE reason='duplicate'"); err != nil {
		return err
	}
	return tx.Commit()
}

// A revived live ID owns its public link. When a live source is refreshed,
// also update the aliases that recorded earlier versions of the same file.
func (c *Catalog) ensureMergedVideoGuards(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx, `CREATE TRIGGER IF NOT EXISTS drop_merged_alias_on_revive AFTER INSERT ON videos
		BEGIN DELETE FROM merged_videos WHERE video_id=NEW.id; END;
		DELETE FROM merged_videos WHERE video_id IN (SELECT id FROM videos);
		CREATE TRIGGER IF NOT EXISTS mark_changed_merged_source
		AFTER UPDATE OF size_bytes, content_hash, sampled_sha256, source_mod_time ON videos
		WHEN OLD.size_bytes IS NOT NEW.size_bytes OR OLD.content_hash IS NOT NEW.content_hash
		  OR OLD.sampled_sha256 IS NOT NEW.sampled_sha256 OR OLD.source_mod_time IS NOT NEW.source_mod_time
		BEGIN
		  UPDATE merged_videos SET source_state='replaced' WHERE video_id IN (
		    SELECT m.video_id FROM merged_videos m JOIN videos observed ON observed.id=NEW.id
		    WHERE m.drive_id=observed.drive_id AND m.file_id=observed.file_id AND m.source_state='recorded'
		    AND `+mergedSourceChanged+`);
		END;`)
	return err
}
