package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AppliedRestore is returned during startup after all restored paths have been
// switched into place. The caller must either CommitAppliedRestore after the
// restored catalog opens/migrates successfully, or RollbackAppliedRestore.
type AppliedRestore struct {
	markerPath string
	marker     restoreMarker
}

// DatabasePath is the actual restore target, including pending restores
// prepared by versions that used a different database filename.
func (applied *AppliedRestore) DatabasePath() string {
	if applied != nil {
		for _, operation := range applied.marker.Operations {
			if operation.Name == "database" && operation.Kind == "file" {
				return operation.Target
			}
		}
	}
	return ""
}

func PendingMarkerPath(dataRoot string) string {
	return filepath.Join(dataRoot, "backups", ".restore-pending.json")
}

func ApplyPendingRestore(dataRoot string) (*AppliedRestore, error) {
	absoluteRoot, err := filepath.Abs(strings.TrimSpace(dataRoot))
	if err != nil || strings.TrimSpace(dataRoot) == "" {
		return nil, errors.New("backup: invalid data root while applying restore")
	}
	markerPath := PendingMarkerPath(absoluteRoot)
	var marker restoreMarker
	if err := readJSONFile(markerPath, &marker); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("backup: read pending restore: %w", err)
	}
	if err := validateRestoreMarker(marker, absoluteRoot); err != nil {
		return nil, err
	}
	if marker.State == "rolledback" {
		if err := cleanupRolledBackRestore(marker); err != nil {
			return nil, fmt.Errorf("backup: clean completed restore rollback: %w", err)
		}
		if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		return nil, nil
	}
	if marker.State == "rolling-back" || marker.State == "rollback-failed" {
		cause := errors.New(strings.TrimSpace(marker.LastError))
		if strings.TrimSpace(marker.LastError) == "" {
			cause = errors.New("interrupted restore rollback")
		}
		if err := rollbackRestoreMarker(markerPath, &marker, cause); err != nil {
			return nil, fmt.Errorf("backup: resume interrupted restore rollback: %w", err)
		}
		return nil, nil
	}
	for index := range marker.Operations {
		if err := applyRestoreOperation(markerPath, &marker, index); err != nil {
			marker.LastError = err.Error()
			_ = writeJSONAtomic(markerPath, marker, 0o600)
			if rollbackErr := rollbackRestoreMarker(markerPath, &marker, err); rollbackErr != nil {
				return nil, fmt.Errorf("backup: apply restore: %v; rollback also failed: %w", err, rollbackErr)
			}
			return nil, fmt.Errorf("backup: apply restore failed and old data was restored: %w", err)
		}
	}
	marker.State = "applied"
	if err := writeJSONAtomic(markerPath, marker, 0o600); err != nil {
		if rollbackErr := rollbackRestoreMarker(markerPath, &marker, err); rollbackErr != nil {
			return nil, fmt.Errorf("backup: persist applied restore: %v; rollback also failed: %w", err, rollbackErr)
		}
		return nil, err
	}
	return &AppliedRestore{markerPath: markerPath, marker: marker}, nil
}

