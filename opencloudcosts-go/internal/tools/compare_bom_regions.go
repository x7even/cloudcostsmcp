// compare_bom_regions.go implements the compare_bom_regions MCP tool.
//
// v1 scope (issue #31, RC3-004): AWS-only for PricingSpec-dict items,
// synchronous per-line region fan-out — no weighting or a providers filter
// yet. It is composed entirely from existing cross-provider machinery —
// estimate_bom's processBOMItems (bom.go) for per-item price resolution, and
// compare_prices' region-fan-out + baseline-delta pattern (this file) for the
// region loop — rather than new AWS-specific plumbing, so the input/output
// contract does not lock in an AWS-specific shape. Non-AWS PricingSpec-dict
// items are reported once at the top level, tagged "not_supported", rather
// than guessed or dropped silently.
//
// Raw-SKU items (RC3-015, GCP parity; extended to Azure alongside the
// sku_lookup tool-layer wiring) additionally accept provider=="gcp" or
// provider=="azure" — resolveBOMSKUItem (bom.go) already resolves any of
// these providers generically via resolveSKULookupProviderFromMap, so no
// per-region plumbing here needs to change, only the partitioning check
// below. Because a single compare_bom_regions call's resolvable items can
// therefore now span more than one provider (e.g. an AWS EC2 SKU and a GCP
// Compute Engine SKU in the same BoM), and a region's regionResult
// aggregates cost across every resolvable item for that region, there is no
// longer one single "the" provider to pass to regionDisplayNameFn — see the
// resolvableProviders computation and its use below.
package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// compareBOMRegionsV1Provider is the only provider compare_bom_regions
// resolves to real prices in v1.
const compareBOMRegionsV1Provider = "aws"

// CompareBOMRegionsInput is the typed input for the compare_bom_regions tool.
type CompareBOMRegionsInput struct {
	Items          []map[string]any `json:"items"`
	Regions        []string         `json:"regions"`
	BaselineRegion string           `json:"baseline_region"`
}

