// Package nightly orchestrates the single nightly maintenance pipeline that
// replaces the legacy scanLoop / crawlerLoop / crawler upload periodic loop.
//
// The full pipeline is fired once per day at the configured HH:mm. The admin
// "扫描所有网盘" action uses TriggerScanAll and intentionally runs only the
// cloud-scan and duplicate-maintenance stages.
//
//	Phase 1: for each non-crawler cloud drive
//	           scan + delete-detection + enqueue thumb + enqueue preview video
//	         wait until all thumb / preview-video queues are idle
//	Phase 2: if any script crawler configured
//	           crawl + enqueue preview video for new videos
//	         wait until preview-video queues are idle
//	Phase 3: crawler local video → cloud upload (single sweep, captcha cooldown still
//	         honored within this call)
//	Phase 4: scan crawler local directories and restore user-requested videos
//	Phase 5: full-library duplicate video maintenance:
//	         exact size+sampled_sha256, title/duration/thumbnail, then content-frame dedupe
//
// The pipeline runs until all phases finish, the process exits, or an admin
// stop request cancels the run. Provider cooldowns may make a single phase take
// a long time, so there is no fixed duration cutoff.
//
// State persistence: the date string of the most recent successfully started
// run is stored in catalog.settings under the key "nightly.last_run_date".
// This survives restarts so a quick crash inside the configured minute won't trigger a
// duplicate pipeline.
package nightly

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/video-site/backend/internal/schedule"
)

const (
	// settingLastRunDate stores the YYYY-MM-DD of the last natural cron-triggered
	// full pipeline run. Manual scan-all runs never update it.
	settingLastRunDate = "nightly.last_run_date"
	// dateLayout matches catalog.GetSetting string semantics; using ISO-8601 date.
	dateLayout = "2006-01-02"
	// pollInterval is the heartbeat for the natural cron decision loop.
	pollInterval = time.Minute
)

type runMode uint8

const (
	runModeScheduled runMode = iota
	runModeScanAll
)

