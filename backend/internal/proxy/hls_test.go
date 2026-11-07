package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

func testHLSSource(rawURL, playlist string) drives.PlaybackSource {
	u, _ := url.Parse(rawURL)
	return drives.PlaybackSource{
		ID: "cloud", Protocol: "hls", Status: "ready",
		Link:  &drives.StreamLink{URL: rawURL, Headers: http.Header{"User-Agent": {"provider-ua"}, "Referer": {"https://115.com/"}}, Expires: time.Now().Add(time.Minute)},
		Relay: &drives.HLSRelay{Playlist: playlist, AllowedHosts: []string{u.Host}},
	}
}

func serveTestHLS(p *Proxy, id, resource string, headers http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/p/hls/video/"+id+"/"+resource, nil)
	r.Header = headers
	w := httptest.NewRecorder()
	p.ServeHLS(w, r, "drive", "file", id, resource)
	return w
}

func TestHLSRelaysNestedPlaylistsKeysMapsAndRangesWithFixedHeaders(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.UserAgent() != "provider-ua" || r.Referer() != "https://115.com/" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Origin") != "" {
			t.Errorf("wrong upstream headers: %v", r.Header)
		}
		if r.Header.Get("Accept-Encoding") != "identity" {
			t.Error("HLS byte range must use the uncompressed representation")
		}
		switch r.URL.Path {
		case "/nested/video.m3u8":
			if r.URL.RawQuery != "signed=child" {
				t.Errorf("query=%q", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			io.WriteString(w, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"../key?signature=secret\"\n#EXT-X-MAP:URI=\"init\"\n#EXTINF:6,\n../segment?signature=secret\n#EXT-X-ENDLIST\n")
		case "/segment":
			if r.URL.RawQuery != "signature=secret" || r.Header.Get("Range") != "bytes=1-3" || r.Header.Get("If-Range") != "range-version" {
				t.Errorf("range/query lost: %s %v", r.URL, r.Header)
			}
			w.Header().Set("Content-Type", "video/mp2t")
			w.Header().Set("Content-Range", "bytes 1-3/5")
			w.Header().Set("Content-Length", "3")
			w.Header().Set("Set-Cookie", "provider=secret")
			w.WriteHeader(http.StatusPartialContent)
			io.WriteString(w, "123")
		case "/key", "/nested/init":
			io.WriteString(w, "key-or-map")
		default:
			t.Errorf("unexpected resource %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	p := New(NewRegistry())
	source := testHLSSource(upstream.URL+"/master.m3u8", "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",URI=\"nested/audio.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"a\"\nnested/video.m3u8?signed=child\n")
	id, _, err := p.StartHLSPlayback("drive", "file", source)
	if err != nil {
		t.Fatal(err)
	}
	root := serveTestHLS(p, id, "index.m3u8", nil)
	if root.Code != 200 || requests.Load() != 0 || strings.Contains(root.Body.String(), upstream.URL) || !strings.Contains(root.Body.String(), `GROUP-ID="a"`) {
		t.Fatalf("master response: %d %s", root.Code, root.Body.String())
	}
	rootLines := strings.Split(strings.TrimSpace(root.Body.String()), "\n")
	childID := rootLines[len(rootLines)-1][strings.LastIndex(rootLines[len(rootLines)-1], "/")+1:]
	child := serveTestHLS(p, id, childID+"?attacker=ignored", nil)
	if child.Code != 404 {
		t.Fatalf("unknown resource=%d", child.Code)
	}
	child = serveTestHLS(p, id, childID, http.Header{"Cookie": {"vs_admin=secret"}, "Authorization": {"Bearer viewer"}, "Origin": {"https://viewer.example"}, "User-Agent": {"viewer-ua"}})
	if child.Code != 200 || strings.Contains(child.Body.String(), "signature=secret") {
		t.Fatalf("child=%d %s", child.Code, child.Body.String())
	}
	// All three resource types are registered, including URI attributes.
	p.hlsMu.Lock()
	resources := make(map[string]string)
	for key, target := range p.hlsSessions[id].resources {
		resources[target.Path] = key
	}
	p.hlsMu.Unlock()
	for _, path := range []string{"/key", "/nested/init", "/segment"} {
		headers := http.Header{"Range": {"bytes=1-3"}, "If-Range": {"range-version"}}
		rr := serveTestHLS(p, id, resources[path], headers)
		if path == "/segment" {
			if rr.Code != 206 || rr.Body.String() != "123" || rr.Header().Get("Content-Range") != "bytes 1-3/5" || rr.Header().Get("Set-Cookie") != "" {
				t.Fatalf("segment=%d %s %v", rr.Code, rr.Body.String(), rr.Header())
			}
		} else if rr.Code != 200 || rr.Body.String() != "key-or-map" {
			t.Fatalf("resource=%s %d", path, rr.Code)
		}
	}
	if child.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("playlist was cacheable")
	}
}

func TestHLSRejectsUnsafeResourcesRedirectsAndAccountHeaders(t *testing.T) {
	var externalRequests atomic.Int32
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { externalRequests.Add(1) }))
	defer external.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, external.URL+"/private", http.StatusFound)
	}))
	defer upstream.Close()
	p := New(NewRegistry())
	for _, raw := range []string{external.URL + "/secret", "file:///etc/passwd", "https://user:secret@115.com/test", "//115.com.evil.example/key", "http://127.0.0.1:1/private", "{$secret}"} {
		_, _, err := p.StartHLSPlayback("drive", "file", testHLSSource(upstream.URL+"/master", "#EXTM3U\n"+raw+"\n"))
		if err == nil {
			t.Fatalf("accepted forbidden resource %q", raw)
		}
	}
	source := testHLSSource(upstream.URL+"/master", "#EXTM3U\nredirect\n")
	source.Link.Headers.Set("Cookie", "account-secret")
	if _, _, err := p.StartHLSPlayback("drive", "file", source); err == nil {
		t.Fatal("account headers accepted")
	}
	source.Link.Headers.Del("Cookie")
	source.Link.Headers["cookie"] = []string{"account-secret"}
	if _, _, err := p.StartHLSPlayback("drive", "file", source); err == nil {
		t.Fatal("noncanonical account headers accepted")
	}
	delete(source.Link.Headers, "cookie")
	id, _, err := p.StartHLSPlayback("drive", "file", source)
	if err != nil {
		t.Fatal(err)
	}
	serveTestHLS(p, id, "index.m3u8", nil)
	var resource string
	for key := range p.hlsSessions[id].resources {
		if key != "index.m3u8" {
			resource = key
		}
	}
	rr := serveTestHLS(p, id, resource, nil)
	if rr.Code != 502 || externalRequests.Load() != 0 || strings.Contains(rr.Body.String(), external.URL) {
		t.Fatalf("unsafe redirect: %d hits=%d body=%s", rr.Code, externalRequests.Load(), rr.Body.String())
	}
	for _, host := range []string{"115.com.evil.example", "evil115.com", "115.com:22"} {
		u, _ := url.Parse("https://" + host + "/test")
		root, _ := url.Parse("https://115.com/master")
		if allowedPlaybackURL(root, u, []string{".115.com"}) {
			t.Errorf("allowed %s", host)
		}
	}
}

