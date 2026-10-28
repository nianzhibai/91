package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/video-site/backend/internal/drives"
)

func TestServeStreamLinkFailureReturnsStructuredErrorAndReportsStatus(t *testing.T) {
	reg := NewRegistry()
	drv := &proxyResultDrive{
		kind: "p115",
		results: []proxyDriveResult{{
			err: errors.New(`115 get file: {"error":"登录超时，请重新登录。"}: user not login`),
		}},
	}
	reg.Set("115", drv)
	p := New(reg)
	var updates []proxyStatusUpdate
	p.SetStreamStatusReporter(func(driveID, status, lastError string) {
		updates = append(updates, proxyStatusUpdate{driveID, status, lastError})
	})

	req := httptest.NewRequest(http.MethodGet, "/p/stream/115/file-1", nil)
	rr := httptest.NewRecorder()
	p.ServeStream(rr, req, "115", "file-1")

	if rr.Code != http.StatusFailedDependency {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusFailedDependency)
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	var payload streamErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v; body=%q", err, rr.Body.String())
	}
	if payload.Code != "drive_auth_failed" {
		t.Fatalf("code = %q, want drive_auth_failed", payload.Code)
	}
	if payload.Message != "115 网盘登录或授权已失效，请联系管理员重新登录" {
		t.Fatalf("message = %q", payload.Message)
	}
	if len(updates) != 1 || updates[0].driveID != "115" || updates[0].status != "error" || updates[0].lastError == "" {
		t.Fatalf("status updates = %#v", updates)
	}
}

func TestServeStreamTripleScreenRelayBypassesRedirectMode(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("video-bytes"))
	}))
	t.Cleanup(upstream.Close)

	reg := NewRegistry()
	reg.Set("115", &proxyFakeSimpleDrive{kind: "p115", url: upstream.URL + "/video.mp4"})
	p := New(reg)

	req := httptest.NewRequest(http.MethodGet, "/p/stream/115/file-1?tripleScreenRelay=1", nil)
	rr := httptest.NewRecorder()
	p.ServeStream(rr, req, "115", "file-1")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if got := rr.Header().Get("Location"); got != "" {
		t.Fatalf("Location = %q, want no redirect", got)
	}
	if got := rr.Body.String(); got != "video-bytes" {
		t.Fatalf("body = %q", got)
	}
}

func TestServeStreamCanDisableClientRequestedRelay(t *testing.T) {
	reg := NewRegistry()
	reg.Set("115", &proxyFakeSimpleDrive{kind: "p115", url: "https://cdn.example/video.mp4"})
	p := New(reg)
	p.SetAllowForcedRelay(false)

	req := httptest.NewRequest(http.MethodGet, "/p/stream/115/file-1?tripleScreenRelay=1", nil)
	rr := httptest.NewRecorder()
	p.ServeStream(rr, req, "115", "file-1")

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusFound)
	}
	if got := rr.Header().Get("Location"); got != "https://cdn.example/video.mp4" {
		t.Fatalf("Location = %q", got)
	}
}

func TestServeStreamReportsRecoveryAndCoalescesRepeatedSuccess(t *testing.T) {
	reg := NewRegistry()
	drv := &proxyResultDrive{
		kind: "guangyapan",
		results: []proxyDriveResult{
			{err: errors.New("invalid_grant: invalidate refresh token")},
			{url: "https://cdn.example/video.mp4"},
		},
	}
	reg.Set("guangyapan", drv)
	p := New(reg)
	var updates []proxyStatusUpdate
	p.SetStreamStatusReporter(func(driveID, status, lastError string) {
		updates = append(updates, proxyStatusUpdate{driveID, status, lastError})
	})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/p/stream/guangyapan/file-1", nil)
		rr := httptest.NewRecorder()
		p.ServeStream(rr, req, "guangyapan", "file-1")
		if i == 0 && rr.Code != http.StatusFailedDependency {
			t.Fatalf("failed request status = %d", rr.Code)
		}
		if i > 0 && rr.Code != http.StatusFound {
			t.Fatalf("successful request %d status = %d", i, rr.Code)
		}
	}

	if len(updates) != 2 {
		t.Fatalf("status updates = %#v, want error then one recovery", updates)
	}
	if updates[0].status != "error" || updates[1].status != "ok" || updates[1].lastError != "" {
		t.Fatalf("status updates = %#v", updates)
	}
}

