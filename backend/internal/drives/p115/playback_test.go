package p115

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/scopedproxy"
)

func playbackJSONResponse(r *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}

func TestCloudPlaybackRetainsMasterAndIsIndependentOfGenerationSession(t *testing.T) {
	const cookie = "UID=u;CID=c;SEID=s"
	const master = "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",URI=\"https://cpats01.115.com/audio.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"audio\"\nhttps://cpats01.115.com/low.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=2,AUDIO=\"audio\"\nhttps://cpats01.115.com/high.m3u8\n"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Cookie") != cookie || r.UserAgent() != p115HLSUserAgent || r.Referer() != p115HLSReferer {
			t.Errorf("incorrect master headers: %v", r.Header)
		}
		if strings.Contains(r.URL.Path, "rejected") {
			io.WriteString(w, `{"state":false,"error":"video not ready"}`)
			return
		}
		io.WriteString(w, master)
	}))
	defer server.Close()
	d := New(Config{ID: "115", Cookie: cookie})
	d.hlsClient, d.hlsMasterBaseURL = server.Client(), server.URL
	d.rememberPickCode("rejected", "rejected")
	d.rememberPickCode("video", "video")
	_, _ = d.GenerationStreamURL(context.Background(), "rejected", false)
	for range 2 {
		sources, err := d.PlaybackSources(context.Background(), "video")
		if err != nil || len(sources) != 1 || sources[0].Status != "ready" {
			t.Fatalf("playback inherited scan rejection: %+v %v", sources, err)
		}
		source := sources[0]
		if source.Relay == nil || source.Relay.Playlist != master || source.Link.Headers.Get("Cookie") != "" || source.Link.ClientRedirectSafe || source.Link.Headers.Get("User-Agent") != p115HLSUserAgent {
			t.Fatalf("invalid relay source: %+v", source)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("playback must use the proxy cache, calls=%d", calls.Load())
	}
}

