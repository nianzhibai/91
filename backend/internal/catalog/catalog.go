package catalog

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/video-site/backend/internal/tagging"
)

//go:embed schema.sql
var schemaSQL string

type Catalog struct {
	db *sql.DB

	// matcher 缓存：按 settings 里的规则版本号失效。标签创建/修改/删除都会
	// bump 版本；Matcher() 每次调用只多花一条单行 SELECT。
	matcherMu      sync.Mutex
	matcherVersion int64
	matcher        *tagging.Matcher
}

// WriteBarrier owns a database connection with a BEGIN IMMEDIATE transaction.
// While it is held, every catalog writer is blocked at SQLite itself, including
// callers that do not participate in higher-level persistence coordination.
type WriteBarrier struct {
	conn *sql.Conn
	once sync.Once
	err  error
}

type CrawlerAssetCounts struct {
	Total       int
	Local       int
	Migrated    int
	Thumbnail   DriveThumbnailCounts
	Teaser      DriveTeaserCounts
	Fingerprint DriveFingerprintCounts
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

// BeginWriteBarrier waits for existing writers and prevents new write
// transactions while still allowing read-only queries and online snapshots.
func (c *Catalog) BeginWriteBarrier(ctx context.Context) (*WriteBarrier, error) {
	if c == nil || c.db == nil {
		return nil, errors.New("catalog: database is not open")
	}
	conn, err := c.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("catalog: reserve write barrier connection: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("catalog: begin write barrier: %w", err)
	}
	return &WriteBarrier{conn: conn}, nil
}

// Close releases the write barrier. It is safe to call more than once.
func (b *WriteBarrier) Close() error {
	if b == nil {
		return nil
	}
	b.once.Do(func() {
		if b.conn == nil {
			return
		}
		if _, err := b.conn.ExecContext(context.Background(), `ROLLBACK`); err != nil {
			b.err = fmt.Errorf("catalog: release write barrier: %w", err)
		}
		if err := b.conn.Close(); err != nil && b.err == nil {
			b.err = fmt.Errorf("catalog: close write barrier connection: %w", err)
		}
		b.conn = nil
	})
	return b.err
}

// BackupTo creates an online, transactionally consistent SQLite snapshot.
// VACUUM INTO uses SQLite's own read transaction, so WAL pages that have not
// yet been checkpointed are included. The destination must not already exist.
func (c *Catalog) BackupTo(ctx context.Context, destination string) error {
	if c == nil || c.db == nil {
		return errors.New("catalog: database is not open")
	}
	if _, err := os.Stat(destination); err == nil {
		return fmt.Errorf("catalog: backup destination already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	quoted := strings.ReplaceAll(destination, "'", "''")
	if _, err := c.db.ExecContext(ctx, "VACUUM INTO '"+quoted+"'"); err != nil {
		return fmt.Errorf("catalog: sqlite online backup: %w", err)
	}
	return nil
}

// ---------- Video ----------

type Video struct {
	ID                 string    `json:"id"`
	DriveID            string    `json:"driveId"`
	FileID             string    `json:"fileId"`
	FileName           string    `json:"fileName"`
	ContentHash        string    `json:"contentHash"`
	SampledSHA256      string    `json:"sampledSha256"`
	FingerprintStatus  string    `json:"fingerprintStatus"`
	FingerprintError   string    `json:"fingerprintError"`
	ParentID           string    `json:"parentId"`
	DirName            string    `json:"dirName"`
	Title              string    `json:"title"`
	Author             string    `json:"author"`
	Tags               []string  `json:"tags"`
	DurationSeconds    int       `json:"durationSeconds"`
	Size               int64     `json:"size"`
	Ext                string    `json:"ext"`
	Quality            string    `json:"quality"`
	ThumbnailURL       string    `json:"thumbnailUrl"`
	ThumbnailUpdatedAt time.Time `json:"thumbnailUpdatedAt"`
	PreviewFileID      string    `json:"previewFileId"`
	PreviewLocal       string    `json:"previewLocal"`
	PreviewUpdatedAt   time.Time `json:"previewUpdatedAt"`
	PreviewStatus      string    `json:"previewStatus"`
	// TranscodeStatus：浏览器兼容性转码状态。
	// ''=未检测 / pending=已入队 / ready=已转码 / skipped=无需转码 / failed=失败。
	TranscodeStatus  string    `json:"transcodeStatus"`
	TranscodeError   string    `json:"transcodeError"`
	TranscodedFileID string    `json:"transcodedFileId"`
	TranscodedSize   int64     `json:"transcodedSize"`
	Views            int       `json:"views"`
	LastViewedAt     time.Time `json:"lastViewedAt"`
	Favorites        int       `json:"favorites"`
	Comments         int       `json:"comments"`
	Likes            int       `json:"likes"`
	LastLikedAt      time.Time `json:"lastLikedAt"`
	Dislikes         int       `json:"dislikes"`
	Hidden           bool      `json:"hidden"`
	Badges           []string  `json:"badges"`
	Description      string    `json:"description"`
	PublishedAt      time.Time `json:"publishedAt"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

func (c *Catalog) UpsertVideo(ctx context.Context, v *Video) error {
	existed := c.videoExists(ctx, v.ID)
	storedTags, err := upsertVideoRow(ctx, c.db, v)
	if err != nil {
		return err
	}
	if !existed {
		if len(storedTags) > 0 {
			return c.replaceVideoTags(ctx, v.ID, storedTags, "manual", true, true)
		}
		assignments, err := c.MatchTagAssignments(ctx, v.Title, v.FileName, v.Author, v.DirName)
		if err != nil {
			return err
		}
		if len(assignments) > 0 {
			_, err = c.ReplaceAutoVideoTags(ctx, v.ID, assignments)
			return err
		}
	}
	return nil
}

type videoRowExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// upsertVideoRow owns the videos-table statement without opening a transaction.
// Ordinary scans call it through UpsertVideo; tombstone restoration calls it
// with an existing transaction so the row, tag assignments, and tombstone can
// move to their new state atomically.
func upsertVideoRow(ctx context.Context, exec videoRowExecer, v *Video) ([]string, error) {
	v.ContentHash = normalizeContentHash(v.ContentHash)
	v.SampledSHA256 = normalizeContentHash(v.SampledSHA256)
	fingerprintStatus := nullableStatus(v.FingerprintStatus)
	if v.SampledSHA256 != "" && (v.FingerprintStatus == "" || v.FingerprintStatus == "pending") {
		fingerprintStatus = "ready"
	}
	storedTags := uniqueStrings(cleanLabels(v.Tags))
	tagsJSON, _ := json.Marshal(storedTags)
	badgesJSON, _ := json.Marshal(v.Badges)
	now := time.Now().UnixMilli()
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.UnixMilli(now)
	}
	v.UpdatedAt = time.UnixMilli(now)
	thumbnailUpdatedAt := int64(0)
	if v.ThumbnailURL != "" {
		if v.ThumbnailUpdatedAt.IsZero() {
			v.ThumbnailUpdatedAt = time.UnixMilli(now)
		}
		thumbnailUpdatedAt = v.ThumbnailUpdatedAt.UnixMilli()
	} else {
		v.ThumbnailUpdatedAt = time.Time{}
	}
	previewUpdatedAt := int64(0)
	if v.PreviewLocal != "" && v.PreviewStatus == "ready" {
		if v.PreviewUpdatedAt.IsZero() {
			v.PreviewUpdatedAt = time.UnixMilli(now)
		}
		previewUpdatedAt = v.PreviewUpdatedAt.UnixMilli()
	} else {
		v.PreviewUpdatedAt = time.Time{}
	}

	_, err := exec.ExecContext(ctx, `
INSERT INTO videos (
  id, drive_id, file_id, file_name, content_hash, sampled_sha256, fingerprint_status, fingerprint_error, parent_id, dir_name, title, author, tags,
	  duration_seconds, size_bytes, ext, quality, thumbnail_url, thumbnail_updated_at, thumbnail_status,
	  preview_file_id, preview_local, preview_updated_at, preview_status,
	  transcode_status, transcode_error, transcoded_file_id, transcoded_size,
	  views, last_viewed_at, favorites, comments, likes, last_liked_at, dislikes,
	  hidden, badges, description, published_at, created_at, updated_at
	) VALUES (
	  ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
	  ?, ?, ?, ?, ?, ?, CASE WHEN COALESCE(?, '') != '' THEN 'ready' ELSE 'pending' END,
	  ?, ?, ?, ?,
	  ?, ?, ?, ?,
	  ?, ?, ?, ?, ?, ?, ?,
	  ?, ?, ?, ?, ?, ?
	)
ON CONFLICT(id) DO UPDATE SET
  file_name       = CASE
                      WHEN excluded.file_name != '' THEN excluded.file_name
                      ELSE videos.file_name
                    END,
  dir_name        = CASE
                      WHEN excluded.dir_name != '' THEN excluded.dir_name
                      ELSE videos.dir_name
                    END,
  title           = excluded.title,
  author          = excluded.author,
  tags            = CASE
                      WHEN excluded.tags NOT IN ('', '[]', 'null') THEN excluded.tags
                      ELSE videos.tags
                    END,
  content_hash    = CASE
                      WHEN excluded.content_hash != '' THEN excluded.content_hash
                      ELSE videos.content_hash
                    END,
  sampled_sha256  = CASE
                      WHEN videos.size_bytes != excluded.size_bytes THEN excluded.sampled_sha256
                      WHEN excluded.sampled_sha256 != '' THEN excluded.sampled_sha256
                      ELSE videos.sampled_sha256
                    END,
  fingerprint_status = CASE
                      WHEN videos.size_bytes != excluded.size_bytes THEN COALESCE(excluded.fingerprint_status, 'pending')
                      WHEN excluded.sampled_sha256 != '' THEN COALESCE(excluded.fingerprint_status, 'ready')
                      ELSE COALESCE(videos.fingerprint_status, 'pending')
                    END,
  fingerprint_error = CASE
                      WHEN videos.size_bytes != excluded.size_bytes THEN COALESCE(excluded.fingerprint_error, '')
                      WHEN excluded.sampled_sha256 != '' THEN COALESCE(excluded.fingerprint_error, '')
                      ELSE COALESCE(videos.fingerprint_error, '')
                    END,
  duration_seconds= excluded.duration_seconds,
  size_bytes      = excluded.size_bytes,
  ext             = excluded.ext,
  quality         = excluded.quality,
  thumbnail_url   = excluded.thumbnail_url,
	thumbnail_updated_at = CASE
	                    WHEN COALESCE(excluded.thumbnail_url, '') = '' THEN 0
	                    WHEN COALESCE(excluded.thumbnail_url, '') != COALESCE(videos.thumbnail_url, '')
	                         OR COALESCE(videos.thumbnail_updated_at, 0) = 0
	                      THEN MAX(COALESCE(videos.thumbnail_updated_at, 0) + 1, excluded.thumbnail_updated_at)
	                    ELSE videos.thumbnail_updated_at
	                  END,
  -- thumbnail_url 写非空就意味着文件已就绪（要么 worker 抽帧填的本地 /p/thumb/<id>，
  -- 要么网盘 API 直接给的远程 URL，要么管理员手动指定）。同步把 status 标 'ready'，
  -- 避免出现 "url 非空 + status='pending'" 的脏状态。url 被改成空（本调用不发生，
  -- 走 clearVolatileOneDriveThumbnails 直 SQL）保留原状态。
  thumbnail_status= CASE
                      WHEN COALESCE(excluded.thumbnail_url, '') != '' THEN 'ready'
                      ELSE videos.thumbnail_status
                    END,
	  badges          = excluded.badges,
	  description     = excluded.description,
  updated_at      = excluded.updated_at
`,
		v.ID, v.DriveID, v.FileID, v.FileName, v.ContentHash, v.SampledSHA256, fingerprintStatus, v.FingerprintError, v.ParentID, v.DirName, v.Title, v.Author, string(tagsJSON),
		v.DurationSeconds, v.Size, v.Ext, v.Quality, v.ThumbnailURL, thumbnailUpdatedAt, v.ThumbnailURL,
		v.PreviewFileID, v.PreviewLocal, previewUpdatedAt, nullableStatus(v.PreviewStatus),
		v.TranscodeStatus, v.TranscodeError, v.TranscodedFileID, v.TranscodedSize,
		v.Views, unixMilliOrZero(v.LastViewedAt), v.Favorites, v.Comments, v.Likes, unixMilliOrZero(v.LastLikedAt), v.Dislikes,
		boolToInt(v.Hidden), string(badgesJSON), v.Description,
		v.PublishedAt.UnixMilli(), v.CreatedAt.UnixMilli(), v.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		return nil, err
	}
	return storedTags, nil
}

func nullableStatus(s string) string {
	if s == "" {
		return "pending"
	}
	return s
}

func (c *Catalog) UpdatePreview(ctx context.Context, id, previewLocal, status string) error {
	now := time.Now().UnixMilli()
	_, err := c.db.ExecContext(ctx,
		`UPDATE videos
		    SET preview_file_id = '',
		        preview_local = ?,
		        preview_updated_at = CASE
		          WHEN ? = 'ready' AND COALESCE(?, '') != ''
		            THEN MAX(COALESCE(preview_updated_at, 0) + 1, ?)
		          ELSE 0
		        END,
		        preview_status = ?,
		        updated_at = ?
		  WHERE id = ?`,
		previewLocal, status, previewLocal, now, status, now, id)
	return err
}

// transcodeCandidateWhereSQL 圈定"可能需要浏览器兼容性转码"的视频：
// webm 容器规范上只允许 vp8/vp9/av1 + vorbis/opus，浏览器必播，不进候选；
// strm 是远程引用没有本体。mp4/m4v 虽然容器兼容，但里面可能装着
// MPEG-4 Part 2 / HEVC 等浏览器解不了的视频轨（表现为黑屏有声音），
// 所以也进候选，由转码 worker 先远程 probe 实际编码，兼容的直接标
// skipped（不下载文件），不兼容的才转码。failed 也保留在候选里，
// 重新点开始转码时会自动重试。
const transcodeCandidateWhereSQL = `COALESCE(ext, '') NOT IN ('webm', 'strm')
	AND COALESCE(transcode_status, '') IN ('', 'pending', 'failed')`

// ListTranscodeCandidates 列出某盘所有转码候选视频。limit<=0 表示不限制。
func (c *Catalog) ListTranscodeCandidates(ctx context.Context, driveID string, limit int) ([]*Video, error) {
	query := `SELECT ` + allVideoCols + ` FROM videos
		 WHERE drive_id = ? AND ` + transcodeCandidateWhereSQL + `
		 ORDER BY created_at ASC, id ASC`
	args := []any{driveID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := c.db.QueryContext(ctx, query, args...)
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

// UpdateVideoTranscode 写回单条视频的转码结果。
// status=ready 时 transcodedFileID/transcodedSize 指向转码产物；
// 其它 status 调用方应传空值，本函数会按传入值原样覆盖。
func (c *Catalog) UpdateVideoTranscode(ctx context.Context, id, status, errMsg, transcodedFileID string, transcodedSize int64) error {
	_, err := c.db.ExecContext(ctx,
		`UPDATE videos SET transcode_status = ?, transcode_error = ?, transcoded_file_id = ?, transcoded_size = ?, updated_at = ? WHERE id = ?`,
		status, errMsg, transcodedFileID, transcodedSize, time.Now().UnixMilli(), id)
	return err
}

// DriveTranscodeCounts 是单盘的转码进度统计。
type DriveTranscodeCounts struct {
	// Pending 是仍在候选集合里、还没有出结果的数量（含从未检测过的）。
	Pending int
	Ready   int
	Failed  int
	Skipped int
}

func (c *Catalog) CountTranscodesByDrive(ctx context.Context) (map[string]DriveTranscodeCounts, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT drive_id,
		        COUNT(CASE WHEN COALESCE(ext, '') NOT IN ('mp4', 'webm', 'm4v', 'strm')
		                    AND COALESCE(transcode_status, '') IN ('', 'pending') THEN 1 END) AS pending_count,
		        COUNT(CASE WHEN COALESCE(transcode_status, '') = 'ready' THEN 1 END) AS ready_count,
		        COUNT(CASE WHEN COALESCE(transcode_status, '') = 'failed' THEN 1 END) AS failed_count,
		        COUNT(CASE WHEN COALESCE(transcode_status, '') = 'skipped' THEN 1 END) AS skipped_count
		  FROM videos
		 GROUP BY drive_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]DriveTranscodeCounts)
	for rows.Next() {
		var driveID string
		var counts DriveTranscodeCounts
		if err := rows.Scan(&driveID, &counts.Pending, &counts.Ready, &counts.Failed, &counts.Skipped); err != nil {
			return nil, err
		}
		out[driveID] = counts
	}
	return out, rows.Err()
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

// ListHiddenVideos 返回所有被标记隐藏（hidden=1）的视频。
// 仅用于一次性把历史「隐藏」视频迁移为黑名单墓碑——隐藏机制已废弃，
// 前台「不再展示」改走拉黑逻辑。
func (c *Catalog) ListHiddenVideos(ctx context.Context) ([]*Video, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+allVideoCols+` FROM videos WHERE COALESCE(hidden, 0) = 1`)
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

// MigrateVideoToDrive rewrites a crawler video row after it has been uploaded
// to another drive. The video id is preserved so tags, favorites, likes and
// view records keep pointing at the same logical video.
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

// UpdateVideoFileIdentity atomically updates the storage filename and display
// title after a local physical file has been renamed.
func (c *Catalog) UpdateVideoFileIdentity(ctx context.Context, videoID, fileID, fileName, title string) error {
	if strings.TrimSpace(videoID) == "" || strings.TrimSpace(fileID) == "" || strings.TrimSpace(fileName) == "" || strings.TrimSpace(title) == "" {
		return fmt.Errorf("catalog: update video file identity: empty value")
	}
	res, err := c.db.ExecContext(ctx, `
UPDATE videos
   SET file_id = ?, file_name = ?, title = ?, updated_at = ?
 WHERE id = ?`, fileID, fileName, title, time.Now().UnixMilli(), videoID)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListVideosByDriveID 列出指定 drive 下所有未隐藏的视频，按 published_at 倒序。
// crawler upload worker uses this to find local crawler rows before uploading
// them to their configured target drive.
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
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx,
		`UPDATE videos SET likes = likes + 1, last_liked_at = ?, updated_at = ? WHERE id = ?`,
		now, now, id); err != nil {
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
	now := time.Now().UnixMilli()
	res, err := tx.ExecContext(ctx,
		`UPDATE videos
		    SET likes = MAX(likes - 1, 0),
		        last_liked_at = CASE WHEN likes <= 1 THEN 0 ELSE last_liked_at END,
		        updated_at = ?
		  WHERE id = ?`,
		now, id)
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
	now := time.Now().UnixMilli()
	res, err := tx.ExecContext(ctx,
		`UPDATE videos SET views = views + 1, last_viewed_at = ?, updated_at = ? WHERE id = ?`,
		now, now, id)
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
	ContentHash            string
	FileName               string
	DirName                string
	Title                  string
	TitleSet               bool
	Author                 string
	AuthorSet              bool
	Tags                   []string
	TagsSet                bool
}

func (c *Catalog) UpdateVideoMeta(ctx context.Context, id string, p VideoMetaPatch) error {
	parts := []string{}
	args := []any{}
	now := time.Now().UnixMilli()
	if p.ThumbnailURL != "" {
		parts = append(parts,
			"thumbnail_url = ?",
			"thumbnail_updated_at = MAX(COALESCE(thumbnail_updated_at, 0) + 1, ?)",
		)
		args = append(args, p.ThumbnailURL, now)
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
	if p.ContentHash != "" {
		parts = append(parts, "content_hash = ?")
		args = append(args, normalizeContentHash(p.ContentHash))
	}
	if p.FileName != "" {
		parts = append(parts, "file_name = ?")
		args = append(args, p.FileName)
	}
	if p.DirName != "" {
		parts = append(parts, "dir_name = ?")
		args = append(args, p.DirName)
	}
	if p.TitleSet {
		parts = append(parts, "title = ?")
		args = append(args, p.Title)
	}
	if p.AuthorSet {
		parts = append(parts, "author = ?")
		args = append(args, p.Author)
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
	args = append(args, now)
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

// FindVideoByDriveFileID resolves the row that owns an actual provider file.
// Migrated crawler rows preserve their original logical ID, so a scanner's
// generated ID is not always the catalog ID.
func (c *Catalog) FindVideoByDriveFileID(ctx context.Context, driveID, fileID string) (*Video, error) {
	driveID = strings.TrimSpace(driveID)
	fileID = strings.TrimSpace(fileID)
	if driveID == "" || fileID == "" {
		return nil, sql.ErrNoRows
	}
	row := c.db.QueryRowContext(ctx,
		`SELECT `+allVideoCols+` FROM videos
		 WHERE drive_id = ? AND file_id = ?
		 ORDER BY created_at ASC, id ASC
		 LIMIT 1`, driveID, fileID)
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

// ListVideoMaintenanceCandidates returns all current catalog videos without the
// public listing dedupe filter. Nightly maintenance needs to see duplicate rows
// that ListVideos intentionally hides from the frontend.
func (c *Catalog) ListVideoMaintenanceCandidates(ctx context.Context) ([]*Video, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+allVideoCols+` FROM videos
		 WHERE COALESCE(hidden, 0) = 0
		 ORDER BY created_at ASC, id ASC`)
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

// ListCrawlerSourceIDs lists source IDs that were already imported by a
// crawler-like drive. It reads both videos and deleted_videos so explicit admin
// deletions remain tombstoned for future crawler runs.
func (c *Catalog) ListCrawlerSourceIDs(ctx context.Context, kind, driveID string) ([]string, error) {
	kind = strings.TrimSpace(kind)
	driveID = strings.TrimSpace(driveID)
	if kind == "" || driveID == "" {
		return nil, nil
	}
	prefix := kind + "-" + driveID + "-"
	rows, err := c.db.QueryContext(ctx,
		`SELECT SUBSTR(id, ?) FROM videos WHERE id LIKE ? || '%'
		 UNION
		 SELECT SUBSTR(id, ?) FROM deleted_videos WHERE id LIKE ? || '%'
		 UNION
		 SELECT source_id FROM crawler_seen_sources
		  WHERE kind = ? AND drive_id = ? AND status IN ('imported', 'duplicate')`,
		len(prefix)+1, prefix, len(prefix)+1, prefix, kind, driveID)
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

// MarkCrawlerSourceSeen records the outcome for a crawler source item. Duplicate
// source IDs are included in future seen files so scripts can skip them before
// the backend downloads the same duplicate content again.
func (c *Catalog) MarkCrawlerSourceSeen(ctx context.Context, kind, driveID, sourceID, status, canonicalVideoID, sampledSHA256 string, size int64) error {
	kind = strings.TrimSpace(kind)
	driveID = strings.TrimSpace(driveID)
	sourceID = strings.TrimSpace(sourceID)
	status = strings.TrimSpace(status)
	if kind == "" || driveID == "" || sourceID == "" {
		return nil
	}
	switch status {
	case "imported", "duplicate":
	default:
		return fmt.Errorf("catalog: unsupported crawler source status %q", status)
	}
	sampledSHA256 = normalizeContentHash(sampledSHA256)
	if size < 0 {
		size = 0
	}
	now := time.Now().UnixMilli()
	_, err := c.db.ExecContext(ctx, `
INSERT INTO crawler_seen_sources (
  kind, drive_id, source_id, status, canonical_video_id, sampled_sha256, size_bytes, first_seen_at, last_seen_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(kind, drive_id, source_id) DO UPDATE SET
  status = excluded.status,
  canonical_video_id = excluded.canonical_video_id,
  sampled_sha256 = CASE
                    WHEN excluded.sampled_sha256 != '' THEN excluded.sampled_sha256
                    ELSE crawler_seen_sources.sampled_sha256
                   END,
  size_bytes = CASE
                WHEN excluded.size_bytes > 0 THEN excluded.size_bytes
                ELSE crawler_seen_sources.size_bytes
               END,
  last_seen_at = excluded.last_seen_at`,
		kind, driveID, sourceID, status, strings.TrimSpace(canonicalVideoID), sampledSHA256, size, now, now)
	return err
}

const (
	DeletedVideoReasonDuplicate = "duplicate"

	DeletedVideoRestorePolicyNone    = "none"
	DeletedVideoRestorePolicyScan    = "scan"
	DeletedVideoRestorePolicyCrawler = "crawler"
	// DeletedVideoRestorePolicyDirect covers sources whose file is retained by
	// this application but that cannot be enumerated, so no scan or crawl will
	// ever rediscover them. Local uploads are the only such source today: the
	// drive supports Stat but returns ErrNotSupported for List. The tombstone
	// already carries the full restore payload, so the row is rebuilt directly
	// instead of waiting for a rediscovery pass that would never come.
	DeletedVideoRestorePolicyDirect = "direct"
)

var (
	ErrDeletedVideoNotRestorable = errors.New("deleted video is not restorable")
	// ErrDeletedVideoSourceCheckRequired prevents catalog-only callers from
	// bypassing the source inspection that a direct restore requires.
	ErrDeletedVideoSourceCheckRequired = errors.New("deleted video source check is required")
	// ErrDeletedVideoSourceMissing means a direct restore was attempted but the
	// retained source file is gone, so rebuilding the row would publish a video
	// that cannot be played.
	ErrDeletedVideoSourceMissing = errors.New("deleted video source file is missing")
)

const deletedVideoRestorePayloadVersion = 1

// DeletedVideoSourceInfo is the provider-owned state observed immediately
// before a direct restore. Catalog uses it both to reject unusable files and to
// invalidate stale source-derived metadata when a retained file was replaced.
type DeletedVideoSourceInfo struct {
	Size    int64
	ModTime time.Time
}

type DeletedVideoRestoreResult struct {
	RestorePolicy string
	Video         *Video
}

type deletedVideoRestorePayload struct {
	Version        int                                `json:"version"`
	Video          *Video                             `json:"video"`
	TagsManual     bool                               `json:"tagsManual"`
	TagAssignments []deletedVideoTagRestoreAssignment `json:"tagAssignments,omitempty"`
}

// deletedVideoTagRestoreAssignment keeps both sides of a tag relation. The tag
// definition may be pruned when the video is tombstoned, while assignment
// source/evidence determines whether future automatic retagging may replace it.
type deletedVideoTagRestoreAssignment struct {
	Label         string `json:"label"`
	Source        string `json:"source"`
	Evidence      string `json:"evidence,omitempty"`
	CreatedAt     int64  `json:"createdAt,omitempty"`
	TagAliases    string `json:"tagAliases,omitempty"`
	TagMatchRules string `json:"tagMatchRules,omitempty"`
	TagSource     string `json:"tagSource,omitempty"`
	TagOrigin     string `json:"tagOrigin,omitempty"`
	TagCreatedAt  int64  `json:"tagCreatedAt,omitempty"`
	TagUpdatedAt  int64  `json:"tagUpdatedAt,omitempty"`
}

type DeleteVideoTombstoneOptions struct {
	Reason           string
	SourceDeleted    bool
	CanonicalVideoID string
}

// DeleteVideoWithTombstone records that a video was removed, then removes the
// visible catalog row. The tombstone is used by scanners/crawlers to avoid
// importing the same source file again.
func (c *Catalog) DeleteVideoWithTombstone(ctx context.Context, id string) error {
	return c.DeleteVideoWithTombstoneOptions(ctx, id, DeleteVideoTombstoneOptions{})
}

// DeleteVideoWithTombstoneReason is the same tombstone path with an optional
// machine reason for admin UI hints. Empty reason means user/admin initiated.
func (c *Catalog) DeleteVideoWithTombstoneReason(ctx context.Context, id, reason string) error {
	return c.DeleteVideoWithTombstoneOptions(ctx, id, DeleteVideoTombstoneOptions{Reason: reason})
}

// DeleteVideoWithTombstoneOptions records restore-relevant facts alongside the
// tombstone. When SourceDeleted is true the source file is gone, so no tombstone
// is retained. CanonicalVideoID links deduplicated rows to the retained video.
func (c *Catalog) DeleteVideoWithTombstoneOptions(ctx context.Context, id string, options DeleteVideoTombstoneOptions) error {
	if options.SourceDeleted {
		return c.DeleteVideo(ctx, id)
	}
	options.Reason = normalizeDeletedVideoReason(options.Reason)
	options.CanonicalVideoID = strings.TrimSpace(options.CanonicalVideoID)
	if options.Reason != DeletedVideoReasonDuplicate {
		options.CanonicalVideoID = ""
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	restoreVideo, err := scanVideo(tx.QueryRowContext(ctx,
		`SELECT `+allVideoCols+` FROM videos WHERE id = ?`, id))
	if err != nil {
		return err
	}
	restorePayloadData, err := buildDeletedVideoRestorePayload(ctx, tx, restoreVideo)
	if err != nil {
		return fmt.Errorf("catalog: encode deleted video restore payload: %w", err)
	}
	restorePayload := string(restorePayloadData)

	restoreVideo.ContentHash = normalizeContentHash(restoreVideo.ContentHash)

	// 先记录这次视频关联的 tag_id，便于事务末尾清理孤儿自动生成标签。
	tagIDs, err := collectVideoTagIDs(ctx, tx, id)
	if err != nil {
		return err
	}

	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO deleted_videos (
  id, drive_id, file_id, parent_id, content_hash, file_name, size_bytes,
  reason, source_deleted, canonical_video_id, restore_requested, restore_payload, deleted_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  drive_id           = excluded.drive_id,
  file_id            = excluded.file_id,
  parent_id          = excluded.parent_id,
  content_hash       = excluded.content_hash,
  file_name          = excluded.file_name,
  size_bytes         = excluded.size_bytes,
  reason             = excluded.reason,
  source_deleted     = excluded.source_deleted,
  canonical_video_id = excluded.canonical_video_id,
  restore_requested  = 0,
  restore_payload    = excluded.restore_payload,
  deleted_at         = excluded.deleted_at`,
		restoreVideo.ID, restoreVideo.DriveID, restoreVideo.FileID, restoreVideo.ParentID, restoreVideo.ContentHash, restoreVideo.FileName, restoreVideo.Size,
		options.Reason, boolToInt(options.SourceDeleted), options.CanonicalVideoID, restorePayload, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM video_tags WHERE video_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM video_shares WHERE video_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM video_reaction_visits WHERE video_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM videos WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	if err := pruneOrphanGeneratedTagsByID(ctx, tx, tagIDs); err != nil {
		return err
	}
	return tx.Commit()
}

func (c *Catalog) DeleteVideo(ctx context.Context, id string) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 先记录这次视频关联的 tag_id，便于事务末尾清理孤儿自动生成标签。
	tagIDs, err := collectVideoTagIDs(ctx, tx, id)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM video_tags WHERE video_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM video_shares WHERE video_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM video_reaction_visits WHERE video_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM videos WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}

	// 自动生成标签在视频删完后若不再被引用就一起回收；内置和自定义标签保留。
	if err := pruneOrphanGeneratedTagsByID(ctx, tx, tagIDs); err != nil {
		return err
	}

	return tx.Commit()
}

// DeletedVideo 是黑名单（墓碑）表里的一条记录。原始视频行已删除，
// 这里只保留扫盘去重和后台展示需要的最小字段；没有 title/封面/作者。
type DeletedVideo struct {
	ID               string `json:"id"`
	DriveID          string `json:"driveId"`
	FileID           string `json:"fileId"`
	ParentID         string `json:"-"`
	FileName         string `json:"fileName"`
	Size             int64  `json:"size"`
	Reason           string `json:"reason"`
	SourceDeleted    bool   `json:"sourceDeleted"`
	CanonicalVideoID string `json:"canonicalVideoId,omitempty"`
	CanonicalTitle   string `json:"canonicalTitle,omitempty"`
	RestorePolicy    string `json:"restorePolicy"`
	DeletedAt        int64  `json:"deletedAt"` // unix 毫秒
}

// ListDeletedVideos 分页列出黑名单视频，按拉黑时间倒序。
// Keyword 非空时按文件名模糊匹配，DriveID 非空时限定来源网盘。
// source_deleted 是旧版本兼容字段；新版本删除源文件成功后会直接清除墓碑。
func (c *Catalog) ListDeletedVideos(ctx context.Context, p ListParams) ([]*DeletedVideo, int, error) {
	if p.PageSize <= 0 {
		p.PageSize = 50
	}
	if p.Page <= 0 {
		p.Page = 1
	}
	var where []string
	var args []any
	where = append(where, "COALESCE(dv.restore_requested, 0) = 0")
	if !p.IncludeSourceDeleted {
		where = append(where, "COALESCE(dv.source_deleted, 0) = 0")
	}
	if kw := strings.TrimSpace(p.Keyword); kw != "" {
		where = append(where, "dv.file_name LIKE ?")
		args = append(args, "%"+kw+"%")
	}
	if driveID := strings.TrimSpace(p.DriveID); driveID != "" {
		where = append(where, "dv.drive_id = ?")
		args = append(args, driveID)
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deleted_videos dv`+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	offset := (p.Page - 1) * p.PageSize
	rows, err := c.db.QueryContext(ctx,
		`SELECT dv.id,
		        COALESCE(dv.drive_id, ''),
		        COALESCE(dv.file_id, ''),
		        COALESCE(dv.file_name, ''),
		        COALESCE(dv.size_bytes, 0),
		        COALESCE(dv.reason, ''),
		        COALESCE(dv.source_deleted, 0),
		        COALESCE(dv.canonical_video_id, ''),
		        COALESCE(cv.title, ''),
		        COALESCE(d.kind, ''),
		        dv.deleted_at
		   FROM deleted_videos dv
		   LEFT JOIN videos cv ON cv.id = dv.canonical_video_id
		   LEFT JOIN drives d ON d.id = dv.drive_id`+whereSQL+`
		  ORDER BY dv.deleted_at DESC
		  LIMIT ? OFFSET ?`,
		append(args, p.PageSize, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []*DeletedVideo
	for rows.Next() {
		v := &DeletedVideo{}
		var sourceDeleted int
		var driveKind string
		if err := rows.Scan(
			&v.ID,
			&v.DriveID,
			&v.FileID,
			&v.FileName,
			&v.Size,
			&v.Reason,
			&sourceDeleted,
			&v.CanonicalVideoID,
			&v.CanonicalTitle,
			&driveKind,
			&v.DeletedAt,
		); err != nil {
			return nil, 0, err
		}
		v.SourceDeleted = sourceDeleted != 0
		v.RestorePolicy = deletedVideoRestorePolicy(v, driveKind)
		out = append(out, v)
	}
	return out, total, rows.Err()
}

// ListDeletedVideosPendingSourceDeletion returns every tombstone whose source
// has not been confirmed deleted. It is intentionally unpaginated because the
// caller runs one serialized background cleanup job and needs a stable work
// snapshot without repeatedly racing a changing page boundary.
func (c *Catalog) ListDeletedVideosPendingSourceDeletion(ctx context.Context) ([]*DeletedVideo, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT id,
       COALESCE(drive_id, ''),
       COALESCE(file_id, ''),
       COALESCE(parent_id, ''),
       COALESCE(file_name, ''),
       COALESCE(size_bytes, 0),
       COALESCE(reason, ''),
       deleted_at
	 FROM deleted_videos
	WHERE COALESCE(source_deleted, 0) = 0
	  AND COALESCE(restore_requested, 0) = 0
 ORDER BY deleted_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*DeletedVideo
	for rows.Next() {
		v := &DeletedVideo{}
		if err := rows.Scan(
			&v.ID,
			&v.DriveID,
			&v.FileID,
			&v.ParentID,
			&v.FileName,
			&v.Size,
			&v.Reason,
			&v.DeletedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ListDeletedVideosPendingSourceDeletionByIDs returns the requested tombstones
// whose source files have not been confirmed deleted. Missing IDs and already
// marked entries are ignored so callers can safely retry stale UI selections.
func (c *Catalog) ListDeletedVideosPendingSourceDeletionByIDs(ctx context.Context, ids []string) ([]*DeletedVideo, error) {
	seen := make(map[string]bool, len(ids))
	normalized := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		normalized = append(normalized, id)
	}
	if len(normalized) == 0 {
		return []*DeletedVideo{}, nil
	}

	placeholders := strings.TrimRight(strings.Repeat("?,", len(normalized)), ",")
	args := make([]any, 0, len(normalized))
	for _, id := range normalized {
		args = append(args, id)
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT id,
       COALESCE(drive_id, ''),
       COALESCE(file_id, ''),
       COALESCE(parent_id, ''),
       COALESCE(file_name, ''),
       COALESCE(size_bytes, 0),
       COALESCE(reason, ''),
       deleted_at
	 FROM deleted_videos
	WHERE COALESCE(source_deleted, 0) = 0
	  AND COALESCE(restore_requested, 0) = 0
	  AND id IN (`+placeholders+`)
 ORDER BY deleted_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*DeletedVideo
	for rows.Next() {
		v := &DeletedVideo{}
		if err := rows.Scan(
			&v.ID,
			&v.DriveID,
			&v.FileID,
			&v.ParentID,
			&v.FileName,
			&v.Size,
			&v.Reason,
			&v.DeletedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (c *Catalog) CountDeletedVideosPendingSourceDeletion(ctx context.Context) (int, error) {
	var count int
	err := c.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM deleted_videos WHERE COALESCE(source_deleted, 0) = 0 AND COALESCE(restore_requested, 0) = 0`,
	).Scan(&count)
	return count, err
}

// PurgeDeletedVideo removes a blacklist tombstone after the source file has
// been deleted. Crawler seen metadata is intentionally retained so crawler
// sources are not fetched again after the user deletes their stored video file.
func (c *Catalog) PurgeDeletedVideo(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return sql.ErrNoRows
	}
	res, err := c.db.ExecContext(ctx, `DELETE FROM deleted_videos WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// RemoveDeletedVideo 允许可扫描来源在后续任务中重新入库。本函数不会触发
// 扫描或爬取。爬虫来源会保留墓碑和 seen 记录，并标记为待目录扫描恢复，
// 使下一轮爬取仍将其当作已见来源跳过。direct 来源必须通过
// RestoreDeletedVideo 提供源文件检查，避免 catalog-only 调用绕过安全边界。
func (c *Catalog) RemoveDeletedVideo(ctx context.Context, id string) error {
	_, err := c.RestoreDeletedVideo(ctx, id, nil)
	return err
}

// RestoreDeletedVideo removes or advances a tombstone according to its policy.
// Direct restoration is the only branch that creates a catalog row, so it
// requires provider-owned source inspection and returns the restored video for
// application-level generation queues. Its row, tag assignments, and tombstone
// deletion commit in one SQLite transaction.
func (c *Catalog) RestoreDeletedVideo(
	ctx context.Context,
	id string,
	inspectSource func(driveID, fileID string) (DeletedVideoSourceInfo, error),
) (DeletedVideoRestoreResult, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return DeletedVideoRestoreResult{}, sql.ErrNoRows
	}

	deleted, driveKind, _, err := c.deletedVideoForRestore(ctx, c.db, id)
	if err != nil {
		return DeletedVideoRestoreResult{}, err
	}
	deleted.RestorePolicy = deletedVideoRestorePolicy(deleted, driveKind)
	if deleted.RestorePolicy == DeletedVideoRestorePolicyNone {
		return DeletedVideoRestoreResult{}, deletedVideoNotRestorableError(deleted)
	}

	if deleted.RestorePolicy == DeletedVideoRestorePolicyDirect {
		if inspectSource == nil {
			return DeletedVideoRestoreResult{}, ErrDeletedVideoSourceCheckRequired
		}
		source, err := inspectSource(deleted.DriveID, deleted.FileID)
		if err != nil {
			return DeletedVideoRestoreResult{}, fmt.Errorf("%w: %v", ErrDeletedVideoSourceMissing, err)
		}
		if source.Size <= 0 {
			return DeletedVideoRestoreResult{}, fmt.Errorf("%w: source file is empty", ErrDeletedVideoSourceMissing)
		}
		return c.restoreDeletedVideoDirect(ctx, id, source)
	}

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return DeletedVideoRestoreResult{}, err
	}
	defer tx.Rollback()
	deleted, driveKind, _, err = c.deletedVideoForRestore(ctx, tx, id)
	if err != nil {
		return DeletedVideoRestoreResult{}, err
	}
	deleted.RestorePolicy = deletedVideoRestorePolicy(deleted, driveKind)
	if deleted.RestorePolicy == DeletedVideoRestorePolicyNone {
		return DeletedVideoRestoreResult{}, deletedVideoNotRestorableError(deleted)
	}
	if deleted.RestorePolicy == DeletedVideoRestorePolicyCrawler {
		prefix := driveKind + "-" + deleted.DriveID + "-"
		if !strings.HasPrefix(deleted.ID, prefix) {
			return DeletedVideoRestoreResult{}, fmt.Errorf("%w: crawler source id is unavailable", ErrDeletedVideoNotRestorable)
		}
		sourceID := strings.TrimSpace(strings.TrimPrefix(deleted.ID, prefix))
		if sourceID == "" {
			return DeletedVideoRestoreResult{}, fmt.Errorf("%w: crawler source id is unavailable", ErrDeletedVideoNotRestorable)
		}
		res, err := tx.ExecContext(ctx, `
UPDATE deleted_videos
   SET restore_requested = 1
	WHERE id = ?
	  AND COALESCE(restore_requested, 0) = 0`, id)
		if err != nil {
			return DeletedVideoRestoreResult{}, err
		}
		if rows, err := res.RowsAffected(); err == nil && rows == 0 {
			return DeletedVideoRestoreResult{}, sql.ErrNoRows
		}
		if err := tx.Commit(); err != nil {
			return DeletedVideoRestoreResult{}, err
		}
		return DeletedVideoRestoreResult{RestorePolicy: DeletedVideoRestorePolicyCrawler}, nil
	}

	res, err := tx.ExecContext(ctx, `
DELETE FROM deleted_videos
 WHERE id = ?
   AND COALESCE(restore_requested, 0) = 0`, id)
	if err != nil {
		return DeletedVideoRestoreResult{}, err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return DeletedVideoRestoreResult{}, sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return DeletedVideoRestoreResult{}, err
	}
	return DeletedVideoRestoreResult{RestorePolicy: DeletedVideoRestorePolicyScan}, nil
}

type deletedVideoRestoreQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (c *Catalog) deletedVideoForRestore(
	ctx context.Context,
	query deletedVideoRestoreQuerier,
	id string,
) (*DeletedVideo, string, string, error) {
	var deleted DeletedVideo
	var sourceDeleted int
	var driveKind string
	var restorePayload string
	err := query.QueryRowContext(ctx, `
SELECT dv.id,
       COALESCE(dv.drive_id, ''),
       COALESCE(dv.file_id, ''),
       COALESCE(dv.parent_id, ''),
       COALESCE(dv.file_name, ''),
       COALESCE(dv.size_bytes, 0),
       COALESCE(dv.reason, ''),
       COALESCE(dv.source_deleted, 0),
       COALESCE(dv.restore_payload, ''),
       COALESCE(d.kind, ''),
       COALESCE(dv.deleted_at, 0)
  FROM deleted_videos dv
  LEFT JOIN drives d ON d.id = dv.drive_id
 WHERE dv.id = ?
   AND COALESCE(dv.restore_requested, 0) = 0`, id).Scan(
		&deleted.ID,
		&deleted.DriveID,
		&deleted.FileID,
		&deleted.ParentID,
		&deleted.FileName,
		&deleted.Size,
		&deleted.Reason,
		&sourceDeleted,
		&restorePayload,
		&driveKind,
		&deleted.DeletedAt,
	)
	if err != nil {
		return nil, "", "", err
	}
	deleted.SourceDeleted = sourceDeleted != 0
	return &deleted, driveKind, restorePayload, nil
}

func deletedVideoNotRestorableError(deleted *DeletedVideo) error {
	switch {
	case deleted == nil:
		return sql.ErrNoRows
	case deleted.SourceDeleted:
		return fmt.Errorf("%w: source file was deleted", ErrDeletedVideoNotRestorable)
	case deleted.Reason == DeletedVideoReasonDuplicate:
		return fmt.Errorf("%w: duplicate videos must use the retained canonical video", ErrDeletedVideoNotRestorable)
	default:
		return fmt.Errorf("%w: source does not support rediscovery", ErrDeletedVideoNotRestorable)
	}
}

func (c *Catalog) restoreDeletedVideoDirect(
	ctx context.Context,
	id string,
	source DeletedVideoSourceInfo,
) (DeletedVideoRestoreResult, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return DeletedVideoRestoreResult{}, err
	}
	defer tx.Rollback()

	deleted, driveKind, restorePayload, err := c.deletedVideoForRestore(ctx, tx, id)
	if err != nil {
		return DeletedVideoRestoreResult{}, err
	}
	deleted.RestorePolicy = deletedVideoRestorePolicy(deleted, driveKind)
	if deleted.RestorePolicy != DeletedVideoRestorePolicyDirect {
		if deleted.RestorePolicy == DeletedVideoRestorePolicyNone {
			return DeletedVideoRestoreResult{}, deletedVideoNotRestorableError(deleted)
		}
		return DeletedVideoRestoreResult{}, fmt.Errorf("%w: restore policy changed to %s", ErrDeletedVideoNotRestorable, deleted.RestorePolicy)
	}

	// A row can coexist with a tombstone only when an older, non-atomic direct
	// restore inserted the row but failed to remove its tombstone. Treat that row
	// as the completed restore and only finish the tombstone transition. Replacing
	// it would discard views, reactions, shares, or edits created in the meantime.
	existing, existingErr := scanVideo(tx.QueryRowContext(ctx,
		`SELECT `+allVideoCols+` FROM videos WHERE id = ?`, id))
	if existingErr == nil {
		if existing.DriveID != deleted.DriveID || existing.FileID != deleted.FileID {
			return DeletedVideoRestoreResult{}, fmt.Errorf(
				"%w: existing video source does not match tombstone", ErrDeletedVideoNotRestorable)
		}
		if deletedVideoSourceChanged(deleted, existing, source) {
			if err := resetPartiallyRestoredVideoSourceTx(ctx, tx, id, source); err != nil {
				return DeletedVideoRestoreResult{}, err
			}
			existing, err = scanVideo(tx.QueryRowContext(ctx,
				`SELECT `+allVideoCols+` FROM videos WHERE id = ?`, id))
			if err != nil {
				return DeletedVideoRestoreResult{}, err
			}
		}
		if err := deletePendingDeletedVideoTx(ctx, tx, id); err != nil {
			return DeletedVideoRestoreResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return DeletedVideoRestoreResult{}, err
		}
		return DeletedVideoRestoreResult{
			RestorePolicy: DeletedVideoRestorePolicyDirect,
			Video:         existing,
		}, nil
	}
	if !errors.Is(existingErr, sql.ErrNoRows) {
		return DeletedVideoRestoreResult{}, existingErr
	}

	payload, err := decodeDeletedVideoRestorePayload(deleted.ID, restorePayload)
	if err != nil {
		return DeletedVideoRestoreResult{}, err
	}
	video := directRestoreVideo(deleted, payload.Video, source)

	// Normal tombstoning removes these relations. Clear any orphaned rows left by
	// an interrupted legacy/manual repair before recreating the exact tag set.
	for _, statement := range []string{
		`DELETE FROM video_tags WHERE video_id = ?`,
		`DELETE FROM video_shares WHERE video_id = ?`,
		`DELETE FROM video_reaction_visits WHERE video_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, statement, id); err != nil {
			return DeletedVideoRestoreResult{}, err
		}
	}
	if _, err := upsertVideoRow(ctx, tx, video); err != nil {
		return DeletedVideoRestoreResult{}, err
	}
	if err := restoreDeletedVideoTagsTx(ctx, tx, video, payload); err != nil {
		return DeletedVideoRestoreResult{}, err
	}
	restoredVideo, err := scanVideo(tx.QueryRowContext(ctx,
		`SELECT `+allVideoCols+` FROM videos WHERE id = ?`, id))
	if err != nil {
		return DeletedVideoRestoreResult{}, err
	}
	if err := deletePendingDeletedVideoTx(ctx, tx, id); err != nil {
		return DeletedVideoRestoreResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeletedVideoRestoreResult{}, err
	}
	return DeletedVideoRestoreResult{
		RestorePolicy: DeletedVideoRestorePolicyDirect,
		Video:         restoredVideo,
	}, nil
}

