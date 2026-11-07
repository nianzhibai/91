package p115

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

func TestGenerationBusinessErrorDisablesHLSUntilNextScan(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if strings.Contains(r.URL.Path, "rejected") {
			io.WriteString(w, `{"state":false,"error":"请验证账号"}`)
			return
		}
		io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nstream.m3u8\n")
	}))
	defer server.Close()
	d := New(Config{ID: "115"})
	d.hlsClient, d.hlsMasterBaseURL = server.Client(), server.URL
	for _, id := range []string{"cached", "rejected", "other"} {
		d.rememberPickCode(id, id)
	}
	ctx := context.Background()
	if _, err := d.GenerationStreamURL(ctx, "cached", false); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"rejected", "cached", "other"} {
		for _, refresh := range []bool{false, true} {
			link, err := d.GenerationStreamURL(ctx, id, refresh)
			if link != nil || !errors.Is(err, drives.ErrGenerationStreamUnavailable) || !strings.Contains(err.Error(), "请验证账号") {
				t.Fatalf("file=%s refresh=%v: link=%v err=%v", id, refresh, link, err)
			}
		}
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests=%d, want cached success plus first rejection only", got)
	}

	// Another attached account must remain usable.
	other := New(Config{ID: "other-account"})
	other.hlsClient, other.hlsMasterBaseURL = server.Client(), server.URL
	other.rememberPickCode("other", "other")
	if _, err := other.GenerationStreamURL(ctx, "other", false); err != nil {
		t.Fatalf("other account was disabled: %v", err)
	}

	d.ResetGenerationStreamForScan()
	if link, err := d.GenerationStreamURL(ctx, "cached", false); err != nil || link == nil {
		t.Fatalf("next scan did not recover: link=%v err=%v", link, err)
	}
	if got := requests.Load(); got != 4 {
		t.Fatalf("requests=%d, want a fresh request after reset", got)
	}
}

func TestGenerationSessionIsolatesInflightResolutions(t *testing.T) {
	for _, reset := range []bool{false, true} {
		name := "concurrent success cannot bypass disable"
		if reset {
			name = "previous scan rejection cannot disable next scan"
		}
		t.Run(name, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "slow") {
					close(started)
					<-release
					if reset {
						io.WriteString(w, `{"state":false,"message":"请验证账号"}`)
						return
					}
				} else if !reset {
					io.WriteString(w, `{"state":false,"msg":"请验证账号"}`)
					return
				}
				io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nstream.m3u8\n")
			}))
			t.Cleanup(func() { once.Do(func() { close(release) }); server.Close() })
			d := New(Config{ID: "115"})
			d.hlsClient, d.hlsMasterBaseURL = server.Client(), server.URL
			d.rememberPickCode("slow", "slow")
			d.rememberPickCode("fast", "fast")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			finished := make(chan error, 1)
			go func() {
				_, err := d.GenerationStreamURL(ctx, "slow", false)
				finished <- err
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if reset {
				d.ResetGenerationStreamForScan()
			}
			_, err := d.GenerationStreamURL(ctx, "fast", false)
			if reset && err != nil || !reset && !errors.Is(err, drives.ErrGenerationStreamUnavailable) {
				t.Fatalf("fast resolution: %v", err)
			}
			once.Do(func() { close(release) })
			if err := <-finished; !errors.Is(err, drives.ErrGenerationStreamUnavailable) {
				t.Fatalf("in-flight resolution should report unavailable: %v", err)
			}
			_, err = d.GenerationStreamURL(ctx, "fast", true)
			if reset && err != nil || !reset && !errors.Is(err, drives.ErrGenerationStreamUnavailable) {
				t.Fatalf("session state after in-flight resolution: %v", err)
			}
		})
	}
}
