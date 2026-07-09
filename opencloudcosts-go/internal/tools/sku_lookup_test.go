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
// collision) and no disambiguating hint is supplied, the region is:
//   - excluded entirely from all_regions_sorted (and therefore from
//     cheapest_price/most_expensive_price/price_delta_pct) — the RC2 fix for
//     Bug 1 ("ambiguous SKU matches silently resolve to 'cheapest', not
//     'correct'"; a silently-chosen price must never reach the primary
//     result), and
//   - surfaced under the top-level ambiguous_in bucket instead, with every
//     candidate row kept in alternate_matches and hint_status explaining why
//     it's still unresolved.
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

	// Must NOT appear in all_regions_sorted (the old, buggy behavior this
	// test used to assert) — an ambiguous multi-product match must never
	// silently feed the headline price.
	if allSorted, ok := resp["all_regions_sorted"].([]any); !ok || len(allSorted) != 0 {
		t.Fatalf("expected all_regions_sorted to be empty (ambiguous region excluded), got %v", resp["all_regions_sorted"])
	}
	// ...and must not feed the cheapest/most-expensive summary fields either.
	if resp["cheapest_price"] != nil || resp["cheapest_region"] != nil {
		t.Errorf("expected no cheapest_price/cheapest_region when the only match is ambiguous-unresolved, got cheapest_price=%v cheapest_region=%v", resp["cheapest_price"], resp["cheapest_region"])
	}
	if resp["most_expensive_price"] != nil || resp["most_expensive_region"] != nil {
		t.Errorf("expected no most_expensive_price/most_expensive_region, got %v / %v", resp["most_expensive_price"], resp["most_expensive_region"])
	}

	ambiguousIn, ok := resp["ambiguous_in"].([]any)
	if !ok || len(ambiguousIn) != 1 {
		t.Fatalf("expected 1 entry in ambiguous_in, got %v", resp["ambiguous_in"])
	}
	entry := ambiguousIn[0].(map[string]any)
	if entry["region"] != "us-east-1" {
		t.Errorf("expected ambiguous_in[0].region = us-east-1, got %v", entry["region"])
	}
	if entry["hint_status"] != awsprovider.HintStatusNoHint {
		t.Errorf("expected hint_status = %q, got %v", awsprovider.HintStatusNoHint, entry["hint_status"])
	}
	if entry["alternate_match_count"] != float64(2) {
		t.Errorf("expected alternate_match_count = 2, got %v", entry["alternate_match_count"])
	}
	alts, ok := entry["alternate_matches"].([]any)
	if !ok || len(alts) != 2 {
		t.Fatalf("expected 2 alternate_matches, got %v", entry["alternate_matches"])
	}

	warnings, ok := resp["warnings"].([]any)
	if !ok || len(warnings) == 0 {
		t.Fatalf("expected a top-level warning about the ambiguous match, got %v", resp["warnings"])
	}

	// No region resolved to an unambiguous match, so the handler's
	// no-unambiguous-match fallback fires (matched is empty) — the message
	// explicitly points the caller at ambiguous_in rather than claiming
	// nothing was found at all.
	if resp["result"] != "no_prices_found" {
		t.Errorf("expected result=no_prices_found (no region resolved unambiguously), got %v", resp["result"])
	}
}

