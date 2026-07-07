// gcp_sku_lookup_test.go tests LookupSKUAcrossRegionsGeneric (RC3-015),
// GCP's raw-skuId counterpart to AWS's get_price_by_sku lookup. Coverage
// focuses on the region-attribution priority rule (geoTaxonomy-first,
// serviceRegions-fallback) documented at the top of gcp_sku_lookup.go, plus
// the multi-service fan-out's error/no-match/duplicate-skuId handling.
package gcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/skulookup"
)

// --------------------------------------------------------------------------
// Fixture helpers
// --------------------------------------------------------------------------

// tierSpec is one (startUsageAmount, unitPrice) pair for lookupSKU's
// tieredRates.
type tierSpec struct {
	start float64
	units string
	nanos int
}

// lookupSKU builds a raw GCP SKU map exposing every field
// LookupSKUAcrossRegionsGeneric's region-attribution and price-extraction
// logic reads: skuId, description, serviceRegions, geoTaxonomy (only set
// when geoType != ""), and one or more tieredRates. Unlike makeSKU
// (gcp_compute_test.go), which only builds a single flat-rate REGIONAL-ish
// fixture, this supports every geoTaxonomy shape this file's tests exercise.
func lookupSKU(skuID, desc string, serviceRegions []string, geoType string, geoRegions []string, tiers []tierSpec) map[string]any {
	tieredRates := make([]any, 0, len(tiers))
	for _, t := range tiers {
		tieredRates = append(tieredRates, map[string]any{
			"startUsageAmount": t.start,
			"unitPrice": map[string]any{
				"units": t.units,
				"nanos": float64(t.nanos),
			},
		})
	}
	regionsAny := make([]any, len(serviceRegions))
	for i, r := range serviceRegions {
		regionsAny[i] = r
	}
	sku := map[string]any{
		"skuId":          skuID,
		"description":    desc,
		"serviceRegions": regionsAny,
		"category": map[string]any{
			"resourceFamily": "Test",
			"resourceGroup":  "Test",
			"usageType":      "OnDemand",
		},
		"pricingInfo": []any{
			map[string]any{
				"pricingExpression": map[string]any{
					"usageUnit":   "h",
					"tieredRates": tieredRates,
				},
			},
		},
	}
	if geoType != "" {
		geoRegionsAny := make([]any, len(geoRegions))
		for i, r := range geoRegions {
			geoRegionsAny[i] = r
		}
		sku["geoTaxonomy"] = map[string]any{
			"type":    geoType,
			"regions": geoRegionsAny,
		}
	}
	return sku
}

// newMultiServiceSKUServer builds a fake Cloud Billing Catalog server whose
// response depends on which service ID the request path names
// (/services/{serviceID}/skus), so tests can drive
// LookupSKUAcrossRegionsGeneric's multi-service fan-out (up to all 13
// onboarded services for a no-hint lookup) with per-service fixtures. Any
// serviceID not present in byServiceID gets a clean empty-catalog 200 —
// callers only need to define handlers for the service(s) a given test cares
// about, not all 13.
func newMultiServiceSKUServer(t *testing.T, byServiceID map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 2 {
			http.NotFound(w, r)
			return
		}
		svcID := parts[1]
		if h, ok := byServiceID[svcID]; ok {
			h(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(skuResponse(nil))
	}))
}

// jsonSKUsHandler returns an http.HandlerFunc serving a fixed SKU list as a
// 200 OK Cloud Billing Catalog page.
func jsonSKUsHandler(skus []map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(skuResponse(skus))
	}
}

