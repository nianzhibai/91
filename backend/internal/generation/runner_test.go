package generation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/tasklimit"
)

type fakeVideo struct {
	drive    string
	order    int
	steps    catalog.GenerationSteps
	deferred time.Time
}

// fakeStore keeps due steps in memory and lists them like the catalog: the
// fingerprint lane by insertion order, the asset lane by media, covers and
// durations, then insertion order.
type fakeStore struct {
	mu     sync.Mutex
	videos map[string]*fakeVideo
	next   int
	// readErr fails GenerationWork for a video; reads counts its calls.
	readErr map[string]error
	reads   map[string]int
}

func newFakeStore() *fakeStore {
	return &fakeStore{videos: make(map[string]*fakeVideo), readErr: make(map[string]error), reads: make(map[string]int)}
}

func (s *fakeStore) failReads(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		delete(s.readErr, id)
	} else {
		s.readErr[id] = err
	}
}

func (s *fakeStore) readCount(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads[id]
}

func (s *fakeStore) add(drive, id string, steps catalog.GenerationSteps) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	s.videos[id] = &fakeVideo{drive: drive, order: s.next, steps: steps}
}

func (s *fakeStore) settle(id string, kind Kind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	steps := &s.videos[id].steps
	switch kind {
	case KindFingerprint:
		steps.Fingerprint = false
	case KindMedia:
		steps.Media = false
	case KindThumbnail:
		steps.Thumbnail = false
	case KindDuration:
		steps.Duration = false
	}
}

func (s *fakeStore) deferredUntil(id string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.videos[id].deferred
}

func rank(steps catalog.GenerationSteps) int {
	switch {
	case steps.Media:
		return 0
	case steps.Thumbnail:
		return 1
	default:
		return 2
	}
}

func (s *fakeStore) ListDueFingerprints(_ context.Context, driveID string, now time.Time, limit int) ([]string, error) {
	return s.listDue(driveID, now, limit, func(steps catalog.GenerationSteps) bool { return steps.Fingerprint }), nil
}

func (s *fakeStore) ListDueAssets(_ context.Context, driveID string, _ bool, now time.Time, limit int) ([]string, error) {
	return s.listDue(driveID, now, limit, catalog.GenerationSteps.AssetsDue), nil
}

func (s *fakeStore) listDue(driveID string, now time.Time, limit int, due func(catalog.GenerationSteps) bool) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id, v := range s.videos {
		if v.drive == driveID && due(v.steps) && !v.deferred.After(now) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := s.videos[ids[i]], s.videos[ids[j]]
		if rank(a.steps) != rank(b.steps) {
			return rank(a.steps) < rank(b.steps)
		}
		return a.order < b.order
	})
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids
}

func (s *fakeStore) GenerationWork(_ context.Context, id string, _ bool) (*catalog.Video, catalog.GenerationSteps, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads[id]++
	if err := s.readErr[id]; err != nil {
		return nil, catalog.GenerationSteps{}, err
	}
	v, ok := s.videos[id]
	if !ok {
		return nil, catalog.GenerationSteps{}, sql.ErrNoRows
	}
	return &catalog.Video{ID: id, DriveID: v.drive, Title: id}, v.steps, nil
}

func (s *fakeStore) CountDueGeneration(_ context.Context, driveID string, _ bool, now time.Time) (catalog.GenerationCounts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var counts catalog.GenerationCounts
	for _, v := range s.videos {
		if v.drive != driveID || v.deferred.After(now) {
			continue
		}
		if v.steps.Fingerprint {
			counts.Fingerprints++
		}
		if v.steps.Media {
			counts.Media++
		}
		if v.steps.Thumbnail {
			counts.Thumbnails++
		}
		if v.steps.Duration {
			counts.Durations++
		}
		if v.steps.Thumbnail || v.steps.Duration {
			counts.Covers++
		}
	}
	return counts, nil
}

func (s *fakeStore) DeferGeneration(_ context.Context, id string, until time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.videos[id].deferred = until
	return nil
}

