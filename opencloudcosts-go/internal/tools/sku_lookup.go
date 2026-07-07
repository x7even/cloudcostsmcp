// sku_lookup.go implements the get_price_by_sku tool: given a raw AWS
// usage-type/SKU string exactly as it appears in a Cost & Usage Report (CUR)
// export (e.g. "CAN1-BoxUsage:r5a.8xlarge"), resolve its price across a list
// of target regions.
//
// This file is deliberately kept separate from lookup.go: lookup.go only
// imports the provider-agnostic internal/providers package, while this file
// must import the concrete internal/providers/aws package to type-assert the
// AWS-specific core logic (LookupSKUAcrossRegions in aws_sku_lookup.go).
// Isolating that import here keeps lookup.go provider-agnostic.
package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	awsprovider "github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers/aws"
	gcpprovider "github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers/gcp"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/skulookup"
)

// --------------------------------------------------------------------------
// GetPriceBySKU — raw AWS usage-type/SKU lookup
// --------------------------------------------------------------------------

// GetPriceBySKUInput is the typed input for the get_price_by_sku tool.
type GetPriceBySKUInput struct {
	Provider       string   `json:"provider"`
	SKU            string   `json:"sku"`
	Service        string   `json:"service"`
	Regions        []string `json:"regions"`
	BaselineRegion string   `json:"baseline_region"`

	// Operation and ProductFamily are optional disambiguating hints for usage-
	// type suffixes that match more than one distinct billable product (e.g.
	// ELB's LCUUsage suffix spans Application/Network/Gateway load balancer
	// pricing; RDS's InstanceUsage:<type> suffix spans every database engine
	// on that instance type). They correspond to the AWS product "operation"
	// attribute (e.g. "CreateDBInstance:0021") and top-level productFamily
	// (e.g. "Load Balancer-Application") respectively — the same columns a
	// real CUR export carries alongside the usage-type/SKU column. See
	// aws.resolveSKUCandidates for exactly how they're applied.
	Operation     string `json:"operation"`
	ProductFamily string `json:"product_family"`
}

// buildSKUAlternateList shapes a set of candidate NormalizedPrice rows
// (sharing one stripped usage-type suffix, still ambiguous after hint/
// canonical-default narrowing) into the JSON shape surfaced under
// alternate_matches. Deliberately carries description/attributes/
// product_family/sku_id/price_per_unit per row and no "chosen" price — the
// caller must pick one, informed by an operation/product_family hint on a
// follow-up call.
func buildSKUAlternateList(prices []models.NormalizedPrice) []map[string]any {
	alts := make([]map[string]any, 0, len(prices))
	for _, alt := range prices {
		a := map[string]any{
			"price_per_unit": priceDict(alt.PricePerUnit, string(alt.Unit)),
		}
		if alt.Description != "" {
			a["description"] = alt.Description
		}
		if alt.ProductFamily != "" {
			a["product_family"] = alt.ProductFamily
		}
		if len(alt.Attributes) > 0 {
			a["attributes"] = alt.Attributes
		}
		if alt.SKUID != "" {
			a["sku_id"] = alt.SKUID
		}
		alts = append(alts, a)
	}
	return alts
}

// HandleGetPriceBySKU implements the get_price_by_sku tool. It resolves a raw
// AWS usage-type/SKU string (as it appears verbatim in a CUR export) against
// each requested region's pricing catalog, and shapes the response to mirror
// compare_prices: a cheapest-first sorted list of matched regions, an
// explicit list of regions where the SKU has no catalog mapping (checked, not
// found — distinct from a fetch failure), and an optional baseline-region
// delta.
func (h *Handler) HandleGetPriceBySKU(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in GetPriceBySKUInput,
) (*mcp.CallToolResult, any, error) {
	defer recoverToResult(ctx, "get_price_by_sku")

	// The schema declares provider's JSON-Schema default as "aws", but the
	// go-sdk does not apply JSON-Schema defaults to the bound Go struct — an
	// omitted "provider" arrives here as "". Coalesce empty to "aws" the same
	// way HandleGetDiscountSummary does for its own default:"aws" field, so a
	// caller who omits provider (as this tool's own docs/examples do) doesn't
	// spuriously get "unsupported_provider".
	providerName := in.Provider
	if providerName == "" {
		providerName = "aws"
	}

	// The provider map is keyed by the canonical lowercase provider name
	// (e.g. "aws"/"gcp", populated in cmd/opencloudcosts/main.go). Lowercase
	// the lookup key so a caller passing "AWS" still resolves the provider,
	// but pass providerName through to the core function's own validation so
	// an unsupported provider (e.g. "azure") produces the core function's
	// honest, structured "unsupported_provider" error rather than a generic
	// "not configured" message.
	lookupP, errOut := resolveSKULookupProviderFromMap(h.providers, providerName, "get_price_by_sku")
	if errOut != nil {
		return errResult(errOut), nil, nil
	}

	return jsonText(h.resolveSKUPriceEntry(ctx, lookupP, providerName, in)), nil, nil
}

