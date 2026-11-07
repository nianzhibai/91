package nightly

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/scanjob"
	"github.com/nianzhibai/91/backend/internal/schedule"
)

// stubSettings is an in-memory SettingStore for tests.
type stubSettings struct {
	mu sync.Mutex
	kv map[string]string
}

func newStubSettings() *stubSettings { return &stubSettings{kv: make(map[string]string)} }

func (s *stubSettings) GetSetting(_ context.Context, key, def string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.kv[key]; ok {
		return v, nil
	}
	return def, nil
}

func (s *stubSettings) SetSetting(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kv[key] = value
	return nil
}

func TestShouldRunChecksDate(t *testing.T) {
	now := time.Date(2026, 5, 27, 1, 30, 0, 0, time.UTC)
	if !shouldRun(now, "") {
		t.Fatal("first ever run with empty last_run_date should be due")
	}
	if !shouldRun(now, "2026-05-26") {
		t.Fatal("yesterday's run should not block today")
	}
	if shouldRun(now, "2026-05-27") {
		t.Fatal("today's run already recorded should block another natural run")
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	if schedule.DefaultTimezone != "Asia/Shanghai" {
		t.Fatalf("DefaultTimezone = %q, want Asia/Shanghai", schedule.DefaultTimezone)
	}
	r := New(Config{Settings: newStubSettings()})
	if r.cfg.CronHour != 1 {
		t.Errorf("CronHour zero-value should fall back to 1, got %d", r.cfg.CronHour)
	}
	if got := r.Timezone(); got != schedule.DefaultTimezone {
		t.Errorf("Timezone zero-value = %q, want %q", got, schedule.DefaultTimezone)
	}
}

func TestNewRejectsInvalidCronHour(t *testing.T) {
	r := New(Config{CronHour: 0, Settings: newStubSettings()})
	if r.cfg.CronHour != 1 {
		t.Fatalf("invalid cron_hour fall back to 1, got %d", r.cfg.CronHour)
	}
	r2 := New(Config{CronHour: -1, Settings: newStubSettings()})
	if r2.cfg.CronHour != 1 {
		t.Fatalf("out-of-range cron_hour fall back to 1, got %d", r2.cfg.CronHour)
	}
	r3 := New(Config{CronHour: 25, Settings: newStubSettings()})
	if r3.cfg.CronHour != 1 {
		t.Fatalf("out-of-range cron_hour fall back to 1, got %d", r3.cfg.CronHour)
	}
}

func TestNewPrefersExplicitStartTimeAndSupportsMidnight(t *testing.T) {
	r := New(Config{CronHour: 8, StartTime: "00:15", Settings: newStubSettings()})
	if got := r.StartTime(); got != "00:15" {
		t.Fatalf("StartTime = %q, want 00:15", got)
	}
	if r.cfg.CronHour != 0 || r.cfg.CronMinute != 15 {
		t.Fatalf("schedule = %02d:%02d, want 00:15", r.cfg.CronHour, r.cfg.CronMinute)
	}
}

func TestNaturalRunMatchesHourAndMinute(t *testing.T) {
	settings := newStubSettings()
	now := time.Date(2026, 5, 27, 0, 14, 0, 0, time.UTC)
	var runs atomic.Int32
	r := New(Config{
		Settings:  settings,
		StartTime: "00:15",
		Timezone:  "Etc/UTC",
		Now:       func() time.Time { return now },
		ListScanTargets: func(context.Context) ([]string, error) {
			runs.Add(1)
			return nil, nil
		},
	})

	r.tryNaturalRun(context.Background())
	if got := runs.Load(); got != 0 {
		t.Fatalf("runs before configured minute = %d, want 0", got)
	}
	now = now.Add(time.Minute)
	r.tryNaturalRun(context.Background())
	if got := runs.Load(); got != 1 {
		t.Fatalf("runs at configured minute = %d, want 1", got)
	}
}

func TestUpdateStartTimeChangesNaturalSchedule(t *testing.T) {
	settings := newStubSettings()
	now := time.Date(2026, 5, 27, 23, 45, 0, 0, time.UTC)
	var runs atomic.Int32
	r := New(Config{
		Settings:  settings,
		StartTime: "01:00",
		Timezone:  "Etc/UTC",
		Now:       func() time.Time { return now },
		ListScanTargets: func(context.Context) ([]string, error) {
			runs.Add(1)
			return nil, nil
		},
	})

	if err := r.UpdateStartTime("23:45"); err != nil {
		t.Fatalf("update start time: %v", err)
	}
	if got := r.StartTime(); got != "23:45" {
		t.Fatalf("StartTime = %q, want 23:45", got)
	}
	r.tryNaturalRun(context.Background())
	if got := runs.Load(); got != 1 {
		t.Fatalf("runs after schedule update = %d, want 1", got)
	}

	if err := r.UpdateStartTime("24:00"); err == nil {
		t.Fatal("invalid schedule update unexpectedly succeeded")
	}
	if got := r.StartTime(); got != "23:45" {
		t.Fatalf("invalid update changed StartTime to %q", got)
	}
}

func TestNaturalRunUsesConfiguredTimezoneAndPersistsItsCalendarDate(t *testing.T) {
	settings := newStubSettings()
	now := time.Date(2026, 5, 26, 18, 15, 0, 0, time.UTC)
	var runs atomic.Int32
	r := New(Config{
		Settings:  settings,
		StartTime: "02:15",
		Timezone:  "Asia/Shanghai",
		Now:       func() time.Time { return now },
		ListScanTargets: func(context.Context) ([]string, error) {
			runs.Add(1)
			return nil, nil
		},
	})

	r.tryNaturalRun(context.Background())
	if got := runs.Load(); got != 1 {
		t.Fatalf("runs at 02:15 Asia/Shanghai = %d, want 1", got)
	}
	if got, _ := settings.GetSetting(context.Background(), settingLastRunDate, ""); got != "2026-05-27" {
		t.Fatalf("last_run_date = %q, want schedule-local date 2026-05-27", got)
	}
}

func TestUpdateScheduleIsAtomicAndRejectsInvalidTimezone(t *testing.T) {
	r := New(Config{
		Settings:  newStubSettings(),
		StartTime: "01:00",
		Timezone:  "Etc/UTC",
	})
	if err := r.UpdateSchedule("02:30", "Asia/Shanghai", true); err != nil {
		t.Fatalf("update schedule: %v", err)
	}
	if startTime, timezone := r.Schedule(); startTime != "02:30" || timezone != "Asia/Shanghai" {
		t.Fatalf("schedule = %s %s, want 02:30 Asia/Shanghai", startTime, timezone)
	}
	if !r.Disabled() {
		t.Fatal("schedule disabled state was not updated")
	}

	if err := r.UpdateSchedule("03:45", "Mars/Olympus", false); err == nil {
		t.Fatal("invalid timezone update unexpectedly succeeded")
	}
	if startTime, timezone := r.Schedule(); startTime != "02:30" || timezone != "Asia/Shanghai" {
		t.Fatalf("rejected update partially changed schedule to %s %s", startTime, timezone)
	}
	if !r.Disabled() {
		t.Fatal("rejected update partially changed disabled state")
	}
}

func TestDisabledScheduleSkipsNaturalRunAndResumesAfterUpdate(t *testing.T) {
	settings := newStubSettings()
	now := time.Date(2026, 5, 27, 2, 30, 0, 0, time.UTC)
	var runs atomic.Int32
	r := New(Config{
		Settings:  settings,
		Disabled:  true,
		StartTime: "02:30",
		Timezone:  "Etc/UTC",
		Now:       func() time.Time { return now },
		ListScanTargets: func(context.Context) ([]string, error) {
			runs.Add(1)
			return nil, nil
		},
	})

	r.tryNaturalRun(context.Background())
	if got := runs.Load(); got != 0 {
		t.Fatalf("runs while disabled = %d, want 0", got)
	}
	if last, _ := settings.GetSetting(context.Background(), settingLastRunDate, ""); last != "" {
		t.Fatalf("disabled schedule consumed last-run date %q", last)
	}

	if err := r.UpdateSchedule("02:30", "Etc/UTC", false); err != nil {
		t.Fatalf("re-enable schedule: %v", err)
	}
	r.tryNaturalRun(context.Background())
	if got := runs.Load(); got != 1 {
		t.Fatalf("runs after re-enabling = %d, want 1", got)
	}
}

func TestDisabledScheduleStillAcceptsManualScanAll(t *testing.T) {
	var scans atomic.Int32
	r := New(Config{
		Settings: newStubSettings(),
		Disabled: true,
		ListScanTargets: func(context.Context) ([]string, error) {
			scans.Add(1)
			return nil, nil
		},
	})

	if !r.TriggerScanAll() {
		t.Fatal("disabled schedule rejected manual scan-all")
	}
	r.runModeLocked(context.Background(), <-r.trigger)
	if got := scans.Load(); got != 1 {
		t.Fatalf("manual scans while disabled = %d, want 1", got)
	}
}

// recorder accumulates the order of phase invocations so tests can assert
// orchestration semantics.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) push(s string) {
	r.mu.Lock()
	r.calls = append(r.calls, s)
	r.mu.Unlock()
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

func TestRunPipelineHonoursPhaseOrder(t *testing.T) {
	rec := &recorder{}
	settings := newStubSettings()

	r := New(Config{
		Settings: settings,
		ListScanTargets: func(context.Context) ([]string, error) {
			rec.push("list-scan")
			return []string{"drive-a", "drive-b"}, nil
		},
		RunScan: func(_ context.Context, id string) scanjob.Result {
			rec.push("scan:" + id)

			return scanjob.Result{State: scanjob.Succeeded}
		},
		ListCrawlerDrives: func(context.Context) []string {
			rec.push("list-crawler")
			return []string{"sp-1"}
		},
		RunCrawlerCrawl: func(_ context.Context, id string) error {
			rec.push("crawl:" + id)
			return nil
		},
		WaitPreviewQueuesIdle: func(context.Context) error {
			rec.push("wait-idle")
			return nil
		},
		RunLocalAssetReconciliation: func(context.Context) (int, error) {
			rec.push("asset-reconciliation")
			return 2, nil
		},
		RunDedupeAssetCleanup: func(context.Context) error {
			rec.push("dedupe-cleanup")
			return nil
		},
	})

	r.runPipeline(context.Background())

	got := rec.snapshot()
	want := []string{
		"list-scan",
		"scan:drive-a",
		"scan:drive-b",
		"wait-idle", // after phase 1
		"asset-reconciliation",
		"wait-idle", // after repaired local assets are admitted
		"list-crawler",
		"crawl:sp-1",
		"wait-idle", // after uploads and restored videos, including drive metadata
		"dedupe-cleanup",
	}
	if len(got) != len(want) {
		t.Fatalf("call sequence len = %d, want %d; got=%v", len(got), len(want), got)
	}
	// Drive scans may finish in either order, but both must precede queue waits.
	slices.Sort(got[1:3])
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call[%d] = %q, want %q (full=%v)", i, got[i], want[i], got)
		}
	}
}

