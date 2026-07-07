// azure_sku_lookup.go implements get_price_by_sku's Azure counterpart to
// AWS's raw usage-type/SKU lookup (internal/providers/aws/aws_sku_lookup.go)
// and GCP's raw skuId lookup (internal/providers/gcp/gcp_sku_lookup.go):
// given a raw Azure Retail Prices API "meterId" string (a GUID-shaped token,
// e.g. "0019e0b6-728e-5eae-b900-5b02fa9ba3c9"), find its price in a list of
// target regions.
//
// serviceHint IS ACCEPTED BUT NOT USED (interface conformance only):
// Unlike AWS (which needs a servicecode to pick which offer-file catalog to
// fetch) and GCP (which needs a service ID to pick which of 13 onboarded
// catalogs to scan), a meterId is already a sufficient server-side filter on
// its own — the Azure Retail Prices API's $filter=meterId eq '...' returns
// every row for that meter across every region/type/tier in one shot,
// regardless of which service billed it. So serviceHint is echoed back on
// the result (ServiceHint) for API-shape parity with the other two
// providers, but it never narrows the fetch or the match. This mirrors how
// AWS's now-permanently-dead providerName validation branch is documented as
// intentionally-unused-but-present in aws_sku_lookup.go, and how GCP leaves
// skulookup.SKUHint entirely unused (see gcp_sku_lookup.go's doc comment).
//
// ONE FETCH COVERS EVERY REGION:
// A single fetchPrices(ctx, {"meterId": sku}, ...) call — with NO
// armRegionName filter — returns every region's row for that meterId at
// once (confirmed against the live API). This file never fetches per
// requested region; it fetches once, buckets the raw rows by ArmRegionName,
// and then runs the disambiguation algorithm below independently against
// each requested region's bucket.
//
// DISAMBIGUATION ALGORITHM (per requested region, in this exact order):
//  1. Filter rows to this region (done by the bucketing step, not repeated
//     per call — see byRegion in getOrFetchAzureSKUCatalog).
//  2. If the bucket mixes IsPrimaryMeterRegion true/false, keep only the
//     true row(s) — this axis is independent of, and resolved strictly
//     before, the type-based narrowing in step 5.
//  3. Zero rows remaining (after step 1 or step 2) => NoMapping for this
//     region.
//  4. Exactly one row remaining => that's the match.
//  5. More than one row remaining => apply hint.ProductFamilyHint. Azure
//     gives this field Azure-specific meaning (NOT the same meaning AWS's
//     productFamily hint carries, which matches AWS's top-level
//     "productFamily" attribute):
//     - hint == "spot" (case-insensitive): filter to rows whose MeterName
//     contains "Spot" (case-insensitive substring) — Azure has no
//     type=="Spot" value; Spot pricing is identified purely by a
//     meterName substring.
//     - any other non-empty hint: filter to rows where
//     Type == hint (case-insensitive equality).
//     - no hint supplied: default-filter to rows where Type ==
//     "Consumption" (the common "on-demand, not Reservation/DevTest"
//     case).
//     Fails closed (this repo's established convention — see
//     resolveSKUCandidates / T41 in aws_sku_lookup.go and
//     docs/plans/T41-sku-lookup.md): a hint matching zero rows reports
//     Ambiguous with HintStatusNoMatch and keeps the ORIGINAL (post-step-2)
//     candidate set in Prices, never silently falling through.
//  6. If step 5 leaves exactly one row: that's the match (HintStatusResolved
//     when an explicit hint was supplied, HintStatusNoHint for the default
//     path). If an EXPLICIT hint narrowed the set but still leaves more than
//     one row: HintStatusAmbiguous, report Ambiguous with every remaining
//     row. No tiebreak is attempted here — see step 7 for why that would be
//     unsafe, and note this also makes the explicit-hint path inherently
//     safe against the Reservation-tier collision described there, since it
//     never tries to pick a "canonical" row out of a multi-row hint match.
//  7. ONLY for the DEFAULT (no-hint) path, when step 5 still leaves more
//     than one row: this is the "graduated tiered pricing" case (multiple
//     Consumption rows for the same product at different usage thresholds).
//     Do NOT assume every such multi-row set is safely resolved by picking
//     the lowest TierMinimumUnits — CONFIRMED FROM LIVE DATA, rows can share
//     an identical (meterId, region, type, isPrimaryMeterRegion,
//     tierMinimumUnits==0.0) tuple while being entirely different products
//     with wildly different RetailPrice (one sampled Reservation collision
//     spanned $5,048 to $118,201, ~23x). "Lowest TierMinimumUnits" does not
//     disambiguate that, since every colliding row ties on that field. So:
//     group by (SkuName, ProductName); resolve to one canonical row plus its
//     sibling tiers ONLY if exactly one group remains AND that group's
//     TierMinimumUnits values are all distinct AND its RetailPrice sequence
//     is monotonic against tier threshold — see resolveAzureTierGroup.
//     Otherwise: Ambiguous (HintStatusAmbiguous), never guess a "cheapest"
//     tiebreak (see docs/plans/T41-sku-lookup.md's documented incident of
//     exactly that failure mode picking the wrong product at ~half price).
//
// CONVERSION: only the winning row(s) per region are ever converted to
// models.NormalizedPrice (via the existing itemToPrice, azure.go), since
// NormalizedPrice drops Type/TierMinimumUnits/IsPrimaryMeterRegion that the
// algorithm above needs while candidates are still being narrowed. When a
// region is Ambiguous, every row that survived to the Ambiguous report (not
// the full unfiltered per-meterId fetch) is converted so the caller can
// inspect them.
//
// UsageTypePrefix/UsageTypeSuffix are AWS-only concepts per
// skulookup.SKULookupResult's doc comment; left "" here, mirroring GCP
// (gcp_sku_lookup.go also leaves them unset).
package azure

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/skulookup"
)

