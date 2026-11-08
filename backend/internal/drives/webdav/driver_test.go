package webdav

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

func TestDriverListsStatsAndStreamsWithRange(t *testing.T) {
	dav := newTestDAV(t)
	dav.addDir("/library")
	dav.addDir("/library/影视")
	dav.addFile("/library/示例 影片.mp4", []byte("0123456789"))
	server := httptest.NewServer(dav)
	t.Cleanup(server.Close)

	drv := New(Config{
		ID:       "dav-main",
		BaseURL:  server.URL + "/dav",
		Username: testDAVUsername,
		Password: testDAVPassword,
		RootID:   "/library",
	})
	ctx := context.Background()
	if err := drv.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	entries, err := drv.List(ctx, drv.RootID())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %#v, want directory and file", entries)
	}
	if !entries[0].IsDir || entries[0].ID != "/library/影视" || entries[0].ParentID != "/library" {
		t.Fatalf("directory entry = %#v", entries[0])
	}
	if entries[1].IsDir || entries[1].ID != "/library/示例 影片.mp4" || entries[1].Size != 10 {
		t.Fatalf("file entry = %#v", entries[1])
	}

	entry, err := drv.Stat(ctx, entries[1].ID)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if entry.Name != "示例 影片.mp4" || entry.MimeType != "video/mp4" || entry.Size != 10 {
		t.Fatalf("stat entry = %#v", entry)
	}

	link, err := drv.StreamURL(ctx, entry.ID)
	if err != nil {
		t.Fatalf("StreamURL: %v", err)
	}
	wantURL := server.URL + "/dav/library/" + url.PathEscape("示例 影片.mp4")
	if link.URL != wantURL {
		t.Fatalf("stream URL = %q, want credential-free URL %q", link.URL, wantURL)
	}
	if !link.PassThroughRedirects {
		t.Fatal("WebDAV stream link must relay upstream redirects to the browser")
	}
	req, err := http.NewRequest(http.MethodGet, link.URL, nil)
	if err != nil {
		t.Fatalf("new stream request: %v", err)
	}
	req.Header = link.Headers.Clone()
	req.Header.Set("Range", "bytes=2-5")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream response: %v", err)
	}
	if resp.StatusCode != http.StatusPartialContent || string(body) != "2345" {
		t.Fatalf("range response = status %d body %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes 2-5/10" {
		t.Fatalf("Content-Range = %q", got)
	}

	headReq, err := http.NewRequest(http.MethodHead, link.URL, nil)
	if err != nil {
		t.Fatalf("new HEAD request: %v", err)
	}
	headReq.Header = link.Headers.Clone()
	headResp, err := http.DefaultClient.Do(headReq)
	if err != nil {
		t.Fatalf("HEAD request: %v", err)
	}
	defer headResp.Body.Close()
	if headResp.StatusCode != http.StatusOK || headResp.ContentLength != 10 {
		t.Fatalf("HEAD response = status %d length %d", headResp.StatusCode, headResp.ContentLength)
	}
}

func TestDriverCreatesDirectoriesUploadsRenamesAndRemoves(t *testing.T) {
	dav := newTestDAV(t)
	dav.addDir("/library")
	server := httptest.NewServer(dav)
	t.Cleanup(server.Close)

	drv := New(Config{
		ID:       "dav-main",
		BaseURL:  server.URL + "/dav/",
		Username: testDAVUsername,
		Password: testDAVPassword,
		RootID:   "/library",
	})
	ctx := context.Background()
	if err := drv.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	dirID, err := drv.EnsureDir(ctx, "Script Crawlers/测试")
	if err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if dirID != "/library/Script Crawlers/测试" {
		t.Fatalf("dir id = %q", dirID)
	}

	payload := []byte("uploaded-video")
	fileID, err := drv.Upload(ctx, dirID, "影片.mp4", bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if fileID != dirID+"/影片.mp4" {
		t.Fatalf("file id = %q", fileID)
	}
	if got := dav.fileData(fileID); !bytes.Equal(got, payload) {
		t.Fatalf("uploaded data = %q", got)
	}

	if err := drv.Rename(ctx, fileID, "重命名.mp4"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	renamedID := dirID + "/重命名.mp4"
	if dav.exists(fileID) || !dav.exists(renamedID) {
		t.Fatalf("rename state: old=%v new=%v", dav.exists(fileID), dav.exists(renamedID))
	}

	if err := drv.Remove(ctx, renamedID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if dav.exists(renamedID) {
		t.Fatal("removed WebDAV file still exists")
	}
	// Source deletion is idempotent so a stale tombstone can be purged safely.
	if err := drv.Remove(ctx, renamedID); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
}

func TestDriverRejectsUnsafePathsAndDirectoryRemoval(t *testing.T) {
	dav := newTestDAV(t)
	dav.addDir("/library")
	dav.addDir("/library/folder")
	server := httptest.NewServer(dav)
	t.Cleanup(server.Close)
	drv := New(Config{
		ID:       "dav-main",
		BaseURL:  server.URL + "/dav",
		Username: testDAVUsername,
		Password: testDAVPassword,
		RootID:   "/library",
	})

	if _, err := drv.Stat(context.Background(), "/outside/file.mp4"); err == nil || !strings.Contains(err.Error(), "outside configured root") {
		t.Fatalf("outside-root Stat error = %v", err)
	}
	if _, err := drv.Upload(context.Background(), drv.RootID(), "../escape.mp4", strings.NewReader("x"), 1); err == nil {
		t.Fatal("Upload accepted an unsafe file name")
	}
	if err := drv.Remove(context.Background(), "/library/folder"); err == nil || !strings.Contains(err.Error(), "refusing to remove directory") {
		t.Fatalf("directory Remove error = %v", err)
	}
	if err := drv.Remove(context.Background(), drv.RootID()); err == nil || !strings.Contains(err.Error(), "configured root") {
		t.Fatalf("root Remove error = %v", err)
	}

	bad := New(Config{ID: "bad", BaseURL: "ftp://example.com/dav", RootID: "/"})
	if err := bad.Init(context.Background()); err == nil || !strings.Contains(err.Error(), "scheme must be http or https") {
		t.Fatalf("invalid scheme error = %v", err)
	}
}

func TestDriverReportsAuthenticationAndRateLimitErrors(t *testing.T) {
	dav := newTestDAV(t)
	dav.addDir("/")
	server := httptest.NewServer(dav)
	t.Cleanup(server.Close)

	wrongPassword := New(Config{
		ID:       "dav-main",
		BaseURL:  server.URL + "/dav",
		Username: testDAVUsername,
		Password: "wrong",
		RootID:   "/",
	})
	if err := wrongPassword.Init(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("authentication error = %v", err)
	}

	rateLimitedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	t.Cleanup(rateLimitedServer.Close)
	rateLimited := New(Config{ID: "dav-rate", BaseURL: rateLimitedServer.URL, RootID: "/"})
	err := rateLimited.Init(context.Background())
	wait, ok := drives.RateLimitRetryAfter(err)
	if !ok || wait != 2*time.Second {
		t.Fatalf("rate limit = wait %s ok %v err %v", wait, ok, err)
	}
}

const (
	testDAVUsername = "91"
	testDAVPassword = "secret"
)

type testDAVNode struct {
	dir      bool
	data     []byte
	modified time.Time
}

type testDAVServer struct {
	t        *testing.T
	mu       sync.Mutex
	basePath string
	nodes    map[string]testDAVNode
}

func newTestDAV(t *testing.T) *testDAVServer {
	t.Helper()
	d := &testDAVServer{
		t:        t,
		basePath: "/dav",
		nodes:    make(map[string]testDAVNode),
	}
	d.addDir("/")
	return d
}

func (d *testDAVServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	username, password, ok := r.BasicAuth()
	if !ok || username != testDAVUsername || password != testDAVPassword {
		w.Header().Set("WWW-Authenticate", `Basic realm="test-webdav"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	remotePath, ok := d.remotePath(r.URL)
	if !ok {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case "PROPFIND":
		d.handlePropfind(w, r, remotePath)
	case http.MethodGet, http.MethodHead:
		d.handleRead(w, r, remotePath)
	case http.MethodPut:
		d.handlePut(w, r, remotePath)
	case "MKCOL":
		d.handleMkcol(w, remotePath)
	case "MOVE":
		d.handleMove(w, r, remotePath)
	case http.MethodDelete:
		d.handleDelete(w, remotePath)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (d *testDAVServer) remotePath(u *url.URL) (string, bool) {
	if u == nil {
		return "", false
	}
	p := path.Clean("/" + u.Path)
	if p == d.basePath {
		return "/", true
	}
	if !strings.HasPrefix(p, d.basePath+"/") {
		return "", false
	}
	return path.Clean(strings.TrimPrefix(p, d.basePath)), true
}

func (d *testDAVServer) handlePropfind(w http.ResponseWriter, r *http.Request, remotePath string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	node, ok := d.nodes[remotePath]
	if !ok {
		http.Error(w, "missing", http.StatusNotFound)
		return
	}
	paths := []string{remotePath}
	if r.Header.Get("Depth") == "1" && node.dir {
		for candidate := range d.nodes {
			if candidate != remotePath && path.Dir(candidate) == remotePath {
				paths = append(paths, candidate)
			}
		}
		sort.Strings(paths[1:])
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?><d:multistatus xmlns:d="DAV:">`)
	for _, itemPath := range paths {
		d.writePropResponse(w, itemPath, d.nodes[itemPath])
	}
	_, _ = io.WriteString(w, `</d:multistatus>`)
}

func (d *testDAVServer) writePropResponse(w io.Writer, remotePath string, node testDAVNode) {
	hrefPath := d.basePath
	if remotePath != "/" {
		hrefPath += remotePath
	}
	if node.dir && !strings.HasSuffix(hrefPath, "/") {
		hrefPath += "/"
	}
	href := (&url.URL{Path: hrefPath}).EscapedPath()
	name := path.Base(remotePath)
	if remotePath == "/" {
		name = "/"
	}
	resourceType := ""
	if node.dir {
		resourceType = `<d:collection/>`
	}
	contentType := ""
	if !node.dir {
		contentType = mime.TypeByExtension(path.Ext(name))
	}
	_, _ = fmt.Fprintf(w,
		`<d:response><d:href>%s</d:href><d:propstat><d:prop><d:displayname>%s</d:displayname><d:resourcetype>%s</d:resourcetype><d:getcontentlength>%d</d:getcontentlength><d:getlastmodified>%s</d:getlastmodified><d:getcontenttype>%s</d:getcontenttype><d:getetag>"test"</d:getetag></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`,
		xmlText(href), xmlText(name), resourceType, len(node.data), node.modified.UTC().Format(http.TimeFormat), xmlText(contentType))
}

func (d *testDAVServer) handleRead(w http.ResponseWriter, r *http.Request, remotePath string) {
	d.mu.Lock()
	node, ok := d.nodes[remotePath]
	d.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if node.dir {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	http.ServeContent(w, r, path.Base(remotePath), node.modified, bytes.NewReader(node.data))
}

func (d *testDAVServer) handlePut(w http.ResponseWriter, r *http.Request, remotePath string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	parent, ok := d.nodes[path.Dir(remotePath)]
	if !ok || !parent.dir {
		w.WriteHeader(http.StatusConflict)
		return
	}
	if _, exists := d.nodes[remotePath]; exists && r.Header.Get("If-None-Match") == "*" {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		d.t.Errorf("read PUT body: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	d.nodes[remotePath] = testDAVNode{data: data, modified: time.Now().UTC()}
	w.WriteHeader(http.StatusCreated)
}

func (d *testDAVServer) handleMkcol(w http.ResponseWriter, remotePath string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.nodes[remotePath]; exists {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	parent, ok := d.nodes[path.Dir(remotePath)]
	if !ok || !parent.dir {
		w.WriteHeader(http.StatusConflict)
		return
	}
	d.nodes[remotePath] = testDAVNode{dir: true, modified: time.Now().UTC()}
	w.WriteHeader(http.StatusCreated)
}

func (d *testDAVServer) handleMove(w http.ResponseWriter, r *http.Request, remotePath string) {
	destination, err := url.Parse(r.Header.Get("Destination"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	destinationPath, ok := d.remotePath(destination)
	if !ok {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	node, exists := d.nodes[remotePath]
	if !exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if _, exists := d.nodes[destinationPath]; exists && r.Header.Get("Overwrite") == "F" {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	d.nodes[destinationPath] = node
	delete(d.nodes, remotePath)
	w.WriteHeader(http.StatusCreated)
}

func (d *testDAVServer) handleDelete(w http.ResponseWriter, remotePath string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	node, ok := d.nodes[remotePath]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if node.dir {
		w.WriteHeader(http.StatusConflict)
		return
	}
	delete(d.nodes, remotePath)
	w.WriteHeader(http.StatusNoContent)
}

func (d *testDAVServer) addDir(remotePath string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nodes[path.Clean(remotePath)] = testDAVNode{dir: true, modified: time.Now().UTC()}
}

func (d *testDAVServer) addFile(remotePath string, data []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nodes[path.Clean(remotePath)] = testDAVNode{data: append([]byte(nil), data...), modified: time.Now().UTC()}
}

func (d *testDAVServer) exists(remotePath string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.nodes[path.Clean(remotePath)]
	return ok
}

func (d *testDAVServer) fileData(remotePath string) []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.nodes[path.Clean(remotePath)].data...)
}

func xmlText(value string) string {
	var out strings.Builder
	_ = xml.EscapeText(&out, []byte(value))
	return out.String()
}
