package backup

import (
	"context"
	"sync"
)

// uploadRuntime coordinates in-process access to upload sessions. Durable
// session state lives in each session's sidecar; this type only tracks
// per-session locks, in-flight range writers, lifecycle flags and progress.
type uploadRuntime struct {
	mu         sync.Mutex
	locks      map[string]*uploadSessionLock
	finalizing map[string]bool
	canceling  map[string]bool
	writers    map[string]map[int]context.CancelFunc
	progress   map[string]OperationProgress
}

type uploadSessionLock struct {
	mu   sync.Mutex
	refs int
}

func newUploadRuntime() *uploadRuntime {
	return &uploadRuntime{
		locks:      make(map[string]*uploadSessionLock),
		finalizing: make(map[string]bool),
		canceling:  make(map[string]bool),
		writers:    make(map[string]map[int]context.CancelFunc),
		progress:   make(map[string]OperationProgress),
	}
}

// lock serializes sidecar read-modify-write cycles for one session.
func (u *uploadRuntime) lock(id string) func() {
	u.mu.Lock()
	lock := u.locks[id]
	if lock == nil {
		lock = &uploadSessionLock{}
		u.locks[id] = lock
	}
	lock.refs++
	u.mu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		u.mu.Lock()
		lock.refs--
		if lock.refs == 0 && u.locks[id] == lock {
			delete(u.locks, id)
		}
		u.mu.Unlock()
	}
}

// active reports whether a session is being finalized or written.
func (u *uploadRuntime) active(id string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.finalizing[id] || len(u.writers[id]) > 0
}

func (u *uploadRuntime) isFinalizing(id string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.finalizing[id]
}

func (u *uploadRuntime) isCanceling(id string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.canceling[id]
}

// beginWrite registers a range writer. It fails when the session is closing
// or the same range is already being written.
func (u *uploadRuntime) beginWrite(id string, index int, cancel context.CancelFunc) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.finalizing[id] || u.canceling[id] {
		return ErrUploadNotFound
	}
	writers := u.writers[id]
	if writers == nil {
		writers = make(map[int]context.CancelFunc)
		u.writers[id] = writers
	}
	if _, active := writers[index]; active {
		return ErrUploadRangeBusy
	}
	writers[index] = cancel
	return nil
}

// endWrite unregisters a range writer and reports whether it was the last
// writer of a session whose cancellation is waiting for it.
func (u *uploadRuntime) endWrite(id string, index int) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	writers := u.writers[id]
	delete(writers, index)
	if len(writers) != 0 {
		return false
	}
	delete(u.writers, id)
	return u.canceling[id]
}

func (u *uploadRuntime) beginFinalize(id string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.finalizing[id] || u.canceling[id] || len(u.writers[id]) > 0 {
		return ErrUploadFinalizing
	}
	u.finalizing[id] = true
	return nil
}

func (u *uploadRuntime) endFinalize(id string) {
	u.mu.Lock()
	delete(u.finalizing, id)
	u.mu.Unlock()
}

// beginCancel marks a session as canceling and returns the cancel functions
// of its in-flight writers. The last writer to exit completes the cleanup.
func (u *uploadRuntime) beginCancel(id string) ([]context.CancelFunc, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.finalizing[id] {
		return nil, ErrUploadFinalizing
	}
	u.canceling[id] = true
	writers := make([]context.CancelFunc, 0, len(u.writers[id]))
	for _, cancel := range u.writers[id] {
		writers = append(writers, cancel)
	}
	return writers, nil
}

// cancelReady reports whether a canceled session no longer has any user.
func (u *uploadRuntime) cancelReady(id string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.canceling[id] && !u.finalizing[id] && len(u.writers[id]) == 0
}

func (u *uploadRuntime) endCancel(id string, removed bool) {
	u.mu.Lock()
	delete(u.canceling, id)
	if removed {
		delete(u.progress, id)
	}
	u.mu.Unlock()
}

func (u *uploadRuntime) setProgress(id string, progress OperationProgress) {
	u.mu.Lock()
	u.progress[id] = normalizeOperationProgress(progress)
	u.mu.Unlock()
}

func (u *uploadRuntime) progressSnapshot(id string) *OperationProgress {
	u.mu.Lock()
	defer u.mu.Unlock()
	progress, ok := u.progress[id]
	if !ok {
		return nil
	}
	return &progress
}

func (u *uploadRuntime) clearProgress(id string) {
	u.mu.Lock()
	delete(u.progress, id)
	u.mu.Unlock()
}
