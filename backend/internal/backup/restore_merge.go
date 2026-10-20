package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/video-site/backend/internal/catalog"
)

// mergeSelectiveRestoreDatabase overlays a filtered backup catalog on top of
// the current catalog. The source snapshot has already had its paths and
// credentials rewritten, so this function only deals with ownership and row
// identity. The target snapshot is supplied by the caller and is never the
// live database.
func mergeSelectiveRestoreDatabase(
	ctx context.Context,
	targetPath string,
	sourcePath string,
	selection BackupSelection,
) error {
	database, err := sql.Open("sqlite", targetPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("backup: open merge target database: %w", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	if err := catalog.MigrateImportDatabase(ctx, database); err != nil {
		return err
	}
	if _, err := database.ExecContext(ctx, `ATTACH DATABASE ? AS backup_source`, sourcePath); err != nil {
		return fmt.Errorf("backup: attach filtered backup database: %w", err)
	}
	detached := false
	defer func() {
		if !detached {
			_, _ = database.ExecContext(context.Background(), `DETACH DATABASE backup_source`)
		}
	}()

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("backup: begin selective restore merge: %w", err)
	}
	rollback := func() { _ = tx.Rollback() }

	replacedDriveIDs, mergedDriveIDs, err := restoreTargetDriveSets(ctx, tx, selection)
	if err != nil {
		rollback()
		return err
	}
	if selection.UploadStorage {
		mergedDriveIDs["local-upload"] = struct{}{}
		mergedDriveIDs[catalog.TelegramLocalDriveID] = struct{}{}
	}
	if err := createTextSet(ctx, tx, "restore_replaced_drives", replacedDriveIDs); err != nil {
		rollback()
		return err
	}
	if err := createTextSet(ctx, tx, "restore_merged_drives", mergedDriveIDs); err != nil {
		rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE restore_replaced_videos (id TEXT PRIMARY KEY)`); err != nil {
		rollback()
		return err
	}
	videoSelection := `drive_id IN (SELECT id FROM restore_replaced_drives)`
	driveSelection := `drive_id IN (SELECT id FROM restore_replaced_drives)`
	driveIDSelection := `id IN (SELECT id FROM restore_replaced_drives)`
	if selection.AllResources() {
		// A complete resource restore still replaces cloud and crawler resources,
		// including orphan rows whose drive metadata disappeared. Upload storage is
		// merged, while existing local storage stays target-owned; restored local
		// content already has a newly generated drive identity.
		videoSelection = `drive_id NOT IN (SELECT id FROM restore_merged_drives)`
		driveSelection = `drive_id NOT IN (SELECT id FROM restore_merged_drives)`
		driveIDSelection = `id NOT IN (SELECT id FROM restore_merged_drives)`
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO restore_replaced_videos (id)
		SELECT id FROM main.videos WHERE `+videoSelection); err != nil {
		rollback()
		return err
	}

	clearStatements := []string{
		`DELETE FROM main.video_reaction_visits WHERE video_id IN (SELECT id FROM restore_replaced_videos)`,
		`DELETE FROM main.video_shares WHERE video_id IN (SELECT id FROM restore_replaced_videos)`,
		`DELETE FROM main.video_tags WHERE video_id IN (SELECT id FROM restore_replaced_videos)`,
		`DELETE FROM main.videos WHERE id IN (SELECT id FROM restore_replaced_videos)`,
		`DELETE FROM main.scans WHERE ` + driveSelection,
		`DELETE FROM main.deleted_videos WHERE ` + driveSelection,
		`DELETE FROM main.crawler_seen_sources WHERE ` + driveSelection,
		`DELETE FROM main.crawler_discoveries WHERE ` + driveSelection,
		`DELETE FROM main.crawler_tasks WHERE ` + driveSelection,
		`DELETE FROM main.crawler_upload_tasks WHERE ` + driveSelection,
		`DELETE FROM main.crawler_upload_results WHERE ` + driveSelection,

		`DELETE FROM main.drives WHERE ` + driveIDSelection,
	}
	for _, statement := range clearStatements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			rollback()
			return fmt.Errorf("backup: clear selected target rows: %w", err)
		}
	}
	if err := validateRestoreDriveIdentityConflicts(ctx, tx); err != nil {
		rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE restore_imported_videos (id TEXT PRIMARY KEY)`); err != nil {
		rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO restore_imported_videos (id)
		SELECT source.id
		  FROM backup_source.videos AS source
		 WHERE NOT EXISTS (SELECT 1 FROM main.videos AS target WHERE target.id = source.id)`); err != nil {
		rollback()
		return fmt.Errorf("backup: identify imported videos: %w", err)
	}

	if err := copyCommonTableRows(ctx, tx, "drives", nil); err != nil {
		rollback()
		return err
	}
	if err := copyCommonTableRows(ctx, tx, "videos", nil); err != nil {
		rollback()
		return err
	}
	if err := mergeVideoReactionVisits(ctx, tx); err != nil {
		rollback()
		return err
	}
	for _, table := range []string{"scans", "deleted_videos", "crawler_seen_sources", "crawler_discoveries", "crawler_tasks", "crawler_upload_tasks", "crawler_upload_results"} {
		if err := copyCommonTableRows(ctx, tx, table, nil); err != nil {
			rollback()
			return err
		}
	}
	if selection.AllResources() {
		if err := mergeDuplicateRecords(ctx, tx); err != nil {
			rollback()
			return err
		}
	}
	if err := mergeTags(ctx, tx); err != nil {
		rollback()
		return err
	}
	if err := mergeVideoTags(ctx, tx); err != nil {
		rollback()
		return err
	}

	if selection.UserInfo {
		if err := mergeUsersByUsername(ctx, tx); err != nil {
			rollback()
			return err
		}
		if err := mergeVideoFavorites(ctx, tx); err != nil {
			rollback()
			return err
		}
	}
	if selection.UploadStorage {
		if err := mergeRemoteUploadJobs(ctx, tx); err != nil {
			rollback()
			return err
		}
	}

	// Sessions and shares are runtime state and are never imported.
	for _, statement := range []string{
		`DELETE FROM main.admin_sessions`,
		`DELETE FROM main.telegram_receipts`,
		`DELETE FROM main.telegram_media_group_updates`,
		`UPDATE main.telegram_connections SET needs_reconnect=1`,
		`INSERT INTO main.telegram_connections(bot_id,needs_reconnect) VALUES(0,1) ON CONFLICT(bot_id) DO UPDATE SET needs_reconnect=1`,
		`DELETE FROM main.video_shares`,
		`DELETE FROM main.shorts_feed_sessions`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("backup: commit selective restore merge: %w", err)
	}
	if _, err := database.ExecContext(ctx, `DETACH DATABASE backup_source`); err != nil {
		return fmt.Errorf("backup: detach filtered backup database: %w", err)
	}
	detached = true
	return verifySQLite(targetPath)
}

