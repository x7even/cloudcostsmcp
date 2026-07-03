// sku_lookup_batch_test.go tests the get_prices_by_sku tool handler
// (HandleGetPricesBySKU) — the batch form of get_price_by_sku. It reuses the
// fixture helpers (skuFixtureJSON) and test hooks declared in
// sku_lookup_test.go.
package tools_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/config"
	awsprovider "github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers/aws"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/tools"
)

// callGetPricesBySKU invokes HandleGetPricesBySKU and decodes the response.
func callGetPricesBySKU(t *testing.T, h *tools.Handler, in tools.GetPricesBySKUInput) map[string]any {
	t.Helper()
	result, _, err := h.HandleGetPricesBySKU(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("HandleGetPricesBySKU returned err: %v", err)
	}
	return decodeResult(t, result)
}

// --------------------------------------------------------------------------
// Happy path
// --------------------------------------------------------------------------

// TestHandleGetPricesBySKU_HappyPath verifies a batch of two SKUs, each
// resolved against the same two regions, produces one fully-shaped
// get_price_by_sku-style entry per SKU in "results", with no top-level
// "errors".
func TestHandleGetPricesBySKU_HappyPath(t *testing.T) {
	awsprovider.ResetSKUCatalogCacheForTesting()

	mux := http.NewServeMux()
	mux.HandleFunc("/AmazonEC2/current/us-east-1/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(skuFixtureJSONMulti([]struct {
			sku, usageType, location, priceUSD string
			extraAttrs                         map[string]string
		}{
			{"SKU-EAST-M5", "BoxUsage:m5.large", "US East (N. Virginia)", "0.0960000000", nil},
			{"SKU-EAST-R5", "BoxUsage:r5.large", "US East (N. Virginia)", "0.1260000000", nil},
		})))
	})
	mux.HandleFunc("/AmazonEC2/current/us-west-2/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(skuFixtureJSONMulti([]struct {
			sku, usageType, location, priceUSD string
			extraAttrs                         map[string]string
		}{
			{"SKU-WEST-M5", "USW2-BoxUsage:m5.large", "US West (Oregon)", "0.1000000000", nil},
			{"SKU-WEST-R5", "USW2-BoxUsage:r5.large", "US West (Oregon)", "0.1300000000", nil},
		})))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	restore := awsprovider.SetBulkPricingBaseURLForTesting(server.URL)
	defer restore()

	realAWS, err := awsprovider.NewProvider(&config.Config{}, nil)
	if err != nil {
		t.Fatalf("awsprovider.NewProvider: %v", err)
	}
	h := tools.New(map[string]tools.Provider{"aws": realAWS})

	resp := callGetPricesBySKU(t, h, tools.GetPricesBySKUInput{
		SKUs:    []string{"BoxUsage:m5.large", "BoxUsage:r5.large"},
		Regions: []string{"us-east-1", "us-west-2"},
	})

	if resp["errors"] != nil {
		t.Fatalf("unexpected errors field: %v", resp["errors"])
	}
	if resp["count"] != float64(2) {
		t.Errorf("count = %v, want 2", resp["count"])
	}

	results, ok := resp["results"].([]any)
	if !ok || len(results) != 2 {
		t.Fatalf("expected 2 entries in results, got %v", resp["results"])
	}

	// Each result entry mirrors a standalone get_price_by_sku response: has
	// its own sku/all_regions_sorted/cheapest_region etc.
	first := results[0].(map[string]any)
	if first["sku"] != "BoxUsage:m5.large" {
		t.Errorf("results[0].sku = %v, want BoxUsage:m5.large", first["sku"])
	}
	if first["cheapest_region"] != "us-east-1" {
		t.Errorf("results[0].cheapest_region = %v, want us-east-1", first["cheapest_region"])
	}
	allSorted, ok := first["all_regions_sorted"].([]any)
	if !ok || len(allSorted) != 2 {
		t.Fatalf("results[0].all_regions_sorted: expected 2 entries, got %v", first["all_regions_sorted"])
	}

	second := results[1].(map[string]any)
	if second["sku"] != "BoxUsage:r5.large" {
		t.Errorf("results[1].sku = %v, want BoxUsage:r5.large", second["sku"])
	}
}

