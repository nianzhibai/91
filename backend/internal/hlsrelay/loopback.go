package hlsrelay

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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/hlsplaylist"
	"github.com/nianzhibai/91/backend/internal/streamhttp"
)

const maxPlaylistBytes = 1 << 20
const maxResources = 64 * 1024

// Loopback owns a task's resource namespace and preserves upstream failures for
// the worker's refresh/cooldown decisions. Signed URLs never appear in errors.
type Loopback struct {
	URL       string
	root      *url.URL
	headers   http.Header
	policy    drives.HLSRelay
	client    *http.Client
	prefix    string
	mu        sync.Mutex
	resources map[string]*url.URL
	paths     map[string]string
	lastErr   error
	ctx       context.Context
	cancel    context.CancelCauseFunc
	close     func()
}

func Start(ctx context.Context, link *drives.StreamLink, policy drives.HLSRelay) (*Loopback, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if link == nil {
		return nil, errors.New("missing HLS link")
	}
	root, err := url.Parse(link.URL)
	if err != nil || !AllowedURL(root, root, policy.AllowedHosts) || streamhttp.HasSensitiveHeaders(link.Headers) || (policy.Authorize != nil && len(policy.AllowedHosts) == 0) {
		return nil, errors.New("invalid HLS input policy")
	}
	if len(policy.Playlist) > maxPlaylistBytes {
		return nil, errors.New("HLS playlist too large")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	relay := &Loopback{root: root, headers: link.Headers.Clone(), policy: policy,
		client: streamhttp.NewClient(0), prefix: "/" + uuid.NewString() + "/",
		resources: make(map[string]*url.URL), paths: make(map[string]string)}
	relay.policy.AllowedHosts = append([]string(nil), policy.AllowedHosts...)
	rootPath := relay.prefix + "index.m3u8"
	relay.resources[rootPath], relay.paths[root.String()] = root, rootPath
	relay.URL = "http://" + listener.Addr().String() + rootPath
	relayCtx, cancel := context.WithCancelCause(ctx)
	relay.ctx, relay.cancel = relayCtx, cancel
	server := &http.Server{Handler: relay, ReadHeaderTimeout: 5 * time.Second, BaseContext: func(net.Listener) context.Context { return relayCtx }}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	var once sync.Once
	relay.close = func() { once.Do(func() { cancel(context.Canceled); _ = server.Close(); _ = listener.Close(); <-done }) }
	go func() {
		select {
		case <-ctx.Done():
			relay.Close()
		case <-done:
		}
	}()
	return relay, nil
}

// Context is canceled with the upstream failure to stop the current decoder
// and its outstanding resource requests. Refreshes must use the parent context.
func (h *Loopback) Context() context.Context { return h.ctx }
func (h *Loopback) Close()                   { h.close() }
func (h *Loopback) ResetError() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ctx.Err() == nil {
		h.lastErr = nil
	}
}
func (h *Loopback) Error() error { h.mu.Lock(); defer h.mu.Unlock(); return h.lastErr }
func (h *Loopback) record(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastErr == nil {
		h.lastErr = err
	}
}

func (h *Loopback) stop(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// The first explicit failure overrides incidental early-read errors.
	// Concurrent failures cannot mask it; rate limits always take precedence.
	_, limited := drives.RateLimitRetryAfter(err)
	if h.ctx.Err() == nil || limited {
		h.lastErr = err
	}
	h.cancel(h.lastErr)
}

func (h *Loopback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	h.mu.Lock()
	target := h.resources[r.URL.Path]
	h.mu.Unlock()
	if target == nil {
		http.NotFound(w, r)
		return
	}
	// A decoder may skip a rejected segment and request another. Once this
	// source fails, only its owner may resolve a replacement and resume reads.
	if h.ctx.Err() != nil {
		http.Error(w, "HLS source request failed", http.StatusBadGateway)
		return
	}
	if r.URL.Path == h.prefix+"index.m3u8" && h.policy.Playlist != "" {
		h.playlist(w, r, target, h.policy.Playlist)
		return
	}
	response, err := Fetch(h.client, r, h.root, target, h.headers, h.policy)
	if err != nil {
		// FFmpeg normally closes reads early after probing or extracting a frame.
		// A consumer cancellation is not an upstream failure.
		if r.Context().Err() == nil {
			h.fail(w, errors.New("HLS upstream request failed"))
		}
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		status := response.StatusCode
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		failure := fmt.Errorf("HLS media HTTP status=%d", status)
		if status == http.StatusTooManyRequests || status == http.StatusMethodNotAllowed {
			retry := retryAfter(response.Header.Get("Retry-After"))
			failure = &drives.RateLimitError{Provider: "HLS", RetryAfter: retry, Err: failure}
		}
		h.stop(failure)
		if value := response.Header.Get("Retry-After"); value != "" {
			w.Header().Set("Retry-After", value)
		}
		w.WriteHeader(status)
		return
	}
	body := bufio.NewReader(response.Body)
	if r.Method != http.MethodHead {
		prefix, _ := body.Peek(len("#EXTM3U"))
		if bytes.Equal(prefix, []byte("#EXTM3U")) || strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "mpegurl") {
			data, err := io.ReadAll(io.LimitReader(body, maxPlaylistBytes+1))
			if err != nil || len(data) > maxPlaylistBytes {
				if r.Context().Err() == nil {
					h.fail(w, errors.New("invalid HLS playlist response"))
				}
				return
			}
			h.playlist(w, r, response.Request.URL, string(data))
			return
		}
	}
	for _, key := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if value := response.Header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	if r.Method != http.MethodHead {
		if _, err := io.Copy(w, body); err != nil && r.Context().Err() == nil {
			h.record(errors.New("HLS media read failed"))
		}
	}
}

func (h *Loopback) playlist(w http.ResponseWriter, r *http.Request, base *url.URL, data string) {
	local, err := hlsplaylist.Rewrite(data, func(raw string) (string, error) {
		target, err := Resolve(h.root, base, raw, h.policy.AllowedHosts)
		if err != nil {
			return "", err
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		key := target.String()
		if local, ok := h.paths[key]; ok {
			return local, nil
		}
		if len(h.resources) >= maxResources {
			return "", errors.New("HLS resource limit reached")
		}
		ext := path.Ext(target.Path)
		// Preserve standard extensions for FFmpeg's HLS demuxer without disclosing
		// provider paths or signed query strings in the local resource namespace.
		if len(ext) > 10 || strings.ContainsAny(ext, "?#%") {
			ext = ""
		}
		local := h.prefix + strconv.Itoa(len(h.resources)) + ext
		h.resources[local], h.paths[key] = target, local
		return local, nil
	})
	if err != nil {
		h.fail(w, errors.New("invalid HLS resource playlist"))
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, local)
	}
}
func (h *Loopback) fail(w http.ResponseWriter, err error) {
	h.stop(err)
	http.Error(w, "HLS source request failed", http.StatusBadGateway)
}
func retryAfter(raw string) time.Duration {
	if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(raw); err == nil {
		if wait := time.Until(deadline); wait > 0 {
			return wait
		}
	}
	return 0
}
