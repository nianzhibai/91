package catalog

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/videoid"
)

const (
	MergedSourceRecorded = "recorded"
	MergedSourceGone     = "gone"
	MergedSourceReplaced = "replaced"
)

var (
	ErrMergedRevisionChanged  = errors.New("合并记录已变化，请重新查询")
	ErrMergedSourceShared     = errors.New("源文件仍被视频引用")
	ErrMergedSourceReplaced   = errors.New("判重时的源文件已被替换")
	ErrMergedSourceUnverified = errors.New("无法核对判重时的源文件")
)

type MergedVideo struct {
	VideoID          string `json:"videoId"`
	CanonicalVideoID string `json:"canonicalVideoId"`
	DriveID          string `json:"driveId"`
	FileID           string `json:"fileId"`
	ParentID         string `json:"parentId"`
	FileName         string `json:"fileName"`
	Size             int64  `json:"sizeBytes"`
	ContentHash      string `json:"contentHash"`
	SampledSHA256    string `json:"sampledSha256"`
	SourceModTime    string `json:"sourceModTime"`
	SourceState      string `json:"sourceState"`
	MergeReason      string `json:"mergeReason"`
	MergedAt         int64  `json:"mergedAt"`
	SharedSource     bool   `json:"sharedSource"`
	SourceRevision   string `json:"sourceRevision"`
}

func (m *MergedVideo) source() *Video {
	return &Video{ID: m.VideoID, DriveID: m.DriveID, FileID: m.FileID, ParentID: m.ParentID, FileName: m.FileName,
		Size: m.Size, ContentHash: m.ContentHash, SampledSHA256: m.SampledSHA256, SourceModTime: m.SourceModTime}
}

const mergedVideoSelect = `SELECT video_id,canonical_video_id,drive_id,file_id,parent_id,file_name,size_bytes,
	content_hash,sampled_sha256,source_mod_time,source_state,merge_reason,merged_at,
	m.source_state='recorded' AND EXISTS(SELECT 1 FROM videos WHERE drive_id=m.drive_id AND file_id=m.file_id AND file_id!='')
	FROM merged_videos m`

func scanMergedVideo(row rowScanner) (*MergedVideo, error) {
	var m MergedVideo
	err := row.Scan(&m.VideoID, &m.CanonicalVideoID, &m.DriveID, &m.FileID, &m.ParentID, &m.FileName, &m.Size,
		&m.ContentHash, &m.SampledSHA256, &m.SourceModTime, &m.SourceState, &m.MergeReason, &m.MergedAt, &m.SharedSource)
	if err != nil {
		return nil, err
	}
	// Bind the token to the source generation, not its removal status. A retry
	// with the same token can acknowledge an already completed deletion.
	data, err := json.Marshal([]any{m.VideoID, m.CanonicalVideoID, m.DriveID, m.FileID, m.ParentID, m.FileName, m.Size, m.ContentHash, m.SampledSHA256, m.SourceModTime, m.MergedAt})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	m.SourceRevision = hex.EncodeToString(sum[:])
	return &m, nil
}

func (c *Catalog) GetMergedVideo(ctx context.Context, id string) (*MergedVideo, error) {
	return scanMergedVideo(c.db.QueryRowContext(ctx, mergedVideoSelect+" WHERE video_id=?", id))
}

type MergedVideoFilter struct {
	CanonicalVideoID, DriveID, SourceState string
	Page, Size                             int
}
type MergedVideoList struct {
	Items []*MergedVideo `json:"items"`
	Total int            `json:"total"`
	Page  int            `json:"page"`
	Size  int            `json:"size"`
}

func (c *Catalog) ListMergedVideos(ctx context.Context, f MergedVideoFilter) (*MergedVideoList, error) {
	var clauses []string
	var args []any
	for _, filter := range []struct{ column, value string }{{"canonical_video_id", f.CanonicalVideoID}, {"drive_id", f.DriveID}, {"source_state", f.SourceState}} {
		if filter.value != "" {
			clauses = append(clauses, filter.column+"=?")
			args = append(args, filter.value)
		}
	}
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}
	if f.Page < 1 {
		f.Page = 1
	}
	if f.Size < 1 || f.Size > 100 {
		f.Size = 100
	}
	list := &MergedVideoList{Items: []*MergedVideo{}, Page: f.Page, Size: f.Size}
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM merged_videos"+where, args...).Scan(&list.Total); err != nil {
		return nil, err
	}
	if list.Total == 0 || f.Page-1 > (list.Total-1)/f.Size {
		return list, nil
	}
	rows, err := tx.QueryContext(ctx, mergedVideoSelect+where+" ORDER BY merged_at DESC,video_id LIMIT ? OFFSET ?", append(args, f.Size, (f.Page-1)*f.Size)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		m, err := scanMergedVideo(rows)
		if err != nil {
			return nil, err
		}
		list.Items = append(list.Items, m)
	}
	return list, rows.Err()
}

