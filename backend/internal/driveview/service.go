package driveview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nianzhibai/91/backend/internal/driveevents"
)

type Snapshot struct {
	Epoch     string          `json:"epoch"`
	DriveID   string          `json:"driveId"`
	Resource  Resource        `json:"resource"`
	Revision  uint64          `json:"revision"`
	UpdatedAt time.Time       `json:"updatedAt"`
	Data      json.RawMessage `json:"data,omitempty"`
	Error     string          `json:"error,omitempty"`
	Status    int             `json:"status,omitempty"`
}

type Loader func(context.Context, string, Resource) (any, error)

type LoadError struct {
	Status int
	Err    error
}

func (e *LoadError) Error() string { return e.Err.Error() }
func (e *LoadError) Unwrap() error { return e.Err }

type cacheKey struct {
	driveID  string
	resource Resource
}
type cacheEntry struct {
	snapshot   Snapshot
	generation uint64
	loadedAt   time.Time
	loading    chan struct{}
}

type Service struct {
	events   *driveevents.Hub
	load     Loader
	period   func(Resource) time.Duration
	epoch    string
	mu       sync.Mutex
	revision uint64
	entries  map[cacheKey]*cacheEntry
	watches  map[string]*driveWatch
}

type driveWatch struct {
	driveID     string
	cancel      context.CancelFunc
	workers     sync.WaitGroup
	refresh     map[Resource]chan struct{}
	subscribers map[*snapshotSubscription]struct{}
}

type resourceCursor struct {
	minimumGeneration uint64
	revision          uint64
}

type snapshotSubscription struct {
	service *Service
	watch   *driveWatch
	wake    chan struct{}
	pending map[Resource]Snapshot
	cursors map[Resource]resourceCursor
}

func New(events *driveevents.Hub, load Loader) *Service {
	return &Service{events: events, load: load, period: refreshPeriod, epoch: uuid.NewString(),
		entries: make(map[cacheKey]*cacheEntry), watches: make(map[string]*driveWatch)}
}

func refreshPeriod(resource Resource) time.Duration {
	if resource == Storage {
		return 15 * time.Second
	}
	return 30 * time.Second
}

func mergeWindow(resource Resource) time.Duration {
	switch resource {
	case Runtime:
		return 300 * time.Millisecond
	case Stats:
		return time.Second
	case Storage:
		return 10 * time.Second
	default:
		return 0
	}
}

// Get shares one computation per drive/resource. Caller cancellation stops its
// wait, while a bounded independent computation can still populate the cache
// for another reader. Each resource has its own cache and loading state.
func (s *Service) Get(ctx context.Context, driveID string, resource Resource, force bool) (Snapshot, error) {
	snapshot, _, err := s.get(ctx, driveID, resource, force)
	return snapshot, err
}

func (s *Service) fresh(entry *cacheEntry, generation uint64, resource Resource) bool {
	if entry == nil || entry.loading != nil || entry.snapshot.Revision == 0 || entry.generation != generation {
		return false
	}
	period := s.period(resource)
	if entry.snapshot.Error != "" {
		period = time.Second
	}
	return time.Since(entry.loadedAt) < period
}

func (s *Service) get(ctx context.Context, driveID string, resource Resource, force bool) (Snapshot, uint64, error) {
	key := cacheKey{driveID, resource}
	minimumGeneration := s.events.Version(driveID, resourceEvents[resource]...)
	for {
		generation := s.events.Version(driveID, resourceEvents[resource]...)
		s.mu.Lock()
		entry := s.entries[key]
		if entry == nil {
			// Revisions belong to the service, so evicting an idle cache entry
			// cannot make an older client response look newer after recreation.
			if len(s.entries) >= 512 {
				var oldestKey cacheKey
				var oldest *cacheEntry
				for candidateKey, candidate := range s.entries {
					if candidate.loading == nil && (oldest == nil || candidate.loadedAt.Before(oldest.loadedAt)) {
						oldestKey, oldest = candidateKey, candidate
					}
				}
				if oldest != nil {
					delete(s.entries, oldestKey)
				}
			}
			entry = &cacheEntry{}
			s.entries[key] = entry
		}
		if !force && s.fresh(entry, generation, resource) {
			snapshot := entry.snapshot
			s.mu.Unlock()
			return snapshot, generation, nil
		}
		if entry.loading == nil {
			entry.loading = make(chan struct{})
			go s.compute(key, entry, generation)
		}
		done := entry.loading
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return Snapshot{}, 0, ctx.Err()
		case <-done:
			s.mu.Lock()
			snapshot := entry.snapshot
			generation := entry.generation
			s.mu.Unlock()
			if resource == Config {
				minimumGeneration = max(minimumGeneration, s.events.Version(driveID, resourceEvents[resource]...))
			}
			if generation < minimumGeneration {
				continue
			}
			return snapshot, generation, nil
		}
	}
}