func TestServeStreamUpstreamAuthFailureReturnsStructuredError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "expired", http.StatusUnauthorized)
	}))
	defer upstream.Close()

	reg := NewRegistry()
	reg.Set("quark", &proxyFakeSimpleDrive{kind: "quark", url: upstream.URL + "/video.mp4"})
	p := New(reg)
	var update proxyStatusUpdate
	p.SetStreamStatusReporter(func(driveID, status, lastError string) {
		update = proxyStatusUpdate{driveID, status, lastError}
	})
	req := httptest.NewRequest(http.MethodGet, "/p/stream/quark/file-1", nil)
	rr := httptest.NewRecorder()
	p.ServeStream(rr, req, "quark", "file-1")

	if rr.Code != http.StatusFailedDependency {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusFailedDependency)
	}
	var payload streamErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Code != "drive_auth_failed" || update.status != "error" {
		t.Fatalf("payload=%#v update=%#v", payload, update)
	}
}

func TestClassifyStreamErrorRecognizesConfiguredDriveAuthFailures(t *testing.T) {
	for _, text := range []string{
		"登录超时，请重新登录: user not login",
		`{"error":"invalid_grant","error_description":"invalidate refresh token"}`,
		"pikpak error_code=9 error=captcha_invalid description=Verification code is invalid",
	} {
		if code, category := classifyStreamError(errors.New(text)); code != "drive_auth_failed" || category != "auth" {
			t.Fatalf("classify %q = (%q, %q)", text, code, category)
		}
	}
}

func TestServeStreamUsesProviderFailureClassification(t *testing.T) {
	tests := []struct {
		kind drives.ProviderErrorKind
		code string
	}{
		{drives.ProviderErrorAuth, "drive_auth_failed"},
		{drives.ProviderErrorUnavailable, "drive_upstream_unavailable"},
		{drives.ProviderErrorOther, "drive_stream_failed"},
	}
	for _, tt := range tests {
		for _, initializing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/init=%v", tt.kind, initializing), func(t *testing.T) {
				// The account endpoint and an earlier rejection can mention tokens
				// even when the final failure is a network/configuration problem.
				err := fmt.Errorf("guangyapan init: %w", &drives.ProviderError{
					Kind: tt.kind,
					Err:  errors.New("guangyapan refresh: refresh_token request failed"),
				})
				registry := NewRegistry()
				p := New(registry)
				if initializing {
					p.SetDriveInitError("guangyapan", "guangyapan", err)
				} else {
					registry.Set("guangyapan", &proxyResultDrive{
						kind: "guangyapan", results: []proxyDriveResult{{err: err}},
					})
				}
				rr := httptest.NewRecorder()
				p.ServeStream(rr, httptest.NewRequest(http.MethodGet, "/p/stream/guangyapan/file", nil), "guangyapan", "file")
				var payload streamErrorResponse
				if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				if rr.Code != http.StatusFailedDependency || payload.Code != tt.code {
					t.Fatalf("status=%d payload=%+v, want %s", rr.Code, payload, tt.code)
				}
				if tt.kind == drives.ProviderErrorUnavailable && payload.Message != "光鸭网盘上游服务暂时不可用，请稍后重试" {
					t.Fatalf("temporary failure message = %q", payload.Message)
				}
			})
		}
	}
}

func TestServeStreamMissingFileDoesNotMarkWholeDriveUnhealthy(t *testing.T) {
	reg := NewRegistry()
	reg.Set("local", &proxyResultDrive{
		kind:    "localstorage",
		results: []proxyDriveResult{{err: os.ErrNotExist}},
	})
	p := New(reg)
	var update proxyStatusUpdate
	p.SetStreamStatusReporter(func(driveID, status, lastError string) {
		update = proxyStatusUpdate{driveID, status, lastError}
	})

	req := httptest.NewRequest(http.MethodGet, "/p/stream/local/missing.mp4", nil)
	rr := httptest.NewRecorder()
	p.ServeStream(rr, req, "local", "missing.mp4")

	if rr.Code != http.StatusFailedDependency {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusFailedDependency)
	}
	var payload streamErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Code != "drive_source_not_found" {
		t.Fatalf("code = %q, want drive_source_not_found", payload.Code)
	}
	if update.status != "ok" || update.lastError != "" {
		t.Fatalf("status update = %#v, missing file must not mark drive error", update)
	}
}

func TestServeStreamConfiguredDriveInitFailureIsNotReportedAsMissing(t *testing.T) {
	p := New(NewRegistry())
	p.SetDriveInitError("115", "p115", errors.New("该账号已在其他端主动退出: session exited"))
	var update proxyStatusUpdate
	p.SetStreamStatusReporter(func(driveID, status, lastError string) {
		update = proxyStatusUpdate{driveID, status, lastError}
	})

	req := httptest.NewRequest(http.MethodGet, "/p/stream/115/file-1", nil)
	rr := httptest.NewRecorder()
	p.ServeStream(rr, req, "115", "file-1")

	if rr.Code != http.StatusFailedDependency {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusFailedDependency)
	}
	var payload streamErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Code != "drive_auth_failed" {
		t.Fatalf("code = %q, want drive_auth_failed", payload.Code)
	}
	if update.status != "error" || update.driveID != "115" {
		t.Fatalf("status update = %#v", update)
	}
}

