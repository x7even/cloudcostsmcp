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

	result, err := awsP.LookupSKUAcrossRegions(ctx, providerName, in.SKU, in.Service, in.Regions)
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

	// matchedRegion pairs a region's chosen matching price with the
	// service-resolution provenance needed for the response entry. When the
	// AWS core logic could not disambiguate multiple product rows sharing
	// the same stripped usage-type suffix (rr.Ambiguous — see
	// resolveSKUCandidates in aws_sku_lookup.go), all candidate rows are
	// kept in alternates so the caller can see and disambiguate them itself
	// instead of silently trusting a single guessed price.
	type matchedRegion struct {
		region      string
		price       models.NormalizedPrice
		serviceUsed string
		mismatch    bool
		ambiguous   bool
		alternates  []models.NormalizedPrice
	}

	var matched []matchedRegion
	var noMapping []map[string]any
	var erroredRegions []map[string]any
	anyAmbiguous := false

	for _, rr := range result.Regions {
		switch {
		case len(rr.Prices) > 0:
			// rr.Prices is already resolved by resolveSKUCandidates: either a
			// single unambiguous row, or (rr.Ambiguous) every row that
			// genuinely shares the suffix with no principled way to pick one.
			// In the ambiguous case a representative (cheapest) is still
			// chosen for sorting/baseline-delta purposes, but every row is
			// surfaced via alternates and the entry is flagged ambiguous so
			// the caller does not mistake the chosen row for a confident
			// match.
			chosen := rr.Prices[0]
			for _, p := range rr.Prices[1:] {
				if p.PricePerUnit < chosen.PricePerUnit {
					chosen = p
				}
			}
			if rr.Ambiguous {
				anyAmbiguous = true
			}
			matched = append(matched, matchedRegion{
				region:      rr.Region,
				price:       chosen,
				serviceUsed: rr.ServiceUsed,
				mismatch:    rr.ServiceMismatch,
				ambiguous:   rr.Ambiguous,
				alternates:  rr.Prices,
			})
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
		// Description/attributes/sku_id disambiguate which specific product
		// row this price came from (e.g. operatingSystem/tenancy for EC2,
		// databaseEngine for RDS) — surfacing them lets a caller reconciling
		// a CUR line item confirm the match, rather than only ever seeing a
		// bare number.
		if m.price.Description != "" {
			e["description"] = m.price.Description
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
		if m.ambiguous {
			e["ambiguous"] = true
			e["alternate_match_count"] = len(m.alternates)
			alts := make([]map[string]any, 0, len(m.alternates))
			for _, alt := range m.alternates {
				alt := alt
				a := map[string]any{
					"price_per_unit": priceDict(alt.PricePerUnit, string(alt.Unit)),
				}
				if alt.Description != "" {
					a["description"] = alt.Description
				}
				if len(alt.Attributes) > 0 {
					a["attributes"] = alt.Attributes
				}
				if alt.SKUID != "" {
					a["sku_id"] = alt.SKUID
				}
				alts = append(alts, a)
			}
			e["alternate_matches"] = alts
		}
		entries = append(entries, e)
	}

	if in.BaselineRegion != "" {
		if err := applyBaselineDeltas(entries, in.BaselineRegion); err != nil {
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
		warnings = append(warnings, "one or more regions matched multiple product rows for this usage-type "+
			"suffix with no unambiguous canonical default (see ambiguous/alternate_matches on the affected "+
			"entries) — the price shown for those regions is only the cheapest of the alternatives, not a "+
			"confirmed match; inspect alternate_matches to pick the correct row")
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
		out["message"] = "No matching price found for this SKU in any of the specified regions. " +
			"Check no_mapping_in (checked, not modeled) and errors_in (fetch failed) for details."
	}

	return jsonText(out), nil, nil
}