func TestRunPipelineReconcilesLocalAssetsWithoutScanTargets(t *testing.T) {
	rec := &recorder{}
	r := New(Config{
		Settings:        newStubSettings(),
		ListScanTargets: func(context.Context) ([]string, error) { return nil, nil },
		WaitPreviewQueuesIdle: func(context.Context) error {
			rec.push("wait-idle")
			return nil
		},
		RunLocalAssetReconciliation: func(context.Context) (int, error) {
			rec.push("asset-reconciliation")
			return 0, nil
		},
		ListCrawlerDrives: func(context.Context) []string { return nil },
		RunDedupeAssetCleanup: func(context.Context) error {
			rec.push("dedupe-cleanup")
			return nil
		},
	})

	r.runPipeline(context.Background())

	got := rec.snapshot()
	want := []string{"wait-idle", "asset-reconciliation", "wait-idle", "dedupe-cleanup"}
	if len(got) != len(want) {
		t.Fatalf("call sequence len = %d, want %d; got=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call[%d] = %q, want %q (full=%v)", i, got[i], want[i], got)
		}
	}
}

func TestUploadedResourcesCompleteBeforeDuplicateMaintenance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	uploaded := false
	waiting := make(chan struct{})
	completed := make(chan struct{})
	deduped := make(chan struct{})
	r := New(Config{
		Settings: newStubSettings(),
		RunTelegramUpload: func(context.Context) error {
			uploaded = true
			return nil
		},
		WaitPreviewQueuesIdle: func(ctx context.Context) error {
			if !uploaded {
				return nil
			}
			close(waiting)
			select {
			case <-completed:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		RunDedupeAssetCleanup: func(context.Context) error { close(deduped); return nil },
	})
	done := make(chan struct{})
	go func() { defer close(done); r.runPipeline(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal("uploaded resource completion was not awaited")
	}
	select {
	case <-deduped:
		t.Fatal("dedupe ran before uploaded resources and duration backfill completed")
	default:
	}
	close(completed)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("pipeline did not finish")
	}
	select {
	case <-deduped:
	default:
		t.Fatal("dedupe did not follow resource completion")
	}
}

func TestRunScanAllOnlyScansConfiguredDrivesAndDedupes(t *testing.T) {
	rec := &recorder{}
	settings := newStubSettings()
	now := time.Date(2026, 8, 3, 11, 30, 0, 0, time.Local)

	r := New(Config{
		Settings: settings,
		Now:      func() time.Time { return now },
		ListScanTargets: func(context.Context) ([]string, error) {
			rec.push("list-scan")
			return []string{"drive-a", "drive-b"}, nil
		},
		RunScan: func(_ context.Context, id string) scanjob.Result {
			rec.push("scan:" + id)

			return scanjob.Result{State: scanjob.Succeeded}
		},
		WaitPreviewQueuesIdle: func(context.Context) error {
			rec.push("wait-idle")
			return nil
		},
		RunLocalAssetReconciliation: func(context.Context) (int, error) {
			rec.push("asset-reconciliation")
			return 1, nil
		},
		ListCrawlerDrives: func(context.Context) []string {
			rec.push("list-crawler")
			return []string{"crawler-a"}
		},
		RunCrawlerCrawl: func(_ context.Context, id string) error {
			rec.push("crawl:" + id)
			return nil
		},
		RunDedupeAssetCleanup: func(context.Context) error {
			rec.push("dedupe-cleanup")
			return nil
		},
	})

	if !r.TriggerScanAll() {
		t.Fatal("TriggerScanAll should queue the manual scan")
	}
	mode := <-r.trigger
	if mode != runModeScanAll {
		t.Fatalf("queued mode = %v, want scan-all", mode)
	}
	r.runModeLocked(context.Background(), mode)

	got := rec.snapshot()
	want := []string{
		"list-scan",
		"scan:drive-a",
		"scan:drive-b",
		"wait-idle",
		"asset-reconciliation",
		"wait-idle",
		"dedupe-cleanup",
	}
	if len(got) != len(want) {
		t.Fatalf("call sequence len = %d, want %d; got=%v", len(got), len(want), got)
	}
	slices.Sort(got[1:3])
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call[%d] = %q, want %q (full=%v)", i, got[i], want[i], got)
		}
	}
	if lastRun, _ := settings.GetSetting(context.Background(), settingLastRunDate, ""); lastRun != "" {
		t.Fatalf("scan-all consumed scheduled nightly date: %q", lastRun)
	}
}

