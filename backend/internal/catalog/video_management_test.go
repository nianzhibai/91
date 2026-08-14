package catalog

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestListHiddenVideosForMigration 验证：隐藏的视频不进可见列表，
// 但能被 ListHiddenVideos 拿到（供一次性迁移为墓碑）。
func TestListHiddenVideosForMigration(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	for _, id := range []string{"v1", "v2", "v3"} {
		if err := cat.UpsertVideo(ctx, &Video{
			ID: id, DriveID: "drive", FileID: "f-" + id, Title: id,
			PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	if err := cat.HideVideo(ctx, "v2"); err != nil {
		t.Fatalf("hide v2: %v", err)
	}

	visible, total, err := cat.ListVideos(ctx, ListParams{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("list visible: %v", err)
	}
	if total != 2 || len(visible) != 2 {
		t.Fatalf("visible total/len = %d/%d, want 2/2", total, len(visible))
	}
	for _, v := range visible {
		if v.ID == "v2" {
			t.Fatalf("hidden v2 leaked into visible list")
		}
	}

	hidden, err := cat.ListHiddenVideos(ctx)
	if err != nil {
		t.Fatalf("list hidden: %v", err)
	}
	if len(hidden) != 1 || hidden[0].ID != "v2" {
		t.Fatalf("ListHiddenVideos = %v, want only v2", hidden)
	}

	current, blacklisted, err := cat.VideoManagementCounts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if current != 2 || blacklisted != 0 {
		t.Fatalf("counts = current %d blacklisted %d, want 2/0", current, blacklisted)
	}
}

// TestBlacklistListAndRemove 验证墓碑表的列出、关键字过滤和移除。
func TestBlacklistListAndRemove(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	seed := []struct{ id, drive, file string }{
		{"d1", "drive", "movie-alpha.avi"},
		{"d2", "drive", "movie-beta.mp4"},
		{"d3", "archive", "clip-gamma.wmv"},
	}
	for _, s := range seed {
		if err := cat.UpsertVideo(ctx, &Video{
			ID: s.id, DriveID: s.drive, FileID: "f-" + s.id, FileName: s.file,
			Title: s.id, PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", s.id, err)
		}
		var err error
		if s.id == "d2" {
			err = cat.DeleteVideoWithTombstoneReason(ctx, s.id, DeletedVideoReasonDuplicate)
		} else {
			err = cat.DeleteVideoWithTombstone(ctx, s.id)
		}
		if err != nil {
			t.Fatalf("tombstone %s: %v", s.id, err)
		}
	}

	items, total, err := cat.ListDeletedVideos(ctx, ListParams{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("list deleted: %v", err)
	}
	if total != 3 || len(items) != 3 {
		t.Fatalf("deleted total/len = %d/%d, want 3/3", total, len(items))
	}
	reasons := map[string]string{}
	for _, item := range items {
		reasons[item.ID] = item.Reason
	}
	if reasons["d1"] != "" || reasons["d3"] != "" {
		t.Fatalf("manual tombstone reasons = %#v, want empty", reasons)
	}
	if reasons["d2"] != DeletedVideoReasonDuplicate {
		t.Fatalf("duplicate tombstone reason = %q, want %q", reasons["d2"], DeletedVideoReasonDuplicate)
	}

	// 关键字过滤
	filtered, ftotal, err := cat.ListDeletedVideos(ctx, ListParams{Keyword: "movie", Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("list deleted filtered: %v", err)
	}
	if ftotal != 2 || len(filtered) != 2 {
		t.Fatalf("filtered total/len = %d/%d, want 2/2", ftotal, len(filtered))
	}

	// 网盘过滤
	driveFiltered, driveTotal, err := cat.ListDeletedVideos(ctx, ListParams{DriveID: "archive", Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("list deleted drive filtered: %v", err)
	}
	if driveTotal != 1 || len(driveFiltered) != 1 || driveFiltered[0].ID != "d3" {
		t.Fatalf("drive filtered = total %d items %#v, want only d3", driveTotal, driveFiltered)
	}

	combined, combinedTotal, err := cat.ListDeletedVideos(ctx, ListParams{Keyword: "movie", DriveID: "archive", Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("list deleted combined filtered: %v", err)
	}
	if combinedTotal != 0 || len(combined) != 0 {
		t.Fatalf("combined filtered total/len = %d/%d, want 0/0", combinedTotal, len(combined))
	}

	// 移出黑名单
	if err := cat.RemoveDeletedVideo(ctx, "d1"); err != nil {
		t.Fatalf("remove d1: %v", err)
	}
	if deleted, err := cat.IsVideoDeleted(ctx, "d1"); err != nil || deleted {
		t.Fatalf("d1 should no longer be blacklisted (deleted=%v err=%v)", deleted, err)
	}
	_, total, err = cat.ListDeletedVideos(ctx, ListParams{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("list deleted after remove: %v", err)
	}
	if total != 2 {
		t.Fatalf("deleted total after remove = %d, want 2", total)
	}

	if err := cat.RemoveDeletedVideo(ctx, "does-not-exist"); err == nil {
		t.Fatalf("remove missing id should return error")
	}

	// counts: 删完一个还剩 2 个黑名单；可见视频已全部被墓碑删除
	current, blacklisted, err := cat.VideoManagementCounts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if current != 0 || blacklisted != 2 {
		t.Fatalf("counts = current %d blacklisted %d, want 0/2", current, blacklisted)
	}
}

func TestBlacklistRestorePolicies(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	seedVideo := func(id, driveID, fileID, title string) {
		t.Helper()
		if err := cat.UpsertVideo(ctx, &Video{
			ID: id, DriveID: driveID, FileID: fileID, FileName: fileID + ".mp4",
			Title: title, Size: 123, PublishedAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	findDeleted := func(id string) *DeletedVideo {
		t.Helper()
		items, _, err := cat.ListDeletedVideos(ctx, ListParams{Page: 1, PageSize: 50, IncludeSourceDeleted: true})
		if err != nil {
			t.Fatalf("list deleted: %v", err)
		}
		for _, item := range items {
			if item.ID == id {
				return item
			}
		}
		t.Fatalf("deleted video %s not found", id)
		return nil
	}

	seedVideo("remote-video", "remote", "remote-file", "Remote")
	if err := cat.DeleteVideoWithTombstone(ctx, "remote-video"); err != nil {
		t.Fatalf("tombstone remote: %v", err)
	}
	if got := findDeleted("remote-video").RestorePolicy; got != DeletedVideoRestorePolicyScan {
		t.Fatalf("remote restore policy = %q, want %q", got, DeletedVideoRestorePolicyScan)
	}
	if err := cat.RemoveDeletedVideo(ctx, "remote-video"); err != nil {
		t.Fatalf("allow remote rediscovery: %v", err)
	}

	if err := cat.UpsertDrive(ctx, &Drive{
		ID: "crawler-a", Kind: "scriptcrawler", Name: "Crawler", RootID: "/", TeaserEnabled: true,
	}); err != nil {
		t.Fatalf("seed crawler drive: %v", err)
	}
	crawlerID := "scriptcrawler-crawler-a-source-1"
	seedVideo(crawlerID, "crawler-a", "source-1.mp4", "Crawler")
	if err := cat.MarkCrawlerSourceSeen(ctx, "scriptcrawler", "crawler-a", "source-1", "imported", crawlerID, "sampled", 123); err != nil {
		t.Fatalf("mark crawler source seen: %v", err)
	}
	if err := cat.DeleteVideoWithTombstone(ctx, crawlerID); err != nil {
		t.Fatalf("tombstone crawler: %v", err)
	}
	if got := findDeleted(crawlerID).RestorePolicy; got != DeletedVideoRestorePolicyCrawler {
		t.Fatalf("crawler restore policy = %q, want %q", got, DeletedVideoRestorePolicyCrawler)
	}
	if err := cat.RemoveDeletedVideo(ctx, crawlerID); err != nil {
		t.Fatalf("allow crawler rediscovery: %v", err)
	}
	seenIDs, err := cat.ListCrawlerSourceIDs(ctx, "scriptcrawler", "crawler-a")
	if err != nil {
		t.Fatalf("list crawler source ids: %v", err)
	}
	if len(seenIDs) != 1 || seenIDs[0] != "source-1" {
		t.Fatalf("crawler source seen after restore request = %#v, want source-1", seenIDs)
	}
	if deleted, err := cat.IsVideoDeleted(ctx, crawlerID); err != nil || !deleted {
		t.Fatalf("crawler restore request must retain internal tombstone: deleted=%v err=%v", deleted, err)
	}
	if requests, err := cat.ListCrawlerRestoreRequests(ctx, "crawler-a"); err != nil || len(requests) != 1 {
		t.Fatalf("crawler restore requests = %#v err=%v, want one", requests, err)
	}
	if items, total, err := cat.ListDeletedVideos(ctx, ListParams{Page: 1, PageSize: 50}); err != nil || total != 0 || len(items) != 0 {
		t.Fatalf("visible blacklist after crawler restore request = total:%d items:%#v err:%v, want empty", total, items, err)
	}
	if _, blacklisted, err := cat.VideoManagementCounts(ctx); err != nil || blacklisted != 0 {
		t.Fatalf("blacklist count after crawler restore request = %d err=%v, want 0", blacklisted, err)
	}

	seedVideo("source-deleted", "remote", "gone", "Gone")
	if err := cat.DeleteVideoWithTombstoneOptions(ctx, "source-deleted", DeleteVideoTombstoneOptions{
		SourceDeleted: true,
	}); err != nil {
		t.Fatalf("delete source-deleted video: %v", err)
	}
	if deleted, err := cat.IsVideoDeleted(ctx, "source-deleted"); err != nil || deleted {
		t.Fatalf("source-deleted should not keep tombstone: deleted=%v err=%v", deleted, err)
	}
	if _, err := cat.GetVideo(ctx, "source-deleted"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("source-deleted catalog lookup error = %v, want sql.ErrNoRows", err)
	}

	// 本地上传的源文件被保留，但这个盘不支持枚举，永远不会被扫盘或爬取重新
	// 发现，所以取消拉黑必须当场重建记录。
	seedVideo("local-upload-video", "local-upload", "upload-file", "Upload")
	if err := cat.DeleteVideoWithTombstone(ctx, "local-upload-video"); err != nil {
		t.Fatalf("tombstone local upload: %v", err)
	}
	if got := findDeleted("local-upload-video").RestorePolicy; got != DeletedVideoRestorePolicyDirect {
		t.Fatalf("local upload restore policy = %q, want %q", got, DeletedVideoRestorePolicyDirect)
	}

	// 源文件已删的本地上传仍然不可恢复：source_deleted 的优先级高于来源类型。
	seedVideo("local-upload-gone", "local-upload", "gone-file", "Gone upload")
	if err := cat.DeleteVideoWithTombstoneOptions(ctx, "local-upload-gone", DeleteVideoTombstoneOptions{
		SourceDeleted: true,
	}); err != nil {
		t.Fatalf("delete local upload with source: %v", err)
	}
	if deleted, err := cat.IsVideoDeleted(ctx, "local-upload-gone"); err != nil || deleted {
		t.Fatalf("source-deleted local upload should not keep tombstone: deleted=%v err=%v", deleted, err)
	}

	// 被去重删掉的本地上传也仍然不可恢复：应当去看保留下来的那一条。
	seedVideo("local-upload-canonical", "local-upload", "canonical-file", "Canonical upload")
	seedVideo("local-upload-duplicate", "local-upload", "duplicate-file", "Duplicate upload")
	if err := cat.DeleteVideoWithTombstoneOptions(ctx, "local-upload-duplicate", DeleteVideoTombstoneOptions{
		Reason:           DeletedVideoReasonDuplicate,
		CanonicalVideoID: "local-upload-canonical",
	}); err != nil {
		t.Fatalf("tombstone duplicate local upload: %v", err)
	}
	if got := findDeleted("local-upload-duplicate").RestorePolicy; got != DeletedVideoRestorePolicyNone {
		t.Fatalf("duplicate local upload restore policy = %q, want %q", got, DeletedVideoRestorePolicyNone)
	}
	if err := cat.RemoveDeletedVideo(ctx, "local-upload-duplicate"); !errors.Is(err, ErrDeletedVideoNotRestorable) {
		t.Fatalf("restore duplicate local upload error = %v, want ErrDeletedVideoNotRestorable", err)
	}

	seedVideo("canonical-video", "remote", "canonical", "Canonical title")
	seedVideo("duplicate-video", "remote-copy", "duplicate", "Duplicate")
	if err := cat.DeleteVideoWithTombstoneOptions(ctx, "duplicate-video", DeleteVideoTombstoneOptions{
		Reason:           DeletedVideoReasonDuplicate,
		CanonicalVideoID: "canonical-video",
	}); err != nil {
		t.Fatalf("tombstone duplicate: %v", err)
	}
	duplicate := findDeleted("duplicate-video")
	if duplicate.RestorePolicy != DeletedVideoRestorePolicyNone ||
		duplicate.CanonicalVideoID != "canonical-video" ||
		duplicate.CanonicalTitle != "Canonical title" {
		t.Fatalf("duplicate metadata = %#v", duplicate)
	}
	if err := cat.RemoveDeletedVideo(ctx, "duplicate-video"); !errors.Is(err, ErrDeletedVideoNotRestorable) {
		t.Fatalf("restore duplicate error = %v, want ErrDeletedVideoNotRestorable", err)
	}

	for _, id := range []string{"local-upload-duplicate", "duplicate-video"} {
		deleted, err := cat.IsVideoDeleted(ctx, id)
		if err != nil || !deleted {
			t.Fatalf("non-restorable tombstone %s was removed: deleted=%v err=%v", id, deleted, err)
		}
	}
}

// 取消拉黑本地上传时记录当场重建，且标题、作者、标签这些用户数据都要跟着回来
// ——墓碑里存的是完整的 restore payload。派生资源（封面/预览/转码产物）在拉黑
// 时已被删除，必须重置为待生成，否则会指向不存在的文件。
func TestRemoveDeletedVideoDirectRestoresLocalUploadLosslessly(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &Video{
		ID: "local-upload-rich", DriveID: "local-upload", FileID: "rich.mp4",
		FileName: "rich.mp4", Title: "用户起的标题", Author: "上传者",
		Tags: []string{"标签一", "标签二"}, Description: "简介", DurationSeconds: 42,
		Size: 4096, Ext: "mp4", Quality: "HD",
		ThumbnailURL: "/p/thumb/local-upload-rich", PreviewLocal: "/data/previews/local-upload-rich.mp4",
		PreviewStatus: "ready", TranscodeStatus: "ready", TranscodedFileID: "transcoded.mp4",
		TranscodedSize: 2048, Views: 7, Likes: 3,
		PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if err := cat.DeleteVideoWithTombstone(ctx, "local-upload-rich"); err != nil {
		t.Fatalf("tombstone video: %v", err)
	}
	if _, err := cat.GetVideo(ctx, "local-upload-rich"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("video lookup after tombstone = %v, want sql.ErrNoRows", err)
	}

	if err := cat.RemoveDeletedVideo(ctx, "local-upload-rich"); err != nil {
		t.Fatalf("remove blacklist: %v", err)
	}

	restored, err := cat.GetVideo(ctx, "local-upload-rich")
	if err != nil {
		t.Fatalf("get restored video: %v", err)
	}
	if restored.Title != "用户起的标题" || restored.Author != "上传者" ||
		restored.Description != "简介" || restored.DurationSeconds != 42 {
		t.Fatalf("user metadata lost: %#v", restored)
	}
	if len(restored.Tags) != 2 || restored.Tags[0] != "标签一" || restored.Tags[1] != "标签二" {
		t.Fatalf("tags = %#v, want both restored", restored.Tags)
	}
	if restored.DriveID != "local-upload" || restored.FileID != "rich.mp4" || restored.Size != 4096 {
		t.Fatalf("source identity lost: %#v", restored)
	}
	if restored.Hidden {
		t.Fatalf("restored video must not be hidden")
	}
	if restored.ThumbnailURL != "" || restored.PreviewLocal != "" ||
		restored.PreviewStatus != "pending" {
		t.Fatalf("derived assets not reset: thumb=%q preview=%q status=%q",
			restored.ThumbnailURL, restored.PreviewLocal, restored.PreviewStatus)
	}
	if restored.TranscodeStatus != "" || restored.TranscodedFileID != "" || restored.TranscodedSize != 0 {
		t.Fatalf("transcode state not reset: %#v", restored)
	}
	if deleted, err := cat.IsVideoDeleted(ctx, "local-upload-rich"); err != nil || deleted {
		t.Fatalf("tombstone still present after restore: deleted=%v err=%v", deleted, err)
	}
	current, blacklisted, err := cat.VideoManagementCounts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if current != 1 || blacklisted != 0 {
		t.Fatalf("counts = current %d blacklisted %d, want 1/0", current, blacklisted)
	}
}

// 墓碑是老版本写的、restore_payload 为空时，仍然要能恢复出一条可用记录：
// 标题从文件名兜底，大小取墓碑里的值。
func TestRemoveDeletedVideoDirectRestoresWithoutPayload(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &Video{
		ID: "legacy-upload", DriveID: "local-upload", FileID: "legacy.mp4",
		FileName: "我的视频.mp4", Title: "旧标题", Size: 512,
		PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if err := cat.DeleteVideoWithTombstone(ctx, "legacy-upload"); err != nil {
		t.Fatalf("tombstone video: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx,
		`UPDATE deleted_videos SET restore_payload = '' WHERE id = ?`, "legacy-upload"); err != nil {
		t.Fatalf("clear restore payload: %v", err)
	}

	if err := cat.RemoveDeletedVideo(ctx, "legacy-upload"); err != nil {
		t.Fatalf("remove blacklist: %v", err)
	}
	restored, err := cat.GetVideo(ctx, "legacy-upload")
	if err != nil {
		t.Fatalf("get restored video: %v", err)
	}
	if restored.FileName != "我的视频.mp4" || restored.Title != "我的视频" {
		t.Fatalf("fallback identity = file %q title %q", restored.FileName, restored.Title)
	}
	if restored.FileID != "legacy.mp4" || restored.Size != 512 {
		t.Fatalf("fallback source = file %q size %d", restored.FileID, restored.Size)
	}
}

func TestDeletedVideoRestoreMetadataMigratesFromOldSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := db.Exec(`
CREATE TABLE deleted_videos (
	id           TEXT PRIMARY KEY,
	drive_id     TEXT NOT NULL DEFAULT '',
	file_id      TEXT NOT NULL DEFAULT '',
	content_hash TEXT NOT NULL DEFAULT '',
	file_name    TEXT NOT NULL DEFAULT '',
	size_bytes   INTEGER NOT NULL DEFAULT 0,
	reason       TEXT NOT NULL DEFAULT '',
	deleted_at   INTEGER NOT NULL
);
INSERT INTO deleted_videos (
	id, drive_id, file_id, content_hash, file_name, size_bytes, reason, deleted_at
) VALUES (
	'legacy-video', 'legacy-drive', 'legacy-file', '', 'legacy.mp4', 123, '', 1
);`); err != nil {
		_ = db.Close()
		t.Fatalf("seed legacy db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	cat, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	items, total, err := cat.ListDeletedVideos(context.Background(), ListParams{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("list migrated tombstone: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("migrated tombstones total/len = %d/%d, want 1/1", total, len(items))
	}
	if items[0].SourceDeleted ||
		items[0].CanonicalVideoID != "" ||
		items[0].RestorePolicy != DeletedVideoRestorePolicyScan {
		t.Fatalf("migrated tombstone = %#v", items[0])
	}
}

func TestSourceDeletedTombstonesArePurgedOnMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := db.Exec(`
CREATE TABLE deleted_videos (
	id                 TEXT PRIMARY KEY,
	drive_id           TEXT NOT NULL DEFAULT '',
	file_id            TEXT NOT NULL DEFAULT '',
	parent_id          TEXT NOT NULL DEFAULT '',
	content_hash       TEXT NOT NULL DEFAULT '',
	file_name          TEXT NOT NULL DEFAULT '',
	size_bytes         INTEGER NOT NULL DEFAULT 0,
	reason             TEXT NOT NULL DEFAULT '',
	source_deleted     INTEGER NOT NULL DEFAULT 0,
	canonical_video_id TEXT NOT NULL DEFAULT '',
	deleted_at         INTEGER NOT NULL
);
CREATE TABLE crawler_seen_sources (
	kind               TEXT NOT NULL,
	drive_id           TEXT NOT NULL,
	source_id          TEXT NOT NULL,
	status             TEXT NOT NULL DEFAULT 'imported',
	canonical_video_id TEXT NOT NULL DEFAULT '',
	sampled_sha256     TEXT NOT NULL DEFAULT '',
	size_bytes         INTEGER NOT NULL DEFAULT 0,
	first_seen_at      INTEGER NOT NULL,
	last_seen_at       INTEGER NOT NULL,
	PRIMARY KEY (kind, drive_id, source_id)
);
INSERT INTO deleted_videos (
	id, drive_id, file_id, parent_id, file_name, size_bytes, source_deleted, deleted_at
) VALUES (
	'scriptcrawler-crawler-a-source-1', 'crawler-a', 'file', 'parent', 'gone.mp4', 123, 1, 1
);
INSERT INTO crawler_seen_sources (
	kind, drive_id, source_id, status, canonical_video_id, sampled_sha256, size_bytes, first_seen_at, last_seen_at
) VALUES (
	'scriptcrawler', 'crawler-a', 'source-1', 'imported', 'scriptcrawler-crawler-a-source-1', 'sampled', 123, 1, 1
);`); err != nil {
		_ = db.Close()
		t.Fatalf("seed legacy source-deleted db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	cat, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	items, total, err := cat.ListDeletedVideos(context.Background(), ListParams{Page: 1, PageSize: 10, IncludeSourceDeleted: true})
	if err != nil {
		t.Fatalf("list migrated tombstones: %v", err)
	}
	if total != 0 || len(items) != 0 {
		t.Fatalf("source-deleted tombstones after migration = total %d items %#v, want none", total, items)
	}
	seen, err := cat.ListCrawlerSourceIDs(context.Background(), "scriptcrawler", "crawler-a")
	if err != nil {
		t.Fatalf("list migrated crawler source ids: %v", err)
	}
	if len(seen) != 1 || seen[0] != "source-1" {
		t.Fatalf("source-deleted crawler seen after migration = %#v, want source-1", seen)
	}
}

func TestDeletedVideoSourceDeletionQueue(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &Video{
		ID: "queued-video", DriveID: "drive", FileID: "file", ParentID: "parent",
		FileName: "clip.mp4", Title: "Clip", Size: 456,
		PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed queued video: %v", err)
	}
	if err := cat.DeleteVideoWithTombstone(ctx, "queued-video"); err != nil {
		t.Fatalf("tombstone queued video: %v", err)
	}

	count, err := cat.CountDeletedVideosPendingSourceDeletion(ctx)
	if err != nil || count != 1 {
		t.Fatalf("pending count = %d, err=%v, want 1", count, err)
	}
	items, err := cat.ListDeletedVideosPendingSourceDeletion(ctx)
	if err != nil {
		t.Fatalf("list pending source deletion: %v", err)
	}
	if len(items) != 1 ||
		items[0].ID != "queued-video" ||
		items[0].ParentID != "parent" ||
		items[0].FileID != "file" {
		t.Fatalf("pending source deletion items = %#v", items)
	}

	byIDs, err := cat.ListDeletedVideosPendingSourceDeletionByIDs(ctx, []string{"missing", "queued-video", "queued-video"})
	if err != nil {
		t.Fatalf("list pending source deletion by ids: %v", err)
	}
	if len(byIDs) != 1 || byIDs[0].ID != "queued-video" {
		t.Fatalf("pending source deletion by ids = %#v", byIDs)
	}

	if err := cat.PurgeDeletedVideo(ctx, "queued-video"); err != nil {
		t.Fatalf("purge deleted video: %v", err)
	}
	byIDs, err = cat.ListDeletedVideosPendingSourceDeletionByIDs(ctx, []string{"queued-video"})
	if err != nil {
		t.Fatalf("list pending source deletion by ids after purge: %v", err)
	}
	if len(byIDs) != 0 {
		t.Fatalf("pending source deletion by ids after purge = %#v", byIDs)
	}
	count, err = cat.CountDeletedVideosPendingSourceDeletion(ctx)
	if err != nil || count != 0 {
		t.Fatalf("pending count after purge = %d, err=%v, want 0", count, err)
	}
	deleted, _, err := cat.ListDeletedVideos(ctx, ListParams{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("list deleted videos: %v", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("purged tombstone should not be listed: %#v", deleted)
	}
	deleted, _, err = cat.ListDeletedVideos(ctx, ListParams{Page: 1, PageSize: 10, IncludeSourceDeleted: true})
	if err != nil {
		t.Fatalf("list deleted videos after purge: %v", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("purged tombstone remained = %#v", deleted)
	}
}

func TestPurgeDeletedVideoKeepsCrawlerSeenMetadata(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	if err := cat.UpsertDrive(ctx, &Drive{
		ID: "crawler-a", Kind: "scriptcrawler", Name: "Crawler", RootID: "/", TeaserEnabled: true,
	}); err != nil {
		t.Fatalf("seed crawler drive: %v", err)
	}

	now := time.Now()
	videoID := "scriptcrawler-crawler-a-source-1"
	if err := cat.UpsertVideo(ctx, &Video{
		ID: videoID, DriveID: "crawler-a", FileID: "source-1.mp4", ParentID: "parent",
		FileName: "source-1.mp4", Title: "Crawler", Size: 123,
		PublishedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed crawler video: %v", err)
	}
	if err := cat.MarkCrawlerSourceSeen(ctx, "scriptcrawler", "crawler-a", "source-1", "imported", videoID, "sampled", 123); err != nil {
		t.Fatalf("mark crawler source seen: %v", err)
	}
	if err := cat.DeleteVideoWithTombstone(ctx, videoID); err != nil {
		t.Fatalf("tombstone crawler video: %v", err)
	}

	if err := cat.PurgeDeletedVideo(ctx, videoID); err != nil {
		t.Fatalf("purge crawler tombstone: %v", err)
	}
	if deleted, err := cat.IsVideoDeleted(ctx, videoID); err != nil || deleted {
		t.Fatalf("crawler tombstone remained: deleted=%v err=%v", deleted, err)
	}
	seen, err := cat.ListCrawlerSourceIDs(ctx, "scriptcrawler", "crawler-a")
	if err != nil {
		t.Fatalf("list crawler source ids: %v", err)
	}
	if len(seen) != 1 || seen[0] != "source-1" {
		t.Fatalf("crawler seen metadata = %#v, want source-1", seen)
	}
}