type proxyStatusUpdate struct {
	driveID   string
	status    string
	lastError string
}

type proxyDriveResult struct {
	url string
	err error
}

type proxyResultDrive struct {
	kind    string
	results []proxyDriveResult
	calls   int
}

func (d *proxyResultDrive) Kind() string               { return d.kind }
func (d *proxyResultDrive) ID() string                 { return d.kind }
func (d *proxyResultDrive) Init(context.Context) error { return nil }
func (d *proxyResultDrive) List(context.Context, string) ([]drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *proxyResultDrive) Stat(context.Context, string) (*drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *proxyResultDrive) StreamURL(context.Context, string) (*drives.StreamLink, error) {
	index := d.calls
	d.calls++
	if index >= len(d.results) {
		index = len(d.results) - 1
	}
	result := d.results[index]
	if result.err != nil {
		return nil, result.err
	}
	return &drives.StreamLink{
		URL: result.url, Headers: http.Header{}, Expires: time.Now().Add(time.Minute), ClientRedirectSafe: redirectKindForTest(d.kind),
	}, nil
}
func (d *proxyResultDrive) Upload(context.Context, string, string, io.Reader, int64) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *proxyResultDrive) EnsureDir(context.Context, string) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *proxyResultDrive) RootID() string { return "0" }

func TestServeStreamRedirectsP115WithRequestUserAgent(t *testing.T) {
	reg := NewRegistry()
	drv := &proxyFakeDrive{kind: "p115"}
	reg.Set("115", drv)

	p := New(reg)
	req := httptest.NewRequest(http.MethodGet, "/p/stream/115/file-1", nil)
	req.Header.Set("User-Agent", "Browser-A")
	rr := httptest.NewRecorder()

	p.ServeStream(rr, req, "115", "file-1")

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusFound)
	}
	if got := rr.Header().Get("Location"); got != "https://cdn.example/file-1?ua=Browser-A" {
		t.Fatalf("Location = %q", got)
	}
	if got := drv.calls[0].ua; got != "Browser-A" {
		t.Fatalf("link UA = %q, want request UA", got)
	}
	if got := rr.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q", got)
	}
}

func TestServeStreamP115CacheIsUserAgentScoped(t *testing.T) {
	reg := NewRegistry()
	drv := &proxyFakeDrive{kind: "p115"}
	reg.Set("115", drv)

	p := New(reg)

	requestP115(t, p, "115", "file-1", "Browser-A")
	requestP115(t, p, "115", "file-1", "Browser-B")
	requestP115(t, p, "115", "file-1", "Browser-A")

	if len(drv.calls) != 2 {
		t.Fatalf("link calls = %d, want 2", len(drv.calls))
	}
	if drv.calls[0].ua != "Browser-A" || drv.calls[1].ua != "Browser-B" {
		t.Fatalf("link UAs = %#v", drv.calls)
	}
}

func TestGetLinkReusesCachedURLBeyondThirtySeconds(t *testing.T) {
	reg := NewRegistry()
	drv := &proxyFakeSimpleDrive{
		kind: "onedrive",
		url:  "https://cdn.example/fresh.mp4",
	}
	reg.Set("onedrive", drv)
	p := New(reg)
	now := time.Now()
	p.cache["onedrive/file-1"] = cachedLink{
		link: &drives.StreamLink{
			URL:     "https://cdn.example/cached.mp4",
			Expires: now.Add(10 * time.Minute),
		},
		fetched: now.Add(-time.Minute),
		used:    now.Add(-time.Minute),
	}

	link, err := p.getLink(context.Background(), drv, "onedrive", "file-1", nil)
	if err != nil {
		t.Fatalf("getLink: %v", err)
	}
	if link.URL != "https://cdn.example/cached.mp4" {
		t.Fatalf("URL = %q, want cached URL", link.URL)
	}
	if drv.calls != 0 {
		t.Fatalf("driver calls = %d, want 0", drv.calls)
	}
}

