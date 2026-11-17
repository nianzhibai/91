package backup

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/nianzhibai/91/backend/internal/catalog"
)

// Every table of a fully migrated catalog needs a backup policy, and every
// policy must describe a table that still exists.
func TestCatalogTablesMatchTheCatalogSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "catalog.db")
	cat, err := catalog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tables, err := schemaTables(ctx, db, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := requireRegisteredTables(tables); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]struct{}, len(catalogTables))
	for _, table := range catalogTables {
		if _, duplicate := seen[table.name]; duplicate {
			t.Errorf("table %s is registered twice", table.name)
		}
		seen[table.name] = struct{}{}
		if _, exists := tables[table.name]; !exists {
			t.Errorf("registered table %s is not part of the catalog schema", table.name)
		}
		ownedByDrive := table.kind == driveRows || table.kind == driveState
		if ownedByDrive != (table.ownerColumn != "") {
			t.Errorf("table %s: owner column %q does not match its kind", table.name, table.ownerColumn)
		}
		if !table.archived() && (table.merge != nil || table.includedWhen != nil ||
			len(table.sanitize) != 0 || len(table.invariants) != 0 || len(table.videoRefs) != 0) {
			t.Errorf("table %s is never archived but declares archive rules", table.name)
		}
		if table.kind != secretRows && len(table.restoreReset) != 0 {
			t.Errorf("table %s resets target rows although it is not secret", table.name)
		}
	}
}

// Deployment-local tables never leave the source, while archives created
// before they were registered, which may still contain their rows, remain
// restorable.
func TestDeploymentLocalTablesAreNotArchivedButTolerated(t *testing.T) {
	env := newTestBackupEnv(t)
	ctx := context.Background()
	if err := env.cat.UpsertDrive(ctx, &catalog.Drive{ID: "cloud", Kind: "pikpak"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.db")
	if err := env.cat.BackupTo(ctx, path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seed := func() {
		t.Helper()
		for _, statement := range []string{
			`INSERT OR REPLACE INTO drive_scan_misses (drive_id, file_id, consecutive_misses, last_missing_at) VALUES ('cloud', 'file', 2, 1)`,
			`INSERT OR REPLACE INTO drive_skip_cleanup_legacy_dirs (drive_id, dir_id, completed_at) VALUES ('cloud', 'dir', 1)`,
			`INSERT OR REPLACE INTO duplicate_asset_cleanup_jobs (video_id, preview_local, attempts, last_error, created_at, updated_at) VALUES ('gone', 'gone/preview.mp4', 0, '', 1, 1)`,
		} {
			if _, err := db.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed()
	selection := BackupSelection{CloudDrives: true}
	if _, err := filterSnapshotDatabase(ctx, path, selection); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"drive_scan_misses", "drive_skip_cleanup_legacy_dirs", "duplicate_asset_cleanup_jobs"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s rows after filtering = %d, err=%v", table, count, err)
		}
	}
	seed()
	if err := validateArchiveDatabaseScope(ctx, path, Manifest{Selection: &selection}); err != nil {
		t.Fatalf("archive with deployment-local rows was rejected: %v", err)
	}
}
