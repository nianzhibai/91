package telegramupload

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
)

func interruptAfterMigration(t *testing.T, cfg Config) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg.OnMigrated = func(*catalog.Video) { cancel() }
	if err := Run(ctx, cfg); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected interruption after source switch, got %v", err)
	}
	jobs, err := cfg.Catalog.ListTelegramUploadCleanups(context.Background(), "", 100)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("missing durable cleanup: %+v %v", jobs, err)
	}
}

func reopenCleanupCatalog(t *testing.T, cfg *Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "restart.db")
	if err := cfg.Catalog.BackupTo(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Catalog.Close(); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	cfg.Catalog = cat
	return path
}

func TestCleanupResumesCommittedTransferAfterRestart(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "local-upload", true: "telegram-library"}[shared], func(t *testing.T) {
			cfg, v, cloud := fixture(t)
			path := filepath.Join(cfg.LocalDirectory, v.FileID)
			if shared {
				cfg.TelegramDirectory = t.TempDir()
				library := filepath.Join(cfg.TelegramDirectory, "library")
				if err := os.Mkdir(library, 0750); err != nil {
					t.Fatal(err)
				}
				id := "job.media"
				if err := os.Rename(path, filepath.Join(library, id)); err != nil {
					t.Fatal(err)
				}
				if _, err := cfg.Catalog.ReserveTelegramLocalFile(context.Background(), catalog.TelegramLocalFile{FileID: id, JobID: "job"}); err != nil {
					t.Fatal(err)
				}
				v.DriveID, v.FileID = catalog.TelegramLocalDriveID, id
				if err := cfg.Catalog.MigrateVideoToDrive(context.Background(), v.ID, catalog.VideoDriveMigration{DriveID: v.DriveID, FileID: id}); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(library, id)
			}
			interruptAfterMigration(t, cfg)
			saved, err := cfg.Catalog.GetVideo(context.Background(), v.ID)
			if err != nil || saved.DriveID != "cloud" {
				t.Fatalf("source switch not committed: %+v %v", saved, err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("interruption removed local source: %v", err)
			}
			if shared {
				offline := cfg.TelegramDirectory + ".offline"
				if err := os.Rename(cfg.TelegramDirectory, offline); err != nil {
					t.Fatal(err)
				}
				if err := Cleanup(context.Background(), cfg); err == nil {
					t.Fatal("unavailable library was treated as a deleted file")
				}
				jobs, err := cfg.Catalog.ListTelegramUploadCleanups(context.Background(), "", 100)
				if err != nil || len(jobs) != 1 {
					t.Fatalf("unavailable library lost its cleanup task: %+v %v", jobs, err)
				}
				if err := os.Rename(offline, cfg.TelegramDirectory); err != nil {
					t.Fatal(err)
				}
			}
			reopenCleanupCatalog(t, &cfg)
			for range 2 {
				if err := Run(context.Background(), cfg); err != nil {
					t.Fatal(err)
				}
			}
			if cloud.uploads != 1 {
				t.Fatalf("cleanup uploaded again: %d", cloud.uploads)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("cleanup retained source: %v", err)
			}
			jobs, err := cfg.Catalog.ListTelegramUploadCleanups(context.Background(), "", 100)
			if err != nil || len(jobs) != 0 {
				t.Fatalf("completed cleanup retained: %+v %v", jobs, err)
			}
		})
	}
}

