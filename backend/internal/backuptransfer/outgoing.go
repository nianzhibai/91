package backuptransfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nianzhibai/91/backend/internal/backup"
)

const (
	maxPeerRangeSize     = int64(1 << 30)
	maxAutomaticAttempts = 6
)

type nonRetryableTransferError struct {
	err error
}

func (e *nonRetryableTransferError) Error() string { return e.err.Error() }
func (e *nonRetryableTransferError) Unwrap() error { return e.err }

func permanentTransferFailure(err error) error {
	if err == nil {
		return nil
	}
	return &nonRetryableTransferError{err: err}
}

func (m *Manager) CreateTransfer(ctx context.Context, input CreateTransferInput) (TransferJob, error) {
	if m == nil {
		return TransferJob{}, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return TransferJob{}, err
	}
	targetURL, err := normalizeTargetURL(input.TargetURL)
	if err != nil {
		return TransferJob{}, err
	}
	token := strings.TrimSpace(input.ReceiveToken)
	if _, ok := receiveTokenID(token); !ok {
		return TransferJob{}, errors.New("目标服务器接收码格式无效")
	}
	record, err := m.backups.BackupRecord(strings.TrimSpace(input.BackupID))
	if err != nil {
		return TransferJob{}, err
	}
	if record.VerificationStatus != "verified" || !validSHA256(record.SHA256) {
		return TransferJob{}, errors.New("只能发送已经完整校验的备份包")
	}
	id, err := randomOpaqueID()
	if err != nil {
		return TransferJob{}, err
	}
	now := m.nowTime()
	stored := &storedTransferJob{
		TransferJob: TransferJob{
			ID:          id,
			BackupID:    record.ID,
			BackupName:  record.Name,
			TargetURL:   targetURL,
			State:       TransferQueued,
			Size:        record.Size,
			SHA256:      strings.ToLower(record.SHA256),
			CreatedAt:   now,
			UpdatedAt:   now,
			Cancellable: true,
		},
		ReceiveToken: token,
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return TransferJob{}, errors.New("服务器直传服务已关闭")
	}
	for _, existing := range m.jobs {
		if existing != nil && !existing.TransferJob.terminal() {
			m.mu.Unlock()
			return TransferJob{}, ErrTransferBusy
		}
	}
	m.jobs[id] = stored
	if err := m.saveJobLocked(stored); err != nil {
		delete(m.jobs, id)
		m.mu.Unlock()
		return TransferJob{}, err
	}
	job := stored.TransferJob
	m.mu.Unlock()
	m.signal()
	return job, nil
}

func (m *Manager) ListTransfers() []TransferJob {
	if m == nil {
		return []TransferJob{}
	}
	m.mu.Lock()
	jobs := sortedJobs(m.jobs)
	progress := make(map[string]*streamProgress, len(m.outgoingProgress))
	for id, tracker := range m.outgoingProgress {
		progress[id] = tracker
	}
	m.mu.Unlock()
	now := m.nowTime()
	for index := range jobs {
		jobs[index].BytesPerSecond = 0
		if tracker := progress[jobs[index].ID]; tracker != nil {
			processed, bytesPerSecond, _ := tracker.snapshot(now, jobs[index].Size)
			if processed > jobs[index].ProcessedBytes {
				jobs[index].ProcessedBytes = processed
			}
			jobs[index].BytesPerSecond = bytesPerSecond
		}
	}
	return jobs
}

