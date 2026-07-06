// Package gcp — unit tests for Cloud DNS pricing (issue #78).
//
// All SKU descriptions used in these fixtures are the exact, live-verified
// strings recovered from the GCP Cloud Billing Catalog API for service
// FA26-5236-B8B5 ("Cloud DNS").
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
// DNS SKU helpers
// --------------------------------------------------------------------------

// dnsSKUThreeTier builds a three-tier global SKU: tier1 at startUsageAmount=0,
// tier2 at startUsageAmount=tier2Threshold, tier3 at
// startUsageAmount=tier3Threshold. Models the ManagedZone SKU
// (8C22-6FC3-D478): $0.20/zone-month (1-25), $0.10/zone-month (26-10,000),
// $0.03/zone-month (10,001+).
func dnsSKUThreeTier(desc string, tier2Threshold, tier3Threshold float64, tier1Units string, tier1Nanos int, tier2Units string, tier2Nanos int, tier3Units string, tier3Nanos int) map[string]any {
	return map[string]any{
		"description":    desc,
		"serviceRegions": []any{"global"},
		"category": map[string]any{
			"resourceGroup": "DNS",
			"usageType":     "OnDemand",
		},
		"geoTaxonomy": map[string]any{
			"type": "GLOBAL",
		},
		"pricingInfo": []any{
			map[string]any{
				"pricingExpression": map[string]any{
					"tieredRates": []any{
						map[string]any{
							"startUsageAmount": float64(0),
							"unitPrice": map[string]any{
								"units": tier1Units,
								"nanos": float64(tier1Nanos),
							},
						},
						map[string]any{
							"startUsageAmount": tier2Threshold,
							"unitPrice": map[string]any{
								"units": tier2Units,
								"nanos": float64(tier2Nanos),
							},
						},
						map[string]any{
							"startUsageAmount": tier3Threshold,
							"unitPrice": map[string]any{
								"units": tier3Units,
								"nanos": float64(tier3Nanos),
							},
						},
					},
				},
			},
		},
	}
}

// dnsSKUTwoTier builds a two-tier global SKU: tier1 at startUsageAmount=0,
// tier2 at startUsageAmount=threshold. Models the DNS Query SKU
// (6DFF-5025-A128): $0.0000004/query (0-1B/mo), $0.0000002/query (1B+/mo).
func dnsSKUTwoTier(desc string, threshold float64, tier1Units string, tier1Nanos int, tier2Units string, tier2Nanos int) map[string]any {
	return map[string]any{
		"description":    desc,
		"serviceRegions": []any{"global"},
		"category": map[string]any{
			"resourceGroup": "DNS",
			"usageType":     "OnDemand",
		},
		"geoTaxonomy": map[string]any{
			"type": "GLOBAL",
		},
		"pricingInfo": []any{
			map[string]any{
				"pricingExpression": map[string]any{
					"tieredRates": []any{
						map[string]any{
							"startUsageAmount": float64(0),
							"unitPrice": map[string]any{
								"units": tier1Units,
								"nanos": float64(tier1Nanos),
							},
						},
						map[string]any{
							"startUsageAmount": threshold,
							"unitPrice": map[string]any{
								"units": tier2Units,
								"nanos": float64(tier2Nanos),
							},
						},
					},
				},
			},
		},
	}
}

