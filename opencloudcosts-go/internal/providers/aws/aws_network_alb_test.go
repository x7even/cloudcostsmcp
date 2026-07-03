package aws

// aws_network_alb_test.go — regression tests for GetALBPrice (RC3-011).
//
// Before the fix, GetALBPrice returned hardcoded PricePerUnit values
// (0.0225/0.008) for every region and never touched the AWSELB pricing
// catalog, so ca-central-1 and eu-west-1 returned byte-identical prices with
// no "fallback" tag. These tests verify that GetALBPrice now fetches real,
// region-varying prices from the AWSELB service (mirroring get_price_by_sku's
// already-correct "CAN1-LCUUsage" resolution: $0.0088/LCU-hr in ca-central-1
// vs $0.0080/LCU-hr in eu-west-1 — see
// https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AWSELB/current/ca-central-1/index.json
// and .../eu-west-1/index.json, fetched 2026-07-03), that Reserved-capacity
// and Trust Store rows (which share productFamily/operation with the
// standard on-demand rows but bill a different dimension) are excluded, and
// that a genuine fetch failure still falls back to static rates tagged
// Attributes["fallback"]="true".

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// albFixtureRow describes one AWSELB product row for the test fixtures below.
type albFixtureRow struct {
	sku          string
	usageType    string
	locationType string
	priceUSD     string
}

// albBulkFixture builds a bulk AWSELB offer-file fixture for a single region,
// covering the standard on-demand hourly/LCU rows plus the Reserved, Trust
// Store, and Outposts rows that share productFamily="Load Balancer-Application"
// and operation="LoadBalancing:Application" but must NOT be picked up as the
// standard on-demand rate.
func albBulkFixture(location string, rows []albFixtureRow) string {
	specs := make([]multiProductSpec, 0, len(rows))
	productFamilyBySKU := make(map[string]string, len(rows))
	for _, r := range rows {
		specs = append(specs, multiProductSpec{
			sku:       r.sku,
			usageType: r.usageType,
			location:  location,
			priceUSD:  r.priceUSD,
			extraAttrs: map[string]string{
				"operation":    "LoadBalancing:Application",
				"locationType": r.locationType,
			},
		})
		productFamilyBySKU[r.sku] = "Load Balancer-Application"
	}
	return skuBulkJSONMultiWithProductFamily(specs, productFamilyBySKU)
}

// standardALBRows returns the full set of ALB-family rows AWS actually
// publishes for a region (hourly, LCU, reserved LCU, Trust Store hourly,
// Outposts hourly, Outposts LCU), parameterized by the on-demand
// hourly/LCU prices so callers can vary them per region.
func standardALBRows(hourlyPrice, lcuPrice string) []albFixtureRow {
	return []albFixtureRow{
		{sku: "SKU-HOURLY", usageType: "LoadBalancerUsage", locationType: "AWS Region", priceUSD: hourlyPrice},
		{sku: "SKU-LCU", usageType: "LCUUsage", locationType: "AWS Region", priceUSD: lcuPrice},
		{sku: "SKU-RESERVED", usageType: "ReservedLCUUsage", locationType: "AWS Region", priceUSD: "0.0200000000"},
		{sku: "SKU-TS", usageType: "TS-LoadBalancerUsage", locationType: "AWS Region", priceUSD: "0.0056000000"},
		{sku: "SKU-OUTPOSTS-HOURLY", usageType: "Outposts-LoadBalancerUsage", locationType: "AWS Outposts", priceUSD: "0.0300000000"},
		{sku: "SKU-OUTPOSTS-LCU", usageType: "Outposts-LCUUsage", locationType: "AWS Outposts", priceUSD: "0.0120000000"},
	}
}

// albPrice finds the price for the given billing_dimension ("hourly" or
// "lcu_hour") within a GetALBPrice result.
func albPrice(prices []models.NormalizedPrice, dimension string) (float64, bool) {
	for _, np := range prices {
		if np.Attributes["billing_dimension"] == dimension {
			return np.PricePerUnit, true
		}
	}
	return 0, false
}