func TestRunPipelineSkipsMigrationWhenNoCrawler(t *testing.T) {
	rec := &recorder{}

	r := New(Config{
		Settings:        newStubSettings(),
		ListScanTargets: func(context.Context) ([]string, error) { return []string{"drive-a"}, nil },
		RunScan: func(_ context.Context, id string) scanjob.Result {
			rec.push("scan:" + id)
			return scanjob.Result{State: scanjob.Succeeded}
		},
		ListCrawlerDrives: func(context.Context) []string { return nil },
		RunCrawlerCrawl:   func(_ context.Context, id string) error { rec.push("crawl:" + id); return nil },
		WaitPreviewQueuesIdle: func(context.Context) error {
			rec.push("wait-idle")
			return nil
		},
		RunLocalAssetReconciliation: func(context.Context) (int, error) {
			rec.push("asset-reconciliation")
			return 0, nil
		},
		RunDedupeAssetCleanup: func(context.Context) error {
			rec.push("dedupe-cleanup")
			return nil
		},
	})

	r.runPipeline(context.Background())

	for _, c := range rec.snapshot() {
		if c == "migrate" || c == "crawl:sp-1" {
			t.Fatalf("phase 2/3 should be skipped when no crawler, got call %q", c)
		}
	}
	foundCleanup := false
	foundAssetReconciliation := false
	for _, c := range rec.snapshot() {
		if c == "dedupe-cleanup" {
			foundCleanup = true
		}
		if c == "asset-reconciliation" {
			foundAssetReconciliation = true
		}
	}
	if !foundCleanup {
		t.Fatalf("dedupe cleanup should still run when crawler is absent; calls=%v", rec.snapshot())
	}
	if !foundAssetReconciliation {
		t.Fatalf("asset reconciliation should run when crawler is absent; calls=%v", rec.snapshot())
	}
}

