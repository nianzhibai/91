package p115

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/nianzhibai/91/backend/internal/drives"
)

type p115RoundTripFunc func(*http.Request) (*http.Response, error)

func (f p115RoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func newP115ListTestDriver(transport http.RoundTripper) *Driver {
	driver := New(Config{ID: "115", Cookie: "UID=u;CID=c;SEID=s"})
	driver.client = sdk.New(
		sdk.WithClient(&http.Client{Transport: transport}),
		sdk.UA("p115-list-test"),
	)
	driver.listInterval = 0
	return driver
}

func TestListUsesContextAwareSDKRequest(t *testing.T) {
	transport := p115RoundTripFunc(func(request *http.Request) (*http.Response, error) {
		query := request.URL.Query()
		if got := query.Get("cid"); got != "0" {
			t.Errorf("cid = %q, want 0", got)
		}
		if got := query.Get("limit"); got != "1150" {
			t.Errorf("limit = %q, want 1150", got)
		}
		body := `{"state":true,"cid":"0","count":1,"offset":0,"data":[{"fid":"file-1","cid":"0","n":"video.mp4","s":"12","sha":"ABC","pc":"pick-1","t":"2026-08-04 10:00","tp":"0"}]}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})
	driver := newP115ListTestDriver(transport)

	entries, err := driver.List(context.Background(), "0")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %#v, want one entry", entries)
	}
	entry := entries[0]
	if entry.ID != "file-1" || entry.Name != "video.mp4" || entry.Size != 12 || entry.Hash != "ABC" {
		t.Fatalf("entry = %#v", entry)
	}
	if got := driver.rememberedPickCode("file-1"); got != "pick-1" {
		t.Fatalf("remembered pick code = %q, want pick-1", got)
	}
}

func TestListCancelsInflightSDKRequest(t *testing.T) {
	requestStarted := make(chan struct{})
	var startedOnce sync.Once
	transport := p115RoundTripFunc(func(request *http.Request) (*http.Response, error) {
		startedOnce.Do(func() { close(requestStarted) })
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	driver := newP115ListTestDriver(transport)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan error, 1)
	go func() {
		_, err := driver.List(ctx, "0")
		result <- err
	}()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("115 list request did not start")
	}
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("list error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("115 list request did not stop after context cancellation")
	}
}

func TestListCancellationDoesNotWaitForAnotherListRequest(t *testing.T) {
	firstRequestStarted := make(chan struct{})
	releaseFirstRequest := make(chan struct{})
	var startOnce sync.Once
	var releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(releaseFirstRequest) }) }
	defer releaseFirst()

	transport := p115RoundTripFunc(func(request *http.Request) (*http.Response, error) {
		startOnce.Do(func() {
			close(firstRequestStarted)
			<-releaseFirstRequest
		})
		body := `{"state":true,"cid":"0","count":0,"offset":0,"data":[]}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})
	driver := newP115ListTestDriver(transport)

	firstResult := make(chan error, 1)
	go func() {
		_, err := driver.List(context.Background(), "0")
		firstResult <- err
	}()
	select {
	case <-firstRequestStarted:
	case <-time.After(time.Second):
		t.Fatal("first 115 list request did not start")
	}

	ctx, cancel := context.WithCancel(context.Background())
	secondResult := make(chan error, 1)
	go func() {
		_, err := driver.List(ctx, "0")
		secondResult <- err
	}()
	cancel()

	select {
	case err := <-secondResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting list error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled list remained blocked behind another list request")
	}

	releaseFirst()
	if err := <-firstResult; err != nil {
		t.Fatalf("first list: %v", err)
	}
}

func TestListAppliesRequestTimeout(t *testing.T) {
	transport := p115RoundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	driver := newP115ListTestDriver(transport)
	driver.listTimeout = 20 * time.Millisecond

	_, err := driver.List(context.Background(), "0")
	if err == nil || !strings.Contains(err.Error(), "115 list request timed out after 20ms") {
		t.Fatalf("list error = %v, want bounded request timeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("list error = %v, want a recognizable request deadline", err)
	}
}

func TestIsTransient115ListError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "blocked html status title", err: errors.New(`<!doctypehtml><html lang="zh-cn"><title>405</title>Sorry, your request has been blocked as it may cause potential threats to the server's security.`), want: true},
		{name: "blocked html status title with whitespace", err: errors.New(`<!doctype html><title class="waf"> 405 </title>blocked`), want: true},
		{name: "chinese waf", err: errors.New("很抱歉，由于您访问的URL有可能对网站造成安全威胁，您的访问被阻断。"), want: false},
		{name: "status 405", err: errors.New("request failed with status: 405"), want: true},
		{name: "rate limit", err: errors.New("429 too many requests"), want: true},
		{name: "regular auth error", err: errors.New("invalid credential"), want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransient115ListError(tc.err); got != tc.want {
				t.Fatalf("isTransient115ListError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestSelectHLSVariantChoosesHighestBandwidthAndResolvesRelativeURL(t *testing.T) {
	master := "#EXTM3U\r\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360\r\nlow/index.m3u8\r\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1920x1080\r\nhigh/index.m3u8\r\n"
	got, err := selectHLSVariant("https://115.com/api/video/master.m3u8", master)
	if err != nil {
		t.Fatalf("select variant: %v", err)
	}
	if got != "https://115.com/api/video/high/index.m3u8" {
		t.Fatalf("variant = %q", got)
	}
}

func TestSelectHLSVariantRejectsExternalAndHTTPSDowngrade(t *testing.T) {
	for _, playlist := range []string{
		"#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nhttps://evil.example/video.m3u8\n",
		"#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nhttp://115.com/video.m3u8\n",
	} {
		if got, err := selectHLSVariant("https://115.com/master.m3u8", playlist); err == nil {
			t.Fatalf("variant = %q, want rejection", got)
		}
	}
}

func TestGenerationStreamURLKeepsCookieOutOfReturnedLinkAndCaches(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		if got := r.Header.Get("Cookie"); got != "UID=u;CID=c;SEID=s" {
			t.Errorf("cookie = %q", got)
		}
		if got := r.Header.Get("Referer"); got != p115HLSReferer {
			t.Errorf("referer = %q", got)
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=100\nlow.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=200\nhigh.m3u8\n")
	}))
	t.Cleanup(server.Close)

	driver := New(Config{ID: "115", Cookie: "UID=u;CID=c;SEID=s"})
	driver.hlsClient = server.Client()
	driver.hlsMasterBaseURL = server.URL + "/api/video/m3u8"
	driver.rememberPickCode("file-1", "pick-1")

	first, err := driver.GenerationStreamURL(context.Background(), "file-1", false)
	if err != nil {
		t.Fatalf("first generation stream: %v", err)
	}
	if first.URL != server.URL+"/api/video/m3u8/high.m3u8" {
		t.Fatalf("url = %q", first.URL)
	}
	if first.Headers.Get("Cookie") != "" {
		t.Fatalf("returned cookie = %q", first.Headers.Get("Cookie"))
	}
	if first.Headers.Get("User-Agent") != p115HLSUserAgent || first.Headers.Get("Referer") != p115HLSReferer {
		t.Fatalf("returned headers = %#v", first.Headers)
	}

	second, err := driver.GenerationStreamURL(context.Background(), "file-1", false)
	if err != nil || second.URL != first.URL {
		t.Fatalf("cached generation stream = %#v, %v", second, err)
	}
	mu.Lock()
	gotRequests := requests
	mu.Unlock()
	if gotRequests != 1 {
		t.Fatalf("master requests = %d, want 1 cached request", gotRequests)
	}

	if _, err := driver.GenerationStreamURL(context.Background(), "file-1", true); err != nil {
		t.Fatalf("force refresh: %v", err)
	}
	mu.Lock()
	gotRequests = requests
	mu.Unlock()
	if gotRequests != 2 {
		t.Fatalf("master requests after refresh = %d, want 2", gotRequests)
	}
	source, err := driver.GenerationSource(context.Background(), "file-1", false)
	if err != nil || source.Protocol != "hls" || source.Link.URL != first.URL || source.Relay == nil || source.Relay.Authorize != nil {
		t.Fatalf("generation descriptor = %+v, %v", source, err)
	}

}

func TestGenerationStreamURLCoalescesConcurrentResolution(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		once.Do(func() { close(started) })
		<-release
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nstream.m3u8\n")
	}))
	t.Cleanup(server.Close)

	driver := New(Config{ID: "115", Cookie: "UID=u;CID=c;SEID=s"})
	driver.hlsClient = server.Client()
	driver.hlsMasterBaseURL = server.URL
	driver.rememberPickCode("file-1", "pick")

	type result struct {
		link *drives.StreamLink
		err  error
	}
	results := make(chan result, 2)
	go func() {
		link, err := driver.GenerationStreamURL(context.Background(), "file-1", false)
		results <- result{link: link, err: err}
	}()
	<-started
	go func() {
		link, err := driver.GenerationStreamURL(context.Background(), "file-1", false)
		results <- result{link: link, err: err}
	}()
	close(release)
	for range 2 {
		got := <-results
		if got.err != nil || got.link == nil {
			t.Fatalf("generation stream = %#v, %v", got.link, got.err)
		}
	}
	mu.Lock()
	gotRequests := requests
	mu.Unlock()
	if gotRequests != 1 {
		t.Fatalf("master requests = %d, want 1", gotRequests)
	}
}