func (s *Service) compute(key cacheKey, entry *cacheEntry, generation uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data, err := s.load(ctx, key.driveID, key.resource)
	var payload json.RawMessage
	if err == nil {
		payload, err = json.Marshal(data)
	}
	next := Snapshot{Epoch: s.epoch, DriveID: key.driveID, Resource: key.resource, Data: payload, UpdatedAt: time.Now().UTC()}
	if err != nil {
		next.Error = err.Error()
		next.Status = 500
		var loadError *LoadError
		if errors.As(err, &loadError) {
			next.Status = loadError.Status
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := entry.snapshot
	if previous.Revision != 0 && bytes.Equal(previous.Data, next.Data) && previous.Error == next.Error && previous.Status == next.Status {
		next.Revision = previous.Revision
	} else {
		s.revision++
		next.Revision = s.revision
	}
	entry.snapshot = next
	entry.generation = generation
	entry.loadedAt = time.Now()
	if watch := s.watches[key.driveID]; watch != nil {
		s.publishLocked(watch, next, generation)
	}
	close(entry.loading)
	entry.loading = nil
}

// Stream shares one watch per drive. Connections only receive snapshots;
// independent resource workers own invalidation and periodic reconciliation.
func (s *Service) Stream(ctx context.Context, driveID string, emit func(Snapshot) error) error {
	sub := s.subscribe(driveID)
	defer sub.close()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-sub.wake:
			pending := sub.take()
			for _, resource := range Resources {
				if snapshot, ok := pending[resource]; ok {
					if err := emit(snapshot); err != nil {
						return err
					}
				}
			}
		}
	}
}

func (s *Service) subscribe(driveID string) *snapshotSubscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	watch := s.watches[driveID]
	if watch == nil {
		ctx, cancel := context.WithCancel(context.Background())
		watch = &driveWatch{driveID: driveID, cancel: cancel, refresh: make(map[Resource]chan struct{}), subscribers: make(map[*snapshotSubscription]struct{})}
		s.watches[driveID] = watch
		watch.workers.Add(len(Resources))
		for _, resource := range Resources {
			events := s.events.Subscribe(driveID)
			watch.refresh[resource] = make(chan struct{}, 1)
			go func(resource Resource, refresh <-chan struct{}) {
				defer watch.workers.Done()
				defer events.Close()
				s.watchResource(ctx, driveID, resource, events, watch, refresh)
			}(resource, watch.refresh[resource])
		}
	}
	sub := &snapshotSubscription{service: s, watch: watch, wake: make(chan struct{}, 1),
		pending: make(map[Resource]Snapshot), cursors: make(map[Resource]resourceCursor)}
	for _, resource := range Resources {
		// A subscriber opened after a write must await that write's generation,
		// even when the cached content revision will remain unchanged.
		generation := s.events.Version(driveID, resourceEvents[resource]...)
		cursor := resourceCursor{minimumGeneration: generation}
		entry := s.entries[cacheKey{driveID, resource}]
		if s.fresh(entry, generation, resource) {
			sub.pending[resource] = entry.snapshot
			cursor.revision = entry.snapshot.Revision
		} else {
			select {
			case watch.refresh[resource] <- struct{}{}:
			default:
			}
		}
		sub.cursors[resource] = cursor
	}
	if len(sub.pending) > 0 {
		sub.wake <- struct{}{}
	}
	watch.subscribers[sub] = struct{}{}
	return sub
}

func (s *snapshotSubscription) take() map[Resource]Snapshot {
	s.service.mu.Lock()
	defer s.service.mu.Unlock()
	pending := s.pending
	s.pending = make(map[Resource]Snapshot)
	return pending
}

func (s *snapshotSubscription) close() {
	s.service.mu.Lock()
	delete(s.watch.subscribers, s)
	last := len(s.watch.subscribers) == 0
	if last && s.service.watches[s.watch.driveID] == s.watch {
		delete(s.service.watches, s.watch.driveID)
	}
	s.service.mu.Unlock()
	if last {
		s.watch.cancel()
		s.watch.workers.Wait()
	}
}

func (s *Service) publishLocked(watch *driveWatch, snapshot Snapshot, generation uint64) {
	if snapshot.Resource == Config && generation < s.events.Version(watch.driveID, resourceEvents[Config]...) {
		return
	}
	for sub := range watch.subscribers {
		cursor := sub.cursors[snapshot.Resource]
		if generation < cursor.minimumGeneration || snapshot.Revision <= cursor.revision {
			continue
		}
		cursor.revision = snapshot.Revision
		sub.cursors[snapshot.Resource] = cursor
		sub.pending[snapshot.Resource] = snapshot
		select {
		case sub.wake <- struct{}{}:
		default:
		}
	}
}

func (s *Service) watchResource(ctx context.Context, driveID string, resource Resource, sub *driveevents.Subscription, watch *driveWatch, refresh <-chan struct{}) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	due := time.Now()
	lastStart := time.Time{}
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-refresh:
			due = time.Now()
			timer.Reset(0)
		case <-sub.Wake:
			change := sub.Take()
			changed, immediate := false, false
			for _, kind := range resourceEvents[resource] {
				if prompt, ok := change.Kinds[kind]; ok {
					changed, immediate = true, immediate || prompt
				}
			}
			if !changed {
				continue
			}
			next := maxTime(time.Now(), lastStart.Add(mergeWindow(resource)))
			if immediate {
				next = time.Now()
			}
			// A burst keeps the first deadline instead of extending a debounce.
			if next.Before(due) {
				timer.Reset(time.Until(next))
				due = next
			}
		case <-timer.C:
			lastStart = time.Now()
			snapshot, generation, err := s.get(ctx, driveID, resource, false)
			if err != nil {
				return
			}
			s.mu.Lock()
			if s.watches[driveID] == watch {
				s.publishLocked(watch, snapshot, generation)
			}
			s.mu.Unlock()
			period := s.period(resource)
			if snapshot.Error != "" {
				failures++
				period = time.Second * time.Duration(1<<min(failures-1, 5))
				period = min(period, 30*time.Second)
			} else {
				failures = 0
			}
			due = time.Now().Add(period)
			timer.Reset(time.Until(due))
		}
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
