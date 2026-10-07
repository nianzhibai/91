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

	"github.com/video-site/backend/internal/catalog"
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

func filterSnapshotDatabase(
	ctx context.Context,
	databasePath string,
	selection BackupSelection,
) (snapshotSelectionState, error) {
	// Resource filtering is independent of restore ownership. The current
	// protocol always merges user accounts into the target catalog during restore.
	allResourcesSelected := selection.AllResources()
	dsn := databasePath + "?_pragma=busy_timeout(5000)"
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return snapshotSelectionState{}, fmt.Errorf("backup: open filtered SQLite snapshot: %w", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	if err := catalog.MigrateImportDatabase(ctx, database); err != nil {
		return snapshotSelectionState{}, err
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return snapshotSelectionState{}, fmt.Errorf("backup: begin filtered snapshot: %w", err)
	}
	rollback := func() {
		_ = tx.Rollback()
	}

	type driveInfo struct {
		id        string
		kind      string
		localPath string
	}
	var drives []driveInfo
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, COALESCE(credentials, '{}') FROM drives`)
	if err != nil {
		rollback()
		return snapshotSelectionState{}, err
	}
	for rows.Next() {
		var id, kind, credentialsJSON string
		if err := rows.Scan(&id, &kind, &credentialsJSON); err != nil {
			_ = rows.Close()
			rollback()
			return snapshotSelectionState{}, err
		}
		var credentials map[string]string
		if err := json.Unmarshal([]byte(credentialsJSON), &credentials); err != nil {
			_ = rows.Close()
			rollback()
			return snapshotSelectionState{}, fmt.Errorf("backup: decode credentials for drive %s: %w", id, err)
		}
		drives = append(drives, driveInfo{
			id:        id,
			kind:      kind,
			localPath: strings.TrimSpace(credentials["path"]),
		})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		rollback()
		return snapshotSelectionState{}, err
	}
	if err := rows.Close(); err != nil {
		rollback()
		return snapshotSelectionState{}, err
	}

	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE backup_selected_drives (id TEXT PRIMARY KEY)`); err != nil {
		rollback()
		return snapshotSelectionState{}, err
	}
	state := snapshotSelectionState{
		Selection:             selection,
		SelectedVideoIDs:      make(map[string]struct{}),
		SelectedPreviewPaths:  make(map[string]string),
		SelectedUploadFiles:   make(map[string]struct{}),
		SelectedCrawlerDrives: make(map[string]struct{}),
	}
	if err := normalizeTelegramSnapshot(ctx, tx, &state); err != nil {
		rollback()
		return snapshotSelectionState{}, err
	}
	for _, drive := range drives {
		if !driveKindSelected(drive.kind, selection) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO backup_selected_drives (id) VALUES (?)`, drive.id); err != nil {
			rollback()
			return snapshotSelectionState{}, err
		}
		if strings.EqualFold(strings.TrimSpace(drive.kind), "scriptcrawler") {
			state.SelectedCrawlerDrives[drive.id] = struct{}{}
		}
		if strings.EqualFold(strings.TrimSpace(drive.kind), "localstorage") {
			if drive.localPath == "" {
				rollback()
				return snapshotSelectionState{}, fmt.Errorf("backup: local storage %s has no configured path", drive.id)
			}
			resolvedPath, err := resolveLocalStoragePath(drive.localPath)
			if err != nil {
				rollback()
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
			rollback()
			return snapshotSelectionState{}, err
		}
	}

	if !allResourcesSelected {
		// A decision can contain snapshots from several resource types. Export
		// this history only with all resources, so a partial backup cannot carry
		// file metadata from an unselected storage through comparison evidence.
		if _, err := tx.ExecContext(ctx, `DELETE FROM duplicate_records`); err != nil {
			rollback()
			return snapshotSelectionState{}, err
		}
		if _, err := tx.ExecContext(ctx, `
DELETE FROM video_reaction_visits
 WHERE video_id NOT IN (SELECT id FROM videos WHERE drive_id IN (SELECT id FROM backup_selected_drives))`); err != nil {
			rollback()
			return snapshotSelectionState{}, err
		}
		if _, err := tx.ExecContext(ctx, `
DELETE FROM video_shares
 WHERE video_id NOT IN (SELECT id FROM videos WHERE drive_id IN (SELECT id FROM backup_selected_drives))`); err != nil {
			rollback()
			return snapshotSelectionState{}, err
		}
		if _, err := tx.ExecContext(ctx, `
DELETE FROM video_tags
 WHERE video_id NOT IN (SELECT id FROM videos WHERE drive_id IN (SELECT id FROM backup_selected_drives))`); err != nil {
			rollback()
			return snapshotSelectionState{}, err
		}
		if _, err := tx.ExecContext(ctx, `
DELETE FROM videos
 WHERE drive_id NOT IN (SELECT id FROM backup_selected_drives)`); err != nil {
			rollback()
			return snapshotSelectionState{}, err
		}
		for _, statement := range []string{
			`DELETE FROM drives WHERE id NOT IN (SELECT id FROM backup_selected_drives)`,
			`DELETE FROM scans WHERE drive_id NOT IN (SELECT id FROM backup_selected_drives)`,
			`DELETE FROM deleted_videos WHERE drive_id NOT IN (SELECT id FROM backup_selected_drives)`,
			`DELETE FROM crawler_seen_sources WHERE drive_id NOT IN (SELECT id FROM backup_selected_drives)`,
			`DELETE FROM crawler_discoveries WHERE drive_id NOT IN (SELECT id FROM backup_selected_drives)`,
			`DELETE FROM crawler_tasks WHERE drive_id NOT IN (SELECT id FROM backup_selected_drives)`,
			`DELETE FROM crawler_upload_tasks WHERE drive_id NOT IN (SELECT id FROM backup_selected_drives)`,
			`DELETE FROM crawler_upload_results WHERE drive_id NOT IN (SELECT id FROM backup_selected_drives)`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				rollback()
				return snapshotSelectionState{}, err
			}
		}
	}
	// Transient sessions, settings, and login protection are not part of the
	// backup protocol. The optional failure table is recreated when SQLite opens.
	for _, statement := range []string{
		`DELETE FROM banned_login_ips`,
		`DROP TABLE IF EXISTS login_failures`,
		`DELETE FROM settings`,
		`DELETE FROM telegram_settings`,
		`DELETE FROM telegram_connections`,
		`DELETE FROM telegram_receipts`,
		`DELETE FROM telegram_media_group_updates`,
		`DELETE FROM telegram_files WHERE video_id='' OR video_id NOT IN (SELECT id FROM videos)`,
		`UPDATE telegram_files SET file_id=''`,
		`UPDATE remote_upload_jobs SET source_payload='' WHERE source_kind='telegram'`,
		`DELETE FROM admin_sessions`,
		`DELETE FROM video_shares`,
		`DELETE FROM shorts_feed_sessions`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			rollback()
			return snapshotSelectionState{}, err
		}
	}
	if !selection.UserInfo {
		if _, err := tx.ExecContext(ctx, `DELETE FROM users`); err != nil {
			rollback()
			return snapshotSelectionState{}, err
		}
	}
	if !selection.UploadStorage {
		if _, err := tx.ExecContext(ctx, `DELETE FROM telegram_files`); err != nil {
			rollback()
			return snapshotSelectionState{}, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM remote_upload_jobs`); err != nil {
			rollback()
			return snapshotSelectionState{}, err
		}
	} else if _, err := tx.ExecContext(ctx, `
DELETE FROM remote_upload_jobs
 WHERE completed_video_id != ''
   AND completed_video_id NOT IN (SELECT id FROM videos)`); err != nil {
		rollback()
		return snapshotSelectionState{}, err
	}

	rows, err = tx.QueryContext(ctx, `
SELECT id, drive_id, file_id, COALESCE(preview_local, '')
  FROM videos`)
	if err != nil {
		rollback()
		return snapshotSelectionState{}, err
	}
	for rows.Next() {
		var id, driveID, fileID, previewPath string
		if err := rows.Scan(&id, &driveID, &fileID, &previewPath); err != nil {
			_ = rows.Close()
			rollback()
			return snapshotSelectionState{}, err
		}
		state.SelectedVideoIDs[id] = struct{}{}
		if previewPath != "" {
			state.SelectedPreviewPaths[id] = previewPath
		}
		if driveID == "local-upload" {
			if fileID != "" {
				state.SelectedUploadFiles[filepath.Base(fileID)] = struct{}{}
			}
		}
		for index := range state.LocalStorageRoots {
			root := &state.LocalStorageRoots[index]
			if root.DriveID != driveID {
				continue
			}
			relative, err := decodeLocalStorageFileID(fileID)
			if err != nil {
				_ = rows.Close()
				rollback()
				return snapshotSelectionState{}, fmt.Errorf("backup: decode local storage file for video %s: %w", id, err)
			}
			if relative != "" {
				root.Files[relative] = struct{}{}
			}
			break
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		rollback()
		return snapshotSelectionState{}, err
	}
	if err := rows.Close(); err != nil {
		rollback()
		return snapshotSelectionState{}, err
	}
	if err := tx.Commit(); err != nil {
		return snapshotSelectionState{}, fmt.Errorf("backup: commit filtered snapshot: %w", err)
	}
	return state, nil
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