// SettingStore is the minimal catalog.Catalog surface we rely on.
type SettingStore interface {
	GetSetting(ctx context.Context, key, defaultValue string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
}

// Config wires the runner to its dependencies. The function-callback shape
// avoids importing main / drives / preview from this package, keeping the
// dependency graph clean.
type Config struct {
	Settings   SettingStore
	CronHour   int // legacy/default hour; zero falls back to 1 unless StartTime is set
	CronMinute int // default 0
	// StartTime is the preferred daily schedule in 24-hour HH:mm form. It can
	// represent midnight and takes precedence over CronHour/CronMinute.
	StartTime string
	// Timezone is the explicit IANA timezone used to interpret StartTime and
	// the persisted last-run calendar date. It never changes the host timezone.
	Timezone string

	// ListScanTargets returns the drive IDs to run Phase 1 on, in deterministic
	// order. Should exclude crawler and localupload drives.
	ListScanTargets func(ctx context.Context) []string

	// RunScan synchronously runs scan + cleanup + enqueueDriveGeneration for
	// one drive. Errors are expected to be logged inside, not surfaced.
	RunScan func(ctx context.Context, driveID string)

	// ListCrawlerDrives returns script crawler drive IDs to crawl in Phase 2.
	// Returns empty slice when no crawler is configured.
	ListCrawlerDrives func(ctx context.Context) []string

	// RunCrawlerCrawl synchronously runs one crawl cycle (downloads + thumbs +
	// preview-video enqueue) for a single crawler drive.
	RunCrawlerCrawl func(ctx context.Context, driveID string)

	// WaitPreviewQueuesIdle blocks until both the thumbnail and preview-video queues
	// across all drives are drained (queue empty + no in-flight task). It must
	// honor ctx cancellation.
	WaitPreviewQueuesIdle func(ctx context.Context) error

	// RunMigration runs crawlerupload.Migrator.RunOnce for Phase 3.
	RunMigration func(ctx context.Context) error

	// RestoreCrawlerVideos scans one crawler's retained local source directory
	// after new-video generation and upload have completed.
	RestoreCrawlerVideos func(ctx context.Context, driveID string) error

	// RunDedupeAssetCleanup runs full-library duplicate video maintenance. It
	// removes duplicate catalog rows and local generated assets, but never
	// deletes cloud source files.
	RunDedupeAssetCleanup func(ctx context.Context) error

	// RunTagMaintenance is optional. The main server leaves this nil because tag
	// matching is event-driven: new videos and administrator tag edits refresh
	// labels immediately.
	RunTagMaintenance func(ctx context.Context) error

	// Now is injected for tests; nil → time.Now.
	Now func() time.Time
}

type Status struct {
	State          string
	Running        bool
	Queued         bool
	StartedAt      time.Time
	LastFinishedAt time.Time
}

// Runner drives the nightly pipeline.
type Runner struct {
	cfg             Config
	trigger         chan runMode  // buffered(1); mutually exclusive manual requests
	scheduleChanged chan struct{} // buffered(1); wake the natural scheduler
	runMu           sync.Mutex    // prevents overlapping pipeline runs
	scheduleMu      sync.RWMutex  // protects schedule fields and location
	location        *time.Location

	stateMu        sync.Mutex
	running        bool
	queued         bool
	startedAt      time.Time
	lastFinishedAt time.Time
	currentCancel  context.CancelFunc
}

// New constructs a Runner. cfg is shallow-copied; defaults are applied.
func New(cfg Config) *Runner {
	if strings.TrimSpace(cfg.StartTime) != "" {
		hour, minute, normalized, err := parseStartTime(cfg.StartTime)
		if err == nil {
			cfg.CronHour = hour
			cfg.CronMinute = minute
			cfg.StartTime = normalized
		} else {
			cfg.CronHour = 1
			cfg.CronMinute = 0
			cfg.StartTime = "01:00"
		}
	} else {
		if cfg.CronHour <= 0 || cfg.CronHour > 23 {
			cfg.CronHour = 1
		}
		if cfg.CronMinute < 0 || cfg.CronMinute > 59 {
			cfg.CronMinute = 0
		}
		cfg.StartTime = fmt.Sprintf("%02d:%02d", cfg.CronHour, cfg.CronMinute)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	timezone := strings.TrimSpace(cfg.Timezone)
	if timezone == "" {
		timezone = schedule.DefaultTimezone
	}
	timezone, location, err := schedule.LoadTimezone(timezone)
	if err != nil {
		timezone, location, _ = schedule.LoadTimezone(schedule.DefaultTimezone)
	}
	cfg.Timezone = timezone
	return &Runner{
		cfg:             cfg,
		location:        location,
		trigger:         make(chan runMode, 1),
		scheduleChanged: make(chan struct{}, 1),
	}
}

// Run is a blocking loop until ctx is done. It wakes up once per minute and
// either fires the natural cron-driven pipeline (when the configured minute matches and
// today hasn't run) or honors a queued full/scan-all manual request.
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	startTime, timezone := r.Schedule()
	log.Printf("[nightly] runner started; start_time=%s timezone=%s", startTime, timezone)
	for {
		select {
		case <-ctx.Done():
			log.Printf("[nightly] runner stopping: %v", ctx.Err())
			return
		case <-t.C:
			r.tryNaturalRun(ctx)
		case <-r.scheduleChanged:
			startTime, timezone := r.Schedule()
			log.Printf("[nightly] schedule updated; start_time=%s timezone=%s", startTime, timezone)
			// Re-evaluate immediately so saving the current minute does not have
			// to wait for the next heartbeat and accidentally miss today's run.
			r.tryNaturalRun(ctx)
		case mode := <-r.trigger:
			log.Printf("[scan-all] manual trigger received")
			r.runModeLocked(ctx, mode)
		}
	}
}

// UpdateStartTime changes the natural daily schedule without restarting the
// service. It does not interrupt a pipeline that is already running.
func (r *Runner) UpdateStartTime(value string) error {
	hour, minute, normalized, err := parseStartTime(value)
	if err != nil {
		return err
	}
	r.scheduleMu.Lock()
	r.cfg.CronHour = hour
	r.cfg.CronMinute = minute
	r.cfg.StartTime = normalized
	r.scheduleMu.Unlock()

	r.signalScheduleChanged()
	return nil
}

// UpdateSchedule atomically changes both components of the natural daily
// schedule. Validation completes before the lock is acquired, so a rejected
// update cannot leave a mixed start-time/timezone pair behind.
func (r *Runner) UpdateSchedule(startTime, timezone string) error {
	hour, minute, normalizedStartTime, err := parseStartTime(startTime)
	if err != nil {
		return err
	}
	normalizedTimezone, location, err := schedule.LoadTimezone(timezone)
	if err != nil {
		return fmt.Errorf("invalid nightly timezone %q: %w", timezone, err)
	}

	r.scheduleMu.Lock()
	r.cfg.CronHour = hour
	r.cfg.CronMinute = minute
	r.cfg.StartTime = normalizedStartTime
	r.cfg.Timezone = normalizedTimezone
	r.location = location
	r.scheduleMu.Unlock()

	r.signalScheduleChanged()
	return nil
}

func (r *Runner) signalScheduleChanged() {
	select {
	case r.scheduleChanged <- struct{}{}:
	default:
	}
}

// StartTime returns the current canonical HH:mm schedule.
func (r *Runner) StartTime() string {
	r.scheduleMu.RLock()
	defer r.scheduleMu.RUnlock()
	return r.cfg.StartTime
}

// Timezone returns the current explicit IANA schedule timezone.
func (r *Runner) Timezone() string {
	r.scheduleMu.RLock()
	defer r.scheduleMu.RUnlock()
	return r.cfg.Timezone
}

// Schedule returns a consistent start-time/timezone pair.
func (r *Runner) Schedule() (string, string) {
	r.scheduleMu.RLock()
	defer r.scheduleMu.RUnlock()
	return r.cfg.StartTime, r.cfg.Timezone
}

// TriggerScanAll asks the runner to scan every configured non-crawler cloud
// drive, wait for newly discovered video assets, and then run full-library
// duplicate maintenance. It deliberately excludes crawler, migration, and
// retained-video restore phases, and does not consume today's scheduled run.
func (r *Runner) TriggerScanAll() bool {
	return r.queueManualRun()
}

func (r *Runner) queueManualRun() bool {
	r.stateMu.Lock()
	if r.running || r.queued {
		r.stateMu.Unlock()
		return false
	}
	r.queued = true
	r.stateMu.Unlock()

	select {
	case r.trigger <- runModeScanAll:
		return true
	default:
		r.stateMu.Lock()
		r.queued = false
		r.stateMu.Unlock()
		return false
	}
}

// StopCurrent cancels the currently running pipeline and drops one queued
// manual trigger, if present. It returns true when there was something to stop.
func (r *Runner) StopCurrent() bool {
	r.stateMu.Lock()
	wasRunning := r.running
	wasQueued := r.queued
	cancel := r.currentCancel
	r.queued = false
	r.stateMu.Unlock()

	if wasQueued {
		select {
		case <-r.trigger:
		default:
		}
	}
	if cancel != nil {
		cancel()
	}
	return wasRunning || wasQueued || cancel != nil
}

func (r *Runner) Status() Status {
	r.stateMu.Lock()
	running := r.running
	queued := r.queued
	startedAt := r.startedAt
	lastFinishedAt := r.lastFinishedAt
	r.stateMu.Unlock()

	state := "idle"
	switch {
	case running && queued:
		state = "running_queued"
	case running:
		state = "running"
	case queued:
		state = "queued"
	}

	return Status{
		State:          state,
		Running:        running,
		Queued:         queued,
		StartedAt:      startedAt,
		LastFinishedAt: lastFinishedAt,
	}
}

// tryNaturalRun checks the cron decision and runs the pipeline if due today.
func (r *Runner) tryNaturalRun(ctx context.Context) {
	instant := r.cfg.Now()
	r.scheduleMu.RLock()
	location := r.location
	hour, minute := r.cfg.CronHour, r.cfg.CronMinute
	r.scheduleMu.RUnlock()
	now := instant.In(location)
	if now.Hour() != hour || now.Minute() != minute {
		return
	}
	last, err := r.readLastRunDate(ctx)
	if err != nil {
		log.Printf("[nightly] read last_run_date: %v", err)
		return
	}
	if !shouldRun(now, last) {
		return
	}
	log.Printf("[nightly] natural cron trigger at %s", now.Format(time.RFC3339))
	r.runModeLockedForDate(ctx, runModeScheduled, now.Format(dateLayout))
}

func parseStartTime(value string) (hour, minute int, normalized string, err error) {
	trimmed := strings.TrimSpace(value)
	parsed, parseErr := time.Parse("15:04", trimmed)
	if parseErr != nil || len(trimmed) != len("15:04") {
		return 0, 0, "", fmt.Errorf("invalid nightly start time %q: expected HH:mm", value)
	}
	return parsed.Hour(), parsed.Minute(), parsed.Format("15:04"), nil
}

// shouldRun returns true when "today" (per now) hasn't already been processed.
func shouldRun(now time.Time, lastRunDate string) bool {
	return lastRunDate != now.Format(dateLayout)
}

// runModeLocked guards both execution modes against overlap. Runs have no
// fixed duration limit and stop only when canceled or their phases complete.
func (r *Runner) runModeLocked(ctx context.Context, mode runMode) {
	r.runModeLockedForDate(ctx, mode, "")
}

func (r *Runner) runModeLockedForDate(ctx context.Context, mode runMode, scheduledDate string) {
	component := "nightly"
	execution := "scheduled"
	if mode == runModeScanAll {
		component = "scan-all"
		execution = "manual"
	}

	if mode == runModeScanAll {
		r.stateMu.Lock()
		queued := r.queued
		r.stateMu.Unlock()
		if !queued {
			log.Printf("[%s] manual trigger was canceled before start", component)
			return
		}
	}
	if !r.runMu.TryLock() {
		log.Printf("[%s] another pipeline is already running, skipping this trigger", component)
		return
	}

	started := r.cfg.Now()
	if mode == runModeScheduled && scheduledDate == "" {
		r.scheduleMu.RLock()
		scheduledDate = started.In(r.location).Format(dateLayout)
		r.scheduleMu.RUnlock()
	}
	runCtx, cancel := context.WithCancel(ctx)
	r.markStarted(started, cancel)
	defer func() {
		cancel()
		r.markFinished(r.cfg.Now())
		r.runMu.Unlock()
	}()

	log.Printf("[%s] pipeline (%s) start", component, execution)

	if mode == runModeScanAll {
		r.runScanAllPipeline(runCtx)
	} else {
		r.runPipeline(runCtx)
	}

	finished := r.cfg.Now()
	log.Printf("[%s] pipeline (%s) finish; took=%s", component, execution, finished.Sub(started).Round(time.Second))

	if mode == runModeScanAll {
		return
	}

	// Mark today as processed regardless of success/error. This is intentional:
	// a partial / failing full pipeline should not automatically trigger again
	// during the same scheduled minute. Scan-all returns above and never writes it.
	if err := r.cfg.Settings.SetSetting(ctx, settingLastRunDate, scheduledDate); err != nil {
		log.Printf("[nightly] persist last_run_date: %v", err)
	}
}

func (r *Runner) markStarted(started time.Time, cancel context.CancelFunc) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	r.running = true
	r.queued = false
	r.startedAt = started
	r.currentCancel = cancel
}

