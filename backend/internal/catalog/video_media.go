package catalog

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/driveevents"
	"github.com/nianzhibai/91/backend/internal/mediasig"
)

// currentSignatureWhere matches rows whose stored signature uses the current
// encoding. Signatures of another format are treated as missing.
var currentSignatureWhere = fmt.Sprintf(`hex(substr(content_signature, 1, 1)) = '%02X'`, mediasig.SignatureFormat)

// MediaWants is the work a media job does for a video. Both parts come from
// one read of the teaser segments; the job also produces a missing
// thumbnail, but a missing thumbnail alone is left to the thumbnail worker.
type MediaWants struct {
	// Teaser renews the preview video.
	Teaser bool
	// Signature samples the perceptual signature.
	Signature bool
}

// Any reports whether a media job has work to do.
func (w MediaWants) Any() bool { return w.Teaser || w.Signature }

// MediaWants decides the media job for v. A pending job samples the signature
// of a video long enough to compare, or of unknown duration until the job has
// probed it, and renews the teaser while previews are enabled. A pending
// teaser alone is generated while previews are enabled.
func (v *Video) MediaWants(previewsEnabled bool) MediaWants {
	if v == nil || v.Hidden {
		return MediaWants{}
	}
	pending := v.MediaPending()
	minimum := int(mediasig.MinDuration / time.Second)
	return MediaWants{
		Teaser:    previewsEnabled && (pending || v.PreviewStatus == "pending"),
		Signature: pending && (v.DurationSeconds == 0 || v.DurationSeconds >= minimum),
	}
}

// MediaPending reports whether v's media job is due. Rows default to pending;
// an unsaved video has not run its job either.
func (v *Video) MediaPending() bool {
	return v.MediaStatus == "pending" || v.MediaStatus == ""
}

// CompleteMediaForSource records a finished media job for source with the
// signature it sampled, or none when the video is too short to compare. Like
// other generated assets, the write is rejected when the row no longer refers
// to the bytes that were read.
func (c *Catalog) CompleteMediaForSource(ctx context.Context, source *Video, signature *mediasig.Signature) error {
	if source == nil {
		return fmt.Errorf("catalog: complete media: missing source")
	}
	var data []byte
	if signature != nil {
		var err error
		if data, err = signature.MarshalBinary(); err != nil {
			return err
		}
	}
	return c.updateVideoRow(ctx, source.ID, source, "content_signature = ?, media_status = 'ready', media_retries = 0", data)
}

// CompletePartialMediaForSource stores a signature that misses planned
// windows, usually because a segment could not be read and fell back to
// another start. The signature is usable at once; the job is left stale so
// maintenance batches read the segments again until a read completes it.
// media_retries counts the rereads.
func (c *Catalog) CompletePartialMediaForSource(ctx context.Context, source *Video, signature *mediasig.Signature) error {
	if source == nil || signature == nil {
		return fmt.Errorf("catalog: complete partial media: missing source or signature")
	}
	data, err := signature.MarshalBinary()
	if err != nil {
		return err
	}
	return c.updateVideoRow(ctx, source.ID, source, `content_signature = ?, media_status = 'stale',
		media_retries = COALESCE(media_retries, 0) + 1`, data)
}

// MarkMediaFailedForSource records a failed media job, so the video is not
// queued again until a retry resets it.
func (c *Catalog) MarkMediaFailedForSource(ctx context.Context, source *Video) error {
	if source == nil {
		return fmt.Errorf("catalog: mark media failed: missing source")
	}
	return c.updateVideoRow(ctx, source.ID, source, "media_status = 'failed'")
}