// recorder is a set of steps that settle their work and log each call.
type recorder struct {
	store *fakeStore
	mu    sync.Mutex
	calls []string
}

func (r *recorder) step(kind Kind) Step {
	return StepFunc(func(_ context.Context, v *catalog.Video) StepOutcome {
		r.mu.Lock()
		r.calls = append(r.calls, v.ID+":"+string(kind))
		r.mu.Unlock()
		r.store.settle(v.ID, kind)
		return StepSettled
	})
}

func (r *recorder) steps() Steps {
	return Steps{
		Fingerprint: r.step(KindFingerprint),
		Media:       r.step(KindMedia),
		Thumbnail:   r.step(KindThumbnail),
		Duration:    r.step(KindDuration),
	}
}

func (r *recorder) log() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func startRunner(t *testing.T, cfg Config) *Runner {
	t.Helper()
	if cfg.Pace == 0 {
		cfg.Pace = time.Millisecond
	}
	runner := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runner.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return runner
}

func waitIdle(t *testing.T, runner *Runner) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runner.WaitIdle(ctx); err != nil {
		t.Fatalf("runner did not become idle: %v (status %+v)", err, runner.Status())
	}
}

// callsOf returns the logged calls for one video, in order.
func callsOf(calls []string, id string) []string {
	var out []string
	for _, call := range calls {
		if strings.HasPrefix(call, id+":") {
			out = append(out, call)
		}
	}
	return out
}

func TestRunnerRunsEachVideosDueStepsInOrder(t *testing.T) {
	store := newFakeStore()
	store.add("drive", "renewal", catalog.GenerationSteps{Media: true})
	store.add("drive", "new", catalog.GenerationSteps{Fingerprint: true, Media: true, Thumbnail: true, Duration: true})
	store.add("drive", "cover", catalog.GenerationSteps{Thumbnail: true})
	store.add("other", "elsewhere", catalog.GenerationSteps{Media: true})
	rec := &recorder{store: store}
	runner := startRunner(t, Config{DriveID: "drive", Store: store, Steps: rec.steps()})
	waitIdle(t, runner)

	calls := rec.log()
	for id, want := range map[string][]string{
		"new":       {"new:fingerprint", "new:media", "new:thumbnail", "new:duration"},
		"renewal":   {"renewal:media"},
		"cover":     {"cover:thumbnail"},
		"elsewhere": nil,
	} {
		if got := callsOf(calls, id); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s calls = %v, want %v (all calls %v)", id, got, want, calls)
		}
	}
	if status := runner.Status(); status.State != "idle" || status.Counts.Any() {
		t.Fatalf("status = %+v, want idle with nothing due", status)
	}
}

func TestFingerprintLaneRunsBesideAssetsWithoutASlot(t *testing.T) {
	store := newFakeStore()
	store.add("drive", "clip", catalog.GenerationSteps{Media: true})
	store.add("drive", "new", catalog.GenerationSteps{Fingerprint: true})
	limiter := tasklimit.New(1)
	mediaStarted, fingerprinted := make(chan struct{}), make(chan struct{})
	runner := startRunner(t, Config{DriveID: "drive", Store: store, Limiter: limiter, Steps: Steps{
		// The media job holds the only slot until the fingerprint is done.
		Media: StepFunc(func(ctx context.Context, v *catalog.Video) StepOutcome {
			close(mediaStarted)
			select {
			case <-fingerprinted:
			case <-time.After(time.Second):
				t.Error("the fingerprint waited for the media job")
			}
			store.settle(v.ID, KindMedia)
			return StepSettled
		}),
		Fingerprint: StepFunc(func(ctx context.Context, v *catalog.Video) StepOutcome {
			<-mediaStarted
			store.settle(v.ID, KindFingerprint)
			close(fingerprinted)
			return StepSettled
		}),
	}})
	waitIdle(t, runner)
	if counts := runner.Status().Counts; counts.Any() {
		t.Fatalf("counts = %+v, want everything done", counts)
	}
}

