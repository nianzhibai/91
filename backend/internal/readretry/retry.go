// Package readretry provides bounded retries for explicitly read-only operations.
// Callers own the operation deadline and must not stack this policy with another
// transport retry loop. Provider rate limits belong to the provider/task policy.
package readretry

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"syscall"
	"time"
)

const MaxRetries = 2

type Options struct {
	// Wait may replace the interruptible timer in tests.
	Wait    func(context.Context, time.Duration) error
	OnRetry func(retry int, delay time.Duration, err error)
}

// Transient recognizes transport failures, not provider/business error text.
// A request-local deadline is retryable only while the caller's context lives.
func Transient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var networkErr net.Error
	return errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &networkErr) && networkErr.Timeout()) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

// Delay takes a one-based retry number: 1s then 3s, plus up to 25% jitter.
func Delay(retry int) time.Duration {
	base := time.Second
	if retry > 1 {
		base = 3 * time.Second
	}
	return base + time.Duration(rand.Int64N(int64(base/4)))
}

func Wait(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

func Do[T any](ctx context.Context, read func() (T, error), options Options) (T, error) {
	var zero T
	wait := options.Wait
	if wait == nil {
		wait = Wait
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		value, err := read()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return zero, ctxErr
		}
		if !Transient(err) || attempt == MaxRetries {
			return value, err
		}
		retry := attempt + 1
		delay := Delay(retry)
		if options.OnRetry != nil {
			options.OnRetry(retry, delay, err)
		}
		if err := wait(ctx, delay); err != nil {
			return zero, err
		}
	}
}
