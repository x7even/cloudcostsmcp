// sku_lookup_test.go tests the get_price_by_sku tool handler
// (HandleGetPriceBySKU), covering the happy path (against a real
// *awsprovider.Provider with its bulk-pricing endpoint mocked out via the
// awsprovider.SetBulkPricingBaseURLForTesting test hook) and the
// validation-error paths that don't require any network mocking.
package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/config"
	awsprovider "github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers/aws"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/tools"
)

// callGetPriceBySKU invokes HandleGetPriceBySKU and decodes the response.
func callGetPriceBySKU(t *testing.T, h *tools.Handler, in tools.GetPriceBySKUInput) map[string]any {
	t.Helper()
	result, _, err := h.HandleGetPriceBySKU(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("HandleGetPriceBySKU returned err: %v", err)
	}
	return decodeResult(t, result)
}

// skuFixtureJSON builds a minimal single-product AWS bulk offer-file fixture
// carrying a "usagetype" attribute and a non-zero OnDemand USD price, mirroring
// the fixture shape used by internal/providers/aws's own sku-lookup tests.
func skuFixtureJSON(sku, usageType, location, priceUSD string) string {
	return fmt.Sprintf(
		`{"products":{%q:{"sku":%q,"productFamily":"Compute Instance","attributes":{"usagetype":%q,"instanceType":"r6id.24xlarge","location":%q,"operatingSystem":"Linux","tenancy":"Shared","capacitystatus":"Used"}}},`+
			`"terms":{"OnDemand":{%q:{%q:{"offerTermCode":"JRTCKXETXF","priceDimensions":{%q:{"unit":"Hrs","pricePerUnit":{"USD":%q}}}}}},"Reserved":{}}}`,
		sku, sku, usageType, location,
		sku, sku+".JRTCKXETXF",
		sku+".JRTCKXETXF.6YS6EN2CT7", priceUSD,
	)
}

// --------------------------------------------------------------------------
// Happy path (real *awsprovider.Provider, mocked bulk endpoint)
// --------------------------------------------------------------------------

