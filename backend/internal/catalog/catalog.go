package catalog

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

type Catalog struct {
	db *sql.DB
}

func Open(path string) (*Catalog, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	c := &Catalog{db: db}
	if err := c.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate catalog: %w", err)
	}
	return c, nil
}

func (c *Catalog) Close() error { return c.db.Close() }

// ---------- Video ----------

var ErrVideoOutsideFrontendTags = errors.New("video must have at least one frontend-accessible tag")

type Video struct {
	ID                string    `json:"id"`
	DriveID           string    `json:"driveId"`
	FileID            string    `json:"fileId"`
	FileName          string    `json:"fileName"`
	ContentHash       string    `json:"contentHash"`
	SampledSHA256     string    `json:"sampledSha256"`
	FingerprintStatus string    `json:"fingerprintStatus"`
	FingerprintError  string    `json:"fingerprintError"`
	ParentID          string    `json:"parentId"`
	Title             string    `json:"title"`
	Author            string    `json:"author"`
	Tags              []string  `json:"tags"`
	DurationSeconds   int       `json:"durationSeconds"`
	Size              int64     `json:"size"`
	Ext               string    `json:"ext"`
	Quality           string    `json:"quality"`
	ThumbnailURL      string    `json:"thumbnailUrl"`
	PreviewFileID     string    `json:"previewFileId"`
	PreviewLocal      string    `json:"previewLocal"`
	PreviewStatus     string    `json:"previewStatus"`
	Views             int       `json:"views"`
	Favorites         int       `json:"favorites"`
	Comments          int       `json:"comments"`
	Likes             int       `json:"likes"`
	Dislikes          int       `json:"dislikes"`
	Category          string    `json:"category"`
	Hidden            bool      `json:"hidden"`
	FrontendSelected  bool      `json:"frontendSelected"`
	Badges            []string  `json:"badges"`
	Description       string    `json:"description"`
	PublishedAt       time.Time `json:"publishedAt"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

func (c *Catalog) UpsertVideo(ctx context.Context, v *Video) error {
	existed := c.videoExists(ctx, v.ID)
	v.ContentHash = normalizeContentHash(v.ContentHash)
	tagsJSON, _ := json.Marshal(v.Tags)
	badgesJSON, _ := json.Marshal(v.Badges)
	now := time.Now().UnixMilli()
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.UnixMilli(now)
	}
	v.UpdatedAt = time.UnixMilli(now)

	_, err := c.db.ExecContext(ctx, `
INSERT INTO videos (
  id, drive_id, file_id, file_name, content_hash, parent_id, title, author, tags,
  duration_seconds, size_bytes, ext, quality, thumbnail_url, thumbnail_status,
  preview_file_id, preview_local, preview_status,
  views, favorites, comments, likes, dislikes,
  category, hidden, frontend_selected, badges, description, published_at, created_at, updated_at
) VALUES (
  ?, ?, ?, ?, ?, ?, ?, ?, ?,
  ?, ?, ?, ?, ?, CASE WHEN COALESCE(?, '') != '' THEN 'ready' ELSE 'pending' END,
  ?, ?, ?,
  ?, ?, ?, ?, ?,
  ?, ?, ?, ?, ?, ?, ?, ?
)
ON CONFLICT(id) DO UPDATE SET
  file_name       = CASE
                      WHEN excluded.file_name != '' THEN excluded.file_name
                      ELSE videos.file_name
                    END,
  title           = excluded.title,
  author          = excluded.author,
  tags            = excluded.tags,
  content_hash    = CASE
                      WHEN excluded.content_hash != '' THEN excluded.content_hash
                      ELSE videos.content_hash
                    END,
  sampled_sha256  = CASE
                      WHEN videos.size_bytes != excluded.size_bytes THEN ''
                      ELSE videos.sampled_sha256
                    END,
  fingerprint_status = CASE
                      WHEN videos.size_bytes != excluded.size_bytes THEN 'pending'
                      ELSE COALESCE(videos.fingerprint_status, 'pending')
                    END,
  fingerprint_error = CASE
                      WHEN videos.size_bytes != excluded.size_bytes THEN ''
                      ELSE COALESCE(videos.fingerprint_error, '')
                    END,
  duration_seconds= excluded.duration_seconds,
  size_bytes      = excluded.size_bytes,
  ext             = excluded.ext,
  quality         = excluded.quality,
  thumbnail_url   = excluded.thumbnail_url,
  -- thumbnail_url 写非空就意味着文件已就绪（要么 worker 抽帧填的本地 /p/thumb/<id>，
  -- 要么网盘 API 直接给的远程 URL，要么管理员手动指定）。同步把 status 标 'ready'，
  -- 避免出现 "url 非空 + status='pending'" 的脏状态。url 被改成空（本调用不发生，
  -- 走 clearVolatileOneDriveThumbnails 直 SQL）保留原状态。
  thumbnail_status= CASE
                      WHEN COALESCE(excluded.thumbnail_url, '') != '' THEN 'ready'
                      ELSE videos.thumbnail_status
                    END,
  category        = excluded.category,
  badges          = excluded.badges,
  description     = excluded.description,
  updated_at      = excluded.updated_at
`,
		v.ID, v.DriveID, v.FileID, v.FileName, v.ContentHash, v.ParentID, v.Title, v.Author, string(tagsJSON),
		v.DurationSeconds, v.Size, v.Ext, v.Quality, v.ThumbnailURL, v.ThumbnailURL,
		v.PreviewFileID, v.PreviewLocal, nullableStatus(v.PreviewStatus),
		v.Views, v.Favorites, v.Comments, v.Likes, v.Dislikes,
		v.Category, boolToInt(v.Hidden), boolToInt(v.FrontendSelected), string(badgesJSON), v.Description,
		v.PublishedAt.UnixMilli(), v.CreatedAt.UnixMilli(), v.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		return err
	}
	if len(v.Tags) > 0 && !existed {
		return c.replaceVideoTags(ctx, v.ID, v.Tags, "auto", false, true)
	}
	return nil
}

func nullableStatus(s string) string {
	if s == "" {
		return "pending"
	}
	return s
}

func (c *Catalog) UpdatePreview(ctx context.Context, id, previewLocal, status string) error {
	_, err := c.db.ExecContext(ctx,
		`UPDATE videos SET preview_file_id = '', preview_local = ?, preview_status = ?, updated_at = ? WHERE id = ?`,
		previewLocal, status, time.Now().UnixMilli(), id)
	return err
}

func (c *Catalog) HideVideo(ctx context.Context, id string) error {
	res, err := c.db.ExecContext(ctx,
		`UPDATE videos SET hidden = 1, updated_at = ? WHERE id = ?`,
		time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (c *Catalog) SetVideoFrontendSelected(ctx context.Context, id string, selected bool) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return sql.ErrNoRows
	}
	if selected {
		accessible, err := c.videoHasFrontendAccessTag(ctx, id)
		if err != nil {
			return err
		}
		if !accessible {
			return ErrVideoOutsideFrontendTags
		}
	}
	res, err := c.db.ExecContext(ctx,
		`UPDATE videos SET frontend_selected = ?, updated_at = ? WHERE id = ?`,
		boolToInt(selected), time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (c *Catalog) IsFrontendAccessible(ctx context.Context, id string) (bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return false, nil
	}
	var n int
	err := c.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM videos WHERE id = ? AND `+frontendVisibleWhereSQL,
		id,
	).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (c *Catalog) videoHasFrontendAccessTag(ctx context.Context, id string) (bool, error) {
	var n int
	err := c.db.QueryRowContext(ctx, `
SELECT COUNT(*)
  FROM video_tags vt
  JOIN tags t ON t.id = vt.tag_id
 WHERE vt.video_id = ?
   AND COALESCE(t.frontend_access, 0) = 1`, id).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MigrateVideoToDrive 把 catalog 里 id=videoID 这条视频迁移到另一个 drive。
// 用于 spider91 → PikPak 的迁移：上传成功后改写 drive_id / file_id /
// content_hash，保留视频自身的 id（spider91-<driveID>-<sourceID>），这样
// 关联表 (video_tags / 收藏 / 点赞) 都不需要动。
//
// scanner 后续看到 PikPak 目录下相同 hash / file_name 的文件时，会通过
// findDuplicate 命中本行，不会再插入重复行。
func (c *Catalog) MigrateVideoToDrive(ctx context.Context, videoID, newDriveID, newFileID, newContentHash string) error {
	if videoID == "" || newDriveID == "" || newFileID == "" {
		return fmt.Errorf("catalog: migrate video: empty id/drive/file")
	}
	res, err := c.db.ExecContext(ctx,
		`UPDATE videos
		   SET drive_id     = ?,
		       file_id      = ?,
		       content_hash = CASE WHEN ? != '' THEN ? ELSE content_hash END,
		       updated_at   = ?
		 WHERE id = ?`,
		newDriveID, newFileID, newContentHash, newContentHash, time.Now().UnixMilli(), videoID)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListVideosByDriveID 列出指定 drive 下所有未隐藏的视频，按 published_at 倒序。
// 给 spider91 → 115/PikPak 迁移 worker 用：扫描 spider91 drive 下所有视频，
// 检查哪些还有本地文件，依次上传到目标盘。
func (c *Catalog) ListVideosByDriveID(ctx context.Context, driveID string, limit int) ([]*Video, error) {
	if driveID == "" {
		return nil, fmt.Errorf("catalog: list videos by drive: empty drive id")
	}
	if limit <= 0 {
		limit = 10000
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+allVideoCols+` FROM videos
		 WHERE drive_id = ? AND COALESCE(hidden, 0) = 0
		 ORDER BY published_at DESC
		 LIMIT ?`,
		driveID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// IncrementLike 原子 +1，返回最新点赞数
func (c *Catalog) IncrementLike(ctx context.Context, id string) (int, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE videos SET likes = likes + 1, updated_at = ? WHERE id = ?`,
		time.Now().UnixMilli(), id); err != nil {
		return 0, err
	}
	var likes int
	if err := tx.QueryRowContext(ctx, `SELECT likes FROM videos WHERE id = ?`, id).Scan(&likes); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return likes, nil
}

// DecrementLike 原子 -1（不会减到负数），返回最新点赞数。
// 视频不存在时返回 sql.ErrNoRows。
func (c *Catalog) DecrementLike(ctx context.Context, id string) (int, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE videos SET likes = MAX(likes - 1, 0), updated_at = ? WHERE id = ?`,
		time.Now().UnixMilli(), id)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, sql.ErrNoRows
	}
	var likes int
	if err := tx.QueryRowContext(ctx, `SELECT likes FROM videos WHERE id = ?`, id).Scan(&likes); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return likes, nil
}

// IncrementView 原子 +1，返回最新观看数。视频不存在时返回 sql.ErrNoRows。
func (c *Catalog) IncrementView(ctx context.Context, id string) (int, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE videos SET views = views + 1, updated_at = ? WHERE id = ?`,
		time.Now().UnixMilli(), id)
	if err != nil {
		return 0, err
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return 0, sql.ErrNoRows
	}
	var views int
	if err := tx.QueryRowContext(ctx, `SELECT views FROM videos WHERE id = ?`, id).Scan(&views); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return views, nil
}

// VideoMetaPatch 轻量更新视频元数据（仅非零值字段会被写入）
type VideoMetaPatch struct {
	ThumbnailURL           string
	ThumbnailStatus        string
	ResetThumbnailFailures bool
	DurationSeconds        int
	Category               string
	ContentHash            string
	FileName               string
	Tags                   []string
	TagsSet                bool
}

func (c *Catalog) UpdateVideoMeta(ctx context.Context, id string, p VideoMetaPatch) error {
	parts := []string{}
	args := []any{}
	if p.ThumbnailURL != "" {
		parts = append(parts, "thumbnail_url = ?")
		args = append(args, p.ThumbnailURL)
	}
	switch {
	case p.ThumbnailStatus != "":
		// 调用方显式指定 status —— 信任之；典型是 worker 把状态置 'failed' 或
		// 在重试时显式置 'pending'。
		status := nullableStatus(p.ThumbnailStatus)
		parts = append(parts, "thumbnail_status = ?")
		args = append(args, status)
		if status == "ready" {
			p.ResetThumbnailFailures = true
		}
	case p.ThumbnailURL != "":
		// 调用方写了 url 但没显式给 status —— 视为"封面就绪"。url 非空意味着
		// 浏览器访问那个 URL 能拿到图（要么是本地 /p/thumb/<id>，要么是网盘 API
		// 直接返回的远程 URL）。同步把 status 标 'ready'，避免 url 非空但 status
		// 仍是 'pending' 的脏状态（修过的历史 bug）。
		parts = append(parts, "thumbnail_status = ?")
		args = append(args, nullableStatus("ready"))
		p.ResetThumbnailFailures = true
	}
	if p.ResetThumbnailFailures {
		parts = append(parts, "thumbnail_failures = 0")
	}
	if p.DurationSeconds > 0 {
		parts = append(parts, "duration_seconds = ?")
		args = append(args, p.DurationSeconds)
	}
	if p.Category != "" {
		parts = append(parts, "category = ?")
		args = append(args, p.Category)
	}
	if p.ContentHash != "" {
		parts = append(parts, "content_hash = ?")
		args = append(args, normalizeContentHash(p.ContentHash))
	}
	if p.FileName != "" {
		parts = append(parts, "file_name = ?")
		args = append(args, p.FileName)
	}
	if p.TagsSet {
		tagsJSON, _ := json.Marshal(p.Tags)
		parts = append(parts, "tags = ?")
		args = append(args, string(tagsJSON))
	}
	if len(parts) == 0 {
		return nil
	}
	parts = append(parts, "updated_at = ?")
	args = append(args, time.Now().UnixMilli())
	args = append(args, id)
	q := `UPDATE videos SET ` + strings.Join(parts, ", ") + ` WHERE id = ?`
	if _, err := c.db.ExecContext(ctx, q, args...); err != nil {
		return err
	}
	if p.TagsSet {
		return c.SetAutoVideoTags(ctx, id, p.Tags)
	}
	return nil
}

func (c *Catalog) IncrementThumbnailFailures(ctx context.Context, id string) (int, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE videos
		    SET thumbnail_failures = COALESCE(thumbnail_failures, 0) + 1,
		        updated_at = ?
		  WHERE id = ?`,
		time.Now().UnixMilli(), id)
	if err != nil {
		return 0, err
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return 0, sql.ErrNoRows
	}

	var failures int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(thumbnail_failures, 0) FROM videos WHERE id = ?`,
		id).Scan(&failures); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return failures, nil
}

// ListCategories 聚合所有 category，按视频数降序
type CategoryStat struct {
	Category string
	Count    int
}

func (c *Catalog) ListCategories(ctx context.Context) ([]CategoryStat, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT COALESCE(category, '') AS c, COUNT(*) AS cnt
		 FROM videos
		 WHERE category IS NOT NULL AND category != ''
		   AND COALESCE(hidden, 0) = 0
		 GROUP BY c
		 ORDER BY cnt DESC, c ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CategoryStat
	for rows.Next() {
		var s CategoryStat
		if err := rows.Scan(&s.Category, &s.Count); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

type TagStat struct {
	Label string
	Count int
}

func (c *Catalog) CountTags(ctx context.Context, labels []string) ([]TagStat, error) {
	out := make([]TagStat, 0, len(labels))
	for _, label := range labels {
		var count int
		if err := c.db.QueryRowContext(ctx,
			`SELECT COUNT(*)
			 FROM video_tags vt
			 JOIN tags t ON t.id = vt.tag_id
			 JOIN videos v ON v.id = vt.video_id
			 WHERE t.label = ? COLLATE NOCASE
			   AND COALESCE(v.hidden, 0) = 0`,
			label,
		).Scan(&count); err != nil {
			return nil, err
		}
		out = append(out, TagStat{Label: label, Count: count})
	}
	return out, nil
}

// ListVideosByPreviewStatus 按预览状态列出全部视频，通常用于启动补扫
func (c *Catalog) ListVideosByPreviewStatus(ctx context.Context, driveID, status string, limit int) ([]*Video, error) {
	if limit <= 0 {
		limit = 10000
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+allVideoCols+` FROM videos
		 WHERE drive_id = ? AND preview_status = ?
		   AND COALESCE(hidden, 0) = 0
		   AND `+uniqueVideoWhereSQL+`
		 ORDER BY created_at ASC LIMIT ?`,
		driveID, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// ListVideosByThumbnailStatus 按封面（thumbnail）状态列出某 drive 下的视频。
//
// 与 ListVideosByPreviewStatus 的区别在 status 字段名：封面用 thumbnail_status，
// 预览用 preview_status；两个 worker 是独立的。本接口主要用于 admin "重生失败
// 封面"操作 —— 把状态为 failed 的封面挑出来重新入队。
func (c *Catalog) ListVideosByThumbnailStatus(ctx context.Context, driveID, status string, limit int) ([]*Video, error) {
	if limit <= 0 {
		limit = 10000
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+allVideoCols+` FROM videos
		 WHERE drive_id = ? AND COALESCE(thumbnail_status, 'pending') = ?
		   AND COALESCE(hidden, 0) = 0
		   AND `+uniqueVideoWhereSQL+`
		 ORDER BY created_at ASC LIMIT ?`,
		driveID, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// ListVideosNeedingThumbnail returns videos that still need thumbnail-worker work.
// Besides missing thumbnails, this includes videos with an existing thumbnail but
// missing duration metadata, because the thumbnail worker probes duration while
// it already has a stream link.
// Failed thumbnails are reported separately and should not block preview-video generation.
// Videos whose local assets were cleared because they are fingerprint duplicates
// stay pending in the DB, but uniqueVideoWhereSQL keeps them out of this queue
// while their canonical sibling still exists.
func (c *Catalog) ListVideosNeedingThumbnail(ctx context.Context, driveID string, limit int) ([]*Video, error) {
	if limit <= 0 {
		limit = 10000
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+allVideoCols+` FROM videos
		 WHERE drive_id = ?
		   AND (
		        COALESCE(thumbnail_url, '') = ''
		        OR COALESCE(duration_seconds, 0) <= 0
		   )
		   AND COALESCE(thumbnail_status, 'pending') NOT IN ('failed', 'skipped')
		   AND COALESCE(hidden, 0) = 0
		   AND `+uniqueVideoWhereSQL+`
		 ORDER BY created_at ASC
		 LIMIT ?`,
		driveID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (c *Catalog) CountVideosNeedingThumbnail(ctx context.Context, driveID string) (int, error) {
	var count int
	err := c.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM videos
		 WHERE drive_id = ?
		   AND (
		        COALESCE(thumbnail_url, '') = ''
		        OR COALESCE(duration_seconds, 0) <= 0
		   )
		   AND COALESCE(thumbnail_status, 'pending') NOT IN ('failed', 'skipped')
		   AND COALESCE(hidden, 0) = 0
		   AND `+uniqueVideoWhereSQL,
		driveID).Scan(&count)
	return count, err
}