// --------------------------------------------------------------------------
// Input bounds
// --------------------------------------------------------------------------

// azureSKUMaxLength bounds the raw meterId string. Mirrors
// gcp.gcpSKUMaxLength / aws.maxSKULength's rationale: real meterId values are
// short GUIDs, far under this cap; the cap exists only to reject
// pathological/abusive input before it's echoed into error messages or used
// to build the outbound $filter, not to constrain any real meterId shape.
const azureSKUMaxLength = 1024

// azureSKUMaxLookupRegions bounds the regions list, mirroring
// gcp.gcpSKUMaxLookupRegions's rationale: the fetch itself is not
// region-scoped (one call covers every region), but an unbounded regions
// list still means unbounded per-region work building the response.
const azureSKUMaxLookupRegions = 30

// azureSKUMaxHintLength bounds hint.ProductFamilyHint, mirroring
// aws.maxHintLength's rationale — real Azure "type"/meterName values used
// here are short; this is generous headroom, not a real-world constraint.
const azureSKUMaxHintLength = 256

// azureSKULookupMaxResults is the maxResults ceiling passed to fetchPrices
// for this file's single-meterId fetch specifically. It must be sized
// generously: a single meterId's row count across every Azure
// region/type/tier can plausibly reach into the hundreds, and this fetch has
// exactly one chance to see all of them (there is no per-region retry —
// getOrFetchAzureSKUCatalog fetches and buckets once, then every requested
// region is resolved from that single bucketed result). Deliberately much
// larger than any other call site in this package uses.
const azureSKULookupMaxResults = 5000

// --------------------------------------------------------------------------
// Process-lifetime catalog memoization (mirrors aws.skuCatalogCache)
// --------------------------------------------------------------------------
//
// WHY THIS CACHE EXISTS: a single meterId fetch already covers every region
// in one HTTP round trip (unlike AWS, which must re-fetch an entire
// per-(service,region) offer file for every candidate). But a caller
// reconciling a CUR/cost-export line-by-line can still repeat the same
// meterId lookup many times (once per invoice line referencing it, or once
// per requested-region re-run with a different hint), and concurrent lookups
// for the same meterId should collapse into one fetch rather than a
// stampede. This is a small, Azure-SKU-lookup-scoped, in-memory,
// coalescing, TTL-bounded, size-capped cache modeled directly on AWS's
// skuCatalogCache (aws_sku_lookup.go) — see that type's doc comment for the
// full reasoning (which applies here unchanged): it is deliberately NOT a
// bare package-level sync.Once (that would pin a transient network failure
// for the rest of the process's life) and deliberately NOT layered on
// Provider.cache (cache.CacheManager does a whole-file rewrite per Set and
// is designed for many small already-priced entries, not raw per-meterId row
// data that needs to survive a follow-up call with a different hint without
// re-fetching).
var azureSKUCatalogCache = struct {
	mu      sync.Mutex
	entries map[string]*azureSKUCatalogEntry
}{entries: make(map[string]*azureSKUCatalogEntry)}

// defaultAzureSKUCatalogEntryTTL bounds how long a fetched meterId's row set
// is reused before being treated as stale and re-fetched. Mirrors
// aws.defaultSKUCatalogEntryTTL's rationale.
const defaultAzureSKUCatalogEntryTTL = 24 * time.Hour

// maxAzureSKUCatalogCacheEntries hard-caps the number of distinct meterId
// entries held at once, combined with the TTL above bounding worst-case
// memory footprint. Mirrors aws.maxSKUCatalogCacheEntries.
const maxAzureSKUCatalogCacheEntries = 128

