package nightly

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/scanjob"
)

func TestMaintenanceScansDrivesConcurrentlyBeforeLaterPhases(t *testing.T) {
	for _, mode := range []runMode{runModeScheduled, runModeScanAll} {
		name := "scheduled"
		if mode == runModeScanAll {
			name = "scan-all"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ids := []string{"drive-a", "drive-b", "drive-c"}
			releases := make(map[string]chan struct{})
			for _, id := range ids {
				releases[id] = make(chan struct{})
			}
			started := make(chan string, len(ids))
			waitStarted := make(chan struct{}, 2)
			generationDone := make(chan struct{})
			var completed atomic.Int32
			rec := &recorder{}
			r := New(Config{
				Settings:        newStubSettings(),
				ListScanTargets: func(context.Context) ([]string, error) { return ids, nil },
				RunScan: func(ctx context.Context, id string) scanjob.Result {
					started <- id
					result := scanjob.Result{DriveID: id, State: scanjob.Succeeded, ScannedCount: 7}
					select {
					case <-releases[id]:
						if id == "drive-b" {
							result.State = scanjob.Failed
							result.ErrorCount = 1
							result.Issues = []scanjob.Issue{{Stage: "scan", Message: "provider unavailable"}}
						}
					case <-ctx.Done():
						result.State = scanjob.Canceled
					}
					completed.Add(1)
					return result
				},
				WaitPreviewQueuesIdle: func(ctx context.Context) error {
					if got := completed.Load(); got != int32(len(ids)) {
						t.Errorf("queue wait started after %d scans, want %d", got, len(ids))
					}
					waitStarted <- struct{}{}
					select {
					case <-generationDone:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				},
				RunLocalAssetReconciliation: func(context.Context) (int, error) {
					rec.push("reconcile")
					return 0, nil
				},
				ListCrawlerDrives:     func(context.Context) []string { return []string{"crawler"} },
				RunCrawlerCrawl:       func(context.Context, string) error { rec.push("crawl"); return nil },
				RunDedupeAssetCleanup: func(context.Context) error { rec.push("dedupe"); return nil },
			})
			if mode == runModeScanAll {
				if !r.TriggerScanAll() {
					t.Fatal("manual scan was not accepted")
				}
				<-r.trigger
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				r.runModeLocked(ctx, mode)
			}()
			t.Cleanup(func() { cancel(); <-done })

			// No drive can finish until every drive has entered its scan callback.
			seen := make(map[string]bool)
			for range ids {
				select {
				case id := <-started:
					if seen[id] {
						t.Fatalf("duplicate scan for %s", id)
					}
					seen[id] = true
				case <-ctx.Done():
					t.Fatalf("drives did not start concurrently: %v", seen)
				}
			}

			// A failed drive reports immediately while slower drives keep running.
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for i, id := range []string{"drive-b", "drive-c", "drive-a"} {
				close(releases[id])
				for len(r.Status().ScanResults) < i+1 {
					select {
					case <-ticker.C:
					case <-ctx.Done():
						t.Fatalf("completed scan %s was not reported", id)
					}
				}
				status := r.Status()
				result := status.ScanResults[i]
				if !status.Running || result.DriveID != id || result.ScannedCount != 7 {
					t.Fatalf("unexpected progress after %s: %+v", id, status)
				}
				if id == "drive-b" {
					if result.State != scanjob.Failed || result.ErrorCount != 1 || len(result.Issues) != 1 {
						t.Fatalf("failed scan details lost: %+v", result)
					}
				} else if result.State != scanjob.Succeeded {
					t.Fatalf("another drive's failure interrupted %s: %+v", id, result)
				}
			}

			select {
			case <-waitStarted:
			case <-ctx.Done():
				t.Fatal("generation wait did not start after all scans finished")
			}
			if calls := rec.snapshot(); len(calls) != 0 {
				t.Fatalf("later phases ran before generation completed: %v", calls)
			}
			close(generationDone)
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("pipeline did not finish")
			}
			status := r.Status()
			if status.Running || status.Outcome != scanjob.Partial || len(status.ScanResults) != len(ids) {
				t.Fatalf("final status = %+v", status)
			}
			want := []string{"reconcile", "dedupe"}
			if mode == runModeScheduled {
				want = []string{"reconcile", "crawl", "dedupe"}
			}
			if calls := rec.snapshot(); !reflect.DeepEqual(calls, want) {
				t.Fatalf("later phases = %v, want %v", calls, want)
			}
		})
	}
}

func TestStopMaintenanceWaitsForEveryScanToExit(t *testing.T) {
	for _, mode := range []runMode{runModeScheduled, runModeScanAll} {
		name := "scheduled"
		if mode == runModeScanAll {
			name = "scan-all"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ids := []string{"drive-a", "drive-b", "drive-c"}
			started := make(chan struct{}, len(ids))
			canceled := make(chan struct{}, len(ids))
			releaseCleanup := make(chan struct{})
			rec := &recorder{}
			r := New(Config{
				Settings:        newStubSettings(),
				ListScanTargets: func(context.Context) ([]string, error) { return ids, nil },
				RunScan: func(ctx context.Context, id string) scanjob.Result {
					started <- struct{}{}
					<-ctx.Done()
					canceled <- struct{}{}
					// Model result persistence and task release after cancellation.
					<-releaseCleanup
					return scanjob.Result{DriveID: id, State: scanjob.Canceled}
				},
				WaitPreviewQueuesIdle: func(context.Context) error { rec.push("wait"); return nil },
				RunLocalAssetReconciliation: func(context.Context) (int, error) {
					rec.push("reconcile")
					return 0, nil
				},
				ListCrawlerDrives: func(context.Context) []string { rec.push("crawlers"); return nil },
				RunDedupeAssetCleanup: func(context.Context) error {
					rec.push("dedupe")
					return nil
				},
			})
			if mode == runModeScanAll {
				if !r.TriggerScanAll() {
					t.Fatal("manual scan was not accepted")
				}
				<-r.trigger
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				r.runModeLocked(ctx, mode)
			}()
			t.Cleanup(func() { cancel(); <-done })
			// Release blocked cleanup even if an assertion fails before StopCurrent.
			var released bool
			t.Cleanup(func() {
				if !released {
					close(releaseCleanup)
				}
			})
			for range ids {
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("not all scans started")
				}
			}
			if !r.StopCurrent() {
				t.Fatal("running pipeline was not stopped")
			}
			for range ids {
				select {
				case <-canceled:
				case <-ctx.Done():
					t.Fatal("stop did not cancel every scan")
				}
			}
			if !r.Status().Running || r.TriggerScanAll() {
				t.Fatal("pipeline ownership was released before scans finished cleanup")
			}
			close(releaseCleanup)
			released = true
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("pipeline did not finish after canceled scans exited")
			}
			status := r.Status()
			if status.Running || status.Outcome != scanjob.Canceled || len(status.ScanResults) != len(ids) {
				t.Fatalf("canceled results were lost: %+v", status)
			}
			seen := make(map[string]bool)
			for _, result := range status.ScanResults {
				if result.State != scanjob.Canceled || seen[result.DriveID] {
					t.Fatalf("unexpected canceled result: %+v", result)
				}
				seen[result.DriveID] = true
			}
			if calls := rec.snapshot(); len(calls) != 0 {
				t.Fatalf("later phases ran after cancellation: %v", calls)
			}
		})
	}
}
