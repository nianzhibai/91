package readretry

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"syscall"
	"testing"
	"time"
)

func TestTransientClassification(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{"success", nil, false},
		{"wrapped timeout", &url.Error{Op: "Get", URL: "https://example.test", Err: &net.DNSError{IsTimeout: true}}, true},
		{"request deadline", context.DeadlineExceeded, true},
		{"connection reset", fmt.Errorf("read: %w", syscall.ECONNRESET), true},
		{"broken pipe", syscall.EPIPE, true},
		{"EOF", io.EOF, true},
		{"truncated response", io.ErrUnexpectedEOF, true},
		{"canceled", context.Canceled, false},
		{"certificate", x509.UnknownAuthorityError{}, false},
		{"authentication", errors.New("invalid credentials"), false},
		{"timeout text only", errors.New("TLS handshake timeout"), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Transient(tt.err); got != tt.want {
				t.Fatalf("Transient(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestReadRetriesRecoverOrStopAtBudget(t *testing.T) {
	for _, failures := range []int{0, 1, 2, 3} {
		t.Run(fmt.Sprint(failures), func(t *testing.T) {
			calls, waits := 0, 0
			value, err := Do(context.Background(), func() (string, error) {
				calls++
				if calls <= failures {
					return "", io.ErrUnexpectedEOF
				}
				return "value", nil
			}, Options{Wait: func(ctx context.Context, delay time.Duration) error {
				waits++
				base := time.Second
				if waits == 2 {
					base = 3 * time.Second
				}
				if delay < base || delay >= base+base/4 {
					t.Fatalf("retry %d delay = %s", waits, delay)
				}
				return ctx.Err()
			}})
			wantCalls := min(failures+1, 3)
			if calls != wantCalls || waits != wantCalls-1 {
				t.Fatalf("calls/waits = %d/%d, want %d/%d", calls, waits, wantCalls, wantCalls-1)
			}
			if failures < 3 && (err != nil || value != "value") {
				t.Fatalf("result = %q, %v", value, err)
			}
			if failures == 3 && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("final error = %v", err)
			}
		})
	}
}

func TestReadStopsOnPermanentErrorOrCancellation(t *testing.T) {
	for _, mode := range []string{"permanent", "already canceled", "during request", "during backoff", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "already canceled" {
				cancel()
			}
			if mode == "deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 20*time.Millisecond)
				defer stop()
			}
			calls := 0
			permanent := errors.New("access denied")
			_, err := Do(ctx, func() (int, error) {
				calls++
				if mode == "permanent" {
					return 0, permanent
				}
				if mode == "during request" {
					cancel()
				}
				return 0, io.EOF
			}, Options{OnRetry: func(int, time.Duration, error) {
				if mode == "during backoff" {
					cancel()
				}
			}})
			want := error(context.Canceled)
			if mode == "permanent" {
				want = permanent
			}
			if mode == "deadline" {
				want = context.DeadlineExceeded
			}
			wantCalls := 1
			if mode == "already canceled" {
				wantCalls = 0
			}
			if calls != wantCalls || !errors.Is(err, want) {
				t.Fatalf("calls=%d error=%v, want %d / %v", calls, err, wantCalls, want)
			}
		})
	}
}
