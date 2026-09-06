package db

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// Backoff bounds for transaction retries.
const (
	retryBaseDelay = 5 * time.Millisecond
	retryMaxDelay  = 500 * time.Millisecond
)

// ErrRetriesExhausted wraps the last retryable error once MaxRetries is spent.
// errors.Is(err, ErrRetriesExhausted) and IsSerializationFailure(err) /
// IsDeadlock(err) all hold on the returned error.
var ErrRetriesExhausted = errors.New("db: transaction retries exhausted")

// shouldRetry is the whole retry policy. attempt is zero-based (the index of
// the attempt that just failed); maxRetries is the number of *additional*
// attempts allowed after the first. Only serialization failures and deadlocks
// are ever retried; any other error, including one that wraps a context
// cancellation, is final.
func shouldRetry(err error, attempt, maxRetries int) bool {
	if err == nil || attempt < 0 || attempt >= maxRetries {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return IsRetryable(err)
}

// backoffDelay returns the sleep before retry number attempt+1: exponential in
// attempt (base 5ms, doubling, capped at 500ms) with jitter in [d/2, d) where
// r is a uniform sample in [0,1).
func backoffDelay(attempt int, r float64) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 16 {
		attempt = 16
	}
	d := retryBaseDelay << uint(attempt)
	if d > retryMaxDelay || d <= 0 {
		d = retryMaxDelay
	}
	if r < 0 {
		r = 0
	}
	if r >= 1 {
		r = 0.999999
	}
	half := d / 2
	return half + time.Duration(float64(half)*r)
}

// sleepFunc lets tests observe and skip the backoff.
type sleepFunc func(ctx context.Context, d time.Duration) error

func sleepWithContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// runWithRetry executes op until it succeeds, returns a non-retryable error,
// or maxRetries additional attempts have been consumed.
func runWithRetry(ctx context.Context, maxRetries int, sleep sleepFunc, op func(ctx context.Context) error) error {
	if maxRetries < 0 {
		maxRetries = 0
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := op(ctx)
		if err == nil {
			return nil
		}
		if !shouldRetry(err, attempt, maxRetries) {
			if maxRetries > 0 && attempt >= maxRetries && IsRetryable(err) {
				return fmt.Errorf("%w after %d attempts: %w", ErrRetriesExhausted, attempt+1, err)
			}
			return err
		}
		if serr := sleep(ctx, backoffDelay(attempt, rand.Float64())); serr != nil { //nolint:gosec // G404: backoff jitter is not security-sensitive
			return fmt.Errorf("%w (last error: %w)", serr, err)
		}
	}
}
