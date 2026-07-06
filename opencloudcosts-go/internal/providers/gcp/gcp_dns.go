// gcp_dns.go — GCP Cloud DNS pricing (domain=dns, service=cloud_dns).
//
// Cloud DNS bills along two independent, region-invariant dimensions:
//   - a per-zone-month charge (ManagedZone), volume-discounted by the total
//     managed-zone count: $0.20/zone-month for zones 1-25, $0.10/zone-month
//     for zones 26-10,000, $0.03/zone-month for zones 10,001+; and
//   - a per-query charge (DNS Query (port 53)), volume-discounted above
//     1,000,000,000 queries/month: $0.0000004/query (equivalently
//     $0.40/million) up to 1B queries/month, then $0.0000002/query
//     ($0.20/million) thereafter.
//
// All rates below were verified live against the GCP Cloud Billing Catalog
// API (service ID FA26-5236-B8B5, "Cloud DNS") and cross-checked against
// https://cloud.google.com/dns/pricing (see issue #78). Both in-scope SKUs
// have geoTaxonomy.type == "GLOBAL" — price does not vary by region, so this
// file never queries or matches on region; every returned NormalizedPrice is
// tagged Region="global" and Attributes["scope"]="global", mirroring the
// Cloud KMS (#77) and External IP Charge (#76) precedent.
//
// Public, private, and forwarding zone types are all aggregated under the
// single ManagedZone SKU/tier ladder above — verified live, there is no
// per-zone-type rate difference. DNS Peering zones are assumed (not
// explicitly confirmed on the pricing page) to aggregate the same way.
// zone_type is therefore modeled as a validated, informational attribute
// only; it never changes the rate selected.
//
// Explicitly out of scope (not priced by this file):
//   - Routing policy queries ($0.70/$0.35 per million) — a real, documented
//     GCP charge, but with no catalog SKU under this service ID.
//   - Health checks and "DNS Armor" advanced threat detection — verified
//     live to not be part of the Cloud DNS service ID at all (a full
//     1777-service catalog scan found no dedicated service for either).
//   - DNSSEC and Response Policies/Response Policy Zones — verified live to
//     not be billed as separate items at all as of the pricing page.
//
// All methods are on *Provider defined in gcp.go (Part 1).
package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// dnsServiceID is the GCP Cloud Billing Catalog service ID for "Cloud DNS" —
// verified live 2026-07-05.
const dnsServiceID = "FA26-5236-B8B5"

// dnsSourceURL is the canonical public pricing page used for cross-checking
// and as the SourceURL stamped on returned prices.
const dnsSourceURL = "https://cloud.google.com/dns/pricing"

// dnsZoneTier2Threshold and dnsZoneTier3Threshold are the ManagedZone
// volume-discount tier boundaries, in zone-count (verified live: tiered SKU
// 8C22-6FC3-D478 has startUsageAmount 0, 25, and 10000).
//
// dnsQueryTier2Threshold is the DNS Query tier boundary, in queries/month
// (verified live: tiered SKU 6DFF-5025-A128 has startUsageAmount 0 and
// 1,000,000,000).
//
// Known limitation: like kmsHSMTierThreshold in gcp_kms.go, these thresholds
// are hardcoded Go constants rather than derived from the live SKU
// tieredRates.startUsageAmount values fetchDNSRates already parses. GCP
// changing a tier boundary without also changing the corresponding rate is
// considered unlikely. Revisit if that assumption ever breaks.
const (
	dnsZoneTier2Threshold  = 25
	dnsZoneTier3Threshold  = 10000
	dnsQueryTier2Threshold = 1_000_000_000
)