// failingHandler always responds with HTTP 500, simulating a service whose
// catalog fetch fails mid-scan.
func failingHandler(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// --------------------------------------------------------------------------
// 1. GLOBAL geoTaxonomy beats a restrictive serviceRegions list (the KMS-bug
//    regression — the single most important test in this file).
// --------------------------------------------------------------------------

func TestLookupSKUAcrossRegionsGeneric_GlobalGeoTaxonomyBeatsRestrictiveServiceRegions(t *testing.T) {
	sku := lookupSKU(
		"SKU-GLOBAL", "Global key version rate",
		[]string{"europe-west1"}, // deliberately restrictive/wrong serviceRegions
		"GLOBAL", nil,
		[]tierSpec{{start: 0, units: "0", nanos: 3_000_000}},
	)
	server := newMultiServiceSKUServer(t, map[string]http.HandlerFunc{
		kmsServiceID: jsonSKUsHandler([]map[string]any{sku}),
	})
	defer server.Close()
	p := newTestProvider(t, server)

	res, err := p.LookupSKUAcrossRegionsGeneric(
		context.Background(), "SKU-GLOBAL", []string{"us-central1"}, "kms", skulookup.SKUHint{})
	if err != nil {
		t.Fatalf("LookupSKUAcrossRegionsGeneric error: %v", err)
	}
	if len(res.Regions) != 1 {
		t.Fatalf("expected 1 region result, got %d", len(res.Regions))
	}
	rr := res.Regions[0]
	// us-central1 is NOT in serviceRegions ([]string{"europe-west1"}) — a
	// match here proves geoTaxonomy.type==GLOBAL is checked (and wins) before
	// any serviceRegions membership fallback, exactly like the live Cloud
	// KMS bug this regression test mirrors.
	if rr.NoMapping || rr.Error != "" {
		t.Fatalf("expected us-central1 to match via GLOBAL geoTaxonomy despite restrictive serviceRegions, got NoMapping=%v Error=%q",
			rr.NoMapping, rr.Error)
	}
	if len(rr.Prices) != 1 {
		t.Fatalf("expected 1 price, got %d", len(rr.Prices))
	}
}

// --------------------------------------------------------------------------
// 2. MULTI_REGIONAL: overlapping geoTaxonomy.regions must not cause a false
//    match — only the description-based short name decides.
// --------------------------------------------------------------------------

func TestLookupSKUAcrossRegionsGeneric_MultiRegionalNoCollision(t *testing.T) {
	// nam5 and nam7 are documented (gcp_firestore.go) to share constituent
	// regions in their geoTaxonomy.regions lists (e.g. both list
	// "us-central1"). This SKU's description says "nam5"; requesting region
	// "nam7" must NOT match even though the constituent-region lists overlap.
	sku := lookupSKU(
		"SKU-NAM5", "Cloud Firestore storage nam5",
		[]string{"global"}, // Firestore's serviceRegions is a useless literal "global"
		"MULTI_REGIONAL", []string{"us-central1", "us-east1", "us-east4"},
		[]tierSpec{{start: 0, units: "0", nanos: 180_000_000}},
	)
	server := newMultiServiceSKUServer(t, map[string]http.HandlerFunc{
		firestoreServiceID: jsonSKUsHandler([]map[string]any{sku}),
	})
	defer server.Close()
	p := newTestProvider(t, server)

	res, err := p.LookupSKUAcrossRegionsGeneric(
		context.Background(), "SKU-NAM5", []string{"nam5", "nam7"}, "firestore", skulookup.SKUHint{})
	if err != nil {
		t.Fatalf("LookupSKUAcrossRegionsGeneric error: %v", err)
	}
	if len(res.Regions) != 2 {
		t.Fatalf("expected 2 region results, got %d", len(res.Regions))
	}
	byRegion := map[string]skulookup.SKULookupRegionResult{}
	for _, rr := range res.Regions {
		byRegion[rr.Region] = rr
	}

	nam5 := byRegion["nam5"]
	if nam5.NoMapping || nam5.Error != "" || len(nam5.Prices) != 1 {
		t.Errorf("expected nam5 to match its own SKU, got NoMapping=%v Error=%q Prices=%d",
			nam5.NoMapping, nam5.Error, len(nam5.Prices))
	}
	nam7 := byRegion["nam7"]
	if !nam7.NoMapping {
		t.Errorf("expected nam7 to NOT match a nam5-tagged SKU (overlapping constituent regions must not cause a false match), got NoMapping=%v Prices=%d",
			nam7.NoMapping, len(nam7.Prices))
	}
}

// --------------------------------------------------------------------------
// 3. REGIONAL: matches only its exact single region.
// --------------------------------------------------------------------------

func TestLookupSKUAcrossRegionsGeneric_RegionalExactMatchOnly(t *testing.T) {
	sku := lookupSKU(
		"SKU-REGIONAL", "Regional disk rate",
		[]string{"us-west1"},
		"REGIONAL", []string{"us-west1"},
		[]tierSpec{{start: 0, units: "0", nanos: 40_000_000}},
	)
	server := newMultiServiceSKUServer(t, map[string]http.HandlerFunc{
		computeServiceID: jsonSKUsHandler([]map[string]any{sku}),
	})
	defer server.Close()
	p := newTestProvider(t, server)

	res, err := p.LookupSKUAcrossRegionsGeneric(
		context.Background(), "SKU-REGIONAL", []string{"us-west1", "us-west2"}, "compute", skulookup.SKUHint{})
	if err != nil {
		t.Fatalf("LookupSKUAcrossRegionsGeneric error: %v", err)
	}
	byRegion := map[string]skulookup.SKULookupRegionResult{}
	for _, rr := range res.Regions {
		byRegion[rr.Region] = rr
	}
	if m := byRegion["us-west1"]; m.NoMapping || len(m.Prices) != 1 {
		t.Errorf("expected us-west1 (exact region) to match, got NoMapping=%v Prices=%d", m.NoMapping, len(m.Prices))
	}
	if m := byRegion["us-west2"]; !m.NoMapping {
		t.Errorf("expected us-west2 (different region) to NOT match a REGIONAL SKU scoped to us-west1, got NoMapping=%v", m.NoMapping)
	}
}

// --------------------------------------------------------------------------
// 4. Tiered-rate SKU: 3+ ascending tiers, Tiered=true.
// --------------------------------------------------------------------------

func TestLookupSKUAcrossRegionsGeneric_TieredRates(t *testing.T) {
	sku := lookupSKU(
		"SKU-TIERED", "Tiered storage rate",
		[]string{"us-central1"},
		"", nil,
		[]tierSpec{
			{start: 0, units: "0", nanos: 100_000_000},
			{start: 1000, units: "0", nanos: 80_000_000},
			{start: 5000, units: "0", nanos: 50_000_000},
		},
	)
	server := newMultiServiceSKUServer(t, map[string]http.HandlerFunc{
		gcsServiceID: jsonSKUsHandler([]map[string]any{sku}),
	})
	defer server.Close()
	p := newTestProvider(t, server)

	res, err := p.LookupSKUAcrossRegionsGeneric(
		context.Background(), "SKU-TIERED", []string{"us-central1"}, "gcs", skulookup.SKUHint{})
	if err != nil {
		t.Fatalf("LookupSKUAcrossRegionsGeneric error: %v", err)
	}
	rr := res.Regions[0]
	if !rr.Tiered {
		t.Fatalf("expected Tiered=true for a 3-tier SKU")
	}
	if len(rr.Prices) != 3 {
		t.Fatalf("expected 3 tier prices, got %d", len(rr.Prices))
	}
	// Ascending order by tier_start_usage, and the first tier's price used
	// as the primary/default entry (rr.Prices[0]).
	wantStarts := []string{"0", "1000", "5000"}
	wantPrices := []float64{0.1, 0.08, 0.05}
	for i, p := range rr.Prices {
		if got := p.Attributes["tier_start_usage"]; got != wantStarts[i] {
			t.Errorf("tier %d: tier_start_usage = %q, want %q", i, got, wantStarts[i])
		}
		if abs(p.PricePerUnit-wantPrices[i]) > 1e-9 {
			t.Errorf("tier %d: price = %v, want %v", i, p.PricePerUnit, wantPrices[i])
		}
	}
}

// --------------------------------------------------------------------------
// 5. Mid-scan error: one candidate service fails, no match among the
//    services that did succeed — must be reported as Error, not NoMapping.
// --------------------------------------------------------------------------

func TestLookupSKUAcrossRegionsGeneric_MidScanErrorNotNoMapping(t *testing.T) {
	server := newMultiServiceSKUServer(t, map[string]http.HandlerFunc{
		kmsServiceID: failingHandler,
	})
	defer server.Close()
	p := newTestProvider(t, server)

	// No service hint: scans all 13 onboarded services. kms 500s; every
	// other service returns an empty (non-matching) catalog.
	res, err := p.LookupSKUAcrossRegionsGeneric(
		context.Background(), "SKU-NOT-FOUND", []string{"us-central1"}, "", skulookup.SKUHint{})
	if err != nil {
		t.Fatalf("LookupSKUAcrossRegionsGeneric error: %v", err)
	}
	if len(res.Regions) != 1 {
		t.Fatalf("expected 1 region result, got %d", len(res.Regions))
	}
	rr := res.Regions[0]
	if rr.NoMapping {
		t.Errorf("expected an incomplete scan (kms failed) to NOT be reported as NoMapping")
	}
	if rr.Error == "" {
		t.Errorf("expected a non-empty Error for an incomplete scan")
	}
	// AttemptedServices must reflect only the services that actually
	// completed (12 of 13 — every onboarded service except kms).
	if len(rr.AttemptedServices) != len(gcpSKULookupServiceOrder)-1 {
		t.Errorf("expected AttemptedServices to list %d completed services, got %d: %v",
			len(gcpSKULookupServiceOrder)-1, len(rr.AttemptedServices), rr.AttemptedServices)
	}
	for _, s := range rr.AttemptedServices {
		if s == "kms" {
			t.Errorf("AttemptedServices must not include the failed service %q, got %v", "kms", rr.AttemptedServices)
		}
	}
}

// --------------------------------------------------------------------------
// 6. Clean full-scan, no match anywhere: NoMapping=true, not Error.
// --------------------------------------------------------------------------

func TestLookupSKUAcrossRegionsGeneric_CleanNoMatch(t *testing.T) {
	server := newMultiServiceSKUServer(t, nil) // every service returns an empty catalog
	defer server.Close()
	p := newTestProvider(t, server)

	res, err := p.LookupSKUAcrossRegionsGeneric(
		context.Background(), "SKU-NOWHERE", []string{"us-central1"}, "", skulookup.SKUHint{})
	if err != nil {
		t.Fatalf("LookupSKUAcrossRegionsGeneric error: %v", err)
	}
	rr := res.Regions[0]
	if !rr.NoMapping {
		t.Errorf("expected a genuine complete-scan miss to be reported as NoMapping, got NoMapping=%v Error=%q", rr.NoMapping, rr.Error)
	}
	if rr.Error != "" {
		t.Errorf("expected no Error for a clean no-match scan, got %q", rr.Error)
	}
	if len(rr.AttemptedServices) != len(gcpSKULookupServiceOrder) {
		t.Errorf("expected AttemptedServices to list all %d onboarded services, got %d: %v",
			len(gcpSKULookupServiceOrder), len(rr.AttemptedServices), rr.AttemptedServices)
	}
}

// --------------------------------------------------------------------------
// 7. Invalid/unrecognized service hint.
// --------------------------------------------------------------------------

func TestLookupSKUAcrossRegionsGeneric_InvalidServiceHint(t *testing.T) {
	server := newMultiServiceSKUServer(t, nil)
	defer server.Close()
	p := newTestProvider(t, server)

	_, err := p.LookupSKUAcrossRegionsGeneric(
		context.Background(), "SKU-ANY", []string{"us-central1"}, "not-a-real-service", skulookup.SKUHint{})
	if err == nil {
		t.Fatalf("expected an error for an unrecognized service hint, got nil")
	}
	skuErr, ok := err.(*skulookup.SKULookupError)
	if !ok {
		t.Fatalf("expected *skulookup.SKULookupError, got %T: %v", err, err)
	}
	if skuErr.Code != skulookup.SKUErrInvalidService {
		t.Errorf("Code = %q, want %q", skuErr.Code, skulookup.SKUErrInvalidService)
	}
}

// --------------------------------------------------------------------------
// 8. Duplicate skuId within one service's catalog: Warning produced, first
//    match used, no crash.
// --------------------------------------------------------------------------

func TestLookupSKUAcrossRegionsGeneric_DuplicateSKUIDWarns(t *testing.T) {
	first := lookupSKU(
		"SKU-DUP", "First matching row",
		[]string{"us-central1"}, "", nil,
		[]tierSpec{{start: 0, units: "0", nanos: 10_000_000}},
	)
	second := lookupSKU(
		"SKU-DUP", "Second matching row (should be ignored)",
		[]string{"us-central1"}, "", nil,
		[]tierSpec{{start: 0, units: "0", nanos: 99_000_000}},
	)
	server := newMultiServiceSKUServer(t, map[string]http.HandlerFunc{
		computeServiceID: jsonSKUsHandler([]map[string]any{first, second}),
	})
	defer server.Close()
	p := newTestProvider(t, server)

	res, err := p.LookupSKUAcrossRegionsGeneric(
		context.Background(), "SKU-DUP", []string{"us-central1"}, "compute", skulookup.SKUHint{})
	if err != nil {
		t.Fatalf("LookupSKUAcrossRegionsGeneric error: %v", err)
	}
	if len(res.Warnings) == 0 {
		t.Fatalf("expected a duplicate-skuId warning, got none")
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "SKU-DUP") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a warning mentioning the duplicated skuId, got: %v", res.Warnings)
	}
	rr := res.Regions[0]
	if len(rr.Prices) != 1 {
		t.Fatalf("expected 1 price (first match used), got %d", len(rr.Prices))
	}
	if abs(rr.Prices[0].PricePerUnit-0.01) > 1e-9 {
		t.Errorf("expected the FIRST matching row's price (0.01) to be used, got %v", rr.Prices[0].PricePerUnit)
	}
	if rr.Prices[0].Description != "First matching row" {
		t.Errorf("expected the first matching row's description, got %q", rr.Prices[0].Description)
	}
}