// TestGetALBPrice_BulkFallback_RegionVaryingPrices is the core RC3-011
// regression test: ca-central-1 and eu-west-1 must return DIFFERENT prices
// (matching the real AWSELB catalog), not the old hardcoded 0.0225/0.008 for
// both regions.
func TestGetALBPrice_BulkFallback_RegionVaryingPrices(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/AWSELB/current/ca-central-1/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(albBulkFixture("Canada (Central)", standardALBRows("0.0247500000", "0.0088000000"))))
	})
	mux.HandleFunc("/AWSELB/current/eu-west-1/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(albBulkFixture("Europe (Ireland)", standardALBRows("0.0252000000", "0.0080000000"))))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	p := newTestProvider(t)
	p.bulkFallback = true
	overrideBulkBaseURL(t, server.URL)
	ctx := context.Background()

	canResult, err := p.GetALBPrice(ctx, "ca-central-1")
	if err != nil {
		t.Fatalf("GetALBPrice(ca-central-1): %v", err)
	}
	euResult, err := p.GetALBPrice(ctx, "eu-west-1")
	if err != nil {
		t.Fatalf("GetALBPrice(eu-west-1): %v", err)
	}

	canLCU, canOK := albPrice(canResult, "lcu_hour")
	euLCU, euOK := albPrice(euResult, "lcu_hour")
	if !canOK || !euOK {
		t.Fatalf("expected an lcu_hour entry in both regions; ca-central-1 ok=%v eu-west-1 ok=%v", canOK, euOK)
	}
	if canLCU != 0.0088 {
		t.Errorf("ca-central-1 LCU price = %v, want 0.0088", canLCU)
	}
	if euLCU != 0.008 {
		t.Errorf("eu-west-1 LCU price = %v, want 0.008", euLCU)
	}
	if canLCU == euLCU {
		t.Fatalf("regression: ca-central-1 and eu-west-1 LCU prices are identical (%v) — GetALBPrice is not region-varying", canLCU)
	}

	canHourly, canHourlyOK := albPrice(canResult, "hourly")
	euHourly, euHourlyOK := albPrice(euResult, "hourly")
	if !canHourlyOK || !euHourlyOK {
		t.Fatalf("expected an hourly entry in both regions; ca-central-1 ok=%v eu-west-1 ok=%v", canHourlyOK, euHourlyOK)
	}
	if canHourly == euHourly {
		t.Fatalf("regression: ca-central-1 and eu-west-1 hourly prices are identical (%v) — GetALBPrice is not region-varying", canHourly)
	}

	// Real data was fetched successfully — must NOT carry the fallback tag.
	for _, np := range canResult {
		if np.Attributes["fallback"] == "true" {
			t.Errorf("ca-central-1 result tagged fallback=true despite a successful live fetch: %+v", np)
		}
		if np.ProductFamily != "Load Balancer-Application" {
			t.Errorf("ProductFamily = %q, want %q", np.ProductFamily, "Load Balancer-Application")
		}
	}
}

// TestGetALBPrice_BulkFallback_ExcludesReservedAndTrustStore verifies that
// Reserved-capacity and Trust Store rows — which share productFamily
// "Load Balancer-Application" and operation "LoadBalancing:Application"
// with the standard on-demand rows — are not mistaken for the standard
// hourly/LCU on-demand charge.
func TestGetALBPrice_BulkFallback_ExcludesReservedAndTrustStore(t *testing.T) {
	body := albBulkFixture("US East (N. Virginia)", standardALBRows("0.0225000000", "0.0080000000"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	p := newTestProvider(t)
	p.bulkFallback = true
	overrideBulkBaseURL(t, server.URL)

	result, err := p.GetALBPrice(context.Background(), "us-east-1")
	if err != nil {
		t.Fatalf("GetALBPrice: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected exactly 2 prices (hourly + lcu_hour), got %d: %+v", len(result), result)
	}

	hourly, hourlyOK := albPrice(result, "hourly")
	lcu, lcuOK := albPrice(result, "lcu_hour")
	if !hourlyOK || !lcuOK {
		t.Fatalf("expected both hourly and lcu_hour entries, got %+v", result)
	}
	if hourly != 0.0225 {
		t.Errorf("hourly price = %v, want 0.0225 (the standard row, not Reserved/TS/Outposts)", hourly)
	}
	if lcu != 0.008 {
		t.Errorf("lcu price = %v, want 0.008 (the standard row, not Reserved/Outposts)", lcu)
	}
}

// TestGetALBPrice_FallbackOnFetchFailure verifies that a genuine fetch
// failure (e.g. the bulk endpoint 404s) still degrades gracefully to the
// static published rates, explicitly tagged fallback=true — the minimum
// bar the issue asked for if a full fix were not possible.
func TestGetALBPrice_FallbackOnFetchFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	p := newTestProvider(t)
	p.bulkFallback = true
	overrideBulkBaseURL(t, server.URL)

	result, err := p.GetALBPrice(context.Background(), "us-east-1")
	if err != nil {
		t.Fatalf("GetALBPrice: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 static fallback prices, got %d: %+v", len(result), result)
	}
	for _, np := range result {
		if np.Attributes["fallback"] != "true" {
			t.Errorf("expected Attributes[fallback]=true on static-fallback entry, got %+v", np)
		}
	}
	hourly, _ := albPrice(result, "hourly")
	lcu, _ := albPrice(result, "lcu_hour")
	if hourly != 0.0225 {
		t.Errorf("fallback hourly price = %v, want 0.0225", hourly)
	}
	if lcu != 0.008 {
		t.Errorf("fallback lcu price = %v, want 0.008", lcu)
	}
}

// TestGetALBPrice_InvalidRegion mirrors the invalid-region behavior of the
// other Get*Price methods (e.g. TestGetLambdaPrice_InvalidRegion).
func TestGetALBPrice_InvalidRegion(t *testing.T) {
	p := newTestProvider(t)
	_, err := p.GetALBPrice(context.Background(), "bad-region-zz")
	if err == nil {
		t.Fatal("expected error for invalid region")
	}
}
