package preview

import (
	"context"
	"sync"
)

// Gate is the process-wide preview switch. Tasks are admitted through it, and
// disabling it cancels every admitted task before Set returns, so running
// generation stops instead of finishing. The zero value is enabled; a nil Gate
// is always enabled.
type Gate struct {
	mu       sync.Mutex
	disabled bool
	tasks    map[*gateTask]struct{}
}

type gateTask struct{ cancel context.CancelFunc }

func (g *Gate) Enabled() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.disabled
}

// Set reports whether the state changed.
func (g *Gate) Set(enabled bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.disabled != enabled {
		return false
	}
	g.disabled = !enabled
	if !enabled {
		for task := range g.tasks {
			task.cancel()
		}
		clear(g.tasks)
	}
	return true
}

// admit returns a task context derived from parent that the gate cancels when
// it is disabled. ok is false while disabled; stop must be called when the task
// finishes.
func (g *Gate) admit(parent context.Context) (ctx context.Context, stop func(), ok bool) {
	ctx, cancel := context.WithCancel(parent)
	if g == nil {
		return ctx, cancel, true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.disabled {
		cancel()
		return nil, nil, false
	}
	task := &gateTask{cancel: cancel}
	if g.tasks == nil {
		g.tasks = make(map[*gateTask]struct{})
	}
	g.tasks[task] = struct{}{}
	return ctx, func() {
		g.mu.Lock()
		delete(g.tasks, task)
		g.mu.Unlock()
		cancel()
	}, true
}