func (r *Runner) markFinished(finished time.Time) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	r.running = false
	r.startedAt = time.Time{}
	r.lastFinishedAt = finished
	r.currentCancel = nil
}

// runPipeline executes the maintenance phases. It returns when the pipeline
// finishes or ctx is done. Errors are logged but not propagated —
// each phase is best-effort; downstream phases still attempt to run unless ctx
// is dead.
func (r *Runner) runPipeline(ctx context.Context) {
	if !r.runScanPhase(ctx, "nightly", "phase 1") {
		return
	}

	// ---------- Phase 2 ----------
	if r.shouldStop(ctx, "nightly", "phase 2") {
		return
	}
	crawlerIDs := []string{}
	if r.cfg.ListCrawlerDrives != nil {
		crawlerIDs = r.cfg.ListCrawlerDrives(ctx)
	}
	if len(crawlerIDs) == 0 {
		log.Printf("[nightly] phase 2/3 skipped: no crawler configured")
		r.runDedupeAssetCleanupPhase(ctx, "nightly", "phase 5")
		r.runTagMaintenancePhase(ctx, "nightly", "phase 6")
		return
	}
	log.Printf("[nightly] phase 2: crawling %d crawler drive(s)", len(crawlerIDs))
	for _, id := range crawlerIDs {
		if ctx.Err() != nil {
			log.Printf("[nightly] phase 2 aborted by ctx: %v", ctx.Err())
			return
		}
		log.Printf("[nightly] phase 2: crawling drive=%s", id)
		r.cfg.RunCrawlerCrawl(ctx, id)
	}
	log.Printf("[nightly] phase 2: waiting for teaser queue to drain")
	if err := r.waitIdle(ctx, "nightly", "phase 2"); err != nil {
		return
	}

	// ---------- Phase 3 ----------
	if r.shouldStop(ctx, "nightly", "phase 3") {
		return
	}
	log.Printf("[nightly] phase 3: crawler upload")
	if r.cfg.RunMigration != nil {
		if err := r.cfg.RunMigration(ctx); err != nil {
			log.Printf("[nightly] phase 3 migration: %v", err)
		}
	}

	// ---------- Phase 4 ----------
	if r.shouldStop(ctx, "nightly", "phase 4") {
		return
	}
	if r.cfg.RestoreCrawlerVideos != nil {
		log.Printf("[nightly] phase 4: restoring retained crawler videos")
		for _, id := range crawlerIDs {
			if err := r.cfg.RestoreCrawlerVideos(ctx, id); err != nil {
				log.Printf("[nightly] phase 4 restore drive=%s: %v", id, err)
			}
		}
	}

	r.runDedupeAssetCleanupPhase(ctx, "nightly", "phase 5")
	r.runTagMaintenancePhase(ctx, "nightly", "phase 6")
}