// resolveSKULookupProviderFromMap is the provider-agnostic successor to the
// AWS-only resolver this file used before RC3-015 (removed — it had zero
// remaining callers once get_price_by_sku/get_prices_by_sku/resolveBOMSKUItem
// were all migrated to this function). It resolves providerName to any concrete
// provider that implements skulookup.SKULookupProvider (today,
// *awsprovider.Provider and *gcpprovider.Provider), rather than only ever
// accepting AWS. get_price_by_sku/get_prices_by_sku (this file) and
// resolveBOMSKUItem (bom.go) use this so raw-SKU lookups work uniformly for
// both providers instead of hardcoding *awsprovider.Provider.
func resolveSKULookupProviderFromMap(provs map[string]Provider, providerName, toolName string) (skulookup.SKULookupProvider, map[string]any) {
	pvdr := provs[strings.ToLower(providerName)]
	if pvdr == nil {
		return nil, map[string]any{
			"error":   "unsupported_provider",
			"message": fmt.Sprintf("%s does not support provider %q.", toolName, providerName),
		}
	}
	switch p := pvdr.(type) {
	case *awsprovider.Provider:
		return p, nil
	case *gcpprovider.Provider:
		return p, nil
	default:
		return nil, map[string]any{
			"error":   "unsupported_provider",
			"message": fmt.Sprintf("%s does not support provider %q.", toolName, providerName),
		}
	}
}

// skuRegionResultKind classifies a single skulookup.SKULookupRegionResult
// into exactly one of five buckets. resolveSKUPriceEntry (looping over every
// region) and resolveBOMSKUItem (bom.go, a single region) both need this
// same four-way discrimination over Prices/Ambiguous/NoMapping/Error — kept
// in one place here rather than reimplemented at each call site.
type skuRegionResultKind int

const (
	skuResultMatched skuRegionResultKind = iota
	skuResultAmbiguous
	skuResultNoMapping
	skuResultError
	skuResultUnresolved // none of Prices/NoMapping/Error set — should not occur in practice
)

func classifySKURegionResult(rr skulookup.SKULookupRegionResult) skuRegionResultKind {
	switch {
	case len(rr.Prices) > 0 && !rr.Ambiguous:
		return skuResultMatched
	case len(rr.Prices) > 0 && rr.Ambiguous:
		return skuResultAmbiguous
	case rr.NoMapping:
		return skuResultNoMapping
	case rr.Error != "":
		return skuResultError
	default:
		return skuResultUnresolved
	}
}

