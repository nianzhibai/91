// Package generation runs each drive's resource generation from the catalog.
//
// A drive has one Runner with two lanes that take the due work the catalog
// records. The fingerprint lane samples a few MiB of one video at a time and
// takes no slot. The asset lane runs media jobs, covers and duration probes
// for videos whose fingerprint has settled, so a duplicate never reaches
// them. It runs up to the drive's parallelism of videos at once, each holding
// one slot of the application-wide limiter, whose size is the number of
// videos decoding at once. Both lanes wait out the drive's cooldown, and
// neither holds a slot while it does.
package generation

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/tasklimit"
)

const (
	defaultPollInterval = time.Minute
	defaultPace         = 500 * time.Millisecond
	defaultRetryDelay   = 6 * time.Hour
	defaultBatchSize    = 32
	countInterval       = time.Second
)

// Kind names a generation step.
type Kind string

const (
	KindFingerprint Kind = "fingerprint"
	KindMedia       Kind = "media"
	KindThumbnail   Kind = "thumbnail"
	KindDuration    Kind = "duration"
)

// StepOutcome describes how a generation step ended.
type StepOutcome uint8

const (
	// StepSettled means the step finished its attempt and the runner may proceed
	// to any remaining steps for the video.
	StepSettled StepOutcome = iota
	// StepPaused means the video stayed pending because the drive entered
	// cooldown or the task was interrupted, which ends the video's turn without
	// deferring it.
	StepPaused
	// StepUnsettled means the step attempted to read or probe the source and
	// left work unsettled (for example, an unreadable stream or missing
	// duration), so the runner should stop this video's turn and let settle
	// defer it rather than immediately retrying the same source in later steps.
	StepUnsettled
)

// Step runs one kind of work for a video whose catalog row says it is due.
type Step interface {
	Run(ctx context.Context, video *catalog.Video) StepOutcome
}

// StepFunc adapts a function to Step.
type StepFunc func(context.Context, *catalog.Video) StepOutcome

func (f StepFunc) Run(ctx context.Context, video *catalog.Video) StepOutcome { return f(ctx, video) }

// Steps are a drive's steps. The fingerprint lane runs Fingerprint; the asset
// lane runs the others in field order.
type Steps struct {
	Fingerprint Step
	Media       Step
	Thumbnail   Step
	Duration    Step
}

func due(steps catalog.GenerationSteps, kind Kind) bool {
	switch kind {
	case KindFingerprint:
		return steps.Fingerprint
	case KindMedia:
		return steps.Media
	case KindThumbnail:
		return steps.Thumbnail
	case KindDuration:
		return steps.Duration
	}
	return false
}

// Store is the catalog surface a runner reads and writes.
type Store interface {
	ListDueFingerprints(ctx context.Context, driveID string, now time.Time, limit int) ([]string, error)
	ListDueAssets(ctx context.Context, driveID string, previewsEnabled bool, now time.Time, limit int) ([]string, error)
	GenerationWork(ctx context.Context, id string, previewsEnabled bool) (*catalog.Video, catalog.GenerationSteps, error)
	CountDueGeneration(ctx context.Context, driveID string, previewsEnabled bool, now time.Time) (catalog.GenerationCounts, error)
	DeferGeneration(ctx context.Context, id string, until time.Time) error
}

type Config struct {
	DriveID string
	Store   Store
	Steps   Steps
	// Limiter is shared by every runner; its size is how many videos run asset
	// steps at once. Nil imposes no limit.
	Limiter *tasklimit.Limiter
	// Parallelism is how many videos the asset lane runs at once, never more
	// than the limiter's size. Zero or less takes as many as the limiter has,
	// or one without a limiter. SetParallelism changes it.
	Parallelism int
	// Cooldown is the drive's provider cooldown, shared with its steps.
	Cooldown *tasklimit.Cooldown
	// PreviewsEnabled reports the preview switch. Nil enables previews.
	PreviewsEnabled func() bool
	// Admit takes task admission for one video. A nil release refuses it: the
	// runner belongs to a runtime that is being replaced, or a configuration
	// change is being applied. The runner then counts as idle, so it cannot
	// hold up that change, and tries again on its next wake or poll. Nil
	// admits everything.
	Admit func() func()
	// Paused starts the runner stopped until the next Wake.
	Paused bool
	// OnStatusChanged runs after a transition of Status.
	OnStatusChanged func(immediate bool)

	// PollInterval is how often an idle runner looks for work that became due
	// without a Wake, such as a deferral that ended.
	PollInterval time.Duration
	// Pace follows each video whose steps read the drive.
	Pace time.Duration
	// RetryDelay defers a video whose steps ended without settling.
	RetryDelay time.Duration
	// BatchSize is how many due videos one catalog read of a lane takes.
	BatchSize int
}

