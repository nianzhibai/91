package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/video-site/backend/internal/drives"
)

type filePlaybackDrive struct {
	drives.Drive
	sources []drives.PlaybackSource
	calls   int
}

func (d *filePlaybackDrive) Kind() string { return "googledrive" }
func (d *filePlaybackDrive) PlaybackSources(context.Context, string) ([]drives.PlaybackSource, error) {
	d.calls++
	return d.sources, nil
}

func filePlaybackProxy(rawURL string) (*Proxy, *filePlaybackDrive) {
	u, _ := url.Parse(rawURL)
	d := &filePlaybackDrive{sources: []drives.PlaybackSource{{
		ID: "cloud-22", Kind: "transcoded", Protocol: "file", Status: "ready", MediaType: "video/mp4",
		Link:      &drives.StreamLink{URL: rawURL, Expires: time.Now().Add(time.Hour)},
		FileRelay: &drives.FileRelay{AllowedHosts: []string{u.Host}},
	}}}
	r := NewRegistry()
	r.Set("google", d)
	return New(r), d
}

func TestPlaybackFileRelaysRangesAndHeadWithoutViewerCredentials(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sig") != "upstream" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.UserAgent() != "drive-web-client" {
			t.Error("viewer parameters or credentials reached media upstream")
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Set-Cookie", "secret=value")
		if r.Header.Get("Range") == "bytes=99-" {
			w.Header().Set("Content-Range", "bytes */10")
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if r.Header.Get("Range") != "bytes=2-4" || r.Header.Get("If-Range") != `"etag"` {
			t.Error("range validation headers were not forwarded")
		}
		w.Header().Set("Content-Range", "bytes 2-4/10")
		w.Header().Set("Content-Length", "3")
		w.WriteHeader(http.StatusPartialContent)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte("234"))
		}
	}))
	defer upstream.Close()
	p, d := filePlaybackProxy(upstream.URL + "/movie?sig=upstream")
	d.sources[0].Link.Headers = http.Header{"User-Agent": {"drive-web-client"}}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		r := httptest.NewRequest(method, "/p/playback/video/cloud-22?sig=attacker&url=https://evil.example", nil)
		r.Header.Set("Range", "bytes=2-4")
		r.Header.Set("If-Range", `"etag"`)
		r.Header.Set("Cookie", "vs_admin=viewer-session")
		r.Header.Set("Authorization", "site-secret")
		w := httptest.NewRecorder()
		p.ServePlaybackFile(w, r, "google", "file", "cloud-22")
		if w.Code != 206 || w.Header().Get("Content-Range") != "bytes 2-4/10" || w.Header().Get("Set-Cookie") != "" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("status=%d headers=%v", w.Code, w.Header())
		}
		if method == "GET" && w.Body.String() != "234" || method == "HEAD" && w.Body.Len() != 0 {
			t.Fatalf("%s body=%q", method, w.Body.String())
		}
	}
	r := httptest.NewRequest("GET", "/p/playback/video/cloud-22", nil)
	r.Header.Set("Range", "bytes=99-")
	w := httptest.NewRecorder()
	p.ServePlaybackFile(w, r, "google", "file", "cloud-22")
	if w.Code != 416 || w.Header().Get("Content-Range") != "bytes */10" {
		t.Fatalf("range failure=%d %v", w.Code, w.Header())
	}
	if d.calls != 1 {
		t.Fatalf("range requests did not reuse playback resolution: %d", d.calls)
	}
}