// resolveSKUPriceEntry resolves a single SKU against lookupP/regions and shapes
// the response. This is the shared core of get_price_by_sku (a single SKU)
// and get_prices_by_sku (a batch of SKUs) — the disambiguation/sorting/
// baseline-delta shaping logic below is intricate (see the ambiguous_in/
// no_mapping_in/errors_in bucketing) and deliberately lives in exactly one
// place rather than being duplicated per caller.
//
// The returned map is exactly what HandleGetPriceBySKU wraps in a
// CallToolResult, whether it represents a successful lookup (no "error" key
// — even the "no unambiguous price found in any region" case, which sets
// "result": "no_prices_found" instead) or a structured error (an "error" key
// is always present). Callers that need to distinguish the two — e.g. the
// batch handler, sorting failed SKUs into a separate errors map — check for
// that "error" key.
func (h *Handler) resolveSKUPriceEntry(
	ctx context.Context,
	lookupP skulookup.SKULookupProvider,
	providerName string,
	in GetPriceBySKUInput,
) map[string]any {
	result, err := lookupP.LookupSKUAcrossRegionsGeneric(ctx, in.SKU, in.Regions, in.Service, skulookup.SKUHint{
		OperationHint:     in.Operation,
		ProductFamilyHint: in.ProductFamily,
	})
	if err != nil {
		var skuErr *skulookup.SKULookupError
		if errors.As(err, &skuErr) {
			return map[string]any{
				"error":   skuErr.Code,
				"message": skuErr.Message,
			}
		}
		return map[string]any{
			"error":     "upstream_failure",
			"message":   "SKU lookup failed. Try again shortly.",
			"retryable": true,
			"regions":   in.Regions,
		}
	}

	// matchedRegion pairs a region's resolved price(s) with the
	// service-resolution provenance needed for the response entry. Only
	// regions resolveSKUCandidates (AWS) / LookupSKUAcrossRegionsGeneric
	// (GCP) could narrow to exactly one billable item — either because the
	// suffix/skuId was unique to begin with, an operation/product_family
	// hint uniquely resolved it (AWS), or the existing canonical-default
	// narrowing uniquely resolved it (AWS) — ever land here. A region whose
	// match is still ambiguous after all of that is NEVER represented as a
	// matchedRegion (see the ambiguousRegions bucket below): "cheapest of
	// several different products" is not a defensible default price, so it
	// must not leak into matched/sorted/cheapest-summary output.
	//
	// tiered/allTiers hold the GCP tiered-rate case: multiple genuine
	// usage-volume tiers of ONE matched item, not alternate candidates
	// requiring disambiguation (see skulookup.SKULookupRegionResult.Tiered).
	// price is always the primary (lowest-usage-tier, when tiered) price
	// used for sorting/cheapest_price/most_expensive_price.
	type matchedRegion struct {
		region      string
		price       models.NormalizedPrice
		serviceUsed string
		mismatch    bool
		hintStatus  string
		tiered      bool
		allTiers    []models.NormalizedPrice
	}

	var matched []matchedRegion
	var ambiguousRegions []map[string]any
	var noMapping []map[string]any
	var erroredRegions []map[string]any
	anyAmbiguous := false

	for _, rr := range result.Regions {
		switch classifySKURegionResult(rr) {
		case skuResultMatched:
			// resolveSKUCandidates (AWS) guarantees exactly one row whenever
			// it reports ambiguous=false; GCP's Tiered case also lands here
			// (Tiered never sets Ambiguous) with more than one row, ordered
			// ascending by usage threshold — Prices[0] is the lowest tier.
			m := matchedRegion{
				region:      rr.Region,
				price:       rr.Prices[0],
				serviceUsed: rr.ServiceUsed,
				mismatch:    rr.ServiceMismatch,
				hintStatus:  rr.HintStatus,
				tiered:      rr.Tiered,
			}
			if rr.Tiered {
				m.allTiers = rr.Prices
			}
			matched = append(matched, m)
		case skuResultAmbiguous:
			// Still ambiguous even after hint-based and canonical-default
			// narrowing: this region is deliberately excluded from matched
			// (and therefore from sorting, cheapest/most_expensive, and
			// baseline deltas) and surfaced only here, with every remaining
			// candidate row and no chosen price, forcing the caller to
			// disambiguate rather than trusting a silently-picked "cheapest"
			// answer.
			anyAmbiguous = true
			ar := map[string]any{
				"region":                rr.Region,
				"service_used":          rr.ServiceUsed,
				"hint_status":           rr.HintStatus,
				"alternate_match_count": len(rr.Prices),
				"alternate_matches":     buildSKUAlternateList(rr.Prices),
			}
			if rr.ServiceMismatch {
				ar["service_mismatch"] = true
			}
			ambiguousRegions = append(ambiguousRegions, ar)
		case skuResultNoMapping:
			noMapping = append(noMapping, map[string]any{
				"region":             rr.Region,
				"attempted_services": rr.AttemptedServices,
			})
		case skuResultError:
			erroredRegions = append(erroredRegions, map[string]any{
				"region": rr.Region,
				"error":  rr.Error,
			})
		}
	}

	sort.Slice(matched, func(i, j int) bool {
		return matched[i].price.PricePerUnit < matched[j].price.PricePerUnit
	})

	entries := make([]map[string]any, 0, len(matched))
	for _, m := range matched {
		e := map[string]any{
			"region":         m.region,
			"region_name":    regionDisplayNameFn(strings.ToLower(providerName), m.region),
			"price_per_unit": priceDict(m.price.PricePerUnit, string(m.price.Unit)),
			"service_used":   m.serviceUsed,
		}
		// hint_status surfaces *why* this region resolved to a single row —
		// "resolved_by_hint" when a supplied operation/product_family hint is
		// what narrowed a multi-product suffix down to this one match (letting
		// a caller confirm the hint actually did the disambiguating work, not
		// just canonical-default narrowing). Omitted (like the other optional
		// fields below) when it's just the uninformative "no_hint_supplied"
		// default. See aws.resolveSKUCandidates.
		if m.hintStatus != "" && m.hintStatus != skulookup.HintStatusNoHint {
			e["hint_status"] = m.hintStatus
		}
		// Description/attributes/product_family/sku_id disambiguate which
		// specific product row this price came from (e.g. operatingSystem/
		// tenancy for EC2, databaseEngine for RDS) — surfacing them lets a
		// caller reconciling a CUR line item confirm the match, rather than
		// only ever seeing a bare number.
		if m.price.Description != "" {
			e["description"] = m.price.Description
		}
		if m.price.ProductFamily != "" {
			e["product_family"] = m.price.ProductFamily
		}
		if len(m.price.Attributes) > 0 {
			e["attributes"] = m.price.Attributes
		}
		if m.price.SKUID != "" {
			e["sku_id"] = m.price.SKUID
		}
		// Mirrors compare_prices/get_prices_batch: only per-hour and
		// per-month priced SKUs get a monthly estimate. Many SKUs reachable
		// through this tool (DynamoDB request units, data-transfer bytes,
		// LCU-hours) are not — inventing a monthly figure for those would be
		// misleading.
		if m.price.Unit == models.PriceUnitPerHour || m.price.Unit == models.PriceUnitPerMonth {
			e["monthly_estimate"] = moneyDict(m.price.MonthlyCost(), "/mo")
		}
		if m.mismatch {
			e["service_mismatch"] = true
		}
		// Tiered (GCP only, see SKULookupRegionResult.Tiered's doc comment):
		// surface every usage-volume tier's rate so a caller can see the full
		// rate schedule, in addition to the primary (lowest-tier) price above
		// used for sorting/cheapest_price/most_expensive_price. This is
		// deliberately its own case rather than folded into ambiguous_in —
		// these are tiers of one matched item, not alternate candidates
		// requiring disambiguation.
		if m.tiered {
			e["tiered"] = true
			allTierRates := make([]map[string]any, 0, len(m.allTiers))
			for _, t := range m.allTiers {
				tier := map[string]any{
					"price_per_unit": priceDict(t.PricePerUnit, string(t.Unit)),
				}
				if startStr, ok := t.Attributes["tier_start_usage"]; ok {
					if start, perr := strconv.ParseFloat(startStr, 64); perr == nil {
						tier["tier_start_usage"] = start
					} else {
						tier["tier_start_usage"] = startStr
					}
				}
				allTierRates = append(allTierRates, tier)
			}
			e["all_tier_rates"] = allTierRates
		}
		entries = append(entries, e)
	}

	if in.BaselineRegion != "" {
		if err := applyBaselineDeltas(entries, in.BaselineRegion, ""); err != nil {
			// entries is built from matched only, so a baseline_region that
			// exists but is stuck in the ambiguous-unresolved bucket (rather
			// than genuinely absent from every requested region) would
			// otherwise surface as a generic "not found in results" error
			// that gives no hint why. Check for that case explicitly and
			// surface it as its own actionable error rather than a silent
			// omission of the delta fields.
			for _, ar := range ambiguousRegions {
				if ar["region"] == in.BaselineRegion {
					return map[string]any{
						"error": "baseline_region_ambiguous",
						"message": fmt.Sprintf(
							"baseline_region %q is ambiguous and unresolved for this SKU (see ambiguous_in) "+
								"— cannot compute baseline deltas. Supply operation/product_family hints to "+
								"resolve it, or choose a different baseline_region.",
							in.BaselineRegion,
						),
					}
				}
			}
			return map[string]any{"error": err.Error()}
		}
	}

	out := map[string]any{
		"sku":                result.SKU,
		"service_source":     result.ServiceSource,
		"all_regions_sorted": entries, // mirrors compare_prices' "all_regions_sorted" field name
	}
	// usage_type_prefix/usage_type_suffix are AWS-only concepts (see
	// SKULookupResult's doc comment): AWS always sets (and callers/tests rely
	// on receiving) both keys, even when the parsed usage-type string happens
	// to carry no prefix ("") — so gate on provider, not on string-emptiness,
	// which would otherwise also suppress a legitimately-empty AWS prefix.
	// GCP never populates these fields at all, so they're omitted for GCP
	// rather than emitted as misleading empty strings.
	if strings.EqualFold(providerName, "aws") {
		out["usage_type_prefix"] = result.UsageTypePrefix
		out["usage_type_suffix"] = result.UsageTypeSuffix
	}
	if result.ServiceHint != "" {
		out["service_hint"] = result.ServiceHint
	}
	if result.InferredService != "" {
		out["inferred_service"] = result.InferredService
	}
	warnings := append([]string{}, result.Warnings...)
	if anyAmbiguous {
		warnings = append(warnings, "one or more regions matched multiple distinct product rows for this "+
			"usage-type suffix that could not be narrowed to a single unambiguous match (see ambiguous_in) — "+
			"those regions are excluded from all_regions_sorted/cheapest_price/most_expensive_price rather "+
			"than defaulting to a guessed price; supply operation and/or product_family hints (see each "+
			"ambiguous_in entry's alternate_matches for the candidate values) to resolve them")
	}
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	if len(matched) > 0 {
		cheapest := matched[0]
		mostExp := matched[len(matched)-1]
		out["cheapest_region"] = cheapest.region
		out["cheapest_price"] = priceDict(cheapest.price.PricePerUnit, string(cheapest.price.Unit))
		out["most_expensive_region"] = mostExp.region
		out["most_expensive_price"] = priceDict(mostExp.price.PricePerUnit, string(mostExp.price.Unit))
		// Mirrors compare_prices' price_delta_pct: % spread between cheapest
		// and most expensive matched region, rounded to 1 decimal. Only
		// computed when there's more than one matched region and the
		// cheapest price is non-zero (avoids a div-by-zero for free tiers).
		if len(matched) > 1 && cheapest.price.PricePerUnit > 0 {
			raw := (mostExp.price.PricePerUnit - cheapest.price.PricePerUnit) / cheapest.price.PricePerUnit * 100
			out["price_delta_pct"] = math.Round(raw*10) / 10
		}
	}
	if len(ambiguousRegions) > 0 {
		out["ambiguous_in"] = ambiguousRegions
	}
	if len(noMapping) > 0 {
		out["no_mapping_in"] = noMapping
	}
	if len(erroredRegions) > 0 {
		out["errors_in"] = erroredRegions
	}
	if in.BaselineRegion != "" {
		out["baseline_region"] = in.BaselineRegion
	}
	if len(matched) == 0 {
		out["result"] = "no_prices_found"
		out["message"] = "No unambiguous matching price found for this SKU in any of the specified regions. " +
			"Check ambiguous_in (matched but needs operation/product_family hints to resolve), no_mapping_in " +
			"(checked, not modeled), and errors_in (fetch failed) for details."
	}

	return out
}

