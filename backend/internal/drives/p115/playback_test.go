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
	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/scopedproxy"
)

func playbackJSONResponse(r *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
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
					minimum := time.Second
					if i == 2 {
						minimum = 3 * time.Second
					}
					if attempts[i].Sub(attempts[i-1]) < minimum {
						t.Errorf("retry %d had no backoff", i)
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
			if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
				t.Fatalf("cached=%v backoff=%v calls=%d err=%v, want one timed-out attempt", cached, backoff, calls, err)
			}
		}
	}
}
