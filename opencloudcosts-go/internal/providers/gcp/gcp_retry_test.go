// Package gcp — tests that fetchSKUs retries transient HTTP failures instead
// of surfacing them as a hard error on the first attempt (RC3-009).
package gcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestFetchSKUsRetriesTransientErrorThenSucceeds verifies that fetchSKUs
// retries on a transient 503 response and returns the successful result
// once the upstream recovers, instead of failing on the first error.
func TestFetchSKUsRetriesTransientErrorThenSucceeds(t *testing.T) {
	skus := []map[string]any{
		makeSKU("N2 Instance Core", "OnDemand", "us-central1", "0", 31_611_000),
	}

	var callCount int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	got, err := p.fetchSKUs(context.Background(), computeServiceID)
	if err != nil {
		t.Fatalf("fetchSKUs: unexpected error after transient failure: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("fetchSKUs: got %d SKUs, want 1", len(got))
	}
	if atomic.LoadInt32(&callCount) != 2 {
		t.Errorf("fetchSKUs: got %d HTTP calls, want 2 (1 failed + 1 retry)", callCount)
	}
}

// TestFetchSKUsDoesNotRetryPermanentError verifies that a permanent 4xx error
// (e.g. 404) is surfaced immediately without a retry loop.
func TestFetchSKUsDoesNotRetryPermanentError(t *testing.T) {
	var callCount int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	_, err := p.fetchSKUs(context.Background(), computeServiceID)
	if err == nil {
		t.Fatal("fetchSKUs: expected error for 404 response")
	}
	if atomic.LoadInt32(&callCount) != 1 {
		t.Errorf("fetchSKUs: got %d HTTP calls, want 1 (no retry on permanent 4xx)", callCount)
	}
}