func TestGetLinkRefreshesAtMaxAgeAndBeforeProviderExpiry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fetched time.Time
		expires time.Time
	}{
		{
			name:    "max age",
			fetched: time.Now().Add(-linkCacheMaxAge),
			expires: time.Now().Add(10 * time.Minute),
		},
		{
			name:    "expiry safety margin",
			fetched: time.Now(),
			expires: time.Now().Add(linkCacheExpiryMargin / 2),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry()
			drv := &proxyFakeSimpleDrive{
				kind: "onedrive",
				url:  "https://cdn.example/fresh.mp4",
			}
			reg.Set("onedrive", drv)
			p := New(reg)
			p.cache["onedrive/file-1"] = cachedLink{
				link: &drives.StreamLink{
					URL:     "https://cdn.example/stale.mp4",
					Expires: tc.expires,
				},
				fetched: tc.fetched,
				used:    tc.fetched,
			}

			link, err := p.getLink(context.Background(), drv, "onedrive", "file-1", nil)
			if err != nil {
				t.Fatalf("getLink: %v", err)
			}
			if link.URL != "https://cdn.example/fresh.mp4" {
				t.Fatalf("URL = %q, want refreshed URL", link.URL)
			}
			if drv.calls != 1 {
				t.Fatalf("driver calls = %d, want 1", drv.calls)
			}
		})
	}
}

func TestConcurrentWarmAndPlaybackShareOneLinkResolution(t *testing.T) {
	reg := NewRegistry()
	drv := newProxyBlockingDrive()
	reg.Set("onedrive", drv)
	p := New(reg)

	const callers = 20
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wait sync.WaitGroup
	for index := 0; index < callers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errs <- p.WarmStreamLink(
				context.Background(),
				"onedrive",
				"file-1",
				nil,
			)
		}()
	}
	close(start)
	<-drv.started
	close(drv.release)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("WarmStreamLink: %v", err)
		}
	}
	if calls := drv.callCount(); calls != 1 {
		t.Fatalf("driver calls = %d, want 1", calls)
	}
}

func TestLinkResolutionTimeoutEvictsInflightWhenDriverIgnoresContext(t *testing.T) {
	reg := NewRegistry()
	drv := newProxyContextIgnoringDrive()
	reg.Set("115", drv)
	p := New(reg)
	p.resolveTimeout = 50 * time.Millisecond
	t.Cleanup(drv.release)

	errs := make(chan error, 1)
	go func() {
		errs <- p.WarmStreamLink(context.Background(), "115", "file-1", nil)
	}()
	select {
	case <-drv.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first link resolution did not start")
	}
	var err error
	select {
	case err = <-errs:
	case <-time.After(time.Second):
		t.Fatal("first link resolution did not honor proxy timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first WarmStreamLink error = %v, want context deadline exceeded", err)
	}

	key := linkCacheKey(drv, "115", "file-1", nil)
	p.cacheMu.Lock()
	_, stillInflight := p.inflight[key]
	p.cacheMu.Unlock()
	if stillInflight {
		t.Fatal("timed-out link resolution still present in inflight")
	}

	// The first provider request is still blocked. A new request must start a
	// fresh resolution instead of joining the poisoned inflight entry.
	link, err := p.getLink(context.Background(), drv, "115", "file-1", nil)
	if err != nil {
		t.Fatalf("second getLink: %v", err)
	}
	if link.URL != "https://cdn.example/fresh.mp4" {
		t.Fatalf("second URL = %q, want fresh provider result", link.URL)
	}
	if calls := drv.callCount(); calls != 2 {
		t.Fatalf("driver calls = %d, want 2", calls)
	}

	drv.release()
	select {
	case <-drv.firstReturned:
	case <-time.After(time.Second):
		t.Fatal("context-ignoring driver did not return after release")
	}
}

func TestWarmStreamLinkUsesTheServeStreamUserAgentCacheKey(t *testing.T) {
	reg := NewRegistry()
	drv := &proxyFakeDrive{kind: "p115"}
	reg.Set("115", drv)
	p := New(reg)
	header := http.Header{"User-Agent": {"Browser-A"}}

	if err := p.WarmStreamLink(context.Background(), "115", "file-1", header); err != nil {
		t.Fatalf("WarmStreamLink: %v", err)
	}
	requestP115(t, p, "115", "file-1", "Browser-A")

	if len(drv.calls) != 1 {
		t.Fatalf("link calls = %d, want one shared warm/play resolution", len(drv.calls))
	}
}

func requestP115(t *testing.T, p *Proxy, driveID, fileID, ua string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/p/stream/"+driveID+"/"+fileID, nil)
	req.Header.Set("User-Agent", ua)
	rr := httptest.NewRecorder()
	p.ServeStream(rr, req, driveID, fileID)
	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusFound)
	}
}

type proxyFakeDrive struct {
	kind  string
	calls []proxyFakeCall
}

type proxyFakeCall struct {
	fileID string
	ua     string
}

