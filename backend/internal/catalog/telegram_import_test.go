package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func telegramTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	c, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err = c.TelegramPaused(context.Background(), 123); err != nil {
		t.Fatal(err)
	}
	return c
}
func enqueueTelegram(t *testing.T, c *Catalog, n int64, unique, id string) {
	t.Helper()
	source := &TelegramSource{BotID: 123, SenderID: 42, FileID: "file-" + unique, UniqueID: unique, Size: 5}
	err := c.AcceptTelegramUpdate(context.Background(), TelegramReceipt{BotID: 123, UpdateID: n, ChatID: 42, MessageID: n, SenderID: 42}, source, id, "video", 100)
	if err != nil {
		t.Fatal(err)
	}
}

func TestTelegramProgressSchedulingPreservesBackoffAndFinalDelivery(t *testing.T) {
	c := telegramTestCatalog(t)
	ctx := context.Background()
	enqueueTelegram(t, c, 1, "progress", "job")
	receipts, err := c.PendingTelegramReceipts(ctx, 123)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipts=%v err=%v", receipts, err)
	}
	r := receipts[0]
	if err := c.SaveTelegramNotification(ctx, r, 51, "progress:first", 0); err != nil {
		t.Fatal(err)
	}
	if ready, err := c.PendingTelegramReceipts(ctx, 123); err != nil || len(ready) != 0 {
		t.Fatalf("cooldown ignored: %v %v", ready, err)
	}
	expireCooldown := func() {
		t.Helper()
		if _, err := c.db.Exec(`UPDATE telegram_receipts SET next_attempt=1 WHERE update_id=1`); err != nil {
			t.Fatal(err)
		}
	}
	expireCooldown()
	// Fresh receipts take priority over repeatedly checked active tasks.
	enqueueTelegram(t, c, 2, "new", "new-job")
	receipts, err = c.PendingTelegramReceipts(ctx, 123)
	if err != nil || len(receipts) != 2 || receipts[0].UpdateID != 2 || receipts[1].ReplyID != 51 {
		t.Fatalf("active progress or fair scheduling lost: %v %v", receipts, err)
	}
	if err := c.SaveTelegramNotification(ctx, r, 0, "", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := c.FailRemoteUploadJob(ctx, "job", "failed"); err != nil {
		t.Fatal(err)
	}
	receipts, err = c.PendingTelegramReceipts(ctx, 123)
	if err != nil || len(receipts) != 1 || receipts[0].UpdateID != 2 {
		t.Fatalf("terminal notification bypassed rate-limit backoff: %v %v", receipts, err)
	}
	expireCooldown()
	receipts, err = c.PendingTelegramReceipts(ctx, 123)
	if err != nil || len(receipts) != 2 {
		t.Fatalf("final result was not scheduled: %v %v", receipts, err)
	}
	if err := c.SaveTelegramNotification(ctx, r, 51, RemoteUploadFailed, 0); err != nil {
		t.Fatal(err)
	}
	expireCooldown()
	receipts, err = c.PendingTelegramReceipts(ctx, 123)
	if err != nil || len(receipts) != 1 || receipts[0].UpdateID != 2 {
		t.Fatalf("delivered final result repeated: %v %v", receipts, err)
	}
}

func TestTelegramAdmissionIsAtomicWhenTaskCreationFails(t *testing.T) {
	ctx := context.Background()
	c := telegramTestCatalog(t)
	enqueueTelegram(t, c, 1, "a", "job")
	source := &TelegramSource{BotID: 123, SenderID: 42, FileID: "file-b", UniqueID: "b"}
	err := c.AcceptTelegramUpdate(ctx, TelegramReceipt{BotID: 123, UpdateID: 2, ChatID: 42, MessageID: 2, SenderID: 42}, source, "job", "duplicate primary key", 100)
	if err == nil {
		t.Fatal("expected task insertion failure")
	}
	var receipts int
	if err = c.db.QueryRow(`SELECT COUNT(*) FROM telegram_receipts`).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("partial receipt survived: %d %v", receipts, err)
	}
	if _, err = c.LatestTelegramFileID(ctx, *source); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("partial file claim survived: %v", err)
	}
}