// runScanAllPipeline is the manual admin workflow: configured cloud drives are
// scanned and newly discovered videos finish their generation work before
// duplicate maintenance runs. Crawler-specific lifecycle phases belong only to
// the scheduled/full pipeline above.
func (r *Runner) runScanAllPipeline(ctx context.Context) {
	if !r.runScanPhase(ctx, "scan-all", "scan") {
		return
	}
	r.runDedupeAssetCleanupPhase(ctx, "scan-all", "dedupe")
}

func (r *Runner) runScanPhase(ctx context.Context, component, phase string) bool {
	if r.shouldStop(ctx, component, phase) {
		return false
	}
	scanIDs := []string{}
	if r.cfg.ListScanTargets != nil {
		scanIDs = r.cfg.ListScanTargets(ctx)
	}
	if len(scanIDs) == 0 {
		log.Printf("[%s] %s skipped: no cloud drives to scan", component, phase)
	} else {
		log.Printf("[%s] %s: scanning %d drive(s)", component, phase, len(scanIDs))
		for _, id := range scanIDs {
			if ctx.Err() != nil {
				log.Printf("[%s] %s aborted by ctx: %v", component, phase, ctx.Err())
				return false
			}
			log.Printf("[%s] %s: scanning drive=%s", component, phase, id)
			r.cfg.RunScan(ctx, id)
		}
		log.Printf("[%s] %s: waiting for preview queues to drain", component, phase)
		if err := r.waitIdle(ctx, component, phase); err != nil {
			return false
		}
	}
	return true
}

