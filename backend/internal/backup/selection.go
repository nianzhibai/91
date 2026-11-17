package backup

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nianzhibai/91/backend/internal/catalog"
)

// snapshotSelectionState is the small amount of catalog state needed while
// selecting file assets after the SQLite snapshot has been filtered.
type snapshotSelectionState struct {
	Selection             BackupSelection
	SelectedVideoIDs      map[string]struct{}
	SelectedPreviewPaths  map[string]string
	SelectedUploadFiles   map[string]struct{}
	TelegramUploadFiles   map[string]catalog.TelegramLocalFile
	SelectedCrawlerDrives map[string]struct{}
	LocalStorageRoots     []snapshotLocalStorageRoot
}

type snapshotLocalStorageRoot struct {
	LocalStorageRoot
	SourcePath string
	Files      map[string]struct{}
}

func (s BackupSelection) AllResources() bool {
	return s.CloudDrives && s.CrawlerScripts && s.UploadStorage && s.LocalStorage
}

// anyResource reports whether any video resource is selected. User
// information alone archives no videos or assets.
func (s BackupSelection) anyResource() bool {
	return s.CloudDrives || s.CrawlerScripts || s.UploadStorage || s.LocalStorage
}

func driveKindSelected(kind string, selection BackupSelection) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "scriptcrawler":
		return selection.CrawlerScripts
	case "localstorage":
		return selection.LocalStorage
	case "local-upload":
		return selection.UploadStorage
	default:
		return selection.CloudDrives
	}
}

