package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/streamhttp"
)

// ServePlaybackFile resolves only a provider's named rendition. The caller
// must authorize the catalog video for every GET/HEAD, including shared videos.
// No upstream URL, account cookie, or browser query is accepted from the client.
func (p *Proxy) ServePlaybackFile(w http.ResponseWriter, r *http.Request, driveID, fileID, sourceID string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	sources, err := p.PlaybackSources(r.Context(), driveID, fileID, r.UserAgent(), false)
	if err != nil {
		p.WritePlaybackError(w, driveID, err)
		return
	}
	var source *drives.PlaybackSource
	for i := range sources {
		if sources[i].ID == sourceID {
			source = &sources[i]
			break
		}
	}
	if source == nil || source.Kind != "transcoded" || source.Protocol != "file" || source.Status != "ready" || source.Link == nil || source.FileRelay == nil {
		// Video/share access has already been checked. A rendition can vanish
		// between range requests without invalidating that access.
		writePlaybackSourceExpired(w)
		return
	}
	if !source.Link.Expires.IsZero() && !time.Now().Before(source.Link.Expires) {
		writePlaybackSourceExpired(w)
		return
	}
	root, err := url.Parse(source.Link.URL)
	if err != nil || !allowedPlaybackURL(root, root, source.FileRelay.AllowedHosts) {
		http.Error(w, "invalid playback source", http.StatusBadGateway)
		return
	}
	for name := range source.Link.Headers {
		switch strings.ToLower(name) {
		case "cookie", "authorization", "proxy-authorization":
			http.Error(w, "invalid playback source", http.StatusBadGateway)
			return
		}
	}
	if playbackFileRedirectSafe(source.Link, r) && !(p.allowForcedRelay.Load() && forceStreamRelay(r)) {
		redirect(w, r, source.Link)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, root.String(), nil)
	if err != nil {
		http.Error(w, "invalid playback request", http.StatusBadGateway)
		return
	}
	req.Header = source.Link.Headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Accept-Encoding", "identity")
	for _, name := range []string{"Range", "If-Range"} {
		if value := r.Header.Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}
	client := *p.http
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !allowedPlaybackURL(root, next.URL, source.FileRelay.AllowedHosts) {
			return errors.New("playback redirect is not allowed")
		}
		return streamhttp.CheckRedirect(next, via)
	}
	resp, err := client.Do(req)
	if err != nil {
		if r.Context().Err() == nil {
			http.Error(w, "playback source request failed", http.StatusBadGateway)
		}
		return
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		// A signed media rejection should refresh the rendition once, not be
		// mistaken for an expired site login or a broken original-file account.
		writePlaybackSourceExpired(w)
		return
	case http.StatusOK, http.StatusPartialContent, http.StatusRequestedRangeNotSatisfiable:
	default:
		if value := resp.Header.Get("Retry-After"); value != "" {
			w.Header().Set("Retry-After", value)
		}
		status := resp.StatusCode
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		w.WriteHeader(status)
		return
	}
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Etag", "Last-Modified"} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, resp.Body); err != nil && r.Context().Err() == nil {
		// Headers are already committed. Abort the HTTP stream so a truncated
		// response cannot be presented as a successfully completed video range.
		panic(http.ErrAbortHandler)
	}
}

func playbackFileRedirectSafe(link *drives.StreamLink, r *http.Request) bool {
	if !clientRedirectSafe(link) {
		return false
	}
	for name, values := range link.Headers {
		// A matching UA is supplied naturally by the redirected browser.
		// Any other required header keeps the source on the server.
		if !strings.EqualFold(name, "User-Agent") || len(values) != 1 || values[0] != r.UserAgent() {
			return false
		}
	}
	return true
}