func TestCloudPlaybackPreservesThrottleCancellationAndUnavailable(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusForbidden, http.StatusTooManyRequests} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			io.WriteString(w, `{"state":false,"error":"not available"}`)
		}))
		d := New(Config{ID: "115"})
		d.hlsClient, d.hlsMasterBaseURL = server.Client(), server.URL
		d.rememberPickCode("video", "pick")
		sources, err := d.PlaybackSources(context.Background(), "video")
		server.Close()
		if status == 429 {
			if _, limited := drives.RateLimitRetryAfter(err); !limited {
				t.Fatalf("throttle lost: %v", err)
			}
			continue
		}
		if err != nil || len(sources) != 1 || sources[0].Status != "unavailable" || sources[0].Link != nil {
			t.Fatalf("status=%d sources=%+v err=%v", status, sources, err)
		}
	}
	d := New(Config{ID: "115"})
	d.rememberPickCode("video", "pick")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.PlaybackSources(ctx, "video"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestStatRecoversFromTLSHandshakeTimeout(t *testing.T) {
	var handshakes atomic.Int32
	releaseFirstHandshake := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"state":true,"data":[{"fid":"file","n":"video.mp4","pc":"pick"}]}`)
	}))
	server.TLS = &tls.Config{GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		if handshakes.Add(1) == 1 {
			<-releaseFirstHandshake
		}
		return nil, nil
	}}
	server.StartTLS()
	t.Cleanup(func() { close(releaseFirstHandshake); server.Close() })
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSHandshakeTimeout = 20 * time.Millisecond
	t.Cleanup(transport.CloseIdleConnections)
	d := newP115ListTestDriver(p115RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Scheme = "https"
		r.URL.Host = server.Listener.Addr().String()
		return transport.RoundTrip(r)
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	entry, err := d.Stat(ctx, "file")
	if err != nil || entry == nil || entry.ID != "file" {
		t.Fatalf("stat after TLS timeout: entry=%v err=%v", entry, err)
	}
	if got := handshakes.Load(); got != 2 {
		t.Fatalf("TLS handshakes=%d, want one failed attempt and one retry", got)
	}
}

func TestStreamReusesPickCodeAcrossLinkResolutions(t *testing.T) {
	for _, cached := range []bool{false, true} {
		name := "first lookup is reused"
		if cached {
			name = "previously remembered pick code skips lookup"
		}
		t.Run(name, func(t *testing.T) {
			lookups, downloads := 0, 0
			d := newP115ListTestDriver(p115RoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/files/get_info" {
					lookups++
					return playbackJSONResponse(r, `{"state":true,"data":[{"fid":"file","pc":"pick"}]}`), nil
				}
				downloads++
				if r.UserAgent() != "browser" {
					t.Errorf("download User-Agent=%q", r.UserAgent())
				}
				return playbackJSONResponse(r, `{"state":false,"errno":99}`), nil
			}))
			if cached {
				d.rememberPickCode("file", "pick")
			}
			for range 2 {
				_, err := d.StreamURLWithHeader(context.Background(), "file", http.Header{"User-Agent": {"browser"}})
				if !errors.Is(err, sdk.ErrNotLogin) {
					t.Fatalf("expected to reach download endpoint, got %v", err)
				}
			}
			wantLookups := 1
			if cached {
				wantLookups = 0
			}
			if lookups != wantLookups || downloads != 2 {
				t.Fatalf("lookups=%d downloads=%d, want %d and 2", lookups, downloads, wantLookups)
			}
		})
	}
}

func TestStreamRefreshesExplicitlyRejectedPickCode(t *testing.T) {
	lookups, downloads := 0, 0
	d := newP115ListTestDriver(p115RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/files/get_info" {
			lookups++
			return playbackJSONResponse(r, `{"state":true,"data":[{"fid":"file","pc":"new-pick"}]}`), nil
		}
		downloads++
		if r.UserAgent() != "browser" {
			t.Errorf("download User-Agent=%q, want browser", r.UserAgent())
		}
		if downloads == 1 {
			return playbackJSONResponse(r, `{"state":false,"errno":50003}`), nil
		}
		return playbackJSONResponse(r, `{"state":false,"errno":99}`), nil
	}))
	d.rememberPickCode("file", "old-pick")
	_, err := d.StreamURLWithHeader(context.Background(), "file", http.Header{"User-Agent": {"browser"}})
	if !errors.Is(err, sdk.ErrNotLogin) || lookups != 1 || downloads != 2 || d.rememberedPickCode("file") != "new-pick" {
		t.Fatalf("refresh: lookups=%d downloads=%d err=%v", lookups, downloads, err)
	}
}

func TestPlaybackReadRetriesAreBoundedAndPreserveRejections(t *testing.T) {
	for _, stage := range []string{"file", "download"} {
		for _, failure := range []string{"network", "auth", "throttle", "missing pick code"} {
			if stage == "download" && failure == "missing pick code" {
				continue
			}
			t.Run(stage+"/"+failure, func(t *testing.T) {
				calls := 0
				var attempts []time.Time
				d := newP115ListTestDriver(p115RoundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					attempts = append(attempts, time.Now())
					switch failure {
					case "network":
						return nil, io.ErrUnexpectedEOF
					case "auth":
						return playbackJSONResponse(r, `{"state":false,"errno":99}`), nil
					case "throttle":
						resp := playbackJSONResponse(r, `<html><title>405</title></html>`)
						resp.StatusCode = http.StatusMethodNotAllowed
						return resp, nil
					default:
						return playbackJSONResponse(r, `{"state":true,"data":[]}`), nil
					}
				}))
				if stage == "download" {
					d.rememberPickCode("file", "pick")
				}
				_, err := d.StreamURL(context.Background(), "file")
				wantCalls := 1
				if failure == "network" {
					wantCalls = 3
					if !errors.Is(err, io.ErrUnexpectedEOF) {
						t.Fatalf("network cause lost: %v", err)
					}
				}
				if err == nil || calls != wantCalls {
					t.Fatalf("calls=%d want=%d err=%v", calls, wantCalls, err)
				}
				for i := 1; i < len(attempts); i++ {
					delay := attempts[i].Sub(attempts[i-1])
					if i == 1 && delay >= time.Second {
						t.Errorf("first retry was not immediate: %s", delay)
					}
					if i == 2 && delay < time.Second {
						t.Errorf("last retry waited %s, want at least 1s", delay)
					}
				}
				if _, limited := drives.RateLimitRetryAfter(err); limited != (failure == "throttle") {
					t.Fatalf("incorrect throttling classification: %v", err)
				}
			})
		}
	}
}

func TestSDKOperationsHonorCancellationAndProxyContext(t *testing.T) {
	for _, operation := range []string{"stat", "stream lookup", "stream download", "generation", "rename", "remove"} {
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx, err := scopedproxy.WithURL(ctx, "http://proxy.example:8080")
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			d := newP115ListTestDriver(p115RoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if !scopedproxy.Configured(r.Context()) {
					t.Error("SDK request lost caller's proxy scope")
				}
				cancel()
				select {
				case <-r.Context().Done():
					return nil, r.Context().Err()
				case <-time.After(time.Second):
					t.Error("SDK request ignored cancellation")
					return nil, errors.New("request did not cancel")
				}
			}))
			switch operation {
			case "stat":
				_, err = d.Stat(ctx, "file")
			case "stream lookup":
				_, err = d.StreamURL(ctx, "file")
			case "stream download":
				d.rememberPickCode("file", "pick")
				_, err = d.StreamURL(ctx, "file")
			case "generation":
				_, err = d.GenerationStreamURL(ctx, "file", false)
			case "rename":
				err = d.Rename(ctx, "file", "new.mp4")
			case "remove":
				err = d.Remove(ctx, "file")
			}
			if !errors.Is(err, context.Canceled) || calls != 1 {
				t.Fatalf("calls=%d err=%v, want one canceled attempt", calls, err)
			}
		})
	}
}

func TestStreamReadDeadlineStopsRequestAndRetry(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, backoff := range []bool{false, true} {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			calls := 0
			d := newP115ListTestDriver(p115RoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				deadline, ok := r.Context().Deadline()
				if !ok || time.Until(deadline) > 30*time.Millisecond {
					t.Error("SDK request did not inherit playback deadline")
					return nil, errors.New("missing deadline")
				}
				if backoff {
					return nil, io.ErrUnexpectedEOF
				}
				<-r.Context().Done()
				return nil, r.Context().Err()
			}))
			if cached {
				d.rememberPickCode("file", "pick")
			}
			_, err := d.StreamURL(ctx, "file")
			cancel()
			wantCalls := 1
			if backoff {
				wantCalls = 2
			}
			if !errors.Is(err, context.DeadlineExceeded) || calls != wantCalls {
				t.Fatalf("cached=%v backoff=%v calls=%d err=%v, want %d calls before deadline", cached, backoff, calls, err, wantCalls)
			}
		}
	}
}
