package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func linkedAlbumImports() []TelegramImport {
	var inputs []TelegramImport
	for i := 1; i <= 2; i++ {
		inputs = append(inputs, TelegramImport{
			Source: &TelegramSource{BotID: 123, SenderID: 42, FileID: fmt.Sprint("file-", i), UniqueID: fmt.Sprint("unique-", i), Size: 5},
			ID:     fmt.Sprint("linked-", i), Title: fmt.Sprint("媒体组 - ", i),
		})
	}
	return inputs
}

func TestLinkedAlbumAdmissionIsAtomicAndDeduplicates(t *testing.T) {
	for _, mode := range []string{"accepted", "queue full", "duplicate ID"} {
		t.Run(mode, func(t *testing.T) {
			c := telegramTestCatalog(t)
			ctx := context.Background()
			inputs := linkedAlbumImports()
			r := TelegramReceipt{BotID: 123, ChatID: 42, SenderID: 42, MessageID: 7, UpdateID: -7}
			limit := 100
			if mode == "queue full" {
				limit = 1
			}
			if mode == "duplicate ID" {
				inputs[1].ID = inputs[0].ID
			}
			err := c.AcceptTelegramMediaLink(ctx, r, inputs, limit)
			if mode == "duplicate ID" {
				if err == nil {
					t.Fatal("expected insertion failure")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if mode == "accepted" {
				if err := c.AcceptTelegramMediaLink(ctx, r, inputs, limit); err != nil {
					t.Fatal(err)
				}
				// Resubmitting another group member with new job IDs reuses both tasks.
				r.MessageID, r.UpdateID = 8, -8
				inputs[0].ID, inputs[1].ID = "retry-1", "retry-2"
				if err := c.AcceptTelegramMediaLink(ctx, r, inputs, limit); err != nil {
					t.Fatal(err)
				}
			}
			wantJobs, wantReceipts := 0, 0
			if mode == "accepted" {
				wantJobs, wantReceipts = 2, 2
			} else if mode == "queue full" {
				wantReceipts = 1
			}
			for table, want := range map[string]int{"remote_upload_jobs": wantJobs, "telegram_files": wantJobs, "telegram_receipts": wantReceipts} {
				var count int
				if err := c.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != want {
					t.Fatalf("%s: got %d, want %d, error %v", table, count, want, err)
				}
			}
			receipts, err := c.PendingTelegramReceipts(ctx, 123)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "queue full" && receipts[0].Response != TelegramResponseQueueFull {
				t.Fatal("queue rejection was not persisted")
			}
			if mode == "accepted" {
				for _, receipt := range receipts {
					if len(receipt.JobIDs) != 2 || receipt.JobIDs[0] != "linked-1" || receipt.JobIDs[1] != "linked-2" {
						t.Fatalf("lost linked jobs: %+v", receipt)
					}
				}
			}
		})
	}
}

func TestLinkedAlbumReceiptTracksAllJobsAndRetry(t *testing.T) {
	c := telegramTestCatalog(t)
	ctx := context.Background()
	r := TelegramReceipt{BotID: 123, ChatID: 42, SenderID: 42, MessageID: 7, UpdateID: -7}
	if err := c.AcceptTelegramMediaLink(ctx, r, linkedAlbumImports(), 100); err != nil {
		t.Fatal(err)
	}
	receipts, err := c.PendingTelegramReceipts(ctx, 123)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("missing group receipt: %+v %v", receipts, err)
	}
	r = receipts[0]
	if err := c.FailRemoteUploadJob(ctx, "linked-1", "first failed"); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveTelegramNotification(ctx, r, 55, "progress:first failed", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE telegram_receipts SET next_attempt=0`); err != nil {
		t.Fatal(err)
	}
	receipts, err = c.PendingTelegramReceipts(ctx, 123)
	if err != nil || len(receipts) != 1 || receipts[0].ReplyID != 55 {
		t.Fatalf("remaining job lost its notification: %+v %v", receipts, err)
	}
	if err := c.FailRemoteUploadJob(ctx, "linked-2", "second failed"); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveTelegramNotification(ctx, r, 55, "group:terminal", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE telegram_receipts SET next_attempt=0`); err != nil {
		t.Fatal(err)
	}
	if pending, err := c.PendingTelegramReceipts(ctx, 123); err != nil || len(pending) != 0 {
		t.Fatalf("terminal group kept notifying: %+v %v", pending, err)
	}
	if err := c.RetryTelegramImport(ctx, "linked-2", 123); err != nil {
		t.Fatal(err)
	}
	receipts, err = c.PendingTelegramReceipts(ctx, 123)
	if err != nil || len(receipts) != 1 || receipts[0].ReplyID != 55 || receipts[0].Delivered != "" {
		t.Fatalf("retry did not resume the group reply: %+v %v", receipts, err)
	}
	// History compaction must retain a group receipt while any member is active.
	if _, err := c.db.Exec(`UPDATE telegram_receipts SET created_at=1`); err != nil {
		t.Fatal(err)
	}
	if err := c.CompactTelegramHistory(ctx, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if pending, err := c.PendingTelegramReceipts(ctx, 123); err != nil || len(pending) != 1 {
		t.Fatalf("compaction expired an active group: %+v %v", pending, err)
	}
}

func TestTelegramReceiptsMigrateExistingMessages(t *testing.T) {
	c := telegramTestCatalog(t)
	ctx := context.Background()
	enqueueTelegram(t, c, 1, "old", "old-job")
	if _, err := c.db.Exec(`ALTER TABLE telegram_receipts DROP COLUMN job_ids`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`ALTER TABLE telegram_receipts DROP COLUMN duplicate_job_ids`); err != nil {
		t.Fatal(err)
	}
	if err := MigrateImportDatabase(ctx, c.db); err != nil {
		t.Fatal(err)
	}
	receipts, err := c.PendingTelegramReceipts(ctx, 123)
	if err != nil || len(receipts) != 1 || receipts[0].JobID != "old-job" || len(receipts[0].JobIDs) != 0 || len(receipts[0].DuplicateJobIDs) != 0 {
		t.Fatalf("existing receipt lost on migration: %+v %v", receipts, err)
	}
	var raw string
	if err := c.db.QueryRow(`SELECT job_ids FROM telegram_receipts`).Scan(&raw); err != nil || !json.Valid([]byte(raw)) {
		t.Fatalf("invalid default linked-job list: %q %v", raw, err)
	}
}