func validateRestoreDriveIdentityConflicts(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT source.id, source.kind, target.kind
		  FROM backup_source.drives AS source
		  JOIN main.drives AS target ON target.id = source.id
		 ORDER BY source.id`)
	if err != nil {
		return fmt.Errorf("backup: inspect restore drive identity conflicts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, sourceKind, targetKind string
		if err := rows.Scan(&id, &sourceKind, &targetKind); err != nil {
			return err
		}
		sourceKind = strings.ToLower(strings.TrimSpace(sourceKind))
		targetKind = strings.ToLower(strings.TrimSpace(targetKind))
		if sourceKind == targetKind && driveUsesMergeRestore(id, sourceKind) {
			continue
		}
		return fmt.Errorf(
			"backup: drive id %q conflicts between restored kind %q and retained target kind %q",
			id,
			sourceKind,
			targetKind,
		)
	}
	return rows.Err()
}

func timeNowMillis() int64 {
	return time.Now().UnixMilli()
}

func restoreTargetDriveSets(
	ctx context.Context,
	tx *sql.Tx,
	selection BackupSelection,
) (map[string]struct{}, map[string]struct{}, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, kind FROM main.drives`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	replaced := make(map[string]struct{})
	merged := make(map[string]struct{})
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return nil, nil, err
		}
		if !driveKindSelected(kind, selection) {
			continue
		}
		if driveUsesMergeRestore(id, kind) {
			merged[id] = struct{}{}
		} else {
			replaced[id] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return replaced, merged, nil
}

