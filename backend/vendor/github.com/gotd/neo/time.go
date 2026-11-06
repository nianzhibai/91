package neo

import (
	"sort"
	"sync"
	"time"
)

// Timer abstracts a single event.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration)
}

// Ticker abstracts a channel that delivers ``ticks'' of a clock at intervals.
type Ticker interface {
	C() <-chan time.Time
	Stop()
	Reset(d time.Duration)
}

// NewTime returns new temporal simulator.
func NewTime(now time.Time) *Time {
	return &Time{
		now:     now,
		moments: map[int]moment{},
	}
}

// Time simulates temporal interactions.
//
// All methods are goroutine-safe.
type Time struct {
	// mux guards internal state. Note that all methods without Unlocked
	// suffix acquire mux.
	mux      sync.Mutex
	now      time.Time
	momentID int

	moments   map[int]moment
	observers []chan struct{}
}

func (t *Time) Timer(d time.Duration) Timer {
	tt := &timer{
		time: t,
		ch:   make(chan time.Time, 1),
	}
	tt.id = t.plan(t.When(d), tt.do)
	return tt
}

func (t *Time) Ticker(d time.Duration) Ticker {
	tt := &ticker{
		time: t,
		ch:   make(chan time.Time, 1),
		dur:  d,
	}
	tt.id = t.plan(t.When(d), tt.do)
	return tt
}

func (t *Time) planUnlocked(when time.Time, do func(now time.Time)) int {
	id := t.momentID
	t.momentID++
	t.moments[id] = moment{
		when: when,
		do:   do,
	}
	t.observeUnlocked()
	return id
}

func (t *Time) plan(when time.Time, do func(now time.Time)) int {
	t.mux.Lock()
	defer t.mux.Unlock()

	return t.planUnlocked(when, do)
}

// stop removes the moment with the given ID from the list of scheduled moments.
// It returns true if a moment existed for the given ID, otherwise it is no-op.
func (t *Time) stop(id int) bool {
	t.mux.Lock()
	defer t.mux.Unlock()

	_, ok := t.moments[id]
	delete(t.moments, id)
	return ok
}

// reset adjusts the moment with the given ID to run after the d duration. It
// creates a new moment if the moment does not already exist. If durp pointer
// is not nil, it is updated with d value while reset is holding Time’s lock.
func (t *Time) reset(d time.Duration, id int, do func(now time.Time), durp *time.Duration) {
	t.mux.Lock()
	defer t.mux.Unlock()
	t.resetUnlocked(d, id, do, durp)
}

// resetUnlocked is like reset but does not acquire the Time’s lock.
func (t *Time) resetUnlocked(d time.Duration, id int, do func(now time.Time), durp *time.Duration) {
	if durp != nil {
		*durp = d
	}

	m, ok := t.moments[id]
	if !ok {
		m = moment{do: do}
	}

	m.when = t.now.Add(d)
	t.moments[id] = m
}

// tickUnlocked applies all scheduled temporal effects.
func (t *Time) tickUnlocked() moments {
	var past moments

	for id, m := range t.moments {
		if m.when.After(t.now) {
			continue
		}
		delete(t.moments, id)
		past = append(past, m)
	}
	sort.Sort(past)

	return past
}

// Now returns the current time.
func (t *Time) Now() time.Time {
	t.mux.Lock()
	defer t.mux.Unlock()
	return t.now
}

// Set travels to specified time.
//
// Also triggers temporal effects.
func (t *Time) Set(now time.Time) {
	t.mux.Lock()
	defer t.mux.Unlock()
	t.setUnlocked(now)
}

// Travel adds duration to current time and returns result.
//
// Also triggers temporal effects.
func (t *Time) Travel(d time.Duration) time.Time {
	t.mux.Lock()
	defer t.mux.Unlock()
	now := t.now.Add(d)
	t.setUnlocked(now)
	return now
}

// TravelDate applies AddDate to current time and returns result.
//
// Also triggers temporal effects.
func (t *Time) TravelDate(years, months, days int) time.Time {
	t.mux.Lock()
	defer t.mux.Unlock()
	now := t.now.AddDate(years, months, days)
	t.setUnlocked(now)
	return now
}

// setUnlocked sets the current time to the given now time and triggers temporal
// effects.
func (t *Time) setUnlocked(now time.Time) {
	t.now = now
	t.tickUnlocked().do(now)
}

// Sleep blocks until duration is elapsed.
func (t *Time) Sleep(d time.Duration) { <-t.After(d) }

// When returns relative time point.
func (t *Time) When(d time.Duration) time.Time {
	return t.Now().Add(d)
}

// After returns new channel that will receive time.Time value with current tme after
// specified duration.
func (t *Time) After(d time.Duration) <-chan time.Time {
	done := make(chan time.Time, 1)
	t.plan(t.When(d), func(now time.Time) {
		done <- now
	})
	return done
}

// Observe return channel that closes on clock calls. The current implementation
// also closes the channel on Ticker’s ticks.
func (t *Time) Observe() <-chan struct{} {
	observer := make(chan struct{})
	t.mux.Lock()
	t.observers = append(t.observers, observer)
	t.mux.Unlock()

	return observer
}

func (t *Time) observeUnlocked() {
	for _, observer := range t.observers {
		close(observer)
	}
	t.observers = t.observers[:0]
}