// TestHandleGetPriceBySKU_HintResolvesAmbiguity verifies the full happy path
// for the RC2 fix: an operation hint (as would come from a CUR export's
// adjacent operation-code column) correctly and deterministically selects
// the Aurora PostgreSQL row out of a 5-engine collision, landing it in
// all_regions_sorted/cheapest_price like an ordinary unambiguous match —
// this is the "hint resolution happy path" flagged as unverified in the RC2
// ambiguity-fix report.
func TestHandleGetPriceBySKU_HintResolvesAmbiguity(t *testing.T) {
	awsprovider.ResetSKUCatalogCacheForTesting()

	body := skuFixtureJSONMulti([]struct {
		sku, usageType, location, priceUSD string
		extraAttrs                         map[string]string
	}{
		{
			sku: "SKU-POSTGRES", usageType: "InstanceUsage:db.r8g.2xl", location: "US East (N. Virginia)",
			priceUSD: "0.9560000000",
			extraAttrs: map[string]string{
				"operation": "CreateDBInstance:0014", "databaseEngine": "PostgreSQL",
			},
		},
		{
			sku: "SKU-AURORA-POSTGRES", usageType: "InstanceUsage:db.r8g.2xl", location: "US East (N. Virginia)",
			priceUSD: "1.1040000000",
			extraAttrs: map[string]string{
				"operation": "CreateDBInstance:0021", "databaseEngine": "Aurora PostgreSQL",
			},
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
		SKU:       "InstanceUsage:db.r8g.2xl",
		Service:   "AmazonRDS",
		Regions:   []string{"us-east-1"},
		Operation: "CreateDBInstance:0021",
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error field: %v", resp)
	}
	if resp["ambiguous_in"] != nil {
		t.Fatalf("expected no ambiguous_in once the hint resolves the row, got %v", resp["ambiguous_in"])
	}
	allSorted, ok := resp["all_regions_sorted"].([]any)
	if !ok || len(allSorted) != 1 {
		t.Fatalf("expected 1 entry in all_regions_sorted, got %v", resp["all_regions_sorted"])
	}
	entry := allSorted[0].(map[string]any)
	if entry["ambiguous"] != nil {
		t.Errorf("expected the resolved entry to carry no ambiguous flag, got %v", entry["ambiguous"])
	}
	priceInfo, ok := entry["price_per_unit"].(map[string]any)
	if !ok {
		t.Fatalf("expected price_per_unit to be present, got %v", entry)
	}
	if priceInfo["amount"] != 1.104 {
		t.Errorf("expected the Aurora PostgreSQL price (1.104), NOT the cheaper plain-PostgreSQL price (0.956) "+
			"that the pre-fix code would have silently picked; got %v", priceInfo["amount"])
	}
	// The hint is what did the disambiguating work here, not canonical-default
	// narrowing (there's no single canonical default across five DB engines) —
	// hint_status must say so, so a caller can confirm the hint mattered.
	if entry["hint_status"] != awsprovider.HintStatusResolved {
		t.Errorf("expected hint_status = %q on the resolved entry, got %v", awsprovider.HintStatusResolved, entry["hint_status"])
	}
	if resp["cheapest_price"] == nil {
		t.Fatalf("expected cheapest_price to be populated now that the only match is resolved, got nil")
	}
	cheapest := resp["cheapest_price"].(map[string]any)
	if cheapest["amount"] != 1.104 {
		t.Errorf("expected cheapest_price.amount = 1.104, got %v", cheapest["amount"])
	}
}

// TestHandleGetPriceBySKU_HintNoMatchFailsClosed verifies that a hint which
// matches none of the candidate rows does not fall through to picking any
// default (cheapest or otherwise) — the region must remain in ambiguous_in
// with hint_status=hint_no_match and the full original candidate set intact.
func TestHandleGetPriceBySKU_HintNoMatchFailsClosed(t *testing.T) {
	awsprovider.ResetSKUCatalogCacheForTesting()

	body := skuFixtureJSONMulti([]struct {
		sku, usageType, location, priceUSD string
		extraAttrs                         map[string]string
	}{
		{
			sku: "SKU-POSTGRES", usageType: "InstanceUsage:db.r8g.2xl", location: "US East (N. Virginia)",
			priceUSD:   "0.9560000000",
			extraAttrs: map[string]string{"operation": "CreateDBInstance:0014", "databaseEngine": "PostgreSQL"},
		},
		{
			sku: "SKU-AURORA-POSTGRES", usageType: "InstanceUsage:db.r8g.2xl", location: "US East (N. Virginia)",
			priceUSD:   "1.1040000000",
			extraAttrs: map[string]string{"operation": "CreateDBInstance:0021", "databaseEngine": "Aurora PostgreSQL"},
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
		SKU:       "InstanceUsage:db.r8g.2xl",
		Service:   "AmazonRDS",
		Regions:   []string{"us-east-1"},
		Operation: "CreateDBInstance:9999", // matches nothing
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error field: %v", resp)
	}
	if allSorted, ok := resp["all_regions_sorted"].([]any); !ok || len(allSorted) != 0 {
		t.Fatalf("expected all_regions_sorted to remain empty when the hint matches nothing, got %v", resp["all_regions_sorted"])
	}
	ambiguousIn, ok := resp["ambiguous_in"].([]any)
	if !ok || len(ambiguousIn) != 1 {
		t.Fatalf("expected 1 entry in ambiguous_in, got %v", resp["ambiguous_in"])
	}
	entry := ambiguousIn[0].(map[string]any)
	if entry["hint_status"] != awsprovider.HintStatusNoMatch {
		t.Errorf("expected hint_status = %q, got %v", awsprovider.HintStatusNoMatch, entry["hint_status"])
	}
	if entry["alternate_match_count"] != float64(2) {
		t.Errorf("expected the full original 2-row candidate set to be preserved (fail closed), got alternate_match_count=%v", entry["alternate_match_count"])
	}
}

// TestHandleGetPriceBySKU_SingleRowHintMismatchFailsClosed is the handler-
// level regression test for RC2 Finding 1/Security Finding 1: a suffix that
// resolves to exactly one catalog row (no multi-product collision at all)
// but carries a supplied operation hint that contradicts that row's actual
// operation attribute. The pre-fix resolveSKUCandidates short-circuited on
// "len(prices) <= 1" before ever looking at the hint, so this single row was
// silently accepted as a confident match — including a hint the caller
// explicitly supplied to rule it out. It must instead fail closed: excluded
// from all_regions_sorted/cheapest_price and surfaced under ambiguous_in
// with hint_status=hint_no_match, exactly like the multi-row case above.
func TestHandleGetPriceBySKU_SingleRowHintMismatchFailsClosed(t *testing.T) {
	awsprovider.ResetSKUCatalogCacheForTesting()

	body := skuFixtureJSONMulti([]struct {
		sku, usageType, location, priceUSD string
		extraAttrs                         map[string]string
	}{
		{
			sku: "SKU-NLB", usageType: "LCUUsage", location: "US East (N. Virginia)",
			priceUSD:   "0.0060000000",
			extraAttrs: map[string]string{"operation": "NetworkLoadBalancing", "productFamily": "Load Balancer-Network"},
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
		SKU:       "LCUUsage",
		Service:   "AWSELB",
		Regions:   []string{"us-east-1"},
		Operation: "LoadBalancing:Application", // contradicts the only row's NetworkLoadBalancing operation
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error field: %v", resp)
	}
	if allSorted, ok := resp["all_regions_sorted"].([]any); !ok || len(allSorted) != 0 {
		t.Fatalf("expected all_regions_sorted to be empty (contradicted single row excluded), got %v", resp["all_regions_sorted"])
	}
	if resp["cheapest_price"] != nil {
		t.Errorf("expected no cheapest_price when the only candidate contradicts the supplied hint, got %v", resp["cheapest_price"])
	}
	ambiguousIn, ok := resp["ambiguous_in"].([]any)
	if !ok || len(ambiguousIn) != 1 {
		t.Fatalf("expected 1 entry in ambiguous_in, got %v", resp["ambiguous_in"])
	}
	entry := ambiguousIn[0].(map[string]any)
	if entry["hint_status"] != awsprovider.HintStatusNoMatch {
		t.Errorf("expected hint_status = %q, got %v", awsprovider.HintStatusNoMatch, entry["hint_status"])
	}
	if entry["alternate_match_count"] != float64(1) {
		t.Errorf("expected the single original row to be preserved as the sole alternate (fail closed), got alternate_match_count=%v", entry["alternate_match_count"])
	}
}

// TestHandleGetPriceBySKU_MultiRegionSummarySkipsAmbiguousAndNoMapping
// verifies the top-level cheapest_region/cheapest_price/most_expensive_*
// summary is computed only from unambiguous matched regions when a single
// request mixes a resolved region, an ambiguous-unresolved region, and a
// no-mapping region.
func TestHandleGetPriceBySKU_MultiRegionSummarySkipsAmbiguousAndNoMapping(t *testing.T) {
	awsprovider.ResetSKUCatalogCacheForTesting()

	// us-east-1: unambiguous single match, price 0.30 (cheapest of the
	// resolved regions).
	usEast1Body := skuFixtureJSON("SKU-USE1", "InstanceUsage:db.r5.large", "US East (N. Virginia)", "0.3000000000")
	// us-west-2: unambiguous single match, price 0.70 (more expensive, but
	// still the most-expensive RESOLVED region — must win most_expensive_*
	// even though the (excluded) ambiguous eu-west-1 candidates include
	// higher prices).
	usWest2Body := skuFixtureJSON("SKU-USW2", "USW2-InstanceUsage:db.r5.large", "US West (Oregon)", "0.7000000000")
	// eu-west-1: genuinely ambiguous (2 engine rows), one of which (0.95) is
	// higher than us-west-2's 0.70 — if the summary computation incorrectly
	// included ambiguous regions, this would wrongly become
	// most_expensive_region.
	euWest1Body := skuFixtureJSONMulti([]struct {
		sku, usageType, location, priceUSD string
		extraAttrs                         map[string]string
	}{
		{
			sku: "SKU-EUW1-MYSQL", usageType: "EU-InstanceUsage:db.r5.large", location: "EU (Ireland)",
			priceUSD:   "0.5000000000",
			extraAttrs: map[string]string{"databaseEngine": "MySQL"},
		},
		{
			sku: "SKU-EUW1-POSTGRES", usageType: "EU-InstanceUsage:db.r5.large", location: "EU (Ireland)",
			priceUSD:   "0.9500000000",
			extraAttrs: map[string]string{"databaseEngine": "PostgreSQL"},
		},
	})
	// ap-southeast-1: no mapping at all (different suffix in its catalog).
	apSoutheast1Body := skuFixtureJSON("SKU-APSE1", "InstanceUsage:db.r5.xlarge", "Asia Pacific (Singapore)", "1.2000000000")

	mux := http.NewServeMux()
	mux.HandleFunc("/AmazonRDS/current/us-east-1/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(usEast1Body))
	})
	mux.HandleFunc("/AmazonRDS/current/us-west-2/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(usWest2Body))
	})
	mux.HandleFunc("/AmazonRDS/current/eu-west-1/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(euWest1Body))
	})
	mux.HandleFunc("/AmazonRDS/current/ap-southeast-1/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(apSoutheast1Body))
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
		SKU:     "InstanceUsage:db.r5.large",
		Service: "AmazonRDS",
		Regions: []string{"us-east-1", "us-west-2", "eu-west-1", "ap-southeast-1"},
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error field: %v", resp)
	}

	allSorted, ok := resp["all_regions_sorted"].([]any)
	if !ok || len(allSorted) != 2 {
		t.Fatalf("expected exactly 2 unambiguous entries (us-east-1, us-west-2) in all_regions_sorted, got %v", resp["all_regions_sorted"])
	}

	if resp["cheapest_region"] != "us-east-1" {
		t.Errorf("cheapest_region = %v, want us-east-1", resp["cheapest_region"])
	}
	cheapest := resp["cheapest_price"].(map[string]any)
	if cheapest["amount"] != 0.3 {
		t.Errorf("cheapest_price.amount = %v, want 0.3", cheapest["amount"])
	}

	// The key assertion: most_expensive_region must be us-west-2 (0.70), NOT
	// eu-west-1 — even though eu-west-1's ambiguous PostgreSQL alternate
	// (0.95) is numerically higher. Ambiguous regions must never leak into
	// this computation.
	if resp["most_expensive_region"] != "us-west-2" {
		t.Errorf("most_expensive_region = %v, want us-west-2 (eu-west-1's ambiguous rows must be excluded)", resp["most_expensive_region"])
	}
	mostExp := resp["most_expensive_price"].(map[string]any)
	if mostExp["amount"] != 0.7 {
		t.Errorf("most_expensive_price.amount = %v, want 0.7", mostExp["amount"])
	}

	ambiguousIn, ok := resp["ambiguous_in"].([]any)
	if !ok || len(ambiguousIn) != 1 {
		t.Fatalf("expected 1 ambiguous_in entry (eu-west-1), got %v", resp["ambiguous_in"])
	}
	if ambiguousIn[0].(map[string]any)["region"] != "eu-west-1" {
		t.Errorf("expected ambiguous_in region = eu-west-1, got %v", ambiguousIn[0])
	}

	noMapping, ok := resp["no_mapping_in"].([]any)
	if !ok || len(noMapping) != 1 {
		t.Fatalf("expected 1 no_mapping_in entry (ap-southeast-1), got %v", resp["no_mapping_in"])
	}
	if noMapping[0].(map[string]any)["region"] != "ap-southeast-1" {
		t.Errorf("expected no_mapping_in region = ap-southeast-1, got %v", noMapping[0])
	}
}

// TestHandleGetPriceBySKU_BaselineRegionAmbiguous verifies the edge case
// called out in the RC2 ambiguity-fix report: if baseline_region itself
// lands in the ambiguous-unresolved bucket, HandleGetPriceBySKU returns a
// specific, actionable baseline_region_ambiguous error rather than silently
// omitting the baseline deltas.
func TestHandleGetPriceBySKU_BaselineRegionAmbiguous(t *testing.T) {
	awsprovider.ResetSKUCatalogCacheForTesting()

	body := skuFixtureJSONMulti([]struct {
		sku, usageType, location, priceUSD string
		extraAttrs                         map[string]string
	}{
		{
			sku: "SKU-MYSQL", usageType: "InstanceUsage:db.r5.large", location: "US East (N. Virginia)",
			priceUSD:   "0.5000000000",
			extraAttrs: map[string]string{"databaseEngine": "MySQL"},
		},
		{
			sku: "SKU-POSTGRES", usageType: "InstanceUsage:db.r5.large", location: "US East (N. Virginia)",
			priceUSD:   "0.4000000000",
			extraAttrs: map[string]string{"databaseEngine": "PostgreSQL"},
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
		SKU:            "InstanceUsage:db.r5.large",
		Service:        "AmazonRDS",
		Regions:        []string{"us-east-1"},
		BaselineRegion: "us-east-1",
	})

	if resp["error"] != "baseline_region_ambiguous" {
		t.Fatalf("error = %v, want baseline_region_ambiguous", resp["error"])
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

// TestHandleGetPriceBySKU_WrongProvider_TypeSwitchDefault verifies that a
// provider registered under some key, but whose concrete type does NOT
// implement skulookup.SKULookupProvider (mockProvider implements only the
// base tools.Provider interface — see lookup_test.go), is still rejected
// with "unsupported_provider" via resolveSKULookupProviderFromMap's
// type-switch default case, not a nil-map miss. This is a generic
// unrecognized-provider-type scenario; it does not claim to model any real
// provider. AWS, GCP, and Azure are all now genuinely supported by
// get_price_by_sku (each implements skulookup.SKULookupProvider and is
// covered by its own provider's *_sku_lookup_test.go), so the key below is
// deliberately a fictitious, never-registered-in-production provider name
// to avoid implying otherwise.
//
// NOTE: this test previously registered the same *awsprovider.Provider
// instance under both "aws" and this key to reach a defense-in-depth
// providerName guard inside AWS's own core LookupSKUAcrossRegions (which
// rejects providerName values other than "aws"). That guard is no longer
// reachable through the provider-agnostic path: LookupSKUAcrossRegionsGeneric
// (the skulookup.SKULookupProvider adapter added for GCP raw-SKU lookups,
// internal/providers/aws/aws_sku_lookup.go) always calls
// p.LookupSKUAcrossRegions(ctx, "aws", ...) with a hardcoded "aws" literal,
// regardless of what key the caller resolved the provider instance under.
// That inner check is therefore dead code when reached via the generic
// interface — a real (if low-impact, since production only ever registers
// each provider under its own canonical key) regression introduced by the
// RC3-015 hoist, flagged here rather than papered over. This test now
// exercises the guard that actually enforces rejection of an
// unrecognized-type provider in production: the provs-map type switch in
// resolveSKULookupProviderFromMap.
func TestHandleGetPriceBySKU_WrongProvider_TypeSwitchDefault(t *testing.T) {
	h := tools.New(map[string]tools.Provider{"faketestcloud": &mockProvider{name: "faketestcloud"}})

	resp := callGetPriceBySKU(t, h, tools.GetPriceBySKUInput{
		Provider: "faketestcloud",
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

// --------------------------------------------------------------------------
// GCP raw-SKU lookup (RC3-015)
// --------------------------------------------------------------------------

// TestHandleGetPriceBySKU_GCPHappyPath verifies a GCP raw skuId resolves
// through HandleGetPriceBySKU against a real *gcpprovider.Provider, and that
// the AWS-only usage_type_prefix/usage_type_suffix fields are omitted
// entirely from the response (rather than present-but-empty) for a GCP
// result, per resolveSKUPriceEntry's doc comment.
func TestHandleGetPriceBySKU_GCPHappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(gcpSKUCatalogFixtureJSON(
			"0055-9F63-3A4D", "N1 Predefined Instance Core running in Americas", "us-central1", "0", 40_000_000)))
	}))
	defer server.Close()
	realGCP := newGCPSKUTestProvider(t, server)
	h := tools.New(map[string]tools.Provider{"gcp": realGCP})

	resp := callGetPriceBySKU(t, h, tools.GetPriceBySKUInput{
		Provider: "gcp",
		SKU:      "0055-9F63-3A4D",
		Service:  "compute",
		Regions:  []string{"us-central1"},
	})

	if resp["error"] != nil {
		t.Fatalf("expected no error, got: %v", resp)
	}
	if resp["cheapest_region"] != "us-central1" {
		t.Errorf("expected cheapest_region us-central1, got %v", resp["cheapest_region"])
	}
	if _, ok := resp["usage_type_prefix"]; ok {
		t.Errorf("expected usage_type_prefix to be omitted for a GCP result, got present: %v", resp["usage_type_prefix"])
	}
	if _, ok := resp["usage_type_suffix"]; ok {
		t.Errorf("expected usage_type_suffix to be omitted for a GCP result, got present: %v", resp["usage_type_suffix"])
	}
}

// --------------------------------------------------------------------------
// Azure raw-SKU lookup (SKU-lookup-tool wiring, third provider alongside AWS/GCP)
// --------------------------------------------------------------------------

// TestHandleGetPriceBySKU_AzureHappyPath verifies an Azure Retail Prices API
// meterId resolves through HandleGetPriceBySKU against a real
// *azureprovider.Provider (proving resolveSKULookupProviderFromMap's
// type-switch reaches its *azureprovider.Provider case, not just the
// AWS/GCP cases), and that the AWS-only usage_type_prefix/usage_type_suffix
// fields are omitted entirely from the response for an Azure result — the
// Azure counterpart to TestHandleGetPriceBySKU_GCPHappyPath above.
func TestHandleGetPriceBySKU_AzureHappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(azureSKUFixtureJSON(
			"00000000-0000-0000-0000-000000000000", "eastus", "D4s v3", "Virtual Machines Dsv3 Series", "Virtual Machines", 0.192)))
	}))
	defer server.Close()
	realAzure := newAzureSKUTestProvider(server)
	h := tools.New(map[string]tools.Provider{"azure": realAzure})

	resp := callGetPriceBySKU(t, h, tools.GetPriceBySKUInput{
		Provider: "azure",
		SKU:      "00000000-0000-0000-0000-000000000000",
		Regions:  []string{"eastus"},
	})

	if resp["error"] != nil {
		t.Fatalf("expected no error, got: %v", resp)
	}
	if resp["cheapest_region"] != "eastus" {
		t.Errorf("expected cheapest_region eastus, got %v", resp["cheapest_region"])
	}
	if _, ok := resp["usage_type_prefix"]; ok {
		t.Errorf("expected usage_type_prefix to be omitted for an Azure result, got present: %v", resp["usage_type_prefix"])
	}
	if _, ok := resp["usage_type_suffix"]; ok {
		t.Errorf("expected usage_type_suffix to be omitted for an Azure result, got present: %v", resp["usage_type_suffix"])
	}
}

// Note: resolveSKUPriceEntry's generic (non-*SKULookupError) upstream_failure
// branch — which now also echoes back "regions": in.Regions as part of this
// fix — is not exercised by a test here. Every current top-level error
// LookupSKUAcrossRegions can return is a *SKULookupError (validation
// failures); per-region fetch failures are captured per-entry in
// result.Regions / surfaced via errors_in, not returned as a top-level err.
// The branch is defensive against a future error-contract change in the
// provider layer, and isn't reachable through the public API today.
