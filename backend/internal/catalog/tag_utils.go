package catalog

import (
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