// TestHandleGetPricesBySKU_PreservesInputOrder verifies results are NOT
// re-sorted by price (unlike get_prices_batch) — a batch of heterogeneous
// SKUs commonly spans incomparable units, so output order mirrors the input
// skus list.
func TestHandleGetPricesBySKU_PreservesInputOrder(t *testing.T) {
	awsprovider.ResetSKUCatalogCacheForTesting()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(skuFixtureJSONMulti([]struct {
			sku, usageType, location, priceUSD string
			extraAttrs                         map[string]string
		}{
			// Priced so that if results were sorted cheapest-first (like
			// get_prices_batch), the order would be reversed from input.
			{"SKU-EXPENSIVE", "BoxUsage:r5.24xlarge", "US East (N. Virginia)", "5.0000000000", nil},
			{"SKU-CHEAP", "BoxUsage:t3.micro", "US East (N. Virginia)", "0.0104000000", nil},
		})))
	}))
	defer server.Close()
	restore := awsprovider.SetBulkPricingBaseURLForTesting(server.URL)
	defer restore()

	realAWS, err := awsprovider.NewProvider(&config.Config{}, nil)
	if err != nil {
		t.Fatalf("awsprovider.NewProvider: %v", err)
	}
	h := tools.New(map[string]tools.Provider{"aws": realAWS})

	resp := callGetPricesBySKU(t, h, tools.GetPricesBySKUInput{
		SKUs:    []string{"BoxUsage:r5.24xlarge", "BoxUsage:t3.micro"},
		Regions: []string{"us-east-1"},
	})

	results, ok := resp["results"].([]any)
	if !ok || len(results) != 2 {
		t.Fatalf("expected 2 entries in results, got %v", resp["results"])
	}
	if results[0].(map[string]any)["sku"] != "BoxUsage:r5.24xlarge" {
		t.Errorf("results[0].sku = %v, want BoxUsage:r5.24xlarge (input order, not price-sorted)",
			results[0].(map[string]any)["sku"])
	}
	if results[1].(map[string]any)["sku"] != "BoxUsage:t3.micro" {
		t.Errorf("results[1].sku = %v, want BoxUsage:t3.micro (input order, not price-sorted)",
			results[1].(map[string]any)["sku"])
	}
}

// --------------------------------------------------------------------------
// Per-SKU errors
// --------------------------------------------------------------------------

// TestHandleGetPricesBySKU_PerSKUErrorSurfaced verifies a SKU whose service
// cannot be inferred (no service hint, and the usage-type pattern is
// deliberately ambiguous between DynamoDB/S3 storage — see
// inferServiceFromUsageType's TimedStorage-ByteHrs note) is reported under
// the top-level "errors" map keyed by that sku, mirroring get_prices_batch's
// per-item error shape, while the other (resolvable) SKU still succeeds in
// "results".
func TestHandleGetPricesBySKU_PerSKUErrorSurfaced(t *testing.T) {
	awsprovider.ResetSKUCatalogCacheForTesting()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(skuFixtureJSON("SKU1", "BoxUsage:m5.large", "US East (N. Virginia)", "0.0960000000")))
	}))
	defer server.Close()
	restore := awsprovider.SetBulkPricingBaseURLForTesting(server.URL)
	defer restore()

	realAWS, err := awsprovider.NewProvider(&config.Config{}, nil)
	if err != nil {
		t.Fatalf("awsprovider.NewProvider: %v", err)
	}
	h := tools.New(map[string]tools.Provider{"aws": realAWS})

	resp := callGetPricesBySKU(t, h, tools.GetPricesBySKUInput{
		SKUs:    []string{"BoxUsage:m5.large", "TimedStorage-ByteHrs"},
		Regions: []string{"us-east-1"},
	})

	results, ok := resp["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("expected 1 successful entry in results, got %v", resp["results"])
	}
	if results[0].(map[string]any)["sku"] != "BoxUsage:m5.large" {
		t.Errorf("results[0].sku = %v, want BoxUsage:m5.large", results[0].(map[string]any)["sku"])
	}
	if resp["count"] != float64(1) {
		t.Errorf("count = %v, want 1", resp["count"])
	}

	errs, ok := resp["errors"].(map[string]any)
	if !ok {
		t.Fatalf("expected an errors map, got %v", resp["errors"])
	}
	entry, ok := errs["TimedStorage-ByteHrs"].(map[string]any)
	if !ok {
		t.Fatalf("expected errors[\"TimedStorage-ByteHrs\"], got %v", errs)
	}
	if entry["status"] != "no_data" {
		t.Errorf("errors[TimedStorage-ByteHrs].status = %v, want no_data", entry["status"])
	}
	if entry["retryable"] != false {
		t.Errorf("errors[TimedStorage-ByteHrs].retryable = %v, want false", entry["retryable"])
	}
	if entry["message"] == "" || entry["message"] == nil {
		t.Errorf("errors[TimedStorage-ByteHrs].message is empty")
	}
}

// --------------------------------------------------------------------------
// Catalog memoization reuse
// --------------------------------------------------------------------------

