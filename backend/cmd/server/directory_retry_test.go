package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
	"github.com/video-site/backend/internal/proxy"
)

type directoryRetryDrive struct {
	serverFakeDrive
	read func(context.Context, string) ([]drives.Entry, error)
}

func (d *directoryRetryDrive) List(ctx context.Context, parent string) ([]drives.Entry, error) {
	return d.read(ctx, parent)
}

type directoryRetryFastDrive struct{ *directoryRetryDrive }

func (d *directoryRetryFastDrive) List(context.Context, string) ([]drives.Entry, error) {
	return nil, errors.New("directory browser must use fast path")
}

func (d *directoryRetryFastDrive) ListDirsOnly(ctx context.Context, parent string) ([]drives.Entry, error) {
	return d.read(ctx, parent)
}

func TestDirectoryBrowserRetriesReads(t *testing.T) {
	for _, fast := range []bool{false, true} {
		for _, outcome := range []string{"recovery", "exhausted", "permanent", "canceled"} {
			name := "list/" + outcome
			if fast {
				name = "fast/" + outcome
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				cat, err := catalog.Open(t.TempDir() + "/catalog.db")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = cat.Close() })
				if err := cat.UpsertDrive(ctx, &catalog.Drive{ID: "drive-id", Kind: "fake", Name: "Test", Status: "ok"}); err != nil {
					t.Fatal(err)
				}
				calls := 0
				var times []time.Time
				permanent := errors.New("access denied")
				drv := &directoryRetryDrive{read: func(ctx context.Context, parent string) ([]drives.Entry, error) {
					calls++
					times = append(times, time.Now())
					if parent != "folder" {
						t.Errorf("parent = %s", parent)
					}
					if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 45*time.Second {
						t.Error("missing bounded browse deadline")
					}
					if outcome == "permanent" {
						return nil, permanent
					}
					if outcome == "canceled" {
						cancel()
					}
					if calls == 1 || outcome == "exhausted" {
						return nil, io.ErrUnexpectedEOF
					}
					return []drives.Entry{{ID: "child", Name: "Child", IsDir: true}, {ID: "file", Name: "clip.mp4"}}, nil
				}}
				registry := proxy.NewRegistry()
				if fast {
					registry.Set("drive-id", &directoryRetryFastDrive{drv})
				} else {
					registry.Set("drive-id", drv)
				}
				app := &App{cat: cat, registry: registry}
				children, err := app.listDriveDirChildren(ctx, "drive-id", "folder")
				wantCalls := 1
				switch outcome {
				case "recovery":
					wantCalls = 2
					if err != nil || len(children) != 1 || children[0].ID != "child" {
						t.Fatalf("children=%v error=%v", children, err)
					}
				case "exhausted":
					wantCalls = 3
					if !errors.Is(err, io.ErrUnexpectedEOF) {
						t.Fatalf("error=%v", err)
					}
				case "permanent":
					if !errors.Is(err, permanent) {
						t.Fatalf("error=%v", err)
					}
				case "canceled":
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("error=%v", err)
					}
				}
				if calls != wantCalls {
					t.Fatalf("calls=%d want %d", calls, wantCalls)
				}
				for i := 1; i < len(times); i++ {
					minimum := time.Second
					if i == 2 {
						minimum = 3 * time.Second
					}
					if times[i].Sub(times[i-1]) < minimum {
						t.Errorf("retry %d had no backoff", i)
					}
				}
				stored, err := cat.GetDrive(context.Background(), "drive-id")
				if err != nil {
					t.Fatal(err)
				}
				if (outcome == "recovery" || outcome == "canceled") && stored.Status != "ok" {
					t.Fatalf("drive status = %s", stored.Status)
				}
			})
		}
	}
}
