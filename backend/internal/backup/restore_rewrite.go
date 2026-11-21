package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/localpath"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
)

// catalogRewrite maps a staged source catalog onto the target deployment.
type catalogRewrite struct {
	selection BackupSelection
	// stagePreviewDir holds the extracted preview assets of the archive.
	stagePreviewDir   string
	sourceDataRoot    string
	sourcePreviewRoot string
	targetDataRoot    string
	targetAssetRoot   string
	targetPreviewRoot string
	localStoragePlans []isolatedLocalStorageRestore
}

// hostPath maps an absolute path below a source root to the matching target
// root. Archives created before portable file references can still contain
// absolute paths of the source host.
func (r catalogRewrite) hostPath(value string) string {
	return rewriteRestoredPath(value, []pathRewrite{
		{from: r.sourcePreviewRoot, to: r.targetPreviewRoot},
		{from: filepath.Dir(r.sourcePreviewRoot), to: r.targetAssetRoot},
		{from: r.sourceDataRoot, to: r.targetDataRoot},
	})
}

// rewriteRestoredDatabase adapts a staged source catalog to the target: local
// storage drives get new identities, host paths are rewritten, and assets
// missing from the archive are marked for regeneration. Runtime state needs
// no reset here because restore only imports archived tables from this
// catalog.
func rewriteRestoredDatabase(ctx context.Context, databasePath string, rewrite catalogRewrite) (ValidationReport, error) {
	rewrite.sourceDataRoot = cleanNonEmptyPath(rewrite.sourceDataRoot)
	rewrite.sourcePreviewRoot = cleanNonEmptyPath(rewrite.sourcePreviewRoot)
	rewrite.targetDataRoot = cleanNonEmptyPath(rewrite.targetDataRoot)
	rewrite.targetAssetRoot = cleanNonEmptyPath(rewrite.targetAssetRoot)
	rewrite.targetPreviewRoot = cleanNonEmptyPath(rewrite.targetPreviewRoot)
	if rewrite.sourcePreviewRoot == "" {
		return ValidationReport{}, errors.New("backup: source preview root is missing")
	}

	database, err := sql.Open("sqlite", databasePath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return ValidationReport{}, err
	}
	defer database.Close()
	if err := catalog.MigrateImportDatabase(ctx, database); err != nil {
		return ValidationReport{}, err
	}
	if err := catalog.MigrateMergedVideos(ctx, database); err != nil {
		return ValidationReport{}, err
	}
	if err := catalog.NormalizeVideoContentHashes(ctx, database); err != nil {
		return ValidationReport{}, err
	}
	if err := catalog.RetireLegacyFingerprints(ctx, database); err != nil {
		return ValidationReport{}, err
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return ValidationReport{}, err
	}
	defer tx.Rollback()

	report := ValidationReport{VerificationStatus: "verified"}
	var localVideoIDs map[string]string
	if rewrite.selection.LocalStorage {
		localVideoIDs, err = rewriteLocalStorageCatalog(ctx, tx, rewrite.localStoragePlans, rewrite.stagePreviewDir)
		if err != nil {
			return ValidationReport{}, err
		}
		for _, plan := range rewrite.localStoragePlans {
			report.PathRewrites = appendLimited(report.PathRewrites, fmt.Sprintf(
				"本地存储已恢复为新的独立存储 %s",
				plan.Name,
			))
		}
	}
	if err := rewriteVideoAssets(ctx, tx, rewrite, localVideoIDs, &report); err != nil {
		return ValidationReport{}, err
	}
	if err := rewriteDriveCredentials(ctx, tx, rewrite, &report); err != nil {
		return ValidationReport{}, err
	}
	if err := rewriteDeletedVideoPayloads(ctx, tx, rewrite, localVideoIDs, &report); err != nil {
		return ValidationReport{}, err
	}
	if err := tx.Commit(); err != nil {
		return ValidationReport{}, err
	}
	if err := verifySQLite(databasePath); err != nil {
		return ValidationReport{}, err
	}
	return report, nil
}

