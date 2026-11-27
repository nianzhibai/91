package applog

import (
	"sync"
)

const liveSubscriberBuffer = 256

type liveSubscriber struct {
	query Query
	ch    chan Record
}

// Hub broadcasts newly ingested records to active SSE subscribers without
// blocking writers.
type Hub struct {
	mu   sync.RWMutex
	subs map[*liveSubscriber]struct{}
}

func NewHub() *Hub {
	return &Hub{subs: make(map[*liveSubscriber]struct{})}
}

func (h *Hub) Subscribe(query Query) (<-chan Record, func()) {
	if h == nil {
		ch := make(chan Record)
		close(ch)
		return ch, func() {}
	}
	sub := &liveSubscriber{
		query: query,
		ch:    make(chan Record, liveSubscriberBuffer),
	}
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, sub)
			h.mu.Unlock()
		})
	}
	return sub.ch, cancel
}

func (h *Hub) Publish(rec Record) {
	if h == nil {
		return
	}
	h.mu.RLock()
	for sub := range h.subs {
		if !sub.query.matchesLive(rec) {
			continue
		}
		select {
		case sub.ch <- rec:
		default:
			// Slow SSE consumer: drop rather than stalling application logging.
		}
	}
	h.mu.RUnlock()
}

func (q Query) matchesLive(rec Record) bool {
	// Live stream ignores pagination bounds (Before/Limit) and matches filters + After.
	liveQuery := q
	liveQuery.Before = 0
	liveQuery.Limit = 0
	return rec.Matches(liveQuery)
}
