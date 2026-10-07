package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/video-site/backend/internal/tagging"
)

func (c *Catalog) CreateTagAndClassify(ctx context.Context, label string, source string) (int, error) {
	tag, err := c.ensureTag(ctx, label, source)
	if err != nil {
		return 0, err
	}
	return c.classifyTag(ctx, tag)
}

// UpdateTag 更新管理后台"编辑标签"内容。普通标签保存完整匹配规则；
// AV 标签只保存车牌前缀规则。
func (c *Catalog) UpdateTag(ctx context.Context, tagID int64, rule tagging.Rule) (Tag, error) {
	tag, err := c.getTagByID(ctx, tagID)
	if err != nil {
		return Tag{}, err
	}
	if strings.EqualFold(tag.Label, avTagLabel) {
		prefixes := tagging.CleanAVCodePrefixes(rule.AVCodePrefixes)
		rule = avRuleFromPrefixes(prefixes)
		rulesJSON, _ := json.Marshal(rule)
		if _, err := c.db.ExecContext(ctx,
			`UPDATE tags SET match_rules = ?, updated_at = ? WHERE id = ?`,
			string(rulesJSON), time.Now().UnixMilli(), tagID); err != nil {
			return Tag{}, err
		}
		if err := c.bumpTagRulesVersion(ctx); err != nil {
			return Tag{}, err
		}
		return c.getTagByID(ctx, tagID)
	}
	rule = cleanTagRule(rule)
	rulesJSON, _ := json.Marshal(rule)
	if _, err := c.db.ExecContext(ctx,
		`UPDATE tags SET match_rules = ?, updated_at = ? WHERE id = ?`,
		string(rulesJSON), time.Now().UnixMilli(), tagID); err != nil {
		return Tag{}, err
	}
	if err := c.bumpTagRulesVersion(ctx); err != nil {
		return Tag{}, err
	}
	return c.getTagByID(ctx, tagID)
}

// UpdateTagAndReconcile saves one rule and refreshes only assignments owned by
// that rule, including AV. It does not run unrelated tag rules or startup cleanup.
func (c *Catalog) UpdateTagAndReconcile(ctx context.Context, tagID int64, rule tagging.Rule) (Tag, int, error) {
	c.tagMaintenanceMu.Lock()
	defer c.tagMaintenanceMu.Unlock()

	tag, err := c.UpdateTag(ctx, tagID, rule)
	if err != nil {
		return Tag{}, 0, err
	}
	changed, err := c.reconcileTagAssignments(ctx, tag)
	return tag, changed, err
}

// ClassifyTagByID applies an existing tag's current rule to matching unlocked
// videos. It never creates new tag definitions.
func (c *Catalog) ClassifyTagByID(ctx context.Context, tagID int64) (int, error) {
	tag, err := c.getTagByID(ctx, tagID)
	if err != nil {
		return 0, err
	}
	return c.classifyTag(ctx, tag)
}

func (c *Catalog) EnsureCrawlerTagForVideoIDPrefix(ctx context.Context, prefix, label string) (int, error) {
	hasVideos, err := c.videoIDPrefixExists(ctx, prefix)
	if err != nil || !hasVideos {
		return 0, err
	}
	tag, err := c.EnsureCrawlerTag(ctx, label)
	if err != nil {
		return 0, err
	}
	return c.addTagForVideoIDPrefix(ctx, prefix, tag)
}