// dnsRates holds the derived, region-invariant Cloud DNS rates, cached under
// dnsRatesCacheKey so repeated calls for different (zone_type, zone_count,
// queries_per_month) combinations don't re-fetch/re-scan the Cloud DNS SKU
// catalog.
type dnsRates struct {
	ZoneTier1  float64 `json:"zone_tier1"`  // 8C22-6FC3-D478 tier1 — $/zone-month, zones 1-25
	ZoneTier2  float64 `json:"zone_tier2"`  // 8C22-6FC3-D478 tier2 — $/zone-month, zones 26-10,000
	ZoneTier3  float64 `json:"zone_tier3"`  // 8C22-6FC3-D478 tier3 — $/zone-month, zones 10,001+
	QueryTier1 float64 `json:"query_tier1"` // 6DFF-5025-A128 tier1 — $/query, 0-1,000,000,000 queries/mo
	QueryTier2 float64 `json:"query_tier2"` // 6DFF-5025-A128 tier2 — $/query, 1,000,000,000+ queries/mo
}

// dnsFallbackRates holds the published, live-verified rates (issue #78) used
// when the live SKU catalog is unavailable or a description match fails.
var dnsFallbackRates = dnsRates{
	ZoneTier1:  0.20,
	ZoneTier2:  0.10,
	ZoneTier3:  0.03,
	QueryTier1: 0.0000004,
	QueryTier2: 0.0000002,
}

// dnsRatesCacheKey caches the derived Cloud DNS rates.
const dnsRatesCacheKey = "gcp:dns:rates"

// dnsValidZoneTypes is the complete set of recognized DNSPricingSpec.ZoneType
// values. priceDNS rejects anything outside this set with an explicit error
// rather than silently accepting a typo.
var dnsValidZoneTypes = map[string]bool{
	"public":     true,
	"private":    true,
	"forwarding": true,
	"peering":    true,
}

// skuAllTierRates extracts every tiered unit price from a raw SKU, sorted
// ascending by startUsageAmount. Unlike skuPrice (gcp.go — first tier only,
// startUsageAmount==0) and skuPaidPrice (gcp_ai.go — first paid tier only,
// startUsageAmount>0), this surfaces every tier. It exists specifically for
// the Cloud DNS ManagedZone SKU, which has THREE tiers (zones 1-25,
// 26-10,000, 10,001+) — skuPrice/skuPaidPrice, designed for two-tier SKUs,
// cannot reach the third tier.
func skuAllTierRates(sku map[string]any) []float64 {
	pi, _ := sku["pricingInfo"].([]any)
	if len(pi) == 0 {
		return nil
	}
	pe, _ := pi[0].(map[string]any)
	if pe == nil {
		return nil
	}
	expr, _ := pe["pricingExpression"].(map[string]any)
	if expr == nil {
		return nil
	}
	tiers, _ := expr["tieredRates"].([]any)

	type startPrice struct {
		start float64
		price float64
	}
	parsed := make([]startPrice, 0, len(tiers))
	for _, t := range tiers {
		tier, _ := t.(map[string]any)
		if tier == nil {
			continue
		}
		up, _ := tier["unitPrice"].(map[string]any)
		if up == nil {
			continue
		}
		start, _ := tier["startUsageAmount"].(float64)
		units, _ := up["units"].(string)
		nanos, _ := up["nanos"].(float64)
		parsed = append(parsed, startPrice{start: start, price: gcpMoney(units, int(nanos))})
	}
	sort.Slice(parsed, func(i, j int) bool { return parsed[i].start < parsed[j].start })

	out := make([]float64, len(parsed))
	for i, sp := range parsed {
		out[i] = sp.price
	}
	return out
}

