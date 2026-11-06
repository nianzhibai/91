package telegram

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/video-site/backend/internal/catalog"
)

func TestNotificationsEditAndSkipUnchangedContent(t *testing.T) {
	s, cat := testService(t)
	ctx := context.Background()
	if err := s.accept(ctx, videoUpdate(1, 42)); err != nil {
		t.Fatal(err)
	}
	receipts, _ := cat.PendingTelegramReceipts(ctx, 123)
	r := receipts[0]
	calls := 0
	c := &fakeTransport{send: func(ctx context.Context, r catalog.TelegramReceipt, m botMessage) (int64, error) {
		calls++
		if calls == 1 && r.ReplyID != 0 {
			t.Error("initial message edited")
		}
		if calls == 2 && r.ReplyID != 55 {
			t.Error("progress did not edit initial reply")
		}
		if calls == 2 && !strings.Contains(m.text, "速度：1.0 MiB/s") {
			t.Error("progress did not include the measured download speed")
		}
		assertValidMessage(t, m.text)
		return 55, nil
	}}
	s.notifyOne(ctx, c, r)
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		receipts, _ = cat.PendingTelegramReceipts(ctx, 123)
		if len(receipts) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(receipts) == 0 {
		t.Fatal("active receipt lost")
	}
	r = receipts[0]
	s.notifyOne(ctx, c, r)
	if calls != 1 {
		t.Fatal("unchanged notification resent")
	}
	if err := cat.TransitionRemoteUploadJob(ctx, r.JobID, catalog.RemoteUploadQueued, catalog.RemoteUploadDownloading); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateRemoteUploadProgress(ctx, r.JobID, 1<<20, 1<<30); err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-2 * time.Second)
	s.downloadProgress.start(r.JobID, started)
	s.downloadProgress.update(r.JobID, 1<<20, started.Add(time.Second))
	s.notifyOne(ctx, c, r)
	if calls != 2 {
		t.Fatal("progress was not edited")
	}
}
func TestDeletedNotificationResendsWithoutChangingJob(t *testing.T) {
	s, cat := testService(t)
	ctx := context.Background()
	_ = s.accept(ctx, videoUpdate(1, 42))
	receipts, _ := cat.PendingTelegramReceipts(ctx, 123)
	r := receipts[0]
	r.ReplyID = 88
	calls := 0
	c := &fakeTransport{send: func(ctx context.Context, r catalog.TelegramReceipt, m botMessage) (int64, error) {
		calls++
		if calls == 1 {
			return 0, &APIError{Code: 400}
		}
		if r.ReplyID != 0 || r.MessageID != 1 {
			t.Error("resend lost reply context")
		}
		return 55, nil
	}}
	s.notifyOne(ctx, c, r)
	job, _ := cat.GetRemoteUploadJob(ctx, r.JobID)
	if calls != 2 || job.State != catalog.RemoteUploadQueued {
		t.Fatalf("calls=%d job=%+v", calls, job)
	}
}
func TestNotificationFailureDoesNotChangeImport(t *testing.T) {
	s, cat := testService(t)
	ctx := context.Background()
	_ = s.accept(ctx, videoUpdate(1, 42))
	receipts, _ := cat.PendingTelegramReceipts(ctx, 123)
	s.notifyOne(ctx, &fakeTransport{send: func(context.Context, catalog.TelegramReceipt, botMessage) (int64, error) {
		return 0, &APIError{Code: 429, RetryAfter: time.Minute}
	}}, receipts[0])
	jobs, _ := cat.ListImportJobs(ctx, "telegram", "", 0, 30)
	if len(jobs) != 1 || jobs[0].State != catalog.RemoteUploadQueued {
		t.Fatal("notification changed import")
	}
}
func TestImportProgressReflectsMeasuredStage(t *testing.T) {
	for _, tc := range []struct{ state, stage, want string }{
		{"downloading", "telegram_download", "正在从 TG 获取"}, {"validating", "", "正在校验"}, {"saving", "", "正在入库"}, {"queued", "retry_wait", "等待重试"},
	} {
		got := importProgressText(&catalog.RemoteUploadJob{State: tc.state, Stage: tc.stage})
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: %s", tc.state, got)
		}
	}
}

func TestForwardedMediaGroupEditsOneReplyForProgressFailureAndRetry(t *testing.T) {
	s, cat := testService(t)
	ctx := context.Background()
	for _, u := range []update{albumPhoto(1, "共同说明"), albumVideo(2, ""), albumVideo(3, "")} {
		if err := s.accept(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	flushTestAlbum(t, s)
	receipts, err := cat.PendingTelegramReceipts(ctx, 123)
	if err != nil || len(receipts) != 1 || len(receipts[0].JobIDs) != 2 {
		t.Fatalf("missing shared album receipt: %+v %v", receipts, err)
	}
	r := receipts[0]
	calls := 0
	transport := &fakeTransport{send: func(ctx context.Context, receipt catalog.TelegramReceipt, message botMessage) (int64, error) {
		calls++
		if receipt.MessageID != 2 || (calls == 1 && receipt.ReplyID != 0) || (calls > 1 && receipt.ReplyID != 55) {
			t.Fatalf("album notification used another reply: %+v", receipt)
		}
		assertValidMessage(t, message.text)
		if !strings.Contains(message.text, "已保存：0 / 2") {
			t.Fatalf("notification lost the group count: %s", message.text)
		}
		if calls == 2 && (!strings.Contains(message.text, "正在从 TG 获取视频") || !strings.Contains(message.text, "40%")) {
			t.Fatalf("shared reply lost video progress: %s", message.text)
		}
		if calls == 3 && (!strings.Contains(message.text, "失败：2") || !strings.Contains(message.text, "媒体组处理完成")) {
			t.Fatalf("shared reply lost final failures: %s", message.text)
		}
		if calls == 4 && (!strings.Contains(message.text, "失败：1") || !strings.Contains(message.text, "正在保存媒体组")) {
			t.Fatalf("retry did not resume the group reply: %s", message.text)
		}
		return 55, nil
	}}
	s.notifyOne(ctx, transport, r)
	r.ReplyID = 55
	if err := cat.TransitionRemoteUploadJob(ctx, r.JobIDs[0], catalog.RemoteUploadQueued, catalog.RemoteUploadDownloading); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateRemoteUploadProgress(ctx, r.JobIDs[0], 2, 5); err != nil {
		t.Fatal(err)
	}
	s.notifyOne(ctx, transport, r)
	for _, id := range r.JobIDs {
		if err := cat.FailRemoteUploadJob(ctx, id, "下载失败"); err != nil {
			t.Fatal(err)
		}
	}
	s.notifyOne(ctx, transport, r)
	if err := cat.RetryTelegramImport(ctx, r.JobIDs[1], 123); err != nil {
		t.Fatal(err)
	}
	receipts, err = cat.PendingTelegramReceipts(ctx, 123)
	if err != nil || len(receipts) != 1 || receipts[0].ReplyID != 55 {
		t.Fatalf("retry lost the persisted group reply: %+v %v", receipts, err)
	}
	s.notifyOne(ctx, transport, receipts[0])
	if calls != 4 {
		t.Fatalf("unexpected album notifications: %d", calls)
	}
}
