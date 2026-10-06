package catalog

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/video-site/backend/internal/tagging"
)

// IsAVCode / ContainsAVCode 委托给 tagging 包（历史实现已迁移）。
func IsAVCode(label string) bool {
	return tagging.IsAVCode(cleanTagLabel(label))
}

func ContainsAVCode(text string) bool {
	return tagging.ContainsAVCode(text)
}

func isAVCodePollutedLabel(label string) bool {
	label = cleanTagLabel(label)
	if label == "" {
		return false
	}
	return tagging.IsAVCode(label) || tagging.ContainsAVCode(label)
}

func cleanLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		label = cleanTagLabel(label)
		if label != "" {
			if isAVCodePollutedLabel(label) {
				label = avTagLabel
			}
			out = append(out, label)
		}
	}
	return out
}

func cleanTagLabel(label string) string {
	return strings.TrimSpace(label)
}

func cleanTagRule(rule tagging.Rule) tagging.Rule {
	return tagging.Rule{
		Keywords: cleanRuleTerms(rule.Keywords),
	}
}

func cleanStoredTagRule(rule tagging.Rule) tagging.Rule {
	return tagging.Rule{
		Keywords:       cleanRuleTerms(rule.Keywords),
		MatchAVCode:    rule.MatchAVCode,
		AVCodePrefixes: tagging.CleanAVCodePrefixes(rule.AVCodePrefixes),
	}
}

func cleanRuleTerms(terms []string) []string {
	out := make([]string, 0, len(terms))
	seen := map[string]struct{}{}
	for _, term := range terms {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		key := strings.ToLower(term)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, term)
	}
	return out
}

// normalizeTagSource 只用于 tags.source。video_tags.source 是标签挂载来源，
// 使用 auto/manual/crawler/telegram/legacy 等独立来源。
func normalizeTagSource(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "system", "builtin", "user":
		return "user"
	default:
		return "generated"
	}
}

func parseSettingBool(value string, defaultValue bool) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y", "on", "enabled":
		return true
	case "0", "false", "no", "n", "off", "disabled":
		return false
	default:
		return defaultValue
	}
}

func uniqueStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		key := strings.ToLower(value)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}

// pruneOrphanGeneratedTagsByID 在事务里检查并删除不再被引用的自动生成标签。
func pruneOrphanGeneratedTagsByID(ctx context.Context, tx *sql.Tx, tagIDs []int64) error {
	for _, tagID := range tagIDs {
		var src string
		err := tx.QueryRowContext(ctx, `SELECT source FROM tags WHERE id = ?`, tagID).Scan(&src)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if normalizeTagSource(src) != "generated" {
			continue
		}
		var refCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM video_tags WHERE tag_id = ?`, tagID).Scan(&refCount); err != nil {
			return err
		}
		if refCount > 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tags WHERE id = ?`, tagID); err != nil {
			return err
		}
	}
	return nil
}

// collectVideoTagIDs 在事务里读出当前视频关联的 tag_id，供后续清理判断。
func collectVideoTagIDs(ctx context.Context, tx *sql.Tx, videoID string) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT tag_id FROM video_tags WHERE video_id = ?`, videoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