func (c *Catalog) RemoveMergedVideoSource(ctx context.Context, id, revision string, remove func(context.Context, *Video) error) (resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	m, err := scanMergedVideo(tx.QueryRowContext(ctx, mergedVideoSelect+" WHERE video_id=?", id))
	if err != nil {
		return err
	}
	if m.SourceRevision != revision {
		return ErrMergedRevisionChanged
	}
	switch m.SourceState {
	case MergedSourceGone:
		return nil
	case MergedSourceReplaced:
		return ErrMergedSourceReplaced
	}
	if m.SharedSource {
		return ErrMergedSourceShared
	}
	if remove == nil {
		return errors.New("source removal is unavailable")
	}
	if m.FileID == "" {
		return ErrMergedSourceUnverified
	}
	token := rand.Text()
	deadline, _ := ctx.Deadline()
	if err := reserveDeletionFile(ctx, tx, m.DriveID, m.FileID, token, deadline); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, c.releaseSourceReservations(ctx, token)) }()
	source := m.source()
	removeErr := remove(ctx, source)
	if removeErr != nil && !errors.Is(removeErr, ErrMergedSourceReplaced) {
		return removeErr
	}
	finish, cancelFinish := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancelFinish()
	tx, err = c.db.BeginTx(finish, nil)
	if err != nil {
		return errors.Join(removeErr, err)
	}
	defer tx.Rollback()
	var owns bool
	if err = tx.QueryRowContext(finish, "SELECT EXISTS(SELECT 1 FROM video_source_removals WHERE drive_id=? AND file_id=? AND token=?)", m.DriveID, m.FileID, token).Scan(&owns); err != nil {
		return err
	}
	if !owns {
		return ErrVideoSourceInUse
	}
	if removeErr != nil {
		_, err = tx.ExecContext(finish, "UPDATE merged_videos SET source_state='replaced' WHERE video_id=? AND source_state='recorded'", id)
	} else {
		err = purgeSourceTombstonesTx(finish, tx, source)
	}
	if err != nil {
		return errors.Join(removeErr, err)
	}
	return errors.Join(removeErr, tx.Commit())
}

// Compare only evidence available for both observations. An equal provider
// hash remains valid across timestamp-only changes.
const mergedSourceChanged = `(m.size_bytes != observed.size_bytes
	OR (m.content_hash != '' AND observed.content_hash != '' AND m.content_hash != observed.content_hash COLLATE NOCASE)
	OR (m.sampled_sha256 != '' AND observed.sampled_sha256 != '' AND m.sampled_sha256 != observed.sampled_sha256 COLLATE NOCASE)
	OR ((m.content_hash = '' OR observed.content_hash = '') AND m.source_mod_time != '' AND observed.source_mod_time != ''
	    AND m.source_mod_time != observed.source_mod_time))`

func markReplacedMergedSources(ctx context.Context, tx *sql.Tx, v *Video) error {
	_, err := tx.ExecContext(ctx, `UPDATE merged_videos AS m SET source_state='replaced'
		FROM (SELECT ? AS size_bytes, ? AS content_hash, ? AS sampled_sha256, ? AS source_mod_time) AS observed
		WHERE m.drive_id=? AND m.file_id=? AND m.source_state='recorded' AND `+mergedSourceChanged,
		v.Size, normalizeContentHash(v.ContentHash), normalizeContentHash(v.SampledSHA256), v.SourceModTime, v.DriveID, v.FileID)
	return err
}

// reconcileMergedScan runs under the same writer reservation as admission.
func reconcileMergedScan(ctx context.Context, tx *sql.Tx, v *Video) (bool, error) {
	if err := markReplacedMergedSources(ctx, tx, v); err != nil {
		return false, err
	}
	var skip bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM merged_videos m
		WHERE m.drive_id=? AND m.file_id=? AND m.source_state='recorded'
		AND ((m.content_hash!='' AND m.content_hash=? COLLATE NOCASE)
		    OR (m.sampled_sha256!='' AND m.sampled_sha256=? COLLATE NOCASE)
		    OR (m.source_mod_time!='' AND m.source_mod_time=?)
		    OR (?='' AND m.file_name=? AND m.file_name!=''))
		AND (EXISTS(SELECT 1 FROM videos WHERE id=m.canonical_video_id AND hidden=0 AND file_id!='')
		    OR EXISTS(SELECT 1 FROM deleted_videos WHERE id=m.canonical_video_id AND restore_requested=0)))`,
		v.DriveID, v.FileID, normalizeContentHash(v.ContentHash), normalizeContentHash(v.SampledSHA256), v.SourceModTime, v.SourceModTime, v.FileName).Scan(&skip)
	if err != nil {
		return false, err
	}
	if skip {
		return true, nil
	}
	// A provider can reuse a file ID. Preserve the old public link, including
	// when a partial restore omitted its target. All later scans bind by file.
	base := v.ID
	for generation := 1; ; generation++ {
		var reserved bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM merged_videos WHERE video_id=? UNION ALL SELECT 1 FROM videos WHERE id=? UNION ALL SELECT 1 FROM deleted_videos WHERE id=?)`, v.ID, v.ID, v.ID).Scan(&reserved); err != nil {
			return false, err
		}
		if !reserved {
			return false, nil
		}
		v.ID = videoid.Generation(base, generation)
	}
}
