package telegram

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	mt "github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func recoveryFixture(t *testing.T) (*updateRecovery, *runtimeState) {
	t.Helper()
	state, err := openRuntimeState(filepath.Join(t.TempDir(), "state.json"), func() {})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SetState(context.Background(), 123, updates.State{Pts: 1, Date: 1, Seq: 1}); err != nil {
		t.Fatal(err)
	}
	r := newUpdateRecovery(state)
	r.connection(true)
	return r, state
}

func waitForRecovery(t *testing.T, r *updateRecovery) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.Synced() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("update recovery did not finish")
}

func TestRecoveryReconnectsWithoutAnUpdateGap(t *testing.T) {
	r, state := recoveryFixture(t)
	var requests atomic.Int32
	r.API = tg.NewClient(mt.InvokeFunc(func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		requests.Add(1)
		output.(*tg.UpdatesDifferenceBox).Difference = &tg.UpdatesDifferenceEmpty{Date: 1, Seq: 1}
		return nil
	}))
	manager := updates.New(updates.Config{Storage: r, AccessHasher: state, UserAccessHasher: state,
		Handler: mt.UpdateHandlerFunc(func(context.Context, tg.UpdatesClass) error { return nil })})
	ctx, cancel := context.WithCancel(context.Background())
	started, done, recoveryDone := make(chan struct{}), make(chan error, 1), make(chan error, 1)
	go func() {
		done <- manager.Run(ctx, r, 123, updates.AuthOptions{IsBot: true, OnStart: func(context.Context) { close(started) }})
	}()
	<-started
	go func() { recoveryDone <- r.run(ctx, manager.Handle) }()
	defer func() { cancel(); <-done; <-recoveryDone }()
	waitForRecovery(t, r)
	for range 2 {
		r.connection(false)
		if r.Synced() {
			t.Fatal("disconnected session is synced")
		}
		r.connection(true)
		r.request()
		// No new message and no sequence gap: reconnection alone must recover.
		waitForRecovery(t, r)
	}
	if requests.Load() != 3 {
		t.Fatalf("expected startup and two reconnect recoveries, got %d", requests.Load())
	}
}

func TestRecoveryRequiresFinalDurableCursor(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"slice", "full", "empty", "storage failure", "disconnect"} {
		t.Run(kind, func(t *testing.T) {
			r, state := recoveryFixture(t)
			final := updates.State{Pts: 5, Qts: 2, Seq: 3, Date: 4}
			r.API = tg.NewClient(mt.InvokeFunc(func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
				var diff tg.UpdatesDifferenceClass = &tg.UpdatesDifference{State: tg.UpdatesState{Pts: 5, Qts: 2, Seq: 3, Date: 4}}
				if kind == "empty" {
					diff = &tg.UpdatesDifferenceEmpty{Seq: 3, Date: 4}
				} else if kind == "slice" {
					diff = &tg.UpdatesDifferenceSlice{IntermediateState: tg.UpdatesState{Pts: 5, Qts: 2, Seq: 3, Date: 4}}
				}
				output.(*tg.UpdatesDifferenceBox).Difference = diff
				return nil
			}))
			if _, err := r.UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{}); err != nil {
				t.Fatal(err)
			}
			if r.Synced() {
				t.Fatal("RPC completion released albums before persistence")
			}
			if err := r.SetState(ctx, 123, updates.State{Pts: 2}); err != nil || r.Synced() {
				t.Fatal("unrelated cursor write released albums", err)
			}
			if kind == "storage failure" {
				state.path = filepath.Join(state.path, "missing")
			}
			if kind == "disconnect" {
				r.connection(false)
				r.connection(true)
			}
			var err error
			if kind == "empty" {
				err = r.SetDateSeq(ctx, 123, 4, 3)
			} else {
				err = r.SetState(ctx, 123, final)
			}
			if (err != nil) != (kind == "storage failure") {
				t.Fatal(err)
			}
			if r.Synced() != (kind == "full" || kind == "empty") {
				t.Fatal("incorrect recovery state after cursor write")
			}
		})
	}
}

func TestRecoveryRejectsResponseFromPreviousConnection(t *testing.T) {
	r, _ := recoveryFixture(t)
	r.API = tg.NewClient(mt.InvokeFunc(func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		r.connection(false)
		r.connection(true)
		output.(*tg.UpdatesDifferenceBox).Difference = &tg.UpdatesDifferenceEmpty{Date: 2, Seq: 2}
		return nil
	}))
	ctx := context.Background()
	if _, err := r.UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetDateSeq(ctx, 123, 2, 2); err != nil || r.Synced() {
		t.Fatal("old RPC result reopened admission", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := r.run(canceled, func(context.Context, tg.UpdatesClass) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal("recovery loop did not stop", err)
	}
}

func TestRecoveryWaitsForBacklogHandlerBeforeReleasingAlbums(t *testing.T) {
	r, state := recoveryFixture(t)
	var calls int
	r.API = tg.NewClient(mt.InvokeFunc(func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		calls++
		messages := []tg.MessageClass{&tg.Message{ID: calls, PeerID: &tg.PeerUser{UserID: 42}}}
		if calls == 1 {
			output.(*tg.UpdatesDifferenceBox).Difference = &tg.UpdatesDifferenceSlice{
				NewMessages: messages, IntermediateState: tg.UpdatesState{Pts: 2, Date: 2, Seq: 2},
			}
		} else {
			output.(*tg.UpdatesDifferenceBox).Difference = &tg.UpdatesDifference{
				NewMessages: messages, State: tg.UpdatesState{Pts: 3, Date: 3, Seq: 3},
			}
		}
		return nil
	}))
	handling, release := make(chan struct{}), make(chan struct{})
	manager := updates.New(updates.Config{Storage: r, AccessHasher: state, UserAccessHasher: state,
		Handler: mt.UpdateHandlerFunc(func(ctx context.Context, input tg.UpdatesClass) error {
			if r.Synced() {
				t.Error("albums were released while backlog was still being accepted")
			}
			if calls == 2 {
				close(handling)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-release:
				}
			}
			return nil
		})})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx, r, 123, updates.AuthOptions{IsBot: true}) }()
	defer func() { cancel(); <-done }()
	select {
	case <-handling:
	case <-time.After(2 * time.Second):
		t.Fatal("final backlog batch was not delivered")
	}
	if r.Synced() {
		t.Fatal("final RPC response released albums before handler completion")
	}
	close(release)
	waitForRecovery(t, r)
	stored, _, err := state.GetState(ctx, 123)
	if err != nil || stored.Pts != 3 {
		t.Fatalf("final cursor is not durable: %+v %v", stored, err)
	}
}

func TestRecoveryRespectsFloodWait(t *testing.T) {
	r, _ := recoveryFixture(t)
	var calls int
	r.API = tg.NewClient(mt.InvokeFunc(func(context.Context, bin.Encoder, bin.Decoder) error {
		calls++
		return &tgerr.Error{Code: 420, Type: "FLOOD_WAIT", Argument: 60}
	}))
	if _, err := r.UpdatesGetDifference(context.Background(), &tg.UpdatesGetDifferenceRequest{}); err == nil {
		t.Fatal("expected flood wait")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := r.UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 || r.Synced() {
		t.Fatalf("flood wait ignored: calls=%d error=%v", calls, err)
	}
}