// Status is a runner's state. State is idle, queued (due work waits for a
// slot), generating, or cooling.
type Status struct {
	State string
	// Active lists the steps in progress, one per running video, the
	// fingerprint lane's first.
	Active        []Activity
	Counts        catalog.GenerationCounts
	CooldownUntil time.Time
	Paused        bool
}

// Activity is the step a lane is running for a video.
type Activity struct {
	Kind  Kind
	ID    string
	Title string
}

type stepEntry struct {
	kind Kind
	step Step
}

// lane takes one kind of due work.
type lane struct {
	name  string
	steps []stepEntry
	// takesSlot makes each video hold a slot of the limiter, and lets the lane
	// run up to the runner's parallelism of videos at once. A lane without
	// slots runs one.
	takesSlot bool
	list      func(ctx context.Context, previews bool, now time.Time) ([]string, error)
	// pending reports whether the lane still has work due in steps.
	pending func(catalog.GenerationSteps) bool
	// next is woken after each of this lane's videos.
	next *lane
	wake chan struct{}
	// freed tells the lane's dispatch that a video ended or the parallelism
	// changed.
	freed chan struct{}

	// Guarded by Runner.mu. seq counts wakes: a catalog read that finds
	// nothing due makes the lane idle only when no wake arrived since the
	// read started.
	seq  uint64
	idle bool
	// jobs are the videos in progress, in the order they started.
	jobs []*job
}

// job is a video in progress on a lane.
type job struct {
	id     string
	cancel context.CancelFunc
	// current is the step running; nil between steps. Guarded by Runner.mu.
	current *Activity
}

func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (l *lane) running(id string) bool {
	return slices.ContainsFunc(l.jobs, func(j *job) bool { return j.id == id })
}

type Runner struct {
	cfg   Config
	lanes []*lane

	mu          sync.Mutex
	paused      bool
	stopped     bool
	parallelism int
	counts      catalog.GenerationCounts
	countedAt   time.Time
	changed     chan struct{}
}

func New(cfg Config) *Runner {
	if cfg.Cooldown == nil {
		cfg.Cooldown = &tasklimit.Cooldown{}
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.Pace <= 0 {
		cfg.Pace = defaultPace
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = defaultRetryDelay
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultBatchSize
	}
	r := &Runner{cfg: cfg, paused: cfg.Paused, parallelism: cfg.Parallelism, changed: make(chan struct{})}
	fingerprints := r.newLane("fingerprints", false, []stepEntry{
		{KindFingerprint, cfg.Steps.Fingerprint},
	}, func(ctx context.Context, _ bool, now time.Time) ([]string, error) {
		return cfg.Store.ListDueFingerprints(ctx, cfg.DriveID, now, cfg.BatchSize)
	}, func(steps catalog.GenerationSteps) bool { return steps.Fingerprint })
	assets := r.newLane("assets", true, []stepEntry{
		{KindMedia, cfg.Steps.Media},
		{KindThumbnail, cfg.Steps.Thumbnail},
		{KindDuration, cfg.Steps.Duration},
	}, func(ctx context.Context, previews bool, now time.Time) ([]string, error) {
		return cfg.Store.ListDueAssets(ctx, cfg.DriveID, previews, now, cfg.BatchSize)
	}, catalog.GenerationSteps.AssetsDue)
	if fingerprints != nil {
		// A settled fingerprint can make the video's assets due.
		fingerprints.next = assets
	}
	return r
}

// newLane adds a lane for the given steps; a lane without any is left out.
func (r *Runner) newLane(name string, takesSlot bool, entries []stepEntry,
	list func(context.Context, bool, time.Time) ([]string, error), pending func(catalog.GenerationSteps) bool) *lane {
	var steps []stepEntry
	for _, entry := range entries {
		if entry.step != nil {
			steps = append(steps, entry)
		}
	}
	if len(steps) == 0 {
		return nil
	}
	l := &lane{name: name, steps: steps, takesSlot: takesSlot, list: list, pending: pending,
		wake: make(chan struct{}, 1), freed: make(chan struct{}, 1)}
	r.lanes = append(r.lanes, l)
	return l
}

// Run takes due work until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	defer r.finish()
	var wg sync.WaitGroup
	for _, l := range r.lanes {
		wg.Go(func() { r.runLane(ctx, l) })
	}
	wg.Wait()
}