// rewriteVideoAssets points preview references at the target preview root
// and marks previews and thumbnails missing from the archive as pending.
func rewriteVideoAssets(
	ctx context.Context,
	tx *sql.Tx,
	rewrite catalogRewrite,
	localVideoIDs map[string]string,
	report *ValidationReport,
) error {
	previousLocalVideoIDs := make(map[string]string, len(localVideoIDs))
	for oldID, newID := range localVideoIDs {
		previousLocalVideoIDs[newID] = oldID
	}
	type videoAsset struct {
		id, previewLocal, previewStatus, thumbnailURL, thumbnailStatus string
	}
	rows, err := tx.QueryContext(ctx, `
SELECT id, COALESCE(preview_local, ''), COALESCE(preview_status, ''),
       COALESCE(thumbnail_url, ''), COALESCE(thumbnail_status, '')
  FROM videos`)
	if err != nil {
		return err
	}
	var videos []videoAsset
	for rows.Next() {
		var video videoAsset
		if err := rows.Scan(&video.id, &video.previewLocal, &video.previewStatus, &video.thumbnailURL, &video.thumbnailStatus); err != nil {
			rows.Close()
			return err
		}
		videos = append(videos, video)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	stagedAssetExists := func(path string) bool {
		info, err := os.Stat(path)
		return err == nil && info.Mode().IsRegular() && info.Size() > 0
	}
	missingCount := 0
	for _, video := range videos {
		rewrittenPreview := rewrite.hostPath(video.previewLocal)
		if previousID := previousLocalVideoIDs[video.id]; previousID != "" {
			rewrittenPreview = remapLocalPreviewPath(
				video.previewLocal,
				rewrittenPreview,
				previousID,
				video.id,
				rewrite.sourcePreviewRoot,
				rewrite.targetPreviewRoot,
			)
			if strings.HasPrefix(video.thumbnailURL, "/p/thumb/") {
				video.thumbnailURL = "/p/thumb/" + video.id
			}
		}
		if rewrittenPreview != video.previewLocal && video.previewLocal != "" {
			report.PathRewrites = appendLimited(report.PathRewrites, fmt.Sprintf(
				"视频 %s 的预览路径已改写到目标数据目录",
				video.id,
			))
		}
		if strings.EqualFold(video.previewStatus, "ready") {
			relative, ok := localpath.ManagedRelative(rewrite.targetPreviewRoot, rewrittenPreview)
			if !ok {
				relative, ok = localpath.ManagedRelative(rewrite.sourcePreviewRoot, video.previewLocal)
			}
			if video.previewLocal == "" || !ok || !stagedAssetExists(filepath.Join(rewrite.stagePreviewDir, relative)) {
				rewrittenPreview = ""
				video.previewStatus = "pending"
				missingCount++
				report.MissingAssets = appendLimited(report.MissingAssets, fmt.Sprintf("视频 %s 的预览文件在源备份中不存在，已标记待生成", video.id))
			}
		}
		if strings.EqualFold(video.thumbnailStatus, "ready") &&
			strings.HasPrefix(video.thumbnailURL, "/p/thumb/") &&
			!stagedAssetExists(mediaasset.ThumbnailPath(rewrite.stagePreviewDir, video.id)) {
			video.thumbnailURL = ""
			video.thumbnailStatus = "pending"
			missingCount++
			report.MissingAssets = appendLimited(report.MissingAssets, fmt.Sprintf("视频 %s 的封面在源备份中不存在，已标记待生成", video.id))
		}
		if relative, ok := localpath.ManagedRelative(rewrite.targetPreviewRoot, rewrittenPreview); ok {
			rewrittenPreview = relative
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE videos
   SET preview_local = ?, preview_status = ?,
       thumbnail_url = ?, thumbnail_status = ?,
       thumbnail_source = CASE WHEN ? = '' THEN '' ELSE thumbnail_source END
 WHERE id = ?`,
			rewrittenPreview,
			video.previewStatus,
			video.thumbnailURL,
			video.thumbnailStatus,
			video.thumbnailURL,
			video.id,
		); err != nil {
			return err
		}
	}
	if missingCount > len(report.MissingAssets) {
		report.Warnings = append(report.Warnings, fmt.Sprintf("另有 %d 项缺失资源已标记待生成", missingCount-len(report.MissingAssets)))
	}
	return nil
}

// rewriteDriveCredentials points crawler scripts at the target asset root and
// connects local storage drives to their restored or configured paths.
func rewriteDriveCredentials(ctx context.Context, tx *sql.Tx, rewrite catalogRewrite, report *ValidationReport) error {
	type driveState struct {
		id, kind string
		creds    map[string]string
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, COALESCE(credentials, '{}') FROM drives`)
	if err != nil {
		return err
	}
	var drives []driveState
	for rows.Next() {
		var drive driveState
		var credentialsJSON string
		if err := rows.Scan(&drive.id, &drive.kind, &credentialsJSON); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(credentialsJSON), &drive.creds); err != nil {
			rows.Close()
			return fmt.Errorf("backup: invalid drive credentials for %s: %w", drive.id, err)
		}
		if drive.creds == nil {
			drive.creds = make(map[string]string)
		}
		drives = append(drives, drive)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	localStorageTargets := make(map[string]string, len(rewrite.localStoragePlans))
	for _, plan := range rewrite.localStoragePlans {
		localStorageTargets[plan.DriveID] = plan.TargetPath
	}
	for _, drive := range drives {
		if scriptPath := drive.creds["script_path"]; scriptPath != "" {
			rewritten := rewrite.hostPath(scriptPath)
			if rewritten != scriptPath {
				drive.creds["script_path"] = rewritten
				report.PathRewrites = appendLimited(report.PathRewrites, fmt.Sprintf("爬虫 %s 的脚本路径已改写", drive.id))
			}
		}
		status := ""
		lastError := ""
		if strings.EqualFold(drive.kind, "localstorage") {
			localPath := strings.TrimSpace(drive.creds["path"])
			if targetPath, restored := localStorageTargets[drive.id]; restored {
				drive.creds["path"] = targetPath
				status = "ok"
			} else if localPath != "" {
				if info, statErr := os.Stat(localPath); statErr != nil || !info.IsDir() {
					status = "disconnected"
					lastError = "恢复后目标服务器找不到本地存储路径：" + localPath
					report.LocalStorageWarnings = appendLimited(report.LocalStorageWarnings, fmt.Sprintf("本地存储 %s：目标路径 %s 不存在，已标记未连接", drive.id, localPath))
				}
			}
		}
		credentialsJSON, err := json.Marshal(drive.creds)
		if err != nil {
			return err
		}
		if status != "" {
			_, err = tx.ExecContext(ctx, `
UPDATE drives SET credentials = ?, status = ?, last_error = ? WHERE id = ?`,
				string(credentialsJSON), status, lastError, drive.id)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE drives SET credentials = ? WHERE id = ?`, string(credentialsJSON), drive.id)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// rewriteDeletedVideoPayloads keeps soft-deleted videos restorable on the
// target by rewriting the host paths and local video IDs of their payloads.
func rewriteDeletedVideoPayloads(
	ctx context.Context,
	tx *sql.Tx,
	rewrite catalogRewrite,
	localVideoIDs map[string]string,
	report *ValidationReport,
) error {
	rows, err := tx.QueryContext(ctx, `
SELECT id, COALESCE(restore_payload, '') FROM deleted_videos WHERE COALESCE(restore_payload, '') != ''`)
	if err != nil {
		return err
	}
	type deletedPayload struct{ id, payload string }
	var deleted []deletedPayload
	for rows.Next() {
		var item deletedPayload
		if err := rows.Scan(&item.id, &item.payload); err != nil {
			rows.Close()
			return err
		}
		deleted = append(deleted, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	rewriteValue := func(value string) string {
		if mapped, ok := localVideoIDs[value]; ok {
			return mapped
		}
		return rewrite.hostPath(value)
	}
	for _, item := range deleted {
		var payload any
		if err := json.Unmarshal([]byte(item.payload), &payload); err != nil {
			continue
		}
		if !rewriteJSONStrings(&payload, rewriteValue) {
			continue
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE deleted_videos SET restore_payload = ? WHERE id = ?`, string(encoded), item.id); err != nil {
			return err
		}
		report.PathRewrites = appendLimited(report.PathRewrites, fmt.Sprintf("删除记录 %s 的恢复载荷路径已改写", item.id))
	}
	return nil
}

type pathRewrite struct{ from, to string }

func rewriteRestoredPath(value string, rewrites []pathRewrite) string {
	value = strings.TrimSpace(value)
	if value == "" || !filepath.IsAbs(value) {
		// Portable file references and other relative strings have no host
		// directory to rewrite. In particular, never resolve them against CWD.
		return value
	}
	clean := filepath.Clean(value)
	for _, rewrite := range rewrites {
		if rewrite.from == "" || rewrite.to == "" {
			continue
		}
		relative, ok := localpath.RelativeWithin(rewrite.from, clean)
		if !ok {
			continue
		}
		return filepath.Join(rewrite.to, relative)
	}
	return value
}

func cleanNonEmptyPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return filepath.Clean(value)
	}
	return filepath.Clean(absolute)
}

func rewriteJSONStrings(value *any, rewrite func(string) string) bool {
	if value == nil {
		return false
	}
	switch typed := (*value).(type) {
	case string:
		rewritten := rewrite(typed)
		if rewritten != typed {
			*value = rewritten
			return true
		}
	case []any:
		changed := false
		for index := range typed {
			if rewriteJSONStrings(&typed[index], rewrite) {
				changed = true
			}
		}
		return changed
	case map[string]any:
		changed := false
		for key, child := range typed {
			if rewriteJSONStrings(&child, rewrite) {
				typed[key] = child
				changed = true
			}
		}
		return changed
	}
	return false
}

func appendLimited(values []string, value string) []string {
	if len(values) >= 100 {
		return values
	}
	return append(values, value)
}
