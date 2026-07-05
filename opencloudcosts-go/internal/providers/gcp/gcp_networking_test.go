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

// TestPriceNetworkCDN_CostMath verifies:
//
//	egress_gb=1000, cache_fill_gb=200
//	→ monthly_egress_cost     = 1000 * $0.02 = $20.00
//	→ monthly_cache_fill_cost = 200  * $0.01 = $2.00
//	→ monthly_total                           = $22.00
func TestPriceNetworkCDN_CostMath(t *testing.T) {
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
		EgressGB:    1000.0,
		CacheFillGB: 200.0,
	}
	_, breakdown, err := p.priceNetworkCDN(ctx, spec)
	if err != nil {
		t.Fatalf("priceNetworkCDN: %v", err)
	}

	// Egress cost: 1000 * $0.02 = $20.00
	wantEgress := 1000.0 * 0.02
	got := toFloat64(breakdown["monthly_egress_cost"])
	if abs(got-wantEgress) > 1e-6 {
		t.Errorf("monthly_egress_cost = %.4f, want %.4f", got, wantEgress)
	}

	// Cache fill cost: 200 * $0.01 = $2.00
	wantFill := 200.0 * 0.01
	got = toFloat64(breakdown["monthly_cache_fill_cost"])
	if abs(got-wantFill) > 1e-6 {
		t.Errorf("monthly_cache_fill_cost = %.4f, want %.4f", got, wantFill)
	}

	// Total: $20.00 + $2.00 = $22.00
	wantTotal := wantEgress + wantFill
	got = toFloat64(breakdown["monthly_total"])
	if abs(got-wantTotal) > 1e-6 {
		t.Errorf("monthly_total = %.4f, want %.4f", got, wantTotal)
	}
}

// --------------------------------------------------------------------------
// Cloud NAT helpers and tests
// --------------------------------------------------------------------------

// fakeNATSKUs returns SKUs matching the GCP Cloud NAT catalog.
// NAT gateway: $0.044/hr, NAT data: $0.045/GB.
func fakeNATSKUs() []map[string]any {
	return []map[string]any{
		makeSKUMultiRegion(
			"Cloud NAT Gateway",
			[]string{"us-central1"},
			"0", 44_000_000, // $0.044
		),
		makeSKUMultiRegion(
			"Cloud NAT Data Processed",
			[]string{"us-central1"},
			"0", 45_000_000, // $0.045
		),
	}
}

// TestPriceNetworkNAT_GatewayRateFromSKU verifies that the NAT gateway hourly
// rate is extracted from the GCP SKU (Cloud NAT Gateway → $0.044/hr).
func TestPriceNetworkNAT_GatewayRateFromSKU(t *testing.T) {
	skus := fakeNATSKUs()
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
		GatewayCount:  1,
		HoursPerMonth: 730,
	}
	prices, breakdown, err := p.priceNetworkNAT(ctx, spec)
	if err != nil {
		t.Fatalf("priceNetworkNAT: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected at least one price")
	}

	// Find the gateway price (SKUID ends with ":gateway").
	var gatewayRate float64
	for _, pr := range prices {
		if len(pr.SKUID) >= 8 && pr.SKUID[len(pr.SKUID)-8:] == ":gateway" {
			gatewayRate = pr.PricePerUnit
			break
		}
	}
	if abs(gatewayRate-0.044) > 1e-9 {
		t.Errorf("NAT gateway rate = %.6f, want 0.044000", gatewayRate)
	}

	// No fallback expected.
	if fb, ok := breakdown["fallback"]; ok && fb == true {
		t.Error("expected no fallback when NAT SKUs are present")
	}
}

// TestPriceNetworkNAT_CostMath verifies:
//
//	gateway_count=2, data_gb=500, hours=730
//	→ monthly_gateway_cost = 2 * $0.044 * 730 = $64.24
//	→ monthly_data_cost    = 500 * $0.045     = $22.50
//	→ monthly_total                            = $86.74
func TestPriceNetworkNAT_CostMath(t *testing.T) {
	skus := fakeNATSKUs()
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
		GatewayCount:  2,
		DataGB:        500.0,
		HoursPerMonth: 730.0,
	}
	_, breakdown, err := p.priceNetworkNAT(ctx, spec)
	if err != nil {
		t.Fatalf("priceNetworkNAT: %v", err)
	}

	// Gateway cost: 2 * $0.044 * 730 = $64.24
	wantGateway := 2.0 * 0.044 * 730.0
	got := toFloat64(breakdown["monthly_gateway_cost"])
	if abs(got-wantGateway) > 1e-6 {
		t.Errorf("monthly_gateway_cost = %.4f, want %.4f", got, wantGateway)
	}

	// Data cost: 500 * $0.045 = $22.50
	wantData := 500.0 * 0.045
	got = toFloat64(breakdown["monthly_data_cost"])
	if abs(got-wantData) > 1e-6 {
		t.Errorf("monthly_data_cost = %.4f, want %.4f", got, wantData)
	}

	// Total: $64.24 + $22.50 = $86.74
	wantTotal := wantGateway + wantData
	got = toFloat64(breakdown["monthly_total"])
	if abs(got-wantTotal) > 1e-6 {
		t.Errorf("monthly_total = %.4f, want %.4f", got, wantTotal)
	}
}

