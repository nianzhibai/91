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
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
	"github.com/nianzhibai/91/backend/internal/localpath"
	"github.com/nianzhibai/91/backend/internal/mediaasset"
)

type restoreMarker struct {
	MarkerVersion int              `json:"markerVersion"`
	BackupID      string           `json:"backupId"`
	DataRoot      string           `json:"dataRoot"`
	StageRoot     string           `json:"stageRoot"`
	CreatedAt     time.Time        `json:"createdAt"`
	State         string           `json:"state"`
	Operations    []restoreSwitch  `json:"operations"`
	Report        ValidationReport `json:"report"`
	LastError     string           `json:"lastError,omitempty"`
}

type restoreSwitch struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Staged    string `json:"staged"`
	Target    string `json:"target"`
	Rollback  string `json:"rollback"`
	State     string `json:"state"`
	HadTarget bool   `json:"hadTarget"`
}

func (m *Manager) PrepareRestore(ctx context.Context, id string) (ValidationReport, error) {
	if m == nil {
		return ValidationReport{}, errors.New("backup: manager is nil")
	}
	m.mu.Lock()
	running := m.current != nil &&
		(m.current.State == "queued" || m.current.State == "running" || m.current.State == "canceling")
	if m.restoreBusy {
		m.mu.Unlock()
		return ValidationReport{}, ErrRestorePending
	}
	m.restoreBusy = true
	m.mu.Unlock()
	m.setRestoreProgress(OperationProgress{Phase: "inspecting"})
	keepRestoreProgress := false
	defer func() {
		m.mu.Lock()
		m.restoreBusy = false
		m.mu.Unlock()
		if !keepRestoreProgress {
			m.clearRestoreProgress()
		}
	}()
	if running {
		return ValidationReport{}, ErrTaskRunning
	}
	if _, err := os.Stat(m.pendingPath); err == nil {
		return ValidationReport{}, ErrRestorePending
	} else if !os.IsNotExist(err) {
		return ValidationReport{}, err
	}
	archivePath, _, err := m.resolveBackup(id)
	if err != nil {
		return ValidationReport{}, err
	}
	available, err := m.availableBytes(m.dataRoot)
	if err != nil {
		return ValidationReport{}, err
	}
	stageID, err := randomID()
	if err != nil {
		return ValidationReport{}, err
	}
	stageRoot := filepath.Join(m.restoreDir, stageID)
	if err := os.MkdirAll(stageRoot, 0o700); err != nil {
		return ValidationReport{}, err
	}
	prepared := false
	maintenanceAcquired := false
	defer func() {
		if !prepared {
			if maintenanceAcquired {
				m.releaseRestoreMaintenance()
			}
			_ = os.RemoveAll(stageRoot)
		}
	}()
	report, err := VerifyAndExtractArchive(ctx, archivePath, stageRoot, VerifyOptions{
		CurrentVersion: m.appVersion,
		AvailableBytes: available,
		Progress: func(progress OperationProgress) {
			switch progress.Phase {
			case "database":
				progress.Phase = "checking-database"
			default:
				progress.Phase = "extracting"
			}
			m.setRestoreProgress(progress)
		},
	})
	if err != nil {
		return ValidationReport{}, err
	}
	selection := *report.Manifest.Selection

	m.setRestoreProgress(OperationProgress{
		Phase:          "rewriting",
		ProcessedBytes: report.Manifest.TotalSize,
		TotalBytes:     report.Manifest.TotalSize,
		ProcessedFiles: report.Manifest.FileCount,
		TotalFiles:     report.Manifest.FileCount,
	})
	targetConfig := *m.appConfig
	targetRuntimeConfig := targetConfig
	targetRuntimeConfig.Storage = config.Storage{
		DataDir:         m.dataRoot,
		DBDir:           filepath.Dir(m.dbPath),
		DBPath:          m.dbPath,
		LocalPreviewDir: m.previewPath,
	}

	stageDatabase := filepath.Join(stageRoot, "payload", "database.sqlite")
	stagePreview := filepath.Join(stageRoot, "payload", "previews")
	for _, dir := range []string{
		stagePreview,
		filepath.Join(stageRoot, "payload", "uploads"),
		filepath.Join(stageRoot, "payload", "crawler-scripts"),
		filepath.Join(stageRoot, "payload", "scriptcrawlers"),
		filepath.Join(stageRoot, "payload", "spider91"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return ValidationReport{}, err
		}
	}
	var localStoragePlans []isolatedLocalStorageRestore
	if selection.LocalStorage {
		localStoragePlans, err = m.planIsolatedLocalStorageRestore(ctx, stageID, report.Manifest.LocalStorage)
		if err != nil {
			return ValidationReport{}, err
		}
	}
	rewriteReport, err := rewriteRestoredDatabase(
		ctx,
		stageDatabase,
		selection,
		stagePreview,
		report.Manifest.SourceDataRoot,
		targetRuntimeConfig,
		m.dataRoot,
		m.assetRoot,
		report.Manifest.SourcePreview,
		localStoragePlans,
	)
	if err != nil {
		return ValidationReport{}, err
	}
	report.PathRewrites = append(report.PathRewrites, rewriteReport.PathRewrites...)
	report.LocalStorageWarnings = append(report.LocalStorageWarnings, rewriteReport.LocalStorageWarnings...)
	report.MissingAssets = append(report.MissingAssets, rewriteReport.MissingAssets...)
	report.Warnings = append(report.Warnings, rewriteReport.Warnings...)
	mergedDatabase := filepath.Join(stageRoot, "payload", "database-merged.sqlite")
	if err := m.beginRestoreMaintenance(ctx); err != nil {
		return ValidationReport{}, err
	}
	maintenanceAcquired = true
	mergeErr := m.catalog.BackupTo(ctx, mergedDatabase)
	if mergeErr != nil {
		return ValidationReport{}, fmt.Errorf("backup: snapshot current database for selective restore: %w", mergeErr)
	}
	sourceReplacementPreviewPaths, err := collectPreviewAssetPaths(ctx, stageDatabase, stagePreview, false)
	if err != nil {
		return ValidationReport{}, fmt.Errorf("backup: inspect restored preview ownership: %w", err)
	}
	targetMergedPreviewPaths, err := collectPreviewAssetPaths(
		ctx,
		mergedDatabase,
		targetRuntimeConfig.Storage.LocalPreviewDir,
		true,
	)
	if err != nil {
		return ValidationReport{}, fmt.Errorf("backup: inspect target preview ownership: %w", err)
	}
	if err := mergeSelectiveRestoreDatabase(ctx, mergedDatabase, stageDatabase, selection); err != nil {
		return ValidationReport{}, err
	}
	if err := os.Remove(stageDatabase); err != nil {
		return ValidationReport{}, err
	}
	if err := os.Rename(mergedDatabase, stageDatabase); err != nil {
		return ValidationReport{}, err
	}

	m.setRestoreProgress(OperationProgress{
		Phase:          "preparing-switch",
		ProcessedBytes: report.Manifest.TotalSize,
		TotalBytes:     report.Manifest.TotalSize,
		ProcessedFiles: report.Manifest.FileCount,
		TotalFiles:     report.Manifest.FileCount,
	})
	previewSource := stagePreview
	resourceSelection := selection.CloudDrives || selection.CrawlerScripts || selection.UploadStorage || selection.LocalStorage
	allResourceSelections := selection.CloudDrives && selection.CrawlerScripts && selection.UploadStorage && selection.LocalStorage
	if resourceSelection && manifestIncludes(report.Manifest, "previews") {
		previewSource, err = prepareMergedPreview(
			ctx,
			stageRoot,
			stagePreview,
			targetRuntimeConfig.Storage.LocalPreviewDir,
			targetMergedPreviewPaths,
			!allResourceSelections,
			sourceReplacementPreviewPaths,
		)
		if err != nil {
			return ValidationReport{}, err
		}
	}
	uploadSource := filepath.Join(stageRoot, "payload", "uploads")
	if selection.UploadStorage && manifestIncludes(report.Manifest, "uploads") {
		uploadSource, err = prepareMergedUploadStorage(
			ctx,
			stageRoot,
			uploadSource,
			filepath.Join(m.assetRoot, "uploads"),
		)
		if err != nil {
			return ValidationReport{}, err
		}
	}
	localStorageSources := []preparedLocalStorage(nil)
	if selection.LocalStorage && len(localStoragePlans) > 0 {
		localStorageSources, err = prepareIsolatedLocalStorage(
			ctx,
			stageRoot,
			report.Manifest.LocalStorage,
			localStoragePlans,
		)
		if err != nil {
			return ValidationReport{}, err
		}
	}
	targets := []struct {
		name   string
		kind   string
		source string
		target string
	}{}
	if resourceSelection && manifestIncludes(report.Manifest, "previews") {
		targets = append(targets, struct {
			name   string
			kind   string
			source string
			target string
		}{name: "previews", kind: "dir", source: previewSource, target: targetRuntimeConfig.Storage.LocalPreviewDir})
	}
	if selection.UploadStorage && manifestIncludes(report.Manifest, "uploads") {
		targets = append(targets, struct {
			name   string
			kind   string
			source string
			target string
		}{name: "uploads", kind: "dir", source: uploadSource, target: filepath.Join(m.assetRoot, "uploads")})
	}
	if selection.CrawlerScripts {
		for _, item := range []struct {
			name string
		}{
			{name: "crawler-scripts"},
			{name: "scriptcrawlers"},
			{name: "spider91"},
		} {
			if !manifestIncludes(report.Manifest, item.name) {
				continue
			}
			targets = append(targets, struct {
				name   string
				kind   string
				source string
				target string
			}{
				name:   item.name,
				kind:   "dir",
				source: filepath.Join(stageRoot, "payload", item.name),
				target: filepath.Join(m.assetRoot, item.name),
			})
		}
	}
	for _, localStorage := range localStorageSources {
		targets = append(targets, struct {
			name   string
			kind   string
			source string
			target string
		}{
			name:   "localstorage-" + archiveDriveComponent(localStorage.DriveID),
			kind:   "dir",
			source: localStorage.Source,
			target: localStorage.Target,
		})
	}
	targets = append(targets,
		struct {
			name   string
			kind   string
			source string
			target string
		}{name: "database-wal", kind: "remove", target: targetRuntimeConfig.Storage.DBPath + "-wal"},
		struct {
			name   string
			kind   string
			source string
			target string
		}{name: "database-shm", kind: "remove", target: targetRuntimeConfig.Storage.DBPath + "-shm"},
		struct {
			name   string
			kind   string
			source string
			target string
		}{name: "database", kind: "file", source: stageDatabase, target: targetRuntimeConfig.Storage.DBPath},
	)
	if err := validateRestoreOperationCount(len(targets)); err != nil {
		return ValidationReport{}, err
	}
	for index := range targets {
		for prior := 0; prior < index; prior++ {
			if restoreTargetPathsOverlap(targets[index].target, targets[prior].target) {
				return ValidationReport{}, fmt.Errorf("backup: restore targets overlap: %s and %s", targets[prior].name, targets[index].name)
			}
		}
	}
	markerReport := report
	markerReport.Manifest.Files = nil
	marker := restoreMarker{
		MarkerVersion: 1,
		BackupID:      id,
		DataRoot:      m.dataRoot,
		StageRoot:     stageRoot,
		CreatedAt:     m.nowTime(),
		State:         "pending",
		Report:        markerReport,
	}
	for _, target := range targets {
		operation, err := prepareRestoreSwitch(stageID, target.name, target.kind, target.source, target.target)
		if err != nil {
			cleanupPreparedSwitches(marker.Operations)
			return ValidationReport{}, err
		}
		marker.Operations = append(marker.Operations, operation)
	}
	if err := writeJSONAtomic(m.pendingPath, marker, 0o600); err != nil {
		cleanupPreparedSwitches(marker.Operations)
		return ValidationReport{}, err
	}
	prepared = true
	keepRestoreProgress = true
	m.setRestoreProgress(OperationProgress{
		Phase:          "ready",
		ProcessedBytes: report.Manifest.TotalSize,
		TotalBytes:     report.Manifest.TotalSize,
		ProcessedFiles: report.Manifest.FileCount,
		TotalFiles:     report.Manifest.FileCount,
	})
	return report, nil
}

