package telegram

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram/updates"
)

func TestRuntimeStateSurvivesRestartAndProtectsSecrets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "updates.json")
	s, err := openRuntimeState(path, cancel)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetState(ctx, 123, updates.State{Pts: 7, Qts: 2, Seq: 3, Date: 100}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetPts(ctx, 123, 9); err != nil {
		t.Fatal(err)
	}
	if err = s.SetUserAccessHash(ctx, 123, 42, 987); err != nil {
		t.Fatal(err)
	}
	if err = s.SetChannelAccessHash(ctx, 123, 12, 345); err != nil {
		t.Fatal(err)
	}
	if err = s.SetChannelPts(ctx, 123, 12, 88); err != nil {
		t.Fatal(err)
	}
	reopened, err := openRuntimeState(path, cancel)
	if err != nil {
		t.Fatal(err)
	}
	state, found, err := reopened.GetState(ctx, 123)
	if err != nil || !found || state.Pts != 9 || state.Seq != 3 {
		t.Fatalf("state: %+v %v %v", state, found, err)
	}
	if hash, ok, err := reopened.GetUserAccessHash(ctx, 123, 42); err != nil || !ok || hash != 987 {
		t.Fatal("lost peer access hash")
	}
	channels := 0
	if err := reopened.ForEachChannels(ctx, 123, func(ctx context.Context, id int64, pts int) error {
		channels++
		if id != 12 || pts != 88 {
			t.Fatal("lost channel state")
		}
		_, _, err := reopened.GetChannelAccessHash(ctx, 123, id)
		return err
	}); err != nil || channels != 1 {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("permissions %o", info.Mode().Perm())
	}
}
func TestStateWriteFailureStopsReceiverWithoutAdvancingDiskCursor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "updates.json")
	s, _ := openRuntimeState(path, cancel)
	if err := s.SetState(ctx, 123, updates.State{Pts: 4}); err != nil {
		t.Fatal(err)
	}
	// Force the atomic replacement to fail while keeping the committed file intact.
	s.path = filepath.Join(path, "missing", "updates.json")
	if err := s.SetPts(ctx, 123, 5); err == nil || ctx.Err() == nil {
		t.Fatal("persistence failure did not stop reception")
	}
	reopened, err := openRuntimeState(path, func() {})
	if err != nil {
		t.Fatal(err)
	}
	state, _, _ := reopened.GetState(context.Background(), 123)
	if state.Pts != 4 {
		t.Fatal("lost last durable cursor")
	}
	if err := s.SetPts(context.Background(), 123, 6); err == nil {
		t.Fatal("failed store resumed advancing")
	}
}
func TestAtomicSessionDoesNotOverwriteOnCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	s := &atomicSession{path: path}
	ctx := context.Background()
	if _, err := s.LoadSession(ctx); !errors.Is(err, session.ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.StoreSession(ctx, []byte("PRIVATE_KEY")); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.StoreSession(canceled, []byte("partial")); err == nil {
		t.Fatal("canceled write succeeded")
	}
	data, err := s.LoadSession(ctx)
	if err != nil || string(data) != "PRIVATE_KEY" {
		t.Fatalf("session corrupted: %s %v", data, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("session is readable by other users")
	}
}
