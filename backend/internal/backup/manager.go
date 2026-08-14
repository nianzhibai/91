package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	"github.com/video-site/backend/internal/atomicfile"
	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/config"
	"github.com/video-site/backend/internal/localpath"
	"github.com/video-site/backend/internal/persistence"
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

	mu              sync.Mutex
	current         *TaskStatus
	currentCancel   context.CancelFunc
	estimate        Estimate
	estimateUntil   time.Time
	uploadBusy      map[string]bool
	uploadWriters   map[string]map[int]context.CancelFunc
	uploadCanceling map[string]bool
	uploadLocks     map[string]*uploadSessionLock
	uploadProgress  map[string]OperationProgress
	restoreBusy     bool
	restoreProgress *OperationProgress
	restoreBarrier  *catalog.WriteBarrier
	restoreGateHeld bool
	closed          bool

	runCtx    context.Context
	runCancel context.CancelFunc
	restart   chan struct{}
}

type sourceSpec struct {
	name   string
	source string
	prefix string
}

func NewManager(cfg Config) (*Manager, error) {
	if cfg.Catalog == nil {
		return nil, errors.New("backup: catalog is required")
	}
	if cfg.AppConfig == nil {
		return nil, errors.New("backup: application config is required")
	}
	runtimeStorage := cfg.RuntimeStorage
	if strings.TrimSpace(runtimeStorage.DBPath) == "" {
		runtimeStorage.DBPath = cfg.AppConfig.Storage.DBPath
	}
	if strings.TrimSpace(runtimeStorage.LocalPreviewDir) == "" {
		runtimeStorage.LocalPreviewDir = cfg.AppConfig.Storage.LocalPreviewDir
	}
	workingDir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("backup: resolve working directory: %w", err)
	}
	dbPath, err := localpath.Resolve(workingDir, runtimeStorage.DBPath)
	if err != nil {
		return nil, errors.New("backup: database path is invalid")
	}
	previewPath, err := localpath.Resolve(workingDir, runtimeStorage.LocalPreviewDir)
	if err != nil {
		return nil, errors.New("backup: preview path is invalid")
	}
	configPath, err := filepath.Abs(strings.TrimSpace(cfg.ConfigPath))
	if err != nil || strings.TrimSpace(cfg.ConfigPath) == "" {
		return nil, errors.New("backup: config path is invalid")
	}
	dataRoot := filepath.Dir(dbPath)
	assetRoot := filepath.Dir(previewPath)
	m := &Manager{
		catalog:         cfg.Catalog,
		appConfig:       cfg.AppConfig,
		configPath:      configPath,
		appVersion:      normalizedVersion(cfg.AppVersion),
		dbPath:          dbPath,
		previewPath:     previewPath,
		dataRoot:        dataRoot,
		assetRoot:       assetRoot,
		backupDir:       filepath.Join(dataRoot, "backups"),
		snapshotDir:     filepath.Join(dataRoot, ".backup-snapshots"),
		restoreDir:      filepath.Join(dataRoot, restoreStageDirName),
		restartManaged:  cfg.RestartManaged,
		now:             cfg.Now,
		availableBytes:  cfg.AvailableBytes,
		uploadBusy:      make(map[string]bool),
		uploadWriters:   make(map[string]map[int]context.CancelFunc),
		uploadCanceling: make(map[string]bool),
		uploadLocks:     make(map[string]*uploadSessionLock),
		uploadProgress:  make(map[string]OperationProgress),
		restart:         make(chan struct{}, 1),
	}
	m.uploadRoot = filepath.Join(m.backupDir, ".uploads")
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

func (m *Manager) sourceSpecs() []sourceSpec {
	return []sourceSpec{
		{name: "previews", source: m.previewPath, prefix: "payload/previews"},
		{name: "uploads", source: filepath.Join(m.assetRoot, "uploads"), prefix: "payload/uploads"},
		{name: "crawler-scripts", source: filepath.Join(m.assetRoot, "crawler-scripts"), prefix: "payload/crawler-scripts"},
		{name: "scriptcrawlers", source: filepath.Join(m.assetRoot, "scriptcrawlers"), prefix: "payload/scriptcrawlers"},
		{name: "spider91", source: filepath.Join(m.assetRoot, "spider91"), prefix: "payload/spider91"},
	}
}

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

	var estimate Estimate
	dbPaths := []string{
		m.dbPath,
		m.dbPath + "-wal",
	}
	for _, dbPath := range dbPaths {
		info, err := os.Lstat(dbPath)
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
	// The archive contains one SQLite snapshot regardless of WAL presence.
	estimate.FileCount = 1
	for _, spec := range m.sourceSpecs() {
		count, size, err := scanSource(ctx, spec.source)
		if err != nil {
			return Estimate{}, fmt.Errorf("backup: estimate %s: %w", spec.name, err)
		}
		estimate.FileCount += count
		estimate.TotalBytes += size
	}
	count, size, err := m.estimateLocalStorageResources(ctx, false)
	if err != nil {
		return Estimate{}, err
	}
	estimate.FileCount += count
	estimate.TotalBytes += size
	available, err := m.availableBytes(m.dataRoot)
	if err != nil {
		return Estimate{}, fmt.Errorf("backup: inspect available space: %w", err)
	}
	estimate.AvailableBytes = available
	estimate.RequiredBytes = requiredBackupBytes(estimate.TotalBytes)
	m.mu.Lock()
	m.estimate = estimate
	m.estimateUntil = now.Add(30 * time.Second)
	m.mu.Unlock()
	return estimate, nil
}

