package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/video-site/backend/internal/tagging"
)

func (c *Catalog) SetManualVideoTags(ctx context.Context, videoID string, labels []string) error {
	if _, err := c.GetVideo(ctx, videoID); err != nil {
		return err
	}
	return c.replaceManualVideoTags(ctx, videoID, labels, false)
}

// SetAutoVideoTags 用引擎结果覆盖视频的 auto/legacy 标签行；来源标签和人工标签保留。
func (c *Catalog) SetAutoVideoTags(ctx context.Context, videoID string, labels []string) error {
	assignments := make([]TagAssignment, 0, len(labels))
	for _, label := range labels {
		assignments = append(assignments, TagAssignment{Label: label, Source: "auto"})
	}
	_, err := c.ReplaceAutoVideoTags(ctx, videoID, assignments)
	return err
}

// ---------- 匹配引擎入口 ----------

// Matcher 返回按当前标签池编译的匹配器；带版本号缓存，标签变更后自动重建。
func (c *Catalog) Matcher(ctx context.Context) (*tagging.Matcher, error) {
	version, err := c.tagRulesVersion(ctx)
	if err != nil {
		return nil, err
	}
	c.matcherMu.Lock()
	if c.matcher != nil && c.matcherVersion == version {
		m := c.matcher
		c.matcherMu.Unlock()
		return m, nil
	}
	c.matcherMu.Unlock()

	m, err := c.buildMatcher(ctx)
	if err != nil {
		return nil, err
	}
	c.matcherMu.Lock()
	c.matcher = m
	c.matcherVersion = version
	c.matcherMu.Unlock()
	return m, nil
}

func (c *Catalog) buildMatcher(ctx context.Context) (*tagging.Matcher, error) {
	return buildTagMatcher(ctx, c.db)
}

type tagMatcherQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func buildTagMatcher(ctx context.Context, query tagMatcherQuerier) (*tagging.Matcher, error) {
	rows, err := query.QueryContext(ctx,
		`SELECT label, COALESCE(match_rules, '{}'), COALESCE(origin, '') FROM tags ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tagRules []tagging.TagRule
	for rows.Next() {
		var label, rulesJSON, origin string
		if err := rows.Scan(&label, &rulesJSON, &origin); err != nil {
			return nil, err
		}
		origin = strings.ToLower(strings.TrimSpace(origin))
		if origin == telegramTagOrigin {
			continue
		}
		var rule tagging.Rule
		_ = json.Unmarshal([]byte(rulesJSON), &rule)
		tagRules = append(tagRules, tagging.TagRule{Label: label, Rule: effectiveRule(label, rule)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tagging.NewMatcher(tagRules), nil
}

// matchTaglessVideoTx initializes automatic tags for a restored row that has
// no surviving assignments and is not manually locked. Read current rules from
// the owning transaction so matching and publication share the same snapshot.
func matchTaglessVideoTx(ctx context.Context, tx *sql.Tx, video *Video) error {
	var needsMatching bool
	if err := tx.QueryRowContext(ctx, `
SELECT COALESCE(tags_manual, 0) = 0
   AND NOT EXISTS (SELECT 1 FROM video_tags WHERE video_id = videos.id)
  FROM videos
 WHERE id = ?`, video.ID).Scan(&needsMatching); err != nil {
		return err
	}
	if !needsMatching {
		return nil
	}
	matcher, err := buildTagMatcher(ctx, tx)
	if err != nil {
		return err
	}
	assignments := matchTagAssignmentsWithMatcher(matcher, video.Title, video.FileName, video.Author, video.DirName, video.AncestorDirNames...)
	changed, err := replaceAutoVideoTagsTx(ctx, tx, video.ID, assignments)
	if err != nil || !changed {
		return err
	}
	return syncVideoTagsJSONTx(ctx, tx, video.ID, false)
}

// effectiveRule 计算标签的生效规则：普通标签无显式规则时按标签名匹配；
// 有显式规则时按规则本身执行。AV 标签例外：它只按番号规则识别，避免
// "AV/JAV/番号" 这类普通描述误触发。
func effectiveRule(label string, rule tagging.Rule) tagging.Rule {
	if strings.EqualFold(label, avTagLabel) {
		prefixes := tagging.CleanAVCodePrefixes(rule.AVCodePrefixes)
		if len(prefixes) == 0 {
			return tagging.Rule{}
		}
		return tagging.Rule{MatchAVCode: true, AVCodePrefixes: prefixes}
	}
	if rule.IsEmpty() {
		return tagging.Rule{Keywords: []string{label}}
	}
	return rule
}

func (c *Catalog) tagRulesVersion(ctx context.Context) (int64, error) {
	raw, err := c.GetSetting(ctx, settingTagRulesVersion, "0")
	if err != nil {
		return 0, err
	}
	version, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, nil
	}
	return version, nil
}

func (c *Catalog) bumpTagRulesVersion(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx, `
INSERT INTO settings (key, value, updated_at) VALUES (?, '1', ?)
ON CONFLICT(key) DO UPDATE SET
  value = CAST(CAST(settings.value AS INTEGER) + 1 AS TEXT),
  updated_at = excluded.updated_at`, settingTagRulesVersion, time.Now().UnixMilli())
	return err
}

func bumpTagRulesVersionTx(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO settings (key, value, updated_at) VALUES (?, '1', ?)
ON CONFLICT(key) DO UPDATE SET
  value = CAST(CAST(settings.value AS INTEGER) + 1 AS TEXT),
  updated_at = excluded.updated_at`, settingTagRulesVersion, time.Now().UnixMilli())
	return err
}

