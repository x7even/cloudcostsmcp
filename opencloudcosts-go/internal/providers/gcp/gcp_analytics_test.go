// Package gcp — unit tests for Cloud Armor and Cloud Monitoring pricing.
package gcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/cache"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/config"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// newTestProviderAnalytics constructs a *Provider backed by the given httptest.Server.
// It uses a temporary directory for the cache.
func newTestProviderAnalytics(t *testing.T, server *httptest.Server) *Provider {
	t.Helper()
	dir := t.TempDir()
	cm, err := cache.New(dir)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	cfg := &config.Config{
		GCPAPIKey:       "test-key",
		CacheTTLHours:   24,
		MetadataTTLDays: 7,
	}
	p := &Provider{
		cfg:        cfg,
		cache:      cm,
		auth:       newGCPAuthProvider(cfg),
		httpClient: server.Client(),
		baseURL:    server.URL,
	}
	return p
}

// makeGlobalSKU creates a minimal SKU dict with a single flat tier and global region.
func makeGlobalSKU(desc string, units string, nanos int) map[string]any {
	return map[string]any{
		"description":    desc,
		"serviceRegions": []any{"global"},
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

// --------------------------------------------------------------------------
// Cloud Armor tests
// --------------------------------------------------------------------------

// TestPriceNetworkArmor_CostMath verifies that Cloud Armor cost is computed as:
//
//	policies × policy_rate + monthly_requests_millions × request_rate
//
// With 3 policies at $0.75 and 50M requests at $0.75/M:
//
//	policy_cost = 3 × $0.75 = $2.25
//	request_cost = 50 × $0.75 = $37.50
//	total = $39.75
func TestPriceNetworkArmor_CostMath(t *testing.T) {
	armorPolicySKU := makeGlobalSKU("Cloud Armor Security Policy", "0", 750_000_000)     // $0.75
	armorRequestSKU := makeGlobalSKU("Cloud Armor Request Evaluation", "0", 750_000_000) // $0.75

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse([]map[string]any{armorPolicySKU, armorRequestSKU}))
	}))
	defer ts.Close()

	p := newTestProviderAnalytics(t, ts)
	spec := &models.NetworkPricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderGCP,
			Domain:   models.PricingDomainNetwork,
			Service:  "cloud_armor",
			Region:   "global",
		},
		PolicyCount:             3,
		MonthlyRequestsMillions: 50.0,
	}

	_, breakdown, err := p.priceNetworkArmor(context.Background(), spec)
	if err != nil {
		t.Fatalf("priceNetworkArmor: %v", err)
	}

	// policy_cost = 3 × $0.75 = $2.25
	policyCost, ok := breakdown["monthly_policy_cost"].(float64)
	if !ok {
		t.Fatalf("monthly_policy_cost missing or wrong type: %v", breakdown["monthly_policy_cost"])
	}
	if abs(policyCost-2.25) > 1e-9 {
		t.Errorf("monthly_policy_cost = %.4f, want 2.25", policyCost)
	}

	// request_cost = 50 × $0.75 = $37.50
	reqCost, ok := breakdown["monthly_request_cost"].(float64)
	if !ok {
		t.Fatalf("monthly_request_cost missing or wrong type: %v", breakdown["monthly_request_cost"])
	}
	if abs(reqCost-37.50) > 1e-9 {
		t.Errorf("monthly_request_cost = %.4f, want 37.50", reqCost)
	}

	// total = $39.75
	total, ok := breakdown["monthly_total"].(float64)
	if !ok {
		t.Fatalf("monthly_total missing or wrong type: %v", breakdown["monthly_total"])
	}
	if abs(total-39.75) > 1e-9 {
		t.Errorf("monthly_total = %.4f, want 39.75", total)
	}
}