func TestHLSSessionsAreScopedExpireAndInvalidateWithDrive(t *testing.T) {
	p := New(NewRegistry())
	source := testHLSSource("https://115.com/master", "#EXTM3U\nsegment\n")
	source.Link.Expires = time.Now().Add(time.Second)
	id, _, err := p.StartHLSPlayback("drive", "file", source)
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(p.hlsSessions[id].expires) < 29*time.Minute {
		t.Fatal("resolution cache deadline truncated a playing movie's session")
	}
	p.hlsSessions[id].expires = time.Now().Add(time.Second)
	serveTestHLS(p, id, "index.m3u8", nil)
	if time.Until(p.hlsSessions[id].expires) < 29*time.Minute {
		t.Fatal("active HLS session was not renewed")
	}
	for _, scope := range [][2]string{{"other", "file"}, {"drive", "other"}} {
		w := httptest.NewRecorder()
		p.ServeHLS(w, httptest.NewRequest("GET", "/hls", nil), scope[0], scope[1], id, "index.m3u8")
		if w.Code != 404 {
			t.Fatalf("wrong scope returned %d", w.Code)
		}
	}
	p.hlsSessions[id].expires = time.Now().Add(-time.Second)
	if w := serveTestHLS(p, id, "index.m3u8", nil); w.Code != 410 {
		t.Fatalf("expired=%d", w.Code)
	}
	id, _, err = p.StartHLSPlayback("drive", "file", source)
	if err != nil {
		t.Fatal(err)
	}
	p.InvalidateDrive("drive")
	if w := serveTestHLS(p, id, "index.m3u8", nil); w.Code != 410 || p.hlsResources != 0 {
		t.Fatalf("invalidated=%d resources=%d", w.Code, p.hlsResources)
	}
	for i := 0; i < hlsMaxSessions+1; i++ {
		if _, _, err := p.StartHLSPlayback("drive", fmt.Sprint(i), source); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.hlsSessions) != hlsMaxSessions || p.hlsResources != hlsMaxSessions {
		t.Fatal("unbounded HLS sessions")
	}
}