func TestRetiringBotAPIOffsetPreservesRestorePause(t *testing.T) {
	c := telegramTestCatalog(t)
	ctx := context.Background()
	if _, err := c.db.Exec(`ALTER TABLE telegram_connections ADD COLUMN next_offset INTEGER NOT NULL DEFAULT 0;
UPDATE telegram_connections SET next_offset=1234,needs_reconnect=1 WHERE bot_id=123`); err != nil {
		t.Fatal(err)
	}
	if err := c.migrateImports(ctx); err != nil {
		t.Fatal(err)
	}
	if paused, err := c.TelegramPaused(ctx, 123); err != nil || !paused {
		t.Fatalf("migration lost the restore pause: %v %v", paused, err)
	}
	var oldColumns int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('telegram_connections') WHERE name='next_offset'`).Scan(&oldColumns); err != nil || oldColumns != 0 {
		t.Fatalf("retired offset remains: %d %v", oldColumns, err)
	}
	if err := c.ResumeTelegram(ctx, 123); err != nil {
		t.Fatal(err)
	}
	if paused, err := c.TelegramPaused(ctx, 123); err != nil || paused {
		t.Fatalf("migrated bot could not resume: %v %v", paused, err)
	}
}
func TestTelegramFileClaimSerializesConcurrentForwards(t *testing.T) {
	c := telegramTestCatalog(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := int64(1); i <= 10; i++ {
		wg.Add(1)
		go func(i int64) {
			defer wg.Done()
			errs <- c.AcceptTelegramUpdate(ctx, TelegramReceipt{BotID: 123, UpdateID: i, ChatID: 42, MessageID: i, SenderID: 42}, &TelegramSource{BotID: 123, SenderID: 42, FileID: "file", UniqueID: "same"}, fmt.Sprint("job-", i), "video", 100)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := c.ListImportJobs(ctx, "telegram", "", 0, 30)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("duplicate imports: %d %v", len(jobs), err)
	}
}
func TestTelegramRetryCannotSupersedeNewerForward(t *testing.T) {
	c := telegramTestCatalog(t)
	ctx := context.Background()
	enqueueTelegram(t, c, 1, "a", "old")
	if _, err := c.CancelRemoteUploadJob(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	enqueueTelegram(t, c, 2, "a", "new")
	if err := c.RetryTelegramImport(ctx, "old", 123); err == nil {
		t.Fatal("old retry superseded new task")
	}
	if _, err := c.CancelRemoteUploadJob(ctx, "new"); err != nil {
		t.Fatal(err)
	}
	if err := c.RetryTelegramImport(ctx, "new", 456); err == nil {
		t.Fatal("different bot retried task")
	}
	if err := c.RetryTelegramImport(ctx, "new", 123); err != nil {
		t.Fatal(err)
	}
	j, err := c.GetRemoteUploadJob(ctx, "new")
	if err != nil || j.State != "queued" || j.CancelRequested {
		t.Fatalf("retry failed: %+v %v", j, err)
	}
}
func TestDelayedTelegramJobDoesNotBlockHTTPAndCancellationWins(t *testing.T) {
	c := telegramTestCatalog(t)
	ctx := context.Background()
	enqueueTelegram(t, c, 1, "a", "tg")
	if _, err := c.ClaimNextImportJob(ctx, false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("disabled TG claimed: %v", err)
	}
	if _, err := c.ClaimNextImportJob(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := c.DelayImport(ctx, "tg", "network", time.Hour, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateRemoteUploadJob(ctx, "http", "https://example.com/v.mp4", "example", "video", nil); err != nil {
		t.Fatal(err)
	}
	j, err := c.ClaimNextImportJob(ctx, true)
	if err != nil || j.ID != "http" {
		t.Fatalf("HTTP was blocked: %+v %v", j, err)
	}
	if _, err = c.CancelRemoteUploadJob(ctx, "tg"); err != nil {
		t.Fatal(err)
	}
	if err = c.DelayImport(ctx, "tg", "network", time.Hour, true); !errors.Is(err, ErrRemoteUploadCanceled) {
		t.Fatalf("canceled job revived: %v", err)
	}
}
func TestTelegramRestoreGateAppliesToNewBotIdentity(t *testing.T) {
	c := telegramTestCatalog(t)
	ctx := context.Background()
	if _, err := c.db.Exec(`INSERT INTO telegram_connections(bot_id,needs_reconnect) VALUES(0,1)`); err != nil {
		t.Fatal(err)
	}
	paused, err := c.TelegramPaused(ctx, 456)
	if err != nil || !paused {
		t.Fatal("restored bot was not paused", err)
	}
	if err = c.ResumeTelegram(ctx, 456); err != nil {
		t.Fatal(err)
	}
	paused, err = c.TelegramPaused(ctx, 456)
	if err != nil || paused {
		t.Fatal("resume failed", err)
	}
}
func TestExpiredTelegramHistoryPreservesDeduplication(t *testing.T) {
	c := telegramTestCatalog(t)
	ctx := context.Background()
	enqueueTelegram(t, c, 1, "a", "old")
	if _, err := c.CancelRemoteUploadJob(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-40 * 24 * time.Hour).UnixMilli()
	if _, err := c.db.Exec(`UPDATE remote_upload_jobs SET finished_at=?`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE telegram_receipts SET created_at=?`, old); err != nil {
		t.Fatal(err)
	}
	if err := c.CompactTelegramHistory(ctx, time.Now().Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	j, err := c.GetRemoteUploadJob(ctx, "old")
	if err != nil || j.SourcePayload != "" {
		t.Fatalf("source not removed: %+v %v", j, err)
	}
	// Redelivery must not resurrect a receipt whose body has been removed.
	enqueueTelegram(t, c, 1, "a", "redelivery")
	jobs, err := c.ListImportJobs(ctx, "telegram", "", 0, 30)
	if err != nil || len(jobs) != 1 {
		t.Fatal("compaction lost idempotency", err)
	}
	if n := c.TelegramNotificationFailures(ctx, 123); n != 0 {
		t.Fatal("expired receipt counted as notification failure")
	}
}