func (r *Runner) runLane(ctx context.Context, l *lane) {
	poll := time.NewTimer(0)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.wake:
		case <-poll.C:
		}
		if !r.isPaused() {
			r.drain(ctx, l)
		}
		poll.Reset(r.cfg.PollInterval)
	}
}

// Wake asks the runner to look for due work now, and resumes a stopped one.
// Callers wake the drive after writing work to the catalog.
func (r *Runner) Wake() {
	r.mu.Lock()
	r.paused = false
	for _, l := range r.lanes {
		l.seq++
		l.idle = false
	}
	r.broadcastLocked()
	r.mu.Unlock()
	for _, l := range r.lanes {
		poke(l.wake)
	}
	r.notify(true)
}

// SetParallelism changes how many videos the asset lane runs at once, as
// Config.Parallelism describes. Videos already running finish either way.
func (r *Runner) SetParallelism(n int) {
	r.mu.Lock()
	r.parallelism = n
	r.mu.Unlock()
	r.NotifyCapacityChanged()
}

// NotifyCapacityChanged wakes lanes waiting for room after the shared limiter's
// size or the drive's parallelism changed.
func (r *Runner) NotifyCapacityChanged() {
	for _, l := range r.lanes {
		poke(l.freed)
	}
}

// capacityLocked is how many videos the lane may run at once.
func (r *Runner) capacityLocked(l *lane) int {
	if !l.takesSlot {
		return 1
	}
	if r.cfg.Limiter == nil {
		return max(r.parallelism, 1)
	}
	limit := r.cfg.Limiter.Limit()
	if r.parallelism <= 0 {
		return limit
	}
	return min(r.parallelism, limit)
}

// wakeLane asks one lane to look for due work, unless the runner is stopped.
func (r *Runner) wakeLane(l *lane) {
	r.mu.Lock()
	if r.paused || r.stopped {
		r.mu.Unlock()
		return
	}
	l.seq++
	l.idle = false
	r.broadcastLocked()
	r.mu.Unlock()
	poke(l.wake)
}

// Stop interrupts the current videos and keeps the runner from taking more
// until the next Wake. The work stays due in the catalog. It reports whether
// the runner was busy.
func (r *Runner) Stop() bool {
	r.mu.Lock()
	busy := !r.idleLocked()
	r.paused = true
	var cancels []context.CancelFunc
	for _, l := range r.lanes {
		for _, j := range l.jobs {
			cancels = append(cancels, j.cancel)
		}
	}
	r.broadcastLocked()
	r.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	r.notify(true)
	return busy
}

// Busy reports whether the drive has work the runner has not finished:
// a video in progress, or due work it has not yet found settled.
func (r *Runner) Busy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.idleLocked()
}

// WaitIdle blocks until the runner is idle: nothing is due, it was stopped,
// or it ended. Work written to the catalog before a Wake is part of the wait.
func (r *Runner) WaitIdle(ctx context.Context) error {
	for {
		r.mu.Lock()
		idle := r.idleLocked()
		changed := r.changed
		r.mu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (r *Runner) Status() Status {
	r.mu.Lock()
	status := Status{Counts: r.counts, Paused: r.paused}
	idle := r.idleLocked()
	for _, l := range r.lanes {
		for _, j := range l.jobs {
			if j.current != nil {
				status.Active = append(status.Active, *j.current)
			}
		}
	}
	r.mu.Unlock()
	until, cooling := r.cfg.Cooldown.Until(time.Now())
	if cooling {
		status.CooldownUntil = until
	}
	switch {
	case idle:
		status.State = "idle"
	case len(status.Active) > 0:
		status.State = "generating"
	case cooling:
		status.State = "cooling"
	default:
		status.State = "queued"
	}
	return status
}

func (r *Runner) idleLocked() bool {
	if r.stopped {
		return true
	}
	for _, l := range r.lanes {
		if len(l.jobs) > 0 || !(r.paused || l.idle) {
			return false
		}
	}
	return true
}

func (r *Runner) broadcastLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *Runner) notify(immediate bool) {
	if r.cfg.OnStatusChanged != nil {
		r.cfg.OnStatusChanged(immediate)
	}
}

func (r *Runner) isPaused() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.paused
}

func (r *Runner) isLaneIdle(l *lane) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return l.idle
}

func (r *Runner) hasCounts() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts.Any()
}

func (r *Runner) previewsEnabled() bool {
	return r.cfg.PreviewsEnabled == nil || r.cfg.PreviewsEnabled()
}

func (r *Runner) finish() {
	r.mu.Lock()
	r.stopped = true
	r.broadcastLocked()
	r.mu.Unlock()
	r.notify(true)
}

