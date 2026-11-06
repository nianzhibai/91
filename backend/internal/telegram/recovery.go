package telegram

import (
	"context"
	"sync"
	"time"

	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// updateRecovery adapts the update manager's API and storage in one place.
// Albums are released only when the final difference's exact cursor is durable.
// A connection change invalidates in-flight results from the old connection.
type updateRecovery struct {
	updates.API
	updates.StateStorage
	mu          sync.Mutex
	generation  uint64
	connected   bool
	synced      bool
	final       *updates.State
	empty       bool
	nextAttempt time.Time
	wake        chan struct{}
}

func newUpdateRecovery(storage updates.StateStorage) *updateRecovery {
	return &updateRecovery{StateStorage: storage, wake: make(chan struct{}, 1)}
}

func (r *updateRecovery) connection(connected bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connected = connected
	if !connected {
		r.generation++
		r.synced, r.final = false, nil
	}
}

func (r *updateRecovery) Synced() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.connected && r.synced
}

func (r *updateRecovery) request() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run only after the update manager has started. Queue recovery through it so
// normal updates, difference slices and cursor commits stay serialized.
func (r *updateRecovery) run(ctx context.Context, handle func(context.Context, tg.UpdatesClass) error) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.wake:
		case <-ticker.C:
		}
		r.mu.Lock()
		needed := r.connected && !r.synced && !time.Now().Before(r.nextAttempt)
		r.mu.Unlock()
		if needed {
			if err := handle(ctx, &tg.UpdatesTooLong{}); err != nil {
				return err
			}
		}
	}
}

func (r *updateRecovery) UpdatesGetDifference(ctx context.Context, request *tg.UpdatesGetDifferenceRequest) (tg.UpdatesDifferenceClass, error) {
	r.mu.Lock()
	delay := time.Until(r.nextAttempt)
	r.mu.Unlock()
	if delay > 0 && !wait(ctx, delay) {
		return nil, ctx.Err()
	}
	r.mu.Lock()
	generation := r.generation
	r.synced, r.final = false, nil
	r.mu.Unlock()
	diff, err := r.API.UpdatesGetDifference(ctx, request)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		delay := 5 * time.Second
		if floodWait, ok := tgerr.AsFloodWait(err); ok {
			delay = max(delay, floodWait)
		}
		r.nextAttempt = time.Now().Add(delay)
	} else {
		r.nextAttempt = time.Time{}
	}
	if err == nil && generation == r.generation {
		switch d := diff.(type) {
		case *tg.UpdatesDifference:
			r.final = &updates.State{Pts: d.State.Pts, Qts: d.State.Qts, Date: d.State.Date, Seq: d.State.Seq}
			r.empty = false
		case *tg.UpdatesDifferenceEmpty:
			r.final = &updates.State{Date: d.Date, Seq: d.Seq}
			r.empty = true
		}
	}
	return diff, err
}

func (r *updateRecovery) SetState(ctx context.Context, userID int64, state updates.State) error {
	if err := r.StateStorage.SetState(ctx, userID, state); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.final != nil && !r.empty && *r.final == state {
		r.synced, r.final = true, nil
	}
	return nil
}

func (r *updateRecovery) SetDateSeq(ctx context.Context, userID int64, date, seq int) error {
	if err := r.StateStorage.SetDateSeq(ctx, userID, date, seq); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.final != nil && r.empty && r.final.Date == date && r.final.Seq == seq {
		r.synced, r.final = true, nil
	}
	return nil
}
