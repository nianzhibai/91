package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/localpath"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
	"github.com/nianzhibai/91/backend/internal/videoid"
)

type isolatedLocalStorageRestore struct {
	SourceDriveID string
	DriveID       string
	Name          string
	TargetPath    string
	CreatedAt     time.Time
}

func (m *Manager) planIsolatedLocalStorageRestore(
	ctx context.Context,
	stageID string,
	roots []LocalStorageRoot,
) ([]isolatedLocalStorageRestore, error) {
	if len(roots) == 0 {
		return nil, nil
	}
	sourceIDs := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		id := strings.TrimSpace(root.DriveID)
		if id == "" || id != root.DriveID {
			return nil, fmt.Errorf("backup: local storage manifest contains an empty drive id")
		}
		if _, exists := sourceIDs[id]; exists {
			return nil, fmt.Errorf("backup: local storage manifest repeats drive %s", id)
		}
		sourceIDs[id] = struct{}{}
	}

	drives, err := m.catalog.ListDrives(ctx)
	if err != nil {
		return nil, fmt.Errorf("backup: inspect target drives for local storage restore: %w", err)
	}
	existingDriveIDs := make(map[string]struct{}, len(drives))
	var existingLocalPaths []string
	for _, drive := range drives {
		if drive == nil {
			continue
		}
		existingDriveIDs[drive.ID] = struct{}{}
		if !strings.EqualFold(strings.TrimSpace(drive.Kind), "localstorage") {
			continue
		}
		existingPath, resolveErr := resolveLocalStoragePath(drive.Credentials["path"])
		if resolveErr == nil {
			existingLocalPaths = append(existingLocalPaths, existingPath)
		}
	}
	orderedIDs := make([]string, 0, len(sourceIDs))
	for id := range sourceIDs {
		orderedIDs = append(orderedIDs, id)
	}
	sort.Strings(orderedIDs)
	createdAt := m.nowTime()
	plans := make([]isolatedLocalStorageRestore, 0, len(orderedIDs))
	reservedPaths := append([]string(nil), existingLocalPaths...)
	for index, sourceDriveID := range orderedIDs {
		driveID := fmt.Sprintf("localstorage-restore-%s-%03d", stageID, index+1)
		if _, exists := existingDriveIDs[driveID]; exists {
			return nil, fmt.Errorf("backup: generated local storage id already exists: %s", driveID)
		}
		var targetPath string
		for _, candidate := range []string{
			filepath.Join(m.assetRoot, "localstorage-restores", driveID),
			filepath.Join(filepath.Dir(m.dataRoot), ".video-site-localstorage-restores", driveID),
		} {
			overlaps := false
			for _, reserved := range reservedPaths {
				if restoreTargetPathsOverlap(reserved, candidate) {
					overlaps = true
					break
				}
			}
			if overlaps {
				continue
			}
			if _, err := os.Lstat(candidate); err == nil {
				continue
			} else if !os.IsNotExist(err) {
				return nil, err
			}
			targetPath = candidate
			break
		}
		if targetPath == "" {
			return nil, fmt.Errorf("backup: cannot allocate an isolated local storage path for %s", sourceDriveID)
		}
		reservedPaths = append(reservedPaths, targetPath)
		plans = append(plans, isolatedLocalStorageRestore{
			SourceDriveID: sourceDriveID,
			DriveID:       driveID,
			Name:          "恢复的本地存储 " + sourceDriveID + " " + createdAt.Format("2006-01-02 15:04:05"),
			TargetPath:    targetPath,
			CreatedAt:     createdAt,
		})
	}
	return plans, nil
}

