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

	drives, err := validateArchiveDrives(ctx, database, manifest)
	if err != nil {
		return err
	}
	present, err := schemaTables(ctx, database, "main")
	if err != nil {
		return err
	}
	for _, table := range catalogTables {
		// Older archives predate some optional tables.
		if _, ok := present[table.name]; !ok {
			continue
		}
		if err := validateArchiveTable(ctx, database, table, selection, drives); err != nil {
			return err
		}
	}
	return nil
}

// validateArchiveDrives checks that every archived drive is selected and that
// the archived local storage drives match the manifest. It returns the IDs of
// the archived drives.
func validateArchiveDrives(ctx context.Context, database *sql.DB, manifest Manifest) (map[string]struct{}, error) {
	selection := *manifest.Selection
	drives := make(map[string]struct{})
	databaseLocalDrives := make(map[string]struct{})
	rows, err := database.QueryContext(ctx, `SELECT id, kind FROM drives`)
	if err != nil {
		return nil, fmt.Errorf("backup: inspect drive scope: %w", err)
	}
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if strings.TrimSpace(id) == "" || !driveKindSelected(kind, selection) {
			_ = rows.Close()
			return nil, fmt.Errorf("backup: drive %q is outside the declared backup selection", id)
		}
		drives[id] = struct{}{}
		if strings.EqualFold(strings.TrimSpace(kind), "localstorage") {
			databaseLocalDrives[id] = struct{}{}
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	manifestLocalDrives := make(map[string]struct{}, len(manifest.LocalStorage))
	for _, root := range manifest.LocalStorage {
		manifestLocalDrives[root.DriveID] = struct{}{}
	}
	if len(databaseLocalDrives) != len(manifestLocalDrives) {
		return nil, fmt.Errorf("backup: local storage database rows do not match manifest metadata")
	}
	for id := range databaseLocalDrives {
		if _, declared := manifestLocalDrives[id]; !declared {
			return nil, fmt.Errorf("backup: local storage drive %q is not declared in manifest metadata", id)
		}
	}
	return drives, nil
}

// validateArchiveTable checks one table against the catalogTables policy.
// Deployment-local tables are ignored: restore never imports them, and
// archives created before they were registered may still contain rows.
func validateArchiveTable(
	ctx context.Context,
	database *sql.DB,
	table catalogTable,
	selection BackupSelection,
	drives map[string]struct{},
) error {
	switch {
	case table.kind == secretRows:
		if err := requireArchiveTableEmpty(ctx, database, table.name); err != nil {
			return fmt.Errorf("backup: %s must not be archived: %w", table.name, err)
		}
		return nil
	case !table.archived():
		return nil
	case !table.includedIn(selection):
		if err := requireArchiveTableEmpty(ctx, database, table.name); err != nil {
			return fmt.Errorf("backup: %s is present although it was not selected: %w", table.name, err)
		}
		return nil
	}
	switch table.kind {
	case driveRows:
		if table.name != "drives" {
			if err := validateArchiveDriveScopedTable(ctx, database, table, drives, selection); err != nil {
				return err
			}
		}
	case videoRows:
		orphans, err := countRows(ctx, database, `SELECT COUNT(*) FROM `+quoteIdentifier(table.name)+
			` WHERE video_id NOT IN (SELECT id FROM videos)`)
		if err != nil {
			return fmt.Errorf("backup: inspect %s: %w", table.name, err)
		}
		if orphans != 0 {
			return fmt.Errorf("backup: %d %s rows reference videos outside the archive", orphans, table.name)
		}
	}
	for _, invariant := range table.invariants {
		if invariant.column != "" {
			exists, err := columnExists(ctx, database, table.name, invariant.column)
			if err != nil {
				return err
			}
			if !exists {
				continue
			}
		}
		invalid, err := countRows(ctx, database, invariant.violations)
		if err != nil {
			return fmt.Errorf("backup: inspect %s: %w", invariant.description, err)
		}
		if invalid != 0 {
			return fmt.Errorf("backup: %d invalid %s rows fall outside the declared archive scope", invalid, invariant.description)
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
	table catalogTable,
	drives map[string]struct{},
	selection BackupSelection,
) error {
	rows, err := database.QueryContext(ctx, `SELECT DISTINCT `+quoteIdentifier(table.ownerColumn)+
		` FROM `+quoteIdentifier(table.name))
	if err != nil {
		return fmt.Errorf("backup: inspect %s scope: %w", table.name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var driveID string
		if err := rows.Scan(&driveID); err != nil {
			return err
		}
		if !archiveDriveReferenceAllowed(driveID, drives, selection) {
			return fmt.Errorf("backup: %s references drive %q outside the declared backup selection", table.name, driveID)
		}
	}
	return rows.Err()
}

func requireArchiveTableEmpty(ctx context.Context, database *sql.DB, table string) error {
	count, err := countRows(ctx, database, `SELECT COUNT(*) FROM `+quoteIdentifier(table))
	if err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("contains %d rows", count)
	}
	return nil
}
