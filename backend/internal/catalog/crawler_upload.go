package catalog

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nianzhibai/91/backend/internal/uploadjob"
)

// CrawlerUploadScope fixes the insertion boundary for one sweep. Updating a
// video's drive after upload cannot shift subsequent pages, and newly crawled
// rows wait for the next sweep.
func (c *Catalog) CrawlerUploadScope(ctx context.Context, driveID string) (beforeRowID int64, count int, err error) {
	err = c.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(rowid), 0), COUNT(*) FROM videos
WHERE drive_id = ? AND COALESCE(hidden, 0) = 0`, driveID).Scan(&beforeRowID, &count)
	return
}

func (c *Catalog) ListCrawlerUploadCandidates(ctx context.Context, driveID, afterID string, beforeRowID int64, limit int) ([]*Video, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := c.db.QueryContext(ctx, `SELECT `+allVideoCols+` FROM videos
WHERE drive_id = ? AND COALESCE(hidden, 0) = 0 AND id > ? AND rowid <= ?
ORDER BY id LIMIT ?`, driveID, afterID, beforeRowID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var videos []*Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		videos = append(videos, v)
	}
	return videos, rows.Err()
}

func (c *Catalog) SaveCrawlerUploadResult(ctx context.Context, result uploadjob.Result) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO crawler_upload_results (drive_id, result)
SELECT ?, ? WHERE EXISTS (SELECT 1 FROM drives WHERE id = ?)
ON CONFLICT(drive_id) DO UPDATE SET result = excluded.result`, result.DriveID, string(payload), result.DriveID)
	if err != nil {
		return err
	}
	if result.TaskID != "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO crawler_upload_tasks(task_id,drive_id,parent_task_id,result) VALUES(?,?,?,?) ON CONFLICT(task_id) DO UPDATE SET result=excluded.result`, result.TaskID, result.DriveID, result.ParentTaskID, string(payload)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (c *Catalog) LatestCrawlerUploadResults(ctx context.Context) (map[string]uploadjob.Result, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT result FROM crawler_upload_results`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]uploadjob.Result)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var result uploadjob.Result
		if err := json.Unmarshal([]byte(payload), &result); err != nil {
			return nil, err
		}
		out[result.DriveID] = result
	}
	return out, rows.Err()
}

func (c *Catalog) CrawlerTaskUploads(ctx context.Context, taskID string) ([]uploadjob.Result, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT result FROM crawler_upload_tasks WHERE parent_task_id=?`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []uploadjob.Result{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var r uploadjob.Result
		if err := json.Unmarshal([]byte(data), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (c *Catalog) interruptCrawlerUploads(ctx context.Context) error {
	rows, err := c.db.QueryContext(ctx, `SELECT result FROM crawler_upload_tasks WHERE json_extract(result,'$.state') IN ('queued','running')`)
	if err != nil {
		return err
	}
	var pending []uploadjob.Result
	for rows.Next() {
		var data string
		if err = rows.Scan(&data); err != nil {
			break
		}
		var r uploadjob.Result
		if err = json.Unmarshal([]byte(data), &r); err != nil {
			break
		}
		pending = append(pending, r)
	}
	readErr := rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	for _, r := range pending {
		r.State = "interrupted"
		r.Message = "服务重启中断"
		r.FinishedAt = time.Now()
		data, err := json.Marshal(r)
		if err != nil {
			return err
		}
		tx, err := c.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE crawler_upload_tasks SET result=? WHERE task_id=?`, string(data), r.TaskID); err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE crawler_upload_results SET result=? WHERE drive_id=? AND json_extract(result,'$.taskId')=?`, string(data), r.DriveID, r.TaskID)
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
