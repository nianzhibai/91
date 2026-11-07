package preview

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/tasklimit"
)

func waitForDispatch(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("dispatcher did not reach the expected state")
}

func TestPreviewDispatcherKeepsOnlyOneTaskWaitingForGlobalBudget(t *testing.T) {
	cat, first := seedPreviewTestVideo(t, "bounded-dispatch")
	w := NewWorker(nil, cat, &concurrentPreviewDrive{})
	release, _ := w.Limiter.Acquire(context.Background())
	defer release()
	for i := 0; i < 100; i++ {
		v := *first
		v.ID, v.FileID = fmt.Sprintf("queued-%d", i), fmt.Sprintf("file-%d", i)
		if err := cat.UpsertVideo(context.Background(), &v); err != nil {
			t.Fatal(err)
		}
		if !w.Enqueue(&v) {
			t.Fatal("enqueue failed")
		}
	}
	var admissions atomic.Int32
	w.TaskGuard = func() func() { admissions.Add(1); return func() {} }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() { defer close(done); w.Run(ctx) }()
	waitForDispatch(t, func() bool { return admissions.Load() > 0 })
	// An occupied global budget must leave the remaining videos in the
	// channel, instead of creating processing goroutines that also wait.
	time.Sleep(30 * time.Millisecond)
	if got := admissions.Load(); got != 1 {
		t.Fatalf("waiting task admissions = %d, want 1", got)
	}
	if got := len(w.ch); got != 99 {
		t.Fatalf("videos remaining in queue = %d, want 99", got)
	}
	if got := w.Status(); got.State != "queued" || got.QueueLength != 100 {
		t.Fatalf("waiting status = %+v", got)
	}
}

func TestPreviewDrivesShareBudgetWithoutBacklogStarvation(t *testing.T) {
	cat, first := seedPreviewTestVideo(t, "fair-drive-a-1")
	limiter := tasklimit.New(1)
	releaseA := make(chan struct{})
	genA := &blockingTeaserGenerator{started: make(chan struct{}, 3), release: releaseA}
	genB := &blockingTeaserGenerator{started: make(chan struct{}, 1), release: make(chan struct{})}
	a := NewWorker(genA, cat, &concurrentPreviewDrive{})
	b := NewWorker(genB, cat, &concurrentPreviewDrive{})
	a.Limiter, b.Limiter = limiter, limiter
	for i := 0; i < 4; i++ {
		v := *first
		v.ID, v.FileID = fmt.Sprintf("fair-video-%d", i), fmt.Sprintf("fair-file-%d", i)
		worker := a
		if i == 3 {
			worker, v.DriveID = b, "drive-b"
		}
		if err := cat.UpsertVideo(context.Background(), &v); err != nil {
			t.Fatal(err)
		}
		if !worker.Enqueue(&v) {
			t.Fatal("enqueue failed")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	doneA, doneB := make(chan struct{}), make(chan struct{})
	go func() { defer close(doneA); a.Run(ctx) }()
	defer func() { cancel(); <-doneA }()
	select {
	case <-genA.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first drive did not start")
	}
	// A has one running task and one waiting task. Its third task must not
	// reserve another place ahead of drive B.
	waitForDispatch(t, func() bool { return len(a.ch) == 1 })
	go func() { defer close(doneB); b.Run(ctx) }()
	defer func() { cancel(); <-doneB }()
	waitForDispatch(t, func() bool { return len(b.ch) == 0 })
	releaseA <- struct{}{}
	select {
	case <-genB.started:
		return // B reached the limiter before A's second task.
	case <-genA.started:
	case <-time.After(2 * time.Second):
		t.Fatal("waiting task did not start")
	}
	releaseA <- struct{}{}
	select {
	case <-genB.started:
	case <-genA.started:
		t.Fatal("drive A's backlog bypassed the waiting drive B")
	case <-time.After(2 * time.Second):
		t.Fatal("waiting drive B did not receive a slot")
	}
}

func TestGlobalBudgetWaitKeepsMediaQueuedAndCancellationKeepsPending(t *testing.T) {
	for _, thumbnail := range []bool{false, true} {
		t.Run(map[bool]string{false: "preview", true: "thumbnail"}[thumbnail], func(t *testing.T) {
			cat, video := seedPreviewTestVideo(t, "waiting")
			limiter := tasklimit.New(1)
			release, _ := limiter.Acquire(context.Background())
			defer release()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var run func()
			var status func() TaskStatus
			var guard func() func()
			admitted := make(chan struct{})
			guard = func() func() { close(admitted); return func() {} }
			if thumbnail {
				w := NewThumbWorker(nil, cat, &previewFakeDrive{})
				w.Limiter, w.TaskGuard = limiter, guard
				w.Enqueue(video)
				run = func() { w.processQueued(ctx, <-w.ch) }
				status = w.Status
			} else {
				w := NewWorker(nil, cat, &previewFakeDrive{})
				w.Limiter, w.TaskGuard = limiter, guard
				w.Enqueue(video)
				run = func() { w.processQueued(ctx, <-w.ch) }
				status = w.Status
			}
			done := make(chan struct{})
			go func() { defer close(done); run() }()
			<-admitted
			select {
			case <-done:
				t.Fatal("task bypassed occupied budget")
			case <-time.After(20 * time.Millisecond):
			}
			if got := status(); got.State != "queued" || got.QueueLength != 1 {
				t.Fatalf("waiting status=%+v", got)
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("cancellation did not stop waiter")
			}
			if got := status(); got.State != "idle" || got.QueueLength != 0 {
				t.Fatalf("canceled status=%+v", got)
			}
			got, err := cat.GetVideo(context.Background(), video.ID)
			if err != nil || got.PreviewStatus != "pending" || got.ThumbnailURL != "" {
				t.Fatalf("canceled task changed video=%+v err=%v", got, err)
			}
		})
	}
}

func TestCooldownDoesNotOccupyGlobalPreviewSlot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	limiter := tasklimit.New(1)
	var cooling, healthy rateLimitState
	cooling.pause(time.Now(), time.Minute)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if release, ok := acquireGenerationSlot(ctx, limiter, &cooling, "preview", nil); ok {
			release()
			t.Error("cooling task acquired a slot")
		}
	}()
	healthyCtx, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	release, ok := acquireGenerationSlot(healthyCtx, limiter, &healthy, "preview", nil)
	if !ok {
		t.Fatal("cooling drive blocked healthy drive")
	}
	release()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cooldown did not cancel")
	}
}