func TestHLSRequestCancellationAndOversizedPlaylist(t *testing.T) {
	started := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/large" {
			w.Header().Set("Content-Type", "application/x-mpegURL")
			io.WriteString(w, "#EXTM3U\n"+strings.Repeat("x", hlsMaxPlaylistBytes))
			return
		}
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer upstream.Close()
	p := New(NewRegistry())
	source := testHLSSource(upstream.URL+"/master", "#EXTM3U\nwait\nlarge\n")
	id, _, err := p.StartHLSPlayback("drive", "file", source)
	if err != nil {
		t.Fatal(err)
	}
	serveTestHLS(p, id, "index.m3u8", nil)
	resources := map[string]string{}
	for key, target := range p.hlsSessions[id].resources {
		resources[target.Path] = key
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.ServeHLS(httptest.NewRecorder(), httptest.NewRequest("GET", "/hls", nil).WithContext(ctx), "drive", "file", id, resources["/wait"])
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("request ignored cancellation")
	}
	if w := serveTestHLS(p, id, resources["/large"], nil); w.Code != 502 {
		t.Fatalf("large playlist=%d", w.Code)
	}
}

func TestHLSUsesRotatedServerCredentialsWithoutRetiringActiveSessions(t *testing.T) {
	var credential atomic.Value
	credential.Store("first")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "account="+credential.Load().(string) {
			t.Errorf("stale or viewer cookie: %s", r.Header.Get("Cookie"))
		}
		io.WriteString(w, "segment")
	}))
	defer upstream.Close()
	p := New(NewRegistry())
	source := testHLSSource(upstream.URL+"/index.m3u8", "#EXTM3U\n#EXTINF:5,\nsegment\n#EXT-X-ENDLIST\n")
	source.Relay.Authorize = func(r *http.Request) { r.Header.Set("Cookie", "account="+credential.Load().(string)) }
	id, _, err := p.StartHLSPlayback("drive", "file", source)
	if err != nil {
		t.Fatal(err)
	}
	root := serveTestHLS(p, id, "index.m3u8", nil)
	var resource string
	for _, line := range strings.Split(root.Body.String(), "\n") {
		if strings.Contains(line, "/p/hls/") {
			resource = line[strings.LastIndex(line, "/")+1:]
			break
		}
	}
	if resource == "" {
		t.Fatal("segment not registered")
	}
	for _, value := range []string{"first", "rotated"} {
		credential.Store(value)
		p.InvalidateDriveLinks("drive")
		rr := serveTestHLS(p, id, resource, http.Header{"Cookie": {"viewer-secret"}})
		if rr.Code != 200 || rr.Body.String() != "segment" {
			t.Fatalf("rotation killed session: %d %s", rr.Code, rr.Body.String())
		}
	}
	p.InvalidateDrive("drive")
	if rr := serveTestHLS(p, id, "index.m3u8", nil); rr.Code != 410 {
		t.Fatalf("remount retained session: %d", rr.Code)
	}
}