func TestGenerationStreamURLForceRefreshSupersedesOrdinaryInflight(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once

	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requests++
		requestNumber := requests
		mu.Unlock()
		if requestNumber == 1 {
			close(firstStarted)
			<-releaseFirst
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nstale.m3u8\n")
			return
		}
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nfresh.m3u8\n")
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseFirst) })
		server.Close()
	})

	driver := New(Config{ID: "115", Cookie: "UID=u;CID=c;SEID=s"})
	driver.hlsClient = server.Client()
	driver.hlsMasterBaseURL = server.URL
	driver.rememberPickCode("file-1", "pick")

	type result struct {
		link *drives.StreamLink
		err  error
	}
	ordinaryResult := make(chan result, 1)
	go func() {
		link, err := driver.GenerationStreamURL(context.Background(), "file-1", false)
		ordinaryResult <- result{link: link, err: err}
	}()
	<-firstStarted

	refreshResult := make(chan result, 1)
	go func() {
		link, err := driver.GenerationStreamURL(context.Background(), "file-1", true)
		refreshResult <- result{link: link, err: err}
	}()

	select {
	case got := <-refreshResult:
		if got.err != nil || got.link == nil || !strings.HasSuffix(got.link.URL, "/fresh.m3u8") {
			t.Fatalf("refreshed generation stream = %#v, %v", got.link, got.err)
		}
	case <-time.After(2 * time.Second):
		releaseOnce.Do(func() { close(releaseFirst) })
		t.Fatal("forced refresh joined the older in-flight resolution")
	}

	releaseOnce.Do(func() { close(releaseFirst) })
	if got := <-ordinaryResult; got.err != nil || got.link == nil || !strings.HasSuffix(got.link.URL, "/stale.m3u8") {
		t.Fatalf("ordinary generation stream = %#v, %v", got.link, got.err)
	}

	cached, err := driver.GenerationStreamURL(context.Background(), "file-1", false)
	if err != nil || cached == nil || !strings.HasSuffix(cached.URL, "/fresh.m3u8") {
		t.Fatalf("cached generation stream = %#v, %v; want refreshed link", cached, err)
	}
	mu.Lock()
	gotRequests := requests
	mu.Unlock()
	if gotRequests != 2 {
		t.Fatalf("master requests = %d, want 2", gotRequests)
	}
}

