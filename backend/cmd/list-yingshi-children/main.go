// 列出影视目录直接子项
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives/p115"
)

func main() {
	cat, err := catalog.Open("data/91.db")
	if err != nil {
		log.Fatal(err)
	}
	defer cat.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	drives, _ := cat.ListDrives(ctx)
	for _, d := range drives {
		if d.Kind != "p115" {
			continue
		}
		drv := p115.New(p115.Config{
			ID:     d.ID,
			Cookie: d.Credentials["cookie"],
			RootID: d.RootID,
		})
		drv.Init(ctx)

		// 影视目录 id 已知
		moviesID := "3385264408451087511"
		entries, err := drv.List(ctx, moviesID)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("=== 影视目录 (%s) 直接子项 共 %d 个 ===\n", moviesID, len(entries))
		for _, e := range entries {
			fmt.Printf("  [%s] dir=%v size=%d %q\n", e.ID, e.IsDir, e.Size, e.Name)
		}

		// 进一步看每个影视子目录里递归到第二层是什么
		for _, e := range entries {
			if !e.IsDir {
				continue
			}
			fmt.Printf("\n--- 影视/%s (%s) ---\n", e.Name, e.ID)
			children, err := drv.List(ctx, e.ID)
			if err != nil {
				fmt.Printf("  list error: %v\n", err)
				continue
			}
			for _, c := range children {
				fmt.Printf("  [%s] dir=%v size=%d %q\n", c.ID, c.IsDir, c.Size, c.Name)
			}
		}
	}
}
