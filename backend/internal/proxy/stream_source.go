package proxy

import (
	"time"

	"github.com/google/uuid"
	"github.com/video-site/backend/internal/drives"
)

const streamObservationTTL = 30 * time.Minute

type streamObservationKey struct{ driveID, fileID, playbackID string }
type observedURLSource struct {
	source string
	used   time.Time
}

// HasOriginalPlaybackProvider lets the API attach optional source information
// without resolving a link or contacting the provider just to render a menu.
func (p *Proxy) HasOriginalPlaybackProvider(driveID string) bool {
	d, ok := p.Registry.Get(driveID)
	if !ok {
		return false
	}
	_, ok = d.(drives.OriginalPlaybackProvider)
	return ok
}

// observeStreamURLSource records the link chosen by the actual media request,
// including cache hits. A per-playback ID keeps simultaneous viewers and old
// buffered streams independent from later URL resolutions for the same file.
func (p *Proxy) observeStreamURLSource(driveID, fileID, playbackID, source string) {
	if len(playbackID) != 36 {
		return
	}
	if _, err := uuid.Parse(playbackID); err != nil {
		return
	}
	now := time.Now()
	key := streamObservationKey{driveID, fileID, playbackID}
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	for k, value := range p.observedURLSources {
		if now.Sub(value.used) >= streamObservationTTL {
			delete(p.observedURLSources, k)
		}
	}
	if _, exists := p.observedURLSources[key]; !exists && len(p.observedURLSources) >= linkCacheMaxEntries {
		var oldest streamObservationKey
		var used time.Time
		for k, value := range p.observedURLSources {
			if used.IsZero() || value.used.Before(used) {
				oldest = k
				used = value.used
			}
		}
		delete(p.observedURLSources, oldest)
	}
	p.observedURLSources[key] = observedURLSource{source: source, used: now}
}

// ObservedStreamURLSource is read-only: UI information requests never resolve a
// new link, fetch video bytes, or change which interface a viewer is using.
func (p *Proxy) ObservedStreamURLSource(driveID, fileID, playbackID string) string {
	key := streamObservationKey{driveID, fileID, playbackID}
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	if value, ok := p.observedURLSources[key]; ok {
		if time.Since(value.used) < streamObservationTTL {
			return value.source
		}
		delete(p.observedURLSources, key)
	}
	return ""
}
