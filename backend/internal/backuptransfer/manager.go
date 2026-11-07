package backuptransfer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nianzhibai/91/backend/internal/backup"
)

type Config struct {
	Backups    *backup.Manager
	RootDir    string
	HTTPClient *http.Client
	Now        func() time.Time
}

type Manager struct {
	backups      *backup.Manager
	rootDir      string
	outgoingDir  string
	receiverPath string
	identityPath string
	client       *http.Client
	now          func() time.Time

	mu               sync.Mutex
	identity         serverIdentity
	receiver         receiverState
	jobs             map[string]*storedTransferJob
	outgoingProgress map[string]*streamProgress
	incomingProgress map[string]*streamProgress
	started          bool
	closed           bool
	runCtx           context.Context
	cancel           context.CancelFunc
	wake             chan struct{}
	done             chan struct{}

	receiveMu     sync.Mutex
	currentID     string
	currentCancel context.CancelFunc
}

func New(cfg Config) (*Manager, error) {
	if cfg.Backups == nil {
		return nil, errors.New("backup transfer: backup manager is required")
	}
	rootDir, err := filepath.Abs(strings.TrimSpace(cfg.RootDir))
	if err != nil || strings.TrimSpace(cfg.RootDir) == "" {
		return nil, errors.New("backup transfer: state root is invalid")
	}
	m := &Manager{
		backups:          cfg.Backups,
		rootDir:          rootDir,
		outgoingDir:      filepath.Join(rootDir, "outgoing"),
		receiverPath:     filepath.Join(rootDir, "receiver.json"),
		identityPath:     filepath.Join(rootDir, "identity.json"),
		client:           cfg.HTTPClient,
		now:              cfg.Now,
		jobs:             make(map[string]*storedTransferJob),
		outgoingProgress: make(map[string]*streamProgress),
		incomingProgress: make(map[string]*streamProgress),
		wake:             make(chan struct{}, 1),
		done:             make(chan struct{}),
	}
	if m.client == nil {
		m.client = newPeerHTTPClient()
	}
	for _, directory := range []string{m.rootDir, m.outgoingDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("backup transfer: create state directory: %w", err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return nil, fmt.Errorf("backup transfer: secure state directory: %w", err)
		}
	}
	if err := m.loadIdentity(); err != nil {
		return nil, err
	}
	if err := m.loadReceiver(); err != nil {
		return nil, err
	}
	if err := m.loadJobs(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) Start(parent context.Context) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.started || m.closed {
		m.mu.Unlock()
		return
	}
	m.runCtx, m.cancel = context.WithCancel(parent)
	m.started = true
	m.mu.Unlock()
	go m.run()
	m.signal()
}

func (m *Manager) Shutdown(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		done := m.done
		started := m.started
		m.mu.Unlock()
		if !started {
			return nil
		}
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m.closed = true
	cancel := m.cancel
	started := m.started
	done := m.done
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if !started {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) ServerID() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.identity.ID
}

func (m *Manager) Capabilities() Capabilities {
	return Capabilities{
		BackupFormatVersions: []int{backup.FormatVersion},
		RangeSize:            TransferRangeSize,
		ParallelStreams:      ParallelStreams,
	}
}

func (m *Manager) nowTime() time.Time {
	if m != nil && m.now != nil {
		return m.now().UTC()
	}
	return time.Now().UTC()
}

func (m *Manager) signal() {
	if m == nil {
		return
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) loadIdentity() error {
	var identity serverIdentity
	if err := readJSONFile(m.identityPath, &identity); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("backup transfer: read server identity: %w", err)
		}
		id, err := randomOpaqueID()
		if err != nil {
			return err
		}
		identity = serverIdentity{ID: id, CreatedAt: m.nowTime()}
		if err := writeJSONAtomic(m.identityPath, identity); err != nil {
			return fmt.Errorf("backup transfer: write server identity: %w", err)
		}
	}
	if !validOpaqueID(identity.ID) || identity.CreatedAt.IsZero() {
		return errors.New("backup transfer: server identity is invalid")
	}
	m.identity = identity
	return nil
}

