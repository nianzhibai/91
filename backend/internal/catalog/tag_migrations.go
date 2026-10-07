package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/video-site/backend/internal/tagging"
)

func (c *Catalog) migrate(ctx context.Context) error {
	if err := c.addColumnIfMissing(ctx, "scans", "result", "TEXT"); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_scans_drive_id ON scans(drive_id, id)`); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "videos", "tags_manual", "INTEGER DEFAULT 0"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "videos", "content_hash", "TEXT DEFAULT ''"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "videos", "sampled_sha256", "TEXT DEFAULT ''"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "videos", "fingerprint_status", "TEXT DEFAULT 'pending'"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "videos", "fingerprint_error", "TEXT DEFAULT ''"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "videos", "file_name", "TEXT DEFAULT ''"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "videos", "hidden", "INTEGER DEFAULT 0"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "videos", "is_canonical", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "videos", "thumbnail_status", "TEXT DEFAULT 'pending'"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "videos", "thumbnail_failures", "INTEGER DEFAULT 0"); err != nil {
		return err
	}
	if err := c.migrateLegacyDurationBackfill(ctx); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "videos", "thumbnail_updated_at", "INTEGER DEFAULT 0"); err != nil {
		return err
	}
	// Older databases only had videos.updated_at. Seed the thumbnail revision
	// once, then keep it independent from views, reactions, and other metadata.
	if _, err := c.db.ExecContext(ctx, `
UPDATE videos
   SET thumbnail_updated_at = updated_at
 WHERE COALESCE(thumbnail_url, '') != ''
   AND COALESCE(thumbnail_updated_at, 0) = 0
`); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "videos", "preview_updated_at", "INTEGER DEFAULT 0"); err != nil {
		return err
	}
	// Legacy previews were versioned with videos.updated_at. Seed a dedicated
	// revision once; future metadata-only writes leave it unchanged.
	if _, err := c.db.ExecContext(ctx, `
UPDATE videos
   SET preview_updated_at = updated_at
 WHERE COALESCE(preview_local, '') != ''
   AND COALESCE(preview_status, 'pending') = 'ready'
   AND COALESCE(preview_updated_at, 0) = 0
`); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "videos", "last_viewed_at", "INTEGER DEFAULT 0"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "videos", "last_liked_at", "INTEGER DEFAULT 0"); err != nil {
		return err
	}
	// 目录身份用于同目录合集。非常早期的库可能还没有 parent_id；先补身份列，
	// 再补仅用于展示和标签匹配的目录名。
	if err := c.addColumnIfMissing(ctx, "videos", "parent_id", "TEXT DEFAULT ''"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "videos", "ancestor_dir_ids", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "videos", "ancestor_dir_names", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// videos.dir_name：视频所在目录名，扫盘时落库；标签全库重算需要用它做匹配材料。
	if err := c.addColumnIfMissing(ctx, "videos", "dir_name", "TEXT DEFAULT ''"); err != nil {
		return err
	}
	// tags.match_rules：标签匹配规则 JSON；video_tags.evidence：命中证据。
	if err := c.addColumnIfMissing(ctx, "tags", "match_rules", "TEXT NOT NULL DEFAULT '{}'"); err != nil {
		return err
	}
	if err := c.dropColumnIfExists(ctx, "tags", "aliases"); err != nil {
		return err
	}
	if err := c.removeRetiredTagRuleFields(ctx); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "tags", "origin", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "video_tags", "evidence", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := c.dropTagTombstones(ctx); err != nil {
		return err
	}
	if err := c.dropColumnIfExists(ctx, "videos", "category"); err != nil {
		return err
	}
	if err := c.dropColumnIfExists(ctx, "videos", "llm_tagged_at"); err != nil {
		return err
	}
	// quality 曾被扫盘统一写成 HD，并不代表真实分辨率；完整退役该无效元数据。
	if err := c.dropColumnIfExists(ctx, "videos", "quality"); err != nil {
		return err
	}
	// 浏览器兼容性转码已整体退役；老库不再保留任务状态和产物引用。
	for _, column := range []string{"transcode_status", "transcode_error", "transcoded_file_id", "transcoded_size"} {
		if err := c.dropColumnIfExists(ctx, "videos", column); err != nil {
			return err
		}
	}
	if err := c.ensureBaseVideoIndexes(ctx); err != nil {
		return err
	}
	// Preview generation is now controlled exclusively by config.yaml.
	if err := c.dropColumnIfExists(ctx, "drives", "teaser_enabled"); err != nil {
		return err
	}
	if err := c.DeleteSettings(ctx, "preview.enabled", "drives.teaser_enabled.default_open_migrated"); err != nil {
		return err
	}
	// drives.skip_dir_ids：每盘扫描跳过目录集合（JSON array of string）。命中
	// 其中任意一个的目录及其全部子目录都不会被递归扫描。替代旧版硬编码"影视"
	// 目录例外分支；旧 drive 升级后默认空数组 → 行为等同于以前未启用跳过。
	if err := c.addColumnIfMissing(ctx, "drives", "skip_dir_ids", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "drives", "skip_cleanup_dir_ids", "TEXT"); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS deleted_videos (
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
	restore_requested  INTEGER NOT NULL DEFAULT 0,
	restore_payload    TEXT NOT NULL DEFAULT '',
	deleted_at         INTEGER NOT NULL
)`); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "deleted_videos", "reason", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "deleted_videos", "parent_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "deleted_videos", "source_deleted", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "deleted_videos", "canonical_video_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "deleted_videos", "restore_requested", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "deleted_videos", "restore_payload", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := c.purgeLegacySourceDeletedTombstones(ctx); err != nil {
		return err
	}
	if err := c.syncDriveScanRootIDToRootID(ctx); err != nil {
		return err
	}
	// 一次性修正：thumbnail_status 列是后加的（DEFAULT 'pending'），所有列加之前
	// 已有 thumbnail_url 的视频都被填成了 pending。worker 入队按 url 判定不会重复
	// 生成，但 status 字段对管理员/统计是误导（admin API 自己已经按 url 计数所以
	// 不受影响，但直接 SQL 查会以为有 N 千个待生成）。
	// 这里把"url 已写但 status 仍是 pending"的修正为 ready；status=failed 不动。
	if err := c.reconcileThumbnailStatusOnce(ctx); err != nil {
		return err
	}
	if err := c.requeueFailedThumbnailsWithReadyPreviewOnce(ctx); err != nil {
		return err
	}
	if err := c.requeueInactivePreviews(ctx); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_videos_content_hash ON videos(content_hash)`); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_videos_content_hash_created ON videos(content_hash, created_at, id)`); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_videos_sampled_sha256 ON videos(size_bytes, sampled_sha256)`); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_videos_sampled_sha256_created ON videos(size_bytes, sampled_sha256, created_at, id)`); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_videos_hidden ON videos(hidden)`); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_videos_last_viewed ON videos(last_viewed_at DESC)`); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_videos_hot ON videos(likes DESC, last_liked_at DESC, published_at DESC)`); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_videos_file_name_size ON videos(file_name, size_bytes)`); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_videos_file_name_size_created ON videos(file_name, size_bytes, created_at, id)`); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_videos_directory ON videos(drive_id, parent_id)`); err != nil {
		return err
	}
	if err := c.ensureCanonicalVideoMaterialization(ctx); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_deleted_videos_drive_file ON deleted_videos(drive_id, file_id)`); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_deleted_videos_drive_hash ON deleted_videos(drive_id, content_hash)`); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_deleted_videos_drive_signature ON deleted_videos(drive_id, file_name, size_bytes)`); err != nil {
		return err
	}
	if err := c.normalizeStoredTagSources(ctx); err != nil {
		return err
	}
	if err := c.backfillCrawlerTagOrigins(ctx); err != nil {
		return err
	}
	if err := c.normalizeRetiredVideoTagSources(ctx); err != nil {
		return err
	}
	if err := c.removeAutomaticTaggingArtifacts(ctx); err != nil {
		return err
	}
	if err := c.clearVolatileOneDriveThumbnails(ctx); err != nil {
		return err
	}
	if err := c.clearRemoteP123ThumbnailsOnce(ctx); err != nil {
		return err
	}
	if err := c.clearRemoteThumbnails(ctx); err != nil {
		return err
	}
	if err := c.hideZeroSizeVideosFromKnownDrives(ctx); err != nil {
		return err
	}
	if _, err := c.clearSyntheticCrawlerAuthorsOnce(ctx); err != nil {
		return err
	}
	if _, err := c.alignCrawlerPublishedAtWithCreatedAtOnce(ctx); err != nil {
		return err
	}
	// admin_sessions.user_id：关联到 users 表，用于区分管理员/普通用户 session
	if err := c.addColumnIfMissing(ctx, "admin_sessions", "user_id", "INTEGER DEFAULT 0"); err != nil {
		return err
	}
	// video_content_samples 是被废弃的内容采样实验遗留表，代码已无引用；
	// 现行内容级查重基于 teaser 帧签名（mediasim），不落库。
	if _, err := c.db.ExecContext(ctx, `DROP TABLE IF EXISTS video_content_samples`); err != nil {
		return err
	}
	return nil
}

func (c *Catalog) removeRetiredTagRuleFields(ctx context.Context) error {
	rows, err := c.db.QueryContext(ctx, `SELECT id, COALESCE(match_rules, '{}') FROM tags`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type update struct {
		id    int64
		rules string
	}
	var updates []update
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		if !strings.Contains(raw, `"words"`) && !strings.Contains(raw, `"excludes"`) {
			continue
		}
		var rule tagging.Rule
		_ = json.Unmarshal([]byte(raw), &rule)
		cleaned := cleanStoredTagRule(rule)
		rulesJSON, _ := json.Marshal(cleaned)
		updates = append(updates, update{id: id, rules: string(rulesJSON)})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(updates) == 0 {
		return nil
	}

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	for _, item := range updates {
		if _, err := tx.ExecContext(ctx,
			`UPDATE tags SET match_rules = ?, updated_at = ? WHERE id = ?`,
			item.rules, now, item.id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return c.bumpTagRulesVersion(ctx)
}

// normalizeStoredTagSources 将 Telegram 来源标签归为自动生成，旧内置标签归为自定义，
// 保留规则和关联。video_tags.source 独立记录 auto/manual/crawler/telegram 等关联来源。
func (c *Catalog) normalizeStoredTagSources(ctx context.Context) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
UPDATE tags
   SET source = CASE
       WHEN lower(trim(COALESCE(origin, ''))) = 'telegram' THEN 'generated'
       WHEN lower(trim(COALESCE(source, ''))) IN ('system', 'builtin', 'user') THEN 'user'
       ELSE 'generated'
   END
 WHERE source IS NULL
    OR source != CASE
       WHEN lower(trim(COALESCE(origin, ''))) = 'telegram' THEN 'generated'
       WHEN lower(trim(COALESCE(source, ''))) IN ('system', 'builtin', 'user') THEN 'user'
       ELSE 'generated'
   END`)
	if err != nil {
		return fmt.Errorf("normalize tag sources: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key IN ('tags.builtin_pack_enabled', 'tags.builtin_pack_initialized_v1', 'tags.av_code_matching_disabled')`); err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		if err := bumpTagRulesVersionTx(ctx, tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (c *Catalog) dropTagTombstones(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx, `DROP TABLE IF EXISTS deleted_tags`)
	return err
}

func (c *Catalog) backfillCrawlerTagOrigins(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx, `
UPDATE tags
   SET origin = 'crawler'
 WHERE COALESCE(origin, '') != 'crawler'
   AND (
       EXISTS (
         SELECT 1
           FROM video_tags vt
          WHERE vt.tag_id = tags.id
            AND lower(trim(COALESCE(vt.source, ''))) = 'crawler'
       )
       OR EXISTS (
         SELECT 1
           FROM drives d
          WHERE d.kind = 'scriptcrawler'
            AND d.name = tags.label COLLATE NOCASE
       )
   )`)
	return err
}