func (c *Catalog) videoIDPrefixExists(ctx context.Context, prefix string) (bool, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return false, errors.New("video id prefix is required")
	}
	var n int
	err := c.db.QueryRowContext(ctx, `
SELECT 1
  FROM videos
 WHERE id LIKE ? || '%'
 LIMIT 1`, prefix).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (c *Catalog) addTagForVideoIDPrefix(ctx context.Context, prefix string, tag Tag) (int, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return 0, errors.New("video id prefix is required")
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT v.id
  FROM videos v
 WHERE v.id LIKE ? || '%'
   AND NOT EXISTS (
	 SELECT 1
	   FROM video_tags vt
	  WHERE vt.video_id = v.id
	    AND vt.tag_id = ?
   )
 ORDER BY v.id ASC`, prefix, tag.ID)
	if err != nil {
		return 0, err
	}
	var videoIDs []string
	for rows.Next() {
		var videoID string
		if err := rows.Scan(&videoID); err != nil {
			rows.Close()
			return 0, err
		}
		videoIDs = append(videoIDs, videoID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, videoID := range videoIDs {
		if err := c.insertVideoTag(ctx, videoID, tag.ID, "crawler", "爬虫:"+tag.Label); err != nil {
			return 0, err
		}
		if err := c.syncVideoTagsJSON(ctx, videoID, c.hasManualTags(ctx, videoID)); err != nil {
			return 0, err
		}
	}
	return len(videoIDs), nil
}

func (c *Catalog) DeleteTag(ctx context.Context, tagID int64) (int, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	_, err = c.getTagByIDTx(ctx, tx, tagID)
	if err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT video_id FROM video_tags WHERE tag_id = ?`, tagID)
	if err != nil {
		return 0, err
	}
	var videoIDs []string
	for rows.Next() {
		var videoID string
		if err := rows.Scan(&videoID); err != nil {
			rows.Close()
			return 0, err
		}
		videoIDs = append(videoIDs, videoID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM video_tags WHERE tag_id = ?`, tagID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tags WHERE id = ?`, tagID); err != nil {
		return 0, err
	}

	affectedVideoIDs := uniqueStrings(videoIDs)
	for _, videoID := range affectedVideoIDs {
		manual := hasManualTagsTx(ctx, tx, videoID)
		if err := syncVideoTagsJSONTx(ctx, tx, videoID, manual); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if err := c.bumpTagRulesVersion(ctx); err != nil {
		return 0, err
	}
	return len(affectedVideoIDs), nil
}

func (c *Catalog) ListTags(ctx context.Context) ([]Tag, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT t.id,
       t.label,
       COALESCE(t.match_rules, '{}'),
       t.source,
       COUNT(DISTINCT videos.id) AS cnt,
       CASE
         WHEN COALESCE(t.origin, '') = 'crawler'
           OR EXISTS (
             SELECT 1
               FROM video_tags vt_origin
              WHERE vt_origin.tag_id = t.id
                AND lower(trim(COALESCE(vt_origin.source, ''))) = 'crawler'
           )
         THEN 1 ELSE 0
       END AS crawler_owned
FROM tags t
LEFT JOIN video_tags vt ON vt.tag_id = t.id
LEFT JOIN videos tagged ON tagged.id = vt.video_id
	AND COALESCE(tagged.hidden, 0) = 0
LEFT JOIN video_dedup_representatives representative
	ON representative.video_id = tagged.id
LEFT JOIN videos ON videos.id = representative.representative_id
	AND COALESCE(videos.hidden, 0) = 0
	AND `+uniqueVideoWhereSQL+`
GROUP BY t.id, t.label, t.match_rules, t.source, t.origin
ORDER BY CASE WHEN t.source = 'user' THEN 0 ELSE 1 END,
         t.label COLLATE NOCASE ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Tag, 0)
	for rows.Next() {
		var tag Tag
		var rulesJSON string
		var crawlerOwned int
		if err := rows.Scan(&tag.ID, &tag.Label, &rulesJSON, &tag.Source, &tag.Count, &crawlerOwned); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(rulesJSON), &tag.MatchRules)
		tag.MatchRules = effectiveRule(tag.Label, tag.MatchRules)
		tag.CrawlerOwned = crawlerOwned != 0
		out = append(out, tag)
	}
	return out, nil
}

// ListUserSelectableTags returns the part of the managed tag catalog intended
// for explicit user assignment. Keeping this policy beside lookup validation
// makes the picker and upload write path use the same source of truth.
func (c *Catalog) ListUserSelectableTags(ctx context.Context) ([]Tag, error) {
	tags, err := c.ListTags(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Tag, 0, len(tags))
	for _, tag := range tags {
		if isUserSelectableTag(tag) {
			out = append(out, tag)
		}
	}
	return out, nil
}

func videoMatchesTagLabelSQL(videoAlias string) string {
	return fmt.Sprintf(`%s.id IN (
			SELECT representative.representative_id
			  FROM video_tags vt
			  JOIN tags tag_filter ON tag_filter.id = vt.tag_id
			  JOIN videos tagged ON tagged.id = vt.video_id
			  JOIN video_dedup_representatives representative
			    ON representative.video_id = tagged.id
			 WHERE tag_filter.label = ? COLLATE NOCASE
			   AND COALESCE(tagged.hidden, 0) = 0
		)`, videoAlias)
}
