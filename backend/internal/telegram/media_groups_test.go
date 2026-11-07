package telegram

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/videoname"
)

func albumVideo(id int64, caption string) update {
	u := videoUpdate(id, 42)
	u.Message.MediaGroupID = "album"
	u.Message.Caption = caption
	u.Message.Video.Name = ""
	u.Message.Video.FileID = fmt.Sprintf("file-%d", id)
	u.Message.Video.UniqueID = fmt.Sprintf("unique-%d", id)
	return u
}

func albumPhoto(id int64, caption string) update {
	u := albumVideo(id, caption)
	u.Message.Video = nil
	u.Message.Photo = []media{{FileID: "photo", UniqueID: "photo-unique", Size: 50}}
	return u
}

func flushTestAlbum(t *testing.T, s *Service) {
	t.Helper()
	if err := s.flushMediaGroups(context.Background(), time.Now().Add(mediaGroupQuietPeriod)); err != nil {
		t.Fatal(err)
	}
}

func albumJobs(t *testing.T, cat *catalog.Catalog) map[string]*catalog.RemoteUploadJob {
	t.Helper()
	jobs, err := cat.ListImportJobs(context.Background(), "telegram", "", 0, 30)
	if err != nil {
		t.Fatal(err)
	}
	byFile := make(map[string]*catalog.RemoteUploadJob)
	for _, job := range jobs {
		source, err := catalog.DecodeTelegramSource(job)
		if err != nil {
			t.Fatal(err)
		}
		byFile[source.UniqueID] = job
	}
	return byFile
}