func TestSettledFingerprintWakesTheAssetLane(t *testing.T) {
	store := newFakeStore()
	rec := &recorder{store: store}
	steps := rec.steps()
	fingerprint := steps.Fingerprint
	// The asset lane reads the catalog, finds nothing and goes idle while the
	// fingerprint runs. Without a poll, only the fingerprint lane can tell it.
	steps.Fingerprint = StepFunc(func(ctx context.Context, v *catalog.Video) StepOutcome {
		time.Sleep(20 * time.Millisecond)
		return fingerprint.Run(ctx, v)
	})
	runner := startRunner(t, Config{DriveID: "drive", Store: store, Steps: steps, PollInterval: time.Hour})
	waitIdle(t, runner)
	store.add("drive", "new", catalog.GenerationSteps{Fingerprint: true, Media: true})
	runner.Wake()
	waitIdle(t, runner)
	if got := rec.log(); !reflect.DeepEqual(got, []string{"new:fingerprint", "new:media"}) {
		t.Fatalf("calls = %v, want the media job right after the fingerprint", got)
	}
}

func TestRunnerRereadsTheRowBeforeEachStep(t *testing.T) {
	store := newFakeStore()
	store.add("drive", "duplicate", catalog.GenerationSteps{Fingerprint: true, Media: true, Thumbnail: true})
	var calls []Kind
	runner := startRunner(t, Config{DriveID: "drive", Store: store, Steps: Steps{
		Fingerprint: StepFunc(func(_ context.Context, v *catalog.Video) StepOutcome {
			calls = append(calls, KindFingerprint)
			// The fingerprint shows a duplicate: nothing else is due.
			store.mu.Lock()
			store.videos[v.ID].steps = catalog.GenerationSteps{}
			store.mu.Unlock()
			return StepSettled
		}),
		Media: StepFunc(func(context.Context, *catalog.Video) StepOutcome {
			calls = append(calls, KindMedia)
			return StepSettled
		}),
	}})
	waitIdle(t, runner)
	if !reflect.DeepEqual(calls, []Kind{KindFingerprint}) {
		t.Fatalf("calls = %v, want only the fingerprint", calls)
	}
}

func TestRunnerDefersVideosLeftUnsettled(t *testing.T) {
	store := newFakeStore()
	store.add("drive", "stubborn", catalog.GenerationSteps{Duration: true})
	attempts := 0
	runner := startRunner(t, Config{DriveID: "drive", Store: store, RetryDelay: time.Hour, Steps: Steps{
		Duration: StepFunc(func(context.Context, *catalog.Video) StepOutcome { attempts++; return StepUnsettled }),
	}})
	waitIdle(t, runner)
	runner.Wake()
	waitIdle(t, runner)
	if attempts != 1 {
		t.Fatalf("attempts = %d, want one until the deferral ends", attempts)
	}
	if until := store.deferredUntil("stubborn"); time.Until(until) < 50*time.Minute {
		t.Fatalf("deferred until %s, want about an hour", until)
	}
}

