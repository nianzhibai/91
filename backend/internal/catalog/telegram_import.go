package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The physical remote_upload_jobs table is shared by HTTP and Telegram imports.
// Keeping its name preserves the existing backup and video-deduplication links.
func (c *Catalog) migrateImports(ctx context.Context) error {
	for _, col := range []struct{ name, definition string }{
		{"source_kind", "TEXT NOT NULL DEFAULT 'http'"},
		{"source_payload", "TEXT NOT NULL DEFAULT ''"},
		{"stage", "TEXT NOT NULL DEFAULT ''"},
		{"retry_count", "INTEGER NOT NULL DEFAULT 0"},
		{"next_attempt", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if err := c.addColumnIfMissing(ctx, "remote_upload_jobs", col.name, col.definition); err != nil {
			return err
		}
	}
	_, err := c.db.ExecContext(ctx, `
 CREATE TABLE IF NOT EXISTS telegram_local_files (
  file_id TEXT PRIMARY KEY, job_id TEXT NOT NULL UNIQUE,
  directory TEXT NOT NULL DEFAULT '', group_key TEXT NOT NULL DEFAULT ''
 );
 CREATE TABLE IF NOT EXISTS telegram_upload_cleanups (
  video_id TEXT PRIMARY KEY,
  source_drive_id TEXT NOT NULL, source_file_id TEXT NOT NULL,
  size_bytes INTEGER NOT NULL, mod_time_ns INTEGER NOT NULL,
  file_identity TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
 );
 CREATE INDEX IF NOT EXISTS idx_telegram_upload_cleanup_source
  ON telegram_upload_cleanups(source_drive_id,source_file_id);
 CREATE TABLE IF NOT EXISTS telegram_connections (
  bot_id INTEGER PRIMARY KEY,
  needs_reconnect INTEGER NOT NULL DEFAULT 0
 );
 CREATE TABLE IF NOT EXISTS telegram_files (
  bot_id INTEGER NOT NULL, file_unique_id TEXT NOT NULL, file_id TEXT NOT NULL,
  job_id TEXT NOT NULL DEFAULT '', video_id TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(bot_id,file_unique_id)
 );
 CREATE TABLE IF NOT EXISTS telegram_receipts (
  bot_id INTEGER NOT NULL, update_id INTEGER NOT NULL, chat_id INTEGER NOT NULL,
  message_id INTEGER NOT NULL, sender_id INTEGER NOT NULL,
  job_id TEXT NOT NULL DEFAULT '', response TEXT NOT NULL DEFAULT '',
  reply_id INTEGER NOT NULL DEFAULT 0, delivered TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 0, next_attempt INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, PRIMARY KEY(bot_id,update_id),
  UNIQUE(bot_id,chat_id,message_id)
 );
 CREATE INDEX IF NOT EXISTS idx_telegram_receipt_jobs ON telegram_receipts(job_id);
 CREATE INDEX IF NOT EXISTS idx_telegram_file_jobs ON telegram_files(job_id);
 CREATE TABLE IF NOT EXISTS telegram_media_group_updates (
  bot_id INTEGER NOT NULL, chat_id INTEGER NOT NULL, media_group_id TEXT NOT NULL,
  message_id INTEGER NOT NULL, update_id INTEGER NOT NULL,
  payload TEXT NOT NULL, received_at INTEGER NOT NULL,
  PRIMARY KEY(bot_id,update_id), UNIQUE(bot_id,chat_id,message_id)
 );
 CREATE INDEX IF NOT EXISTS idx_telegram_media_groups
  ON telegram_media_group_updates(bot_id,chat_id,media_group_id);
 `)
	if err != nil {
		return err
	}
	for _, column := range []string{"job_ids", "duplicate_job_ids"} {
		if err := c.addColumnIfMissing(ctx, "telegram_receipts", column, "TEXT NOT NULL DEFAULT '[]'"); err != nil {
			return err
		}
	}
	for _, column := range []string{"directory", "group_key"} {
		if err := c.addColumnIfMissing(ctx, "telegram_local_files", column, "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	if _, err := c.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_telegram_local_files_group ON telegram_local_files(group_key)`); err != nil {
		return err
	}
	if err := c.dropColumnIfExists(ctx, "telegram_connections", "next_offset"); err != nil {
		return err
	}
	// File identities already name entries in library/. Physical placement now
	// follows the configured storage root, including for existing acquisitions.
	return c.dropColumnIfExists(ctx, "telegram_local_files", "root")
}

type TelegramSource struct {
	BotID           int64  `json:"botId"`
	SenderID        int64  `json:"senderId"`
	FileID          string `json:"fileId"`
	UniqueID        string `json:"uniqueId"`
	FileName        string `json:"fileName"`
	Size            int64  `json:"size"`
	MIME            string `json:"mime"`
	MediaGroupID    string `json:"mediaGroupId,omitempty"`
	MediaGroupTitle string `json:"mediaGroupTitle,omitempty"`
	MediaGroupDate  int64  `json:"mediaGroupDate,omitempty"`
}

type TelegramReceipt struct {
	BotID     int64
	UpdateID  int64
	ChatID    int64
	MessageID int64
	SenderID  int64
	JobID     string
	JobIDs    []string
	// DuplicateJobIDs records videos already saved when this message arrived.
	// Completing a new task later must not turn its original receipt into a duplicate.
	DuplicateJobIDs []string
	Response        string
	ReplyID         int64
	Delivered       string
	Attempts        int
}

// TelegramResponseQueueFull is rendered by the Telegram presentation layer.
const TelegramResponseQueueFull = "queue_full"

// TelegramPaused initializes the bot's restore gate and returns its current state.
// MTProto cursors belong to the receiver session, not the import catalog.
func (c *Catalog) TelegramPaused(ctx context.Context, botID int64) (bool, error) {
	_, err := c.db.ExecContext(ctx, `INSERT OR IGNORE INTO telegram_connections(bot_id,needs_reconnect) VALUES(?,COALESCE((SELECT needs_reconnect FROM telegram_connections WHERE bot_id=0),0))`, botID)
	if err != nil {
		return false, err
	}
	var paused bool
	err = c.db.QueryRowContext(ctx, `SELECT needs_reconnect FROM telegram_connections WHERE bot_id=?`, botID).Scan(&paused)
	return paused, err
}

func (c *Catalog) ResumeTelegram(ctx context.Context, botID int64) error {
	_, err := c.db.ExecContext(ctx, `UPDATE telegram_connections SET needs_reconnect=0 WHERE bot_id=? OR bot_id=0`, botID)
	return err
}

// AcceptTelegramUpdate commits the receipt, file claim and task together.
// Only after it succeeds may the receiver advance its MTProto cursor.
func (c *Catalog) AcceptTelegramUpdate(ctx context.Context, receipt TelegramReceipt, source *TelegramSource, id, title string, limit int) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := acceptTelegramUpdate(ctx, tx, receipt, source, id, title, limit); err != nil {
		return err
	}
	return tx.Commit()
}

func acceptTelegramUpdate(ctx context.Context, tx *sql.Tx, receipt TelegramReceipt, source *TelegramSource, id, title string, limit int) error {
	var exists int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM telegram_receipts WHERE bot_id=? AND (update_id=? OR (chat_id=? AND message_id=?))`, receipt.BotID, receipt.UpdateID, receipt.ChatID, receipt.MessageID).Scan(&exists)
	if err != nil {
		return err
	}
	if exists == 0 {
		if source != nil && receipt.Response == "" {
			var duplicate bool
			receipt.JobID, duplicate, receipt.Response, err = admitTelegramSource(ctx, tx, source, id, title, limit)
			if err != nil {
				return err
			}
			if duplicate {
				receipt.DuplicateJobIDs = []string{receipt.JobID}
			}
		}
		jobIDs := "[]"
		if len(receipt.JobIDs) > 0 {
			payload, err := json.Marshal(receipt.JobIDs)
			if err != nil {
				return err
			}
			jobIDs = string(payload)
		}
		duplicateJobIDs := "[]"
		if len(receipt.DuplicateJobIDs) > 0 {
			payload, err := json.Marshal(receipt.DuplicateJobIDs)
			if err != nil {
				return err
			}
			duplicateJobIDs = string(payload)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO telegram_receipts(bot_id,update_id,chat_id,message_id,sender_id,job_id,job_ids,duplicate_job_ids,response,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, receipt.BotID, receipt.UpdateID, receipt.ChatID, receipt.MessageID, receipt.SenderID, receipt.JobID, jobIDs, duplicateJobIDs, receipt.Response, time.Now().UnixMilli())
		if err != nil {
			return err
		}
	}
	return nil
}

// admitTelegramSource shares file deduplication and queue admission between
// individual messages and media groups received directly or through links.
func admitTelegramSource(ctx context.Context, tx *sql.Tx, source *TelegramSource, id, title string, limit int) (string, bool, string, error) {
	var jobID, videoID string
	err := tx.QueryRowContext(ctx, `SELECT job_id,video_id FROM telegram_files WHERE bot_id=? AND file_unique_id=?`, source.BotID, source.UniqueID).Scan(&jobID, &videoID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", false, "", err
	}
	var state string
	if jobID != "" {
		err = tx.QueryRowContext(ctx, `SELECT state FROM remote_upload_jobs WHERE id=?`, jobID).Scan(&state)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", false, "", err
		}
	}
	var valid int
	if videoID != "" {
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM videos WHERE id=?`, videoID).Scan(&valid); err != nil {
			return "", false, "", err
		}
	}
	if valid == 0 && state != RemoteUploadQueued && state != RemoteUploadDownloading && state != RemoteUploadValidating && state != RemoteUploadSaving {
		var pending int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_upload_jobs WHERE source_kind='telegram' AND state IN ('queued','downloading','validating','saving')`).Scan(&pending); err != nil {
			return "", false, "", err
		}
		if pending >= limit {
			return "", false, TelegramResponseQueueFull, nil
		}
		payload, err := json.Marshal(source)
		if err != nil {
			return "", false, "", err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO remote_upload_jobs(id,source_kind,source_payload,source_label,requested_title,tags,state,total_bytes,created_at,updated_at) VALUES(?,'telegram',?,'Telegram',?,'[]','queued',?,?,?)`, id, string(payload), title, source.Size, time.Now().UnixMilli(), time.Now().UnixMilli())
		if err != nil {
			return "", false, "", err
		}
		jobID = id
		_, err = tx.ExecContext(ctx, `INSERT INTO telegram_files(bot_id,file_unique_id,file_id,job_id,video_id) VALUES(?,?,?,?,'') ON CONFLICT(bot_id,file_unique_id) DO UPDATE SET file_id=excluded.file_id,job_id=excluded.job_id,video_id=''`, source.BotID, source.UniqueID, source.FileID, id)
		if err != nil {
			return "", false, "", err
		}
	}
	// Refresh a bot-scoped file ID without exposing it through task APIs.
	_, err = tx.ExecContext(ctx, `UPDATE telegram_files SET file_id=? WHERE bot_id=? AND file_unique_id=?`, source.FileID, source.BotID, source.UniqueID)
	if err != nil {
		return "", false, "", err
	}
	return jobID, valid > 0, "", nil
}