func (c *Catalog) normalizeRetiredVideoTagSources(ctx context.Context) error {
	rows, err := c.db.QueryContext(ctx, `
SELECT DISTINCT video_id
  FROM video_tags
 WHERE lower(trim(COALESCE(source, ''))) = 'llm'`)
	if err != nil {
		return err
	}
	var videoIDs []string
	for rows.Next() {
		var videoID string
		if err := rows.Scan(&videoID); err != nil {
			rows.Close()
			return err
		}
		videoIDs = append(videoIDs, videoID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(videoIDs) == 0 {
		return nil
	}
	if _, err := c.db.ExecContext(ctx, `
DELETE FROM video_tags
 WHERE lower(trim(COALESCE(source, ''))) = 'llm'`); err != nil {
		return err
	}
	for _, videoID := range videoIDs {
		if err := c.syncVideoTagsJSON(ctx, videoID, c.hasManualTags(ctx, videoID)); err != nil {
			return err
		}
	}
	return nil
}

func (c *Catalog) purgeLegacySourceDeletedTombstones(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM deleted_videos WHERE COALESCE(source_deleted, 0) = 1`)
	return err
}

func (c *Catalog) addColumnIfMissing(ctx context.Context, table, column, definition string) error {
	_, err := c.addColumnIfMissingReportNew(ctx, table, column, definition)
	return err
}

func (c *Catalog) dropColumnIfExists(ctx context.Context, table, column string) error {
	rows, err := c.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if strings.EqualFold(name, column) {
			found = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !found {
		return nil
	}
	if _, err = c.db.ExecContext(ctx, `ALTER TABLE `+table+` DROP COLUMN `+column); err == nil {
		return nil
	}
	if table == "videos" && isRetiredVideoColumn(column) {
		log.Printf("[catalog] native drop column videos.%s failed, rebuilding videos table with current columns: %v", column, err)
		return c.rebuildVideosTableWithCurrentColumns(ctx)
	}
	return err
}

func isRetiredVideoColumn(column string) bool {
	for _, retired := range []string{
		"category",
		"llm_tagged_at",
		"quality",
		"transcode_status",
		"transcode_error",
		"transcoded_file_id",
		"transcoded_size",
	} {
		if strings.EqualFold(column, retired) {
			return true
		}
	}
	return false
}

func (c *Catalog) ensureBaseVideoIndexes(ctx context.Context) error {
	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS idx_videos_drive ON videos(drive_id, file_id)`,
		`CREATE INDEX IF NOT EXISTS idx_videos_pub ON videos(published_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_videos_created ON videos(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_videos_duration ON videos(duration_seconds)`,
		`CREATE INDEX IF NOT EXISTS idx_videos_views ON videos(views DESC)`,
	} {
		if _, err := c.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

const canonicalMaterializationMarker = "videos.is_canonical.v1"

const dedupRepresentativesMarker = "videos.dedup_representatives.v1"

const canonicalRowsMatchingNewVideoSQL = `(videos.id = NEW.id
	OR (COALESCE(NEW.content_hash, '') != ''
		AND videos.content_hash = NEW.content_hash)
	OR (NEW.size_bytes > 0
		AND COALESCE(NEW.sampled_sha256, '') != ''
		AND videos.size_bytes = NEW.size_bytes
		AND videos.sampled_sha256 = NEW.sampled_sha256)
	OR (NEW.size_bytes > 0
		AND COALESCE(NEW.file_name, '') != ''
		AND videos.size_bytes = NEW.size_bytes
		AND videos.file_name = NEW.file_name))`

const canonicalRowsMatchingOldVideoSQL = `(videos.id = OLD.id
	OR (COALESCE(OLD.content_hash, '') != ''
		AND videos.content_hash = OLD.content_hash)
	OR (OLD.size_bytes > 0
		AND COALESCE(OLD.sampled_sha256, '') != ''
		AND videos.size_bytes = OLD.size_bytes
		AND videos.sampled_sha256 = OLD.sampled_sha256)
	OR (OLD.size_bytes > 0
		AND COALESCE(OLD.file_name, '') != ''
		AND videos.size_bytes = OLD.size_bytes
		AND videos.file_name = OLD.file_name))`

// ensureCanonicalVideoMaterialization installs row-local maintenance triggers
// and performs a one-time full backfill. The trigger UPDATE runs inside the
// originating SQLite statement, so every Catalog writer, direct INSERT, bulk
// DELETE and restored database keeps the derived flag transactionally aligned.
func (c *Catalog) ensureCanonicalVideoMaterialization(ctx context.Context) error {
	if _, err := c.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS video_dedup_representatives (
	video_id          TEXT NOT NULL,
	basis             TEXT NOT NULL CHECK (basis IN ('self', 'content_hash', 'sampled_sha256', 'file_name_size')),
	representative_id TEXT NOT NULL,
	PRIMARY KEY (video_id, basis)
);
CREATE INDEX IF NOT EXISTS idx_video_dedup_representative
	ON video_dedup_representatives(representative_id, video_id);
`); err != nil {
		return fmt.Errorf("create dedup representative table: %w", err)
	}

	canonicalValueSQL := `CASE WHEN ` + dynamicUniqueVideoWhereSQL + ` THEN 1 ELSE 0 END`
	insertRepresentativeRefreshSQL := refreshInsertedVideoDedupRepresentativesSQL()
	updateRepresentativeRefreshSQL := refreshUpdatedVideoDedupRepresentativesSQL()
	deleteRepresentativeRefreshSQL := refreshDeletedVideoDedupRepresentativesSQL()
	triggerSQL := `
DROP TRIGGER IF EXISTS maintain_video_canonical_after_insert;
DROP TRIGGER IF EXISTS maintain_video_canonical_after_update;
DROP TRIGGER IF EXISTS maintain_video_canonical_after_delete;
DROP TRIGGER IF EXISTS maintain_video_representatives_after_insert;
DROP TRIGGER IF EXISTS maintain_video_representatives_after_update;
DROP TRIGGER IF EXISTS maintain_video_representatives_after_delete;

CREATE TRIGGER maintain_video_canonical_after_insert
AFTER INSERT ON videos
BEGIN
	UPDATE videos
	   SET is_canonical = ` + canonicalValueSQL + `
	 WHERE ` + canonicalRowsMatchingNewVideoSQL + `
	   AND is_canonical != ` + canonicalValueSQL + `;
END;

CREATE TRIGGER maintain_video_canonical_after_update
AFTER UPDATE OF id, content_hash, sampled_sha256, size_bytes, file_name, created_at ON videos
WHEN OLD.id IS NOT NEW.id
	OR COALESCE(OLD.content_hash, '') IS NOT COALESCE(NEW.content_hash, '')
	OR COALESCE(OLD.sampled_sha256, '') IS NOT COALESCE(NEW.sampled_sha256, '')
	OR COALESCE(OLD.size_bytes, 0) IS NOT COALESCE(NEW.size_bytes, 0)
	OR COALESCE(OLD.file_name, '') IS NOT COALESCE(NEW.file_name, '')
	OR OLD.created_at IS NOT NEW.created_at
BEGIN
	UPDATE videos
	   SET is_canonical = ` + canonicalValueSQL + `
	 WHERE (` + canonicalRowsMatchingOldVideoSQL + ` OR ` + canonicalRowsMatchingNewVideoSQL + `)
	   AND is_canonical != ` + canonicalValueSQL + `;
END;

CREATE TRIGGER maintain_video_canonical_after_delete
AFTER DELETE ON videos
BEGIN
	UPDATE videos
	   SET is_canonical = ` + canonicalValueSQL + `
	 WHERE ` + canonicalRowsMatchingOldVideoSQL + `
	   AND is_canonical != ` + canonicalValueSQL + `;
END;

CREATE TRIGGER maintain_video_representatives_after_insert
AFTER INSERT ON videos
BEGIN
` + insertRepresentativeRefreshSQL + `
END;

CREATE TRIGGER maintain_video_representatives_after_update
AFTER UPDATE OF id, content_hash, sampled_sha256, size_bytes, file_name, created_at ON videos
WHEN OLD.id IS NOT NEW.id
	OR COALESCE(OLD.content_hash, '') IS NOT COALESCE(NEW.content_hash, '')
	OR COALESCE(OLD.sampled_sha256, '') IS NOT COALESCE(NEW.sampled_sha256, '')
	OR COALESCE(OLD.size_bytes, 0) IS NOT COALESCE(NEW.size_bytes, 0)
	OR COALESCE(OLD.file_name, '') IS NOT COALESCE(NEW.file_name, '')
	OR OLD.created_at IS NOT NEW.created_at
BEGIN
` + updateRepresentativeRefreshSQL + `
END;

CREATE TRIGGER maintain_video_representatives_after_delete
AFTER DELETE ON videos
BEGIN
` + deleteRepresentativeRefreshSQL + `
END;
`
	if _, err := c.db.ExecContext(ctx, triggerSQL); err != nil {
		return fmt.Errorf("install canonical video triggers: %w", err)
	}

	marker, err := c.GetSetting(ctx, canonicalMaterializationMarker, "")
	if err != nil {
		return fmt.Errorf("read canonical materialization marker: %w", err)
	}
	if strings.TrimSpace(marker) != "1" {
		if _, err := c.db.ExecContext(ctx, `
UPDATE videos
	SET is_canonical = `+canonicalValueSQL+`
	WHERE is_canonical != `+canonicalValueSQL); err != nil {
			return fmt.Errorf("backfill canonical videos: %w", err)
		}
		var inconsistent int
		if err := c.db.QueryRowContext(ctx, `
SELECT COUNT(*)
	FROM videos
	WHERE is_canonical != `+canonicalValueSQL).Scan(&inconsistent); err != nil {
			return fmt.Errorf("verify canonical video backfill: %w", err)
		}
		if inconsistent != 0 {
			return fmt.Errorf("verify canonical video backfill: %d inconsistent rows", inconsistent)
		}
		if err := c.SetSetting(ctx, canonicalMaterializationMarker, "1"); err != nil {
			return fmt.Errorf("write canonical materialization marker: %w", err)
		}
	}

	representativeMarker, err := c.GetSetting(ctx, dedupRepresentativesMarker, "")
	if err != nil {
		return fmt.Errorf("read dedup representative marker: %w", err)
	}
	if strings.TrimSpace(representativeMarker) != "1" {
		tx, err := c.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin dedup representative backfill: %w", err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, rebuildVideoDedupRepresentativesSQL("1 = 1", "")); err != nil {
			return fmt.Errorf("backfill dedup representatives: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO settings (key, value, updated_at) VALUES (?, '1', ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
`, dedupRepresentativesMarker, time.Now().UnixMilli()); err != nil {
			return fmt.Errorf("write dedup representative marker: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit dedup representative backfill: %w", err)
		}
	}
	// This legacy index makes SQLite prefer a full visible-row scan followed by
	// a temporary sort over the canonical expression indexes below.
	if _, err := c.db.ExecContext(ctx, `
DROP INDEX IF EXISTS idx_videos_visible_pub;
DROP INDEX IF EXISTS idx_videos_canonical_hot_ready;
DROP INDEX IF EXISTS idx_videos_canonical_recent_ready;
`); err != nil {
		return fmt.Errorf("drop superseded visible video index: %w", err)
	}

	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS idx_videos_canonical_id
			ON videos(id)
			WHERE is_canonical = 1 AND COALESCE(hidden, 0) = 0`,
		`CREATE INDEX IF NOT EXISTS idx_videos_canonical_latest
			ON videos(published_at DESC, id ASC)
			WHERE is_canonical = 1 AND COALESCE(hidden, 0) = 0`,
		`CREATE INDEX IF NOT EXISTS idx_videos_canonical_latest_ready
			ON videos(
				CASE WHEN COALESCE(thumbnail_url, '') != '' THEN 0 ELSE 1 END,
				published_at DESC,
				id ASC
			)
			WHERE is_canonical = 1 AND COALESCE(hidden, 0) = 0`,
	} {
		if _, err := c.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("create canonical video index: %w", err)
		}
	}
	return nil
}

func rebuildVideoDedupRepresentativesSQL(scopeSQL, removedVideoIDSQL string) string {
	deleteWhere := `video_id IN (SELECT videos.id FROM videos WHERE ` + scopeSQL + `)`
	if strings.TrimSpace(removedVideoIDSQL) != "" {
		deleteWhere = `video_id = ` + removedVideoIDSQL + ` OR ` + deleteWhere
	}
	return `
	DELETE FROM video_dedup_representatives
	 WHERE ` + deleteWhere + `;

	INSERT OR REPLACE INTO video_dedup_representatives (video_id, basis, representative_id)
	SELECT videos.id, 'self', videos.id
	  FROM videos
	 WHERE ` + scopeSQL + `;

	INSERT OR REPLACE INTO video_dedup_representatives (video_id, basis, representative_id)
	SELECT videos.id,
	       'content_hash',
	       (SELECT canonical.id
	          FROM videos canonical
	         WHERE canonical.content_hash = videos.content_hash
	           AND COALESCE(canonical.content_hash, '') != ''
	         ORDER BY canonical.created_at ASC, canonical.id ASC
	         LIMIT 1)
	  FROM videos
	 WHERE ` + scopeSQL + `
	   AND COALESCE(videos.content_hash, '') != '';

	INSERT OR REPLACE INTO video_dedup_representatives (video_id, basis, representative_id)
	SELECT videos.id,
	       'sampled_sha256',
	       (SELECT canonical.id
	          FROM videos canonical
	         WHERE canonical.sampled_sha256 = videos.sampled_sha256
	           AND canonical.size_bytes = videos.size_bytes
	           AND COALESCE(canonical.sampled_sha256, '') != ''
	           AND canonical.size_bytes > 0
	         ORDER BY canonical.created_at ASC, canonical.id ASC
	         LIMIT 1)
	  FROM videos
	 WHERE ` + scopeSQL + `
	   AND COALESCE(videos.sampled_sha256, '') != ''
	   AND videos.size_bytes > 0;

	INSERT OR REPLACE INTO video_dedup_representatives (video_id, basis, representative_id)
	SELECT videos.id,
	       'file_name_size',
	       (SELECT canonical.id
	          FROM videos canonical
	         WHERE canonical.file_name = videos.file_name
	           AND canonical.size_bytes = videos.size_bytes
	           AND COALESCE(canonical.file_name, '') != ''
	           AND canonical.size_bytes > 0
	         ORDER BY canonical.created_at ASC, canonical.id ASC
	         LIMIT 1)
	  FROM videos
	 WHERE ` + scopeSQL + `
	   AND COALESCE(videos.file_name, '') != ''
	   AND videos.size_bytes > 0;
`
}

func refreshInsertedVideoDedupRepresentativesSQL() string {
	return `
	DELETE FROM video_dedup_representatives WHERE video_id = NEW.id;
	INSERT OR REPLACE INTO video_dedup_representatives (video_id, basis, representative_id)
	VALUES (NEW.id, 'self', NEW.id);
` + refreshVideoDedupRepresentativeGroupsSQL("", "NEW")
}

func refreshUpdatedVideoDedupRepresentativesSQL() string {
	return `
	DELETE FROM video_dedup_representatives WHERE video_id = OLD.id OR video_id = NEW.id;
	INSERT OR REPLACE INTO video_dedup_representatives (video_id, basis, representative_id)
	VALUES (NEW.id, 'self', NEW.id);
` + refreshVideoDedupRepresentativeGroupsSQL("OLD", "NEW")
}

func refreshDeletedVideoDedupRepresentativesSQL() string {
	return `
	DELETE FROM video_dedup_representatives WHERE video_id = OLD.id;
` + refreshVideoDedupRepresentativeGroupsSQL("OLD", "")
}

func refreshVideoDedupRepresentativeGroupsSQL(oldRef, newRef string) string {
	hashScope := dedupRepresentativeHashScopeSQL(oldRef, newRef)
	sampleScope := dedupRepresentativeSampleScopeSQL(oldRef, newRef)
	fileScope := dedupRepresentativeFileScopeSQL(oldRef, newRef)
	return `
	INSERT OR REPLACE INTO video_dedup_representatives (video_id, basis, representative_id)
	SELECT videos.id,
	       'content_hash',
	       (SELECT canonical.id
	          FROM videos canonical
	         WHERE canonical.content_hash = videos.content_hash
	           AND COALESCE(canonical.content_hash, '') != ''
	         ORDER BY canonical.created_at ASC, canonical.id ASC
	         LIMIT 1)
	  FROM videos
	 WHERE ` + hashScope + `;

	INSERT OR REPLACE INTO video_dedup_representatives (video_id, basis, representative_id)
	SELECT videos.id,
	       'sampled_sha256',
	       (SELECT canonical.id
	          FROM videos canonical
	         WHERE canonical.sampled_sha256 = videos.sampled_sha256
	           AND canonical.size_bytes = videos.size_bytes
	           AND COALESCE(canonical.sampled_sha256, '') != ''
	           AND canonical.size_bytes > 0
	         ORDER BY canonical.created_at ASC, canonical.id ASC
	         LIMIT 1)
	  FROM videos
	 WHERE ` + sampleScope + `;

	INSERT OR REPLACE INTO video_dedup_representatives (video_id, basis, representative_id)
	SELECT videos.id,
	       'file_name_size',
	       (SELECT canonical.id
	          FROM videos canonical
	         WHERE canonical.file_name = videos.file_name
	           AND canonical.size_bytes = videos.size_bytes
	           AND COALESCE(canonical.file_name, '') != ''
	           AND canonical.size_bytes > 0
	         ORDER BY canonical.created_at ASC, canonical.id ASC
	         LIMIT 1)
	  FROM videos
	 WHERE ` + fileScope + `;
`
}

func dedupRepresentativeHashScopeSQL(refs ...string) string {
	conditions := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		conditions = append(conditions, `(COALESCE(`+ref+`.content_hash, '') != ''
		AND videos.content_hash = `+ref+`.content_hash)`)
	}
	return strings.Join(conditions, " OR ")
}

func dedupRepresentativeSampleScopeSQL(refs ...string) string {
	conditions := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		conditions = append(conditions, `(`+ref+`.size_bytes > 0
		AND COALESCE(`+ref+`.sampled_sha256, '') != ''
		AND videos.size_bytes = `+ref+`.size_bytes
		AND videos.sampled_sha256 = `+ref+`.sampled_sha256)`)
	}
	return strings.Join(conditions, " OR ")
}

func dedupRepresentativeFileScopeSQL(refs ...string) string {
	conditions := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		conditions = append(conditions, `(`+ref+`.size_bytes > 0
		AND COALESCE(`+ref+`.file_name, '') != ''
		AND videos.size_bytes = `+ref+`.size_bytes
		AND videos.file_name = `+ref+`.file_name)`)
	}
	return strings.Join(conditions, " OR ")
}

var currentVideoColumnNames = []string{
	"id",
	"drive_id",
	"file_id",
	"file_name",
	"content_hash",
	"sampled_sha256",
	"fingerprint_status",
	"fingerprint_error",
	"parent_id",
	"ancestor_dir_ids",
	"ancestor_dir_names",
	"dir_name",
	"title",
	"author",
	"tags",
	"duration_seconds",
	"size_bytes",
	"ext",
	"thumbnail_url",
	"thumbnail_updated_at",
	"thumbnail_status",
	"thumbnail_failures",
	"preview_file_id",
	"preview_local",
	"preview_updated_at",
	"preview_status",
	"views",
	"last_viewed_at",
	"favorites",
	"comments",
	"likes",
	"last_liked_at",
	"dislikes",
	"hidden",
	"is_canonical",
	"tags_manual",
	"badges",
	"description",
	"published_at",
	"created_at",
	"updated_at",
}

const createCurrentVideosTableSQL = `
CREATE TABLE videos_schema_rebuild_new (
    id                 TEXT PRIMARY KEY,
    drive_id           TEXT NOT NULL,
    file_id            TEXT NOT NULL,
    file_name          TEXT DEFAULT '',
    content_hash       TEXT DEFAULT '',
    sampled_sha256     TEXT DEFAULT '',
    fingerprint_status TEXT DEFAULT 'pending',
    fingerprint_error  TEXT DEFAULT '',
    parent_id          TEXT,
    ancestor_dir_ids   TEXT NOT NULL DEFAULT '',
    ancestor_dir_names TEXT NOT NULL DEFAULT '',
    dir_name           TEXT DEFAULT '',
    title              TEXT NOT NULL,
    author             TEXT,
    tags               TEXT,
    duration_seconds   INTEGER DEFAULT 0,
    size_bytes         INTEGER DEFAULT 0,
    ext                TEXT,
    thumbnail_url      TEXT,
	thumbnail_updated_at INTEGER DEFAULT 0,
    thumbnail_status   TEXT DEFAULT 'pending',
    thumbnail_failures INTEGER DEFAULT 0,
    preview_file_id    TEXT,
    preview_local      TEXT,
    preview_updated_at INTEGER DEFAULT 0,
    preview_status     TEXT DEFAULT 'pending',
    views              INTEGER DEFAULT 0,
    last_viewed_at     INTEGER DEFAULT 0,
    favorites          INTEGER DEFAULT 0,
    comments           INTEGER DEFAULT 0,
    likes              INTEGER DEFAULT 0,
    last_liked_at      INTEGER DEFAULT 0,
    dislikes           INTEGER DEFAULT 0,
    hidden             INTEGER DEFAULT 0,
    is_canonical       INTEGER NOT NULL DEFAULT 1,
    tags_manual        INTEGER DEFAULT 0,
    badges             TEXT,
    description        TEXT,
    published_at       INTEGER NOT NULL,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL
)`

func (c *Catalog) rebuildVideosTableWithCurrentColumns(ctx context.Context) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS videos_schema_rebuild_new`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, createCurrentVideosTableSQL); err != nil {
		return err
	}
	cols := strings.Join(currentVideoColumnNames, ", ")
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO videos_schema_rebuild_new (`+cols+`) SELECT `+cols+` FROM videos`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE videos`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE videos_schema_rebuild_new RENAME TO videos`); err != nil {
		return err
	}
	return tx.Commit()
}

// addColumnIfMissingReportNew 与 addColumnIfMissing 同步，但额外返回 added=true 表示
// 本次确实创建了新列（即旧 schema 缺这列），方便调用方仅在迁移路径里补做一次性
// 数据初始化（如把全局 setting 同步到新 per-drive 字段）。
//
// 已存在该列时返回 added=false，任何 ALTER TABLE 错误也直接透传。
func (c *Catalog) addColumnIfMissingReportNew(ctx context.Context, table, column, definition string) (bool, error) {
	rows, err := c.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if strings.EqualFold(name, column) {
			return false, nil
		}
	}
	if _, err := c.db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column+` `+definition); err != nil {
		return false, err
	}
	return true, nil
}

// reconcileThumbnailStatusOnce 把所有"封面 URL 已写但 thumbnail_status 仍停留在
// 'pending'"的视频行修正为 'ready'。仅在历史上没跑过这条迁移时执行（marker 守护）。
//
// 为什么需要：thumbnail_status 列是历史某次加进 schema 的（addColumnIfMissing
// 在 tags.go:51，DEFAULT 'pending'）。列加入时所有已存在的视频 thumbnail_url
// 已经填好（指向本地 /p/thumb/<id>），但 status 列 ALTER 时按 DEFAULT 全部填了
// 'pending'。worker 入队按 url 判定（不看 status）所以行为正确，但：
//   - 直接 SQL 查 thumbnail_status='pending' 会以为有几千条待生成
//   - 管理员凭直觉认知字段名时会被误导
//
// 修正策略：
//   - thumbnail_url 非空 + status 非 'ready' + status 非 'failed' + status 非 'skipped' → 改成 'ready'
//   - status='failed' 不动（这是 worker 显式标的失败，要保留以便管理员手动重生）
//   - status='skipped' 不动（已有封面但时长探测不可用，避免重启后重复排队）
//
// 幂等保证：marker setting 写过就不再跑，避免每次重启都 update 一遍。
func (c *Catalog) reconcileThumbnailStatusOnce(ctx context.Context) error {
	const markerKey = "videos.thumbnail_status.url_present_to_ready_migrated"
	marker, err := c.GetSetting(ctx, markerKey, "")
	if err != nil {
		return fmt.Errorf("read %s marker: %w", markerKey, err)
	}
	if strings.TrimSpace(marker) == "1" {
		return nil
	}
	res, err := c.db.ExecContext(ctx, `
UPDATE videos
   SET thumbnail_status = 'ready',
       updated_at = ?
 WHERE COALESCE(thumbnail_url, '') != ''
   AND COALESCE(thumbnail_status, 'pending') NOT IN ('ready', 'failed', 'skipped')
`, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("reconcile thumbnail_status: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected > 0 {
		log.Printf("[catalog] reconciled %d video(s) thumbnail_status pending→ready (url already written)", affected)
	}
	if err := c.SetSetting(ctx, markerKey, "1"); err != nil {
		return fmt.Errorf("write %s marker: %w", markerKey, err)
	}
	return nil
}

// requeueFailedThumbnailsWithReadyPreviewOnce repairs rows created before
// preview completion became a thumbnail retry signal. Future rows are handled
// transactionally by UpdatePreview; the marker prevents a permanently bad
// source video from being retried on every process restart.
func (c *Catalog) requeueFailedThumbnailsWithReadyPreviewOnce(ctx context.Context) error {
	const markerKey = "videos.failed_thumbnail.ready_preview_requeued_v2"
	marker, err := c.GetSetting(ctx, markerKey, "")
	if err != nil {
		return fmt.Errorf("read %s marker: %w", markerKey, err)
	}
	if strings.TrimSpace(marker) == "1" {
		return nil
	}
	result, err := c.db.ExecContext(ctx, `
UPDATE videos
   SET thumbnail_status = 'pending',
       thumbnail_failures = 0,
       updated_at = ?
 WHERE COALESCE(thumbnail_url, '') = ''
   AND COALESCE(thumbnail_status, 'pending') = 'failed'
   AND COALESCE(preview_status, 'pending') = 'ready'
   AND TRIM(COALESCE(preview_local, '')) != ''
`, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("requeue failed thumbnails with ready preview: %w", err)
	}
	if affected, rowsErr := result.RowsAffected(); rowsErr == nil && affected > 0 {
		log.Printf("[catalog] requeued %d failed thumbnail(s) with a ready local preview", affected)
	}
	if err := c.SetSetting(ctx, markerKey, "1"); err != nil {
		return fmt.Errorf("write %s marker: %w", markerKey, err)
	}
	return nil
}

func (c *Catalog) requeueInactivePreviews(ctx context.Context) error {
	res, err := c.db.ExecContext(ctx, `
UPDATE videos
   SET preview_file_id = '',
       preview_local = '',
	   preview_updated_at = 0,
       preview_status = 'pending',
       updated_at = ?
 WHERE COALESCE(preview_status, 'pending') IN ('skipped', 'disabled')
`, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("requeue inactive previews: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected > 0 {
		log.Printf("[catalog] requeued %d inactive preview(s) for generation", affected)
	}
	return nil
}

func (c *Catalog) clearVolatileOneDriveThumbnails(ctx context.Context) error {
	// 把 OneDrive 过期的 mediap.svc.ms thumb URL 清空，让 worker 重新抽帧生成本地封面。
	// 同步把 thumbnail_status 重置为 'pending'：清空后 url 是空的，本应进 worker 重做，
	// 若 status 还停留在 'ready' / 'failed' 会和 ListVideosNeedingThumbnail 的语义不一致
	// （admin/统计按 url 看：空 + 非 'failed' = pending；status='failed' 会让重做被阻断）。
	_, err := c.db.ExecContext(ctx, `
UPDATE videos
   SET thumbnail_url = '',
	   thumbnail_updated_at = 0,
       thumbnail_status = 'pending',
       updated_at = ?
 WHERE lower(COALESCE(thumbnail_url, '')) LIKE 'https://%mediap.svc.ms/transform/thumbnail%'
`, time.Now().UnixMilli())
	return err
}

func (c *Catalog) clearRemoteP123ThumbnailsOnce(ctx context.Context) error {
	// 123网盘列表返回的缩略图尺寸和稳定性都不适合作为站内封面；清空历史写入的
	// 远程 URL，让封面 worker 统一从视频直链抽帧生成本地 /p/thumb/<id>。
	const markerKey = "videos.p123.remote_thumbnails_cleared"
	marker, err := c.GetSetting(ctx, markerKey, "")
	if err != nil {
		return fmt.Errorf("read %s marker: %w", markerKey, err)
	}
	if strings.TrimSpace(marker) == "1" {
		return nil
	}

	var p123Drives int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM drives WHERE kind = 'p123'`).Scan(&p123Drives); err != nil {
		return fmt.Errorf("count p123 drives: %w", err)
	}
	if p123Drives == 0 {
		return nil
	}

	res, err := c.db.ExecContext(ctx, `
	UPDATE videos
	   SET thumbnail_url = '',
	       thumbnail_updated_at = 0,
	       thumbnail_status = 'pending',
	       thumbnail_failures = 0,
	       updated_at = ?
	 WHERE EXISTS (
	       SELECT 1
	         FROM drives
	        WHERE drives.id = videos.drive_id
	          AND drives.kind = 'p123'
	   )
	   AND (
	       lower(COALESCE(thumbnail_url, '')) LIKE 'http://%'
	       OR lower(COALESCE(thumbnail_url, '')) LIKE 'https://%'
	   )
	`, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err == nil && affected > 0 {
		log.Printf("[catalog] cleared %d remote 123pan thumbnail(s) for local regeneration", affected)
	}
	if err := c.SetSetting(ctx, markerKey, "1"); err != nil {
		return fmt.Errorf("write %s marker: %w", markerKey, err)
	}
	return nil
}

