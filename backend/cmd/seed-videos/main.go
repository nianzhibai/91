// 本地开发用的一次性数据放大工具：以现有视频为模板生成可列出的测试行，
// 用来压测无限滚动和虚拟列表。生成记录会单独登记，清理时不会按通配符猜测。
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"regexp"
	"time"

	_ "modernc.org/sqlite"
)

var safeSeedPrefix = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)

type seedResult struct {
	purged   int64
	inserted int64
	total    int64
	listable int64
}

func main() {
	dbPath := flag.String("db", "./data/video-site.db", "sqlite path")
	count := flag.Int("count", 0, "how many synthetic rows to insert")
	prefix := flag.String("prefix", "seed", "generated-row namespace")
	purge := flag.Bool("purge", false, "delete generated rows registered under this namespace")
	flag.Parse()

	result, err := seedVideos(*dbPath, *count, *prefix, *purge)
	if err != nil {
		log.Fatal(err)
	}
	if *purge {
		fmt.Printf("purged %d registered rows\n", result.purged)
	}
	if *count > 0 {
		fmt.Printf("inserted %d registered rows\n", result.inserted)
	}
	fmt.Printf("total rows: %d, listable rows: %d\n", result.total, result.listable)
}

func seedVideos(dbPath string, count int, prefix string, purge bool) (seedResult, error) {
	var result seedResult
	if !safeSeedPrefix.MatchString(prefix) {
		return result, fmt.Errorf("prefix must be 1-32 ASCII letters, digits, hyphens or underscores, and start with a letter or digit")
	}
	if count < 0 {
		return result, fmt.Errorf("count cannot be negative")
	}

	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return result, fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return result, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS dev_seed_video_rows (
    video_id   TEXT PRIMARY KEY,
    namespace  TEXT NOT NULL,
    created_at INTEGER NOT NULL
)`); err != nil {
		return result, fmt.Errorf("create seed registry: %w", err)
	}

	if purge {
		res, err := tx.Exec(`
DELETE FROM videos
 WHERE id IN (
     SELECT video_id FROM dev_seed_video_rows WHERE namespace = ?
 )`, prefix)
		if err != nil {
			return result, fmt.Errorf("purge registered videos: %w", err)
		}
		result.purged, err = res.RowsAffected()
		if err != nil {
			return result, fmt.Errorf("count purged videos: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM dev_seed_video_rows WHERE namespace = ?`, prefix); err != nil {
			return result, fmt.Errorf("purge seed registry: %w", err)
		}
	}

	if count > 0 {
		now := time.Now().UnixMilli()
		res, err := tx.Exec(`
WITH RECURSIVE seq(n) AS (
    SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?
),
template AS (
    SELECT *
      FROM videos
     WHERE id NOT IN (SELECT video_id FROM dev_seed_video_rows)
       AND COALESCE(hidden, 0) = 0
       AND is_canonical = 1
       AND (
            drive_id = 'local-upload'
            OR EXISTS (SELECT 1 FROM drives WHERE drives.id = videos.drive_id)
            OR NOT EXISTS (SELECT 1 FROM drives)
       )
     ORDER BY created_at, id
     LIMIT 1
)
INSERT INTO videos (
    id, drive_id, file_id, file_name, content_hash, sampled_sha256,
    fingerprint_status, fingerprint_error, parent_id, dir_name, title, author, tags,
    duration_seconds, size_bytes, ext, thumbnail_url,
    thumbnail_updated_at, thumbnail_status, preview_local, preview_updated_at,
    preview_status, views, favorites, comments, likes, dislikes, hidden,
    is_canonical, badges, description, published_at, created_at, updated_at
)
SELECT
    ? || '-' || printf('%05d', seq.n),
    template.drive_id,
    template.file_id,
    ? || '-' || printf('%05d', seq.n) || '.mp4',
    '', '', 'failed', 'development seed row; fingerprint disabled',
    template.parent_id, template.dir_name,
    '压测视频 ' || printf('%05d', seq.n),
    template.author, template.tags,
    template.duration_seconds,
    COALESCE(template.size_bytes, 0) + seq.n,
    template.ext, template.thumbnail_url,
    template.thumbnail_updated_at, template.thumbnail_status,
    template.preview_local, template.preview_updated_at, template.preview_status,
    seq.n % 97, 0, 0, seq.n % 53, 0, 0, 1,
    template.badges, template.description,
    ? - seq.n * 60000, ?, ?
FROM seq, template`, count, prefix, prefix, now, now, now)
		if err != nil {
			return result, fmt.Errorf("insert seed videos: %w", err)
		}
		result.inserted, err = res.RowsAffected()
		if err != nil {
			return result, fmt.Errorf("count inserted videos: %w", err)
		}
		if result.inserted != int64(count) {
			return result, fmt.Errorf(
				"inserted %d rows, expected %d; database needs one visible non-seed template video",
				result.inserted,
				count,
			)
		}

		if _, err := tx.Exec(`
WITH RECURSIVE seq(n) AS (
    SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?
)
INSERT INTO dev_seed_video_rows (video_id, namespace, created_at)
SELECT ? || '-' || printf('%05d', seq.n), ?, ? FROM seq`, count, prefix, prefix, now); err != nil {
			return result, fmt.Errorf("register seed videos: %w", err)
		}
	}

	if err := tx.QueryRow(`SELECT COUNT(*) FROM videos`).Scan(&result.total); err != nil {
		return result, fmt.Errorf("count all videos: %w", err)
	}
	if err := tx.QueryRow(`
SELECT COUNT(*)
  FROM videos
 WHERE COALESCE(hidden, 0) = 0
   AND is_canonical = 1
   AND (
        drive_id = 'local-upload'
        OR EXISTS (SELECT 1 FROM drives WHERE drives.id = videos.drive_id)
        OR NOT EXISTS (SELECT 1 FROM drives)
   )`).Scan(&result.listable); err != nil {
		return result, fmt.Errorf("count listable videos: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("commit seed transaction: %w", err)
	}
	return result, nil
}
