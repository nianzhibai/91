package catalog

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nianzhibai/91/backend/internal/driveevents"
	"github.com/nianzhibai/91/backend/internal/localpath"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
)

// MigrateManagedPaths converts legacy deployment paths into portable file
// identities before any workers run. It never changes files. The conversion
// is idempotent and also accepts a legacy database moved before its first run
// with this version. New writes already use relative references.
func (c *Catalog) MigrateManagedPaths(ctx context.Context, previewDir string) (resultValue int, resultErr error) {
	defer c.notifyDriveWrite(&resultErr, "", driveevents.AssetPathsChanged)
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	changed := 0
	update := func(statement, id, old, value string) error {
		if value == old {
			return nil
		}
		if _, err := tx.ExecContext(ctx, statement, value, id); err != nil {
			return err
		}
		changed++
		return nil
	}
	for _, table := range []struct{ name, key string }{{"videos", "id"}, {"duplicate_asset_cleanup_jobs", "video_id"}} {
		rows, err := readManagedPathRows(ctx, tx, `SELECT `+table.key+`, preview_local FROM `+table.name+` WHERE COALESCE(preview_local, '') != ''`)
		if err != nil {
			return 0, err
		}
		for _, row := range rows {
			value := portablePreviewReference(previewDir, row.id, row.value)
			if err := update(`UPDATE `+table.name+` SET preview_local = ? WHERE `+table.key+` = ?`, row.id, row.value, value); err != nil {
				return 0, err
			}
		}
	}
	deleted, err := readManagedPathRows(ctx, tx, `SELECT id, restore_payload FROM deleted_videos WHERE COALESCE(restore_payload, '') != ''`)
	if err != nil {
		return 0, err
	}
	for _, row := range deleted {
		value, err := migrateDeletedPreviewReference(row.value, func(value string) string {
			return portablePreviewReference(previewDir, row.id, value)
		})
		if err != nil {
			return 0, fmt.Errorf("convert deleted video %s paths: %w", row.id, err)
		}
		if err := update(`UPDATE deleted_videos SET restore_payload = ? WHERE id = ?`, row.id, row.value, value); err != nil {
			return 0, err
		}
	}
	scripts, err := readManagedPathRows(ctx, tx, `SELECT id, credentials FROM drives WHERE kind = 'scriptcrawler'`)
	if err != nil {
		return 0, err
	}
	importDir := filepath.Join(filepath.Dir(previewDir), "crawler-scripts")
	for _, row := range scripts {
		var credentials map[string]string
		if err := json.Unmarshal([]byte(row.value), &credentials); err != nil {
			return 0, fmt.Errorf("convert crawler %s path: %w", row.id, err)
		}
		path := strings.TrimSpace(credentials["script_path"])
		if path == "" || credentials["script_file"] != "" {
			continue
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return 0, err
		}
		if relative, ok := localpath.RelativeWithin(importDir, absolute); ok && relative != "." {
			credentials["script_file"] = filepath.ToSlash(relative)
			delete(credentials, "script_path")
		} else if relocatedImportedScript(absolute, importDir) {
			// Imported scripts in older releases were always direct children.
			credentials["script_file"] = filepath.Base(absolute)
			delete(credentials, "script_path")
		} else {
			// External scripts have their own location, unrelated to the data
			// directory. Preserve their identity, including old CWD-relative paths.
			credentials["script_path"] = absolute
		}
		data, err := json.Marshal(credentials)
		if err != nil {
			return 0, err
		}
		if err := update(`UPDATE drives SET credentials = ? WHERE id = ?`, row.id, row.value, string(data)); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return changed, nil
}

// The legacy schema did not distinguish imported and external scripts. After
// a move, require a matching file in the import directory before treating an
// old crawler-scripts path as managed. An unrelated external directory with
// the same name must not silently change the crawler's executable.
func relocatedImportedScript(oldPath, importDir string) bool {
	if filepath.Base(filepath.Dir(oldPath)) != "crawler-scripts" {
		return false
	}
	newPath := filepath.Join(importDir, filepath.Base(oldPath))
	newInfo, err := os.Lstat(newPath)
	if err != nil || !newInfo.Mode().IsRegular() {
		return false
	}
	oldInfo, err := os.Stat(oldPath)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil || !oldInfo.Mode().IsRegular() || oldInfo.Size() != newInfo.Size() {
		return false
	}
	oldData, err := os.ReadFile(oldPath)
	if err != nil {
		return false
	}
	newData, err := os.ReadFile(newPath)
	return err == nil && bytes.Equal(oldData, newData)
}

func portablePreviewReference(root, id, value string) string {
	if strings.TrimSpace(value) == "" {
		return value
	}
	// Old relative paths were relative to the process, not to the preview root.
	if relative, ok := localpath.RelativeWithin(root, value); ok && relative != "." {
		return filepath.ToSlash(relative)
	}
	if !filepath.IsAbs(value) && !strings.HasPrefix(value, "."+string(filepath.Separator)) &&
		filepath.Dir(value) != filepath.Join("data", "previews") {
		// New relative references already belong to root, including nested
		// files restored from a backup. Only the old default/CWD-relative form
		// needs the legacy generated-filename fallback below.
		return value
	}
	// Generated previews have a deterministic name. This also converts old
	// paths when their original directory no longer exists after a manual move.
	for _, candidate := range mediaasset.PreviewPathCandidates(filepath.Dir(value), id) {
		if filepath.Clean(value) == candidate {
			return filepath.Base(candidate)
		}
	}
	return value
}

type managedPathRow struct{ id, value string }

func readManagedPathRows(ctx context.Context, tx *sql.Tx, query string) ([]managedPathRow, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []managedPathRow
	for rows.Next() {
		var row managedPathRow
		if err := rows.Scan(&row.id, &row.value); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// Only rewrite the preview reference, preserving unknown fields and user text
// in both versioned and legacy restore payloads.
func migrateDeletedPreviewReference(encoded string, convert func(string) string) (string, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(encoded), &payload); err != nil {
		return "", err
	}
	if video, ok := payload["video"]; ok {
		converted, err := migrateDeletedPreviewReference(string(video), convert)
		if err != nil || converted == string(video) {
			return encoded, err
		}
		payload["video"] = json.RawMessage(converted)
	} else {
		var path string
		if raw, ok := payload["previewLocal"]; !ok {
			return encoded, nil
		} else if err := json.Unmarshal(raw, &path); err != nil {
			return "", err
		}
		converted := convert(path)
		if converted == path {
			return encoded, nil
		}
		payload["previewLocal"], _ = json.Marshal(converted)
	}
	data, err := json.Marshal(payload)
	return string(data), err
}