// RequestMediaRenewal queues the media job of the given videos again. Their
// current teasers stay visible until the job replaces them.
func (c *Catalog) RequestMediaRenewal(ctx context.Context, ids []string) (resultErr error) {
	defer c.notifyDriveWrite(&resultErr, "", driveevents.GenerationChanged)
	const chunkSize = 500
	for start := 0; start < len(ids); start += chunkSize {
		chunk := ids[start:min(start+chunkSize, len(ids))]
		args := []any{time.Now().UnixMilli()}
		for _, id := range chunk {
			args = append(args, id)
		}
		if _, err := c.db.ExecContext(ctx, `UPDATE videos SET media_status = 'pending', media_retries = 0, updated_at = ?
			 WHERE id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")+`)`, args...); err != nil {
			return err
		}
	}
	return nil
}

// markOutdatedSignaturesStale queues finished jobs for renewal in maintenance
// batches when their signature no longer fits the current scheme: it uses an
// older format, or the video was too short to sign when the job ran and is
// long enough now.
func (c *Catalog) markOutdatedSignaturesStale(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx, `UPDATE videos SET media_status = 'stale'
		 WHERE media_status = 'ready'
		   AND (content_signature IS NOT NULL AND NOT `+currentSignatureWhere+`
		        OR content_signature IS NULL AND COALESCE(duration_seconds, 0) >= ?)`,
		int(mediasig.MinDuration/time.Second))
	return err
}

// PromoteStaleMedia turns up to limit stale media jobs into pending ones and
// returns those videos, so renewals proceed in batches that do not hold up
// maintenance. Rereads of partial signatures go first, then the oldest
// videos.
func (c *Catalog) PromoteStaleMedia(ctx context.Context, limit int) (resultValue []*Video, resultErr error) {
	defer c.notifyDriveWrite(&resultErr, "", driveevents.GenerationChanged)
	if limit <= 0 {
		return nil, nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+allVideoCols+` FROM videos
		 WHERE media_status = 'stale' AND COALESCE(hidden, 0) = 0 AND `+uniqueVideoWhereSQL+`
		 ORDER BY COALESCE(media_retries, 0) > 0 DESC, created_at ASC, id ASC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var promoted []*Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		promoted = append(promoted, v)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	for _, v := range promoted {
		if _, err := tx.ExecContext(ctx, `UPDATE videos SET media_status = 'pending', updated_at = ? WHERE id = ? AND media_status = 'stale'`, now, v.ID); err != nil {
			return nil, err
		}
		v.MediaStatus = "pending"
	}
	return promoted, tx.Commit()
}

// ListVideosNeedingMedia returns the drive's visible, unique videos for which
// MediaWants reports work.
func (c *Catalog) ListVideosNeedingMedia(ctx context.Context, driveID string, previewsEnabled bool) ([]*Video, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+allVideoCols+` FROM videos
		 WHERE drive_id = ?
		   AND (preview_status = 'pending' OR COALESCE(media_status, 'pending') = 'pending')
		   AND COALESCE(hidden, 0) = 0
		   AND `+uniqueVideoWhereSQL+`
		 ORDER BY created_at ASC`, driveID)
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
		if v.MediaWants(previewsEnabled).Any() {
			out = append(out, v)
		}
	}
	return out, rows.Err()
}

// LoadVideoSignatures returns the current signatures of the given videos.
// Videos without one are absent from the result.
func (c *Catalog) LoadVideoSignatures(ctx context.Context, ids []string) (map[string]*mediasig.Signature, error) {
	out := make(map[string]*mediasig.Signature, len(ids))
	const chunkSize = 500
	for start := 0; start < len(ids); start += chunkSize {
		chunk := ids[start:min(start+chunkSize, len(ids))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := c.db.QueryContext(ctx,
			`SELECT id, content_signature FROM videos
			  WHERE id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")+`)
			    AND `+currentSignatureWhere, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var data []byte
			if err := rows.Scan(&id, &data); err != nil {
				rows.Close()
				return nil, err
			}
			signature := &mediasig.Signature{}
			if err := signature.UnmarshalBinary(data); err != nil {
				rows.Close()
				return nil, fmt.Errorf("catalog: decode signature of %s: %w", id, err)
			}
			out[id] = signature
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