type proxyBlockingDrive struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newProxyBlockingDrive() *proxyBlockingDrive {
	return &proxyBlockingDrive{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (d *proxyBlockingDrive) Kind() string               { return "onedrive" }
func (d *proxyBlockingDrive) ID() string                 { return "blocking" }
func (d *proxyBlockingDrive) Init(context.Context) error { return nil }
func (d *proxyBlockingDrive) List(context.Context, string) ([]drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *proxyBlockingDrive) Stat(context.Context, string) (*drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *proxyBlockingDrive) StreamURL(ctx context.Context, _ string) (*drives.StreamLink, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	d.once.Do(func() { close(d.started) })
	select {
	case <-d.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &drives.StreamLink{
		URL:     "https://cdn.example/video.mp4",
		Expires: time.Now().Add(10 * time.Minute),
	}, nil
}
func (d *proxyBlockingDrive) Upload(context.Context, string, string, io.Reader, int64) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *proxyBlockingDrive) EnsureDir(context.Context, string) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *proxyBlockingDrive) RootID() string { return "0" }
func (d *proxyBlockingDrive) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

type proxyContextIgnoringDrive struct {
	mu            sync.Mutex
	calls         int
	releaseFirst  chan struct{}
	releaseOnce   sync.Once
	firstStarted  chan struct{}
	firstReturned chan struct{}
}

func newProxyContextIgnoringDrive() *proxyContextIgnoringDrive {
	return &proxyContextIgnoringDrive{
		releaseFirst:  make(chan struct{}),
		firstStarted:  make(chan struct{}),
		firstReturned: make(chan struct{}),
	}
}

func (d *proxyContextIgnoringDrive) Kind() string               { return "p115" }
func (d *proxyContextIgnoringDrive) ID() string                 { return "context-ignoring" }
func (d *proxyContextIgnoringDrive) Init(context.Context) error { return nil }
func (d *proxyContextIgnoringDrive) List(context.Context, string) ([]drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *proxyContextIgnoringDrive) Stat(context.Context, string) (*drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *proxyContextIgnoringDrive) StreamURL(context.Context, string) (*drives.StreamLink, error) {
	d.mu.Lock()
	d.calls++
	call := d.calls
	d.mu.Unlock()

	if call == 1 {
		close(d.firstStarted)
		<-d.releaseFirst
		close(d.firstReturned)
		return &drives.StreamLink{
			URL:                "https://cdn.example/stale.mp4",
			Expires:            time.Now().Add(10 * time.Minute),
			ClientRedirectSafe: true,
		}, nil
	}
	return &drives.StreamLink{
		URL:                "https://cdn.example/fresh.mp4",
		Expires:            time.Now().Add(10 * time.Minute),
		ClientRedirectSafe: true,
	}, nil
}
func (d *proxyContextIgnoringDrive) Upload(context.Context, string, string, io.Reader, int64) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *proxyContextIgnoringDrive) EnsureDir(context.Context, string) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *proxyContextIgnoringDrive) RootID() string { return "0" }
func (d *proxyContextIgnoringDrive) release() {
	d.releaseOnce.Do(func() { close(d.releaseFirst) })
}
func (d *proxyContextIgnoringDrive) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *proxyFakeDrive) Kind() string { return d.kind }
func (d *proxyFakeDrive) ID() string   { return "fake" }
func (d *proxyFakeDrive) Init(context.Context) error {
	return nil
}
func (d *proxyFakeDrive) List(context.Context, string) ([]drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *proxyFakeDrive) Stat(context.Context, string) (*drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *proxyFakeDrive) StreamURL(ctx context.Context, fileID string) (*drives.StreamLink, error) {
	return d.StreamURLWithHeader(ctx, fileID, nil)
}
func (d *proxyFakeDrive) StreamURLWithHeader(_ context.Context, fileID string, header http.Header) (*drives.StreamLink, error) {
	ua := header.Get("User-Agent")
	d.calls = append(d.calls, proxyFakeCall{fileID: fileID, ua: ua})
	return &drives.StreamLink{
		URL:                "https://cdn.example/" + fileID + "?ua=" + ua,
		Headers:            http.Header{"User-Agent": {ua}},
		Expires:            time.Now().Add(time.Minute),
		ClientRedirectSafe: redirectKindForTest(d.kind),
	}, nil
}
func (d *proxyFakeDrive) Upload(context.Context, string, string, io.Reader, int64) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *proxyFakeDrive) EnsureDir(context.Context, string) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *proxyFakeDrive) RootID() string { return "0" }

