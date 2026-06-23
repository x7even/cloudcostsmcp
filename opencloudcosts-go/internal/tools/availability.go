// availability.go implements the 4 discovery/availability MCP tools:
//   - list_regions
//   - list_instance_types
//   - find_cheapest_region
//   - find_available_regions
//
// Fan-out tools use golang.org/x/sync/errgroup with a semaphore of 32
// concurrent goroutines. Context cancellation propagates through errgroup so
// an expired parent deadline aborts all in-flight fetches.
package tools

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// maxConcurrentRegions is the semaphore size for fan-out region fetches.
const maxConcurrentRegions = 32

// --------------------------------------------------------------------------
// Input types
// --------------------------------------------------------------------------

// ListRegionsInput is the typed input for the list_regions tool.
type ListRegionsInput struct {
	Provider string `json:"provider"`
	Domain   string `json:"domain"`
}

// ListInstanceTypesInput is the typed input for the list_instance_types tool.
type ListInstanceTypesInput struct {
	Provider    string   `json:"provider"`
	Region      string   `json:"region"`
	Family      string   `json:"family"`
	MinVCPU     *int     `json:"min_vcpu"`
	MinMemoryGB *float64 `json:"min_memory_gb"`
	GPU         bool     `json:"gpu"`
	MaxResults  int      `json:"max_results"`
}

// FindCheapestRegionInput is the typed input for the find_cheapest_region tool.
type FindCheapestRegionInput struct {
	Spec           map[string]any `json:"spec"`
	Regions        []string       `json:"regions"`
	BaselineRegion string         `json:"baseline_region"`
}

// FindAvailableRegionsInput is the typed input for the find_available_regions tool.
type FindAvailableRegionsInput struct {
	Spec           map[string]any `json:"spec"`
	Regions        []string       `json:"regions"`
	BaselineRegion string         `json:"baseline_region"`
}

// --------------------------------------------------------------------------
// list_regions
// --------------------------------------------------------------------------

// HandleListRegions implements the list_regions tool.
func (h *Handler) HandleListRegions(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in ListRegionsInput,
) (*mcp.CallToolResult, any, error) {
	defer recoverToResult(ctx, "list_regions")

	pvdr := h.provider(in.Provider)
	if pvdr == nil {
		return errResult(map[string]any{
			"error": fmt.Sprintf("Provider '%s' not configured.", in.Provider),
		}), nil, nil
	}

	domain := in.Domain
	if domain == "" {
		domain = "compute"
	}

	regions, err := pvdr.ListRegions(ctx, domain)
	if err != nil {
		return errResult(map[string]any{
			"error": err.Error(),
		}), nil, nil
	}

	regionList := make([]map[string]any, 0, len(regions))
	for _, r := range regions {
		regionList = append(regionList, map[string]any{
			"code": r,
			"name": regionDisplayNameFn(in.Provider, r),
		})
	}

	return jsonText(map[string]any{
		"provider": in.Provider,
		"domain":   domain,
		"regions":  regionList,
		"count":    len(regions),
	}), nil, nil
}

// --------------------------------------------------------------------------
// list_instance_types
// --------------------------------------------------------------------------

// HandleListInstanceTypes implements the list_instance_types tool.
func (h *Handler) HandleListInstanceTypes(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in ListInstanceTypesInput,
) (*mcp.CallToolResult, any, error) {
	defer recoverToResult(ctx, "list_instance_types")

	pvdr := h.provider(in.Provider)
	if pvdr == nil {
		return errResult(map[string]any{
			"error": fmt.Sprintf("Provider '%s' not configured.", in.Provider),
		}), nil, nil
	}

	var minVCPUs int
	if in.MinVCPU != nil {
		minVCPUs = *in.MinVCPU
	}

	var minMemGB float64
	if in.MinMemoryGB != nil {
		minMemGB = *in.MinMemoryGB
	}

	types, err := pvdr.ListInstanceTypes(ctx, in.Region, in.Family, minVCPUs, minMemGB, in.GPU)
	if err != nil {
		return errResult(map[string]any{
			"error": err.Error(),
		}), nil, nil
	}

	// Apply max_results cap (default 25 when zero/unset, matching Python behaviour).
	maxResults := in.MaxResults
	if maxResults <= 0 {
		maxResults = 25
	}
	total := len(types)
	if len(types) > maxResults {
		types = types[:maxResults]
	}

	instanceTypes := make([]map[string]any, 0, len(types))
	for _, t := range types {
		entry := map[string]any{
			"instance_type": t.InstanceType,
			"vcpu":          t.VCPU,
			"memory_gb":     t.MemoryGB,
		}
		if t.GPUCount > 0 {
			entry["gpu_count"] = t.GPUCount
		}
		if t.GPUType != "" {
			entry["gpu_type"] = t.GPUType
		}
		instanceTypes = append(instanceTypes, entry)
	}

	out := map[string]any{
		"provider":       in.Provider,
		"region":         in.Region,
		"count":          len(types),
		"instance_types": instanceTypes,
	}
	if total > maxResults {
		out["note"] = fmt.Sprintf("Showing %d of %d results — use filters to narrow", len(types), total)
	}

	return jsonText(out), nil, nil
}