func (r *Runner) shouldStop(ctx context.Context, component, phase string) bool {
	if err := ctx.Err(); err != nil {
		log.Printf("[%s] %s: ctx done (%v), bailing out", component, phase, err)
		return true
	}
	return false
}

// waitIdle calls the configured WaitPreviewQueuesIdle, logging the outcome.
func (r *Runner) waitIdle(ctx context.Context, component, phase string) error {
	if r.cfg.WaitPreviewQueuesIdle == nil {
		return nil
	}
	if err := r.cfg.WaitPreviewQueuesIdle(ctx); err != nil {
		log.Printf("[%s] %s: wait preview queues: %v", component, phase, err)
		return err
	}
	return nil
}

func (r *Runner) runTagMaintenancePhase(ctx context.Context, component, phase string) {
	if r.cfg.RunTagMaintenance == nil {
		return
	}
	if r.shouldStop(ctx, component, phase) {
		return
	}
	log.Printf("[%s] %s: tag maintenance", component, phase)
	if err := r.cfg.RunTagMaintenance(ctx); err != nil {
		log.Printf("[%s] %s tag maintenance: %v", component, phase, err)
	}
}

func (r *Runner) runDedupeAssetCleanupPhase(ctx context.Context, component, phase string) {
	if r.shouldStop(ctx, component, phase) {
		return
	}
	if r.cfg.RunDedupeAssetCleanup == nil {
		return
	}
	log.Printf("[%s] %s: duplicate video maintenance", component, phase)
	if err := r.cfg.RunDedupeAssetCleanup(ctx); err != nil {
		log.Printf("[%s] %s duplicate video maintenance: %v", component, phase, err)
	}
}

// readLastRunDate reads the persisted last_run_date or returns "" when unset.
func (r *Runner) readLastRunDate(ctx context.Context) (string, error) {
	if r.cfg.Settings == nil {
		return "", nil
	}
	return r.cfg.Settings.GetSetting(ctx, settingLastRunDate, "")
}