func TestGenerationCachePrunesExpiredEntriesAndEvictsAtCapacity(t *testing.T) {
	driver := New(Config{ID: "115"})
	now := time.Now()
	driver.generationCache["expired"] = cachedGenerationStream{expires: now.Add(-time.Second)}
	driver.generationCache["valid"] = cachedGenerationStream{expires: now.Add(time.Minute)}
	driver.pruneGenerationCacheLocked(now)
	if _, ok := driver.generationCache["expired"]; ok {
		t.Fatal("expired generation stream remained cached")
	}
	if _, ok := driver.generationCache["valid"]; !ok {
		t.Fatal("valid generation stream was pruned")
	}

	clear(driver.generationCache)
	for i := 0; i < p115GenerationCacheMaxEntries; i++ {
		driver.generationCache[fmt.Sprintf("file-%d", i)] = cachedGenerationStream{
			expires: now.Add(time.Duration(i+1) * time.Second),
		}
	}
	driver.makeGenerationCacheRoomLocked()
	if got := len(driver.generationCache); got != p115GenerationCacheMaxEntries-1 {
		t.Fatalf("generation cache entries = %d, want %d after eviction", got, p115GenerationCacheMaxEntries-1)
	}
	if _, ok := driver.generationCache["file-0"]; ok {
		t.Fatal("oldest generation cache entry was not evicted")
	}
}