// --------------------------------------------------------------------------
// GetEgressPrice tests
// --------------------------------------------------------------------------

// TestGetEgressPrice_InternetAmericas verifies that internet egress from
// us-east1 returns the Americas tier-1 base rate ($0.08/GB).
// The httptest server returns empty SKUs, so the code falls back to the
// hardcoded Americas rate.
func TestGetEgressPrice_InternetAmericas(t *testing.T) {
	// Return no SKUs — forces fallback to gcpInternetEgressBaseRate["americas"].
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(networkingSKUResponse(nil))
	}))
	defer ts.Close()

	p := newNetworkingTestProvider(t, ts)
	ctx := context.Background()

	// us-east1 is in "americas" → fallback rate = 0.08.
	prices, err := p.GetEgressPrice(ctx, "us-east1", "", 0)
	if err != nil {
		t.Fatalf("GetEgressPrice: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected at least one price")
	}

	wantRate := gcpInternetEgressBaseRate["americas"] // 0.08
	if abs(prices[0].PricePerUnit-wantRate) > 1e-9 {
		t.Errorf("internet egress rate (Americas) = %.4f, want %.4f", prices[0].PricePerUnit, wantRate)
	}
	if prices[0].Service != "inter_region_egress" {
		t.Errorf("service = %q, want inter_region_egress", prices[0].Service)
	}
}

// TestGetEgressPrice_CrossContinent verifies that egress between us-east1
// (americas) and eu-west1 (emea) returns the cross-continent rate ($0.08/GB).
func TestGetEgressPrice_CrossContinent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(networkingSKUResponse(nil))
	}))
	defer ts.Close()

	p := newNetworkingTestProvider(t, ts)
	ctx := context.Background()

	// us-east1 (americas) → eu-west1 (emea): cross-continent rate = $0.08/GB.
	prices, err := p.GetEgressPrice(ctx, "us-east1", "eu-west1", 0)
	if err != nil {
		t.Fatalf("GetEgressPrice (cross-continent): %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected at least one price")
	}

	wantRate := gcpIntraEgressRate["cross"] // 0.08
	if abs(prices[0].PricePerUnit-wantRate) > 1e-9 {
		t.Errorf("cross-continent egress rate = %.4f, want %.4f", prices[0].PricePerUnit, wantRate)
	}
}

// TestGetEgressPrice_InternetUnit verifies that GetEgressPrice's internet
// egress (destRegion == "") is labeled as a flat per-GB price, not a
// per-GB-month recurring rate (issue #30(d): unit mislabeling regression).
func TestGetEgressPrice_InternetUnit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(networkingSKUResponse(nil))
	}))
	defer ts.Close()

	p := newNetworkingTestProvider(t, ts)
	ctx := context.Background()

	prices, err := p.GetEgressPrice(ctx, "us-east1", "", 0)
	if err != nil {
		t.Fatalf("GetEgressPrice: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected at least one price")
	}
	if prices[0].Unit != models.PriceUnitPerGB {
		t.Errorf("Unit = %v, want %v (internet egress must not be per_gb_month)", prices[0].Unit, models.PriceUnitPerGB)
	}
}

// TestGetEgressPrice_InterRegionUnit verifies that GetEgressPrice's
// inter-region egress (destRegion != "") is labeled as a flat per-GB price,
// not a per-GB-month recurring rate (issue #30(d): unit mislabeling regression).
func TestGetEgressPrice_InterRegionUnit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(networkingSKUResponse(nil))
	}))
	defer ts.Close()

	p := newNetworkingTestProvider(t, ts)
	ctx := context.Background()

	prices, err := p.GetEgressPrice(ctx, "us-east1", "eu-west1", 0)
	if err != nil {
		t.Fatalf("GetEgressPrice (inter-region): %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected at least one price")
	}
	if prices[0].Unit != models.PriceUnitPerGB {
		t.Errorf("Unit = %v, want %v (inter-region egress must not be per_gb_month)", prices[0].Unit, models.PriceUnitPerGB)
	}
}

// TestPriceNetworkEgress_InternetTieredUnit verifies that the tiered
// internet-egress path in priceNetworkEgress (the `default:` case for
// destination_type "internet") is labeled as a flat per-GB price, not a
// per-GB-month recurring rate (issue #30(d): unit mislabeling regression).
func TestPriceNetworkEgress_InternetTieredUnit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(networkingSKUResponse(nil))
	}))
	defer ts.Close()

	p := newNetworkingTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.NetworkPricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Region: "us-east1",
		},
		SourceRegion:    "us-east1",
		DestinationType: "internet",
		DataGBPerMonth:  100.0,
		NetworkTier:     "premium",
	}
	prices, _, err := p.priceNetworkEgress(ctx, spec)
	if err != nil {
		t.Fatalf("priceNetworkEgress: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected at least one price")
	}
	if prices[0].Unit != models.PriceUnitPerGB {
		t.Errorf("Unit = %v, want %v (tiered internet egress must not be per_gb_month)", prices[0].Unit, models.PriceUnitPerGB)
	}
}

