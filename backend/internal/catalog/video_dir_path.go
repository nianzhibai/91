package catalog

import "context"

// videos.dir_path is the "/"-joined chain of directories between the drive's
// scan root and the video, stored so keyword search can match folder names
// without decoding JSON per row. The scan root itself is excluded: every video
// on the drive lives under it, so its name says nothing about any one video and
// would make a search for it match the whole library.
//
// Directory names reach videos through scans, patches, drive migrations and
// backup restores, so SQLite triggers derive the column on every write instead
// of each writer maintaining it. Scanned rows carry ancestor_dir_names, whose
// first entry is the scan root. Rows without that chain (scanned before it was
// recorded, or Telegram library files) fall back to dir_name unless the video
// sits directly in the drive root, where dir_name names the root.
func videoDirPathSQL(row string) string {
	chain := `CASE WHEN json_valid(` + row + `.ancestor_dir_names) THEN ` + row + `.ancestor_dir_names ELSE '[]' END`
	return `CASE
	WHEN json_array_length(` + chain + `) > 0 THEN COALESCE((
		SELECT group_concat(value, '/' ORDER BY key)
		  FROM json_each(` + chain + `)
		 WHERE key > 0 AND type = 'text' AND value != ''
	), '')
	WHEN ` + row + `.parent_id IN (SELECT root_id FROM drives WHERE drives.id = ` + row + `.drive_id)
	  OR (` + row + `.drive_id = '` + TelegramLocalDriveID + `' AND ` + row + `.parent_id = '` + TelegramLibraryRootID + `') THEN ''
	ELSE COALESCE(` + row + `.dir_name, '')
END`
}

// videoPathSQL is the searchable full path: directory chain plus file name.
const videoPathSQL = `CASE WHEN videos.dir_path = '' THEN COALESCE(videos.file_name, '')
	ELSE videos.dir_path || '/' || COALESCE(videos.file_name, '') END`

// addVideoDirPathColumn runs before any videos table rebuild, which copies
// dir_path along with the other current columns.
func (c *Catalog) addVideoDirPathColumn(ctx context.Context) error {
	added, err := c.addColumnIfMissingReportNew(ctx, "videos", "dir_path", "TEXT NOT NULL DEFAULT ''")
	if err != nil || !added {
		return err
	}
	_, err = c.db.ExecContext(ctx, `UPDATE videos SET dir_path = `+videoDirPathSQL("videos"))
	return err
}

// ensureVideoDirPathTriggers recreates the triggers on every open so they
// always carry the current definition; a videos table rebuild also drops them.
func (c *Catalog) ensureVideoDirPathTriggers(ctx context.Context) error {
	sync := `UPDATE videos SET dir_path = ` + videoDirPathSQL("NEW") + ` WHERE id = NEW.id;`
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`DROP TRIGGER IF EXISTS sync_video_dir_path_on_insert`,
		`DROP TRIGGER IF EXISTS sync_video_dir_path_on_update`,
		`CREATE TRIGGER sync_video_dir_path_on_insert AFTER INSERT ON videos BEGIN ` + sync + ` END`,
		`CREATE TRIGGER sync_video_dir_path_on_update AFTER UPDATE OF ancestor_dir_names, dir_name, parent_id, drive_id ON videos BEGIN ` + sync + ` END`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}
