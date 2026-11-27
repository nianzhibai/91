package catalog

import (
	"context"
	"fmt"
	"time"

	"github.com/nianzhibai/91/backend/internal/applog"
)

const settingCrawlerSyntheticAuthorsCleaned = "crawler.synthetic_authors_cleaned_v1"

// clearSyntheticCrawlerAuthorsOnce removes author values historically filled
// with a crawler drive ID or crawler display name. Crawler videos retain their
// source identity in the stable video ID and crawler tag, so these values do
// not belong in the author field.
func (c *Catalog) clearSyntheticCrawlerAuthorsOnce(ctx context.Context) (int64, error) {
	marker, err := c.GetSetting(ctx, settingCrawlerSyntheticAuthorsCleaned, "")
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
   SET author = ''
 WHERE trim(COALESCE(author, '')) != ''
   AND EXISTS (
       SELECT 1
         FROM drives d
        WHERE d.kind = 'scriptcrawler'
          AND substr(videos.id, 1, length('scriptcrawler-' || d.id || '-')) = 'scriptcrawler-' || d.id || '-'
          AND (
              lower(trim(videos.author)) = lower(trim(d.id))
              OR lower(trim(videos.author)) = lower(trim(d.name))
          )
   )`)
	if err != nil {
		return 0, fmt.Errorf("clear synthetic crawler authors: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count cleared synthetic crawler authors: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO settings (key, value, updated_at) VALUES (?, '1', ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		settingCrawlerSyntheticAuthorsCleaned, time.Now().UnixMilli()); err != nil {
		return 0, fmt.Errorf("mark synthetic crawler authors cleaned: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if removed > 0 {
		applog.Info(ctx, "cleared synthetic crawler author values", "component", "catalog", "stage", "migrate", "removed", removed)
	}
	return removed, nil
}