// fetchDNSRates returns the live, derived Cloud DNS rates, caching the
// result. A zero field means no matching SKU/tier was found for that rate
// bucket and the caller should fall back to dnsFallbackRates.
//
// Matching is case-insensitive substring matching against SKU descriptions
// (never exact-string-equality — the same convention as gcp_kms.go, since
// GCP catalog wording can drift).
func (p *Provider) fetchDNSRates(ctx context.Context) dnsRates {
	if raw, ok := p.cache.GetMetadata(dnsRatesCacheKey); ok {
		var r dnsRates
		if err := json.Unmarshal(raw, &r); err == nil {
			return r
		}
	}

	var rates dnsRates
	skus, err := p.fetchSKUs(ctx, dnsServiceID)
	if err != nil {
		slog.Warn("gcp dns: fetch SKUs failed", "err", err)
		return rates
	}

	for _, sku := range skus {
		desc, _ := sku["description"].(string)
		descLower := strings.ToLower(desc)

		switch {
		case strings.Contains(descLower, "managed zone") || strings.Contains(descLower, "managedzone"):
			tiers := skuAllTierRates(sku)
			if len(tiers) > 0 {
				rates.ZoneTier1 = tiers[0]
			}
			if len(tiers) > 1 {
				rates.ZoneTier2 = tiers[1]
			}
			if len(tiers) > 2 {
				rates.ZoneTier3 = tiers[2]
			}

		case strings.Contains(descLower, "dns query"):
			tiers := skuAllTierRates(sku)
			if len(tiers) > 0 {
				rates.QueryTier1 = tiers[0]
			}
			if len(tiers) > 1 {
				rates.QueryTier2 = tiers[1]
			}
		}
	}

	if raw, err := json.Marshal(rates); err == nil {
		p.cache.SetMetadata(dnsRatesCacheKey, raw, p.cfg.MetadataTTL())
	}
	return rates
}

