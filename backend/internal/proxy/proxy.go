package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/streamhttp"
)

type streamURLWithHeader interface {
	StreamURLWithHeader(ctx context.Context, fileID string, header http.Header) (*drives.StreamLink, error)
}

const (
	// StreamLink.Expires 已由各网盘 driver 给出保守期限；这里再设一个统一上限，
	// 避免供应商撤销链接后长时间复用，同时不再每 30 秒重复换链。
	linkCacheMaxAge       = 5 * time.Minute
	linkCacheExpiryMargin = 15 * time.Second
	linkCacheMaxEntries   = 2048
	linkResolveTimeout    = 15 * time.Second
)

// Registry 管理多个 Drive 实例
type Registry struct {
	mu     sync.RWMutex
	drives map[string]drives.Drive
}

func NewRegistry() *Registry {
	return &Registry{drives: make(map[string]drives.Drive)}
}

func (r *Registry) Set(id string, d drives.Drive) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drives[id] = d
}

func (r *Registry) Get(id string) (drives.Drive, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.drives[id]
	return d, ok
}

func (r *Registry) All() []drives.Drive {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]drives.Drive, 0, len(r.drives))
	for _, d := range r.drives {
		out = append(out, d)
	}
	return out
}

func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.drives, id)
}

// Proxy 根据 driveID + fileID 反向代理到真实网盘直链
type Proxy struct {
	Registry *Registry
	// linkCache key: driveID + "/" + fileID (+ User-Agent for UA-bound links)
	cacheMu           sync.Mutex
	cache             map[string]cachedLink
	inflight          map[string]*linkCall
	playbackCache     map[string]cachedPlayback
	playbackInflight  map[string]*playbackCall
	playbackPreparing map[string]*playbackCall
	// resolveTimeout is kept on the proxy so timeout behavior can be tested
	// without making production tests wait for the full provider deadline.
	resolveTimeout time.Duration
	http           *http.Client
	relay          *http.Client
	hlsMu          sync.Mutex
	hlsSessions    map[string]*hlsSession
	hlsResources   int

	allowForcedRelay atomic.Bool

	statusMu       sync.Mutex
	statusReporter StreamStatusReporter
	reportedStatus map[string]string
	initErrors     map[string]driveInitError
}

// StreamStatusReporter receives playback-observed drive health transitions.
// lastError is intentionally the original provider error so the admin page can
// retain the information needed to repair an expired login or authorization.
type StreamStatusReporter func(driveID, status, lastError string)

type cachedLink struct {
	link    *drives.StreamLink
	fetched time.Time
	used    time.Time
}

type linkCall struct {
	done chan struct{}
	once sync.Once
	link *drives.StreamLink
	err  error
}

type driveInitError struct {
	kind string
	err  error
}

func New(r *Registry) *Proxy {
	p := &Proxy{
		Registry:          r,
		cache:             make(map[string]cachedLink),
		inflight:          make(map[string]*linkCall),
		playbackCache:     make(map[string]cachedPlayback),
		playbackInflight:  make(map[string]*playbackCall),
		playbackPreparing: make(map[string]*playbackCall),
		resolveTimeout:    linkResolveTimeout,
		reportedStatus:    make(map[string]string),
		initErrors:        make(map[string]driveInitError),
		hlsSessions:       make(map[string]*hlsSession),
		http:              streamhttp.NewClient(0), // 流式不设超时
		relay:             streamhttp.NewNoRedirectClient(0),
	}
	p.allowForcedRelay.Store(true)
	return p
}

// SetAllowForcedRelay lets operators retain redirect-only bandwidth policy.
// Authentication remains mandatory regardless of this setting.
func (p *Proxy) SetAllowForcedRelay(allow bool) {
	p.allowForcedRelay.Store(allow)
}

// SetDriveInitError keeps a configured-but-unavailable drive visible to the
// playback layer. Without this, an Init failure leaves no Registry entry and
// playback is incorrectly reported as a missing drive/file (404).
func (p *Proxy) SetDriveInitError(driveID, driveKind string, err error) {
	if err == nil {
		return
	}
	p.statusMu.Lock()
	p.initErrors[driveID] = driveInitError{kind: driveKind, err: err}
	p.statusMu.Unlock()
}