func TestHandleGetPriceBySKU_HappyPath(t *testing.T) {
	awsprovider.ResetSKUCatalogCacheForTesting()

	mux := http.NewServeMux()
	mux.HandleFunc("/AmazonEC2/current/us-east-1/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(skuFixtureJSON("SKU1", "BoxUsage:r6id.24xlarge", "US East (N. Virginia)", "0.5000000000")))
	})
	mux.HandleFunc("/AmazonEC2/current/us-west-2/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(skuFixtureJSON("SKU2", "USW2-BoxUsage:r6id.24xlarge", "US West (Oregon)", "0.6000000000")))
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

	resp := callGetPriceBySKU(t, h, tools.GetPriceBySKUInput{
		SKU:     "BoxUsage:r6id.24xlarge",
		Service: "AmazonEC2",
		Regions: []string{"us-east-1", "us-west-2"},
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error field: %v", resp)
	}
	if resp["usage_type_suffix"] != "BoxUsage:r6id.24xlarge" {
		t.Errorf("usage_type_suffix = %v, want BoxUsage:r6id.24xlarge", resp["usage_type_suffix"])
	}
	if resp["service_source"] != "explicit" {
		t.Errorf("service_source = %v, want explicit", resp["service_source"])
	}

	allSorted, ok := resp["all_regions_sorted"].([]any)
	if !ok || len(allSorted) != 2 {
		t.Fatalf("expected 2 entries in all_regions_sorted, got %v", resp["all_regions_sorted"])
	}
	// Cheapest-first: us-east-1 (0.5) before us-west-2 (0.6).
	first := allSorted[0].(map[string]any)
	if first["region"] != "us-east-1" {
		t.Errorf("expected cheapest-first order, first region = %v", first["region"])
	}

	if resp["cheapest_region"] != "us-east-1" {
		t.Errorf("cheapest_region = %v, want us-east-1", resp["cheapest_region"])
	}
	if resp["most_expensive_region"] != "us-west-2" {
		t.Errorf("most_expensive_region = %v, want us-west-2", resp["most_expensive_region"])
	}
	if resp["no_mapping_in"] != nil {
		t.Errorf("expected no no_mapping_in entries, got %v", resp["no_mapping_in"])
	}
	if resp["errors_in"] != nil {
		t.Errorf("expected no errors_in entries, got %v", resp["errors_in"])
	}
}

// skuFixtureJSONMulti builds a multi-product AWS bulk offer-file fixture
// from several (sku, usageType, extraAttrs, priceUSD) rows sharing a
// stripped usage-type suffix — mirrors
// internal/providers/aws/aws_sku_lookup_test.go's skuBulkJSONMulti, kept as
// its own minimal copy here since this package's tests exercise the
// tool-handler layer (HandleGetPriceBySKU) rather than importing the aws
// package's internal test helpers.
func skuFixtureJSONMulti(specs []struct {
	sku, usageType, location, priceUSD string
	extraAttrs                         map[string]string
}) string {
	products := make(map[string]any, len(specs))
	onDemand := make(map[string]any, len(specs))
	for _, s := range specs {
		attrs := map[string]string{
			"usagetype":    s.usageType,
			"instanceType": "r6id.24xlarge",
			"location":     s.location,
		}
		for k, v := range s.extraAttrs {
			attrs[k] = v
		}
		products[s.sku] = map[string]any{
			"sku":           s.sku,
			"productFamily": "Compute Instance",
			"attributes":    attrs,
		}
		termCode := s.sku + ".JRTCKXETXF"
		rateCode := termCode + ".6YS6EN2CT7"
		onDemand[s.sku] = map[string]any{
			termCode: map[string]any{
				"offerTermCode": "JRTCKXETXF",
				"priceDimensions": map[string]any{
					rateCode: map[string]any{
						"unit":         "Hrs",
						"pricePerUnit": map[string]string{"USD": s.priceUSD},
					},
				},
			},
		}
	}
	doc := map[string]any{
		"products": products,
		"terms": map[string]any{
			"OnDemand": onDemand,
			"Reserved": map[string]any{},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err) // test-only helper; a marshal failure here is a test bug
	}
	return string(b)
}

// TestHandleGetPriceBySKU_AmbiguousMatchSurfacedNotSilentlyPicked verifies
// that when a region's catalog has multiple rows sharing the requested
// usage-type suffix with no canonical-default resolution (RDS engine
// collision), the response entry is flagged ambiguous with every candidate
// surfaced via alternate_matches, rather than silently reporting one price
// as "the" match.
func TestHandleGetPriceBySKU_AmbiguousMatchSurfacedNotSilentlyPicked(t *testing.T) {
	awsprovider.ResetSKUCatalogCacheForTesting()

	body := skuFixtureJSONMulti([]struct {
		sku, usageType, location, priceUSD string
		extraAttrs                         map[string]string
	}{
		{
			sku: "SKU-MYSQL", usageType: "InstanceUsage:db.r5.large", location: "US East (N. Virginia)",
			priceUSD:   "0.5000000000",
			extraAttrs: map[string]string{"databaseEngine": "MySQL", "deploymentOption": "Single-AZ"},
		},
		{
			sku: "SKU-POSTGRES", usageType: "InstanceUsage:db.r5.large", location: "US East (N. Virginia)",
			priceUSD:   "0.4000000000",
			extraAttrs: map[string]string{"databaseEngine": "PostgreSQL", "deploymentOption": "Single-AZ"},
		},
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	restore := awsprovider.SetBulkPricingBaseURLForTesting(server.URL)
	defer restore()

	realAWS, err := awsprovider.NewProvider(&config.Config{}, nil)
	if err != nil {
		t.Fatalf("awsprovider.NewProvider: %v", err)
	}
	h := tools.New(map[string]tools.Provider{"aws": realAWS})

	resp := callGetPriceBySKU(t, h, tools.GetPriceBySKUInput{
		SKU:     "InstanceUsage:db.r5.large",
		Service: "AmazonRDS",
		Regions: []string{"us-east-1"},
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error field: %v", resp)
	}
	allSorted, ok := resp["all_regions_sorted"].([]any)
	if !ok || len(allSorted) != 1 {
		t.Fatalf("expected 1 entry in all_regions_sorted, got %v", resp["all_regions_sorted"])
	}
	entry := allSorted[0].(map[string]any)
	if entry["ambiguous"] != true {
		t.Fatalf("expected ambiguous=true on the entry, got %v", entry)
	}
	alts, ok := entry["alternate_matches"].([]any)
	if !ok || len(alts) != 2 {
		t.Fatalf("expected 2 alternate_matches, got %v", entry["alternate_matches"])
	}
	warnings, ok := resp["warnings"].([]any)
	if !ok || len(warnings) == 0 {
		t.Fatalf("expected a top-level warning about the ambiguous match, got %v", resp["warnings"])
	}
}

// TestHandleGetPriceBySKU_NoMappingRegion verifies a region whose catalog
// fetches fine but has no matching row surfaces under no_mapping_in, and does
// not produce a top-level error.
func TestHandleGetPriceBySKU_NoMappingRegion(t *testing.T) {
	awsprovider.ResetSKUCatalogCacheForTesting()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(skuFixtureJSON("SKU1", "BoxUsage:m5.xlarge", "US East (N. Virginia)", "0.2000000000")))
	}))
	defer server.Close()
	restore := awsprovider.SetBulkPricingBaseURLForTesting(server.URL)
	defer restore()

	realAWS, err := awsprovider.NewProvider(&config.Config{}, nil)
	if err != nil {
		t.Fatalf("awsprovider.NewProvider: %v", err)
	}
	h := tools.New(map[string]tools.Provider{"aws": realAWS})

	resp := callGetPriceBySKU(t, h, tools.GetPriceBySKUInput{
		SKU:     "BoxUsage:r6id.24xlarge",
		Service: "AmazonEC2",
		Regions: []string{"us-east-1"},
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error field: %v", resp)
	}
	noMapping, ok := resp["no_mapping_in"].([]any)
	if !ok || len(noMapping) != 1 {
		t.Fatalf("expected 1 no_mapping_in entry, got %v", resp["no_mapping_in"])
	}
	if resp["result"] != "no_prices_found" {
		t.Errorf("expected result=no_prices_found, got %v", resp["result"])
	}
}

// --------------------------------------------------------------------------
// Validation-error paths (no network mocking required)
// --------------------------------------------------------------------------

// TestHandleGetPriceBySKU_MissingRegions verifies an empty regions list is
// rejected with the core function's structured "regions_required" error.
func TestHandleGetPriceBySKU_MissingRegions(t *testing.T) {
	realAWS, err := awsprovider.NewProvider(&config.Config{}, nil)
	if err != nil {
		t.Fatalf("awsprovider.NewProvider: %v", err)
	}
	h := tools.New(map[string]tools.Provider{"aws": realAWS})

	resp := callGetPriceBySKU(t, h, tools.GetPriceBySKUInput{
		SKU:     "BoxUsage:r6id.24xlarge",
		Service: "AmazonEC2",
		Regions: nil,
	})

	if resp["error"] != "regions_required" {
		t.Errorf("error = %v, want regions_required", resp["error"])
	}
}

// TestHandleGetPriceBySKU_MissingSKU verifies an empty sku is rejected with
// the core function's structured "sku_required" error.
func TestHandleGetPriceBySKU_MissingSKU(t *testing.T) {
	realAWS, err := awsprovider.NewProvider(&config.Config{}, nil)
	if err != nil {
		t.Fatalf("awsprovider.NewProvider: %v", err)
	}
	h := tools.New(map[string]tools.Provider{"aws": realAWS})

	resp := callGetPriceBySKU(t, h, tools.GetPriceBySKUInput{
		SKU:     "",
		Service: "AmazonEC2",
		Regions: []string{"us-east-1"},
	})

	if resp["error"] != "sku_required" {
		t.Errorf("error = %v, want sku_required", resp["error"])
	}
}

// TestHandleGetPriceBySKU_WrongProvider verifies a provider other than "aws"
// (and not registered) is rejected with unsupported_provider, without
// reaching the AWS core logic or any network call.
func TestHandleGetPriceBySKU_WrongProvider(t *testing.T) {
	h := tools.New(nil) // no providers registered at all

	resp := callGetPriceBySKU(t, h, tools.GetPriceBySKUInput{
		Provider: "gcp",
		SKU:      "BoxUsage:r6id.24xlarge",
		Regions:  []string{"us-east-1"},
	})

	if resp["error"] != "unsupported_provider" {
		t.Errorf("error = %v, want unsupported_provider", resp["error"])
	}
}

// TestHandleGetPriceBySKU_WrongProvider_AWSCoreValidation verifies that even
// when a provider IS registered under a non-aws key that happens to resolve
// to a real *awsprovider.Provider, an explicit provider= mismatch is still
// surfaced. To actually reach the core function's own providerName guard
// (defense-in-depth double-check, per the core-logic agent's design note #2)
// rather than stopping at the handler's own pvdr==nil check, the same
// *awsprovider.Provider instance is deliberately registered under the
// "azure" key too, so h.provider("azure") resolves non-nil and the handler's
// type-assertion succeeds, letting LookupSKUAcrossRegions(ctx, "azure", ...)
// itself reject the providerName mismatch.
func TestHandleGetPriceBySKU_WrongProvider_AWSCoreValidation(t *testing.T) {
	realAWS, err := awsprovider.NewProvider(&config.Config{}, nil)
	if err != nil {
		t.Fatalf("awsprovider.NewProvider: %v", err)
	}
	h := tools.New(map[string]tools.Provider{"aws": realAWS, "azure": realAWS})

	resp := callGetPriceBySKU(t, h, tools.GetPriceBySKUInput{
		Provider: "azure",
		SKU:      "BoxUsage:r6id.24xlarge",
		Regions:  []string{"us-east-1"},
	})

	if resp["error"] != "unsupported_provider" {
		t.Errorf("error = %v, want unsupported_provider", resp["error"])
	}
}

// TestHandleGetPriceBySKU_DefaultProviderIsAWS verifies that omitting
// provider entirely defaults to "aws" (regression test for the
// provider-default bug the wiring agent found and fixed: the go-sdk does not
// apply JSON-Schema defaults to bound Go structs, so an omitted provider
// arrives as "").
func TestHandleGetPriceBySKU_DefaultProviderIsAWS(t *testing.T) {
	awsprovider.ResetSKUCatalogCacheForTesting()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(skuFixtureJSON("SKU1", "BoxUsage:r6id.24xlarge", "US East (N. Virginia)", "0.5000000000")))
	}))
	defer server.Close()
	restore := awsprovider.SetBulkPricingBaseURLForTesting(server.URL)
	defer restore()

	realAWS, err := awsprovider.NewProvider(&config.Config{}, nil)
	if err != nil {
		t.Fatalf("awsprovider.NewProvider: %v", err)
	}
	h := tools.New(map[string]tools.Provider{"aws": realAWS})

	resp := callGetPriceBySKU(t, h, tools.GetPriceBySKUInput{
		// Provider intentionally omitted.
		SKU:     "BoxUsage:r6id.24xlarge",
		Service: "AmazonEC2",
		Regions: []string{"us-east-1"},
	})

	if resp["error"] != nil {
		t.Fatalf("expected omitted provider to default to aws, got error: %v", resp)
	}
	if resp["cheapest_region"] != "us-east-1" {
		t.Errorf("expected a match in us-east-1, got %v", resp)
	}
}