func deletePendingDeletedVideoTx(ctx context.Context, tx *sql.Tx, id string) error {
	res, err := tx.ExecContext(ctx, `
DELETE FROM deleted_videos
 WHERE id = ?
   AND COALESCE(restore_requested, 0) = 0`, id)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func resetPartiallyRestoredVideoSourceTx(
	ctx context.Context,
	tx *sql.Tx,
	id string,
	source DeletedVideoSourceInfo,
) error {
	_, err := tx.ExecContext(ctx, `
UPDATE videos
   SET size_bytes = ?,
       content_hash = '',
       sampled_sha256 = '',
       fingerprint_status = 'pending',
       fingerprint_error = '',
       duration_seconds = 0,
       thumbnail_url = '',
       thumbnail_updated_at = 0,
       thumbnail_status = 'pending',
       thumbnail_failures = 0,
       preview_file_id = '',
       preview_local = '',
       preview_updated_at = 0,
       preview_status = 'pending',
       transcode_status = '',
       transcode_error = '',
       transcoded_file_id = '',
       transcoded_size = 0,
       updated_at = ?
 WHERE id = ?`, source.Size, time.Now().UnixMilli(), id)
	return err
}

// CrawlerRestoreRequest is a crawler tombstone that the user removed from the
// blacklist and that is waiting for its retained local source file to be
// discovered after a crawl pipeline completes.
type CrawlerRestoreRequest struct {
	ID       string
	DriveID  string
	FileID   string
	FileName string
	Size     int64
	Video    *Video
}

func (c *Catalog) ListCrawlerRestoreRequests(ctx context.Context, driveID string) ([]CrawlerRestoreRequest, error) {
	driveID = strings.TrimSpace(driveID)
	if driveID == "" {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT id,
       COALESCE(drive_id, ''),
       COALESCE(file_id, ''),
       COALESCE(file_name, ''),
       COALESCE(size_bytes, 0),
       COALESCE(restore_payload, '')
  FROM deleted_videos
 WHERE drive_id = ?
   AND COALESCE(source_deleted, 0) = 0
   AND COALESCE(restore_requested, 0) = 1
 ORDER BY deleted_at, id`, driveID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CrawlerRestoreRequest
	for rows.Next() {
		var request CrawlerRestoreRequest
		var payload string
		if err := rows.Scan(&request.ID, &request.DriveID, &request.FileID, &request.FileName, &request.Size, &payload); err != nil {
			return nil, err
		}
		if strings.TrimSpace(payload) != "" {
			restored, err := decodeDeletedVideoRestorePayload(request.ID, payload)
			if err != nil {
				return nil, err
			}
			request.Video = restored.Video
		}
		out = append(out, request)
	}
	return out, rows.Err()
}

// CompleteCrawlerRestore removes the internal pending tombstone after the
// caller has verified the local source and restored the catalog row.
func (c *Catalog) CompleteCrawlerRestore(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return sql.ErrNoRows
	}
	res, err := c.db.ExecContext(ctx, `
DELETE FROM deleted_videos
 WHERE id = ?
   AND COALESCE(restore_requested, 0) = 1`, id)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// VideoManagementCounts 返回后台视频管理两个标签的计数：
// current=当前可见（与「当前视频」页一致的去重+在线盘+hidden=0 口径），
// blacklisted=仍有源文件待管理的黑名单墓碑数。
func (c *Catalog) VideoManagementCounts(ctx context.Context) (current, blacklisted int, err error) {
	currentSQL := `SELECT COUNT(*) FROM videos WHERE COALESCE(hidden, 0) = 0 AND ` + activeDriveWhereSQL + ` AND ` + uniqueVideoWhereSQL
	if err = c.db.QueryRowContext(ctx, currentSQL).Scan(&current); err != nil {
		return 0, 0, err
	}
	if err = c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deleted_videos WHERE COALESCE(source_deleted, 0) = 0 AND COALESCE(restore_requested, 0) = 0`).Scan(&blacklisted); err != nil {
		return 0, 0, err
	}
	return current, blacklisted, nil
}

func (c *Catalog) IsVideoDeleted(ctx context.Context, id string) (bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return false, nil
	}
	var found int
	err := c.db.QueryRowContext(ctx, `SELECT 1 FROM deleted_videos WHERE id = ? LIMIT 1`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (c *Catalog) IsDeletedVideoCandidate(ctx context.Context, id, driveID, fileID, contentHash, fileName string, size int64) (bool, error) {
	id = strings.TrimSpace(id)
	driveID = strings.TrimSpace(driveID)
	fileID = strings.TrimSpace(fileID)
	contentHash = normalizeContentHash(contentHash)
	fileName = strings.TrimSpace(fileName)
	if id == "" && driveID == "" {
		return false, nil
	}

	var found int
	err := c.db.QueryRowContext(ctx, `
SELECT 1
  FROM deleted_videos
 WHERE id = ?
    OR (drive_id = ? AND ? != '' AND file_id = ?)
    OR (drive_id = ? AND ? != '' AND content_hash = ?)
    OR (drive_id = ? AND ? != '' AND ? > 0 AND file_name = ? AND size_bytes = ?)
 LIMIT 1`,
		id,
		driveID, fileID, fileID,
		driveID, contentHash, contentHash,
		driveID, fileName, size, fileName, size,
	).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
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

// FindEquivalentVideo returns the earliest visible video that represents the
// same content as source by strong hash or sampled fingerprint, regardless of
// which drive currently owns it.
func (c *Catalog) FindEquivalentVideo(ctx context.Context, source *Video) (*Video, error) {
	if source == nil {
		return nil, sql.ErrNoRows
	}
	where, args, ok := equivalentVideoLookupWhere(source)
	if !ok {
		return nil, sql.ErrNoRows
	}
	args = append([]any{source.ID}, args...)
	row := c.db.QueryRowContext(ctx,
		`SELECT `+allVideoCols+` FROM videos
		 WHERE id != ?
		   AND COALESCE(hidden, 0) = 0
		   AND COALESCE(file_id, '') != ''
		   AND (`+where+`)
		 ORDER BY created_at ASC, id ASC
		 LIMIT 1`, args...)
	return scanVideo(row)
}

// FindVideoBySampledFingerprint returns the earliest visible video with the
// same file size and sampled fingerprint as source.
func (c *Catalog) FindVideoBySampledFingerprint(ctx context.Context, source *Video) (*Video, error) {
	if source == nil || source.Size <= 0 {
		return nil, sql.ErrNoRows
	}
	sampled := normalizeContentHash(source.SampledSHA256)
	if sampled == "" {
		return nil, sql.ErrNoRows
	}
	row := c.db.QueryRowContext(ctx,
		`SELECT `+allVideoCols+` FROM videos
		 WHERE id != ?
		   AND COALESCE(hidden, 0) = 0
		   AND COALESCE(file_id, '') != ''
		   AND size_bytes = ?
		   AND COALESCE(sampled_sha256, '') != ''
		   AND sampled_sha256 = ?
		 ORDER BY created_at ASC, id ASC
		 LIMIT 1`,
		source.ID, source.Size, sampled)
	return scanVideo(row)
}

// ListNearDuplicateVideoCandidates returns visible videos that are cheap
// candidates for perceptual duplicate checking: same-ish duration and a ready
// thumbnail URL. Callers are expected to apply title similarity and image SSIM.
func (c *Catalog) ListNearDuplicateVideoCandidates(ctx context.Context, source *Video, durationToleranceSeconds, limit int) ([]*Video, error) {
	if source == nil || strings.TrimSpace(source.Title) == "" || source.DurationSeconds <= 0 {
		return nil, nil
	}
	if durationToleranceSeconds < 0 {
		durationToleranceSeconds = 0
	}
	if limit <= 0 {
		limit = 200
	}
	minDuration := source.DurationSeconds - durationToleranceSeconds
	if minDuration < 1 {
		minDuration = 1
	}
	maxDuration := source.DurationSeconds + durationToleranceSeconds
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+allVideoCols+` FROM videos
		 WHERE id != ?
		   AND COALESCE(hidden, 0) = 0
		   AND COALESCE(file_id, '') != ''
		   AND COALESCE(thumbnail_url, '') != ''
		   AND COALESCE(duration_seconds, 0) BETWEEN ? AND ?
		 ORDER BY ABS(duration_seconds - ?) ASC, created_at ASC, id ASC
		 LIMIT ?`,
		source.ID, minDuration, maxDuration, source.DurationSeconds, limit)
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

// FindEquivalentVideoOnDrive returns a visible video on driveID that represents
// the same content as source by strong hash or sampled fingerprint.
func (c *Catalog) FindEquivalentVideoOnDrive(ctx context.Context, source *Video, driveID string) (*Video, error) {
	driveID = strings.TrimSpace(driveID)
	if source == nil || driveID == "" {
		return nil, sql.ErrNoRows
	}
	where, args, ok := equivalentVideoLookupWhere(source)
	if !ok {
		return nil, sql.ErrNoRows
	}
	args = append([]any{driveID, source.ID}, args...)
	row := c.db.QueryRowContext(ctx,
		`SELECT `+allVideoCols+` FROM videos
		 WHERE drive_id = ?
		   AND id != ?
		   AND COALESCE(hidden, 0) = 0
		   AND COALESCE(file_id, '') != ''
		   AND (`+where+`)
		 ORDER BY created_at ASC, id ASC
		 LIMIT 1`, args...)
	return scanVideo(row)
}

// HasReadyEquivalentPreview reports whether another visible row for the same
// content already has a ready preview video.
func (c *Catalog) HasReadyEquivalentPreview(ctx context.Context, source *Video) (bool, error) {
	if source == nil {
		return false, nil
	}
	where, args, ok := equivalentVideoLookupWhere(source)
	if !ok {
		return false, nil
	}
	args = append([]any{source.ID}, args...)
	var found int
	err := c.db.QueryRowContext(ctx,
		`SELECT 1 FROM videos
		 WHERE id != ?
		   AND COALESCE(hidden, 0) = 0
		   AND COALESCE(preview_status, 'pending') = 'ready'
		   AND (`+where+`)
		 LIMIT 1`, args...).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func equivalentVideoLookupWhere(source *Video) (string, []any, bool) {
	if source == nil {
		return "", nil, false
	}
	var parts []string
	var args []any
	if hash := normalizeContentHash(source.ContentHash); hash != "" {
		parts = append(parts, "(COALESCE(content_hash, '') != '' AND content_hash = ?)")
		args = append(args, hash)
	}
	if source.Size > 0 {
		if sampled := normalizeContentHash(source.SampledSHA256); sampled != "" {
			parts = append(parts, "(size_bytes = ? AND COALESCE(sampled_sha256, '') != '' AND sampled_sha256 = ?)")
			args = append(args, source.Size, sampled)
		}
	}
	if len(parts) == 0 {
		return "", nil, false
	}
	return strings.Join(parts, " OR "), args, true
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
	CrawlerID             string
	Tag                   string
	Sort                  string // latest | hot | recent
	ThumbnailReadyOnly    bool
	PreferReadyThumbnails bool
	SkipTotal             bool
	IncludeSourceDeleted  bool
	CreatedAtFrom         int64 // inclusive unix milliseconds
	CreatedAtBefore       int64 // exclusive unix milliseconds
	DurationSecondsMin    int   // inclusive; zero disables the lower bound
	DurationSecondsMax    int   // inclusive; zero disables the upper bound
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
		where = append(where, "(title LIKE ? OR author LIKE ? OR file_name LIKE ?)")
		like := "%" + p.Keyword + "%"
		args = append(args, like, like, like)
	}
	if p.DriveID != "" {
		where = append(where, "drive_id = ?")
		args = append(args, p.DriveID)
	}
	if crawlerID := strings.TrimSpace(p.CrawlerID); crawlerID != "" {
		where = append(where, `EXISTS (
			SELECT 1
			  FROM crawler_seen_sources AS crawler_source
			 WHERE crawler_source.kind = 'scriptcrawler'
			   AND crawler_source.drive_id = ?
			   AND crawler_source.status = 'imported'
			   AND crawler_source.canonical_video_id = videos.id
		)`)
		args = append(args, crawlerID)
	}
	if p.CreatedAtFrom > 0 {
		where = append(where, "videos.created_at >= ?")
		args = append(args, p.CreatedAtFrom)
	}
	if p.CreatedAtBefore > 0 {
		where = append(where, "videos.created_at < ?")
		args = append(args, p.CreatedAtBefore)
	}
	if p.DurationSecondsMin > 0 || p.DurationSecondsMax > 0 {
		where = append(where, "COALESCE(videos.duration_seconds, 0) > 0")
		if p.DurationSecondsMin > 0 {
			where = append(where, "videos.duration_seconds >= ?")
			args = append(args, p.DurationSecondsMin)
		}
		if p.DurationSecondsMax > 0 {
			where = append(where, "videos.duration_seconds <= ?")
			args = append(args, p.DurationSecondsMax)
		}
	}
	if p.Tag != "" {
		where = append(where, videoMatchesTagLabelSQL("videos"))
		args = append(args, p.Tag)
	}
	if p.ThumbnailReadyOnly {
		where = append(where, "COALESCE(thumbnail_url, '') != ''")
	}
	where = append(where, "COALESCE(hidden, 0) = 0")
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
		// 热度 = 点赞数；点赞数相同按最近点赞时间，最后用发布时间兜底。
		orderBy = " ORDER BY " + readyOrderPrefix + "likes DESC, COALESCE(last_liked_at, 0) DESC, published_at DESC"
	case "recent":
		orderBy = " ORDER BY " + readyOrderPrefix + "COALESCE(last_viewed_at, 0) DESC, published_at DESC"
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
		  WHERE COALESCE(hidden, 0) = 0
		    AND `+activeDriveWhereSQL+`
		    AND `+uniqueVideoWhereSQL,
	).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total, nil
}

// ListVisibleVideoIDs returns a stable snapshot of every video currently
// eligible for the public feed. The shorts API shuffles this snapshot once per
// feed session so clients only need to send a small token and cursor afterward.
func (c *Catalog) ListVisibleVideoIDs(ctx context.Context) ([]string, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT videos.id FROM videos
		  WHERE COALESCE(videos.hidden, 0) = 0
		    AND `+activeDriveWhereSQL+`
		    AND `+uniqueVideoWhereSQL+`
		  ORDER BY videos.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// ListVisibleVideoIDsByThumbnailReadiness returns a stable snapshot of every
// public video, split so callers can randomize ready and pending thumbnails
// independently while still preferring ready thumbnails within a full round.
func (c *Catalog) ListVisibleVideoIDsByThumbnailReadiness(ctx context.Context) (readyIDs, pendingIDs []string, err error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT videos.id,
		        CASE WHEN COALESCE(videos.thumbnail_url, '') != '' THEN 1 ELSE 0 END
		   FROM videos
		  WHERE COALESCE(videos.hidden, 0) = 0
		    AND `+activeDriveWhereSQL+`
		    AND `+uniqueVideoWhereSQL+`
		  ORDER BY videos.id ASC`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var thumbnailReady int
		if err := rows.Scan(&id, &thumbnailReady); err != nil {
			return nil, nil, err
		}
		if thumbnailReady != 0 {
			readyIDs = append(readyIDs, id)
		} else {
			pendingIDs = append(pendingIDs, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return readyIDs, pendingIDs, nil
}

// ListVisibleVideoIDsLatest returns a deterministic latest-first snapshot for
// the home page's rotating "latest" section. Ready thumbnails keep the same
// preference as /api/list while the limit bounds the session snapshot.
func (c *Catalog) ListVisibleVideoIDsLatest(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT videos.id
		   FROM videos
		  WHERE COALESCE(videos.hidden, 0) = 0
		    AND `+activeDriveWhereSQL+`
		    AND `+uniqueVideoWhereSQL+`
		  ORDER BY CASE WHEN COALESCE(videos.thumbnail_url, '') != '' THEN 0 ELSE 1 END,
		           videos.published_at DESC,
		           videos.id ASC
		  LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// VisibleVideosByIDs loads the still-visible subset of ids while preserving
// the caller's order. Feed sessions are snapshots, so videos deleted, hidden,
// or superseded by deduplication after the snapshot was created are skipped.
func (c *Catalog) VisibleVideosByIDs(ctx context.Context, ids []string) ([]*Video, error) {
	cleaned := cleanVideoIDs(ids)
	if len(cleaned) == 0 {
		return nil, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(cleaned)), ",")
	args := make([]any, 0, len(cleaned))
	for _, id := range cleaned {
		args = append(args, id)
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+allVideoCols+` FROM videos
		  WHERE videos.id IN (`+placeholders+`)
		    AND COALESCE(videos.hidden, 0) = 0
		    AND `+activeDriveWhereSQL+`
		    AND `+uniqueVideoWhereSQL,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byID := make(map[string]*Video, len(cleaned))
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		byID[v.ID] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]*Video, 0, len(byID))
	for _, id := range cleaned {
		if v := byID[id]; v != nil {
			out = append(out, v)
		}
	}
	return out, nil
}

// RandomVideosExcluding 从对前台可见的视频里，随机返回 limit 个不在 excludeIDs 中的视频。
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
	whereSQL := `WHERE COALESCE(hidden, 0) = 0
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

func (c *Catalog) CountCrawlerAssets(ctx context.Context, crawlerID string, prefixes []string) (CrawlerAssetCounts, error) {
	var out CrawlerAssetCounts
	crawlerID = strings.TrimSpace(crawlerID)
	prefixes = cleanCrawlerIDPrefixes(prefixes)
	if crawlerID == "" || len(prefixes) == 0 {
		return out, nil
	}

	where := make([]string, 0, len(prefixes))
	args := make([]any, 0, 2+len(prefixes))
	args = append(args, crawlerID, crawlerID)
	for range prefixes {
		where = append(where, "id LIKE ? ESCAPE '\\'")
	}
	for _, prefix := range prefixes {
		args = append(args, escapeSQLLike(prefix)+"%")
	}
	query := `SELECT
		        COUNT(*) AS total_count,
		        COUNT(CASE WHEN drive_id = ? THEN 1 END) AS local_count,
		        COUNT(CASE WHEN drive_id != ? THEN 1 END) AS migrated_count,
		        COUNT(CASE WHEN EXISTS (
		                     SELECT 1 FROM videos AS asset_dup
		                      WHERE ` + crawlerAssetEquivalentSQL("asset_dup", "videos") + `
		                        AND COALESCE(asset_dup.thumbnail_url, '') != ''
		                  ) THEN 1 END) AS thumbnail_ready_count,
		        COUNT(CASE WHEN NOT EXISTS (
		                     SELECT 1 FROM videos AS asset_dup
		                      WHERE ` + crawlerAssetEquivalentSQL("asset_dup", "videos") + `
		                        AND COALESCE(asset_dup.thumbnail_url, '') != ''
		                  )
		                     AND COALESCE(thumbnail_url, '') = ''
		                     AND COALESCE(thumbnail_status, 'pending') NOT IN ('failed', 'skipped') THEN 1 END) AS thumbnail_pending_count,
		        COUNT(CASE WHEN NOT EXISTS (
		                     SELECT 1 FROM videos AS asset_dup
		                      WHERE ` + crawlerAssetEquivalentSQL("asset_dup", "videos") + `
		                        AND COALESCE(asset_dup.thumbnail_url, '') != ''
		                  )
		                     AND COALESCE(thumbnail_url, '') = ''
		                     AND COALESCE(thumbnail_status, 'pending') = 'failed' THEN 1 END) AS thumbnail_failed_count,
		        COUNT(CASE WHEN EXISTS (
		                     SELECT 1 FROM videos AS asset_dup
		                      WHERE ` + crawlerAssetEquivalentSQL("asset_dup", "videos") + `
		                        AND COALESCE(asset_dup.preview_status, 'pending') = 'ready'
		                  ) THEN 1 END) AS teaser_ready_count,
		        COUNT(CASE WHEN NOT EXISTS (
		                     SELECT 1 FROM videos AS asset_dup
		                      WHERE ` + crawlerAssetEquivalentSQL("asset_dup", "videos") + `
		                        AND COALESCE(asset_dup.preview_status, 'pending') = 'ready'
		                  )
		                     AND COALESCE(preview_status, 'pending') = 'pending' THEN 1 END) AS teaser_pending_count,
		        COUNT(CASE WHEN NOT EXISTS (
		                     SELECT 1 FROM videos AS asset_dup
		                      WHERE ` + crawlerAssetEquivalentSQL("asset_dup", "videos") + `
		                        AND COALESCE(asset_dup.preview_status, 'pending') = 'ready'
		                  )
		                     AND COALESCE(preview_status, 'pending') = 'failed' THEN 1 END) AS teaser_failed_count,
		        COUNT(CASE WHEN COALESCE(sampled_sha256, '') != ''
		                      OR COALESCE(fingerprint_status, 'pending') = 'ready' THEN 1 END) AS fingerprint_ready_count,
		        COUNT(CASE WHEN size_bytes > 0
		                     AND COALESCE(sampled_sha256, '') = ''
		                     AND COALESCE(fingerprint_status, 'pending') = 'pending' THEN 1 END) AS fingerprint_pending_count,
		        COUNT(CASE WHEN COALESCE(sampled_sha256, '') = ''
		                     AND COALESCE(fingerprint_status, 'pending') = 'failed' THEN 1 END) AS fingerprint_failed_count
		   FROM videos
		  WHERE COALESCE(hidden, 0) = 0
		    AND (` + strings.Join(where, " OR ") + `)`
	err := c.db.QueryRowContext(ctx, query, args...).Scan(
		&out.Total,
		&out.Local,
		&out.Migrated,
		&out.Thumbnail.Ready,
		&out.Thumbnail.Pending,
		&out.Thumbnail.Failed,
		&out.Teaser.Ready,
		&out.Teaser.Pending,
		&out.Teaser.Failed,
		&out.Fingerprint.Ready,
		&out.Fingerprint.Pending,
		&out.Fingerprint.Failed,
	)
	return out, err
}

func crawlerAssetEquivalentSQL(candidateAlias, sourceAlias string) string {
	return fmt.Sprintf(`(%[1]s.id = %[2]s.id
		OR (COALESCE(%[2]s.content_hash, '') != ''
		    AND %[1]s.content_hash = %[2]s.content_hash)
		OR (%[2]s.size_bytes > 0
		    AND COALESCE(%[2]s.sampled_sha256, '') != ''
		    AND %[1]s.size_bytes = %[2]s.size_bytes
		    AND %[1]s.sampled_sha256 = %[2]s.sampled_sha256))`, candidateAlias, sourceAlias)
}

func cleanCrawlerIDPrefixes(prefixes []string) []string {
	out := make([]string, 0, len(prefixes))
	seen := map[string]bool{}
	for _, prefix := range prefixes {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" || seen[prefix] {
			continue
		}
		seen[prefix] = true
		out = append(out, prefix)
	}
	return out
}

func escapeSQLLike(raw string) string {
	raw = strings.ReplaceAll(raw, `\`, `\\`)
	raw = strings.ReplaceAll(raw, `%`, `\%`)
	raw = strings.ReplaceAll(raw, `_`, `\_`)
	return raw
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
		parts = append(parts, "preview_file_id = ''", "preview_local = ''", "preview_updated_at = 0", "preview_status = 'pending'")
	}
	if clearThumbnail {
		parts = append(parts, "thumbnail_url = ''", "thumbnail_updated_at = 0", "thumbnail_status = 'pending'")
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
	// TeaserEnabled 控制是否给本盘生成预览视频；封面生成不受影响。
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
	return c.upsertDrive(ctx, d, true, false)
}

// UpsertDrivePreservingSkipDirIDs writes the authoritative drive fields while
// retaining the current skip_dir_ids value when the row already exists. It is
// intended for admin edits whose request omitted skipDirIds: preserving the
// value in SQL avoids a read-then-write race with the dedicated skip-dir API.
// New rows still receive the normalized value from d (normally an empty list).
func (c *Catalog) UpsertDrivePreservingSkipDirIDs(ctx context.Context, d *Drive) error {
	return c.upsertDrive(ctx, d, false, false)
}

// UpsertDrivePatchingCredentials updates drive metadata while atomically
// applying d.Credentials as a JSON merge patch to the latest stored
// credentials. This prevents an admin edit from rolling back tokens refreshed
// after the edit form was opened.
func (c *Catalog) UpsertDrivePatchingCredentials(ctx context.Context, d *Drive) error {
	return c.upsertDrive(ctx, d, true, true)
}

// UpsertDrivePatchingCredentialsPreservingSkipDirIDs combines credential patch
// semantics with the omitted-skipDirIds behavior used by the admin form.
func (c *Catalog) UpsertDrivePatchingCredentialsPreservingSkipDirIDs(ctx context.Context, d *Drive) error {
	return c.upsertDrive(ctx, d, false, true)
}

func (c *Catalog) upsertDrive(ctx context.Context, d *Drive, replaceSkipDirIDs, patchCredentials bool) error {
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
  credentials    = CASE
                     WHEN ? != 0 THEN json_patch(
                       CASE WHEN json_valid(COALESCE(drives.credentials, '')) THEN drives.credentials ELSE '{}' END,
                       excluded.credentials
                     )
                     ELSE excluded.credentials
                   END,
  status         = excluded.status,
  last_error     = excluded.last_error,
  teaser_enabled = excluded.teaser_enabled,
  skip_dir_ids   = CASE WHEN ? != 0 THEN excluded.skip_dir_ids ELSE drives.skip_dir_ids END,
  updated_at     = excluded.updated_at
`, d.ID, d.Kind, d.Name, d.RootID, d.ScanRootID, string(cred), d.Status, d.LastError, boolToInt(d.TeaserEnabled), string(skipDirsJSON),
		d.CreatedAt.UnixMilli(), d.UpdatedAt.UnixMilli(), boolToInt(patchCredentials), boolToInt(replaceSkipDirIDs))
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
	case "pikpak", "guangyapan":
		if rootID == "0" {
			return ""
		}
		return rootID
	case "onedrive", "googledrive":
		if rootID == "" {
			return "root"
		}
		return rootID
	case "webdav":
		if rootID == "" {
			return "/"
		}
		return path.Clean("/" + rootID)
	case "localstorage", "scriptcrawler":
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
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Drive-scoped scan and crawler state has no foreign-key ownership in the
	// legacy schema. Delete it in the same transaction as the drive so removing
	// a drive cannot leave hidden state behind or affect a later drive that
	// reuses the same ID. Video rows are intentionally handled by the caller:
	// migrated crawler videos already belong to their destination drive and must
	// survive removal of the crawler that originally discovered them.
	for _, query := range []string{
		`DELETE FROM drive_scan_misses WHERE drive_id = ?`,
		`DELETE FROM scans WHERE drive_id = ?`,
		`DELETE FROM crawler_seen_sources WHERE drive_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, query, id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM drives WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// PatchDriveCredentials atomically merges the supplied credential keys into
// the stored JSON object. Runtime token/cookie refreshes must use this method
// instead of UpsertDrive: a driver instance can hold an old Drive snapshot,
// and replacing the full row from that snapshot can roll back independently
// saved settings such as skip_dir_ids or root_id.
func (c *Catalog) PatchDriveCredentials(ctx context.Context, id string, updates map[string]string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("catalog: patch drive credentials: empty id")
	}
	cleaned := make(map[string]string, len(updates))
	for rawKey, value := range updates {
		key := strings.TrimSpace(rawKey)
		if key == "" {
			continue
		}
		cleaned[key] = value
	}
	if len(cleaned) == 0 {
		return nil
	}
	payload, err := json.Marshal(cleaned)
	if err != nil {
		return fmt.Errorf("catalog: marshal drive credential patch: %w", err)
	}
	res, err := c.db.ExecContext(ctx, `
UPDATE drives
   SET credentials = json_patch(
         CASE WHEN json_valid(COALESCE(credentials, '')) THEN credentials ELSE '{}' END,
         ?
       ),
       updated_at = ?
 WHERE id = ?`, string(payload), time.Now().UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("catalog: patch drive credentials: %w", err)
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetDriveRuntimeStatus updates only the connection state observed while the
// server is already running. Playback and other runtime checks must not use
// UpsertDrive here: doing so could overwrite credentials or drive settings with
// a stale in-memory copy.
func (c *Catalog) SetDriveRuntimeStatus(ctx context.Context, id, status, lastError string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("catalog: set drive runtime status: empty id")
	}
	if status != "ok" && status != "error" {
		return fmt.Errorf("catalog: set drive runtime status: invalid status %q", status)
	}
	// Avoid touching updated_at when retries report a state already persisted.
	// This keeps admin polling useful and prevents needless SQLite writes.
	_, err := c.db.ExecContext(ctx, `
UPDATE drives
   SET status = ?, last_error = ?, updated_at = ?
 WHERE id = ?
   AND (COALESCE(status, '') != ? OR COALESCE(last_error, '') != ?)`,
		status, lastError, time.Now().UnixMilli(), id, status, lastError)
	return err
}

// SetDriveTeaserEnabled 切换某盘的预览视频生成开关。
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

type SessionInfo struct {
	ExpiresAt time.Time
	UserID    int64
}

func (c *Catalog) CreateSession(ctx context.Context, token string, ttl time.Duration, userID int64) error {
	now := time.Now()
	return c.CreateSessionUntil(ctx, token, now.Add(ttl), userID)
}

func (c *Catalog) CreateSessionUntil(ctx context.Context, token string, expiresAt time.Time, userID int64) error {
	now := time.Now()
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO admin_sessions (token, created_at, expires_at, user_id) VALUES (?, ?, ?, ?)`,
		token, now.UnixMilli(), expiresAt.UnixMilli(), userID)
	return err
}

func (c *Catalog) GetSession(ctx context.Context, token string) (SessionInfo, bool, error) {
	var expires int64
	var userID int64
	err := c.db.QueryRowContext(ctx,
		`SELECT expires_at, COALESCE(user_id, 0) FROM admin_sessions WHERE token = ?`,
		token).Scan(&expires, &userID)
	if err == sql.ErrNoRows {
		return SessionInfo{}, false, nil
	}
	if err != nil {
		return SessionInfo{}, false, err
	}
	return SessionInfo{
		ExpiresAt: time.UnixMilli(expires),
		UserID:    userID,
	}, true, nil
}

func (c *Catalog) ValidateSession(ctx context.Context, token string) (bool, int64, error) {
	session, found, err := c.GetSession(ctx, token)
	if err != nil || !found {
		return false, 0, err
	}
	return time.Now().Before(session.ExpiresAt), session.UserID, nil
}

func (c *Catalog) UpdateSessionExpires(ctx context.Context, token string, expiresAt time.Time) error {
	res, err := c.db.ExecContext(ctx,
		`UPDATE admin_sessions SET expires_at = ? WHERE token = ?`,
		expiresAt.UnixMilli(), token)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (c *Catalog) DeleteSession(ctx context.Context, token string) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE token = ?`, token)
	return err
}

func (c *Catalog) DeleteSessionsForUser(ctx context.Context, userID int64) error {
	if userID <= 0 {
		return nil
	}
	_, err := c.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE user_id = ?`, userID)
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

// DeleteSettings removes obsolete keys atomically. Runtime state such as
// nightly.last_run_date remains in SQLite; this is used only by explicit
// schema migrations that move durable configuration elsewhere.
func (c *Catalog) DeleteSettings(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DropLegacyDuplicateReviewTable retires the queue left by releases that sent
// the 0.80-0.92 content-similarity band to manual review. Callers invoke this
// only after a successful content-deduplication pass under the new automatic
// threshold, so removing the queue can never replace processing its videos.
func (c *Catalog) DropLegacyDuplicateReviewTable(ctx context.Context) (bool, error) {
	var exists int
	if err := c.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'duplicate_review_pairs'
	`).Scan(&exists); err != nil {
		return false, err
	}
	if exists == 0 {
		return false, nil
	}
	// IF EXISTS keeps the migration idempotent even if a maintenance command and
	// the server reach this boundary at the same time after both observed the
	// legacy table.
	if _, err := c.db.ExecContext(ctx, `DROP TABLE IF EXISTS duplicate_review_pairs`); err != nil {
		return false, err
	}
	return true, nil
}

// ---------- helpers ----------

const allVideoCols = `
id, drive_id, file_id, COALESCE(file_name, ''), COALESCE(content_hash, ''),
COALESCE(sampled_sha256, ''), COALESCE(fingerprint_status, 'pending'), COALESCE(fingerprint_error, ''),
COALESCE(parent_id, ''), COALESCE(dir_name, ''), title, COALESCE(author, ''), COALESCE(tags, '[]'),
duration_seconds, size_bytes, COALESCE(ext, ''), COALESCE(quality, ''), COALESCE(thumbnail_url, ''), COALESCE(thumbnail_updated_at, 0),
COALESCE(preview_file_id, ''), COALESCE(preview_local, ''), COALESCE(preview_updated_at, 0), COALESCE(preview_status, 'pending'),
	COALESCE(transcode_status, ''), COALESCE(transcode_error, ''), COALESCE(transcoded_file_id, ''), COALESCE(transcoded_size, 0),
	views, COALESCE(last_viewed_at, 0), favorites, comments, likes, COALESCE(last_liked_at, 0), dislikes,
	COALESCE(hidden, 0), COALESCE(badges, '[]'), COALESCE(description, ''),
	published_at, created_at, updated_at
	`

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

// uniqueVideoWhereSQL is the hot-path materialized form of the exact legacy
// predicate below. SQLite triggers update it in the same statement/transaction
// as every insert, delete, or dedup-key change.
const uniqueVideoWhereSQL = `videos.is_canonical = 1`

// dynamicUniqueVideoWhereSQL remains the source of truth for migration,
// trigger recomputation and consistency tests. Keep its three independent
// clauses intact: a row may map to different representatives by hash, sampled
// fingerprint, and filename+size, so a single canonical_video_id is lossy.
const dynamicUniqueVideoWhereSQL = `((COALESCE(videos.content_hash, '') = ''
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
	var publishedAt, createdAt, updatedAt, thumbnailUpdatedAt, previewUpdatedAt, lastViewedAt, lastLikedAt int64
	var hidden int
	err := row.Scan(
		&v.ID, &v.DriveID, &v.FileID, &v.FileName, &v.ContentHash,
		&v.SampledSHA256, &v.FingerprintStatus, &v.FingerprintError,
		&v.ParentID, &v.DirName, &v.Title, &v.Author, &tagsJSON,
		&v.DurationSeconds, &v.Size, &v.Ext, &v.Quality, &v.ThumbnailURL, &thumbnailUpdatedAt,
		&v.PreviewFileID, &v.PreviewLocal, &previewUpdatedAt, &v.PreviewStatus,
		&v.TranscodeStatus, &v.TranscodeError, &v.TranscodedFileID, &v.TranscodedSize,
		&v.Views, &lastViewedAt, &v.Favorites, &v.Comments, &v.Likes, &lastLikedAt, &v.Dislikes,
		&hidden, &badgesJSON, &v.Description,
		&publishedAt, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(tagsJSON), &v.Tags)
	_ = json.Unmarshal([]byte(badgesJSON), &v.Badges)
	v.Hidden = hidden == 1
	v.PublishedAt = time.UnixMilli(publishedAt)
	v.CreatedAt = time.UnixMilli(createdAt)
	v.UpdatedAt = time.UnixMilli(updatedAt)
	if thumbnailUpdatedAt > 0 {
		v.ThumbnailUpdatedAt = time.UnixMilli(thumbnailUpdatedAt)
	}
	if previewUpdatedAt > 0 {
		v.PreviewUpdatedAt = time.UnixMilli(previewUpdatedAt)
	}
	if lastViewedAt > 0 {
		v.LastViewedAt = time.UnixMilli(lastViewedAt)
	}
	if lastLikedAt > 0 {
		v.LastLikedAt = time.UnixMilli(lastLikedAt)
	}
	return v, nil
}

func normalizeContentHash(hash string) string {
	return strings.ToLower(strings.TrimSpace(hash))
}

func normalizeDeletedVideoReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case DeletedVideoReasonDuplicate:
		return DeletedVideoReasonDuplicate
	default:
		return ""
	}
}

type deletedVideoRestorePayloadQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func buildDeletedVideoRestorePayload(
	ctx context.Context,
	query deletedVideoRestorePayloadQuerier,
	video *Video,
) ([]byte, error) {
	if video == nil {
		return nil, sql.ErrNoRows
	}
	payload := deletedVideoRestorePayload{
		Version: deletedVideoRestorePayloadVersion,
		Video:   video,
	}
	var tagsManual int
	if err := query.QueryRowContext(ctx,
		`SELECT COALESCE(tags_manual, 0) FROM videos WHERE id = ?`, video.ID,
	).Scan(&tagsManual); err != nil {
		return nil, err
	}
	payload.TagsManual = tagsManual != 0

	rows, err := query.QueryContext(ctx, `
SELECT t.label,
       COALESCE(vt.source, ''),
       COALESCE(vt.evidence, ''),
       COALESCE(vt.created_at, 0),
       COALESCE(t.aliases, '[]'),
       COALESCE(t.match_rules, '{}'),
       COALESCE(t.source, 'user'),
       COALESCE(t.origin, ''),
       COALESCE(t.created_at, 0),
       COALESCE(t.updated_at, 0)
  FROM video_tags vt
  JOIN tags t ON t.id = vt.tag_id
 WHERE vt.video_id = ?
 ORDER BY vt.created_at, t.id`, video.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := make(map[string]bool)
	for rows.Next() {
		var assignment deletedVideoTagRestoreAssignment
		if err := rows.Scan(
			&assignment.Label,
			&assignment.Source,
			&assignment.Evidence,
			&assignment.CreatedAt,
			&assignment.TagAliases,
			&assignment.TagMatchRules,
			&assignment.TagSource,
			&assignment.TagOrigin,
			&assignment.TagCreatedAt,
			&assignment.TagUpdatedAt,
		); err != nil {
			return nil, err
		}
		payload.TagAssignments = append(payload.TagAssignments, assignment)
		seen[strings.ToLower(strings.TrimSpace(assignment.Label))] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Heal old or externally-written rows whose JSON labels have no matching
	// relation. Their provenance is unknowable; preserve an explicit manual lock
	// when present and otherwise leave them replaceable by automatic retagging.
	for _, label := range video.Tags {
		key := strings.ToLower(strings.TrimSpace(label))
		if key == "" || seen[key] {
			continue
		}
		payload.TagAssignments = append(payload.TagAssignments,
			fallbackDeletedVideoTagAssignment(label, payload.TagsManual))
	}
	return json.Marshal(payload)
}

func decodeDeletedVideoRestorePayload(id, encoded string) (deletedVideoRestorePayload, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return deletedVideoRestorePayload{Version: deletedVideoRestorePayloadVersion}, nil
	}
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal([]byte(encoded), &probe); err != nil {
		return deletedVideoRestorePayload{}, fmt.Errorf("catalog: decode restore payload for %s: %w", id, err)
	}
	var payload deletedVideoRestorePayload
	if probe.Version > 0 {
		if probe.Version != deletedVideoRestorePayloadVersion {
			return deletedVideoRestorePayload{}, fmt.Errorf(
				"catalog: decode restore payload for %s: unsupported version %d", id, probe.Version)
		}
		if err := json.Unmarshal([]byte(encoded), &payload); err != nil {
			return deletedVideoRestorePayload{}, fmt.Errorf("catalog: decode restore payload for %s: %w", id, err)
		}
		if payload.Video == nil {
			return deletedVideoRestorePayload{}, fmt.Errorf("catalog: decode restore payload for %s: missing video", id)
		}
	} else {
		// Versions before this follow-up stored a raw Video JSON object. Assignment
		// provenance cannot be recovered, so retaining its labels as manual is the
		// only backward-compatible choice that does not discard user selections.
		var video Video
		if err := json.Unmarshal([]byte(encoded), &video); err != nil {
			return deletedVideoRestorePayload{}, fmt.Errorf("catalog: decode restore payload for %s: %w", id, err)
		}
		payload = deletedVideoRestorePayload{
			Version:    deletedVideoRestorePayloadVersion,
			Video:      &video,
			TagsManual: len(video.Tags) > 0,
		}
	}
	if payload.Video != nil && len(payload.TagAssignments) == 0 {
		for _, label := range payload.Video.Tags {
			payload.TagAssignments = append(payload.TagAssignments,
				fallbackDeletedVideoTagAssignment(label, payload.TagsManual))
		}
	}
	if payload.Video != nil && len(payload.Video.Tags) == 0 && len(payload.TagAssignments) > 0 {
		for _, assignment := range payload.TagAssignments {
			payload.Video.Tags = append(payload.Video.Tags, assignment.Label)
		}
	}
	return payload, nil
}

func fallbackDeletedVideoTagAssignment(label string, manual bool) deletedVideoTagRestoreAssignment {
	assignment := deletedVideoTagRestoreAssignment{
		Label:         strings.TrimSpace(label),
		Source:        "auto",
		TagAliases:    "[]",
		TagMatchRules: "{}",
		TagSource:     "generated",
	}
	if manual {
		assignment.Source = "manual"
		assignment.TagSource = "user"
	}
	return assignment
}

func restoreDeletedVideoTagsTx(
	ctx context.Context,
	tx *sql.Tx,
	video *Video,
	payload deletedVideoRestorePayload,
) error {
	if video == nil {
		return sql.ErrNoRows
	}
	now := time.Now().UnixMilli()
	for _, assignment := range payload.TagAssignments {
		assignment.Label = strings.TrimSpace(assignment.Label)
		if assignment.Label == "" {
			continue
		}
		if !json.Valid([]byte(assignment.TagAliases)) {
			assignment.TagAliases = "[]"
		}
		if !json.Valid([]byte(assignment.TagMatchRules)) {
			assignment.TagMatchRules = "{}"
		}
		if strings.TrimSpace(assignment.TagSource) == "" {
			assignment.TagSource = "generated"
			if payload.TagsManual || assignment.Source == "manual" {
				assignment.TagSource = "user"
			}
		}
		if assignment.TagCreatedAt <= 0 {
			assignment.TagCreatedAt = now
		}
		if assignment.TagUpdatedAt <= 0 {
			assignment.TagUpdatedAt = assignment.TagCreatedAt
		}
		if assignment.CreatedAt <= 0 {
			assignment.CreatedAt = now
		}
		if strings.TrimSpace(assignment.Source) == "" {
			assignment.Source = "auto"
			if payload.TagsManual {
				assignment.Source = "manual"
			}
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO tags (label, aliases, match_rules, source, origin, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(label) DO NOTHING`,
			assignment.Label,
			assignment.TagAliases,
			assignment.TagMatchRules,
			assignment.TagSource,
			assignment.TagOrigin,
			assignment.TagCreatedAt,
			assignment.TagUpdatedAt,
		); err != nil {
			return err
		}
		var tagID int64
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM tags WHERE label = ? COLLATE NOCASE`, assignment.Label,
		).Scan(&tagID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO video_tags (video_id, tag_id, source, evidence, created_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(video_id, tag_id) DO UPDATE SET
  source = excluded.source,
  evidence = excluded.evidence,
  created_at = excluded.created_at`,
			video.ID, tagID, assignment.Source, assignment.Evidence, assignment.CreatedAt,
		); err != nil {
			return err
		}
	}
	return syncVideoTagsJSONTx(ctx, tx, video.ID, payload.TagsManual)
}

// directRestoreVideo rebuilds the catalog row for a direct restore. Derived
// assets were deleted with the original row, so their paths and state are reset
// for application workers. Provider-observed size/timestamps make legacy
// payloads usable and invalidate fingerprints if the retained file changed.
func directRestoreVideo(deleted *DeletedVideo, restoreVideo *Video, source DeletedVideoSourceInfo) *Video {
	video := &Video{}
	if restoreVideo != nil {
		copy := *restoreVideo
		copy.Tags = append([]string(nil), restoreVideo.Tags...)
		copy.Badges = append([]string(nil), restoreVideo.Badges...)
		video = &copy
	}
	originalSize := video.Size
	video.ID = deleted.ID
	video.DriveID = deleted.DriveID
	video.FileID = deleted.FileID
	video.ParentID = deleted.ParentID
	if strings.TrimSpace(video.FileName) == "" {
		video.FileName = strings.TrimSpace(deleted.FileName)
	}
	if strings.TrimSpace(video.FileName) == "" {
		video.FileName = deleted.FileID
	}
	if strings.TrimSpace(video.Title) == "" {
		video.Title = strings.TrimSuffix(video.FileName, path.Ext(video.FileName))
	}
	if strings.TrimSpace(video.Ext) == "" {
		video.Ext = strings.TrimPrefix(strings.ToLower(path.Ext(video.FileName)), ".")
	}
	original := *video
	original.Size = originalSize
	if deletedVideoSourceChanged(deleted, &original, source) {
		video.ContentHash = ""
		video.SampledSHA256 = ""
		video.FingerprintStatus = "pending"
		video.FingerprintError = ""
		video.DurationSeconds = 0
	}
	if video.SampledSHA256 == "" && video.FingerprintStatus == "ready" {
		video.FingerprintStatus = "pending"
		video.FingerprintError = ""
	}
	video.Size = source.Size
	fallbackTime := source.ModTime
	if fallbackTime.IsZero() && deleted.DeletedAt > 0 {
		fallbackTime = time.UnixMilli(deleted.DeletedAt)
	}
	if fallbackTime.IsZero() {
		fallbackTime = time.Now()
	}
	if video.CreatedAt.IsZero() {
		video.CreatedAt = fallbackTime
	}
	if video.PublishedAt.IsZero() {
		video.PublishedAt = video.CreatedAt
	}
	// The row was tombstoned, never hidden; restoring it must not resurrect the
	// deprecated hidden flag even if an old payload carried it.
	video.Hidden = false
	video.ThumbnailURL = ""
	video.ThumbnailUpdatedAt = time.Time{}
	video.PreviewFileID = ""
	video.PreviewLocal = ""
	video.PreviewUpdatedAt = time.Time{}
	video.PreviewStatus = "pending"
	video.TranscodeStatus = ""
	video.TranscodeError = ""
	video.TranscodedFileID = ""
	video.TranscodedSize = 0
	return video
}

func deletedVideoSourceChanged(
	deleted *DeletedVideo,
	video *Video,
	source DeletedVideoSourceInfo,
) bool {
	expectedSize := int64(0)
	if deleted != nil {
		expectedSize = deleted.Size
	}
	if expectedSize <= 0 && video != nil {
		expectedSize = video.Size
	}
	if expectedSize > 0 && source.Size != expectedSize {
		return true
	}
	if video != nil && video.Size != source.Size {
		return true
	}
	// deleted_at is millisecond precision. Requiring a full millisecond beyond
	// it avoids treating a source written in the same truncated millisecond as a
	// post-tombstone replacement.
	if deleted != nil && !source.ModTime.IsZero() && deleted.DeletedAt > 0 &&
		source.ModTime.After(time.UnixMilli(deleted.DeletedAt).Add(time.Millisecond)) {
		return true
	}
	return expectedSize <= 0 && video != nil &&
		(video.ContentHash != "" || video.SampledSHA256 != "")
}

// deletedVideoRestorePolicy decides how (and whether) a tombstone can go back
// to the library. The order matters: a missing source file and a deduplicated
// row are unrestorable no matter which drive they came from, so those are
// checked before the per-drive branches below.
func deletedVideoRestorePolicy(v *DeletedVideo, driveKind string) string {
	if v == nil ||
		v.SourceDeleted ||
		v.Reason == DeletedVideoReasonDuplicate {
		return DeletedVideoRestorePolicyNone
	}
	if strings.TrimSpace(v.DriveID) == "local-upload" {
		return DeletedVideoRestorePolicyDirect
	}
	if strings.TrimSpace(driveKind) == "scriptcrawler" {
		return DeletedVideoRestorePolicyCrawler
	}
	return DeletedVideoRestorePolicyScan
}

func unixMilliOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