func archiveDriveComponent(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

func resolveLocalStoragePath(raw string) (string, error) {
	raw = strings.TrimSpace(os.ExpandEnv(raw))
	if raw == "" {
		return "", errors.New("local storage path is empty")
	}
	if strings.HasPrefix(raw, "~") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			switch {
			case raw == "~":
				raw = home
			case strings.HasPrefix(raw, "~/") || strings.HasPrefix(raw, `~\`):
				raw = filepath.Join(home, raw[2:])
			}
		}
	}
	return filepath.Abs(raw)
}

func decodeLocalStorageFileID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" || id == "/" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		// A few pre-driver catalog rows stored the relative path directly.
		// Accept that legacy form only after applying the same containment rules.
		raw = []byte(id)
	}
	relative := filepath.ToSlash(filepath.Clean(filepath.FromSlash(string(raw))))
	if relative == "." || relative == "" {
		return "", nil
	}
	if strings.HasPrefix(relative, "../") || relative == ".." || strings.HasPrefix(relative, "/") {
		return "", errors.New("file id escapes local storage root")
	}
	return relative, nil
}

// filterSnapshotDatabase reduces a catalog snapshot to the selection. It
// removes rows outside the selected drives, every table the archive must not
// carry, and returns the state needed to select file assets.
func filterSnapshotDatabase(
	ctx context.Context,
	databasePath string,
	selection BackupSelection,
) (snapshotSelectionState, error) {
	database, err := sql.Open("sqlite", databasePath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return snapshotSelectionState{}, fmt.Errorf("backup: open filtered SQLite snapshot: %w", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	// The archive carries a single database file, so the snapshot must not
	// keep committed pages in a WAL.
	var journalMode string
	if err := database.QueryRowContext(ctx, `PRAGMA journal_mode = DELETE`).Scan(&journalMode); err != nil {
		return snapshotSelectionState{}, fmt.Errorf("backup: configure SQLite snapshot journal: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(journalMode), "delete") {
		return snapshotSelectionState{}, fmt.Errorf("backup: SQLite snapshot journal mode is %q, want delete", journalMode)
	}
	if err := catalog.MigrateImportDatabase(ctx, database); err != nil {
		return snapshotSelectionState{}, err
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return snapshotSelectionState{}, fmt.Errorf("backup: begin filtered snapshot: %w", err)
	}
	defer tx.Rollback()
	tables, err := schemaTables(ctx, tx, "main")
	if err != nil {
		return snapshotSelectionState{}, err
	}
	if err := requireRegisteredTables(tables); err != nil {
		return snapshotSelectionState{}, err
	}
	state, err := selectSnapshotDrives(ctx, tx, selection)
	if err != nil {
		return snapshotSelectionState{}, err
	}
	if err := normalizeTelegramSnapshot(ctx, tx, &state); err != nil {
		return snapshotSelectionState{}, err
	}
	if err := pruneSnapshotTables(ctx, tx, selection, tables); err != nil {
		return snapshotSelectionState{}, err
	}
	if err := collectSnapshotAssets(ctx, tx, &state); err != nil {
		return snapshotSelectionState{}, err
	}
	if err := tx.Commit(); err != nil {
		return snapshotSelectionState{}, fmt.Errorf("backup: commit filtered snapshot: %w", err)
	}
	return state, nil
}

// selectSnapshotDrives records the selected drives in the temporary
// backup_selected_drives table and prepares the per-drive asset state.
func selectSnapshotDrives(ctx context.Context, tx *sql.Tx, selection BackupSelection) (snapshotSelectionState, error) {
	type driveInfo struct {
		id        string
		kind      string
		localPath string
	}
	var drives []driveInfo
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, COALESCE(credentials, '{}') FROM drives`)
	if err != nil {
		return snapshotSelectionState{}, err
	}
	for rows.Next() {
		var id, kind, credentialsJSON string
		if err := rows.Scan(&id, &kind, &credentialsJSON); err != nil {
			_ = rows.Close()
			return snapshotSelectionState{}, err
		}
		var credentials map[string]string
		if err := json.Unmarshal([]byte(credentialsJSON), &credentials); err != nil {
			_ = rows.Close()
			return snapshotSelectionState{}, fmt.Errorf("backup: decode credentials for drive %s: %w", id, err)
		}
		drives = append(drives, driveInfo{id: id, kind: kind, localPath: strings.TrimSpace(credentials["path"])})
	}
	if err := rows.Close(); err != nil {
		return snapshotSelectionState{}, err
	}

	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE backup_selected_drives (id TEXT PRIMARY KEY)`); err != nil {
		return snapshotSelectionState{}, err
	}
	state := snapshotSelectionState{
		Selection:             selection,
		SelectedVideoIDs:      make(map[string]struct{}),
		SelectedPreviewPaths:  make(map[string]string),
		SelectedUploadFiles:   make(map[string]struct{}),
		SelectedCrawlerDrives: make(map[string]struct{}),
	}
	for _, drive := range drives {
		if !driveKindSelected(drive.kind, selection) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO backup_selected_drives (id) VALUES (?)`, drive.id); err != nil {
			return snapshotSelectionState{}, err
		}
		switch strings.ToLower(strings.TrimSpace(drive.kind)) {
		case "scriptcrawler":
			state.SelectedCrawlerDrives[drive.id] = struct{}{}
		case "localstorage":
			if drive.localPath == "" {
				return snapshotSelectionState{}, fmt.Errorf("backup: local storage %s has no configured path", drive.id)
			}
			resolvedPath, err := resolveLocalStoragePath(drive.localPath)
			if err != nil {
				return snapshotSelectionState{}, fmt.Errorf("backup: resolve local storage %s: %w", drive.id, err)
			}
			state.LocalStorageRoots = append(state.LocalStorageRoots, snapshotLocalStorageRoot{
				LocalStorageRoot: LocalStorageRoot{
					DriveID:     drive.id,
					ArchivePath: archiveDriveComponent(drive.id),
				},
				SourcePath: resolvedPath,
				Files:      make(map[string]struct{}),
			})
		}
	}
	// The built-in local upload drive is intentionally not stored as a drives row
	// on every deployment, but its videos still belong to the upload selection.
	if selection.UploadStorage {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO backup_selected_drives (id) VALUES ('local-upload')`); err != nil {
			return snapshotSelectionState{}, err
		}
	}
	return state, nil
}

// pruneSnapshotTables applies the catalogTables policy to a snapshot: rows of
// unselected drives, tables outside the selection, rows whose video was
// removed, and never-archived tables are deleted, then selected tables are
// sanitized.
func pruneSnapshotTables(ctx context.Context, tx *sql.Tx, selection BackupSelection, present map[string]struct{}) error {
	type statement struct{ table, sql string }
	var statements []statement
	// A full resource backup deliberately keeps orphan rows whose drive
	// metadata disappeared before the backup was created.
	if !selection.AllResources() {
		for _, table := range catalogTables {
			if table.kind == driveRows {
				statements = append(statements, statement{table.name, `DELETE FROM ` + quoteIdentifier(table.name) +
					` WHERE ` + quoteIdentifier(table.ownerColumn) + ` NOT IN (SELECT id FROM backup_selected_drives)`})
			}
		}
	}
	for _, table := range catalogTables {
		quoted := quoteIdentifier(table.name)
		switch {
		case table.kind == derivedRows:
		case !table.includedIn(selection):
			statements = append(statements, statement{table.name, `DELETE FROM ` + quoted})
		case table.kind == videoRows:
			statements = append(statements, statement{table.name, `DELETE FROM ` + quoted + ` WHERE video_id NOT IN (SELECT id FROM videos)`})
		}
	}
	for _, table := range catalogTables {
		if table.includedIn(selection) {
			for _, sanitize := range table.sanitize {
				statements = append(statements, statement{table.name, sanitize})
			}
		}
	}
	for _, statement := range statements {
		if _, ok := present[statement.table]; !ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, statement.sql); err != nil {
			return fmt.Errorf("backup: filter %s: %w", statement.table, err)
		}
	}
	return nil
}

// collectSnapshotAssets records the files owned by the archived videos.
func collectSnapshotAssets(ctx context.Context, tx *sql.Tx, state *snapshotSelectionState) error {
	rows, err := tx.QueryContext(ctx, `
SELECT id, drive_id, file_id, COALESCE(preview_local, '')
  FROM videos`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, driveID, fileID, previewPath string
		if err := rows.Scan(&id, &driveID, &fileID, &previewPath); err != nil {
			return err
		}
		state.SelectedVideoIDs[id] = struct{}{}
		if previewPath != "" {
			state.SelectedPreviewPaths[id] = previewPath
		}
		if driveID == "local-upload" && fileID != "" {
			state.SelectedUploadFiles[filepath.Base(fileID)] = struct{}{}
		}
		for index := range state.LocalStorageRoots {
			root := &state.LocalStorageRoots[index]
			if root.DriveID != driveID {
				continue
			}
			relative, err := decodeLocalStorageFileID(fileID)
			if err != nil {
				return fmt.Errorf("backup: decode local storage file for video %s: %w", id, err)
			}
			if relative != "" {
				root.Files[relative] = struct{}{}
			}
			break
		}
	}
	return rows.Err()
}

// compactSnapshotDatabase rebuilds the filtered catalog into a new SQLite
// file. SQL DELETE only removes rows from the logical database; with SQLite's
// default secure_delete setting, passwords and drive credentials can remain in
// freelist pages and would otherwise be copied into the ZIP archive.
func compactSnapshotDatabase(ctx context.Context, databasePath string) error {
	compactedPath := databasePath + ".compacted"
	if _, err := os.Lstat(compactedPath); err == nil {
		return fmt.Errorf("backup: compacted SQLite snapshot already exists: %s", compactedPath)
	} else if !os.IsNotExist(err) {
		return err
	}

	database, err := sql.Open("sqlite", databasePath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("backup: open filtered SQLite snapshot for compaction: %w", err)
	}
	database.SetMaxOpenConns(1)
	quoted := strings.ReplaceAll(compactedPath, "'", "''")
	if _, err := database.ExecContext(ctx, "VACUUM INTO '"+quoted+"'"); err != nil {
		_ = database.Close()
		_ = os.Remove(compactedPath)
		return fmt.Errorf("backup: compact filtered SQLite snapshot: %w", err)
	}
	if err := database.Close(); err != nil {
		_ = os.Remove(compactedPath)
		return fmt.Errorf("backup: close filtered SQLite snapshot after compaction: %w", err)
	}
	if err := verifySQLite(compactedPath); err != nil {
		_ = os.Remove(compactedPath)
		return fmt.Errorf("backup: verify compacted SQLite snapshot: %w", err)
	}
	info, err := os.Stat(databasePath)
	if err != nil {
		_ = os.Remove(compactedPath)
		return err
	}
	mode := info.Mode().Perm()
	if mode == 0 {
		mode = 0o600
	}
	if err := os.Chmod(compactedPath, mode); err != nil {
		_ = os.Remove(compactedPath)
		return err
	}
	if err := os.Remove(databasePath); err != nil {
		_ = os.Remove(compactedPath)
		return fmt.Errorf("backup: replace filtered SQLite snapshot: %w", err)
	}
	if err := os.Rename(compactedPath, databasePath); err != nil {
		_ = os.Remove(compactedPath)
		return fmt.Errorf("backup: activate compacted SQLite snapshot: %w", err)
	}
	return nil
}