func (m *Manager) CancelTransfer(id string) (TransferJob, error) {
	if m == nil {
		return TransferJob{}, ErrUnavailable
	}
	id = strings.TrimSpace(id)
	m.mu.Lock()
	job := m.jobs[id]
	if job == nil {
		m.mu.Unlock()
		return TransferJob{}, ErrTransferNotFound
	}
	if job.TransferJob.terminal() {
		result := job.TransferJob
		m.mu.Unlock()
		return result, ErrTransferTerminal
	}
	job.CancelRequested = true
	job.State = TransferCanceled
	job.Error = ""
	job.NextAttemptAt = time.Time{}
	job.Cancellable = false
	job.Retryable = false
	job.UpdatedAt = m.nowTime()
	if job.FinishedAt.IsZero() {
		job.FinishedAt = job.UpdatedAt
	}
	err := m.saveJobLocked(job)
	cancel := m.currentCancel
	if m.currentID != id {
		cancel = nil
	}
	result := job.TransferJob
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.signal()
	return result, err
}

func (m *Manager) RetryTransfer(id string) (TransferJob, error) {
	if m == nil {
		return TransferJob{}, ErrUnavailable
	}
	id = strings.TrimSpace(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job == nil {
		return TransferJob{}, ErrTransferNotFound
	}
	if job.State != TransferFailed || !job.Retryable || strings.TrimSpace(job.ReceiveToken) == "" {
		return job.TransferJob, ErrTransferNotRetryable
	}
	now := m.nowTime()
	job.State = TransferQueued
	job.Error = ""
	job.Attempts = 0
	job.NextAttemptAt = time.Time{}
	job.FinishedAt = time.Time{}
	job.UpdatedAt = now
	job.Cancellable = true
	job.Retryable = false
	job.CancelRequested = false
	if err := m.saveJobLocked(job); err != nil {
		return TransferJob{}, err
	}
	m.signal()
	return job.TransferJob, nil
}

func (m *Manager) BackupInUse(backupID string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, job := range m.jobs {
		if job != nil && job.BackupID == backupID && !job.TransferJob.terminal() {
			return true
		}
	}
	return false
}

func (m *Manager) run() {
	defer close(m.done)
	for {
		if err := m.runCtx.Err(); err != nil {
			return
		}
		cleanupPending := m.cleanupCanceledImports()
		id, wait := m.nextRunnableJob()
		if id == "" {
			if cleanupPending && wait > time.Second {
				wait = time.Second
			}
			timer := time.NewTimer(wait)
			select {
			case <-m.runCtx.Done():
				timer.Stop()
				return
			case <-m.wake:
				timer.Stop()
			case <-timer.C:
			}
			continue
		}
		m.processOne(id)
	}
}

func (m *Manager) nextRunnableJob() (string, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.nowTime()
	candidates := make([]*storedTransferJob, 0)
	wait := 30 * time.Second
	for _, job := range m.jobs {
		if job == nil {
			continue
		}
		if job.CancelRequested && job.ReceiveToken != "" {
			if job.NextAttemptAt.After(now) {
				remaining := job.NextAttemptAt.Sub(now)
				if remaining < wait {
					wait = remaining
				}
				continue
			}
			candidates = append(candidates, job)
			continue
		}
		if job.TransferJob.terminal() || (job.State != TransferQueued && job.State != TransferRetrying) {
			continue
		}
		if job.State == TransferRetrying && job.NextAttemptAt.After(now) {
			remaining := job.NextAttemptAt.Sub(now)
			if remaining < wait {
				wait = remaining
			}
			continue
		}
		candidates = append(candidates, job)
	}
	if len(candidates) == 0 {
		if wait < 50*time.Millisecond {
			wait = 50 * time.Millisecond
		}
		return "", wait
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
	})
	job := candidates[0]
	if job.CancelRequested {
		job.State = TransferCanceled
		job.Cancellable = false
		job.Retryable = false
		job.NextAttemptAt = time.Time{}
		job.UpdatedAt = now
		if job.FinishedAt.IsZero() {
			job.FinishedAt = now
		}
		if err := m.saveJobLocked(job); err != nil {
			return "", time.Second
		}
		return job.ID, 0
	}
	job.State = TransferConnecting
	job.Error = ""
	job.NextAttemptAt = time.Time{}
	job.UpdatedAt = now
	if job.StartedAt.IsZero() {
		job.StartedAt = now
	}
	job.Cancellable = true
	job.Retryable = false
	if err := m.saveJobLocked(job); err != nil {
		job.State = TransferFailed
		job.Error = err.Error()
		job.Cancellable = false
		job.Retryable = true
		job.FinishedAt = now
		_ = m.saveJobLocked(job)
		return "", time.Second
	}
	return job.ID, 0
}

