package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/video-site/backend/internal/fixedtags"
)

var ErrUnknownTag = errors.New("unknown tag")
var ErrSystemTag = errors.New("system tag cannot be deleted")
var ErrDeletedTag = errors.New("tag was previously deleted")

const avTagLabel = "AV"

var (
	avCodePattern              = regexp.MustCompile(`(?i)^[A-Z]{2,8}[-_ ]?\d{3,6}(?:[-_ ]?[A-Z0-9]{1,4})?$`)
	ccAVCodePattern            = regexp.MustCompile(`(?i)^CC[-_ ]?\d{3,8}(?:[-_ ]?[A-Z0-9]{1,4})?$`)
	fc2AVCodePattern           = regexp.MustCompile(`(?i)^FC2[-_ ]?(?:PPV[-_ ]?)?\d{4,8}(?:[-_ ]?[A-Z0-9]{1,4})?$`)
	numericPrefixAVCodePattern = regexp.MustCompile(`(?i)^\d{2,4}[A-Z]{2,8}[-_ ]?\d{3,6}(?:[-_ ]?[A-Z0-9]{1,4})?$`)
	avCodeInTextPattern        = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])((?:[A-Z]{2,8}[-_ ]?\d{3,6}(?:[-_ ]?[A-Z0-9]{1,4})?)|(?:CC[-_ ]?\d{3,8}(?:[-_ ]?[A-Z0-9]{1,4})?)|(?:FC2[-_ ]?(?:PPV[-_ ]?)?\d{4,8}(?:[-_ ]?[A-Z0-9]{1,4})?)|(?:\d{2,4}[A-Z]{2,8}[-_ ]?\d{3,6}(?:[-_ ]?[A-Z0-9]{1,4})?))(?:$|[^A-Za-z0-9])`)
)

type Tag struct {
	ID             int64    `json:"id"`
	Label          string   `json:"label"`
	Aliases        []string `json:"aliases,omitempty"`
	Source         string   `json:"source"`
	FrontendAccess bool     `json:"frontendAccess"`
	Count          int      `json:"count"`
}

func (c *Catalog) migrate(ctx context.Context) error {
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
	if err := c.addColumnIfMissing(ctx, "videos", "frontend_selected", "INTEGER DEFAULT 0"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "tags", "frontend_access", "INTEGER DEFAULT 0"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "videos", "thumbnail_status", "TEXT DEFAULT 'pending'"); err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "videos", "thumbnail_failures", "INTEGER DEFAULT 0"); err != nil {
		return err
	}
	// drives.teaser_enabled：每盘预览视频开关，替代旧的全局 preview.enabled。
	// 升级路径：直接让 ALTER TABLE 的 DEFAULT 1 兜底 —— 每个现存 drive 都默认开启，
	// 不读旧的 settings.preview.enabled 字段。这样老用户即便之前关过全局开关，
	// 升级后所有盘也都恢复"默认生成预览视频"，跟新建保持一致。
	if _, err := c.addColumnIfMissingReportNew(ctx, "drives", "teaser_enabled", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	// drives.skip_dir_ids：每盘扫描跳过目录集合（JSON array of string）。命中
	// 其中任意一个的目录及其全部子目录都不会被递归扫描。替代旧版硬编码"影视"
	// 目录例外分支；旧 drive 升级后默认空数组 → 行为等同于以前未启用跳过。
	if err := c.addColumnIfMissing(ctx, "drives", "skip_dir_ids", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		return err
	}
	if err := c.syncDriveScanRootIDToRootID(ctx); err != nil {
		return err
	}
	// 一次性修正：早期版本（短暂存在过）会把现存 drive 的 teaser_enabled 同步成
	// 旧的全局 preview.enabled 值，导致升级后所有 drive 都是关。"默认开启"约定下，
	// 这里一次性把所有 drive 强制重置为 1，并用 marker setting 记号，避免之后
	// 再覆盖用户后续在 UI 里 per-drive 改成关的设置。
	if err := c.resetDriveTeaserEnabledToDefaultOnce(ctx); err != nil {
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
	if _, err := c.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_videos_visible_pub ON videos(COALESCE(hidden, 0), published_at DESC)`); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_videos_file_name_size ON videos(file_name, size_bytes)`); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_videos_file_name_size_created ON videos(file_name, size_bytes, created_at, id)`); err != nil {
		return err
	}
	if err := c.seedSystemTags(ctx); err != nil {
		return err
	}
	if err := c.backfillVideoTags(ctx); err != nil {
		return err
	}
	if err := c.collapseAVCodeTags(ctx); err != nil {
		return err
	}
	if err := c.createCollectionTagsFromCategories(ctx); err != nil {
		return err
	}
	if err := c.classifySystemTags(ctx); err != nil {
		return err
	}
	if err := c.clearVolatileOneDriveThumbnails(ctx); err != nil {
		return err
	}
	if err := c.clearRemoteP123ThumbnailsOnce(ctx); err != nil {
		return err
	}
	if err := c.clearRemoteNonSpider91Thumbnails(ctx); err != nil {
		return err
	}
	if err := c.hideZeroSizeVideosFromKnownDrives(ctx); err != nil {
		return err
	}
	if err := c.pruneOrphanCollectionTags(ctx); err != nil {
		return err
	}
	return nil
}

