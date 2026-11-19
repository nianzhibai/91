package catalog

import (
	"context"
	"database/sql"
	"sort"
	"testing"
	"time"
)

// TestListVideoFileIDsByDrive 校验上传 worker 用到的轻量 file_id 查询：
// - 只返回指定 drive 的 file_id；不返回其它 drive 的
// - 跳过 file_id 为空的视频
// - 返回顺序无要求，但每个 file_id 只出现一次
func TestListVideoFileIDsByDrive(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	insert := func(id, drive, fileID string) {
		if err := cat.UpsertVideo(ctx, &Video{
			ID:          id,
			DriveID:     drive,
			FileID:      fileID,
			Title:       id,
			PublishedAt: now,
		}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}

	insert("scriptcrawler-A-source001", "crawler-a", "source001.mp4")
	insert("scriptcrawler-A-source002", "crawler-a", "source002.flv")
	insert("scriptcrawler-A-source003", "crawler-a", "source003.mp4")
	// 不同 drive 的视频不应出现
	insert("quark-other-fid", "drive-quark", "abcdef")
	// 空 file_id 应被过滤
	insert("scriptcrawler-A-empty", "crawler-a", "")

	got, err := cat.ListVideoFileIDsByDrive(ctx, "crawler-a")
	if err != nil {
		t.Fatalf("ListVideoFileIDsByDrive: %v", err)
	}
	sort.Strings(got)
	want := []string{"source001.mp4", "source002.flv", "source003.mp4"}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("got %d ids, want %d: got=%v", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// 空 drive 返回空列表，不报错
	other, err := cat.ListVideoFileIDsByDrive(ctx, "no-such-drive")
	if err != nil {
		t.Fatalf("ListVideoFileIDsByDrive empty: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("non-existent drive: got %v, want empty", other)
	}
}

// TestListCrawlerSourceIDsFindsMigratedVideos 校验：即使爬虫视频被上传迁移
// 到目标网盘（drive_id 改了），ListCrawlerSourceIDs 仍能通过 video.id 前缀
// 找到这些 source_id。这是 crawler 写 seen 文件的关键不变量，否则下一次
// 爬取会把已爬过的 source_id 当作"新"的再爬一遍。
func TestListCrawlerSourceIDsFindsMigratedVideos(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	insert := func(id, drive, fileID string) {
		if err := cat.UpsertVideo(ctx, &Video{
			ID:          id,
			DriveID:     drive,
			FileID:      fileID,
			Title:       id,
			PublishedAt: now,
		}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}

	// 1) 仍在本地爬虫 drive 下的视频（未上传）
	insert("scriptcrawler-crawler-a-source001", "crawler-a", "source001.mp4")
	// 2) 已上传到目标盘的视频：drive_id 变了，但 id 仍保留 crawler 来源前缀。
	insert("scriptcrawler-crawler-a-source002", "target-drive", "TARGET-FILE-ID-2")
	insert("scriptcrawler-crawler-a-source003", "target-drive", "TARGET-FILE-ID-3")
	// 3) 别的爬虫 drive 的视频，不应混进来
	insert("scriptcrawler-other-source999", "other-crawler", "source999.mp4")
	// 4) 完全无关的视频
	insert("quark-some-fid", "drive-quark", "abc")

	got, err := cat.ListCrawlerSourceIDs(ctx, "scriptcrawler", "crawler-a")
	if err != nil {
		t.Fatalf("ListCrawlerSourceIDs: %v", err)
	}
	sort.Strings(got)
	want := []string{"source001", "source002", "source003"}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("got %d source ids, want %d: got=%v", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// 不存在的 drive 返回空列表
	other, err := cat.ListCrawlerSourceIDs(ctx, "scriptcrawler", "no-such-drive")
	if err != nil {
		t.Fatalf("ListCrawlerSourceIDs empty: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("non-existent drive: got %v, want empty", other)
	}
}

func TestDeleteVideoWithTombstonePreventsReimport(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &Video{
		ID:            "scriptcrawler-crawler-a-source004",
		DriveID:       "crawler-a",
		FileID:        "source004.mp4",
		FileName:      "source004.mp4",
		ContentHash:   "ABCDEF",
		Title:         "Deleted Source",
		Size:          2048,
		PreviewStatus: "ready",
		PublishedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := cat.DeleteVideoWithTombstoneOptions(ctx, "scriptcrawler-crawler-a-source004", DeleteVideoTombstoneOptions{}); err != nil {
		t.Fatalf("delete with tombstone: %v", err)
	}
	if _, err := cat.GetVideo(ctx, "scriptcrawler-crawler-a-source004"); err != sql.ErrNoRows {
		t.Fatalf("get deleted video error = %v, want sql.ErrNoRows", err)
	}
	deleted, err := cat.IsDeletedVideoCandidate(ctx, "scriptcrawler-crawler-a-source004", "crawler-a", "source004.mp4", "abcdef", "source004.mp4", 2048)
	if err != nil {
		t.Fatalf("check deleted candidate: %v", err)
	}
	if !deleted {
		t.Fatal("deleted candidate was not recognized")
	}
	sourceIDs, err := cat.ListCrawlerSourceIDs(ctx, "scriptcrawler", "crawler-a")
	if err != nil {
		t.Fatalf("ListCrawlerSourceIDs: %v", err)
	}
	if len(sourceIDs) != 1 || sourceIDs[0] != "source004" {
		t.Fatalf("source ids = %#v, want [source004]", sourceIDs)
	}
}