func (m *Manager) processOne(id string) {
	jobCtx, cancel := context.WithCancel(m.runCtx)
	m.mu.Lock()
	job := m.jobs[id]
	if job == nil {
		m.mu.Unlock()
		cancel()
		return
	}
	m.currentID = id
	m.currentCancel = cancel
	cancelPending := job.CancelRequested
	m.mu.Unlock()

	var err error
	if !cancelPending {
		err = m.processTransfer(jobCtx, id)
	}
	cancel()

	m.mu.Lock()
	job = m.jobs[id]
	if m.currentID == id {
		m.currentID = ""
		m.currentCancel = nil
	}
	if job == nil {
		m.mu.Unlock()
		return
	}
	jobSnapshot := *job
	shutdown := m.runCtx.Err() != nil
	canceled := job.CancelRequested
	m.mu.Unlock()

	if canceled {
		if !shutdown {
			m.cancelRemoteImport(id, jobSnapshot)
		}
		return
	}
	if shutdown {
		m.requeueAfterShutdown(id)
		return
	}
	if err == nil {
		return
	}
	if errors.Is(err, ErrImportCanceled) {
		m.finishCanceledByReceiver(id)
		return
	}
	m.failOrRetry(id, err, transferErrorTemporary(err))
}

func (m *Manager) processTransfer(ctx context.Context, id string) error {
	job, err := m.jobSnapshot(id)
	if err != nil {
		return err
	}
	record, err := m.backups.BackupRecord(job.BackupID)
	if err != nil {
		if errors.Is(err, backup.ErrBackupNotFound) {
			return permanentTransferFailure(err)
		}
		return err
	}
	if record.Size != job.Size || !strings.EqualFold(record.SHA256, job.SHA256) || record.VerificationStatus != "verified" {
		return permanentTransferFailure(errors.New("源备份包已变化，发送任务已停止"))
	}
	file, info, _, err := m.backups.OpenBackup(job.BackupID)
	if err != nil {
		if errors.Is(err, backup.ErrBackupNotFound) {
			return permanentTransferFailure(err)
		}
		return err
	}
	defer file.Close()
	if info.Size() != job.Size {
		return permanentTransferFailure(errors.New("源备份包大小已变化"))
	}

	capabilities, err := m.peerCapabilities(ctx, job.TargetURL, job.ReceiveToken)
	if err != nil {
		return err
	}
	if capabilities.RangeSize != TransferRangeSize ||
		capabilities.ParallelStreams <= 0 ||
		!containsInt(capabilities.BackupFormatVersions, backup.FormatVersion) {
		return permanentTransferFailure(errors.New("目标服务器不支持当前备份传输协议"))
	}
	status, err := m.peerBeginImport(ctx, job.TargetURL, job.ReceiveToken, ImportRequest{
		TransferID:     job.ID,
		SourceServerID: m.ServerID(),
		BackupID:       job.BackupID,
		FileName:       job.BackupName,
		Size:           job.Size,
		SHA256:         job.SHA256,
		FormatVersion:  backup.FormatVersion,
	})
	if err != nil {
		return err
	}
	if err := validateImportStatus(status, job); err != nil {
		return permanentTransferFailure(err)
	}
	if status.State == ImportCanceled {
		return ErrImportCanceled
	}
	if status.State == ImportCompleted {
		if err := validateCompletedImport(status, job); err != nil {
			return permanentTransferFailure(err)
		}
		return m.finishCompleted(id, *status.Record)
	}
	if status.State != ImportUploading && status.State != ImportFinalizing {
		return permanentTransferFailure(errors.New("目标服务器返回了无效的传输状态"))
	}
	committed, err := committedRanges(status)
	if err != nil {
		return permanentTransferFailure(err)
	}
	committedBytes := bytesForCommitted(committed, job.Size, status.RangeSize)
	if status.CommittedBytes != committedBytes {
		return permanentTransferFailure(errors.New("目标服务器返回的续传进度不一致"))
	}
	m.updateProgress(id, TransferUploading, status.TotalRanges, len(committed), committedBytes)
	if status.State == ImportUploading {
		streams := min(ParallelStreams, capabilities.ParallelStreams)
		if err := m.uploadRanges(
			ctx,
			id,
			job,
			file,
			status,
			committed,
			streams,
		); err != nil {
			return err
		}
	}
	m.updateProgress(id, TransferFinalizing, status.TotalRanges, status.TotalRanges, job.Size)
	completed, err := m.peerFinalizeImport(ctx, job.TargetURL, job.ReceiveToken, job.ID)
	if err != nil {
		return err
	}
	if err := validateImportStatus(completed, job); err != nil {
		return permanentTransferFailure(err)
	}
	if err := validateCompletedImport(completed, job); err != nil {
		return permanentTransferFailure(err)
	}
	return m.finishCompleted(id, *completed.Record)
}