// TestPriceNetworkArmor_FallbackOnFetchError verifies that when the SKU fetch fails,
// Cloud Armor uses hardcoded fallback rates ($0.75 for policy and request) and sets
// breakdown["fallback"] = true.
func TestPriceNetworkArmor_FallbackOnFetchError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return HTTP 500 to simulate a fetch failure.
		http.Error(w, "service unavailable", http.StatusInternalServerError)
	}))
	defer ts.Close()

	p := newTestProviderAnalytics(t, ts)
	spec := &models.NetworkPricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderGCP,
			Domain:   models.PricingDomainNetwork,
			Service:  "cloud_armor",
			Region:   "global",
		},
		PolicyCount:             1,
		MonthlyRequestsMillions: 0.0,
	}

	prices, breakdown, err := p.priceNetworkArmor(context.Background(), spec)
	if err != nil {
		t.Fatalf("priceNetworkArmor: unexpected error: %v", err)
	}

	// Must set fallback = true.
	fallback, ok := breakdown["fallback"].(bool)
	if !ok || !fallback {
		t.Errorf("expected breakdown['fallback'] = true, got %v", breakdown["fallback"])
	}

	// Fallback policy rate should be $0.75.
	if len(prices) == 0 {
		t.Fatal("expected at least one price entry")
	}
	var policyPrice *models.NormalizedPrice
	for i := range prices {
		if strings.Contains(prices[i].SKUID, "policy") {
			policyPrice = &prices[i]
			break
		}
	}
	if policyPrice == nil {
		t.Fatal("no policy price found in returned prices")
	}
	if abs(policyPrice.PricePerUnit-0.75) > 1e-9 {
		t.Errorf("fallback policy rate = %.4f, want 0.75", policyPrice.PricePerUnit)
	}

	// Fallback request rate should be $0.75.
	var requestPrice *models.NormalizedPrice
	for i := range prices {
		if strings.Contains(prices[i].SKUID, "request") {
			requestPrice = &prices[i]
			break
		}
	}
	if requestPrice == nil {
		t.Fatal("no request price found in returned prices")
	}
	if abs(requestPrice.PricePerUnit-0.75) > 1e-9 {
		t.Errorf("fallback request rate = %.4f, want 0.75", requestPrice.PricePerUnit)
	}
}

// --------------------------------------------------------------------------
// Cloud Monitoring (Observability) tests
// --------------------------------------------------------------------------

// TestPriceObservability_TieredCostSmall verifies that small ingestion volumes
// (below 100K MiB/month) are priced using the tier-1 rate only.
//
// With ingestion_mib=200.0:
//
//	free tier = 150 MiB
//	billable = 200 - 150 = 50 MiB
//	cost = 50 × $0.258 = $12.90
func TestPriceObservability_TieredCostSmall(t *testing.T) {
	monitoringSKU := makeGlobalSKU("Cloud Monitoring Metric Ingestion", "0", 258_000_000) // $0.258/MiB

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse([]map[string]any{monitoringSKU}))
	}))
	defer ts.Close()

	p := newTestProviderAnalytics(t, ts)
	spec := &models.ObservabilityPricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderGCP,
			Domain:   models.PricingDomainObservability,
			Service:  "cloud_monitoring",
			Region:   "global",
		},
		IngestionMiB: 200.0,
	}

	_, breakdown, err := p.priceObservability(context.Background(), spec)
	if err != nil {
		t.Fatalf("priceObservability: %v", err)
	}

	// 200 - 150 free = 50 MiB billable × $0.258 = $12.90
	// Go formats as "$12.9000/month"
	costStr, ok := breakdown["estimated_monthly_cost"].(string)
	if !ok {
		t.Fatalf("estimated_monthly_cost missing or wrong type: %v", breakdown["estimated_monthly_cost"])
	}
	expected := fmt.Sprintf("$%.4f/month", 50.0*0.258)
	if costStr != expected {
		t.Errorf("estimated_monthly_cost = %q, want %q", costStr, expected)
	}
}

