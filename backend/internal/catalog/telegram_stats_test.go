package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/video-site/backend/internal/schedule"
)

func TestCountTelegramImportsAcrossSendersAndHistoryCompaction(t *testing.T) {
	c := telegramTestCatalog(t)
	ctx := context.Background()
	_, location, err := schedule.LoadTimezone("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, location)
	assertStats := func(want TelegramImportStats) {
		t.Helper()
		got, err := c.CountTelegramImports(ctx, now)
		if err != nil || got != want {
			t.Fatalf("statistics = %+v, error = %v; want %+v", got, err, want)
		}
	}
	assertStats(TelegramImportStats{})
	if _, err := c.TelegramPaused(ctx, 456); err != nil {
		t.Fatal(err)
	}
	for n, fixture := range []struct {
		id, state string
		finished  time.Time
	}{
		{"queued", RemoteUploadQueued, time.Time{}},
		{"downloading", RemoteUploadDownloading, time.Time{}},
		{"validating", RemoteUploadValidating, time.Time{}},
		{"saving", RemoteUploadSaving, time.Time{}},
		{"today", RemoteUploadCompleted, now.Add(-time.Hour)},
		{"yesterday", RemoteUploadCompleted, now.AddDate(0, 0, -1)},
		{"old", RemoteUploadCompleted, now.AddDate(0, 0, -60)},
		{"failed", RemoteUploadFailed, now},
		{"canceled", RemoteUploadCanceled, now},
	} {
		botID, senderID := int64(123), int64(42)
		if n%2 == 0 {
			botID, senderID = 456, 99
		}
		receipt := TelegramReceipt{BotID: botID, UpdateID: int64(n + 1), ChatID: senderID, MessageID: int64(n + 1), SenderID: senderID}
		source := &TelegramSource{BotID: botID, SenderID: senderID, FileID: fixture.id, UniqueID: fixture.id, Size: 5}
		if err := c.AcceptTelegramUpdate(ctx, receipt, source, fixture.id, fixture.id, 100); err != nil {
			t.Fatal(err)
		}
		// A second sender forwarding the same file reuses the original task.
		receipt.UpdateID += 100
		receipt.MessageID += 100
		receipt.ChatID, receipt.SenderID, source.SenderID = 100, 100, 100
		if err := c.AcceptTelegramUpdate(ctx, receipt, source, fixture.id+"-duplicate", fixture.id, 100); err != nil {
			t.Fatal(err)
		}
		finished := int64(0)
		if !fixture.finished.IsZero() {
			finished = fixture.finished.UnixMilli()
		}
		if _, err := c.db.Exec(`UPDATE remote_upload_jobs SET state=?,finished_at=?,created_at=? WHERE id=?`,
			fixture.state, finished, now.AddDate(0, 0, -90).UnixMilli(), fixture.id); err != nil {
			t.Fatal(err)
		}
	}
	// Other import sources do not contribute to either ongoing or success counts.
	for _, state := range []string{RemoteUploadQueued, RemoteUploadCompleted} {
		if _, err := c.db.Exec(`INSERT INTO remote_upload_jobs(id,source_kind,state,finished_at,created_at,updated_at)
VALUES(?,'http',?,?,?,?)`, "http-"+state, state, now.UnixMilli(), now.UnixMilli(), now.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	want := TelegramImportStats{Active: 4, TodayCompleted: 1, TotalCompleted: 3}
	assertStats(want)
	if err := c.CompactTelegramHistory(ctx, now.AddDate(0, 0, -30)); err != nil {
		t.Fatal(err)
	}
	assertStats(want)
}

func TestCountTelegramImportsUsesCalendarDayBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, timezone, startUTC, endUTC string
	}{
		{"Shanghai", "Asia/Shanghai", "2026-09-22T16:00:00Z", "2026-09-23T16:00:00Z"},
		{"short day", "Europe/Berlin", "2026-03-28T23:00:00Z", "2026-03-29T22:00:00Z"},
		{"long day", "Europe/Berlin", "2026-10-24T22:00:00Z", "2026-10-25T23:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := telegramTestCatalog(t)
			_, location, err := schedule.LoadTimezone(tc.timezone)
			if err != nil {
				t.Fatal(err)
			}
			start, err := time.Parse(time.RFC3339, tc.startUTC)
			if err != nil {
				t.Fatal(err)
			}
			end, err := time.Parse(time.RFC3339, tc.endUTC)
			if err != nil {
				t.Fatal(err)
			}
			for n, finished := range []time.Time{start.Add(-time.Millisecond), start, end.Add(-time.Millisecond), end} {
				if _, err := c.db.Exec(`INSERT INTO remote_upload_jobs(id,source_kind,state,finished_at,created_at,updated_at)
VALUES(?,'telegram','completed',?,?,?)`, n, finished.UnixMilli(), start.Add(-time.Hour).UnixMilli(), finished.UnixMilli()); err != nil {
					t.Fatal(err)
				}
			}
			got, err := c.CountTelegramImports(context.Background(), start.Add(12*time.Hour).In(location))
			want := TelegramImportStats{TodayCompleted: 2, TotalCompleted: 4}
			if err != nil || got != want {
				t.Fatalf("statistics = %+v, error = %v; want %+v", got, err, want)
			}
		})
	}
}
