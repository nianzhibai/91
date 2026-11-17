package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nianzhibai/91/backend/internal/atomicfile"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
	"github.com/nianzhibai/91/backend/internal/persistence"
)

const diskSafetyReserve int64 = 64 << 20

type Config struct {
	Catalog        *catalog.Catalog
	AppConfig      *config.Config
	RuntimeStorage config.Storage
	ConfigPath     string
	AppVersion     string
	RestartManaged bool
	Now            func() time.Time
	AvailableBytes func(path string) (int64, error)

	TelegramFilesRoot func() string
}

type Manager struct {
	catalog        *catalog.Catalog
	appConfig      *config.Config
	configPath     string
	appVersion     string
	dbPath         string
	previewPath    string
	dataRoot       string
	assetRoot      string
	backupDir      string
	snapshotDir    string
	restoreDir     string
	uploadRoot     string
	pendingPath    string
	restartManaged bool
	now            func() time.Time
	availableBytes func(string) (int64, error)

	telegramFilesRoot func() string
	uploads           *uploadRuntime

	mu              sync.Mutex
	current         *TaskStatus
	currentCancel   context.CancelFunc
	estimate        Estimate
	estimateUntil   time.Time
	restoreBusy     bool
	restoreProgress *OperationProgress
	restoreBarrier  *catalog.WriteBarrier
	restoreGateHeld bool
	closed          bool

	runCtx    context.Context
	runCancel context.CancelFunc
	restart   chan struct{}
}

func NewManager(cfg Config) (*Manager, error) {
	if cfg.Catalog == nil {
		return nil, errors.New("backup: catalog is required")
	}
	if cfg.AppConfig == nil {
		return nil, errors.New("backup: application config is required")
	}
	runtimeStorage := cfg.RuntimeStorage
	if strings.TrimSpace(runtimeStorage.DataDir) == "" {
		runtimeStorage = cfg.AppConfig.Storage
	}
	workingDir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("backup: resolve working directory: %w", err)
	}
	runtimeStorage, err = config.ResolveStoragePaths(runtimeStorage, workingDir)
	if err != nil {
		return nil, fmt.Errorf("backup: resolve data directory: %w", err)
	}
	dbPath, previewPath := runtimeStorage.DBPath, runtimeStorage.LocalPreviewDir
	configPath, err := filepath.Abs(strings.TrimSpace(cfg.ConfigPath))
	if err != nil || strings.TrimSpace(cfg.ConfigPath) == "" {
		return nil, errors.New("backup: config path is invalid")
	}
	dataRoot := runtimeStorage.DataDir
	assetRoot := filepath.Dir(previewPath)
	m := &Manager{
		catalog:        cfg.Catalog,
		appConfig:      cfg.AppConfig,
		configPath:     configPath,
		appVersion:     normalizedVersion(cfg.AppVersion),
		dbPath:         dbPath,
		previewPath:    previewPath,
		dataRoot:       dataRoot,
		assetRoot:      assetRoot,
		backupDir:      filepath.Join(dataRoot, "backups"),
		snapshotDir:    filepath.Join(dataRoot, ".backup-snapshots"),
		restoreDir:     filepath.Join(dataRoot, restoreStageDirName),
		restartManaged: cfg.RestartManaged,
		now:            cfg.Now,
		availableBytes: cfg.AvailableBytes,
		uploads:        newUploadRuntime(),
		restart:        make(chan struct{}, 1),
	}
	m.telegramFilesRoot = cfg.TelegramFilesRoot
	m.uploadRoot = filepath.Join(m.backupDir, ".uploads")
	if m.telegramFilesRoot == nil {
		m.telegramFilesRoot = func() string { return cfg.AppConfig.Telegram.LocalFilesRoot }
	}
	if m.availableBytes == nil {
		m.availableBytes = availableDiskBytes
	}
	m.pendingPath = filepath.Join(m.backupDir, ".restore-pending.json")
	m.runCtx, m.runCancel = context.WithCancel(context.Background())
	for _, dir := range []string{m.dataRoot, m.backupDir, m.snapshotDir, m.restoreDir, m.uploadRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			m.runCancel()
			return nil, fmt.Errorf("backup: create %s: %w", dir, err)
		}
	}
	if err := m.cleanupInterrupted(); err != nil {
		m.runCancel()
		return nil, err
	}
	if err := m.cleanupExpiredUploads(); err != nil {
		m.runCancel()
		return nil, err
	}
	return m, nil
}

