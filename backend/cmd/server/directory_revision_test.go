package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/api"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/driveevents"
)

func TestDirectoryRevisionFollowsCompletedRuntimeUpdates(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			name := "immediate"
			if deferred {
				name = "deferred"
			}
			if fail {
				name += "/failed_attach"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				cat, err := catalog.Open(t.TempDir() + "/catalog.db")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { cat.Close() })
				if err := cat.UpsertDrive(ctx, &catalog.Drive{ID: "drive", Kind: "quark", RootID: "0"}); err != nil {
					t.Fatal(err)
				}
				app := &App{cat: cat}
				sub := cat.DriveEvents().Subscribe("drive")
				defer sub.Close()
				version := func() uint64 { return cat.DriveEvents().Version("drive", driveevents.DriveRuntimeChanged) }
				done := func() {}
				if deferred {
					var admitted bool
					_, done, admitted = app.registerDriveTaskContext(ctx, "drive", 0)
					if !admitted {
						t.Fatal("initial task was rejected")
					}
					defer done()
				}
				lease, reason := app.beginDriveConfigUpdate("drive")
				if lease == nil || reason != "" {
					t.Fatalf("begin update: %s", reason)
				}
				defer lease.Release()
				if reason := lease.Authorize(api.DriveConfigUpdateRuntime); reason != "" {
					t.Fatal(reason)
				}
				attachError := errors.New("invalid replacement credentials")
				wasDeferred, err := lease.Commit(api.DriveConfigUpdateRuntime, func() error {
					if version() != 0 {
						t.Error("directory revision changed before runtime application finished")
					}
					if fail {
						return attachError
					}
					return nil
				})
				var wantErr error
				if fail && !deferred {
					wantErr = attachError
				}
				if wasDeferred != deferred || !errors.Is(err, wantErr) {
					t.Fatalf("commit deferred/error = %v/%v", wasDeferred, err)
				}
				if version() != 0 {
					t.Fatal("directory revision changed while task admission was blocked")
				}
				lease.Release()
				if deferred && version() != 0 {
					t.Fatal("pending credentials invalidated directory reads before tasks exited")
				}
				done()
				select {
				case <-sub.Wake:
				case <-time.After(2 * time.Second):
					t.Fatal("completed runtime update did not publish directory invalidation")
				}
				if !sub.Take().Kinds[driveevents.DriveRuntimeChanged] || version() == 0 {
					t.Fatal("missing directory invalidation")
				}
				_, release, admitted := app.registerDriveTaskContext(ctx, "drive", 0)
				if !admitted {
					t.Fatal("directory invalidation was published before reads could start")
				}
				release()
			})
		}
	}
}

func TestDirectoryRevisionIgnoresScanPolicyUpdates(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	app := &App{cat: cat}
	lease, reason := app.beginDriveConfigUpdate("drive")
	if lease == nil || reason != "" {
		t.Fatalf("begin scan policy update: %s", reason)
	}
	defer lease.Release()
	if reason := lease.Authorize(api.DriveConfigUpdateScan); reason != "" {
		t.Fatal(reason)
	}
	if _, err := lease.Commit(api.DriveConfigUpdateScan, nil); err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if cat.DriveEvents().Version("drive", driveevents.DriveRuntimeChanged) != 0 {
		t.Fatal("scan policy update invalidated directory reads")
	}
}
