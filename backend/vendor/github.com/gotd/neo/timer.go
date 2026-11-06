package neo

import "time"

type timer struct {
	time *Time
	ch   chan time.Time
	id   int
}

func (t *timer) C() <-chan time.Time {
	return t.ch
}

func (t *timer) Stop() bool {
	return t.time.stop(t.id)
}

func (t *timer) Reset(d time.Duration) {
	t.time.reset(d, t.id, t.do, nil)
}

// do is the timer’s moment callback. It sends the now time to the underlying
// channel. Note that do runs under Time’s lock.
func (t *timer) do(now time.Time) {
	t.ch <- now
}