func TestServeStreamRedirectsPikPakWithoutUserAgentScopedCache(t *testing.T) {
	reg := NewRegistry()
	drv := &proxyFakePikPakDrive{}
	reg.Set("pikpak", drv)

	p := New(reg)

	// 三次请求，两个不同 UA。pikpak 取链不依赖 UA，所以缓存 key 只看
	// driveID/fileID，在统一缓存有效期内只会真正调用 driver 一次。
	requestPikPak(t, p, "pikpak", "file-1", "Browser-A")
	requestPikPak(t, p, "pikpak", "file-1", "Browser-B")
	requestPikPak(t, p, "pikpak", "file-1", "Browser-A")

	if drv.calls != 1 {
		t.Fatalf("link calls = %d, want 1 (cache must not be UA-scoped for pikpak)", drv.calls)
	}
}

func TestServeStreamPikPakSetsRedirectHeaders(t *testing.T) {
	reg := NewRegistry()
	drv := &proxyFakePikPakDrive{}
	reg.Set("pikpak", drv)

	p := New(reg)
	req := httptest.NewRequest(http.MethodGet, "/p/stream/pikpak/file-1", nil)
	req.Header.Set("User-Agent", "Browser-A")
	rr := httptest.NewRecorder()

	p.ServeStream(rr, req, "pikpak", "file-1")

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusFound)
	}
	if got := rr.Header().Get("Location"); got != "https://cdn.pikpak.example/file-1" {
		t.Fatalf("Location = %q", got)
	}
	if got := rr.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q", got)
	}
}

func TestServeStreamRedirectsOneDrive(t *testing.T) {
	reg := NewRegistry()
	drv := &proxyFakeSimpleDrive{
		kind: "onedrive",
		url:  "https://public.onedrive.example/video.mp4",
	}
	reg.Set("onedrive", drv)

	p := New(reg)
	req := httptest.NewRequest(http.MethodGet, "/p/stream/onedrive/file-1", nil)
	rr := httptest.NewRecorder()

	p.ServeStream(rr, req, "onedrive", "file-1")

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusFound)
	}
	if got := rr.Header().Get("Location"); got != "https://public.onedrive.example/video.mp4" {
		t.Fatalf("Location = %q", got)
	}
	if drv.calls != 1 {
		t.Fatalf("link calls = %d, want 1", drv.calls)
	}
}

func TestServeStreamRedirectsP123(t *testing.T) {
	reg := NewRegistry()
	drv := &proxyFakeSimpleDrive{
		kind: "p123",
		url:  "https://cdn.123pan.example/video.mp4",
	}
	reg.Set("p123", drv)

	p := New(reg)
	req := httptest.NewRequest(http.MethodGet, "/p/stream/p123/file-1", nil)
	rr := httptest.NewRecorder()

	p.ServeStream(rr, req, "p123", "file-1")

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusFound)
	}
	if got := rr.Header().Get("Location"); got != "https://cdn.123pan.example/video.mp4" {
		t.Fatalf("Location = %q", got)
	}
	if drv.calls != 1 {
		t.Fatalf("link calls = %d, want 1", drv.calls)
	}
}

func TestServeStreamRedirectsWopan(t *testing.T) {
	reg := NewRegistry()
	drv := &proxyFakeSimpleDrive{
		kind: "wopan",
		url:  "https://du.smartont.net:8445/openapi/download?fid=encoded",
	}
	reg.Set("wopan", drv)

	p := New(reg)
	req := httptest.NewRequest(http.MethodGet, "/p/stream/wopan/file-1", nil)
	rr := httptest.NewRecorder()

	p.ServeStream(rr, req, "wopan", "file-1")

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusFound)
	}
	if got := rr.Header().Get("Location"); got != "https://du.smartont.net:8445/openapi/download?fid=encoded" {
		t.Fatalf("Location = %q", got)
	}
	if drv.calls != 1 {
		t.Fatalf("link calls = %d, want 1", drv.calls)
	}
}

func TestServeStreamRedirectsGuangYaPan(t *testing.T) {
	reg := NewRegistry()
	drv := &proxyFakeSimpleDrive{
		kind: "guangyapan",
		url:  "https://cdn.guangyapan.example/video.mp4?sign=encoded",
	}
	reg.Set("guangyapan", drv)

	p := New(reg)
	req := httptest.NewRequest(http.MethodGet, "/p/stream/guangyapan/file-1", nil)
	rr := httptest.NewRecorder()

	p.ServeStream(rr, req, "guangyapan", "file-1")

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusFound)
	}
	if got := rr.Header().Get("Location"); got != "https://cdn.guangyapan.example/video.mp4?sign=encoded" {
		t.Fatalf("Location = %q", got)
	}
	if drv.calls != 1 {
		t.Fatalf("link calls = %d, want 1", drv.calls)
	}
}