// HandleCompareBOMRegions implements the compare_bom_regions tool.
func (h *Handler) HandleCompareBOMRegions(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in CompareBOMRegionsInput,
) (*mcp.CallToolResult, any, error) {
	defer recoverToResult(ctx, "compare_bom_regions")

	if len(in.Regions) == 0 {
		return errResult(map[string]any{
			"error": "regions is required and must be a non-empty list of region codes.",
		}), nil, nil
	}
	if len(in.Items) == 0 {
		return errResult(map[string]any{
			"error": "items is required and must be a non-empty list of BoM line items.",
		}), nil, nil
	}

	// Partition items up front: v1 resolves AWS PricingSpec-dict items and
	// AWS/GCP/Azure raw-SKU items. Unsupported items are reported once
	// (provider does not vary per region), not re-derived on every region
	// iteration.
	var resolvable []map[string]any
	var notSupported []map[string]any
	for idx, item := range in.Items {
		label := fmt.Sprintf("Item %d", idx+1)

		// Raw-SKU items are implicitly AWS (same default get_price_by_sku
		// applies to a missing provider) and also accept an explicit
		// provider=="gcp" (RC3-015) or provider=="azure" — resolveBOMSKUItem
		// resolves any of these providers generically. An item naming any
		// other provider is routed to notSupported here, exactly like any
		// other unsupported item, rather than being rejected once per region
		// inside processBOMItems below.
		if _, ok := rawBOMSKU(item); ok {
			pvdrName, hasPvdr := item["provider"].(string)
			if !hasPvdr || pvdrName == "" || strings.EqualFold(pvdrName, compareBOMRegionsV1Provider) ||
				strings.EqualFold(pvdrName, "gcp") || strings.EqualFold(pvdrName, "azure") {
				resolvable = append(resolvable, item)
				continue
			}
			notSupported = append(notSupported, map[string]any{
				"item":     label,
				"provider": pvdrName,
				"source":   "not_supported",
				"reason":   "compare_bom_regions raw-SKU items support aws, gcp, and azure providers only — this provider is not yet supported.",
			})
			continue
		}

		pvdrName, _ := item["provider"].(string)
		if strings.ToLower(pvdrName) != compareBOMRegionsV1Provider {
			notSupported = append(notSupported, map[string]any{
				"item":     label,
				"provider": pvdrName,
				"source":   "not_supported",
				"reason":   "compare_bom_regions v1 is AWS-only (RC3-004) — this provider is not yet supported.",
			})
			continue
		}
		resolvable = append(resolvable, item)
	}

	// regionNameProvider decides what provider to pass to regionDisplayNameFn
	// for the per-region "region_name" field below. A region's regionResult
	// aggregates cost across every resolvable item for that region, so if
	// resolvable items span more than one provider (an AWS item and a GCP
	// item both requesting, say, region "us-central1"/"us-east-1"), there is
	// no single correct provider whose display-name map applies — falling
	// back to the bare region code (regionDisplayNameFn's own behavior for an
	// unrecognized provider, see internal/utils/regions.go) is the smallest
	// correct fix, rather than guessing one provider or resolving a display
	// name per line item (region_name is a per-region, not per-line-item,
	// field in this tool's response shape).
	resolvableProviders := map[string]struct{}{}
	for _, item := range resolvable {
		pvdrName, _ := item["provider"].(string)
		pvdrName = strings.ToLower(pvdrName)
		if pvdrName == "" {
			pvdrName = compareBOMRegionsV1Provider // raw-SKU/PricingSpec-dict default
		}
		resolvableProviders[pvdrName] = struct{}{}
	}
	regionNameProvider := compareBOMRegionsV1Provider
	if len(resolvableProviders) == 1 {
		for p := range resolvableProviders {
			regionNameProvider = p
		}
	} else if len(resolvableProviders) > 1 {
		regionNameProvider = "" // mixed providers: force the bare-region-code fallback
	}

	type regionResult struct {
		region       string
		totalMonthly float64
		lineItems    []map[string]any
		errs         []string
		status       string // ok | no_data | partial
	}

	sem := make(chan struct{}, 10)
	results := make([]regionResult, len(in.Regions))
	var wg sync.WaitGroup

	for i, region := range in.Regions {
		wg.Add(1)
		go func(idx int, rgn string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Override region on every resolvable item for this region —
			// mirrors compare_prices' per-region spec override (lookup.go).
			regionItems := make([]map[string]any, len(resolvable))
			for j, item := range resolvable {
				clone := make(map[string]any, len(item)+1)
				for k, v := range item {
					clone[k] = v
				}
				clone["region"] = rgn
				regionItems[j] = clone
			}

			lineItems, errs := processBOMItems(ctx, h.providers, regionItems)
			liMaps := make([]map[string]any, 0, len(lineItems))
			var total float64
			for _, li := range lineItems {
				total += li.monthlyCost
				liMaps = append(liMaps, li.toMap())
			}
			// A region with zero resolved line items is no_data. A region
			// with some resolved and some errored is "partial" — its total
			// is real but understated (some resolvable items failed), so it
			// must not be reported as unqualified "ok" alongside regions
			// where every item resolved cleanly.
			status := regionStatusOK
			switch {
			case len(lineItems) == 0:
				status = regionStatusNoData
			case len(errs) > 0:
				status = regionStatusPartial
			}
			results[idx] = regionResult{
				region:       rgn,
				totalMonthly: total,
				lineItems:    liMaps,
				errs:         errs,
				status:       status,
			}
		}(i, region)
	}
	wg.Wait()

	// Sort cheapest-first; regions with no resolvable data sort last.
	sort.SliceStable(results, func(a, b int) bool {
		ra, rb := results[a], results[b]
		if ra.status == regionStatusNoData && rb.status != regionStatusNoData {
			return false
		}
		if rb.status == regionStatusNoData && ra.status != regionStatusNoData {
			return true
		}
		return ra.totalMonthly < rb.totalMonthly
	})

	entries := make([]map[string]any, 0, len(results))
	var baselineTotal float64
	var baselineFound bool
	for _, r := range results {
		if r.region == in.BaselineRegion && r.status == regionStatusOK {
			baselineTotal = r.totalMonthly
			baselineFound = true
		}
	}

	for _, r := range results {
		e := map[string]any{
			"region":        r.region,
			"region_name":   regionDisplayNameFn(regionNameProvider, r.region),
			"total_monthly": moneyDict(r.totalMonthly, "/mo"),
			"line_items":    r.lineItems,
		}
		if len(r.errs) > 0 {
			e["errors"] = r.errs
		}
		switch r.status {
		case regionStatusNoData:
			e["status"] = "no_data"
		case regionStatusPartial:
			e["status"] = "partial"
		}
		if in.BaselineRegion != "" {
			// Degrade gracefully (RC3-002): a missing baseline region nulls
			// delta fields rather than failing the whole call.
			if baselineFound && r.status == regionStatusOK {
				dm := r.totalMonthly - baselineTotal
				var pct float64
				if baselineTotal > 0 {
					pct = (dm / baselineTotal) * 100
				}
				e["delta_monthly"] = fmt.Sprintf("$%+.2f/mo", dm)
				e["delta_pct"] = fmt.Sprintf("%+.1f%%", pct)
			} else {
				e["delta_monthly"] = nil
				e["delta_pct"] = nil
			}
		}
		entries = append(entries, e)
	}

	out := map[string]any{
		"regions": entries,
	}
	if in.BaselineRegion != "" {
		out["baseline_region"] = in.BaselineRegion
		if !baselineFound {
			out["baseline_region_error"] = fmt.Sprintf(
				"baseline region %q not found among resolved regions.", in.BaselineRegion)
		}
	}
	if len(notSupported) > 0 {
		out["not_supported"] = notSupported
	}
	return jsonText(out), nil, nil
}
