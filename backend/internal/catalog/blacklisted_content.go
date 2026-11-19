package catalog

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// DeletedVideoReasonBlacklistedContent marks an entry that was created
// automatically because a newly fingerprinted video matched blacklisted content.
const DeletedVideoReasonBlacklistedContent = "blacklisted_content"

// A blacklist entry blocks its own source and, while blocks_content is set,
// every copy of its content. Restoring a video lifts the content block of
// matching entries; their own sources stay blacklisted.
func (c *Catalog) ensureBlacklistedContent(ctx context.Context) error {
	added, err := c.addColumnIfMissingReportNew(ctx, "deleted_videos", "sampled_sha256", "TEXT NOT NULL DEFAULT ''")
	if err != nil {
		return err
	}
	if err := c.addColumnIfMissing(ctx, "deleted_videos", "blocks_content", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if added {
		// Entries written before fingerprints were recorded keep them in their
		// restore snapshot or, for retired aliases, in the merge record.
		if _, err := c.db.ExecContext(ctx, `
UPDATE deleted_videos
   SET sampled_sha256 = lower(trim(json_extract(restore_payload, '$.video.sampledSha256')))
 WHERE json_valid(restore_payload)
   AND COALESCE(json_extract(restore_payload, '$.video.sampledSha256'), '') != '';
UPDATE deleted_videos
   SET sampled_sha256 = (SELECT lower(trim(m.sampled_sha256)) FROM merged_videos m WHERE m.video_id = deleted_videos.id)
 WHERE sampled_sha256 = ''
   AND EXISTS (SELECT 1 FROM merged_videos m WHERE m.video_id = deleted_videos.id AND m.sampled_sha256 != '');
UPDATE deleted_videos SET content_hash = lower(trim(content_hash)) WHERE content_hash != lower(trim(content_hash));`); err != nil {
			return err
		}
	}
	_, err = c.db.ExecContext(ctx, `
DROP INDEX IF EXISTS idx_deleted_videos_drive_hash;
CREATE INDEX IF NOT EXISTS idx_deleted_videos_content_hash ON deleted_videos(content_hash);
CREATE INDEX IF NOT EXISTS idx_deleted_videos_sampled ON deleted_videos(sampled_sha256, size_bytes);`)
	return err
}

// blacklistedContentWhere matches active entries holding the content of v: the
// same provider hash or size-qualified sampled fingerprint on any drive, or the
// same file name and size on v's drive when no exact signal contradicts it.
// Callers that require proof pass v without a drive.
func blacklistedContentWhere(v *Video) (string, []any, bool) {
	hash := normalizeContentHash(v.ContentHash)
	sampled := normalizeContentHash(v.SampledSHA256)
	var parts []string
	var args []any
	if hash != "" {
		parts = append(parts, "content_hash = ?")
		args = append(args, hash)
	}
	if sampled != "" && v.Size > 0 {
		parts = append(parts, "(sampled_sha256 = ? AND size_bytes = ?)")
		args = append(args, sampled, v.Size)
	}
	if v.DriveID != "" && v.FileName != "" && v.Size > 0 {
		parts = append(parts, `(drive_id = ? AND file_name = ? AND size_bytes = ?
			AND (content_hash = '' OR ? = '' OR content_hash = ?)
			AND (sampled_sha256 = '' OR ? = '' OR sampled_sha256 = ?))`)
		args = append(args, v.DriveID, v.FileName, v.Size, hash, hash, sampled, sampled)
	}
	if len(parts) == 0 {
		return "", nil, false
	}
	return "blocks_content = 1 AND restore_requested = 0 AND (" + strings.Join(parts, " OR ") + ")", args, true
}

type blacklistQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// findBlacklistedContent returns the earliest active entry holding v's content.
func findBlacklistedContent(ctx context.Context, q blacklistQuerier, v *Video) (string, error) {
	where, args, ok := blacklistedContentWhere(v)
	if !ok {
		return "", nil
	}
	var id string
	err := q.QueryRowContext(ctx, `SELECT id FROM deleted_videos WHERE `+where+` ORDER BY deleted_at, id LIMIT 1`, args...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// releaseBlacklistedContentTx runs before the entry id is restored. Copies of
// the restored content are then judged against the restored video by the
// ordinary duplicate rules instead of being blocked.
func releaseBlacklistedContentTx(ctx context.Context, tx *sql.Tx, id string) error {
	v := &Video{}
	err := tx.QueryRowContext(ctx, `SELECT drive_id, file_name, size_bytes, content_hash, sampled_sha256
		FROM deleted_videos WHERE id = ?`, id).Scan(&v.DriveID, &v.FileName, &v.Size, &v.ContentHash, &v.SampledSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	where, args, ok := blacklistedContentWhere(v)
	if !ok {
		return nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE deleted_videos SET blocks_content = 0 WHERE id != ? AND `+where, append([]any{id}, args...)...)
	return err
}

// blacklistFingerprintedContentTx runs when a row's sampled fingerprint becomes
// ready, which is the first time a copy from a drive with another hash
// algorithm can be recognized. A visible copy of the same content means the
// content is not blacklisted; ordinary deduplication handles the row then.
func blacklistFingerprintedContentTx(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	v, err := scanVideo(tx.QueryRowContext(ctx, `SELECT `+allVideoCols+` FROM videos WHERE id = ? AND `+duplicateLookupScope, id))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	exact, exactArgs, ok := equivalentVideoLookupWhere(v)
	if !ok {
		return false, nil
	}
	var represented bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM videos WHERE id != ? AND `+duplicateLookupScope+` AND (`+exact+`))`,
		append([]any{id}, exactArgs...)...).Scan(&represented); err != nil || represented {
		return false, err
	}
	match, err := findBlacklistedContent(ctx, tx, &Video{ContentHash: v.ContentHash, SampledSHA256: v.SampledSHA256, Size: v.Size})
	if err != nil || match == "" {
		return false, err
	}
	err = deleteVideoWithTombstoneTx(ctx, tx, v, DeleteVideoTombstoneOptions{
		AssetCleanupAfter: time.Now(),
		Reason:            DeletedVideoReasonBlacklistedContent,
	})
	return err == nil, err
}

// SkipBlacklistedCrawlerSource remembers a downloaded crawler source whose
// content is blacklisted, so it is neither imported nor downloaded again.
func (c *Catalog) SkipBlacklistedCrawlerSource(ctx context.Context, v *Video, seen CrawlerSourceSeen) (bool, error) {
	if v == nil || seen.Kind == "" || seen.DriveID == "" || seen.SourceID == "" {
		return false, errors.New("catalog: blacklisted crawler source requires video and seen identity")
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	match, err := findBlacklistedContent(ctx, tx, &Video{ContentHash: v.ContentHash, SampledSHA256: v.SampledSHA256, Size: v.Size})
	if err != nil || match == "" {
		return false, err
	}
	if err := markCrawlerSourceSeen(ctx, tx, seen.Kind, seen.DriveID, seen.SourceID, CrawlerSourceBlacklisted, match, v.SampledSHA256, v.Size); err != nil {
		return false, err
	}
	if err := bindCrawlerDiscovery(ctx, tx, seen.DriveID, seen.DiscoveryKey, seen.SourceID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
