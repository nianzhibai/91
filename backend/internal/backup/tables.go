package backup

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// tableKind says how a catalog table takes part in backup and restore.
type tableKind int

const (
	// driveRows are archived rows owned by the drive in ownerColumn. They
	// follow the drive selection and replace the target's rows on restore.
	driveRows tableKind = iota + 1
	// videoRows are archived rows owned by the video in video_id. They follow
	// their video and are cleared with the target videos a restore replaces.
	videoRows
	// catalogRows are archived rows without an owning drive or video, such as
	// tag definitions and user accounts.
	catalogRows
	// driveState is deployment-local progress owned by a drive. It is never
	// archived; the target's rows for replaced drives are cleared on restore.
	driveState
	// secretRows hold credentials, sessions, tokens or host-specific Telegram
	// state. They are never archived and archives must not contain them.
	secretRows
	// localRows are deployment-local jobs that are neither archived nor
	// touched by a restore.
	localRows
	// derivedRows are maintained by triggers from other tables.
	derivedRows
)

// catalogTable is the backup policy of one catalog table. Every table of the
// catalog schema must have exactly one entry; creating a backup fails for an
// unregistered table so new state is never exported or dropped by accident.
type catalogTable struct {
	name string
	kind tableKind
	// ownerColumn names the owning drive of driveRows and driveState tables.
	ownerColumn string
	// includedWhen limits an archived table to the selections that contain it.
	// Nil includes the table whenever its owner is selected.
	includedWhen func(BackupSelection) bool
	// sanitize removes or blanks selected rows the archive must not carry.
	sanitize []string
	// invariants are checked on every archive whose selection includes the
	// table; each counts violating rows, which must be zero.
	invariants []tableInvariant
	// videoRefs are columns that follow a video when a restore assigns it a
	// new identity. A videoRows table's video_id is always remapped.
	videoRefs []string
	// merge imports archived rows into the restore target. Nil copies the
	// columns both schemas share, ignoring rows whose key already exists.
	merge func(context.Context, *sql.Tx) error
	// restoreReset runs against the restore target for secretRows tables.
	restoreReset []string
}

type tableInvariant struct {
	description string
	violations  string
	// column names a column the query needs. Archives created before the
	// column existed skip the check.
	column string
}