func TestRunnerKeepsCoolingVideosDueWithoutHoldingASlot(t *testing.T) {
	store := newFakeStore()
	store.add("cooling", "throttled", catalog.GenerationSteps{Media: true})
	store.add("cooling", "waiting", catalog.GenerationSteps{Media: true})
	store.add("healthy", "clip", catalog.GenerationSteps{Media: true})
	limiter := tasklimit.New(1)
	cooldown := &tasklimit.Cooldown{}
	var mu sync.Mutex
	var calls []string
	throttle := StepFunc(func(_ context.Context, v *catalog.Video) StepOutcome {
		mu.Lock()
		calls = append(calls, v.ID)
		mu.Unlock()
		cooldown.Pause(time.Now(), time.Hour)
		return StepPaused
	})
	cooling := startRunner(t, Config{DriveID: "cooling", Store: store, Limiter: limiter, Cooldown: cooldown, Steps: Steps{Media: throttle}})
	rec := &recorder{store: store}
	healthy := startRunner(t, Config{DriveID: "healthy", Store: store, Limiter: limiter, Steps: rec.steps()})

	waitIdle(t, healthy)
	if got := rec.log(); !reflect.DeepEqual(got, []string{"clip:media"}) {
		t.Fatalf("healthy drive calls = %v", got)
	}
	deadline := time.Now().Add(time.Second)
	for cooling.Status().State != "cooling" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	status := cooling.Status()
	if status.State != "cooling" || !cooling.Busy() || status.Counts.Media != 2 {
		t.Fatalf("cooling status = %+v, want both videos due behind the cooldown", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(calls, []string{"throttled"}) {
		t.Fatalf("cooling drive calls = %v, want no read during its cooldown", calls)
	}
	if !store.deferredUntil("throttled").IsZero() {
		t.Fatal("a cooling video was deferred")
	}
}

func TestCloudRunnersShareSlotsOneVideoAtATime(t *testing.T) {
	store := newFakeStore()
	for _, drive := range []string{"a", "b", "c"} {
		for i := range 3 {
			store.add(drive, fmt.Sprintf("%s-%d", drive, i), catalog.GenerationSteps{Media: true})
		}
	}
	limiter := tasklimit.New(2)
	var mu sync.Mutex
	active, peak := map[string]int{}, 0
	perDrivePeak := 0
	step := StepFunc(func(_ context.Context, v *catalog.Video) StepOutcome {
		mu.Lock()
		active[v.DriveID]++
		total := 0
		for _, n := range active {
			total += n
		}
		peak = max(peak, total)
		perDrivePeak = max(perDrivePeak, active[v.DriveID])
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		active[v.DriveID]--
		mu.Unlock()
		store.settle(v.ID, KindMedia)
		return StepSettled
	})
	var runners []*Runner
	for _, drive := range []string{"a", "b", "c"} {
		runners = append(runners, startRunner(t, Config{DriveID: drive, Store: store, Limiter: limiter, Parallelism: 1, Steps: Steps{Media: step}}))
	}
	for _, runner := range runners {
		waitIdle(t, runner)
	}
	if peak != 2 || perDrivePeak != 1 {
		t.Fatalf("peak = %d, per drive = %d; want two drives at once, one video each", peak, perDrivePeak)
	}
}

func TestRunnerStopInterruptsUntilWake(t *testing.T) {
	store := newFakeStore()
	store.add("drive", "long", catalog.GenerationSteps{Media: true})
	started := make(chan struct{})
	var once sync.Once
	step := StepFunc(func(ctx context.Context, v *catalog.Video) StepOutcome {
		first := false
		once.Do(func() { first = true })
		if first {
			close(started)
			<-ctx.Done()
			return StepPaused
		}
		store.settle(v.ID, KindMedia)
		return StepSettled
	})
	runner := startRunner(t, Config{DriveID: "drive", Store: store, Steps: Steps{Media: step}})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("step did not start")
	}
	if !runner.Stop() {
		t.Fatal("stop found the runner idle")
	}
	waitIdle(t, runner)
	if status := runner.Status(); !status.Paused || status.Counts.Media != 1 {
		t.Fatalf("stopped status = %+v, want the work still due", status)
	}
	runner.Wake()
	waitIdle(t, runner)
	if counts := runner.Status().Counts; counts.Any() {
		t.Fatalf("counts after wake = %+v, want the work done", counts)
	}
}

func TestRunnerWaitIdleCoversWorkWrittenBeforeWake(t *testing.T) {
	store := newFakeStore()
	rec := &recorder{store: store}
	runner := startRunner(t, Config{DriveID: "drive", Store: store, Steps: rec.steps(), PollInterval: time.Hour})
	waitIdle(t, runner)
	store.add("drive", "uploaded", catalog.GenerationSteps{Media: true})
	runner.Wake()
	waitIdle(t, runner)
	if got := rec.log(); !reflect.DeepEqual(got, []string{"uploaded:media"}) {
		t.Fatalf("calls = %v", got)
	}
}

func TestRunnerStartsNoWorkWithoutAdmission(t *testing.T) {
	store := newFakeStore()
	store.add("drive", "clip", catalog.GenerationSteps{Media: true})
	rec := &recorder{store: store}
	runner := startRunner(t, Config{DriveID: "drive", Store: store, Steps: rec.steps(), Admit: func() func() { return nil }})
	time.Sleep(20 * time.Millisecond)
	if got := rec.log(); len(got) != 0 || runner.Busy() {
		t.Fatalf("calls = %v busy = %v, want the work left due without holding up the runtime change", got, runner.Busy())
	}
	if counts := runner.Status().Counts; counts.Media != 1 {
		t.Fatalf("counts = %+v, want the work still due", counts)
	}
}

func TestPausedRunnerWaitsForWake(t *testing.T) {
	store := newFakeStore()
	store.add("drive", "clip", catalog.GenerationSteps{Media: true})
	rec := &recorder{store: store}
	runner := startRunner(t, Config{DriveID: "drive", Store: store, Steps: rec.steps(), Paused: true})
	time.Sleep(20 * time.Millisecond)
	if got := rec.log(); len(got) != 0 || runner.Busy() {
		t.Fatalf("paused runner calls = %v busy = %v", got, runner.Busy())
	}
	runner.Wake()
	waitIdle(t, runner)
	if got := rec.log(); !reflect.DeepEqual(got, []string{"clip:media"}) {
		t.Fatalf("calls = %v", got)
	}
}

func TestRunnerPacesOnlyVideosThatReadTheDrive(t *testing.T) {
	store := newFakeStore()
	// Listed as due, gone by the time the runner reads them.
	for _, id := range []string{"gone-1", "gone-2"} {
		store.add("drive", id, catalog.GenerationSteps{Media: true})
		store.failReads(id, sql.ErrNoRows)
	}
	store.add("drive", "clip", catalog.GenerationSteps{Media: true})
	rec := &recorder{store: store}
	startRunner(t, Config{DriveID: "drive", Store: store, Steps: rec.steps(), Pace: time.Hour})
	deadline := time.Now().Add(2 * time.Second)
	for len(rec.log()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := rec.log(); !reflect.DeepEqual(got, []string{"clip:media"}) {
		t.Fatalf("calls = %v, want the clip without pausing after videos that ran nothing", got)
	}
}

func TestRunnerWaitsForTheNextWakeAfterAnUnreadableRow(t *testing.T) {
	store := newFakeStore()
	store.add("drive", "clip", catalog.GenerationSteps{Media: true})
	store.failReads("clip", errors.New("database is locked"))
	rec := &recorder{store: store}
	runner := startRunner(t, Config{DriveID: "drive", Store: store, Steps: rec.steps(), PollInterval: time.Hour})
	deadline := time.Now().Add(time.Second)
	for store.readCount("clip") == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	if reads := store.readCount("clip"); reads != 1 {
		t.Fatalf("reads = %d, want one until the next wake", reads)
	}
	if !runner.Busy() {
		t.Fatal("runner looks idle with the work still due")
	}
	store.failReads("clip", nil)
	runner.Wake()
	waitIdle(t, runner)
	if got := rec.log(); !reflect.DeepEqual(got, []string{"clip:media"}) {
		t.Fatalf("calls = %v", got)
	}
}

// concurrency records how many videos run a step at once.
type concurrency struct {
	mu     sync.Mutex
	active map[string]int
	peak   int
	videos map[string]int
	twice  bool
}

func (c *concurrency) enter(v *catalog.Video) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil {
		c.active, c.videos = map[string]int{}, map[string]int{}
	}
	c.active[v.DriveID]++
	c.peak = max(c.peak, c.active[v.DriveID])
	if c.videos[v.ID]++; c.videos[v.ID] > 1 {
		c.twice = true
	}
}

func (c *concurrency) leave(v *catalog.Video) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active[v.DriveID]--
	c.videos[v.ID]--
}