func prepareRestoreSwitch(stageID, name, kind, source, target string) (restoreSwitch, error) {
	target, err := filepath.Abs(strings.TrimSpace(target))
	if err != nil || target == "" {
		return restoreSwitch{}, fmt.Errorf("backup: invalid restore target for %s", name)
	}
	if target == string(filepath.Separator) || filepath.Dir(target) == target {
		return restoreSwitch{}, fmt.Errorf("backup: unsafe restore target for %s", name)
	}
	if kind != "file" && kind != "dir" && kind != "remove" {
		return restoreSwitch{}, fmt.Errorf("backup: invalid restore operation kind %q", kind)
	}
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return restoreSwitch{}, err
	}
	base := filepath.Base(target)
	staged := filepath.Join(parent, "."+base+".restore-stage-"+stageID)
	rollback := filepath.Join(parent, "."+base+".restore-rollback-"+stageID)
	for _, candidate := range []string{staged, rollback} {
		if _, err := os.Lstat(candidate); err == nil {
			return restoreSwitch{}, fmt.Errorf("backup: restore staging path already exists: %s", candidate)
		} else if !os.IsNotExist(err) {
			return restoreSwitch{}, err
		}
	}
	if kind == "remove" {
		if err := os.WriteFile(staged, []byte("remove\n"), 0o600); err != nil {
			return restoreSwitch{}, fmt.Errorf("backup: prepare %s removal: %w", name, err)
		}
	} else {
		if err := moveOrCopyRestoreSource(source, staged, kind); err != nil {
			return restoreSwitch{}, fmt.Errorf("backup: prepare %s switch: %w", name, err)
		}
	}
	return restoreSwitch{
		Name:     name,
		Kind:     kind,
		Staged:   staged,
		Target:   target,
		Rollback: rollback,
		State:    "pending",
	}, nil
}

