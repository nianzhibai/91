package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type CrawlerIdentity struct{ DiscoveryKey, SourceID string }

// BackfillCrawlerSources indexes only the exact historical crawler ID prefix.
// Existing explicit identities (including v3 IDs) take precedence.
func (c *Catalog) BackfillCrawlerSources(ctx context.Context, driveID string) error {
	prefix := "scriptcrawler-" + driveID + "-"
	_, err := c.db.ExecContext(ctx, `INSERT OR IGNORE INTO crawler_seen_sources
 (kind,drive_id,source_id,status,canonical_video_id,first_seen_at,last_seen_at)
 SELECT 'scriptcrawler',?,substr(v.id,?), 'imported',v.id,?,?
 FROM (SELECT id FROM videos UNION SELECT id FROM deleted_videos) v
 WHERE substr(v.id,1,?)=? AND length(v.id)>? AND substr(v.id,length(?)+1,3)!='v3~'
 AND NOT EXISTS(SELECT 1 FROM crawler_seen_sources s WHERE s.kind='scriptcrawler' AND s.drive_id=? AND s.canonical_video_id=v.id)`, driveID, len([]rune(prefix))+1, time.Now().UnixMilli(), time.Now().UnixMilli(), len([]rune(prefix)), prefix, len([]rune(prefix)), prefix, driveID)
	return err
}

// KnownCrawlerCandidates performs one bounded query for a discovery page.
func (c *Catalog) KnownCrawlerCandidates(ctx context.Context, driveID string, candidates []CrawlerIdentity) (map[string]bool, error) {
	out := map[string]bool{}
	if len(candidates) == 0 {
		return out, nil
	}
	values := make([]string, len(candidates))
	args := []any{}
	for i, candidate := range candidates {
		values[i] = "(?,?)"
		args = append(args, candidate.DiscoveryKey, candidate.SourceID)
	}
	args = append(args, driveID, driveID)
	rows, err := c.db.QueryContext(ctx, `WITH candidates(discovery_key,source_id) AS (VALUES `+strings.Join(values, ",")+`)
 SELECT c.discovery_key FROM candidates c
 LEFT JOIN crawler_discoveries a ON a.drive_id=? AND a.discovery_key=c.discovery_key
 JOIN crawler_seen_sources s ON s.kind='scriptcrawler' AND s.drive_id=?
 AND s.source_id=CASE WHEN c.source_id!='' THEN c.source_id ELSE a.source_id END
 WHERE s.status IN ('imported','duplicate','blacklisted')`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		out[key] = true
	}
	return out, rows.Err()
}
func (c *Catalog) BindCrawlerDiscovery(ctx context.Context, driveID, key, sourceID string) error {
	return bindCrawlerDiscovery(ctx, c.db, driveID, key, sourceID)
}
func bindCrawlerDiscovery(ctx context.Context, exec videoRowExecer, driveID, key, sourceID string) error {
	if key == "" {
		return nil
	}
	// Never create an alias for a merely discovered or failed candidate.
	_, err := exec.ExecContext(ctx, `INSERT INTO crawler_discoveries(drive_id,discovery_key,source_id)
 SELECT ?,?,? WHERE EXISTS(SELECT 1 FROM crawler_seen_sources WHERE kind='scriptcrawler' AND drive_id=? AND source_id=?)
 ON CONFLICT(drive_id,discovery_key) DO UPDATE SET source_id=excluded.source_id`, driveID, key, sourceID, driveID, sourceID)
	return err
}
func (c *Catalog) CrawlerSourceForVideo(ctx context.Context, driveID, videoID string) (string, error) {
	var id string
	err := c.db.QueryRowContext(ctx, `SELECT source_id FROM crawler_seen_sources WHERE kind='scriptcrawler' AND drive_id=? AND canonical_video_id=? AND status='imported' LIMIT 1`, driveID, videoID).Scan(&id)
	return id, err
}

// ImportCrawlerVideo commits the video and its identity together. The importer
// attaches tags after commit; a tag failure cannot turn an inserted video into a
// failed import or leave a video without its source identity.
func (c *Catalog) ImportCrawlerVideo(ctx context.Context, v *Video, sourceID, key string) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM videos WHERE id=? UNION ALL SELECT 1 FROM deleted_videos WHERE id=? LIMIT 1`, v.ID, v.ID).Scan(&exists)
	if err == nil {
		return fmt.Errorf("crawler video identity already exists: %s", v.ID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err = upsertVideoRow(ctx, tx, v); err != nil {
		return err
	}
	if err = markCrawlerSourceSeen(ctx, tx, "scriptcrawler", v.DriveID, sourceID, "imported", v.ID, v.SampledSHA256, v.Size); err != nil {
		return err
	}
	if err = bindCrawlerDiscovery(ctx, tx, v.DriveID, key, sourceID); err != nil {
		return err
	}
	return tx.Commit()
}
