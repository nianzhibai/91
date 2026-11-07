package nightly

import (
	"context"
	"errors"
	"testing"

	"github.com/nianzhibai/91/backend/internal/scanjob"
)

func TestScanAllRetainsEachOutcomeAndContinuesAfterFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		states []scanjob.State
		want   scanjob.State
	}{
		{"success", []scanjob.State{scanjob.Succeeded}, scanjob.Succeeded},
		{"failure", []scanjob.State{scanjob.Failed}, scanjob.Failed},
		{"partial directory failure", []scanjob.State{scanjob.Partial}, scanjob.Partial},
		{"continues after failed drive", []scanjob.State{scanjob.Failed, scanjob.Succeeded}, scanjob.Partial},
		{"individual drive canceled", []scanjob.State{scanjob.Canceled}, scanjob.Canceled},
		{"skipped", []scanjob.State{scanjob.Skipped}, scanjob.Skipped},
		{"one skipped", []scanjob.State{scanjob.Skipped, scanjob.Succeeded}, scanjob.Partial},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids := []string{}
			states := make(map[string]scanjob.State)
			for i := range tc.states {
				id := string(rune('a' + i))
				ids = append(ids, id)
				states[id] = tc.states[i]
			}
			r := New(Config{
				Settings:        newStubSettings(),
				ListScanTargets: func(context.Context) ([]string, error) { return ids, nil },
				RunScan: func(_ context.Context, id string) scanjob.Result {
					return scanjob.Result{DriveID: id, State: states[id], ScannedCount: 7}
				},
			})
			if !r.TriggerScanAll() {
				t.Fatal("manual scan was not accepted")
			}
			<-r.trigger
			r.runModeLocked(context.Background(), runModeScanAll)
			status := r.Status()
			if status.Running || status.Queued || status.Outcome != tc.want || len(status.ScanResults) != len(tc.states) {
				t.Fatalf("status = %+v, want outcome %s", status, tc.want)
			}
			for _, result := range status.ScanResults {
				if state, ok := states[result.DriveID]; !ok || result.State != state || result.ScannedCount != 7 {
					t.Fatalf("lost result: %+v", result)
				}
				delete(states, result.DriveID)
			}
		})
	}
}

func TestScanAllSurfacesTargetListingAndLaterPhaseErrors(t *testing.T) {
	for _, stage := range []string{"list", "wait", "reconcile", "dedupe"} {
		t.Run(stage, func(t *testing.T) {
			boom := errors.New("test phase failure")
			r := New(Config{
				Settings: newStubSettings(),
				ListScanTargets: func(context.Context) ([]string, error) {
					if stage == "list" {
						return nil, boom
					}
					return []string{"drive"}, nil
				},
				RunScan: func(context.Context, string) scanjob.Result { return scanjob.Result{State: scanjob.Succeeded} },
				WaitPreviewQueuesIdle: func(context.Context) error {
					if stage == "wait" {
						return boom
					}
					return nil
				},
				RunLocalAssetReconciliation: func(context.Context) (int, error) {
					if stage == "reconcile" {
						return 0, boom
					}
					return 0, nil
				},
				RunDedupeAssetCleanup: func(context.Context) error {
					if stage == "dedupe" {
						return boom
					}
					return nil
				},
			})
			if !r.TriggerScanAll() {
				t.Fatal("manual scan was not accepted")
			}
			<-r.trigger
			r.runModeLocked(context.Background(), runModeScanAll)
			status := r.Status()
			if status.Outcome != scanjob.Failed && status.Outcome != scanjob.Partial {
				t.Fatalf("error masked: %+v", status)
			}
			if len(status.Issues) != 1 || status.Issues[0].Message != boom.Error() {
				t.Fatalf("issues = %+v", status.Issues)
			}
		})
	}
}

func TestScanAllReportsCancellationAndResetsPreviousOutcomeOnNextRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := New(Config{
		Settings:        newStubSettings(),
		ListScanTargets: func(context.Context) ([]string, error) { return []string{"drive"}, nil },
		RunScan: func(context.Context, string) scanjob.Result {
			cancel()
			return scanjob.Result{DriveID: "drive", State: scanjob.Canceled}
		},
	})
	if !r.TriggerScanAll() {
		t.Fatal("manual scan was not accepted")
	}
	<-r.trigger
	r.runModeLocked(ctx, runModeScanAll)
	if r.Status().Outcome != scanjob.Canceled {
		t.Fatalf("status = %+v", r.Status())
	}
	r.cfg.RunScan = func(context.Context, string) scanjob.Result {
		return scanjob.Result{DriveID: "drive", State: scanjob.Succeeded}
	}
	if !r.TriggerScanAll() {
		t.Fatal("manual scan was not accepted")
	}
	<-r.trigger
	r.runModeLocked(context.Background(), runModeScanAll)
	status := r.Status()
	if status.Outcome != scanjob.Succeeded || len(status.ScanResults) != 1 {
		t.Fatalf("stale outcome retained: %+v", status)
	}
}