// LookupTagLabel 查询某个标签是否已存在（大小写不敏感），返回库中的规范写法。
func (c *Catalog) LookupTagLabel(ctx context.Context, label string) (string, bool, error) {
	label = cleanTagLabel(label)
	if label == "" {
		return "", false, nil
	}
	tag, err := c.getTagByLabel(ctx, label)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return tag.Label, true, nil
}

// LookupUserSelectableTagLabel resolves a label that may be assigned explicitly
// by a user. Generated provenance tags are maintained by their import pipelines.
func (c *Catalog) LookupUserSelectableTagLabel(ctx context.Context, label string) (string, bool, error) {
	label = cleanTagLabel(label)
	if label == "" {
		return "", false, nil
	}
	tag, err := c.getTagByLabel(ctx, label)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !isUserSelectableTag(tag) {
		return "", false, nil
	}
	return tag.Label, true, nil
}

func isUserSelectableTag(tag Tag) bool {
	return normalizeTagSource(tag.Source) == "user"
}

// MatchTags 对一段文本运行标签匹配，返回命中的标签名。
func (c *Catalog) MatchTags(ctx context.Context, text string) ([]string, error) {
	matcher, err := c.Matcher(ctx)
	if err != nil {
		return nil, err
	}
	return matcher.MatchLabels(text), nil
}

// MatchTagAssignments matches video metadata against the existing tag pool.
// Matching never creates tag definitions; recognized codes only add AV.
func (c *Catalog) MatchTagAssignments(ctx context.Context, title, fileName, author, dirName string, ancestorDirNames ...string) ([]TagAssignment, error) {
	matcher, err := c.Matcher(ctx)
	if err != nil {
		return nil, err
	}
	return matchTagAssignmentsWithMatcher(matcher, title, fileName, author, dirName, ancestorDirNames...), nil
}

func matchTagAssignmentsWithMatcher(matcher *tagging.Matcher, title, fileName, author, dirName string, ancestorDirNames ...string) []TagAssignment {
	matches := matcher.Match(matchFields(title, fileName, author, dirName, ancestorDirNames...)...)
	out := make([]TagAssignment, 0, len(matches))
	for _, m := range matches {
		out = append(out, TagAssignment{Label: m.Label, Source: "auto", Evidence: m.Evidence()})
	}
	return out
}

func (c *Catalog) ensureTag(ctx context.Context, label string, source string) (Tag, error) {
	return c.ensureTagWithRules(ctx, label, tagging.Rule{}, source)
}

// EnsureTag ensures that a tag exists and returns its canonical database row.
func (c *Catalog) EnsureTag(ctx context.Context, label, source string) (Tag, error) {
	return c.ensureTag(ctx, label, source)
}

// EnsureCrawlerTag creates a tag owned by a crawler's import pipeline.
func (c *Catalog) EnsureCrawlerTag(ctx context.Context, label string) (Tag, error) {
	label = cleanTagLabel(label)
	tag, err := c.ensureTagDefinition(ctx, label, tagging.Rule{}, "generated")
	if err != nil {
		return Tag{}, err
	}
	if err := c.markTagOrigin(ctx, tag.ID, "crawler"); err != nil {
		return Tag{}, err
	}
	tag.CrawlerOwned = true
	return tag, nil
}

