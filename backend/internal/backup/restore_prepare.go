package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/atomicfile"
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

// PrepareRestore verifies and stages a backup for restore. The staged catalog
// is merged into a snapshot of the current catalog, every live path the
// restore changes is staged next to its target, and a pending marker is
// written. Catalog writes stay paused until a restart applies the restore.
func (m *Manager) PrepareRestore(ctx context.Context, id string) (ValidationReport, error) {
	if m == nil {
		return ValidationReport{}, errors.New("backup: manager is nil")
	}
	finish, err := m.beginRestorePreparation()
	if err != nil {
		return ValidationReport{}, err
	}
	prepared := false
	defer func() { finish(prepared) }()

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
	p := &restorePreparation{backupID: id, stageID: stageID, stageRoot: filepath.Join(m.restoreDir, stageID)}
	if err := os.MkdirAll(p.stageRoot, 0o700); err != nil {
		return ValidationReport{}, err
	}
	maintenance := false
	defer func() {
		if !prepared {
			if maintenance {
				m.releaseRestoreMaintenance()
			}
			_ = os.RemoveAll(p.stageRoot)
		}
	}()

	if err := m.extractRestoreArchive(ctx, p, archivePath, available); err != nil {
		return ValidationReport{}, err
	}
	if err := m.rewriteStagedCatalog(ctx, p); err != nil {
		return ValidationReport{}, err
	}
	// The merge snapshots the live catalog, so writers stay paused from here
	// until the restart applies the restore.
	if err := m.beginRestoreMaintenance(ctx); err != nil {
		return ValidationReport{}, err
	}
	maintenance = true
	if err := m.mergeStagedCatalog(ctx, p); err != nil {
		return ValidationReport{}, err
	}
	targets, err := m.planRestoreTargets(ctx, p)
	if err != nil {
		return ValidationReport{}, err
	}
	if err := m.writeRestoreMarker(p, targets); err != nil {
		return ValidationReport{}, err
	}
	prepared = true
	m.setRestoreProgress(p.completedProgress("ready"))
	return p.report, nil
}

// restorePreparation is the state PrepareRestore passes between its stages.
type restorePreparation struct {
	backupID   string
	stageID    string
	stageRoot  string
	report     ValidationReport
	selection  BackupSelection
	localPlans []isolatedLocalStorageRestore
	// Preview assets owned by the archive's replacing resources and by the
	// target's merged resources, read before the catalogs are merged.
	sourcePreviewPaths []string
	targetPreviewPaths []string
}

// restoreTarget is one live path a restore replaces or removes.
type restoreTarget struct {
	name   string
	kind   string
	source string
	target string
}

func (p *restorePreparation) payloadPath(name string) string {
	return filepath.Join(p.stageRoot, "payload", name)
}

// completedProgress reports the whole archive as processed in a later phase.
func (p *restorePreparation) completedProgress(phase string) OperationProgress {
	return OperationProgress{
		Phase:          phase,
		ProcessedBytes: p.report.Manifest.TotalSize,
		TotalBytes:     p.report.Manifest.TotalSize,
		ProcessedFiles: p.report.Manifest.FileCount,
		TotalFiles:     p.report.Manifest.FileCount,
	}
}

// beginRestorePreparation claims the single restore slot. The returned
// function releases it; restore progress is kept only for a staged restore.
func (m *Manager) beginRestorePreparation() (func(prepared bool), error) {
	m.mu.Lock()
	if m.restoreBusy {
		m.mu.Unlock()
		return nil, ErrRestorePending
	}
	if m.current != nil &&
		(m.current.State == "queued" || m.current.State == "running" || m.current.State == "canceling") {
		m.mu.Unlock()
		return nil, ErrTaskRunning
	}
	m.restoreBusy = true
	m.mu.Unlock()
	finish := func(prepared bool) {
		m.mu.Lock()
		m.restoreBusy = false
		m.mu.Unlock()
		if !prepared {
			m.clearRestoreProgress()
		}
	}
	if _, err := os.Stat(m.pendingPath); err == nil {
		finish(false)
		return nil, ErrRestorePending
	} else if !os.IsNotExist(err) {
		finish(false)
		return nil, err
	}
	m.setRestoreProgress(OperationProgress{Phase: "inspecting"})
	return finish, nil
}