func (c *concurrency) result() (peak int, twice bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peak, c.twice
}

func TestLocalSourceTakesEverySlot(t *testing.T) {
	store := newFakeStore()
	for i := range 9 {
		store.add("local", fmt.Sprintf("clip-%d", i), catalog.GenerationSteps{Fingerprint: true, Media: true})
	}
	var assets, fingerprints concurrency
	runner := startRunner(t, Config{DriveID: "local", Store: store, Limiter: tasklimit.New(3), Steps: Steps{
		Fingerprint: StepFunc(func(_ context.Context, v *catalog.Video) StepOutcome {
			fingerprints.enter(v)
			time.Sleep(time.Millisecond)
			fingerprints.leave(v)
			store.settle(v.ID, KindFingerprint)
			return StepSettled
		}),
		Media: StepFunc(func(_ context.Context, v *catalog.Video) StepOutcome {
			assets.enter(v)
			time.Sleep(10 * time.Millisecond)
			assets.leave(v)
			store.settle(v.ID, KindMedia)
			return StepSettled
		}),
	}})
	waitIdle(t, runner)
	if peak, twice := assets.result(); peak != 3 || twice {
		t.Fatalf("asset peak = %d, twice = %v; want every slot, each video once", peak, twice)
	}
	if peak, _ := fingerprints.result(); peak != 1 {
		t.Fatalf("fingerprint peak = %d, want one at a time", peak)
	}
	if counts := runner.Status().Counts; counts.Any() {
		t.Fatalf("counts = %+v, want everything done", counts)
	}
}

