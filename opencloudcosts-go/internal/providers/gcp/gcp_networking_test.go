// Package gcp — unit tests for networking pricing (LB, CDN, NAT, egress).
package gcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// --------------------------------------------------------------------------
// Networking SKU helpers
// --------------------------------------------------------------------------

// makeSKUMultiRegion builds a minimal GCP SKU map[string]any with multiple
// serviceRegions, matching the format used by buildNetworkingIndex.
func makeSKUMultiRegion(desc string, regions []string, units string, nanos int) map[string]any {
	regionAny := make([]any, len(regions))
	for i, r := range regions {
		regionAny[i] = r
	}
	return map[string]any{
		"description":    desc,
		"serviceRegions": regionAny,
		"category": map[string]any{
			"usageType": "OnDemand",
		},
		"pricingInfo": []any{
			map[string]any{
				"pricingExpression": map[string]any{
					"tieredRates": []any{
						map[string]any{
							"startUsageAmount": float64(0),
							"unitPrice": map[string]any{
								"units": units,
								"nanos": float64(nanos),
							},
						},
					},
				},
			},
		},
	}
}

// networkingSKUResponse wraps networking SKUs for the httptest server.
func networkingSKUResponse(skus []map[string]any) []byte {
	resp := map[string]any{
		"skus":          skus,
		"nextPageToken": "",
	}
	b, _ := json.Marshal(resp)
	return b
}

// newNetworkingTestProvider creates a Provider backed by the given httptest.Server.
func newNetworkingTestProvider(t *testing.T, server *httptest.Server) *Provider {
	t.Helper()
	return newTestProvider(t, server)
}

// fakeLBSKUs returns SKUs matching the GCP Load Balancing catalog.
// External HTTP(S) rule: $0.008/hr, data processed: $0.008/GB.
func fakeLBSKUs() []map[string]any {
	return []map[string]any{
		makeSKUMultiRegion(
			"External HTTP(S) Load Balancing Rule",
			[]string{"us-central1", "global"},
			"0", 8_000_000, // $0.008
		),
		makeSKUMultiRegion(
			"TCP Proxy Load Balancing Rule",
			[]string{"us-central1", "global"},
			"0", 6_000_000, // $0.006
		),
		makeSKUMultiRegion(
			"Data processed by External HTTP(S) Load Balancing",
			[]string{"global"},
			"0", 8_000_000, // $0.008
		),
	}
}

// toFloat64 safely converts a breakdown map value to float64.
func toFloat64(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return 0
}

// --------------------------------------------------------------------------
// Cloud Load Balancing tests
// --------------------------------------------------------------------------

// TestPriceNetworkLB_RateFromSKU verifies that the LB forwarding rule hourly
// rate is extracted from the GCP SKU (External HTTP(S) Load Balancing Rule
// → $0.008/hr).
func TestPriceNetworkLB_RateFromSKU(t *testing.T) {
	skus := fakeLBSKUs()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(networkingSKUResponse(skus))
	}))
	defer ts.Close()

	p := newNetworkingTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.NetworkPricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Region: "us-central1",
		},
		LBType:        "https",
		RuleCount:     1,
		HoursPerMonth: 730,
	}
	prices, breakdown, err := p.priceNetworkLB(ctx, spec)
	if err != nil {
		t.Fatalf("priceNetworkLB: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected at least one price")
	}

	// Find the forwarding rule price (component=forwarding_rule).
	var ruleRate float64
	for _, pr := range prices {
		if pr.Attributes["component"] == "forwarding_rule" {
			ruleRate = pr.PricePerUnit
			break
		}
	}
	if abs(ruleRate-0.008) > 1e-9 {
		t.Errorf("rule rate = %.6f, want 0.008000", ruleRate)
	}

	// Verify no fallback was triggered.
	if fb, ok := breakdown["fallback"]; ok && fb == true {
		t.Error("expected fallback=false (SKUs were provided), but fallback=true was set")
	}
}

