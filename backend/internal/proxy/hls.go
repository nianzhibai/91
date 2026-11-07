package proxy

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/hlsplaylist"
	"github.com/nianzhibai/91/backend/internal/hlsrelay"
)

const (
	hlsMaxPlaylistBytes = 1 << 20
	hlsMaxSessions      = 256
	hlsMaxResources     = 64 * 1024
	// Expire idle namespaces, not an actively playing movie. StreamLink.Expires
	// is a conservative resolution-cache deadline, not a media signature expiry.
	hlsSessionTTL = 30 * time.Minute
)

type hlsSession struct {
	driveID, fileID string
	headers         http.Header
	policy          drives.HLSRelay
	expires, used   time.Time
	resources       map[string]*url.URL // protected by Proxy.hlsMu
	failureStatus   int                 // last source rejection, protected by Proxy.hlsMu
	retryAfter      string
}

// StartHLSPlayback creates a bounded, opaque resource namespace. The API must
// authorize the video again for every request before calling ServeHLS.
func (p *Proxy) StartHLSPlayback(driveID, fileID string, source drives.PlaybackSource) (string, time.Time, error) {
	if source.Protocol != "hls" || source.Link == nil || source.Relay == nil || len(source.Relay.Playlist) > hlsMaxPlaylistBytes {
		return "", time.Time{}, errors.New("invalid HLS playback source")
	}
	policy := *source.Relay
	policy.AllowedHosts = append([]string(nil), policy.AllowedHosts...)
	root, err := url.Parse(source.Link.URL)
	if err != nil || !allowedPlaybackURL(root, root, policy.AllowedHosts) {
		return "", time.Time{}, errors.New("HLS source host is not allowed")
	}
	// Static account headers are forbidden. A provider's server-only Authorize
	// callback may supply current credentials after the destination is validated.
	for name := range source.Link.Headers {
		switch strings.ToLower(name) {
		case "cookie", "authorization", "proxy-authorization":
			return "", time.Time{}, errors.New("HLS source contains account headers")
		}
	}
	now := time.Now()
	expires := now.Add(hlsSessionTTL)
	if deadline := source.Link.Expires; !deadline.IsZero() && !deadline.After(now) {
		return "", time.Time{}, errors.New("HLS source has expired")
	}
	if policy.Playlist != "" {
		// Validate all initial references before advertising a ready source.
		if _, err := hlsplaylist.Rewrite(policy.Playlist, func(raw string) (string, error) {
			_, err := resolveHLSResource(root, root, raw, policy.AllowedHosts)
			return "resource", err
		}); err != nil {
			return "", time.Time{}, err
		}
	}
	id := uuid.NewString()
	p.hlsMu.Lock()
	defer p.hlsMu.Unlock()
	p.pruneHLSSessionsLocked(now)
	for len(p.hlsSessions) >= hlsMaxSessions || p.hlsResources >= hlsMaxResources {
		p.evictHLSSessionLocked("")
	}
	p.hlsSessions[id] = &hlsSession{
		driveID: driveID, fileID: fileID, headers: source.Link.Headers.Clone(), policy: policy,
		expires: expires, used: now, resources: map[string]*url.URL{"index.m3u8": root},
	}
	p.hlsResources++
	return id, expires, nil
}