// azureSKUCatalogEntry holds the memoized raw-row-bucketed-by-region result
// for one meterId. Storing raw azureRetailItem rows (not
// []models.NormalizedPrice) is deliberate: a cache hit must still be able to
// re-run the disambiguation algorithm with a different hint on a follow-up
// call without re-fetching, and NormalizedPrice has already dropped fields
// (Type, TierMinimumUnits, IsPrimaryMeterRegion) the algorithm needs.
type azureSKUCatalogEntry struct {
	once     sync.Once
	byRegion map[string][]azureRetailItem
	err      error

	// fetchedAt mirrors aws.skuCatalogEntry.fetchedAt: set to time.Now()
	// (under azureSKUCatalogCache.mu, by the sole goroutine running
	// once.Do's body) once the fetch completes; stays the zero Time while a
	// fetch is in flight, so getAzureSKUCatalogEntry/evictOldestAzureSKUEntryLocked
	// never treat/evict an in-flight entry as stale/evictable.
	fetchedAt time.Time
}

// getAzureSKUCatalogEntry returns the (possibly new) cache slot for key,
// creating it under the map mutex if absent or stale. Mirrors
// aws.getSKUCatalogEntry.
func getAzureSKUCatalogEntry(key string, ttl time.Duration) *azureSKUCatalogEntry {
	azureSKUCatalogCache.mu.Lock()
	defer azureSKUCatalogCache.mu.Unlock()

	if e, ok := azureSKUCatalogCache.entries[key]; ok {
		if e.fetchedAt.IsZero() || time.Since(e.fetchedAt) <= ttl {
			return e
		}
		delete(azureSKUCatalogCache.entries, key)
	}

	if len(azureSKUCatalogCache.entries) >= maxAzureSKUCatalogCacheEntries {
		evictOldestAzureSKUEntryLocked()
	}

	e := &azureSKUCatalogEntry{}
	azureSKUCatalogCache.entries[key] = e
	return e
}

// evictOldestAzureSKUEntryLocked removes the completed entry with the oldest
// fetchedAt. Mirrors aws.evictOldestLocked. Callers must hold
// azureSKUCatalogCache.mu.
func evictOldestAzureSKUEntryLocked() {
	var oldestKey string
	var oldestAt time.Time
	for k, e := range azureSKUCatalogCache.entries {
		if e.fetchedAt.IsZero() {
			continue
		}
		if oldestKey == "" || e.fetchedAt.Before(oldestAt) {
			oldestKey, oldestAt = k, e.fetchedAt
		}
	}
	if oldestKey != "" {
		delete(azureSKUCatalogCache.entries, oldestKey)
	}
}

// azureSKUCatalogCacheTTL returns p's configured cache TTL, falling back to
// defaultAzureSKUCatalogEntryTTL when p is nil or its cacheTTL is unset.
// Mirrors aws.skuCatalogCacheTTL.
func azureSKUCatalogCacheTTL(p *Provider) time.Duration {
	if p != nil && p.cacheTTL > 0 {
		return p.cacheTTL
	}
	return defaultAzureSKUCatalogEntryTTL
}

// getOrFetchAzureSKUCatalog returns the memoized region->rows index for
// meterID, fetching it via fetchAzureSKUCatalog on first use or once the
// previous fetch has aged past its TTL. A failed fetch is intentionally NOT
// memoized permanently — mirrors aws.getOrFetchSKUCatalog's eviction-on-error
// behavior, letting the next caller retry from scratch instead of being
// stuck with a transient network failure for the rest of the entry's TTL.
func (p *Provider) getOrFetchAzureSKUCatalog(ctx context.Context, meterID string) (map[string][]azureRetailItem, error) {
	key := cacheKey("sku_lookup", "", map[string]string{"sku": meterID})
	entry := getAzureSKUCatalogEntry(key, azureSKUCatalogCacheTTL(p))
	entry.once.Do(func() {
		entry.byRegion, entry.err = fetchAzureSKUCatalog(ctx, p, meterID)
		azureSKUCatalogCache.mu.Lock()
		entry.fetchedAt = time.Now()
		azureSKUCatalogCache.mu.Unlock()
	})
	if entry.err != nil {
		azureSKUCatalogCache.mu.Lock()
		if azureSKUCatalogCache.entries[key] == entry {
			delete(azureSKUCatalogCache.entries, key)
		}
		azureSKUCatalogCache.mu.Unlock()
	}
	return entry.byRegion, entry.err
}