func CommitAppliedRestore(applied *AppliedRestore) error {
	if applied == nil {
		return nil
	}
	marker := &applied.marker
	for _, operation := range marker.Operations {
		if err := removeRestoreArtifact(operation.Rollback); err != nil {
			return fmt.Errorf("backup: remove restore rollback %s: %w", operation.Name, err)
		}
		if err := removeRestoreArtifact(operation.Staged); err != nil {
			return fmt.Errorf("backup: remove restore staging %s: %w", operation.Name, err)
		}
	}
	_ = os.RemoveAll(marker.StageRoot)
	reportPath := filepath.Join(
		filepath.Dir(applied.markerPath),
		"last-restore-report.json",
	)
	result := struct {
		BackupID   string           `json:"backupId"`
		RestoredAt time.Time        `json:"restoredAt"`
		Report     ValidationReport `json:"report"`
	}{
		BackupID:   marker.BackupID,
		RestoredAt: time.Now().UTC(),
		Report:     marker.Report,
	}
	if err := writeJSONAtomic(reportPath, result, 0o600); err != nil {
		return err
	}
	if err := os.Remove(applied.markerPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func RollbackAppliedRestore(applied *AppliedRestore, cause error) error {
	if applied == nil {
		return nil
	}
	return rollbackRestoreMarker(applied.markerPath, &applied.marker, cause)
}

func validateRestoreMarker(marker restoreMarker, dataRoot string) error {
	if marker.MarkerVersion != 1 || marker.BackupID == "" ||
		validateRestoreOperationCount(len(marker.Operations)) != nil {
		return errors.New("backup: pending restore marker is invalid")
	}
	switch marker.State {
	case "pending", "applied", "rolling-back", "rollback-failed", "rolledback":
	default:
		return errors.New("backup: pending restore marker has an invalid state")
	}
	markerRoot, err := filepath.Abs(marker.DataRoot)
	if err != nil || filepath.Clean(markerRoot) != filepath.Clean(dataRoot) {
		return errors.New("backup: pending restore belongs to another data root")
	}
	stageRoot, err := filepath.Abs(marker.StageRoot)
	expectedStageParent := filepath.Join(filepath.Clean(dataRoot), restoreStageDirName)
	if err != nil || filepath.Dir(stageRoot) != expectedStageParent ||
		filepath.Base(stageRoot) == "." || filepath.Base(stageRoot) == ".." {
		return errors.New("backup: pending restore staging root is invalid")
	}
	seenTargets := make(map[string]bool, len(marker.Operations))
	targets := make([]string, 0, len(marker.Operations))
	for _, operation := range marker.Operations {
		switch operation.State {
		case "pending", "target-moved", "applied", "rolledback":
		default:
			return fmt.Errorf("backup: invalid restore state for %s", operation.Name)
		}
		if operation.Kind != "file" && operation.Kind != "dir" && operation.Kind != "remove" {
			return fmt.Errorf("backup: invalid restore kind for %s", operation.Name)
		}
		target, err := filepath.Abs(operation.Target)
		if err != nil || target == string(filepath.Separator) || filepath.Dir(target) == target {
			return fmt.Errorf("backup: unsafe restore target for %s", operation.Name)
		}
		if seenTargets[target] {
			return fmt.Errorf("backup: duplicate restore target for %s", operation.Name)
		}
		seenTargets[target] = true
		for _, existing := range targets {
			if restoreTargetPathsOverlap(existing, target) {
				return fmt.Errorf("backup: overlapping restore target for %s", operation.Name)
			}
		}
		targets = append(targets, target)
		parent := filepath.Dir(target)
		for label, candidate := range map[string]string{
			"staged":   operation.Staged,
			"rollback": operation.Rollback,
		} {
			absolute, err := filepath.Abs(candidate)
			if err != nil || filepath.Dir(absolute) != parent {
				return fmt.Errorf("backup: %s path for %s is not adjacent to its target", label, operation.Name)
			}
			base := filepath.Base(absolute)
			targetBase := filepath.Base(target)
			if !strings.HasPrefix(base, "."+targetBase+".restore-") {
				return fmt.Errorf("backup: %s path for %s has an invalid name", label, operation.Name)
			}
		}
	}
	return nil
}

func validateRestoreOperationCount(count int) error {
	if count <= 0 || count > maxRestoreOperations {
		return fmt.Errorf("backup: restore requires %d operations; allowed range is 1-%d", count, maxRestoreOperations)
	}
	return nil
}

func applyRestoreOperation(markerPath string, marker *restoreMarker, index int) error {
	operation := &marker.Operations[index]
	if operation.State == "applied" {
		return nil
	}
	targetInfo, targetErr := os.Lstat(operation.Target)
	targetExists := targetErr == nil
	if targetErr != nil && !os.IsNotExist(targetErr) {
		return targetErr
	}
	if targetExists && targetInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("backup: restore target %s is a symbolic link", operation.Name)
	}
	_, stagedErr := os.Lstat(operation.Staged)
	stagedExists := stagedErr == nil
	if stagedErr != nil && !os.IsNotExist(stagedErr) {
		return stagedErr
	}
	_, rollbackErr := os.Lstat(operation.Rollback)
	rollbackExists := rollbackErr == nil
	if rollbackErr != nil && !os.IsNotExist(rollbackErr) {
		return rollbackErr
	}

	// Recover state after a crash between a rename and marker fsync.
	if !stagedExists {
		if operation.Kind == "remove" && !targetExists {
			operation.HadTarget = rollbackExists
			operation.State = "applied"
			return writeJSONAtomic(markerPath, marker, 0o600)
		}
		if targetExists {
			operation.HadTarget = rollbackExists
			operation.State = "applied"
			return writeJSONAtomic(markerPath, marker, 0o600)
		}
		return fmt.Errorf("backup: staged %s is missing", operation.Name)
	}
	if targetExists && rollbackExists {
		return fmt.Errorf("backup: both target and rollback exist for %s", operation.Name)
	}
	if targetExists {
		if err := os.Rename(operation.Target, operation.Rollback); err != nil {
			return fmt.Errorf("backup: move old %s aside: %w", operation.Name, err)
		}
		operation.HadTarget = true
		operation.State = "target-moved"
		if err := writeJSONAtomic(markerPath, marker, 0o600); err != nil {
			return err
		}
	} else if rollbackExists {
		operation.HadTarget = true
		operation.State = "target-moved"
	} else {
		operation.HadTarget = false
	}
	if operation.Kind == "remove" {
		if err := os.Remove(operation.Staged); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("backup: remove %s staging marker: %w", operation.Name, err)
		}
		operation.State = "applied"
		return writeJSONAtomic(markerPath, marker, 0o600)
	}
	if err := os.Rename(operation.Staged, operation.Target); err != nil {
		return fmt.Errorf("backup: activate restored %s: %w", operation.Name, err)
	}
	operation.State = "applied"
	return writeJSONAtomic(markerPath, marker, 0o600)
}