func (m *Manager) uploadRanges(
	ctx context.Context,
	id string,
	job storedTransferJob,
	file io.ReaderAt,
	status ImportStatus,
	committed map[int]bool,
	streams int,
) error {
	pending := make([]int, 0, status.TotalRanges-len(committed))
	for index := 0; index < status.TotalRanges; index++ {
		if !committed[index] {
			pending = append(pending, index)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	if streams < 1 {
		streams = 1
	}
	if streams > len(pending) {
		streams = len(pending)
	}

	tracker := m.replaceOutgoingProgress(id, status.CommittedBytes)
	defer m.clearOutgoingProgress(id)
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	queue := make(chan int, len(pending))
	for _, index := range pending {
		queue <- index
	}
	close(queue)

	processedBytes := status.CommittedBytes
	processedRanges := len(committed)
	var progressMu sync.Mutex
	var errorMu sync.Mutex
	var firstError error
	recordError := func(err error) {
		if err == nil {
			return
		}
		errorMu.Lock()
		if firstError == nil {
			firstError = err
			cancel()
		}
		errorMu.Unlock()
	}

	var workers sync.WaitGroup
	workers.Add(streams)
	for range streams {
		go func() {
			defer workers.Done()
			for index := range queue {
				if err := workerCtx.Err(); err != nil {
					return
				}
				offset := int64(index) * status.RangeSize
				size := min(status.RangeSize, job.Size-offset)
				if size <= 0 {
					recordError(permanentTransferFailure(errors.New("备份传输区间大小无效")))
					return
				}
				tracker.begin(index)
				section := io.NewSectionReader(file, offset, size)
				err := m.peerPutRange(
					workerCtx,
					job.TargetURL,
					job.ReceiveToken,
					job.ID,
					index,
					offset,
					size,
					job.Size,
					section,
					func(bytes int64) { tracker.add(index, bytes, m.nowTime()) },
				)
				if err != nil {
					tracker.abandon(index)
					recordError(err)
					return
				}
				progressMu.Lock()
				processedBytes += size
				processedRanges++
				tracker.commit(index, processedBytes, m.nowTime())
				m.updateProgress(
					id,
					TransferUploading,
					status.TotalRanges,
					processedRanges,
					processedBytes,
				)
				progressMu.Unlock()
			}
		}()
	}
	workers.Wait()
	errorMu.Lock()
	err := firstError
	errorMu.Unlock()
	if err != nil {
		return err
	}
	return ctx.Err()
}

func (m *Manager) jobSnapshot(id string) (storedTransferJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job == nil {
		return storedTransferJob{}, ErrTransferNotFound
	}
	return *job, nil
}

func (m *Manager) updateProgress(id, state string, totalRanges, processedRanges int, processedBytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job == nil || job.CancelRequested || job.TransferJob.terminal() {
		return
	}
	job.State = state
	job.TotalRanges = totalRanges
	job.ProcessedRanges = processedRanges
	job.ProcessedBytes = processedBytes
	job.UpdatedAt = m.nowTime()
	_ = m.saveJobLocked(job)
}

func (m *Manager) finishCompleted(id string, record backup.BackupRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job == nil {
		return ErrTransferNotFound
	}
	if job.CancelRequested {
		return context.Canceled
	}
	now := m.nowTime()
	job.State = TransferCompleted
	job.ProcessedBytes = job.Size
	job.ProcessedRanges = job.TotalRanges
	job.TargetBackupID = record.ID
	job.TargetBackupName = record.Name
	job.Error = ""
	job.UpdatedAt = now
	job.FinishedAt = now
	job.Cancellable = false
	job.Retryable = false
	job.ReceiveToken = ""
	job.CancelRequested = false
	job.NextAttemptAt = time.Time{}
	return m.saveJobLocked(job)
}

func (m *Manager) finishCanceled(id string) {
	m.finishCanceledWithError(id, "")
}

func (m *Manager) finishCanceledByReceiver(id string) {
	m.finishCanceledWithError(id, ErrImportCanceled.Error())
}

func (m *Manager) finishCanceledWithError(id, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job == nil {
		return
	}
	now := m.nowTime()
	job.State = TransferCanceled
	job.Error = message
	job.NextAttemptAt = time.Time{}
	job.UpdatedAt = now
	if job.FinishedAt.IsZero() {
		job.FinishedAt = now
	}
	job.Cancellable = false
	job.Retryable = false
	job.ReceiveToken = ""
	job.CancelRequested = false
	_ = m.saveJobLocked(job)
}

func (m *Manager) cancelRemoteImport(id string, job storedTransferJob) {
	if strings.TrimSpace(job.ReceiveToken) == "" {
		m.finishCanceled(id)
		return
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(m.runCtx, 30*time.Second)
	err := m.peerCancelImport(cleanupCtx, job.TargetURL, job.ReceiveToken, id)
	cleanupCancel()
	if err == nil {
		m.finishCanceled(id)
		return
	}
	if m.runCtx.Err() != nil {
		return
	}
	m.retryRemoteCancellation(id, err)
}

func (m *Manager) retryRemoteCancellation(id string, cause error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job == nil || !job.CancelRequested || strings.TrimSpace(job.ReceiveToken) == "" {
		return
	}
	now := m.nowTime()
	job.Attempts++
	job.State = TransferCanceled
	job.Error = publicTransferError(cause)
	job.NextAttemptAt = now.Add(retryDelay(job.Attempts))
	job.UpdatedAt = now
	if job.FinishedAt.IsZero() {
		job.FinishedAt = now
	}
	job.Cancellable = false
	job.Retryable = false
	_ = m.saveJobLocked(job)
	m.signal()
}

func (m *Manager) requeueAfterShutdown(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job == nil || job.CancelRequested || job.TransferJob.terminal() {
		return
	}
	job.State = TransferQueued
	job.UpdatedAt = m.nowTime()
	job.Cancellable = true
	_ = m.saveJobLocked(job)
}

func (m *Manager) failOrRetry(id string, cause error, temporary bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job == nil || job.CancelRequested || job.TransferJob.terminal() {
		return
	}
	now := m.nowTime()
	job.Attempts++
	job.Error = publicTransferError(cause)
	job.UpdatedAt = now
	if temporary && job.Attempts < maxAutomaticAttempts {
		job.State = TransferRetrying
		job.NextAttemptAt = now.Add(retryDelay(job.Attempts))
		job.Cancellable = true
		job.Retryable = false
		_ = m.saveJobLocked(job)
		m.signal()
		return
	}
	job.State = TransferFailed
	job.NextAttemptAt = time.Time{}
	job.FinishedAt = now
	job.Cancellable = false
	job.Retryable = temporary && strings.TrimSpace(job.ReceiveToken) != ""
	if !job.Retryable {
		job.ReceiveToken = ""
	}
	_ = m.saveJobLocked(job)
}

func retryDelay(attempt int) time.Duration {
	delays := []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute}
	if attempt <= 0 {
		return delays[0]
	}
	if attempt > len(delays) {
		return delays[len(delays)-1]
	}
	return delays[attempt-1]
}

func publicTransferError(err error) string {
	if err == nil {
		return "服务器发送失败"
	}
	if errors.Is(err, context.Canceled) {
		return "服务器发送已中断"
	}
	message := strings.TrimSpace(err.Error())
	if message == "" {
		return "服务器发送失败"
	}
	return message
}

func normalizeTargetURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) > 2048 {
		return "", errors.New("目标服务器地址过长")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil || !parsed.IsAbs() || parsed.Opaque != "" {
		return "", errors.New("请输入完整的目标服务器 HTTP 或 HTTPS 地址")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("目标服务器仅支持 HTTP 或 HTTPS")
	}
	if parsed.User != nil || parsed.Hostname() == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("目标服务器地址不能包含凭据、查询参数或片段")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", errors.New("目标服务器地址不能包含额外路径")
	}
	parsed.Path = ""
	parsed.RawPath = ""
	return strings.TrimSuffix(parsed.String(), "/"), nil
}

