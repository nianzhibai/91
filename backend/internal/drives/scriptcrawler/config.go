package scriptcrawler

import "strings"

// IsConfigured reports whether catalog credentials describe an active crawler.
// A missing script path is the durable marker left by the legacy delete flow;
// such rows must not own runtime workers or participate in upload migration.
func IsConfigured(credentials map[string]string) bool {
	return strings.TrimSpace(credentials["script_path"]) != ""
}