func TestServeStreamProxiesWebDAVAuthorizationAndRange(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Basic dmlkZW86c2VjcmV0" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Range"); got != "bytes=2-5" {
			t.Errorf("Range = %q", got)
		}
		w.Header().Set("Content-Range", "bytes 2-5/10")
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "2345")
	}))
	t.Cleanup(upstream.Close)

	reg := NewRegistry()
	drv := &proxyFakeSimpleDrive{
		kind:    "webdav",
		url:     upstream.URL + "/dav/video.mp4",
		headers: http.Header{"Authorization": {"Basic dmlkZW86c2VjcmV0"}},
	}
	reg.Set("webdav", drv)

	p := New(reg)
	req := httptest.NewRequest(http.MethodGet, "/p/stream/webdav/video.mp4", nil)
	req.Header.Set("Range", "bytes=2-5")
	rr := httptest.NewRecorder()
	p.ServeStream(rr, req, "webdav", "/video.mp4")

	if rr.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusPartialContent)
	}
	if got := rr.Body.String(); got != "2345" {
		t.Fatalf("body = %q, want range bytes", got)
	}
	if got := rr.Header().Get("Location"); got != "" {
		t.Fatalf("Location = %q, WebDAV must stay behind the backend proxy", got)
	}
}

func TestServeStreamRelaysWebDAV302ToBrowser(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("backend followed the WebDAV redirect instead of relaying it")
		http.Error(w, "unexpected backend request", http.StatusInternalServerError)
	}))
	t.Cleanup(cdn.Close)

	openList := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Basic dmlkZW86c2VjcmV0" {
			t.Errorf("OpenList Authorization = %q", got)
		}
		if got := r.Header.Get("Cookie"); got != "dav_session=secret" {
			t.Errorf("OpenList Cookie = %q", got)
		}
		if got := r.Header.Get("Range"); got != "bytes=2-5" {
			t.Errorf("OpenList Range = %q, want bytes=2-5", got)
		}
		http.Redirect(w, r, cdn.URL+"/signed/video.mp4", http.StatusFound)
	}))
	t.Cleanup(openList.Close)

	reg := NewRegistry()
	reg.Set("webdav", &proxyFakeSimpleDrive{
		kind:                 "webdav",
		url:                  openList.URL + "/dav/video.mp4",
		passThroughRedirects: true,
		headers: http.Header{
			"Authorization": {"Basic dmlkZW86c2VjcmV0"},
			"Cookie":        {"dav_session=secret"},
			"User-Agent":    {"video-site-webdav"},
		},
	})

	p := New(reg)
	req := httptest.NewRequest(http.MethodGet, "/p/stream/webdav/video.mp4", nil)
	req.Header.Set("Range", "bytes=2-5")
	rr := httptest.NewRecorder()
	p.ServeStream(rr, req, "webdav", "/video.mp4")

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d body=%q, want %d", rr.Code, rr.Body.String(), http.StatusFound)
	}
	if got := rr.Header().Get("Location"); got != cdn.URL+"/signed/video.mp4" {
		t.Fatalf("Location = %q, want public CDN URL", got)
	}
	if got := rr.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q, want no-referrer", got)
	}
	if got := rr.Header().Get("Authorization"); got != "" {
		t.Fatalf("browser response leaked Authorization: %q", got)
	}
	if got := rr.Header().Get("Set-Cookie"); got != "" {
		t.Fatalf("browser response leaked upstream cookie: %q", got)
	}
}

func TestServeStreamRejectsUnsafeWebDAVRedirect(t *testing.T) {
	openList := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "file:///etc/passwd")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(openList.Close)

	reg := NewRegistry()
	reg.Set("webdav", &proxyFakeSimpleDrive{
		kind: "webdav",
		url:  openList.URL + "/dav/video.mp4",
		headers: http.Header{
			"Authorization": {"Basic secret"},
		},
		passThroughRedirects: true,
	})

	p := New(reg)
	req := httptest.NewRequest(http.MethodGet, "/p/stream/webdav/video.mp4", nil)
	rr := httptest.NewRecorder()
	p.ServeStream(rr, req, "webdav", "/video.mp4")

	if rr.Code != http.StatusFailedDependency {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusFailedDependency)
	}
}

func TestServeStreamServesLocalFilePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	reg := NewRegistry()
	drv := &proxyFakeSimpleDrive{
		kind: "localstorage",
		url:  path,
	}
	reg.Set("local", drv)

	p := New(reg)
	req := httptest.NewRequest(http.MethodGet, "/p/stream/local/file-1", nil)
	req.Header.Set("Range", "bytes=2-5")
	rr := httptest.NewRecorder()

	p.ServeStream(rr, req, "local", "file-1")

	if rr.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusPartialContent)
	}
	if got := rr.Body.String(); got != "2345" {
		t.Fatalf("body = %q, want range bytes", got)
	}
	if drv.calls != 1 {
		t.Fatalf("link calls = %d, want 1", drv.calls)
	}
}

