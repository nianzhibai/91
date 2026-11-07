package backuptransfer

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/backup"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
)

func newReceiverTestBackupManager(
	t *testing.T,
	now func() time.Time,
) (*backup.Manager, string) {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Config{
		Storage: config.Storage{
			DataDir:         root,
			DBPath:          filepath.Join(root, "91.db"),
			LocalPreviewDir: filepath.Join(root, "previews"),
		},
	}
	if err := os.MkdirAll(cfg.Storage.LocalPreviewDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("storage: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Open(cfg.Storage.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := backup.NewManager(backup.Config{
		Catalog:        cat,
		AppConfig:      cfg,
		ConfigPath:     configPath,
		AppVersion:     "v1.0.0",
		Now:            now,
		AvailableBytes: func(string) (int64, error) { return 1 << 40, nil },
	})
	if err != nil {
		_ = cat.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		manager.Close()
		_ = cat.Close()
	})
	return manager, root
}

func TestBeginImportRecreatesExpiredStagingAfterTokenWasRefreshed(t *testing.T) {
	current := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return current }
	backups, root := newReceiverTestBackupManager(t, now)
	stateRoot := filepath.Join(root, "peer-transfer")
	receiver, err := New(Config{Backups: backups, RootDir: stateRoot, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	token, err := receiver.GenerateReceiveToken(10 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	request := ImportRequest{
		TransferID:     strings.Repeat("1", 32),
		SourceServerID: strings.Repeat("2", 32),
		BackupID:       "video-site-91-full-source",
		FileName:       "video-site-91-full-source.zip",
		Size:           1,
		SHA256:         strings.Repeat("a", 64),
		FormatVersion:  backup.FormatVersion,
	}
	initial, err := receiver.BeginImport(context.Background(), token.Token, request)
	if err != nil {
		t.Fatal(err)
	}

	current = current.Add(70 * time.Hour)
	if _, err := receiver.ImportStatus(context.Background(), token.Token, request.TransferID); err != nil {
		t.Fatalf("refresh bound receive token: %v", err)
	}
	current = current.Add(3 * time.Hour)
	recreated, err := receiver.BeginImport(context.Background(), token.Token, request)
	if err != nil {
		t.Fatal(err)
	}
	if recreated.State != ImportUploading || !recreated.ExpiresAt.After(initial.ExpiresAt) {
		t.Fatalf("recreated import = %+v, initial expiry = %s", recreated, initial.ExpiresAt)
	}

	// The replacement receipt and refreshed token survive a target restart.
	restarted, err := New(Config{Backups: backups, RootDir: stateRoot, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := restarted.BeginImport(context.Background(), token.Token, request)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.TransferID != request.TransferID || !resumed.ExpiresAt.Equal(recreated.ExpiresAt) {
		t.Fatalf("resumed import = %+v, recreated = %+v", resumed, recreated)
	}
	stateBody, err := os.ReadFile(filepath.Join(stateRoot, "receiver.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stateBody), token.Token) {
		t.Fatal("receiver persisted the raw pairing token")
	}
}

func TestAdminCancelReceiveTransferPersistsCanceledStateWhileActiveRangeStops(t *testing.T) {
	now := func() time.Time {
		return time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	}
	backups, root := newReceiverTestBackupManager(t, now)
	receiver, err := New(Config{
		Backups: backups,
		RootDir: filepath.Join(root, "peer-transfer"),
		Now:     now,
	})
	if err != nil {
		t.Fatal(err)
	}
	token, err := receiver.GenerateReceiveToken(10 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	request := ImportRequest{
		TransferID:     strings.Repeat("3", 32),
		SourceServerID: strings.Repeat("4", 32),
		BackupID:       "cancel-active-source",
		FileName:       "cancel-active-source.zip",
		Size:           TransferRangeSize,
		SHA256:         strings.Repeat("a", 64),
		FormatVersion:  backup.FormatVersion,
	}
	if _, err := receiver.BeginImport(context.Background(), token.Token, request); err != nil {
		t.Fatal(err)
	}
	receives := receiver.ListReceiveTransfers(context.Background())
	if len(receives) != 1 || !receives[0].Cancellable {
		t.Fatalf("active receive transfer = %+v, want cancellable", receives)
	}
	receipt := receiver.receiver.Receipts[receiptKey(token.ID, request.TransferID)]

	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	putResult := make(chan error, 1)
	go func() {
		putResult <- receiver.PutImportRange(
			context.Background(),
			token.Token,
			request.TransferID,
			0,
			0,
			TransferRangeSize,
			TransferRangeSize,
			reader,
		)
	}()
	writeResult := make(chan error, 1)
	go func() {
		_, writeErr := writer.Write(make([]byte, 1<<20))
		writeResult <- writeErr
	}()
	select {
	case writeErr := <-writeResult:
		if writeErr != nil {
			t.Fatal(writeErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("active import range did not begin reading")
	}

	if err := receiver.CancelReceiveTransfer(request.TransferID); err != nil {
		t.Fatal(err)
	}
	receives = receiver.ListReceiveTransfers(context.Background())
	if len(receives) != 1 || receives[0].State != ImportCanceled || receives[0].Cancellable {
		t.Fatalf("receive state after cancellation = %+v", receives)
	}
	select {
	case putErr := <-putResult:
		if putErr == nil {
			t.Fatal("active import range succeeded after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("active import range did not stop after cancellation")
	}
	if _, err := backups.UploadStatus(receipt.UploadID); !errors.Is(err, backup.ErrUploadNotFound) {
		t.Fatalf("staging upload after cancellation = %v, want ErrUploadNotFound", err)
	}
	if pending := receiver.cleanupCanceledImports(); pending {
		t.Fatal("canceled import cleanup remained pending after writers stopped")
	}
	receiver.mu.Lock()
	cleaned := receiver.receiver.Receipts[receiptKey(token.ID, request.TransferID)]
	receiver.mu.Unlock()
	if cleaned.State != ImportCanceled || cleaned.UploadID != "" {
		t.Fatalf("cleaned canceled receipt = %+v", cleaned)
	}
	if err := receiver.CancelReceiveTransfer(request.TransferID); err != nil {
		t.Fatalf("idempotent admin cancellation: %v", err)
	}
	if err := receiver.CancelImport(context.Background(), token.Token, request.TransferID); err != nil {
		t.Fatalf("idempotent peer cancellation: %v", err)
	}
	if err := receiver.CancelReceiveTransfer(strings.Repeat("f", 32)); !errors.Is(err, ErrImportNotFound) {
		t.Fatalf("missing admin receive cancellation = %v, want ErrImportNotFound", err)
	}
}

func TestCanceledImportCleanupResumesAfterRestart(t *testing.T) {
	now := func() time.Time {
		return time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	}
	backups, root := newReceiverTestBackupManager(t, now)
	stateRoot := filepath.Join(root, "peer-transfer")
	receiver, err := New(Config{Backups: backups, RootDir: stateRoot, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	token, err := receiver.GenerateReceiveToken(10 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	request := ImportRequest{
		TransferID:     strings.Repeat("5", 32),
		SourceServerID: strings.Repeat("6", 32),
		BackupID:       "restart-cancel-source",
		FileName:       "restart-cancel-source.zip",
		Size:           1,
		SHA256:         strings.Repeat("b", 64),
		FormatVersion:  backup.FormatVersion,
	}
	if _, err := receiver.BeginImport(context.Background(), token.Token, request); err != nil {
		t.Fatal(err)
	}
	key := receiptKey(token.ID, request.TransferID)
	receipt := receiver.receiver.Receipts[key]
	if err := receiver.setReceiptState(token.ID, request.TransferID, ImportCanceled, nil); err != nil {
		t.Fatal(err)
	}

	restarted, err := New(Config{Backups: backups, RootDir: stateRoot, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(context.Background())
	restarted.Start(runCtx)
	defer func() {
		stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := restarted.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown restarted receiver: %v", err)
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		restarted.mu.Lock()
		cleaned := restarted.receiver.Receipts[key]
		restarted.mu.Unlock()
		_, statusErr := backups.UploadStatus(receipt.UploadID)
		if cleaned.State == ImportCanceled && cleaned.UploadID == "" &&
			errors.Is(statusErr, backup.ErrUploadNotFound) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("canceled import staging cleanup did not resume after restart")
}

func TestImmediatelyRevokedReceiveTokenSurvivesStateReloadAsRevoked(t *testing.T) {
	current := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return current }
	backups, root := newReceiverTestBackupManager(t, now)
	stateRoot := filepath.Join(root, "peer-transfer")
	receiver, err := New(Config{Backups: backups, RootDir: stateRoot, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	token, err := receiver.GenerateReceiveToken(10 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := receiver.RevokeReceiveToken(token.ID); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(Config{Backups: backups, RootDir: stateRoot, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.AuthorizeReceiveToken(token.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("authorize revoked token = %v, want ErrUnauthorized", err)
	}
}
