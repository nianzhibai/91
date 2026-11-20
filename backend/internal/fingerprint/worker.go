package fingerprint

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"syscall"
	"time"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/dedupe"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/streamhttp"
	"github.com/nianzhibai/91/backend/internal/tasklimit"
)

const defaultWorkerQueueSize = 10000

type Config struct {
	Limiter *tasklimit.Limiter
	// RateLimitCooldown is the pause when a throttling error gives no retry time.
	RateLimitCooldown time.Duration
	// StallTimeout ends a remote read that receives no data for this long.
	StallTimeout time.Duration
	HTTPClient   *http.Client
}

type Worker struct {
	Catalog         *catalog.Catalog
	Drive           drives.Drive
	Config          Config
	TaskGuard       func() func()
	OnStatusChanged func(bool)
	// OnContentReplaced receives a row whose recomputed fingerprint showed that
	// its source was replaced in place. Its generated assets were cleared and
	// need to be scheduled again.
	OnContentReplaced func(*catalog.Video)
	// Cooldown is shared by all workers of the drive. The application injects
	// it before Run starts; standalone workers get their own.
	Cooldown *tasklimit.Cooldown

	ch       chan *catalog.Video
	queue    videoQueue
	activity taskActivity
	http     *http.Client
}

type TaskStatus struct {
	State         string
	CurrentTitle  string
	QueueLength   int
	CooldownUntil time.Time
}

func NewWorker(cat *catalog.Catalog, drv drives.Drive, cfg Config) *Worker {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = streamhttp.NewClient(0)
	}
	if cfg.RateLimitCooldown <= 0 {
		cfg.RateLimitCooldown = drives.DefaultCooldown
	}
	return &Worker{
		Catalog:  cat,
		Drive:    drv,
		Config:   cfg,
		Cooldown: &tasklimit.Cooldown{},
		ch:       make(chan *catalog.Video, defaultWorkerQueueSize),
		http:     hc,
	}
}

func (w *Worker) Enqueue(v *catalog.Video) bool {
	defer w.notifyStatus(false)
	if v == nil {
		return false
	}
	if !w.queue.reserve(v.ID) {
		return true
	}
	select {
	case w.ch <- v:
		return true
	default:
		w.queue.release(v.ID)
		return false
	}
}

func (w *Worker) EnqueueBlocking(ctx context.Context, v *catalog.Video) bool {
	defer w.notifyStatus(false)
	if v == nil {
		return false
	}
	if !w.queue.reserve(v.ID) {
		return true
	}
	select {
	case w.ch <- v:
		return true
	case <-ctx.Done():
		w.queue.release(v.ID)
		return false
	}
}

func (w *Worker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case v := <-w.ch:
			w.processQueued(ctx, v)
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
}

func (w *Worker) Status() TaskStatus {
	if w == nil {
		return TaskStatus{State: "idle"}
	}
	currentID, currentTitle := w.activity.current()
	status := TaskStatus{
		State:        "idle",
		CurrentTitle: currentTitle,
		QueueLength:  w.queue.lengthExcluding(currentID),
	}
	if until, ok := w.Cooldown.Until(time.Now()); ok {
		status.State = "cooling"
		status.CooldownUntil = until
		return status
	}
	if currentID != "" {
		status.State = "generating"
		return status
	}
	if status.QueueLength > 0 {
		status.State = "queued"
	}
	return status
}

// WaitIdle blocks until the fingerprint queue is empty and no item is being processed.
func (w *Worker) WaitIdle(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if w.queue.lengthExcluding("") == 0 {
		return nil
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if w.queue.lengthExcluding("") == 0 {
				return nil
			}
		}
	}
}