func (c *Catalog) LatestTelegramFileID(ctx context.Context, s TelegramSource) (string, error) {
	var id string
	err := c.db.QueryRowContext(ctx, `SELECT file_id FROM telegram_files WHERE bot_id=? AND file_unique_id=?`, s.BotID, s.UniqueID).Scan(&id)
	return id, err
}

const telegramVideoIDsSQL = `
 SELECT video_id FROM telegram_files WHERE video_id!=''
 UNION
 SELECT completed_video_id FROM remote_upload_jobs WHERE source_kind='telegram' AND state='completed'
`

// ListLocalTelegramVideos uses durable import provenance, not editable tags.
// A cursor keeps a failed upload from blocking later videos in this sweep.
func (c *Catalog) ListLocalTelegramVideos(ctx context.Context, afterID string, limit int) ([]*Video, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := c.db.QueryContext(ctx, `SELECT `+allVideoCols+` FROM videos
WHERE drive_id IN ('local-upload','telegram-local') AND COALESCE(hidden,0)=0 AND id>?
AND id IN (`+telegramVideoIDsSQL+`)
ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (c *Catalog) InvalidateMissingTelegramVideo(ctx context.Context, botID int64, uniqueID string) error {
	_, err := c.db.ExecContext(ctx, `UPDATE telegram_files SET video_id='' WHERE bot_id=? AND file_unique_id=?`, botID, uniqueID)
	return err
}
func (c *Catalog) TelegramVideoID(ctx context.Context, botID int64, uniqueID string) (string, error) {
	var id string
	err := c.db.QueryRowContext(ctx, `SELECT video_id FROM telegram_files WHERE bot_id=? AND file_unique_id=?`, botID, uniqueID).Scan(&id)
	return id, err
}

// ImportJobFilterActive groups unfinished imports without changing their stored states.
const ImportJobFilterActive = "active"

func (c *Catalog) ListImportJobs(ctx context.Context, source, stateFilter string, before int64, limit int) ([]*RemoteUploadJob, error) {
	if limit < 1 || limit > 100 {
		limit = 30
	}
	query := `SELECT ` + remoteUploadJobCols + ` FROM remote_upload_jobs WHERE (?='' OR source_kind=?)`
	args := []any{source, source}
	switch stateFilter {
	case "":
	case ImportJobFilterActive:
		query += ` AND state IN (?, ?, ?, ?)`
		args = append(args, RemoteUploadQueued, RemoteUploadDownloading, RemoteUploadValidating, RemoteUploadSaving)
	default:
		query += ` AND state=?`
		args = append(args, stateFilter)
	}
	query += ` AND (?=0 OR sequence<?) ORDER BY sequence DESC LIMIT ?`
	args = append(args, before, before, limit)
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*RemoteUploadJob{}
	for rows.Next() {
		j, err := scanRemoteUploadJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (c *Catalog) SetImportStage(ctx context.Context, id, stage string) error {
	return c.updateActiveRemoteUpload(ctx, id, `stage=?,updated_at=?`, stage, time.Now().UnixMilli())
}

func (c *Catalog) DelayImport(ctx context.Context, id, message string, delay time.Duration, count bool) error {
	increment := 0
	if count {
		increment = 1
	}
	res, err := c.db.ExecContext(ctx, `UPDATE remote_upload_jobs SET state='queued',stage='retry_wait',retry_count=retry_count+?,next_attempt=?,error_message=?,temp_file='',final_file='',completed_video_id='',bytes_downloaded=0,updated_at=? WHERE id=? AND cancel_requested=0 AND state IN ('downloading','validating','saving')`, increment, time.Now().Add(delay).UnixMilli(), message, time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return c.remoteUploadTransitionError(ctx, id)
	}
	return nil
}

func (c *Catalog) RetryTelegramImport(ctx context.Context, id string, botID int64) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state, payload string
	err = tx.QueryRowContext(ctx, `SELECT state,source_payload FROM remote_upload_jobs WHERE id=? AND source_kind='telegram'`, id).Scan(&state, &payload)
	if err != nil {
		return err
	}
	if state != RemoteUploadFailed && state != RemoteUploadCanceled {
		return ErrRemoteUploadInvalidTransition
	}
	var s TelegramSource
	if err = json.Unmarshal([]byte(payload), &s); err != nil {
		return err
	}
	if s.BotID != botID {
		return errors.New("请连接提交该任务的机器人后重试")
	}
	var mapped, video string
	if err = tx.QueryRowContext(ctx, `SELECT job_id,video_id FROM telegram_files WHERE bot_id=? AND file_unique_id=?`, botID, s.UniqueID).Scan(&mapped, &video); err != nil {
		return err
	}
	if mapped != id || video != "" {
		return errors.New("该文件已有更新的任务或已保存的视频")
	}
	_, err = tx.ExecContext(ctx, `UPDATE remote_upload_jobs SET state='queued',stage='',retry_count=0,next_attempt=0,cancel_requested=0,error_message='',finished_at=0,updated_at=? WHERE id=?`, time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE telegram_receipts SET delivered='',attempts=0,next_attempt=0 WHERE job_id=? OR EXISTS (SELECT 1 FROM json_each(job_ids) WHERE value=?)`, id, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (c *Catalog) PendingTelegramReceipts(ctx context.Context, botID int64) ([]TelegramReceipt, error) {
	rows, err := c.db.QueryContext(ctx, `
 SELECT r.bot_id,r.update_id,r.chat_id,r.message_id,r.sender_id,r.job_id,r.job_ids,r.duplicate_job_ids,
        r.response,r.reply_id,r.delivered,r.attempts
 FROM telegram_receipts r LEFT JOIN remote_upload_jobs j ON j.id=r.job_id
 WHERE r.bot_id=? AND r.attempts<8 AND r.next_attempt<=? AND r.response!='__ignore__'
 AND (
  r.delivered=''
  OR (r.job_id!='' AND (
   j.state IN ('queued','downloading','validating','saving')
   OR (j.state IN ('completed','failed','canceled') AND r.delivered!=j.state)
  ))
  OR (json_array_length(r.job_ids)>0 AND r.delivered!='group:terminal')
 )
 ORDER BY r.next_attempt,r.created_at LIMIT 30`, botID, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TelegramReceipt{}
	for rows.Next() {
		var r TelegramReceipt
		var jobIDs, duplicateJobIDs string
		if err := rows.Scan(&r.BotID, &r.UpdateID, &r.ChatID, &r.MessageID, &r.SenderID, &r.JobID, &jobIDs, &duplicateJobIDs, &r.Response, &r.ReplyID, &r.Delivered, &r.Attempts); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(jobIDs), &r.JobIDs); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(duplicateJobIDs), &r.DuplicateJobIDs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (c *Catalog) SaveTelegramNotification(ctx context.Context, r TelegramReceipt, reply int64, state string, delay time.Duration) error {
	if delay > 0 {
		_, err := c.db.ExecContext(ctx, `UPDATE telegram_receipts SET attempts=attempts+1,next_attempt=? WHERE bot_id=? AND update_id=?`, time.Now().Add(delay).UnixMilli(), r.BotID, r.UpdateID)
		return err
	}
	// Active tasks are revisited for progress updates at most every five seconds.
	// Scheduling unchanged content too prevents older jobs starving new receipts.
	_, err := c.db.ExecContext(ctx, `UPDATE telegram_receipts SET reply_id=?,delivered=?,attempts=0,next_attempt=? WHERE bot_id=? AND update_id=?`, reply, state, time.Now().Add(5*time.Second).UnixMilli(), r.BotID, r.UpdateID)
	return err
}

func (c *Catalog) TelegramNotificationFailures(ctx context.Context, botID int64) int {
	var n int
	_ = c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM telegram_receipts WHERE bot_id=? AND attempts>=8 AND delivered!='expired'`, botID).Scan(&n)
	return n
}

func DecodeTelegramSource(j *RemoteUploadJob) (TelegramSource, error) {
	var s TelegramSource
	err := json.Unmarshal([]byte(j.SourcePayload), &s)
	if err == nil && (s.BotID == 0 || s.FileID == "" || s.UniqueID == "") {
		err = fmt.Errorf("invalid Telegram source")
	}
	return s, err
}

// MigrateImportDatabase upgrades a staged archive, never the live restore target.
func MigrateImportDatabase(ctx context.Context, db *sql.DB) error {
	return (&Catalog{db: db}).migrateImports(ctx)
}

// CompactTelegramHistory keeps only the small task/result anchors required by
// permanent file deduplication and message idempotency. Expired failures must
// be resubmitted; we no longer retain their downloadable file references.
func (c *Catalog) CompactTelegramHistory(ctx context.Context, before time.Time) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE remote_upload_jobs SET source_payload='' WHERE source_kind='telegram' AND state IN ('completed','failed','canceled') AND finished_at>0 AND finished_at<?`, before.UnixMilli()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE telegram_files SET file_id='' WHERE job_id IN (SELECT id FROM remote_upload_jobs WHERE state IN ('completed','failed','canceled') AND finished_at>0 AND finished_at<?)`, before.UnixMilli()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `
 UPDATE telegram_receipts SET response='__ignore__',delivered='expired',attempts=8
 WHERE created_at<? AND (
  job_id='' OR job_id IN (
   SELECT id FROM remote_upload_jobs
   WHERE state IN ('completed','failed','canceled') AND finished_at>0 AND finished_at<?
  )
 ) AND NOT EXISTS (
  SELECT 1 FROM json_each(job_ids) linked JOIN remote_upload_jobs j ON j.id=linked.value
  WHERE j.state IN ('queued','downloading','validating','saving') OR j.finished_at>=?
 )`, before.UnixMilli(), before.UnixMilli(), before.UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}