// drain takes the lane's due videos, up to the lane's capacity at once,
// until none is left, the runner is stopped, or a video could not start. It
// returns once the videos it started have ended.
func (r *Runner) drain(ctx context.Context, l *lane) {
	var videos sync.WaitGroup
	// halt is the first outcome that ends the drain; a refusal overrides any.
	var halt atomic.Uint32
	defer func() {
		videos.Wait()
		if videoOutcome(halt.Load()) == videoRefused {
			r.mu.Lock()
			l.idle = true
			r.broadcastLocked()
			r.mu.Unlock()
			r.notify(true)
		}
	}()
	halted := func() bool { return halt.Load() != uint32(videoDone) }
	for ctx.Err() == nil && !halted() {
		r.mu.Lock()
		seq, paused := l.seq, r.paused
		r.mu.Unlock()
		if paused {
			return
		}
		previews := r.previewsEnabled()
		ids, err := l.list(ctx, previews, time.Now())
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("[generation] drive=%s lane=%s list due work: %v", r.cfg.DriveID, l.name, err)
			}
			return
		}
		if len(ids) > 0 || !r.isLaneIdle(l) || r.hasCounts() {
			r.refreshCounts(ctx, previews, true)
		}
		r.setIdle(l, len(ids) == 0, seq)
		if len(ids) == 0 {
			return
		}
		started := 0
		for _, id := range ids {
			j, jobCtx, stop := r.claim(ctx, l, id, halted)
			if stop {
				return
			}
			if j == nil {
				continue
			}
			started++
			videos.Go(func() {
				outcome := r.runVideo(ctx, jobCtx, l, j)
				if outcome == videoRefused {
					halt.Store(uint32(outcome))
				} else if outcome != videoDone {
					halt.CompareAndSwap(uint32(videoDone), uint32(outcome))
				}
				r.endVideo(l, j)
			})
		}
		if started == 0 {
			// Every due video is running: read again once one ends.
			select {
			case <-ctx.Done():
			case <-l.freed:
			}
		}
	}
}

// claim starts the video on the lane once the lane has room for it. It
// returns no job for a video the lane is already running, and stop when the
// lane must take no more videos.
func (r *Runner) claim(ctx context.Context, l *lane, id string, halted func() bool) (j *job, jobCtx context.Context, stop bool) {
	for {
		r.mu.Lock()
		switch {
		case r.paused || ctx.Err() != nil || halted():
			r.mu.Unlock()
			return nil, nil, true
		case l.running(id):
			r.mu.Unlock()
			return nil, nil, false
		case len(l.jobs) < r.capacityLocked(l):
			jobCtx, cancel := context.WithCancel(ctx)
			j := &job{id: id, cancel: cancel}
			l.jobs = append(l.jobs, j)
			r.broadcastLocked()
			r.mu.Unlock()
			return j, jobCtx, false
		}
		r.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-l.freed:
		}
	}
}

// setIdle records the outcome of a catalog read that started at wake count
// seq. A wake after it started keeps the lane busy until the next read.
func (r *Runner) setIdle(l *lane, idle bool, seq uint64) {
	r.mu.Lock()
	changed := false
	switch {
	case idle && l.seq == seq && !l.idle:
		l.idle, changed = true, true
	case !idle && l.idle:
		l.idle, changed = false, true
	}
	if changed {
		r.broadcastLocked()
	}
	r.mu.Unlock()
	if changed {
		r.notify(true)
	}
}

type videoOutcome uint32

const (
	videoDone videoOutcome = iota
	// videoStopped: the runner was stopped or ended before or during the video.
	videoStopped
	// videoRefused: the video found no task admission.
	videoRefused
	// videoUnreadable: the catalog could not be read. The lane stays busy
	// with the work still due and reads again on its next wake or poll,
	// instead of taking the same video again at once.
	videoUnreadable
)

