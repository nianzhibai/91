package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/video-site/backend/internal/tagging"
)

func (c *Catalog) CreateTagAndClassify(ctx context.Context, label string, aliases []string, source string) (int, error) {
	tag, err := c.ensureTag(ctx, label, aliases, source)
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
		aliasesJSON, _ := json.Marshal([]string{})
		rulesJSON, _ := json.Marshal(rule)
		if _, err := c.db.ExecContext(ctx,
			`UPDATE tags SET aliases = ?, match_rules = ?, updated_at = ? WHERE id = ?`,
			string(aliasesJSON), string(rulesJSON), time.Now().UnixMilli(), tagID); err != nil {
			return Tag{}, err
		}
		if err := c.setAVCodeMatchingDisabled(ctx, len(prefixes) == 0); err != nil {
			return Tag{}, err
		}
		if err := c.bumpTagRulesVersion(ctx); err != nil {
			return Tag{}, err
		}
		return c.getTagByID(ctx, tagID)
	}
	rule = cleanTagRule(rule)
	aliasesJSON, _ := json.Marshal([]string{})
	rulesJSON, _ := json.Marshal(rule)
	if _, err := c.db.ExecContext(ctx,
		`UPDATE tags SET aliases = ?, match_rules = ?, updated_at = ? WHERE id = ?`,
		string(aliasesJSON), string(rulesJSON), time.Now().UnixMilli(), tagID); err != nil {
		return Tag{}, err
	}
	if err := c.bumpTagRulesVersion(ctx); err != nil {
		return Tag{}, err
	}
	return c.getTagByID(ctx, tagID)
}

