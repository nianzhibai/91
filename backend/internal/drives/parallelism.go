package drives

import "strings"

// DefaultGenerationParallelism is how many videos a source of this kind
// generates at once unless an administrator sets its own number. A source on
// this machine's disk costs only decoding, so it is bounded by the
// application-wide limit alone (zero). A provider gets one at a time, which
// keeps reads under its throttling.
func DefaultGenerationParallelism(kind string) int {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "local-upload", "telegram-local", "localstorage", "scriptcrawler":
		return 0
	}
	return 1
}