// TestHandleGetPricesBySKU_ReusesMemoizedCatalog verifies that two SKUs in
// the same batch that both resolve to AmazonEC2/us-east-1 trigger exactly
// one offer-file fetch for that (service, region) pair — the batch handler
// must reuse the same process-lifetime skuCatalogCache get_price_by_sku
// uses, not fetch the catalog once per SKU.
func TestHandleGetPricesBySKU_ReusesMemoizedCatalog(t *testing.T) {
	awsprovider.ResetSKUCatalogCacheForTesting()

	var fetches int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&fetches, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(skuFixtureJSONMulti([]struct {
			sku, usageType, location, priceUSD string
			extraAttrs                         map[string]string
		}{
			{"SKU-M5", "BoxUsage:m5.large", "US East (N. Virginia)", "0.0960000000", nil},
			{"SKU-R5", "BoxUsage:r5.large", "US East (N. Virginia)", "0.1260000000", nil},
			{"SKU-C5", "BoxUsage:c5.large", "US East (N. Virginia)", "0.0850000000", nil},
		})))
	}))
	defer server.Close()
	restore := awsprovider.SetBulkPricingBaseURLForTesting(server.URL)
	defer restore()

	realAWS, err := awsprovider.NewProvider(&config.Config{}, nil)
	if err != nil {
		t.Fatalf("awsprovider.NewProvider: %v", err)
	}
	h := tools.New(map[string]tools.Provider{"aws": realAWS})

	resp := callGetPricesBySKU(t, h, tools.GetPricesBySKUInput{
		SKUs:    []string{"BoxUsage:m5.large", "BoxUsage:r5.large", "BoxUsage:c5.large"},
		Regions: []string{"us-east-1"},
	})

	if resp["errors"] != nil {
		t.Fatalf("unexpected errors field: %v", resp["errors"])
	}
	results, ok := resp["results"].([]any)
	if !ok || len(results) != 3 {
		t.Fatalf("expected 3 entries in results, got %v", resp["results"])
	}

	if got := atomic.LoadInt64(&fetches); got != 1 {
		t.Errorf("offer-file fetch count = %d, want 1 (catalog should be memoized across SKUs sharing service/region)", got)
	}
}

// --------------------------------------------------------------------------
// Validation-error paths (no network mocking required)
// --------------------------------------------------------------------------

// TestHandleGetPricesBySKU_MissingSKUs verifies an empty skus list is
// rejected with a structured "skus_required" error.
func TestHandleGetPricesBySKU_MissingSKUs(t *testing.T) {
	realAWS, err := awsprovider.NewProvider(&config.Config{}, nil)
	if err != nil {
		t.Fatalf("awsprovider.NewProvider: %v", err)
	}
	h := tools.New(map[string]tools.Provider{"aws": realAWS})

	resp := callGetPricesBySKU(t, h, tools.GetPricesBySKUInput{
		SKUs:    nil,
		Regions: []string{"us-east-1"},
	})

	if resp["error"] != "skus_required" {
		t.Errorf("error = %v, want skus_required", resp["error"])
	}
}

// TestHandleGetPricesBySKU_TooManySKUs verifies a skus list beyond the batch
// cap is rejected with a structured "too_many_skus" error, without making
// any network call.
func TestHandleGetPricesBySKU_TooManySKUs(t *testing.T) {
	realAWS, err := awsprovider.NewProvider(&config.Config{}, nil)
	if err != nil {
		t.Fatalf("awsprovider.NewProvider: %v", err)
	}
	h := tools.New(map[string]tools.Provider{"aws": realAWS})

	skus := make([]string, 26)
	for i := range skus {
		skus[i] = "BoxUsage:m5.large"
	}

	resp := callGetPricesBySKU(t, h, tools.GetPricesBySKUInput{
		SKUs:    skus,
		Regions: []string{"us-east-1"},
	})

	if resp["error"] != "too_many_skus" {
		t.Errorf("error = %v, want too_many_skus", resp["error"])
	}
}

// TestHandleGetPricesBySKU_MissingRegions verifies an empty regions list is
// rejected with a structured "regions_required" error.
func TestHandleGetPricesBySKU_MissingRegions(t *testing.T) {
	realAWS, err := awsprovider.NewProvider(&config.Config{}, nil)
	if err != nil {
		t.Fatalf("awsprovider.NewProvider: %v", err)
	}
	h := tools.New(map[string]tools.Provider{"aws": realAWS})

	resp := callGetPricesBySKU(t, h, tools.GetPricesBySKUInput{
		SKUs:    []string{"BoxUsage:m5.large"},
		Regions: nil,
	})

	if resp["error"] != "regions_required" {
		t.Errorf("error = %v, want regions_required", resp["error"])
	}
}