// UpdateTagAndReconcile saves one rule and refreshes only assignments owned by
// that rule. Ordinary tags use the single-tag keyword reconciler; AV uses its
// scoped umbrella-and-series reconciler. Neither path runs unrelated tag rules
// or startup cleanup.
func (c *Catalog) UpdateTagAndReconcile(ctx context.Context, tagID int64, rule tagging.Rule) (Tag, int, error) {
	c.tagMaintenanceMu.Lock()
	defer c.tagMaintenanceMu.Unlock()

	previousTag, err := c.getTagByID(ctx, tagID)
	if err != nil {
		return Tag{}, 0, err
	}
	tag, err := c.UpdateTag(ctx, tagID, rule)
	if err != nil {
		return Tag{}, 0, err
	}
	if strings.EqualFold(tag.Label, avTagLabel) {
		changed, err := c.reconcileAVTagAssignments(ctx, previousTag, tag)
		return tag, changed, err
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

func (c *Catalog) EnsureTagForVideoIDPrefix(ctx context.Context, prefix, label string, aliases []string, source string) (int, error) {
	return c.ensureTagForVideoIDPrefix(ctx, prefix, label, aliases, source, true)
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
	return c.addTagForVideoIDPrefix(ctx, prefix, tag, false)
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

func (c *Catalog) ensureTagForVideoIDPrefix(ctx context.Context, prefix, label string, aliases []string, source string, respectAutoGenerateSetting bool) (int, error) {
	tag, err := c.ensureTagWithRulesInternal(ctx, label, aliases, tagging.Rule{}, source, respectAutoGenerateSetting)
	if err != nil {
		return 0, err
	}
	return c.addTagForVideoIDPrefix(ctx, prefix, tag, true)
}

func (c *Catalog) addTagForVideoIDPrefix(ctx context.Context, prefix string, tag Tag, skipManual bool) (int, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return 0, errors.New("video id prefix is required")
	}
	manualWhere := ""
	if skipManual {
		manualWhere = "   AND COALESCE(v.tags_manual, 0) = 0\n"
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT v.id
  FROM videos v
 WHERE v.id LIKE ? || '%'
`+manualWhere+`
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

	tag, err := c.getTagByIDTx(ctx, tx, tagID)
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
	if strings.EqualFold(tag.Label, avTagLabel) {
		avSeriesVideoIDs, err := cleanupGeneratedAVSeriesTagsTx(ctx, tx)
		if err != nil {
			return 0, err
		}
		videoIDs = append(videoIDs, avSeriesVideoIDs...)
		if err := setAVCodeMatchingDisabledTx(ctx, tx, true); err != nil {
			return 0, err
		}
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

func cleanupGeneratedAVSeriesTagsTx(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT DISTINCT vt.video_id
  FROM video_tags vt
  JOIN tags t ON t.id = vt.tag_id
 WHERE lower(trim(COALESCE(t.source, ''))) = 'generated'
   AND lower(trim(COALESCE(t.origin, ''))) = ?`, avSeriesOrigin)
	if err != nil {
		return nil, err
	}
	var videoIDs []string
	for rows.Next() {
		var videoID string
		if err := rows.Scan(&videoID); err != nil {
			rows.Close()
			return nil, err
		}
		videoIDs = append(videoIDs, videoID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM video_tags
 WHERE tag_id IN (
       SELECT id
         FROM tags
        WHERE lower(trim(COALESCE(source, ''))) = 'generated'
          AND lower(trim(COALESCE(origin, ''))) = ?
 )`, avSeriesOrigin); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM tags
 WHERE lower(trim(COALESCE(source, ''))) = 'generated'
   AND lower(trim(COALESCE(origin, ''))) = ?`, avSeriesOrigin); err != nil {
		return nil, err
	}
	return videoIDs, nil
}

func (c *Catalog) cleanupInvalidAVSeriesTags(ctx context.Context) error {
	allowedLabels := map[string]struct{}{}
	avCodes, err := c.avCodeMatcher(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, prefix := range avCodes.Prefixes() {
		allowedLabels[strings.ToLower(prefix)] = struct{}{}
	}

	rows, err := c.db.QueryContext(ctx, `
SELECT id, label
  FROM tags
 WHERE lower(trim(COALESCE(source, ''))) = 'generated'
   AND lower(trim(COALESCE(origin, ''))) = ?`, avSeriesOrigin)
	if err != nil {
		return err
	}
	var tagIDs []int64
	for rows.Next() {
		var tagID int64
		var label string
		if err := rows.Scan(&tagID, &label); err != nil {
			rows.Close()
			return err
		}
		if _, ok := allowedLabels[strings.ToLower(tagging.NormalizeAVCodePrefix(label))]; !ok {
			tagIDs = append(tagIDs, tagID)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(tagIDs) == 0 {
		return nil
	}

	args := make([]any, 0, len(tagIDs))
	for _, tagID := range tagIDs {
		args = append(args, tagID)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	affectedRows, err := tx.QueryContext(ctx, `SELECT DISTINCT video_id FROM video_tags WHERE tag_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return err
	}
	var videoIDs []string
	for affectedRows.Next() {
		var videoID string
		if err := affectedRows.Scan(&videoID); err != nil {
			affectedRows.Close()
			return err
		}
		videoIDs = append(videoIDs, videoID)
	}
	if err := affectedRows.Err(); err != nil {
		affectedRows.Close()
		return err
	}
	if err := affectedRows.Close(); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM video_tags WHERE tag_id IN (`+placeholders+`)`, args...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tags WHERE id IN (`+placeholders+`)`, args...); err != nil {
		return err
	}
	for _, videoID := range uniqueStrings(videoIDs) {
		manual := hasManualTagsTx(ctx, tx, videoID)
		if err := syncVideoTagsJSONTx(ctx, tx, videoID, manual); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := c.bumpTagRulesVersion(ctx); err != nil {
		return err
	}
	log.Printf("[catalog] removed %d invalid AV series tag(s)", len(tagIDs))
	return nil
}

func (c *Catalog) ListTags(ctx context.Context) ([]Tag, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT t.id,
       t.label,
       t.aliases,
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
GROUP BY t.id, t.label, t.aliases, t.match_rules, t.source, t.origin
ORDER BY cnt DESC, t.label ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Tag, 0)
	for rows.Next() {
		var tag Tag
		var aliasesJSON, rulesJSON string
		var crawlerOwned int
		if err := rows.Scan(&tag.ID, &tag.Label, &aliasesJSON, &rulesJSON, &tag.Source, &tag.Count, &crawlerOwned); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(aliasesJSON), &tag.Aliases)
		_ = json.Unmarshal([]byte(rulesJSON), &tag.MatchRules)
		tag.MatchRules = effectiveRule(tag.Label, tag.Aliases, tag.MatchRules)
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
