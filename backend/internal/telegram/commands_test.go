package telegram

import (
	"context"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/config"
)

func TestServiceRegistersCommandsWithoutBlockingReception(t *testing.T) {
	s, cat := testService(t)
	s.client = nil
	s.setState("connecting", nil)
	started := make(chan struct{})
	accepted := make(chan struct{})
	s.SetWake(func() {
		select {
		case <-accepted:
		default:
			close(accepted)
		}
	})
	s.newTransport = func(config.Telegram, string) (transport, error) {
		return &fakeTransport{
			run: func(ctx context.Context, ready func(user) error, handle func(context.Context, []update) error, _ func(bool)) error {
				if err := ready(user{ID: 123, Username: "test_bot"}); err != nil {
					return err
				}
				select {
				case <-started:
				case <-ctx.Done():
					return ctx.Err()
				}
				if err := handle(ctx, []update{videoUpdate(1, 42)}); err != nil {
					return err
				}
				<-ctx.Done()
				return ctx.Err()
			},
			commands: func(ctx context.Context) error { close(started); <-ctx.Done(); return ctx.Err() },
		}, nil
	}
	s.Start(context.Background())
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Fatal(err)
		}
	}()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("menu synchronization blocked reception")
	}
	jobs, err := cat.ListImportJobs(context.Background(), "telegram", "", 0, 30)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs: %v %v", jobs, err)
	}
}