// TestHandleGetPricesBySKU_WrongProvider verifies a provider other than
// "aws" (and not registered) is rejected with unsupported_provider, without
// reaching the AWS core logic or any network call.
func TestHandleGetPricesBySKU_WrongProvider(t *testing.T) {
	h := tools.New(nil) // no providers registered at all

	resp := callGetPricesBySKU(t, h, tools.GetPricesBySKUInput{
		Provider: "gcp",
		SKUs:     []string{"BoxUsage:m5.large"},
		Regions:  []string{"us-east-1"},
	})

	if resp["error"] != "unsupported_provider" {
		t.Errorf("error = %v, want unsupported_provider", resp["error"])
	}
}

// TestHandleGetPricesBySKU_DefaultProviderIsAWS verifies that omitting
// provider entirely defaults to "aws" (mirrors get_price_by_sku's own
// default-provider regression test).
func TestHandleGetPricesBySKU_DefaultProviderIsAWS(t *testing.T) {
	awsprovider.ResetSKUCatalogCacheForTesting()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(skuFixtureJSON("SKU1", "BoxUsage:m5.large", "US East (N. Virginia)", "0.0960000000")))
	}))
	defer server.Close()
	restore := awsprovider.SetBulkPricingBaseURLForTesting(server.URL)
	defer restore()

	realAWS, err := awsprovider.NewProvider(&config.Config{}, nil)
	if err != nil {
		t.Fatalf("awsprovider.NewProvider: %v", err)
	}
	h := tools.New(map[string]tools.Provider{"aws": realAWS})

	resp := callGetPricesBySKU(t, h, tools.GetPricesBySKUInput{
		// Provider intentionally omitted.
		SKUs:    []string{"BoxUsage:m5.large"},
		Regions: []string{"us-east-1"},
	})

	if resp["error"] != nil {
		t.Fatalf("expected omitted provider to default to aws, got error: %v", resp)
	}
	if resp["count"] != float64(1) {
		t.Errorf("count = %v, want 1", resp["count"])
	}
}

// --------------------------------------------------------------------------
// baseline_region applied per SKU
// --------------------------------------------------------------------------

// TestHandleGetPricesBySKU_BaselineRegionAppliedPerSKU verifies
// baseline_region is threaded through to every SKU's own per-region delta
// computation, exactly as a standalone get_price_by_sku call would.
func TestHandleGetPricesBySKU_BaselineRegionAppliedPerSKU(t *testing.T) {
	awsprovider.ResetSKUCatalogCacheForTesting()

	mux := http.NewServeMux()
	mux.HandleFunc("/AmazonEC2/current/us-east-1/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(skuFixtureJSON("SKU-EAST", "BoxUsage:m5.large", "US East (N. Virginia)", "0.0960000000")))
	})
	mux.HandleFunc("/AmazonEC2/current/us-west-2/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(skuFixtureJSON("SKU-WEST", "USW2-BoxUsage:m5.large", "US West (Oregon)", "0.1200000000")))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	restore := awsprovider.SetBulkPricingBaseURLForTesting(server.URL)
	defer restore()

	realAWS, err := awsprovider.NewProvider(&config.Config{}, nil)
	if err != nil {
		t.Fatalf("awsprovider.NewProvider: %v", err)
	}
	h := tools.New(map[string]tools.Provider{"aws": realAWS})

	resp := callGetPricesBySKU(t, h, tools.GetPricesBySKUInput{
		SKUs:           []string{"BoxUsage:m5.large"},
		Regions:        []string{"us-east-1", "us-west-2"},
		BaselineRegion: "us-east-1",
	})

	if resp["baseline_region"] != "us-east-1" {
		t.Errorf("baseline_region = %v, want us-east-1", resp["baseline_region"])
	}
	results, ok := resp["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("expected 1 entry in results, got %v", resp["results"])
	}
	entry := results[0].(map[string]any)
	if entry["baseline_region"] != "us-east-1" {
		t.Errorf("results[0].baseline_region = %v, want us-east-1", entry["baseline_region"])
	}
	allSorted, ok := entry["all_regions_sorted"].([]any)
	if !ok || len(allSorted) != 2 {
		t.Fatalf("expected 2 entries in all_regions_sorted, got %v", entry["all_regions_sorted"])
	}
	for _, raw := range allSorted {
		row := raw.(map[string]any)
		if row["region"] == "us-west-2" {
			if _, ok := row["delta_pct"]; !ok {
				t.Errorf("us-west-2 entry missing delta_pct: %v", row)
			}
		}
	}
}