func (c *Catalog) clearRemoteThumbnails(ctx context.Context) error {
	// 不再使用网盘侧返回的远程缩略图。清空历史 http/https thumbnail_url 后，
	// 封面 worker 会重新从视频中间帧生成本地 /p/thumb/<id>。
	res, err := c.db.ExecContext(ctx, `
UPDATE videos
   SET thumbnail_url = '',
	   thumbnail_updated_at = 0,
       thumbnail_status = 'pending',
       thumbnail_failures = 0,
       updated_at = ?
 WHERE (
       lower(COALESCE(thumbnail_url, '')) LIKE 'http://%'
       OR lower(COALESCE(thumbnail_url, '')) LIKE 'https://%'
   )
`, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err == nil && affected > 0 {
		log.Printf("[catalog] cleared %d remote thumbnail(s) for local regeneration", affected)
	}
	return nil
}

func (c *Catalog) hideZeroSizeVideosFromKnownDrives(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx, `
UPDATE videos
   SET hidden = 1,
       updated_at = ?
 WHERE COALESCE(size_bytes, 0) <= 0
   AND COALESCE(hidden, 0) = 0
   AND EXISTS (
	 SELECT 1
	   FROM drives
	  WHERE drives.id = videos.drive_id
   )
`, time.Now().UnixMilli())
	return err
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}