// --------------------------------------------------------------------------
// find_cheapest_region
// --------------------------------------------------------------------------

// HandleFindCheapestRegion implements the find_cheapest_region tool.
//
// Fan-out: errgroup.WithContext + semaphore of 32 goroutines.
// Each goroutine fetches ALL prices for one region and contributes them to
// a flat price list (mirrors Python's extend). Results are stable-sorted
// ascending by price_per_unit.
func (h *Handler) HandleFindCheapestRegion(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in FindCheapestRegionInput,
) (*mcp.CallToolResult, any, error) {
	defer recoverToResult(ctx, "find_cheapest_region")

	baseSpec, err := unmarshalSpec(in.Spec)
	if err != nil {
		return jsonText(specErrorResponse(err, in.Spec)), nil, nil
	}

	pvdr := h.provider(string(baseSpec.GetProvider()))
	if pvdr == nil {
		return errResult(map[string]any{
			"error": fmt.Sprintf("Provider '%s' not configured.", baseSpec.GetProvider()),
		}), nil, nil
	}

	providerStr := string(baseSpec.GetProvider())

	// Determine region set and scoping.
	allRegionsRequested := len(in.Regions) == 1 && in.Regions[0] == "all"
	regions, scoped, err := resolveRegions(ctx, pvdr, in.Regions, allRegionsRequested)
	if err != nil {
		return errResult(map[string]any{"error": err.Error()}), nil, nil
	}

	// Pre-build per-region spec maps sequentially (no shared mutable state).
	type specEntry struct {
		region  string
		specMap map[string]any
	}
	specEntries := make([]specEntry, len(regions))
	baseSpecMap := fillDomain(in.Spec)
	for i, region := range regions {
		sm := copySpecMap(baseSpecMap)
		sm["region"] = region
		specEntries[i] = specEntry{region: region, specMap: sm}
	}

	// Per-region result slot (index-stable, no mutex needed).
	type regionResult struct {
		prices           []models.NormalizedPrice
		notConfiguredMsg string
	}
	results := make([]regionResult, len(regions))

	// Fan-out with errgroup + buffered channel semaphore.
	g, gctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, maxConcurrentRegions)

	for i, se := range specEntries {
		idx := i
		entry := se
		g.Go(func() error {
			sem <- struct{}{}
			defer func() { <-sem }()

			// Check if parent deadline already fired.
			if gctx.Err() != nil {
				return gctx.Err()
			}

			spec, parseErr := unmarshalSpec(entry.specMap)
			if parseErr != nil {
				// Region parse failure → treat as not-available.
				return nil
			}

			result, fetchErr := pvdr.GetPrice(gctx, spec)
			if fetchErr != nil {
				// Deadline / cancellation must abort the group.
				if gctx.Err() != nil {
					return gctx.Err()
				}
				// Detect "not configured" errors.
				if isNotConfigured(fetchErr) {
					results[idx] = regionResult{notConfiguredMsg: fetchErr.Error()}
					return nil
				}
				// Other errors: region not available — swallow.
				return nil
			}

			if result != nil {
				results[idx] = regionResult{prices: result.PublicPrices}
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		// Only a context error reaches here.
		return errResult(map[string]any{
			"error":     "context_cancelled",
			"message":   err.Error(),
			"retryable": true,
		}), nil, nil
	}

	// Aggregate results.
	var allPrices []models.NormalizedPrice
	var notAvailable []string
	var configErrMsg string

	for i, region := range regions {
		rr := results[i]
		if rr.notConfiguredMsg != "" && configErrMsg == "" {
			configErrMsg = rr.notConfiguredMsg
		}
		if len(rr.prices) > 0 {
			allPrices = append(allPrices, rr.prices...)
		} else if rr.notConfiguredMsg == "" {
			notAvailable = append(notAvailable, region)
		}
	}

	if len(allPrices) == 0 {
		if configErrMsg != "" {
			return jsonText(map[string]any{
				"result":   "provider_not_configured",
				"provider": providerStr,
				"error":    configErrMsg,
			}), nil, nil
		}
		return jsonText(map[string]any{
			"result":  "no_prices_found",
			"message": "No pricing found in any region.",
		}), nil, nil
	}

	// Stable sort ascending by price_per_unit.
	sort.SliceStable(allPrices, func(i, j int) bool {
		return allPrices[i].PricePerUnit < allPrices[j].PricePerUnit
	})

	// Build entries list.
	entries := make([]map[string]any, 0, len(allPrices))
	for _, p := range allPrices {
		e := map[string]any{
			"region":         p.Region,
			"region_name":    regionDisplayNameFn(providerStr, p.Region),
			"price_per_unit": priceDict(p.PricePerUnit, string(p.Unit)),
		}
		if p.Unit == models.PriceUnitPerHour || p.Unit == models.PriceUnitPerMonth {
			e["monthly_estimate"] = moneyDict(p.MonthlyCost(), "/mo")
		}
		entries = append(entries, e)
	}

	// Apply baseline deltas if requested.
	if in.BaselineRegion != "" {
		if err := applyBaselineDeltas(entries, in.BaselineRegion); err != nil {
			return errResult(map[string]any{"error": err.Error()}), nil, nil
		}
	}

	cheapest := allPrices[0]
	mostExp := allPrices[len(allPrices)-1]

	var priceDeltaPct any
	if cheapest.PricePerUnit > 0 {
		raw := (mostExp.PricePerUnit - cheapest.PricePerUnit) / cheapest.PricePerUnit * 100
		priceDeltaPct = math.Round(raw*10) / 10
	}

	// not_available_in: nil when empty (matches Python's `not_available or None`).
	var notAvailableOut any
	if len(notAvailable) > 0 {
		notAvailableOut = notAvailable
	}

	out := map[string]any{
		"provider":              providerStr,
		"domain":                string(baseSpec.GetDomain()),
		"service":               baseSpec.GetService(),
		"cheapest_region":       cheapest.Region,
		"cheapest_region_name":  regionDisplayNameFn(providerStr, cheapest.Region),
		"cheapest_price":        priceDict(cheapest.PricePerUnit, string(cheapest.Unit)),
		"most_expensive_region": mostExp.Region,
		"most_expensive_price":  priceDict(mostExp.PricePerUnit, string(mostExp.Unit)),
		"price_delta_pct":       priceDeltaPct,
		"all_regions_sorted":    entries,
		"not_available_in":      notAvailableOut,
	}

	if in.BaselineRegion != "" {
		out["baseline_region"] = in.BaselineRegion
		out["baseline_region_name"] = regionDisplayNameFn(providerStr, in.BaselineRegion)
	}

	if scoped {
		out["note"] = fmt.Sprintf(
			"Searched %d major %s regions. Pass regions=['all'] to search all available regions (slower on first run).",
			len(regions), strings.ToUpper(providerStr),
		)
	}

	return jsonText(out), nil, nil
}

// --------------------------------------------------------------------------
// find_available_regions
// --------------------------------------------------------------------------

// HandleFindAvailableRegions implements the find_available_regions tool.
//
// Fan-out: same errgroup+semaphore pattern as find_cheapest_region.
// For each region with prices, only prices[0] is kept (one entry per region),
// matching Python's `available.append((region, prices[0]))`.
func (h *Handler) HandleFindAvailableRegions(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in FindAvailableRegionsInput,
) (*mcp.CallToolResult, any, error) {
	defer recoverToResult(ctx, "find_available_regions")

	baseSpec, err := unmarshalSpec(in.Spec)
	if err != nil {
		return jsonText(specErrorResponse(err, in.Spec)), nil, nil
	}

	pvdr := h.provider(string(baseSpec.GetProvider()))
	if pvdr == nil {
		return errResult(map[string]any{
			"error": fmt.Sprintf("Provider '%s' not configured.", baseSpec.GetProvider()),
		}), nil, nil
	}

	providerStr := string(baseSpec.GetProvider())

	// Determine region set and scoping.
	allRegionsRequested := len(in.Regions) == 1 && in.Regions[0] == "all"
	regions, scoped, err := resolveRegions(ctx, pvdr, in.Regions, allRegionsRequested)
	if err != nil {
		return errResult(map[string]any{"error": err.Error()}), nil, nil
	}

	// Pre-build per-region spec maps sequentially.
	type specEntry struct {
		region  string
		specMap map[string]any
	}
	specEntries := make([]specEntry, len(regions))
	baseSpecMap := fillDomain(in.Spec)
	for i, region := range regions {
		sm := copySpecMap(baseSpecMap)
		sm["region"] = region
		specEntries[i] = specEntry{region: region, specMap: sm}
	}

	// Per-region result slot: one price (first) or nil.
	type regionResult struct {
		price *models.NormalizedPrice
	}
	results := make([]regionResult, len(regions))

	// Fan-out with errgroup + buffered channel semaphore.
	g, gctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, maxConcurrentRegions)

	for i, se := range specEntries {
		idx := i
		entry := se
		g.Go(func() error {
			sem <- struct{}{}
			defer func() { <-sem }()

			if gctx.Err() != nil {
				return gctx.Err()
			}

			spec, parseErr := unmarshalSpec(entry.specMap)
			if parseErr != nil {
				return nil
			}

			result, fetchErr := pvdr.GetPrice(gctx, spec)
			if fetchErr != nil {
				if gctx.Err() != nil {
					return gctx.Err()
				}
				// Any other error → region not available.
				return nil
			}

			if result != nil && len(result.PublicPrices) > 0 {
				p := result.PublicPrices[0]
				results[idx] = regionResult{price: &p}
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return errResult(map[string]any{
			"error":     "context_cancelled",
			"message":   err.Error(),
			"retryable": true,
		}), nil, nil
	}

	// Aggregate: separate available from not-available.
	type availEntry struct {
		region string
		price  models.NormalizedPrice
	}
	var available []availEntry
	var notAvailable []string

	for i, region := range regions {
		if results[i].price != nil {
			available = append(available, availEntry{region: region, price: *results[i].price})
		} else {
			notAvailable = append(notAvailable, region)
		}
	}

	if len(available) == 0 {
		return jsonText(map[string]any{
			"result":  "not_available",
			"message": "Not available in any of the checked regions.",
		}), nil, nil
	}

	// Sort available by price_per_unit ascending.
	sort.SliceStable(available, func(i, j int) bool {
		return available[i].price.PricePerUnit < available[j].price.PricePerUnit
	})

	// Build entries list.
	entries := make([]map[string]any, 0, len(available))
	for _, av := range available {
		p := av.price
		e := map[string]any{
			"region":         av.region,
			"region_name":    regionDisplayNameFn(providerStr, av.region),
			"price_per_unit": priceDict(p.PricePerUnit, string(p.Unit)),
		}
		if p.Unit == models.PriceUnitPerHour || p.Unit == models.PriceUnitPerMonth {
			e["monthly_estimate"] = moneyDict(p.MonthlyCost(), "/mo")
		}
		entries = append(entries, e)
	}

	// Apply baseline deltas if requested.
	if in.BaselineRegion != "" {
		if err := applyBaselineDeltas(entries, in.BaselineRegion); err != nil {
			return errResult(map[string]any{"error": err.Error()}), nil, nil
		}
	}

	// not_available_in: nil when empty (Python: `not_available or None`).
	var notAvailableOut any
	if len(notAvailable) > 0 {
		notAvailableOut = notAvailable
	}

	out := map[string]any{
		"provider":                      providerStr,
		"domain":                        string(baseSpec.GetDomain()),
		"available_in":                  len(available),
		"not_available_in":              notAvailableOut,
		"regions_sorted_cheapest_first": entries,
	}

	if in.BaselineRegion != "" {
		out["baseline_region"] = in.BaselineRegion
	}

	if scoped {
		out["note"] = fmt.Sprintf(
			"Searched %d major %s regions. Pass regions=['all'] to check all available regions.",
			len(regions), strings.ToUpper(providerStr),
		)
	}

	return jsonText(out), nil, nil
}

// --------------------------------------------------------------------------
// Shared helpers
// --------------------------------------------------------------------------

// resolveRegions determines which regions to query and whether the result is
// scoped to major regions. It mirrors the Python logic in find_cheapest_region
// and find_available_regions:
//
//   - If regions is nil/empty or ["all"] → fetch all available from provider.
//     When ["all"] was NOT requested, scope to major regions if provider has any.
//   - Otherwise use the supplied region list as-is (scoped = false).
func resolveRegions(
	ctx context.Context,
	pvdr Provider,
	inputRegions []string,
	allRegionsRequested bool,
) (regions []string, scoped bool, err error) {
	if len(inputRegions) == 0 || allRegionsRequested {
		allAvailable, listErr := pvdr.ListRegions(ctx, "compute")
		if listErr != nil {
			return nil, false, listErr
		}
		major := pvdr.MajorRegions()
		if len(major) > 0 && !allRegionsRequested {
			return major, true, nil
		}
		return allAvailable, false, nil
	}
	// Caller provided an explicit list.
	return inputRegions, false, nil
}

// copySpecMap returns a shallow copy of a map[string]any.
func copySpecMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