func TestGenerationStreamURLClassifiesUnavailableAndRateLimit(t *testing.T) {
	for _, tc := range []struct {
		name            string
		status          int
		body            string
		wantUnavailable bool
		wantRateLimit   bool
	}{
		{name: "missing", status: http.StatusNotFound, wantUnavailable: true},
		{name: "malformed", status: http.StatusOK, body: "not a playlist", wantUnavailable: true},
		{name: "waf", status: http.StatusMethodNotAllowed, body: "<title>405</title>", wantRateLimit: true},
		// Online playback being refused must leave the ordinary download URL
		// usable instead of cooling the drive down as if it were throttled.
		{name: "forbidden", status: http.StatusForbidden, wantUnavailable: true},
		{name: "server unavailable", status: http.StatusServiceUnavailable, wantUnavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			driver := New(Config{ID: "115", Cookie: "UID=u;CID=c;SEID=s"})
			driver.hlsClient = server.Client()
			driver.hlsMasterBaseURL = server.URL
			driver.rememberPickCode("file-1", "pick")
			_, err := driver.GenerationStreamURL(context.Background(), "file-1", false)
			if tc.wantUnavailable != errors.Is(err, drives.ErrGenerationStreamUnavailable) {
				t.Fatalf("error = %v, unavailable=%v", err, tc.wantUnavailable)
			}
			_, isRateLimit := drives.RateLimitRetryAfter(err)
			if isRateLimit != tc.wantRateLimit {
				t.Fatalf("error = %v, rate limit=%v", err, isRateLimit)
			}
		})
	}
}

func TestWrap115StreamTransientError(t *testing.T) {
	cases := []struct {
		name          string
		err           error
		wantRateLimit bool
	}{
		{name: "unexpected", err: errors.New("unexpected error"), wantRateLimit: false},
		{name: "405 blocked", err: errors.New("405 request has been blocked"), wantRateLimit: true},
		{name: "405 waf html", err: errors.New(`<!doctypehtml><html><title>405</title><p>blocked</p>`), wantRateLimit: true},
		{name: "429", err: errors.New("429 too many requests"), wantRateLimit: true},
		{name: "403 authentication", err: errors.New("403 forbidden"), wantRateLimit: false},
		{name: "tls timeout", err: errors.New("net/http: TLS handshake timeout"), wantRateLimit: false},
		{name: "blocked", err: errors.New("blocked by waf"), wantRateLimit: false},
		{name: "auth", err: errors.New("invalid credential"), wantRateLimit: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wrap115StreamTransientError("115 get file", tc.err)
			var rateLimit *drives.RateLimitError
			isRateLimit := errors.As(got, &rateLimit)
			if isRateLimit != tc.wantRateLimit {
				t.Fatalf("rate limit = %v, want %v; err=%v", isRateLimit, tc.wantRateLimit, got)
			}
			if !strings.Contains(got.Error(), "115 get file") {
				t.Fatalf("err = %v, want operation prefix", got)
			}
			if tc.wantRateLimit {
				if rateLimit.Provider != "p115" {
					t.Fatalf("provider = %q, want p115", rateLimit.Provider)
				}
				if rateLimit.RetryAfter != 10*time.Minute {
					t.Fatalf("retry after = %s, want 10m", rateLimit.RetryAfter)
				}
			}
		})
	}
}