func TestCleanupRetriesDatabaseFailureAfterFileDeletion(t *testing.T) {
	cfg, v, cloud := fixture(t)
	interruptAfterMigration(t, cfg)
	path := reopenCleanupCatalog(t, &cfg)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER reject_cleanup BEFORE DELETE ON telegram_upload_cleanups BEGIN SELECT RAISE(FAIL,'injected cleanup failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := Cleanup(context.Background(), cfg); err == nil {
		t.Fatal("cleanup database failure was hidden")
	}
	if _, err := os.Stat(filepath.Join(cfg.LocalDirectory, v.FileID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source was not deleted before failure: %v", err)
	}
	jobs, err := cfg.Catalog.ListTelegramUploadCleanups(context.Background(), "", 100)
	if err != nil || len(jobs) != 1 || jobs[0].Attempts != 1 || !strings.Contains(jobs[0].LastError, "injected cleanup failure") {
		t.Fatalf("failure not retained: %+v %v", jobs, err)
	}
	if _, err := db.Exec(`DROP TRIGGER reject_cleanup`); err != nil {
		t.Fatal(err)
	}
	if err := Cleanup(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	jobs, err = cfg.Catalog.ListTelegramUploadCleanups(context.Background(), "", 100)
	if err != nil || len(jobs) != 0 || cloud.uploads != 1 {
		t.Fatalf("cleanup retry did not finish: %+v uploads=%d %v", jobs, cloud.uploads, err)
	}
}

func TestCleanupPreservesReplacedAndReferencedSources(t *testing.T) {
	for _, mode := range []string{"replaced", "referenced", "changed", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			cfg, v, cloud := fixture(t)
			interruptAfterMigration(t, cfg)
			path := filepath.Join(cfg.LocalDirectory, v.FileID)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "replaced", "symlink":
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if mode == "symlink" {
					if err := os.Symlink(path+".original", path); err != nil {
						t.Skip(err)
					}
				} else {
					if err := os.WriteFile(path, []byte("other"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
						t.Fatal(err)
					}
				}
			case "referenced":
				if err := cfg.Catalog.UpsertVideo(context.Background(), &catalog.Video{ID: "new-owner", DriveID: v.DriveID, FileID: v.FileID, Title: "new", Size: v.Size}); err != nil {
					t.Fatal(err)
				}
			case "changed":
				if err := os.Chtimes(path, info.ModTime(), info.ModTime().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			if err := Cleanup(context.Background(), cfg); err == nil {
				t.Fatal("changed source was accepted")
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("changed source removed: %v", err)
			}
			jobs, err := cfg.Catalog.ListTelegramUploadCleanups(context.Background(), "", 100)
			if err != nil || len(jobs) != 1 || jobs[0].Attempts != 1 || jobs[0].LastError == "" || cloud.uploads != 1 {
				t.Fatalf("unsafe cleanup result: %+v uploads=%d %v", jobs, cloud.uploads, err)
			}
		})
	}
}

func TestTransferPreservesErrorClassificationAcrossStages(t *testing.T) {
	for _, stage := range []string{"directory", "listing", "upload", "verify", "playback"} {
		for _, kind := range []string{"rate-limit", "auth"} {
			t.Run(stage+"/"+kind, func(t *testing.T) {
				cfg, v, cloud := fixture(t)
				cause := errors.New("provider rejected request")
				var providerErr error = &drives.ProviderError{Kind: drives.ProviderErrorAuth, Err: cause}
				if kind == "rate-limit" {
					providerErr = &drives.RateLimitError{Provider: "test", RetryAfter: 17 * time.Second, Err: cause}
				}
				switch stage {
				case "directory":
					cloud.directoryErr = providerErr
				case "listing":
					cloud.listErr = providerErr
				case "upload":
					cloud.uploadErr = providerErr
				case "verify":
					cloud.statErr = providerErr
				case "playback":
					cloud.streamErr = providerErr
				}
				// This second import must not be uploaded after a per-file rate limit.
				ctx := context.Background()
				if err := cfg.Catalog.AcceptTelegramUpdate(ctx, catalog.TelegramReceipt{BotID: 123, UpdateID: 2, MessageID: 2, ChatID: 42, SenderID: 42}, &catalog.TelegramSource{BotID: 123, SenderID: 42, FileID: "other", UniqueID: "other", Size: 5}, "other-job", "other", 100); err != nil {
					t.Fatal(err)
				}
				if err := cfg.Catalog.TransitionRemoteUploadJob(ctx, "other-job", catalog.RemoteUploadQueued, catalog.RemoteUploadSaving); err != nil {
					t.Fatal(err)
				}
				other := &catalog.Video{ID: "zz-video", DriveID: "local-upload", FileID: "other.mp4", FileName: "other.mp4", Title: "other", Size: 5, Ext: "mp4"}
				if err := cfg.Catalog.FinalizeRemoteUpload(ctx, "other-job", other, nil, nil); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cfg.LocalDirectory, other.FileID), []byte("other"), 0600); err != nil {
					t.Fatal(err)
				}
				err := Run(ctx, cfg)
				if !errors.Is(err, cause) {
					t.Fatalf("cause discarded: %v", err)
				}
				if kind == "rate-limit" {
					if delay, limited := drives.RateLimitRetryAfter(err); !limited || delay != 17*time.Second {
						t.Fatalf("rate limit discarded: delay=%s limited=%t err=%v", delay, limited, err)
					}
					if stage != "directory" && stage != "listing" && cloud.uploads != 1 {
						t.Fatalf("sweep continued after rate limit: uploads=%d", cloud.uploads)
					}
				} else {
					var auth *drives.ProviderError
					if !errors.As(err, &auth) || auth.Kind != drives.ProviderErrorAuth {
						t.Fatalf("authentication classification discarded: %v", err)
					}
				}
				saved, err := cfg.Catalog.GetVideo(ctx, v.ID)
				if err != nil || saved.DriveID != v.DriveID {
					t.Fatalf("failed transfer changed source: %+v %v", saved, err)
				}
			})
		}
	}
}

