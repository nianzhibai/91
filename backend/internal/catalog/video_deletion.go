package catalog

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/nianzhibai/91/backend/internal/dedupe"
	"github.com/nianzhibai/91/backend/internal/driveevents"
)

// Physical deletion of a multi-file group requires the preview revision and
// the user's choice. Record-only deletion always covers the group.
type VideoDeletionOptions struct {
	DeleteSource      bool   `json:"deleteSource,omitempty"`
	IncludeDuplicates *bool  `json:"includeDuplicates,omitempty"`
	GroupRevision     string `json:"groupRevision,omitempty"`
}

type VideoDeletionSource struct {
	DriveID  string `json:"driveId"`
	FileID   string `json:"fileId"`
	FileName string `json:"fileName"`
}

type VideoDeletionPreview struct {
	Sources              []VideoDeletionSource `json:"sources"`
	GroupRevision        string                `json:"groupRevision"`
	RequiresConfirmation bool                  `json:"requiresConfirmation"`
}

var (
	ErrDeletionScopeIncomplete = errors.New("请先查询删除范围，再提交 groupRevision 和 includeDuplicates")
	ErrDeletionScopeChanged    = errors.New("删除范围已变化，请重新查询后确认")
)

type deletionFile struct {
	video   *Video
	live    []*Video
	retired []*Video
	skipped []*Video
}

type deletionGroup struct {
	files    []*deletionFile
	bySource map[[2]string]*deletionFile
}

func (g *deletionGroup) file(v *Video) *deletionFile {
	key := [2]string{v.DriveID, v.FileID}
	if f := g.bySource[key]; f != nil {
		return f
	}
	f := &deletionFile{video: v}
	g.bySource[key] = f
	g.files = append(g.files, f)
	return f
}

// Traverse current catalog relationships. Visit physical files breadth first so
// that removing them in reverse order leaves the remaining group reachable on retry.
// The group matches the catalog's visible cards, for record-only and physical
// deletion alike.
func loadDeletionGroup(ctx context.Context, tx *sql.Tx, id string) (*deletionGroup, error) {
	root, err := scanVideo(tx.QueryRowContext(ctx, "SELECT "+allVideoCols+" FROM videos WHERE id = ?", id))
	if err != nil {
		return nil, err
	}
	g := &deletionGroup{bySource: make(map[[2]string]*deletionFile)}
	g.file(root)
	visited := make(map[[2]string]bool)
	for i := 0; i < len(g.files); i++ {
		file := g.files[i]
		v := file.video
		rows, err := tx.QueryContext(ctx, "SELECT "+allVideoCols+` FROM videos
			WHERE id = ? OR (drive_id = ? AND file_id = ? AND file_id != '') ORDER BY id`, v.ID, v.DriveID, v.FileID)
		if err != nil {
			return nil, err
		}
		file.live, err = readDeletionVideos(rows)
		if err != nil {
			return nil, err
		}
		for _, owner := range file.live {
			keys, err := deletionRepresentativeKeys(ctx, tx, owner.ID)
			if err != nil {
				return nil, err
			}
			for _, key := range keys {
				if visited[key] {
					continue
				}
				visited[key] = true
				rows, err := tx.QueryContext(ctx, "SELECT "+allVideoCols+` FROM videos WHERE id IN (
					SELECT video_id FROM video_dedup_representatives WHERE basis = ? AND representative_id = ?) ORDER BY id`, key[0], key[1])
				if err != nil {
					return nil, err
				}
				peers, err := readDeletionVideos(rows)
				if err != nil {
					return nil, err
				}
				for _, peer := range peers {
					g.file(peer)
				}
			}
			held, err := sizeHeldDuplicates(ctx, tx, owner)
			if err != nil {
				return nil, err
			}
			for _, peer := range held {
				g.file(peer)
			}
		}
	}

	// Maintenance flattens merged aliases onto the final live ID. It keeps
	// their provider files. A newly admitted owner supersedes a historical source.
	liveFiles := slices.Clone(g.files)
	for _, file := range liveFiles {
		for _, owner := range file.live {
			if err := g.loadMergedSources(ctx, tx, owner); err != nil {
				return nil, err
			}
		}
	}
	return g, nil
}

