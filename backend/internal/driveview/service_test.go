package driveview

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/driveevents"
)

func TestChangesMergeScopedSignalsWithoutBlockingWriters(t *testing.T) {
	hub := &driveevents.Hub{}
	a, b := hub.Subscribe("a"), hub.Subscribe("b")
	defer a.Close()
	defer b.Close()
	for i := 0; i < 10000; i++ {
		hub.Notify("a", false, driveevents.ActivityChanged)
	}
	hub.Notify("", true, driveevents.MediaChanged)
	<-a.Wake
	change := a.Take()
	if _, ok := change.Kinds[driveevents.ActivityChanged]; !ok || !change.Kinds[driveevents.MediaChanged] {
		t.Fatalf("merged change: %+v", change)
	}
	<-b.Wake
	if _, ok := b.Take().Kinds[driveevents.ActivityChanged]; ok {
		t.Fatal("a's runtime reached b")
	}
}

func TestConcurrentReadersShareComputationAndCallerCancellation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var loads atomic.Int32
	service := New(&driveevents.Hub{}, func(ctx context.Context, _ string, _ Resource) (any, error) {
		if loads.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return 7, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := service.Get(ctx, "a", Stats, false); first <- err }()
	<-started
	second := make(chan Snapshot, 1)
	go func() { value, _ := service.Get(context.Background(), "a", Stats, false); second <- value }()
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	close(release)
	value := <-second
	if string(value.Data) != "7" || loads.Load() != 1 {
		t.Fatalf("value=%s loads=%d", value.Data, loads.Load())
	}
}

func TestPostWriteReadDoesNotJoinAnOlderSnapshot(t *testing.T) {
	hub := &driveevents.Hub{}
	started, release := make(chan struct{}), make(chan struct{})
	var loads atomic.Int32
	service := New(hub, func(context.Context, string, Resource) (any, error) {
		if loads.Add(1) == 1 {
			close(started)
			<-release
			return "old", nil
		}
		return "saved", nil
	})
	old := make(chan Snapshot, 1)
	go func() { value, _ := service.Get(context.Background(), "a", Config, false); old <- value }()
	<-started
	hub.Notify("a", true, driveevents.DriveMetadataChanged)
	saved := make(chan Snapshot, 1)
	go func() { value, _ := service.Get(context.Background(), "a", Config, true); saved <- value }()
	close(release)
	previous, current := <-old, <-saved
	if string(current.Data) != `"saved"` || string(previous.Data) != `"saved"` || current.Revision != previous.Revision {
		t.Fatalf("old=%+v saved=%+v", previous, current)
	}
}

