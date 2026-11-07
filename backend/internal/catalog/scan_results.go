package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/nianzhibai/91/backend/internal/driveevents"
	"github.com/nianzhibai/91/backend/internal/scanjob"
)

func (c *Catalog) SaveScanResult(ctx context.Context, result scanjob.Result) (resultErr error) {
	defer c.notifyDriveWrite(&resultErr, result.DriveID, driveevents.ScanResultChanged)
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, `
INSERT INTO scans (drive_id, started_at, finished_at, scanned, added, error, result)
SELECT ?, ?, ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM drives WHERE id = ?)`,
		result.DriveID, result.StartedAt.UnixMilli(), result.FinishedAt.UnixMilli(),
		result.ScannedCount, result.AddedCount, result.Message, string(payload), result.DriveID)
	return err
}

func (c *Catalog) LatestScanResults(ctx context.Context) (map[string]scanjob.Result, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT result FROM scans
WHERE id IN (SELECT MAX(id) FROM scans WHERE result IS NOT NULL GROUP BY drive_id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := make(map[string]scanjob.Result)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var result scanjob.Result
		if err := json.Unmarshal([]byte(payload), &result); err != nil {
			return nil, err
		}
		results[result.DriveID] = result
	}
	return results, rows.Err()
}

func (c *Catalog) LatestScanResult(ctx context.Context, driveID string) (*scanjob.Result, error) {
	var payload string
	err := c.db.QueryRowContext(ctx, `SELECT result FROM scans WHERE drive_id = ? AND result IS NOT NULL ORDER BY id DESC LIMIT 1`, driveID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result scanjob.Result
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		return nil, err
	}
	return &result, nil
}
