package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/video-site/backend/internal/drives"
)

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
		URL:     "https://cdn.example/" + fileID + "?ua=" + ua,
		Headers: http.Header{"User-Agent": {ua}},
		Expires: time.Now().Add(time.Minute),
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

	// 三次请求，两个不同 UA。pikpak 取链不依赖 UA，所以缓存 key 只看 driveID/fileID，
	// 30 秒内只会真正调用 driver 一次。
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
		URL:     "https://cdn.pikpak.example/" + fileID,
		Headers: http.Header{},
		Expires: time.Now().Add(10 * time.Minute),
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
	kind  string
	url   string
	calls int
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
		URL:     d.url,
		Headers: http.Header{},
		Expires: time.Now().Add(10 * time.Minute),
	}, nil
}
func (d *proxyFakeSimpleDrive) Upload(context.Context, string, string, io.Reader, int64) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *proxyFakeSimpleDrive) EnsureDir(context.Context, string) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *proxyFakeSimpleDrive) RootID() string { return "0" }
