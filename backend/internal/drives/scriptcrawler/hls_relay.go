package scriptcrawler

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nianzhibai/91/backend/internal/hlsplaylist"
)

const maxHLSPlaylistBytes = 4 * 1024 * 1024

// hlsRelay keeps all source requests in the importer's HTTP client. FFmpeg
// receives local URLs for playlists, segments, initialization maps and keys,
// so it shares ordinary downloads' proxy, TLS and request-header behavior.
type hlsRelay struct {
	client  *http.Client
	headers http.Header
	base    url.URL

	mu        sync.Mutex
	resources map[string]*url.URL
	paths     map[string]string
	firstErr  error
}

func startHLSRelay(ctx context.Context, client *http.Client, ref MediaRef) (string, func(), *hlsRelay, error) {
	if client == nil {
		return "", nil, nil, fmt.Errorf("HLS HTTP client is not configured")
	}
	if err := validateHTTPURL(ref.URL); err != nil {
		return "", nil, nil, err
	}
	source, _ := url.Parse(ref.URL)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, nil, fmt.Errorf("start HLS relay: %w", err)
	}
	relay := &hlsRelay{
		client: client, headers: mediaRequestHeaders(ref),
		base:      url.URL{Scheme: "http", Host: listener.Addr().String(), Path: "/" + uuid.NewString()},
		resources: make(map[string]*url.URL), paths: make(map[string]string),
	}
	input, err := relay.resourceURL(source)
	if err != nil {
		_ = listener.Close()
		return "", nil, nil, err
	}
	relayCtx, cancel := context.WithCancel(ctx)
	server := &http.Server{
		Handler: relay, ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context { return relayCtx },
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(listener)
	}()
	closeRelay := func() {
		cancel()
		_ = server.Close()
		<-done
	}
	return input, closeRelay, relay, nil
}

func (h *hlsRelay) resourceURL(source *url.URL) (string, error) {
	if (source.Scheme != "http" && source.Scheme != "https") || source.Host == "" {
		return "", fmt.Errorf("HLS resources must use absolute HTTP(S) URLs")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	key := source.String()
	resourcePath, ok := h.paths[key]
	if !ok {
		resourcePath = fmt.Sprintf("%s/%d%s", h.base.Path, len(h.resources), path.Ext(source.Path))
		h.resources[resourcePath] = source
		h.paths[key] = resourcePath
	}
	local := h.base
	local.Path = resourcePath
	return local.String(), nil
}

func (h *hlsRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	h.mu.Lock()
	source := h.resources[r.URL.Path]
	h.mu.Unlock()
	if source == nil {
		http.NotFound(w, r)
		return
	}
	target := *source
	if r.URL.RawQuery != "" {
		if target.RawQuery != "" {
			target.RawQuery += "&"
		}
		target.RawQuery += r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), nil)
	if err != nil {
		h.fail(w, err)
		return
	}
	req.Header = h.headers.Clone()
	// HLS byte ranges refer to the uncompressed representation, and playlists
	// must remain readable here before their resource URLs are rewritten.
	req.Header.Set("Accept-Encoding", "identity")
	for _, name := range []string{"Range", "If-Range"} {
		if value := r.Header.Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.fail(w, err)
		return
	}
	defer resp.Body.Close()
	body := bufio.NewReader(resp.Body)
	prefix, _ := body.Peek(len("#EXTM3U"))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && bytes.Equal(prefix, []byte("#EXTM3U")) {
		data, err := io.ReadAll(io.LimitReader(body, maxHLSPlaylistBytes+1))
		if err == nil && len(data) > maxHLSPlaylistBytes {
			err = fmt.Errorf("HLS playlist exceeds %d bytes", maxHLSPlaylistBytes)
		}
		if err != nil {
			h.fail(w, err)
			return
		}
		base := &target
		if resp.Request != nil && resp.Request.URL != nil {
			base = resp.Request.URL
		}
		playlist, err := h.rewritePlaylist(string(data), base)
		if err != nil {
			h.fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, playlist)
		return
	}
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, body); err != nil {
		h.recordError(err)
	}
}

func (h *hlsRelay) rewritePlaylist(playlist string, base *url.URL) (string, error) {
	resolve := func(raw string) (string, error) {
		if raw == "" {
			return "", fmt.Errorf("HLS resource URI is empty")
		}
		reference, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("invalid HLS resource URI: %w", err)
		}
		return h.resourceURL(base.ResolveReference(reference))
	}
	return hlsplaylist.Rewrite(playlist, resolve)
}

func (h *hlsRelay) recordError(err error) {
	// net/http errors include the signed source URL in their message. Keep
	// the underlying failure without exposing that URL in task results.
	var requestErr *url.Error
	if errors.As(err, &requestErr) {
		err = requestErr.Err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.firstErr == nil {
		h.firstErr = err
	}
}

func (h *hlsRelay) error() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.firstErr
}

func (h *hlsRelay) fail(w http.ResponseWriter, err error) {
	h.recordError(err)
	http.Error(w, "HLS source request failed", http.StatusBadGateway)
}