func driveUsesMergeRestore(id, kind string) bool {
	kind = strings.ToLower(strings.TrimSpace(kind))
	return strings.EqualFold(strings.TrimSpace(id), "local-upload") ||
		kind == "local-upload" || kind == "localstorage"
}

func createTextSet(ctx context.Context, tx *sql.Tx, name string, values map[string]struct{}) error {
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE `+quoteIdentifier(name)+` (id TEXT PRIMARY KEY)`); err != nil {
		return err
	}
	keys := make([]string, 0, len(values))
	for value := range values {
		keys = append(keys, value)
	}
	sort.Strings(keys)
	for _, value := range keys {
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+quoteIdentifier(name)+` (id) VALUES (?)`, value); err != nil {
			return err
		}
	}
	return nil
}

func copyCommonTableRows(
	ctx context.Context,
	tx *sql.Tx,
	table string,
	exclude map[string]struct{},
) error {
	targetColumns, err := tableColumnNames(ctx, tx, "main", table)
	if err != nil {
		return err
	}
	sourceColumns, err := tableColumnNames(ctx, tx, "backup_source", table)
	if err != nil {
		return err
	}
	sourceSet := make(map[string]struct{}, len(sourceColumns))
	for _, column := range sourceColumns {
		sourceSet[column] = struct{}{}
	}
	columns := make([]string, 0, len(targetColumns))
	for _, column := range targetColumns {
		if _, skip := exclude[column]; skip {
			continue
		}
		if _, exists := sourceSet[column]; exists {
			columns = append(columns, column)
		}
	}
	if len(columns) == 0 {
		return nil
	}
	quoted := quoteIdentifiers(columns)
	statement := "INSERT OR IGNORE INTO main." + quoteIdentifier(table) + " (" + strings.Join(quoted, ", ") + ") SELECT " + strings.Join(quoted, ", ") + " FROM backup_source." + quoteIdentifier(table)
	if _, err := tx.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("backup: merge %s: %w", table, err)
	}
	return nil
}

// Historical IDs and snapshots describe the environment in which a decision
// happened, so they are not rewritten during restore or cleared with live rows.
// Merging the same archive twice must not double its observation counters.
func mergeDuplicateRecords(ctx context.Context, tx *sql.Tx) error {
	columns, err := tableColumnNames(ctx, tx, "backup_source", "duplicate_records")
	if err != nil || len(columns) == 0 {
		return err
	}
	if err := copyCommonTableRows(ctx, tx, "duplicate_records", nil); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
UPDATE main.duplicate_records AS target
   SET first_seen_at = MIN(first_seen_at, (SELECT first_seen_at FROM backup_source.duplicate_records WHERE record_key = target.record_key)),
       last_seen_at = MAX(last_seen_at, (SELECT last_seen_at FROM backup_source.duplicate_records WHERE record_key = target.record_key)),
       occurrences = MAX(occurrences, (SELECT occurrences FROM backup_source.duplicate_records WHERE record_key = target.record_key))
 WHERE record_key IN (SELECT record_key FROM backup_source.duplicate_records)`)
	return err
}

func mergeRemoteUploadJobs(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE restore_imported_upload_jobs (id TEXT PRIMARY KEY)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO restore_imported_upload_jobs (id)
		SELECT source.id
		  FROM backup_source.remote_upload_jobs AS source
		 WHERE NOT EXISTS (
			SELECT 1 FROM main.remote_upload_jobs AS target WHERE target.id = source.id
		)`); err != nil {
		return err
	}
	if err := copyCommonTableRows(ctx, tx, "remote_upload_jobs", map[string]struct{}{"sequence": {}}); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO main.telegram_files(bot_id,file_unique_id,file_id,job_id,video_id) SELECT bot_id,file_unique_id,'',job_id,video_id FROM backup_source.telegram_files WHERE video_id!='' AND video_id IN (SELECT id FROM main.videos)`); err != nil {
		return err
	}
	now := timeNowMillis()
	if _, err := tx.ExecContext(ctx, `UPDATE main.remote_upload_jobs
		 SET state = 'canceled', source_url = '', cancel_requested = 1,
		     temp_file = '', final_file = '', error_message = '恢复时已取消',
		     updated_at = ?, finished_at = ?
		 WHERE id IN (SELECT id FROM restore_imported_upload_jobs)
		   AND state IN ('queued', 'downloading', 'validating', 'saving')`, now, now); err != nil {
		return err
	}
	return nil
}