// catalogTables is ordered for restore: owners are merged before the rows
// that reference them.
var catalogTables = []catalogTable{
	{name: "drives", kind: driveRows, ownerColumn: "id"},
	{name: "videos", kind: driveRows, ownerColumn: "drive_id"},
	{name: "scans", kind: driveRows, ownerColumn: "drive_id"},
	{name: "deleted_videos", kind: driveRows, ownerColumn: "drive_id", videoRefs: []string{"canonical_video_id"}},
	{name: "crawler_seen_sources", kind: driveRows, ownerColumn: "drive_id", videoRefs: []string{"canonical_video_id"}},
	{name: "crawler_discoveries", kind: driveRows, ownerColumn: "drive_id"},
	{name: "crawler_tasks", kind: driveRows, ownerColumn: "drive_id"},
	{name: "crawler_upload_tasks", kind: driveRows, ownerColumn: "drive_id"},
	{name: "crawler_upload_results", kind: driveRows, ownerColumn: "drive_id"},

	// Visits are imported only for videos new to the target, so restoring the
	// same archive twice cannot count a visit twice.
	{name: "video_reaction_visits", kind: videoRows, merge: mergeVideoReactionVisits},
	// Tag definitions are global, including unassigned labels; assignments
	// are matched to target tags by label.
	{name: "tags", kind: catalogRows, merge: mergeTags},
	{
		name: "video_tags", kind: videoRows, merge: mergeVideoTags,
		invariants: []tableInvariant{{
			description: "video tag assignments",
			violations:  `SELECT COUNT(*) FROM video_tags WHERE tag_id NOT IN (SELECT id FROM tags)`,
		}},
	},
	// A decision can contain snapshots from several resource types, so the
	// history is archived only when every resource is.
	{name: "duplicate_records", kind: catalogRows, includedWhen: BackupSelection.AllResources, merge: mergeDuplicateRecords},
	{name: "users", kind: catalogRows, includedWhen: selectsUserInfo, merge: mergeUsersByUsername},
	{
		name: "remote_upload_jobs", kind: catalogRows, includedWhen: selectsUploadStorage,
		videoRefs: []string{"completed_video_id"},
		merge:     mergeRemoteUploadJobs,
		sanitize: []string{
			`DELETE FROM remote_upload_jobs WHERE completed_video_id != '' AND completed_video_id NOT IN (SELECT id FROM videos)`,
			`UPDATE remote_upload_jobs SET source_payload = '' WHERE source_kind = 'telegram'`,
		},
		invariants: []tableInvariant{
			{
				description: "upload jobs",
				violations:  `SELECT COUNT(*) FROM remote_upload_jobs WHERE COALESCE(completed_video_id, '') != '' AND completed_video_id NOT IN (SELECT id FROM videos)`,
			},
			{
				description: "Telegram source metadata",
				violations:  `SELECT COUNT(*) FROM remote_upload_jobs WHERE source_kind = 'telegram' AND source_payload != ''`,
				column:      "source_payload",
			},
		},
	},
	{
		name: "telegram_files", kind: videoRows, includedWhen: selectsUploadStorage,
		merge:    mergeTelegramFiles,
		sanitize: []string{`UPDATE telegram_files SET file_id = ''`},
		invariants: []tableInvariant{{
			description: "Telegram file mappings",
			violations:  `SELECT COUNT(*) FROM telegram_files WHERE file_id != '' OR job_id NOT IN (SELECT id FROM remote_upload_jobs)`,
		}},
	},

	{name: "drive_scan_misses", kind: driveState, ownerColumn: "drive_id"},
	{name: "drive_skip_cleanup_legacy_dirs", kind: driveState, ownerColumn: "drive_id"},

	{name: "admin_sessions", kind: secretRows, restoreReset: []string{`DELETE FROM main.admin_sessions`}},
	{name: "banned_login_ips", kind: secretRows},
	{name: "login_failures", kind: secretRows},
	{name: "settings", kind: secretRows},
	{name: "video_shares", kind: secretRows, restoreReset: []string{`DELETE FROM main.video_shares`}},
	{name: "shorts_feed_sessions", kind: secretRows, restoreReset: []string{`DELETE FROM main.shorts_feed_sessions`}},
	{name: "telegram_settings", kind: secretRows},
	{
		name: "telegram_connections", kind: secretRows,
		restoreReset: []string{
			`UPDATE main.telegram_connections SET needs_reconnect = 1`,
			`INSERT INTO main.telegram_connections (bot_id, needs_reconnect) VALUES (0, 1) ON CONFLICT(bot_id) DO UPDATE SET needs_reconnect = 1`,
		},
	},
	{name: "telegram_receipts", kind: secretRows, restoreReset: []string{`DELETE FROM main.telegram_receipts`}},
	{name: "telegram_media_group_updates", kind: secretRows, restoreReset: []string{`DELETE FROM main.telegram_media_group_updates`}},
	{name: "telegram_local_files", kind: secretRows},
	{name: "telegram_upload_cleanups", kind: secretRows},

	{name: "duplicate_asset_cleanup_jobs", kind: localRows},
	{name: "video_dedup_representatives", kind: derivedRows},
}

func selectsUserInfo(s BackupSelection) bool { return s.UserInfo }

func (t catalogTable) archived() bool {
	return t.kind == driveRows || t.kind == videoRows || t.kind == catalogRows
}

func (t catalogTable) includedIn(selection BackupSelection) bool {
	return t.archived() && (t.includedWhen == nil || t.includedWhen(selection))
}

// queryer is satisfied by *sql.DB and *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// schemaTables returns the names of the user tables in a database schema.
func schemaTables(ctx context.Context, db queryer, schema string) (map[string]struct{}, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM `+quoteIdentifier(schema)+`.sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables[name] = struct{}{}
	}
	return tables, rows.Err()
}

// requireRegisteredTables rejects a schema containing a table without a
// backup policy.
func requireRegisteredTables(tables map[string]struct{}) error {
	registered := make(map[string]struct{}, len(catalogTables))
	for _, table := range catalogTables {
		registered[table.name] = struct{}{}
	}
	var missing []string
	for name := range tables {
		if _, ok := registered[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("backup: catalog tables have no backup policy: %s", strings.Join(missing, ", "))
	}
	return nil
}

func columnExists(ctx context.Context, db queryer, table, column string) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&count)
	return count > 0, err
}

func countRows(ctx context.Context, db queryer, query string) (int64, error) {
	var count int64
	err := db.QueryRowContext(ctx, query).Scan(&count)
	return count, err
}