func TestPlaybackFileRedirectsOnlyWhenTheBrowserCanUseTheLink(t *testing.T) {
	var mediaCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaCalls.Add(1)
		if r.UserAgent() != "browser-a" || r.Header.Get("Cookie") != "" {
			t.Error("relay lost the source UA or forwarded site credentials")
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(206)
		_, _ = w.Write([]byte("media"))
	}))
	defer upstream.Close()
	p, d := filePlaybackProxy(upstream.URL + "/movie?sig=private")
	d.sources[0].Link.ClientRedirectSafe = true
	d.sources[0].Link.Headers = http.Header{"User-Agent": {"browser-a"}}
	request := func(method, path, ua string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("User-Agent", ua)
		r.Header.Set("Cookie", "site-secret")
		w := httptest.NewRecorder()
		p.ServePlaybackFile(w, r, "google", "file", "cloud-22")
		return w
	}
	for _, method := range []string{"GET", "HEAD"} {
		w := request(method, "/p/playback/video/cloud-22", "browser-a")
		if w.Code != 302 || w.Header().Get("Location") != d.sources[0].Link.URL || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") || w.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("invalid redirect: %d %v", w.Code, w.Header())
		}
	}
	if mediaCalls.Load() != 0 {
		t.Fatal("302 delivery fetched upstream media")
	}
	if w := request("GET", "/p/playback/video/cloud-22?tripleScreenRelay=1", "browser-a"); w.Code != 206 || w.Body.String() != "media" {
		t.Fatalf("forced relay=%d %s", w.Code, w.Body.String())
	}
	p.SetAllowForcedRelay(false)
	if w := request("GET", "/p/playback/video/cloud-22?tripleScreenRelay=1", "browser-a"); w.Code != 302 {
		t.Fatalf("disabled forced relay=%d", w.Code)
	}
	if w := request("GET", "/p/playback/video/cloud-22", "browser-b"); w.Code != 206 {
		t.Fatalf("mismatched source UA was redirected: %d", w.Code)
	}
	d.sources[0].Link.Headers.Set("X-Required", "server-only")
	if w := request("GET", "/p/playback/video/cloud-22", "browser-a"); w.Code != 206 {
		t.Fatalf("required server header was dropped: %d", w.Code)
	}
	if mediaCalls.Load() != 3 {
		t.Fatalf("unexpected media reads: %d", mediaCalls.Load())
	}
}

func TestPlaybackFileRejectsUnsafePolicyAndRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unsafe destination was fetched") }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer origin.Close()
	for _, mode := range []string{"redirect", "host", "credential", "lowercase-credential"} {
		t.Run(mode, func(t *testing.T) {
			p, d := filePlaybackProxy(origin.URL)
			if mode == "host" {
				d.sources[0].FileRelay.AllowedHosts = []string{"safe.example"}
			}
			if mode == "credential" {
				d.sources[0].Link.Headers = http.Header{"Cookie": []string{"account-secret"}}
			}
			if mode == "lowercase-credential" {
				d.sources[0].Link.Headers = http.Header{"authorization": []string{"account-secret"}}
			}
			w := httptest.NewRecorder()
			p.ServePlaybackFile(w, httptest.NewRequest("GET", "/p/playback/video/cloud-22", nil), "google", "file", "cloud-22")
			if w.Code != 502 || strings.Contains(w.Body.String(), "account-secret") || w.Header().Get("Location") != "" {
				t.Fatalf("unsafe response %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestPlaybackFileSignedRejectionPermitsBoundedRefresh(t *testing.T) {
	for _, status := range []int{401, 403, 404, 410} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte("signed-secret"))
			}))
			defer upstream.Close()
			p, _ := filePlaybackProxy(upstream.URL)
			w := httptest.NewRecorder()
			p.ServePlaybackFile(w, httptest.NewRequest("GET", "/p/playback/video/cloud-22", nil), "google", "file", "cloud-22")
			var payload map[string]string
			_ = json.Unmarshal(w.Body.Bytes(), &payload)
			if w.Code != 410 || payload["code"] != "playback_source_expired" || strings.Contains(w.Body.String(), "signed-secret") {
				t.Fatalf("response %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestPlaybackFileCancellationStopsUpstream(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(stopped)
	}))
	defer upstream.Close()
	p, _ := filePlaybackProxy(upstream.URL)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { p.ServePlaybackFile(w, r, "google", "file", "cloud-22") }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
	done := make(chan error, 1)
	go func() {
		resp, err := server.Client().Do(r)
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
		cancel()
	case <-ctx.Done():
		t.Fatal("upstream did not start")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("request error %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream remained active")
	}
}

func TestPlaybackFileAbortsTruncatedMediaInsteadOfCompletingIt(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		_, _ = w.Write([]byte("123"))
	}))
	defer upstream.Close()
	p, _ := filePlaybackProxy(upstream.URL)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { p.ServePlaybackFile(w, r, "google", "file", "cloud-22") }))
	defer server.Close()
	resp, err := server.Client().Get(server.URL)
	if err == nil {
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
	}
	if err == nil {
		t.Fatal("truncated media completed successfully")
	}
}
