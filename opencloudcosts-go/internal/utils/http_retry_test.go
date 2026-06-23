package utils

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// httpErr creates an HTTPStatusError for the given status code.
func httpErr(code int) *HTTPStatusError {
	return &HTTPStatusError{StatusCode: code, Message: http.StatusText(code)}
}

// ---------------------------------------------------------------------------
// IsTransient
// ---------------------------------------------------------------------------

func TestIsTransientRetryableStatusCodes(t *testing.T) {
	for _, code := range []int{429, 500, 502, 503, 504} {
		if !IsTransient(httpErr(code)) {
			t.Errorf("code %d must be transient", code)
		}
	}
}

func TestIsTransientNonRetryableStatusCodes(t *testing.T) {
	for _, code := range []int{200, 201, 400, 401, 403, 404, 422} {
		if IsTransient(httpErr(code)) {
			t.Errorf("code %d must NOT be transient", code)
		}
	}
}

func TestIsTransientNetworkError(t *testing.T) {
	// A plain non-HTTP error (connection refused, timeout, etc.) is transient.
	if !IsTransient(errors.New("connection refused")) {
		t.Error("plain network errors must be transient")
	}
}

func TestIsTransientNilNotTransient(t *testing.T) {
	if IsTransient(nil) {
		t.Error("nil must not be transient")
	}
}

func TestIsTransientUnrelatedErrorIsTransient(t *testing.T) {
	// A ValueError-equivalent: since it's not an HTTPStatusError we treat it as
	// a network-style error (transient) — same as the Python implementation which
	// checks isinstance(exc, (TimeoutException, ConnectError, RemoteProtocolError)).
	// In practice DoWithRetry would retry once; the key constraint is that non-HTTP
	// errors are NOT treated as permanent (4xx) failures.
	if !IsTransient(errors.New("unexpected EOF")) {
		t.Error("non-HTTP errors must be considered transient")
	}
}

// ---------------------------------------------------------------------------
// DoWithRetry — success on first attempt
// ---------------------------------------------------------------------------

func TestDoWithRetrySucceedsOnFirstAttempt(t *testing.T) {
	calls := 0
	err := DoWithRetry(context.Background(), func(_ context.Context) error {
		calls++
		return nil
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls: got %d want 1", calls)
	}
}

// ---------------------------------------------------------------------------
// DoWithRetry — retry on transient, then succeed
// ---------------------------------------------------------------------------

func TestDoWithRetryRetriesTransientThenSucceeds(t *testing.T) {
	calls := 0
	err := DoWithRetry(context.Background(), func(_ context.Context) error {
		calls++
		if calls < 2 {
			return httpErr(500)
		}
		return nil
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls: got %d want 2", calls)
	}
}

// ---------------------------------------------------------------------------
// DoWithRetry — exhaust max attempts
// ---------------------------------------------------------------------------

func TestDoWithRetryExhaustsMaxAttempts(t *testing.T) {
	calls := 0
	err := DoWithRetry(context.Background(), func(_ context.Context) error {
		calls++
		return httpErr(503)
	})
	if err == nil {
		t.Error("expected error after exhausting attempts")
	}
	if calls != MaxRetryAttempts {
		t.Errorf("calls: got %d want %d", calls, MaxRetryAttempts)
	}
}

// ---------------------------------------------------------------------------
// DoWithRetry — no retry on permanent 4xx
// ---------------------------------------------------------------------------

func TestDoWithRetryDoesNotRetryNonTransient(t *testing.T) {
	calls := 0
	err := DoWithRetry(context.Background(), func(_ context.Context) error {
		calls++
		return httpErr(404)
	})
	if err == nil {
		t.Error("expected error for 404")
	}
	if calls != 1 {
		t.Errorf("404 must not trigger retry; calls: got %d want 1", calls)
	}
}

func TestDoWithRetryDoesNotRetry400(t *testing.T) {
	calls := 0
	_ = DoWithRetry(context.Background(), func(_ context.Context) error {
		calls++
		return httpErr(400)
	})
	if calls != 1 {
		t.Errorf("400 must not trigger retry; calls: got %d want 1", calls)
	}
}

func TestDoWithRetryDoesNotRetry401(t *testing.T) {
	calls := 0
	_ = DoWithRetry(context.Background(), func(_ context.Context) error {
		calls++
		return httpErr(401)
	})
	if calls != 1 {
		t.Errorf("401 must not trigger retry; calls: got %d want 1", calls)
	}
}

// ---------------------------------------------------------------------------
// DoWithRetry — context cancellation
// ---------------------------------------------------------------------------

func TestDoWithRetryRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0

	err := DoWithRetry(ctx, func(_ context.Context) error {
		calls++
		cancel() // cancel after first call
		return httpErr(503)
	})

	if err == nil {
		t.Error("expected context error")
	}
	if !errors.Is(err, context.Canceled) {
		// err may be context.Canceled or the last HTTP error depending on timing
		// either is acceptable — what matters is we don't run MaxRetryAttempts times
		t.Logf("got error: %v (acceptable as long as we stopped early)", err)
	}
	// Must not have run all three attempts
	if calls >= MaxRetryAttempts {
		t.Errorf("context cancellation must prevent full retry; calls: got %d", calls)
	}
}

func TestDoWithRetryContextAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before any attempt

	calls := 0
	err := DoWithRetry(ctx, func(_ context.Context) error {
		calls++
		return nil
	})
	if err == nil {
		t.Error("expected error for pre-cancelled context")
	}
	if calls != 0 {
		t.Errorf("fn must not be called with pre-cancelled context; calls: got %d", calls)
	}
}

func TestDoWithRetryContextDeadlineExceeded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	time.Sleep(5 * time.Millisecond) // ensure deadline has passed

	calls := 0
	err := DoWithRetry(ctx, func(_ context.Context) error {
		calls++
		return nil
	})
	if err == nil {
		t.Error("expected error for expired deadline")
	}
	if calls != 0 {
		t.Errorf("fn must not be called after deadline; calls: got %d", calls)
	}
}

// ---------------------------------------------------------------------------
// backoffDelay
// ---------------------------------------------------------------------------

func TestBackoffDelayAttempt0Is1s(t *testing.T) {
	d := backoffDelay(0)
	if d != time.Second {
		t.Errorf("attempt 0: got %v want 1s", d)
	}
}

func TestBackoffDelayAttempt1Is2s(t *testing.T) {
	d := backoffDelay(1)
	if d != 2*time.Second {
		t.Errorf("attempt 1: got %v want 2s", d)
	}
}

func TestBackoffDelayAttempt2Is4s(t *testing.T) {
	d := backoffDelay(2)
	if d != 4*time.Second {
		t.Errorf("attempt 2: got %v want 4s", d)
	}
}

func TestBackoffDelayCappedAt30s(t *testing.T) {
	// attempt 5 → 1 * 2^5 = 32s, must be capped at 30s
	d := backoffDelay(5)
	if d != RetryMaxDelay {
		t.Errorf("attempt 5: got %v want %v (cap)", d, RetryMaxDelay)
	}
}
