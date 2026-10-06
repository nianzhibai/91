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

	"github.com/video-site/backend/internal/driveevents"
	"github.com/video-site/backend/internal/tagging"
)

//go:embed schema.sql
var schemaSQL string

type Catalog struct {
	db          *sql.DB
	driveEvents driveevents.Hub

	// matcher 缓存：按 settings 里的规则版本号失效。标签创建/修改/删除都会
	// bump 版本；Matcher() 每次调用只多花一条单行 SELECT。
	matcherMu      sync.Mutex
	matcherVersion int64
	matcher        *tagging.Matcher

	// tagMaintenanceMu serializes rule edits with full-library maintenance so a
	// matcher built from an older rule set cannot overwrite a just-edited tag.
	tagMaintenanceMu sync.Mutex
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
	// Reserve the writer before a read-modify-write transaction takes a snapshot.
	// Deferred transactions can fail on promotion with SQLITE_BUSY even with a
	// busy timeout. Read-only transactions and ordinary WAL reads stay concurrent.
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	c := &Catalog{db: db}
	if err := c.migrateImports(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate imports: %w", err)
	}
	if err := c.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate catalog: %w", err)
	}
	if err := c.migrateAdminSetup(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate administrator setup: %w", err)
	}
	return c, nil
}

func (c *Catalog) Close() error {
	return c.db.Close()
}

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
	AncestorDirIDs     []string  `json:"ancestorDirIds,omitempty"`
	AncestorDirNames   []string  `json:"ancestorDirNames,omitempty"`
	DirName            string    `json:"dirName"`
	Title              string    `json:"title"`
	Author             string    `json:"author"`
	Tags               []string  `json:"tags"`
	DurationSeconds    int       `json:"durationSeconds"`
	Size               int64     `json:"size"`
	Ext                string    `json:"ext"`
	ThumbnailURL       string    `json:"thumbnailUrl"`
	ThumbnailUpdatedAt time.Time `json:"thumbnailUpdatedAt"`
	PreviewFileID      string    `json:"previewFileId"`
	PreviewLocal       string    `json:"previewLocal"` // relative to <storage.data_dir>/previews
	PreviewUpdatedAt   time.Time `json:"previewUpdatedAt"`
	PreviewStatus      string    `json:"previewStatus"`
	Views              int       `json:"views"`
	LastViewedAt       time.Time `json:"lastViewedAt"`
	Favorites          int       `json:"favorites"`
	Comments           int       `json:"comments"`
	Likes              int       `json:"likes"`
	LastLikedAt        time.Time `json:"lastLikedAt"`
	Dislikes           int       `json:"dislikes"`
	Hidden             bool      `json:"hidden"`
	Badges             []string  `json:"badges"`
	Description        string    `json:"description"`
	PublishedAt        time.Time `json:"publishedAt"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

// VideoSummary is the public card-sized projection of a video. List feeds use
// this instead of loading the full persistence object, whose hashes, storage
// locations, processing state and description are only needed by detail and
// maintenance paths.
type VideoSummary struct {
	ID                 string
	Title              string
	Author             string
	DurationSeconds    int
	ThumbnailURL       string
	ThumbnailUpdatedAt time.Time
	PreviewUpdatedAt   time.Time
	Views              int
	Favorites          int
	Comments           int
	Likes              int
	Dislikes           int
	Badges             []string
	PublishedAt        time.Time
}

// RecommendationCandidateParams describes one bounded recommendation pool.
// Tags are matched as a single "any tag" filter so callers do not need to run
// one public-list query per tag. Empty Tags selects from the whole public
// library.
type RecommendationCandidateParams struct {
	Tags               []string
	ExcludeIDs         []string
	ThumbnailReadyOnly bool
	Limit              int
}

// VideoCollectionOrderItem is the smallest projection needed to calculate a
// video's position in a naturally sorted directory collection. Summary
// requests use it instead of hydrating full Video persistence objects.
type VideoCollectionOrderItem struct {
	ID        string
	FileName  string
	Title     string
	DirName   string
	CreatedAt time.Time
}

func (c *Catalog) UpsertVideo(ctx context.Context, v *Video) error {
	existed := c.videoExists(ctx, v.ID)
	storedTags, err := upsertVideoRow(ctx, c.db, v)
	if err != nil {
		return err
	}
	c.driveEvents.Notify("", false, driveevents.MediaChanged)
	if !existed {
		if len(storedTags) > 0 {
			return c.replaceManualVideoTags(ctx, v.ID, storedTags, true)
		}
		assignments, err := c.MatchTagAssignments(ctx, v.Title, v.FileName, v.Author, v.DirName, v.AncestorDirNames...)
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
// Scan admission and tombstone restoration call it with their existing write
// transaction. General callers use UpsertVideo, which also initializes tags.
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
	ancestorDirIDsJSON := ""
	if v.AncestorDirIDs != nil {
		payload, _ := json.Marshal(v.AncestorDirIDs)
		ancestorDirIDsJSON = string(payload)
	}
	ancestorDirNamesJSON := ""
	if v.AncestorDirNames != nil {
		payload, _ := json.Marshal(v.AncestorDirNames)
		ancestorDirNamesJSON = string(payload)
	}
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
  id, drive_id, file_id, file_name, content_hash, sampled_sha256, fingerprint_status, fingerprint_error, parent_id, ancestor_dir_ids, ancestor_dir_names, dir_name, title, author, tags,
	  duration_seconds, size_bytes, ext, thumbnail_url, thumbnail_updated_at, thumbnail_status,
	  preview_file_id, preview_local, preview_updated_at, preview_status,
	  views, last_viewed_at, favorites, comments, likes, last_liked_at, dislikes,
	  hidden, badges, description, published_at, created_at, updated_at
	) VALUES (
	  ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
	  ?, ?, ?, ?, ?, CASE WHEN COALESCE(?, '') != '' THEN 'ready' ELSE 'pending' END,
	  ?, ?, ?, ?,
	  ?, ?, ?, ?, ?, ?, ?,
	  ?, ?, ?, ?, ?, ?
	)
ON CONFLICT(id) DO UPDATE SET
  file_name       = CASE
                      WHEN excluded.file_name != '' THEN excluded.file_name
                      ELSE videos.file_name
                    END,
  parent_id       = CASE
                      WHEN excluded.parent_id != '' THEN excluded.parent_id
                      ELSE videos.parent_id
                    END,
  ancestor_dir_ids = CASE
                      WHEN excluded.ancestor_dir_ids != '' THEN excluded.ancestor_dir_ids
                      ELSE videos.ancestor_dir_ids
                    END,
  ancestor_dir_names = CASE
                      WHEN excluded.ancestor_dir_names != '' THEN excluded.ancestor_dir_names
                      ELSE videos.ancestor_dir_names
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
		v.ID, v.DriveID, v.FileID, v.FileName, v.ContentHash, v.SampledSHA256, fingerprintStatus, v.FingerprintError, v.ParentID, ancestorDirIDsJSON, ancestorDirNamesJSON, v.DirName, v.Title, v.Author, string(tagsJSON),
		v.DurationSeconds, v.Size, v.Ext, v.ThumbnailURL, thumbnailUpdatedAt, v.ThumbnailURL,
		v.PreviewFileID, v.PreviewLocal, previewUpdatedAt, nullableStatus(v.PreviewStatus),
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

func (c *Catalog) UpdatePreview(ctx context.Context, id, previewLocal, status string) (resultErr error) {
	defer c.notifyDriveWrite(&resultErr, "", driveevents.MediaChanged)
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
		        thumbnail_status = CASE
		          WHEN ? = 'ready'
		           AND COALESCE(?, '') != ''
		           AND COALESCE(thumbnail_url, '') = ''
		            THEN 'pending'
		          ELSE thumbnail_status
		        END,
		        thumbnail_failures = CASE
		          WHEN ? = 'ready'
		           AND COALESCE(?, '') != ''
		           AND COALESCE(thumbnail_url, '') = ''
		            THEN 0
		          ELSE thumbnail_failures
		        END,
		        updated_at = ?
		  WHERE id = ?`,
		previewLocal, status, previewLocal, now, status,
		status, previewLocal, status, previewLocal,
		now, id)
	return err
}