// fetchAzureSKUCatalog fetches every row for meterID (one server-side
// $filter=meterId eq '...' call, no armRegionName filter — this covers
// every region in a single shot) and buckets the raw rows by ArmRegionName.
func fetchAzureSKUCatalog(ctx context.Context, p *Provider, meterID string) (map[string][]azureRetailItem, error) {
	items, err := p.fetchPrices(ctx, map[string]string{"meterId": meterID}, azureSKULookupMaxResults)
	if err != nil {
		return nil, fmt.Errorf("azure sku lookup: fetch meterId %q: %w", meterID, err)
	}
	if len(items) == azureSKULookupMaxResults {
		// This fetch has exactly one chance to see every row for meterID
		// (see azureSKULookupMaxResults's doc comment) — hitting the cap
		// exactly is indistinguishable, from here, between "the catalog
		// happened to have exactly this many matching rows" and "there were
		// more rows this lookup never saw," which would silently under-report
		// candidates/regions for meterID. Warn so it's at least visible.
		slog.Warn("azure sku lookup: fetched row count exactly equals max_results; result may be silently truncated",
			"meter_id", meterID, "max_results", azureSKULookupMaxResults)
	}
	byRegion := make(map[string][]azureRetailItem, len(items))
	for _, item := range items {
		byRegion[item.ArmRegionName] = append(byRegion[item.ArmRegionName], item)
	}
	return byRegion, nil
}

// --------------------------------------------------------------------------
// Per-region disambiguation
// --------------------------------------------------------------------------

// filterAzurePrimaryMeterRegion implements algorithm step 2: when bucket
// mixes IsPrimaryMeterRegion true/false, keep only the true row(s); when it
// does not mix (all true, or all false — no established "primary" among
// them), leave bucket unchanged. This axis is resolved independently of, and
// strictly before, the type-based narrowing in step 5.
func filterAzurePrimaryMeterRegion(bucket []azureRetailItem) []azureRetailItem {
	var primaryTrue, primaryFalse []azureRetailItem
	for _, r := range bucket {
		if r.IsPrimaryMeterRegion {
			primaryTrue = append(primaryTrue, r)
		} else {
			primaryFalse = append(primaryFalse, r)
		}
	}
	if len(primaryTrue) > 0 && len(primaryFalse) > 0 {
		return primaryTrue
	}
	return bucket
}

// applyAzureSKUHint implements algorithm step 5's three branches (spot
// substring / explicit type equality / default Consumption). explicitHint
// reports whether a non-empty hint was actually supplied (as opposed to the
// default no-hint path), which the caller needs to pick the right
// HintStatus and to decide whether step 7's tier-collision-aware resolution
// is even eligible to run (default path only — see this file's top-of-file
// doc comment for why the explicit-hint path is inherently safe without it).
func applyAzureSKUHint(rows []azureRetailItem, productFamilyHint string) (filtered []azureRetailItem, explicitHint bool) {
	hint := strings.TrimSpace(productFamilyHint)
	if hint == "" {
		return filterAzureRowsByType(rows, "Consumption"), false
	}
	if strings.EqualFold(hint, "spot") {
		return filterAzureRowsByMeterNameSubstring(rows, "Spot"), true
	}
	return filterAzureRowsByType(rows, hint), true
}

func filterAzureRowsByType(rows []azureRetailItem, wantType string) []azureRetailItem {
	var out []azureRetailItem
	for _, r := range rows {
		if strings.EqualFold(r.Type, wantType) {
			out = append(out, r)
		}
	}
	return out
}

func filterAzureRowsByMeterNameSubstring(rows []azureRetailItem, substr string) []azureRetailItem {
	var out []azureRetailItem
	lowerSubstr := strings.ToLower(substr)
	for _, r := range rows {
		if strings.Contains(strings.ToLower(r.MeterName), lowerSubstr) {
			out = append(out, r)
		}
	}
	return out
}

// azureSKUProductGroupKey identifies the (SkuName, ProductName) pair that
// must be identical across a row set for it to be genuine usage-volume tiers
// of ONE billable product line — see resolveAzureTierGroup.
type azureSKUProductGroupKey struct{ skuName, productName string }

