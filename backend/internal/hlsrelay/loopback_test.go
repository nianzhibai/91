package hlsrelay

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

func relayGET(t *testing.T, client *http.Client, raw string) (int, string) {
	t.Helper()
	response, err := client.Get(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(body)
}

func TestLoopbackRewritesAllResourcesWithCurrentAuthorization(t *testing.T) {
	var cookie atomic.Value
	cookie.Store("session=first")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Cookie") != cookie.Load().(string) {
			t.Errorf("stale cookie: %q", r.Header.Get("Cookie"))
		}
		if r.Header.Get("Origin") != "https://provider.example" {
			t.Error("missing origin")
		}
		switch r.URL.Path {
		case "/master":
			http.Redirect(w, r, "/lists/master.m3u8?signature=secret", 302)
		case "/lists/master.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=100\nchild.m3u8?signature=secret\n")
		case "/lists/child.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:3\n#EXT-X-KEY:METHOD=AES-128,URI=\"key?secret=1\"\n#EXT-X-MAP:URI=\"init.mp4?secret=1\"\n#EXTINF:3,\nchunk.ts?secret=1\n#EXT-X-ENDLIST\n")
		default:
			if strings.Contains(r.URL.RawQuery, "injected") {
				t.Error("consumer query reached upstream")
			}
			if r.Header.Get("Range") != "bytes=0-3" {
				t.Errorf("range=%s", r.Header.Get("Range"))
			}
			w.Header().Set("Content-Range", "bytes 0-3/4")
			w.WriteHeader(206)
			fmt.Fprint(w, "data")
		}
	}))
	defer server.Close()
	root, _ := url.Parse(server.URL)
	relay, err := Start(context.Background(), &drives.StreamLink{URL: server.URL + "/master", Headers: http.Header{"Origin": {"https://provider.example"}}}, drives.HLSRelay{AllowedHosts: []string{root.Host}, Authorize: func(r *http.Request) { r.Header.Set("Cookie", cookie.Load().(string)) }})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	client := server.Client()
	_, master := relayGET(t, client, relay.URL)
	if strings.Contains(master, "secret") || strings.Contains(master, server.URL) {
		t.Fatalf("leaked upstream: %s", master)
	}
	base, _ := url.Parse(relay.URL)
	child, _ := base.Parse(strings.Split(strings.TrimSpace(master), "\n")[2])
	cookie.Store("session=rotated")
	_, playlist := relayGET(t, client, child.String())
	var resources []string
	for _, line := range strings.Split(playlist, "\n") {
		if strings.Contains(line, "URI=\"") {
			resources = append(resources, strings.Split(line, "\"")[1])
		} else if strings.HasPrefix(line, "/") {
			resources = append(resources, line)
		}
	}
	if len(resources) != 3 {
		t.Fatalf("resources=%v", resources)
	}
	for _, resource := range resources {
		local, _ := base.Parse(resource + "?injected=1")
		req, _ := http.NewRequest("GET", local.String(), nil)
		req.Header.Set("Range", "bytes=0-3")
		req.Header.Set("Cookie", "viewer=secret")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 206 || resp.Header.Get("Content-Range") == "" {
			t.Fatalf("range response=%v", resp)
		}
	}
	if relay.Error() != nil || requests.Load() != 6 {
		t.Fatalf("requests=%d err=%v", requests.Load(), relay.Error())
	}
}

func TestLoopbackBlocksUntrustedResourcesAndRedirects(t *testing.T) {
	var forbidden atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forbidden.Add(1) }))
	defer evil.Close()
	for _, redirect := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if redirect {
				http.Redirect(w, r, evil.URL, 302)
			} else {
				fmt.Fprintf(w, "#EXTM3U\n#EXTINF:1,\n%s/secret.ts\n", evil.URL)
			}
		}))
		root, _ := url.Parse(server.URL)
		relay, err := Start(context.Background(), &drives.StreamLink{URL: server.URL}, drives.HLSRelay{AllowedHosts: []string{root.Host}, Authorize: func(r *http.Request) { r.Header.Set("Cookie", "account=secret") }})
		if err != nil {
			t.Fatal(err)
		}
		status, _ := relayGET(t, server.Client(), relay.URL)
		if status != 502 || relay.Error() == nil {
			t.Fatalf("status=%d error=%v", status, relay.Error())
		}
		relay.Close()
		server.Close()
	}
	if forbidden.Load() != 0 {
		t.Fatal("untrusted destination requested")
	}
}

