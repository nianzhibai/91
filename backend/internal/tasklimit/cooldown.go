package tasklimit

import (
	"context"
	"sync"
	"time"
)

// Cooldown pauses one drive's provider work after the provider throttles it
// or is briefly unavailable. Every worker reading the drive shares a single
// Cooldown, so a limit seen by any of them pauses all of them. The zero value
// is not cooling down.
type Cooldown struct {
	mu    sync.Mutex
	until time.Time
}

// Pause extends the cooldown to at least now+d and returns when it ends. A
// shorter pause never cuts an active one short.
func (c *Cooldown) Pause(now time.Time, d time.Duration) time.Time {
	until := now.Add(d)
	c.mu.Lock()
	defer c.mu.Unlock()
	if until.After(c.until) {
		c.until = until
	}
	return c.until
}

// Until reports when an active cooldown ends.
func (c *Cooldown) Until(now time.Time) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !now.Before(c.until) {
		return time.Time{}, false
	}
	return c.until, true
}

// Wait blocks until no cooldown is active, following extensions made while it
// waits. It returns ctx.Err() if ctx ends first.
func (c *Cooldown) Wait(ctx context.Context) error {
	for {
		until, cooling := c.Until(time.Now())
		if !cooling {
			return ctx.Err()
		}
		timer := time.NewTimer(time.Until(until))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// AcquireAfter waits out cooldown before taking a slot, so a cooling drive
// never holds shared budget. A cooldown can begin while the caller waits in
// line; the slot is then returned and the wait starts over.
func (l *Limiter) AcquireAfter(ctx context.Context, owner string, cooldown *Cooldown) (release func(), err error) {
	for {
		if err := cooldown.Wait(ctx); err != nil {
			return nil, err
		}
		release, err := l.Acquire(ctx, owner)
		if err != nil {
			return nil, err
		}
		if _, cooling := cooldown.Until(time.Now()); !cooling {
			return release, nil
		}
		release()
	}
}