func TestMediaGroupPhotoCaptionNamesVideosBeforeAdmission(t *testing.T) {
	s, cat := testService(t)
	ctx := context.Background()
	var photo update
	if err := json.Unmarshal([]byte(`{"update_id":25,"message":{"message_id":25,"chat":{"id":42,"type":"private"},"from":{"id":42},"media_group_id":"album","caption":"海边旅行\n#旅行","photo":[{"file_id":"photo","file_unique_id":"photo-unique","file_size":50}]}}`), &photo); err != nil {
		t.Fatal(err)
	}
	// Exercise a caption arriving after videos, and order by Telegram message
	// identity rather than arrival order. Admission waits for the whole group.
	updates := []update{albumVideo(28, ""), albumVideo(26, ""), photo, albumVideo(27, "")}
	for i := range updates {
		updates[i].ID = int64(100 + i)
	}
	for _, u := range updates {
		if err := s.accept(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.flushMediaGroups(ctx, time.Now().Add(-mediaGroupQuietPeriod)); err != nil {
		t.Fatal(err)
	}
	if jobs := albumJobs(t, cat); len(jobs) != 0 {
		t.Fatalf("videos queued before captions settled: %v", jobs)
	}
	if _, err := cat.ClaimNextImportJob(ctx, true); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("worker could claim incomplete album: %v", err)
	}
	if receipts, err := cat.PendingTelegramReceipts(ctx, s.BotID()); err != nil || len(receipts) != 0 {
		t.Fatalf("premature album replies: %v %v", receipts, err)
	}
	flushTestAlbum(t, s)
	jobs := albumJobs(t, cat)
	if len(jobs) != 3 {
		t.Fatalf("jobs=%v", jobs)
	}
	for id := int64(26); id <= 28; id++ {
		job := jobs[fmt.Sprintf("unique-%d", id)]
		if job == nil || job.RequestedTitle != fmt.Sprintf("海边旅行 #旅行 - %d", id-25) || job.State != "queued" {
			t.Fatalf("message %d: %+v", id, job)
		}
		source, err := catalog.DecodeTelegramSource(job)
		if err != nil || source.MediaGroupID != "album" || source.MediaGroupTitle != "海边旅行\n#旅行" || source.MediaGroupDate == 0 {
			t.Fatalf("album storage metadata was not persisted: %+v %v", source, err)
		}
	}
	receipts, err := cat.PendingTelegramReceipts(ctx, s.BotID())
	if err != nil || len(receipts) != 1 {
		t.Fatalf("album did not share one receipt: %v %v", receipts, err)
	}
	for _, r := range receipts {
		if r.MessageID != 26 || len(r.JobIDs) != 3 || r.JobID != "" || r.Response != "" {
			t.Fatalf("unexpected receipt: %+v", r)
		}
		for i, id := range r.JobIDs {
			if id != jobs[fmt.Sprintf("unique-%d", 26+i)].ID {
				t.Fatalf("group receipt lost a member: %+v", r)
			}
		}
	}
	for _, u := range updates {
		if err := s.accept(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	flushTestAlbum(t, s)
	if len(albumJobs(t, cat)) != 3 {
		t.Fatal("redelivery created duplicate jobs")
	}
}

func TestMediaGroupCaptionSurvivesReceiverRestart(t *testing.T) {
	s, cat := testService(t)
	ctx := context.Background()
	if err := s.accept(ctx, albumVideo(2, "")); err != nil {
		t.Fatal(err)
	}
	restarted := New(s.cfg, s.token, cat, s.uploadDir, s.reserve)
	restarted.botID = s.BotID()
	if err := restarted.accept(ctx, albumPhoto(1, "重启后收到的说明")); err != nil {
		t.Fatal(err)
	}
	flushTestAlbum(t, restarted)
	job := albumJobs(t, cat)["unique-2"]
	if job == nil || job.RequestedTitle != "重启后收到的说明" {
		t.Fatalf("caption or buffered video lost after restart: %+v", job)
	}
}

func TestMediaGroupTitlePolicy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		updates []update
		want    map[string]string
	}{
		{"caption on first video", []update{albumVideo(1, "旅行"), albumVideo(2, "")}, map[string]string{"unique-1": "旅行 - 1", "unique-2": "旅行 - 2"}},
		{"own video caption", []update{albumPhoto(1, "旅行"), albumVideo(2, ""), albumVideo(3, "单独的标题")}, map[string]string{"unique-2": "旅行 - 1", "unique-3": "单独的标题"}},
		{"no caption", []update{albumPhoto(1, ""), albumVideo(2, "")}, map[string]string{"unique-2": "1789975384"}},
		{"untitled videos in the same second", []update{albumVideo(1, ""), albumVideo(2, "")}, map[string]string{"unique-1": "1789975384 - 1", "unique-2": "1789975384 - 2"}},
		{"source marker policy unchanged", []update{albumPhoto(1, "▎Source"), albumVideo(2, "")}, map[string]string{"unique-2": "▎Source"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, cat := testService(t)
			for _, u := range tc.updates {
				if err := s.accept(context.Background(), u); err != nil {
					t.Fatal(err)
				}
			}
			flushTestAlbum(t, s)
			jobs := albumJobs(t, cat)
			if len(jobs) != len(tc.want) {
				t.Fatalf("jobs=%v", jobs)
			}
			for file, title := range tc.want {
				if jobs[file] == nil || jobs[file].RequestedTitle != title {
					t.Fatalf("file %s: got %+v want %q", file, jobs[file], title)
				}
			}
		})
	}
}

func TestMediaGroupDocumentUsesSharedCaptionAndFilenameFallback(t *testing.T) {
	for _, caption := range []string{"文档视频", ""} {
		t.Run(caption, func(t *testing.T) {
			s, cat := testService(t)
			u := albumVideo(2, "")
			u.Message.Document, u.Message.Video = u.Message.Video, nil
			u.Message.Document.Name = "原文件.mp4"
			for _, input := range []update{albumPhoto(1, caption), u} {
				if err := s.accept(context.Background(), input); err != nil {
					t.Fatal(err)
				}
			}
			flushTestAlbum(t, s)
			want := caption
			if want == "" {
				want = "原文件"
			}
			if job := albumJobs(t, cat)["unique-2"]; job == nil || job.RequestedTitle != want {
				t.Fatalf("document title=%+v want=%s", job, want)
			}
		})
	}
}

func TestMediaGroupLongTitlesKeepDistinctSuffixes(t *testing.T) {
	s, cat := testService(t)
	for _, u := range []update{albumPhoto(1, strings.Repeat("很长的说明", 60)), albumVideo(2, ""), albumVideo(3, "")} {
		if err := s.accept(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	flushTestAlbum(t, s)
	jobs := albumJobs(t, cat)
	for id := 2; id <= 3; id++ {
		job := jobs[fmt.Sprintf("unique-%d", id)]
		if job == nil || len(job.RequestedTitle) > 120 || !strings.HasSuffix(job.RequestedTitle, fmt.Sprintf(" - %d", id-1)) {
			t.Fatalf("numbering lost after title truncation: %+v", job)
		}
		if err := videoname.ValidateUploadTitle(job.RequestedTitle, ".mp4"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMediaGroupPhotoOnlyRepliesOnceAndStandalonePhotoStillReplies(t *testing.T) {
	s, cat := testService(t)
	ctx := context.Background()
	for _, u := range []update{albumPhoto(1, "说明"), albumPhoto(2, "")} {
		if err := s.accept(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	flushTestAlbum(t, s)
	u := albumPhoto(3, "")
	u.Message.MediaGroupID = ""
	if err := s.accept(ctx, u); err != nil {
		t.Fatal(err)
	}
	receipts, err := cat.PendingTelegramReceipts(ctx, s.BotID())
	if err != nil || len(receipts) != 2 || len(albumJobs(t, cat)) != 0 {
		t.Fatalf("receipts=%v err=%v", receipts, err)
	}
	for _, r := range receipts {
		if !strings.Contains(renderReceiptMessage(r).text, "请发送视频") || r.MessageID == 2 {
			t.Fatalf("unexpected photo reply: %+v", r)
		}
	}
}

func TestMediaGroupRechecksAuthorizationAfterBuffering(t *testing.T) {
	s, cat := testService(t)
	ctx := context.Background()
	for _, u := range []update{albumPhoto(1, "说明"), albumVideo(2, "")} {
		if err := s.accept(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	s.cfg.AllowedUserIDs = nil
	flushTestAlbum(t, s)
	if len(albumJobs(t, cat)) != 0 {
		t.Fatal("revoked sender's album was imported")
	}
	if replies, err := cat.PendingTelegramReceipts(ctx, s.BotID()); err != nil || len(replies) != 0 {
		t.Fatalf("revoked sender received replies: %v %v", replies, err)
	}
	if err := s.accept(ctx, albumVideo(3, "")); err != nil {
		t.Fatal(err)
	}
	if groups, err := cat.PendingTelegramMediaGroups(ctx, s.BotID(), time.Now().Add(time.Hour)); err != nil || len(groups) != 0 {
		t.Fatalf("unauthorized message buffered: %v %v", groups, err)
	}
}

func TestMediaGroupRejectsWholeGroupWithOneExplanation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
		size  int64
		want  string
	}{
		{"queue full", 1, 5, "队列已满"},
		{"file too large", 100, 6, "超过配置"},
		{"file size unknown", 100, 0, "无法确认视频大小"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, cat := testService(t)
			s.cfg.MaxPendingJobs, s.cfg.MaxFileSizeBytes = tc.limit, 5
			ctx := context.Background()
			last := albumVideo(3, "")
			last.Message.Video.Size = tc.size
			updates := []update{albumPhoto(1, "说明"), albumVideo(2, ""), last}
			for _, u := range updates {
				if err := s.accept(ctx, u); err != nil {
					t.Fatal(err)
				}
			}
			flushTestAlbum(t, s)
			if jobs := albumJobs(t, cat); len(jobs) != 0 {
				t.Fatalf("rejected album partially queued: %v", jobs)
			}
			receipts, err := cat.PendingTelegramReceipts(ctx, s.BotID())
			if err != nil || len(receipts) != 1 || !strings.Contains(renderReceiptMessage(receipts[0]).text, tc.want) {
				t.Fatalf("expected one group rejection: %v %v", receipts, err)
			}
			for _, u := range updates {
				if err := s.accept(ctx, u); err != nil {
					t.Fatal(err)
				}
			}
			if groups, err := cat.PendingTelegramMediaGroups(ctx, s.BotID(), time.Now().Add(time.Hour)); err != nil || len(groups) != 0 {
				t.Fatalf("rejected group messages staged again: %v %v", groups, err)
			}
		})
	}
}