func TestLoopbackRetainsRateLimitAndClosesOnCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "17"); w.WriteHeader(429) }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	relay, err := Start(ctx, &drives.StreamLink{URL: server.URL + "?account=secret"}, drives.HLSRelay{})
	if err != nil {
		t.Fatal(err)
	}
	status, _ := relayGET(t, server.Client(), relay.URL)
	delay, limited := drives.RateLimitRetryAfter(relay.Error())
	if status != 429 || !limited || delay != 17*time.Second || strings.Contains(relay.Error().Error(), "secret") {
		t.Fatalf("status=%d err=%v retry=%v", status, relay.Error(), delay)
	}
	cancel()
	relay.Close()
	client := &http.Client{Timeout: time.Second}
	if response, err := client.Get(relay.URL); err == nil {
		response.Body.Close()
		t.Fatal("relay still open")
	}
}

func TestLoopbackRejectionCancelsInflightAndBlocksFurtherReads(t *testing.T) {
	for _, status := range []int{403, 405, 412, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			started, canceled := make(chan struct{}), make(chan struct{})
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				switch r.URL.Path {
				case "/index.m3u8":
					fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\nslow.ts\n#EXTINF:1,\nreject.ts\n#EXT-X-ENDLIST\n")
				case "/slow.ts":
					close(started)
					<-r.Context().Done()
					close(canceled)
				default:
					w.Header().Set("Retry-After", "17")
					w.WriteHeader(status)
				}
			}))
			defer server.Close()
			relay, err := Start(context.Background(), &drives.StreamLink{URL: server.URL + "/index.m3u8"}, drives.HLSRelay{})
			if err != nil {
				t.Fatal(err)
			}
			defer relay.Close()
			_, playlist := relayGET(t, server.Client(), relay.URL)
			lines := strings.Split(playlist, "\n")
			base, _ := url.Parse(relay.URL)
			slow, _ := base.Parse(lines[2])
			reject, _ := base.Parse(lines[4])
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				if response, err := server.Client().Get(slow.String()); err == nil {
					response.Body.Close()
				}
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("slow request did not start")
			}
			if got, _ := relayGET(t, server.Client(), reject.String()); got != status {
				t.Fatalf("status=%d, want %d", got, status)
			}
			select {
			case <-canceled:
			case <-time.After(3 * time.Second):
				t.Fatal("inflight upstream request was not canceled")
			}
			<-finished
			if !drives.ErrorMentionsHTTPStatus(context.Cause(relay.Context()), status) {
				t.Fatalf("lost cancellation cause: %v", context.Cause(relay.Context()))
			}
			relay.ResetError()
			relayGET(t, server.Client(), slow.String())
			if requests.Load() != 3 || !drives.ErrorMentionsHTTPStatus(relay.Error(), status) {
				t.Fatalf("rejected source resumed: requests=%d err=%v", requests.Load(), relay.Error())
			}
		})
	}
}

func TestLoopbackConsumerCancellationKeepsSourceUsable(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(started)
			<-r.Context().Done()
			close(canceled)
			return
		}
		fmt.Fprint(w, "media data")
	}))
	defer server.Close()
	relay, err := Start(context.Background(), &drives.StreamLink{URL: server.URL}, drives.HLSRelay{})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", relay.URL, nil)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		if response, err := server.Client().Do(request); err == nil {
			response.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request was not canceled")
	}
	<-finished
	status, data := relayGET(t, server.Client(), relay.URL)
	if status != 200 || data != "media data" || relay.Context().Err() != nil || relay.Error() != nil {
		t.Fatalf("consumer cancellation poisoned source: status=%d data=%q err=%v", status, data, relay.Error())
	}
}
