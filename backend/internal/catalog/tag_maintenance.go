package catalog

import (
	"context"
	"encoding/json"

	"github.com/video-site/backend/internal/tagging"
)

// 标签维护按当前规则重算视频，标签定义独立于视频引用数保留。

// retagVideoRow 是重算时读取的最小视频行。
type retagVideoRow struct {
	id               string
	title            string
	author           string
	fileName         string
	dirName          string
	ancestorDirNames []string
	manual           bool
	assignments      []TagAssignment
}

func (c *Catalog) CountVideosForRetag(ctx context.Context) (int, error) {
	var count int
	err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM videos`).Scan(&count)
	return count, err
}

// RetagVideosBatch recalculates engine-managed assignments for one page of
// videos using the existing tag matcher without creating new tag definitions.
// 返回 (本批处理数, 最后一个 id, 是否已到结尾)。
func (c *Catalog) RetagVideosBatch(ctx context.Context, matcher *tagging.Matcher, afterID string, limit int) (int, string, bool, error) {
	if limit <= 0 {
		limit = 500
	}
	query := `
SELECT id, title, COALESCE(author, ''), COALESCE(file_name, ''), COALESCE(dir_name, ''), COALESCE(ancestor_dir_names, ''), COALESCE(tags_manual, 0)
  FROM videos
 WHERE id > ? ORDER BY id ASC LIMIT ?`

	rows, err := c.db.QueryContext(ctx, query, afterID, limit)
	if err != nil {
		return 0, afterID, false, err
	}
	var batch []retagVideoRow
	for rows.Next() {
		var row retagVideoRow
		var dirNamesJSON string
		var manual int
		if err := rows.Scan(&row.id, &row.title, &row.author, &row.fileName, &row.dirName, &dirNamesJSON, &manual); err != nil {
			rows.Close()
			return 0, afterID, false, err
		}
		_ = json.Unmarshal([]byte(dirNamesJSON), &row.ancestorDirNames)
		row.manual = manual == 1
		batch = append(batch, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, afterID, false, err
	}
	if err := rows.Close(); err != nil {
		return 0, afterID, false, err
	}
	if len(batch) == 0 {
		return 0, afterID, true, nil
	}
	for i := range batch {
		if batch[i].manual {
			continue
		}
		batch[i].assignments = matchTagAssignmentsWithMatcher(matcher, batch[i].title, batch[i].fileName, batch[i].author, batch[i].dirName, batch[i].ancestorDirNames...)
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, afterID, false, err
	}
	defer tx.Rollback()
	for _, row := range batch {
		if row.manual {
			continue
		}
		changed, err := replaceAutoVideoTagsTx(ctx, tx, row.id, row.assignments)
		if err != nil {
			return 0, afterID, false, err
		}
		if changed {
			if err := syncVideoTagsJSONTx(ctx, tx, row.id, false); err != nil {
				return 0, afterID, false, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, afterID, false, err
	}
	lastID := batch[len(batch)-1].id
	return len(batch), lastID, len(batch) < limit, nil
}

// ReconcileVideoTags refreshes assignments from current rules.
func (c *Catalog) ReconcileVideoTags(ctx context.Context) error {
	c.tagMaintenanceMu.Lock()
	defer c.tagMaintenanceMu.Unlock()
	if err := c.removeAutomaticTaggingArtifacts(ctx); err != nil {
		return err
	}
	matcher, err := c.Matcher(ctx)
	if err != nil {
		return err
	}
	lastID := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, nextID, done, err := c.RetagVideosBatch(ctx, matcher, lastID, 500)
		if err != nil {
			return err
		}
		lastID = nextID
		if done {
			return nil
		}
	}
}
