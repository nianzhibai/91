package backup

func normalizeOperationProgress(progress OperationProgress) OperationProgress {
	if progress.ProcessedBytes < 0 {
		progress.ProcessedBytes = 0
	}
	if progress.TotalBytes < 0 {
		progress.TotalBytes = 0
	}
	if progress.TotalBytes > 0 && progress.ProcessedBytes > progress.TotalBytes {
		progress.ProcessedBytes = progress.TotalBytes
	}
	if progress.ProcessedFiles < 0 {
		progress.ProcessedFiles = 0
	}
	if progress.TotalFiles < 0 {
		progress.TotalFiles = 0
	}
	if progress.TotalFiles > 0 && progress.ProcessedFiles > progress.TotalFiles {
		progress.ProcessedFiles = progress.TotalFiles
	}
	return progress
}

func (m *Manager) setRestoreProgress(progress OperationProgress) {
	if m == nil {
		return
	}
	progress = normalizeOperationProgress(progress)
	m.mu.Lock()
	m.restoreProgress = &progress
	m.mu.Unlock()
}

func (m *Manager) restoreProgressSnapshot() *OperationProgress {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.restoreProgress == nil {
		return nil
	}
	copy := *m.restoreProgress
	return &copy
}

func (m *Manager) clearRestoreProgress() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.restoreProgress = nil
	m.mu.Unlock()
}
