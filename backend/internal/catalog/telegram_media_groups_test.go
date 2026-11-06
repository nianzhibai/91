package catalog

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func stagedAlbumUpdate(id int64) TelegramMediaGroupUpdate {
	return TelegramMediaGroupUpdate{BotID: 123, ChatID: 42, MessageID: id, UpdateID: id, MediaGroupID: "album", Payload: fmt.Sprintf(`{"update_id":%d}`, id)}
}

func TestTelegramMediaGroupsUseArrivalOrderWithOpaqueUpdateIDs(t *testing.T) {
	c := telegramTestCatalog(t)
	ctx := context.Background()
	for _, id := range []int64{100, 101, 200, 201, 50, 51} {
		u := stagedAlbumUpdate(id)
		u.UpdateID = -id
		u.MediaGroupID = fmt.Sprint(id / 100)
		if err := c.StageTelegramMediaGroupUpdate(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	// Two groups arrived in the same millisecond. A delayed older message must
	// still follow both; IDs are only a deterministic tie-breaker within a time.
	if _, err := c.db.Exec(`UPDATE telegram_media_group_updates SET received_at=CASE WHEN message_id<100 THEN 20 ELSE 10 END`); err != nil {
		t.Fatal(err)
	}
	groups, err := c.PendingTelegramMediaGroups(ctx, 123, time.UnixMilli(30))
	if err != nil || len(groups) != 3 {
		t.Fatalf("groups=%v error=%v", groups, err)
	}
	for i, want := range []string{"1", "2", "0"} {
		if groups[i].ID != want || len(groups[i].Updates) != 2 {
			t.Fatalf("group %d: %+v, want %s with two members", i, groups[i], want)
		}
		if accepted, err := c.AcceptTelegramMediaGroup(ctx, groups[i], albumImports(groups[i]), 2); err != nil || !accepted {
			t.Fatal("admission failed", err)
		}
	}
	jobs, err := c.ListImportJobs(ctx, "telegram", "", 0, 30)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs=%v error=%v", jobs, err)
	}
	for _, job := range jobs {
		if job.ID != "job-100" && job.ID != "job-101" {
			t.Fatalf("newer group took the oldest group's queue capacity: %s", job.ID)
		}
	}
}

func pendingAlbum(t *testing.T, c *Catalog) TelegramMediaGroup {
	t.Helper()
	groups, err := c.PendingTelegramMediaGroups(context.Background(), 123, time.Now().Add(time.Hour))
	if err != nil || len(groups) != 1 {
		t.Fatalf("groups=%v err=%v", groups, err)
	}
	return groups[0]
}

func albumImports(group TelegramMediaGroup) []TelegramImport {
	var imports []TelegramImport
	for _, u := range group.Updates {
		imports = append(imports, TelegramImport{
			Receipt: TelegramReceipt{BotID: u.BotID, ChatID: u.ChatID, MessageID: u.MessageID, UpdateID: u.UpdateID, SenderID: 42},
			Source:  &TelegramSource{BotID: u.BotID, SenderID: 42, FileID: fmt.Sprint("file-", u.MessageID), UniqueID: fmt.Sprint("unique-", u.MessageID), Size: 5},
			ID:      fmt.Sprint("job-", u.MessageID), Title: fmt.Sprint("相册 - ", u.MessageID),
		})
	}
	return imports
}

func TestTelegramMediaGroupInboxSurvivesDatabaseReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "catalog.db")
	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c != nil {
			c.Close()
		}
	})
	if _, err := c.TelegramPaused(ctx, 123); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2} {
		if err := c.StageTelegramMediaGroupUpdate(ctx, stagedAlbumUpdate(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	group := pendingAlbum(t, c)
	if len(group.Updates) != 2 || group.Updates[0].UpdateID != 1 || group.Updates[1].UpdateID != 2 {
		t.Fatalf("buffer or ordering lost on reopen: %+v", group)
	}
	// Replayed updates neither replace the payload nor postpone a ready group.
	if _, err := c.db.Exec(`UPDATE telegram_media_group_updates SET received_at=1`); err != nil {
		t.Fatal(err)
	}
	duplicate := stagedAlbumUpdate(1)
	duplicate.Payload = "must not replace original"
	if err := c.StageTelegramMediaGroupUpdate(ctx, duplicate); err != nil {
		t.Fatal(err)
	}
	groups, err := c.PendingTelegramMediaGroups(ctx, 123, time.UnixMilli(2))
	if err != nil || len(groups) != 1 || len(groups[0].Updates) != 2 || groups[0].Updates[0].Payload != stagedAlbumUpdate(1).Payload {
		t.Fatalf("redelivery changed collection window or payload: %v %v", groups, err)
	}
}

func TestTelegramMediaGroupAdmissionIsAtomicAndIdempotent(t *testing.T) {
	c := telegramTestCatalog(t)
	ctx := context.Background()
	for _, id := range []int64{1, 2} {
		if err := c.StageTelegramMediaGroupUpdate(ctx, stagedAlbumUpdate(id)); err != nil {
			t.Fatal(err)
		}
	}
	group := pendingAlbum(t, c)
	imports := albumImports(group)
	imports[1].ID = imports[0].ID
	if accepted, err := c.AcceptTelegramMediaGroup(ctx, group, imports, 100); err == nil || accepted {
		t.Fatal("expected second task insertion to fail")
	}
	for _, table := range []string{"remote_upload_jobs", "telegram_receipts", "telegram_files"} {
		var n int
		if err := c.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("partial album survived in %s: %d %v", table, n, err)
		}
	}
	if len(pendingAlbum(t, c).Updates) != 2 {
		t.Fatal("failed admission discarded buffered messages")
	}
	// A later independent message may have already advanced the polling cursor.
	if err := c.AcceptTelegramUpdate(ctx, TelegramReceipt{BotID: 123, UpdateID: 9, MessageID: 9, ChatID: 42, Response: "__ignore__"}, nil, "", "", 100); err != nil {
		t.Fatal(err)
	}
	if accepted, err := c.AcceptTelegramMediaGroup(ctx, group, albumImports(group), 100); err != nil || !accepted {
		t.Fatalf("admission failed: %v %v", accepted, err)
	}
	receipts, err := c.PendingTelegramReceipts(ctx, 123)
	if err != nil || len(receipts) != 1 || receipts[0].MessageID != 1 || len(receipts[0].JobIDs) != 2 {
		t.Fatalf("expected one receipt tracking both videos: %+v %v", receipts, err)
	}
	for i, id := range receipts[0].JobIDs {
		if id != fmt.Sprint("job-", i+1) {
			t.Fatalf("group receipt lost a video: %+v", receipts[0])
		}
	}
	var silent int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM telegram_receipts WHERE response='__ignore__'`).Scan(&silent); err != nil || silent != 2 {
		t.Fatalf("missing silent deduplication receipt: %d %v", silent, err)
	}
	if accepted, err := c.AcceptTelegramMediaGroup(ctx, group, albumImports(group), 100); err != nil || accepted {
		t.Fatalf("stale snapshot admitted twice: %v %v", accepted, err)
	}
	for _, id := range []int64{1, 2} {
		if err := c.StageTelegramMediaGroupUpdate(ctx, stagedAlbumUpdate(id)); err != nil {
			t.Fatal(err)
		}
	}
	if groups, err := c.PendingTelegramMediaGroups(ctx, 123, time.Now().Add(time.Hour)); err != nil || len(groups) != 0 {
		t.Fatalf("completed updates staged again: %v %v", groups, err)
	}
}

func TestTelegramMediaGroupWaitsForMembersAddedAfterSnapshot(t *testing.T) {
	c := telegramTestCatalog(t)
	ctx := context.Background()
	if err := c.StageTelegramMediaGroupUpdate(ctx, stagedAlbumUpdate(1)); err != nil {
		t.Fatal(err)
	}
	group := pendingAlbum(t, c)
	if err := c.StageTelegramMediaGroupUpdate(ctx, stagedAlbumUpdate(2)); err != nil {
		t.Fatal(err)
	}
	if accepted, err := c.AcceptTelegramMediaGroup(ctx, group, albumImports(group), 100); err != nil || accepted {
		t.Fatalf("incomplete group published: %v %v", accepted, err)
	}
	if jobs, err := c.ListImportJobs(ctx, "telegram", "", 0, 30); err != nil || len(jobs) != 0 {
		t.Fatalf("partial jobs created: %v %v", jobs, err)
	}
	group = pendingAlbum(t, c)
	if len(group.Updates) != 2 {
		t.Fatalf("new member lost: %+v", group)
	}
	if accepted, err := c.AcceptTelegramMediaGroup(ctx, group, albumImports(group), 100); err != nil || !accepted {
		t.Fatalf("complete group rejected: %v %v", accepted, err)
	}
}

func TestTelegramMediaGroupIdentityIncludesBotAndChat(t *testing.T) {
	c := telegramTestCatalog(t)
	ctx := context.Background()
	if _, err := c.TelegramPaused(ctx, 456); err != nil {
		t.Fatal(err)
	}
	for _, u := range []TelegramMediaGroupUpdate{
		{BotID: 123, ChatID: 42, UpdateID: 1, MessageID: 1, MediaGroupID: "same", Payload: "first"},
		{BotID: 123, ChatID: 43, UpdateID: 2, MessageID: 1, MediaGroupID: "same", Payload: "second"},
		{BotID: 456, ChatID: 42, UpdateID: 1, MessageID: 1, MediaGroupID: "same", Payload: "third"},
	} {
		if err := c.StageTelegramMediaGroupUpdate(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	for bot, want := range map[int64]int{123: 2, 456: 1} {
		groups, err := c.PendingTelegramMediaGroups(ctx, bot, time.Now().Add(time.Hour))
		if err != nil || len(groups) != want {
			t.Fatalf("bot=%d groups=%v err=%v", bot, groups, err)
		}
		for _, group := range groups {
			if group.BotID != bot || len(group.Updates) != 1 {
				t.Fatalf("unrelated albums mixed: %+v", group)
			}
		}
	}
}