// resolveAzureTierGroup implements algorithm step 7: attempts to resolve
// rows (already narrowed to the default Type=="Consumption" path with more
// than one row remaining) to a confirmed tier ladder. Returns ok=false
// whenever any part of that confirmation fails — callers MUST report
// Ambiguous rather than guess, per this file's doc comment's account of the
// live Reservation-tier-collision incident this guards against.
//
// Resolution requires ALL of:
//  1. Exactly one (SkuName, ProductName) group among rows — more than one
//     means rows are not the same billable product line at all (this alone
//     is what separates the live collision incident's rows, which had
//     different SkuName/ProductName).
//  2. Every row in that group has a distinct TierMinimumUnits — a duplicate
//     threshold is not a real tier ladder (an independent second guard,
//     since the collision incident's rows also happened to share an
//     identical tierMinimumUnits==0.0).
//  3. RetailPrice is monotonic (either consistently non-increasing —
//     the common volume-discount shape — or consistently non-decreasing)
//     against ascending TierMinimumUnits order. A non-monotonic sequence is
//     not a clean graduated-tier step function and must not be silently
//     resolved.
//
// On success, tiers is every row in the confirmed group, sorted ascending
// by TierMinimumUnits — tiers[0] is the base-tier canonical row.
func resolveAzureTierGroup(rows []azureRetailItem) (tiers []azureRetailItem, ok bool) {
	groups := make(map[azureSKUProductGroupKey][]azureRetailItem, len(rows))
	for _, r := range rows {
		key := azureSKUProductGroupKey{skuName: r.SkuName, productName: r.ProductName}
		groups[key] = append(groups[key], r)
	}
	if len(groups) != 1 {
		return nil, false
	}
	var group []azureRetailItem
	for _, g := range groups {
		group = g
	}

	seenTiers := make(map[float64]bool, len(group))
	for _, r := range group {
		if seenTiers[r.TierMinimumUnits] {
			return nil, false
		}
		seenTiers[r.TierMinimumUnits] = true
	}

	sorted := append([]azureRetailItem(nil), group...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].TierMinimumUnits < sorted[j].TierMinimumUnits })

	if !azureRetailPriceMonotonic(sorted) {
		return nil, false
	}
	return sorted, true
}

// azureRetailPriceMonotonic reports whether sorted's RetailPrice sequence is
// consistently non-increasing or consistently non-decreasing.
func azureRetailPriceMonotonic(sorted []azureRetailItem) bool {
	nonIncreasing, nonDecreasing := true, true
	for i := 1; i < len(sorted); i++ {
		switch {
		case sorted[i].RetailPrice > sorted[i-1].RetailPrice:
			nonIncreasing = false
		case sorted[i].RetailPrice < sorted[i-1].RetailPrice:
			nonDecreasing = false
		}
	}
	return nonIncreasing || nonDecreasing
}

// azureSKUItemTerm infers a models.PricingTerm for a matched raw
// azureRetailItem. This is necessarily best-effort: azureRetailItem does not
// carry the API's separate "reservationTerm" field (1 Year vs 3 Years) — no
// existing fetch path in this package reads it back from the response (see
// GetComputePrice, which only ever sends reservationTerm as an outbound
// filter) — so a matched Reservation row is reported as Reserved1Yr
// regardless of its actual commitment length. A caller needing the exact
// term should treat this as "some reservation", not literal, and inspect
// Attributes["type"]/Attributes["unitOfMeasure"] for more detail.
func azureSKUItemTerm(item azureRetailItem) models.PricingTerm {
	meter := strings.ToLower(item.MeterName)
	if strings.Contains(meter, "spot") || strings.Contains(meter, "low priority") {
		return models.PricingTermSpot
	}
	if strings.EqualFold(item.Type, "Reservation") {
		return models.PricingTermReserved1Yr
	}
	return models.PricingTermOnDemand
}

