// Package azure_test — tests that fetchPrices retries transient HTTP failures
// instead of surfacing them as a hard error on the first attempt (RC3-009).
package azure_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// TestGetComputePrice_RetriesTransientErrorThenSucceeds verifies that a
// transient 503 from the Azure Retail Prices API is retried rather than
// surfaced immediately as a hard error.
func TestGetComputePrice_RetriesTransientErrorThenSucceeds(t *testing.T) {
	body, err := json.Marshal(apiResponse([]azureItem{vmItem}))
	if err != nil {
		t.Fatal(err)
	}

	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(body) //nolint
	}))
	defer srv.Close()

	p := newTestProvider(t, srv)
	prices, err := p.GetComputePrice(context.Background(), "Standard_D4s_v3", "eastus", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("GetComputePrice: unexpected error after transient failure: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("GetComputePrice: expected at least one price, got 0")
	}
	if atomic.LoadInt32(&callCount) != 2 {
		t.Errorf("GetComputePrice: got %d HTTP calls, want 2 (1 failed + 1 retry)", callCount)
	}
}

// TestGetComputePrice_DoesNotRetryPermanentError verifies that a permanent
// 4xx error (e.g. 404) is surfaced immediately without a retry loop.
func TestGetComputePrice_DoesNotRetryPermanentError(t *testing.T) {
	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv)
	_, err := p.GetComputePrice(context.Background(), "Standard_D4s_v3", "eastus", "Linux", models.PricingTermOnDemand)
	if err == nil {
		t.Fatal("GetComputePrice: expected error for 404 response")
	}
	if atomic.LoadInt32(&callCount) != 1 {
		t.Errorf("GetComputePrice: got %d HTTP calls, want 1 (no retry on permanent 4xx)", callCount)
	}
}
