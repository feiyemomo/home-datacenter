package utils

import (
	"math/rand"
	"time"
)

// Retry runs fn up to maxAttempts times (>=1) with exponential backoff
// and jitter, starting at baseDelay and doubling up to maxDelay. If
// isRetryable is non-nil, a failure that returns false aborts retrying
// immediately. It sleeps between attempts (never on the last). Returns
// nil on success, or fn's last error.
//
// This centralizes the backoff policy that used to be hand-rolled in
// two places (automation engine + main.pushRetentionWithRetry) so the
// behaviour stays consistent.
func Retry(maxAttempts int, baseDelay, maxDelay time.Duration, isRetryable func(error) bool, fn func() error) error {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	delay := baseDelay
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if isRetryable != nil && !isRetryable(lastErr) {
			return lastErr
		}
		if attempt == maxAttempts-1 {
			break
		}
		time.Sleep(delay + jitter(delay))
		if delay < maxDelay {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
	return lastErr
}

// jitter returns ±20% of d to decorrelate retries across instances.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	frac := time.Duration(rand.Int63n(int64(d)/5 + 1))
	if rand.Intn(2) == 0 {
		return -frac
	}
	return frac
}