func (m *Manager) Start(parent context.Context) {
	if m == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-parent.Done():
				m.runCancel()
				return
			case <-m.runCtx.Done():
				return
			case <-ticker.C:
				_ = m.cleanupExpiredUploads()
			}
		}
	}()
}

func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.closed = true
	runCancel := m.runCancel
	m.mu.Unlock()
	if runCancel != nil {
		runCancel()
	}
	m.releaseRestoreMaintenance()
}

func (m *Manager) beginRestoreMaintenance(ctx context.Context) error {
	persistence.Lock()
	barrier, err := m.catalog.BeginWriteBarrier(ctx)
	if err != nil {
		persistence.Unlock()
		return fmt.Errorf("backup: enter restore maintenance mode: %w", err)
	}
	m.mu.Lock()
	if m.closed || m.restoreBarrier != nil || m.restoreGateHeld {
		closed := m.closed
		m.mu.Unlock()
		_ = barrier.Close()
		persistence.Unlock()
		if closed {
			return errors.New("backup: manager is closed")
		}
		return ErrRestorePending
	}
	m.restoreBarrier = barrier
	m.restoreGateHeld = true
	m.mu.Unlock()
	return nil
}

func (m *Manager) releaseRestoreMaintenance() {
	if m == nil {
		return
	}
	m.mu.Lock()
	barrier := m.restoreBarrier
	gateHeld := m.restoreGateHeld
	m.restoreBarrier = nil
	m.restoreGateHeld = false
	m.mu.Unlock()
	if barrier != nil {
		_ = barrier.Close()
	}
	if gateHeld {
		persistence.Unlock()
	}
}

func (m *Manager) RestartRequested() <-chan struct{} {
	return m.restart
}

func (m *Manager) RequestRestart() {
	if m == nil {
		return
	}
	select {
	case m.restart <- struct{}{}:
	default:
	}
}

func (m *Manager) RestartManaged() bool {
	return m != nil && m.restartManaged
}

func (m *Manager) nowTime() time.Time {
	if m != nil && m.now != nil {
		return m.now().UTC()
	}
	return time.Now().UTC()
}

// Estimate reports the size of a full backup for the overview page. Local
// storage problems are ignored here and reported when a backup is created.
func (m *Manager) Estimate(ctx context.Context) (Estimate, error) {
	if m == nil {
		return Estimate{}, errors.New("backup: manager is nil")
	}
	now := m.nowTime()
	m.mu.Lock()
	if !m.estimateUntil.IsZero() && now.Before(m.estimateUntil) {
		cached := m.estimate
		m.mu.Unlock()
		return cached, nil
	}
	m.mu.Unlock()
	estimate, err := m.estimateSelection(ctx, FullBackupSelection(), false)
	if err != nil {
		return Estimate{}, err
	}
	m.mu.Lock()
	m.estimate = estimate
	m.estimateUntil = now.Add(30 * time.Second)
	m.mu.Unlock()
	return estimate, nil
}

// EstimateForSelection is the conservative preflight check for a backup. It
// charges whole selected directories; the final task size is replaced with
// the manifest size after snapshotting.
func (m *Manager) EstimateForSelection(ctx context.Context, selection BackupSelection) (Estimate, error) {
	if !selection.Any() {
		return Estimate{}, ErrNoBackupContent
	}
	return m.estimateSelection(ctx, selection, true)
}

