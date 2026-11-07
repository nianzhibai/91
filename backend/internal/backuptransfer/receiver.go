package backuptransfer

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/backup"
)

const receiveTokenPrefix = "v91r_"

func (m *Manager) GenerateReceiveToken(ttl time.Duration) (ReceiveToken, error) {
	if m == nil {
		return ReceiveToken{}, ErrUnavailable
	}
	if ttl <= 0 {
		ttl = defaultPairingTTL
	}
	if ttl > time.Hour {
		ttl = time.Hour
	}
	id, err := randomOpaqueID()
	if err != nil {
		return ReceiveToken{}, err
	}
	secret, err := randomSecret()
	if err != nil {
		return ReceiveToken{}, err
	}
	raw := receiveTokenPrefix + id + "_" + secret
	digest := sha256.Sum256([]byte(raw))
	now := m.nowTime()
	stored := storedReceiveToken{
		ID:        id,
		Hash:      hex.EncodeToString(digest[:]),
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.receiver.Tokens[id] = stored
	if err := m.saveReceiverLocked(); err != nil {
		delete(m.receiver.Tokens, id)
		return ReceiveToken{}, err
	}
	return ReceiveToken{
		ID:        id,
		Token:     raw,
		CreatedAt: stored.CreatedAt,
		ExpiresAt: stored.ExpiresAt,
	}, nil
}

func (m *Manager) ListReceiveTokens() []ReceiveTokenInfo {
	if m == nil {
		return []ReceiveTokenInfo{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ReceiveTokenInfo, 0, len(m.receiver.Tokens))
	for _, token := range m.receiver.Tokens {
		out = append(out, ReceiveTokenInfo{
			ID:              token.ID,
			CreatedAt:       token.CreatedAt,
			ExpiresAt:       token.ExpiresAt,
			LastUsedAt:      token.LastUsedAt,
			BoundTransferID: token.BoundTransferID,
			Revoked:         token.Revoked,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (m *Manager) ListReceiveTransfers(ctx context.Context) []ReceiveTransfer {
	if m == nil {
		return []ReceiveTransfer{}
	}
	m.mu.Lock()
	receipts := make([]storedReceipt, 0, len(m.receiver.Receipts))
	progress := make(map[string]*streamProgress, len(m.incomingProgress))
	for _, receipt := range m.receiver.Receipts {
		receipts = append(receipts, receipt)
	}
	for id, tracker := range m.incomingProgress {
		progress[id] = tracker
	}
	m.mu.Unlock()

	now := m.nowTime()
	result := make([]ReceiveTransfer, 0, len(receipts))
	for _, receipt := range receipts {
		item := ReceiveTransfer{
			ID:             receipt.Request.TransferID,
			SourceServerID: receipt.Request.SourceServerID,
			BackupID:       receipt.Request.BackupID,
			BackupName:     receipt.Request.FileName,
			State:          receipt.State,
			Size:           receipt.Request.Size,
			CreatedAt:      receipt.CreatedAt,
			UpdatedAt:      receipt.UpdatedAt,
			Cancellable:    receipt.State == ImportUploading,
		}
		status, err := m.statusForReceipt(ctx, receipt)
		if err != nil {
			item.State = TransferFailed
			item.Error = err.Error()
		} else {
			item.State = status.State
			item.ProcessedBytes = status.CommittedBytes
			if status.Record != nil {
				item.TargetBackupID = status.Record.ID
			}
		}
		if tracker := progress[item.ID]; tracker != nil {
			processed, bytesPerSecond, lastActivity := tracker.snapshot(now, item.Size)
			if processed > item.ProcessedBytes {
				item.ProcessedBytes = processed
			}
			item.BytesPerSecond = bytesPerSecond
			if lastActivity.After(item.UpdatedAt) {
				item.UpdatedAt = lastActivity
			}
		}
		if item.State == ImportCompleted || item.State == ImportCanceled || item.State == TransferFailed {
			item.FinishedAt = receipt.UpdatedAt
			m.clearIncomingProgress(item.ID)
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID > result[j].ID
		}
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result
}

func (m *Manager) RevokeReceiveToken(id string) error {
	if m == nil {
		return ErrUnavailable
	}
	id = strings.TrimSpace(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	token, ok := m.receiver.Tokens[id]
	if !ok {
		return ErrUnauthorized
	}
	revokedAt := m.nowTime()
	if revokedAt.Before(token.CreatedAt) {
		revokedAt = token.CreatedAt
	}
	token.Revoked = true
	token.ExpiresAt = revokedAt
	m.receiver.Tokens[id] = token
	return m.saveReceiverLocked()
}

func (m *Manager) AuthorizeReceiveToken(raw string) error {
	if m == nil {
		return ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := m.authenticateTokenLocked(raw, "")
	return err
}

func (m *Manager) BeginImport(ctx context.Context, rawToken string, request ImportRequest) (ImportStatus, error) {
	if m == nil {
		return ImportStatus{}, ErrUnavailable
	}
	request.FileName = filepath.Base(strings.TrimSpace(request.FileName))
	request.SHA256 = strings.ToLower(strings.TrimSpace(request.SHA256))
	if err := validateImportRequest(request); err != nil {
		return ImportStatus{}, err
	}
	m.receiveMu.Lock()
	defer m.receiveMu.Unlock()

	m.mu.Lock()
	token, err := m.authenticateTokenLocked(rawToken, request.TransferID)
	if err != nil {
		m.mu.Unlock()
		return ImportStatus{}, err
	}
	key := receiptKey(token.ID, request.TransferID)
	if existing, ok := m.receiver.Receipts[key]; ok {
		if !sameImportRequest(existing.Request, request) {
			m.mu.Unlock()
			return ImportStatus{}, ErrImportConflict
		}
		m.mu.Unlock()
		status, statusErr := m.statusForReceipt(ctx, existing)
		if !errors.Is(statusErr, ErrImportNotFound) {
			if statusErr == nil {
				if status.State != ImportCompleted && status.RangeSize != TransferRangeSize {
					_ = m.backups.CancelUpload(existing.UploadID)
					m.clearIncomingProgress(request.TransferID)
					return m.restartExpiredImport(ctx, rawToken, key, existing)
				}
				m.incomingProgressFor(request.TransferID, status.CommittedBytes)
				statusErr = m.touchReceiveToken(existing.TokenID, request.TransferID)
			}
			return status, statusErr
		}
		// A sender may legitimately resume after the target's staging upload
		// expired. Keep the durable transfer receipt and replace only its
		// staging session, so a new pairing code and sender job are unnecessary.
		return m.restartExpiredImport(ctx, rawToken, key, existing)
	}
	if token.BoundTransferID != "" && token.BoundTransferID != request.TransferID {
		m.mu.Unlock()
		return ImportStatus{}, ErrTokenBound
	}
	m.mu.Unlock()

	session, err := m.backups.BeginRangeUpload(ctx, backup.BeginUploadInput{
		FileName: request.FileName,
		Size:     request.Size,
		SHA256:   request.SHA256,
	}, TransferRangeSize)
	if err != nil {
		return ImportStatus{}, err
	}
	now := m.nowTime()
	receipt := storedReceipt{
		TokenID:   token.ID,
		Request:   request,
		UploadID:  session.ID,
		State:     ImportUploading,
		CreatedAt: now,
		UpdatedAt: now,
	}
	m.mu.Lock()
	latest, authErr := m.authenticateTokenLocked(rawToken, request.TransferID)
	if authErr == nil && latest.BoundTransferID != "" && latest.BoundTransferID != request.TransferID {
		authErr = ErrTokenBound
	}
	if authErr == nil {
		latest.BoundTransferID = request.TransferID
		latest.LastUsedAt = now
		latest.ExpiresAt = now.Add(boundTokenTTL)
		m.receiver.Tokens[latest.ID] = latest
		m.receiver.Receipts[key] = receipt
		authErr = m.saveReceiverLocked()
	}
	m.mu.Unlock()
	if authErr != nil {
		_ = m.backups.CancelUpload(session.ID)
		return ImportStatus{}, authErr
	}
	status := importStatusFromSession(receipt, session)
	m.incomingProgressFor(request.TransferID, status.CommittedBytes)
	return status, nil
}

func (m *Manager) restartExpiredImport(
	ctx context.Context,
	rawToken string,
	key string,
	receipt storedReceipt,
) (ImportStatus, error) {
	session, err := m.backups.BeginRangeUpload(ctx, backup.BeginUploadInput{
		FileName: receipt.Request.FileName,
		Size:     receipt.Request.Size,
		SHA256:   receipt.Request.SHA256,
	}, TransferRangeSize)
	if err != nil {
		return ImportStatus{}, err
	}
	now := m.nowTime()
	m.mu.Lock()
	token, authErr := m.authenticateTokenLocked(rawToken, receipt.Request.TransferID)
	current, exists := m.receiver.Receipts[key]
	if authErr == nil && (!exists || current.UploadID != receipt.UploadID) {
		authErr = ErrImportConflict
	}
	if authErr == nil {
		current.UploadID = session.ID
		current.State = ImportUploading
		current.Record = nil
		current.UpdatedAt = now
		token.LastUsedAt = now
		token.ExpiresAt = now.Add(boundTokenTTL)
		m.receiver.Tokens[token.ID] = token
		m.receiver.Receipts[key] = current
		authErr = m.saveReceiverLocked()
		receipt = current
	}
	m.mu.Unlock()
	if authErr != nil {
		_ = m.backups.CancelUpload(session.ID)
		return ImportStatus{}, authErr
	}
	status := importStatusFromSession(receipt, session)
	m.incomingProgressFor(receipt.Request.TransferID, status.CommittedBytes)
	return status, nil
}

func (m *Manager) ImportStatus(ctx context.Context, rawToken, transferID string) (ImportStatus, error) {
	receipt, err := m.authorizedReceipt(rawToken, transferID)
	if err != nil {
		return ImportStatus{}, err
	}
	status, err := m.statusForReceipt(ctx, receipt)
	if err == nil {
		if touchErr := m.touchReceiveToken(receipt.TokenID, transferID); touchErr != nil {
			return ImportStatus{}, touchErr
		}
	}
	return status, err
}

func (m *Manager) PutImportRange(
	ctx context.Context,
	rawToken string,
	transferID string,
	index int,
	offset, size, totalSize int64,
	body io.Reader,
) error {
	receipt, err := m.authorizedReceipt(rawToken, transferID)
	if err != nil {
		return err
	}
	if receipt.State != ImportUploading {
		if receipt.State == ImportCompleted {
			return nil
		}
		if receipt.State == ImportCanceled {
			return ErrImportCanceled
		}
		return backup.ErrUploadFinalizing
	}
	expectedSize := transferRangeBytes(receipt.Request.Size, index)
	expectedOffset := int64(index) * TransferRangeSize
	if expectedSize <= 0 || offset != expectedOffset || size != expectedSize || totalSize != receipt.Request.Size {
		return errors.New("传输区间范围与备份元数据不一致")
	}
	session, err := m.backups.UploadStatus(receipt.UploadID)
	if err != nil {
		return err
	}
	committedBytes := uploadSessionBytes(session)
	tracker := m.incomingProgressFor(transferID, committedBytes)
	if !tracker.begin(index) {
		return backup.ErrUploadRangeBusy
	}
	updated, wrote, err := m.backups.PutRange(
		ctx,
		receipt.UploadID,
		index,
		body,
		func(bytes int64) { tracker.add(index, bytes, m.nowTime()) },
	)
	if err != nil {
		tracker.abandon(index)
		if m.receiptCanceled(receipt.TokenID, transferID) {
			return ErrImportCanceled
		}
		return err
	}
	if wrote {
		tracker.commit(index, uploadSessionBytes(updated), m.nowTime())
	} else {
		tracker.abandon(index)
		tracker.setCommitted(uploadSessionBytes(updated))
	}
	return m.touchReceiveToken(receipt.TokenID, transferID)
}

func (m *Manager) FinalizeImport(ctx context.Context, rawToken, transferID string) (ImportStatus, error) {
	if m == nil {
		return ImportStatus{}, ErrUnavailable
	}
	m.receiveMu.Lock()
	defer m.receiveMu.Unlock()
	receipt, err := m.authorizedReceipt(rawToken, transferID)
	if err != nil {
		return ImportStatus{}, err
	}
	if receipt.State == ImportCompleted && receipt.Record != nil {
		return importStatusFromReceipt(receipt), nil
	}
	if receipt.State == ImportCanceled {
		return ImportStatus{}, ErrImportCanceled
	}
	if err := m.setReceiptState(receipt.TokenID, transferID, ImportFinalizing, nil); err != nil {
		return ImportStatus{}, err
	}
	m.clearIncomingProgress(transferID)
	record, finalizeErr := m.backups.FinalizeUpload(ctx, receipt.UploadID, receipt.Request.SHA256)
	if errors.Is(finalizeErr, backup.ErrUploadNotFound) {
		var found bool
		record, found, finalizeErr = m.backups.FindImportedUpload(ctx, receipt.UploadID)
		if finalizeErr == nil && !found {
			finalizeErr = backup.ErrUploadNotFound
		}
	}
	if finalizeErr != nil {
		if stateErr := m.setReceiptState(receipt.TokenID, transferID, ImportUploading, nil); stateErr != nil {
			return ImportStatus{}, errors.Join(finalizeErr, stateErr)
		}
		return ImportStatus{}, finalizeErr
	}
	if err := m.setReceiptState(receipt.TokenID, transferID, ImportCompleted, &record); err != nil {
		return ImportStatus{}, err
	}
	_ = m.touchReceiveToken(receipt.TokenID, transferID)
	receipt.State = ImportCompleted
	receipt.Record = &record
	return importStatusFromReceipt(receipt), nil
}

func (m *Manager) CancelImport(ctx context.Context, rawToken, transferID string) error {
	if m == nil {
		return ErrUnavailable
	}
	m.receiveMu.Lock()
	defer m.receiveMu.Unlock()
	receipt, err := m.authorizedReceipt(rawToken, transferID)
	if err != nil {
		if errors.Is(err, ErrImportNotFound) && validOpaqueID(strings.TrimSpace(transferID)) {
			return nil
		}
		return err
	}
	return m.cancelReceiveReceipt(receipt)
}

// CancelReceiveTransfer is the administrator-authenticated cancellation path.
// It deliberately resolves the durable receipt locally instead of requiring
// the one-time peer credential, which is never retained in plaintext here.
func (m *Manager) CancelReceiveTransfer(transferID string) error {
	if m == nil {
		return ErrUnavailable
	}
	m.receiveMu.Lock()
	defer m.receiveMu.Unlock()
	receipt, err := m.receiveReceiptByTransferID(transferID)
	if err != nil {
		return err
	}
	return m.cancelReceiveReceipt(receipt)
}

// cancelReceiveReceipt persists the terminal state before interrupting active
// writers. That ordering prevents a canceled import from becoming resumable if
// the process exits while its staging directory is being removed.
func (m *Manager) cancelReceiveReceipt(receipt storedReceipt) error {
	if receipt.State == ImportCompleted {
		return nil
	}
	if receipt.State == ImportCanceled {
		return nil
	}
	if receipt.State == ImportFinalizing {
		return ErrTransferTerminal
	}
	transferID := receipt.Request.TransferID
	if err := m.setReceiptState(receipt.TokenID, transferID, ImportCanceled, nil); err != nil {
		return err
	}
	m.clearIncomingProgress(transferID)
	_ = m.backups.CancelUpload(receipt.UploadID)
	m.signal()
	return nil
}

func (m *Manager) receiveReceiptByTransferID(transferID string) (storedReceipt, error) {
	transferID = strings.TrimSpace(transferID)
	if !validOpaqueID(transferID) {
		return storedReceipt{}, ErrImportNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var match storedReceipt
	found := false
	for _, receipt := range m.receiver.Receipts {
		if receipt.Request.TransferID != transferID {
			continue
		}
		if found {
			return storedReceipt{}, ErrImportConflict
		}
		match = receipt
		found = true
	}
	if !found {
		return storedReceipt{}, ErrImportNotFound
	}
	return match, nil
}

// cleanupCanceledImports converges durable receiver cancellation with staging
// cleanup. A non-empty upload ID on a canceled receipt is the persistent work
// marker, so interrupted cleanup resumes after a service restart.
func (m *Manager) cleanupCanceledImports() bool {
	if m == nil {
		return false
	}
	m.receiveMu.Lock()
	defer m.receiveMu.Unlock()

	type canceledUpload struct {
		key      string
		uploadID string
	}
	m.mu.Lock()
	pendingUploads := make([]canceledUpload, 0)
	for key, receipt := range m.receiver.Receipts {
		if receipt.State == ImportCanceled && receipt.UploadID != "" {
			pendingUploads = append(pendingUploads, canceledUpload{key: key, uploadID: receipt.UploadID})
		}
	}
	m.mu.Unlock()

	pending := false
	for _, upload := range pendingUploads {
		cancelErr := m.backups.CancelUpload(upload.uploadID)
		if cancelErr == nil {
			if _, statusErr := m.backups.UploadStatus(upload.uploadID); statusErr == nil {
				pending = true
				continue
			} else if !errors.Is(statusErr, backup.ErrUploadNotFound) {
				pending = true
				continue
			}
		}
		if cancelErr != nil && !errors.Is(cancelErr, backup.ErrUploadNotFound) {
			pending = true
			continue
		}

		m.mu.Lock()
		receipt, exists := m.receiver.Receipts[upload.key]
		if !exists || receipt.State != ImportCanceled || receipt.UploadID != upload.uploadID {
			m.mu.Unlock()
			continue
		}
		previous := receipt
		receipt.UploadID = ""
		m.receiver.Receipts[upload.key] = receipt
		if err := m.saveReceiverLocked(); err != nil {
			m.receiver.Receipts[upload.key] = previous
			pending = true
		}
		m.mu.Unlock()
	}
	return pending
}

func (m *Manager) authorizedReceipt(rawToken, transferID string) (storedReceipt, error) {
	if !validOpaqueID(strings.TrimSpace(transferID)) {
		return storedReceipt{}, ErrImportNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	token, err := m.authenticateTokenLocked(rawToken, transferID)
	if err != nil {
		return storedReceipt{}, err
	}
	receipt, ok := m.receiver.Receipts[receiptKey(token.ID, transferID)]
	if !ok {
		return storedReceipt{}, ErrImportNotFound
	}
	return receipt, nil
}

func (m *Manager) receiptCanceled(tokenID, transferID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	receipt, ok := m.receiver.Receipts[receiptKey(tokenID, transferID)]
	return ok && receipt.State == ImportCanceled
}

func (m *Manager) authenticateTokenLocked(raw, transferID string) (storedReceiveToken, error) {
	id, ok := receiveTokenID(raw)
	if !ok {
		return storedReceiveToken{}, ErrUnauthorized
	}
	token, ok := m.receiver.Tokens[id]
	if !ok || token.Revoked || !m.nowTime().Before(token.ExpiresAt) {
		return storedReceiveToken{}, ErrUnauthorized
	}
	expected, err := hex.DecodeString(token.Hash)
	if err != nil || len(expected) != sha256.Size {
		return storedReceiveToken{}, ErrUnauthorized
	}
	actual := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	if subtle.ConstantTimeCompare(expected, actual[:]) != 1 {
		return storedReceiveToken{}, ErrUnauthorized
	}
	if transferID != "" && token.BoundTransferID != "" && token.BoundTransferID != transferID {
		return storedReceiveToken{}, ErrTokenBound
	}
	return token, nil
}

func (m *Manager) touchReceiveToken(tokenID, transferID string) error {
	now := m.nowTime()
	m.mu.Lock()
	defer m.mu.Unlock()
	token, ok := m.receiver.Tokens[tokenID]
	if !ok || token.Revoked || token.BoundTransferID != transferID {
		return ErrUnauthorized
	}
	if !token.LastUsedAt.IsZero() && now.Sub(token.LastUsedAt) < time.Minute &&
		token.ExpiresAt.After(now.Add(boundTokenTTL-time.Minute)) {
		return nil
	}
	token.LastUsedAt = now
	token.ExpiresAt = now.Add(boundTokenTTL)
	m.receiver.Tokens[tokenID] = token
	return m.saveReceiverLocked()
}

func (m *Manager) setReceiptState(tokenID, transferID, state string, record *backup.BackupRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := receiptKey(tokenID, transferID)
	receipt, ok := m.receiver.Receipts[key]
	if !ok {
		return ErrImportNotFound
	}
	receipt.State = state
	receipt.UpdatedAt = m.nowTime()
	if record != nil {
		copy := *record
		receipt.Record = &copy
	}
	m.receiver.Receipts[key] = receipt
	return m.saveReceiverLocked()
}

func (m *Manager) statusForReceipt(ctx context.Context, receipt storedReceipt) (ImportStatus, error) {
	if receipt.State == ImportCompleted || receipt.State == ImportCanceled {
		return importStatusFromReceipt(receipt), nil
	}
	session, err := m.backups.UploadStatus(receipt.UploadID)
	if err == nil {
		return importStatusFromSession(receipt, session), nil
	}
	if !errors.Is(err, backup.ErrUploadNotFound) {
		return ImportStatus{}, err
	}
	record, found, recoverErr := m.backups.FindImportedUpload(ctx, receipt.UploadID)
	if recoverErr != nil && !errors.Is(recoverErr, backup.ErrUploadNotFound) {
		return ImportStatus{}, recoverErr
	}
	if !found {
		return ImportStatus{}, ErrImportNotFound
	}
	if err := m.setReceiptState(receipt.TokenID, receipt.Request.TransferID, ImportCompleted, &record); err != nil {
		return ImportStatus{}, err
	}
	receipt.State = ImportCompleted
	receipt.Record = &record
	return importStatusFromReceipt(receipt), nil
}

func importStatusFromSession(receipt storedReceipt, session backup.UploadSession) ImportStatus {
	state := ImportUploading
	if session.State == ImportFinalizing {
		state = ImportFinalizing
	}
	ranges := indexRanges(session.Received)
	committedBytes := uploadSessionBytes(session)
	return ImportStatus{
		TransferID:     receipt.Request.TransferID,
		State:          state,
		Size:           session.Size,
		SHA256:         receipt.Request.SHA256,
		RangeSize:      session.ChunkSize,
		TotalRanges:    session.TotalChunks,
		Committed:      ranges,
		CommittedBytes: committedBytes,
		ExpiresAt:      session.ExpiresAt,
	}
}

func importStatusFromReceipt(receipt storedReceipt) ImportStatus {
	status := ImportStatus{
		TransferID: receipt.Request.TransferID,
		State:      receipt.State,
		Size:       receipt.Request.Size,
		SHA256:     receipt.Request.SHA256,
		RangeSize:  TransferRangeSize,
	}
	status.TotalRanges = int((status.Size-1)/status.RangeSize + 1)
	if receipt.State == ImportCompleted && receipt.Record != nil {
		copy := *receipt.Record
		status.Record = &copy
		status.CommittedBytes = status.Size
		if status.TotalRanges > 0 {
			status.Committed = []IndexRange{{Start: 0, End: status.TotalRanges - 1}}
		}
	}
	return status
}

func indexRanges(chunks []backup.UploadChunk) []IndexRange {
	if len(chunks) == 0 {
		return []IndexRange{}
	}
	indexes := make([]int, 0, len(chunks))
	for _, chunk := range chunks {
		indexes = append(indexes, chunk.Index)
	}
	sort.Ints(indexes)
	ranges := make([]IndexRange, 0, len(indexes))
	current := IndexRange{Start: indexes[0], End: indexes[0]}
	for _, index := range indexes[1:] {
		if index == current.End+1 {
			current.End = index
			continue
		}
		ranges = append(ranges, current)
		current = IndexRange{Start: index, End: index}
	}
	return append(ranges, current)
}

func uploadSessionBytes(session backup.UploadSession) int64 {
	var total int64
	for _, chunk := range session.Received {
		total += chunk.Size
	}
	return total
}

func transferRangeBytes(size int64, index int) int64 {
	if size <= 0 || index < 0 || int64(index) > (size-1)/TransferRangeSize {
		return 0
	}
	offset := int64(index) * TransferRangeSize
	return min(TransferRangeSize, size-offset)
}

func validateImportRequest(request ImportRequest) error {
	if !validOpaqueID(request.TransferID) || !validOpaqueID(request.SourceServerID) {
		return errors.New("服务器传输编号无效")
	}
	if strings.TrimSpace(request.BackupID) == "" || request.FileName == "" || request.FileName == "." ||
		!strings.EqualFold(filepath.Ext(request.FileName), ".zip") {
		return errors.New("服务器传输备份名称无效")
	}
	if request.Size <= 0 {
		return errors.New("服务器传输备份大小无效")
	}
	if request.FormatVersion != backup.FormatVersion {
		return fmt.Errorf("不支持备份格式版本 %d", request.FormatVersion)
	}
	decoded, err := hex.DecodeString(request.SHA256)
	if err != nil || len(decoded) != sha256.Size {
		return errors.New("服务器传输备份 SHA-256 无效")
	}
	return nil
}

func sameImportRequest(left, right ImportRequest) bool {
	return left.TransferID == right.TransferID &&
		left.SourceServerID == right.SourceServerID &&
		left.BackupID == right.BackupID &&
		left.FileName == right.FileName &&
		left.Size == right.Size &&
		strings.EqualFold(left.SHA256, right.SHA256) &&
		left.FormatVersion == right.FormatVersion
}

func receiveTokenID(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, receiveTokenPrefix) {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(raw, receiveTokenPrefix), "_")
	if len(parts) != 2 || !validOpaqueID(parts[0]) || len(parts[1]) != 64 {
		return "", false
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return "", false
	}
	return parts[0], true
}