func (c *Catalog) HideVideo(ctx context.Context, id string) (resultErr error) {
	defer c.notifyDriveWrite(&resultErr, "", driveevents.MediaChanged)
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

// VideoDriveMigration is the complete storage identity of a video after a
// cross-drive move. Directory identity belongs to the storage location and
// must be committed with drive/file identity; otherwise readers can observe a
// video on the destination drive while it still belongs to the source folder.
type VideoDriveMigration struct {
	// Optional source identity guards against deletion or another migration
	// while an external upload was in progress.
	SourceDriveID    string
	SourceFileID     string
	DriveID          string
	FileID           string
	ContentHash      string
	ParentID         string
	DirName          string
	AncestorDirNames []string
	FileName         string
	Title            string
}

// MigrateVideoToDrive atomically rewrites a video row after it has been
// uploaded to another drive. The video id is preserved so tags, favorites,
// likes and view records keep pointing at the same logical video.
//
// scanner 后续看到目标目录下相同 hash / file_name 的文件时，会通过
// findDuplicate 命中本行，不会再插入重复行。
func (c *Catalog) MigrateVideoToDrive(ctx context.Context, videoID string, target VideoDriveMigration) (resultErr error) {
	defer c.notifyDriveWrite(&resultErr, "", driveevents.MediaChanged)
	return migrateVideoToDrive(ctx, c.db, videoID, target)
}

func migrateVideoToDrive(ctx context.Context, exec videoRowExecer, videoID string, target VideoDriveMigration) error {
	if strings.TrimSpace(videoID) == "" || strings.TrimSpace(target.DriveID) == "" || strings.TrimSpace(target.FileID) == "" {
		return fmt.Errorf("catalog: migrate video: empty id/drive/file")
	}
	// A new storage location cannot inherit names from the old directory chain.
	dirNames := target.AncestorDirNames
	if dirNames == nil {
		dirNames = []string{}
	}
	dirNamesJSON, _ := json.Marshal(dirNames)
	res, err := exec.ExecContext(ctx,
		`UPDATE videos
		   SET drive_id     = ?,
		       file_id      = ?,
		       content_hash = CASE WHEN ? != '' THEN ? ELSE content_hash END,
		       parent_id    = ?,
		       dir_name     = ?,
		       ancestor_dir_names = ?,
		       file_name    = CASE WHEN ? != '' THEN ? ELSE file_name END,
		       title        = CASE WHEN ? != '' THEN ? ELSE title END,
		       updated_at   = ?
		 WHERE id = ? AND (? = '' OR drive_id = ?) AND (? = '' OR file_id = ?)`,
		target.DriveID,
		target.FileID,
		target.ContentHash,
		target.ContentHash,
		target.ParentID,
		target.DirName,
		string(dirNamesJSON),
		target.FileName,
		target.FileName,
		target.Title,
		target.Title,
		time.Now().UnixMilli(),
		videoID,
		target.SourceDriveID, target.SourceDriveID,
		target.SourceFileID, target.SourceFileID,
	)
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
func (c *Catalog) UpdateVideoFileIdentity(ctx context.Context, videoID, fileID, fileName, title string) (resultErr error) {
	defer c.notifyDriveWrite(&resultErr, "", driveevents.MediaChanged)
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

// VideoMetaPatch 轻量更新视频元数据。大多数字段仍按非零值更新；带 Set
// 标记的字段允许调用方显式写入零值。
type VideoMetaPatch struct {
	ThumbnailURL           string
	ThumbnailStatus        string
	ResetThumbnailFailures bool
	DurationSeconds        int
	DurationSecondsSet     bool
	ContentHash            string
	FileName               string
	ParentID               string
	ParentIDSet            bool
	DirName                string
	DirNameSet             bool
	AncestorDirIDs         []string
	AncestorDirIDsSet      bool
	AncestorDirNames       []string
	AncestorDirNamesSet    bool
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
	if p.DurationSecondsSet || p.DurationSeconds > 0 {
		parts = append(parts, "duration_seconds = ?")
		duration := p.DurationSeconds
		if duration < 0 {
			duration = 0
		}
		args = append(args, duration)
	}
	if p.ContentHash != "" {
		parts = append(parts, "content_hash = ?")
		args = append(args, normalizeContentHash(p.ContentHash))
	}
	if p.FileName != "" {
		parts = append(parts, "file_name = ?")
		args = append(args, p.FileName)
	}
	if p.ParentIDSet || p.ParentID != "" {
		parts = append(parts, "parent_id = ?")
		args = append(args, p.ParentID)
	}
	if p.DirNameSet || p.DirName != "" {
		parts = append(parts, "dir_name = ?")
		args = append(args, p.DirName)
	}
	if p.AncestorDirIDsSet {
		ancestorDirIDs := p.AncestorDirIDs
		if ancestorDirIDs == nil {
			ancestorDirIDs = []string{}
		}
		payload, _ := json.Marshal(ancestorDirIDs)
		parts = append(parts, "ancestor_dir_ids = ?")
		args = append(args, string(payload))
	}
	if p.AncestorDirNamesSet {
		dirNames := p.AncestorDirNames
		if dirNames == nil {
			dirNames = []string{}
		}
		payload, _ := json.Marshal(dirNames)
		parts = append(parts, "ancestor_dir_names = ?")
		args = append(args, string(payload))
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
	c.driveEvents.Notify("", false, driveevents.MediaChanged)
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

// LocalThumbnailReference is a snapshot of the persisted ownership link
// between a video and its generated thumbnail. ThumbnailVersion is used as a
// compare-and-swap token so a filesystem check cannot clear a thumbnail that
// was regenerated after the snapshot was read.
type LocalThumbnailReference struct {
	VideoID          string
	ThumbnailVersion int64
}

// ListCanonicalLocalThumbnailReferences returns videos whose persisted
// thumbnail points at the generated local asset owned by that same video. The
// catalog deliberately does not inspect the filesystem; the application layer
// owns that boundary and decides which references no longer have a backing
// file.
func (c *Catalog) ListCanonicalLocalThumbnailReferences(ctx context.Context) ([]LocalThumbnailReference, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT id, COALESCE(thumbnail_updated_at, 0)
		  FROM videos
		 WHERE thumbnail_url = '/p/thumb/' || id
		 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	references := make([]LocalThumbnailReference, 0)
	for rows.Next() {
		var reference LocalThumbnailReference
		if err := rows.Scan(&reference.VideoID, &reference.ThumbnailVersion); err != nil {
			return nil, err
		}
		references = append(references, reference)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return references, nil
}

// ResetMissingLocalThumbnails atomically returns stale generated-thumbnail
// references to the normal pending state. The URL and version guards preserve
// a thumbnail that may have been refreshed by a concurrent worker after the
// caller inspected the filesystem.
func (c *Catalog) ResetMissingLocalThumbnails(ctx context.Context, references []LocalThumbnailReference) (resultValue int, resultErr error) {
	defer c.notifyDriveWrite(&resultErr, "", driveevents.MediaChanged)
	if len(references) == 0 {
		return 0, nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	statement, err := tx.PrepareContext(ctx, `
		UPDATE videos
		   SET thumbnail_url = '',
		       thumbnail_updated_at = 0,
		       thumbnail_status = 'pending',
		       thumbnail_failures = 0
		 WHERE id = ?
		   AND thumbnail_url = '/p/thumb/' || id
		   AND COALESCE(thumbnail_updated_at, 0) = ?`)
	if err != nil {
		return 0, err
	}
	defer statement.Close()

	seen := make(map[string]struct{}, len(references))
	reset := 0
	for _, reference := range references {
		if _, duplicate := seen[reference.VideoID]; duplicate {
			continue
		}
		seen[reference.VideoID] = struct{}{}
		result, err := statement.ExecContext(ctx, reference.VideoID, reference.ThumbnailVersion)
		if err != nil {
			return 0, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		reset += int(affected)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return reset, nil
}

// LocalPreviewReference is the persisted ownership link between a video and
// its generated teaser file. Filesystem validation belongs to the application
// layer because the catalog must not depend on one host's storage layout.
type LocalPreviewReference struct {
	VideoID      string
	PreviewLocal string
}

// ListReadyLocalPreviewReferences returns teaser files that the catalog claims
// are ready. A ready row without a local path cannot be validated here and is
// intentionally excluded; UpdatePreview never creates that state.
func (c *Catalog) ListReadyLocalPreviewReferences(ctx context.Context) ([]LocalPreviewReference, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT id, preview_local
		  FROM videos
		 WHERE COALESCE(preview_status, 'pending') = 'ready'
		   AND TRIM(COALESCE(preview_local, '')) != ''
		 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	references := make([]LocalPreviewReference, 0)
	for rows.Next() {
		var reference LocalPreviewReference
		if err := rows.Scan(&reference.VideoID, &reference.PreviewLocal); err != nil {
			return nil, err
		}
		references = append(references, reference)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return references, nil
}

// ResetMissingLocalPreviews atomically returns stale ready references to the
// normal generation queue. The exact-path guard protects a teaser that may
// have been regenerated after the application inspected the filesystem.
func (c *Catalog) ResetMissingLocalPreviews(ctx context.Context, references []LocalPreviewReference) (resultValue int, resultErr error) {
	defer c.notifyDriveWrite(&resultErr, "", driveevents.MediaChanged)
	if len(references) == 0 {
		return 0, nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	statement, err := tx.PrepareContext(ctx, `
		UPDATE videos
		   SET preview_file_id = '',
		       preview_local = '',
		       preview_updated_at = 0,
		       preview_status = 'pending',
		       updated_at = ?
		 WHERE id = ?
		   AND COALESCE(preview_status, 'pending') = 'ready'
		   AND preview_local = ?`)
	if err != nil {
		return 0, err
	}
	defer statement.Close()

	seen := make(map[string]struct{}, len(references))
	reset := 0
	now := time.Now().UnixMilli()
	for _, reference := range references {
		if _, duplicate := seen[reference.VideoID]; duplicate {
			continue
		}
		seen[reference.VideoID] = struct{}{}
		result, err := statement.ExecContext(ctx, now, reference.VideoID, reference.PreviewLocal)
		if err != nil {
			return 0, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		reset += int(affected)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return reset, nil
}

// ListVideosNeedingThumbnail only admits missing covers. Remaining missing
// durations are processed after this drive finishes its resource generation.
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
		   AND COALESCE(thumbnail_url, '') = ''
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
		   AND COALESCE(thumbnail_url, '') = ''
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

// ListVisibleVideosByDirectory returns the public videos stored beside the
// current video. A directory identity is scoped by drive: provider folder IDs
// are not globally unique. Empty parent IDs are deliberately rejected because
// crawler and standalone-upload rows use an empty value to mean "no directory";
// grouping those rows would create one unrelated pseudo collection.
func (c *Catalog) ListVisibleVideosByDirectory(ctx context.Context, driveID, parentID string) ([]*Video, error) {
	driveID = strings.TrimSpace(driveID)
	parentID = strings.TrimSpace(parentID)
	if driveID == "" || parentID == "" {
		return []*Video{}, nil
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+allVideoCols+` FROM videos
		 WHERE videos.drive_id = ?
		   AND videos.parent_id = ?
		   AND COALESCE(videos.hidden, 0) = 0
		   AND `+activeDriveWhereSQL+`
		   AND `+uniqueVideoWhereSQL,
		driveID, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Video, 0)
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ListVisibleVideoCollectionOrderByDirectory returns only the fields required
// to calculate a public directory collection's natural order and summary.
func (c *Catalog) ListVisibleVideoCollectionOrderByDirectory(ctx context.Context, driveID, parentID string) ([]VideoCollectionOrderItem, error) {
	driveID = strings.TrimSpace(driveID)
	parentID = strings.TrimSpace(parentID)
	if driveID == "" || parentID == "" {
		return []VideoCollectionOrderItem{}, nil
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT videos.id,
		        COALESCE(videos.file_name, ''),
		        videos.title,
		        COALESCE(videos.dir_name, ''),
		        videos.created_at
		   FROM videos
		  WHERE videos.drive_id = ?
		    AND videos.parent_id = ?
		    AND COALESCE(videos.hidden, 0) = 0
		    AND `+activeDriveWhereSQL+`
		    AND `+uniqueVideoWhereSQL,
		driveID, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]VideoCollectionOrderItem, 0)
	for rows.Next() {
		var item VideoCollectionOrderItem
		var createdAt int64
		if err := rows.Scan(
			&item.ID,
			&item.FileName,
			&item.Title,
			&item.DirName,
			&createdAt,
		); err != nil {
			return nil, err
		}
		item.CreatedAt = time.UnixMilli(createdAt)
		items = append(items, item)
	}
	return items, rows.Err()
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
		 WHERE drive_id NOT IN ('local-upload','telegram-local')
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
		`SELECT SUBSTR(id, ?) FROM videos v WHERE substr(id,1,length(?))=? AND instr(substr(v.id,?), 'v3~')!=1 AND NOT EXISTS(SELECT 1 FROM crawler_seen_sources s WHERE s.canonical_video_id=v.id AND s.kind=? AND s.drive_id=?)
 UNION
 SELECT SUBSTR(id, ?) FROM deleted_videos v WHERE substr(id,1,length(?))=? AND instr(substr(v.id,?), 'v3~')!=1 AND NOT EXISTS(SELECT 1 FROM crawler_seen_sources s WHERE s.canonical_video_id=v.id AND s.kind=? AND s.drive_id=?)
		 UNION
		 SELECT source_id FROM crawler_seen_sources
		  WHERE kind = ? AND drive_id = ? AND status IN ('imported', 'duplicate')`,
		len([]rune(prefix))+1, prefix, prefix, len([]rune(prefix))+1, kind, driveID, len([]rune(prefix))+1, prefix, prefix, len([]rune(prefix))+1, kind, driveID, kind, driveID)
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
// source IDs are filtered by the coordinator before resolving or downloading.
func (c *Catalog) MarkCrawlerSourceSeen(ctx context.Context, kind, driveID, sourceID, status, canonicalVideoID, sampledSHA256 string, size int64) error {
	return markCrawlerSourceSeen(ctx, c.db, kind, driveID, sourceID, status, canonicalVideoID, sampledSHA256, size)
}

func markCrawlerSourceSeen(ctx context.Context, exec videoRowExecer, kind, driveID, sourceID, status, canonicalVideoID, sampledSHA256 string, size int64) error {
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
	_, err := exec.ExecContext(ctx, `
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
func (c *Catalog) DeleteVideoWithTombstoneOptions(ctx context.Context, id string, options DeleteVideoTombstoneOptions) (resultErr error) {
	defer c.notifyDriveWrite(&resultErr, "", driveevents.MediaChanged)
	if options.SourceDeleted {
		return c.DeleteVideo(ctx, id)
	}
	options = normalizeDeleteVideoTombstoneOptions(options)
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
	if err := deleteVideoWithTombstoneTx(ctx, tx, restoreVideo, options); err != nil {
		return err
	}
	return tx.Commit()
}

func normalizeDeleteVideoTombstoneOptions(options DeleteVideoTombstoneOptions) DeleteVideoTombstoneOptions {
	options.Reason = normalizeDeletedVideoReason(options.Reason)
	options.CanonicalVideoID = strings.TrimSpace(options.CanonicalVideoID)
	if options.Reason != DeletedVideoReasonDuplicate {
		options.CanonicalVideoID = ""
	}
	return options
}

// deleteVideoWithTombstoneTx moves one already-loaded video into the tombstone
// table. Owning the transaction in the caller lets a full dedupe plan redirect
// references and remove every group member atomically.
func deleteVideoWithTombstoneTx(ctx context.Context, tx *sql.Tx, restoreVideo *Video, options DeleteVideoTombstoneOptions) error {
	if restoreVideo == nil {
		return sql.ErrNoRows
	}
	restorePayloadData, err := buildDeletedVideoRestorePayload(ctx, tx, restoreVideo)
	if err != nil {
		return fmt.Errorf("catalog: encode deleted video restore payload: %w", err)
	}
	restorePayload := string(restorePayloadData)

	restoreVideo.ContentHash = normalizeContentHash(restoreVideo.ContentHash)

	// 先记录这次视频关联的 tag_id，便于事务末尾清理孤儿自动生成标签。
	tagIDs, err := collectVideoTagIDs(ctx, tx, restoreVideo.ID)
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM video_tags WHERE video_id = ?`, restoreVideo.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM video_shares WHERE video_id = ?`, restoreVideo.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM video_reaction_visits WHERE video_id = ?`, restoreVideo.ID); err != nil {
		return err
	}
	if err := deleteDriveScanMissForVideoTx(ctx, tx, restoreVideo.ID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM videos WHERE id = ?`, restoreVideo.ID)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	if err := pruneOrphanGeneratedTagsByID(ctx, tx, tagIDs); err != nil {
		return err
	}
	return nil
}

func (c *Catalog) DeleteVideo(ctx context.Context, id string) (resultErr error) {
	defer c.notifyDriveWrite(&resultErr, "", driveevents.MediaChanged)
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
	if err := deleteDriveScanMissForVideoTx(ctx, tx, id); err != nil {
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

func deleteDriveScanMissForVideoTx(ctx context.Context, tx *sql.Tx, videoID string) error {
	_, err := tx.ExecContext(ctx, `
DELETE FROM drive_scan_misses
 WHERE EXISTS (
       SELECT 1
         FROM videos
        WHERE videos.id = ?
          AND videos.drive_id = drive_scan_misses.drive_id
          AND videos.file_id = drive_scan_misses.file_id
 )`, videoID)
	return err
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
) (resultValue DeletedVideoRestoreResult, resultErr error) {
	defer c.notifyDriveWrite(&resultErr, "", driveevents.MediaChanged)
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

func (c *Catalog) UpdateVideoFingerprint(ctx context.Context, id, sampledSHA256, status, errText string) (resultErr error) {
	defer c.notifyDriveWrite(&resultErr, "", driveevents.GenerationChanged)
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
	SourceKind            string // telegram; import provenance independent of current drive
	Tag                   string
	Sort                  string // latest | hot | recent
	LikedOnly             bool
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

type videoListQuery struct {
	whereSQL string
	orderBy  string
	args     []any
}

// buildVideoListQuery owns the public-list filtering and ordering contract.
// Both paginated responses and cursor-feed snapshots must use this exact query;
// otherwise the two views can silently disagree about which videos are visible
// or how ties are ordered.
func buildVideoListQuery(p ListParams) videoListQuery {
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
	if p.SourceKind == "telegram" {
		where = append(where, "videos.id IN ("+telegramVideoIDsSQL+")")
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
	if p.LikedOnly {
		where = append(where, "videos.likes > 0")
	}
	where = append(where, "COALESCE(hidden, 0) = 0")
	where = append(where, activeDriveWhereSQL)
	where = append(where, uniqueVideoWhereSQL)

	whereSQL := " WHERE " + strings.Join(where, " AND ")

	readyOrderPrefix := ""
	if p.PreferReadyThumbnails {
		readyOrderPrefix = "CASE WHEN COALESCE(videos.thumbnail_url, '') != '' THEN 0 ELSE 1 END, "
	}

	// Every order ends in videos.id so a snapshot has one deterministic total
	// order even when timestamps and reaction counters are tied.
	orderBy := " ORDER BY " + readyOrderPrefix + "videos.published_at DESC, videos.id ASC"
	switch p.Sort {
	case "hot":
		// 热度 = 点赞数；点赞数相同按最近点赞时间，最后用发布时间兜底。
		orderBy = " ORDER BY " + readyOrderPrefix + "videos.likes DESC, COALESCE(videos.last_liked_at, 0) DESC, videos.published_at DESC, videos.id ASC"
	case "recent":
		orderBy = " ORDER BY " + readyOrderPrefix + "COALESCE(videos.last_viewed_at, 0) DESC, videos.published_at DESC, videos.id ASC"
	}

	return videoListQuery{whereSQL: whereSQL, orderBy: orderBy, args: args}
}

func (c *Catalog) ListVideos(ctx context.Context, p ListParams) ([]*Video, int, error) {
	if p.PageSize <= 0 {
		p.PageSize = 24
	}
	if p.Page <= 0 {
		p.Page = 1
	}
	query := buildVideoListQuery(p)

	var total int
	if !p.SkipTotal {
		if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM videos"+query.whereSQL, query.args...).Scan(&total); err != nil {
			return nil, 0, err
		}
	}

	// list
	offset := (p.Page - 1) * p.PageSize
	queryArgs := append(append([]any(nil), query.args...), p.PageSize, offset)
	rows, err := c.db.QueryContext(ctx,
		"SELECT "+allVideoCols+" FROM videos"+query.whereSQL+query.orderBy+" LIMIT ? OFFSET ?",
		queryArgs...)
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
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// ListVideoIDs freezes the ordered identity set for one public-list query.
// A positive limit bounds the snapshot; zero leaves it unbounded. Feed handlers
// page through this immutable ID slice instead of applying OFFSET repeatedly
// to a live, mutable ordering.
func (c *Catalog) ListVideoIDs(ctx context.Context, p ListParams, limit int) ([]string, error) {
	query := buildVideoListQuery(p)
	querySQL := "SELECT videos.id FROM videos" + query.whereSQL + query.orderBy
	if limit > 0 {
		querySQL += " LIMIT ?"
		query.args = append(query.args, limit)
	}
	rows, err := c.db.QueryContext(ctx, querySQL, query.args...)
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

const videoSummaryCols = `
videos.id, videos.title, COALESCE(videos.author, ''),
COALESCE(videos.duration_seconds, 0), COALESCE(videos.thumbnail_url, ''),
COALESCE(videos.thumbnail_updated_at, 0), COALESCE(videos.preview_updated_at, 0),
COALESCE(videos.views, 0), COALESCE(videos.favorites, 0),
COALESCE(videos.comments, 0), COALESCE(videos.likes, 0), COALESCE(videos.dislikes, 0),
COALESCE(videos.badges, '[]'), videos.published_at
`

// ListRecommendationCandidates loads one small, latest-first candidate window
// using the same public visibility and deduplication rules as video listings.
// It deliberately returns VideoSummary rather than the full persistence model:
// recommendation cards do not need hashes, storage paths, processing state or
// descriptions.
func (c *Catalog) ListRecommendationCandidates(ctx context.Context, p RecommendationCandidateParams) ([]*VideoSummary, error) {
	if p.Limit <= 0 {
		return nil, nil
	}

	tags := uniqueStrings(cleanLabels(p.Tags))
	excluded := cleanVideoIDs(p.ExcludeIDs)
	where := []string{
		"COALESCE(videos.hidden, 0) = 0",
		activeDriveWhereSQL,
		uniqueVideoWhereSQL,
	}
	args := make([]any, 0, len(tags)+len(excluded)+1)

	if p.ThumbnailReadyOnly {
		where = append(where, "COALESCE(videos.thumbnail_url, '') != ''")
	}
	if len(tags) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(tags)), ",")
		where = append(where, `videos.id IN (
			SELECT representative.representative_id
			  FROM video_tags vt
			  JOIN tags tag_filter ON tag_filter.id = vt.tag_id
			  JOIN videos tagged ON tagged.id = vt.video_id
			  JOIN video_dedup_representatives representative
			    ON representative.video_id = tagged.id
			 WHERE tag_filter.label COLLATE NOCASE IN (`+placeholders+`)
			   AND COALESCE(tagged.hidden, 0) = 0
		)`)
		for _, tag := range tags {
			args = append(args, tag)
		}
	}
	if len(excluded) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(excluded)), ",")
		where = append(where, "videos.id NOT IN ("+placeholders+")")
		for _, id := range excluded {
			args = append(args, id)
		}
	}
	args = append(args, p.Limit)

	rows, err := c.db.QueryContext(ctx,
		`SELECT `+videoSummaryCols+` FROM videos
		  WHERE `+strings.Join(where, " AND ")+`
		  ORDER BY videos.published_at DESC, videos.id ASC
		  LIMIT ?`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*VideoSummary, 0, p.Limit)
	for rows.Next() {
		video, err := scanVideoSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, video)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// VisibleVideoSummariesByIDs loads only the fields rendered by public video
// cards. Detail, playback and maintenance fields deliberately stay out of this
// hot path. Results retain the caller's snapshot order.
func (c *Catalog) VisibleVideoSummariesByIDs(ctx context.Context, ids []string) ([]*VideoSummary, error) {
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
		`SELECT `+videoSummaryCols+` FROM videos
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

	byID := make(map[string]*VideoSummary, len(cleaned))
	for rows.Next() {
		video, err := scanVideoSummary(rows)
		if err != nil {
			return nil, err
		}
		byID[video.ID] = video
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]*VideoSummary, 0, len(byID))
	for _, id := range cleaned {
		if video := byID[id]; video != nil {
			out = append(out, video)
		}
	}
	return out, nil
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

// DriveAssetStats groups the three aggregate maps consumed together by the
// admin drive list. A single query computes an exact snapshot while scanning
// videos once rather than three times.
type DriveAssetStats struct {
	Teasers      map[string]DriveTeaserCounts
	Thumbnails   map[string]DriveThumbnailCounts
	Fingerprints map[string]DriveFingerprintCounts
}

// CountDriveAssetStats reads one exact snapshot for every drive. Thumbnail and
// teaser counts retain the public canonical-video filter; fingerprint work
// deliberately includes duplicate rows, matching the former independent
// queries.
func (c *Catalog) CountDriveAssetStats(ctx context.Context) (DriveAssetStats, error) {
	return c.countDriveAssetStats(ctx, "")
}

func (c *Catalog) CountDriveAssetStatsForDrive(ctx context.Context, driveID string) (DriveAssetStats, error) {
	return c.countDriveAssetStats(ctx, driveID)
}

func (c *Catalog) countDriveAssetStats(ctx context.Context, driveID string) (DriveAssetStats, error) {
	query := `SELECT drive_id,
		        COUNT(CASE WHEN is_canonical = 1
		                     AND COALESCE(preview_status, 'pending') = 'ready' THEN 1 END) AS teaser_ready_count,
		        COUNT(CASE WHEN is_canonical = 1
		                     AND COALESCE(preview_status, 'pending') = 'pending' THEN 1 END) AS teaser_pending_count,
		        COUNT(CASE WHEN is_canonical = 1
		                     AND COALESCE(preview_status, 'pending') = 'failed' THEN 1 END) AS teaser_failed_count,
		        COUNT(CASE WHEN is_canonical = 1
		                     AND COALESCE(thumbnail_url, '') != '' THEN 1 END) AS thumbnail_ready_count,
		        COUNT(CASE WHEN is_canonical = 1
		                     AND COALESCE(thumbnail_url, '') = ''
		                     AND COALESCE(thumbnail_status, 'pending') NOT IN ('failed', 'skipped') THEN 1 END) AS thumbnail_pending_count,
		        COUNT(CASE WHEN is_canonical = 1
		                     AND COALESCE(thumbnail_url, '') = ''
		                     AND COALESCE(thumbnail_status, 'pending') = 'failed' THEN 1 END) AS thumbnail_failed_count,
		        COUNT(CASE WHEN is_canonical = 1
		                     AND COALESCE(thumbnail_url, '') != ''
		                     AND COALESCE(duration_seconds, 0) <= 0 THEN 1 END) AS duration_pending_count,
		        COUNT(CASE WHEN COALESCE(sampled_sha256, '') != ''
		                      OR COALESCE(fingerprint_status, 'pending') = 'ready' THEN 1 END) AS ready_count,
		        COUNT(CASE WHEN size_bytes > 0
		                     AND COALESCE(sampled_sha256, '') = ''
		                     AND COALESCE(fingerprint_status, 'pending') = 'pending' THEN 1 END) AS pending_count,
		        COUNT(CASE WHEN COALESCE(sampled_sha256, '') = ''
		                     AND COALESCE(fingerprint_status, 'pending') = 'failed' THEN 1 END) AS failed_count
		   FROM videos
		  WHERE COALESCE(hidden, 0) = 0
		`
	var args []any
	if driveID != "" {
		query += " AND drive_id = ?"
		args = append(args, driveID)
	}
	query += " GROUP BY drive_id"
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return DriveAssetStats{}, err
	}
	defer rows.Close()

	out := DriveAssetStats{
		Teasers:      make(map[string]DriveTeaserCounts),
		Thumbnails:   make(map[string]DriveThumbnailCounts),
		Fingerprints: make(map[string]DriveFingerprintCounts),
	}
	for rows.Next() {
		var driveID string
		var teaser DriveTeaserCounts
		var thumbnail DriveThumbnailCounts
		var fingerprint DriveFingerprintCounts
		if err := rows.Scan(
			&driveID,
			&teaser.Ready,
			&teaser.Pending,
			&teaser.Failed,
			&thumbnail.Ready,
			&thumbnail.Pending,
			&thumbnail.Failed,
			&thumbnail.DurationPending,
			&fingerprint.Ready,
			&fingerprint.Pending,
			&fingerprint.Failed,
		); err != nil {
			return DriveAssetStats{}, err
		}
		out.Teasers[driveID] = teaser
		out.Thumbnails[driveID] = thumbnail
		out.Fingerprints[driveID] = fingerprint
	}
	if err := rows.Err(); err != nil {
		return DriveAssetStats{}, err
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
	return c.listLocalMediaRefs(ctx, "")
}

func (c *Catalog) ListLocalMediaRefsForDrive(ctx context.Context, driveID string) ([]LocalMediaRef, error) {
	return c.listLocalMediaRefs(ctx, driveID)
}

func (c *Catalog) listLocalMediaRefs(ctx context.Context, driveID string) ([]LocalMediaRef, error) {
	query := `SELECT drive_id, id, COALESCE(preview_local, '') FROM videos`
	var args []any
	if driveID != "" {
		query += " WHERE drive_id = ?"
		args = append(args, driveID)
	}
	rows, err := c.db.QueryContext(ctx, query, args...)
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
func (c *Catalog) ClearGeneratedAssets(ctx context.Context, videoID string, clearPreview, clearThumbnail bool) (resultErr error) {
	defer c.notifyDriveWrite(&resultErr, "", driveevents.MediaChanged)
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
	// SkipDirIDs 是用户在管理后台为该盘选定的"扫描跳过目录"集合（网盘侧的目录 fileID）。
	// scanner 发现阶段命中后不递归、不收集文件，也不参与缺失确认。名单变化后，
	// 下一次扫盘会先执行策略清理，让这些目录的历史记录直接退出媒体库管理范围。
	// 含义按"目录 ID 自身"匹配，所以同名目录在不同父级下需要分别选定。
	SkipDirIDs []string  `json:"skipDirIds,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

type DriveUpsertOptions struct {
	ReplaceSkipDirIDs bool
	PatchCredentials  bool
}

// UpsertDrive persists a complete configuration. On an existing row it
// deliberately preserves status/last_error; those fields are owned by the
// mounted runtime and must be changed through SetDriveRuntimeStatus.
func (c *Catalog) UpsertDrive(ctx context.Context, d *Drive) error {
	return c.upsertDrive(ctx, d, DriveUpsertOptions{
		ReplaceSkipDirIDs: true,
	})
}

// UpsertDriveWithOptions lets partial admin forms preserve independently saved
// settings atomically instead of performing stale read-then-write merges.
func (c *Catalog) UpsertDriveWithOptions(ctx context.Context, d *Drive, options DriveUpsertOptions) error {
	return c.upsertDrive(ctx, d, options)
}

// UpsertDrivePreservingSkipDirIDs writes the authoritative drive fields while
// retaining the current skip_dir_ids value when the row already exists. It is
// intended for admin edits whose request omitted skipDirIds: preserving the
// value in SQL avoids a read-then-write race with the dedicated skip-dir API.
// New rows still receive the normalized value from d (normally an empty list).
func (c *Catalog) UpsertDrivePreservingSkipDirIDs(ctx context.Context, d *Drive) error {
	return c.upsertDrive(ctx, d, DriveUpsertOptions{})
}

// UpsertDrivePatchingCredentials updates drive metadata while atomically
// applying d.Credentials as a JSON merge patch to the latest stored
// credentials. This prevents an admin edit from rolling back tokens refreshed
// after the edit form was opened.
func (c *Catalog) UpsertDrivePatchingCredentials(ctx context.Context, d *Drive) error {
	return c.upsertDrive(ctx, d, DriveUpsertOptions{
		ReplaceSkipDirIDs: true,
		PatchCredentials:  true,
	})
}

// UpsertDrivePatchingCredentialsPreservingSkipDirIDs combines credential patch
// semantics with the omitted-skipDirIds behavior used by the admin form.
func (c *Catalog) UpsertDrivePatchingCredentialsPreservingSkipDirIDs(ctx context.Context, d *Drive) error {
	return c.upsertDrive(ctx, d, DriveUpsertOptions{
		PatchCredentials: true,
	})
}

func (c *Catalog) upsertDrive(ctx context.Context, d *Drive, options DriveUpsertOptions) (resultErr error) {
	// A new/recreated drive also invalidates cached missing-resource results.
	defer c.notifyDriveWrite(&resultErr, d.ID, driveevents.DriveChanged)
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
INSERT INTO drives (id, kind, name, root_id, scan_root_id, credentials, status, last_error, skip_dir_ids, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  kind           = excluded.kind,
  name           = excluded.name,
  root_id        = excluded.root_id,
  scan_root_id   = excluded.scan_root_id,
  credentials    = CASE
                     WHEN ? != 0 AND excluded.kind = 'googledrive' THEN json_remove(
                       json_patch(
                         CASE WHEN json_valid(COALESCE(drives.credentials, '')) THEN drives.credentials ELSE '{}' END,
                         excluded.credentials
                       ),
                       '$.use_online_api', '$.api_url_address'
                     )
                     WHEN ? != 0 THEN json_patch(
                       CASE WHEN json_valid(COALESCE(drives.credentials, '')) THEN drives.credentials ELSE '{}' END,
                       excluded.credentials
                     )
                     ELSE excluded.credentials
                   END,
  status         = drives.status,
  last_error     = drives.last_error,
  skip_dir_ids   = CASE WHEN ? != 0 THEN excluded.skip_dir_ids ELSE drives.skip_dir_ids END,
  updated_at     = excluded.updated_at
`, d.ID, d.Kind, d.Name, d.RootID, d.ScanRootID, string(cred), d.Status, d.LastError, string(skipDirsJSON),
		d.CreatedAt.UnixMilli(), d.UpdatedAt.UnixMilli(), boolToInt(options.PatchCredentials), boolToInt(options.PatchCredentials),
		boolToInt(options.ReplaceSkipDirIDs))
	return err
}

func normalizeDriveRootFields(d *Drive) {
	if d == nil {
		return
	}
	d.RootID = NormalizeDriveRootID(d.Kind, d.RootID)
	d.ScanRootID = d.RootID
}

// NormalizeDriveRootID returns the canonical runtime root for a provider. API
// validation uses the same function before reserving a configuration update so
// root-change classification cannot diverge from persistence.
func NormalizeDriveRootID(kind, rootID string) string {
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
	rows, err := c.db.QueryContext(ctx, `SELECT id, kind, name, root_id, COALESCE(scan_root_id, ''), COALESCE(credentials, '{}'), status, COALESCE(last_error, ''), COALESCE(skip_dir_ids, '[]'), created_at, updated_at FROM drives ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Drive
	for rows.Next() {
		d := &Drive{}
		var credsStr, skipDirsStr string
		var createdAt, updatedAt int64
		if err := rows.Scan(&d.ID, &d.Kind, &d.Name, &d.RootID, &d.ScanRootID, &credsStr, &d.Status, &d.LastError, &skipDirsStr, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(credsStr), &d.Credentials)
		_ = json.Unmarshal([]byte(skipDirsStr), &d.SkipDirIDs)
		normalizeDriveRootFields(d)
		d.CreatedAt = time.UnixMilli(createdAt)
		d.UpdatedAt = time.UnixMilli(updatedAt)
		out = append(out, d)
	}
	return out, nil
}

func (c *Catalog) GetDrive(ctx context.Context, id string) (*Drive, error) {
	row := c.db.QueryRowContext(ctx, `SELECT id, kind, name, root_id, COALESCE(scan_root_id, ''), COALESCE(credentials, '{}'), status, COALESCE(last_error, ''), COALESCE(skip_dir_ids, '[]'), created_at, updated_at FROM drives WHERE id = ?`, id)
	d := &Drive{}
	var credsStr, skipDirsStr string
	var createdAt, updatedAt int64
	if err := row.Scan(&d.ID, &d.Kind, &d.Name, &d.RootID, &d.ScanRootID, &credsStr, &d.Status, &d.LastError, &skipDirsStr, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(credsStr), &d.Credentials)
	_ = json.Unmarshal([]byte(skipDirsStr), &d.SkipDirIDs)
	normalizeDriveRootFields(d)
	d.CreatedAt = time.UnixMilli(createdAt)
	d.UpdatedAt = time.UnixMilli(updatedAt)
	return d, nil
}

func (c *Catalog) DeleteDrive(ctx context.Context, id string) (resultErr error) {
	defer c.notifyDriveWrite(&resultErr, id, driveevents.DriveChanged)
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
		`DELETE FROM drive_skip_cleanup_legacy_dirs WHERE drive_id = ?`,
		`DELETE FROM scans WHERE drive_id = ?`,
		`DELETE FROM crawler_upload_results WHERE drive_id = ?`,
		`DELETE FROM crawler_seen_sources WHERE drive_id = ?`,
		`DELETE FROM crawler_discoveries WHERE drive_id = ?`,
		`DELETE FROM crawler_tasks WHERE drive_id = ?`,
		`DELETE FROM crawler_upload_tasks WHERE drive_id = ?`,
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
func (c *Catalog) PatchDriveCredentials(ctx context.Context, id string, updates map[string]string) (resultErr error) {
	defer c.notifyDriveWrite(&resultErr, id, driveevents.DriveMetadataChanged)
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

// PatchDriveCredentialsIfMatch applies a runtime credential rotation only
// while both its provider kind and source credential are still current. It
// prevents a refresh that completed after an administrator changed provider or
// supplied a replacement token/cookie from overwriting that explicit edit. A
// false result is a normal stale-write rejection, not an error.
func (c *Catalog) PatchDriveCredentialsIfMatch(
	ctx context.Context,
	id, expectedKind, anchorKey, anchorValue string,
	updates map[string]string,
) (resultValue bool, resultErr error) {
	defer func() {
		if resultValue {
			c.notifyDriveWrite(&resultErr, id, driveevents.DriveMetadataChanged)
		}
	}()
	id = strings.TrimSpace(id)
	expectedKind = strings.TrimSpace(expectedKind)
	anchorKey = strings.TrimSpace(anchorKey)
	if id == "" {
		return false, fmt.Errorf("catalog: patch drive credentials if match: empty id")
	}
	if expectedKind == "" {
		return false, fmt.Errorf("catalog: patch drive credentials if match: empty expected kind")
	}
	if anchorKey == "" {
		return false, fmt.Errorf("catalog: patch drive credentials if match: empty anchor key")
	}
	cleaned := make(map[string]string, len(updates))
	for rawKey, value := range updates {
		key := strings.TrimSpace(rawKey)
		if key != "" {
			cleaned[key] = value
		}
	}
	if len(cleaned) == 0 {
		return true, nil
	}
	payload, err := json.Marshal(cleaned)
	if err != nil {
		return false, fmt.Errorf("catalog: marshal conditional drive credential patch: %w", err)
	}
	res, err := c.db.ExecContext(ctx, `
UPDATE drives
   SET credentials = json_patch(
         CASE WHEN json_valid(COALESCE(credentials, '')) THEN credentials ELSE '{}' END,
         ?
       ),
       updated_at = ?
 WHERE id = ?
   AND kind = ?
   AND COALESCE(json_extract(
         CASE WHEN json_valid(COALESCE(credentials, '')) THEN credentials ELSE '{}' END,
         ?
       ), '') = ?`, string(payload), time.Now().UnixMilli(), id, expectedKind, "$."+anchorKey, anchorValue)
	if err != nil {
		return false, fmt.Errorf("catalog: patch drive credentials if match: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("catalog: inspect conditional drive credential patch: %w", err)
	}
	return rows > 0, nil
}

// UpdateDriveRuntimeState atomically records a runtime status transition and
// a small credential-state patch without replacing configuration columns. It
// is used by crawler completion, where last_crawl_at and the observed result
// belong to one event. Administrative configuration must not call this method.
func (c *Catalog) UpdateDriveRuntimeState(
	ctx context.Context,
	id, expectedKind, status, lastError string,
	credentialUpdates map[string]string,
) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("catalog: update drive runtime state: empty id")
	}
	expectedKind = strings.TrimSpace(expectedKind)
	if expectedKind == "" {
		return fmt.Errorf("catalog: update drive runtime state: empty expected kind")
	}
	if status != "ok" && status != "error" {
		return fmt.Errorf("catalog: update drive runtime state: invalid status %q", status)
	}
	cleaned := make(map[string]string, len(credentialUpdates))
	for rawKey, value := range credentialUpdates {
		if key := strings.TrimSpace(rawKey); key != "" {
			cleaned[key] = value
		}
	}
	payload, err := json.Marshal(cleaned)
	if err != nil {
		return fmt.Errorf("catalog: marshal drive runtime credential patch: %w", err)
	}
	res, err := c.db.ExecContext(ctx, `
UPDATE drives
   SET credentials = json_patch(
         CASE WHEN json_valid(COALESCE(credentials, '')) THEN credentials ELSE '{}' END,
         ?
       ),
       status = ?,
       last_error = ?,
       updated_at = ?
 WHERE id = ?
   AND kind = ?`, string(payload), status, lastError, time.Now().UnixMilli(), id, expectedKind)
	if err != nil {
		return fmt.Errorf("catalog: update drive runtime state: %w", err)
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
func (c *Catalog) SetDriveRuntimeStatus(ctx context.Context, id, status, lastError string) (resultErr error) {
	defer c.notifyDriveWrite(&resultErr, id, driveevents.DriveMetadataChanged)
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

// SetDriveSkipDirIDs 重写某盘的"扫描跳过目录"集合（直接覆盖，不做增量合并）。
//
// 与 UpsertDrive 的区别：只动 skip_dir_ids + updated_at，不要求调用方重传
// kind / name / credentials 等字段（避免管理后台保存跳过目录时把凭证误覆盖）。
//
// 入参 ids 可以是 nil 或空切片，等价于"清空跳过列表"。元素会按字符串原样存储；
// 调用方负责在保存前 trim/去重；这里只保证编码成 JSON 数组。
//
// drive 不存在时返回 sql.ErrNoRows，调用方可以照此返回 404。
func (c *Catalog) SetDriveSkipDirIDs(ctx context.Context, id string, ids []string) (resultErr error) {
	defer c.notifyDriveWrite(&resultErr, id, driveevents.DriveMetadataChanged)
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

// CreateVerifiedUserSession prevents a login verified before a password reset
// from creating a fresh session after that reset revoked the old sessions.
func (c *Catalog) CreateVerifiedUserSession(ctx context.Context, token string, expiresAt time.Time, userID int64, passwordHash string) (bool, error) {
	result, err := c.db.ExecContext(ctx, `
INSERT INTO admin_sessions (token, created_at, expires_at, user_id)
SELECT ?, ?, ?, id FROM users WHERE id = ? AND password = ? AND banned = 0`,
		token, time.Now().UnixMilli(), expiresAt.UnixMilli(), userID, passwordHash)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
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

// ---------- helpers ----------

const allVideoCols = `
id, drive_id, file_id, COALESCE(file_name, ''), COALESCE(content_hash, ''),
COALESCE(sampled_sha256, ''), COALESCE(fingerprint_status, 'pending'), COALESCE(fingerprint_error, ''),
COALESCE(parent_id, ''), COALESCE(ancestor_dir_ids, ''), COALESCE(ancestor_dir_names, ''), COALESCE(dir_name, ''), title, COALESCE(author, ''), COALESCE(tags, '[]'),
duration_seconds, size_bytes, COALESCE(ext, ''), COALESCE(thumbnail_url, ''), COALESCE(thumbnail_updated_at, 0),
COALESCE(preview_file_id, ''), COALESCE(preview_local, ''), COALESCE(preview_updated_at, 0), COALESCE(preview_status, 'pending'),
	views, COALESCE(last_viewed_at, 0), favorites, comments, likes, COALESCE(last_liked_at, 0), dislikes,
	COALESCE(hidden, 0), COALESCE(badges, '[]'), COALESCE(description, ''),
	published_at, created_at, updated_at
	`

const activeDriveWhereSQL = `(videos.drive_id IN ('local-upload','telegram-local')
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

func scanVideoSummary(row rowScanner) (*VideoSummary, error) {
	video := &VideoSummary{}
	var thumbnailUpdatedAt, previewUpdatedAt, publishedAt int64
	var badgesJSON string
	if err := row.Scan(
		&video.ID,
		&video.Title,
		&video.Author,
		&video.DurationSeconds,
		&video.ThumbnailURL,
		&thumbnailUpdatedAt,
		&previewUpdatedAt,
		&video.Views,
		&video.Favorites,
		&video.Comments,
		&video.Likes,
		&video.Dislikes,
		&badgesJSON,
		&publishedAt,
	); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(badgesJSON), &video.Badges)
	if thumbnailUpdatedAt > 0 {
		video.ThumbnailUpdatedAt = time.UnixMilli(thumbnailUpdatedAt)
	}
	if previewUpdatedAt > 0 {
		video.PreviewUpdatedAt = time.UnixMilli(previewUpdatedAt)
	}
	video.PublishedAt = time.UnixMilli(publishedAt)
	return video, nil
}

func scanVideo(row rowScanner) (*Video, error) {
	v := &Video{}
	var ancestorDirIDsJSON, ancestorDirNamesJSON, tagsJSON, badgesJSON string
	var publishedAt, createdAt, updatedAt, thumbnailUpdatedAt, previewUpdatedAt, lastViewedAt, lastLikedAt int64
	var hidden int
	err := row.Scan(
		&v.ID, &v.DriveID, &v.FileID, &v.FileName, &v.ContentHash,
		&v.SampledSHA256, &v.FingerprintStatus, &v.FingerprintError,
		&v.ParentID, &ancestorDirIDsJSON, &ancestorDirNamesJSON, &v.DirName, &v.Title, &v.Author, &tagsJSON,
		&v.DurationSeconds, &v.Size, &v.Ext, &v.ThumbnailURL, &thumbnailUpdatedAt,
		&v.PreviewFileID, &v.PreviewLocal, &previewUpdatedAt, &v.PreviewStatus,
		&v.Views, &lastViewedAt, &v.Favorites, &v.Comments, &v.Likes, &lastLikedAt, &v.Dislikes,
		&hidden, &badgesJSON, &v.Description,
		&publishedAt, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	if ancestorDirIDsJSON != "" {
		_ = json.Unmarshal([]byte(ancestorDirIDsJSON), &v.AncestorDirIDs)
	}
	if ancestorDirNamesJSON != "" {
		_ = json.Unmarshal([]byte(ancestorDirNamesJSON), &v.AncestorDirNames)
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
INSERT INTO tags (label, match_rules, source, origin, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(label) DO NOTHING`,
			assignment.Label,
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
	if strings.TrimSpace(v.DriveID) == "local-upload" || strings.TrimSpace(v.DriveID) == TelegramLocalDriveID {
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
