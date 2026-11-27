package catalog

import (
	"context"
	"fmt"
	"time"

	"github.com/nianzhibai/91/backend/internal/applog"
)

const settingCrawlerPublishedAtAligned = "crawler.published_at_aligned_with_created_at_v1"

// alignCrawlerPublishedAtWithCreatedAtOnce repairs crawler videos imported
// before crawler timestamps became backend-owned. The stable scriptcrawler ID
// covers normal and migrated rows; crawler_seen_sources also covers legacy rows
// whose canonical ID did not use that prefix.
func (c *Catalog) alignCrawlerPublishedAtWithCreatedAtOnce(ctx context.Context) (int64, error) {
	marker, err := c.GetSetting(ctx, settingCrawlerPublishedAtAligned, "")
	if err != nil {
		return 0, err
	}
	if marker == "1" {
		return 0, nil
	}

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
UPDATE videos
   SET published_at = created_at
 WHERE created_at > 0
   AND published_at != created_at
   AND (
       id LIKE 'scriptcrawler-%'
       OR EXISTS (
           SELECT 1
             FROM crawler_seen_sources AS source
            WHERE source.kind = 'scriptcrawler'
              AND source.status = 'imported'
              AND source.canonical_video_id = videos.id
       )
   )`)
	if err != nil {
		return 0, fmt.Errorf("align crawler published_at with created_at: %w", err)
	}
	aligned, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count aligned crawler timestamps: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO settings (key, value, updated_at) VALUES (?, '1', ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		settingCrawlerPublishedAtAligned, time.Now().UnixMilli()); err != nil {
		return 0, fmt.Errorf("mark crawler published_at migration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if aligned > 0 {
		applog.Info(ctx, "aligned published_at with created_at for crawler videos", "component", "catalog", "stage", "migrate", "aligned", aligned)
	}
	return aligned, nil
}