// rewriteLocalStorageCatalog maps every archived local storage drive to its own
// restore-owned drive. Keeping the source-drive namespace prevents otherwise
// valid identical relative file IDs from colliding during restore.
func rewriteLocalStorageCatalog(
	ctx context.Context,
	tx *sql.Tx,
	plans []isolatedLocalStorageRestore,
	stagePreviewDir string,
) (map[string]string, error) {
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE restore_local_drive_ids (old_id TEXT PRIMARY KEY, new_id TEXT NOT NULL UNIQUE)`); err != nil {
		return nil, err
	}
	planBySource := make(map[string]isolatedLocalStorageRestore, len(plans))
	for _, plan := range plans {
		if plan.SourceDriveID == "" || plan.DriveID == "" {
			return nil, errors.New("backup: local storage restore plan is incomplete")
		}
		if _, exists := planBySource[plan.SourceDriveID]; exists {
			return nil, fmt.Errorf("backup: local storage restore plan repeats drive %s", plan.SourceDriveID)
		}
		planBySource[plan.SourceDriveID] = plan
		if _, err := tx.ExecContext(ctx, `INSERT INTO restore_local_drive_ids (old_id, new_id) VALUES (?, ?)`, plan.SourceDriveID, plan.DriveID); err != nil {
			return nil, err
		}
		var kind string
		if err := tx.QueryRowContext(ctx, `SELECT kind FROM drives WHERE id = ?`, plan.SourceDriveID).Scan(&kind); err != nil {
			return nil, fmt.Errorf("backup: archived local storage drive %s is missing from database: %w", plan.SourceDriveID, err)
		}
		if !strings.EqualFold(strings.TrimSpace(kind), "localstorage") {
			return nil, fmt.Errorf("backup: archived drive %s is not local storage", plan.SourceDriveID)
		}
	}

	// Archive validation guarantees that every archived local storage drive is
	// declared in the manifest and therefore has a plan.
	unplanned, err := countRows(ctx, tx, `SELECT COUNT(*) FROM drives
		WHERE lower(trim(kind)) = 'localstorage' AND id NOT IN (SELECT old_id FROM restore_local_drive_ids)`)
	if err != nil {
		return nil, err
	}
	if unplanned != 0 {
		return nil, errors.New("backup: archived local storage drives have no restore plan")
	}
	if len(plans) == 0 {
		return nil, nil
	}
	present, err := schemaTables(ctx, tx, "main")
	if err != nil {
		return nil, err
	}

	type localVideo struct {
		oldID      string
		oldDriveID string
		fileID     string
		newDriveID string
	}
	localVideoRows, err := tx.QueryContext(ctx, `
SELECT id, drive_id, file_id
  FROM videos
 WHERE drive_id IN (SELECT old_id FROM restore_local_drive_ids)
 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var videos []localVideo
	for localVideoRows.Next() {
		var video localVideo
		if err := localVideoRows.Scan(&video.oldID, &video.oldDriveID, &video.fileID); err != nil {
			_ = localVideoRows.Close()
			return nil, err
		}
		video.newDriveID = planBySource[video.oldDriveID].DriveID
		videos = append(videos, video)
	}
	if err := localVideoRows.Close(); err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE restore_local_video_ids (old_id TEXT PRIMARY KEY, new_id TEXT NOT NULL UNIQUE, new_drive_id TEXT NOT NULL)`); err != nil {
		return nil, err
	}
	oldToNew := make(map[string]string, len(videos))
	newToOld := make(map[string]string, len(videos))
	for _, video := range videos {
		newID := videoid.ForDrive("localstorage", video.newDriveID, video.fileID)
		if previous, exists := newToOld[newID]; exists && previous != video.oldID {
			return nil, fmt.Errorf(
				"backup: local storage files %s and %s resolve to the same restored video id",
				previous,
				video.oldID,
			)
		}
		if err := moveLocalVideoAssets(stagePreviewDir, video.oldID, newID); err != nil {
			return nil, fmt.Errorf("backup: remap preview assets for %s: %w", video.oldID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO restore_local_video_ids (old_id, new_id, new_drive_id) VALUES (?, ?, ?)`, video.oldID, newID, video.newDriveID); err != nil {
			return nil, err
		}
		oldToNew[video.oldID] = newID
		newToOld[newID] = video.oldID
	}

	deletedRows, err := tx.QueryContext(ctx, `
SELECT id, drive_id, file_id
  FROM deleted_videos
 WHERE drive_id IN (SELECT old_id FROM restore_local_drive_ids)
 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var deletedVideos []localVideo
	for deletedRows.Next() {
		var video localVideo
		if err := deletedRows.Scan(&video.oldID, &video.oldDriveID, &video.fileID); err != nil {
			_ = deletedRows.Close()
			return nil, err
		}
		video.newDriveID = planBySource[video.oldDriveID].DriveID
		deletedVideos = append(deletedVideos, video)
	}
	if err := deletedRows.Close(); err != nil {
		return nil, err
	}
	for _, video := range deletedVideos {
		newID := videoid.ForDrive("localstorage", video.newDriveID, video.fileID)
		var existing string
		scanErr := tx.QueryRowContext(ctx, `SELECT old_id FROM restore_local_video_ids WHERE new_id = ?`, newID).Scan(&existing)
		if scanErr == nil && existing != video.oldID {
			return nil, fmt.Errorf("backup: deleted local videos %s and %s resolve to the same restored video id", existing, video.oldID)
		}
		if scanErr != nil && scanErr != sql.ErrNoRows {
			return nil, scanErr
		}
		if scanErr == sql.ErrNoRows {
			if _, err := tx.ExecContext(ctx, `INSERT INTO restore_local_video_ids (old_id, new_id, new_drive_id) VALUES (?, ?, ?)`, video.oldID, newID, video.newDriveID); err != nil {
				return nil, err
			}
		}
		oldToNew[video.oldID] = newID
	}

	// Every video reference follows its video to the new identity.
	for _, table := range catalogTables {
		if _, ok := present[table.name]; !ok {
			continue
		}
		columns := table.videoRefs
		if table.kind == videoRows {
			columns = append([]string{"video_id"}, columns...)
		}
		for _, column := range columns {
			name, column := quoteIdentifier(table.name), quoteIdentifier(column)
			if _, err := tx.ExecContext(ctx, `UPDATE `+name+`
				SET `+column+` = (SELECT new_id FROM restore_local_video_ids WHERE old_id = `+name+`.`+column+`)
				WHERE `+column+` IN (SELECT old_id FROM restore_local_video_ids)`); err != nil {
				return nil, err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE videos
   SET id = (SELECT new_id FROM restore_local_video_ids WHERE old_id = videos.id),
	   drive_id = (SELECT new_drive_id FROM restore_local_video_ids WHERE old_id = videos.id)
 WHERE id IN (SELECT old_id FROM restore_local_video_ids)`); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE deleted_videos
   SET id = (SELECT new_id FROM restore_local_video_ids WHERE old_id = deleted_videos.id),
	   drive_id = (SELECT new_drive_id FROM restore_local_video_ids WHERE old_id = deleted_videos.id)
 WHERE drive_id IN (SELECT old_id FROM restore_local_drive_ids)`); err != nil {
		return nil, err
	}
	// Videos and tombstones were moved above together with their new IDs.
	for _, table := range catalogTables {
		if table.kind != driveRows {
			continue
		}
		switch table.name {
		case "drives", "videos", "deleted_videos":
			continue
		}
		if _, ok := present[table.name]; !ok {
			continue
		}
		name, column := quoteIdentifier(table.name), quoteIdentifier(table.ownerColumn)
		if _, err := tx.ExecContext(ctx, `UPDATE `+name+`
			SET `+column+` = (SELECT new_id FROM restore_local_drive_ids WHERE old_id = `+name+`.`+column+`)
			WHERE `+column+` IN (SELECT old_id FROM restore_local_drive_ids)`); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM drives WHERE lower(trim(kind)) = 'localstorage'`); err != nil {
		return nil, err
	}
	for _, plan := range plans {
		credentials, err := json.Marshal(map[string]string{"path": plan.TargetPath})
		if err != nil {
			return nil, err
		}
		createdAt := plan.CreatedAt.UnixMilli()
		if _, err := tx.ExecContext(ctx, `
INSERT INTO drives (
  id, kind, name, root_id, scan_root_id, credentials, status, last_error,
  skip_dir_ids, created_at, updated_at
) VALUES (?, 'localstorage', ?, '/', '/', ?, 'ok', '', '[]', ?, ?)`,
			plan.DriveID,
			plan.Name,
			string(credentials),
			createdAt,
			createdAt,
		); err != nil {
			return nil, err
		}
	}
	return oldToNew, nil
}

