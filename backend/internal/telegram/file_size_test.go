package telegram

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/mediaimport"
)

func TestAcceptRequiresKnownVideoSizeWithinLimit(t *testing.T) {
	for _, kind := range []string{"video", "document"} {
		for _, tc := range []struct {
			name, sizeField, rejection string
		}{
			{"missing", "", "无法确认视频大小"},
			{"zero", `,"file_size":0`, "无法确认视频大小"},
			{"negative", `,"file_size":-1`, "无法确认视频大小"},
			{"over limit", `,"file_size":6`, "超过配置的单文件大小限制"},
			{"at limit", `,"file_size":5`, ""},
			{"below limit", `,"file_size":4`, ""},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				s, cat := testService(t)
				s.cfg.MaxFileSizeBytes = 5
				ctx := context.Background()
				var u update
				body := fmt.Sprintf(`{"update_id":1,"message":{"message_id":1,"from":{"id":42},"chat":{"id":42,"type":"private"},"%s":{"file_id":"file","file_unique_id":"unique","file_name":"video.mp4","mime_type":"video/mp4"%s}}}`, kind, tc.sizeField)
				if err := json.Unmarshal([]byte(body), &u); err != nil {
					t.Fatal(err)
				}
				if err := s.accept(ctx, u); err != nil {
					t.Fatal(err)
				}
				jobs, err := cat.ListImportJobs(ctx, "telegram", "", 0, 30)
				if err != nil {
					t.Fatal(err)
				}
				receipts, err := cat.PendingTelegramReceipts(ctx, s.BotID())
				if err != nil || len(receipts) != 1 {
					t.Fatalf("receipts=%+v error=%v", receipts, err)
				}
				if tc.rejection != "" {
					if len(jobs) != 0 || receipts[0].JobID != "" || !strings.Contains(receipts[0].Response, tc.rejection) {
						t.Fatalf("invalid size queued for download: jobs=%+v receipt=%+v", jobs, receipts[0])
					}
				} else if len(jobs) != 1 || receipts[0].JobID != jobs[0].ID {
					t.Fatalf("valid video was rejected: jobs=%+v receipt=%+v", jobs, receipts[0])
				}
			})
		}
	}
}

func TestFetchRejectsInvalidQueuedSizeBeforeRequestingFile(t *testing.T) {
	for _, size := range []int64{0, -1, 6} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s, cat := testService(t)
			s.cfg.MaxFileSizeBytes = 5
			ctx := context.Background()
			// Bypass reception to model jobs persisted by an older version, or
			// accepted before the configured limit was reduced.
			source := catalog.TelegramSource{BotID: s.BotID(), SenderID: 42, FileID: "file", UniqueID: "unique", FileName: "video.mp4", MIME: "video/mp4", Size: size}
			receipt := catalog.TelegramReceipt{BotID: s.BotID(), UpdateID: 1, ChatID: 42, MessageID: 1, SenderID: 42}
			if err := cat.AcceptTelegramUpdate(ctx, receipt, &source, "queued", "video", 100); err != nil {
				t.Fatal(err)
			}
			job, err := cat.GetRemoteUploadJob(ctx, "queued")
			if err != nil {
				t.Fatal(err)
			}
			var requests atomic.Int64
			s.client = &fakeTransport{download: func(context.Context, string, io.WriterAt) error { requests.Add(1); return nil }}
			progressCalled := false
			_, err = s.Fetch(ctx, job, func(string, int64, int64) error {
				progressCalled = true
				return nil
			})
			var sourceErr *mediaimport.SourceError
			if !errors.As(err, &sourceErr) || sourceErr.WaitForAvailability || sourceErr.RetryAfter != 0 {
				t.Fatalf("invalid size must fail without retries: %v", err)
			}
			if requests.Load() != 0 || progressCalled {
				t.Fatalf("invalid size started acquisition: requests=%d progress=%v", requests.Load(), progressCalled)
			}
			if _, err := cat.TelegramLocalFile(ctx, job.ID+".media"); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("invalid size reserved library storage: %v", err)
			}
			if entries, err := os.ReadDir(s.cfg.LocalFilesRoot); err != nil || len(entries) != 0 {
				t.Fatalf("invalid size changed shared storage: entries=%v error=%v", entries, err)
			}
		})
	}
}