func (m *Manager) loadReceiver() error {
	state := receiverState{
		Tokens:   make(map[string]storedReceiveToken),
		Receipts: make(map[string]storedReceipt),
	}
	if err := readJSONFile(m.receiverPath, &state); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("backup transfer: read receiver state: %w", err)
	}
	if state.Tokens == nil {
		state.Tokens = make(map[string]storedReceiveToken)
	}
	if state.Receipts == nil {
		state.Receipts = make(map[string]storedReceipt)
	}
	now := m.nowTime()
	changed := false
	for id, token := range state.Tokens {
		if !validOpaqueID(id) || token.ID != id || !validSHA256(token.Hash) ||
			token.CreatedAt.IsZero() || token.ExpiresAt.IsZero() ||
			token.ExpiresAt.Before(token.CreatedAt) ||
			!token.boundTransferIDIsValid() {
			return fmt.Errorf("backup transfer: receive token state %q is invalid", id)
		}
	}
	for key, receipt := range state.Receipts {
		if err := validateStoredReceipt(key, receipt, state.Tokens); err != nil {
			return fmt.Errorf("backup transfer: receipt %q is invalid: %w", key, err)
		}
		token := state.Tokens[receipt.TokenID]
		terminal := receipt.State == ImportCompleted || receipt.State == ImportCanceled
		if (terminal && now.After(receipt.UpdatedAt.Add(completedReceiptTTL))) ||
			(!terminal && now.After(token.ExpiresAt.Add(completedReceiptTTL))) {
			delete(state.Receipts, key)
			changed = true
		}
	}
	for id, token := range state.Tokens {
		if now.After(token.ExpiresAt.Add(completedReceiptTTL)) {
			delete(state.Tokens, id)
			changed = true
		}
	}
	m.receiver = state
	if changed {
		return m.saveReceiverLocked()
	}
	return nil
}

func (token storedReceiveToken) boundTransferIDIsValid() bool {
	return token.BoundTransferID == "" || validOpaqueID(token.BoundTransferID)
}

func validateStoredReceipt(
	key string,
	receipt storedReceipt,
	tokens map[string]storedReceiveToken,
) error {
	if !validOpaqueID(receipt.TokenID) || !validOpaqueID(receipt.Request.TransferID) ||
		key != receiptKey(receipt.TokenID, receipt.Request.TransferID) ||
		(receipt.UploadID != "" && !validOpaqueID(receipt.UploadID)) ||
		receipt.CreatedAt.IsZero() || receipt.UpdatedAt.IsZero() ||
		receipt.UpdatedAt.Before(receipt.CreatedAt) {
		return errors.New("invalid receipt identity")
	}
	if receipt.Request.FileName != filepath.Base(strings.TrimSpace(receipt.Request.FileName)) ||
		receipt.Request.SHA256 != strings.ToLower(strings.TrimSpace(receipt.Request.SHA256)) {
		return errors.New("invalid normalized receipt request")
	}
	if err := validateImportRequest(receipt.Request); err != nil {
		return err
	}
	token, ok := tokens[receipt.TokenID]
	if !ok || token.BoundTransferID != receipt.Request.TransferID {
		return errors.New("receipt token binding is missing")
	}
	switch receipt.State {
	case ImportUploading, ImportFinalizing:
		if receipt.UploadID == "" || receipt.Record != nil {
			return errors.New("active receipt contains a terminal record")
		}
	case ImportCompleted:
		if receipt.UploadID == "" || receipt.Record == nil || receipt.Record.ID == "" || receipt.Record.Name == "" ||
			receipt.Record.Size != receipt.Request.Size || receipt.Record.VerificationStatus != "verified" ||
			!strings.EqualFold(receipt.Record.SHA256, receipt.Request.SHA256) {
			return errors.New("completed receipt record is invalid")
		}
	case ImportCanceled:
		if receipt.Record != nil {
			return errors.New("canceled receipt contains a record")
		}
	default:
		return errors.New("invalid receipt state")
	}
	return nil
}