func moveLocalVideoAssets(root, oldID, newID string) error {
	groups := []struct {
		oldPaths []string
		newPath  string
	}{
		{oldPaths: mediaasset.PreviewPathCandidates(root, oldID), newPath: mediaasset.PreviewPath(root, newID)},
		{oldPaths: mediaasset.ThumbnailPathCandidates(root, oldID), newPath: mediaasset.ThumbnailPath(root, newID)},
		{oldPaths: []string{mediaasset.ShortsBackgroundPath(root, oldID)}, newPath: mediaasset.ShortsBackgroundPath(root, newID)},
		{oldPaths: []string{mediaasset.FrameSignaturePath(root, oldID)}, newPath: mediaasset.FrameSignaturePath(root, newID)},
	}
	for _, group := range groups {
		moved := false
		for _, oldPath := range group.oldPaths {
			info, err := os.Lstat(oldPath)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				continue
			}
			if !moved {
				if _, err := os.Lstat(group.newPath); err == nil {
					return fmt.Errorf("restored preview destination already exists: %s", group.newPath)
				} else if !os.IsNotExist(err) {
					return err
				}
				if err := os.MkdirAll(filepath.Dir(group.newPath), 0o755); err != nil {
					return err
				}
				if err := os.Rename(oldPath, group.newPath); err != nil {
					return err
				}
				moved = true
				continue
			}
			if err := os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func remapLocalPreviewPath(
	original string,
	rewritten string,
	oldID string,
	newID string,
	sourcePreviewRoot string,
	targetPreviewRoot string,
) string {
	for _, root := range []string{sourcePreviewRoot, targetPreviewRoot} {
		relative, ok := localpath.ManagedRelative(root, original)
		if !ok && root == targetPreviewRoot {
			relative, ok = localpath.ManagedRelative(root, rewritten)
		}
		if !ok {
			continue
		}
		for _, candidate := range mediaasset.PreviewPathCandidates(root, oldID) {
			candidateRelative, candidateOK := localpath.RelativeWithin(root, candidate)
			if candidateOK && filepath.Clean(candidateRelative) == filepath.Clean(relative) {
				return mediaasset.PreviewPath(targetPreviewRoot, newID)
			}
		}
	}
	return rewritten
}