// --------------------------------------------------------------------------
// GetPricesBySKU — batch form of get_price_by_sku
// --------------------------------------------------------------------------

// maxSKUsPerBatch caps the skus list. Each SKU triggers its own fan-out
// across regions (LookupSKUAcrossRegions), and while the process-lifetime
// skuCatalogCache (aws_sku_lookup.go) collapses duplicate (service, region)
// catalog fetches across SKUs that resolve to the same service, a batch of
// SKUs that all resolve to distinct services still pays that fetch cost
// once each — an unbounded skus list is a resource-exhaustion vector for the
// same reason aws.maxSKULookupRegions bounds the regions list, so it is
// rejected up front.
const maxSKUsPerBatch = 25

// skuBatchFanoutLimit bounds how many SKUs are resolved concurrently. Each
// SKU lookup already fans out across its own regions internally (semaphore
// of 10, see LookupSKUAcrossRegions), so an unbounded outer fan-out here
// could multiply that by len(skus) simultaneous multi-hundred-MB catalog
// fetches in the worst case (all SKUs resolving to distinct services). The
// shared skuCatalogCache still collapses duplicate fetches for SKUs that
// share a (service, region), so this only throttles that worst case.
const skuBatchFanoutLimit = 4

// GetPricesBySKUInput is the typed input for the get_prices_by_sku tool.
type GetPricesBySKUInput struct {
	Provider       string   `json:"provider"`
	SKUs           []string `json:"skus"`
	Regions        []string `json:"regions"`
	BaselineRegion string   `json:"baseline_region"`
}

