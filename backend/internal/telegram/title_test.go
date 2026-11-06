package telegram

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"
)

func TestUntitledVideoUsesMessageTimestamp(t *testing.T) {
	s, cat := testService(t)
	ctx := context.Background()
	var u update
	if err := json.Unmarshal([]byte(`{"update_id":1,"message":{"message_id":29,"date":1789975384,"from":{"id":42},"chat":{"id":42,"type":"private"},"video":{"file_id":"video","file_unique_id":"unique","file_size":5,"mime_type":"video/mp4"}}}`), &u); err != nil {
		t.Fatal(err)
	}
	if err := s.accept(ctx, u); err != nil {
		t.Fatal(err)
	}
	job := albumJobs(t, cat)["unique"]
	if job == nil || job.RequestedTitle != "1789975384" {
		t.Fatalf("untitled video did not use message date: %+v", job)
	}
	if err := s.accept(ctx, u); err != nil {
		t.Fatal(err)
	}
	jobs := albumJobs(t, cat)
	if len(jobs) != 1 || jobs["unique"].RequestedTitle != job.RequestedTitle {
		t.Fatalf("redelivery changed the timestamp title: %v", jobs)
	}
}

func TestMissingMessageDateUsesPersistedReceptionTime(t *testing.T) {
	s, cat := testService(t)
	ctx := context.Background()
	u := albumVideo(29, "")
	u.Message.Date = 0
	before := time.Now().Unix()
	if err := s.accept(ctx, u); err != nil {
		t.Fatal(err)
	}
	after := time.Now().Unix()
	groups, err := cat.PendingTelegramMediaGroups(ctx, s.BotID(), time.Now().Add(time.Hour))
	if err != nil || len(groups) != 1 || len(groups[0].Updates) != 1 {
		t.Fatalf("buffered groups=%v err=%v", groups, err)
	}
	var persisted update
	if err := json.Unmarshal([]byte(groups[0].Updates[0].Payload), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Message.Date < before || persisted.Message.Date > after {
		t.Fatalf("reception time was not persisted: %d", persisted.Message.Date)
	}
	restarted := New(s.cfg, s.token, cat, s.uploadDir, s.reserve)
	restarted.botID = s.BotID()
	flushTestAlbum(t, restarted)
	job := albumJobs(t, cat)["unique-29"]
	if job == nil || job.RequestedTitle != strconv.FormatInt(persisted.Message.Date, 10) {
		t.Fatalf("receiver restart changed reception timestamp: %+v", job)
	}
}

func TestTimestampFallbackPreservesCaptionAndFilenamePriority(t *testing.T) {
	for _, tc := range []struct{ caption, filename, want string }{
		{"附文标题\n第二行", "原文件.mp4", "附文标题 第二行"},
		{"\n  第一段  \r\n\n 第二段\t#标签 \n 第三段  ", "原文件.mp4", "第一段 第二段 #标签 第三段"},
		{"", "原文件.mp4", "原文件"},
		{"", "", "1789975384"},
		{" \n ", "", "1789975384"},
		{"", "<>.mp4", "1789975384"},
		{"▎Source", "原文件.mp4", "▎Source"},
	} {
		if got := videoTitle(tc.caption, tc.filename, 1789975384); got != tc.want {
			t.Errorf("caption=%q filename=%q: got %q want %q", tc.caption, tc.filename, got, tc.want)
		}
	}
}

func TestBufferedUntitledVideoWithoutDateUsesStoredReceptionTime(t *testing.T) {
	s, cat := testService(t)
	ctx := context.Background()
	u := albumVideo(29, "")
	u.Message.Date = 0
	// Model an album already buffered by a receiver that did not capture date.
	if err := s.stageMediaGroup(ctx, u); err != nil {
		t.Fatal(err)
	}
	groups, err := cat.PendingTelegramMediaGroups(ctx, s.BotID(), time.Now().Add(time.Hour))
	if err != nil || len(groups) != 1 || len(groups[0].Updates) != 1 {
		t.Fatalf("buffered groups=%v err=%v", groups, err)
	}
	timestamp := groups[0].Updates[0].ReceivedAt / 1000
	flushTestAlbum(t, s)
	job := albumJobs(t, cat)["unique-29"]
	if timestamp <= 0 || job == nil || job.RequestedTitle != strconv.FormatInt(timestamp, 10) {
		t.Fatalf("missing date did not use stored reception time: %+v", job)
	}
}