func TestAssetLaneNeverRunsAVideoTwiceAtOnce(t *testing.T) {
	store := newFakeStore()
	store.add("local", "slow", catalog.GenerationSteps{Media: true})
	var c concurrency
	release := make(chan struct{})
	runner := startRunner(t, Config{DriveID: "local", Store: store, Limiter: tasklimit.New(3), PollInterval: time.Millisecond,
		Steps: Steps{Media: StepFunc(func(_ context.Context, v *catalog.Video) StepOutcome {
			c.enter(v)
			<-release
			c.leave(v)
			store.settle(v.ID, KindMedia)
			return StepSettled
		})}})
	// The video stays due while it runs; wakes and polls list it again.
	for range 5 {
		runner.Wake()
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	waitIdle(t, runner)
	if peak, twice := c.result(); peak != 1 || twice {
		t.Fatalf("peak = %d, twice = %v; want the video once", peak, twice)
	}
}

func TestSetParallelismAppliesToTheNextVideos(t *testing.T) {
	store := newFakeStore()
	for i := range 6 {
		store.add("drive", fmt.Sprintf("clip-%d", i), catalog.GenerationSteps{Media: true})
	}
	var c concurrency
	first := make(chan struct{})
	var once sync.Once
	gate := make(chan struct{})
	runner := startRunner(t, Config{DriveID: "drive", Store: store, Limiter: tasklimit.New(5), Parallelism: 1,
		Steps: Steps{Media: StepFunc(func(_ context.Context, v *catalog.Video) StepOutcome {
			c.enter(v)
			once.Do(func() { close(first) })
			<-gate
			time.Sleep(10 * time.Millisecond)
			c.leave(v)
			store.settle(v.ID, KindMedia)
			return StepSettled
		})}})
	<-first
	time.Sleep(10 * time.Millisecond)
	if peak, _ := c.result(); peak != 1 {
		t.Fatalf("peak = %d before the change, want one", peak)
	}
	runner.SetParallelism(2)
	time.Sleep(10 * time.Millisecond)
	close(gate)
	waitIdle(t, runner)
	if peak, _ := c.result(); peak != 2 {
		t.Fatalf("peak = %d after the change, want two", peak)
	}
}

func TestRunnerStopInterruptsEveryRunningVideo(t *testing.T) {
	store := newFakeStore()
	for i := range 3 {
		store.add("local", fmt.Sprintf("clip-%d", i), catalog.GenerationSteps{Media: true})
	}
	var started sync.WaitGroup
	started.Add(3)
	runner := startRunner(t, Config{DriveID: "local", Store: store, Limiter: tasklimit.New(3), Steps: Steps{
		Media: StepFunc(func(ctx context.Context, v *catalog.Video) StepOutcome {
			started.Done()
			<-ctx.Done()
			return StepPaused
		}),
	}})
	started.Wait()
	if active := runner.Status().Active; len(active) != 3 {
		t.Fatalf("active = %+v, want three videos", active)
	}
	if !runner.Stop() {
		t.Fatal("stop found the runner idle")
	}
	waitIdle(t, runner)
	if status := runner.Status(); !status.Paused || len(status.Active) != 0 || status.Counts.Media != 3 {
		t.Fatalf("stopped status = %+v, want all three still due", status)
	}
}

func TestStepUnsettledStopsLaterStepsAndDefersVideo(t *testing.T) {
	store := newFakeStore()
	store.add("drive", "broken", catalog.GenerationSteps{Media: true, Thumbnail: true, Duration: true})
	var calls []Kind
	runner := startRunner(t, Config{DriveID: "drive", Store: store, RetryDelay: time.Hour, Steps: Steps{
		Media: StepFunc(func(_ context.Context, v *catalog.Video) StepOutcome {
			calls = append(calls, KindMedia)
			store.settle(v.ID, KindMedia)
			return StepUnsettled
		}),
		Thumbnail: StepFunc(func(_ context.Context, _ *catalog.Video) StepOutcome {
			calls = append(calls, KindThumbnail)
			return StepSettled
		}),
		Duration: StepFunc(func(_ context.Context, _ *catalog.Video) StepOutcome {
			calls = append(calls, KindDuration)
			return StepSettled
		}),
	}})
	waitIdle(t, runner)
	if !reflect.DeepEqual(calls, []Kind{KindMedia}) {
		t.Fatalf("calls = %v, want later steps skipped after an unsettled source failure", calls)
	}
	if until := store.deferredUntil("broken"); time.Until(until) < 50*time.Minute {
		t.Fatalf("deferred until %s, want about an hour", until)
	}
}

func TestDisablingPreviewsMidVideoDoesNotDeferSkippedTeaser(t *testing.T) {
	store := newFakeStore()
	store.add("drive", "clip", catalog.GenerationSteps{Media: true})
	var previews atomicBool
	previews.Store(true)
	started := make(chan struct{})
	proceed := make(chan struct{})
	// Respect the preview switch in GenerationWork just like the catalog does.
	switchingStore := &previewSwitchStore{fakeStore: store, previews: &previews}
	runner := startRunner(t, Config{
		DriveID:         "drive",
		Store:           switchingStore,
		PreviewsEnabled: previews.Load,
		RetryDelay:      time.Hour,
		Steps: Steps{
			Media: StepFunc(func(_ context.Context, _ *catalog.Video) StepOutcome {
				close(started)
				<-proceed
				// Preview switch was disabled while the step waited; nothing to do.
				return StepSettled
			}),
		},
	})
	<-started
	previews.Store(false)
	close(proceed)
	waitIdle(t, runner)
	if until := store.deferredUntil("clip"); !until.IsZero() {
		t.Fatalf("deferred until %s, want no deferral after disabling previews mid-video", until)
	}
}

type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (b *atomicBool) Store(v bool) {
	b.mu.Lock()
	b.v = v
	b.mu.Unlock()
}

func (b *atomicBool) Load() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.v
}

type previewSwitchStore struct {
	*fakeStore
	previews *atomicBool
}

func (s *previewSwitchStore) ListDueAssets(ctx context.Context, driveID string, previews bool, now time.Time, limit int) ([]string, error) {
	if !previews {
		return nil, nil
	}
	return s.fakeStore.ListDueAssets(ctx, driveID, previews, now, limit)
}

func (s *previewSwitchStore) GenerationWork(ctx context.Context, id string, previews bool) (*catalog.Video, catalog.GenerationSteps, error) {
	video, steps, err := s.fakeStore.GenerationWork(ctx, id, previews)
	if err != nil {
		return nil, catalog.GenerationSteps{}, err
	}
	if !previews {
		steps.Media = false
	}
	return video, steps, nil
}
