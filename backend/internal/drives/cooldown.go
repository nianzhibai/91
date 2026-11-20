package drives

import (
	"net/http"
	"strings"
	"time"
)

// This file is the provider throttling policy shared by every worker that
// reads a drive's media. Errors carrying a RateLimitError are always throttling;
// the statuses below are how each provider reports throttling or a brief
// outage without one.

// DefaultCooldown is how long a drive's provider work pauses when a throttling
// error does not say when to retry.
const DefaultCooldown = 5 * time.Minute

var cooldownStatuses = map[string][]int{
	"p115":        {http.StatusMethodNotAllowed, http.StatusTooManyRequests},
	"pikpak":      {http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 509},
	"p123":        {http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout},
	"wopan":       {http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 509},
	"guangyapan":  {http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 509},
	"googledrive": {http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout},
}

// ShouldCooldown reports whether an error status from a drive of this kind
// means the provider is throttling or briefly unavailable, so its work should
// pause and retry rather than fail the item.
func ShouldCooldown(kind string, err error) bool {
	if err == nil {
		return false
	}
	return ErrorMentionsHTTPStatus(err, cooldownStatuses[strings.ToLower(kind)]...)
}

// CooldownFor is the pause for a drive of this kind when a throttling error
// does not say when to retry.
func CooldownFor(kind string) time.Duration {
	switch strings.ToLower(kind) {
	case "p115", "p123", "onedrive", "wopan", "guangyapan":
		return 10 * time.Minute
	}
	return DefaultCooldown
}
