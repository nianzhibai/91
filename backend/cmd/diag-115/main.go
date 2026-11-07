package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	dbPath := "data/91.db"
	if len(os.Args) > 1 {
		dbPath = os.Args[1]
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	// 找疑似影视的：扩展名是 mkv 或者标题里有 S0xExx 这种集数格式
	q := `
SELECT id, file_id, parent_id, title, file_name, ext, duration_seconds
FROM videos
WHERE drive_id IN (SELECT id FROM drives WHERE kind='p115')
  AND COALESCE(hidden,0)=0
  AND (
    ext='.mkv'
    OR title LIKE '%S0%E0%' OR title LIKE '%S1%E0%' OR title LIKE '%S2%E0%'
    OR title LIKE '%2160p%' OR title LIKE '%1080p%' OR title LIKE '%720p%'
    OR title LIKE '%WEB-DL%' OR title LIKE '%BluRay%' OR title LIKE '%H.265%' OR title LIKE '%x265%' OR title LIKE '%x264%'
  )
LIMIT 60`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	count := 0
	parentSet := map[string]int{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		rows.Scan(ptrs...)
		row := ""
		for i, c := range cols {
			row += fmt.Sprintf("%s=%v ", c, vals[i])
		}
		fmt.Println(row)
		if pid, ok := vals[2].(string); ok {
			parentSet[pid]++
		}
		count++
	}
	fmt.Printf("\n命中 %d 条\n", count)

	fmt.Println("\n这些视频的 parent_id 分布：")
	for k, v := range parentSet {
		fmt.Printf("  parent=%s count=%d\n", k, v)
	}
}