func TestStreamKeepsRuntimeIndependentOfSlowStorageAndResynchronizes(t *testing.T) {
	hub := &driveevents.Hub{}
	var progress atomic.Int32
	service := New(hub, func(ctx context.Context, _ string, resource Resource) (any, error) {
		if resource == Storage {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return progress.Load(), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan Snapshot, 20)
	done := make(chan error, 1)
	go func() { done <- service.Stream(ctx, "a", func(value Snapshot) error { out <- value; return nil }) }()
	defer func() { cancel(); <-done }()
	initial := waitSnapshot(t, out, Runtime)
	progress.Store(5)
	hub.Notify("a", true, driveevents.ActivityChanged)
	updated := waitSnapshot(t, out, Runtime)
	if updated.Revision <= initial.Revision || string(updated.Data) != "5" {
		t.Fatalf("update=%+v", updated)
	}
	// A separate connection receives the newest snapshot even with no new signal.
	otherCtx, otherCancel := context.WithCancel(context.Background())
	otherOut := make(chan Snapshot, 20)
	otherDone := make(chan error, 1)
	go func() {
		otherDone <- service.Stream(otherCtx, "a", func(value Snapshot) error { otherOut <- value; return nil })
	}()
	value := waitSnapshot(t, otherOut, Runtime)
	otherCancel()
	<-otherDone
	if value.Revision != updated.Revision {
		t.Fatalf("reconnect=%+v", value)
	}
}

func TestContinuousProgressDoesNotExtendTheCoalescingDeadline(t *testing.T) {
	hub := &driveevents.Hub{}
	var progress atomic.Int32
	service := New(hub, func(context.Context, string, Resource) (any, error) { return progress.Load(), nil })
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan Snapshot, 100)
	done := make(chan error, 1)
	go func() { done <- service.Stream(ctx, "a", func(value Snapshot) error { out <- value; return nil }) }()
	defer func() { cancel(); <-done }()
	waitSnapshot(t, out, Runtime)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	start := time.Now()
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	for {
		select {
		case <-ticker.C:
			progress.Add(1)
			hub.Notify("a", false, driveevents.ActivityChanged)
		case value := <-out:
			if value.Resource == Runtime && string(value.Data) != "0" {
				if time.Since(start) > time.Second {
					t.Fatal("progress notification was starved")
				}
				return
			}
		case <-timeout.C:
			t.Fatal("continuous changes never published")
		}
	}
}

func TestFailuresAreResourceScopedAndNewProcessesHaveNewEpochs(t *testing.T) {
	loader := func(_ context.Context, _ string, resource Resource) (any, error) {
		if resource == Storage {
			return nil, errors.New("disk failed")
		}
		return map[string]int{"count": 1}, nil
	}
	one, two := New(&driveevents.Hub{}, loader), New(&driveevents.Hub{}, loader)
	storage, _ := one.Get(context.Background(), "a", Storage, false)
	stats, _ := one.Get(context.Background(), "a", Stats, false)
	restarted, _ := two.Get(context.Background(), "a", Stats, false)
	if storage.Error == "" || stats.Error != "" || stats.Epoch == restarted.Epoch {
		t.Fatalf("storage=%+v stats=%+v", storage, stats)
	}
	var payload map[string]int
	if err := json.Unmarshal(stats.Data, &payload); err != nil || payload["count"] != 1 {
		t.Fatal("stats missing")
	}
}

func TestDriveEventsInvalidateOnlyTheirDependentResources(t *testing.T) {
	hub := &driveevents.Hub{}
	var loads [4]atomic.Int32
	service := New(hub, func(_ context.Context, _ string, resource Resource) (any, error) {
		for index, candidate := range Resources {
			if candidate == resource {
				return loads[index].Add(1), nil
			}
		}
		return nil, errors.New("unknown resource")
	})
	ctx := context.Background()
	for _, resource := range Resources {
		if _, err := service.Get(ctx, "a", resource, false); err != nil {
			t.Fatal(err)
		}
	}
	hub.Notify("a", false, driveevents.GenerationChanged)
	for index, resource := range Resources {
		service.Get(ctx, "a", resource, false)
		want := int32(1)
		if resource == Stats {
			want = 2
		}
		if loads[index].Load() != want {
			t.Fatalf("%s: loads=%d want=%d", resource, loads[index].Load(), want)
		}
	}
	hub.Notify("a", true, driveevents.DriveChanged)
	for index, resource := range Resources {
		before := loads[index].Load()
		service.Get(ctx, "a", resource, false)
		if loads[index].Load() != before+1 {
			t.Fatalf("drive recreation did not invalidate %s", resource)
		}
	}
}

func TestConnectionsSharePeriodicReconciliationAndReleaseTheirWatch(t *testing.T) {
	var loads atomic.Int32
	service := New(&driveevents.Hub{}, func(_ context.Context, _ string, resource Resource) (any, error) {
		if resource == Runtime {
			return loads.Add(1), nil
		}
		return 0, nil
	})
	service.period = func(Resource) time.Duration { return 50 * time.Millisecond }
	first := service.subscribe("a")
	defer first.close()
	initial := waitSubscriptionSnapshot(t, first, Runtime)
	second := service.subscribe("a")
	defer second.close()
	other := waitSubscriptionSnapshot(t, second, Runtime)
	if first.watch != second.watch || other.Revision != initial.Revision || loads.Load() != 1 {
		t.Fatal("a second connection created a separate read schedule")
	}
	updated := waitSubscriptionSnapshot(t, first, Runtime)
	other = waitSubscriptionSnapshot(t, second, Runtime)
	if updated.Revision != other.Revision || string(updated.Data) != "2" || loads.Load() != 2 {
		t.Fatalf("periodic reads were not shared: first=%+v second=%+v loads=%d", updated, other, loads.Load())
	}
	second.close()
	if len(service.watches) != 1 {
		t.Fatal("closing one reader stopped the remaining reader")
	}
	first.close()
	if len(service.watches) != 0 {
		t.Fatal("an idle watch remained after its last reader closed")
	}
}

func TestSlowConnectionsReceiveLatestSnapshotsWithoutBlockingReaders(t *testing.T) {
	var loads atomic.Int32
	service := New(&driveevents.Hub{}, func(context.Context, string, Resource) (any, error) { return loads.Add(1), nil })
	slow := service.subscribe("a")
	defer slow.close()
	waitSubscriptionSnapshot(t, slow, Config)
	fast := service.subscribe("a")
	defer fast.close()
	waitSubscriptionSnapshot(t, fast, Config)
	for index := 0; index < 20; index++ {
		latest, err := service.Get(context.Background(), "a", Config, true)
		if err != nil {
			t.Fatal(err)
		}
		seen := waitSubscriptionSnapshot(t, fast, Config)
		if seen.Revision != latest.Revision {
			t.Fatal("a slow connection delayed the other connection")
		}
	}
	latest, _ := service.Get(context.Background(), "a", Config, false)
	seen := waitSubscriptionSnapshot(t, slow, Config)
	if seen.Revision != latest.Revision {
		t.Fatal("the slow connection retained an unbounded queue of older results")
	}
}

func TestNewConnectionsAwaitInvalidatedSnapshotsEvenWhenContentIsUnchanged(t *testing.T) {
	hub := &driveevents.Hub{}
	started, proceed := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(proceed) })
	defer release()
	var configReads atomic.Int32
	service := New(hub, func(ctx context.Context, _ string, resource Resource) (any, error) {
		if resource == Config && configReads.Add(1) == 2 {
			close(started)
			select {
			case <-proceed:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return "unchanged", nil
	})
	first := service.subscribe("a")
	defer first.close()
	initial := waitSubscriptionSnapshot(t, first, Config)
	hub.Notify("a", true, driveevents.DriveMetadataChanged)
	waitReadStarted(t, started)
	second := service.subscribe("a")
	defer second.close()
	if _, ok := second.take()[Config]; ok {
		t.Fatal("new reader received an invalidated snapshot")
	}
	release()
	current := waitSubscriptionSnapshot(t, second, Config)
	if current.Revision != initial.Revision || string(current.Data) != `"unchanged"` {
		t.Fatalf("new reader did not receive the revalidated unchanged content: %+v", current)
	}
}

func TestNewConnectionsIgnoreReadsThatPredateDriveRecreation(t *testing.T) {
	hub := &driveevents.Hub{}
	oldStarted, oldProceed := make(chan struct{}), make(chan struct{})
	currentStarted, currentProceed := make(chan struct{}), make(chan struct{})
	releaseOld := sync.OnceFunc(func() { close(oldProceed) })
	releaseCurrent := sync.OnceFunc(func() { close(currentProceed) })
	defer releaseOld()
	defer releaseCurrent()
	var configReads atomic.Int32
	service := New(hub, func(ctx context.Context, _ string, resource Resource) (any, error) {
		if resource == Config {
			if configReads.Add(1) == 1 {
				close(oldStarted)
				select {
				case <-oldProceed:
					return nil, &LoadError{Status: 404, Err: errors.New("previous drive missing")}
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			close(currentStarted)
			select {
			case <-currentProceed:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return "recreated", nil
	})
	first := service.subscribe("a")
	defer first.close()
	waitReadStarted(t, oldStarted)
	hub.Notify("a", true, driveevents.DriveChanged)
	second := service.subscribe("a")
	defer second.close()
	releaseOld()
	waitReadStarted(t, currentStarted)
	if _, ok := first.take()[Config]; ok {
		t.Fatal("a config read invalidated by recreation reached an existing subscriber")
	}
	if _, ok := second.take()[Config]; ok {
		t.Fatal("an older missing-drive read reached a new subscriber")
	}
	releaseCurrent()
	current := waitSubscriptionSnapshot(t, second, Config)
	if current.Error != "" || string(current.Data) != `"recreated"` {
		t.Fatalf("new subscriber did not receive the recreated drive: %+v", current)
	}
}

func waitReadStarted(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("shared read never started")
	}
}

func waitSubscriptionSnapshot(t *testing.T, sub *snapshotSubscription, resource Resource) Snapshot {
	t.Helper()
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case <-sub.wake:
			if snapshot, ok := sub.take()[resource]; ok {
				return snapshot
			}
		case <-timeout.C:
			t.Fatalf("missing shared %s snapshot", resource)
			return Snapshot{}
		}
	}
}

func waitSnapshot(t *testing.T, out <-chan Snapshot, resource Resource) Snapshot {
	t.Helper()
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case value := <-out:
			if value.Resource == resource {
				return value
			}
		case <-timeout.C:
			t.Fatalf("missing %s snapshot", resource)
			return Snapshot{}
		}
	}
}