func (c *Catalog) markTagOrigin(ctx context.Context, tagID int64, origin string) error {
	origin = strings.TrimSpace(strings.ToLower(origin))
	if origin != "crawler" {
		origin = ""
	}
	res, err := c.db.ExecContext(ctx, `
UPDATE tags
   SET origin = ?, updated_at = ?
 WHERE id = ?
   AND COALESCE(origin, '') != ?`, origin, time.Now().UnixMilli(), tagID, origin)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		return c.bumpTagRulesVersion(ctx)
	}
	return nil
}

// EnsureCrawlerTagForVideo ensures a single crawler-owned video carries its
// crawler provenance tag, including on manually curated videos.
func (c *Catalog) EnsureCrawlerTagForVideo(ctx context.Context, videoID, label string) (bool, error) {
	videoID = strings.TrimSpace(videoID)
	if videoID == "" {
		return false, errors.New("video id is required")
	}
	tag, err := c.EnsureCrawlerTag(ctx, label)
	if err != nil {
		return false, err
	}
	changed, labelAdded, err := c.upsertVideoTagAssignment(ctx, videoID, tag.ID, "crawler", "爬虫:"+tag.Label)
	if err != nil {
		return false, err
	}
	if labelAdded {
		if err := c.syncVideoTagsJSON(ctx, videoID, c.hasManualTags(ctx, videoID)); err != nil {
			return changed, err
		}
	}
	return changed, nil
}

// ensureTagWithRules 建标签（存在则复用）。规则只在两种情况下写入：
// 新建时、或已有行的 match_rules 为空时（升级回填）；不会覆盖管理员显式改过的规则。
func (c *Catalog) ensureTagWithRules(ctx context.Context, label string, rule tagging.Rule, source string) (Tag, error) {
	if source == "" {
		source = "user"
	}
	if source != "user" {
		return Tag{}, ErrInvalidTagSource
	}
	return c.ensureTagDefinition(ctx, label, rule, source)
}

// ensureTagDefinition persists definitions for explicit user tags and
// the crawler import pipeline. It never derives labels from text.
func (c *Catalog) ensureTagDefinition(ctx context.Context, label string, rule tagging.Rule, source string) (Tag, error) {
	label = cleanTagLabel(label)
	if label == "" {
		return Tag{}, errors.New("tag label is required")
	}
	if isAVCodePollutedLabel(label) {
		label = avTagLabel
		rule = avTagRule
		source = "user"
	}
	if source == "generated" {
		tag, err := c.getTagByLabel(ctx, label)
		if err == nil {
			return tag, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Tag{}, err
		}
	}
	rulesJSON, _ := json.Marshal(rule)
	now := time.Now().UnixMilli()
	res, err := c.db.ExecContext(ctx, `
INSERT OR IGNORE INTO tags (label, match_rules, source, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)`, label, string(rulesJSON), source, now, now)
	if err != nil {
		return Tag{}, err
	}
	inserted := false
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		inserted = true
	}
	changed := inserted
	if !inserted {
		if !rule.IsEmpty() {
			// 升级回填：已有行没有显式规则时补上默认规则。
			res, err := c.db.ExecContext(ctx, `
UPDATE tags SET match_rules = ?, updated_at = ?
 WHERE label = ? COLLATE NOCASE
   AND COALESCE(match_rules, '{}') IN ('', '{}', 'null')`,
				string(rulesJSON), now, label)
			if err != nil {
				return Tag{}, err
			}
			if n, err := res.RowsAffected(); err == nil && n > 0 {
				changed = true
			}
		}
	}
	if changed {
		if err := c.bumpTagRulesVersion(ctx); err != nil {
			return Tag{}, err
		}
	}
	return c.getTagByLabel(ctx, label)
}

func matchFields(title, fileName, author, dirName string, ancestorDirNames ...string) []tagging.Field {
	fields := []tagging.Field{
		{Name: "标题", Text: title},
		{Name: "文件名", Text: fileName},
		{Name: "作者", Text: author},
		{Name: "目录", Text: dirName},
	}
	seen := map[string]bool{strings.ToLower(strings.TrimSpace(dirName)): true}
	for i := len(ancestorDirNames) - 1; i >= 0; i-- {
		name := strings.TrimSpace(ancestorDirNames[i])
		key := strings.ToLower(name)
		if name == "" || seen[key] {
			continue
		}
		seen[key] = true
		fields = append(fields, tagging.Field{Name: "上级目录", Text: name})
	}
	return fields
}
