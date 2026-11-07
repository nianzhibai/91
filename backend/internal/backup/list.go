package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (m *Manager) List(ctx context.Context) (ListResult, error) {
	estimate, err := m.Estimate(ctx)
	if err != nil {
		return ListResult{}, err
	}
	entries, err := os.ReadDir(m.backupDir)
	if err != nil {
		return ListResult{}, err
	}
	records := make([]BackupRecord, 0)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return ListResult{}, err
		}
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".zip") {
			continue
		}
		if !hasKnownBackupNamePrefix(entry.Name()) {
			continue
		}
		record, err := m.backupRecordFromEntry(entry)
		if err != nil {
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].Name > records[j].Name
		}
		return records[i].CreatedAt.After(records[j].CreatedAt)
	})
	_, pendingErr := os.Stat(m.pendingPath)
	return ListResult{
		Backups:         records,
		Current:         m.Current(),
		RestoreProgress: m.restoreProgressSnapshot(),
		Estimate:        estimate,
		RestartManaged:  m.restartManaged,
		PendingRestore:  pendingErr == nil,
	}, nil
}

// BackupRecord returns the local metadata for one backup artifact without
// scanning unrelated application assets. Server-to-server transfers use this
// to pin an exact, already-verified archive before starting a durable job.
func (m *Manager) BackupRecord(id string) (BackupRecord, error) {
	archivePath, name, err := m.resolveBackup(id)
	if err != nil {
		return BackupRecord{}, err
	}
	entry, err := os.Stat(archivePath)
	if err != nil {
		if os.IsNotExist(err) {
			return BackupRecord{}, ErrBackupNotFound
		}
		return BackupRecord{}, err
	}
	return m.backupRecord(name, archivePath, entry)
}

func (m *Manager) backupRecordFromEntry(entry os.DirEntry) (BackupRecord, error) {
	info, err := entry.Info()
	if err != nil {
		return BackupRecord{}, err
	}
	return m.backupRecord(entry.Name(), filepath.Join(m.backupDir, entry.Name()), info)
}

func (m *Manager) backupRecord(name, archivePath string, info os.FileInfo) (BackupRecord, error) {
	if !info.Mode().IsRegular() {
		return BackupRecord{}, ErrBackupNotFound
	}
	record := BackupRecord{
		ID:                 strings.TrimSuffix(name, filepath.Ext(name)),
		Name:               name,
		Size:               info.Size(),
		CreatedAt:          info.ModTime().UTC(),
		VerificationStatus: "unchecked",
	}
	var meta archiveMeta
	if err := readJSONFile(metaPath(archivePath), &meta); err == nil &&
		meta.Name == name &&
		meta.Size == info.Size() &&
		meta.ModifiedAt.Equal(info.ModTime().UTC()) &&
		meta.Manifest.FormatVersion == FormatVersion &&
		meta.Manifest.Selection != nil &&
		meta.Manifest.Selection.Any() {
		record.SHA256 = meta.SHA256
		record.VerificationStatus = "verified"
		record.Imported = meta.Imported
		applyManifestToRecord(&record, meta.Manifest, info.ModTime())
		return record, nil
	}
	manifest, inspectErr := InspectArchive(archivePath)
	if inspectErr != nil {
		record.VerificationStatus = "invalid"
		record.VerificationError = inspectErr.Error()
		return record, nil
	}
	applyManifestToRecord(&record, manifest, info.ModTime())
	return record, nil
}

func applyManifestToRecord(record *BackupRecord, manifest Manifest, fallback time.Time) {
	if record == nil {
		return
	}
	record.CreatedAt = archiveTimestamp(manifest, fallback)
	record.AppVersion = manifest.AppVersion
	record.SourceDataRoot = manifest.SourceDataRoot
	record.FileCount = manifest.FileCount
	record.ExpandedSize = manifest.TotalSize
	record.Included = append([]string(nil), manifest.Included...)
	if manifest.Selection != nil {
		selection := *manifest.Selection
		record.Selection = &selection
	}
}

func (m *Manager) OpenBackup(id string) (*os.File, os.FileInfo, string, error) {
	archivePath, name, err := m.resolveBackup(id)
	if err != nil {
		return nil, nil, "", err
	}
	file, err := os.Open(archivePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, "", ErrBackupNotFound
		}
		return nil, nil, "", err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, "", err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, "", ErrBackupNotFound
	}
	return file, info, name, nil
}

func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	restoreBusy := m.restoreBusy
	m.mu.Unlock()
	if restoreBusy {
		return ErrRestorePending
	}
	archivePath, _, err := m.resolveBackup(id)
	if err != nil {
		return err
	}
	var marker restoreMarker
	if err := readJSONFile(m.pendingPath, &marker); err == nil && marker.BackupID == id {
		return errors.New("该备份正在等待恢复，不能删除")
	}
	if err := os.Remove(archivePath); err != nil {
		if os.IsNotExist(err) {
			return ErrBackupNotFound
		}
		return err
	}
	_ = os.Remove(metaPath(archivePath))
	return nil
}

func (m *Manager) resolveBackup(id string) (string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id ||
		strings.ContainsAny(id, `/\`+"\x00") ||
		!hasKnownBackupNamePrefix(id) {
		return "", "", ErrBackupNotFound
	}
	name := id + ".zip"
	archivePath := filepath.Join(m.backupDir, name)
	relative, err := filepath.Rel(m.backupDir, archivePath)
	if err != nil || relative != name {
		return "", "", ErrBackupNotFound
	}
	info, err := os.Lstat(archivePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", ErrBackupNotFound
		}
		return "", "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", ErrBackupNotFound
	}
	return archivePath, name, nil
}

func hasKnownBackupNamePrefix(name string) bool {
	return strings.HasPrefix(name, backupNamePrefix) ||
		strings.HasPrefix(name, previousBackupNamePrefix) ||
		strings.HasPrefix(name, legacyBackupNamePrefix)
}

func readJSONFile(filePath string, destination any) error {
	info, err := os.Lstat(filePath)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("backup: JSON sidecar is not a regular file")
	}
	if info.Size() > maxJSONSidecarBytes {
		return fmt.Errorf("backup: JSON sidecar is too large")
	}
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) ||
		openedInfo.Size() > maxJSONSidecarBytes {
		_ = file.Close()
		return fmt.Errorf("backup: JSON sidecar changed while opening")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxJSONSidecarBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if int64(len(data)) > maxJSONSidecarBytes {
		return fmt.Errorf("backup: JSON sidecar is too large")
	}
	return json.Unmarshal(data, destination)
}