func moveOrCopyRestoreSource(source, destination, kind string) error {
	if err := os.Rename(source, destination); err == nil {
		return nil
	}
	if kind == "file" {
		info, err := os.Stat(source)
		if err != nil {
			return err
		}
		if err := linkOrCopy(source, destination, info.Mode().Perm()); err != nil {
			return err
		}
		return os.Remove(source)
	}
	if err := copyDirectory(source, destination); err != nil {
		return err
	}
	return os.RemoveAll(source)
}

func copyDirectory(source, destination string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("backup: restore source is not a directory")
	}
	if err := os.Mkdir(destination, info.Mode().Perm()); err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(destination)
		}
	}()
	err = filepath.WalkDir(source, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == source {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("backup: symbolic link appeared in restore staging")
		}
		relative, err := filepath.Rel(source, current)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Mkdir(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return errors.New("backup: non-regular file appeared in restore staging")
		}
		return linkOrCopy(current, target, info.Mode().Perm())
	})
	if err != nil {
		return err
	}
	success = true
	return nil
}

func cleanupPreparedSwitches(operations []restoreSwitch) {
	for _, operation := range operations {
		_ = os.RemoveAll(operation.Staged)
	}
}

func rewriteRestoredDatabase(
	ctx context.Context,
	databasePath string,
	selection BackupSelection,
	stagePreviewDir string,
	sourceDataRoot string,
	targetConfig config.Config,
	targetDataRoot string,
	targetAssetRoot string,
	sourcePreviewOverride string,
	localStoragePlans []isolatedLocalStorageRestore,
) (ValidationReport, error) {
	dsn := databasePath + "?_pragma=busy_timeout(5000)"
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return ValidationReport{}, err
	}
	defer database.Close()
	if err := catalog.MigrateImportDatabase(ctx, database); err != nil {
		return ValidationReport{}, err
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return ValidationReport{}, err
	}
	defer tx.Rollback()

	report := ValidationReport{VerificationStatus: "verified"}
	sourceDataRoot = cleanNonEmptyPath(sourceDataRoot)
	sourcePreviewRoot := cleanNonEmptyPath(sourcePreviewOverride)
	if sourcePreviewRoot == "" {
		return ValidationReport{}, errors.New("backup: source preview root is missing")
	}
	sourceAssetRoot := cleanNonEmptyPath(filepath.Dir(sourcePreviewRoot))
	targetPreviewRoot := cleanNonEmptyPath(targetConfig.Storage.LocalPreviewDir)
	targetDataRoot = cleanNonEmptyPath(targetDataRoot)
	targetAssetRoot = cleanNonEmptyPath(targetAssetRoot)
	rewrite := func(value string) string {
		return rewriteRestoredPath(value, []pathRewrite{
			{from: sourcePreviewRoot, to: targetPreviewRoot},
			{from: sourceAssetRoot, to: targetAssetRoot},
			{from: sourceDataRoot, to: targetDataRoot},
		})
	}
	localVideoIDs := make(map[string]string)
	if selection.LocalStorage {
		localVideoIDs, err = rewriteLocalStorageCatalog(ctx, tx, localStoragePlans, stagePreviewDir)
		if err != nil {
			return ValidationReport{}, err
		}
		for _, plan := range localStoragePlans {
			report.PathRewrites = appendLimited(report.PathRewrites, fmt.Sprintf(
				"本地存储已恢复为新的独立存储 %s",
				plan.Name,
			))
		}
	}
	previousLocalVideoIDs := make(map[string]string, len(localVideoIDs))
	for oldID, newID := range localVideoIDs {
		previousLocalVideoIDs[newID] = oldID
	}

	rows, err := tx.QueryContext(ctx, `
SELECT id, COALESCE(preview_local, ''), COALESCE(preview_status, ''),
       COALESCE(thumbnail_url, ''), COALESCE(thumbnail_status, ''),
       COALESCE(fingerprint_status, '')
  FROM videos`)
	if err != nil {
		return ValidationReport{}, err
	}
	type videoAsset struct {
		id, previewLocal, previewStatus, thumbnailURL, thumbnailStatus, fingerprintStatus string
	}
	var videos []videoAsset
	for rows.Next() {
		var video videoAsset
		if err := rows.Scan(
			&video.id,
			&video.previewLocal,
			&video.previewStatus,
			&video.thumbnailURL,
			&video.thumbnailStatus,
			&video.fingerprintStatus,
		); err != nil {
			rows.Close()
			return ValidationReport{}, err
		}
		videos = append(videos, video)
	}
	if err := rows.Close(); err != nil {
		return ValidationReport{}, err
	}
	missingCount := 0
	for _, video := range videos {
		rewrittenPreview := rewrite(video.previewLocal)
		if previousID := previousLocalVideoIDs[video.id]; previousID != "" {
			rewrittenPreview = remapLocalPreviewPath(
				video.previewLocal,
				rewrittenPreview,
				previousID,
				video.id,
				sourcePreviewRoot,
				targetPreviewRoot,
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
		previewMissing := false
		if strings.EqualFold(video.previewStatus, "ready") {
			if video.previewLocal == "" {
				previewMissing = true
			} else {
				relative, ok := localpath.ManagedRelative(targetPreviewRoot, rewrittenPreview)
				if !ok {
					relative, ok = localpath.ManagedRelative(sourcePreviewRoot, video.previewLocal)
				}
				if !ok {
					previewMissing = true
				} else if info, statErr := os.Stat(filepath.Join(stagePreviewDir, relative)); statErr != nil ||
					!info.Mode().IsRegular() || info.Size() <= 0 {
					previewMissing = true
				}
			}
		}
		if previewMissing {
			rewrittenPreview = ""
			video.previewStatus = "pending"
			missingCount++
			report.MissingAssets = appendLimited(report.MissingAssets, fmt.Sprintf("视频 %s 的预览文件在源备份中不存在，已标记待生成", video.id))
		}
		thumbnailMissing := false
		if strings.EqualFold(video.thumbnailStatus, "ready") &&
			strings.HasPrefix(video.thumbnailURL, "/p/thumb/") {
			info, statErr := os.Stat(mediaasset.ThumbnailPath(stagePreviewDir, video.id))
			thumbnailMissing = statErr != nil || !info.Mode().IsRegular() || info.Size() <= 0
		}
		if thumbnailMissing {
			video.thumbnailURL = ""
			video.thumbnailStatus = "pending"
			missingCount++
			report.MissingAssets = appendLimited(report.MissingAssets, fmt.Sprintf("视频 %s 的封面在源备份中不存在，已标记待生成", video.id))
		}
		if relative, ok := localpath.ManagedRelative(targetPreviewRoot, rewrittenPreview); ok {
			rewrittenPreview = relative
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE videos
   SET preview_local = ?, preview_status = ?,
       thumbnail_url = ?, thumbnail_status = ?
 WHERE id = ?`,
			rewrittenPreview,
			video.previewStatus,
			video.thumbnailURL,
			video.thumbnailStatus,
			video.id,
		); err != nil {
			return ValidationReport{}, err
		}
	}
	if missingCount > len(report.MissingAssets) {
		report.Warnings = append(report.Warnings, fmt.Sprintf("另有 %d 项缺失资源已标记待生成", missingCount-len(report.MissingAssets)))
	}

	driveRows, err := tx.QueryContext(ctx, `SELECT id, kind, COALESCE(credentials, '{}') FROM drives`)
	if err != nil {
		return ValidationReport{}, err
	}
	type driveState struct {
		id, kind string
		creds    map[string]string
	}
	var drives []driveState
	for driveRows.Next() {
		var drive driveState
		var credentialsJSON string
		if err := driveRows.Scan(&drive.id, &drive.kind, &credentialsJSON); err != nil {
			driveRows.Close()
			return ValidationReport{}, err
		}
		if err := json.Unmarshal([]byte(credentialsJSON), &drive.creds); err != nil {
			driveRows.Close()
			return ValidationReport{}, fmt.Errorf("backup: invalid drive credentials for %s: %w", drive.id, err)
		}
		if drive.creds == nil {
			drive.creds = make(map[string]string)
		}
		drives = append(drives, drive)
	}
	if err := driveRows.Close(); err != nil {
		return ValidationReport{}, err
	}
	localStorageTargets := make(map[string]string, len(localStoragePlans))
	for _, plan := range localStoragePlans {
		localStorageTargets[plan.DriveID] = plan.TargetPath
	}
	for _, drive := range drives {
		if scriptPath := drive.creds["script_path"]; scriptPath != "" {
			rewritten := rewrite(scriptPath)
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
				localPath = targetPath
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
			return ValidationReport{}, err
		}
		if status != "" {
			_, err = tx.ExecContext(ctx, `
UPDATE drives SET credentials = ?, status = ?, last_error = ? WHERE id = ?`,
				string(credentialsJSON), status, lastError, drive.id)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE drives SET credentials = ? WHERE id = ?`, string(credentialsJSON), drive.id)
		}
		if err != nil {
			return ValidationReport{}, err
		}
	}

	deletedRows, err := tx.QueryContext(ctx, `
SELECT id, COALESCE(restore_payload, '') FROM deleted_videos WHERE COALESCE(restore_payload, '') != ''`)
	if err != nil {
		return ValidationReport{}, err
	}
	type deletedPayload struct{ id, payload string }
	var deleted []deletedPayload
	for deletedRows.Next() {
		var item deletedPayload
		if err := deletedRows.Scan(&item.id, &item.payload); err != nil {
			deletedRows.Close()
			return ValidationReport{}, err
		}
		deleted = append(deleted, item)
	}
	if err := deletedRows.Close(); err != nil {
		return ValidationReport{}, err
	}
	for _, item := range deleted {
		var payload any
		if err := json.Unmarshal([]byte(item.payload), &payload); err != nil {
			continue
		}
		rewritePayloadValue := func(value string) string {
			if mapped, ok := localVideoIDs[value]; ok {
				return mapped
			}
			return rewrite(value)
		}
		changed := rewriteJSONStrings(&payload, rewritePayloadValue)
		if !changed {
			continue
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return ValidationReport{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE deleted_videos SET restore_payload = ? WHERE id = ?`, string(encoded), item.id); err != nil {
			return ValidationReport{}, err
		}
		report.PathRewrites = appendLimited(report.PathRewrites, fmt.Sprintf("删除记录 %s 的恢复载荷路径已改写", item.id))
	}

	now := time.Now().UnixMilli()
	for _, statement := range []string{
		`DELETE FROM video_shares`,
		`DELETE FROM telegram_receipts`,
		`DELETE FROM telegram_media_group_updates`,
		`DELETE FROM telegram_connections`,
		`INSERT INTO telegram_connections(bot_id,needs_reconnect) VALUES(0,1)`,
		`DELETE FROM shorts_feed_sessions`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return ValidationReport{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE remote_upload_jobs
   SET state = 'canceled', source_url = '', cancel_requested = 1,
       temp_file = '', final_file = '', error_message = '恢复时已取消',
       updated_at = ?, finished_at = ?
 WHERE state IN ('queued', 'downloading', 'validating', 'saving')`, now, now); err != nil {
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
		relative, ok := relativeWithin(rewrite.from, clean)
		if !ok {
			continue
		}
		return filepath.Join(rewrite.to, relative)
	}
	return value
}

func relativeWithin(root, candidate string) (string, bool) {
	return localpath.RelativeWithin(root, candidate)
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
