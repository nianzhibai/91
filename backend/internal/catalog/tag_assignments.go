package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/video-site/backend/internal/fixedtags"
	"github.com/video-site/backend/internal/tagging"
)

const tagSelectCols = `id, label, aliases, COALESCE(match_rules, '{}'), source, 0`

func (c *Catalog) getTagByLabel(ctx context.Context, label string) (Tag, error) {
	row := c.db.QueryRowContext(ctx,
		`SELECT `+tagSelectCols+` FROM tags WHERE label = ? COLLATE NOCASE`,
		label)
	return scanTag(row)
}

func (c *Catalog) getTagByID(ctx context.Context, id int64) (Tag, error) {
	row := c.db.QueryRowContext(ctx,
		`SELECT `+tagSelectCols+` FROM tags WHERE id = ?`,
		id)
	return scanTag(row)
}

// classifyTag 用单个标签的规则对全库做“只增”分类（新建标签或显式补分类时调用）。
func (c *Catalog) classifyTag(ctx context.Context, tag Tag) (int, error) {
	matcher := tagging.NewMatcher([]tagging.TagRule{
		{Label: tag.Label, Rule: effectiveRule(tag.Label, tag.Aliases, tag.MatchRules)},
	})
	rows, err := c.db.QueryContext(ctx, `
SELECT id, title, COALESCE(author, ''), COALESCE(file_name, ''), COALESCE(dir_name, ''), COALESCE(tags_manual, 0)
FROM videos`)
	if err != nil {
		return 0, err
	}

	type hit struct {
		videoID  string
		evidence string
	}
	var hits []hit
	for rows.Next() {
		var videoID, title, author, fileName, dirName string
		var manual int
		if err := rows.Scan(&videoID, &title, &author, &fileName, &dirName, &manual); err != nil {
			rows.Close()
			return 0, err
		}
		if manual == 1 {
			continue
		}
		matches := matcher.Match(matchFields(title, fileName, author, dirName)...)
		if len(matches) == 0 {
			continue
		}
		hits = append(hits, hit{videoID: videoID, evidence: matches[0].Evidence()})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	changedCount := 0
	for _, h := range hits {
		changed, labelAdded, err := c.upsertVideoTagAssignment(ctx, h.videoID, tag.ID, "auto", h.evidence)
		if err != nil {
			return 0, err
		}
		if changed {
			changedCount++
			if labelAdded {
				if err := c.syncVideoTagsJSON(ctx, h.videoID, false); err != nil {
					return 0, err
				}
			}
		}
	}
	return changedCount, nil
}

type tagAssignmentMutationAction uint8

const (
	tagAssignmentInsert tagAssignmentMutationAction = iota + 1
	tagAssignmentUpdate
	tagAssignmentDelete
)

type tagAssignmentMutation struct {
	videoID           string
	tagID             int64
	action            tagAssignmentMutationAction
	evidence          string
	membershipChanged bool
}

// reconcileTagAssignments evaluates one edited tag against all unlocked videos
// and changes only that tag's engine-owned assignment. Other tag definitions and
// assignments are deliberately outside this operation.
func (c *Catalog) reconcileTagAssignments(ctx context.Context, tag Tag) (int, error) {
	matcher := tagging.NewMatcher([]tagging.TagRule{
		{Label: tag.Label, Rule: effectiveRule(tag.Label, tag.Aliases, tag.MatchRules)},
	})
	rows, err := c.db.QueryContext(ctx, `
SELECT v.id,
       v.title,
       COALESCE(v.author, ''),
       COALESCE(v.file_name, ''),
       COALESCE(v.dir_name, ''),
       COALESCE(v.tags_manual, 0),
       CASE WHEN vt.tag_id IS NULL THEN 0 ELSE 1 END,
       COALESCE(vt.source, ''),
       COALESCE(vt.evidence, '')
  FROM videos v
  LEFT JOIN video_tags vt
    ON vt.video_id = v.id
   AND vt.tag_id = ?
 ORDER BY v.id ASC`, tag.ID)
	if err != nil {
		return 0, err
	}

	var mutations []tagAssignmentMutation
	for rows.Next() {
		var videoID, title, author, fileName, dirName string
		var manual, assigned int
		var source, evidence string
		if err := rows.Scan(
			&videoID,
			&title,
			&author,
			&fileName,
			&dirName,
			&manual,
			&assigned,
			&source,
			&evidence,
		); err != nil {
			rows.Close()
			return 0, err
		}
		if manual == 1 {
			continue
		}

		matches := matcher.Match(matchFields(title, fileName, author, dirName)...)
		if len(matches) == 0 {
			normalizedSource := strings.ToLower(strings.TrimSpace(source))
			if assigned == 1 && (normalizedSource == "auto" || normalizedSource == "legacy") {
				mutations = append(mutations, tagAssignmentMutation{
					videoID:           videoID,
					tagID:             tag.ID,
					action:            tagAssignmentDelete,
					membershipChanged: true,
				})
			}
			continue
		}

		nextEvidence := matches[0].Evidence()
		if assigned == 0 {
			mutations = append(mutations, tagAssignmentMutation{
				videoID:           videoID,
				tagID:             tag.ID,
				action:            tagAssignmentInsert,
				evidence:          nextEvidence,
				membershipChanged: true,
			})
			continue
		}
		if !shouldReplaceVideoTagAssignment(source, "auto") {
			continue
		}
		if normalizeVideoTagSource(source) == "auto" && evidence == nextEvidence {
			continue
		}
		mutations = append(mutations, tagAssignmentMutation{
			videoID:  videoID,
			tagID:    tag.ID,
			action:   tagAssignmentUpdate,
			evidence: nextEvidence,
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(mutations) == 0 {
		return 0, nil
	}

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	changedCount, membershipChanged, err := applyTagAssignmentMutationsTx(ctx, tx, mutations)
	if err != nil {
		return 0, err
	}
	for _, videoID := range membershipChanged {
		if err := syncVideoTagsJSONTx(ctx, tx, videoID, false); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return changedCount, nil
}

func applyTagAssignmentMutationsTx(ctx context.Context, tx *sql.Tx, mutations []tagAssignmentMutation) (int, []string, error) {
	changedCount := 0
	createdAt := time.Now().UnixMilli()
	membershipChanged := make([]string, 0, len(mutations))
	membershipSeen := make(map[string]struct{}, len(mutations))
	for _, mutation := range mutations {
		var result sql.Result
		var err error
		switch mutation.action {
		case tagAssignmentInsert:
			result, err = tx.ExecContext(ctx, `
INSERT OR IGNORE INTO video_tags (video_id, tag_id, source, evidence, created_at)
VALUES (?, ?, 'auto', ?, ?)`, mutation.videoID, mutation.tagID, mutation.evidence, createdAt)
		case tagAssignmentUpdate:
			result, err = tx.ExecContext(ctx, `
UPDATE video_tags
   SET source = 'auto', evidence = ?
 WHERE video_id = ?
   AND tag_id = ?
   AND lower(trim(COALESCE(source, ''))) IN ('', 'auto', 'legacy', 'propagated')`,
				mutation.evidence, mutation.videoID, mutation.tagID)
		case tagAssignmentDelete:
			result, err = tx.ExecContext(ctx, `
DELETE FROM video_tags
 WHERE video_id = ?
   AND tag_id = ?
   AND lower(trim(COALESCE(source, ''))) IN ('auto', 'legacy')`, mutation.videoID, mutation.tagID)
		default:
			continue
		}
		if err != nil {
			return 0, nil, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return 0, nil, err
		}
		if changed == 0 {
			continue
		}
		changedCount++
		if !mutation.membershipChanged {
			continue
		}
		if _, ok := membershipSeen[mutation.videoID]; ok {
			continue
		}
		membershipSeen[mutation.videoID] = struct{}{}
		membershipChanged = append(membershipChanged, mutation.videoID)
	}
	return changedCount, membershipChanged, nil
}

func (c *Catalog) replaceVideoTags(ctx context.Context, videoID string, labels []string, source string, manual bool, createMissing bool) error {
	labels = uniqueStrings(cleanLabels(labels))
	if createMissing {
		ensureSource := "legacy"
		if source == "manual" {
			ensureSource = "user"
		}
		for _, label := range labels {
			if _, err := c.ensureTag(ctx, label, nil, ensureSource); err != nil {
				return err
			}
		}
	} else {
		if err := c.validateTagsExist(ctx, labels); err != nil {
			return err
		}
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM video_tags WHERE video_id = ?`, videoID); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for _, label := range labels {
		tag, err := c.getTagByLabelTx(ctx, tx, label)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO video_tags (video_id, tag_id, source, evidence, created_at) VALUES (?, ?, ?, '', ?)`,
			videoID, tag.ID, source, now); err != nil {
			return err
		}
	}
	manualValue := 0
	if manual {
		manualValue = 1
	}
	if _, err := tx.ExecContext(ctx, `UPDATE videos SET tags_manual = ? WHERE id = ?`, manualValue, videoID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return c.syncVideoTagsJSON(ctx, videoID, manual)
}

// ReplaceAutoVideoTags 用给定分配覆盖视频的引擎标签（source IN auto/legacy），
// 其它来源的行保留。人工锁定视频直接跳过。返回是否发生了变更。
func (c *Catalog) ReplaceAutoVideoTags(ctx context.Context, videoID string, assignments []TagAssignment) (bool, error) {
	if c.hasManualTags(ctx, videoID) {
		return false, nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	changed, err := replaceAutoVideoTagsTx(ctx, tx, videoID, assignments)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	if err := syncVideoTagsJSONTx(ctx, tx, videoID, false); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

type videoTagAssignmentRow struct {
	tagID    int64
	label    string
	source   string
	evidence string
}

type desiredVideoTagAssignment struct {
	tagID    int64
	label    string
	source   string
	evidence string
}

// replaceAutoVideoTagsTx 是 ReplaceAutoVideoTags 的事务内实现，供批量重算复用。
// 返回是否有实际变更（用于跳过无谓的 JSON 同步）。
func replaceAutoVideoTagsTx(ctx context.Context, tx *sql.Tx, videoID string, assignments []TagAssignment) (bool, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT t.id, t.label, COALESCE(vt.source, ''), COALESCE(vt.evidence, '')
  FROM video_tags vt
  JOIN tags t ON t.id = vt.tag_id
 WHERE vt.video_id = ?`, videoID)
	if err != nil {
		return false, err
	}
	current := map[string]videoTagAssignmentRow{}
	for rows.Next() {
		var row videoTagAssignmentRow
		if err := rows.Scan(&row.tagID, &row.label, &row.source, &row.evidence); err != nil {
			rows.Close()
			return false, err
		}
		current[strings.ToLower(row.label)] = row
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}

	desired := map[string]desiredVideoTagAssignment{}
	for _, a := range assignments {
		label := cleanTagLabel(a.Label)
		if label == "" {
			continue
		}
		tag, err := getTagByLabelTxRaw(ctx, tx, label)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return false, err
		}
		source := normalizeVideoTagSource(a.Source)
		desired[strings.ToLower(tag.Label)] = desiredVideoTagAssignment{
			tagID:    tag.ID,
			label:    tag.Label,
			source:   source,
			evidence: a.Evidence,
		}
	}

	changed := false
	now := time.Now().UnixMilli()
	for key, existing := range current {
		if _, ok := desired[key]; ok {
			continue
		}
		if existing.source != "auto" && existing.source != "legacy" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM video_tags WHERE video_id = ? AND tag_id = ? AND source IN ('auto', 'legacy')`,
			videoID, existing.tagID); err != nil {
			return false, err
		}
		changed = true
	}

	for key, desiredRow := range desired {
		if existing, ok := current[key]; ok {
			if !shouldReplaceVideoTagAssignment(existing.source, desiredRow.source) {
				continue
			}
			evidence := desiredRow.evidence
			if evidence == "" {
				evidence = existing.evidence
			}
			if normalizeVideoTagSource(existing.source) == desiredRow.source && existing.evidence == evidence {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE video_tags SET source = ?, evidence = ? WHERE video_id = ? AND tag_id = ?`,
				desiredRow.source, evidence, videoID, desiredRow.tagID); err != nil {
				return false, err
			}
			changed = true
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO video_tags (video_id, tag_id, source, evidence, created_at) VALUES (?, ?, ?, ?, ?)`,
			videoID, desiredRow.tagID, desiredRow.source, desiredRow.evidence, now); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}

// AddVideoTagAssignments 给视频追加标签（series/propagated/crawler 等来源）。
// 只挂已存在的标签；人工锁定视频跳过。返回实际新增或来源/证据更新数。
func (c *Catalog) AddVideoTagAssignments(ctx context.Context, videoID string, assignments []TagAssignment) (int, error) {
	if len(assignments) == 0 {
		return 0, nil
	}
	if c.hasManualTags(ctx, videoID) {
		return 0, nil
	}
	changedCount := 0
	labelAdded := false
	for _, a := range assignments {
		label := cleanTagLabel(a.Label)
		if label == "" {
			continue
		}
		tag, err := c.getTagByLabel(ctx, label)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return changedCount, err
		}
		source := normalizeVideoTagSource(a.Source)
		changed, labelWasAdded, err := c.upsertVideoTagAssignment(ctx, videoID, tag.ID, source, a.Evidence)
		if err != nil {
			return changedCount, err
		}
		if changed {
			changedCount++
		}
		if labelWasAdded {
			labelAdded = true
		}
	}
	if labelAdded {
		if err := c.syncVideoTagsJSON(ctx, videoID, false); err != nil {
			return changedCount, err
		}
	}
	return changedCount, nil
}

func (c *Catalog) upsertVideoTagAssignment(ctx context.Context, videoID string, tagID int64, source, evidence string) (bool, bool, error) {
	source = normalizeVideoTagSource(source)
	var existingSource, existingEvidence string
	err := c.db.QueryRowContext(ctx,
		`SELECT COALESCE(source, ''), COALESCE(evidence, '') FROM video_tags WHERE video_id = ? AND tag_id = ?`,
		videoID, tagID).Scan(&existingSource, &existingEvidence)
	if errors.Is(err, sql.ErrNoRows) {
		res, err := c.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO video_tags (video_id, tag_id, source, evidence, created_at) VALUES (?, ?, ?, ?, ?)`,
			videoID, tagID, source, evidence, time.Now().UnixMilli())
		if err != nil {
			return false, false, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return true, true, nil
		}
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if !shouldReplaceVideoTagAssignment(existingSource, source) {
		return false, false, nil
	}
	if evidence == "" {
		evidence = existingEvidence
	}
	if normalizeVideoTagSource(existingSource) == source && existingEvidence == evidence {
		return false, false, nil
	}
	_, err = c.db.ExecContext(ctx,
		`UPDATE video_tags SET source = ?, evidence = ? WHERE video_id = ? AND tag_id = ?`,
		source, evidence, videoID, tagID)
	if err != nil {
		return false, false, err
	}
	return true, false, nil
}

func normalizeVideoTagSource(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "manual":
		return "manual"
	case "crawler":
		return "crawler"
	case "series":
		return "series"
	case "propagated":
		return "propagated"
	case "legacy":
		return "legacy"
	case "auto", "":
		return "auto"
	default:
		return "auto"
	}
}

func shouldReplaceVideoTagAssignment(existingSource, incomingSource string) bool {
	existingSource = normalizeVideoTagSource(existingSource)
	incomingSource = normalizeVideoTagSource(incomingSource)
	if existingSource == incomingSource {
		return true
	}
	return videoTagAssignmentPriority(incomingSource) > videoTagAssignmentPriority(existingSource)
}

func videoTagAssignmentPriority(source string) int {
	switch normalizeVideoTagSource(source) {
	case "manual":
		return 100
	case "crawler":
		return 90
	case "series":
		return 80
	case "auto":
		return 60
	case "propagated":
		return 50
	case "legacy":
		return 40
	default:
		return 0
	}
}

// ListVideoTagMetadata returns assignment source/evidence for the requested
// videos in one query. Keys are video ID, then canonical tag label.
func (c *Catalog) ListVideoTagMetadata(ctx context.Context, videoIDs []string) (map[string]map[string]VideoTagMetadata, error) {
	out := make(map[string]map[string]VideoTagMetadata)
	seen := make(map[string]bool, len(videoIDs))
	args := make([]any, 0, len(videoIDs))
	for _, videoID := range videoIDs {
		videoID = strings.TrimSpace(videoID)
		if videoID == "" || seen[videoID] {
			continue
		}
		seen[videoID] = true
		args = append(args, videoID)
	}
	if len(args) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	rows, err := c.db.QueryContext(ctx, `
SELECT vt.video_id, t.label, COALESCE(vt.source, ''), COALESCE(vt.evidence, '')
  FROM video_tags vt
  JOIN tags t ON t.id = vt.tag_id
 WHERE vt.video_id IN (`+placeholders+`)
 ORDER BY vt.video_id, t.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var videoID, label string
		var metadata VideoTagMetadata
		if err := rows.Scan(&videoID, &label, &metadata.Source, &metadata.Evidence); err != nil {
			return nil, err
		}
		if out[videoID] == nil {
			out[videoID] = make(map[string]VideoTagMetadata)
		}
		out[videoID][label] = metadata
	}
	return out, rows.Err()
}

func (c *Catalog) addVideoTags(ctx context.Context, videoID string, labels []string, source string, createMissing bool) (bool, error) {
	labels = uniqueStrings(cleanLabels(labels))
	changed := false
	for _, label := range labels {
		added, err := c.addVideoTag(ctx, videoID, label, source, createMissing)
		if err != nil {
			return false, err
		}
		if added {
			changed = true
		}
	}
	return changed, nil
}

func (c *Catalog) addVideoTag(ctx context.Context, videoID, label, source string, createMissing bool) (bool, error) {
	if createMissing {
		ensureSource := "legacy"
		if source == "manual" {
			ensureSource = "user"
		}
		if _, err := c.ensureTag(ctx, label, nil, ensureSource); err != nil {
			return false, err
		}
	}
	tag, err := c.getTagByLabel(ctx, label)
	if err != nil {
		return false, err
	}
	now := time.Now().UnixMilli()
	res, err := c.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO video_tags (video_id, tag_id, source, evidence, created_at) VALUES (?, ?, ?, '', ?)`,
		videoID, tag.ID, source, now)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (c *Catalog) insertVideoTag(ctx context.Context, videoID string, tagID int64, source, evidence string) error {
	_, err := c.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO video_tags (video_id, tag_id, source, evidence, created_at) VALUES (?, ?, ?, ?, ?)`,
		videoID, tagID, source, evidence, time.Now().UnixMilli())
	return err
}

func (c *Catalog) collapseAVCodeTags(ctx context.Context) error {
	enabled, err := c.avCodeMatchingEnabled(ctx)
	if err != nil || !enabled {
		return err
	}
	if _, err := c.ensureTagWithRules(ctx, avTagLabel, fixedtags.AliasesFor(avTagLabel), avTagRule, fixedtags.SourceBuiltin); err != nil {
		return err
	}
	if err := c.removeAVLegacyAliases(ctx); err != nil {
		return err
	}

	rows, err := c.db.QueryContext(ctx, `SELECT id, label FROM tags`)
	if err != nil {
		return err
	}

	type pollutedTag struct {
		id    int64
		label string
	}
	var polluted []pollutedTag
	for rows.Next() {
		var tag pollutedTag
		if err := rows.Scan(&tag.id, &tag.label); err != nil {
			return err
		}
		if strings.EqualFold(tag.label, avTagLabel) || !isAVCodePollutedLabel(tag.label) {
			continue
		}
		polluted = append(polluted, tag)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, tag := range polluted {
		videoIDs, err := c.videoIDsForTagID(ctx, tag.id)
		if err != nil {
			return err
		}
		for _, videoID := range videoIDs {
			if _, err := c.addVideoTag(ctx, videoID, avTagLabel, "auto", false); err != nil {
				return err
			}
		}
		if _, err := c.db.ExecContext(ctx, `DELETE FROM video_tags WHERE tag_id = ?`, tag.id); err != nil {
			return err
		}
		if _, err := c.db.ExecContext(ctx, `DELETE FROM tags WHERE id = ?`, tag.id); err != nil {
			return err
		}
		for _, videoID := range videoIDs {
			if err := c.syncVideoTagsJSON(ctx, videoID, c.hasManualTags(ctx, videoID)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Catalog) removeAVLegacyAliases(ctx context.Context) error {
	tag, err := c.getTagByLabel(ctx, avTagLabel)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	aliases := tag.Aliases
	if len(aliases) == 0 {
		return nil
	}
	filtered := aliases[:0]
	removed := false
	for _, alias := range aliases {
		if _, ok := avLegacyAliases[strings.ToLower(strings.TrimSpace(alias))]; ok {
			removed = true
			continue
		}
		filtered = append(filtered, alias)
	}
	if !removed {
		return nil
	}
	aliasesJSON, _ := json.Marshal(filtered)
	res, err := c.db.ExecContext(ctx,
		`UPDATE tags SET aliases = ?, updated_at = ? WHERE id = ?`,
		string(aliasesJSON), time.Now().UnixMilli(), tag.ID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		return c.bumpTagRulesVersion(ctx)
	}
	return nil
}

func (c *Catalog) videoIDsForTagID(ctx context.Context, tagID int64) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT video_id FROM video_tags WHERE tag_id = ?`, tagID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var videoIDs []string
	for rows.Next() {
		var videoID string
		if err := rows.Scan(&videoID); err != nil {
			return nil, err
		}
		videoIDs = append(videoIDs, videoID)
	}
	return videoIDs, rows.Err()
}

func (c *Catalog) videoIDSetForTagID(ctx context.Context, tagID int64) (map[string]bool, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT video_id FROM video_tags WHERE tag_id = ?`, tagID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var videoID string
		if err := rows.Scan(&videoID); err != nil {
			return nil, err
		}
		out[videoID] = true
	}
	return out, rows.Err()
}

func (c *Catalog) validateTagsExist(ctx context.Context, labels []string) error {
	for _, label := range labels {
		if _, err := c.getTagByLabel(ctx, label); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: %s", ErrUnknownTag, label)
			}
			return err
		}
	}
	return nil
}

func (c *Catalog) syncVideoTagsJSON(ctx context.Context, videoID string, manual bool) error {
	rows, err := c.db.QueryContext(ctx, `
SELECT t.label
FROM video_tags vt
JOIN tags t ON t.id = vt.tag_id
WHERE vt.video_id = ?
ORDER BY t.id ASC`, videoID)
	if err != nil {
		return err
	}
	var labels []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return err
		}
		labels = append(labels, label)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	labelsJSON, _ := json.Marshal(labels)
	manualValue := 0
	if manual {
		manualValue = 1
	}
	_, err = c.db.ExecContext(ctx,
		`UPDATE videos SET tags = ?, tags_manual = ?, updated_at = ? WHERE id = ?`,
		string(labelsJSON), manualValue, time.Now().UnixMilli(), videoID)
	return err
}

func (c *Catalog) hasManualTags(ctx context.Context, videoID string) bool {
	var manual int
	err := c.db.QueryRowContext(ctx, `SELECT COALESCE(tags_manual, 0) FROM videos WHERE id = ?`, videoID).Scan(&manual)
	return err == nil && manual == 1
}

func (c *Catalog) videoExists(ctx context.Context, videoID string) bool {
	var exists int
	err := c.db.QueryRowContext(ctx, `SELECT 1 FROM videos WHERE id = ?`, videoID).Scan(&exists)
	return err == nil
}

func (c *Catalog) tagExists(ctx context.Context, label string) bool {
	var exists int
	err := c.db.QueryRowContext(ctx, `SELECT 1 FROM tags WHERE label = ? COLLATE NOCASE`, label).Scan(&exists)
	return err == nil
}

func (c *Catalog) getTagByLabelTx(ctx context.Context, tx *sql.Tx, label string) (Tag, error) {
	return getTagByLabelTxRaw(ctx, tx, label)
}

func getTagByLabelTxRaw(ctx context.Context, tx *sql.Tx, label string) (Tag, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+tagSelectCols+` FROM tags WHERE label = ? COLLATE NOCASE`,
		label)
	return scanTag(row)
}

func (c *Catalog) getTagByIDTx(ctx context.Context, tx *sql.Tx, id int64) (Tag, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+tagSelectCols+` FROM tags WHERE id = ?`,
		id)
	return scanTag(row)
}

func hasManualTagsTx(ctx context.Context, tx *sql.Tx, videoID string) bool {
	var manual int
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(tags_manual, 0) FROM videos WHERE id = ?`, videoID).Scan(&manual)
	return err == nil && manual == 1
}

func syncVideoTagsJSONTx(ctx context.Context, tx *sql.Tx, videoID string, manual bool) error {
	rows, err := tx.QueryContext(ctx, `
SELECT t.label
FROM video_tags vt
JOIN tags t ON t.id = vt.tag_id
WHERE vt.video_id = ?
ORDER BY t.id ASC`, videoID)
	if err != nil {
		return err
	}
	var labels []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			rows.Close()
			return err
		}
		labels = append(labels, label)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	labelsJSON, _ := json.Marshal(labels)
	manualValue := 0
	if manual {
		manualValue = 1
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE videos SET tags = ?, tags_manual = ?, updated_at = ? WHERE id = ?`,
		string(labelsJSON), manualValue, time.Now().UnixMilli(), videoID)
	return err
}

type tagRowScanner interface {
	Scan(dest ...any) error
}

func scanTag(row tagRowScanner) (Tag, error) {
	var tag Tag
	var aliasesJSON, rulesJSON string
	if err := row.Scan(&tag.ID, &tag.Label, &aliasesJSON, &rulesJSON, &tag.Source, &tag.Count); err != nil {
		return Tag{}, err
	}
	_ = json.Unmarshal([]byte(aliasesJSON), &tag.Aliases)
	_ = json.Unmarshal([]byte(rulesJSON), &tag.MatchRules)
	return tag, nil
}