func (c *Catalog) GetVideo(ctx context.Context, id string) (*Video, error) {
	row := c.db.QueryRowContext(ctx, `SELECT `+allVideoCols+` FROM videos WHERE id = ?`, id)
	return scanVideo(row)
}

func (c *Catalog) ListVideosByDrive(ctx context.Context, driveID string) ([]*Video, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+allVideoCols+` FROM videos WHERE drive_id = ? ORDER BY created_at ASC, id ASC`,
		driveID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (c *Catalog) ListVideosByIDPrefix(ctx context.Context, prefix string) ([]*Video, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil, fmt.Errorf("catalog: list videos by id prefix: empty prefix")
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+allVideoCols+` FROM videos
		 WHERE SUBSTR(id, 1, LENGTH(?)) = ?
		 ORDER BY created_at ASC, id ASC`,
		prefix, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (c *Catalog) ListVideosWithMissingDrive(ctx context.Context) ([]*Video, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+allVideoCols+` FROM videos
		 WHERE drive_id != 'local-upload'
		   AND NOT EXISTS (
		       SELECT 1
		         FROM drives
		        WHERE drives.id = videos.drive_id
		   )
		 ORDER BY drive_id ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ListVideoFileIDsByDrive 只返回某 drive 下所有视频的 file_id 集合，
// 比 ListVideosByDrive 轻量。
func (c *Catalog) ListVideoFileIDsByDrive(ctx context.Context, driveID string) ([]string, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT file_id FROM videos WHERE drive_id = ? AND file_id != ''`,
		driveID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var fid string
		if err := rows.Scan(&fid); err != nil {
			return nil, err
		}
		out = append(out, fid)
	}
	return out, rows.Err()
}

// ListSpider91Viewkeys 列出某个 spider91 drive 历史上爬过的所有 ID 后缀。
// 函数名保留历史叫法；新 spider91 数据的后缀是 91 mp4 源 ID，不再是 viewkey。
//
// 不能再用 ListVideoFileIDsByDrive：那个只看 drive_id，但 spider91 视频
// 一旦被 spider91migrate 迁移到 PikPak，drive_id 就变成 PikPak 了。
//
// 这里按 video.id 前缀 "spider91-<driveID>-" 查，即使迁移后视频也仍能被
// 找到——id 本身会保留 "spider91-<driveID>-<sourceID>" 这个来源前缀。
//
// 用途：crawler 把这个集合写到 seen 文件，让 Python/Go 跳过已爬过的视频，
// 配合 --target-new 真正凑出 N 个未爬过的视频。
func (c *Catalog) ListSpider91Viewkeys(ctx context.Context, driveID string) ([]string, error) {
	prefix := "spider91-" + driveID + "-"
	rows, err := c.db.QueryContext(ctx,
		`SELECT SUBSTR(id, ?) FROM videos WHERE id LIKE ? || '%'`,
		len(prefix)+1, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var vk string
		if err := rows.Scan(&vk); err != nil {
			return nil, err
		}
		if vk = strings.TrimSpace(vk); vk != "" {
			out = append(out, vk)
		}
	}
	return out, rows.Err()
}

func (c *Catalog) DeleteVideo(ctx context.Context, id string) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 先记录这次视频关联的 tag_id，便于事务末尾清理孤儿 collection 标签
	tagIDs, err := collectVideoTagIDs(ctx, tx, id)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM video_tags WHERE video_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM videos WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}

	// collection 标签是 scanner 按目录名机器生成的；视频删完后若不再被引用就一起回收。
	// system / user / auto / legacy 不在此处删除，避免破坏管理员手动维护的标签语义。
	if err := pruneOrphanCollectionTagsByID(ctx, tx, tagIDs); err != nil {
		return err
	}

	return tx.Commit()
}