func TestTransferDiagnosticLogsKeepCauseAndRedactSecrets(t *testing.T) {
	cfg, v, cloud := fixture(t)
	cause := errors.New("HTTP 503 access_token=PRIVATE_TOKEN https://user:PRIVATE_PASSWORD@example.com/upload?token=PRIVATE_QUERY")
	cloud.uploadErr = cause
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	err := Run(context.Background(), cfg)
	if !errors.Is(err, cause) || strings.Contains(err.Error(), "PRIVATE_") {
		t.Fatalf("public message or cause changed: %v", err)
	}
	if strings.Contains(output.String(), "PRIVATE_") {
		t.Fatalf("diagnostic log exposed secrets: %s", output.String())
	}
	found := false
	for _, line := range strings.Split(output.String(), "\n") {
		_, encoded, ok := strings.Cut(line, "@applog ")
		if !ok {
			continue
		}
		var entry applog.Entry
		if err := json.Unmarshal([]byte(encoded), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.VideoID == v.ID && entry.Stage == "upload" && strings.Contains(entry.Error, "HTTP 503") && strings.Contains(entry.Error, "REDACTED") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing classified diagnostic: %s", output.String())
	}
}

func TestTransferDiagnosticLogsPreserveCancellationLevel(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	logFailure(context.Background(), "video", "cloud", failure("upload", "TG 转存已取消", context.Canceled))
	_, encoded, ok := strings.Cut(strings.TrimSpace(output.String()), "@applog ")
	if !ok {
		t.Fatalf("missing diagnostic: %s", output.String())
	}
	var entry applog.Entry
	if err := json.Unmarshal([]byte(encoded), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Level != applog.LevelWarning || entry.Stage != "upload" || entry.Error != context.Canceled.Error() {
		t.Fatalf("cancellation classification discarded: %+v", entry)
	}
}

func TestTransferDiagnosticLogsPreserveJoinedFailures(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	joined := errors.Join(
		failure("cleanup", "清理失败", errors.New("filesystem read-only")),
		failure("directory", "读取目录失败", errors.New("HTTP 503 access_token=PRIVATE_TOKEN")),
	)
	logFailure(context.Background(), "", "cloud", joined)
	if !strings.Contains(output.String(), "filesystem read-only") || !strings.Contains(output.String(), "HTTP 503") || strings.Contains(output.String(), "PRIVATE_TOKEN") {
		t.Fatalf("joined diagnostic lost a cause or exposed credentials: %s", output.String())
	}
}