// extractRestoreArchive verifies the archive while extracting it into the
// stage root.
func (m *Manager) extractRestoreArchive(ctx context.Context, p *restorePreparation, archivePath string, available int64) error {
	report, err := VerifyAndExtractArchive(ctx, archivePath, p.stageRoot, VerifyOptions{
		CurrentVersion: m.appVersion,
		AvailableBytes: available,
		Progress: func(progress OperationProgress) {
			if progress.Phase == "database" {
				progress.Phase = "checking-database"
			} else {
				progress.Phase = "extracting"
			}
			m.setRestoreProgress(progress)
		},
	})
	if err != nil {
		return err
	}
	p.report = report
	p.selection = *report.Manifest.Selection
	// ZIP directory entries are not extracted, so empty asset roots are
	// created here.
	for _, source := range assetSources {
		if err := os.MkdirAll(p.payloadPath(source.name), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// rewriteStagedCatalog adapts the extracted catalog to this deployment.
func (m *Manager) rewriteStagedCatalog(ctx context.Context, p *restorePreparation) error {
	m.setRestoreProgress(p.completedProgress("rewriting"))
	if p.selection.LocalStorage {
		plans, err := m.planIsolatedLocalStorageRestore(ctx, p.stageID, p.report.Manifest.LocalStorage)
		if err != nil {
			return err
		}
		p.localPlans = plans
	}
	rewritten, err := rewriteRestoredDatabase(ctx, p.payloadPath("database.sqlite"), catalogRewrite{
		selection:         p.selection,
		stagePreviewDir:   p.payloadPath("previews"),
		sourceDataRoot:    p.report.Manifest.SourceDataRoot,
		sourcePreviewRoot: p.report.Manifest.SourcePreview,
		targetDataRoot:    m.dataRoot,
		targetAssetRoot:   m.assetRoot,
		targetPreviewRoot: m.previewPath,
		localStoragePlans: p.localPlans,
	})
	if err != nil {
		return err
	}
	p.report.PathRewrites = append(p.report.PathRewrites, rewritten.PathRewrites...)
	p.report.LocalStorageWarnings = append(p.report.LocalStorageWarnings, rewritten.LocalStorageWarnings...)
	p.report.MissingAssets = append(p.report.MissingAssets, rewritten.MissingAssets...)
	p.report.Warnings = append(p.report.Warnings, rewritten.Warnings...)
	return nil
}

// mergeStagedCatalog overlays the staged catalog on a snapshot of the live
// catalog; the result replaces the staged database as the restored catalog.
func (m *Manager) mergeStagedCatalog(ctx context.Context, p *restorePreparation) error {
	stageDatabase := p.payloadPath("database.sqlite")
	mergedDatabase := p.payloadPath("database-merged.sqlite")
	if err := m.catalog.BackupTo(ctx, mergedDatabase); err != nil {
		return fmt.Errorf("backup: snapshot current database for selective restore: %w", err)
	}
	var err error
	p.sourcePreviewPaths, err = collectPreviewAssetPaths(ctx, stageDatabase, p.payloadPath("previews"), false)
	if err != nil {
		return fmt.Errorf("backup: inspect restored preview ownership: %w", err)
	}
	p.targetPreviewPaths, err = collectPreviewAssetPaths(ctx, mergedDatabase, m.previewPath, true)
	if err != nil {
		return fmt.Errorf("backup: inspect target preview ownership: %w", err)
	}
	if err := mergeSelectiveRestoreDatabase(ctx, mergedDatabase, stageDatabase, p.selection); err != nil {
		return err
	}
	if err := os.Remove(stageDatabase); err != nil {
		return err
	}
	return os.Rename(mergedDatabase, stageDatabase)
}

// planRestoreTargets prepares the replacement of every live path the restore
// changes: each included asset directory, each restored local storage root,
// and finally the database with its WAL and shared-memory files.
func (m *Manager) planRestoreTargets(ctx context.Context, p *restorePreparation) ([]restoreTarget, error) {
	m.setRestoreProgress(p.completedProgress("preparing-switch"))
	var targets []restoreTarget
	for _, section := range p.report.Manifest.Included {
		if section == "database" || section == localStorageSection {
			continue
		}
		source := p.payloadPath(section)
		var err error
		switch section {
		case "previews":
			source, err = prepareMergedPreview(
				ctx,
				p.stageRoot,
				source,
				m.previewPath,
				p.targetPreviewPaths,
				!p.selection.AllResources(),
				p.sourcePreviewPaths,
			)
		case "uploads":
			source, err = prepareMergedUploadStorage(ctx, p.stageRoot, source, m.assetPath(section))
		}
		if err != nil {
			return nil, err
		}
		targets = append(targets, restoreTarget{name: section, kind: "dir", source: source, target: m.assetPath(section)})
	}
	if p.selection.LocalStorage {
		localStorage, err := prepareIsolatedLocalStorage(ctx, p.stageRoot, p.report.Manifest.LocalStorage, p.localPlans)
		if err != nil {
			return nil, err
		}
		for _, prepared := range localStorage {
			targets = append(targets, restoreTarget{
				name:   "localstorage-" + archiveDriveComponent(prepared.DriveID),
				kind:   "dir",
				source: prepared.Source,
				target: prepared.Target,
			})
		}
	}
	targets = append(targets,
		restoreTarget{name: "database-wal", kind: "remove", target: m.dbPath + "-wal"},
		restoreTarget{name: "database-shm", kind: "remove", target: m.dbPath + "-shm"},
		restoreTarget{name: "database", kind: "file", source: p.payloadPath("database.sqlite"), target: m.dbPath},
	)
	if err := validateRestoreOperationCount(len(targets)); err != nil {
		return nil, err
	}
	for index := range targets {
		for prior := 0; prior < index; prior++ {
			if restoreTargetPathsOverlap(targets[index].target, targets[prior].target) {
				return nil, fmt.Errorf("backup: restore targets overlap: %s and %s", targets[prior].name, targets[index].name)
			}
		}
	}
	return targets, nil
}

// writeRestoreMarker stages every target next to its live path and records
// the switch plan that the next start applies.
func (m *Manager) writeRestoreMarker(p *restorePreparation, targets []restoreTarget) error {
	markerReport := p.report
	markerReport.Manifest.Files = nil
	marker := restoreMarker{
		MarkerVersion: 1,
		BackupID:      p.backupID,
		DataRoot:      m.dataRoot,
		StageRoot:     p.stageRoot,
		CreatedAt:     m.nowTime(),
		State:         "pending",
		Report:        markerReport,
	}
	for _, target := range targets {
		operation, err := prepareRestoreSwitch(p.stageID, target.name, target.kind, target.source, target.target)
		if err != nil {
			cleanupPreparedSwitches(marker.Operations)
			return err
		}
		marker.Operations = append(marker.Operations, operation)
	}
	if err := atomicfile.WriteJSON(m.pendingPath, marker, 0o600); err != nil {
		cleanupPreparedSwitches(marker.Operations)
		return err
	}
	return nil
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