func (w *Worker) processQueued(ctx context.Context, v *catalog.Video) {
	if v == nil {
		return
	}
	if w.Catalog == nil || w.Drive == nil || v.ID == "" {
		w.queue.release(v.ID)
		return
	}
	ctx = applog.WithFields(applog.NewTask(ctx, "fingerprint", w.Drive.ID()), applog.Fields{VideoID: v.ID, FileID: v.FileID})
	// A cooling drive waits before taking task admission or shared budget.
	if err := w.Cooldown.Wait(ctx); err != nil {
		w.queue.release(v.ID)
		return
	}
	if w.TaskGuard != nil {
		release := w.TaskGuard()
		if release == nil {
			w.queue.release(v.ID)
			return
		}
		defer release()
	}
	defer func() { w.queue.release(v.ID); w.notifyStatus(true) }()
	if err := ctx.Err(); err != nil {
		return
	}
	current, err := w.Catalog.GetVideo(ctx, v.ID)
	if err != nil {
		if ctx.Err() == nil {
			applog.Error(ctx, "Read queued video failed", err, applog.Fields{Stage: "lookup"})
		}
		return
	}
	if current.DriveID != w.Drive.ID() || dedupe.IsCurrentFingerprint(current.SampledSHA256) || current.Hidden {
		return
	}
	release, err := w.Config.Limiter.AcquireAfter(ctx, w.Cooldown)
	if err != nil {
		return
	}
	w.activity.start(current)
	w.notifyStatus(true)
	defer w.activity.done()
	sum, err := compute(ctx, w.Drive, current, w.Config, w.http)
	release()
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		if w.pauseForProviderError(ctx, err) {
			// The video stays pending, and the reconciler queues it again.
			return
		}
		applog.Error(ctx, "Fingerprint computation failed", err, applog.Fields{Stage: "compute"})
		_, _ = w.Catalog.UpdateVideoFingerprintForSource(ctx, current, "", "failed", err.Error())
		return
	}
	replaced, err := w.Catalog.UpdateVideoFingerprintForSource(ctx, current, sum, "ready", "")
	if err != nil {
		applog.Error(ctx, "Save fingerprint failed", err, applog.Fields{Stage: "save"})
		return
	}
	if !replaced {
		applog.Info(ctx, "Fingerprint ready", applog.Fields{})
		return
	}
	applog.Info(ctx, "Source replaced in place; regenerating its assets", applog.Fields{})
	if w.OnContentReplaced != nil {
		if updated, err := w.Catalog.GetVideo(ctx, current.ID); err == nil {
			w.OnContentReplaced(updated)
		}
	}
}

// pauseForProviderError pauses the drive's provider work and reports true when
// err says nothing about the file itself: the provider is throttling or briefly
// unavailable, or the read timed out or lost its connection.
func (w *Worker) pauseForProviderError(ctx context.Context, err error) bool {
	wait, limited := drives.RateLimitRetryAfter(err)
	if !limited && !drives.ShouldCooldown(w.Drive.Kind(), err) && !transientTransportError(err) {
		return false
	}
	if wait <= 0 {
		wait = w.Config.RateLimitCooldown
	}
	until := w.Cooldown.Pause(time.Now(), wait)
	w.notifyStatus(true)
	applog.Warn(ctx, "Fingerprint paused; drive cooling down until "+until.Format(time.RFC3339), err, applog.Fields{Stage: "compute"})
	return true
}

func transientTransportError(err error) bool {
	var timeout interface{ Timeout() bool }
	return (errors.As(err, &timeout) && timeout.Timeout()) || errors.Is(err, syscall.ECONNRESET)
}

type taskActivity struct {
	mu           sync.Mutex
	currentID    string
	currentTitle string
}

func (a *taskActivity) start(v *catalog.Video) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if v == nil {
		a.currentID = ""
		a.currentTitle = ""
		return
	}
	a.currentID = v.ID
	a.currentTitle = v.Title
}

func (a *taskActivity) done() {
	a.mu.Lock()
	a.currentID = ""
	a.currentTitle = ""
	a.mu.Unlock()
}

func (a *taskActivity) current() (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.currentID, a.currentTitle
}

type videoQueue struct {
	mu  sync.Mutex
	ids map[string]struct{}
}

func (q *videoQueue) reserve(id string) bool {
	if id == "" {
		return true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ids == nil {
		q.ids = make(map[string]struct{})
	}
	if _, ok := q.ids[id]; ok {
		return false
	}
	q.ids[id] = struct{}{}
	return true
}

func (q *videoQueue) release(id string) {
	if id == "" {
		return
	}
	q.mu.Lock()
	delete(q.ids, id)
	q.mu.Unlock()
}

func (q *videoQueue) lengthExcluding(currentID string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.ids)
	if currentID != "" {
		if _, ok := q.ids[currentID]; ok {
			n--
		}
	}
	if n < 0 {
		return 0
	}
	return n
}

func (w *Worker) notifyStatus(immediate bool) {
	if w.OnStatusChanged != nil {
		w.OnStatusChanged(immediate)
	}
}