// TestPriceNetworkLB_CostMath verifies that:
//
//	rule_count=3, hours=730, data_gb=100
//	→ monthly_rule_cost = 3 * $0.008 * 730 = $17.52
//	→ monthly_data_cost = 100 * $0.008    = $0.80
//	→ monthly_total                        = $18.32
func TestPriceNetworkLB_CostMath(t *testing.T) {
	skus := fakeLBSKUs()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(networkingSKUResponse(skus))
	}))
	defer ts.Close()

	p := newNetworkingTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.NetworkPricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Region: "us-central1",
		},
		LBType:        "https",
		RuleCount:     3,
		DataGB:        100.0,
		HoursPerMonth: 730.0,
	}
	_, breakdown, err := p.priceNetworkLB(ctx, spec)
	if err != nil {
		t.Fatalf("priceNetworkLB: %v", err)
	}

	// Rule cost: 3 * $0.008 * 730 = $17.52
	wantRuleCost := 3.0 * 0.008 * 730.0
	got := toFloat64(breakdown["monthly_rule_cost"])
	if abs(got-wantRuleCost) > 1e-6 {
		t.Errorf("monthly_rule_cost = %.4f, want %.4f", got, wantRuleCost)
	}

	// Data cost: 100 * $0.008 = $0.80
	wantDataCost := 100.0 * 0.008
	got = toFloat64(breakdown["monthly_data_cost"])
	if abs(got-wantDataCost) > 1e-6 {
		t.Errorf("monthly_data_cost = %.4f, want %.4f", got, wantDataCost)
	}

	// Total: $17.52 + $0.80 = $18.32
	wantTotal := wantRuleCost + wantDataCost
	got = toFloat64(breakdown["monthly_total"])
	if abs(got-wantTotal) > 1e-6 {
		t.Errorf("monthly_total = %.4f, want %.4f", got, wantTotal)
	}
}

// TestPriceNetworkLB_Fallback verifies that when no matching SKUs are found,
// the LB price uses the hardcoded fallback rate ($0.008/hr) and sets fallback=true.
func TestPriceNetworkLB_Fallback(t *testing.T) {
	// Return an empty SKU list → no matching networking SKUs.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(networkingSKUResponse(nil))
	}))
	defer ts.Close()

	p := newNetworkingTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.NetworkPricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Region: "us-central1",
		},
		LBType:        "https",
		RuleCount:     1,
		HoursPerMonth: 730,
	}
	prices, breakdown, err := p.priceNetworkLB(ctx, spec)
	if err != nil {
		t.Fatalf("priceNetworkLB (fallback): %v", err)
	}

	// Fallback must be true.
	fb, ok := breakdown["fallback"]
	if !ok || fb != true {
		t.Errorf("expected fallback=true, got %v (ok=%v)", fb, ok)
	}

	// Fallback rule rate must be $0.008/hr.
	var ruleRate float64
	for _, pr := range prices {
		if pr.Attributes["component"] == "forwarding_rule" {
			ruleRate = pr.PricePerUnit
			break
		}
	}
	if abs(ruleRate-0.008) > 1e-9 {
		t.Errorf("fallback rule rate = %.6f, want 0.008000", ruleRate)
	}
}

// --------------------------------------------------------------------------
// Cloud CDN helpers and tests
// --------------------------------------------------------------------------

// fakeCDNSKUs returns SKUs matching the GCP Cloud CDN catalog.
// CDN egress: $0.02/GB, CDN fill: $0.01/GB.
func fakeCDNSKUs() []map[string]any {
	return []map[string]any{
		makeSKUMultiRegion(
			"Network CDN Cache Egress from North America",
			[]string{"us-central1", "global"},
			"0", 20_000_000, // $0.02
		),
		makeSKUMultiRegion(
			"Network CDN Cache Fill from North America to North America",
			[]string{"us-central1", "global"},
			"0", 10_000_000, // $0.01
		),
	}
}

// TestPriceNetworkCDN_EgressRateFromSKU verifies that the CDN egress rate is
// extracted from the GCP SKU (Network CDN Cache Egress → $0.02/GB).
func TestPriceNetworkCDN_EgressRateFromSKU(t *testing.T) {
	skus := fakeCDNSKUs()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(networkingSKUResponse(skus))
	}))
	defer ts.Close()

	p := newNetworkingTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.NetworkPricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Region: "us-central1",
		},
	}
	prices, breakdown, err := p.priceNetworkCDN(ctx, spec)
	if err != nil {
		t.Fatalf("priceNetworkCDN: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected at least one price")
	}

	// Find egress price (SKUID ends with ":egress").
	var egressRate float64
	for _, pr := range prices {
		if len(pr.SKUID) >= 7 && pr.SKUID[len(pr.SKUID)-7:] == ":egress" {
			egressRate = pr.PricePerUnit
			break
		}
	}
	if abs(egressRate-0.02) > 1e-9 {
		t.Errorf("CDN egress rate = %.6f, want 0.020000", egressRate)
	}

	// No fallback expected.
	if fb, ok := breakdown["fallback"]; ok && fb == true {
		t.Error("expected no fallback when CDN SKUs are present")
	}
}