func validateImportStatus(status ImportStatus, job storedTransferJob) error {
	if status.TransferID != job.ID || status.Size != job.Size || !strings.EqualFold(status.SHA256, job.SHA256) {
		return errors.New("目标服务器返回了不匹配的传输会话")
	}
	if status.RangeSize != TransferRangeSize || status.RangeSize > maxPeerRangeSize {
		return errors.New("目标服务器传输区间大小无效")
	}
	expectedRanges := int((job.Size-1)/status.RangeSize + 1)
	if status.TotalRanges != expectedRanges || status.TotalRanges <= 0 {
		return errors.New("目标服务器传输区间数量无效")
	}
	return nil
}

func validateCompletedImport(status ImportStatus, job storedTransferJob) error {
	if status.State != ImportCompleted || status.Record == nil ||
		status.Record.ID == "" || status.Record.Name == "" ||
		status.Record.Size != job.Size || status.Record.VerificationStatus != "verified" ||
		!strings.EqualFold(status.Record.SHA256, job.SHA256) {
		return errors.New("目标服务器未返回有效的备份入库回执")
	}
	return nil
}

func committedRanges(status ImportStatus) (map[int]bool, error) {
	committed := make(map[int]bool)
	for _, indexRange := range status.Committed {
		if indexRange.Start < 0 || indexRange.End < indexRange.Start || indexRange.End >= status.TotalRanges {
			return nil, errors.New("目标服务器续传区间无效")
		}
		for index := indexRange.Start; index <= indexRange.End; index++ {
			committed[index] = true
		}
	}
	return committed, nil
}

func bytesForCommitted(committed map[int]bool, size, rangeSize int64) int64 {
	var total int64
	totalRanges := int((size-1)/rangeSize + 1)
	for index := 0; index < totalRanges; index++ {
		if !committed[index] {
			continue
		}
		rangeBytes := rangeSize
		if index == totalRanges-1 {
			rangeBytes = size - int64(index)*rangeSize
		}
		total += rangeBytes
	}
	return total
}

func containsInt(values []int, expected int) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(strings.TrimSpace(value))
	return err == nil && len(decoded) == sha256.Size
}
