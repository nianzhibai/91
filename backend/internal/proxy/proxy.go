package proxy

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sync"
	"time"

	"github.com/video-site/backend/internal/drives"
)

type streamURLWithHeader interface {
	StreamURLWithHeader(ctx context.Context, fileID string, header http.Header) (*drives.StreamLink, error)
}

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
	cacheMu sync.Mutex
	cache   map[string]cachedLink
	http    *http.Client
}

type cachedLink struct {
	link    *drives.StreamLink
	fetched time.Time
}

func New(r *Registry) *Proxy {
	return &Proxy{
		Registry: r,
		cache:    make(map[string]cachedLink),
		http: &http.Client{
			Timeout: 0, // 流式不设超时
		},
	}
}

func (p *Proxy) getLink(ctx context.Context, d drives.Drive, driveID, fileID string, header http.Header) (*drives.StreamLink, error) {
	key := linkCacheKey(d, driveID, fileID, header)

	p.cacheMu.Lock()
	if c, ok := p.cache[key]; ok {
		// 缓存 30 秒，且不超过 link.Expires
		if time.Since(c.fetched) < 30*time.Second && time.Now().Before(c.link.Expires) {
			p.cacheMu.Unlock()
			return c.link, nil
		}
	}
	p.cacheMu.Unlock()

	var (
		link *drives.StreamLink
		err  error
	)
	if h, ok := d.(streamURLWithHeader); ok {
		link, err = h.StreamURLWithHeader(ctx, fileID, header)
	} else {
		link, err = d.StreamURL(ctx, fileID)
	}
	if err != nil {
		return nil, err
	}
	p.cacheMu.Lock()
	p.cache[key] = cachedLink{link: link, fetched: time.Now()}
	p.cacheMu.Unlock()
	return link, nil
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
		http.Error(w, errDriveNotFound.Error(), errDriveNotFound.Code)
		return
	}

	link, err := p.getLink(r.Context(), d, driveID, fileID, r.Header)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if shouldRedirect(d) {
		redirect(w, r, link)
		return
	}
	p.serve(w, r, link)
}

// shouldRedirect 返回 true 时，/p/stream 不再反代视频字节，
// 而是用 302 让浏览器直连网盘 CDN。
//
// 只把"自己签名 URL 即可下载、不需要持久 Header 鉴权"的网盘放进来：
//   - p115：CDN 签名链接，UA 通过 streamURLWithHeader 在取链时使用，
//     302 之后浏览器用自己的 UA 直连，CDN 仍然认签名
//   - pikpak：与 OpenList 一致，WebContentLink / media link 都是自签 URL，
//     CDN 不校验请求头，直连可获得最佳带宽并避免占用 backend 出站
//   - onedrive：Microsoft Graph 返回的 @microsoft.graph.downloadUrl 是短期
//     免鉴权下载 URL，不需要后端继续代传视频字节
//   - p123：123网盘 download_info 返回的下载页会再跳 CDN；driver 已在后端
//     先解出最终 Location，浏览器可直接 302 到该短期地址
//   - wopan：联通网盘 GetDownloadUrlV2 返回的是短期直链，OpenList 也是直接
//     将该 URL 交给客户端使用；不需要后端持续代传视频字节
//   - guangyapan：光鸭 get_res_download_url 返回 signedURL / downloadUrl，
//     浏览器可直接访问，不需要后端持续代传视频字节
//
// 其余网盘（如夸克等）仍走反代，因为它们的下载
// 链接通常需要随请求带上后端持有的 Cookie / Authorization / Range
// 的特殊处理，浏览器拿不到这些上下文。
func shouldRedirect(d drives.Drive) bool {
	switch d.Kind() {
	case "p115", "pikpak", "onedrive", "p123", "wopan", "guangyapan":
		return true
	}
	return false
}

func redirect(w http.ResponseWriter, r *http.Request, link *drives.StreamLink) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "max-age=0, no-cache, no-store, must-revalidate")
	http.Redirect(w, r, link.URL, http.StatusFound)
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, link *drives.StreamLink) {
	// 构造上游请求
	u, err := url.Parse(link.URL)
	if err != nil {
		http.Error(w, "bad upstream url", http.StatusBadGateway)
		return
	}
	if localPath, ok := localFilePath(u, link.URL); ok {
		w.Header().Set("Cache-Control", "private, max-age=300")
		http.ServeFile(w, r, localPath)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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

	resp, err := p.http.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

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
