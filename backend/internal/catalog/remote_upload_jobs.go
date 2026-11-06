package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	RemoteUploadQueued      = "queued"
	RemoteUploadDownloading = "downloading"
	RemoteUploadValidating  = "validating"
	RemoteUploadSaving      = "saving"
	RemoteUploadCompleted   = "completed"
	RemoteUploadFailed      = "failed"
	RemoteUploadCanceled    = "canceled"
)

var (
	ErrRemoteUploadCanceled          = errors.New("remote upload was canceled")
	ErrRemoteUploadTerminal          = errors.New("remote upload is already finished")
	ErrRemoteUploadInvalidTransition = errors.New("remote upload state changed")
)

type RemoteUploadJob struct {
	ID               string
	SourceKind       string
	SourcePayload    string
	Stage            string
	RetryCount       int
	NextAttempt      int64
	Sequence         int64
	SourceURL        string
	SourceLabel      string
	RequestedTitle   string
	ResolvedTitle    string
	Tags             []string
	State            string
	BytesDownloaded  int64
	TotalBytes       int64
	CancelRequested  bool
	ErrorMessage     string
	TempFile         string
	FinalFile        string
	CompletedVideoID string
	CreatedAt        time.Time
	StartedAt        time.Time
	UpdatedAt        time.Time
	FinishedAt       time.Time
}

func (j *RemoteUploadJob) Terminal() bool {
	if j == nil {
		return false
	}
	switch j.State {
	case RemoteUploadCompleted, RemoteUploadFailed, RemoteUploadCanceled:
		return true
	default:
		return false
	}
}

func (j *RemoteUploadJob) CanCancel() bool {
	return j != nil && !j.Terminal() && !j.CancelRequested
}

type RemoteUploadCleanupRef struct {
	JobID     string
	TempFile  string
	FinalFile string
}