func (m *Manager) estimateSelection(ctx context.Context, selection BackupSelection, strict bool) (Estimate, error) {
	// The archive contains one SQLite snapshot regardless of WAL presence.
	estimate := Estimate{FileCount: 1}
	for _, databasePath := range []string{m.dbPath, m.dbPath + "-wal"} {
		info, err := os.Lstat(databasePath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return Estimate{}, err
		}
		if info.Mode().IsRegular() {
			estimate.TotalBytes += info.Size()
		}
	}
	add := func(count int, size int64) {
		estimate.FileCount += count
		estimate.TotalBytes += size
	}
	for _, source := range selectedAssetSources(selection) {
		count, size, err := scanSource(ctx, m.assetPath(source.name))
		if err != nil {
			return Estimate{}, fmt.Errorf("backup: estimate %s: %w", source.name, err)
		}
		add(count, size)
	}
	if selection.UploadStorage {
		count, size, err := m.catalog.TelegramLocalStorageSize(ctx)
		if err != nil {
			return Estimate{}, err
		}
		add(count, size)
	}
	if selection.LocalStorage {
		count, size, err := m.estimateLocalStorageResources(ctx, strict)
		if err != nil {
			return Estimate{}, err
		}
		add(count, size)
	}
	available, err := m.availableBytes(m.dataRoot)
	if err != nil {
		return Estimate{}, fmt.Errorf("backup: inspect available space: %w", err)
	}
	estimate.AvailableBytes = available
	estimate.RequiredBytes = requiredBackupBytes(estimate.TotalBytes)
	return estimate, nil
}

func (m *Manager) estimateLocalStorageResources(ctx context.Context, strict bool) (int, int64, error) {
	drives, err := m.catalog.ListDrives(ctx)
	if err != nil {
		return 0, 0, err
	}
	var count int
	var size int64
	for _, drive := range drives {
		if drive == nil || !strings.EqualFold(strings.TrimSpace(drive.Kind), "localstorage") {
			continue
		}
		rawPath := strings.TrimSpace(drive.Credentials["path"])
		if rawPath == "" {
			if strict {
				return 0, 0, fmt.Errorf("backup: local storage %s has no configured path", drive.ID)
			}
			continue
		}
		root, err := resolveLocalStoragePath(rawPath)
		if err != nil {
			return 0, 0, fmt.Errorf("backup: estimate local storage %s: %w", drive.ID, err)
		}
		videos, err := m.catalog.ListVideosByDrive(ctx, drive.ID)
		if err != nil {
			return 0, 0, fmt.Errorf("backup: estimate local storage %s: %w", drive.ID, err)
		}
		seen := make(map[string]struct{})
		for _, video := range videos {
			if video == nil {
				continue
			}
			relative, decodeErr := decodeLocalStorageFileID(video.FileID)
			if decodeErr != nil {
				if strict {
					return 0, 0, fmt.Errorf("backup: decode local storage file for video %s: %w", video.ID, decodeErr)
				}
				continue
			}
			if relative == "" {
				continue
			}
			candidate := filepath.Join(root, filepath.FromSlash(relative))
			clean, _, info, ok, resolveErr := resolveContainedRegularFile(root, candidate)
			if resolveErr != nil {
				if strict {
					return 0, 0, fmt.Errorf("backup: inspect local storage file for video %s: %w", video.ID, resolveErr)
				}
				continue
			}
			if !ok {
				continue
			}
			if _, exists := seen[clean]; exists {
				continue
			}
			seen[clean] = struct{}{}
			count++
			size += info.Size()
		}
	}
	return count, size, nil
}

func requiredBackupBytes(total int64) int64 {
	if total < 0 {
		total = 0
	}
	if total > (1<<62)-diskSafetyReserve {
		return 1<<62 - 1
	}
	// The conservative 2x allowance covers the ZIP plus a copy fallback when
	// hard links are unavailable. Most deployments consume much less.
	if total > (1<<62-diskSafetyReserve)/2 {
		return 1<<62 - 1
	}
	return total*2 + diskSafetyReserve
}