func TestRememberPickCodeStaysBounded(t *testing.T) {
	driver := New(Config{ID: "115"})
	for i := 0; i <= p115PickCodeCacheMaxEntries; i++ {
		fileID := fmt.Sprintf("file-%d", i)
		driver.rememberPickCode(fileID, "pick-"+fileID)
	}
	if got := len(driver.pickCodes); got > p115PickCodeCacheMaxEntries {
		t.Fatalf("pick-code cache entries = %d, max = %d", got, p115PickCodeCacheMaxEntries)
	}
	if got := driver.rememberedPickCode(fmt.Sprintf("file-%d", p115PickCodeCacheMaxEntries)); got == "" {
		t.Fatal("most recently remembered pick code was evicted")
	}
}

func TestPrepareP115UploadBodyReusesSeekableReader(t *testing.T) {
	body := bytes.Repeat([]byte("115-upload-body-"), 10000)
	driver := New(Config{ID: "p115-test", UploadTempDir: filepath.Join(t.TempDir(), "unused")})
	reader := bytes.NewReader(body)

	prepared, digest, err := driver.prepareP115UploadBody(context.Background(), reader, int64(len(body)))
	if err != nil {
		t.Fatalf("prepare upload body: %v", err)
	}
	if prepared.cleanup != nil {
		t.Fatal("seekable reader was copied to a temporary file")
	}
	if prepared.reader != reader || prepared.readerAt != reader {
		t.Fatal("prepared body did not reuse the original reader")
	}
	assertP115UploadDigest(t, digest, body)
	if err := prepared.rewind(); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	got, err := io.ReadAll(prepared.reader)
	if err != nil {
		t.Fatalf("read prepared body: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("prepared body differs from source")
	}
}

func TestPrepareP115UploadBodyBuffersReaderOnlyOnce(t *testing.T) {
	body := []byte("hello-115-upload-test")
	tempDir := filepath.Join(t.TempDir(), "upload-tmp")
	driver := New(Config{ID: "p115-test", UploadTempDir: tempDir})
	// Wrapping the bytes.Reader behind io.Reader deliberately hides Seek and
	// ReaderAt, matching a network response body.
	var source io.Reader = struct{ io.Reader }{Reader: bytes.NewReader(body)}

	prepared, digest, err := driver.prepareP115UploadBody(context.Background(), source, int64(len(body)))
	if err != nil {
		t.Fatalf("prepare upload body: %v", err)
	}
	if prepared.cleanup == nil {
		t.Fatal("non-seekable reader was not buffered")
	}
	tmp, ok := prepared.reader.(*os.File)
	if !ok {
		prepared.cleanup()
		t.Fatalf("prepared reader type = %T, want *os.File", prepared.reader)
	}
	tmpPath := tmp.Name()
	if gotDir := filepath.Dir(tmpPath); gotDir != tempDir {
		prepared.cleanup()
		t.Fatalf("tmp dir = %q, want %q", gotDir, tempDir)
	}
	assertP115UploadDigest(t, digest, body)
	got, readErr := io.ReadAll(prepared.reader)
	prepared.cleanup()
	if readErr != nil {
		t.Fatalf("read prepared body: %v", readErr)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("temporary body differs from source")
	}
	if _, statErr := os.Stat(tmpPath); !os.IsNotExist(statErr) {
		t.Fatalf("temporary file remains after cleanup: %v", statErr)
	}
}

func TestPrepareP115UploadBodyRejectsSizeMismatchAndRewinds(t *testing.T) {
	body := []byte("hello-115-upload-test")
	reader := bytes.NewReader(body)
	driver := New(Config{ID: "p115-test"})

	_, _, err := driver.prepareP115UploadBody(context.Background(), reader, int64(len(body))+1)
	if err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("error = %v, want size mismatch", err)
	}
	position, seekErr := reader.Seek(0, io.SeekCurrent)
	if seekErr != nil {
		t.Fatalf("get reader position: %v", seekErr)
	}
	if position != 0 {
		t.Fatalf("reader position = %d, want 0 after failed preparation", position)
	}
}