// azureSKUUnit derives the most appropriate models.PriceUnit for a raw
// Retail Prices API row from its UnitOfMeasure (and, for a couple of
// meterName-only cases, MeterName) fields. This is deliberately best-effort
// and generic: unlike every existing per-domain Azure handler in this
// package (each of which knows in advance exactly which unit its own
// service uses, e.g. azure_functions's hand-picked GB-second/per-request/
// per-hour switch), a domain-agnostic raw-SKU lookup has no a priori
// knowledge of which unit fits an arbitrary meterId — mirrors
// gcp_sku_lookup.go's equivalent gcpSKUUnit() heuristic.
//
// When nothing matches, this falls back to models.PriceUnitPerUnit rather
// than silently defaulting to per-hour: PriceUnitPerHour flows into
// NormalizedPrice.MonthlyCost()/HourlyCost() as an active *730/÷730
// multiplier, so a wrong per-hour label doesn't just mislabel the row, it
// actively corrupts any derived monthly/hourly figure. That previously
// happened for every non-VM meter (e.g. a Reservation row's UnitOfMeasure
// is a contract-length label like "1 Year", not an hourly rate — the old
// hardcoded per-hour default overstated its "monthly cost" by ~730x).
// PriceUnitPerUnit is a safe inert fallback: both MonthlyCost and
// HourlyCost return PricePerUnit unchanged for any unlisted unit.
//
// KNOWN LIMITATION: this does not attempt the "leading quantity" packaging
// normalization some Azure UnitOfMeasure strings encode (e.g. "10K",
// "1M", "100/Hour" priced per that many units, not per 1) — see
// azure.go's azure_functions handler for the one place in this package
// that does attempt a (partial, space-separated-only) version of that
// normalization for a known meter shape. Applying it generically here is
// unsafe: a Reservation row's UnitOfMeasure of "3 Years" would be
// misread as "priced per 3 units" and its PricePerUnit wrongly divided by
// 3. Left as a documented gap rather than risking that regression.
func azureSKUUnit(item azureRetailItem) models.PriceUnit {
	uom := strings.ToLower(item.UnitOfMeasure)
	meter := strings.ToLower(item.MeterName)

	switch {
	// GB-second before GB/Month and GB below: "1 GB Second" contains "gb"
	// and would otherwise match one of the coarser GB cases first.
	case strings.Contains(uom, "gb") && strings.Contains(uom, "second"):
		return models.PriceUnitPerGBSecond
	case strings.Contains(uom, "gb") && strings.Contains(uom, "month"):
		return models.PriceUnitPerGBMonth
	case strings.Contains(uom, "gb"):
		return models.PriceUnitPerGB
	case strings.Contains(uom, "hour"):
		return models.PriceUnitPerHour
	case strings.Contains(uom, "month"):
		return models.PriceUnitPerMonth
	case strings.Contains(meter, "operation") || strings.Contains(uom, "operation"):
		return models.PriceUnitPerOperation
	case strings.Contains(meter, "execution") || strings.Contains(meter, "invocation") ||
		strings.Contains(meter, "request") || strings.Contains(meter, "transaction"):
		return models.PriceUnitPerRequest
	case strings.Contains(meter, "query"):
		return models.PriceUnitPerQuery
	default:
		return models.PriceUnitPerUnit
	}
}

// azureSKUItemToPrice converts one winning/candidate raw azureRetailItem row
// to a models.NormalizedPrice, via the existing itemToPrice (azure.go),
// passing the row's own ArmRegionName. Enriches the result with the raw
// fields itemToPrice does not carry (type, tierMinimumUnits,
// isPrimaryMeterRegion) so a caller inspecting an Ambiguous or Tiered result
// still has access to the attributes the disambiguation algorithm itself
// used, even though NormalizedPrice has no first-class fields for them.
func azureSKUItemToPrice(item azureRetailItem) models.NormalizedPrice {
	term := azureSKUItemTerm(item)
	unit := azureSKUUnit(item)
	pp := itemToPrice(item, item.ArmRegionName, term, item.ServiceName)
	var np models.NormalizedPrice
	if pp != nil {
		np = *pp
		// itemToPrice hardcodes PriceUnitPerHour (correct for its own VM/
		// disk/etc. call sites, which only ever pass compute-shaped rows) —
		// override with the unit actually derived from this row's own
		// UnitOfMeasure, since a domain-agnostic raw-SKU lookup can match
		// any meter shape (storage, bandwidth, requests, ...), not just
		// hourly compute.
		np.Unit = unit
	} else {
		// itemToPrice returns nil for a zero RetailPrice. A zero-priced raw
		// meterId row is unusual but not impossible (e.g. some free-tier
		// meters) — synthesize a minimal NormalizedPrice rather than
		// dropping the row the disambiguation algorithm has already
		// selected as this region's match/candidate.
		np = models.NormalizedPrice{
			Provider:      models.CloudProviderAzure,
			Service:       item.ServiceName,
			SKUID:         item.MeterID,
			ProductFamily: item.ServiceFamily,
			Description:   item.SkuName,
			Region:        item.ArmRegionName,
			PricingTerm:   term,
			PricePerUnit:  0,
			Unit:          unit,
			Currency:      "USD",
		}
	}
	attrs := make(map[string]string, len(np.Attributes)+4)
	for k, v := range np.Attributes {
		attrs[k] = v
	}
	attrs["type"] = item.Type
	attrs["tierMinimumUnits"] = fmt.Sprintf("%g", item.TierMinimumUnits)
	attrs["isPrimaryMeterRegion"] = fmt.Sprintf("%t", item.IsPrimaryMeterRegion)
	// tier_start_usage is consumed generically (no provider gate — see
	// bom.go's resolveBOMSKUItem) by the same graduated-tiered-pricing path
	// GCP's raw-SKU lookup already populates it for (gcp_sku_lookup.go).
	// Set unconditionally (harmless on non-tiered rows, since bom.go only
	// reads it when SKULookupRegionResult.Tiered is true) so that when
	// resolveAzureTierGroup succeeds and rr.Tiered is set, every resulting
	// tier row already carries the attribute the cost calculation needs —
	// without this, a tiered Azure SKU silently prices at $0.00/mo (every
	// tier gets skipped by tierStartUsage's ok=false path).
	attrs["tier_start_usage"] = fmt.Sprintf("%g", item.TierMinimumUnits)
	np.Attributes = attrs
	return np
}