// mergeUsersByUsername imports only source accounts that do not already exist
// in the target. The primary key is deliberately omitted so every imported
// account receives a target-local ID; sessions are cleared separately.
func mergeUsersByUsername(ctx context.Context, tx *sql.Tx) error {
	targetColumns, err := tableColumnNames(ctx, tx, "main", "users")
	if err != nil {
		return err
	}
	sourceColumns, err := tableColumnNames(ctx, tx, "backup_source", "users")
	if err != nil {
		return err
	}
	sourceSet := make(map[string]struct{}, len(sourceColumns))
	for _, column := range sourceColumns {
		sourceSet[column] = struct{}{}
	}
	if _, exists := sourceSet["username"]; !exists {
		return errors.New("backup: source users table has no username column")
	}
	columns := make([]string, 0, len(targetColumns))
	for _, column := range targetColumns {
		if column == "id" {
			continue
		}
		if _, exists := sourceSet[column]; exists {
			columns = append(columns, column)
		}
	}
	if len(columns) == 0 {
		return errors.New("backup: users table has no compatible columns")
	}
	quoted := quoteIdentifiers(columns)
	statement := `INSERT INTO main.users (` + strings.Join(quoted, ", ") + `)
		SELECT ` + strings.Join(quoted, ", ") + `
		  FROM backup_source.users AS source
		 WHERE NOT EXISTS (
			SELECT 1 FROM main.users AS target
			 WHERE target.username = source.username COLLATE NOCASE
		)`
	if _, err := tx.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("backup: merge users by username: %w", err)
	}
	return nil
}

func tableColumnNames(ctx context.Context, tx *sql.Tx, schema, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA "+schema+`.table_info('`+strings.ReplaceAll(table, "'", "''")+`')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return columns, nil
}

func mergeTags(ctx context.Context, tx *sql.Tx) error {
	return copyCommonTableRows(ctx, tx, "tags", map[string]struct{}{"id": {}})
}

func mergeVideoTags(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO main.video_tags (video_id, tag_id, source, evidence, created_at)
		SELECT source.video_id, target.id, source.source, source.evidence, source.created_at
		  FROM backup_source.video_tags AS source
		  JOIN backup_source.tags AS source_tag ON source_tag.id = source.tag_id
		  JOIN main.tags AS target ON target.label = source_tag.label COLLATE NOCASE`)
	if err != nil {
		return fmt.Errorf("backup: merge video tags: %w", err)
	}
	return nil
}

func mergeVideoReactionVisits(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO main.video_reaction_visits (
			video_id, visit_id, reaction, created_at, updated_at
		)
		SELECT source.video_id, source.visit_id, source.reaction,
		       source.created_at, source.updated_at
		  FROM backup_source.video_reaction_visits AS source
		  JOIN restore_imported_videos AS imported ON imported.id = source.video_id`)
	if err != nil {
		return fmt.Errorf("backup: merge video reaction visits: %w", err)
	}
	return nil
}

// Favorites are account-scoped, so the archive's user ids are resolved by
// username: the same person can hold a different id in the target database.
// Archives created before favorites existed have no such table, and
// tableColumnNames reports an empty column set for a missing table.
func mergeVideoFavorites(ctx context.Context, tx *sql.Tx) error {
	sourceColumns, err := tableColumnNames(ctx, tx, "backup_source", "video_favorites")
	if err != nil || len(sourceColumns) == 0 {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO main.video_favorites (user_id, video_id, created_at)
		SELECT target_user.id, source.video_id, source.created_at
		  FROM backup_source.video_favorites AS source
		  JOIN backup_source.users AS source_user ON source_user.id = source.user_id
		  JOIN main.users AS target_user
		    ON target_user.username = source_user.username COLLATE NOCASE
		  JOIN restore_imported_videos AS imported ON imported.id = source.video_id`); err != nil {
		return fmt.Errorf("backup: merge video favorites: %w", err)
	}
	return nil
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func quoteIdentifiers(values []string) []string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = quoteIdentifier(value)
	}
	return quoted
}
