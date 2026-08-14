// 本地开发用的一次性数据放大工具：以现有的某条视频为模板生成大量可列出的
// 行，用来压测列表页的无限滚动 / 虚拟列表。只做 INSERT，不改动既有数据。
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"time"

	_ "modernc.org/sqlite"
)

func main() {
	dbPath := flag.String("db", "./data/video-site.db", "sqlite path")
	count := flag.Int("count", 0, "how many synthetic rows to insert")
	prefix := flag.String("prefix", "seed", "id prefix for generated rows")
	purge := flag.Bool("purge", false, "delete previously generated rows with this prefix")
	flag.Parse()

	db, err := sql.Open("sqlite", *dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	like := *prefix + "-%"

	if *purge {
		res, err := db.Exec(`DELETE FROM videos WHERE id LIKE ?`, like)
		if err != nil {
			log.Fatal(err)
		}
		removed, _ := res.RowsAffected()
		fmt.Printf("purged %d generated rows\n", removed)
	}

	if *count > 0 {
		now := time.Now().UnixMilli()
		// content_hash / sampled_sha256 置空 + file_name 唯一，避免被去重触发器
		// 判成同一条视频的副本而丢掉 is_canonical。
		res, err := db.Exec(`
WITH RECURSIVE seq(n) AS (
    SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?
),
template AS (
    SELECT * FROM videos WHERE id NOT LIKE ? ORDER BY created_at LIMIT 1
)
INSERT INTO videos (
    id, drive_id, file_id, file_name, content_hash, sampled_sha256,
    fingerprint_status, parent_id, dir_name, title, author, tags,
    duration_seconds, size_bytes, ext, quality, thumbnail_url,
    thumbnail_updated_at, thumbnail_status, preview_local, preview_updated_at,
    preview_status, views, favorites, comments, likes, dislikes, hidden,
    is_canonical, badges, description, published_at, created_at, updated_at
)
SELECT
    ? || '-' || printf('%05d', seq.n),
    template.drive_id,
    -- 复用模板行的真实 file_id，生成的视频才真的能播放和出封面。
    template.file_id,
    ? || '-' || printf('%05d', seq.n) || '.mp4',
    '', '', 'pending',
    template.parent_id, template.dir_name,
    '压测视频 ' || printf('%05d', seq.n),
    template.author, template.tags,
    template.duration_seconds,
    COALESCE(template.size_bytes, 0) + seq.n,
    template.ext, template.quality, template.thumbnail_url,
    template.thumbnail_updated_at, template.thumbnail_status,
    template.preview_local, template.preview_updated_at, template.preview_status,
    seq.n % 97, 0, 0, seq.n % 53, 0, 0, 1,
    template.badges, template.description,
    ? - seq.n * 60000, ?, ?
FROM seq, template`,
			*count, like, *prefix, *prefix, now, now, now)
		if err != nil {
			log.Fatal(err)
		}
		inserted, _ := res.RowsAffected()
		fmt.Printf("inserted %d rows\n", inserted)
	}

	var total, listable int
	if err := db.QueryRow(`SELECT COUNT(*) FROM videos`).Scan(&total); err != nil {
		log.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM videos WHERE COALESCE(hidden,0)=0 AND is_canonical=1`).Scan(&listable); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("total rows: %d, listable rows: %d\n", total, listable)
}