func TestRunPipelineExitsWhenContextCancelledMidPhase(t *testing.T) {
	rec := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := New(Config{
		Settings: newStubSettings(),
		ListScanTargets: func(context.Context) ([]string, error) {
			return []string{"drive-a", "drive-b", "drive-c"}, nil
		},
		RunScan: func(_ context.Context, id string) scanjob.Result {
			rec.push("scan:" + id)
			if id == "drive-a" {
				cancel()
			}

			return scanjob.Result{State: scanjob.Succeeded}
		},
		ListCrawlerDrives:     func(context.Context) []string { return []string{"x"} },
		RunCrawlerCrawl:       func(context.Context, string) error { rec.push("crawl"); return nil },
		WaitPreviewQueuesIdle: func(context.Context) error { rec.push("wait-idle"); return nil },
		RunLocalAssetReconciliation: func(context.Context) (int, error) {
			rec.push("asset-reconciliation")
			return 0, nil
		},
		RunDedupeAssetCleanup: func(context.Context) error { rec.push("dedupe-cleanup"); return nil },
	})

	r.runPipeline(ctx)

	got := rec.snapshot()
	for _, c := range got {
		if c == "crawl" || c == "migrate" || c == "asset-reconciliation" || c == "wait-idle" {
			t.Fatalf("subsequent phase should not run after cancel, got call %q", c)
		}
		if c == "dedupe-cleanup" {
			t.Fatalf("dedupe cleanup should not run after cancel, got call %q", c)
		}
	}
}

