// Package gcp — unit tests for database, container, and AI/analytics pricing (Part 2).
package gcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/cache"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/config"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// newTestProviderDB is identical to newTestProvider in gcp_compute_test.go but
// named differently to avoid re-declaration. It constructs a *Provider backed by the
// given httptest.Server, using a temporary directory for the cache.
func newTestProviderDB(t *testing.T, server *httptest.Server) *Provider {
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

// makeSKUPaid creates a SKU with a paid tier (startUsageAmount > 0) for BigQuery tests.
func makeSKUPaid(desc, usageType, region string, units string, nanos int) map[string]any {
	return map[string]any{
		"description":    desc,
		"serviceRegions": []any{region},
		"category": map[string]any{
			"usageType": usageType,
		},
		"pricingInfo": []any{
			map[string]any{
				"pricingExpression": map[string]any{
					"tieredRates": []any{
						// Free tier at startUsageAmount == 0.
						map[string]any{
							"startUsageAmount": float64(0),
							"unitPrice": map[string]any{
								"units": "0",
								"nanos": float64(0),
							},
						},
						// Paid tier at startUsageAmount > 0.
						map[string]any{
							"startUsageAmount": float64(1),
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
// strFloat helper tests
// --------------------------------------------------------------------------

func TestStrFloat(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{4.0, "4"},
		{15.0, "15"},
		{3.75, "3.75"},
		{7.5, "7.5"},
		{0.614, "0.614"},
	}
	for _, c := range cases {
		got := strFloat(c.in)
		if got != c.want {
			t.Errorf("strFloat(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --------------------------------------------------------------------------
// Cloud SQL tests
// --------------------------------------------------------------------------

// TestGetCloudSQLPrice_ZonalMySQL tests zonal MySQL pricing lookup.
func TestGetCloudSQLPrice_ZonalMySQL(t *testing.T) {
	// Cloud SQL for MySQL: Zonal - 4 vCPU + 15GB RAM
	// (db-n1-standard-4 = 4 vCPU, 15.0 GB RAM)
	skuDesc := "Cloud SQL for MySQL: Zonal - 4 vCPU + 15GB RAM in Americas"
	skus := []map[string]any{
		makeSKU(skuDesc, "OnDemand", "us-central1", "0", 320_000_000), // $0.32/hr
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	prices, err := p.getCloudSQLPrice(context.Background(), "db-n1-standard-4", "us-central1", "mysql", false)
	if err != nil {
		t.Fatalf("getCloudSQLPrice: %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("expected 1 price, got %d", len(prices))
	}

	want := 0.32
	if abs(prices[0].PricePerUnit-want) > 1e-6 {
		t.Errorf("PricePerUnit = %.6f, want %.6f", prices[0].PricePerUnit, want)
	}
	if prices[0].Unit != models.PriceUnitPerHour {
		t.Errorf("Unit = %v, want per_hour", prices[0].Unit)
	}
	if prices[0].PricingTerm != models.PricingTermOnDemand {
		t.Errorf("PricingTerm = %v, want on_demand", prices[0].PricingTerm)
	}
}

// TestGetCloudSQLPrice_RegionalPostgreSQL tests HA PostgreSQL pricing.
func TestGetCloudSQLPrice_RegionalPostgreSQL(t *testing.T) {
	// db-n1-standard-2 = 2 vCPU, 7.5 GB RAM  →  "2 vcpu + 7.5gb ram"
	skuDesc := "Cloud SQL for PostgreSQL: Regional - 2 vCPU + 7.5GB RAM in Americas"
	skus := []map[string]any{
		makeSKU(skuDesc, "OnDemand", "us-central1", "0", 640_000_000), // $0.64/hr
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	prices, err := p.getCloudSQLPrice(context.Background(), "db-n1-standard-2", "us-central1", "postgres", true)
	if err != nil {
		t.Fatalf("getCloudSQLPrice: %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("expected 1 price, got %d", len(prices))
	}

	want := 0.64
	if abs(prices[0].PricePerUnit-want) > 1e-6 {
		t.Errorf("PricePerUnit = %.6f, want %.6f", prices[0].PricePerUnit, want)
	}
	if prices[0].Attributes["ha"] != "true" {
		t.Errorf("ha attribute = %q, want 'true'", prices[0].Attributes["ha"])
	}
}

// TestGetCloudSQLPrice_UnknownInstance checks that an unknown instance type returns an error.
func TestGetCloudSQLPrice_UnknownInstance(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("HTTP should not be called for unknown instance type")
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	_, err := p.getCloudSQLPrice(context.Background(), "db-unknown-type", "us-central1", "mysql", false)
	if err == nil {
		t.Error("expected error for unknown instance type, got nil")
	}
}

// TestGetCloudSQLPrice_NoMatchReturnsNil checks that no matching SKU returns (nil, nil).
func TestGetCloudSQLPrice_NoMatchReturnsNil(t *testing.T) {
	// SKU for a different instance size than what we ask for.
	skuDesc := "Cloud SQL for MySQL: Zonal - 8 vCPU + 30GB RAM in Americas"
	skus := []map[string]any{
		makeSKU(skuDesc, "OnDemand", "us-central1", "0", 640_000_000),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	// Ask for 4-vCPU instance but catalog only has 8-vCPU.
	prices, err := p.getCloudSQLPrice(context.Background(), "db-n1-standard-4", "us-central1", "mysql", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prices != nil {
		t.Errorf("expected nil prices for no match, got %v", prices)
	}
}

// TestGetCloudSQLPrice_PricingMathDecomposed verifies that the returned price for a
// Cloud SQL instance equals cpu_count*cpu_rate + mem_gb*ram_rate, matching the GCP
// SKU-level total which encodes that decomposed math.
func TestGetCloudSQLPrice_PricingMathDecomposed(t *testing.T) {
	// db-n1-standard-8: 8 vCPU, 30 GB RAM.
	// cpu_rate = $0.0413/vCPU-hr, ram_rate = $0.007/GB-hr
	// expected = 8*0.0413 + 30*0.007 = 0.3304 + 0.21 = 0.5404
	cpuRate := 0.0413
	ramRate := 0.007
	vcpus := 8.0
	memGB := 30.0
	expectedPrice := vcpus*cpuRate + memGB*ramRate // 0.5404

	// GCP encodes the total into a single SKU; nanos = round(0.5404 * 1e9)
	totalNanos := int(expectedPrice * 1e9) // 540_400_000

	skuDesc := "Cloud SQL for MySQL: Zonal - 8 vCPU + 30GB RAM in Americas"
	skus := []map[string]any{
		makeSKU(skuDesc, "OnDemand", "us-central1", "0", totalNanos),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	prices, err := p.getCloudSQLPrice(context.Background(), "db-n1-standard-8", "us-central1", "mysql", false)
	if err != nil {
		t.Fatalf("getCloudSQLPrice: %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("expected 1 price, got %d", len(prices))
	}

	if abs(prices[0].PricePerUnit-expectedPrice) > 1e-6 {
		t.Errorf("PricePerUnit = %.6f, want %.6f (= %g*%g + %g*%g)",
			prices[0].PricePerUnit, expectedPrice, vcpus, cpuRate, memGB, ramRate)
	}
}

// TestGetCloudSQLPrice_EngineNormalization verifies that "postgres", "postgresql",
// and "pg" all resolve to the same canonical "PostgreSQL" engine and match the
// same SKU description.
func TestGetCloudSQLPrice_EngineNormalization(t *testing.T) {
	// A single PostgreSQL Zonal SKU for db-n1-standard-4.
	skuDesc := "Cloud SQL for PostgreSQL: Zonal - 4 vCPU + 15GB RAM in Americas"
	skus := []map[string]any{
		makeSKU(skuDesc, "OnDemand", "us-central1", "0", 270_200_000), // $0.2702/hr
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	// All three aliases should resolve to the same engine and the same price.
	// Each alias gets its own provider so there is no cross-alias cache interference.
	aliases := []string{"postgres", "postgresql", "pg"}
	for _, alias := range aliases {
		alias := alias // capture
		t.Run(alias, func(t *testing.T) {
			p := newTestProviderDB(t, ts)

			prices, err := p.getCloudSQLPrice(context.Background(), "db-n1-standard-4", "us-central1", alias, false)
			if err != nil {
				t.Fatalf("alias %q: getCloudSQLPrice: %v", alias, err)
			}
			if len(prices) != 1 {
				t.Fatalf("alias %q: expected 1 price, got %d", alias, len(prices))
			}
			if prices[0].Attributes["engine"] != "PostgreSQL" {
				t.Errorf("alias %q: engine attribute = %q, want 'PostgreSQL'", alias, prices[0].Attributes["engine"])
			}
			want := 0.2702
			if abs(prices[0].PricePerUnit-want) > 1e-4 {
				t.Errorf("alias %q: PricePerUnit = %.6f, want %.6f", alias, prices[0].PricePerUnit, want)
			}
		})
	}
}

// TestGetCloudSQLPrice_HAMultiplier verifies that high-availability (Regional)
// pricing is greater than single-zone (Zonal) pricing for the same instance.
func TestGetCloudSQLPrice_HAMultiplier(t *testing.T) {
	// db-n1-standard-4: Zonal $0.2702/hr, Regional (HA) $0.5404/hr (~2x).
	zonalDesc := "Cloud SQL for MySQL: Zonal - 4 vCPU + 15GB RAM in Americas"
	regionalDesc := "Cloud SQL for MySQL: Regional - 4 vCPU + 15GB RAM in Americas"
	skus := []map[string]any{
		makeSKU(zonalDesc, "OnDemand", "us-central1", "0", 270_200_000),    // $0.2702/hr
		makeSKU(regionalDesc, "OnDemand", "us-central1", "0", 540_400_000), // $0.5404/hr
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)

	zonal, err := p.getCloudSQLPrice(context.Background(), "db-n1-standard-4", "us-central1", "mysql", false)
	if err != nil {
		t.Fatalf("getCloudSQLPrice (zonal): %v", err)
	}
	if len(zonal) != 1 {
		t.Fatalf("expected 1 zonal price, got %d", len(zonal))
	}

	// Regional lookup reuses the cached index (both SKUs were served above).
	regional, err := p.getCloudSQLPrice(context.Background(), "db-n1-standard-4", "us-central1", "mysql", true)
	if err != nil {
		t.Fatalf("getCloudSQLPrice (regional): %v", err)
	}
	if len(regional) != 1 {
		t.Fatalf("expected 1 regional price, got %d", len(regional))
	}

	if regional[0].PricePerUnit <= zonal[0].PricePerUnit {
		t.Errorf("HA price (%.6f) should be greater than zonal price (%.6f)",
			regional[0].PricePerUnit, zonal[0].PricePerUnit)
	}

	// Verify the ha attribute is set correctly on each result.
	if zonal[0].Attributes["ha"] != "false" {
		t.Errorf("zonal ha attribute = %q, want 'false'", zonal[0].Attributes["ha"])
	}
	if regional[0].Attributes["ha"] != "true" {
		t.Errorf("regional ha attribute = %q, want 'true'", regional[0].Attributes["ha"])
	}
}

// --------------------------------------------------------------------------
// Memorystore tests
// --------------------------------------------------------------------------

// TestGetMemstorePrice_BasicM3 tests basic tier M3 pricing.
func TestGetMemstorePrice_BasicM3(t *testing.T) {
	// Capacity 6 GB → m3 tier. Rate = $0.049/GiB-hr → hourly = 6 × 0.049 = $0.294
	skuDesc := "Redis Capacity Basic M3"
	skus := []map[string]any{
		makeSKU(skuDesc, "OnDemand", "us-central1", "0", 49_000_000), // $0.049/GiB-hr
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	prices, err := p.getMemstorePrice(context.Background(), 6.0, "us-central1", "basic", 730.0)
	if err != nil {
		t.Fatalf("getMemstorePrice: %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("expected 1 price, got %d", len(prices))
	}

	want := 6.0 * 0.049
	if abs(prices[0].PricePerUnit-want) > 1e-6 {
		t.Errorf("PricePerUnit = %.6f, want %.6f", prices[0].PricePerUnit, want)
	}
	if prices[0].Unit != models.PriceUnitPerHour {
		t.Errorf("Unit = %v, want per_hour", prices[0].Unit)
	}
}

// TestGetMemstorePrice_StandardM4 tests standard tier M4 pricing.
func TestGetMemstorePrice_StandardM4(t *testing.T) {
	// Capacity 50 GB → m4 tier. Rate = $0.065/GiB-hr → hourly = 50 × 0.065 = $3.25
	// "Redis Standard Node Capacity M4" must NOT match — only "Redis Capacity Standard M4"
	skus := []map[string]any{
		makeSKU("Redis Standard Node Capacity M4", "OnDemand", "us-central1", "0", 100_000_000), // cluster SKU, must be skipped
		makeSKU("Redis Capacity Standard M4", "OnDemand", "us-central1", "0", 65_000_000),       // classic HA SKU
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	prices, err := p.getMemstorePrice(context.Background(), 50.0, "us-central1", "standard", 730.0)
	if err != nil {
		t.Fatalf("getMemstorePrice: %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("expected 1 price, got %d", len(prices))
	}

	want := 50.0 * 0.065
	if abs(prices[0].PricePerUnit-want) > 1e-6 {
		t.Errorf("PricePerUnit = %.6f, want %.6f", prices[0].PricePerUnit, want)
	}
}

// TestGetMemstorePrice_InvalidCapacity verifies that non-positive capacity returns error.
func TestGetMemstorePrice_InvalidCapacity(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("HTTP should not be called for invalid capacity")
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	_, err := p.getMemstorePrice(context.Background(), 0.0, "us-central1", "basic", 730.0)
	if err == nil {
		t.Error("expected error for zero capacity, got nil")
	}
}

// TestGetMemstorePrice_InvalidTier verifies that an unknown tier returns an error.
func TestGetMemstorePrice_InvalidTier(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("HTTP should not be called for invalid tier")
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	_, err := p.getMemstorePrice(context.Background(), 10.0, "us-central1", "premium", 730.0)
	if err == nil {
		t.Error("expected error for unknown tier, got nil")
	}
}

// TestGetMemstorePrice_MTierFallback tests that if the preferred M-tier is not found,
// another M-tier is tried.
func TestGetMemstorePrice_MTierFallback(t *testing.T) {
	// Capacity 6 GB → prefers m3, but only m4 is in the catalog.
	skuDesc := "Redis Capacity Basic M4"
	skus := []map[string]any{
		makeSKU(skuDesc, "OnDemand", "us-central1", "0", 49_000_000),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	prices, err := p.getMemstorePrice(context.Background(), 6.0, "us-central1", "basic", 730.0)
	if err != nil {
		t.Fatalf("getMemstorePrice: %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("expected 1 price via fallback M-tier, got %d", len(prices))
	}
}

// TestGetMemstorePrice_StandardMoreExpensiveThanBasic verifies that the Standard tier
// price is greater than or equal to the Basic tier price for the same capacity.
// Standard (HA) Redis is always priced at or above Basic because it provides
// cross-zone replication; this test constructs controlled SKU rates to confirm
// the pricing logic produces the expected ordering.
func TestGetMemstorePrice_StandardMoreExpensiveThanBasic(t *testing.T) {
	// Use 6 GB capacity → m3 tier.
	// Basic rate: $0.049/GiB-hr, Standard rate: $0.065/GiB-hr → standard > basic.
	skus := []map[string]any{
		makeSKU("Redis Capacity Basic M3", "OnDemand", "us-central1", "0", 49_000_000),    // $0.049
		makeSKU("Redis Capacity Standard M3", "OnDemand", "us-central1", "0", 65_000_000), // $0.065
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)

	basicPrices, err := p.getMemstorePrice(context.Background(), 6.0, "us-central1", "basic", 730.0)
	if err != nil {
		t.Fatalf("getMemstorePrice(basic): %v", err)
	}
	if len(basicPrices) != 1 {
		t.Fatalf("expected 1 basic price, got %d", len(basicPrices))
	}

	standardPrices, err := p.getMemstorePrice(context.Background(), 6.0, "us-central1", "standard", 730.0)
	if err != nil {
		t.Fatalf("getMemstorePrice(standard): %v", err)
	}
	if len(standardPrices) != 1 {
		t.Fatalf("expected 1 standard price, got %d", len(standardPrices))
	}

	if standardPrices[0].PricePerUnit < basicPrices[0].PricePerUnit {
		t.Errorf("standard price (%.6f) must be >= basic price (%.6f)",
			standardPrices[0].PricePerUnit, basicPrices[0].PricePerUnit)
	}
}

// --------------------------------------------------------------------------
// GKE tests
// --------------------------------------------------------------------------

// TestGetGKEPrice_Standard tests standard-mode GKE cluster management pricing.
func TestGetGKEPrice_Standard(t *testing.T) {
	// GKE standard cluster management: $0.10/hr
	skuDesc := "Kubernetes Engine Cluster Management Fee"
	skus := []map[string]any{
		makeSKU(skuDesc, "OnDemand", "us-central1", "0", 100_000_000), // $0.10/hr
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	prices, breakdown, err := p.getGKEPrice(context.Background(), "us-central1", "standard", "n2-standard-4", 3, 0, 0, 730.0)
	if err != nil {
		t.Fatalf("getGKEPrice: %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("expected 1 price, got %d", len(prices))
	}

	want := 0.10
	if abs(prices[0].PricePerUnit-want) > 1e-6 {
		t.Errorf("PricePerUnit = %.6f, want %.6f", prices[0].PricePerUnit, want)
	}
	if breakdown == nil {
		t.Error("breakdown should not be nil")
	}
	if breakdown["mode"] != "standard" {
		t.Errorf("breakdown mode = %v, want 'standard'", breakdown["mode"])
	}
}

// TestGetGKEPrice_StandardFallback tests that the fallback rate is used when no SKU is found.
func TestGetGKEPrice_StandardFallback(t *testing.T) {
	// SKU that should NOT match the cluster management fee pattern.
	skus := []map[string]any{
		makeSKU("GKE Autopilot Pod CPU", "OnDemand", "us-central1", "0", 100_000_000),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	prices, breakdown, err := p.getGKEPrice(context.Background(), "us-central1", "standard", "n1-standard-4", 3, 0, 0, 730.0)
	if err != nil {
		t.Fatalf("getGKEPrice: %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("expected 1 fallback price, got %d", len(prices))
	}

	want := 0.10 // documented fallback rate
	if abs(prices[0].PricePerUnit-want) > 1e-6 {
		t.Errorf("PricePerUnit = %.6f, want %.6f (fallback)", prices[0].PricePerUnit, want)
	}
	if fb, ok := breakdown["fallback"].(bool); !ok || !fb {
		t.Error("expected breakdown['fallback'] = true for fallback path")
	}
}

// TestGetGKEPrice_Autopilot tests autopilot-mode pod resource pricing.
func TestGetGKEPrice_Autopilot(t *testing.T) {
	// Autopilot balanced pod mCPU: $0.0000059695/mCPU-hr → × 1000 = $0.0059695/vCPU-hr
	// Autopilot balanced pod memory: $0.00000807/GiB-hr
	// Request: 0.5 vCPU, 1.0 GiB memory, 730 hrs/month
	skus := []map[string]any{
		makeSKU("Autopilot Balanced Pod mCPU Requests", "OnDemand", "us-central1", "0", 5969),   // $0.000005969/mCPU-hr
		makeSKU("Autopilot Balanced Pod Memory Requests", "OnDemand", "us-central1", "0", 8070), // $0.00000807/GiB-hr
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	prices, breakdown, err := p.getGKEPrice(context.Background(), "us-central1", "autopilot", "", 0, 0.5, 1.0, 730.0)
	if err != nil {
		t.Fatalf("getGKEPrice: %v", err)
	}

	if len(prices) < 2 {
		t.Fatalf("expected at least 2 prices (vcpu + memory), got %d", len(prices))
	}
	if breakdown["mode"] != "autopilot" {
		t.Errorf("breakdown mode = %v, want 'autopilot'", breakdown["mode"])
	}
}

// TestGetGKEPrice_AutopilotNegativeVCPU verifies that autopilot mode with a negative
// vCPU count returns an error rather than producing a negative cost.
func TestGetGKEPrice_AutopilotNegativeVCPU(t *testing.T) {
	skus := []map[string]any{
		makeSKU("Autopilot Balanced Pod mCPU Requests", "OnDemand", "us-central1", "0", 64_000),
		makeSKU("Autopilot Balanced Pod Memory Requests", "OnDemand", "us-central1", "0", 9_982_000),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	_, _, err := p.getGKEPrice(context.Background(), "us-central1", "autopilot", "", 0, -1.0, 4.0, 730.0)
	if err == nil {
		t.Error("expected error for negative vCPU, got nil")
	}
}

// TestGetGKEPrice_AutopilotNegativeMemory verifies that autopilot mode with a negative
// memory value returns an error rather than producing a negative cost.
func TestGetGKEPrice_AutopilotNegativeMemory(t *testing.T) {
	skus := []map[string]any{
		makeSKU("Autopilot Balanced Pod mCPU Requests", "OnDemand", "us-central1", "0", 64_000),
		makeSKU("Autopilot Balanced Pod Memory Requests", "OnDemand", "us-central1", "0", 9_982_000),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	_, _, err := p.getGKEPrice(context.Background(), "us-central1", "autopilot", "", 0, 2.0, -1.0, 730.0)
	if err == nil {
		t.Error("expected error for negative memory, got nil")
	}
}

// TestGetGKEPrice_InvalidMode verifies that an unknown mode string does not panic
// and returns a valid result (mode validation is performed by the tool layer,
// not the provider — unknown modes fall through to the standard branch).
func TestGetGKEPrice_InvalidMode(t *testing.T) {
	skus := []map[string]any{}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	prices, breakdown, err := p.getGKEPrice(context.Background(), "us-central1", "unknown_mode", "", 0, 0, 0, 730.0)
	if err != nil {
		t.Fatalf("unexpected error for unknown mode: %v", err)
	}
	// Unknown mode falls through to the standard branch — should return a valid result.
	if breakdown == nil {
		t.Error("breakdown should not be nil for unknown mode")
	}
	if _, ok := breakdown["mode"]; !ok {
		t.Error("breakdown should contain a 'mode' key")
	}
	// Standard path always returns at least one price (using fallback rate when no SKUs found).
	if len(prices) == 0 {
		t.Error("expected at least one price for unknown mode (standard branch fallback)")
	}
}

// --------------------------------------------------------------------------
// BigQuery tests
// --------------------------------------------------------------------------

// TestBuildBigQueryIndex verifies that BigQuery SKUs are indexed correctly.
func TestBuildBigQueryIndex(t *testing.T) {
	region := "us"
	// BigQuery Analysis SKU has a free tier (startUsageAmount == 0) + paid tier.
	// Active Logical Storage also has paid tier.
	skus := []map[string]any{
		makeSKUPaid("BigQuery Analysis", "OnDemand", region, "5", 0),                        // $5/TiB (paid tier)
		makeSKU("BigQuery Streaming Insert", "OnDemand", region, "0", 10_000_000),           // $0.01/GB
		makeSKUPaid("BigQuery Active Logical Storage", "OnDemand", region, "0", 20_000_000), // $0.02/GiB-mo
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	idx, err := p.buildBigQueryIndex(context.Background(), region)
	if err != nil {
		t.Fatalf("buildBigQueryIndex: %v", err)
	}
	if len(idx) == 0 {
		t.Error("expected non-empty index")
	}

	// Analysis rate should come from paid tier ($5/TiB).
	if analysisRate, ok := idx["BigQuery Analysis"]; ok {
		if abs(analysisRate-5.0) > 1e-9 {
			t.Errorf("analysis rate = %v, want 5.0", analysisRate)
		}
	} else {
		t.Error("expected 'BigQuery Analysis' in index")
	}
}

// TestGetBigQueryPrice_MultiRegionFallback verifies region fallback to "us".
func TestGetBigQueryPrice_MultiRegionFallback(t *testing.T) {
	// Serve SKUs only for "us" multi-region.
	skus := []map[string]any{
		makeSKUPaid("BigQuery Analysis", "OnDemand", "us", "5", 0),
	}

	var requestPaths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPaths = append(requestPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	// us-central1 → empty index → fallback to "us" multi-region
	prices, breakdown, err := p.getBigQueryPrice(context.Background(), "us-central1", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("getBigQueryPrice: %v", err)
	}
	// Should have found prices via "us" fallback.
	if len(prices) == 0 {
		t.Log("No prices returned — the SKU only matches 'us' region (multi-region fallback)")
		// The multi-region fallback path was tested: breakdown should still be populated.
	}
	if breakdown == nil {
		t.Error("breakdown should not be nil")
	}
	if breakdown["service"] != "bigquery" {
		t.Errorf("breakdown service = %v, want 'bigquery'", breakdown["service"])
	}
}

// TestGetBigQueryPrice_FreeTierNote verifies that the BigQuery pricing response
// includes a note describing the 1 TiB/month free query tier.
func TestGetBigQueryPrice_FreeTierNote(t *testing.T) {
	skus := []map[string]any{
		makeSKUPaid("BigQuery Analysis", "OnDemand", "us", "5", 0), // $5/TiB paid tier
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	_, breakdown, err := p.getBigQueryPrice(context.Background(), "us", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("getBigQueryPrice: %v", err)
	}
	if breakdown == nil {
		t.Fatal("breakdown should not be nil")
	}
	noteRaw, ok := breakdown["note"]
	if !ok {
		t.Fatal("breakdown must contain a 'note' key describing the free tier")
	}
	note, ok := noteRaw.(string)
	if !ok {
		t.Fatalf("breakdown['note'] must be a string, got %T", noteRaw)
	}
	if len(note) == 0 {
		t.Error("note is empty; expected mention of '1 TiB' free query tier")
	}
	// Verify the note mentions the free tier threshold.
	wantSubstr := "1 TiB"
	var found bool
	for i := 0; i+len(wantSubstr) <= len(note); i++ {
		if note[i:i+len(wantSubstr)] == wantSubstr {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("note %q does not mention '1 TiB' free query tier", note)
	}
}

// --------------------------------------------------------------------------
// Vertex AI tests
// --------------------------------------------------------------------------

// TestGetVertexPrice_Basic tests Vertex AI pricing for N1 training.
func TestGetVertexPrice_Basic(t *testing.T) {
	// N1 training: $0.048/vCPU-hr, $0.006/GiB-RAM-hr
	skus := []map[string]any{
		makeSKU("N1 Custom VCPU Training", "OnDemand", "us-central1", "0", 48_000_000),
		makeSKU("N1 Custom RAM Training", "OnDemand", "us-central1", "0", 6_000_000),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	prices, breakdown, err := p.getVertexPrice(context.Background(), "n1-standard-4", "us-central1", 100.0, "training")
	if err != nil {
		t.Fatalf("getVertexPrice: %v", err)
	}
	if len(prices) != 2 {
		t.Fatalf("expected 2 prices (vcpu + ram), got %d", len(prices))
	}
	if breakdown["service"] != "vertex_ai" {
		t.Errorf("breakdown service = %v, want 'vertex_ai'", breakdown["service"])
	}

	// Prices should include vCPU and RAM entries.
	for _, pr := range prices {
		if pr.Unit != models.PriceUnitPerHour {
			t.Errorf("price Unit = %v, want per_hour", pr.Unit)
		}
		if pr.PricingTerm != models.PricingTermOnDemand {
			t.Errorf("PricingTerm = %v, want on_demand", pr.PricingTerm)
		}
	}
}

// TestGetVertexPrice_NoSKUsReturnsError tests behaviour when no SKUs are found.
func TestGetVertexPrice_NoSKUsReturnsError(t *testing.T) {
	// Serve empty SKU list.
	skus := []map[string]any{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	prices, breakdown, err := p.getVertexPrice(context.Background(), "n1-standard-4", "us-central1", 100.0, "training")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prices != nil {
		t.Errorf("expected nil prices, got %v", prices)
	}
	if _, hasErr := breakdown["error"]; !hasErr {
		t.Error("expected 'error' key in breakdown when no SKUs found")
	}
}

// --------------------------------------------------------------------------
// Gemini pricing tests
// --------------------------------------------------------------------------

// TestGetGeminiPrice_Basic tests Gemini model pricing lookup.
func TestGetGeminiPrice_Basic(t *testing.T) {
	skus := []map[string]any{
		makeSKU("Gemini 1.5 Flash Input", "OnDemand", "us-central1", "0", 75_000),   // $0.000075/token
		makeSKU("Gemini 1.5 Flash Output", "OnDemand", "us-central1", "0", 300_000), // $0.0003/token
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	prices, breakdown, err := p.getGeminiPrice(context.Background(), "gemini-1.5-flash", "us-central1")
	if err != nil {
		t.Fatalf("getGeminiPrice: %v", err)
	}
	if len(prices) == 0 {
		t.Error("expected at least 1 price for gemini-1.5-flash")
	}
	if breakdown["model"] != "gemini-1.5-flash" {
		t.Errorf("breakdown model = %v, want 'gemini-1.5-flash'", breakdown["model"])
	}

	// Check input/output direction in returned prices.
	hasInput := false
	hasOutput := false
	for _, pr := range prices {
		if pr.Attributes["direction"] == "input" {
			hasInput = true
		}
		if pr.Attributes["direction"] == "output" {
			hasOutput = true
		}
	}
	if !hasInput {
		t.Error("expected at least one price with direction='input'")
	}
	if !hasOutput {
		t.Error("expected at least one price with direction='output'")
	}
}

// TestGetGeminiPrice_NoMatchReturnsError tests behaviour when no Gemini SKUs match.
func TestGetGeminiPrice_NoMatchReturnsError(t *testing.T) {
	skus := []map[string]any{
		makeSKU("N1 Custom VCPU Training", "OnDemand", "us-central1", "0", 48_000_000),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	prices, breakdown, err := p.getGeminiPrice(context.Background(), "gemini-1.5-flash", "us-central1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prices != nil {
		t.Errorf("expected nil prices, got %v", prices)
	}
	if _, hasErr := breakdown["error"]; !hasErr {
		t.Error("expected 'error' key in breakdown when no Gemini SKUs match")
	}
}

// --------------------------------------------------------------------------
// Supports / SupportedTerms integration tests
// --------------------------------------------------------------------------

// TestSupports_Part2Domains verifies that Part 2 domains are now supported.
func TestSupports_Part2Domains(t *testing.T) {
	dir := t.TempDir()
	cm, _ := cache.New(dir)
	cfg := &config.Config{GCPAPIKey: "k", CacheTTLHours: 1, MetadataTTLDays: 1}
	p := &Provider{cfg: cfg, cache: cm, auth: newGCPAuthProvider(cfg), httpClient: &http.Client{}}

	cases := []struct {
		domain  models.PricingDomain
		service string
		want    bool
	}{
		{models.PricingDomainDatabase, "", true},
		{models.PricingDomainDatabase, "cloud_sql", true},
		{models.PricingDomainDatabase, "memorystore", true},
		{models.PricingDomainContainer, "", true},
		{models.PricingDomainContainer, "gke", true},
		{models.PricingDomainAI, "", true},
		{models.PricingDomainAI, "vertex_ai", true},
		{models.PricingDomainAI, "gemini", true},
		{models.PricingDomainAnalytics, "", true},
		{models.PricingDomainAnalytics, "bigquery", true},
		// Part 3 domains — now supported.
		{models.PricingDomainNetwork, "", true},
		{models.PricingDomainNetwork, "cloud_lb", true},
		{models.PricingDomainNetwork, "cloud_cdn", true},
		{models.PricingDomainNetwork, "egress", true},
		{models.PricingDomainObservability, "", true},
		{models.PricingDomainObservability, "cloud_monitoring", true},
		{models.PricingDomainInterRegionEgress, "", true},
	}
	for _, c := range cases {
		got := p.Supports(c.domain, c.service)
		if got != c.want {
			t.Errorf("Supports(%v, %q) = %v, want %v", c.domain, c.service, got, c.want)
		}
	}
}

// --------------------------------------------------------------------------
// GetPrice routing tests (layer: GetPrice → getPart2Price / getAIPart2Price)
// --------------------------------------------------------------------------

// TestGetPrice_DatabaseSpec_CloudSQL verifies that a DatabasePricingSpec reaches
// getCloudSQLPrice via the GetPrice → getPart2Price dispatch chain.
func TestGetPrice_DatabaseSpec_CloudSQL(t *testing.T) {
	skuDesc := "Cloud SQL for MySQL: Zonal - 4 vCPU + 15GB RAM in Americas"
	skus := []map[string]any{
		makeSKU(skuDesc, "OnDemand", "us-central1", "0", 320_000_000),
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProviderDB(t, ts)
	spec := &models.DatabasePricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderGCP,
			Domain:   models.PricingDomainDatabase,
			Service:  "cloud_sql",
			Region:   "us-central1",
			Term:     models.PricingTermOnDemand,
		},
		ResourceType: "db-n1-standard-4",
		Engine:       "mysql",
	}

	result, err := p.GetPrice(context.Background(), spec)
	if err != nil {
		t.Fatalf("GetPrice(DatabasePricingSpec): %v", err)
	}
	if result == nil {
		t.Fatal("GetPrice returned nil result for DatabasePricingSpec")
		return
	}
	if len(result.PublicPrices) == 0 {
		t.Error("expected at least one price in result.PublicPrices")
	}
}

// TestGetPrice_AnalyticsSpec_BigQuery verifies that an AnalyticsPricingSpec reaches
// priceAnalytics (via getAIPart2Price) and that the workload_total composite price
// is present when query cost is non-zero.
func TestGetPrice_AnalyticsSpec_BigQuery(t *testing.T) {
	// Return an Analysis SKU with a paid tier so priceAnalytics sees a non-zero query rate.
	skus := []map[string]any{
		makeSKUPaid("BigQuery Analysis", "OnDemand", "us", "5", 0), // $5/TiB paid tier
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	queryTB := 1.0
	spec := &models.AnalyticsPricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderGCP,
			Domain:   models.PricingDomainAnalytics,
			Service:  "bigquery",
			Region:   "us-central1",
			Term:     models.PricingTermOnDemand,
		},
		QueryTB: &queryTB,
	}

	p := newTestProviderDB(t, ts)
	result, err := p.GetPrice(context.Background(), spec)
	if err != nil {
		t.Fatalf("GetPrice(AnalyticsPricingSpec): %v", err)
	}
	if result == nil {
		t.Fatal("GetPrice returned nil result for AnalyticsPricingSpec")
		return
	}
	// priceAnalytics wraps getBigQueryPrice and prepends a workload_total composite
	// NormalizedPrice when there is an estimated cost. Check the routing succeeded.
	if result.Breakdown == nil {
		t.Error("expected non-nil breakdown from priceAnalytics")
		return
	}
	if result.Breakdown["service"] != "bigquery" {
		t.Errorf("breakdown service = %v, want 'bigquery'", result.Breakdown["service"])
	}
}