func TestLocalFilePathWindowsDrive(t *testing.T) {
	// Unix 绝对路径在 Linux 上是合法本地文件，在 Windows 上不是
	unixAbsWant := false
	if runtime.GOOS != "windows" {
		unixAbsWant = true
	}
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"backslash", `E:\videos\file.mp4`, true},
		{"forward_slash", `E:/videos/file.mp4`, true},
		{"lowercase", `d:\file.mp4`, true},
		{"unix_abs", `/mnt/videos/file.mp4`, unixAbsWant},
		{"http_url", `http://example.com/file.mp4`, false},
		{"too_short", `E:`, false},
		{"no_separator", `E:file.mp4`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, _ := url.Parse(tc.raw)
			_, got := localFilePath(u, tc.raw)
			if got != tc.want {
				t.Fatalf("localFilePath(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func requestPikPak(t *testing.T, p *Proxy, driveID, fileID, ua string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/p/stream/"+driveID+"/"+fileID, nil)
	req.Header.Set("User-Agent", ua)
	rr := httptest.NewRecorder()
	p.ServeStream(rr, req, driveID, fileID)
	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusFound)
	}
}

// proxyFakePikPakDrive 故意不实现 streamURLWithHeader，
// 用来回归 pikpak 取链不带 UA 作用域、且走 302 的不变量。
type proxyFakePikPakDrive struct {
	calls int
}

func (d *proxyFakePikPakDrive) Kind() string { return "pikpak" }
func (d *proxyFakePikPakDrive) ID() string   { return "pikpak" }
func (d *proxyFakePikPakDrive) Init(context.Context) error {
	return nil
}
func (d *proxyFakePikPakDrive) List(context.Context, string) ([]drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *proxyFakePikPakDrive) Stat(context.Context, string) (*drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *proxyFakePikPakDrive) StreamURL(_ context.Context, fileID string) (*drives.StreamLink, error) {
	d.calls++
	return &drives.StreamLink{
		URL:                "https://cdn.pikpak.example/" + fileID,
		Headers:            http.Header{},
		Expires:            time.Now().Add(10 * time.Minute),
		ClientRedirectSafe: true,
	}, nil
}
func (d *proxyFakePikPakDrive) Upload(context.Context, string, string, io.Reader, int64) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *proxyFakePikPakDrive) EnsureDir(context.Context, string) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *proxyFakePikPakDrive) RootID() string { return "0" }

type proxyFakeSimpleDrive struct {
	kind                 string
	url                  string
	headers              http.Header
	passThroughRedirects bool
	calls                int
}

func (d *proxyFakeSimpleDrive) Kind() string { return d.kind }
func (d *proxyFakeSimpleDrive) ID() string   { return d.kind }
func (d *proxyFakeSimpleDrive) Init(context.Context) error {
	return nil
}
func (d *proxyFakeSimpleDrive) List(context.Context, string) ([]drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *proxyFakeSimpleDrive) Stat(context.Context, string) (*drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *proxyFakeSimpleDrive) StreamURL(context.Context, string) (*drives.StreamLink, error) {
	d.calls++
	return &drives.StreamLink{
		URL:                  d.url,
		Headers:              d.headers.Clone(),
		Expires:              time.Now().Add(10 * time.Minute),
		PassThroughRedirects: d.passThroughRedirects,
		ClientRedirectSafe:   redirectKindForTest(d.kind),
	}, nil
}

func redirectKindForTest(kind string) bool {
	switch kind {
	case "p115", "pikpak", "onedrive", "p123", "wopan", "guangyapan":
		return true
	default:
		return false
	}
}
func (d *proxyFakeSimpleDrive) Upload(context.Context, string, string, io.Reader, int64) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *proxyFakeSimpleDrive) EnsureDir(context.Context, string) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *proxyFakeSimpleDrive) RootID() string { return "0" }

func TestClientRedirectSafeRequiresAbsoluteHTTPURL(t *testing.T) {
	for _, link := range []*drives.StreamLink{
		nil,
		{URL: "https://cdn.example/file", ClientRedirectSafe: false},
		{URL: "javascript:alert(1)", ClientRedirectSafe: true},
		{URL: "/relative", ClientRedirectSafe: true},
	} {
		if clientRedirectSafe(link) {
			t.Fatalf("link %#v unexpectedly accepted", link)
		}
	}
	if !clientRedirectSafe(&drives.StreamLink{URL: "https://cdn.example/file", ClientRedirectSafe: true}) {
		t.Fatal("absolute HTTPS redirect was rejected")
	}
}