// SetStreamStatusReporter connects playback results to the persistent drive
// status maintained by the server. Repeated failures of the same category are
// coalesced so normal player retries do not write the database on every request.
func (p *Proxy) SetStreamStatusReporter(reporter StreamStatusReporter) {
	p.statusMu.Lock()
	defer p.statusMu.Unlock()
	p.statusReporter = reporter
	p.reportedStatus = make(map[string]string)
}

// InvalidateDrive removes links and observed health left by an older driver
// instance. It is called whenever credentials are saved/re-mounted so the next
// playback cannot reuse a stale URL or suppress a repeated authentication error.
func (p *Proxy) InvalidateDrive(driveID string) {
	p.hlsMu.Lock()
	for id, session := range p.hlsSessions {
		if session.driveID == driveID {
			p.removeHLSSessionLocked(id)
		}
	}
	p.hlsMu.Unlock()
	prefix := driveID + "/"
	p.cacheMu.Lock()
	for key := range p.playbackCache {
		if strings.HasPrefix(key, prefix) {
			delete(p.playbackCache, key)
		}
	}
	for key := range p.playbackInflight {
		if strings.HasPrefix(key, prefix) {
			delete(p.playbackInflight, key)
		}
	}
	for key := range p.playbackPreparing {
		if strings.HasPrefix(key, prefix) {
			delete(p.playbackPreparing, key)
		}
	}
	for key := range p.cache {
		if strings.HasPrefix(key, prefix) {
			delete(p.cache, key)
		}
	}
	// 已经发出的 provider 请求无法安全中断，但把它从表里移走后，新请求不会
	// 等待旧凭证的结果；旧请求完成时也会因身份不再匹配而跳过写缓存。
	for key := range p.inflight {
		if strings.HasPrefix(key, prefix) {
			delete(p.inflight, key)
		}
	}
	p.cacheMu.Unlock()

	p.statusMu.Lock()
	delete(p.reportedStatus, driveID)
	delete(p.initErrors, driveID)
	p.statusMu.Unlock()
}

func (p *Proxy) driveInitError(driveID string) (driveInitError, bool) {
	p.statusMu.Lock()
	defer p.statusMu.Unlock()
	result, ok := p.initErrors[driveID]
	return result, ok
}

// DriveInitializationError returns a recorded mount failure without contacting
// the provider. InvalidateDrive clears it when credentials or the driver change.
func (p *Proxy) DriveInitializationError(driveID string) error {
	failure, _ := p.driveInitError(driveID)
	return failure.err
}

func (p *Proxy) getLink(ctx context.Context, d drives.Drive, driveID, fileID string, header http.Header) (*drives.StreamLink, error) {
	key := linkCacheKey(d, driveID, fileID, header)
	now := time.Now()

	p.cacheMu.Lock()
	if c, ok := p.cache[key]; ok {
		if cachedLinkValid(c, now) {
			c.used = now
			p.cache[key] = c
			p.cacheMu.Unlock()
			return c.link, nil
		}
		delete(p.cache, key)
	}
	if call, ok := p.inflight[key]; ok {
		p.cacheMu.Unlock()
		return waitForLinkCall(ctx, call)
	}
	call := &linkCall{done: make(chan struct{})}
	p.inflight[key] = call
	p.cacheMu.Unlock()

	// 解析不跟随某一个浏览器请求的取消：预热和真实播放可能同时等待它，
	// 任何一个调用方离开都不应让其他调用方重新向网盘换链。统一超时负责
	// 给脱离请求生命周期的工作兜底。
	resolveTimeout := p.resolveTimeout
	if resolveTimeout <= 0 {
		resolveTimeout = linkResolveTimeout
	}
	resolveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resolveTimeout)
	requestHeader := header.Clone()
	go func() {
		defer cancel()
		link, err := resolveStreamLink(resolveCtx, d, fileID, requestHeader)
		if timeoutErr := resolveCtx.Err(); timeoutErr != nil {
			p.finishLinkCall(key, call, nil, timeoutErr)
			return
		}
		p.finishLinkCall(key, call, link, err)
	}()

	// 为未遵守 ctx 的 driver 保留兜底。单独的看门狗
	// 保证即使 provider 一直不返回，超时也会结束 call、移除 inflight 并唤醒
	// 所有等待者；迟到的 provider 结果由 linkCall.once 丢弃。
	go func() {
		<-resolveCtx.Done()
		p.finishLinkCall(key, call, nil, resolveCtx.Err())
	}()

	return waitForLinkCall(ctx, call)
}

