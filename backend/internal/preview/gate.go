package preview

import "sync"

// Gate is the process-wide preview switch. Media jobs consult it to decide
// whether they produce teasers; signatures and thumbnails are generated
// either way. The zero value is enabled; a nil Gate is always enabled.
type Gate struct {
	mu       sync.Mutex
	disabled bool
}

func (g *Gate) Enabled() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.disabled
}

// Set reports whether the state changed.
func (g *Gate) Set(enabled bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.disabled != enabled {
		return false
	}
	g.disabled = !enabled
	return true
}
