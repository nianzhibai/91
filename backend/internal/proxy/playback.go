package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

type cachedPlayback struct {
	sources []drives.PlaybackSource
	until   time.Time
}

type playbackCall struct {
	done    chan struct{}
	once    sync.Once
	sources []drives.PlaybackSource
	err     error
}

// PlaybackSources resolves optional cloud metadata only. A nil result uses the
// existing original-file playback route. Refresh bypasses cache, not inflight
// work, so simultaneous user retries still share one provider request.
func (p *Proxy) PlaybackSources(ctx context.Context, driveID, fileID, userAgent string, refresh bool) ([]drives.PlaybackSource, error) {
	return p.resolvePlayback(ctx, driveID, fileID, userAgent, refresh, false)
}

// PreparePlaybackSources coalesces explicit preparation separately from read-only
// lookups. Refreshing the metadata cache never resets provider submission guards.
func (p *Proxy) PreparePlaybackSources(ctx context.Context, driveID, fileID string) ([]drives.PlaybackSource, error) {
	return p.resolvePlayback(ctx, driveID, fileID, "", true, true)
}

func (p *Proxy) resolvePlayback(ctx context.Context, driveID, fileID, userAgent string, refresh, prepare bool) ([]drives.PlaybackSource, error) {
	d, ok := p.Registry.Get(driveID)
	if !ok {
		if failure, found := p.driveInitError(driveID); found {
			return nil, failure.err
		}
		return nil, errDriveNotFound
	}
	var resolve func(context.Context, string) ([]drives.PlaybackSource, error)
	key := driveID + "/" + fileID
	switch provider := d.(type) {
	case drives.UserAgentPlaybackSourceProvider:
		// Bound persistent keys independently of the request header's length.
		key += fmt.Sprintf("|ua=%x", sha256.Sum256([]byte(userAgent)))
		resolve = func(ctx context.Context, fileID string) ([]drives.PlaybackSource, error) {
			return provider.PlaybackSourcesWithUserAgent(ctx, fileID, userAgent)
		}
	case drives.PlaybackSourceProvider:
		resolve = provider.PlaybackSources
	default:
		return nil, nil
	}
	if prepare {
		preparer, ok := d.(drives.PlaybackSourcePreparer)
		if !ok {
			return nil, fmt.Errorf("drive does not support playback preparation")
		}
		resolve = preparer.PreparePlayback
	}
	p.cacheMu.Lock()
	calls := p.playbackInflight
	if prepare {
		calls = p.playbackPreparing
		// Older read-only lookups may finish after submission. Their results
		// must not replace the post-submission state in the shared query cache.
		delete(p.playbackInflight, key)
	}
	if c, found := p.playbackCache[key]; found {
		if !refresh && time.Now().Before(c.until) {
			p.cacheMu.Unlock()
			return c.sources, nil
		}
		delete(p.playbackCache, key)
	}
	call := calls[key]
	if call == nil {
		call = &playbackCall{done: make(chan struct{})}
		calls[key] = call
		deadline := p.resolveTimeout
		if deadline <= 0 {
			deadline = linkResolveTimeout
		}
		resolveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deadline)
		finish := func(sources []drives.PlaybackSource, err error) {
			call.once.Do(func() {
				p.cacheMu.Lock()
				defer p.cacheMu.Unlock()
				call.sources, call.err = sources, err
				if calls[key] == call {
					delete(calls, key)
					if prepare {
						delete(p.playbackInflight, key)
					}
					if err == nil && sources != nil {
						now := time.Now()
						ttl := 2 * time.Minute
						if !hasReadyPlaybackSource(sources) || hasProcessingPlaybackSource(sources) {
							ttl = 10 * time.Second
						}
						until := now.Add(ttl)
						for _, source := range sources {
							link := source.Link
							if link != nil && !link.Expires.IsZero() && link.Expires.Before(until) {
								until = link.Expires
							}
						}
						for k, c := range p.playbackCache {
							if now.After(c.until) {
								delete(p.playbackCache, k)
							}
						}
						if len(p.playbackCache) >= linkCacheMaxEntries {
							for k := range p.playbackCache {
								delete(p.playbackCache, k)
								break
							}
						}
						p.playbackCache[key] = cachedPlayback{sources: sources, until: until}
					}
				}
				close(call.done)
			})
		}
		go func() {
			defer cancel()
			sources, err := resolve(resolveCtx, fileID)
			if err == nil && sources == nil {
				err = fmt.Errorf("provider returned nil playback sources")
			}
			if resolveCtx.Err() != nil {
				err = resolveCtx.Err()
			}
			finish(sources, err)
		}()
		go func() { <-resolveCtx.Done(); finish(nil, resolveCtx.Err()) }()
	}
	p.cacheMu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-call.done:
		return call.sources, call.err
	}
}

func (p *Proxy) WritePlaybackError(w http.ResponseWriter, driveID string, err error) {
	kind := ""
	if d, ok := p.Registry.Get(driveID); ok {
		kind = d.Kind()
	} else if failure, ok := p.driveInitError(driveID); ok {
		kind = failure.kind
	}
	// Optional preview failures do not determine original-file drive health.
	writeStreamError(w, kind, err)
}

func hasReadyPlaybackSource(sources []drives.PlaybackSource) bool {
	for _, source := range sources {
		if source.Status == "ready" && source.Link != nil {
			return true
		}
	}
	return false
}

func hasProcessingPlaybackSource(sources []drives.PlaybackSource) bool {
	for _, source := range sources {
		if source.Status == "processing" {
			return true
		}
	}
	return false
}

// InvalidateStreamLink refreshes only this viewer's original-file link. Cloud
// links and other files remain cached; concurrent resolutions remain shared.
func (p *Proxy) InvalidateStreamLink(driveID, fileID string, header http.Header) {
	d, ok := p.Registry.Get(driveID)
	if !ok {
		return
	}
	p.cacheMu.Lock()
	delete(p.cache, linkCacheKey(d, driveID, fileID, header))
	p.cacheMu.Unlock()
}

// Provider playback prewarming belongs to the active/next frontend element,
// rather than a feed response that can contain many never-played videos.
func (p *Proxy) HasPlaybackProvider(driveID string) bool {
	d, ok := p.Registry.Get(driveID)
	if !ok {
		return false
	}
	switch d.(type) {
	case drives.PlaybackSourceProvider, drives.UserAgentPlaybackSourceProvider:
		return true
	default:
		return false
	}
}

func (p *Proxy) HasPlaybackPreparer(driveID string) bool {
	d, ok := p.Registry.Get(driveID)
	if !ok {
		return false
	}
	_, ok = d.(drives.PlaybackSourcePreparer)
	return ok
}

func allowedPlaybackURL(root, target *url.URL, hosts []string) bool {
	if root == nil || target == nil || target.Host == "" || target.User != nil || target.Fragment != "" ||
		(target.Scheme != "https" && !(root.Scheme == "http" && target.Scheme == "http")) {
		return false
	}
	for _, host := range hosts {
		host = strings.ToLower(host)
		if strings.HasPrefix(host, ".") {
			if target.Port() != "" && target.Port() != "443" {
				continue
			}
			name := strings.ToLower(target.Hostname())
			if name == host[1:] || strings.HasSuffix(name, host) {
				return true
			}
		} else if strings.EqualFold(target.Host, host) {
			return true
		}
	}
	return false
}

func writePlaybackSourceExpired(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusGone)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": "playback_source_expired", "message": "兼容播放地址已过期，请重新获取"})
}