func (c *Catalog) CreateRemoteUploadJob(
	ctx context.Context,
	id, sourceURL, sourceLabel, requestedTitle string,
	tags []string,
) (*RemoteUploadJob, error) {
	tags = uniqueStrings(cleanLabels(tags))
	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	_, err = c.db.ExecContext(ctx, `
INSERT INTO remote_upload_jobs (
  id, source_url, source_label, requested_title, tags, state,
  created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id,
		strings.TrimSpace(sourceURL),
		strings.TrimSpace(sourceLabel),
		strings.TrimSpace(requestedTitle),
		string(tagsJSON),
		RemoteUploadQueued,
		now,
		now,
	)
	if err != nil {
		return nil, err
	}
	return c.GetRemoteUploadJob(ctx, id)
}

func (c *Catalog) GetRemoteUploadJob(ctx context.Context, id string) (*RemoteUploadJob, error) {
	row := c.db.QueryRowContext(ctx, `
SELECT `+remoteUploadJobCols+`
  FROM remote_upload_jobs
 WHERE id = ?`,
		strings.TrimSpace(id))
	return scanRemoteUploadJob(row)
}

func (c *Catalog) ListRemoteUploadJobs(ctx context.Context, limit int) ([]*RemoteUploadJob, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT `+remoteUploadJobCols+`
  FROM remote_upload_jobs
 WHERE source_kind = 'http'
 ORDER BY sequence DESC
 LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*RemoteUploadJob, 0, limit)
	for rows.Next() {
		job, err := scanRemoteUploadJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// ClaimNextRemoteUploadJob atomically takes the oldest queued job. Only one
// worker is started in production, but the conditional UPDATE also keeps this
// safe if startup code is accidentally invoked twice.
func (c *Catalog) ClaimNextRemoteUploadJob(ctx context.Context) (*RemoteUploadJob, error) {
	return c.ClaimNextImportJob(ctx, true)
}

func (c *Catalog) ClaimNextImportJob(ctx context.Context, allowTelegram bool) (*RemoteUploadJob, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var id string
	err = tx.QueryRowContext(ctx, `
SELECT id
  FROM remote_upload_jobs
 WHERE state = ? AND cancel_requested = 0 AND next_attempt <= ?
 AND (source_kind = 'http' OR (source_kind = 'telegram' AND ?))
 ORDER BY sequence ASC
 LIMIT 1`, RemoteUploadQueued, time.Now().UnixMilli(), allowTelegram).Scan(&id)
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	res, err := tx.ExecContext(ctx, `
UPDATE remote_upload_jobs
   SET state = ?,
       bytes_downloaded = 0,
       total_bytes = 0,
       error_message = '',
       temp_file = '',
       final_file = '',
       completed_video_id = '',
       started_at = ?,
       updated_at = ?
 WHERE id = ? AND state = ? AND cancel_requested = 0`,
		RemoteUploadDownloading, now, now, id, RemoteUploadQueued)
	if err != nil {
		return nil, err
	}
	if affected, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if affected != 1 {
		return nil, ErrRemoteUploadInvalidTransition
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return c.GetRemoteUploadJob(ctx, id)
}

func (c *Catalog) SetRemoteUploadTempFile(ctx context.Context, id, tempFile string) error {
	return c.updateActiveRemoteUpload(ctx, id, `
temp_file = ?, updated_at = ?`, filepathBaseOnly(tempFile), time.Now().UnixMilli())
}

func (c *Catalog) UpdateRemoteUploadProgress(ctx context.Context, id string, downloaded, total int64) error {
	if downloaded < 0 {
		downloaded = 0
	}
	if total < 0 {
		total = 0
	}
	return c.updateActiveRemoteUpload(ctx, id, `
bytes_downloaded = ?, total_bytes = ?, updated_at = ?`,
		downloaded, total, time.Now().UnixMilli())
}

func (c *Catalog) TransitionRemoteUploadJob(ctx context.Context, id, from, to string) error {
	now := time.Now().UnixMilli()
	res, err := c.db.ExecContext(ctx, `
UPDATE remote_upload_jobs
   SET state = ?, updated_at = ?
 WHERE id = ? AND state = ? AND cancel_requested = 0`,
		to, now, id, from)
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err != nil {
		return err
	} else if affected == 1 {
		return nil
	}
	return c.remoteUploadTransitionError(ctx, id)
}

func (c *Catalog) PrepareRemoteUploadSaving(
	ctx context.Context,
	id, tempFile, finalFile, videoID, resolvedTitle string,
) error {
	return c.updateActiveRemoteUpload(ctx, id, `
temp_file = ?, final_file = ?, completed_video_id = ?,
resolved_title = ?, updated_at = ?`,
		filepathBaseOnly(tempFile),
		filepathBaseOnly(finalFile),
		strings.TrimSpace(videoID),
		strings.TrimSpace(resolvedTitle),
		time.Now().UnixMilli(),
	)
}

// FinalizeRemoteUpload atomically publishes an acquired video, its tags, and
// the completed job. The caller has already made its source file durable.
func (c *Catalog) FinalizeRemoteUpload(
	ctx context.Context,
	jobID string,
	video *Video,
	manualTags []string,
	autoTags []TagAssignment,
) error {
	if video == nil {
		return errors.New("catalog: remote upload video is nil")
	}
	manualTags = uniqueStrings(cleanLabels(manualTags))

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var state, sourceKind string
	var cancelRequested int
	if err := tx.QueryRowContext(ctx, `
SELECT state, cancel_requested, source_kind
  FROM remote_upload_jobs
 WHERE id = ?`, jobID).Scan(&state, &cancelRequested, &sourceKind); err != nil {
		return err
	}
	if cancelRequested != 0 {
		return ErrRemoteUploadCanceled
	}
	if state != RemoteUploadSaving {
		return ErrRemoteUploadInvalidTransition
	}

	if video.DriveID == TelegramLocalDriveID {
		var owner, directory string
		if err := tx.QueryRowContext(ctx, `SELECT job_id,directory FROM telegram_local_files WHERE file_id=?`, video.FileID).Scan(&owner, &directory); err != nil {
			return err
		}
		if owner != jobID || sourceKind != "telegram" {
			return errors.New("catalog: TG file belongs to another import")
		}
		if directory == "" {
			directory = TelegramLibraryRootID
		}
		video.ParentID, video.DirName = directory, directory
	}
	type assignment struct {
		tagID    int64
		label    string
		source   string
		evidence string
	}
	assignments := make([]assignment, 0, len(manualTags)+len(autoTags))
	seen := make(map[string]struct{})
	if sourceKind == "telegram" {
		tag, err := ensureTelegramTagTx(ctx, tx)
		if err != nil {
			return err
		}
		seen[strings.ToLower(tag.Label)] = struct{}{}
		assignments = append(assignments, assignment{
			tagID: tag.ID, label: tag.Label, source: "telegram", evidence: "Telegram 视频导入",
		})
	}
	if len(manualTags) > 0 {
		for _, label := range manualTags {
			tag, err := getTagByLabelTxRaw(ctx, tx, label)
			if err != nil {
				return err
			}
			key := strings.ToLower(tag.Label)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			assignments = append(assignments, assignment{
				tagID:  tag.ID,
				label:  tag.Label,
				source: "manual",
			})
		}
	} else {
		for _, item := range autoTags {
			label := cleanTagLabel(item.Label)
			if label == "" {
				continue
			}
			tag, err := getTagByLabelTxRaw(ctx, tx, label)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			key := strings.ToLower(tag.Label)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			assignments = append(assignments, assignment{
				tagID:    tag.ID,
				label:    tag.Label,
				source:   normalizeVideoTagSource(item.Source),
				evidence: item.Evidence,
			})
		}
	}

	labels := make([]string, 0, len(assignments))
	for _, item := range assignments {
		labels = append(labels, item.label)
	}
	tagsJSON, err := json.Marshal(labels)
	if err != nil {
		return err
	}

	now := time.Now()
	if video.CreatedAt.IsZero() {
		video.CreatedAt = now
	}
	if video.PublishedAt.IsZero() {
		video.PublishedAt = now
	}
	video.UpdatedAt = now
	manual := 0
	if len(manualTags) > 0 {
		manual = 1
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO videos (
  id, drive_id, file_id, file_name, parent_id, dir_name, fingerprint_status,
  title, author, tags, size_bytes, ext,
  thumbnail_status, preview_status, tags_manual,
  published_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?, ?, 'pending', 'pending', ?, ?, ?, ?)`,
		video.ID,
		video.DriveID,
		video.FileID,
		video.FileName,
		video.ParentID,
		video.DirName,
		video.Title,
		video.Author,
		string(tagsJSON),
		video.Size,
		video.Ext,
		manual,
		video.PublishedAt.UnixMilli(),
		video.CreatedAt.UnixMilli(),
		video.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		return err
	}
	for _, item := range assignments {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO video_tags (video_id, tag_id, source, evidence, created_at)
VALUES (?, ?, ?, ?, ?)`,
			video.ID, item.tagID, item.source, item.evidence, now.UnixMilli()); err != nil {
			return err
		}
	}

	res, err := tx.ExecContext(ctx, `
UPDATE remote_upload_jobs
   SET state = ?,
       source_url = '',
       resolved_title = ?,
       bytes_downloaded = ?,
       total_bytes = CASE WHEN total_bytes > 0 THEN total_bytes ELSE ? END,
       cancel_requested = 0,
       error_message = '',
       temp_file = '',
       final_file = '',
       completed_video_id = ?,
       updated_at = ?,
       finished_at = ?
 WHERE id = ? AND state = ? AND cancel_requested = 0`,
		RemoteUploadCompleted,
		video.Title,
		video.Size,
		video.Size,
		video.ID,
		now.UnixMilli(),
		now.UnixMilli(),
		jobID,
		RemoteUploadSaving,
	)
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err != nil {
		return err
	} else if affected != 1 {
		return ErrRemoteUploadInvalidTransition
	}
	if _, err := tx.ExecContext(ctx, `UPDATE telegram_files SET video_id = ? WHERE job_id = ?`, video.ID, jobID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	video.Tags = labels
	return nil
}

func (c *Catalog) FailRemoteUploadJob(ctx context.Context, id, message string) error {
	return c.finishRemoteUploadJob(ctx, id, RemoteUploadFailed, message)
}

func (c *Catalog) MarkRemoteUploadCanceled(ctx context.Context, id string) error {
	return c.finishRemoteUploadJob(ctx, id, RemoteUploadCanceled, "")
}

func (c *Catalog) finishRemoteUploadJob(ctx context.Context, id, state, message string) error {
	now := time.Now().UnixMilli()
	res, err := c.db.ExecContext(ctx, `
UPDATE remote_upload_jobs
   SET state = ?,
       source_url = '',
       cancel_requested = 0,
       error_message = ?,
       temp_file = '',
       final_file = '',
       completed_video_id = '',
       updated_at = ?,
       finished_at = ?
 WHERE id = ? AND state NOT IN (?, ?, ?)`,
		state,
		strings.TrimSpace(message),
		now,
		now,
		id,
		RemoteUploadCompleted,
		RemoteUploadFailed,
		RemoteUploadCanceled,
	)
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err != nil {
		return err
	} else if affected == 1 {
		return nil
	}
	return ErrRemoteUploadTerminal
}

// CancelRemoteUploadJob immediately cancels queued work. Running work records
// cancel_requested first; the manager then aborts the request, cleans files,
// and turns it into the terminal canceled state.
func (c *Catalog) CancelRemoteUploadJob(ctx context.Context, id string) (*RemoteUploadJob, error) {
	now := time.Now().UnixMilli()
	res, err := c.db.ExecContext(ctx, `
UPDATE remote_upload_jobs
   SET state = CASE WHEN state = ? THEN ? ELSE state END,
       source_url = CASE WHEN state = ? THEN '' ELSE source_url END,
       cancel_requested = CASE WHEN state = ? THEN 0 ELSE 1 END,
       error_message = CASE WHEN state = ? THEN '' ELSE error_message END,
       temp_file = CASE WHEN state = ? THEN '' ELSE temp_file END,
       final_file = CASE WHEN state = ? THEN '' ELSE final_file END,
       completed_video_id = CASE WHEN state = ? THEN '' ELSE completed_video_id END,
       updated_at = ?,
       finished_at = CASE WHEN state = ? THEN ? ELSE finished_at END
 WHERE id = ? AND state NOT IN (?, ?, ?)`,
		RemoteUploadQueued, RemoteUploadCanceled,
		RemoteUploadQueued,
		RemoteUploadQueued,
		RemoteUploadQueued,
		RemoteUploadQueued,
		RemoteUploadQueued,
		RemoteUploadQueued,
		now,
		RemoteUploadQueued, now,
		strings.TrimSpace(id),
		RemoteUploadCompleted, RemoteUploadFailed, RemoteUploadCanceled,
	)
	if err != nil {
		return nil, err
	}
	if affected, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if affected != 1 {
		job, getErr := c.GetRemoteUploadJob(ctx, id)
		if getErr != nil {
			return nil, getErr
		}
		if job.Terminal() {
			return nil, ErrRemoteUploadTerminal
		}
		return nil, ErrRemoteUploadInvalidTransition
	}
	return c.GetRemoteUploadJob(ctx, id)
}

// ListInterruptedRemoteUploadArtifacts is intentionally read-only. Startup
// removes these files before RecoverRemoteUploadJobs clears their names, so a
// permission failure never loses the only durable cleanup reference.
func (c *Catalog) ListInterruptedRemoteUploadArtifacts(ctx context.Context) ([]RemoteUploadCleanupRef, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT id, temp_file, final_file
  FROM remote_upload_jobs
 WHERE state IN (?, ?, ?)`,
		RemoteUploadDownloading, RemoteUploadValidating, RemoteUploadSaving)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []RemoteUploadCleanupRef
	for rows.Next() {
		var ref RemoteUploadCleanupRef
		if err := rows.Scan(&ref.JobID, &ref.TempFile, &ref.FinalFile); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return refs, nil
}

// RecoverRemoteUploadJobs runs after interrupted artifacts were cleaned.
// Active jobs restart from byte zero; jobs whose cancellation was already
// requested become terminal canceled records.
func (c *Catalog) RecoverRemoteUploadJobs(ctx context.Context) ([]RemoteUploadCleanupRef, error) {
	refs, err := c.ListInterruptedRemoteUploadArtifacts(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `
UPDATE remote_upload_jobs
   SET state = ?,
       source_url = '',
       cancel_requested = 0,
       error_message = '',
       temp_file = '',
       final_file = '',
       completed_video_id = '',
       updated_at = ?,
       finished_at = ?
 WHERE state IN (?, ?, ?) AND cancel_requested = 1`,
		RemoteUploadCanceled,
		now,
		now,
		RemoteUploadDownloading,
		RemoteUploadValidating,
		RemoteUploadSaving,
	); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE remote_upload_jobs
   SET state = ?,
       bytes_downloaded = 0,
       total_bytes = 0,
       cancel_requested = 0,
       error_message = '',
       temp_file = '',
       final_file = '',
       completed_video_id = '',
       started_at = 0,
       updated_at = ?,
       finished_at = 0
 WHERE state IN (?, ?, ?)`,
		RemoteUploadQueued,
		now,
		RemoteUploadDownloading,
		RemoteUploadValidating,
		RemoteUploadSaving,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return refs, nil
}

func (c *Catalog) RequeueRemoteUploadOnShutdown(ctx context.Context, id string) error {
	now := time.Now().UnixMilli()
	_, err := c.db.ExecContext(ctx, `
UPDATE remote_upload_jobs
   SET state = CASE WHEN cancel_requested = 1 THEN ? ELSE ? END,
       source_url = CASE WHEN cancel_requested = 1 THEN '' ELSE source_url END,
       bytes_downloaded = 0,
       total_bytes = 0,
       cancel_requested = 0,
       error_message = '',
       temp_file = '',
       final_file = '',
       completed_video_id = '',
       started_at = 0,
       updated_at = ?,
       finished_at = CASE WHEN cancel_requested = 1 THEN ? ELSE 0 END
 WHERE id = ? AND state IN (?, ?, ?)`,
		RemoteUploadCanceled,
		RemoteUploadQueued,
		now,
		now,
		id,
		RemoteUploadDownloading,
		RemoteUploadValidating,
		RemoteUploadSaving,
	)
	return err
}

func (c *Catalog) DeleteExpiredRemoteUploadJobs(ctx context.Context, before time.Time) (int64, error) {
	if err := c.CompactTelegramHistory(ctx, time.Now().Add(-30*24*time.Hour)); err != nil {
		return 0, err
	}
	res, err := c.db.ExecContext(ctx, `
DELETE FROM remote_upload_jobs
 WHERE state IN (?, ?, ?)
   AND finished_at > 0
   AND finished_at < ? AND source_kind = 'http'`,
		RemoteUploadCompleted,
		RemoteUploadFailed,
		RemoteUploadCanceled,
		before.UnixMilli(),
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (c *Catalog) updateActiveRemoteUpload(ctx context.Context, id, setSQL string, args ...any) error {
	args = append(args,
		id,
		RemoteUploadDownloading,
		RemoteUploadValidating,
		RemoteUploadSaving,
	)
	res, err := c.db.ExecContext(ctx, `
UPDATE remote_upload_jobs
   SET `+setSQL+`
 WHERE id = ?
   AND state IN (?, ?, ?)
   AND cancel_requested = 0`, args...)
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err != nil {
		return err
	} else if affected == 1 {
		return nil
	}
	return c.remoteUploadTransitionError(ctx, id)
}

func (c *Catalog) remoteUploadTransitionError(ctx context.Context, id string) error {
	job, err := c.GetRemoteUploadJob(ctx, id)
	if err != nil {
		return err
	}
	if job.CancelRequested || job.State == RemoteUploadCanceled {
		return ErrRemoteUploadCanceled
	}
	if job.Terminal() {
		return ErrRemoteUploadTerminal
	}
	return ErrRemoteUploadInvalidTransition
}

func filepathBaseOnly(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.ReplaceAll(value, `\`, "/")
	parts := strings.Split(value, "/")
	return parts[len(parts)-1]
}

const remoteUploadJobCols = `
id, source_url, source_label, requested_title, resolved_title, tags, state,
bytes_downloaded, total_bytes, cancel_requested, error_message,
temp_file, final_file, completed_video_id,
created_at, started_at, updated_at, finished_at, source_kind, source_payload, stage, retry_count, next_attempt, sequence`

func scanRemoteUploadJob(row rowScanner) (*RemoteUploadJob, error) {
	var job RemoteUploadJob
	var tagsJSON string
	var cancelRequested int
	var createdAt, startedAt, updatedAt, finishedAt int64
	if err := row.Scan(
		&job.ID,
		&job.SourceURL,
		&job.SourceLabel,
		&job.RequestedTitle,
		&job.ResolvedTitle,
		&tagsJSON,
		&job.State,
		&job.BytesDownloaded,
		&job.TotalBytes,
		&cancelRequested,
		&job.ErrorMessage,
		&job.TempFile,
		&job.FinalFile,
		&job.CompletedVideoID,
		&createdAt,
		&startedAt,
		&updatedAt,
		&finishedAt,
		&job.SourceKind, &job.SourcePayload, &job.Stage, &job.RetryCount, &job.NextAttempt, &job.Sequence,
	); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(tagsJSON), &job.Tags); err != nil {
		return nil, fmt.Errorf("catalog: decode remote upload tags: %w", err)
	}
	if job.Tags == nil {
		job.Tags = []string{}
	}
	job.CancelRequested = cancelRequested != 0
	job.CreatedAt = unixMilliTime(createdAt)
	job.StartedAt = unixMilliTime(startedAt)
	job.UpdatedAt = unixMilliTime(updatedAt)
	job.FinishedAt = unixMilliTime(finishedAt)
	return &job, nil
}

func unixMilliTime(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(value)
}