func azureSKUConvertAll(rows []azureRetailItem) []models.NormalizedPrice {
	out := make([]models.NormalizedPrice, 0, len(rows))
	for _, r := range rows {
		out = append(out, azureSKUItemToPrice(r))
	}
	return out
}

// resolveAzureSKURegion runs the full per-region disambiguation algorithm
// (steps 1-7, see this file's top-of-file doc comment) against bucket (every
// row already filtered to this region by the caller) and hint.
func resolveAzureSKURegion(bucket []azureRetailItem, region string, hint skulookup.SKUHint) skulookup.SKULookupRegionResult {
	rr := skulookup.SKULookupRegionResult{Region: region}

	// Step 3 (first occurrence): no rows at all for this region.
	if len(bucket) == 0 {
		rr.NoMapping = true
		return rr
	}

	// Step 2.
	rows := filterAzurePrimaryMeterRegion(bucket)

	// Step 3 (second occurrence): the primary/non-primary split removed
	// every row (should not happen in practice — a mix always keeps at
	// least the primary row(s) — but handled defensively).
	if len(rows) == 0 {
		rr.NoMapping = true
		return rr
	}

	// ServiceUsed is invariant across every row in this region's bucket (see
	// azureSKUServiceUsed's doc comment: a single meterId's rows share one
	// ServiceName in every real Azure catalog row observed), so it is safe
	// to compute once here from the full (post-step-2) rows set and reuse it
	// at every return point below, rather than recomputing it from whatever
	// narrower subset (filtered/tiers) happens to be in scope at each
	// return.
	rr.ServiceUsed = azureSKUServiceUsed(rows)

	// Step 4. Deliberately short-circuits BEFORE hint.ProductFamilyHint is
	// even consulted (step 5) — this is a direct reading of this file's own
	// top-of-file algorithm doc ("4. Exactly one row remaining => that's
	// the match" precedes "5. ... apply hint.ProductFamilyHint"), not an
	// oversight. It does diverge from AWS's resolveSKUCandidates, which
	// still validates a supplied hint even when only one candidate remains
	// (see aws_sku_lookup.go). Left as specified: with only one row in the
	// bucket there is no second candidate a hint could disambiguate away
	// from, so there is nothing for step 5 to narrow.
	if len(rows) == 1 {
		rr.Prices = []models.NormalizedPrice{azureSKUItemToPrice(rows[0])}
		rr.HintStatus = skulookup.HintStatusNoHint
		return rr
	}

	// Step 5.
	filtered, explicitHint := applyAzureSKUHint(rows, hint.ProductFamilyHint)

	switch {
	case len(filtered) == 0:
		// Fails closed: original (post-step-2) candidate set, still
		// ambiguous, never silently ignoring the hint. HintStatusNoMatch is
		// only accurate when a hint was actually SUPPLIED and matched
		// nothing (skulookup.HintStatusNoMatch's doc: "a hint was SUPPLIED
		// but matched zero rows") — the default no-hint path (which
		// defensively also runs through this branch, since
		// applyAzureSKUHint's Consumption default can itself filter every
		// row away) reports HintStatusNoHint instead, mirroring the
		// len(filtered) == 1 branch just below.
		rr.Ambiguous = true
		if explicitHint {
			rr.HintStatus = skulookup.HintStatusNoMatch
		} else {
			rr.HintStatus = skulookup.HintStatusNoHint
		}
		rr.Prices = azureSKUConvertAll(rows)
		return rr

	case len(filtered) == 1:
		rr.Prices = []models.NormalizedPrice{azureSKUItemToPrice(filtered[0])}
		if explicitHint {
			rr.HintStatus = skulookup.HintStatusResolved
		} else {
			rr.HintStatus = skulookup.HintStatusNoHint
		}
		return rr

	default:
		// len(filtered) > 1.
		if !explicitHint {
			// Step 7 — tier-collision-aware resolution, default path only.
			// See resolveAzureTierGroup's doc for why an explicit hint never
			// attempts this (step 6 sends any explicit-hint multi-row result
			// straight to Ambiguous instead).
			if tiers, ok := resolveAzureTierGroup(filtered); ok {
				rr.Prices = azureSKUConvertAll(tiers)
				rr.Tiered = len(tiers) > 1
				rr.HintStatus = skulookup.HintStatusNoHint
				return rr
			}
		}
		rr.Ambiguous = true
		rr.HintStatus = skulookup.HintStatusAmbiguous
		rr.Prices = azureSKUConvertAll(filtered)
		return rr
	}
}