// runVideo runs the lane's due steps of one claimed video under jobCtx,
// inside a slot when the lane takes one.
func (r *Runner) runVideo(ctx, jobCtx context.Context, l *lane, j *job) videoOutcome {
	// Wait out the drive's cooldown before taking admission or a slot.
	if err := r.cfg.Cooldown.Wait(jobCtx); err != nil {
		return videoStopped
	}
	if r.cfg.Admit != nil {
		release := r.cfg.Admit()
		if release == nil {
			return videoRefused
		}
		defer release()
	}
	releaseSlot := func() {}
	if l.takesSlot {
		release, err := r.cfg.Limiter.AcquireAfter(jobCtx, r.cfg.DriveID, r.cfg.Cooldown)
		if err != nil {
			return videoStopped
		}
		releaseSlot = release
	}
	ran, outcome, err := r.process(jobCtx, l, j)
	releaseSlot()
	r.setCurrent(j, nil)
	if jobCtx.Err() != nil {
		return videoStopped
	}
	if err != nil {
		log.Printf("[generation] drive=%s lane=%s video=%s read work: %v", r.cfg.DriveID, l.name, j.id, err)
		return videoUnreadable
	}
	if outcome != StepPaused {
		r.settle(ctx, l, j.id, r.previewsEnabled())
	}
	// Wake the next lane while this video still counts as running, so the
	// runner never looks idle between the two.
	if l.next != nil {
		r.wakeLane(l.next)
	}
	r.refreshCounts(ctx, r.previewsEnabled(), false)
	if !ran {
		return videoDone
	}
	// Pace only videos that read the drive.
	select {
	case <-jobCtx.Done():
		return videoStopped
	case <-time.After(r.cfg.Pace):
	}
	return videoDone
}

func (r *Runner) endVideo(l *lane, j *job) {
	j.cancel()
	r.mu.Lock()
	l.jobs = slices.DeleteFunc(l.jobs, func(other *job) bool { return other == j })
	r.broadcastLocked()
	r.mu.Unlock()
	poke(l.freed)
	r.notify(true)
}

// process runs the lane's due steps of the video in order, reading its row
// before each one: a step changes what the next needs, and the other lane may
// have changed the row too. It reports whether any step ran, the stopping
// outcome, and an error when the row could not be read. A video that is gone
// has nothing to run.
func (r *Runner) process(ctx context.Context, l *lane, j *job) (ran bool, outcome StepOutcome, err error) {
	for _, entry := range l.steps {
		video, steps, err := r.cfg.Store.GenerationWork(ctx, j.id, r.previewsEnabled())
		if errors.Is(err, sql.ErrNoRows) {
			return ran, StepSettled, nil
		}
		if err != nil {
			return ran, StepSettled, err
		}
		if video.DriveID != r.cfg.DriveID || !l.pending(steps) {
			return ran, StepSettled, nil
		}
		if !due(steps, entry.kind) {
			continue
		}
		r.setCurrent(j, &Activity{Kind: entry.kind, ID: video.ID, Title: video.Title})
		ran = true
		if stepOutcome := entry.step.Run(ctx, video); stepOutcome != StepSettled {
			return ran, stepOutcome, nil
		}
	}
	return ran, StepSettled, nil
}

// settle defers a video whose lane steps all ran yet left the lane's work
// due, so a failure that records nothing cannot make the drive take it again
// and again. A cooling drive keeps its videos due for the end of the
// cooldown. Only one lane has work due for a video at a time, so the
// deferral holds back no other lane.
func (r *Runner) settle(ctx context.Context, l *lane, id string, previews bool) {
	if _, cooling := r.cfg.Cooldown.Until(time.Now()); cooling {
		return
	}
	_, steps, err := r.cfg.Store.GenerationWork(ctx, id, previews)
	if err != nil || !l.pending(steps) {
		return
	}
	until := time.Now().Add(r.cfg.RetryDelay)
	if err := r.cfg.Store.DeferGeneration(ctx, id, until); err != nil {
		log.Printf("[generation] drive=%s video=%s defer unsettled work: %v", r.cfg.DriveID, id, err)
		return
	}
	log.Printf("[generation] drive=%s lane=%s video=%s left %+v unsettled; deferred until %s",
		r.cfg.DriveID, l.name, id, steps, until.Format(time.RFC3339))
}

func (r *Runner) setCurrent(j *job, current *Activity) {
	r.mu.Lock()
	j.current = current
	r.mu.Unlock()
	r.notify(true)
}

// refreshCounts reads the drive's due counts for Status. Between catalog
// reads they refresh at most once per countInterval: a count covers the
// whole drive, and fingerprints can finish many times a second.
func (r *Runner) refreshCounts(ctx context.Context, previews, force bool) {
	r.mu.Lock()
	recent := time.Since(r.countedAt) < countInterval
	r.mu.Unlock()
	if recent && !force {
		return
	}
	counts, err := r.cfg.Store.CountDueGeneration(ctx, r.cfg.DriveID, previews, time.Now())
	if err != nil {
		return
	}
	r.mu.Lock()
	changed := counts != r.counts
	r.counts, r.countedAt = counts, time.Now()
	r.mu.Unlock()
	if changed {
		r.notify(false)
	}
}