func (p *Proxy) finishLinkCall(key string, call *linkCall, link *drives.StreamLink, err error) {
	call.once.Do(func() {
		p.cacheMu.Lock()
		call.link = link
		call.err = err
		if current := p.inflight[key]; current == call {
			delete(p.inflight, key)
			if err == nil && link != nil {
				p.storeCachedLinkLocked(key, link, time.Now())
			}
		}
		close(call.done)
		p.cacheMu.Unlock()
	})
}

func resolveStreamLink(ctx context.Context, d drives.Drive, fileID string, header http.Header) (*drives.StreamLink, error) {
	if h, ok := d.(streamURLWithHeader); ok {
		return h.StreamURLWithHeader(ctx, fileID, header)
	}
	return d.StreamURL(ctx, fileID)
}

func waitForLinkCall(ctx context.Context, call *linkCall) (*drives.StreamLink, error) {
	select {
	case <-call.done:
		return call.link, call.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func cachedLinkValid(c cachedLink, now time.Time) bool {
	if c.link == nil || c.link.Expires.IsZero() {
		return false
	}
	if now.Sub(c.fetched) >= linkCacheMaxAge {
		return false
	}
	return now.Add(linkCacheExpiryMargin).Before(c.link.Expires)
}

func (p *Proxy) storeCachedLinkLocked(key string, link *drives.StreamLink, now time.Time) {
	for existingKey, cached := range p.cache {
		if !cachedLinkValid(cached, now) {
			delete(p.cache, existingKey)
		}
	}
	next := cachedLink{link: link, fetched: now, used: now}
	if !cachedLinkValid(next, now) {
		return
	}
	if len(p.cache) >= linkCacheMaxEntries {
		var oldestKey string
		var oldestUsed time.Time
		for existingKey, cached := range p.cache {
			if oldestKey == "" || cached.used.Before(oldestUsed) {
				oldestKey = existingKey
				oldestUsed = cached.used
			}
		}
		delete(p.cache, oldestKey)
	}
	p.cache[key] = next
}

// WarmStreamLink resolves the same cache entry used by ServeStream without
// transferring media bytes. Concurrent warm/play requests share one provider call.
func (p *Proxy) WarmStreamLink(ctx context.Context, driveID, fileID string, header http.Header) error {
	d, ok := p.Registry.Get(driveID)
	if !ok {
		return errDriveNotFound
	}
	_, err := p.getLink(ctx, d, driveID, fileID, header)
	return err
}

func linkCacheKey(d drives.Drive, driveID, fileID string, header http.Header) string {
	key := driveID + "/" + fileID
	if _, ok := d.(streamURLWithHeader); ok {
		key += "|ua=" + header.Get("User-Agent")
	}
	return key
}

func (p *Proxy) ServeStream(w http.ResponseWriter, r *http.Request, driveID, fileID string) {
	d, ok := p.Registry.Get(driveID)
	if !ok {
		if initFailure, unavailable := p.driveInitError(driveID); unavailable {
			p.reportStreamResult(driveID, initFailure.err)
			writeStreamError(w, initFailure.kind, initFailure.err)
			return
		}
		http.Error(w, errDriveNotFound.Error(), errDriveNotFound.Code)
		return
	}

	link, err := p.getLink(r.Context(), d, driveID, fileID, r.Header)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// A browser-aborted request is not a provider health result.
		} else if streamErrorAffectsDrive(err) {
			p.reportStreamResult(driveID, err)
		} else {
			p.reportStreamResult(driveID, nil)
		}
		writeStreamError(w, d.Kind(), err)
		return
	}
	forceRelay := p.allowForcedRelay.Load() && forceStreamRelay(r)
	if clientRedirectSafe(link) && !forceRelay {
		p.reportStreamResult(driveID, nil)
		redirect(w, r, link)
		return
	}
	if err := p.serve(w, r, link, forceRelay); err != nil {
		if errors.Is(err, context.Canceled) {
			// Browser navigation and canceled range requests say nothing about
			// provider health, so retain the previous observed state.
		} else if streamErrorAffectsDrive(err) {
			p.reportStreamResult(driveID, err)
		} else {
			// A missing/deleted individual file does not mean the drive login is
			// unhealthy. Successful link resolution still proves connectivity.
			p.reportStreamResult(driveID, nil)
		}
		writeStreamError(w, d.Kind(), err)
		return
	}
	p.reportStreamResult(driveID, nil)
}

