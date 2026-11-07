package generationsource

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

func testSource(url string) *drives.GenerationSource {
	return &drives.GenerationSource{Protocol: "hls", Link: &drives.StreamLink{URL: url, Expires: time.Now().Add(time.Minute), Headers: http.Header{"Referer": {"original"}}}, Relay: &drives.HLSRelay{AllowedHosts: []string{"media.example"}}, RefreshStatuses: []int{403}}
}

func TestCacheCoalescesAndRefreshSupersedesInflight(t *testing.T) {
	var cache Cache
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	resolve := func(ctx context.Context, id string) (*drives.GenerationSource, error) {
		n := calls.Add(1)
		if n == 1 {
			close(started)
			<-release
		}
		return testSource(fmt.Sprintf("https://media.example/%d", n)), nil
	}
	go func() { defer close(finished); _, _ = cache.Resolve(context.Background(), "file", false, resolve) }()
	<-started
	fresh, err := cache.Resolve(context.Background(), "file", true, resolve)
	if err != nil || fresh.Link.URL != "https://media.example/2" {
		t.Fatalf("refresh: %+v %v", fresh, err)
	}
	close(release)
	<-finished
	cached, _ := cache.Resolve(context.Background(), "file", false, resolve)
	if cached.Link.URL != fresh.Link.URL || calls.Load() != 2 {
		t.Fatalf("stale result replaced refresh: %+v calls=%d", cached, calls.Load())
	}
	cached.Link.Headers.Set("Referer", "modified")
	cached.Relay.AllowedHosts[0] = "wrong"
	cached.RefreshStatuses[0] = 500
	clone, _ := cache.Resolve(context.Background(), "file", false, resolve)
	if clone.Link.Headers.Get("Referer") != "original" || clone.Relay.AllowedHosts[0] != "media.example" || clone.RefreshStatuses[0] != 403 {
		t.Fatal("caller mutated cache")
	}
}

func TestCacheWaiterCancellationAndSharedRefresh(t *testing.T) {
	var cache Cache
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	resolve := func(ctx context.Context, id string) (*drives.GenerationSource, error) {
		calls.Add(1)
		close(started)
		<-release
		return testSource("https://media.example/a"), nil
	}
	go func() { defer close(finished); _, _ = cache.Resolve(context.Background(), "file", true, resolve) }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := cache.Resolve(ctx, "file", true, resolve); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter: %v", err)
	}
	close(release)
	<-finished
	if calls.Load() != 1 {
		t.Fatal("refresh did not coalesce")
	}
}

func TestCacheUnavailableIsFileScopedAndErrorsAreNotCached(t *testing.T) {
	var cache Cache
	calls := 0
	resolve := func(ctx context.Context, id string) (*drives.GenerationSource, error) {
		calls++
		if id == "pending" {
			return nil, drives.ErrGenerationStreamUnavailable
		}
		if id == "limited" {
			return nil, &drives.RateLimitError{RetryAfter: time.Second}
		}
		return testSource("https://media.example/ready"), nil
	}
	for range 2 {
		_, _ = cache.Resolve(context.Background(), "pending", false, resolve)
	}
	if calls != 1 {
		t.Fatal("unavailable source not cached")
	}
	if source, err := cache.Resolve(context.Background(), "ready", false, resolve); err != nil || source == nil {
		t.Fatalf("another file disabled: %v", err)
	}
	for range 2 {
		if _, err := cache.Resolve(context.Background(), "limited", false, resolve); err == nil {
			t.Fatal("lost rate limit")
		}
	}
	if calls != 4 {
		t.Fatalf("calls=%d", calls)
	}
	cache.mu.Lock()
	value := cache.entries["ready"]
	value.expires = time.Now().Add(-time.Second)
	cache.entries["ready"] = value
	cache.mu.Unlock()
	_, _ = cache.Resolve(context.Background(), "ready", false, resolve)
	if calls != 5 {
		t.Fatal("expired result reused")
	}
}

func TestReadyHLSSkipsPendingExpiredAndNonHLSSources(t *testing.T) {
	sources := []drives.PlaybackSource{
		{Protocol: "file", Status: "ready", Link: &drives.StreamLink{URL: "https://media.example/file"}},
		{Protocol: "hls", Status: "processing", Link: &drives.StreamLink{URL: "https://media.example/pending"}},
		{Protocol: "hls", Status: "ready", Link: &drives.StreamLink{URL: "https://media.example/expired", Expires: time.Now().Add(-time.Second)}},
		{Protocol: "hls", Status: "ready", Link: &drives.StreamLink{URL: "https://media.example/ready"}},
	}
	source, err := ReadyHLS(sources)
	if err != nil || source.Link.URL != sources[3].Link.URL || source.Relay == nil {
		t.Fatalf("source=%+v err=%v", source, err)
	}
	if _, err := ReadyHLS(sources[:3]); !errors.Is(err, drives.ErrGenerationStreamUnavailable) {
		t.Fatalf("unavailable=%v", err)
	}
}
