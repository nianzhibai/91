// trace-parents:
// 用数据库里所有 mkv 视频的 parent_id 去 115 上 List 看那些目录在哪儿。
// 再列出 115 根目录下所有 dir，看影视的兄弟目录里是否含 Better Call Saul、Normal People 这种。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives/p115"
)

func main() {
	dbPath := flag.String("db", "data/91.db", "sqlite path")
	flag.Parse()

	cat, err := catalog.Open(*dbPath)
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer cat.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	all, err := cat.ListDrives(ctx)
	if err != nil {
		log.Fatal(err)
	}
	for _, d := range all {
		if d.Kind != "p115" {
			continue
		}
		drv := p115.New(p115.Config{
			ID:     d.ID,
			Cookie: d.Credentials["cookie"],
			RootID: d.RootID,
		})
		if err := drv.Init(ctx); err != nil {
			log.Fatalf("init: %v", err)
		}

		// 1) 列出 115 根目录所有 dir
		fmt.Println("=== 115 根目录下所有目录 ===")
		entries, err := drv.List(ctx, d.RootID)
		if err != nil {
			log.Fatal(err)
		}
		for _, e := range entries {
			if e.IsDir {
				fmt.Printf("  [%s] %q\n", e.ID, e.Name)
			}
		}

		// 2) 数据库中 mkv 视频去重 parent_id
		dbItems, err := cat.ListVideosByDrive(ctx, d.ID)
		if err != nil {
			log.Fatal(err)
		}
		parents := map[string]string{} // parent_id -> sample title
		for _, v := range dbItems {
			if v.Ext == ".mkv" || v.Ext == "mkv" {
				if _, ok := parents[v.ParentID]; !ok {
					parents[v.ParentID] = v.Title
				}
			}
		}
		fmt.Printf("\n=== 数据库里 mkv 视频的 parent_id 共 %d 个 ===\n", len(parents))

		// 3) 对每个 parent_id 调 driver.List，看 115 上这目录还在不在
		// 注意：List 接口接收 dirID 返回该目录下子项，这里我们没法直接拿"目录信息"，
		// 但可以用一招：如果那个 dir 还在并且能 List 出内容，就算可访问。
		// 进一步：把这些 parent_id 当作目录 List 一次，看子项里都是同一系列（剧集）就基本能确定它是剧目录。
		fmt.Println("\n=== 逐个 parent_id 探测 ===")
		for parentID, sampleTitle := range parents {
			fmt.Printf("\nparent_id=%s sample=%q\n", parentID, sampleTitle)
			children, err := drv.List(ctx, parentID)
			if err != nil {
				fmt.Printf("  list error: %v\n", err)
				continue
			}
			// 取前 5 个看
			for i, c := range children {
				if i >= 5 {
					fmt.Printf("  ... (more)\n")
					break
				}
				fmt.Printf("  [%s] dir=%v %q\n", c.ID, c.IsDir, c.Name)
			}
		}

		_ = strings.TrimSpace
	}
}
