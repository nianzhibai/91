package backuptransfer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/video-site/backend/internal/atomicfile"
	"github.com/video-site/backend/internal/backup"
)

const maxStateFileBytes int64 = 16 << 20

type serverIdentity struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
}

type storedReceiveToken struct {
	ID              string    `json:"id"`
	Hash            string    `json:"hash"`
	CreatedAt       time.Time `json:"createdAt"`
	ExpiresAt       time.Time `json:"expiresAt"`
	LastUsedAt      time.Time `json:"lastUsedAt,omitempty"`
	BoundTransferID string    `json:"boundTransferId,omitempty"`
	Revoked         bool      `json:"revoked,omitempty"`
}

type storedReceipt struct {
	TokenID   string               `json:"tokenId"`
	Request   ImportRequest        `json:"request"`
	UploadID  string               `json:"uploadId,omitempty"`
	State     string               `json:"state"`
	Record    *backup.BackupRecord `json:"record,omitempty"`
	CreatedAt time.Time            `json:"createdAt"`
	UpdatedAt time.Time            `json:"updatedAt"`
}

type receiverState struct {
	Tokens   map[string]storedReceiveToken `json:"tokens"`
	Receipts map[string]storedReceipt      `json:"receipts"`
}

type storedTransferJob struct {
	TransferJob
	ReceiveToken    string `json:"receiveToken,omitempty"`
	CancelRequested bool   `json:"cancelRequested,omitempty"`
}

func readJSONFile(path string, destination any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("backup transfer: state is not a regular file")
	}
	if stateFilePermissionsTooBroad(info.Mode()) {
		return errors.New("backup transfer: state file permissions are too broad")
	}
	if info.Size() > maxStateFileBytes {
		return errors.New("backup transfer: state file is too large")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxStateFileBytes+1))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("backup transfer: state has trailing JSON")
		}
		return err
	}
	return nil
}

func writeJSONAtomic(path string, value any) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".transfer-state-*.tmp")
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
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	removeTemporary = false
	return atomicfile.SyncDirectory(directory)
}

func validOpaqueID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validJobFileName(name string) (string, bool) {
	if filepath.Ext(name) != ".json" {
		return "", false
	}
	id := strings.TrimSuffix(name, ".json")
	return id, validOpaqueID(id)
}

func receiptKey(tokenID, transferID string) string {
	return tokenID + ":" + transferID
}

func (m *Manager) saveReceiverLocked() error {
	return writeJSONAtomic(m.receiverPath, m.receiver)
}

func (m *Manager) saveJobLocked(job *storedTransferJob) error {
	if job == nil || !validOpaqueID(job.ID) {
		return fmt.Errorf("backup transfer: invalid outgoing job")
	}
	return writeJSONAtomic(filepath.Join(m.outgoingDir, job.ID+".json"), job)
}