func TestRunPipelineRecordsLastRunDateAfterCompletion(t *testing.T) {
	settings := newStubSettings()
	now := time.Date(2026, 5, 27, 1, 5, 0, 0, time.UTC)
	r := New(Config{
		Settings:              settings,
		Now:                   func() time.Time { return now },
		ListScanTargets:       func(context.Context) ([]string, error) { return nil, nil },
		WaitPreviewQueuesIdle: func(context.Context) error { return nil },
	})

	r.runModeLocked(context.Background(), runModeScheduled)

	got, _ := settings.GetSetting(context.Background(), settingLastRunDate, "")
	if got != "2026-05-27" {
		t.Fatalf("last_run_date = %q, want 2026-05-27", got)
	}
}

func TestRunModeLockedDropsOverlappingRuns(t *testing.T) {
	var (
		started      atomic.Int32
		releaseFirst = make(chan struct{})
	)
	r := New(Config{
		Settings: newStubSettings(),
		ListScanTargets: func(context.Context) ([]string, error) {
			started.Add(1)
			<-releaseFirst
			return nil, nil
		},
		WaitPreviewQueuesIdle: func(context.Context) error { return nil },
	})

	go r.runModeLocked(context.Background(), runModeScheduled)

	// Wait for first to start
	for started.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	// Second trigger should bail out without invoking ListScanTargets again
	r.runModeLocked(context.Background(), runModeScheduled)
	if started.Load() != 1 {
		t.Fatalf("overlapping run should be dropped; started=%d", started.Load())
	}
	close(releaseFirst)
}