func rollbackRestoreMarker(markerPath string, marker *restoreMarker, cause error) error {
	if cause != nil {
		marker.LastError = cause.Error()
	}
	marker.State = "rolling-back"
	if err := writeJSONAtomic(markerPath, marker, 0o600); err != nil {
		return fmt.Errorf("backup: persist rollback intent: %w", err)
	}

	var rollbackErrors []string
	for index := len(marker.Operations) - 1; index >= 0; index-- {
		operation := &marker.Operations[index]
		if operation.State == "rolledback" {
			continue
		}
		if err := rollbackRestoreOperation(operation); err != nil {
			rollbackErrors = append(rollbackErrors, operation.Name+": "+err.Error())
			continue
		}
		operation.State = "rolledback"
		if err := writeJSONAtomic(markerPath, marker, 0o600); err != nil {
			rollbackErrors = append(rollbackErrors, operation.Name+": persist state: "+err.Error())
			break
		}
	}
	if len(rollbackErrors) > 0 {
		marker.State = "rollback-failed"
		_ = writeJSONAtomic(markerPath, marker, 0o600)
		return errors.New(strings.Join(rollbackErrors, "; "))
	}
	marker.State = "rolledback"
	_ = writeJSONAtomic(markerPath, marker, 0o600)
	failurePath := filepath.Join(
		filepath.Dir(markerPath),
		"last-restore-failure.json",
	)
	_ = writeJSONAtomic(failurePath, marker, 0o600)
	if err := cleanupRolledBackRestore(*marker); err != nil {
		return fmt.Errorf("backup: cleanup rejected restored data: %w", err)
	}
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func cleanupRolledBackRestore(marker restoreMarker) error {
	for _, operation := range marker.Operations {
		if err := removeRestoreArtifact(operation.Staged); err != nil {
			return fmt.Errorf("%s staging: %w", operation.Name, err)
		}
	}
	if err := os.RemoveAll(marker.StageRoot); err != nil {
		return err
	}
	return nil
}

func rollbackRestoreOperation(operation *restoreSwitch) error {
	if operation == nil {
		return errors.New("empty restore operation")
	}
	targetExists, err := restoreArtifactExists(operation.Target)
	if err != nil {
		return err
	}
	stagedExists, err := restoreArtifactExists(operation.Staged)
	if err != nil {
		return err
	}
	rollbackExists, err := restoreArtifactExists(operation.Rollback)
	if err != nil {
		return err
	}

	// A pending operation with both its original target and staged replacement
	// still present was never touched by the apply loop.
	if operation.State == "pending" && targetExists && stagedExists && !rollbackExists {
		return nil
	}

	if operation.Kind == "remove" {
		if rollbackExists {
			if targetExists {
				return errors.New("both target and rollback copy exist")
			}
			if err := os.Rename(operation.Rollback, operation.Target); err != nil {
				return fmt.Errorf("restore removed target: %w", err)
			}
			operation.HadTarget = true
			return nil
		}
		if operation.HadTarget && !targetExists {
			return errors.New("rollback copy for removed target is missing")
		}
		return nil
	}

	if rollbackExists {
		// The original value is still beside the target. Move the rejected
		// restored value back to staging, then reactivate the original.
		if targetExists {
			if stagedExists {
				return errors.New("target, staging, and rollback copies all exist")
			}
			if err := os.Rename(operation.Target, operation.Staged); err != nil {
				return fmt.Errorf("move rejected restored value to staging: %w", err)
			}
			targetExists = false
			stagedExists = true
		}
		if targetExists {
			return errors.New("target remained present during rollback")
		}
		if err := os.Rename(operation.Rollback, operation.Target); err != nil {
			return fmt.Errorf("reactivate original value: %w", err)
		}
		operation.HadTarget = true
		return nil
	}

	if operation.HadTarget {
		// A prior rollback attempt may have already restored the original and
		// crashed before persisting operation.State.
		if targetExists && stagedExists {
			return nil
		}
		return errors.New("original rollback copy is missing")
	}

	// There was no original target. Preserve the rejected restored value under
	// its staging name, or recognize that an earlier attempt already did so.
	if targetExists {
		if stagedExists {
			return errors.New("target and staging copy both exist without an original")
		}
		if err := os.Rename(operation.Target, operation.Staged); err != nil {
			return fmt.Errorf("move new restored value back to staging: %w", err)
		}
		return nil
	}
	if stagedExists {
		return nil
	}
	return errors.New("restored target and staging copy are both missing")
}

func restoreArtifactExists(candidate string) (bool, error) {
	info, err := os.Lstat(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("restore artifact is a symbolic link")
	}
	return true, nil
}

func removeRestoreArtifact(candidate string) error {
	if candidate == "" {
		return nil
	}
	info, err := os.Lstat(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.IsDir() {
		return os.RemoveAll(candidate)
	}
	return os.Remove(candidate)
}
