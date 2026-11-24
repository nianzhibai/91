// Package tasklimit coordinates resource-intensive tasks across drive workers.
package tasklimit

import (
	"context"
	"sync"
)

// Limiter shares slots fairly among owners: a free slot goes to the waiting
// owner that holds the fewest, and owners holding the same number are served
// in arrival order. An owner able to use every slot therefore takes them all
// while nobody else waits, yet cannot keep an idle owner waiting behind its
// queue. Changing the limit never interrupts running tasks; after a
// reduction, new tasks wait for enough slots to drain. A nil limiter imposes
// no limit, for callers without a shared resource budget.
type Limiter struct {
	mu      sync.Mutex
	limit   int
	active  int
	held    map[string]int
	waiters []*waiter
}

type waiter struct {
	owner   string
	ready   chan struct{}
	granted bool
}

func New(limit int) *Limiter {
	l := &Limiter{held: make(map[string]int)}
	l.SetLimit(limit)
	return l
}

func (l *Limiter) SetLimit(limit int) {
	if limit < 1 {
		limit = 1
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limit = limit
	l.dispatchLocked()
}

// Limit is the number of slots.
func (l *Limiter) Limit() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit
}

// Acquire waits for a slot on behalf of owner without starting work. The
// returned release function is idempotent and must be called when work
// finishes, before retry delays.
func (l *Limiter) Acquire(ctx context.Context, owner string) (release func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l == nil {
		return func() {}, nil
	}
	w := &waiter{owner: owner, ready: make(chan struct{})}
	l.mu.Lock()
	l.waiters = append(l.waiters, w)
	l.dispatchLocked()
	l.mu.Unlock()

	select {
	case <-ctx.Done():
	case <-w.ready:
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// Cancellation may race a grant. In either case return the slot, so a
	// stopped drive cannot leak capacity or start work after waiting.
	if err := ctx.Err(); err != nil {
		if w.granted {
			l.releaseLocked(owner)
		} else {
			for i, pending := range l.waiters {
				if pending == w {
					l.waiters = append(l.waiters[:i], l.waiters[i+1:]...)
					break
				}
			}
			l.dispatchLocked()
		}
		return nil, err
	}
	return sync.OnceFunc(func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.releaseLocked(owner)
	}), nil
}

func (l *Limiter) releaseLocked(owner string) {
	l.active--
	if l.held[owner]--; l.held[owner] <= 0 {
		delete(l.held, owner)
	}
	l.dispatchLocked()
}

func (l *Limiter) dispatchLocked() {
	for l.active < l.limit && len(l.waiters) > 0 {
		next := 0
		for i, w := range l.waiters[1:] {
			if l.held[w.owner] < l.held[l.waiters[next].owner] {
				next = i + 1
			}
		}
		w := l.waiters[next]
		l.waiters = append(l.waiters[:next], l.waiters[next+1:]...)
		w.granted = true
		l.active++
		l.held[w.owner]++
		close(w.ready)
	}
}
