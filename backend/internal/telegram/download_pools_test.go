package telegram

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gotd/td/bin"
	mt "github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

type testDownloadPool struct{ closed atomic.Int32 }

func (*testDownloadPool) Invoke(context.Context, bin.Encoder, bin.Decoder) error { return nil }
func (p *testDownloadPool) Close() error                                         { p.closed.Add(1); return nil }

func TestDownloadPoolsReuseConnectionsAndCloseWithReceiver(t *testing.T) {
	var pools downloadPools
	var opens atomic.Int32
	first, second := &testDownloadPool{}, &testDownloadPool{}
	open := func(_ context.Context, dc int, threads int64) (mt.CloseInvoker, error) {
		opens.Add(1)
		if threads != 6 {
			t.Errorf("connection limit = %d, want 6", threads)
		}
		if dc == 5 {
			return first, nil
		}
		return second, nil
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pool, err := pools.get(ctx, 5, 6, open)
			if err != nil || pool != first {
				t.Errorf("did not reuse DC 5: %v", err)
			}
		}()
	}
	wg.Wait()
	if opens.Load() != 1 || first.closed.Load() != 0 {
		t.Fatal("concurrent downloads rebuilt or closed their shared pool")
	}
	pool, err := pools.get(ctx, 2, 6, open)
	if err != nil || pool != second || opens.Load() != 2 {
		t.Fatal("datacenters shared the same download pool", err)
	}
	pools.close()
	pools.close()
	if first.closed.Load() != 1 || second.closed.Load() != 1 {
		t.Fatal("receiver shutdown did not close each pool exactly once")
	}
	if _, err := pools.get(ctx, 5, 6, open); err == nil || opens.Load() != 2 {
		t.Fatal("closed receiver opened another pool")
	}
}

func TestDownloadPoolsRetryFailedCreation(t *testing.T) {
	var pools downloadPools
	defer pools.close()
	failed := errors.New("connection failed")
	if _, err := pools.get(context.Background(), 5, 6, func(context.Context, int, int64) (mt.CloseInvoker, error) {
		return nil, failed
	}); !errors.Is(err, failed) {
		t.Fatal("connection error lost", err)
	}
	want := &testDownloadPool{}
	pool, err := pools.get(context.Background(), 5, 6, func(context.Context, int, int64) (mt.CloseInvoker, error) {
		return want, nil
	})
	if err != nil || pool != want {
		t.Fatal("failed pool creation could not retry", err)
	}
}

func TestDownloadPoolsCancelCreationWithoutLeakingConnection(t *testing.T) {
	var pools downloadPools
	defer pools.close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	created := &testDownloadPool{}
	_, err := pools.get(ctx, 5, 6, func(context.Context, int, int64) (mt.CloseInvoker, error) {
		cancel()
		return created, nil
	})
	if !errors.Is(err, context.Canceled) || created.closed.Load() != 1 || len(pools.pools) != 0 {
		t.Fatal("canceled creation retained a pool", err)
	}
}

func TestDownloadRouteReusesHomeAuthorization(t *testing.T) {
	cfg := tg.Config{ThisDC: 2, DCOptions: []tg.DCOption{
		{ID: 2, MediaOnly: true}, {ID: 4, MediaOnly: true}, {ID: 5},
	}}
	for dc, want := range map[int]downloadRoute{2: routeHome, 4: routeMedia, 5: routeData} {
		if got := downloadRouteFor(cfg, dc); got != want {
			t.Errorf("DC %d route = %d, want %d", dc, got, want)
		}
	}
}
