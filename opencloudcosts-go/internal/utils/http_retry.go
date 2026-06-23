// Package utils provides shared utilities for cloud pricing providers.
// This file ports http_retry.py: exponential-backoff HTTP retry with context
// cancellation support and 4xx-permanent-failure semantics.
//
// Design:
//   - Retryable:   HTTP 429, 500, 502, 503, 504 and network errors (connection
//     refused, timeout, EOF).
//   - Not retried: Any other HTTP 4xx (permanent failures).
//   - Max attempts: 3 (matching tenacity stop_after_attempt(3)).
//   - Back-off:    1s, 2s, 4s … capped at 30s (matching wait_exponential).
//   - Context:     If ctx is cancelled or deadline exceeds between attempts, the
//     context error is returned immediately.
package utils

import (
	"context"
	"math"
	"net/http"
	"time"
)

// transientHTTPCodes are server-side or rate-limit codes that may resolve on retry.
var transientHTTPCodes = map[int]bool{
	http.StatusTooManyRequests:     true, // 429
	http.StatusInternalServerError: true, // 500
	http.StatusBadGateway:          true, // 502
	http.StatusServiceUnavailable:  true, // 503
	http.StatusGatewayTimeout:      true, // 504
}

// MaxRetryAttempts is the maximum number of attempts (initial + retries).
const MaxRetryAttempts = 3

// RetryBaseDelay is the initial back-off interval.
const RetryBaseDelay = time.Second

// RetryMaxDelay caps the exponential back-off.
const RetryMaxDelay = 30 * time.Second

// HTTPStatusError is the error type produced when an HTTP response has a
// non-2xx status that should be communicated to the caller.
type HTTPStatusError struct {
	StatusCode int
	Message    string
}

func (e *HTTPStatusError) Error() string {
	return e.Message
}

// IsTransient reports whether err represents a condition that may resolve on retry.
// Mirrors _is_transient() in http_retry.py:
//   - HTTPStatusError with a code in {429, 500, 502, 503, 504}
//   - Any other error (network error: timeout, connection refused, EOF, etc.)
//     is treated as transient UNLESS it is an HTTPStatusError with a non-retryable
//     status code (i.e. any 4xx not in transientHTTPCodes).
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if se, ok := err.(*HTTPStatusError); ok {
		return transientHTTPCodes[se.StatusCode]
	}
	// Non-HTTP errors (network, timeout, EOF) are transient.
	return true
}

// DoWithRetry calls fn up to MaxRetryAttempts times, retrying on transient errors.
// It respects ctx cancellation between attempts. On permanent failure (non-retryable
// status code) or context cancellation it returns immediately.
//
// Usage:
//
//	err := DoWithRetry(ctx, func(ctx context.Context) error {
//	    resp, err := http.Get(url)
//	    if err != nil { return err }
//	    if resp.StatusCode >= 400 {
//	        return &utils.HTTPStatusError{StatusCode: resp.StatusCode}
//	    }
//	    return nil
//	})
func DoWithRetry(ctx context.Context, fn func(ctx context.Context) error) error {
	var lastErr error
	for attempt := 0; attempt < MaxRetryAttempts; attempt++ {
		// Check context before each attempt.
		if err := ctx.Err(); err != nil {
			return err
		}

		lastErr = fn(ctx)
		if lastErr == nil {
			return nil
		}

		if !IsTransient(lastErr) {
			return lastErr // permanent failure — don't retry
		}

		// Don't sleep after the last attempt.
		if attempt+1 < MaxRetryAttempts {
			delay := backoffDelay(attempt)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return lastErr
}

// backoffDelay returns the wait duration before the (attempt+1)th retry.
// Implements exponential back-off: base * 2^attempt, capped at RetryMaxDelay.
// attempt=0 → 1s, attempt=1 → 2s, attempt=2 → 4s, …
func backoffDelay(attempt int) time.Duration {
	d := float64(RetryBaseDelay) * math.Pow(2, float64(attempt))
	if d > float64(RetryMaxDelay) {
		d = float64(RetryMaxDelay)
	}
	return time.Duration(d)
}
