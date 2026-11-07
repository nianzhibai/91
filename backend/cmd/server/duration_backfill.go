package main

import (
	"context"
	"log"
	"sync"

	"github.com/nianzhibai/91/backend/internal/preview"
)

// A backfill belongs to the same runtime generation as its resource workers.
// Requests arriving while resources are still being generated share one sweep.
type driveDurationBackfill struct {
	ctx    context.Context
	worker *preview.ThumbWorker

	mu        sync.Mutex
	requested bool
	done      chan struct{}
	err       error
	producers int
	produced  chan struct{}
}

// Hold the handoff open until every resource producer has finished queueing.
// Workers becoming briefly idle during a bulk enqueue is not a round boundary.
func (a *App) beginDriveResourceEnqueue(driveID string) func() {
	a.mu.Lock()
	backfill := a.durationBackfills[driveID]
	a.mu.Unlock()
	if backfill == nil {
		return func() {}
	}
	backfill.mu.Lock()
	if backfill.producers == 0 {
		backfill.produced = make(chan struct{})
	}
	backfill.producers++
	backfill.mu.Unlock()
	return sync.OnceFunc(func() {
		backfill.mu.Lock()
		backfill.producers--
		start := backfill.ctx.Err() == nil && backfill.requestLocked()
		if backfill.producers == 0 {
			close(backfill.produced)
			backfill.produced = nil
		}
		backfill.mu.Unlock()
		if start {
			go a.runDriveDurationBackfill(driveID, backfill)
		}
	})
}

func (a *App) scheduleDriveDurationBackfill(driveID string) {
	a.mu.Lock()
	backfill := a.durationBackfills[driveID]
	a.mu.Unlock()
	if backfill == nil || backfill.ctx.Err() != nil {
		return
	}
	backfill.mu.Lock()
	start := backfill.requestLocked()
	backfill.mu.Unlock()
	if start {
		go a.runDriveDurationBackfill(driveID, backfill)
	}
}

func (b *driveDurationBackfill) requestLocked() bool {
	b.requested = true
	if b.done != nil {
		return false
	}
	b.done = make(chan struct{})
	b.err = nil
	return true
}

func (a *App) runDriveDurationBackfill(driveID string, backfill *driveDurationBackfill) {
	for {
		taskCtx, done := a.registerDriveTaskContextWaiting(backfill.ctx, driveID, 0)
		err := func() error {
			defer done()
			for {
				backfill.mu.Lock()
				backfill.requested = false
				produced := backfill.produced
				backfill.mu.Unlock()
				if produced != nil {
					select {
					case <-taskCtx.Done():
						return taskCtx.Err()
					case <-produced:
					}
				}
				if err := a.waitDriveGenerationQueuesIdle(taskCtx, driveID); err != nil {
					return err
				}
				backfill.mu.Lock()
				requested := backfill.requested || backfill.producers > 0
				backfill.mu.Unlock()
				if !requested {
					break
				}
			}
			return a.backfillDriveDurations(taskCtx, driveID, backfill.worker)
		}()
		if err != nil && backfill.ctx.Err() == nil {
			log.Printf("[duration] drive=%s backfill: %v", driveID, err)
		}
		backfill.mu.Lock()
		backfill.err = err
		if backfill.requested && backfill.ctx.Err() == nil {
			backfill.mu.Unlock()
			continue
		}
		close(backfill.done)
		backfill.done = nil
		backfill.mu.Unlock()
		return
	}
}

func (b *driveDurationBackfill) busy() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.done != nil || b.producers > 0
}

func (b *driveDurationBackfill) wait(ctx context.Context) error {
	if b == nil {
		return nil
	}
	for {
		b.mu.Lock()
		done := b.produced
		if done == nil {
			done = b.done
		}
		err := b.err
		b.mu.Unlock()
		if done == nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
		}
	}
}

func (a *App) waitDurationBackfillsIdle(ctx context.Context, driveID string) error {
	a.mu.Lock()
	var backfills []*driveDurationBackfill
	if driveID != "" {
		backfills = append(backfills, a.durationBackfills[driveID])
	} else {
		for _, backfill := range a.durationBackfills {
			backfills = append(backfills, backfill)
		}
	}
	a.mu.Unlock()
	for _, backfill := range backfills {
		if err := backfill.wait(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) waitDriveResourcesIdle(ctx context.Context, driveID string) error {
	if err := a.waitDriveGenerationQueuesIdle(ctx, driveID); err != nil {
		return err
	}
	return a.waitDurationBackfillsIdle(ctx, driveID)
}

func (a *App) backfillDriveDurations(ctx context.Context, driveID string, worker *preview.ThumbWorker) error {
	afterID := ""
	queued := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		videos, err := a.cat.ListVideosNeedingDuration(ctx, driveID, afterID, 100)
		if err != nil {
			return err
		}
		if len(videos) == 0 {
			break
		}
		for _, video := range videos {
			if !worker.EnqueueDurationBlocking(ctx, video) {
				return ctx.Err()
			}
			queued++
		}
		// Advance past failures as well as successes: one attempt per round.
		afterID = videos[len(videos)-1].ID
	}
	log.Printf("[duration] enqueued backfill after drive generation drive=%s count=%d", driveID, queued)
	return worker.WaitIdle(ctx)
}
