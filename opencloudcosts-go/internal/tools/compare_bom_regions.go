// compare_bom_regions.go implements the compare_bom_regions MCP tool.
//
// v1 scope (issue #31, RC3-004): AWS-only, synchronous per-line region
// fan-out over PricingSpec-dict and raw-SKU items — no weighting or a
// providers filter yet. It is composed entirely from existing cross-provider
// machinery —
// estimate_bom's processBOMItems (bom.go) for per-item price resolution, and
// compare_prices' region-fan-out + baseline-delta pattern (this file) for the
// region loop — rather than new AWS-specific plumbing, so the input/output
// contract does not lock in an AWS-specific shape. Non-AWS items are reported
// once at the top level, tagged "not_supported", rather than guessed or
// dropped silently.
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

	// Partition items up front: v1 only resolves AWS items. Non-AWS items are
	// reported once (provider does not vary per region), not re-derived on
	// every region iteration.
	var resolvable []map[string]any
	var notSupported []map[string]any
	for idx, item := range in.Items {
		label := fmt.Sprintf("Item %d", idx+1)

		// Raw-SKU items are implicitly AWS (same default get_price_by_sku
		// applies to a missing provider) — but an item that explicitly names
		// a non-AWS provider is routed to notSupported here, exactly like any
		// other non-AWS item, rather than being rejected once per region
		// inside processBOMItems below.
		if _, ok := rawBOMSKU(item); ok {
			pvdrName, hasPvdr := item["provider"].(string)
			if !hasPvdr || pvdrName == "" || strings.EqualFold(pvdrName, compareBOMRegionsV1Provider) {
				resolvable = append(resolvable, item)
				continue
			}
			notSupported = append(notSupported, map[string]any{
				"item":     label,
				"provider": pvdrName,
				"source":   "not_supported",
				"reason":   "compare_bom_regions v1 is AWS-only (RC3-004) — this provider is not yet supported.",
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

	type regionResult struct {
		region       string
		totalMonthly float64
		lineItems    []map[string]any
		errs         []string
		status       string // ok | no_data
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
			status := regionStatusOK
			if len(lineItems) == 0 {
				status = regionStatusNoData
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
			"region_name":   regionDisplayNameFn(compareBOMRegionsV1Provider, r.region),
			"total_monthly": moneyDict(r.totalMonthly, "/mo"),
			"line_items":    r.lineItems,
		}
		if len(r.errs) > 0 {
			e["errors"] = r.errs
		}
		if r.status == regionStatusNoData {
			e["status"] = "no_data"
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