func scanSource(ctx context.Context, root string) (int, int64, error) {
	info, err := os.Lstat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return 0, 0, nil
	}
	if info.Mode().IsRegular() {
		if excludedBackupFile(filepath.Base(root)) {
			return 0, 0, nil
		}
		return 1, info.Size(), nil
	}
	if !info.IsDir() {
		return 0, 0, nil
	}
	var count int
	var size int64
	err = filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if current == root {
			return nil
		}
		name := entry.Name()
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if excludedBackupDir(name) {
				return filepath.SkipDir
			}
			return nil
		}
		if excludedBackupFile(name) {
			return nil
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if fileInfo.Mode().IsRegular() {
			count++
			size += fileInfo.Size()
		}
		return nil
	})
	return count, size, err
}

func excludedBackupDir(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "backups", "upload-tmp", "transcode-tmp", ".backup-snapshots",
		".restore-staging", ".uploads", "__pycache__", ".cache", "cache",
		"node_modules":
		return true
	default:
		return false
	}
}

func excludedBackupFile(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return true
	}
	if lower == ".version" || lower == ".ds_store" || strings.HasSuffix(lower, ".pyc") {
		return true
	}
	if strings.HasPrefix(lower, ".thumbnail-") {
		return true
	}
	for _, suffix := range []string{".part", ".tmp", ".swp", "-wal", "-shm"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

func (m *Manager) Create(ctx context.Context, requested ...BackupSelection) (*TaskStatus, error) {
	selection := FullBackupSelection()
	if len(requested) > 0 {
		selection = requested[0]
		if !selection.Any() {
			return nil, ErrNoBackupContent
		}
	}
	estimate, err := m.EstimateForSelection(ctx, selection)
	if err != nil {
		return nil, err
	}
	if estimate.AvailableBytes < estimate.RequiredBytes {
		return nil, fmt.Errorf("%w：需要至少 %d 字节，可用 %d 字节", ErrInsufficientSpace, estimate.RequiredBytes, estimate.AvailableBytes)
	}
	m.mu.Lock()
	if m.restoreBusy {
		m.mu.Unlock()
		return nil, ErrRestorePending
	}
	if m.current != nil && (m.current.State == "queued" || m.current.State == "running" || m.current.State == "canceling") {
		m.mu.Unlock()
		return nil, ErrTaskRunning
	}
	if _, statErr := os.Stat(m.pendingPath); statErr == nil {
		m.mu.Unlock()
		return nil, ErrRestorePending
	} else if !os.IsNotExist(statErr) {
		m.mu.Unlock()
		return nil, statErr
	}
	id, err := randomID()
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	started := m.nowTime()
	taskCtx, cancel := context.WithCancel(m.runCtx)
	m.currentCancel = cancel
	m.current = &TaskStatus{
		ID:          id,
		State:       "queued",
		Phase:       "preparing",
		StartedAt:   started,
		Cancellable: true,
	}
	status := *m.current
	m.mu.Unlock()
	go m.runBackup(taskCtx, id, started, selection)
	return &status, nil
}

func (m *Manager) Current() *TaskStatus {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil {
		return nil
	}
	status := *m.current
	return &status
}

func (m *Manager) Cancel() error {
	if m == nil {
		return ErrNoRunningTask
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil || (m.current.State != "queued" && m.current.State != "running") || m.currentCancel == nil {
		return ErrNoRunningTask
	}
	m.current.State = "canceling"
	m.current.Phase = "canceling"
	m.current.Cancellable = false
	m.currentCancel()
	return nil
}

func (m *Manager) updateTask(id string, update func(*TaskStatus)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil || m.current.ID != id {
		return
	}
	update(m.current)
	if m.current.ProcessedBytes > 0 {
		elapsed := m.nowTime().Sub(m.current.StartedAt).Seconds()
		if elapsed > 0 {
			m.current.BytesPerSecond = int64(float64(m.current.ProcessedBytes) / elapsed)
		}
	}
}

func (m *Manager) finishTask(id, state string, err error) {
	m.updateTask(id, func(status *TaskStatus) {
		status.State = state
		status.Phase = state
		status.FinishedAt = m.nowTime()
		status.Cancellable = false
		if err != nil {
			status.Error = err.Error()
		}
	})
	m.mu.Lock()
	if m.current != nil && m.current.ID == id {
		m.currentCancel = nil
	}
	m.estimateUntil = time.Time{}
	m.mu.Unlock()
}

func (m *Manager) runBackup(ctx context.Context, id string, createdAt time.Time, selection BackupSelection) {
	m.updateTask(id, func(status *TaskStatus) {
		status.State = "running"
		status.Phase = "estimating"
	})
	estimate, err := m.EstimateForSelection(ctx, selection)
	if err != nil {
		m.finishTask(id, taskEndState(ctx, "failed"), err)
		return
	}
	if estimate.AvailableBytes < estimate.RequiredBytes {
		m.finishTask(id, "failed", fmt.Errorf("%w：需要至少 %d 字节，可用 %d 字节", ErrInsufficientSpace, estimate.RequiredBytes, estimate.AvailableBytes))
		return
	}
	m.updateTask(id, func(status *TaskStatus) {
		status.FileCount = estimate.FileCount
		status.TotalBytes = estimate.TotalBytes
		status.Phase = "snapshotting"
	})

	snapshotRoot := filepath.Join(m.snapshotDir, id)
	if err := os.MkdirAll(filepath.Join(snapshotRoot, "payload"), 0o700); err != nil {
		m.finishTask(id, "failed", err)
		return
	}
	defer os.RemoveAll(snapshotRoot)
	snapshotState, err := m.createSnapshot(ctx, snapshotRoot, selection)
	if err != nil {
		m.finishTask(id, taskEndState(ctx, "failed"), err)
		return
	}

	m.updateTask(id, func(status *TaskStatus) { status.Phase = "hashing" })
	manifest, err := m.buildManifest(ctx, id, createdAt, snapshotRoot, snapshotState)
	if err != nil {
		m.finishTask(id, taskEndState(ctx, "failed"), err)
		return
	}
	m.updateTask(id, func(status *TaskStatus) {
		status.FileCount = manifest.FileCount
		status.TotalBytes = manifest.TotalSize
		status.ProcessedBytes = 0
		status.ProcessedFiles = 0
		status.Phase = "compressing"
	})

	name := backupNamePrefix + createdAt.Local().Format("20060102-150405") + ".zip"
	finalPath := filepath.Join(m.backupDir, name)
	if _, statErr := os.Stat(finalPath); statErr == nil {
		name = strings.TrimSuffix(name, ".zip") + "-" + id[:8] + ".zip"
		finalPath = filepath.Join(m.backupDir, name)
	}
	partPath := finalPath + ".part"
	_ = os.Remove(partPath)
	archiveHash, archiveSize, err := writeArchive(ctx, partPath, snapshotRoot, manifest, func(fileBytes int64, fileDone bool) {
		m.updateTask(id, func(status *TaskStatus) {
			status.ProcessedBytes += fileBytes
			if fileDone {
				status.ProcessedFiles++
			}
		})
	})
	if err != nil {
		_ = os.Remove(partPath)
		m.finishTask(id, taskEndState(ctx, "failed"), err)
		return
	}

	m.updateTask(id, func(status *TaskStatus) { status.Phase = "verifying" })
	report, err := VerifyArchive(ctx, partPath, VerifyOptions{
		CurrentVersion: m.appVersion,
		TempDir:        m.snapshotDir,
		AvailableBytes: estimate.AvailableBytes,
	})
	if err != nil {
		_ = os.Remove(partPath)
		m.finishTask(id, taskEndState(ctx, "failed"), err)
		return
	}
	if err := os.Rename(partPath, finalPath); err != nil {
		_ = os.Remove(partPath)
		m.finishTask(id, "failed", fmt.Errorf("backup: publish archive: %w", err))
		return
	}
	info, err := os.Stat(finalPath)
	if err != nil {
		m.finishTask(id, "failed", err)
		return
	}
	meta := archiveMeta{
		ID:         strings.TrimSuffix(name, ".zip"),
		Name:       name,
		Size:       archiveSize,
		SHA256:     archiveHash,
		ModifiedAt: info.ModTime().UTC(),
		VerifiedAt: m.nowTime(),
		Manifest:   report.Manifest,
	}
	_ = atomicfile.WriteJSON(metaPath(finalPath), meta, 0o600)
	m.updateTask(id, func(status *TaskStatus) {
		status.Name = name
		status.ProcessedFiles = manifest.FileCount
		status.ProcessedBytes = manifest.TotalSize
	})
	m.finishTask(id, "completed", nil)
}

func taskEndState(ctx context.Context, fallback string) string {
	if ctx != nil && errors.Is(ctx.Err(), context.Canceled) {
		return "canceled"
	}
	return fallback
}

func (m *Manager) createSnapshot(
	ctx context.Context,
	snapshotRoot string,
	selection BackupSelection,
) (snapshotSelectionState, error) {
	persistence.Lock()
	defer persistence.Unlock()
	if err := ctx.Err(); err != nil {
		return snapshotSelectionState{}, err
	}
	dbDestination := filepath.Join(snapshotRoot, "payload", "database.sqlite")
	if err := m.catalog.BackupTo(ctx, dbDestination); err != nil {
		return snapshotSelectionState{}, err
	}
	state, err := filterSnapshotDatabase(ctx, dbDestination, selection)
	if err != nil {
		return snapshotSelectionState{}, err
	}
	if err := compactSnapshotDatabase(ctx, dbDestination); err != nil {
		return snapshotSelectionState{}, err
	}
	selection = state.Selection
	// A backup of every resource keeps whole asset directories, so files that
	// no live catalog row references, such as assets of soft-deleted videos,
	// are preserved as well.
	wholeDirectories := selection.AllResources()
	for _, source := range selectedAssetSources(selection) {
		if !wholeDirectories && source.snapshotSelected != nil {
			if err := source.snapshotSelected(m, ctx, snapshotRoot, state); err != nil {
				return snapshotSelectionState{}, err
			}
			continue
		}
		destination := filepath.Join(snapshotRoot, "payload", source.name)
		if err := snapshotSource(ctx, m.assetPath(source.name), destination); err != nil {
			return snapshotSelectionState{}, fmt.Errorf("backup: snapshot %s: %w", source.name, err)
		}
	}
	if selection.LocalStorage {
		if err := m.snapshotSelectedLocalStorage(ctx, snapshotRoot, state); err != nil {
			return snapshotSelectionState{}, err
		}
	}
	if selection.UploadStorage {
		if err := m.snapshotTelegramUploads(ctx, snapshotRoot, state); err != nil {
			return snapshotSelectionState{}, err
		}
	}
	return state, nil
}

func snapshotSource(ctx context.Context, source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		if os.IsNotExist(err) {
			// Keep an empty directory for optional persistent roots.
			if filepath.Ext(destination) == "" {
				return os.MkdirAll(destination, 0o755)
			}
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if info.Mode().IsRegular() {
		if excludedBackupFile(filepath.Base(source)) {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		return linkOrCopy(source, destination, info.Mode().Perm())
	}
	if !info.IsDir() {
		return nil
	}
	if err := os.MkdirAll(destination, info.Mode().Perm()); err != nil {
		return err
	}
	return filepath.WalkDir(source, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if current == source {
			return nil
		}
		relative, err := filepath.Rel(source, current)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if excludedBackupDir(entry.Name()) {
				return filepath.SkipDir
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if excludedBackupFile(entry.Name()) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return linkOrCopy(current, target, info.Mode().Perm())
	})
}

func linkOrCopy(source, destination string, mode os.FileMode) error {
	if err := os.Link(source, destination); err == nil {
		return nil
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(destination)
		return copyErr
	}
	if syncErr != nil {
		_ = os.Remove(destination)
		return syncErr
	}
	if closeErr != nil {
		_ = os.Remove(destination)
		return closeErr
	}
	return nil
}

func (m *Manager) buildManifest(
	ctx context.Context,
	id string,
	createdAt time.Time,
	snapshotRoot string,
	state snapshotSelectionState,
) (Manifest, error) {
	manifest := Manifest{
		FormatVersion:  FormatVersion,
		AppVersion:     m.appVersion,
		CreatedAt:      createdAt.UTC(),
		SourceDataRoot: filepath.Clean(m.dataRoot),
		SourceDBPath:   filepath.Clean(m.dbPath),
		SourcePreview:  filepath.Clean(m.previewPath),
		Selection:      &state.Selection,
	}
	for _, root := range state.LocalStorageRoots {
		manifest.LocalStorage = append(manifest.LocalStorage, root.LocalStorageRoot)
	}
	manifest.Included = includedForSelection(state.Selection, len(state.LocalStorageRoots) > 0)
	err := filepath.WalkDir(filepath.Join(snapshotRoot, "payload"), func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(snapshotRoot, current)
		if err != nil {
			return err
		}
		sum, size, err := hashFile(ctx, current)
		if err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, ManifestFile{
			Path:   filepath.ToSlash(relative),
			Size:   size,
			SHA256: sum,
			Mode:   uint32(info.Mode().Perm()),
		})
		manifest.TotalSize += size
		return nil
	})
	if err != nil {
		return Manifest{}, err
	}
	sort.Slice(manifest.Files, func(i, j int) bool {
		return manifest.Files[i].Path < manifest.Files[j].Path
	})
	manifest.FileCount = len(manifest.Files)
	if manifest.FileCount == 0 {
		return Manifest{}, errors.New("backup: snapshot is empty")
	}
	return manifest, nil
}

func hashFile(ctx context.Context, filePath string) (string, int64, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	buffer := make([]byte, 1<<20)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			written, writeErr := hash.Write(buffer[:n])
			if writeErr != nil {
				return "", 0, writeErr
			}
			size += int64(written)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return "", 0, readErr
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func randomID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func metaPath(archivePath string) string {
	return archivePath + ".meta.json"
}

func normalizedVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return "unknown"
	}
	return version
}

func cleanArchivePath(name string) (string, bool) {
	if name == "" || strings.ContainsRune(name, 0) || strings.Contains(name, `\`) {
		return "", false
	}
	if strings.HasPrefix(name, "/") || path.IsAbs(name) {
		return "", false
	}
	clean := path.Clean(name)
	if clean == "." || clean != name || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}

func (m *Manager) cleanupInterrupted() error {
	entries, err := os.ReadDir(m.backupDir)
	if err != nil {
		return fmt.Errorf("backup: inspect backup directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// Interrupted archive writes leave .part files and interrupted sidecar
		// writes leave .tmp files.
		name := strings.ToLower(entry.Name())
		if strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".tmp") {
			_ = os.Remove(filepath.Join(m.backupDir, entry.Name()))
		}
	}
	if err := os.RemoveAll(m.snapshotDir); err != nil {
		return fmt.Errorf("backup: clear interrupted snapshots: %w", err)
	}
	return os.MkdirAll(m.snapshotDir, 0o700)
}