func (c *Catalog) FindVideoByContentHash(ctx context.Context, hash string) (*Video, error) {
	hash = normalizeContentHash(hash)
	if hash == "" {
		return nil, sql.ErrNoRows
	}
	row := c.db.QueryRowContext(ctx,
		`SELECT `+allVideoCols+`
		 FROM videos
		 WHERE content_hash = ?
		 ORDER BY created_at ASC, id ASC
		 LIMIT 1`, hash)
	return scanVideo(row)
}

func (c *Catalog) FindVideoByFileSignature(ctx context.Context, fileName string, size int64) (*Video, error) {
	if fileName == "" || size <= 0 {
		return nil, sql.ErrNoRows
	}
	row := c.db.QueryRowContext(ctx,
		`SELECT `+allVideoCols+`
		 FROM videos
		 WHERE file_name = ? AND size_bytes = ?
		 ORDER BY created_at ASC, id ASC
		 LIMIT 1`, fileName, size)
	return scanVideo(row)
}

func (c *Catalog) ListVideosNeedingFingerprint(ctx context.Context, driveID string, limit int) ([]*Video, error) {
	if limit <= 0 {
		limit = 10000
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+allVideoCols+` FROM videos
		 WHERE drive_id = ?
		   AND size_bytes > 0
		   AND COALESCE(sampled_sha256, '') = ''
		   AND COALESCE(fingerprint_status, 'pending') = 'pending'
		   AND COALESCE(hidden, 0) = 0
		 ORDER BY created_at ASC, id ASC
		 LIMIT ?`,
		driveID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ListVideosByFingerprintStatus lists visible videos on a drive by fingerprint status.
// It is used by the admin "retry failed fingerprints" action to reset failed rows
// back to pending and enqueue them again.
func (c *Catalog) ListVideosByFingerprintStatus(ctx context.Context, driveID, status string, limit int) ([]*Video, error) {
	if limit <= 0 {
		limit = 10000
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+allVideoCols+` FROM videos
		 WHERE drive_id = ?
		   AND COALESCE(sampled_sha256, '') = ''
		   AND COALESCE(fingerprint_status, 'pending') = ?
		   AND COALESCE(hidden, 0) = 0
		 ORDER BY created_at ASC, id ASC
		 LIMIT ?`,
		driveID, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (c *Catalog) UpdateVideoFingerprint(ctx context.Context, id, sampledSHA256, status, errText string) error {
	sampledSHA256 = normalizeContentHash(sampledSHA256)
	if status == "" {
		status = "pending"
	}
	if len(errText) > 500 {
		errText = errText[:500]
	}
	res, err := c.db.ExecContext(ctx,
		`UPDATE videos
		    SET sampled_sha256 = ?,
		        fingerprint_status = ?,
		        fingerprint_error = ?,
		        updated_at = ?
		  WHERE id = ?`,
		sampledSHA256, status, errText, time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

type ListParams struct {
	Keyword               string
	DriveID               string
	Tag                   string
	Category              string
	Sort                  string // latest | hot | week | long
	ThumbnailReadyOnly    bool
	PreferReadyThumbnails bool
	FrontendOnly          bool
	SkipTotal             bool
	Page                  int
	PageSize              int
}

func (c *Catalog) ListVideos(ctx context.Context, p ListParams) ([]*Video, int, error) {
	if p.PageSize <= 0 {
		p.PageSize = 24
	}
	if p.Page <= 0 {
		p.Page = 1
	}

	var where []string
	var args []any
	if p.Keyword != "" {
		where = append(where, "(title LIKE ? OR author LIKE ?)")
		like := "%" + p.Keyword + "%"
		args = append(args, like, like)
	}
	if p.DriveID != "" {
		where = append(where, "drive_id = ?")
		args = append(args, p.DriveID)
	}
	if p.Tag != "" {
		where = append(where, videoMatchesTagLabelSQL("videos"))
		args = append(args, p.Tag)
	}
	if p.Category != "" && p.Category != "all" {
		where = append(where, "category = ?")
		args = append(args, p.Category)
	}
	if p.ThumbnailReadyOnly {
		where = append(where, "COALESCE(thumbnail_url, '') != ''")
	}
	if p.FrontendOnly {
		where = append(where, frontendVisibleWhereSQL)
	} else {
		where = append(where, "COALESCE(hidden, 0) = 0")
	}
	where = append(where, activeDriveWhereSQL)
	where = append(where, uniqueVideoWhereSQL)

	whereSQL := ""
	whereSQL = " WHERE " + strings.Join(where, " AND ")

	readyOrderPrefix := ""
	if p.PreferReadyThumbnails {
		readyOrderPrefix = "CASE WHEN COALESCE(thumbnail_url, '') != '' THEN 0 ELSE 1 END, "
	}

	orderBy := " ORDER BY " + readyOrderPrefix + "published_at DESC"
	switch p.Sort {
	case "hot":
		// 热度 = 点赞数，点赞相同按最新
		orderBy = " ORDER BY " + readyOrderPrefix + "likes DESC, published_at DESC"
	case "week":
		orderBy = " ORDER BY " + readyOrderPrefix + "likes DESC"
	case "long":
		orderBy = " ORDER BY " + readyOrderPrefix + "duration_seconds DESC"
	}

	var total int
	if !p.SkipTotal {
		if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM videos"+whereSQL, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
	}

	// list
	offset := (p.Page - 1) * p.PageSize
	rows, err := c.db.QueryContext(ctx,
		"SELECT "+allVideoCols+" FROM videos"+whereSQL+orderBy+" LIMIT ? OFFSET ?",
		append(args, p.PageSize, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []*Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, v)
	}
	return out, total, nil
}

// CountVisibleVideos 返回当前对前台可见的视频总数（未隐藏、且通过去重规则）。
// 用于短视频模式判断"已经轮过一遍"。
func (c *Catalog) CountVisibleVideos(ctx context.Context) (int, error) {
	var total int
	err := c.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM videos
		  WHERE `+frontendVisibleWhereSQL+`
		    AND `+activeDriveWhereSQL+`
		    AND `+uniqueVideoWhereSQL,
	).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total, nil
}

// RandomVideosExcluding 从对前台可见的视频里，随机返回 limit 个不在 excludeIDs 中的视频。
// 短视频模式用：客户端把当前轮已看的视频 id 传过来，避免本轮重复。
// 如果剩余可选数量 < limit，就返回所有可选项；调用方负责判断是否需要开新一轮。
// limit <= 0 时返回 nil, nil。
func (c *Catalog) RandomVideosExcluding(ctx context.Context, excludeIDs []string, limit int) ([]*Video, error) {
	return c.randomVideosExcluding(ctx, excludeIDs, limit, false)
}

func (c *Catalog) RandomVideosWithReadyThumbnailsExcluding(ctx context.Context, excludeIDs []string, limit int) ([]*Video, error) {
	return c.randomVideosExcluding(ctx, excludeIDs, limit, true)
}

func (c *Catalog) randomVideosExcluding(ctx context.Context, excludeIDs []string, limit int, thumbnailReadyOnly bool) ([]*Video, error) {
	if limit <= 0 {
		return nil, nil
	}

	cleaned := cleanVideoIDs(excludeIDs)
	args := make([]any, 0, len(cleaned)+1)
	whereSQL := `WHERE ` + frontendVisibleWhereSQL + `
		           AND ` + activeDriveWhereSQL + `
		           AND ` + uniqueVideoWhereSQL
	if thumbnailReadyOnly {
		whereSQL += " AND COALESCE(thumbnail_url, '') != ''"
	}
	if len(cleaned) > 0 {
		placeholders := strings.Repeat("?,", len(cleaned))
		placeholders = placeholders[:len(placeholders)-1]
		whereSQL += " AND id NOT IN (" + placeholders + ")"
		for _, id := range cleaned {
			args = append(args, id)
		}
	}
	args = append(args, limit)

	rows, err := c.db.QueryContext(ctx,
		`SELECT `+allVideoCols+` FROM videos `+whereSQL+`
		 ORDER BY RANDOM() LIMIT ?`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func cleanVideoIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	cleaned := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		cleaned = append(cleaned, id)
	}
	return cleaned
}

func cleanTagLabels(labels []string) []string {
	seen := make(map[string]struct{}, len(labels))
	cleaned := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		key := strings.ToLower(label)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		cleaned = append(cleaned, label)
	}
	return cleaned
}

func (c *Catalog) LeastPopulatedVisibleUniqueTag(ctx context.Context, labels []string) (string, error) {
	cleaned := cleanTagLabels(labels)
	bestLabel := ""
	bestCount := 0
	for _, label := range cleaned {
		var count int
		if err := c.db.QueryRowContext(ctx,
			`SELECT COUNT(*)
			   FROM videos
			  WHERE `+frontendVisibleWhereSQL+`
			    AND `+activeDriveWhereSQL+`
			    AND `+uniqueVideoWhereSQL+`
			    AND EXISTS (

			      SELECT 1
			        FROM video_tags vt
			        JOIN tags t ON t.id = vt.tag_id
			       WHERE vt.video_id = videos.id
			         AND t.label = ? COLLATE NOCASE
			    )`,
			label,
		).Scan(&count); err != nil {
			return "", err
		}
		if count == 0 {
			continue
		}
		if bestLabel == "" || count < bestCount {
			bestLabel = label
			bestCount = count
		}
	}
	return bestLabel, nil
}

func (c *Catalog) RandomVideosByTagExcluding(ctx context.Context, tag string, excludeIDs []string, limit int) ([]*Video, error) {
	if limit <= 0 {
		return nil, nil
	}
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return nil, nil
	}

	cleaned := cleanVideoIDs(excludeIDs)
	args := make([]any, 0, len(cleaned)+2)
	args = append(args, tag)
	whereSQL := `WHERE ` + frontendVisibleWhereSQL + `
		           AND ` + activeDriveWhereSQL + `
		           AND ` + uniqueVideoWhereSQL + `
		           AND EXISTS (
		             SELECT 1
		               FROM video_tags vt
		               JOIN tags t ON t.id = vt.tag_id
		              WHERE vt.video_id = videos.id
		                AND t.label = ? COLLATE NOCASE
		           )`
	if len(cleaned) > 0 {
		placeholders := strings.Repeat("?,", len(cleaned))
		placeholders = placeholders[:len(placeholders)-1]
		whereSQL += " AND id NOT IN (" + placeholders + ")"
		for _, id := range cleaned {
			args = append(args, id)
		}
	}
	args = append(args, limit)

	rows, err := c.db.QueryContext(ctx,
		`SELECT `+allVideoCols+` FROM videos `+whereSQL+`
		 ORDER BY RANDOM() LIMIT ?`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Catalog) RandomVideosForPreferredVideoExcluding(ctx context.Context, preferredVideoID string, excludeIDs []string, limit int) ([]*Video, error) {
	if limit <= 0 {
		return nil, nil
	}
	preferredVideoID = strings.TrimSpace(preferredVideoID)
	if preferredVideoID == "" {
		return c.RandomVideosExcluding(ctx, excludeIDs, limit)
	}

	preferredExclude := append([]string{}, excludeIDs...)
	preferredExclude = append(preferredExclude, preferredVideoID)

	preferred, err := c.GetVideo(ctx, preferredVideoID)
	if err != nil || preferred == nil || preferred.Hidden || len(preferred.Tags) == 0 {
		return c.RandomVideosExcluding(ctx, preferredExclude, limit)
	}
	tag, err := c.LeastPopulatedVisibleUniqueTag(ctx, preferred.Tags)
	if err != nil {
		return nil, err
	}
	if tag == "" {
		return c.RandomVideosExcluding(ctx, preferredExclude, limit)
	}

	items, err := c.RandomVideosByTagExcluding(ctx, tag, preferredExclude, limit)
	if err != nil {
		return nil, err
	}
	if len(items) >= limit {
		return items, nil
	}

	mergedExclude := make([]string, 0, len(preferredExclude)+len(items))
	mergedExclude = append(mergedExclude, preferredExclude...)
	for _, item := range items {
		if item != nil {
			mergedExclude = append(mergedExclude, item.ID)
		}
	}
	fallback, err := c.RandomVideosExcluding(ctx, mergedExclude, limit-len(items))
	if err != nil {
		return nil, err
	}
	return append(items, fallback...), nil
}

type DriveTeaserCounts struct {
	Ready   int
	Pending int
	Failed  int
}

type DriveThumbnailCounts struct {
	Ready           int
	Pending         int
	Failed          int
	DurationPending int
}

type DriveFingerprintCounts struct {
	Ready   int
	Pending int
	Failed  int
}

func (c *Catalog) CountTeasersByDrive(ctx context.Context) (map[string]DriveTeaserCounts, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT drive_id,
		        COUNT(CASE WHEN COALESCE(preview_status, 'pending') = 'ready' THEN 1 END) AS ready_count,
		        COUNT(CASE WHEN COALESCE(preview_status, 'pending') = 'pending' THEN 1 END) AS pending_count,
		        COUNT(CASE WHEN COALESCE(preview_status, 'pending') = 'failed' THEN 1 END) AS failed_count
		   FROM videos
		  WHERE COALESCE(hidden, 0) = 0
		    AND `+uniqueVideoWhereSQL+`
		  GROUP BY drive_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]DriveTeaserCounts)
	for rows.Next() {
		var driveID string
		var counts DriveTeaserCounts
		if err := rows.Scan(&driveID, &counts.Ready, &counts.Pending, &counts.Failed); err != nil {
			return nil, err
		}
		out[driveID] = counts
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Catalog) CountThumbnailsByDrive(ctx context.Context) (map[string]DriveThumbnailCounts, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT drive_id,
		        COUNT(CASE WHEN COALESCE(thumbnail_url, '') != '' THEN 1 END) AS ready_count,
		        COUNT(CASE WHEN COALESCE(thumbnail_url, '') = ''
		                     AND COALESCE(thumbnail_status, 'pending') NOT IN ('failed', 'skipped') THEN 1 END) AS pending_count,
		        COUNT(CASE WHEN COALESCE(thumbnail_url, '') = ''
		                     AND COALESCE(thumbnail_status, 'pending') = 'failed' THEN 1 END) AS failed_count,
		        COUNT(CASE WHEN COALESCE(thumbnail_url, '') != ''
		                     AND COALESCE(duration_seconds, 0) <= 0
		                     AND COALESCE(thumbnail_status, 'pending') NOT IN ('failed', 'skipped') THEN 1 END) AS duration_pending_count
		   FROM videos
		  WHERE COALESCE(hidden, 0) = 0
		    AND `+uniqueVideoWhereSQL+`
		  GROUP BY drive_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]DriveThumbnailCounts)
	for rows.Next() {
		var driveID string
		var counts DriveThumbnailCounts
		if err := rows.Scan(&driveID, &counts.Ready, &counts.Pending, &counts.Failed, &counts.DurationPending); err != nil {
			return nil, err
		}
		out[driveID] = counts
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Catalog) CountFingerprintsByDrive(ctx context.Context) (map[string]DriveFingerprintCounts, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT drive_id,
		        COUNT(CASE WHEN COALESCE(sampled_sha256, '') != ''
		                      OR COALESCE(fingerprint_status, 'pending') = 'ready' THEN 1 END) AS ready_count,
		        COUNT(CASE WHEN size_bytes > 0
		                     AND COALESCE(sampled_sha256, '') = ''
		                     AND COALESCE(fingerprint_status, 'pending') = 'pending' THEN 1 END) AS pending_count,
		        COUNT(CASE WHEN COALESCE(sampled_sha256, '') = ''
		                     AND COALESCE(fingerprint_status, 'pending') = 'failed' THEN 1 END) AS failed_count
		   FROM videos
		  WHERE COALESCE(hidden, 0) = 0
		  GROUP BY drive_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]DriveFingerprintCounts)
	for rows.Next() {
		var driveID string
		var counts DriveFingerprintCounts
		if err := rows.Scan(&driveID, &counts.Ready, &counts.Pending, &counts.Failed); err != nil {
			return nil, err
		}
		out[driveID] = counts
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Catalog) CountVideosNeedingFingerprint(ctx context.Context, driveID string) (int, error) {
	var count int
	err := c.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM videos
		 WHERE drive_id = ?
		   AND size_bytes > 0
		   AND COALESCE(sampled_sha256, '') = ''
		   AND COALESCE(fingerprint_status, 'pending') = 'pending'
		   AND COALESCE(hidden, 0) = 0`,
		driveID).Scan(&count)
	return count, err
}

type LocalMediaRef struct {
	DriveID      string
	VideoID      string
	PreviewLocal string
}

func (c *Catalog) ListLocalMediaRefs(ctx context.Context) ([]LocalMediaRef, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT drive_id, id, COALESCE(preview_local, '')
		   FROM videos`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LocalMediaRef
	for rows.Next() {
		var ref LocalMediaRef
		if err := rows.Scan(&ref.DriveID, &ref.VideoID, &ref.PreviewLocal); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// DuplicateAssetCleanupCandidate points at a non-canonical video in a
// size+sampled_sha256 duplicate group that still owns generated local assets.
// The cleanup job uses this to remove duplicate thumbnails/preview videos without
// touching the original cloud file or deleting the catalog row.
type DuplicateAssetCleanupCandidate struct {
	VideoID       string
	DriveID       string
	Title         string
	PreviewLocal  string
	ThumbnailURL  string
	CanonicalID   string
	SampledSHA256 string
	Size          int64
}

// ListDuplicateAssetCleanupCandidates returns duplicate videos whose own local
// generated assets can be cleared. A group canonical is the same representative
// used by uniqueVideoWhereSQL: earliest created_at, then lexicographically
// smallest id.
func (c *Catalog) ListDuplicateAssetCleanupCandidates(ctx context.Context, limit int) ([]DuplicateAssetCleanupCandidate, error) {
	if limit <= 0 {
		limit = 10000
	}
	rows, err := c.db.QueryContext(ctx, `
WITH canonical AS (
	SELECT v.id, v.size_bytes, v.sampled_sha256
	  FROM videos v
	 WHERE v.size_bytes > 0
	   AND COALESCE(v.sampled_sha256, '') != ''
	   AND NOT EXISTS (
		 SELECT 1
		   FROM videos earlier
		  WHERE earlier.size_bytes = v.size_bytes
		    AND earlier.sampled_sha256 = v.sampled_sha256
		    AND COALESCE(earlier.sampled_sha256, '') != ''
		    AND earlier.size_bytes > 0
		    AND (
			  earlier.created_at < v.created_at
			  OR (earlier.created_at = v.created_at AND earlier.id < v.id)
		    )
	   )
)
SELECT dup.id,
       dup.drive_id,
       dup.title,
       COALESCE(dup.preview_local, ''),
       COALESCE(dup.thumbnail_url, ''),
       canonical.id,
       dup.sampled_sha256,
       dup.size_bytes
  FROM videos dup
  JOIN canonical
    ON canonical.size_bytes = dup.size_bytes
   AND canonical.sampled_sha256 = dup.sampled_sha256
 WHERE dup.id != canonical.id
   AND dup.size_bytes > 0
   AND COALESCE(dup.sampled_sha256, '') != ''
   AND (
	 COALESCE(dup.preview_local, '') != ''
	 OR COALESCE(dup.thumbnail_url, '') = '/p/thumb/' || dup.id
   )
 ORDER BY dup.created_at ASC, dup.id ASC
 LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DuplicateAssetCleanupCandidate
	for rows.Next() {
		var item DuplicateAssetCleanupCandidate
		if err := rows.Scan(
			&item.VideoID,
			&item.DriveID,
			&item.Title,
			&item.PreviewLocal,
			&item.ThumbnailURL,
			&item.CanonicalID,
			&item.SampledSHA256,
			&item.Size,
		); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ClearGeneratedAssets clears DB references to generated local assets for a
// video. The statuses go back to pending so the video can regenerate assets if
// it later becomes the canonical item after its older duplicate is removed.
func (c *Catalog) ClearGeneratedAssets(ctx context.Context, videoID string, clearPreview, clearThumbnail bool) error {
	parts := []string{}
	args := []any{}
	if clearPreview {
		parts = append(parts, "preview_file_id = ''", "preview_local = ''", "preview_status = 'pending'")
	}
	if clearThumbnail {
		parts = append(parts, "thumbnail_url = ''", "thumbnail_status = 'pending'")
	}
	if len(parts) == 0 {
		return nil
	}
	parts = append(parts, "updated_at = ?")
	args = append(args, time.Now().UnixMilli(), videoID)
	res, err := c.db.ExecContext(ctx, `UPDATE videos SET `+strings.Join(parts, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ---------- Drive ----------

type Drive struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	RootID string `json:"rootId"`
	// Deprecated: 扫描入口固定等于 RootID；字段保留用于兼容旧数据/API。
	ScanRootID  string            `json:"scanRootId"`
	Credentials map[string]string `json:"credentials,omitempty"`
	Status      string            `json:"status"`
	LastError   string            `json:"lastError,omitempty"`
	// TeaserEnabled 控制是否给本盘生成预览视频/封面。
	// 替代早期的全局 preview.enabled 开关；新建 drive 时 UpsertDrive 默认置 true。
	TeaserEnabled bool `json:"teaserEnabled"`
	// SkipDirIDs 是用户在管理后台为该盘选定的"扫描跳过目录"集合（网盘侧的目录 fileID）。
	// scanner 在 walk 时命中其中任意一个就直接 continue —— 不递归、不收集文件，也
	// 不参与 stats 统计。替代旧版硬编码"影视"目录的特例分支。
	// 含义按"目录 ID 自身"匹配，所以同名目录在不同父级下需要分别选定。
	SkipDirIDs []string  `json:"skipDirIds,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

func (c *Catalog) UpsertDrive(ctx context.Context, d *Drive) error {
	normalizeDriveRootFields(d)
	cred, _ := json.Marshal(d.Credentials)
	skipDirs := d.SkipDirIDs
	if skipDirs == nil {
		skipDirs = []string{}
	}
	skipDirsJSON, _ := json.Marshal(skipDirs)
	now := time.Now().UnixMilli()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.UnixMilli(now)
	}
	d.UpdatedAt = time.UnixMilli(now)
	_, err := c.db.ExecContext(ctx, `
INSERT INTO drives (id, kind, name, root_id, scan_root_id, credentials, status, last_error, teaser_enabled, skip_dir_ids, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  kind           = excluded.kind,
  name           = excluded.name,
  root_id        = excluded.root_id,
  scan_root_id   = excluded.scan_root_id,
  credentials    = excluded.credentials,
  status         = excluded.status,
  last_error     = excluded.last_error,
  teaser_enabled = excluded.teaser_enabled,
  skip_dir_ids   = excluded.skip_dir_ids,
  updated_at     = excluded.updated_at
`, d.ID, d.Kind, d.Name, d.RootID, d.ScanRootID, string(cred), d.Status, d.LastError, boolToInt(d.TeaserEnabled), string(skipDirsJSON),
		d.CreatedAt.UnixMilli(), d.UpdatedAt.UnixMilli())
	return err
}

func normalizeDriveRootFields(d *Drive) {
	if d == nil {
		return
	}
	d.RootID = normalizeDriveRootID(d.Kind, d.RootID)
	d.ScanRootID = d.RootID
}

func normalizeDriveRootID(kind, rootID string) string {
	rootID = strings.TrimSpace(rootID)
	switch kind {
	case "pikpak":
		if rootID == "0" {
			return ""
		}
		return rootID
	case "onedrive", "googledrive":
		if rootID == "" {
			return "root"
		}
		return rootID
	case "localstorage", "spider91":
		return "/"
	default:
		if rootID == "" {
			return "0"
		}
		return rootID
	}
}

func (c *Catalog) syncDriveScanRootIDToRootID(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx, `
UPDATE drives
   SET scan_root_id = root_id,
       updated_at = ?
 WHERE COALESCE(scan_root_id, '') != COALESCE(root_id, '')`, time.Now().UnixMilli())
	return err
}

func (c *Catalog) ListDrives(ctx context.Context) ([]*Drive, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT id, kind, name, root_id, COALESCE(scan_root_id, ''), COALESCE(credentials, '{}'), status, COALESCE(last_error, ''), COALESCE(teaser_enabled, 1), COALESCE(skip_dir_ids, '[]'), created_at, updated_at FROM drives ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Drive
	for rows.Next() {
		d := &Drive{}
		var credsStr, skipDirsStr string
		var teaserEnabled int
		var createdAt, updatedAt int64
		if err := rows.Scan(&d.ID, &d.Kind, &d.Name, &d.RootID, &d.ScanRootID, &credsStr, &d.Status, &d.LastError, &teaserEnabled, &skipDirsStr, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(credsStr), &d.Credentials)
		_ = json.Unmarshal([]byte(skipDirsStr), &d.SkipDirIDs)
		normalizeDriveRootFields(d)
		d.TeaserEnabled = teaserEnabled != 0
		d.CreatedAt = time.UnixMilli(createdAt)
		d.UpdatedAt = time.UnixMilli(updatedAt)
		out = append(out, d)
	}
	return out, nil
}

func (c *Catalog) GetDrive(ctx context.Context, id string) (*Drive, error) {
	row := c.db.QueryRowContext(ctx, `SELECT id, kind, name, root_id, COALESCE(scan_root_id, ''), COALESCE(credentials, '{}'), status, COALESCE(last_error, ''), COALESCE(teaser_enabled, 1), COALESCE(skip_dir_ids, '[]'), created_at, updated_at FROM drives WHERE id = ?`, id)
	d := &Drive{}
	var credsStr, skipDirsStr string
	var teaserEnabled int
	var createdAt, updatedAt int64
	if err := row.Scan(&d.ID, &d.Kind, &d.Name, &d.RootID, &d.ScanRootID, &credsStr, &d.Status, &d.LastError, &teaserEnabled, &skipDirsStr, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(credsStr), &d.Credentials)
	_ = json.Unmarshal([]byte(skipDirsStr), &d.SkipDirIDs)
	normalizeDriveRootFields(d)
	d.TeaserEnabled = teaserEnabled != 0
	d.CreatedAt = time.UnixMilli(createdAt)
	d.UpdatedAt = time.UnixMilli(updatedAt)
	return d, nil
}

func (c *Catalog) DeleteDrive(ctx context.Context, id string) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM drives WHERE id = ?`, id)
	return err
}

// SetDriveTeaserEnabled 切换某盘的预览视频/封面生成开关。
//
// 与 UpsertDrive 的区别：只动 teaser_enabled + updated_at 一列，不要求调用方
// 重传 kind / name / credentials 等容易踩坑的字段。
//
// drive 不存在时返回 sql.ErrNoRows，调用方可以照此返回 404。
func (c *Catalog) SetDriveTeaserEnabled(ctx context.Context, id string, enabled bool) error {
	if id == "" {
		return fmt.Errorf("catalog: set drive teaser_enabled: empty id")
	}
	res, err := c.db.ExecContext(ctx,
		`UPDATE drives SET teaser_enabled = ?, updated_at = ? WHERE id = ?`,
		boolToInt(enabled), time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetDriveSkipDirIDs 重写某盘的"扫描跳过目录"集合（直接覆盖，不做增量合并）。
//
// 与 UpsertDrive 的区别：只动 skip_dir_ids + updated_at，不要求调用方重传
// kind / name / credentials 等字段（避免管理后台保存跳过目录时把凭证误覆盖）。
//
// 入参 ids 可以是 nil 或空切片，等价于"清空跳过列表"。元素会按字符串原样存储；
// 调用方负责在保存前 trim/去重；这里只保证编码成 JSON 数组。
//
// drive 不存在时返回 sql.ErrNoRows，调用方可以照此返回 404。
func (c *Catalog) SetDriveSkipDirIDs(ctx context.Context, id string, ids []string) error {
	if id == "" {
		return fmt.Errorf("catalog: set drive skip_dir_ids: empty id")
	}
	if ids == nil {
		ids = []string{}
	}
	payload, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("catalog: marshal skip_dir_ids: %w", err)
	}
	res, err := c.db.ExecContext(ctx,
		`UPDATE drives SET skip_dir_ids = ?, updated_at = ? WHERE id = ?`,
		string(payload), time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ---------- Admin session ----------

func (c *Catalog) CreateSession(ctx context.Context, token string, ttl time.Duration) error {
	now := time.Now()
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO admin_sessions (token, created_at, expires_at) VALUES (?, ?, ?)`,
		token, now.UnixMilli(), now.Add(ttl).UnixMilli())
	return err
}

func (c *Catalog) ValidateSession(ctx context.Context, token string) (bool, error) {
	var expires int64
	err := c.db.QueryRowContext(ctx, `SELECT expires_at FROM admin_sessions WHERE token = ?`, token).Scan(&expires)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return time.Now().UnixMilli() < expires, nil
}

func (c *Catalog) DeleteSession(ctx context.Context, token string) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE token = ?`, token)
	return err
}

func (c *Catalog) BanLoginIP(ctx context.Context, ip, reason string) error {
	now := time.Now().UnixMilli()
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO banned_login_ips (ip, reason, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(ip) DO UPDATE SET reason = excluded.reason`,
		ip, reason, now)
	return err
}

func (c *Catalog) IsLoginIPBanned(ctx context.Context, ip string) (bool, error) {
	var exists int
	err := c.db.QueryRowContext(ctx, `SELECT 1 FROM banned_login_ips WHERE ip = ?`, ip).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ---------- Settings ----------

func (c *Catalog) GetSetting(ctx context.Context, key, defaultValue string) (string, error) {
	var v string
	err := c.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return defaultValue, nil
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

func (c *Catalog) SetSetting(ctx context.Context, key, value string) error {
	_, err := c.db.ExecContext(ctx, `
INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
`, key, value, time.Now().UnixMilli())
	return err
}

// ---------- helpers ----------

const allVideoCols = `
id, drive_id, file_id, COALESCE(file_name, ''), COALESCE(content_hash, ''),
COALESCE(sampled_sha256, ''), COALESCE(fingerprint_status, 'pending'), COALESCE(fingerprint_error, ''),
COALESCE(parent_id, ''), title, COALESCE(author, ''), COALESCE(tags, '[]'),
duration_seconds, size_bytes, COALESCE(ext, ''), COALESCE(quality, ''), COALESCE(thumbnail_url, ''),
COALESCE(preview_file_id, ''), COALESCE(preview_local, ''), COALESCE(preview_status, 'pending'),
views, favorites, comments, likes, dislikes,
COALESCE(category, ''), COALESCE(hidden, 0), COALESCE(frontend_selected, 0), COALESCE(badges, '[]'), COALESCE(description, ''),
published_at, created_at, updated_at
`

const frontendVisibleWhereSQL = `COALESCE(hidden, 0) = 0
	AND COALESCE(frontend_selected, 0) = 1
	AND EXISTS (
		SELECT 1
		  FROM video_tags frontend_vt
		  JOIN tags frontend_t ON frontend_t.id = frontend_vt.tag_id
		 WHERE frontend_vt.video_id = videos.id
		   AND COALESCE(frontend_t.frontend_access, 0) = 1
	)`

const activeDriveWhereSQL = `(videos.drive_id = 'local-upload'
	OR EXISTS (
		SELECT 1
		  FROM drives
		 WHERE drives.id = videos.drive_id
	)
	OR NOT EXISTS (
		SELECT 1
		  FROM drives
	))`

const uniqueVideoWhereSQL = `((COALESCE(videos.content_hash, '') = ''
		OR NOT EXISTS (
			SELECT 1
			FROM videos AS dup
			WHERE dup.content_hash = videos.content_hash
			  AND COALESCE(dup.content_hash, '') != ''
			  AND (
				dup.created_at < videos.created_at
				OR (dup.created_at = videos.created_at AND dup.id < videos.id)
			  )
		))
	AND (COALESCE(videos.sampled_sha256, '') = ''
		OR videos.size_bytes <= 0
		OR NOT EXISTS (
			SELECT 1
			FROM videos AS dup
			WHERE dup.sampled_sha256 = videos.sampled_sha256
			  AND dup.size_bytes = videos.size_bytes
			  AND COALESCE(dup.sampled_sha256, '') != ''
			  AND dup.size_bytes > 0
			  AND (
				dup.created_at < videos.created_at
				OR (dup.created_at = videos.created_at AND dup.id < videos.id)
			  )
		))
	AND (COALESCE(videos.file_name, '') = ''
		OR videos.size_bytes <= 0
		OR NOT EXISTS (
			SELECT 1
			FROM videos AS dup
			WHERE dup.file_name = videos.file_name
			  AND dup.size_bytes = videos.size_bytes
			  AND COALESCE(dup.file_name, '') != ''
			  AND dup.size_bytes > 0
			  AND (
				dup.created_at < videos.created_at
				OR (dup.created_at = videos.created_at AND dup.id < videos.id)
			  )
		)))`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanVideo(row rowScanner) (*Video, error) {
	v := &Video{}
	var tagsJSON, badgesJSON string
	var publishedAt, createdAt, updatedAt int64
	var hidden, frontendSelected int
	err := row.Scan(
		&v.ID, &v.DriveID, &v.FileID, &v.FileName, &v.ContentHash,
		&v.SampledSHA256, &v.FingerprintStatus, &v.FingerprintError,
		&v.ParentID, &v.Title, &v.Author, &tagsJSON,
		&v.DurationSeconds, &v.Size, &v.Ext, &v.Quality, &v.ThumbnailURL,
		&v.PreviewFileID, &v.PreviewLocal, &v.PreviewStatus,
		&v.Views, &v.Favorites, &v.Comments, &v.Likes, &v.Dislikes,
		&v.Category, &hidden, &frontendSelected, &badgesJSON, &v.Description,
		&publishedAt, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(tagsJSON), &v.Tags)
	_ = json.Unmarshal([]byte(badgesJSON), &v.Badges)
	v.Hidden = hidden == 1
	v.FrontendSelected = frontendSelected == 1
	v.PublishedAt = time.UnixMilli(publishedAt)
	v.CreatedAt = time.UnixMilli(createdAt)
	v.UpdatedAt = time.UnixMilli(updatedAt)
	return v, nil
}

func normalizeContentHash(hash string) string {
	return strings.ToLower(strings.TrimSpace(hash))
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
