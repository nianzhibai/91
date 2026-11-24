package tasklimit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitPending(t *testing.T, l *Limiter, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		n := len(l.waiters)
		l.mu.Unlock()
		if n == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("did not reach %d pending tasks", count)
}

func take(t *testing.T, ch <-chan func()) func() {
	t.Helper()
	select {
	case release := <-ch:
		return release
	case <-time.After(time.Second):
		t.Fatal("task did not acquire a slot")
		return nil
	}
}

func TestLimitResizePreservesRunningTasksAndFIFO(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := New(1)
	first, _ := l.Acquire(ctx, "")
	defer first()
	waiting := []chan func(){make(chan func(), 1), make(chan func(), 1), make(chan func(), 1)}
	for i, ch := range waiting {
		go func() {
			if release, err := l.Acquire(ctx, ""); err == nil {
				ch <- release
			}
		}()
		waitPending(t, l, i+1)
	}
	l.SetLimit(2)
	second := take(t, waiting[0])
	defer second()
	l.SetLimit(1)
	first()
	first() // A duplicate release must not admit another task.
	l.mu.Lock()
	active, pending := l.active, len(l.waiters)
	l.mu.Unlock()
	if active != 1 || pending != 2 {
		t.Fatalf("after shrink/release: active=%d pending=%d", active, pending)
	}
	second()
	third := take(t, waiting[1])
	third()
	take(t, waiting[2])()
}

func TestCancellationRemovesWaiterAndDoesNotLeakRacingGrant(t *testing.T) {
	for i := 0; i < 100; i++ {
		l := New(1)
		first, _ := l.Acquire(context.Background(), "")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			release, err := l.Acquire(ctx, "")
			if release != nil {
				release()
			}
			done <- err
		}()
		waitPending(t, l, 1)
		if i%2 == 0 {
			cancel()
			first()
		} else {
			first()
			cancel()
		}
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		l.mu.Lock()
		active, pending := l.active, len(l.waiters)
		l.mu.Unlock()
		if active != 0 || pending != 0 {
			t.Fatalf("leaked capacity: active=%d pending=%d", active, pending)
		}
	}
}

func TestAggregateConcurrency(t *testing.T) {
	l := New(3)
	var active, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := l.Acquire(context.Background(), "")
			if err != nil {
				t.Error(err)
				return
			}
			n := active.Add(1)
			for p := peak.Load(); n > p; p = peak.Load() {
				if peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			active.Add(-1)
			release()
		}()
	}
	wg.Wait()
	if peak.Load() != 3 {
		t.Fatalf("peak=%d, want 3", peak.Load())
	}
}

func TestFreeSlotGoesToTheOwnerHoldingFewest(t *testing.T) {
	ctx := context.Background()
	l := New(3)
	var local []func()
	for range 3 {
		release, err := l.Acquire(ctx, "local")
		if err != nil {
			t.Fatal(err)
		}
		local = append(local, release)
	}
	// The local source queues more work before the cloud drive arrives.
	localNext, cloud := make(chan func(), 1), make(chan func(), 1)
	go func() {
		if release, err := l.Acquire(ctx, "local"); err == nil {
			localNext <- release
		}
	}()
	waitPending(t, l, 1)
	go func() {
		if release, err := l.Acquire(ctx, "cloud"); err == nil {
			cloud <- release
		}
	}()
	waitPending(t, l, 2)

	local[0]()
	releaseCloud := take(t, cloud)
	l.mu.Lock()
	pending := len(l.waiters)
	l.mu.Unlock()
	if pending != 1 {
		t.Fatalf("pending = %d, want the local task still waiting", pending)
	}
	local[1]()
	take(t, localNext)()
	releaseCloud()
	local[2]()
}
