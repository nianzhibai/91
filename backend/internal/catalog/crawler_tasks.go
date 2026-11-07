package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/nianzhibai/91/backend/internal/crawljob"
)

func (c *Catalog) CreateCrawlerTask(ctx context.Context, r crawljob.Result) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, `INSERT INTO crawler_tasks(task_id,drive_id,state,accepted_at,result) VALUES(?,?,?,?,?)`, r.TaskID, r.DriveID, r.State, r.AcceptedAt.UnixMilli(), string(data))
	return err
}
func (c *Catalog) SaveCrawlerTask(ctx context.Context, r crawljob.Result) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	res, err := c.db.ExecContext(ctx, `UPDATE crawler_tasks SET state=?, result=? WHERE task_id=?`, r.State, string(data), r.TaskID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return sql.ErrNoRows
	}
	return err
}
func (c *Catalog) GetCrawlerTask(ctx context.Context, driveID, taskID string) (*crawljob.Result, error) {
	var data string
	err := c.db.QueryRowContext(ctx, `SELECT result FROM crawler_tasks WHERE drive_id=? AND task_id=?`, driveID, taskID).Scan(&data)
	if err != nil {
		return nil, err
	}
	var r crawljob.Result
	err = json.Unmarshal([]byte(data), &r)
	return &r, err
}
func (c *Catalog) ListCrawlerTasks(ctx context.Context, driveID string, limit int) ([]crawljob.Result, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := c.db.QueryContext(ctx, `SELECT result FROM crawler_tasks WHERE drive_id=? ORDER BY accepted_at DESC, rowid DESC LIMIT ?`, driveID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []crawljob.Result{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var r crawljob.Result
		if err := json.Unmarshal([]byte(data), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (c *Catalog) LatestCrawlerTasks(ctx context.Context) (map[string]crawljob.Result, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT result FROM crawler_tasks t WHERE rowid=(SELECT rowid FROM crawler_tasks WHERE drive_id=t.drive_id ORDER BY accepted_at DESC,rowid DESC LIMIT 1)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]crawljob.Result{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var r crawljob.Result
		if err := json.Unmarshal([]byte(data), &r); err != nil {
			return nil, err
		}
		out[r.DriveID] = r
	}
	return out, rows.Err()
}

// InterruptCrawlerTasks is called once at service startup, never on ordinary
// catalog opens (backups and tests can open a second connection to a live DB).
func (c *Catalog) InterruptCrawlerTasks(ctx context.Context) error {
	rows, err := c.db.QueryContext(ctx, `SELECT result FROM crawler_tasks WHERE state IN ('queued','running')`)
	if err != nil {
		return err
	}
	var pending []crawljob.Result
	for rows.Next() {
		var data string
		if err = rows.Scan(&data); err != nil {
			break
		}
		var r crawljob.Result
		if err = json.Unmarshal([]byte(data), &r); err != nil {
			break
		}
		pending = append(pending, r)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, r := range pending {
		r.State = "interrupted"
		r.StopReason = "service_restart"
		r.FinishedAt = time.Now()
		if err := c.SaveCrawlerTask(ctx, r); err != nil {
			return err
		}
	}
	return c.interruptCrawlerUploads(ctx)
}

// CrawlerGenerationFailures reports outcomes for this task's committed videos,
// without counting older failures in the crawler's global worker queues.
func (c *Catalog) CrawlerGenerationFailures(ctx context.Context, videoIDs []string) ([]crawljob.Issue, error) {
	if len(videoIDs) == 0 {
		return nil, nil
	}
	ids, err := json.Marshal(videoIDs)
	if err != nil {
		return nil, err
	}
	rows, err := c.db.QueryContext(ctx, `SELECT id,COALESCE(thumbnail_status,''),COALESCE(preview_status,''),COALESCE(fingerprint_status,'')
 FROM videos WHERE id IN (SELECT value FROM json_each(?))`, string(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var issues []crawljob.Issue
	for rows.Next() {
		var id, thumbnail, preview, fingerprint string
		if err := rows.Scan(&id, &thumbnail, &preview, &fingerprint); err != nil {
			return nil, err
		}
		for _, asset := range []struct{ state, code, message string }{{thumbnail, "thumbnail_failed", "封面生成失败"}, {preview, "preview_failed", "预览生成失败"}, {fingerprint, "fingerprint_failed", "指纹生成失败"}} {
			if asset.state == "failed" {
				issues = append(issues, crawljob.Issue{VideoID: id, Stage: "generation", Code: asset.code, Message: asset.message})
			}
		}
	}
	return issues, rows.Err()
}