// ServeHLS serves only URLs discovered in this video's provider playlists.
// Browser query parameters and credentials are never forwarded upstream.
func (p *Proxy) ServeHLS(w http.ResponseWriter, r *http.Request, driveID, fileID, sessionID, resourceID string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	p.hlsMu.Lock()
	session := p.hlsSessions[sessionID]
	if session == nil {
		p.hlsMu.Unlock()
		writePlaybackSourceExpired(w)
		return
	}
	if session.driveID != driveID || session.fileID != fileID {
		p.hlsMu.Unlock()
		http.NotFound(w, r)
		return
	}
	now := time.Now()
	if !now.Before(session.expires) {
		p.removeHLSSessionLocked(sessionID)
		p.hlsMu.Unlock()
		writePlaybackSourceExpired(w)
		return
	}
	target := session.resources[resourceID]
	root := session.resources["index.m3u8"]
	failureStatus, retryAfter := session.failureStatus, session.retryAfter
	session.used = now
	session.expires = now.Add(hlsSessionTTL)
	p.hlsMu.Unlock()
	if target == nil {
		http.NotFound(w, r)
		return
	}
	if failureStatus != 0 && (failureStatus == http.StatusTooManyRequests || strings.Contains(r.Header.Get("Accept"), "application/json")) {
		writeHLSUpstreamFailure(w, failureStatus, retryAfter)
		return
	}
	if resourceID == "index.m3u8" && session.policy.Playlist != "" {
		p.serveHLSPlaylist(w, r, sessionID, session, root, session.policy.Playlist)
		return
	}
	resp, err := hlsrelay.Fetch(p.http, r, root, target, session.headers, session.policy)
	if err != nil {
		hlsRequestFailed(w)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		status := resp.StatusCode
		retryAfter := resp.Header.Get("Retry-After")
		switch status {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone, http.StatusTooManyRequests:
			// Native HLS hides the failed segment URL. Retain source rejections
			// for master diagnostics without blocking normal rendition switching.
			// Rate limits stop all requests within this session.
			p.hlsMu.Lock()
			if session.failureStatus == 0 || status == http.StatusTooManyRequests {
				session.failureStatus, session.retryAfter = status, retryAfter
			}
			status, retryAfter = session.failureStatus, session.retryAfter
			p.hlsMu.Unlock()
		}
		writeHLSUpstreamFailure(w, status, retryAfter)
		return
	}
	if r.Method == http.MethodHead {
		copyHLSMediaHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		return
	}
	body := bufio.NewReader(resp.Body)
	prefix, _ := body.Peek(len("#EXTM3U"))
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if bytes.Equal(prefix, []byte("#EXTM3U")) || strings.Contains(contentType, "mpegurl") {
		data, err := io.ReadAll(io.LimitReader(body, hlsMaxPlaylistBytes+1))
		if err != nil || len(data) > hlsMaxPlaylistBytes {
			hlsRequestFailed(w)
			return
		}
		p.serveHLSPlaylist(w, r, sessionID, session, resp.Request.URL, string(data))
		return
	}
	copyHLSMediaHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, body)
}

func writeHLSUpstreamFailure(w http.ResponseWriter, status int, retryAfter string) {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		// Site access was checked before ServeHLS. An upstream rejection only
		// invalidates this signed source, just like progressive playback.
		writePlaybackSourceExpired(w)
	default:
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		w.WriteHeader(status)
	}
}

func (p *Proxy) serveHLSPlaylist(w http.ResponseWriter, r *http.Request, id string, session *hlsSession, base *url.URL, playlist string) {
	basePath := r.URL.EscapedPath()
	basePath = basePath[:strings.LastIndex(basePath, "/")+1]
	p.hlsMu.Lock()
	if p.hlsSessions[id] != session {
		p.hlsMu.Unlock()
		writePlaybackSourceExpired(w)
		return
	}
	root := session.resources["index.m3u8"]
	local, err := hlsplaylist.Rewrite(playlist, func(raw string) (string, error) {
		target, err := resolveHLSResource(root, base, raw, session.policy.AllowedHosts)
		if err != nil {
			return "", err
		}
		digest := sha256.Sum256([]byte(target.String()))
		resourceID := hex.EncodeToString(digest[:16])
		if session.resources[resourceID] == nil {
			for p.hlsResources >= hlsMaxResources {
				if !p.evictHLSSessionLocked(id) {
					return "", errors.New("HLS resource limit reached")
				}
			}
			session.resources[resourceID] = target
			p.hlsResources++
		}
		return basePath + resourceID, nil
	})
	p.hlsMu.Unlock()
	if err != nil {
		hlsRequestFailed(w)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, local)
	}
}

func resolveHLSResource(root, base *url.URL, raw string, hosts []string) (*url.URL, error) {
	return hlsrelay.Resolve(root, base, raw, hosts)
}

func copyHLSMediaHeaders(dst, src http.Header) {
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if value := src.Get(name); value != "" {
			dst.Set(name, value)
		}
	}
}

func hlsRequestFailed(w http.ResponseWriter) {
	http.Error(w, "HLS source request failed", http.StatusBadGateway)
}

func (p *Proxy) removeHLSSessionLocked(id string) {
	if session := p.hlsSessions[id]; session != nil {
		p.hlsResources -= len(session.resources)
		delete(p.hlsSessions, id)
	}
}

func (p *Proxy) pruneHLSSessionsLocked(now time.Time) {
	for id, session := range p.hlsSessions {
		if !now.Before(session.expires) {
			p.removeHLSSessionLocked(id)
		}
	}
}

func (p *Proxy) evictHLSSessionLocked(except string) bool {
	oldestID := ""
	var oldest time.Time
	for id, session := range p.hlsSessions {
		if id != except && (oldestID == "" || session.used.Before(oldest)) {
			oldestID, oldest = id, session.used
		}
	}
	if oldestID == "" {
		return false
	}
	p.removeHLSSessionLocked(oldestID)
	return true
}
