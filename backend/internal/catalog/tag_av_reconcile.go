package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"

	"github.com/video-site/backend/internal/tagging"
)

type avReconcileTag struct {
	Tag
	origin string
}

type avReconcileVideo struct {
	id               string
	avEvidence       string
	series           string
	seriesEvidence   string
	ordinaryEvidence map[string]string
}

type avCurrentAssignment struct {
	source   string
	evidence string
}

// reconcileAVTagAssignments refreshes the AV umbrella and its derived series
// labels without evaluating or replacing unrelated tag rules. previousTag is
// used to include non-generated label collisions for prefixes that were just
// removed; those labels retain their own ordinary matching semantics.
func (c *Catalog) reconcileAVTagAssignments(ctx context.Context, previousTag, tag Tag) (int, error) {
	previousPrefixes := avPrefixLabelSet(previousTag)
	allowedPrefixes := avPrefixLabelSet(tag)
	avEnabled, err := c.avCodeMatchingEnabled(ctx)
	if err != nil {
		return 0, err
	}
	if !avEnabled {
		allowedPrefixes = map[string]struct{}{}
	}
	candidateLabels := make(map[string]struct{}, len(previousPrefixes)+len(allowedPrefixes))
	for label := range previousPrefixes {
		candidateLabels[label] = struct{}{}
	}
	for label := range allowedPrefixes {
		candidateLabels[label] = struct{}{}
	}

	knownTags, err := c.loadAVReconcileTags(ctx, tag.ID, candidateLabels)
	if err != nil {
		return 0, err
	}
	ordinaryMatcher, err := c.avCollisionMatcher(ctx, knownTags, candidateLabels)
	if err != nil {
		return 0, err
	}
	var activePrefixes []string
	if avEnabled {
		activePrefixes = effectiveRule(tag.Label, tag.Aliases, tag.MatchRules).AVCodePrefixes
	}
	codeMatcher := tagging.NewAVCodeMatcher(activePrefixes)

	videos, desiredSeries, err := c.scanAVReconcileVideos(ctx, codeMatcher, ordinaryMatcher)
	if err != nil {
		return 0, err
	}
	seriesLabels := make([]string, 0, len(desiredSeries))
	for label := range desiredSeries {
		seriesLabels = append(seriesLabels, label)
	}
	sort.Strings(seriesLabels)
	for _, label := range seriesLabels {
		if _, err := c.ensureAVSeriesTag(ctx, label); err != nil {
			return 0, err
		}
	}

	// Reload after ensuring newly discovered series labels so the reconciliation
	// uses their canonical IDs and origins.
	knownTags, err = c.loadAVReconcileTags(ctx, tag.ID, candidateLabels)
	if err != nil {
		return 0, err
	}
	tagsByLabel := make(map[string]avReconcileTag, len(knownTags))
	managedTagIDs := make(map[int64]struct{}, len(knownTags))
	for _, knownTag := range knownTags {
		labelKey := strings.ToLower(strings.TrimSpace(knownTag.Label))
		tagsByLabel[labelKey] = knownTag
		if knownTag.ID == tag.ID || strings.EqualFold(strings.TrimSpace(knownTag.origin), avSeriesOrigin) {
			managedTagIDs[knownTag.ID] = struct{}{}
			continue
		}
		if _, ok := candidateLabels[strings.ToLower(tagging.NormalizeAVCodePrefix(knownTag.Label))]; ok {
			managedTagIDs[knownTag.ID] = struct{}{}
		}
	}

	current, err := c.loadCurrentAVAssignments(ctx, managedTagIDs)
	if err != nil {
		return 0, err
	}
	mutations := make([]tagAssignmentMutation, 0)
	for _, video := range videos {
		var desired map[int64]string
		if len(video.ordinaryEvidence) > 0 || video.avEvidence != "" || video.series != "" {
			desired = make(map[int64]string, len(video.ordinaryEvidence)+2)
		}
		for label, evidence := range video.ordinaryEvidence {
			if ordinaryTag, ok := tagsByLabel[label]; ok {
				desired[ordinaryTag.ID] = evidence
			}
		}
		if video.avEvidence != "" {
			desired[tag.ID] = video.avEvidence
		}
		if video.series != "" {
			if seriesTag, ok := tagsByLabel[strings.ToLower(video.series)]; ok {
				// The ordinary tag matcher runs before AV-series derivation in the
				// full matcher, so its evidence wins when a user tag has the same
				// label as a series prefix.
				if _, exists := desired[seriesTag.ID]; !exists {
					desired[seriesTag.ID] = video.seriesEvidence
				}
			}
		}

		currentForVideo := current[video.id]
		for tagID, assignment := range currentForVideo {
			if _, ok := desired[tagID]; ok {
				continue
			}
			source := strings.ToLower(strings.TrimSpace(assignment.source))
			if source != "auto" && source != "legacy" {
				continue
			}
			mutations = append(mutations, tagAssignmentMutation{
				videoID:           video.id,
				tagID:             tagID,
				action:            tagAssignmentDelete,
				membershipChanged: true,
			})
		}
		for tagID, evidence := range desired {
			assignment, exists := currentForVideo[tagID]
			if !exists {
				mutations = append(mutations, tagAssignmentMutation{
					videoID:           video.id,
					tagID:             tagID,
					action:            tagAssignmentInsert,
					evidence:          evidence,
					membershipChanged: true,
				})
				continue
			}
			if !shouldReplaceVideoTagAssignment(assignment.source, "auto") {
				continue
			}
			if normalizeVideoTagSource(assignment.source) == "auto" && assignment.evidence == evidence {
				continue
			}
			mutations = append(mutations, tagAssignmentMutation{
				videoID:  video.id,
				tagID:    tagID,
				action:   tagAssignmentUpdate,
				evidence: evidence,
			})
		}
	}

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	changedCount, changedVideoIDs, err := applyTagAssignmentMutationsTx(ctx, tx, mutations)
	if err != nil {
		return 0, err
	}
	affectedVideos := make(map[string]struct{}, len(changedVideoIDs))
	for _, videoID := range changedVideoIDs {
		affectedVideos[videoID] = struct{}{}
	}
	cleanedVideos, removedAssignments, removedTags, err := cleanupAVSeriesTagsTx(ctx, tx, allowedPrefixes)
	if err != nil {
		return 0, err
	}
	changedCount += removedAssignments
	for _, videoID := range cleanedVideos {
		affectedVideos[videoID] = struct{}{}
	}
	if removedTags > 0 {
		if err := bumpTagRulesVersionTx(ctx, tx); err != nil {
			return 0, err
		}
	}

	videoIDs := make([]string, 0, len(affectedVideos))
	for videoID := range affectedVideos {
		videoIDs = append(videoIDs, videoID)
	}
	sort.Strings(videoIDs)
	for _, videoID := range videoIDs {
		if err := syncVideoTagsJSONTx(ctx, tx, videoID, hasManualTagsTx(ctx, tx, videoID)); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return changedCount, nil
}

func avPrefixLabelSet(tag Tag) map[string]struct{} {
	prefixes := effectiveRule(tag.Label, tag.Aliases, tag.MatchRules).AVCodePrefixes
	out := make(map[string]struct{}, len(prefixes))
	for _, prefix := range tagging.CleanAVCodePrefixes(prefixes) {
		out[strings.ToLower(prefix)] = struct{}{}
	}
	return out
}

func (c *Catalog) loadAVReconcileTags(ctx context.Context, avTagID int64, candidateLabels map[string]struct{}) ([]avReconcileTag, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT id,
       label,
       COALESCE(aliases, '[]'),
       COALESCE(match_rules, '{}'),
       COALESCE(source, ''),
       COALESCE(origin, '')
  FROM tags
 ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tags []avReconcileTag
	for rows.Next() {
		var tag avReconcileTag
		var aliasesJSON, rulesJSON string
		if err := rows.Scan(&tag.ID, &tag.Label, &aliasesJSON, &rulesJSON, &tag.Source, &tag.origin); err != nil {
			return nil, err
		}
		labelKey := strings.ToLower(tagging.NormalizeAVCodePrefix(tag.Label))
		isSeries := strings.EqualFold(strings.TrimSpace(tag.origin), avSeriesOrigin)
		if tag.ID != avTagID && !isSeries {
			if _, ok := candidateLabels[labelKey]; !ok {
				continue
			}
		}
		_ = json.Unmarshal([]byte(aliasesJSON), &tag.Aliases)
		_ = json.Unmarshal([]byte(rulesJSON), &tag.MatchRules)
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}

func (c *Catalog) avCollisionMatcher(ctx context.Context, tags []avReconcileTag, candidateLabels map[string]struct{}) (*tagging.Matcher, error) {
	builtinEnabled, err := c.BuiltinTagsEnabled(ctx)
	if err != nil {
		return nil, err
	}
	rules := make([]tagging.TagRule, 0)
	for _, tag := range tags {
		if strings.EqualFold(tag.Label, avTagLabel) || strings.EqualFold(strings.TrimSpace(tag.origin), avSeriesOrigin) {
			continue
		}
		if _, ok := candidateLabels[strings.ToLower(tagging.NormalizeAVCodePrefix(tag.Label))]; !ok {
			continue
		}
		if !builtinEnabled && normalizeTagSource(tag.Source) == "builtin" {
			continue
		}
		rules = append(rules, tagging.TagRule{
			Label: tag.Label,
			Rule:  effectiveRule(tag.Label, tag.Aliases, tag.MatchRules),
		})
	}
	return tagging.NewMatcher(rules), nil
}

func (c *Catalog) scanAVReconcileVideos(ctx context.Context, codeMatcher *tagging.AVCodeMatcher, ordinaryMatcher *tagging.Matcher) ([]avReconcileVideo, map[string]struct{}, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT id,
       title,
       COALESCE(author, ''),
       COALESCE(file_name, ''),
       COALESCE(dir_name, '')
  FROM videos
 WHERE COALESCE(tags_manual, 0) = 0
 ORDER BY id ASC`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var videos []avReconcileVideo
	desiredSeries := make(map[string]struct{})
	for rows.Next() {
		var video avReconcileVideo
		var title, author, fileName, dirName string
		if err := rows.Scan(&video.id, &title, &author, &fileName, &dirName); err != nil {
			return nil, nil, err
		}
		fields := matchFields(title, fileName, author, dirName)
		for _, field := range fields {
			if strings.TrimSpace(field.Text) == "" {
				continue
			}
			code := codeMatcher.Find(field.Text)
			if code == "" {
				continue
			}
			video.avEvidence = code
			if field.Name != "" {
				video.avEvidence = field.Name + ":" + code
			}
			video.series = codeMatcher.SeriesOf(code)
			video.seriesEvidence = video.avEvidence
			if video.series != "" {
				desiredSeries[video.series] = struct{}{}
			}
			break
		}
		for _, match := range ordinaryMatcher.Match(fields...) {
			if video.ordinaryEvidence == nil {
				video.ordinaryEvidence = make(map[string]string)
			}
			video.ordinaryEvidence[strings.ToLower(strings.TrimSpace(match.Label))] = match.Evidence()
		}
		videos = append(videos, video)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return videos, desiredSeries, nil
}

func (c *Catalog) loadCurrentAVAssignments(ctx context.Context, managedTagIDs map[int64]struct{}) (map[string]map[int64]avCurrentAssignment, error) {
	out := make(map[string]map[int64]avCurrentAssignment)
	if len(managedTagIDs) == 0 {
		return out, nil
	}
	ids := make([]int64, 0, len(managedTagIDs))
	for tagID := range managedTagIDs {
		ids = append(ids, tagID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids))
	for _, tagID := range ids {
		args = append(args, tagID)
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT vt.video_id,
       vt.tag_id,
       COALESCE(vt.source, ''),
       COALESCE(vt.evidence, '')
  FROM video_tags vt
  JOIN videos v ON v.id = vt.video_id
 WHERE COALESCE(v.tags_manual, 0) = 0
   AND vt.tag_id IN (`+placeholders+`)
 ORDER BY vt.video_id, vt.tag_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var videoID string
		var tagID int64
		var assignment avCurrentAssignment
		if err := rows.Scan(&videoID, &tagID, &assignment.source, &assignment.evidence); err != nil {
			return nil, err
		}
		if out[videoID] == nil {
			out[videoID] = make(map[int64]avCurrentAssignment)
		}
		out[videoID][tagID] = assignment
	}
	return out, rows.Err()
}

// cleanupAVSeriesTagsTx removes generated AV-series definitions that are no
// longer valid for the edited prefix set, plus valid definitions with no
// references. Invalid definitions are removed even if a historical higher-
// priority assignment still references them, matching the existing AV cleanup
// semantics.
func cleanupAVSeriesTagsTx(ctx context.Context, tx *sql.Tx, allowedPrefixes map[string]struct{}) ([]string, int, int, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT t.id, t.label, COUNT(vt.video_id)
  FROM tags t
  LEFT JOIN video_tags vt ON vt.tag_id = t.id
 WHERE lower(trim(COALESCE(t.source, ''))) = 'generated'
   AND lower(trim(COALESCE(t.origin, ''))) = ?
 GROUP BY t.id, t.label
 ORDER BY t.id ASC`, avSeriesOrigin)
	if err != nil {
		return nil, 0, 0, err
	}
	var tagIDs []int64
	for rows.Next() {
		var tagID int64
		var label string
		var references int
		if err := rows.Scan(&tagID, &label, &references); err != nil {
			rows.Close()
			return nil, 0, 0, err
		}
		_, allowed := allowedPrefixes[strings.ToLower(tagging.NormalizeAVCodePrefix(label))]
		if !allowed || references == 0 {
			tagIDs = append(tagIDs, tagID)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, 0, err
	}
	if err := rows.Close(); err != nil {
		return nil, 0, 0, err
	}
	if len(tagIDs) == 0 {
		return nil, 0, 0, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(tagIDs)), ",")
	args := make([]any, 0, len(tagIDs))
	for _, tagID := range tagIDs {
		args = append(args, tagID)
	}
	affectedRows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT video_id FROM video_tags WHERE tag_id IN (`+placeholders+`) ORDER BY video_id`, args...)
	if err != nil {
		return nil, 0, 0, err
	}
	var videoIDs []string
	for affectedRows.Next() {
		var videoID string
		if err := affectedRows.Scan(&videoID); err != nil {
			affectedRows.Close()
			return nil, 0, 0, err
		}
		videoIDs = append(videoIDs, videoID)
	}
	if err := affectedRows.Err(); err != nil {
		affectedRows.Close()
		return nil, 0, 0, err
	}
	if err := affectedRows.Close(); err != nil {
		return nil, 0, 0, err
	}

	result, err := tx.ExecContext(ctx, `DELETE FROM video_tags WHERE tag_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, 0, 0, err
	}
	removedAssignments64, err := result.RowsAffected()
	if err != nil {
		return nil, 0, 0, err
	}
	result, err = tx.ExecContext(ctx, `DELETE FROM tags WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, 0, 0, err
	}
	removedTags64, err := result.RowsAffected()
	if err != nil {
		return nil, 0, 0, err
	}
	return videoIDs, int(removedAssignments64), int(removedTags64), nil
}