// azureSKUServiceUsed returns a representative ServiceName (e.g. "Virtual
// Machines") for a non-empty row set, for SKULookupRegionResult.ServiceUsed.
// A single meterId's rows share one ServiceName in every real Azure catalog
// row observed, so rows[0] is a safe, non-arbitrary representative even for
// an Ambiguous multi-row result.
func azureSKUServiceUsed(rows []azureRetailItem) string {
	if len(rows) == 0 {
		return ""
	}
	return rows[0].ServiceName
}

// uniformAzureSKURegionResults builds one skulookup.SKULookupRegionResult per
// region, each a copy of tmpl with only Region varying. Mirrors
// gcp.uniformRegionResults.
func uniformAzureSKURegionResults(regions []string, tmpl skulookup.SKULookupRegionResult) []skulookup.SKULookupRegionResult {
	out := make([]skulookup.SKULookupRegionResult, len(regions))
	for i, region := range regions {
		rr := tmpl
		rr.Region = region
		out[i] = rr
	}
	return out
}

// --------------------------------------------------------------------------
// LookupSKUAcrossRegionsGeneric — skulookup.SKULookupProvider conformance
// --------------------------------------------------------------------------

// LookupSKUAcrossRegionsGeneric resolves the price of a raw Azure Retail
// Prices API meterId string in each of the given regions. serviceHint is
// accepted for skulookup.SKULookupProvider interface conformance but is NOT
// used to filter — see this file's top-of-file doc comment for why a
// meterId alone is already a sufficient server-side filter. hint.OperationHint
// is likewise accepted but unused: Azure has no concept analogous to AWS's
// "operation" attribute for this lookup. Only hint.ProductFamilyHint is used
// (Azure-specific meaning — see the algorithm doc above).
func (p *Provider) LookupSKUAcrossRegionsGeneric(
	ctx context.Context, sku string, regions []string, serviceHint string, hint skulookup.SKUHint,
) (*skulookup.SKULookupResult, error) {
	_ = hint.OperationHint

	if sku == "" {
		return nil, &skulookup.SKULookupError{Code: skulookup.SKUErrSKURequired, Message: "sku must not be empty"}
	}
	if len(sku) > azureSKUMaxLength {
		return nil, &skulookup.SKULookupError{
			Code: skulookup.SKUErrSKUTooLong,
			Message: fmt.Sprintf(
				"sku must be at most %d characters (got %d) — real Azure meterId values are GUID-shaped, far shorter",
				azureSKUMaxLength, len(sku)),
		}
	}
	if len(regions) == 0 {
		return nil, &skulookup.SKULookupError{
			Code:    skulookup.SKUErrRegionsRequired,
			Message: "regions must contain at least one Azure ARM region name",
		}
	}
	if len(regions) > azureSKUMaxLookupRegions {
		return nil, &skulookup.SKULookupError{
			Code: skulookup.SKUErrTooManyRegions,
			Message: fmt.Sprintf(
				"regions must contain at most %d entries (got %d)", azureSKUMaxLookupRegions, len(regions)),
		}
	}
	if len(hint.ProductFamilyHint) > azureSKUMaxHintLength {
		return nil, &skulookup.SKULookupError{
			Code: skulookup.SKUErrHintTooLong,
			Message: fmt.Sprintf(
				"product_family must be at most %d characters (got %d)",
				azureSKUMaxHintLength, len(hint.ProductFamilyHint)),
		}
	}

	result := &skulookup.SKULookupResult{
		SKU:         sku,
		ServiceHint: serviceHint,
	}
	if serviceHint != "" {
		// Azure has no candidate-service resolution step for this lookup
		// (see top-of-file doc) — "explicit" here only means "the caller
		// supplied one", not "it was used to pick anything".
		result.ServiceSource = "explicit"
		result.Warnings = append(result.Warnings,
			"service is not used to filter Azure raw-SKU lookup — meterId alone is a sufficient "+
				"server-side filter; the supplied service hint is echoed back but has no effect on the match")
	} else {
		result.ServiceSource = "not_applicable"
	}

	byRegion, err := p.getOrFetchAzureSKUCatalog(ctx, sku)
	if err != nil {
		msg := fmt.Sprintf("could not fetch Azure Retail Prices catalog for meterId %q: %v", sku, err)
		result.Regions = uniformAzureSKURegionResults(regions, skulookup.SKULookupRegionResult{Error: msg})
		return result, nil
	}

	regionResults := make([]skulookup.SKULookupRegionResult, len(regions))
	for i, region := range regions {
		regionResults[i] = resolveAzureSKURegion(byRegion[region], region, hint)
	}
	result.Regions = regionResults
	return result, nil
}
