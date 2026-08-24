package mediasim

import "strings"

// TitlePrefixBuckets builds the low-cost prefix index shared by title-based
// clustering and near-duplicate candidate generation.
func TitlePrefixBuckets(keys []string, prefixRunes int) []string {
	if prefixRunes <= 0 {
		prefixRunes = 12
	}
	seen := make(map[string]struct{})
	var out []string
	var fallback []string
	for _, key := range keys {
		runes := []rune(key)
		if len(runes) == 0 {
			continue
		}
		if len(runes) > prefixRunes {
			runes = runes[:prefixRunes]
		}
		bucket := string(runes)
		if _, ok := seen[bucket]; ok {
			continue
		}
		seen[bucket] = struct{}{}
		if lowInformationTitleBucket(bucket) {
			fallback = append(fallback, bucket)
			continue
		}
		out = append(out, bucket)
	}
	if len(out) > 0 {
		return out
	}
	return fallback
}

func lowInformationTitleBucket(bucket string) bool {
	if strings.HasPrefix(bucket, "www") {
		return true
	}
	if strings.Contains(bucket, "com") {
		limit := len(bucket)
		if limit > 8 {
			limit = 8
		}
		for _, r := range bucket[:limit] {
			if r >= '0' && r <= '9' {
				return true
			}
		}
	}
	return false
}

// TitleLengthCouldReachThreshold rejects pairs whose normalized title lengths
// cannot possibly reach the requested similarity score.
func TitleLengthCouldReachThreshold(leftKeys, rightKeys []string, threshold float64) bool {
	for _, left := range leftKeys {
		leftLen := len([]rune(left))
		if leftLen == 0 {
			continue
		}
		for _, right := range rightKeys {
			rightLen := len([]rune(right))
			if rightLen == 0 {
				continue
			}
			maxLen := leftLen
			minLen := rightLen
			if rightLen > maxLen {
				maxLen = rightLen
				minLen = leftLen
			}
			if float64(minLen)/float64(maxLen) >= threshold {
				return true
			}
		}
	}
	return false
}

func TitleQGrams(keys []string, n int) map[string]struct{} {
	out := make(map[string]struct{})
	if n <= 0 {
		n = 4
	}
	for _, key := range keys {
		runes := []rune(key)
		if len(runes) == 0 {
			continue
		}
		if len(runes) <= n {
			out[string(runes)] = struct{}{}
			continue
		}
		for i := 0; i+n <= len(runes); i++ {
			out[string(runes[i:i+n])] = struct{}{}
		}
	}
	return out
}

func QGramContainment(left, right map[string]struct{}) float64 {
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	smaller := left
	larger := right
	if len(right) < len(left) {
		smaller = right
		larger = left
	}
	common := 0
	for gram := range smaller {
		if _, ok := larger[gram]; ok {
			common++
		}
	}
	return float64(common) / float64(len(smaller))
}