// TestPriceObservability_TieredCostLarge verifies that large ingestion volumes
// correctly cross the tier-1 boundary into tier-2.
//
// With ingestion_mib=150000.0 (using fallback rates):
//
//	free tier = 150 MiB
//	billable = 150000 - 150 = 149850 MiB
//	tier1 = 100000 × $0.258 = $25800.00
//	tier2 = 49850 × $0.151 = $7527.35
//	total = $33327.35
func TestPriceObservability_TieredCostLarge(t *testing.T) {
	// Use an HTTP 500 to force fallback rates so the test is deterministic.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service unavailable", http.StatusInternalServerError)
	}))
	defer ts.Close()

	p := newTestProviderAnalytics(t, ts)
	spec := &models.ObservabilityPricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderGCP,
			Domain:   models.PricingDomainObservability,
			Service:  "cloud_monitoring",
			Region:   "global",
		},
		IngestionMiB: 150000.0,
	}

	_, breakdown, err := p.priceObservability(context.Background(), spec)
	if err != nil {
		t.Fatalf("priceObservability: %v", err)
	}

	// billable = 149850 MiB
	// tier1 = 100000 × 0.258 = 25800.00
	// tier2 = 49850 × 0.151 = 7527.35
	// total = 33327.35
	billable := 150000.0 - cloudMonitoringFreeTierMiB // 149850
	tier1Amount := cloudMonitoringT1Limit             // 100000
	tier2Amount := billable - tier1Amount             // 49850
	expectedCost := tier1Amount*cloudMonitoringT1Rate + tier2Amount*cloudMonitoringT2Rate

	costStr, ok := breakdown["estimated_monthly_cost"].(string)
	if !ok {
		t.Fatalf("estimated_monthly_cost missing or wrong type: %v", breakdown["estimated_monthly_cost"])
	}
	expected := fmt.Sprintf("$%.4f/month", expectedCost)
	if costStr != expected {
		t.Errorf("estimated_monthly_cost = %q, want %q", costStr, expected)
	}

	// Must use fallback rates.
	fallback, ok := breakdown["fallback"].(bool)
	if !ok || !fallback {
		t.Errorf("expected breakdown['fallback'] = true, got %v", breakdown["fallback"])
	}
}

// TestPriceObservability_FallbackOnFetchError verifies that when the SKU fetch fails,
// Cloud Monitoring uses fallback rates and sets breakdown["fallback"] = true,
// with the expected free tier and tier rate strings present.
func TestPriceObservability_FallbackOnFetchError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service not found", http.StatusInternalServerError)
	}))
	defer ts.Close()

	p := newTestProviderAnalytics(t, ts)
	spec := &models.ObservabilityPricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderGCP,
			Domain:   models.PricingDomainObservability,
			Service:  "cloud_monitoring",
			Region:   "global",
		},
		IngestionMiB: 0.0,
	}

	_, breakdown, err := p.priceObservability(context.Background(), spec)
	if err != nil {
		t.Fatalf("priceObservability: unexpected error: %v", err)
	}

	// Must set fallback = true.
	fallback, ok := breakdown["fallback"].(bool)
	if !ok || !fallback {
		t.Errorf("expected breakdown['fallback'] = true, got %v", breakdown["fallback"])
	}

	// free_tier_mib must be present and equal to 150.
	freeTier, ok := breakdown["free_tier_mib"].(float64)
	if !ok {
		t.Fatalf("free_tier_mib missing or wrong type: %v", breakdown["free_tier_mib"])
	}
	if abs(freeTier-150.0) > 1e-9 {
		t.Errorf("free_tier_mib = %v, want 150", freeTier)
	}

	// Tier rates must contain the expected values.
	tier1Str, ok := breakdown["tier1_rate_per_mib"].(string)
	if !ok {
		t.Fatalf("tier1_rate_per_mib missing or wrong type: %v", breakdown["tier1_rate_per_mib"])
	}
	if !strings.Contains(tier1Str, "0.258") {
		t.Errorf("tier1_rate_per_mib = %q, want to contain '0.258'", tier1Str)
	}

	tier2Str, ok := breakdown["tier2_rate_per_mib"].(string)
	if !ok {
		t.Fatalf("tier2_rate_per_mib missing or wrong type: %v", breakdown["tier2_rate_per_mib"])
	}
	if !strings.Contains(tier2Str, "0.151") {
		t.Errorf("tier2_rate_per_mib = %q, want to contain '0.151'", tier2Str)
	}

	tier3Str, ok := breakdown["tier3_rate_per_mib"].(string)
	if !ok {
		t.Fatalf("tier3_rate_per_mib missing or wrong type: %v", breakdown["tier3_rate_per_mib"])
	}
	if !strings.Contains(tier3Str, "0.062") {
		t.Errorf("tier3_rate_per_mib = %q, want to contain '0.062'", tier3Str)
	}
}