// dnsSKUSingleTier builds a single-tier global SKU: one tier at
// startUsageAmount=0 carrying the rate directly. Used to simulate a live
// response that is missing a tier (regression coverage for the all-or-
// nothing fallback guard).
func dnsSKUSingleTier(desc, units string, nanos int) map[string]any {
	return map[string]any{
		"description":    desc,
		"serviceRegions": []any{"global"},
		"category": map[string]any{
			"resourceGroup": "DNS",
			"usageType":     "OnDemand",
		},
		"geoTaxonomy": map[string]any{
			"type": "GLOBAL",
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

// dnsSKUResponse wraps DNS SKUs for the httptest server.
func dnsSKUResponse(skus []map[string]any) []byte {
	resp := map[string]any{
		"skus":          skus,
		"nextPageToken": "",
	}
	b, _ := json.Marshal(resp)
	return b
}

// fakeDNSSKUs returns the two live-verified Cloud DNS SKUs (service
// FA26-5236-B8B5), using their exact description strings.
func fakeDNSSKUs() []map[string]any {
	return []map[string]any{
		// ManagedZone — $0.20/zone-mo (1-25), $0.10/zone-mo (26-10,000), $0.03/zone-mo (10,001+).
		dnsSKUThreeTier("ManagedZone", 25, 10000,
			"0", 200_000_000, // $0.20
			"0", 100_000_000, // $0.10
			"0", 30_000_000, // $0.03
		), // 8C22-6FC3-D478

		// DNS Query (port 53) — $0.0000004/query (0-1B/mo), $0.0000002/query (1B+/mo).
		dnsSKUTwoTier("DNS Query (port 53)", 1_000_000_000,
			"0", 400, // $0.0000004
			"0", 200, // $0.0000002
		), // 6DFF-5025-A128
	}
}

// --------------------------------------------------------------------------
// Rate-parsing / fetch tests
// --------------------------------------------------------------------------

// TestFetchDNSRates_ParsesAllTiers verifies fetchDNSRates extracts all three
// ManagedZone tiers and both DNS Query tiers from the live SKU shapes.
func TestFetchDNSRates_ParsesAllTiers(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(dnsSKUResponse(fakeDNSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	rates := p.fetchDNSRates(ctx)
	if abs(rates.ZoneTier1-0.20) > 1e-9 {
		t.Errorf("ZoneTier1 = %.6f, want 0.200000", rates.ZoneTier1)
	}
	if abs(rates.ZoneTier2-0.10) > 1e-9 {
		t.Errorf("ZoneTier2 = %.6f, want 0.100000", rates.ZoneTier2)
	}
	if abs(rates.ZoneTier3-0.03) > 1e-9 {
		t.Errorf("ZoneTier3 = %.6f, want 0.030000", rates.ZoneTier3)
	}
	if abs(rates.QueryTier1-0.0000004) > 1e-12 {
		t.Errorf("QueryTier1 = %.9f, want 0.000000400", rates.QueryTier1)
	}
	if abs(rates.QueryTier2-0.0000002) > 1e-12 {
		t.Errorf("QueryTier2 = %.9f, want 0.000000200", rates.QueryTier2)
	}
}

// TestFetchDNSRates_Cached verifies that fetchDNSRates reads its derived rate
// map from cache instead of calling fetchSKUs/HTTP at all, mirroring
// TestPriceKMS_RatesCached.
func TestFetchDNSRates_Cached(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected HTTP call: fetchDNSRates should have used the cached rate map")
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	seeded := dnsRates{ZoneTier1: 0.99, ZoneTier2: 0.5, ZoneTier3: 0.1, QueryTier1: 0.000001, QueryTier2: 0.0000005}
	raw, err := json.Marshal(seeded)
	if err != nil {
		t.Fatalf("marshal seeded rates: %v", err)
	}
	p.cache.SetMetadata(dnsRatesCacheKey, raw, p.cfg.MetadataTTL())

	rates := p.fetchDNSRates(ctx)
	if rates != seeded {
		t.Errorf("fetchDNSRates = %+v, want cached %+v", rates, seeded)
	}
}

// TestFetchDNSRates_DuplicateMatchKeepsFirst verifies that if more than one
// SKU matches the "managed zone" (or "dns query") substring pattern,
// fetchDNSRates keeps the first match rather than silently letting a later
// SKU overwrite it (last-write-wins).
func TestFetchDNSRates_DuplicateMatchKeepsFirst(t *testing.T) {
	skus := []map[string]any{
		dnsSKUThreeTier("ManagedZone", 25, 10000,
			"0", 200_000_000, // $0.20 — first match, should win
			"0", 100_000_000,
			"0", 30_000_000,
		),
		dnsSKUThreeTier("ManagedZone (duplicate catalog entry)", 25, 10000,
			"0", 999_000_000, // $0.99 — later match, must be discarded
			"0", 999_000_000,
			"0", 999_000_000,
		),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(dnsSKUResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	rates := p.fetchDNSRates(ctx)
	if abs(rates.ZoneTier1-0.20) > 1e-9 {
		t.Errorf("ZoneTier1 = %.6f, want first match's 0.200000 (duplicate must not overwrite)", rates.ZoneTier1)
	}
}

// TestFetchDNSRates_SkipsNonGlobalMatch verifies that a SKU whose
// geoTaxonomy.type is present and not "GLOBAL" is skipped, even if its
// description would otherwise match — Cloud DNS pricing is region-invariant
// (see file header), so a regional SKU matching the same substring must
// never be mistaken for the real rate.
func TestFetchDNSRates_SkipsNonGlobalMatch(t *testing.T) {
	regional := dnsSKUThreeTier("ManagedZone", 25, 10000,
		"0", 999_000_000, // must be skipped
		"0", 999_000_000,
		"0", 999_000_000,
	)
	regional["geoTaxonomy"] = map[string]any{"type": "REGIONAL"}

	skus := []map[string]any{
		regional,
		dnsSKUThreeTier("ManagedZone", 25, 10000,
			"0", 200_000_000, // $0.20 — the real, GLOBAL-scoped SKU
			"0", 100_000_000,
			"0", 30_000_000,
		),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(dnsSKUResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	rates := p.fetchDNSRates(ctx)
	if abs(rates.ZoneTier1-0.20) > 1e-9 {
		t.Errorf("ZoneTier1 = %.6f, want 0.200000 (non-GLOBAL SKU must be skipped)", rates.ZoneTier1)
	}
}

// --------------------------------------------------------------------------
// priceDNS — headline rate, scope, and dispatch tests
// --------------------------------------------------------------------------

// TestPriceDNS_ReturnsBothLineItems verifies priceDNS returns exactly two
// NormalizedPrice entries (ManagedZone + DNS Query) with the correct
// headline (tier1) rates, both tagged region-invariant.
func TestPriceDNS_ReturnsBothLineItems(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(dnsSKUResponse(fakeDNSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.DNSPricingSpec{ZoneType: "public"}
	prices, breakdown, err := p.priceDNS(ctx, spec)
	if err != nil {
		t.Fatalf("priceDNS: %v", err)
	}
	if len(prices) != 2 {
		t.Fatalf("expected exactly two prices (zone + query), got %d", len(prices))
	}

	var zone, query *models.NormalizedPrice
	for i := range prices {
		switch prices[i].Unit {
		case models.PriceUnitPerZoneMonth:
			zone = &prices[i]
		case models.PriceUnitPerQuery:
			query = &prices[i]
		}
	}
	if zone == nil {
		t.Fatal("no per_zone_month price returned")
	}
	if query == nil {
		t.Fatal("no per_query price returned")
	}
	if abs(zone.PricePerUnit-0.20) > 1e-9 {
		t.Errorf("zone headline rate = %.6f, want 0.200000", zone.PricePerUnit)
	}
	if abs(query.PricePerUnit-0.0000004) > 1e-12 {
		t.Errorf("query headline rate = %.9f, want 0.000000400", query.PricePerUnit)
	}
	for _, pr := range []*models.NormalizedPrice{zone, query} {
		if pr.Region != "global" || pr.Attributes["scope"] != "global" {
			t.Errorf("region/scope not tagged global: region=%q attrs=%v", pr.Region, pr.Attributes)
		}
	}
	if fb, ok := breakdown["fallback"]; ok && fb == true {
		t.Error("expected no fallback when both DNS SKUs are present")
	}
	tier2 := mustFloat64(t, breakdown["zone_tier2_rate"], "zone_tier2_rate")
	tier3 := mustFloat64(t, breakdown["zone_tier3_rate"], "zone_tier3_rate")
	if abs(tier2-0.10) > 1e-9 {
		t.Errorf("zone_tier2_rate = %.6f, want 0.100000", tier2)
	}
	if abs(tier3-0.03) > 1e-9 {
		t.Errorf("zone_tier3_rate = %.6f, want 0.030000", tier3)
	}
}

// TestPriceDNS_ZoneTypeIsPriceNeutral verifies that every valid zone_type
// resolves to the identical ManagedZone rate — zone_type is informational
// only and must never select a different rate.
func TestPriceDNS_ZoneTypeIsPriceNeutral(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(dnsSKUResponse(fakeDNSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	for _, zt := range []string{"public", "private", "forwarding", "peering"} {
		t.Run(zt, func(t *testing.T) {
			spec := &models.DNSPricingSpec{ZoneType: zt}
			prices, _, err := p.priceDNS(ctx, spec)
			if err != nil {
				t.Fatalf("priceDNS: %v", err)
			}
			for _, pr := range prices {
				if pr.Unit == models.PriceUnitPerZoneMonth && abs(pr.PricePerUnit-0.20) > 1e-9 {
					t.Errorf("zone_type=%s: zone rate = %.6f, want 0.200000 (price-neutral)", zt, pr.PricePerUnit)
				}
			}
		})
	}
}

// TestPriceDNS_DefaultZoneTypeIsPublic verifies that an empty ZoneType
// defaults to "public".
func TestPriceDNS_DefaultZoneTypeIsPublic(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(dnsSKUResponse(fakeDNSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.DNSPricingSpec{}
	prices, _, err := p.priceDNS(ctx, spec)
	if err != nil {
		t.Fatalf("priceDNS: %v", err)
	}
	for _, pr := range prices {
		if pr.Attributes["zone_type"] != "public" {
			t.Errorf("zone_type attribute = %q, want %q", pr.Attributes["zone_type"], "public")
		}
	}
}

// TestPriceDNS_UnrecognizedZoneTypeIsInformationalOnly verifies that an
// unrecognized ZoneType value does NOT error — zone_type never affects price
// (it's a purely descriptive attribute; see gcp_dns.go's file header and
// dnsKnownZoneTypes doc comment), so an unfamiliar value is surfaced as an
// informational breakdown note rather than rejected, and the price is still
// computed normally (and identically to a recognized zone_type).
func TestPriceDNS_UnrecognizedZoneTypeIsInformationalOnly(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(dnsSKUResponse(fakeDNSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.DNSPricingSpec{ZoneType: "quantum"}
	prices, breakdown, err := p.priceDNS(ctx, spec)
	if err != nil {
		t.Fatalf("priceDNS: unexpected error for unrecognized zone_type: %v", err)
	}
	if len(prices) != 2 {
		t.Fatalf("priceDNS: expected 2 prices, got %d", len(prices))
	}

	unrecognized, ok := breakdown["zone_type_unrecognized"]
	if !ok || unrecognized != true {
		t.Fatalf("priceDNS: expected breakdown[\"zone_type_unrecognized\"]=true, got %v (ok=%v)", unrecognized, ok)
	}
	if _, ok := breakdown["zone_type_unrecognized_note"].(string); !ok {
		t.Fatalf("priceDNS: expected breakdown[\"zone_type_unrecognized_note\"] to be a string, got %v", breakdown["zone_type_unrecognized_note"])
	}

	// Price is unaffected: compare against the recognized "public" zone_type.
	publicPrices, publicBreakdown, err := p.priceDNS(ctx, &models.DNSPricingSpec{ZoneType: "public"})
	if err != nil {
		t.Fatalf("priceDNS: unexpected error for zone_type=public: %v", err)
	}
	if _, ok := publicBreakdown["zone_type_unrecognized"]; ok {
		t.Fatalf("priceDNS: did not expect zone_type_unrecognized for zone_type=public")
	}
	if prices[0].PricePerUnit != publicPrices[0].PricePerUnit {
		t.Fatalf("priceDNS: zone rate differs by zone_type: quantum=%v public=%v", prices[0].PricePerUnit, publicPrices[0].PricePerUnit)
	}
	if prices[1].PricePerUnit != publicPrices[1].PricePerUnit {
		t.Fatalf("priceDNS: query rate differs by zone_type: quantum=%v public=%v", prices[1].PricePerUnit, publicPrices[1].PricePerUnit)
	}
}

// TestPriceDNS_DispatchViaGetPrice verifies that a domain="dns",
// service="cloud_dns" spec routes through GetPrice -> Supports ->
// getPart3Price -> priceDNS end-to-end.
func TestPriceDNS_DispatchViaGetPrice(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(dnsSKUResponse(fakeDNSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	if !p.Supports(models.PricingDomainDNS, "cloud_dns") {
		t.Fatal("Supports(dns, cloud_dns) = false, want true")
	}

	spec := &models.DNSPricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Domain:  models.PricingDomainDNS,
			Service: "cloud_dns",
		},
		ZoneType: "public",
	}
	result, err := p.GetPrice(ctx, spec)
	if err != nil {
		t.Fatalf("GetPrice: %v", err)
	}
	if len(result.PublicPrices) != 2 {
		t.Fatalf("GetPrice did not dispatch to priceDNS: %+v", result.PublicPrices)
	}
	for _, pr := range result.PublicPrices {
		if pr.Service != "cloud_dns" {
			t.Errorf("Service = %q, want %q", pr.Service, "cloud_dns")
		}
	}
}

// --------------------------------------------------------------------------
// Tiered cost-math tests — zone-count dimension
// --------------------------------------------------------------------------

// TestPriceDNS_ZoneCostMath_BelowTier2Threshold verifies zone_monthly_cost for
// a zone count strictly within tier1 (1-25): 10 zones * $0.20 = $2.00.
func TestPriceDNS_ZoneCostMath_BelowTier2Threshold(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(dnsSKUResponse(fakeDNSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	zoneCount := 10.0
	spec := &models.DNSPricingSpec{ZoneCount: &zoneCount}
	_, breakdown, err := p.priceDNS(ctx, spec)
	if err != nil {
		t.Fatalf("priceDNS: %v", err)
	}
	got := mustFloat64(t, breakdown["zone_monthly_cost"], "zone_monthly_cost")
	want := 10.0 * 0.20
	if abs(got-want) > 1e-6 {
		t.Errorf("zone_monthly_cost = %.4f, want %.4f", got, want)
	}
}

// TestPriceDNS_ZoneCostMath_AtTier2Threshold verifies the exact tier1/tier2
// boundary (25 zones): all 25 zones bill at tier1 ($0.20), none spill into
// tier2 — the 26th zone is what enters tier2.
func TestPriceDNS_ZoneCostMath_AtTier2Threshold(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(dnsSKUResponse(fakeDNSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	zoneCount := 25.0
	spec := &models.DNSPricingSpec{ZoneCount: &zoneCount}
	_, breakdown, err := p.priceDNS(ctx, spec)
	if err != nil {
		t.Fatalf("priceDNS: %v", err)
	}
	got := mustFloat64(t, breakdown["zone_monthly_cost"], "zone_monthly_cost")
	want := 25.0 * 0.20
	if abs(got-want) > 1e-6 {
		t.Errorf("zone_monthly_cost at threshold = %.4f, want %.4f (all tier1)", got, want)
	}
}

// TestPriceDNS_ZoneCostMath_CrossesTier2Threshold verifies zone_monthly_cost
// splits correctly when the zone count crosses into tier2 (26-10,000):
// 30 zones = 25 @ $0.20 + 5 @ $0.10, NOT 30 * $0.20.
func TestPriceDNS_ZoneCostMath_CrossesTier2Threshold(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(dnsSKUResponse(fakeDNSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	zoneCount := 30.0
	spec := &models.DNSPricingSpec{ZoneCount: &zoneCount}
	_, breakdown, err := p.priceDNS(ctx, spec)
	if err != nil {
		t.Fatalf("priceDNS: %v", err)
	}
	got := mustFloat64(t, breakdown["zone_monthly_cost"], "zone_monthly_cost")
	want := 25.0*0.20 + 5.0*0.10
	if abs(got-want) > 1e-6 {
		t.Errorf("zone_monthly_cost = %.4f, want %.4f (tiered split, not flat tier1*qty)", got, want)
	}
}

// TestPriceDNS_ZoneCostMath_CrossesTier3Threshold verifies zone_monthly_cost
// reaches the third tier (10,001+): 10,010 zones =
// 25 @ $0.20 + 9,975 @ $0.10 + 10 @ $0.03.
func TestPriceDNS_ZoneCostMath_CrossesTier3Threshold(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(dnsSKUResponse(fakeDNSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	zoneCount := 10010.0
	spec := &models.DNSPricingSpec{ZoneCount: &zoneCount}
	_, breakdown, err := p.priceDNS(ctx, spec)
	if err != nil {
		t.Fatalf("priceDNS: %v", err)
	}
	got := mustFloat64(t, breakdown["zone_monthly_cost"], "zone_monthly_cost")
	want := 25.0*0.20 + 9975.0*0.10 + 10.0*0.03
	if abs(got-want) > 1e-6 {
		t.Errorf("zone_monthly_cost = %.4f, want %.4f (reaches tier3)", got, want)
	}
}

// TestPriceDNS_ZoneCostMath_AtTier3Threshold verifies the exact tier2/tier3
// boundary (10,000 zones): all 10,000 zones bill across tier1+tier2, none
// spill into tier3 — the 10,001st zone is what enters tier3.
func TestPriceDNS_ZoneCostMath_AtTier3Threshold(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(dnsSKUResponse(fakeDNSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	zoneCount := 10000.0
	spec := &models.DNSPricingSpec{ZoneCount: &zoneCount}
	_, breakdown, err := p.priceDNS(ctx, spec)
	if err != nil {
		t.Fatalf("priceDNS: %v", err)
	}
	got := mustFloat64(t, breakdown["zone_monthly_cost"], "zone_monthly_cost")
	want := 25.0*0.20 + 9975.0*0.10
	if abs(got-want) > 1e-6 {
		t.Errorf("zone_monthly_cost at threshold = %.4f, want %.4f (tier1+tier2 only)", got, want)
	}
}

// --------------------------------------------------------------------------
// Tiered cost-math tests — query-volume dimension
// --------------------------------------------------------------------------

// TestPriceDNS_QueryCostMath_BelowThreshold verifies query_monthly_cost for a
// volume strictly within tier1: 1,000,000 queries * $0.0000004 = $0.40.
func TestPriceDNS_QueryCostMath_BelowThreshold(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(dnsSKUResponse(fakeDNSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	queries := 1_000_000.0
	spec := &models.DNSPricingSpec{QueriesPerMonth: &queries}
	_, breakdown, err := p.priceDNS(ctx, spec)
	if err != nil {
		t.Fatalf("priceDNS: %v", err)
	}
	got := mustFloat64(t, breakdown["query_monthly_cost"], "query_monthly_cost")
	want := 1_000_000.0 * 0.0000004
	if abs(got-want) > 1e-6 {
		t.Errorf("query_monthly_cost = %.6f, want %.6f", got, want)
	}
}

// TestPriceDNS_QueryCostMath_AtThreshold verifies the exact tier boundary
// (1,000,000,000 queries): the full volume bills at tier1.
func TestPriceDNS_QueryCostMath_AtThreshold(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(dnsSKUResponse(fakeDNSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	queries := 1_000_000_000.0
	spec := &models.DNSPricingSpec{QueriesPerMonth: &queries}
	_, breakdown, err := p.priceDNS(ctx, spec)
	if err != nil {
		t.Fatalf("priceDNS: %v", err)
	}
	got := mustFloat64(t, breakdown["query_monthly_cost"], "query_monthly_cost")
	want := 1_000_000_000.0 * 0.0000004
	if abs(got-want) > 1e-3 {
		t.Errorf("query_monthly_cost at threshold = %.4f, want %.4f (all tier1)", got, want)
	}
}

// TestPriceDNS_QueryCostMath_CrossesThreshold verifies query_monthly_cost
// splits correctly across the 1B threshold: 1,100,000,000 queries =
// 1,000,000,000 @ $0.0000004 + 100,000,000 @ $0.0000002.
func TestPriceDNS_QueryCostMath_CrossesThreshold(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(dnsSKUResponse(fakeDNSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	queries := 1_100_000_000.0
	spec := &models.DNSPricingSpec{QueriesPerMonth: &queries}
	_, breakdown, err := p.priceDNS(ctx, spec)
	if err != nil {
		t.Fatalf("priceDNS: %v", err)
	}
	got := mustFloat64(t, breakdown["query_monthly_cost"], "query_monthly_cost")
	want := 1_000_000_000.0*0.0000004 + 100_000_000.0*0.0000002
	if abs(got-want) > 1e-3 {
		t.Errorf("query_monthly_cost = %.4f, want %.4f (tiered split, not flat tier1*qty)", got, want)
	}
}

// TestPriceDNS_MonthlyCost_SumsBothDimensions verifies that when both
// zone_count and queries_per_month are supplied, the top-level monthly_cost
// is the sum of zone_monthly_cost and query_monthly_cost (both dimensions
// are complementary, unlike KMS's mutually-exclusive unit selector).
func TestPriceDNS_MonthlyCost_SumsBothDimensions(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(dnsSKUResponse(fakeDNSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	zoneCount := 5.0
	queries := 2_000_000.0
	spec := &models.DNSPricingSpec{ZoneCount: &zoneCount, QueriesPerMonth: &queries}
	_, breakdown, err := p.priceDNS(ctx, spec)
	if err != nil {
		t.Fatalf("priceDNS: %v", err)
	}
	zoneCost := mustFloat64(t, breakdown["zone_monthly_cost"], "zone_monthly_cost")
	queryCost := mustFloat64(t, breakdown["query_monthly_cost"], "query_monthly_cost")
	total := mustFloat64(t, breakdown["monthly_cost"], "monthly_cost")
	want := zoneCost + queryCost
	if abs(total-want) > 1e-6 {
		t.Errorf("monthly_cost = %.6f, want %.6f (zone_monthly_cost + query_monthly_cost)", total, want)
	}
}

// TestPriceDNS_NoMonthlyCostWithoutQuantities verifies that monthly_cost (and
// the per-dimension cost fields) are absent when neither ZoneCount nor
// QueriesPerMonth is supplied — a rate-only lookup should not fabricate a
// zero-dollar estimate.
func TestPriceDNS_NoMonthlyCostWithoutQuantities(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(dnsSKUResponse(fakeDNSSKUs()))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.DNSPricingSpec{}
	_, breakdown, err := p.priceDNS(ctx, spec)
	if err != nil {
		t.Fatalf("priceDNS: %v", err)
	}
	if _, ok := breakdown["monthly_cost"]; ok {
		t.Error("monthly_cost unexpectedly present with no zone_count or queries_per_month")
	}
	if _, ok := breakdown["zone_monthly_cost"]; ok {
		t.Error("zone_monthly_cost unexpectedly present with no zone_count")
	}
	if _, ok := breakdown["query_monthly_cost"]; ok {
		t.Error("query_monthly_cost unexpectedly present with no queries_per_month")
	}
}

// --------------------------------------------------------------------------
// Fallback tests
// --------------------------------------------------------------------------

// TestPriceDNS_Fallback verifies that when no matching SKU is found, priceDNS
// falls back to the hardcoded published rates and sets
// breakdown["fallback"]=true.
func TestPriceDNS_Fallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(dnsSKUResponse(nil))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.DNSPricingSpec{}
	prices, breakdown, err := p.priceDNS(ctx, spec)
	if err != nil {
		t.Fatalf("priceDNS (fallback): %v", err)
	}
	fb, ok := breakdown["fallback"]
	if !ok || fb != true {
		t.Errorf("expected fallback=true, got %v (ok=%v)", fb, ok)
	}
	for _, pr := range prices {
		switch pr.Unit {
		case models.PriceUnitPerZoneMonth:
			if abs(pr.PricePerUnit-dnsFallbackRates.ZoneTier1) > 1e-9 {
				t.Errorf("fallback zone rate = %.6f, want %.6f", pr.PricePerUnit, dnsFallbackRates.ZoneTier1)
			}
		case models.PriceUnitPerQuery:
			if abs(pr.PricePerUnit-dnsFallbackRates.QueryTier1) > 1e-12 {
				t.Errorf("fallback query rate = %.9f, want %.9f", pr.PricePerUnit, dnsFallbackRates.QueryTier1)
			}
		}
	}
}

// TestPriceDNS_ZoneTieredFallbackWhenTier3Zero verifies that priceDNS engages
// the fallback path when the live rate map has nonzero ZoneTier1/ZoneTier2
// but a zero ZoneTier3 (e.g. a live catalog response missing the third
// tier). Regression test mirroring TestPriceKMS_TieredFallbackWhenTier2Zero:
// checking only tier1 (or only tier1+tier2) for zero would silently price
// zones above 10,000 at $0.00/mo instead of the fallback rate.
func TestPriceDNS_ZoneTieredFallbackWhenTier3Zero(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected HTTP call: fetchDNSRates should have used the cached rate map")
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	// Seed a live rates map with nonzero tier1/tier2 but a zero tier3.
	seeded := dnsRates{
		ZoneTier1:  0.20,
		ZoneTier2:  0.10,
		ZoneTier3:  0, // missing
		QueryTier1: dnsFallbackRates.QueryTier1,
		QueryTier2: dnsFallbackRates.QueryTier2,
	}
	raw, err := json.Marshal(seeded)
	if err != nil {
		t.Fatalf("marshal seeded rates: %v", err)
	}
	p.cache.SetMetadata(dnsRatesCacheKey, raw, p.cfg.MetadataTTL())

	spec := &models.DNSPricingSpec{}
	prices, breakdown, err := p.priceDNS(ctx, spec)
	if err != nil {
		t.Fatalf("priceDNS: %v", err)
	}
	fb, ok := breakdown["fallback"]
	if !ok || fb != true {
		t.Errorf("expected fallback=true when zone tier3 resolves to 0, got %v (ok=%v)", fb, ok)
	}
	for _, pr := range prices {
		if pr.Unit == models.PriceUnitPerZoneMonth && abs(pr.PricePerUnit-dnsFallbackRates.ZoneTier1) > 1e-9 {
			t.Errorf("headline zone rate = %.6f, want %.6f (fallback tier1)", pr.PricePerUnit, dnsFallbackRates.ZoneTier1)
		}
	}
	tier3 := mustFloat64(t, breakdown["zone_tier3_rate"], "zone_tier3_rate")
	if abs(tier3-dnsFallbackRates.ZoneTier3) > 1e-9 {
		t.Errorf("zone_tier3_rate = %.6f, want %.6f (fallback tier3, not $0.00)", tier3, dnsFallbackRates.ZoneTier3)
	}

	// Verify tier3 usage is actually priced at the fallback rate, not $0.00:
	// 10,010 zones crosses the 10,000 threshold, so 10 must be billed at the
	// fallback tier3 rate.
	zoneCount := 10010.0
	spec.ZoneCount = &zoneCount
	_, breakdown, err = p.priceDNS(ctx, spec)
	if err != nil {
		t.Fatalf("priceDNS: %v", err)
	}
	want := dnsZoneTier2Threshold*dnsFallbackRates.ZoneTier1 +
		(dnsZoneTier3Threshold-dnsZoneTier2Threshold)*dnsFallbackRates.ZoneTier2 +
		(zoneCount-dnsZoneTier3Threshold)*dnsFallbackRates.ZoneTier3
	got := mustFloat64(t, breakdown["zone_monthly_cost"], "zone_monthly_cost")
	if abs(got-want) > 1e-6 {
		t.Errorf("zone_monthly_cost = %.4f, want %.4f (tier3 usage priced at fallback rate, not $0.00)", got, want)
	}
}

// TestPriceDNS_QueryTieredFallbackWhenTier2Zero verifies that priceDNS
// engages the fallback path when the live rate map has a nonzero
// QueryTier1 but a zero QueryTier2. Regression test mirroring
// TestPriceKMS_TieredFallbackWhenTier2Zero for the query dimension.
func TestPriceDNS_QueryTieredFallbackWhenTier2Zero(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected HTTP call: fetchDNSRates should have used the cached rate map")
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	seeded := dnsRates{
		ZoneTier1:  dnsFallbackRates.ZoneTier1,
		ZoneTier2:  dnsFallbackRates.ZoneTier2,
		ZoneTier3:  dnsFallbackRates.ZoneTier3,
		QueryTier1: 0.0000009,
		QueryTier2: 0, // missing
	}
	raw, err := json.Marshal(seeded)
	if err != nil {
		t.Fatalf("marshal seeded rates: %v", err)
	}
	p.cache.SetMetadata(dnsRatesCacheKey, raw, p.cfg.MetadataTTL())

	spec := &models.DNSPricingSpec{}
	prices, breakdown, err := p.priceDNS(ctx, spec)
	if err != nil {
		t.Fatalf("priceDNS: %v", err)
	}
	fb, ok := breakdown["fallback"]
	if !ok || fb != true {
		t.Errorf("expected fallback=true when query tier2 resolves to 0, got %v (ok=%v)", fb, ok)
	}
	for _, pr := range prices {
		if pr.Unit == models.PriceUnitPerQuery && abs(pr.PricePerUnit-dnsFallbackRates.QueryTier1) > 1e-12 {
			t.Errorf("headline query rate = %.9f, want %.9f (fallback tier1)", pr.PricePerUnit, dnsFallbackRates.QueryTier1)
		}
	}
	tier2 := mustFloat64(t, breakdown["query_tier2_rate"], "query_tier2_rate")
	if abs(tier2-dnsFallbackRates.QueryTier2) > 1e-12 {
		t.Errorf("query_tier2_rate = %.9f, want %.9f (fallback tier2, not $0.00)", tier2, dnsFallbackRates.QueryTier2)
	}

	// Verify tier2 usage is actually priced at the fallback rate, not $0.00.
	queries := 1_100_000_000.0
	spec.QueriesPerMonth = &queries
	_, breakdown, err = p.priceDNS(ctx, spec)
	if err != nil {
		t.Fatalf("priceDNS: %v", err)
	}
	want := dnsQueryTier2Threshold*dnsFallbackRates.QueryTier1 + (queries-dnsQueryTier2Threshold)*dnsFallbackRates.QueryTier2
	got := mustFloat64(t, breakdown["query_monthly_cost"], "query_monthly_cost")
	if abs(got-want) > 1e-3 {
		t.Errorf("query_monthly_cost = %.4f, want %.4f (tier2 usage priced at fallback rate, not $0.00)", got, want)
	}
}

// TestPriceDNS_ManagedZoneMissingLeavesQueryUnaffected verifies that a live
// SKU response missing the ManagedZone SKU entirely (but with the DNS Query
// SKU present) triggers fallback only for the zone dimension, while the
// query rate is still sourced live — the two dimensions' fallback decisions
// are independent.
func TestPriceDNS_ManagedZoneMissingLeavesQueryUnaffected(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Only the DNS Query SKU — ManagedZone is absent.
		_, _ = w.Write(dnsSKUResponse([]map[string]any{
			dnsSKUTwoTier("DNS Query (port 53)", 1_000_000_000, "0", 400, "0", 200),
		}))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spec := &models.DNSPricingSpec{}
	prices, breakdown, err := p.priceDNS(ctx, spec)
	if err != nil {
		t.Fatalf("priceDNS: %v", err)
	}
	fb, ok := breakdown["fallback"]
	if !ok || fb != true {
		t.Errorf("expected fallback=true when ManagedZone SKU is missing, got %v (ok=%v)", fb, ok)
	}
	for _, pr := range prices {
		if pr.Unit == models.PriceUnitPerZoneMonth && abs(pr.PricePerUnit-dnsFallbackRates.ZoneTier1) > 1e-9 {
			t.Errorf("zone rate = %.6f, want %.6f (fallback, SKU missing)", pr.PricePerUnit, dnsFallbackRates.ZoneTier1)
		}
		if pr.Unit == models.PriceUnitPerQuery && abs(pr.PricePerUnit-0.0000004) > 1e-12 {
			t.Errorf("query rate = %.9f, want 0.000000400 (live, SKU present)", pr.PricePerUnit)
		}
	}
}

// --------------------------------------------------------------------------
// skuAllTierRates unit test
// --------------------------------------------------------------------------

// TestSkuAllTierRates_ExtractsThreeTiersSortedByThreshold verifies the
// helper used specifically because skuPrice/skuPaidPrice (two-tier-oriented)
// cannot reach the ManagedZone SKU's third tier.
func TestSkuAllTierRates_ExtractsThreeTiersSortedByThreshold(t *testing.T) {
	sku := dnsSKUThreeTier("ManagedZone", 25, 10000, "0", 200_000_000, "0", 100_000_000, "0", 30_000_000)
	got := skuAllTierRates(sku)
	if len(got) != 3 {
		t.Fatalf("expected 3 tiers, got %d: %v", len(got), got)
	}
	want := []float64{0.20, 0.10, 0.03}
	for i := range want {
		if abs(got[i]-want[i]) > 1e-9 {
			t.Errorf("tier[%d] = %.6f, want %.6f", i, got[i], want[i])
		}
	}
}

// TestSkuAllTierRates_SingleTier verifies the helper degrades gracefully to a
// one-element slice for a single-tier SKU (simulating a partial live
// response).
func TestSkuAllTierRates_SingleTier(t *testing.T) {
	sku := dnsSKUSingleTier("ManagedZone", "0", 200_000_000)
	got := skuAllTierRates(sku)
	if len(got) != 1 {
		t.Fatalf("expected 1 tier, got %d: %v", len(got), got)
	}
	if abs(got[0]-0.20) > 1e-9 {
		t.Errorf("tier[0] = %.6f, want 0.200000", got[0])
	}
}
