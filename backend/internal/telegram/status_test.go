package telegram

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/video-site/backend/internal/catalog"
)

func TestStatusCommandUsesGlobalLiveCountsWithoutCreatingImport(t *testing.T) {
	s, cat := testService(t)
	ctx := context.Background()
	u := videoUpdate(1, 42)
	u.Message.Video, u.Message.Text = nil, "/status@test_bot"
	if err := s.accept(ctx, u); err != nil {
		t.Fatal(err)
	}
	// Replayed command updates remain a single reply and never become imports.
	if err := s.accept(ctx, u); err != nil {
		t.Fatal(err)
	}
	if jobs, err := cat.ListImportJobs(ctx, "telegram", "", 0, 30); err != nil || len(jobs) != 0 {
		t.Fatalf("status created an import: jobs=%v error=%v", jobs, err)
	}
	receipts, err := cat.PendingTelegramReceipts(ctx, s.BotID())
	if err != nil || len(receipts) != 1 || receipts[0].Response != responseStatus {
		t.Fatalf("status receipt = %+v, error = %v", receipts, err)
	}
	// A task from another sender admitted after the command is counted at reply time.
	if err := cat.AcceptTelegramUpdate(ctx, catalog.TelegramReceipt{
		BotID: 123, UpdateID: 2, ChatID: 99, MessageID: 2, SenderID: 99,
	}, &catalog.TelegramSource{BotID: 123, SenderID: 99, FileID: "file", UniqueID: "unique"}, "other-sender", "video", 100); err != nil {
		t.Fatal(err)
	}
	if err := cat.AcceptTelegramUpdate(ctx, catalog.TelegramReceipt{
		BotID: 123, UpdateID: 3, ChatID: 99, MessageID: 3, SenderID: 99,
	}, &catalog.TelegramSource{BotID: 123, SenderID: 99, FileID: "saved", UniqueID: "saved"}, "completed", "saved", 100); err != nil {
		t.Fatal(err)
	}
	if err := cat.TransitionRemoteUploadJob(ctx, "completed", catalog.RemoteUploadQueued, catalog.RemoteUploadSaving); err != nil {
		t.Fatal(err)
	}
	if err := cat.FinalizeRemoteUpload(ctx, "completed", &catalog.Video{
		ID: "saved", DriveID: "local-upload", FileID: "saved.mp4", FileName: "saved.mp4", Title: "saved", Size: 5,
	}, nil, nil); err != nil {
		t.Fatal(err)
	}
	sent := make(chan string, 2)
	c := &fakeTransport{send: func(ctx context.Context, r catalog.TelegramReceipt, m botMessage) (int64, error) {
		sent <- m.text
		return 55, nil
	}}

	s.notifyOne(ctx, c, receipts[0])
	text := <-sent
	assertValidMessage(t, text)
	for _, expected := range []string{"转存统计", "进行中：1 个", "今日成功：1 个", "累计成功：1 个"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("status missing %q: %s", expected, text)
		}
	}
	pending, err := cat.PendingTelegramReceipts(ctx, s.BotID())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range pending {
		if r.UpdateID == u.ID {
			t.Fatal("delivered status remained pending")
		}
	}
	// Access is checked again if a reply was pending when the allowlist changed.
	s.cfg.AllowedUserIDs = nil
	s.notifyOne(ctx, c, receipts[0])
	if text := <-sent; !strings.Contains(text, "需要开通权限") || strings.Contains(text, "累计成功") {
		t.Fatalf("revoked user received statistics: %s", text)
	}
}

func TestStatusMessageUsesLiveTimezone(t *testing.T) {
	s, cat := testService(t)
	ctx := context.Background()
	if err := s.accept(ctx, videoUpdate(1, 42)); err != nil {
		t.Fatal(err)
	}
	jobs, err := cat.ListImportJobs(ctx, "telegram", "", 0, 30)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%v error=%v", jobs, err)
	}
	id := jobs[0].ID
	if err := cat.TransitionRemoteUploadJob(ctx, id, catalog.RemoteUploadQueued, catalog.RemoteUploadSaving); err != nil {
		t.Fatal(err)
	}
	if err := cat.FinalizeRemoteUpload(ctx, id, &catalog.Video{
		ID: "saved", DriveID: "local-upload", FileID: "saved.mp4", FileName: "saved.mp4", Title: "saved", Size: 5,
	}, nil, nil); err != nil {
		t.Fatal(err)
	}
	job, err := cat.GetRemoteUploadJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	// Query at the next midnight in one zone while the other zone is still
	// on the completion day. This verifies the calendar even without its label.
	finished := job.FinishedAt.UTC()
	now := time.Date(finished.Year(), finished.Month(), finished.Day(), 16, 0, 0, 0, time.UTC)
	shanghaiCount, utcCount := 0, 1
	if !finished.Before(now) {
		now = time.Date(finished.Year(), finished.Month(), finished.Day()+1, 0, 0, 0, 0, time.UTC)
		shanghaiCount, utcCount = 1, 0
	}
	timezone := "Asia/Shanghai"
	s.statusTimezone = func() string { return timezone }
	for _, tc := range []struct {
		zone  string
		today int
	}{{"Asia/Shanghai", shanghaiCount}, {"Etc/UTC", utcCount}} {
		timezone = tc.zone
		message, err := s.statusMessage(ctx, now)
		if err != nil || !strings.Contains(message.text, fmt.Sprintf("今日成功：%d 个", tc.today)) || !strings.Contains(message.text, "累计成功：1 个") {
			t.Fatalf("status did not use live timezone %q: %+v, %v", tc.zone, message, err)
		}
	}
	timezone = "invalid/timezone"
	if _, err := s.statusMessage(ctx, now); err == nil {
		t.Fatal("invalid timezone silently changed today's calendar")
	}
}

func TestStatusNotificationReportsQueryFailure(t *testing.T) {
	s, cat := testService(t)
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	sent := make(chan string, 1)
	c := &fakeTransport{send: func(ctx context.Context, r catalog.TelegramReceipt, m botMessage) (int64, error) {
		sent <- m.text
		return 55, nil
	}}

	s.notifyOne(context.Background(), c, catalog.TelegramReceipt{
		BotID: 123, ChatID: 42, MessageID: 1, SenderID: 42, Response: responseStatus,
	})
	text := <-sent
	if !strings.Contains(text, "暂时无法读取转存统计") || strings.Contains(text, "成功：0") || strings.Contains(text, "database") {
		t.Fatalf("query failure produced misleading statistics or exposed internal errors: %s", text)
	}
}
