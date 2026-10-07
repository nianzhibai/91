package backup

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// validateArchiveDatabaseScope makes the manifest a trustworthy restore
// contract. Hashes prove that bytes did not change; this check proves that the
// filtered catalog represented by those bytes actually matches the scope the
// administrator is being asked to restore.
func validateArchiveDatabaseScope(ctx context.Context, databasePath string, manifest Manifest) error {
	if manifest.Selection == nil {
		return fmt.Errorf("backup: selection is missing while validating database scope")
	}
	selection := *manifest.Selection
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(databasePath),
		RawQuery: "mode=ro&_pragma=busy_timeout(5000)",
	}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("backup: open SQLite snapshot for scope validation: %w", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	if err := validateTelegramArchiveScope(ctx, database, selection); err != nil {
		return err
	}

	drives := make(map[string]struct{})
	databaseLocalDrives := make(map[string]struct{})
	rows, err := database.QueryContext(ctx, `SELECT id, kind FROM drives`)
	if err != nil {
		return fmt.Errorf("backup: inspect drive scope: %w", err)
	}
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			_ = rows.Close()
			return err
		}
		if strings.TrimSpace(id) == "" || !driveKindSelected(kind, selection) {
			_ = rows.Close()
			return fmt.Errorf("backup: drive %q is outside the declared backup selection", id)
		}
		drives[id] = struct{}{}
		if strings.EqualFold(strings.TrimSpace(kind), "localstorage") {
			databaseLocalDrives[id] = struct{}{}
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}

	manifestLocalDrives := make(map[string]struct{}, len(manifest.LocalStorage))
	for _, root := range manifest.LocalStorage {
		manifestLocalDrives[root.DriveID] = struct{}{}
	}
	if len(databaseLocalDrives) != len(manifestLocalDrives) {
		return fmt.Errorf("backup: local storage database rows do not match manifest metadata")
	}
	for id := range databaseLocalDrives {
		if _, declared := manifestLocalDrives[id]; !declared {
			return fmt.Errorf("backup: local storage drive %q is not declared in manifest metadata", id)
		}
	}
	for id := range manifestLocalDrives {
		if _, exists := databaseLocalDrives[id]; !exists {
			return fmt.Errorf("backup: manifest local storage drive %q is missing from the database", id)
		}
	}

	resourceSelected := selection.CloudDrives || selection.CrawlerScripts ||
		selection.UploadStorage || selection.LocalStorage
	rows, err = database.QueryContext(ctx, `SELECT id, drive_id FROM videos`)
	if err != nil {
		return fmt.Errorf("backup: inspect video scope: %w", err)
	}
	for rows.Next() {
		var id, driveID string
		if err := rows.Scan(&id, &driveID); err != nil {
			_ = rows.Close()
			return err
		}
		if !resourceSelected || !archiveDriveReferenceAllowed(driveID, drives, selection) {
			_ = rows.Close()
			return fmt.Errorf("backup: video %q references drive %q outside the declared backup selection", id, driveID)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, table := range []string{"scans", "deleted_videos", "crawler_seen_sources"} {
		if err := validateArchiveDriveScopedTable(ctx, database, table, drives, selection); err != nil {
			return err
		}
	}
	// v3 tables are optional in older archives, but when present obey the
	// same drive selection as source history.
	for _, table := range []string{"crawler_discoveries", "crawler_tasks", "crawler_upload_tasks", "crawler_upload_results"} {
		var present int
		if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&present); err != nil {
			return err
		}
		if present != 0 {
			if err := validateArchiveDriveScopedTable(ctx, database, table, drives, selection); err != nil {
				return err
			}
		}
	}
	// Older archives predate this optional internal history table.
	if !selection.AllResources() {
		var present int
		if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='duplicate_records'`).Scan(&present); err != nil {
			return err
		}
		if present != 0 {
			if err := requireArchiveTableEmpty(ctx, database, "duplicate_records"); err != nil {
				return fmt.Errorf("backup: duplicate history requires all resources: %w", err)
			}
		}
	}
	if !selection.UserInfo {
		if err := requireArchiveTableEmpty(ctx, database, "users"); err != nil {
			return fmt.Errorf("backup: user rows are present although user information was not selected: %w", err)
		}
	}
	if !selection.UploadStorage {
		if err := requireArchiveTableEmpty(ctx, database, "remote_upload_jobs"); err != nil {
			return fmt.Errorf("backup: upload jobs are present although upload storage was not selected: %w", err)
		}
	} else {
		var invalidJobs int64
		if err := database.QueryRowContext(ctx, `
			SELECT COUNT(*)
			  FROM remote_upload_jobs AS jobs
			 WHERE COALESCE(jobs.completed_video_id, '') != ''
			   AND NOT EXISTS (SELECT 1 FROM videos WHERE videos.id = jobs.completed_video_id)`).Scan(&invalidJobs); err != nil {
			return err
		}
		if invalidJobs != 0 {
			return fmt.Errorf("backup: %d upload jobs reference videos outside the archive", invalidJobs)
		}
	}

	for _, table := range []string{
		"admin_sessions", "settings", "video_shares", "shorts_feed_sessions", "banned_login_ips",
	} {
		if err := requireArchiveTableEmpty(ctx, database, table); err != nil {
			return fmt.Errorf("backup: transient table %s must be empty: %w", table, err)
		}
	}

	// Tag definitions are global catalog data, including unassigned labels.
	// Only assignments must reference videos and tags present in this archive.
	for _, relationship := range []struct {
		name  string
		query string
	}{
		{
			name: "video reaction visits",
			query: `SELECT COUNT(*) FROM video_reaction_visits AS source
				WHERE NOT EXISTS (SELECT 1 FROM videos WHERE videos.id = source.video_id)`,
		},
		{
			name: "video tag assignments",
			query: `SELECT COUNT(*) FROM video_tags AS source
				WHERE NOT EXISTS (SELECT 1 FROM videos WHERE videos.id = source.video_id)
				   OR NOT EXISTS (SELECT 1 FROM tags WHERE tags.id = source.tag_id)`,
		},
	} {
		var invalid int64
		if err := database.QueryRowContext(ctx, relationship.query).Scan(&invalid); err != nil {
			return fmt.Errorf("backup: inspect %s: %w", relationship.name, err)
		}
		if invalid != 0 {
			return fmt.Errorf("backup: %d invalid %s rows fall outside the declared archive scope", invalid, relationship.name)
		}
	}

	return nil
}

func archiveDriveReferenceAllowed(
	driveID string,
	drives map[string]struct{},
	selection BackupSelection,
) bool {
	if _, exists := drives[driveID]; exists {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(driveID), "local-upload") {
		return selection.UploadStorage
	}
	// A full resource snapshot deliberately preserves legacy orphan rows whose
	// drive metadata disappeared before the backup was created.
	return selection.AllResources()
}

func validateArchiveDriveScopedTable(
	ctx context.Context,
	database *sql.DB,
	table string,
	drives map[string]struct{},
	selection BackupSelection,
) error {
	rows, err := database.QueryContext(ctx, `SELECT DISTINCT drive_id FROM `+quoteIdentifier(table))
	if err != nil {
		return fmt.Errorf("backup: inspect %s scope: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var driveID string
		if err := rows.Scan(&driveID); err != nil {
			return err
		}
		if !archiveDriveReferenceAllowed(driveID, drives, selection) {
			return fmt.Errorf("backup: %s references drive %q outside the declared backup selection", table, driveID)
		}
	}
	return rows.Err()
}

func requireArchiveTableEmpty(ctx context.Context, database *sql.DB, table string) error {
	var count int64
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+quoteIdentifier(table)).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("contains %d rows", count)
	}
	return nil
}

// Telegram tables are optional in older archives. When present, they must not
// smuggle receiver sessions or downloadable source metadata into an archive.
func validateTelegramArchiveScope(ctx context.Context, db *sql.DB, selection BackupSelection) error {
	var payloadColumn int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('remote_upload_jobs') WHERE name='source_payload'`).Scan(&payloadColumn); err != nil {
		return err
	}
	if payloadColumn > 0 {
		var private int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_upload_jobs WHERE source_kind='telegram' AND source_payload!=''`).Scan(&private); err != nil {
			return err
		}
		if private > 0 {
			return fmt.Errorf("backup: Telegram source metadata must be empty")
		}
	}

	for _, table := range []string{"telegram_settings", "telegram_connections", "telegram_receipts", "telegram_media_group_updates", "telegram_files", "telegram_local_files", "telegram_upload_cleanups"} {
		var present int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&present); err != nil {
			return err
		}
		if present == 0 {
			continue
		}
		if table != "telegram_files" || !selection.UploadStorage {
			if err := requireArchiveTableEmpty(ctx, db, table); err != nil {
				return err
			}
			continue
		}
		var invalid int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM telegram_files WHERE file_id!='' OR video_id='' OR video_id NOT IN (SELECT id FROM videos) OR job_id NOT IN (SELECT id FROM remote_upload_jobs)`).Scan(&invalid); err != nil {
			return err
		}
		if invalid != 0 {
			return fmt.Errorf("backup: invalid Telegram file mappings")
		}
	}
	return nil
}