// Use the same groups as the catalog's visible cards, including the
// filename/size fallback. Each indexed group is expanded just once.
func deletionRepresentativeKeys(ctx context.Context, tx *sql.Tx, id string) ([][2]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT basis, representative_id FROM video_dedup_representatives
		WHERE video_id=? ORDER BY basis`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys [][2]string
	for rows.Next() {
		var key [2]string
		if err := rows.Scan(&key[0], &key[1]); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// Rows waiting for a usable provider size are outside the visible groups but
// still own their files. Exact copies join the group so a later scan cannot
// bring back blacklisted content when the size recovers.
func sizeHeldDuplicates(ctx context.Context, tx *sql.Tx, owner *Video) ([]*Video, error) {
	where, args, ok := equivalentVideoLookupWhere(owner)
	if !ok {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+allVideoCols+` FROM videos
		WHERE hidden = ? AND COALESCE(file_id, '') != '' AND (`+where+`) ORDER BY id`, append([]any{hiddenForInvalidSize}, args...)...)
	if err != nil {
		return nil, err
	}
	return readDeletionVideos(rows)
}

func readDeletionVideos(rows *sql.Rows) ([]*Video, error) {
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

func (g *deletionGroup) loadMergedSources(ctx context.Context, tx *sql.Tx, owner *Video) error {
	rows, err := tx.QueryContext(ctx, `SELECT video_id, drive_id, file_id, file_name, parent_id, size_bytes, content_hash, sampled_sha256, source_mod_time,source_state,
		EXISTS (SELECT 1 FROM videos WHERE drive_id = m.drive_id AND file_id = m.file_id)
		FROM merged_videos m WHERE canonical_video_id = ?
		ORDER BY video_id`, owner.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		v := &Video{}
		var state string
		var owned bool
		if err := rows.Scan(&v.ID, &v.DriveID, &v.FileID, &v.FileName, &v.ParentID, &v.Size, &v.ContentHash, &v.SampledSHA256, &v.SourceModTime, &state, &owned); err != nil {
			return err
		}
		if v.FileID != "" && state == MergedSourceRecorded && (!owned || g.bySource[[2]string{v.DriveID, v.FileID}] != nil) {
			f := g.file(v)
			f.retired = append(f.retired, v)
		}
		// A scan skipped against this member can still have an independent
		// physical file after the member's own file was removed.
		if err := g.loadSkippedSources(ctx, tx, v); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	return g.loadSkippedSources(ctx, tx, owner)
}

func (g *deletionGroup) loadSkippedSources(ctx context.Context, tx *sql.Tx, owner *Video) error {
	// A skipped scan can leave a real file on another drive without a videos
	// row. Use its latest observation only while its live comparison still
	// matches. Crawler downloads rejected before upload have no stored file.
	observations, err := tx.QueryContext(ctx, `SELECT source_snapshot, reason FROM duplicate_records r
		WHERE canonical_video_id = ? AND origin = 'scan' AND outcome = 'skipped_import'
		AND file_id != '' AND record_key = (
			SELECT record_key FROM duplicate_records latest WHERE latest.drive_id = r.drive_id AND latest.file_id = r.file_id
			AND latest.origin = 'scan' ORDER BY last_seen_at DESC, record_key DESC LIMIT 1)
		AND NOT EXISTS (SELECT 1 FROM videos WHERE id = r.video_id OR (drive_id = r.drive_id AND file_id = r.file_id))
		AND NOT EXISTS (SELECT 1 FROM merged_videos WHERE video_id = r.video_id OR (drive_id = r.drive_id AND file_id = r.file_id AND source_state='recorded'))
		AND NOT EXISTS (SELECT 1 FROM deleted_videos WHERE id = r.video_id OR (drive_id = r.drive_id AND file_id = r.file_id))
		ORDER BY drive_id, file_id`, owner.ID)
	if err != nil {
		return err
	}
	defer observations.Close()
	for observations.Next() {
		var raw, reason string
		if err := observations.Scan(&raw, &reason); err != nil {
			return err
		}
		var s duplicateSnapshot
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			return err
		}
		v := &Video{ID: s.ID, DriveID: s.DriveID, FileID: s.FileID, ParentID: s.ParentID, FileName: s.FileName, Size: s.SizeBytes, ContentHash: s.ContentHash, SampledSHA256: s.SampledSHA256}
		weak := reason == dedupe.ReasonFileNameSize && weakDuplicate(v, owner)
		if exactDuplicateReason(v, owner) == "" && !weak {
			continue
		}
		f := g.file(v)
		f.skipped = append(f.skipped, v)
	}
	return observations.Err()
}

func (g *deletionGroup) blacklist(ctx context.Context, tx *sql.Tx, cleanupAfter time.Time) error {
	for _, f := range g.files {
		for _, v := range f.live {
			if err := deleteVideoWithTombstoneTx(ctx, tx, v, DeleteVideoTombstoneOptions{AssetCleanupAfter: cleanupAfter}); err != nil {
				return err
			}
		}
		for _, v := range slices.Concat(f.retired, f.skipped) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO deleted_videos(id, drive_id, file_id, file_name, parent_id, size_bytes, content_hash, sampled_sha256, deleted_at)
					VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET blocks_content = 1`, v.ID, v.DriveID, v.FileID, v.FileName, v.ParentID, v.Size,
				normalizeContentHash(v.ContentHash), normalizeContentHash(v.SampledSHA256), time.Now().UnixMilli()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Catalog) PreviewVideoDeletion(ctx context.Context, id string) (*VideoDeletionPreview, error) {
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	group, err := loadDeletionGroup(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	return group.preview(), nil
}

func (g *deletionGroup) preview() *VideoDeletionPreview {
	preview := &VideoDeletionPreview{RequiresConfirmation: len(g.files) > 1}
	h := sha256.New()
	enc := json.NewEncoder(h)
	for _, file := range g.files {
		v := file.video
		preview.Sources = append(preview.Sources, VideoDeletionSource{v.DriveID, v.FileID, v.FileName})
		for _, members := range [][]*Video{file.live, file.retired, file.skipped} {
			for _, member := range members {
				_ = enc.Encode([]any{member.ID, member.DriveID, member.FileID, member.Size, member.ContentHash, member.SampledSHA256, member.FileName, member.SourceModTime})
			}
		}
	}
	preview.GroupRevision = hex.EncodeToString(h.Sum(nil))
	return preview
}

// DeleteVideoGroup commits record-only removals together. For physical deletes,
// reserve the selected sources in the planning transaction, then release the
// SQLite writer before calling providers. Each successful file is committed
// separately; the requested file is last and remains a retry target on failure.
func (c *Catalog) DeleteVideoGroup(ctx context.Context, id string, options VideoDeletionOptions, cleanupAfter time.Time,
	remove func(context.Context, *Video, bool) error, cleanAssets func(context.Context, *Video) error) (resultErr error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if !options.DeleteSource {
		id, err = resolveVideoID(ctx, tx, id)
		if err != nil {
			return err
		}
	}
	g, err := loadDeletionGroup(ctx, tx, id)
	if err != nil {
		return err
	}
	if options.GroupRevision != "" && options.GroupRevision != g.preview().GroupRevision {
		return ErrDeletionScopeChanged
	}
	if !options.DeleteSource {
		if err := g.blacklist(ctx, tx, cleanupAfter); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		c.driveEvents.Notify("", false, driveevents.MediaChanged)
		return nil
	}

	if len(g.files) > 1 && (options.IncludeDuplicates == nil || options.GroupRevision == "") {
		return ErrDeletionScopeIncomplete
	}
	successor := ""
	if options.IncludeDuplicates != nil && !*options.IncludeDuplicates {
		successor = g.successor()
		g.files = g.files[:1]
	}
	if remove == nil {
		return errors.New("source removal is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	deadline, _ := ctx.Deadline()
	token := rand.Text()
	for _, f := range g.files {
		v := f.video
		if v.FileID == "" {
			return fmt.Errorf("video %s has no source file", v.ID)
		}
		if err := reserveDeletionFile(ctx, tx, v.DriveID, v.FileID, token, deadline); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, c.releaseSourceReservations(ctx, token)) }()
	for _, f := range slices.Backward(g.files) {
		if err := remove(ctx, f.video, len(f.live) == 0); err != nil {
			return fmt.Errorf("delete source %s/%s: %w", f.video.DriveID, f.video.FileID, err)
		}
		finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		err := c.finishDeletionFile(finish, f, token, successor)
		if err == nil && cleanAssets != nil {
			for _, v := range slices.Concat(f.live, f.retired) {
				if err = cleanAssets(finish, v); err != nil {
					break
				}
			}
		}
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

func reserveDeletionFile(ctx context.Context, tx *sql.Tx, drive, file, token string, deadline time.Time) error {
	res, err := tx.ExecContext(ctx, `INSERT INTO video_source_removals (drive_id, file_id, token, expires_at)
		VALUES(?,?,?,?) ON CONFLICT(drive_id,file_id) DO UPDATE SET token=excluded.token, expires_at=excluded.expires_at
		WHERE video_source_removals.expires_at <= unixepoch()`, drive, file, token, deadline.Add(time.Minute).Unix())
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return ErrVideoSourceInUse
	}
	return nil
}

// successor is the earliest visible member that keeps its file when only the
// selected file is removed.
func (g *deletionGroup) successor() string {
	var keep *Video
	for _, f := range g.files[1:] {
		for _, v := range f.live {
			if v.Hidden || v.FileID == "" {
				continue
			}
			if keep == nil || v.CreatedAt.Before(keep.CreatedAt) || (v.CreatedAt.Equal(keep.CreatedAt) && v.ID < keep.ID) {
				keep = v
			}
		}
	}
	if keep == nil {
		return ""
	}
	return keep.ID
}

func (c *Catalog) finishDeletionFile(ctx context.Context, f *deletionFile, token, successor string) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	v := f.video
	res, err := tx.ExecContext(ctx, "DELETE FROM video_source_removals WHERE drive_id=? AND file_id=? AND token=?", v.DriveID, v.FileID, token)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return ErrVideoSourceInUse
	}
	for _, member := range f.live {
		current, err := scanVideo(tx.QueryRowContext(ctx, "SELECT "+allVideoCols+" FROM videos WHERE id=? AND drive_id=? AND file_id=?", member.ID, v.DriveID, v.FileID))
		if err != nil {
			return err
		}
		if err := enqueueDuplicateAssetCleanupTx(ctx, tx, current); err != nil {
			return err
		}
		if err := deleteVideoRowTx(ctx, tx, current.ID); err != nil {
			return err
		}
	}
	if err := purgeSourceTombstonesTx(ctx, tx, v); err != nil {
		return err
	}
	if err := rehomeMergedSourcesTx(ctx, tx, f.live, successor); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	c.driveEvents.Notify("", false, driveevents.MediaChanged)
	return nil
}

// Merged copies on files that are kept outlive their deleted canonical. A
// surviving group member takes over their public IDs and scan skips. Without
// one, the copies are released so the next scan admits them under their own
// IDs instead of new generation IDs.
func rehomeMergedSourcesTx(ctx context.Context, tx *sql.Tx, removed []*Video, successor string) error {
	if successor != "" {
		var live bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM videos WHERE id = ? AND `+duplicateLookupScope+`)`, successor).Scan(&live); err != nil {
			return err
		}
		if !live {
			successor = ""
		}
	}
	for _, v := range removed {
		var err error
		if successor != "" {
			_, err = tx.ExecContext(ctx, `UPDATE merged_videos SET canonical_video_id = ? WHERE canonical_video_id = ?`, successor, v.ID)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM merged_videos WHERE canonical_video_id = ? AND source_state = ?`, v.ID, MergedSourceRecorded)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
