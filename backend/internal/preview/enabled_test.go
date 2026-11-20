package preview

import (
	"context"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
)

func TestGateCancelsAdmittedTasksWhenDisabled(t *testing.T) {
	var gate Gate
	if !gate.Enabled() || gate.Set(true) {
		t.Fatal("zero gate must be enabled and unchanged by enabling")
	}
	ctx, stop, ok := gate.admit(context.Background())
	if !ok {
		t.Fatal("enabled gate rejected a task")
	}
	defer stop()
	finished, stopFinished, _ := gate.admit(context.Background())
	stopFinished()
	if !gate.Set(false) || ctx.Err() == nil {
		t.Fatal("disabling did not synchronously cancel the admitted task")
	}
	if len(gate.tasks) != 0 || finished.Err() == nil {
		t.Fatal("gate retained task registrations")
	}
	if _, _, ok := gate.admit(context.Background()); ok {
		t.Fatal("disabled gate admitted a task")
	}
	if !gate.Set(true) {
		t.Fatal("enabling did not report a change")
	}
	next, stopNext, ok := gate.admit(context.Background())
	if !ok || next.Err() != nil {
		t.Fatal("re-enabled gate did not admit a live task")
	}
	stopNext()
	var missing *Gate
	if !missing.Enabled() {
		t.Fatal("nil gate must be enabled")
	}
}

func TestGlobalSwitchRejectsNewAndQueuedPreviews(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "global-preview")
	gen := &fakeTeaserGenerator{}
	worker := NewWorker(gen, cat, &previewFakeDrive{})
	worker.Gate = &Gate{}
	worker.Gate.Set(false)
	if worker.Enqueue(video) || worker.EnqueueBlocking(ctx, video) || worker.Status().QueueLength != 0 {
		t.Fatal("disabled worker admitted a preview")
	}
	worker.Gate.Set(true)
	if !worker.Enqueue(video) {
		t.Fatal("enabled worker rejected a preview")
	}
	worker.Gate.Set(false)
	worker.processQueued(ctx, <-worker.ch)
	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil || got.PreviewStatus != "pending" || worker.Status().QueueLength != 0 || gen.generateCalls != 0 {
		t.Fatalf("queued preview was not left pending: %+v, %v", got, err)
	}
	worker.Gate.Set(true)
	if !worker.Enqueue(video) {
		t.Fatal("re-enabled worker rejected pending preview")
	}
	run := worker.prepareQueued(ctx, <-worker.ch)
	if run == nil {
		t.Fatal("could not prepare preview")
	}
	worker.Gate.Set(false)
	run()
	if gen.generateCalls != 0 || worker.Status().QueueLength != 0 {
		t.Fatal("preview started after global switch was disabled")
	}
	worker.Gate.Set(true)
	worker.Enqueue(video)
	worker.processQueued(ctx, <-worker.ch)
	got, err = cat.GetVideo(ctx, video.ID)
	if err != nil || got.PreviewStatus != "ready" || gen.generateCalls != 1 {
		t.Fatalf("re-enabled preview did not complete: %+v, %v", got, err)
	}
}

// startBlockedPreview runs one queued preview until its generator is inside
// Generate, and returns a channel closed when the task has fully finished.
func startBlockedPreview(t *testing.T, ctx context.Context, id string) (*Worker, *Gate, <-chan struct{}) {
	t.Helper()
	cat, video := seedPreviewTestVideo(t, id)
	gen := &blockingTeaserGenerator{started: make(chan struct{}, 1), release: make(chan struct{})}
	worker := NewWorker(gen, cat, &previewFakeDrive{})
	gate := &Gate{}
	worker.Gate = gate
	if !worker.Enqueue(video) {
		t.Fatal("could not enqueue preview")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.processQueued(ctx, <-worker.ch)
	}()
	select {
	case <-gen.started:
	case <-ctx.Done():
		t.Fatal("generation did not start")
	}
	return worker, gate, done
}

func TestDisablingPreviewsCancelsRunningGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	worker, gate, done := startBlockedPreview(t, ctx, "running-global-preview")
	gate.Set(false)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("running generation was not canceled")
	}
	got, err := worker.Catalog.GetVideo(ctx, "running-global-preview")
	if err != nil || got.PreviewStatus != "pending" || got.PreviewLocal != "" {
		t.Fatalf("canceled generation changed the video: %+v, %v", got, err)
	}
	status := worker.Status()
	if status.QueueLength != 0 || !status.CooldownUntil.IsZero() {
		t.Fatalf("canceled generation was requeued or cooled down: %+v", status)
	}
}

func TestReenablingPreviewsRequeuesInterruptedGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	worker, gate, done := startBlockedPreview(t, ctx, "reopened-global-preview")
	// The resume scan can run while the interrupted task still holds its queue
	// reservation; the task itself must hand the video back.
	gate.Set(false)
	gate.Set(true)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("interrupted generation did not finish")
	}
	if worker.Status().QueueLength != 1 {
		t.Fatalf("interrupted preview was not requeued: %+v", worker.Status())
	}
}

// fullQueueWorker returns a worker whose one-slot queue is already full with
// an unrelated video, so any non-blocking readmission would be lost.
func fullQueueWorker(t *testing.T, id string) (*Worker, *Gate, *catalog.Video) {
	t.Helper()
	cat, video := seedPreviewTestVideo(t, id)
	worker := NewWorker(&fakeTeaserGenerator{}, cat, &previewFakeDrive{})
	worker.ch = make(chan *catalog.Video, 1)
	gate := &Gate{}
	worker.Gate = gate
	if !worker.Enqueue(video) {
		t.Fatal("could not enqueue preview")
	}
	if <-worker.ch != video || !worker.Enqueue(&catalog.Video{ID: "filler"}) {
		t.Fatal("could not fill the queue")
	}
	return worker, gate, video
}

// toggleAndResume mimics a quick off/on while video is still reserved: the
// resume scan's enqueue is absorbed by the existing reservation.
func toggleAndResume(t *testing.T, worker *Worker, gate *Gate, video *catalog.Video) {
	t.Helper()
	gate.Set(false)
	gate.Set(true)
	if !worker.Enqueue(video) {
		t.Fatal("resume enqueue reported failure")
	}
}

func TestQuickToggleReadmitsVideoHeldByDispatcherWithFullQueue(t *testing.T) {
	ctx := context.Background()
	worker, gate, video := fullQueueWorker(t, "dispatcher-toggle")
	toggled := false
	worker.TaskGuard = func() func() {
		if !toggled {
			toggled = true
			toggleAndResume(t, worker, gate, video)
		}
		return func() {}
	}
	run := worker.prepareQueued(ctx, video)
	if run == nil {
		t.Fatal("video stopped by the gate was dropped instead of readmitted")
	}
	run()
	got, err := worker.Catalog.GetVideo(ctx, video.ID)
	if err != nil || got.PreviewStatus != "ready" {
		t.Fatalf("readmitted preview did not complete: %+v, %v", got, err)
	}
}

func TestQuickToggleRequeuesPreparedTaskWithFullQueue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	worker, gate, video := fullQueueWorker(t, "task-toggle")
	filler := <-worker.ch
	run := worker.prepareQueued(ctx, video)
	if run == nil {
		t.Fatal("could not prepare preview")
	}
	worker.ch <- filler
	toggleAndResume(t, worker, gate, video)
	done := make(chan struct{})
	go func() {
		defer close(done)
		run()
	}()
	// The task must wait for queue space instead of giving up on the full
	// queue; it cannot finish before the queue is drained.
	select {
	case <-done:
		t.Fatal("task stopped by the gate gave up on the full queue")
	case <-time.After(100 * time.Millisecond):
	}
	if got := <-worker.ch; got != filler {
		t.Fatalf("queue head = %s, want filler", got.ID)
	}
	select {
	case got := <-worker.ch:
		if got.ID != video.ID {
			t.Fatalf("requeued %s, want %s", got.ID, video.ID)
		}
	case <-ctx.Done():
		t.Fatal("task stopped by the gate was not requeued")
	}
}
