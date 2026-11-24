package tasklimit

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCooldownPauseNeverShortensActiveCooldown(t *testing.T) {
	var c Cooldown
	now := time.Now()
	long := c.Pause(now, time.Hour)
	if got := c.Pause(now, time.Minute); !got.Equal(long) {
		t.Fatalf("shorter pause ended at %s, want %s", got, long)
	}
	if until, ok := c.Until(now); !ok || !until.Equal(long) {
		t.Fatalf("Until = %s/%v, want %s/true", until, ok, long)
	}
	if _, ok := c.Until(long); ok {
		t.Fatal("cooldown still active at its end")
	}
}

func TestCooldownWaitFollowsExtension(t *testing.T) {
	var c Cooldown
	start := time.Now()
	c.Pause(start, 20*time.Millisecond)
	go func() {
		time.Sleep(5 * time.Millisecond)
		c.Pause(time.Now(), 60*time.Millisecond)
	}()
	if err := c.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Fatalf("Wait returned after %s, before the extended cooldown ended", elapsed)
	}
}

func TestCoolingDriveDoesNotOccupySharedSlot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	limiter := New(1)
	var cooling, healthy Cooldown
	cooling.Pause(time.Now(), time.Minute)
	done := make(chan error, 1)
	go func() {
		release, err := limiter.AcquireAfter(ctx, "cooling", &cooling)
		if err == nil {
			release()
		}
		done <- err
	}()
	healthyCtx, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	release, err := limiter.AcquireAfter(healthyCtx, "healthy", &healthy)
	if err != nil {
		t.Fatalf("cooling drive blocked healthy drive: %v", err)
	}
	release()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cooling acquire = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cooldown wait did not cancel")
	}
}

func TestAcquireAfterReturnsSlotWhenCooldownStartsInLine(t *testing.T) {
	limiter := New(1)
	holder, err := limiter.Acquire(context.Background(), "holder")
	if err != nil {
		t.Fatal(err)
	}
	var c Cooldown
	acquired := make(chan func(), 1)
	go func() {
		release, err := limiter.AcquireAfter(context.Background(), "drive", &c)
		if err == nil {
			acquired <- release
		}
	}()
	waitPending(t, limiter, 1)
	// The drive starts cooling while its task waits for the shared slot.
	c.Pause(time.Now(), 50*time.Millisecond)
	holder()

	other, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	release, err := limiter.Acquire(other, "other")
	if err != nil {
		t.Fatalf("cooling task kept the slot: %v", err)
	}
	release()

	select {
	case release := <-acquired:
		release()
	case <-time.After(time.Second):
		t.Fatal("task was not admitted after the cooldown")
	}
}
