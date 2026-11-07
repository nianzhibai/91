// list-115-yingshi:
//
// 1. 从数据库中读取 115 网盘的 cookie
// 2. 遍历 115 根目录寻找名为"影视"的子目录
// 3. 递归列出该目录下全部视频文件 ID
// 4. 与数据库中的 videos 表交叉，列出所有匹配的视频
// 5. 命令行参数 --apply 时，删除（DeleteVideo）匹配到的视频
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/drives/p115"
)

func main() {
	dbPath := flag.String("db", "data/91.db", "sqlite path")
	apply := flag.Bool("apply", false, "actually delete matching videos (default: dry-run)")
	flag.Parse()

	cat, err := catalog.Open(*dbPath)
	if err != nil {
		log.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	all, err := cat.ListDrives(ctx)
	if err != nil {
		log.Fatalf("list drives: %v", err)
	}
	var p115Drives []*catalog.Drive
	for _, d := range all {
		if d.Kind == "p115" {
			p115Drives = append(p115Drives, d)
		}
	}
	if len(p115Drives) == 0 {
		log.Fatalf("no p115 drive registered")
	}

	for _, d := range p115Drives {
		fmt.Printf("\n========== drive=%s name=%s rootID=%s ==========\n", d.ID, d.Name, d.RootID)
		cookie := d.Credentials["cookie"]
		if cookie == "" {
			fmt.Printf("  drive %s has empty cookie, skip\n", d.ID)
			continue
		}
		drv := p115.New(p115.Config{
			ID:     d.ID,
			Cookie: cookie,
			RootID: d.RootID,
		})
		if err := drv.Init(ctx); err != nil {
			fmt.Printf("  init driver error: %v\n", err)
			continue
		}

		entries, err := drv.List(ctx, d.RootID)
		if err != nil {
			fmt.Printf("  list root error: %v\n", err)
			continue
		}
		var moviesDirs []drives.Entry
		for _, e := range entries {
			if e.IsDir && strings.TrimSpace(e.Name) == "影视" {
				moviesDirs = append(moviesDirs, e)
			}
		}
		if len(moviesDirs) == 0 {
			fmt.Println("  根目录下没有名为 \"影视\" 的目录")
			// 顺便列一下根目录下所有目录帮助排查
			fmt.Println("  根目录下的目录有：")
			for _, e := range entries {
				if e.IsDir {
					fmt.Printf("    [%s] %q\n", e.ID, e.Name)
				}
			}
			continue
		}
		for _, m := range moviesDirs {
			fmt.Printf("  发现影视目录: id=%s name=%q\n", m.ID, m.Name)
		}

		// 收集影视目录下所有文件 ID
		excluded := map[string]struct{}{}
		for _, m := range moviesDirs {
			if err := walkCollectFiles(ctx, drv, m.ID, 0, 8, excluded); err != nil {
				fmt.Printf("  walk %s error: %v\n", m.Name, err)
			}
		}
		fmt.Printf("  影视目录下共收集到 %d 个文件 ID\n", len(excluded))

		// 在数据库里找匹配的视频
		dbItems, err := cat.ListVideosByDrive(ctx, d.ID)
		if err != nil {
			fmt.Printf("  list catalog videos error: %v\n", err)
			continue
		}
		var matched []*catalog.Video
		for _, v := range dbItems {
			if _, ok := excluded[v.FileID]; ok {
				matched = append(matched, v)
			}
		}
		fmt.Printf("  数据库中匹配到 %d 个视频\n", len(matched))
		for i, v := range matched {
			if i >= 30 {
				fmt.Printf("    ... (more, +%d)\n", len(matched)-i)
				break
			}
			fmt.Printf("    file_id=%s  title=%q  ext=%s\n", v.FileID, v.Title, v.Ext)
		}

		// 调试：列出影视目录下前 5 个文件，以及数据库里疑似影视的视频 file_id 样本
		if len(excluded) > 0 {
			fmt.Println("\n  [调试] 影视目录下文件 ID 样本（前 10 个）:")
			n := 0
			for fid := range excluded {
				fmt.Printf("    %s\n", fid)
				n++
				if n >= 10 {
					break
				}
			}
		}
		fmt.Println("\n  [调试] 数据库里 ext=mkv 的视频（前 15 个）:")
		n := 0
		for _, v := range dbItems {
			if v.Ext == ".mkv" || v.Ext == "mkv" {
				_, inExcluded := excluded[v.FileID]
				fmt.Printf("    file_id=%s parent_id=%s in_yingshi=%v title=%q\n",
					v.FileID, v.ParentID, inExcluded, v.Title)
				n++
				if n >= 15 {
					break
				}
			}
		}

		if !*apply {
			fmt.Println("  ** dry-run **，不删除任何数据。带 --apply 重新运行才会真正 DeleteVideo。")
			continue
		}

		// 真正删除
		removed := 0
		for _, v := range matched {
			if err := cat.DeleteVideo(ctx, v.ID); err != nil {
				fmt.Printf("    DeleteVideo %s error: %v\n", v.ID, err)
				continue
			}
			removed++
		}
		fmt.Printf("  已删除 %d 个视频\n", removed)
	}
}

func walkCollectFiles(ctx context.Context, drv drives.Drive, dirID string, depth, maxDepth int, out map[string]struct{}) error {
	if depth >= maxDepth {
		return nil
	}
	entries, err := drv.List(ctx, dirID)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir {
			if err := walkCollectFiles(ctx, drv, e.ID, depth+1, maxDepth, out); err != nil {
				return err
			}
			continue
		}
		if e.ID != "" {
			out[e.ID] = struct{}{}
		}
	}
	return nil
}
