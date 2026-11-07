package catalog

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/scanjob"
)

func TestScanResultsSurviveReopenAndBelongToTheirDrive(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "catalog.db")
	cat, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cat.Close() }()
	if err := cat.UpsertDrive(ctx, &Drive{ID: "drive", Kind: "fake", Name: "Drive"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, state := range []scanjob.State{scanjob.Failed, scanjob.Succeeded} {
		if err := cat.SaveScanResult(ctx, scanjob.Result{DriveID: "drive", State: state, StartedAt: now, FinishedAt: now, AddedCount: 3, CleanedCount: 2}); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	cat, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	results, err := cat.LatestScanResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results["drive"].State != scanjob.Succeeded || results["drive"].AddedCount != 3 || results["drive"].CleanedCount != 2 {
		t.Fatalf("results = %+v", results)
	}
	if err := cat.DeleteDrive(ctx, "drive"); err != nil {
		t.Fatal(err)
	}
	// A late completion cannot recreate state after deletion.
	if err := cat.SaveScanResult(ctx, scanjob.Result{DriveID: "drive", State: scanjob.Canceled}); err != nil {
		t.Fatal(err)
	}
	results, err = cat.LatestScanResults(ctx)
	if err != nil || len(results) != 0 {
		t.Fatalf("results after deletion = %+v, err = %v", results, err)
	}
}
