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

// dnsKnownZoneTypes is the set of DNSPricingSpec.ZoneType values verified
// against the public pricing page. zone_type is a purely descriptive,
// non-rate-selecting attribute (see file header) — an unrecognized value is
// therefore surfaced as an informational breakdown note rather than
// rejected, so a new GCP zone type (or a caller's slightly different
// terminology) never blocks an otherwise-correct price lookup.
var dnsKnownZoneTypes = map[string]bool{
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
// cannot reach the third tier. All three share the low-level JSON-unwrap
// step via skuTierList (gcp.go).
func skuAllTierRates(sku map[string]any) []float64 {
	tiers := skuTierList(sku)
	out := make([]float64, len(tiers))
	for i, t := range tiers {
		out[i] = t.price
	}
	return out
}

// fetchDNSRates returns the live, derived Cloud DNS rates, caching the
// result. A zero field means no matching SKU/tier was found for that rate
// bucket and the caller should fall back to dnsFallbackRates.
//
// Matching is case-insensitive substring matching against SKU descriptions
// (never exact-string-equality — the same convention as gcp_kms.go, since
// GCP catalog wording can drift). Both in-scope SKUs are documented (file
// header) as geoTaxonomy.type=="GLOBAL"; any SKU whose geoTaxonomy.type is
// present and non-GLOBAL is skipped defensively so a regional SKU that
// happens to share substring wording is never mistaken for the (global)
// Cloud DNS rate. If more than one SKU still matches a given bucket, the
// first match wins and later matches are logged and discarded rather than
// silently overwriting it (last-write-wins).
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

	var zoneMatched, queryMatched bool
	for _, sku := range skus {
		desc, _ := sku["description"].(string)
		descLower := strings.ToLower(desc)

		if geo, ok := sku["geoTaxonomy"].(map[string]any); ok {
			if geoType, _ := geo["type"].(string); geoType != "" && geoType != "GLOBAL" {
				continue
			}
		}

		switch {
		case strings.Contains(descLower, "managed zone") || strings.Contains(descLower, "managedzone"):
			if zoneMatched {
				slog.Warn("gcp dns: multiple SKUs matched 'managed zone'; keeping first match", "description", desc)
				continue
			}
			zoneMatched = true
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
			if queryMatched {
				slog.Warn("gcp dns: multiple SKUs matched 'dns query'; keeping first match", "description", desc)
				continue
			}
			queryMatched = true
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

// resolveTieredRates resolves each of fallback's per-tier rates against the
// corresponding live rate via pickRate (gcp_kms.go), then applies the same
// all-or-nothing guard used for Cloud KMS's two-tier rates: if ANY live tier
// is missing (zero), ALL tiers for that dimension fall back together, so a
// partially-populated live response (e.g. tier1 present, tier2/tier3 absent)
// is never silently priced with one tier live and another stale/zero.
// live and fallback must be the same length; extra fallback entries beyond
// len(live) are treated as missing from live.
func resolveTieredRates(live []float64, fallback []float64) (rates []float64, usedFallback bool) {
	rates = make([]float64, len(fallback))
	for i, fb := range fallback {
		var l float64
		if i < len(live) {
			l = live[i]
		}
		r, fbUsed := pickRate(l, fb)
		rates[i] = r
		if fbUsed {
			usedFallback = true
		}
	}
	if usedFallback {
		copy(rates, fallback)
	}
	return rates, usedFallback
}

// newDNSPrice builds a Cloud DNS NormalizedPrice with the fields common to
// every DNS line item (Provider/Service/ProductFamily/PricingTerm/Currency)
// already filled in and global scope already stamped, so priceDNS's
// per-line-item construction only needs to supply what actually differs.
func newDNSPrice(skuID, description string, pricePerUnit float64, unit models.PriceUnit, attrs map[string]string) *models.NormalizedPrice {
	price := &models.NormalizedPrice{
		Provider:      models.CloudProviderGCP,
		Service:       "cloud_dns",
		SKUID:         skuID,
		ProductFamily: "Cloud DNS",
		Description:   description,
		PricingTerm:   models.PricingTermOnDemand,
		PricePerUnit:  pricePerUnit,
		Unit:          unit,
		Currency:      "USD",
		Attributes:    attrs,
	}
	stampGlobalScope(price)
	return price
}

// addTieredEstimate computes the tiered cost for quantity over tiers via
// computeTieredCost (gcp_networking.go), stashes the "<key>_monthly_cost"
// and "<key>_tier_breakdown" entries into breakdown, and returns the total
// cost. It is shared by priceDNS's ZoneCount and QueriesPerMonth branches,
// which previously each duplicated this assembly independently.
func addTieredEstimate(breakdown map[string]any, key string, tiers []egressTier, quantity float64) (cost float64, ok bool) {
	result := computeTieredCost(tiers, quantity)
	breakdown[key+"_monthly_cost"] = breakdownMoney(result.TotalCost, "/mo")
	breakdown[key+"_tier_breakdown"] = result.TierBreakdown
	return result.TotalCost, true
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
	// Cheap, in-memory zone_type normalization first, before the
	// potentially cold-cache/network-bound fetchDNSRates call below.
	// zone_type is validated only informationally (dnsKnownZoneTypes, below)
	// and never causes an error — see its doc comment — so there is nothing
	// here that can currently abort the request; the ordering is kept
	// anyway as the correct default shape (validate cheaply before doing
	// I/O) for any future validation added to this spec.
	zoneType := strings.ToLower(spec.ZoneType)
	if zoneType == "" {
		zoneType = "public"
	}
	zoneTypeRecognized := dnsKnownZoneTypes[zoneType]

	rates := p.fetchDNSRates(ctx)

	// Zone tiers and query tiers each get the same all-or-nothing
	// live/fallback resolution via the shared resolveTieredRates helper
	// (see its doc comment) — previously duplicated near-verbatim once per
	// dimension.
	zoneRates, zoneFallback := resolveTieredRates(
		[]float64{rates.ZoneTier1, rates.ZoneTier2, rates.ZoneTier3},
		[]float64{dnsFallbackRates.ZoneTier1, dnsFallbackRates.ZoneTier2, dnsFallbackRates.ZoneTier3},
	)
	zoneTier1, zoneTier2, zoneTier3 := zoneRates[0], zoneRates[1], zoneRates[2]

	queryRates, queryFallback := resolveTieredRates(
		[]float64{rates.QueryTier1, rates.QueryTier2},
		[]float64{dnsFallbackRates.QueryTier1, dnsFallbackRates.QueryTier2},
	)
	queryTier1, queryTier2 := queryRates[0], queryRates[1]

	// zonePrice and queryPrice's Attributes both carry only zone_type today.
	// queryAttrs is a copy of zoneAttrs, not a shared reference: stampGlobalScope
	// (called once per price via newDNSPrice) mutates its Attributes map in
	// place, so two NormalizedPrices must never point at the same map.
	zoneAttrs := map[string]string{"zone_type": zoneType}
	queryAttrs := make(map[string]string, len(zoneAttrs))
	for k, v := range zoneAttrs {
		queryAttrs[k] = v
	}

	zonePrice := newDNSPrice(
		fmt.Sprintf("gcp:dns:managedzone:%s", zoneType),
		"Cloud DNS ManagedZone (volume-discounted per zone-month; all zone types share one tier ladder)",
		zoneTier1,
		models.PriceUnitPerZoneMonth,
		zoneAttrs,
	)
	queryPrice := newDNSPrice(
		"gcp:dns:query",
		"DNS Query (port 53)",
		queryTier1,
		models.PriceUnitPerQuery,
		queryAttrs,
	)

	breakdown := map[string]any{
		"zone_type":        zoneType,
		"zone_tier1_rate":  breakdownMoney(zoneTier1, "/zone-month (zones 1-25)"),
		"zone_tier2_rate":  breakdownMoney(zoneTier2, "/zone-month (zones 26-10,000)"),
		"zone_tier3_rate":  breakdownMoney(zoneTier3, "/zone-month (zones 10,001+)"),
		"query_tier1_rate": breakdownMoney(queryTier1, "/query (0-1,000,000,000 queries/mo)"),
		"query_tier2_rate": breakdownMoney(queryTier2, "/query (1,000,000,000+ queries/mo)"),
		"zone_type_note":   "zone_type does not affect price: public, private, forwarding, and peering zones all share the same ManagedZone tier ladder (verified live).",
	}
	if !zoneTypeRecognized {
		breakdown["zone_type_unrecognized"] = true
		breakdown["zone_type_unrecognized_note"] = fmt.Sprintf(
			"zone_type %q is not one of the verified values (public, private, forwarding, peering); this does not affect price, since zone_type never changes the rate.",
			spec.ZoneType,
		)
	}
	if zoneFallback || queryFallback {
		breakdown["fallback"] = true
		breakdown["fallback_note"] = "Using hardcoded fallback rate(s); live SKU catalog unavailable or returned no match. Verify current rates at " + dnsSourceURL + "."
	}

	// monthly_cost (and the per-dimension zone_monthly_cost/query_monthly_cost)
	// must account for the tiered volume-discount structure surfaced above —
	// a flat rate*quantity product would overcharge/undercharge across a tier
	// boundary. Both dimensions are expressed as an []egressTier list and
	// priced through the shared addTieredEstimate helper (above), which itself
	// calls computeTieredCost (gcp_networking.go), rather than hand-rolling
	// clamp-and-subtract arithmetic. egressTier.thresholdGB is reused here as
	// a generic tier threshold (zone count or query count, not gigabytes) —
	// the field name is a holdover from its original egress-pricing use (see
	// gcp_kms.go for the same reuse pattern).
	var totalCost float64
	haveEstimate := false
	if spec.ZoneCount != nil {
		zoneTiers := []egressTier{
			{thresholdGB: 0, rate: zoneTier1, label: "tier1"},
			{thresholdGB: dnsZoneTier2Threshold, rate: zoneTier2, label: "tier2"},
			{thresholdGB: dnsZoneTier3Threshold, rate: zoneTier3, label: "tier3"},
		}
		cost, _ := addTieredEstimate(breakdown, "zone", zoneTiers, *spec.ZoneCount)
		totalCost += cost
		haveEstimate = true
	}
	if spec.QueriesPerMonth != nil {
		queryTiers := []egressTier{
			{thresholdGB: 0, rate: queryTier1, label: "tier1"},
			{thresholdGB: dnsQueryTier2Threshold, rate: queryTier2, label: "tier2"},
		}
		cost, _ := addTieredEstimate(breakdown, "query", queryTiers, *spec.QueriesPerMonth)
		totalCost += cost
		haveEstimate = true
	}
	if haveEstimate {
		breakdown["monthly_cost"] = breakdownMoney(totalCost, "/mo")
	}

	return annotateFreshWithURL([]models.NormalizedPrice{*zonePrice, *queryPrice}, dnsSourceURL), breakdown, nil
}