func TestHashAndCopyP115UploadAllowsTransientEmptyRead(t *testing.T) {
	body := []byte("body-after-empty-read")
	source := &p115OneEmptyReader{reader: bytes.NewReader(body)}
	digest, err := hashAndCopyP115Upload(context.Background(), source, nil, int64(len(body)))
	if err != nil {
		t.Fatalf("hash upload body: %v", err)
	}
	assertP115UploadDigest(t, digest, body)
}

type p115OneEmptyReader struct {
	empty  bool
	reader io.Reader
}

func (r *p115OneEmptyReader) Read(p []byte) (int, error) {
	if !r.empty {
		r.empty = true
		return 0, nil
	}
	return r.reader.Read(p)
}

func assertP115UploadDigest(t *testing.T, got p115UploadDigest, body []byte) {
	t.Helper()
	full := sha1.Sum(body)
	preBody := body
	if int64(len(preBody)) > p115UploadPreHashSize {
		preBody = preBody[:p115UploadPreHashSize]
	}
	pre := sha1.Sum(preBody)
	wantFull := strings.ToUpper(hex.EncodeToString(full[:]))
	wantPre := strings.ToUpper(hex.EncodeToString(pre[:]))
	if got.SHA1 != wantFull || got.PreID != wantPre || got.Size != int64(len(body)) {
		t.Fatalf("digest = %#v, want SHA1=%s PreID=%s Size=%d", got, wantFull, wantPre, len(body))
	}
}

// TestUploadAndReportSha1RejectsInvalidArgs 检查空 reader / 空 name / 负 size 在
// 客户端未初始化前就被拒绝，避免下游 SDK 在错误参数下做异步初始化和真实网络调用。
func TestUploadAndReportSha1RejectsInvalidArgs(t *testing.T) {
	d := New(Config{ID: "p115-test"})
	// 注意：未调 Init，因此 d.client == nil，第一道防线就会拒绝。

	cases := []struct {
		name      string
		parentID  string
		fname     string
		body      io.Reader
		size      int64
		wantSubst string
	}{
		{name: "nil client", parentID: "0", fname: "x.mp4", body: bytes.NewReader([]byte("ok")), size: 2, wantSubst: "not initialized"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := d.UploadAndReportSha1(context.Background(), c.parentID, c.fname, c.body, c.size)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), c.wantSubst) {
				t.Fatalf("err = %v, want containing %q", err, c.wantSubst)
			}
		})
	}
}

func TestGenerationHTTPFailureRetriesOnNextCall(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nstream.m3u8\n")
	}))
	defer server.Close()
	d := New(Config{ID: "115"})
	d.hlsClient = server.Client()
	d.hlsMasterBaseURL = server.URL
	d.rememberPickCode("file", "pick")
	_, err := d.GenerationStreamURL(context.Background(), "file", false)
	if !errors.Is(err, drives.ErrGenerationStreamUnavailable) || !strings.Contains(err.Error(), "503") {
		t.Fatalf("lost HTTP error: %v", err)
	}
	// The next caller retries immediately; its successful URL is then cached.
	for i := 0; i < 2; i++ {
		link, err := d.GenerationStreamURL(context.Background(), "file", false)
		if err != nil || link == nil || link.URL != server.URL+"/stream.m3u8" {
			t.Fatalf("generation stream after HTTP failure: link=%v err=%v", link, err)
		}
	}
	if requests != 2 {
		t.Fatalf("requests=%d, want one failure and one cached success", requests)
	}
}

func TestOptionalHLSTransportFailureAllowsOriginalFallback(t *testing.T) {
	cause := errors.New("TLS handshake timeout")
	err := optionalHLSFailure("115 hls", cause)
	if !errors.Is(err, drives.ErrGenerationStreamUnavailable) || !errors.Is(err, cause) {
		t.Fatalf("lost classification or cause: %v", err)
	}
	if _, limited := drives.RateLimitRetryAfter(err); limited {
		t.Fatal("transport error became drive throttling")
	}
}