func (c *Catalog) addColumnIfMissing(ctx context.Context, table, column, definition string) error {
	_, err := c.addColumnIfMissingReportNew(ctx, table, column, definition)
	return err
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

// resetDriveTeaserEnabledToDefaultOnce 把所有现存 drive 的 teaser_enabled 强制
// 设为 1（开启），但仅在历史上没跑过这条迁移时执行（用 marker setting 记号）。
//
// 为什么需要：早期短暂存在过的版本会从旧的全局 preview.enabled = "0" 同步到
// 所有 drive 的 teaser_enabled = 0；用户报告升级后页面全显示"预览视频关"。新版
// 约定 per-drive 默认开启，所以这里跑一次性修正。
//
// 幂等保证：marker setting 设过了就不再跑，确保用户在 UI 里把某盘关了不会被
// 重启时反复打开。
func (c *Catalog) resetDriveTeaserEnabledToDefaultOnce(ctx context.Context) error {
	const markerKey = "drives.teaser_enabled.default_open_migrated"
	marker, err := c.GetSetting(ctx, markerKey, "")
	if err != nil {
		return fmt.Errorf("read %s marker: %w", markerKey, err)
	}
	if strings.TrimSpace(marker) == "1" {
		return nil
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE drives SET teaser_enabled = 1, updated_at = ?`, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("reset teaser_enabled to default: %w", err)
	}
	if err := c.SetSetting(ctx, markerKey, "1"); err != nil {
		return fmt.Errorf("write %s marker: %w", markerKey, err)
	}
	return nil
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

func (c *Catalog) clearVolatileOneDriveThumbnails(ctx context.Context) error {
	// 把 OneDrive 过期的 mediap.svc.ms thumb URL 清空，让 worker 重新抽帧生成本地封面。
	// 同步把 thumbnail_status 重置为 'pending'：清空后 url 是空的，本应进 worker 重做，
	// 若 status 还停留在 'ready' / 'failed' 会和 ListVideosNeedingThumbnail 的语义不一致
	// （admin/统计按 url 看：空 + 非 'failed' = pending；status='failed' 会让重做被阻断）。
	_, err := c.db.ExecContext(ctx, `
UPDATE videos
   SET thumbnail_url = '',
       thumbnail_status = 'pending',
       updated_at = ?
 WHERE lower(COALESCE(thumbnail_url, '')) LIKE 'https://%mediap.svc.ms/transform/thumbnail%'
`, time.Now().UnixMilli())
	return err
}

func (c *Catalog) clearRemoteP123ThumbnailsOnce(ctx context.Context) error {
	// 123 云盘列表返回的缩略图尺寸和稳定性都不适合作为站内封面；清空历史写入的
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

func (c *Catalog) clearRemoteNonSpider91Thumbnails(ctx context.Context) error {
	// 非 91Spider 视频不再使用网盘侧返回的远程缩略图。清空历史 http/https
	// thumbnail_url 后，封面 worker 会重新从视频中间帧生成本地 /p/thumb/<id>。
	// 91Spider 的封面是爬虫下载后保存到本地 /p/thumb/<id>，不受这条规则影响。
	res, err := c.db.ExecContext(ctx, `
UPDATE videos
   SET thumbnail_url = '',
       thumbnail_status = 'pending',
       thumbnail_failures = 0,
       updated_at = ?
 WHERE (
       lower(COALESCE(thumbnail_url, '')) LIKE 'http://%'
       OR lower(COALESCE(thumbnail_url, '')) LIKE 'https://%'
   )
   AND NOT EXISTS (
       SELECT 1
         FROM drives
        WHERE drives.id = videos.drive_id
          AND drives.kind = 'spider91'
   )
`, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err == nil && affected > 0 {
		log.Printf("[catalog] cleared %d remote non-91Spider thumbnail(s) for local regeneration", affected)
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

func (c *Catalog) seedSystemTags(ctx context.Context) error {
	for _, label := range fixedtags.Labels {
		if _, err := c.ensureTag(ctx, label, fixedtags.AliasesFor(label), "system"); err != nil {
			return err
		}
	}
	return nil
}

func (c *Catalog) classifySystemTags(ctx context.Context) error {
	total := 0
	for _, label := range fixedtags.Labels {
		tag, err := c.getTagByLabel(ctx, label)
		if err != nil {
			return err
		}
		classified, err := c.classifyTag(ctx, tag)
		if err != nil {
			return err
		}
		total += classified
	}
	if total > 0 {
		log.Printf("[catalog] classified %d existing video tag(s) using system tags", total)
	}
	return nil
}

func (c *Catalog) backfillVideoTags(ctx context.Context) error {
	rows, err := c.db.QueryContext(ctx, `
SELECT id, COALESCE(tags, '[]')
FROM videos
WHERE COALESCE(tags, '') NOT IN ('', '[]', 'null')
  AND NOT EXISTS (
	SELECT 1
	  FROM video_tags vt
	 WHERE vt.video_id = videos.id
  )`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var videoID, tagsJSON string
		if err := rows.Scan(&videoID, &tagsJSON); err != nil {
			return err
		}
		var labels []string
		if err := json.Unmarshal([]byte(tagsJSON), &labels); err != nil {
			continue
		}
		if len(labels) == 0 {
			continue
		}
		added, err := c.addVideoTags(ctx, videoID, labels, "legacy", true)
		if err != nil {
			return err
		}
		if added {
			if err := c.syncVideoTagsJSON(ctx, videoID, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Catalog) createCollectionTagsFromCategories(ctx context.Context) error {
	rows, err := c.db.QueryContext(ctx, `
SELECT category, COUNT(*) FROM videos
WHERE COALESCE(category, '') != ''
GROUP BY category`)
	if err != nil {
		return err
	}
	type categoryStat struct {
		category string
		count    int
	}
	var categories []categoryStat
	for rows.Next() {
		var stat categoryStat
		if err := rows.Scan(&stat.category, &stat.count); err != nil {
			return err
		}
		categories = append(categories, stat)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, stat := range categories {
		if isAVCodePollutedLabel(stat.category) {
			if _, err := c.ensureTag(ctx, avTagLabel, fixedtags.AliasesFor(avTagLabel), "system"); err != nil {
				return err
			}
			if err := c.addTagToVideosByCategory(ctx, stat.category, avTagLabel, "auto"); err != nil {
				return err
			}
			continue
		}
		if stat.count < 3 {
			continue
		}
		if !LooksLikeCollectionTag(stat.category) {
			continue
		}
		if c.tagDeleted(ctx, stat.category) {
			continue
		}
		if _, err := c.ensureTag(ctx, stat.category, nil, "collection"); err != nil {
			return err
		}
		if err := c.addCollectionTagToVideos(ctx, stat.category); err != nil {
			return err
		}
	}
	return nil
}

func (c *Catalog) CreateTagAndClassify(ctx context.Context, label string, aliases []string, source string) (int, error) {
	tag, err := c.ensureTag(ctx, label, aliases, source)
	if err != nil {
		return 0, err
	}
	return c.classifyTag(ctx, tag)
}

func (c *Catalog) EnsureTagForVideoIDPrefix(ctx context.Context, prefix, label string, aliases []string, source string) (int, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return 0, errors.New("video id prefix is required")
	}
	tag, err := c.ensureTag(ctx, label, aliases, source)
	if err != nil {
		return 0, err
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT v.id
  FROM videos v
 WHERE v.id LIKE ? || '%'
   AND COALESCE(v.tags_manual, 0) = 0
   AND NOT EXISTS (
	 SELECT 1
	   FROM video_tags vt
	  WHERE vt.video_id = v.id
	    AND vt.tag_id = ?
   )
 ORDER BY v.id ASC`, prefix, tag.ID)
	if err != nil {
		return 0, err
	}
	var videoIDs []string
	for rows.Next() {
		var videoID string
		if err := rows.Scan(&videoID); err != nil {
			rows.Close()
			return 0, err
		}
		videoIDs = append(videoIDs, videoID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, videoID := range videoIDs {
		if err := c.insertVideoTag(ctx, videoID, tag.ID, "auto"); err != nil {
			return 0, err
		}
		if err := c.syncVideoTagsJSON(ctx, videoID, false); err != nil {
			return 0, err
		}
	}
	return len(videoIDs), nil
}

func (c *Catalog) DeleteTag(ctx context.Context, tagID int64) (int, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	tag, err := c.getTagByIDTx(ctx, tx, tagID)
	if err != nil {
		return 0, err
	}
	if tag.Source == "system" {
		return 0, ErrSystemTag
	}

	rows, err := tx.QueryContext(ctx, `SELECT video_id FROM video_tags WHERE tag_id = ?`, tagID)
	if err != nil {
		return 0, err
	}
	var videoIDs []string
	for rows.Next() {
		var videoID string
		if err := rows.Scan(&videoID); err != nil {
			rows.Close()
			return 0, err
		}
		videoIDs = append(videoIDs, videoID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM video_tags WHERE tag_id = ?`, tagID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tags WHERE id = ?`, tagID); err != nil {
		return 0, err
	}
	if err := markDeletedTagTx(ctx, tx, tag); err != nil {
		return 0, err
	}

	for _, videoID := range videoIDs {
		manual := hasManualTagsTx(ctx, tx, videoID)
		if err := syncVideoTagsJSONTx(ctx, tx, videoID, manual); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(videoIDs), nil
}

func (c *Catalog) SetTagFrontendAccess(ctx context.Context, tagID int64, enabled bool) error {
	if tagID <= 0 {
		return sql.ErrNoRows
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE tags SET frontend_access = ?, updated_at = ? WHERE id = ?`,
		boolToInt(enabled), time.Now().UnixMilli(), tagID)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	if !enabled {
		if _, err := tx.ExecContext(ctx, `
UPDATE videos
   SET frontend_selected = 0,
       updated_at = ?
 WHERE COALESCE(frontend_selected, 0) = 1
   AND NOT EXISTS (
       SELECT 1
         FROM video_tags vt
         JOIN tags t ON t.id = vt.tag_id
        WHERE vt.video_id = videos.id
          AND COALESCE(t.frontend_access, 0) = 1
   )`, time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (c *Catalog) ListTags(ctx context.Context) ([]Tag, error) {
	rows, err := c.db.QueryContext(ctx, `
WITH tagged_tags AS (
	SELECT vt.tag_id,
	       tagged.id,
	       COALESCE(tagged.content_hash, '') AS content_hash,
	       COALESCE(tagged.sampled_sha256, '') AS sampled_sha256,
	       tagged.size_bytes,
	       COALESCE(tagged.file_name, '') AS file_name
	  FROM video_tags vt
	  JOIN videos tagged ON tagged.id = vt.video_id
	 WHERE COALESCE(tagged.hidden, 0) = 0
),
tag_candidates AS (
	SELECT tag_id, id AS video_id
	  FROM tagged_tags
	UNION ALL
	SELECT tag_id,
	       (SELECT canonical.id
	          FROM videos canonical
	         WHERE tagged_tags.content_hash != ''
	           AND canonical.content_hash = tagged_tags.content_hash
	           AND COALESCE(canonical.content_hash, '') != ''
	         ORDER BY canonical.created_at ASC, canonical.id ASC
	         LIMIT 1) AS video_id
	  FROM tagged_tags
	 WHERE content_hash != ''
	UNION ALL
	SELECT tag_id,
	       (SELECT canonical.id
	          FROM videos canonical
	         WHERE tagged_tags.sampled_sha256 != ''
	           AND tagged_tags.size_bytes > 0
	           AND canonical.sampled_sha256 = tagged_tags.sampled_sha256
	           AND canonical.size_bytes = tagged_tags.size_bytes
	           AND COALESCE(canonical.sampled_sha256, '') != ''
	           AND canonical.size_bytes > 0
	         ORDER BY canonical.created_at ASC, canonical.id ASC
	         LIMIT 1) AS video_id
	  FROM tagged_tags
	 WHERE sampled_sha256 != '' AND size_bytes > 0
	UNION ALL
	SELECT tag_id,
	       (SELECT canonical.id
	          FROM videos canonical
	         WHERE tagged_tags.file_name != ''
	           AND tagged_tags.size_bytes > 0
	           AND canonical.file_name = tagged_tags.file_name
	           AND canonical.size_bytes = tagged_tags.size_bytes
	           AND COALESCE(canonical.file_name, '') != ''
	           AND canonical.size_bytes > 0
	         ORDER BY canonical.created_at ASC, canonical.id ASC
	         LIMIT 1) AS video_id
	  FROM tagged_tags
	 WHERE file_name != '' AND size_bytes > 0
)
SELECT t.id, t.label, t.aliases, t.source, COALESCE(t.frontend_access, 0), COUNT(DISTINCT videos.id) AS cnt
FROM tags t
LEFT JOIN tag_candidates tc ON tc.tag_id = t.id AND tc.video_id IS NOT NULL
LEFT JOIN videos ON videos.id = tc.video_id
	AND COALESCE(videos.hidden, 0) = 0
	AND `+uniqueVideoWhereSQL+`
GROUP BY t.id, t.label, t.aliases, t.source, COALESCE(t.frontend_access, 0)
ORDER BY cnt DESC, t.label ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tag
	for rows.Next() {
		tag, err := scanTag(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tag)
	}
	return out, nil
}

func (c *Catalog) ListFrontendTags(ctx context.Context) ([]Tag, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT t.id,
       t.label,
       t.aliases,
       t.source,
       COALESCE(t.frontend_access, 0),
       COUNT(DISTINCT videos.id) AS cnt
  FROM tags t
  LEFT JOIN video_tags vt ON vt.tag_id = t.id
  LEFT JOIN videos ON videos.id = vt.video_id
	AND `+frontendVisibleWhereSQL+`
	AND `+activeDriveWhereSQL+`
	AND `+uniqueVideoWhereSQL+`
 WHERE COALESCE(t.frontend_access, 0) = 1
 GROUP BY t.id, t.label, t.aliases, t.source, COALESCE(t.frontend_access, 0)
 ORDER BY cnt DESC, t.label ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tag
	for rows.Next() {
		tag, err := scanTag(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tag)
	}
	return out, rows.Err()
}

func videoMatchesTagLabelSQL(videoAlias string) string {
	return fmt.Sprintf(`%s.id IN (
			WITH tagged_videos AS (
				SELECT tagged.id,
				       COALESCE(tagged.content_hash, '') AS content_hash,
				       COALESCE(tagged.sampled_sha256, '') AS sampled_sha256,
				       tagged.size_bytes,
				       COALESCE(tagged.file_name, '') AS file_name
				  FROM video_tags vt
				  JOIN tags tag_filter ON tag_filter.id = vt.tag_id
				  JOIN videos tagged ON tagged.id = vt.video_id
				 WHERE tag_filter.label = ? COLLATE NOCASE
				   AND COALESCE(tagged.hidden, 0) = 0
			),
			tag_candidates AS (
				SELECT id AS video_id
				  FROM tagged_videos
				UNION ALL
				SELECT (SELECT canonical.id
				          FROM videos canonical
				         WHERE tagged_videos.content_hash != ''
				           AND canonical.content_hash = tagged_videos.content_hash
				           AND COALESCE(canonical.content_hash, '') != ''
				         ORDER BY canonical.created_at ASC, canonical.id ASC
				         LIMIT 1) AS video_id
				  FROM tagged_videos
				 WHERE content_hash != ''
				UNION ALL
				SELECT (SELECT canonical.id
				          FROM videos canonical
				         WHERE tagged_videos.sampled_sha256 != ''
				           AND tagged_videos.size_bytes > 0
				           AND canonical.sampled_sha256 = tagged_videos.sampled_sha256
				           AND canonical.size_bytes = tagged_videos.size_bytes
				           AND COALESCE(canonical.sampled_sha256, '') != ''
				           AND canonical.size_bytes > 0
				         ORDER BY canonical.created_at ASC, canonical.id ASC
				         LIMIT 1) AS video_id
				  FROM tagged_videos
				 WHERE sampled_sha256 != '' AND size_bytes > 0
				UNION ALL
				SELECT (SELECT canonical.id
				          FROM videos canonical
				         WHERE tagged_videos.file_name != ''
				           AND tagged_videos.size_bytes > 0
				           AND canonical.file_name = tagged_videos.file_name
				           AND canonical.size_bytes = tagged_videos.size_bytes
				           AND COALESCE(canonical.file_name, '') != ''
				           AND canonical.size_bytes > 0
				         ORDER BY canonical.created_at ASC, canonical.id ASC
				         LIMIT 1) AS video_id
				  FROM tagged_videos
				 WHERE file_name != '' AND size_bytes > 0
			)
			SELECT video_id
			  FROM tag_candidates
			 WHERE video_id IS NOT NULL
		)`, videoAlias)
}

func (c *Catalog) SetManualVideoTags(ctx context.Context, videoID string, labels []string) error {
	if _, err := c.GetVideo(ctx, videoID); err != nil {
		return err
	}
	return c.replaceVideoTags(ctx, videoID, labels, "manual", true, false)
}

func (c *Catalog) SetAutoVideoTags(ctx context.Context, videoID string, labels []string) error {
	if c.hasManualTags(ctx, videoID) {
		return nil
	}
	return c.replaceVideoTags(ctx, videoID, labels, "auto", false, false)
}

func (c *Catalog) MatchTags(ctx context.Context, text string) ([]string, error) {
	tags, err := c.ListTags(ctx)
	if err != nil {
		return nil, err
	}
	matcher := normalizeTagText(text)
	out := make([]string, 0, len(tags))
	if ContainsAVCode(text) {
		out = append(out, avTagLabel)
	}
	for _, tag := range tags {
		candidates := append([]string{tag.Label}, tag.Aliases...)
		for _, candidate := range candidates {
			if matcher.contains(candidate) {
				out = append(out, tag.Label)
				break
			}
		}
	}
	return sortLabelsByTagOrder(tags, uniqueStrings(out)), nil
}

func (c *Catalog) EnsureCollectionTag(ctx context.Context, label string) (string, bool, error) {
	label = cleanTagLabel(label)
	if isAVCodePollutedLabel(label) {
		if _, err := c.ensureTag(ctx, avTagLabel, fixedtags.AliasesFor(avTagLabel), "system"); err != nil {
			return "", false, err
		}
		if err := c.addTagToVideosByCategory(ctx, label, avTagLabel, "auto"); err != nil {
			return "", false, err
		}
		return avTagLabel, true, nil
	}
	if !LooksLikeCollectionTag(label) {
		return "", false, nil
	}
	if c.tagDeleted(ctx, label) {
		return "", false, nil
	}
	if !c.tagExists(ctx, label) {
		count, err := c.categoryVideoCount(ctx, label)
		if err != nil {
			return "", false, err
		}
		if count < 2 {
			return "", false, nil
		}
	}
	if _, err := c.ensureTag(ctx, label, nil, "collection"); err != nil {
		return "", false, err
	}
	if err := c.addCollectionTagToVideos(ctx, label); err != nil {
		return "", false, err
	}
	return label, true, nil
}

func (c *Catalog) ensureTag(ctx context.Context, label string, aliases []string, source string) (Tag, error) {
	label = cleanTagLabel(label)
	if label == "" {
		return Tag{}, errors.New("tag label is required")
	}
	if isAVCodePollutedLabel(label) {
		label = avTagLabel
		aliases = fixedtags.AliasesFor(avTagLabel)
		source = "system"
	}
	if source == "" {
		source = "user"
	}
	if source != "system" && source != "user" && c.tagDeleted(ctx, label) {
		return Tag{}, ErrDeletedTag
	}
	if source == "system" || source == "user" {
		if err := c.restoreDeletedTag(ctx, label); err != nil {
			return Tag{}, err
		}
	}
	aliases = cleanAliases(aliases, label)
	aliasesJSON, _ := json.Marshal(aliases)
	now := time.Now().UnixMilli()
	if _, err := c.db.ExecContext(ctx, `
INSERT OR IGNORE INTO tags (label, aliases, source, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)`, label, string(aliasesJSON), source, now, now); err != nil {
		return Tag{}, err
	}
	if len(aliases) > 0 {
		if _, err := c.db.ExecContext(ctx,
			`UPDATE tags SET aliases = ?, updated_at = ? WHERE label = ? COLLATE NOCASE`,
			string(aliasesJSON), now, label); err != nil {
			return Tag{}, err
		}
	}
	return c.getTagByLabel(ctx, label)
}

func (c *Catalog) getTagByLabel(ctx context.Context, label string) (Tag, error) {
	row := c.db.QueryRowContext(ctx,
		`SELECT id, label, aliases, source, COALESCE(frontend_access, 0), 0 FROM tags WHERE label = ? COLLATE NOCASE`,
		label)
	return scanTag(row)
}

func (c *Catalog) classifyTag(ctx context.Context, tag Tag) (int, error) {
	existingIDs, err := c.videoIDSetForTagID(ctx, tag.ID)
	if err != nil {
		return 0, err
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT id, title, COALESCE(author, ''), COALESCE(category, ''), COALESCE(tags_manual, 0)
FROM videos`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	classified := 0
	for rows.Next() {
		var videoID, title, author, category string
		var manual int
		if err := rows.Scan(&videoID, &title, &author, &category, &manual); err != nil {
			return 0, err
		}
		if manual == 1 {
			continue
		}
		matcher := normalizeTagText(title + " " + author + " " + category)
		if !matcher.contains(tag.Label) {
			matchedAlias := false
			for _, alias := range tag.Aliases {
				if matcher.contains(alias) {
					matchedAlias = true
					break
				}
			}
			if !matchedAlias {
				continue
			}
		}
		if existingIDs[videoID] {
			continue
		}
		if err := c.insertVideoTag(ctx, videoID, tag.ID, "auto"); err != nil {
			return 0, err
		}
		existingIDs[videoID] = true
		classified++
		if err := c.syncVideoTagsJSON(ctx, videoID, false); err != nil {
			return 0, err
		}
	}
	return classified, nil
}

func (c *Catalog) replaceVideoTags(ctx context.Context, videoID string, labels []string, source string, manual bool, createMissing bool) error {
	labels = uniqueStrings(cleanLabels(labels))
	if source != "manual" {
		labels = c.filterDeletedTagLabels(ctx, labels)
	}
	if createMissing {
		for _, label := range labels {
			if _, err := c.ensureTag(ctx, label, nil, "legacy"); err != nil {
				if errors.Is(err, ErrDeletedTag) {
					continue
				}
				return err
			}
		}
	} else {
		if err := c.validateTagsExist(ctx, labels); err != nil {
			return err
		}
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM video_tags WHERE video_id = ?`, videoID); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for _, label := range labels {
		tag, err := c.getTagByLabelTx(ctx, tx, label)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO video_tags (video_id, tag_id, source, created_at) VALUES (?, ?, ?, ?)`,
			videoID, tag.ID, source, now); err != nil {
			return err
		}
	}
	manualValue := 0
	if manual {
		manualValue = 1
	}
	if _, err := tx.ExecContext(ctx, `UPDATE videos SET tags_manual = ? WHERE id = ?`, manualValue, videoID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return c.syncVideoTagsJSON(ctx, videoID, manual)
}

func (c *Catalog) addVideoTags(ctx context.Context, videoID string, labels []string, source string, createMissing bool) (bool, error) {
	labels = uniqueStrings(cleanLabels(labels))
	if source != "manual" {
		labels = c.filterDeletedTagLabels(ctx, labels)
	}
	changed := false
	for _, label := range labels {
		added, err := c.addVideoTag(ctx, videoID, label, source, createMissing)
		if err != nil {
			return false, err
		}
		if added {
			changed = true
		}
	}
	return changed, nil
}

func (c *Catalog) addVideoTag(ctx context.Context, videoID, label, source string, createMissing bool) (bool, error) {
	if source != "manual" && c.tagDeleted(ctx, label) {
		return false, nil
	}
	if createMissing {
		if _, err := c.ensureTag(ctx, label, nil, "legacy"); err != nil {
			if errors.Is(err, ErrDeletedTag) {
				return false, nil
			}
			return false, err
		}
	}
	tag, err := c.getTagByLabel(ctx, label)
	if err != nil {
		return false, err
	}
	now := time.Now().UnixMilli()
	res, err := c.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO video_tags (video_id, tag_id, source, created_at) VALUES (?, ?, ?, ?)`,
		videoID, tag.ID, source, now)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (c *Catalog) insertVideoTag(ctx context.Context, videoID string, tagID int64, source string) error {
	_, err := c.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO video_tags (video_id, tag_id, source, created_at) VALUES (?, ?, ?, ?)`,
		videoID, tagID, source, time.Now().UnixMilli())
	return err
}

func (c *Catalog) addCollectionTagToVideos(ctx context.Context, category string) error {
	return c.addTagToVideosByCategory(ctx, category, category, "auto")
}

func (c *Catalog) addTagToVideosByCategory(ctx context.Context, category, label, source string) error {
	tag, err := c.getTagByLabel(ctx, label)
	if err != nil {
		return err
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT v.id
  FROM videos v
 WHERE v.category = ?
   AND COALESCE(v.tags_manual, 0) = 0
   AND NOT EXISTS (
	 SELECT 1
	   FROM video_tags vt
	  WHERE vt.video_id = v.id
	    AND vt.tag_id = ?
   )`, category, tag.ID)
	if err != nil {
		return err
	}
	var videoIDs []string
	for rows.Next() {
		var videoID string
		if err := rows.Scan(&videoID); err != nil {
			return err
		}
		videoIDs = append(videoIDs, videoID)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, videoID := range videoIDs {
		if err := c.insertVideoTag(ctx, videoID, tag.ID, source); err != nil {
			return err
		}
		if err := c.syncVideoTagsJSON(ctx, videoID, false); err != nil {
			return err
		}
	}
	return nil
}

func (c *Catalog) collapseAVCodeTags(ctx context.Context) error {
	if _, err := c.ensureTag(ctx, avTagLabel, fixedtags.AliasesFor(avTagLabel), "system"); err != nil {
		return err
	}

	rows, err := c.db.QueryContext(ctx, `SELECT id, label FROM tags`)
	if err != nil {
		return err
	}

	type pollutedTag struct {
		id    int64
		label string
	}
	var polluted []pollutedTag
	for rows.Next() {
		var tag pollutedTag
		if err := rows.Scan(&tag.id, &tag.label); err != nil {
			return err
		}
		if strings.EqualFold(tag.label, avTagLabel) || !isAVCodePollutedLabel(tag.label) {
			continue
		}
		polluted = append(polluted, tag)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, tag := range polluted {
		videoIDs, err := c.videoIDsForTagID(ctx, tag.id)
		if err != nil {
			return err
		}
		for _, videoID := range videoIDs {
			if _, err := c.addVideoTag(ctx, videoID, avTagLabel, "auto", false); err != nil {
				return err
			}
		}
		if _, err := c.db.ExecContext(ctx, `DELETE FROM video_tags WHERE tag_id = ?`, tag.id); err != nil {
			return err
		}
		if _, err := c.db.ExecContext(ctx, `DELETE FROM tags WHERE id = ?`, tag.id); err != nil {
			return err
		}
		for _, videoID := range videoIDs {
			if err := c.syncVideoTagsJSON(ctx, videoID, c.hasManualTags(ctx, videoID)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Catalog) videoIDsForTagID(ctx context.Context, tagID int64) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT video_id FROM video_tags WHERE tag_id = ?`, tagID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var videoIDs []string
	for rows.Next() {
		var videoID string
		if err := rows.Scan(&videoID); err != nil {
			return nil, err
		}
		videoIDs = append(videoIDs, videoID)
	}
	return videoIDs, rows.Err()
}

func (c *Catalog) videoIDSetForTagID(ctx context.Context, tagID int64) (map[string]bool, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT video_id FROM video_tags WHERE tag_id = ?`, tagID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var videoID string
		if err := rows.Scan(&videoID); err != nil {
			return nil, err
		}
		out[videoID] = true
	}
	return out, rows.Err()
}

func (c *Catalog) validateTagsExist(ctx context.Context, labels []string) error {
	for _, label := range labels {
		if _, err := c.getTagByLabel(ctx, label); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: %s", ErrUnknownTag, label)
			}
			return err
		}
	}
	return nil
}

func (c *Catalog) syncVideoTagsJSON(ctx context.Context, videoID string, manual bool) error {
	rows, err := c.db.QueryContext(ctx, `
SELECT t.label
FROM video_tags vt
JOIN tags t ON t.id = vt.tag_id
WHERE vt.video_id = ?
ORDER BY t.id ASC`, videoID)
	if err != nil {
		return err
	}
	var labels []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return err
		}
		labels = append(labels, label)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	labelsJSON, _ := json.Marshal(labels)
	manualValue := 0
	if manual {
		manualValue = 1
	}
	_, err = c.db.ExecContext(ctx,
		`UPDATE videos SET tags = ?, tags_manual = ?, updated_at = ? WHERE id = ?`,
		string(labelsJSON), manualValue, time.Now().UnixMilli(), videoID)
	return err
}

func (c *Catalog) hasManualTags(ctx context.Context, videoID string) bool {
	var manual int
	err := c.db.QueryRowContext(ctx, `SELECT COALESCE(tags_manual, 0) FROM videos WHERE id = ?`, videoID).Scan(&manual)
	return err == nil && manual == 1
}

func (c *Catalog) videoExists(ctx context.Context, videoID string) bool {
	var exists int
	err := c.db.QueryRowContext(ctx, `SELECT 1 FROM videos WHERE id = ?`, videoID).Scan(&exists)
	return err == nil
}

func (c *Catalog) tagExists(ctx context.Context, label string) bool {
	var exists int
	err := c.db.QueryRowContext(ctx, `SELECT 1 FROM tags WHERE label = ? COLLATE NOCASE`, label).Scan(&exists)
	return err == nil
}

func (c *Catalog) tagDeleted(ctx context.Context, label string) bool {
	label = cleanTagLabel(label)
	if label == "" {
		return false
	}
	var exists int
	err := c.db.QueryRowContext(ctx, `SELECT 1 FROM deleted_tags WHERE label = ? COLLATE NOCASE`, label).Scan(&exists)
	return err == nil
}

func (c *Catalog) filterDeletedTagLabels(ctx context.Context, labels []string) []string {
	if len(labels) == 0 {
		return labels
	}
	out := labels[:0]
	for _, label := range labels {
		if c.tagDeleted(ctx, label) {
			continue
		}
		out = append(out, label)
	}
	return out
}

func (c *Catalog) restoreDeletedTag(ctx context.Context, label string) error {
	label = cleanTagLabel(label)
	if label == "" {
		return nil
	}
	_, err := c.db.ExecContext(ctx, `DELETE FROM deleted_tags WHERE label = ? COLLATE NOCASE`, label)
	return err
}

func (c *Catalog) categoryVideoCount(ctx context.Context, category string) (int, error) {
	var count int
	err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM videos WHERE category = ?`, category).Scan(&count)
	return count, err
}

func (c *Catalog) getTagByLabelTx(ctx context.Context, tx *sql.Tx, label string) (Tag, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT id, label, aliases, source, COALESCE(frontend_access, 0), 0 FROM tags WHERE label = ? COLLATE NOCASE`,
		label)
	return scanTag(row)
}

func (c *Catalog) getTagByIDTx(ctx context.Context, tx *sql.Tx, id int64) (Tag, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT id, label, aliases, source, COALESCE(frontend_access, 0), 0 FROM tags WHERE id = ?`,
		id)
	return scanTag(row)
}

func hasManualTagsTx(ctx context.Context, tx *sql.Tx, videoID string) bool {
	var manual int
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(tags_manual, 0) FROM videos WHERE id = ?`, videoID).Scan(&manual)
	return err == nil && manual == 1
}

func markDeletedTagTx(ctx context.Context, tx *sql.Tx, tag Tag) error {
	label := cleanTagLabel(tag.Label)
	if label == "" {
		return nil
	}
	now := time.Now().UnixMilli()
	_, err := tx.ExecContext(ctx, `
INSERT INTO deleted_tags (label, source, deleted_at)
VALUES (?, ?, ?)
ON CONFLICT(label) DO UPDATE SET
  source = excluded.source,
  deleted_at = excluded.deleted_at`, label, tag.Source, now)
	return err
}

func syncVideoTagsJSONTx(ctx context.Context, tx *sql.Tx, videoID string, manual bool) error {
	rows, err := tx.QueryContext(ctx, `
SELECT t.label
FROM video_tags vt
JOIN tags t ON t.id = vt.tag_id
WHERE vt.video_id = ?
ORDER BY t.id ASC`, videoID)
	if err != nil {
		return err
	}
	var labels []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			rows.Close()
			return err
		}
		labels = append(labels, label)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	labelsJSON, _ := json.Marshal(labels)
	manualValue := 0
	if manual {
		manualValue = 1
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE videos SET tags = ?, tags_manual = ?, updated_at = ? WHERE id = ?`,
		string(labelsJSON), manualValue, time.Now().UnixMilli(), videoID)
	return err
}

type tagRowScanner interface {
	Scan(dest ...any) error
}

func scanTag(row tagRowScanner) (Tag, error) {
	var tag Tag
	var aliasesJSON string
	var frontendAccess int
	if err := row.Scan(&tag.ID, &tag.Label, &aliasesJSON, &tag.Source, &frontendAccess, &tag.Count); err != nil {
		return Tag{}, err
	}
	_ = json.Unmarshal([]byte(aliasesJSON), &tag.Aliases)
	tag.FrontendAccess = frontendAccess == 1
	return tag, nil
}

type normalizedTagText struct {
	lower   string
	compact string
	tokens  map[string]struct{}
}

func normalizeTagText(s string) normalizedTagText {
	lower := strings.ToLower(s)
	var compact strings.Builder
	var spaced strings.Builder
	for _, r := range lower {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			compact.WriteRune(r)
			spaced.WriteRune(r)
			continue
		}
		spaced.WriteByte(' ')
	}
	tokens := make(map[string]struct{})
	for _, token := range strings.Fields(spaced.String()) {
		tokens[token] = struct{}{}
	}
	return normalizedTagText{lower: lower, compact: compact.String(), tokens: tokens}
}

func (n normalizedTagText) contains(alias string) bool {
	lowerAlias := strings.ToLower(strings.TrimSpace(alias))
	compactAlias := compactTagText(lowerAlias)
	if compactAlias == "" {
		return false
	}
	if isShortASCIIWord(compactAlias) && compactAlias == lowerAlias {
		_, ok := n.tokens[compactAlias]
		return ok
	}
	if strings.Contains(n.lower, lowerAlias) {
		return true
	}
	return strings.Contains(n.compact, compactAlias)
}

func compactTagText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isShortASCIIWord(s string) bool {
	if len(s) > 3 {
		return false
	}
	for _, r := range s {
		if r > unicode.MaxASCII || (!unicode.IsLetter(r) && !unicode.IsDigit(r)) {
			return false
		}
	}
	return true
}

func LooksLikeCollectionTag(label string) bool {
	label = cleanTagLabel(label)
	if label == "" {
		return false
	}
	if isAVCodePollutedLabel(label) {
		return false
	}
	runes := []rune(label)
	if len(runes) < 2 || len(runes) > 24 {
		return false
	}
	lower := strings.ToLower(label)
	blocked := map[string]bool{
		"v": true, "pv": true, "my pack": true, "my upload": true,
		"视频": true, "视频1": true, "第一直播": true, "男人必备": true,
		"瑟女聚集地": true, "成人色游": true, "ai女友": true,
	}
	if blocked[lower] {
		return false
	}
	hasLetter := false
	for _, r := range label {
		if unicode.IsLetter(r) {
			hasLetter = true
			break
		}
	}
	if !hasLetter {
		return false
	}
	for _, r := range label {
		switch r {
		case '，', '。', '！', '？', '；', '、', '：', '~', '～':
			return false
		}
	}
	return true
}

func IsAVCode(label string) bool {
	label = cleanTagLabel(label)
	if label == "" {
		return false
	}
	return avCodePattern.MatchString(label) || ccAVCodePattern.MatchString(label) || fc2AVCodePattern.MatchString(label) || numericPrefixAVCodePattern.MatchString(label)
}

func ContainsAVCode(text string) bool {
	return avCodeInTextPattern.MatchString(text)
}

func isAVCodePollutedLabel(label string) bool {
	label = cleanTagLabel(label)
	if label == "" {
		return false
	}
	return IsAVCode(label) || ContainsAVCode(label)
}

func cleanLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		label = cleanTagLabel(label)
		if label != "" {
			if isAVCodePollutedLabel(label) {
				label = avTagLabel
			}
			out = append(out, label)
		}
	}
	return out
}

func cleanTagLabel(label string) string {
	return strings.TrimSpace(label)
}

func cleanAliases(aliases []string, label string) []string {
	out := make([]string, 0, len(aliases))
	seen := map[string]bool{strings.ToLower(label): true}
	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		key := strings.ToLower(alias)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, alias)
	}
	return out
}

func uniqueStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		key := strings.ToLower(value)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}

func sortLabelsByTagOrder(tags []Tag, labels []string) []string {
	order := make(map[string]int, len(tags))
	for i, tag := range tags {
		order[strings.ToLower(tag.Label)] = i
	}
	sort.SliceStable(labels, func(i, j int) bool {
		return order[strings.ToLower(labels[i])] < order[strings.ToLower(labels[j])]
	})
	return labels
}

// pruneOrphanCollectionTags 删除所有 source='collection' 且不再被任何 video_tags 引用的标签。
// 在 migrate 末尾调用，相当于启动时自愈：之前 DeleteVideo 没顺带清理留下的孤儿，会在重启时被收回。
// 只动 collection：system 是固定标签需保留；user 是管理员手动建的；auto/legacy 默认有视频在引用。
func (c *Catalog) pruneOrphanCollectionTags(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx, `
DELETE FROM tags
 WHERE source = 'collection'
   AND id NOT IN (SELECT tag_id FROM video_tags)`)
	return err
}

// pruneOrphanCollectionTagsByID 在事务里检查一组候选 tag_id，删除其中
// source='collection' 且已经没有视频引用的标签。供 DeleteVideo 调用。
func pruneOrphanCollectionTagsByID(ctx context.Context, tx *sql.Tx, tagIDs []int64) error {
	for _, tagID := range tagIDs {
		var src string
		err := tx.QueryRowContext(ctx, `SELECT source FROM tags WHERE id = ?`, tagID).Scan(&src)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if src != "collection" {
			continue
		}
		var refCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM video_tags WHERE tag_id = ?`, tagID).Scan(&refCount); err != nil {
			return err
		}
		if refCount > 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tags WHERE id = ?`, tagID); err != nil {
			return err
		}
	}
	return nil
}

// collectVideoTagIDs 在事务里读出当前视频关联的 tag_id，供后续清理判断。
func collectVideoTagIDs(ctx context.Context, tx *sql.Tx, videoID string) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT tag_id FROM video_tags WHERE video_id = ?`, videoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