func (m *Manager) loadJobs() error {
	entries, err := os.ReadDir(m.outgoingDir)
	if err != nil {
		return fmt.Errorf("backup transfer: list outgoing jobs: %w", err)
	}
	now := m.nowTime()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		id, ok := validJobFileName(entry.Name())
		if !ok {
			continue
		}
		var job storedTransferJob
		if err := readJSONFile(filepath.Join(m.outgoingDir, entry.Name()), &job); err != nil {
			return fmt.Errorf("backup transfer: read outgoing job %s: %w", id, err)
		}
		if err := m.validateStoredJob(id, &job); err != nil {
			return err
		}
		if job.TransferJob.terminal() && !job.CancelRequested && !job.FinishedAt.IsZero() &&
			now.After(job.FinishedAt.Add(finishedJobTTL)) {
			if err := os.Remove(filepath.Join(m.outgoingDir, entry.Name())); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("backup transfer: remove expired outgoing job %s: %w", id, err)
			}
			continue
		}
		changed := false
		job.BytesPerSecond = 0
		if job.CancelRequested {
			if job.ReceiveToken == "" {
				job.CancelRequested = false
				job.NextAttemptAt = time.Time{}
				changed = true
			} else if job.State != TransferCanceled || job.FinishedAt.IsZero() ||
				job.Cancellable || job.Retryable {
				job.State = TransferCanceled
				job.Cancellable = false
				job.Retryable = false
				if job.FinishedAt.IsZero() {
					job.FinishedAt = now
				}
				job.UpdatedAt = now
				changed = true
			}
		} else {
			switch job.State {
			case TransferConnecting, TransferUploading, TransferFinalizing:
				job.State = TransferQueued
				job.Cancellable = true
				job.UpdatedAt = now
				changed = true
			}
		}
		copy := job
		m.jobs[id] = &copy
		if changed {
			if err := m.saveJobLocked(&copy); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) validateStoredJob(id string, job *storedTransferJob) error {
	if job == nil || job.ID != id || !validOpaqueID(job.ID) || strings.TrimSpace(job.BackupID) == "" ||
		strings.TrimSpace(job.BackupName) == "" || job.Size <= 0 || !validSHA256(job.SHA256) ||
		job.CreatedAt.IsZero() || job.UpdatedAt.IsZero() || job.UpdatedAt.Before(job.CreatedAt) ||
		job.ProcessedBytes < 0 || job.ProcessedBytes > job.Size || job.TotalRanges < 0 ||
		job.ProcessedRanges < 0 || job.ProcessedRanges > job.TotalRanges ||
		(!job.FinishedAt.IsZero() && job.FinishedAt.Before(job.CreatedAt)) {
		return fmt.Errorf("backup transfer: outgoing job %s is invalid", id)
	}
	normalizedTarget, err := normalizeTargetURL(job.TargetURL)
	if err != nil || normalizedTarget != job.TargetURL {
		return fmt.Errorf("backup transfer: outgoing job %s has an invalid target", id)
	}
	if job.ReceiveToken != "" {
		if _, ok := receiveTokenID(job.ReceiveToken); !ok {
			return fmt.Errorf("backup transfer: outgoing job %s has an invalid receive token", id)
		}
	}
	switch job.State {
	case TransferQueued, TransferConnecting, TransferUploading, TransferFinalizing, TransferRetrying:
		if job.ReceiveToken == "" || !job.FinishedAt.IsZero() {
			return fmt.Errorf("backup transfer: active outgoing job %s is invalid", id)
		}
	case TransferCompleted:
		if job.ReceiveToken != "" || job.FinishedAt.IsZero() ||
			job.TargetBackupID == "" || job.TargetBackupName == "" {
			return fmt.Errorf("backup transfer: completed outgoing job %s is invalid", id)
		}
	case TransferCanceled:
		if job.FinishedAt.IsZero() || (!job.CancelRequested && job.ReceiveToken != "") {
			return fmt.Errorf("backup transfer: canceled outgoing job %s is invalid", id)
		}
	case TransferFailed:
		if job.FinishedAt.IsZero() || (job.Retryable && job.ReceiveToken == "") ||
			(!job.Retryable && job.ReceiveToken != "") {
			return fmt.Errorf("backup transfer: failed outgoing job %s is invalid", id)
		}
	default:
		return fmt.Errorf("backup transfer: outgoing job %s has an invalid state", id)
	}
	return nil
}

func randomOpaqueID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func randomSecret() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func sortedJobs(jobs map[string]*storedTransferJob) []TransferJob {
	out := make([]TransferJob, 0, len(jobs))
	for _, stored := range jobs {
		if stored == nil {
			continue
		}
		job := stored.TransferJob
		out = append(out, job)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}