// TestCtxCancelPreventsLaterPhases 校验：ctx 在 phase 边界已取消（进程退出）时，
// 后续 phase 不会启动。"ctx 已 done 就 bail" 仍保留。
func TestCtxCancelPreventsLaterPhases(t *testing.T) {
	rec := &recorder{}
	settings := newStubSettings()

	r := New(Config{
		Settings:        settings,
		ListScanTargets: func(context.Context) ([]string, error) { return nil, nil },
		WaitPreviewQueuesIdle: func(ctx context.Context) error {
			return ctx.Err()
		},
		ListCrawlerDrives: func(context.Context) []string {
			rec.push("list-crawler")
			return []string{"x"}
		},
		RunCrawlerCrawl: func(context.Context, string) error { rec.push("crawl"); return nil },
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r.runPipeline(ctx)

	for _, c := range rec.snapshot() {
		if c == "crawl" || c == "migrate" || c == "list-crawler" {
			t.Fatalf("later phase should not run after ctx done; got %q", c)
		}
	}
}

func TestTriggerScanAllIsNonBlocking(t *testing.T) {
	r := New(Config{Settings: newStubSettings()})
	// fill the trigger channel
	if !r.TriggerScanAll() {
		t.Fatal("first TriggerScanAll should be accepted")
	}
	// Second call must not block
	done := make(chan struct{})
	var accepted bool
	go func() {
		accepted = r.TriggerScanAll()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("TriggerScanAll blocked when channel is full")
	}
	if accepted {
		t.Fatal("second TriggerScanAll should be ignored when trigger channel is full")
	}
}

func TestStatusTracksQueuedRunningAndFinished(t *testing.T) {
	blockScan := make(chan struct{})
	scanStarted := make(chan struct{})
	var startedOnce sync.Once
	r := New(Config{
		Settings: newStubSettings(),
		ListScanTargets: func(context.Context) ([]string, error) {
			return []string{"drive"}, nil
		},
		RunScan: func(context.Context, string) scanjob.Result {
			startedOnce.Do(func() { close(scanStarted) })
			<-blockScan

			return scanjob.Result{State: scanjob.Succeeded}
		},
	})

	if got := r.Status(); got.State != "idle" || got.Running || got.Queued {
		t.Fatalf("initial status = %#v, want idle", got)
	}

	if !r.TriggerScanAll() {
		t.Fatal("TriggerScanAll should queue a manual run")
	}
	if got := r.Status(); got.State != "queued" || got.Running || !got.Queued {
		t.Fatalf("queued status = %#v, want queued", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	select {
	case <-scanStarted:
	case <-time.After(time.Second):
		t.Fatal("pipeline did not start")
	}

	if got := r.Status(); got.State != "running" || !got.Running || got.Queued || got.StartedAt.IsZero() {
		t.Fatalf("running status = %#v, want running with startedAt", got)
	}

	if r.TriggerScanAll() {
		t.Fatal("TriggerScanAll during a run should be ignored")
	}
	if got := r.Status(); got.State != "running" || !got.Running || got.Queued {
		t.Fatalf("status after ignored trigger = %#v, want running", got)
	}

	close(blockScan)
	deadline := time.After(time.Second)
	for {
		got := r.Status()
		if !got.Running && !got.Queued && !got.LastFinishedAt.IsZero() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("status did not finish; got=%#v", got)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestStopCurrentCancelsRunningPipeline(t *testing.T) {
	scanStarted := make(chan struct{})
	scanCanceled := make(chan struct{})
	var startedOnce sync.Once
	r := New(Config{
		Settings: newStubSettings(),
		ListScanTargets: func(context.Context) ([]string, error) {
			return []string{"drive"}, nil
		},
		RunScan: func(ctx context.Context, _ string) scanjob.Result {
			startedOnce.Do(func() { close(scanStarted) })
			<-ctx.Done()
			close(scanCanceled)

			return scanjob.Result{State: scanjob.Succeeded}
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	if !r.TriggerScanAll() {
		t.Fatal("TriggerScanAll should queue a manual run")
	}
	select {
	case <-scanStarted:
	case <-time.After(time.Second):
		t.Fatal("pipeline did not start")
	}

	if !r.StopCurrent() {
		t.Fatal("StopCurrent should report a running pipeline")
	}
	select {
	case <-scanCanceled:
	case <-time.After(time.Second):
		t.Fatal("StopCurrent did not cancel pipeline context")
	}
}

func TestStopCurrentDropsQueuedTrigger(t *testing.T) {
	r := New(Config{Settings: newStubSettings()})
	if !r.TriggerScanAll() {
		t.Fatal("TriggerScanAll should queue a manual run")
	}
	if !r.StopCurrent() {
		t.Fatal("StopCurrent should report a queued pipeline")
	}
	if got := r.Status(); got.State != "idle" || got.Running || got.Queued {
		t.Fatalf("status = %#v, want idle after dropping queued trigger", got)
	}
	if !r.TriggerScanAll() {
		t.Fatal("TriggerScanAll should accept a new request after queued stop")
	}
}

func TestTriggerScanAllAcceptsOnlyOneConcurrentRequest(t *testing.T) {
	r := New(Config{Settings: newStubSettings()})

	const callers = 16
	start := make(chan struct{})
	results := make(chan bool, callers)
	for i := 0; i < callers; i++ {
		go func() {
			<-start
			results <- r.TriggerScanAll()
		}()
	}
	close(start)

	accepted := 0
	for i := 0; i < callers; i++ {
		if <-results {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted triggers = %d, want 1", accepted)
	}
	if got := r.Status(); got.State != "queued" || got.Running || !got.Queued {
		t.Fatalf("status = %#v, want one queued trigger", got)
	}
}

func TestScheduledCrawlerFailureReachesRoundResult(t *testing.T) {
	r := New(Config{Settings: newStubSettings(), ListScanTargets: func(context.Context) ([]string, error) { return nil, nil }, ListCrawlerDrives: func(context.Context) []string { return []string{"crawler"} }, RunCrawlerCrawl: func(context.Context, string) error { return fmt.Errorf("source unavailable") }})
	r.runPipeline(context.Background())
	if len(r.issues) != 1 || r.issues[0].Stage != "crawl" {
		t.Fatalf("crawler failure lost: %+v", r.issues)
	}
}
