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
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	awsprovider "github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers/aws"
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
	// (e.g. "aws", populated in cmd/opencloudcosts/main.go). Lowercase the
	// lookup key so a caller passing "AWS" still resolves the provider, but
	// pass providerName through to the core function's own validation so an
	// unsupported provider (e.g. "gcp") produces the core function's honest,
	// structured "unsupported_provider" error rather than a generic "not
	// configured" message.
	pvdr := h.provider(strings.ToLower(providerName))
	if pvdr == nil {
		return errResult(map[string]any{
			"error":   "unsupported_provider",
			"message": fmt.Sprintf("get_price_by_sku only supports provider=\"aws\" (got %q).", providerName),
		}), nil, nil
	}

	awsP, ok := pvdr.(*awsprovider.Provider)
	if !ok {
		// Should not be reachable in practice (only "aws" resolves to an AWS
		// provider instance), but guards against a future provider map key
		// aliasing collision.
		return errResult(map[string]any{
			"error":   "unsupported_provider",
			"message": fmt.Sprintf("get_price_by_sku only supports provider=\"aws\" (got %q).", providerName),
		}), nil, nil
	}

	result, err := awsP.LookupSKUAcrossRegions(ctx, providerName, in.SKU, in.Service, in.Regions, in.Operation, in.ProductFamily)
	if err != nil {
		var skuErr *awsprovider.SKULookupError
		if errors.As(err, &skuErr) {
			return errResult(map[string]any{
				"error":   skuErr.Code,
				"message": skuErr.Message,
			}), nil, nil
		}
		return errResult(map[string]any{
			"error":     "upstream_failure",
			"message":   "SKU lookup failed. Try again shortly.",
			"retryable": true,
		}), nil, nil
	}

	// matchedRegion pairs a region's single resolved price with the
	// service-resolution provenance needed for the response entry. Only
	// regions resolveSKUCandidates could narrow to exactly one row — either
	// because the suffix was unique to begin with, an operation/
	// product_family hint uniquely resolved it, or the existing canonical-
	// default narrowing uniquely resolved it — ever land here. A region
	// whose match is still ambiguous after all of that is NEVER represented
	// as a matchedRegion (see the ambiguousRegions bucket below): "cheapest
	// of several different products" is not a defensible default price, so
	// it must not leak into matched/sorted/cheapest-summary output.
	type matchedRegion struct {
		region      string
		price       models.NormalizedPrice
		serviceUsed string
		mismatch    bool
		hintStatus  string
	}

	var matched []matchedRegion
	var ambiguousRegions []map[string]any
	var noMapping []map[string]any
	var erroredRegions []map[string]any
	anyAmbiguous := false

	for _, rr := range result.Regions {
		switch {
		case len(rr.Prices) > 0 && !rr.Ambiguous:
			// resolveSKUCandidates guarantees exactly one row whenever it
			// reports ambiguous=false.
			matched = append(matched, matchedRegion{
				region:      rr.Region,
				price:       rr.Prices[0],
				serviceUsed: rr.ServiceUsed,
				mismatch:    rr.ServiceMismatch,
				hintStatus:  rr.HintStatus,
			})
		case len(rr.Prices) > 0 && rr.Ambiguous:
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
		case rr.NoMapping:
			noMapping = append(noMapping, map[string]any{
				"region":             rr.Region,
				"attempted_services": rr.AttemptedServices,
			})
		case rr.Error != "":
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
			"region_name":    regionDisplayNameFn("aws", m.region),
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
		if m.hintStatus != "" && m.hintStatus != awsprovider.HintStatusNoHint {
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
		entries = append(entries, e)
	}

	if in.BaselineRegion != "" {
		if err := applyBaselineDeltas(entries, in.BaselineRegion); err != nil {
			// entries is built from matched only, so a baseline_region that
			// exists but is stuck in the ambiguous-unresolved bucket (rather
			// than genuinely absent from every requested region) would
			// otherwise surface as a generic "not found in results" error
			// that gives no hint why. Check for that case explicitly and
			// surface it as its own actionable error rather than a silent
			// omission of the delta fields.
			for _, ar := range ambiguousRegions {
				if ar["region"] == in.BaselineRegion {
					return errResult(map[string]any{
						"error": "baseline_region_ambiguous",
						"message": fmt.Sprintf(
							"baseline_region %q is ambiguous and unresolved for this SKU (see ambiguous_in) "+
								"— cannot compute baseline deltas. Supply operation/product_family hints to "+
								"resolve it, or choose a different baseline_region.",
							in.BaselineRegion,
						),
					}), nil, nil
				}
			}
			return errResult(map[string]any{"error": err.Error()}), nil, nil
		}
	}

	out := map[string]any{
		"sku":                result.SKU,
		"usage_type_prefix":  result.UsageTypePrefix,
		"usage_type_suffix":  result.UsageTypeSuffix,
		"service_source":     result.ServiceSource,
		"all_regions_sorted": entries, // mirrors compare_prices' "all_regions_sorted" field name
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

	return jsonText(out), nil, nil
}