// priceDNS returns Cloud DNS pricing for the given DNSPricingSpec: one
// NormalizedPrice for the ManagedZone rate and one for the DNS Query rate.
// Zone-count and query-volume are independent, complementary billing
// dimensions (unlike Cloud KMS's mutually-exclusive unit selector), so both
// line items are always returned together, mirroring the multi-line-item
// shape used by Cloud LB/Cloud NAT in gcp_networking.go.
func (p *Provider) priceDNS(
	ctx context.Context,
	spec *models.DNSPricingSpec,
) ([]models.NormalizedPrice, map[string]any, error) {
	rates := p.fetchDNSRates(ctx)

	zoneType := strings.ToLower(spec.ZoneType)
	if zoneType == "" {
		zoneType = "public"
	}
	if !dnsValidZoneTypes[zoneType] {
		return nil, nil, fmt.Errorf("gcp dns: invalid zone_type %q: must be one of 'public', 'private', 'forwarding', 'peering'", spec.ZoneType)
	}

	// Zone tiers: a tiered rate is only fully resolved when ALL THREE tiers
	// come back nonzero from the live catalog; if any is zero (e.g. a live
	// response populates tier1 but omits tier2/tier3), all three must fall
	// back together so a missing tier is never silently priced at $0.00 —
	// the same guard as kmsHSMKeyVersionTier1/Tier2 in gcp_kms.go, extended
	// to three tiers.
	zoneTier1, fbZ1 := pickRate(rates.ZoneTier1, dnsFallbackRates.ZoneTier1)
	zoneTier2, fbZ2 := pickRate(rates.ZoneTier2, dnsFallbackRates.ZoneTier2)
	zoneTier3, fbZ3 := pickRate(rates.ZoneTier3, dnsFallbackRates.ZoneTier3)
	zoneFallback := fbZ1 || fbZ2 || fbZ3
	if zoneFallback {
		zoneTier1 = dnsFallbackRates.ZoneTier1
		zoneTier2 = dnsFallbackRates.ZoneTier2
		zoneTier3 = dnsFallbackRates.ZoneTier3
	}

	// Query tiers: same all-or-nothing guard as the zone tiers above.
	queryTier1, fbQ1 := pickRate(rates.QueryTier1, dnsFallbackRates.QueryTier1)
	queryTier2, fbQ2 := pickRate(rates.QueryTier2, dnsFallbackRates.QueryTier2)
	queryFallback := fbQ1 || fbQ2
	if queryFallback {
		queryTier1 = dnsFallbackRates.QueryTier1
		queryTier2 = dnsFallbackRates.QueryTier2
	}

	attrs := map[string]string{"zone_type": zoneType}

	zonePrice := models.NormalizedPrice{
		Provider:      models.CloudProviderGCP,
		Service:       "cloud_dns",
		SKUID:         fmt.Sprintf("gcp:dns:managedzone:%s", zoneType),
		ProductFamily: "Cloud DNS",
		Description:   "Cloud DNS ManagedZone (volume-discounted per zone-month; all zone types share one tier ladder)",
		PricingTerm:   models.PricingTermOnDemand,
		PricePerUnit:  zoneTier1,
		Unit:          models.PriceUnitPerZoneMonth,
		Currency:      "USD",
		Attributes:    attrs,
	}
	stampGlobalScope(&zonePrice)

	queryPrice := models.NormalizedPrice{
		Provider:      models.CloudProviderGCP,
		Service:       "cloud_dns",
		SKUID:         "gcp:dns:query",
		ProductFamily: "Cloud DNS",
		Description:   "DNS Query (port 53)",
		PricingTerm:   models.PricingTermOnDemand,
		PricePerUnit:  queryTier1,
		Unit:          models.PriceUnitPerQuery,
		Currency:      "USD",
		Attributes:    map[string]string{"zone_type": zoneType},
	}
	stampGlobalScope(&queryPrice)

	breakdown := map[string]any{
		"zone_type":        zoneType,
		"zone_tier1_rate":  breakdownMoney(zoneTier1, "/zone-month (zones 1-25)"),
		"zone_tier2_rate":  breakdownMoney(zoneTier2, "/zone-month (zones 26-10,000)"),
		"zone_tier3_rate":  breakdownMoney(zoneTier3, "/zone-month (zones 10,001+)"),
		"query_tier1_rate": breakdownMoney(queryTier1, "/query (0-1,000,000,000 queries/mo)"),
		"query_tier2_rate": breakdownMoney(queryTier2, "/query (1,000,000,000+ queries/mo)"),
		"zone_type_note":   "zone_type does not affect price: public, private, forwarding, and peering zones all share the same ManagedZone tier ladder (verified live).",
	}
	if zoneFallback || queryFallback {
		breakdown["fallback"] = true
		breakdown["fallback_note"] = "Using hardcoded fallback rate(s); live SKU catalog unavailable or returned no match. Verify current rates at " + dnsSourceURL + "."
	}

	// monthly_cost (and the per-dimension zone_monthly_cost/query_monthly_cost)
	// must account for the tiered volume-discount structure surfaced above —
	// a flat rate*quantity product would overcharge/undercharge across a tier
	// boundary. Both dimensions are expressed as an []egressTier list and
	// priced through the shared computeTieredCost helper (gcp_networking.go)
	// rather than hand-rolling clamp-and-subtract arithmetic.
	// egressTier.thresholdGB is reused here as a generic tier threshold
	// (zone count or query count, not gigabytes) — the field name is a
	// holdover from its original egress-pricing use (see gcp_kms.go for the
	// same reuse pattern).
	var totalCost float64
	haveEstimate := false
	if spec.ZoneCount != nil {
		zoneTiers := []egressTier{
			{thresholdGB: 0, rate: zoneTier1, label: "tier1"},
			{thresholdGB: dnsZoneTier2Threshold, rate: zoneTier2, label: "tier2"},
			{thresholdGB: dnsZoneTier3Threshold, rate: zoneTier3, label: "tier3"},
		}
		result := computeTieredCost(zoneTiers, *spec.ZoneCount)
		breakdown["zone_monthly_cost"] = breakdownMoney(result.TotalCost, "/mo")
		breakdown["zone_tier_breakdown"] = result.TierBreakdown
		totalCost += result.TotalCost
		haveEstimate = true
	}
	if spec.QueriesPerMonth != nil {
		queryTiers := []egressTier{
			{thresholdGB: 0, rate: queryTier1, label: "tier1"},
			{thresholdGB: dnsQueryTier2Threshold, rate: queryTier2, label: "tier2"},
		}
		result := computeTieredCost(queryTiers, *spec.QueriesPerMonth)
		breakdown["query_monthly_cost"] = breakdownMoney(result.TotalCost, "/mo")
		breakdown["query_tier_breakdown"] = result.TierBreakdown
		totalCost += result.TotalCost
		haveEstimate = true
	}
	if haveEstimate {
		breakdown["monthly_cost"] = breakdownMoney(totalCost, "/mo")
	}

	return annotateFreshWithURL([]models.NormalizedPrice{zonePrice, queryPrice}, dnsSourceURL), breakdown, nil
}
