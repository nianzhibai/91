// Package generationsource caches provider inputs independently of online playback.
package generationsource

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

const maxEntries = 512

type entry struct {
	source  *drives.GenerationSource
	err     error
	expires time.Time
}
type call struct {
	done    chan struct{}
	refresh bool
	entry
}

// Cache is scoped to one mounted drive. Its zero value is ready to use.
// Only unavailable sources are negatively cached; authentication, cancellation
// and rate-limit errors are always returned to the worker.
type Cache struct {
	mu       sync.Mutex
	entries  map[string]entry
	inflight map[string]*call
}

func (c *Cache) Resolve(ctx context.Context, fileID string, refresh bool, resolve func(context.Context, string) (*drives.GenerationSource, error)) (*drives.GenerationSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]entry)
		c.inflight = make(map[string]*call)
	}
	now := time.Now()
	for key, value := range c.entries {
		if !now.Before(value.expires) {
			delete(c.entries, key)
		}
	}
	if refresh {
		delete(c.entries, fileID)
	}
	if value, ok := c.entries[fileID]; ok {
		c.mu.Unlock()
		return clone(value.source), value.err
	}
	if pending := c.inflight[fileID]; pending != nil && (!refresh || pending.refresh) {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-pending.done:
			return clone(pending.source), pending.err
		}
	}
	pending := &call{done: make(chan struct{}), refresh: refresh}
	c.inflight[fileID] = pending
	c.mu.Unlock()

	source, err := resolve(ctx, fileID)
	if ctx.Err() != nil {
		source, err = nil, ctx.Err()
	}
	if err == nil && (source == nil || source.Link == nil || strings.TrimSpace(source.Link.URL) == "") {
		source, err = nil, drives.ErrGenerationStreamUnavailable
	}
	value := entry{source: clone(source), err: err}
	now = time.Now()
	if err == nil {
		value.expires = now.Add(2 * time.Minute)
		if deadline := source.Link.Expires; !deadline.IsZero() && deadline.Before(value.expires) {
			value.expires = deadline
		}
	} else if errors.Is(err, drives.ErrGenerationStreamUnavailable) {
		value.source = nil
		value.expires = now.Add(30 * time.Second)
	}
	c.mu.Lock()
	if c.inflight[fileID] == pending {
		delete(c.inflight, fileID)
		if now.Before(value.expires) {
			if len(c.entries) >= maxEntries {
				var oldest string
				var expiry time.Time
				for key, e := range c.entries {
					if expiry.IsZero() || e.expires.Before(expiry) {
						oldest, expiry = key, e.expires
					}
				}
				delete(c.entries, oldest)
			}
			c.entries[fileID] = value
		}
	}
	pending.entry = value
	close(pending.done)
	c.mu.Unlock()
	return clone(value.source), value.err
}

func clone(source *drives.GenerationSource) *drives.GenerationSource {
	if source == nil {
		return nil
	}
	result := *source
	result.RefreshStatuses = append([]int(nil), source.RefreshStatuses...)
	if source.Link != nil {
		link := *source.Link
		link.Headers = link.Headers.Clone()
		result.Link = &link
	}
	if source.Relay != nil {
		relay := *source.Relay
		relay.AllowedHosts = append([]string(nil), relay.AllowedHosts...)
		result.Relay = &relay
	}
	return &result
}

// ReadyHLS selects the first ready rendition in the provider's preference order.
// Pending renditions never start polling or provider-side preparation here.
func ReadyHLS(sources []drives.PlaybackSource) (*drives.GenerationSource, error) {
	for _, source := range sources {
		if source.Protocol != "hls" || source.Status != "ready" || source.Link == nil || strings.TrimSpace(source.Link.URL) == "" {
			continue
		}
		if !source.Link.Expires.IsZero() && !time.Now().Before(source.Link.Expires) {
			continue
		}
		relay := source.Relay
		if relay == nil {
			relay = &drives.HLSRelay{}
		}
		return clone(&drives.GenerationSource{Protocol: "hls", Link: source.Link, Relay: relay, RefreshStatuses: []int{403}}), nil
	}
	return nil, drives.ErrGenerationStreamUnavailable
}