// --------------------------------------------------------------------------
// Cloud Armor tests (Fix: fallback disclosure for missing policy/request rates)
// --------------------------------------------------------------------------

// fakeCloudArmorSKUs returns SKUs with both a policy-rate and a request-rate
// entry so priceNetworkArmor can extract both from the live catalog.
func fakeCloudArmorSKUs() []map[string]any {
	return []map[string]any{
		makeSKUMultiRegion(
			"Cloud Armor Security Policy",
			[]string{"global"},
			"0", 750_000_000, // $0.75
		),
		makeSKUMultiRegion(
			"Cloud Armor Request Evaluation",
			[]string{"global"},
			"0", 750_000_000, // $0.75
		),
	}
}

// TestPriceNetworkArmor_FetchError verifies that when the SKU server returns
// HTTP 500, priceNetworkArmor uses hardcoded fallback rates and sets
// breakdown["fallback"]=true and breakdown["fallback_note"] to a non-empty string.
func TestPriceNetworkArmor_FetchError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	p := newNetworkingTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.NetworkPricingSpec{
		BasePricingSpec: models.BasePricingSpec{Region: "global"},
		PolicyCount:     1,
	}
	_, breakdown, err := p.priceNetworkArmor(ctx, spec)
	if err != nil {
		t.Fatalf("priceNetworkArmor (fetch error): %v", err)
	}

	if fb, ok := breakdown["fallback"]; !ok || fb != true {
		t.Errorf("expected breakdown[\"fallback\"]=true on SKU fetch error, got %v (ok=%v)", fb, ok)
	}
	note, _ := breakdown["fallback_note"].(string)
	if note == "" {
		t.Error("expected non-empty breakdown[\"fallback_note\"] on SKU fetch error")
	}
}

// TestPriceNetworkArmor_PartialSKU verifies that when the SKU list contains
// only a policy-rate entry (no request-rate SKU), the missing request rate
// triggers fallback=true (previously undisclosed). This exercises the fix to
// broaden the fallback flag: `fallback = fallback || policyRate == 0 || requestRate == 0`.
func TestPriceNetworkArmor_PartialSKU(t *testing.T) {
	// Only provide the policy-rate SKU; omit the request-rate SKU.
	partialSKUs := []map[string]any{
		makeSKUMultiRegion(
			"Cloud Armor Security Policy",
			[]string{"global"},
			"0", 750_000_000, // $0.75
		),
		// No request-rate SKU → requestRate stays 0 → fallback should trigger.
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(networkingSKUResponse(partialSKUs))
	}))
	defer ts.Close()

	p := newNetworkingTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.NetworkPricingSpec{
		BasePricingSpec:         models.BasePricingSpec{Region: "global"},
		PolicyCount:             2,
		MonthlyRequestsMillions: 10,
	}
	_, breakdown, err := p.priceNetworkArmor(ctx, spec)
	if err != nil {
		t.Fatalf("priceNetworkArmor (partial SKU): %v", err)
	}

	if fb, ok := breakdown["fallback"]; !ok || fb != true {
		t.Errorf("expected breakdown[\"fallback\"]=true when request-rate SKU is missing, got %v (ok=%v)", fb, ok)
	}
	note, _ := breakdown["fallback_note"].(string)
	if note == "" {
		t.Error("expected non-empty breakdown[\"fallback_note\"] when request-rate SKU is missing")
	}
}

// TestPriceNetworkArmor_LiveSKU verifies that when the SKU list provides both
// a policy-rate and a request-rate entry, breakdown["fallback"] is NOT set
// (or is false) and breakdown["fallback_note"] is absent.
func TestPriceNetworkArmor_LiveSKU(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(networkingSKUResponse(fakeCloudArmorSKUs()))
	}))
	defer ts.Close()

	p := newNetworkingTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.NetworkPricingSpec{
		BasePricingSpec:         models.BasePricingSpec{Region: "global"},
		PolicyCount:             1,
		MonthlyRequestsMillions: 5,
	}
	_, breakdown, err := p.priceNetworkArmor(ctx, spec)
	if err != nil {
		t.Fatalf("priceNetworkArmor (live SKU): %v", err)
	}

	if fb := breakdown["fallback"]; fb == true {
		t.Error("expected breakdown[\"fallback\"] to be absent/false when both SKUs are present")
	}
	if note := breakdown["fallback_note"]; note != nil {
		t.Errorf("expected breakdown[\"fallback_note\"] to be absent, got %v", note)
	}
}