func (p *Proxy) reportStreamResult(driveID string, err error) {
	state := "ok"
	status := "ok"
	lastError := ""
	if err != nil {
		code, _ := classifyStreamError(err)
		state = "error:" + code
		status = "error"
		lastError = err.Error()
	}

	p.statusMu.Lock()
	defer p.statusMu.Unlock()
	if previous, ok := p.reportedStatus[driveID]; ok && previous == state {
		return
	}
	p.reportedStatus[driveID] = state
	if p.statusReporter != nil {
		// Keep this serialized with the state transition. Otherwise concurrent
		// failed/recovered requests could persist in the opposite order.
		p.statusReporter(driveID, status, lastError)
	}
}

func redirect(w http.ResponseWriter, r *http.Request, link *drives.StreamLink) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "max-age=0, no-cache, no-store, must-revalidate")
	http.Redirect(w, r, link.URL, http.StatusFound)
}

func clientRedirectSafe(link *drives.StreamLink) bool {
	if link == nil || !link.ClientRedirectSafe {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(link.URL))
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
}

func forceStreamRelay(r *http.Request) bool {
	return r.URL.Query().Get("tripleScreenRelay") == "1"
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, link *drives.StreamLink, forceRelay bool) error {
	// 构造上游请求
	u, err := url.Parse(link.URL)
	if err != nil {
		return fmt.Errorf("bad upstream url: %w", err)
	}
	if localPath, ok := localFilePath(u, link.URL); ok {
		w.Header().Set("Cache-Control", "private, max-age=300")
		http.ServeFile(w, r, localPath)
		return nil
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), nil)
	if err != nil {
		return fmt.Errorf("build upstream request: %w", err)
	}
	// 复制上游请求头
	for k, vs := range link.Headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// 透传 Range
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}

	client := p.http
	if link.PassThroughRedirects && !forceRelay {
		client = p.relay
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request upstream: %w", err)
	}
	defer resp.Body.Close()
	if link.PassThroughRedirects && isRedirectStatus(resp.StatusCode) {
		return relayUpstreamRedirect(w, resp)
	}
	if resp.StatusCode >= http.StatusBadRequest && resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		return &upstreamHTTPError{StatusCode: resp.StatusCode}
	}

	// 透传响应头
	for _, k := range []string{
		"Content-Type", "Content-Length", "Content-Range",
		"Accept-Ranges", "Last-Modified", "Etag",
	} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return nil
}

func isRedirectStatus(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

func relayUpstreamRedirect(w http.ResponseWriter, resp *http.Response) error {
	target, err := resp.Location()
	if err != nil {
		return fmt.Errorf("invalid upstream redirect: %w", err)
	}
	if (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" || target.User != nil {
		return fmt.Errorf("unsafe upstream redirect target")
	}
	w.Header().Set("Location", target.String())
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "max-age=0, no-cache, no-store, must-revalidate")
	w.WriteHeader(resp.StatusCode)
	return nil
}

type upstreamHTTPError struct {
	StatusCode int
}

func (e *upstreamHTTPError) Error() string {
	return fmt.Sprintf("upstream returned HTTP %d %s", e.StatusCode, http.StatusText(e.StatusCode))
}

func streamErrorAffectsDrive(err error) bool {
	if errors.Is(err, os.ErrNotExist) || drives.ErrorMentionsHTTPStatus(err, http.StatusNotFound, http.StatusGone) {
		return false
	}
	var upstream *upstreamHTTPError
	if !errors.As(err, &upstream) {
		return true
	}
	switch upstream.StatusCode {
	case http.StatusNotFound, http.StatusGone, http.StatusRequestedRangeNotSatisfiable:
		return false
	default:
		return true
	}
}

type streamErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeStreamError(w http.ResponseWriter, driveKind string, err error) {
	code, category := classifyStreamError(err)
	label := driveLabel(driveKind)
	message := label + "获取播放地址失败，请稍后重试或联系管理员"
	switch category {
	case "auth":
		message = label + "登录或授权已失效，请联系管理员重新登录"
	case "rate_limit":
		message = label + "当前正在限流，请稍后重试"
	case "not_found":
		message = label + "中的视频文件不存在或已失效，请联系管理员重新扫描"
	case "unavailable":
		message = label + "上游服务暂时不可用，请稍后重试"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// These are application-level drive dependency failures. A gateway 502
	// can be replaced by a CDN, discarding the code/message the player needs.
	w.WriteHeader(http.StatusFailedDependency)
	_ = json.NewEncoder(w).Encode(streamErrorResponse{Code: code, Message: message})
}

func classifyStreamError(err error) (code, category string) {
	var upstream *upstreamHTTPError
	if errors.As(err, &upstream) {
		switch upstream.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusProxyAuthRequired:
			return "drive_auth_failed", "auth"
		case http.StatusNotFound, http.StatusGone:
			return "drive_source_not_found", "not_found"
		case http.StatusTooManyRequests:
			return "drive_rate_limited", "rate_limit"
		default:
			if upstream.StatusCode >= http.StatusInternalServerError {
				return "drive_upstream_unavailable", "unavailable"
			}
			return "drive_stream_failed", "generic"
		}
	}
	if _, ok := drives.RateLimitRetryAfter(err); ok {
		return "drive_rate_limited", "rate_limit"
	}
	var provider *drives.ProviderError
	if errors.As(err, &provider) {
		switch provider.Kind {
		case drives.ProviderErrorAuth:
			return "drive_auth_failed", "auth"
		case drives.ProviderErrorUnavailable:
			return "drive_upstream_unavailable", "unavailable"
		default:
			return "drive_stream_failed", "generic"
		}
	}
	if errors.Is(err, os.ErrNotExist) || drives.ErrorMentionsHTTPStatus(err, http.StatusNotFound, http.StatusGone) {
		return "drive_source_not_found", "not_found"
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{
		"无效登录信息", "无效的令牌",
		"登录超时", "请重新登录", "登录已失效", "未登录", "主动退出",
		"user not login", "not logged in", "invalid_grant", "invalid grant",
		"refresh token", "refresh_token", "token expired", "expired token",
		"invalid token", "token is invalid", "unauthorized", "unauthenticated",
		"captcha_invalid", "verification code is invalid", "cookie invalid",
		"invalid cookie", "cookie expired", "session exited",
	} {
		if strings.Contains(text, marker) {
			return "drive_auth_failed", "auth"
		}
	}
	if drives.ErrorMentionsHTTPStatus(err, http.StatusUnauthorized, http.StatusForbidden, http.StatusProxyAuthRequired) {
		return "drive_auth_failed", "auth"
	}
	if drives.ErrorMentionsHTTPStatus(err, http.StatusTooManyRequests) {
		return "drive_rate_limited", "rate_limit"
	}
	return "drive_stream_failed", "generic"
}

func driveLabel(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "p115":
		return "115 网盘"
	case "p123":
		return "123 网盘"
	case "guangyapan":
		return "光鸭网盘"
	case "pikpak":
		return "PikPak"
	case "wopan":
		return "联通网盘"
	case "yun139":
		return "移动网盘"
	case "onedrive":
		return "OneDrive"
	case "googledrive":
		return "Google Drive"
	case "webdav":
		return "WebDAV"
	case "quark":
		return "夸克网盘"
	case "localstorage", "local-upload", "telegram-local":
		return "本地存储"
	default:
		return "网盘"
	}
}

// ServeLocal 服务本地预览视频文件
func (p *Proxy) ServeLocal(w http.ResponseWriter, r *http.Request, path string) {
	http.ServeFile(w, r, path)
}

func localFilePath(u *url.URL, raw string) (string, bool) {
	if u == nil {
		return "", false
	}
	// Windows 盘符绝对路径，如 E:\videos\file.mp4
	// url.Parse 会把盘符当作 scheme（如 "e"），所以必须在 scheme 检查之前处理
	if isWindowsDrivePath(raw) {
		return raw, true
	}
	if u.Scheme == "file" && u.Path != "" {
		return u.Path, true
	}
	if u.Scheme == "" && u.Host == "" && filepath.IsAbs(raw) {
		return raw, true
	}
	return "", false
}

// isWindowsDrivePath 检查是否为 Windows 盘符绝对路径，如 C:\path 或 D:/path
func isWindowsDrivePath(p string) bool {
	if len(p) < 3 {
		return false
	}
	c := p[0]
	if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
		return false
	}
	if p[1] != ':' {
		return false
	}
	return p[2] == '\\' || p[2] == '/'
}

var errDriveNotFound = &httpError{Code: http.StatusNotFound, Msg: "drive not found"}

type httpError struct {
	Code int
	Msg  string
}

func (e *httpError) Error() string { return e.Msg }