// EstimateForSelection keeps the preflight check conservative while avoiding
// charging a partial backup for unrelated application asset roots. The final
// task size is always replaced with the manifest size after snapshotting.
func (m *Manager) EstimateForSelection(ctx context.Context, selection BackupSelection) (Estimate, error) {
	if !selection.Any() {
		return Estimate{}, ErrNoBackupContent
	}
	var estimate Estimate
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
	estimate.FileCount = 1
	addSource := func(name, source string) error {
		count, size, err := scanSource(ctx, source)
		if err != nil {
			return fmt.Errorf("backup: estimate %s: %w", name, err)
		}
		estimate.FileCount += count
		estimate.TotalBytes += size
		return nil
	}
	if selection.CloudDrives || selection.CrawlerScripts || selection.UploadStorage || selection.LocalStorage {
		if err := addSource("previews", m.previewPath); err != nil {
			return Estimate{}, err
		}
	}
	if selection.UploadStorage {
		if err := addSource("uploads", filepath.Join(m.assetRoot, "uploads")); err != nil {
			return Estimate{}, err
		}
	}
	if selection.CrawlerScripts {
		for _, spec := range []sourceSpec{
			{name: "crawler-scripts", source: filepath.Join(m.assetRoot, "crawler-scripts")},
			{name: "scriptcrawlers", source: filepath.Join(m.assetRoot, "scriptcrawlers")},
			{name: "spider91", source: filepath.Join(m.assetRoot, "spider91")},
		} {
			if err := addSource(spec.name, spec.source); err != nil {
				return Estimate{}, err
			}
		}
	}
	if selection.LocalStorage {
		count, size, err := m.estimateLocalStorageResources(ctx, true)
		if err != nil {
			return Estimate{}, err
		}
		estimate.FileCount += count
		estimate.TotalBytes += size
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
			for _, fileID := range []string{video.FileID, video.TranscodedFileID} {
				relative, decodeErr := decodeLocalStorageFileID(fileID)
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
	if err := writeArchive(ctx, partPath, snapshotRoot, manifest, func(fileBytes int64, fileDone bool) {
		m.updateTask(id, func(status *TaskStatus) {
			status.ProcessedBytes += fileBytes
			if fileDone {
				status.ProcessedFiles++
			}
		})
	}); err != nil {
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
	archiveHash, archiveSize, err := hashFile(ctx, partPath)
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
	_ = writeJSONAtomic(metaPath(finalPath), meta, 0o600)
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
	if err := redactSnapshotDatabase(ctx, dbDestination, selection.UserInfo); err != nil {
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
	if selection.AllResources() {
		for _, spec := range m.sourceSpecs() {
			destination := filepath.Join(snapshotRoot, filepath.FromSlash(spec.prefix))
			if err := snapshotSource(ctx, spec.source, destination); err != nil {
				return snapshotSelectionState{}, fmt.Errorf("backup: snapshot %s: %w", spec.name, err)
			}
		}
		if selection.LocalStorage {
			if err := m.snapshotSelectedLocalStorage(ctx, snapshotRoot, state); err != nil {
				return snapshotSelectionState{}, err
			}
		}
	} else {
		if selection.CloudDrives || selection.CrawlerScripts || selection.UploadStorage || selection.LocalStorage {
			if err := m.snapshotSelectedPreviews(ctx, snapshotRoot, state); err != nil {
				return snapshotSelectionState{}, err
			}
		}
		if selection.UploadStorage {
			if err := m.snapshotSelectedUploads(ctx, snapshotRoot, state); err != nil {
				return snapshotSelectionState{}, err
			}
		}
		if selection.CrawlerScripts {
			for _, spec := range []sourceSpec{
				{name: "crawler-scripts", source: filepath.Join(m.assetRoot, "crawler-scripts"), prefix: "payload/crawler-scripts"},
				{name: "spider91", source: filepath.Join(m.assetRoot, "spider91"), prefix: "payload/spider91"},
			} {
				if err := snapshotSource(ctx, spec.source, filepath.Join(snapshotRoot, filepath.FromSlash(spec.prefix))); err != nil {
					return snapshotSelectionState{}, fmt.Errorf("backup: snapshot %s: %w", spec.name, err)
				}
			}
			if err := m.snapshotSelectedCrawlerVideos(ctx, snapshotRoot, state); err != nil {
				return snapshotSelectionState{}, err
			}
		}
		if selection.LocalStorage {
			if err := m.snapshotSelectedLocalStorage(ctx, snapshotRoot, state); err != nil {
				return snapshotSelectionState{}, err
			}
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

func writeJSONAtomic(filePath string, value any, mode os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
		return err
	}
	directory := filepath.Dir(filePath)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(filePath)+"-*.part")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode.Perm()); err != nil {
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, filePath); err != nil {
		return err
	}
	removeTemporary = false
	return atomicfile.SyncDirectory(directory)
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
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".part") {
			_ = os.Remove(filepath.Join(m.backupDir, entry.Name()))
		}
	}
	if err := os.RemoveAll(m.snapshotDir); err != nil {
		return fmt.Errorf("backup: clear interrupted snapshots: %w", err)
	}
	return os.MkdirAll(m.snapshotDir, 0o700)
}