// HandleGetPricesBySKU implements the get_prices_by_sku tool: a batch form
// of get_price_by_sku for resolving many raw AWS usage-type/SKU strings
// (e.g. every distinct line item in a CUR export) against the same set of
// target regions in one call. It reuses resolveSKUPriceEntry — the exact
// same per-SKU resolution and response-shaping logic get_price_by_sku uses
// — for each sku, fanned out concurrently (bounded by skuBatchFanoutLimit)
// so repeated (service, region) catalog fetches across SKUs benefit from the
// process-lifetime skuCatalogCache memoization.
//
// Each entry in "results" has exactly the shape a standalone get_price_by_sku
// call for that sku would return (including its own ambiguous_in/
// no_mapping_in/errors_in/baseline delta fields) — unlike get_prices_batch,
// results are NOT re-sorted by price, since distinct SKUs commonly price in
// different units (per-hour, per-GB, per-request, ...) that are not
// meaningfully comparable; they are returned in the same order as the input
// skus list. A sku whose lookup fails outright (e.g. empty string, or a
// usage-type pattern get_price_by_sku cannot infer a service for) is instead
// reported in the top-level "errors" map, keyed by that sku string, mirroring
// get_prices_batch's per-item error shape.
func (h *Handler) HandleGetPricesBySKU(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in GetPricesBySKUInput,
) (*mcp.CallToolResult, any, error) {
	defer recoverToResult(ctx, "get_prices_by_sku")

	// See HandleGetPriceBySKU's identical coalesce for why: the go-sdk does
	// not apply JSON-Schema defaults to the bound Go struct.
	providerName := in.Provider
	if providerName == "" {
		providerName = "aws"
	}

	lookupP, errOut := resolveSKULookupProviderFromMap(h.providers, providerName, "get_prices_by_sku")
	if errOut != nil {
		return errResult(errOut), nil, nil
	}

	if len(in.SKUs) == 0 {
		return errResult(map[string]any{
			"error":   "skus_required",
			"message": "skus must contain at least one raw AWS usage-type/SKU string",
		}), nil, nil
	}
	if len(in.SKUs) > maxSKUsPerBatch {
		return errResult(map[string]any{
			"error": "too_many_skus",
			"message": fmt.Sprintf(
				"skus must contain at most %d entries (got %d) — call get_prices_by_sku in smaller "+
					"batches, or use get_price_by_sku for one-off lookups.",
				maxSKUsPerBatch, len(in.SKUs),
			),
		}), nil, nil
	}
	if len(in.Regions) == 0 {
		return errResult(map[string]any{
			"error":   "regions_required",
			"message": "regions must contain at least one AWS region code",
		}), nil, nil
	}

	// Fan out across SKUs, each resolved via the exact same per-SKU logic
	// get_price_by_sku uses (resolveSKUPriceEntry), bounded by
	// skuBatchFanoutLimit.
	outs := make([]map[string]any, len(in.SKUs))
	sem := make(chan struct{}, skuBatchFanoutLimit)
	var wg sync.WaitGroup
	for i, sku := range in.SKUs {
		wg.Add(1)
		go func(idx int, s string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			outs[idx] = h.resolveSKUPriceEntry(ctx, lookupP, providerName, GetPriceBySKUInput{
				SKU:            s,
				Regions:        in.Regions,
				BaselineRegion: in.BaselineRegion,
			})
		}(i, sku)
	}
	wg.Wait()

	results := make([]map[string]any, 0, len(outs))
	errMap := make(map[string]any, len(outs))
	for i, o := range outs {
		sku := in.SKUs[i]
		if errVal, isErr := o["error"]; isErr {
			msg, _ := o["message"].(string)
			if msg == "" {
				msg = fmt.Sprintf("%v", errVal)
			}
			retryable, _ := o["retryable"].(bool)
			status := regionStatusNoData
			if retryable {
				status = regionStatusTransient
			}
			errMap[sku] = map[string]any{
				"message":   msg,
				"status":    status,
				"retryable": retryable,
			}
			continue
		}
		results = append(results, o)
	}

	out := map[string]any{
		"provider": providerName,
		"skus":     in.SKUs,
		"regions":  in.Regions,
		"count":    len(results),
		"results":  results,
	}
	if in.BaselineRegion != "" {
		out["baseline_region"] = in.BaselineRegion
	}
	if len(errMap) > 0 {
		out["errors"] = errMap
	}
	return jsonText(out), nil, nil
}
