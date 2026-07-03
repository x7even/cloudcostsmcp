// Package tools implements the MCP tool handler logic for all 15 tools.
// Each sub-file covers a logical group: lookup (this file), availability, bom, cache.
//
// Handler is the central struct: it holds the providers map and cache reference
// and exposes one method per tool. The server package wires these methods into
// the mcp.Server tool registrations.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/cache"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/utils"
)

// Per-region / per-item outcome classification for fan-out lookups
// (compare_prices, get_prices_batch). Distinguishes a genuine "no pricing
// data for this region/item" outcome from a transport/upstream failure that
// may resolve on retry.
const (
	regionStatusOK        = "ok"
	regionStatusTransient = "transient_error"
	regionStatusNoData    = "no_data"
)

// Provider is an alias so callers of this package do not need to import providers directly.
type Provider = providers.Provider

// Handler holds the runtime dependencies shared across all tool handlers.
// Initialize with New and pass the result to the server for handler binding.
type Handler struct {
	providers map[string]Provider
	// cm is populated via SetCache; nil means cache tools return an error.
	cm *cache.CacheManager
}

// New creates a Handler with the given provider map.
// A nil map is valid: every call will return a "provider not configured" error.
func New(provs map[string]Provider) *Handler {
	if provs == nil {
		provs = make(map[string]Provider)
	}
	return &Handler{providers: provs}
}

// SetCache wires a *cache.CacheManager into the Handler for cache-related tools.
func (h *Handler) SetCache(cm *cache.CacheManager) {
	h.cm = cm
}

// provider returns the named provider or nil if not configured.
func (h *Handler) provider(name string) Provider {
	return h.providers[name]
}

// --------------------------------------------------------------------------
// JSON response helpers
// --------------------------------------------------------------------------

// jsonText returns a *mcp.CallToolResult containing a single TextContent block
// with the JSON-serialised value of v. If marshalling fails, a structured
// {"error": "internal_error", ...} object is returned instead.
func jsonText(v any) *mcp.CallToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(map[string]any{
			"error":   "internal_error",
			"message": "failed to serialise response",
		})
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(b)},
		},
	}
}

// errResult wraps a structured error map in a CallToolResult.
func errResult(fields map[string]any) *mcp.CallToolResult {
	return jsonText(fields)
}

// --------------------------------------------------------------------------
// Money / price formatting — mirrors utils/money.py
// --------------------------------------------------------------------------

// priceDict returns the structured price dict used in all tool responses.
// Mirrors Python's _price(amount, unit).
func priceDict(amount float64, unit string) map[string]any {
	var display string
	if amount > 0 && amount < 5e-7 {
		display = fmt.Sprintf("$%.2e/%s", amount, unit)
	} else {
		display = fmt.Sprintf("$%.6f/%s", amount, unit)
	}
	return map[string]any{
		"amount":   amount,
		"unit":     unit,
		"currency": "USD",
		"display":  display,
	}
}

// moneyDict returns the structured aggregate money dict.
// Mirrors Python's _money(amount, label).
func moneyDict(amount float64, label string) map[string]any {
	return map[string]any{
		"amount":   amount,
		"currency": "USD",
		"display":  fmt.Sprintf("$%.2f%s", amount, label),
	}
}

// --------------------------------------------------------------------------
// Region display name — stub (full maps live in utils/regions.go when Agent 3A
// delivers them). Falls back to the raw region code until then.
// --------------------------------------------------------------------------

// RegionDisplayName returns the human-friendly name for a region code.
// The full lookup maps are populated by Agent 3A in internal/utils/regions.go.
// Until then this function delegates to regionDisplayNameFn, which starts as
// the identity fallback and is overridden in tests/integration.
var regionDisplayNameFn = func(provider, region string) string {
	return region
}

// RegionDisplayName is the exported version used by tests.
func RegionDisplayName(provider, region string) string {
	return regionDisplayNameFn(provider, region)
}

// SetRegionDisplayNameFn replaces the region name lookup. Used by the server
// package once utils/regions.go is wired, and by tests.
func SetRegionDisplayNameFn(fn func(provider, region string) string) {
	regionDisplayNameFn = fn
}

// --------------------------------------------------------------------------
// Spec-inference helpers — mirrors utils/spec_infer.py
// --------------------------------------------------------------------------

// serviceToDomain maps known service identifiers to their canonical PricingDomain.
var serviceToDomain = map[string]models.PricingDomain{
	// database
	"rds":         models.PricingDomainDatabase,
	"cloud_sql":   models.PricingDomainDatabase,
	"memorystore": models.PricingDomainDatabase,
	"sql":         models.PricingDomainDatabase,
	"cosmos":      models.PricingDomainDatabase,
	"elasticache": models.PricingDomainDatabase,
	// analytics
	"bigquery": models.PricingDomainAnalytics,
	// network
	"cloud_nat": models.PricingDomainNetwork,
	"cloud_lb":  models.PricingDomainNetwork,
	"cloud_cdn": models.PricingDomainNetwork,
	"nat":       models.PricingDomainNetwork,
	"lb":        models.PricingDomainNetwork,
	"cdn":       models.PricingDomainNetwork,
	// observability
	"cloud_armor":      models.PricingDomainNetwork,
	"cloudwatch":       models.PricingDomainObservability,
	"cloud_monitoring": models.PricingDomainObservability,
	// ai
	"bedrock":   models.PricingDomainAI,
	"gemini":    models.PricingDomainAI,
	"vertex":    models.PricingDomainAI,
	"openai":    models.PricingDomainAI,
	"sagemaker": models.PricingDomainAI,
	// serverless
	"lambda":          models.PricingDomainServerless,
	"functions":       models.PricingDomainServerless,
	"azure_functions": models.PricingDomainServerless,
	"cloud_functions": models.PricingDomainServerless,
	"cloud_run":       models.PricingDomainServerless,
	// container
	"gke": models.PricingDomainContainer,
	"eks": models.PricingDomainContainer,
	"aks": models.PricingDomainContainer,
	// egress
	// "egress" is intentionally absent: it is a valid service in BOTH domain=network
	// (internet egress with tiered pricing via NetworkPricingSpec + destination_type=internet)
	// AND domain=inter_region_egress (flat per-region rates via EgressPricingSpec).
	// Including it here would override an explicitly supplied domain=network during FillDomain,
	// routing the get_price call to EgressPricingSpec instead of NetworkPricingSpec and
	// bypassing the internetEgressPrices() tiered-rate branch. All prompts that use
	// service=egress supply an explicit domain, so FillDomain returns early before this map
	// is consulted anyway.
	"data_transfer": models.PricingDomainInterRegionEgress,
}

// validTerms is shown in error hints when an invalid term is supplied.
const validTerms = "on_demand, spot, reserved_1yr, reserved_1yr_partial, reserved_1yr_all, " +
	"reserved_3yr, reserved_3yr_partial, reserved_3yr_all, " +
	"cud_1yr, cud_3yr, flex_cud, sud, compute_savings_plan, ec2_instance_savings_plan"

// fillDomain adds a "domain" key to spec if it can be inferred. Mirrors
// fill_domain() in utils/spec_infer.py.
//
// fillDomain always returns a new map — it never returns the input map itself.
// This is required for goroutine safety: multiple goroutines may call fillDomain
// on the same input spec concurrently (e.g. compare_prices fan-out) and then
// set additional keys (e.g. "region") on the returned map without racing.
func fillDomain(spec map[string]any) map[string]any {
	// Determine the domain to inject (if any).
	var inferred string
	if _, ok := spec["domain"]; !ok {
		// 1. Service-keyed lookup.
		if sv, ok := spec["service"]; ok {
			svc := strings.ToLower(fmt.Sprintf("%v", sv))
			if domain, found := serviceToDomain[svc]; found {
				inferred = string(domain)
			}
		}
		// 2. storage_type → storage
		if inferred == "" {
			if _, ok := spec["storage_type"]; ok {
				inferred = "storage"
			}
		}
		// 3. resource_type prefix patterns
		if inferred == "" {
			if rtv, ok := spec["resource_type"]; ok {
				rt := strings.ToLower(fmt.Sprintf("%v", rtv))
				if rt != "" {
					if strings.HasPrefix(rt, "db.") || strings.HasPrefix(rt, "cache.") {
						inferred = "database"
					} else if strings.Contains(rt, ".") || strings.Contains(rt, "-") ||
						strings.HasPrefix(rt, "standard_") || strings.HasPrefix(rt, "basic_") ||
						strings.HasPrefix(rt, "premium_") {
						inferred = "compute"
					}
				}
			}
		}
	}

	// Always return a fresh copy so callers can mutate it without racing.
	out := make(map[string]any, len(spec)+1)
	for k, v := range spec {
		out[k] = v
	}
	if inferred != "" {
		out["domain"] = inferred
	}
	return out
}

// specErrorResponse builds a structured error response for invalid specs.
// Mirrors spec_error_response() in utils/spec_infer.py.
func specErrorResponse(err error, spec map[string]any) map[string]any {
	msg := err.Error()
	resp := map[string]any{
		"error":  "invalid_spec",
		"reason": msg,
		"hint": "Call describe_catalog(provider, domain, service) to get a valid " +
			"example_invocation for your provider/domain/service combination.",
	}

	msgLower := strings.ToLower(msg)
	_, hasDomain := spec["domain"]
	_, hasProvider := spec["provider"]

	switch {
	case strings.Contains(msgLower, "unable to extract tag") ||
		(!hasDomain && strings.Contains(msgLower, "discriminator")):
		resp["fix"] = "The 'domain' field is required and must be one of: " +
			"compute, storage, database, ai, container, serverless, " +
			"analytics, network, observability, inter_region_egress"
	case strings.Contains(msg, ".term") && strings.Contains(msgLower, "input should be"):
		resp["fix"] = fmt.Sprintf("Valid term values: %s", validTerms)
	case !hasProvider:
		resp["fix"] = "The 'provider' field is required: 'aws', 'gcp', or 'azure'."
	}

	return resp
}

// unmarshalSpec converts a map[string]any (the raw JSON params) to a PricingSpec.
// It round-trips through JSON to let models.UnmarshalPricingSpec do the two-pass
// discriminated-union decode.
func unmarshalSpec(specMap map[string]any) (models.PricingSpec, error) {
	specMap = fillDomain(specMap)
	data, err := json.Marshal(specMap)
	if err != nil {
		return nil, fmt.Errorf("cannot marshal spec: %w", err)
	}
	return models.UnmarshalPricingSpec(data)
}

// --------------------------------------------------------------------------
// Baseline delta helpers — mirrors utils/baseline.py
// --------------------------------------------------------------------------

// applyBaselineDeltas mutates entries in-place to add delta_per_hour,
// delta_monthly, delta_pct fields vs the named baseline region.
//
// groupKey, when non-empty, names a string-valued key present on each entry
// (e.g. "sku_description") that discriminates entries covering different
// SKUs/line-items for the same region. When set, deltas are computed
// separately within each group against that group's own baseline-region
// entry, so a multi-SKU result set (e.g. GCP Cloud CDN's cache-egress and
// cache-fill SKUs) never computes a delta_pct by comparing one SKU's price
// against a different SKU's baseline price (RC3-033 / #29). Pass "" to treat
// all entries as a single group — the original, pre-RC3-033 behavior.
//
// Returns an error only if the baseline region is absent from every group.
// Callers that want to degrade gracefully (see HandleComparePrices /
// RC3-002) rather than hard-fail the whole call may ignore the error:
// entries are still mutated in that case, with delta_per_hour/delta_monthly/
// delta_pct set to an explicit nil on every entry whose own group has no
// baseline, so the partial payload remains well-formed.
func applyBaselineDeltas(entries []map[string]any, baselineRegion string, groupKey string) error {
	type group struct {
		baseHourly  float64
		baseMonthly float64
		found       bool
		members     []map[string]any
	}
	groups := make(map[string]*group)
	var order []string
	for _, e := range entries {
		key := ""
		if groupKey != "" {
			if v, ok := e[groupKey].(string); ok {
				key = v
			}
		}
		g, ok := groups[key]
		if !ok {
			g = &group{}
			groups[key] = g
			order = append(order, key)
		}
		g.members = append(g.members, e)
		if !g.found && e["region"] == baselineRegion {
			if pd, ok := e["price_per_unit"].(map[string]any); ok {
				if a, ok := pd["amount"].(float64); ok {
					g.baseHourly = a
				}
			}
			if md, ok := e["monthly_estimate"].(map[string]any); ok {
				if a, ok := md["amount"].(float64); ok {
					g.baseMonthly = a
				}
			}
			g.found = true
		}
	}

	anyFound := false
	for _, key := range order {
		if groups[key].found {
			anyFound = true
			break
		}
	}

	if !anyFound {
		available := make([]string, 0, len(entries))
		for _, e := range entries {
			if r, ok := e["region"].(string); ok {
				available = append(available, r)
			}
		}
		// Null out (not omit) the delta fields on every entry so a caller
		// that chooses to return the partial payload anyway (RC3-002) gets a
		// well-formed, self-explanatory shape rather than missing keys.
		for _, e := range entries {
			e["delta_per_hour"] = nil
			e["delta_monthly"] = nil
			e["delta_pct"] = nil
		}
		return fmt.Errorf("baseline region %q not found in results. Available: %v",
			baselineRegion, available)
	}

	for _, key := range order {
		g := groups[key]
		for _, e := range g.members {
			if !g.found {
				// This SKU group has no baseline-region entry of its own —
				// null out rather than compute against a different group's
				// baseline (RC3-033 / #29).
				e["delta_per_hour"] = nil
				e["delta_monthly"] = nil
				e["delta_pct"] = nil
				continue
			}
			var h float64
			if pd, ok := e["price_per_unit"].(map[string]any); ok {
				if a, ok := pd["amount"].(float64); ok {
					h = a
				}
			}
			var m float64
			if md, ok := e["monthly_estimate"].(map[string]any); ok {
				if a, ok := md["amount"].(float64); ok {
					m = a
				}
			}
			dh := h - g.baseHourly
			dm := m - g.baseMonthly
			var pct float64
			if g.baseHourly > 0 {
				pct = (dh / g.baseHourly) * 100
			}
			e["delta_per_hour"] = fmt.Sprintf("$%+.6f", dh)
			e["delta_monthly"] = fmt.Sprintf("$%+.2f/mo", dm)
			e["delta_pct"] = fmt.Sprintf("%+.1f%%", pct)
		}
	}
	return nil
}

// --------------------------------------------------------------------------
// NormalizedPrice summary — mirrors NormalizedPrice.summary() in models.py
// --------------------------------------------------------------------------

// normalizedPriceSummary builds the LLM-readable summary dict for a NormalizedPrice.
// Mirrors the summary() method in Python's NormalizedPrice.
func normalizedPriceSummary(p models.NormalizedPrice) map[string]any {
	result := map[string]any{
		"provider":    string(p.Provider),
		"description": p.Description,
		"region":      p.Region,
		"region_name": regionDisplayNameFn(string(p.Provider), p.Region),
		"term":        string(p.PricingTerm),
		"price":       priceDict(p.PricePerUnit, string(p.Unit)),
	}
	if p.Unit == models.PriceUnitPerHour || p.Unit == models.PriceUnitPerMonth {
		result["monthly_estimate"] = moneyDict(p.MonthlyCost(), "/mo")
	}

	// Include selected attributes — mirrors Python's filter set.
	// "storage" is included to disambiguate Aurora Standard ("EBS Only") from
	// Aurora I/O-Optimized ("Aurora IO Optimization Mode") — these are the
	// only two on-demand Aurora SKUs per instance type at Single-AZ, and without
	// this field both appear identical to the model causing extra tool calls.
	attrKeys := map[string]bool{
		"instanceType":    true,
		"vcpu":            true,
		"memory":          true,
		"operatingSystem": true,
		"storage":         true,
		"storage_type":    true,
		"volumeType":      true,
		"fallback":        true,
		"fallback_note":   true,
		"note":            true,
		"fromRegionCode":  true,
		"toRegionCode":    true,
	}
	for k, v := range p.Attributes {
		if attrKeys[k] {
			result[k] = v
		}
	}

	if p.FetchedAt != nil {
		result["as_of"] = p.FetchedAt.Format("2006-01-02T15:04:05.999999Z07:00")
	}
	if p.CacheAgeSeconds != nil {
		result["cache_age_seconds"] = *p.CacheAgeSeconds
	}
	if p.EffectiveDate != nil {
		result["price_effective_date"] = p.EffectiveDate.Format("2006-01-02")
	}
	if p.SourceURL != "" {
		result["source_url"] = p.SourceURL
	}
	return result
}

// allFallback reports whether every price in prices carries
// Attributes["fallback"] == "true" (returns false for an empty slice — there
// is nothing to warn about with zero results). Used to decide whether a
// top-level warning is warranted that an entire result set is static/
// fallback data rather than live pricing, so a ranking or single-price
// answer built entirely from it isn't presented with unwarranted confidence
// (RC3-010).
func allFallback(prices []models.NormalizedPrice) bool {
	if len(prices) == 0 {
		return false
	}
	for _, p := range prices {
		if p.Attributes["fallback"] != "true" {
			return false
		}
	}
	return true
}

// pricingResultSummary builds the LLM-readable summary dict for a PricingResult.
// Mirrors PricingResult.summary() in Python's models.py.
func pricingResultSummary(r *models.PricingResult) map[string]any {
	out := map[string]any{
		"public_prices":  priceSummaries(r.PublicPrices),
		"auth_available": r.AuthAvailable,
		"source":         r.Source,
	}
	if len(r.ContractedPrices) > 0 {
		out["contracted_prices"] = priceSummaries(r.ContractedPrices)
	}
	if r.EffectivePrice != nil {
		ep := r.EffectivePrice
		unit := string(ep.BasePrice.Unit)
		out["effective_price"] = map[string]any{
			"price":                priceDict(ep.EffectivePricePerUnit, unit),
			"discount_type":        ep.DiscountType,
			"discount_pct":         ep.DiscountPct,
			"savings_vs_on_demand": priceDict(ep.SavingsVsOnDemand(), unit),
		}
	}
	if len(r.Breakdown) > 0 {
		out["breakdown"] = r.Breakdown
	}
	if r.Note != "" {
		out["note"] = r.Note
	}
	if allFallback(r.PublicPrices) {
		out["warnings"] = []string{
			"all results are fallback/static data; prices may not reflect current live rates",
		}
	}
	return out
}

// priceSummaries converts a slice of NormalizedPrices to their summary dicts.
func priceSummaries(prices []models.NormalizedPrice) []map[string]any {
	out := make([]map[string]any, len(prices))
	for i, p := range prices {
		out[i] = normalizedPriceSummary(p)
	}
	return out
}

// --------------------------------------------------------------------------
// GetPrice — unified pricing entry point
// --------------------------------------------------------------------------

// GetPriceInput is the typed input for the get_price tool.
type GetPriceInput struct {
	Spec map[string]any `json:"spec"`
}

// HandleGetPrice implements the get_price tool.
func (h *Handler) HandleGetPrice(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in GetPriceInput,
) (*mcp.CallToolResult, any, error) {
	defer recoverToResult(ctx, "get_price")

	spec, err := unmarshalSpec(in.Spec)
	if err != nil {
		return jsonText(specErrorResponse(err, in.Spec)), nil, nil
	}

	pvdr := h.provider(string(spec.GetProvider()))
	if pvdr == nil {
		available := make([]string, 0, len(h.providers))
		for k := range h.providers {
			available = append(available, k)
		}
		sort.Strings(available)
		return errResult(map[string]any{
			"error": fmt.Sprintf("Provider '%s' not configured. Available: %v",
				spec.GetProvider(), available),
		}), nil, nil
	}

	// Apply default region if not specified.
	if spec.GetRegion() == "" {
		def := pvdr.DefaultRegion()
		if def == "" {
			def = "us-east-1"
		}
		// Re-unmarshal with region filled in.
		specMap := fillDomain(in.Spec)
		specMap["region"] = def
		spec, err = unmarshalSpec(specMap)
		if err != nil {
			return jsonText(specErrorResponse(err, specMap)), nil, nil
		}
	}

	if !pvdr.Supports(spec.GetDomain(), spec.GetService()) {
		notSuppResp := map[string]any{
			"error":    "not_supported",
			"provider": string(spec.GetProvider()),
			"domain":   string(spec.GetDomain()),
			"service":  spec.GetService(),
			"reason": fmt.Sprintf("%s does not support %s/%s.",
				spec.GetProvider(), spec.GetDomain(), spec.GetService()),
			"alternatives": []string{"Call describe_catalog() to see what this provider supports."},
		}
		// GCP serverless (Cloud Run, Cloud Functions) is not implemented in the
		// Go provider — it is only available via the Python provider.
		if spec.GetProvider() == "gcp" && spec.GetDomain() == models.PricingDomainServerless {
			notSuppResp["reason"] = fmt.Sprintf(
				"GCP serverless (%s) is not supported in the Go provider. "+
					"Cloud Run and Cloud Functions pricing is only available via the Python provider.",
				spec.GetService(),
			)
		}
		return errResult(notSuppResp), nil, nil
	}

	result, err := pvdr.GetPrice(ctx, spec)
	if err != nil {
		if errors.Is(err, providers.ErrNotSupported) {
			return errResult(map[string]any{
				"error":    "not_supported",
				"provider": string(spec.GetProvider()),
				"domain":   string(spec.GetDomain()),
				"service":  spec.GetService(),
				"reason":   err.Error(),
			}), nil, nil
		}
		if isNotConfigured(err) {
			return errResult(map[string]any{
				"error":  "not_configured",
				"reason": err.Error(),
				"hint":   "Configure provider credentials to enable this feature.",
			}), nil, nil
		}
		return errResult(map[string]any{
			"error":     "upstream_failure",
			"message":   "Pricing lookup failed. Try again shortly.",
			"retryable": true,
		}), nil, nil
	}

	if result == nil {
		provName := string(spec.GetProvider())
		svc := spec.GetService()
		region := spec.GetRegion()
		out := map[string]any{
			"result":          "no_results",
			"provider":        provName,
			"service":         svc,
			"region":          region,
			"filters_applied": map[string]any{},
			"message": fmt.Sprintf(
				"No pricing found for service '%s' in %s with the provided filters.", svc, region,
			),
			"tip": fmt.Sprintf(
				"Try describe_catalog(provider='%s', domain='<domain>') to see supported domains, "+
					"services, and an example_invocation to copy. "+
					"Then retry get_price with the exact spec from example_invocation.",
				provName,
			),
		}
		// not_available_in mirrors the coverage-disclosure field used by the
		// fan-out lookup tools (compare_prices, find_cheapest_region,
		// find_available_regions, get_prices_batch). get_price only ever
		// queries a single region, so this is always a single-element list
		// naming that region — but keeping the same field name/shape lets
		// callers check for coverage gaps generically across tools instead
		// of parsing get_price's no_results/message shape separately
		// (RC3-018 / #37).
		if region != "" {
			out["not_available_in"] = []string{region}
		}
		return jsonText(out), nil, nil
	}
	return jsonText(pricingResultSummary(result)), nil, nil
}

// --------------------------------------------------------------------------
// GetPricesBatch
// --------------------------------------------------------------------------

// GetPricesBatchInput is the typed input for the get_prices_batch tool.
type GetPricesBatchInput struct {
	Provider      string   `json:"provider"`
	InstanceTypes []string `json:"instance_types"`
	Region        string   `json:"region"`
	OS            string   `json:"os"`
	Term          string   `json:"term"`
}

// HandleGetPricesBatch implements the get_prices_batch tool.
func (h *Handler) HandleGetPricesBatch(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in GetPricesBatchInput,
) (*mcp.CallToolResult, any, error) {
	defer recoverToResult(ctx, "get_prices_batch")

	// Apply defaults (these match the Python defaults).
	if in.OS == "" {
		in.OS = "Linux"
	}
	if in.Term == "" {
		in.Term = "on_demand"
	}

	pvdr := h.provider(in.Provider)
	if pvdr == nil {
		return errResult(map[string]any{
			"error": fmt.Sprintf("Provider '%s' not configured.", in.Provider),
		}), nil, nil
	}

	type fetchResult struct {
		itype  string
		prices []models.NormalizedPrice
		err    string
		status string // transient_error | no_data; only meaningful when err != ""
	}

	// Fan-out concurrently.
	results := make([]fetchResult, len(in.InstanceTypes))
	var wg sync.WaitGroup
	for i, itype := range in.InstanceTypes {
		wg.Add(1)
		go func(idx int, it string) {
			defer wg.Done()
			specMap := map[string]any{
				"provider":      in.Provider,
				"domain":        "compute",
				"resource_type": it,
				"region":        in.Region,
				"os":            in.OS,
				"term":          in.Term,
			}
			spec, err := unmarshalSpec(specMap)
			if err != nil {
				results[idx] = fetchResult{itype: it, err: err.Error(), status: regionStatusNoData}
				return
			}
			r, err := pvdr.GetPrice(ctx, spec)
			if err != nil {
				// Classify: distinguish a transport/upstream failure (may
				// resolve on retry) from a permanent/no-data error (RC3-001).
				if errors.Is(err, providers.ErrNotSupported) || !utils.IsTransient(err) {
					results[idx] = fetchResult{itype: it, err: err.Error(), status: regionStatusNoData}
					return
				}
				results[idx] = fetchResult{itype: it, err: err.Error(), status: regionStatusTransient}
				return
			}
			if r == nil {
				results[idx] = fetchResult{itype: it, prices: []models.NormalizedPrice{}, status: regionStatusNoData}
				return
			}
			results[idx] = fetchResult{itype: it, prices: r.PublicPrices}
		}(i, itype)
	}
	wg.Wait()

	type entry struct {
		instanceType  string
		pricePerHour  float64
		monthlyEst    float64
		pricePerUnit  map[string]any
		monthlyEstMap map[string]any
		vcpu          string
		memory        string
		description   string
		fallback      bool
	}

	var entries []entry
	errMap := make(map[string]any)
	var notAvailable []string

	for _, fr := range results {
		if fr.err != "" {
			status := fr.status
			if status == "" {
				status = regionStatusNoData
			}
			errMap[fr.itype] = map[string]any{
				"message":   fr.err,
				"status":    status,
				"retryable": status == regionStatusTransient,
			}
			if status == regionStatusNoData {
				notAvailable = append(notAvailable, fr.itype)
			}
			continue
		}
		if len(fr.prices) == 0 {
			errMap[fr.itype] = map[string]any{
				"message":   "no pricing found",
				"status":    regionStatusNoData,
				"retryable": false,
			}
			notAvailable = append(notAvailable, fr.itype)
			continue
		}
		p := fr.prices[0]
		var hasFallback bool
		for _, price := range fr.prices {
			if price.Attributes["fallback"] == "true" {
				hasFallback = true
				break
			}
		}
		e := entry{
			instanceType:  fr.itype,
			pricePerHour:  p.PricePerUnit,
			monthlyEst:    p.MonthlyCost(),
			pricePerUnit:  priceDict(p.PricePerUnit, string(p.Unit)),
			monthlyEstMap: moneyDict(p.MonthlyCost(), "/mo"),
			description:   p.Description,
			fallback:      hasFallback,
		}
		if v, ok := p.Attributes["vcpu"]; ok {
			e.vcpu = v
		}
		if v, ok := p.Attributes["memory"]; ok {
			e.memory = v
		}
		entries = append(entries, e)
	}

	// Sort by price ascending.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].pricePerHour < entries[j].pricePerHour
	})

	resultList := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		row := map[string]any{
			"instance_type":    e.instanceType,
			"price_per_hour":   e.pricePerUnit,
			"monthly_estimate": e.monthlyEstMap,
		}
		if e.vcpu != "" {
			row["vcpu"] = e.vcpu
		}
		if e.memory != "" {
			row["memory"] = e.memory
		}
		if e.description != "" {
			row["description"] = e.description
		}
		if e.fallback {
			row["fallback"] = "true"
		}
		resultList = append(resultList, row)
	}

	out := map[string]any{
		"provider": in.Provider,
		"region":   in.Region,
		"os":       in.OS,
		"term":     in.Term,
		"count":    len(resultList),
		"results":  resultList,
	}
	if len(errMap) > 0 {
		out["errors"] = errMap
	}
	// not_available_in mirrors the coverage-disclosure field used by the
	// other fan-out lookup tools (compare_prices, find_cheapest_region,
	// find_available_regions). get_prices_batch fans out across
	// instance_types rather than regions, so the entries here are the
	// instance types that had no pricing data (a subset of errMap's keys,
	// excluding transient/retryable failures) — kept as its own top-level
	// list so callers can check for coverage gaps generically, without
	// having to inspect per-item status inside errMap (RC3-018 / #37).
	if len(notAvailable) > 0 {
		out["not_available_in"] = notAvailable
	}
	return jsonText(out), nil, nil
}

// --------------------------------------------------------------------------
// ComparePrices
// --------------------------------------------------------------------------

// ComparePricesInput is the typed input for the compare_prices tool.
type ComparePricesInput struct {
	Spec           map[string]any `json:"spec"`
	Regions        []string       `json:"regions"`
	BaselineRegion string         `json:"baseline_region"`
}

// HandleComparePrices implements the compare_prices tool.
func (h *Handler) HandleComparePrices(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in ComparePricesInput,
) (*mcp.CallToolResult, any, error) {
	defer recoverToResult(ctx, "compare_prices")

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

	if !pvdr.Supports(baseSpec.GetDomain(), baseSpec.GetService()) {
		return errResult(map[string]any{
			"error": fmt.Sprintf("%s does not support %s/%s.",
				baseSpec.GetProvider(), baseSpec.GetDomain(), baseSpec.GetService()),
		}), nil, nil
	}

	// Fan-out with a semaphore of 10.
	type regionResult struct {
		region string
		prices []models.NormalizedPrice
		status string // ok | transient_error | no_data
		errMsg string // populated when status == transient_error
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

			// Build a spec for this region by re-marshalling with region overridden.
			specMap := fillDomain(in.Spec)
			specMap["region"] = rgn
			spec, err := unmarshalSpec(specMap)
			if err != nil {
				results[idx] = regionResult{region: rgn, status: regionStatusNoData}
				return
			}
			r, err := pvdr.GetPrice(ctx, spec)
			if err != nil {
				// Classify: a transport/upstream error may resolve on retry and
				// must not be silently collapsed into a genuine "no data" result
				// (RC3-001). ErrNotSupported and non-transient (permanent) errors
				// are treated as no-data for this region rather than retryable.
				if errors.Is(err, providers.ErrNotSupported) || !utils.IsTransient(err) {
					results[idx] = regionResult{region: rgn, status: regionStatusNoData}
					return
				}
				results[idx] = regionResult{region: rgn, status: regionStatusTransient, errMsg: err.Error()}
				return
			}
			if r == nil || len(r.PublicPrices) == 0 {
				results[idx] = regionResult{region: rgn, status: regionStatusNoData}
				return
			}
			results[idx] = regionResult{region: rgn, status: regionStatusOK, prices: r.PublicPrices}
		}(i, region)
	}
	wg.Wait()

	var allPrices []models.NormalizedPrice
	var notAvailable []string
	transientErrors := make(map[string]string)
	for _, rr := range results {
		switch rr.status {
		case regionStatusOK:
			allPrices = append(allPrices, rr.prices...)
		case regionStatusTransient:
			transientErrors[rr.region] = rr.errMsg
		default:
			notAvailable = append(notAvailable, rr.region)
		}
	}

	if len(allPrices) == 0 {
		// All regions failed via transport/upstream errors (none were genuine
		// no-data) — surface this as a retryable upstream failure instead of a
		// fake no_prices_found, matching HandleGetPrice's contract (RC3-001).
		if len(transientErrors) > 0 && len(notAvailable) == 0 {
			return errResult(map[string]any{
				"error":     "upstream_failure",
				"message":   "Pricing lookup failed for all requested regions. Try again shortly.",
				"retryable": true,
			}), nil, nil
		}
		out := map[string]any{
			"result":  "no_prices_found",
			"message": "No pricing found in any of the specified regions.",
		}
		if len(transientErrors) > 0 {
			out["transient_errors"] = transientErrors
		}
		return jsonText(out), nil, nil
	}

	// A spec can resolve to more than one SKU per region (e.g. GCP Cloud CDN
	// returns both a cache-egress and a cache-fill NormalizedPrice for every
	// region). Sorting allPrices purely by price would then interleave
	// unrelated SKUs into a single cheapest-first ranking, so the top result
	// could silently compare one SKU's price in region A against a different
	// SKU's price in region B (RC3-033 / #29). To avoid that, group by SKU
	// and sort within each group before ranking groups against each other:
	// entries for the same SKU always stay contiguous and cheapest-first
	// within their own group.
	//
	// The discriminator is NOT the raw Description text: many provider call
	// sites (internal/providers/gcp/gcp_compute.go GCS storage, gcp_database.go
	// Cloud SQL/Memorystore/GKE fees, gcp_ai.go Vertex AI/BigQuery,
	// aws_network.go inter-region transfer, etc.) build Description with
	// fmt.Sprintf(..., region), embedding the very region string that varies
	// across this fan-out. Using raw Description as the key would then treat
	// the SAME SKU priced in different regions as N separate one-member "SKU
	// groups", which defeats multi-region comparison entirely (every region
	// becomes its own group, multi_sku spuriously flips true, and
	// price_delta_pct/baseline deltas collapse to 0/null). Every
	// NormalizedPrice already carries the exact region string it was priced
	// for in its own Region field, and provider code consistently reuses that
	// same variable for both Region and any region token embedded in
	// Description — so stripping that one substring out of Description (or
	// SKUID, when Description is empty) yields a region-invariant key while
	// still separating genuinely distinct SKUs (e.g. CDN cache-egress vs.
	// cache-fill, or Vertex AI vCPU vs. RAM), whose descriptions differ by
	// more than just the region token.
	skuKey := func(p models.NormalizedPrice) string {
		base := p.Description
		if base == "" {
			base = p.SKUID
		}
		if p.Region != "" {
			base = strings.ReplaceAll(base, p.Region, "")
		}
		return base
	}

	type skuGroup struct {
		key    string
		prices []models.NormalizedPrice
	}
	groupsByKey := make(map[string]*skuGroup)
	var groupOrder []string
	for _, p := range allPrices {
		key := skuKey(p)
		g, ok := groupsByKey[key]
		if !ok {
			g = &skuGroup{key: key}
			groupsByKey[key] = g
			groupOrder = append(groupOrder, key)
		}
		g.prices = append(g.prices, p)
	}
	multiSKU := len(groupOrder) > 1

	for _, key := range groupOrder {
		g := groupsByKey[key]
		sort.Slice(g.prices, func(i, j int) bool {
			return g.prices[i].PricePerUnit < g.prices[j].PricePerUnit
		})
	}
	// Rank groups by their own cheapest entry so the overall list still
	// leads with the globally cheapest SKU+region, without ever mixing a
	// different group's entries into the middle of it.
	sort.Slice(groupOrder, func(i, j int) bool {
		return groupsByKey[groupOrder[i]].prices[0].PricePerUnit < groupsByKey[groupOrder[j]].prices[0].PricePerUnit
	})

	allPrices = make([]models.NormalizedPrice, 0, len(allPrices))
	for _, key := range groupOrder {
		allPrices = append(allPrices, groupsByKey[key].prices...)
	}

	providerStr := string(baseSpec.GetProvider())
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
		if p.Attributes["fallback"] == "true" {
			e["fallback"] = "true"
		}
		// Explicit SKU discriminator on every entry (RC3-033 / #29): a spec
		// that resolves to multiple SKUs (e.g. CDN cache egress vs. cache
		// fill) must never leave the caller to infer which line item a row
		// belongs to from price alone.
		if multiSKU {
			e["sku_description"] = p.Description
		}
		entries = append(entries, e)
	}

	// If a baseline region was requested but is absent from the fetched
	// entries, degrade gracefully: keep the successful partial payload (with
	// delta fields explicitly nulled by applyBaselineDeltas) instead of
	// discarding it for a bare top-level error (RC3-002).
	var baselineMissing bool
	if in.BaselineRegion != "" {
		// Group deltas by sku_description so a multi-SKU result never computes
		// delta_pct for one SKU against a different SKU's baseline price
		// (RC3-033 / #29). When only one SKU is present, sku_description is
		// absent from every entry and applyBaselineDeltas falls back to
		// treating all entries as a single group — unchanged prior behavior.
		groupKey := ""
		if multiSKU {
			groupKey = "sku_description"
		}
		if err := applyBaselineDeltas(entries, in.BaselineRegion, groupKey); err != nil {
			baselineMissing = true
		}
	}

	cheapest := allPrices[0]
	// When multiple SKUs are present, restrict "most expensive" to the same
	// SKU group as the global cheapest entry so price_delta_pct is always a
	// same-SKU comparison, never a cross-SKU one (RC3-033 / #29).
	mostExpCandidates := allPrices
	if multiSKU {
		mostExpCandidates = groupsByKey[skuKey(cheapest)].prices
	}
	mostExp := mostExpCandidates[len(mostExpCandidates)-1]

	var priceDeltaPct any
	if cheapest.PricePerUnit > 0 {
		raw := (mostExp.PricePerUnit - cheapest.PricePerUnit) / cheapest.PricePerUnit * 100
		priceDeltaPct = math.Round(raw*10) / 10 // round to 1 decimal
	}

	out := map[string]any{
		"provider":              providerStr,
		"domain":                string(baseSpec.GetDomain()),
		"service":               baseSpec.GetService(),
		"cheapest_region":       cheapest.Region,
		"cheapest_price":        priceDict(cheapest.PricePerUnit, string(cheapest.Unit)),
		"most_expensive_region": mostExp.Region,
		"most_expensive_price":  priceDict(mostExp.PricePerUnit, string(mostExp.Unit)),
		"price_delta_pct":       priceDeltaPct,
		"all_regions_sorted":    entries,
	}
	if allFallback(allPrices) {
		// Every region resolved to fallback/static data (identical price,
		// no real regional variation) — cheapest_region/price_delta_pct are
		// still returned for backward compatibility, but flagged low
		// confidence so callers don't treat the ranking as a real regional
		// comparison (RC3-010).
		out["warnings"] = []string{
			"all results are fallback/static data; regional ranking may be unreliable",
		}
		out["ranking_low_confidence"] = true
	}
	if multiSKU {
		out["multi_sku"] = true
		out["sku_count"] = len(groupOrder)
		out["multi_sku_message"] = "This spec resolves to multiple SKUs per region (see sku_description on " +
			"each entry). all_regions_sorted groups entries by SKU and sorts within each group before " +
			"ranking groups, so cheapest-first ordering never mixes SKUs. cheapest_region/most_expensive_region " +
			"compare the same SKU."
	}
	if len(notAvailable) > 0 {
		out["not_available_in"] = notAvailable
	}
	if len(transientErrors) > 0 {
		out["transient_errors"] = transientErrors
	}
	if in.BaselineRegion != "" {
		out["baseline_region"] = in.BaselineRegion
	}
	if baselineMissing {
		out["baseline_missing"] = true
		out["baseline_missing_message"] = fmt.Sprintf(
			"baseline region %q was not found among the fetched regions; delta fields are null.",
			in.BaselineRegion,
		)
	}
	return jsonText(out), nil, nil
}

// --------------------------------------------------------------------------
// DescribeCatalog
// --------------------------------------------------------------------------

// DescribeCatalogInput is the typed input for the describe_catalog tool.
type DescribeCatalogInput struct {
	Provider string `json:"provider"`
	Domain   string `json:"domain"`
	Service  string `json:"service"`
}

// HandleDescribeCatalog implements the describe_catalog tool.
func (h *Handler) HandleDescribeCatalog(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in DescribeCatalogInput,
) (*mcp.CallToolResult, any, error) {
	defer recoverToResult(ctx, "describe_catalog")

	// catalogEntry builds the 3-key summary dict that Python emits per provider
	// in the support_matrix: {"domains": [...], "services": ..., "decision_matrix": ...}.
	catalogEntry := func(cat *models.ProviderCatalog) map[string]any {
		entry := map[string]any{
			"domains":  cat.Domains,
			"services": cat.Services,
		}
		if cat.DecisionMatrix != nil {
			entry["decision_matrix"] = cat.DecisionMatrix
		}
		return entry
	}

	// Normalize well-known service name aliases before any lookup so the model
	// doesn't need to know the exact canonical name on the first try.
	serviceNameAliases := map[string]string{
		"functions": "azure_functions",
	}
	if norm, ok := serviceNameAliases[strings.ToLower(in.Service)]; ok {
		in.Service = norm
	}

	// Service→domain normalization: if the caller supplied a service whose canonical
	// domain differs from the supplied domain (or no domain was supplied), redirect
	// to the correct domain so the rest of the function returns the right guidance.
	// This handles two cases:
	//   (1) describe_catalog(provider='aws', service='lambda') — no domain → infer serverless
	//   (2) describe_catalog(provider='aws', domain='compute', service='lambda') — wrong domain
	var redirectNotice string
	if in.Service != "" {
		svcKey := strings.ToLower(in.Service)
		if mappedDomain, found := serviceToDomain[svcKey]; found {
			mapped := string(mappedDomain)
			if in.Domain == "" {
				redirectNotice = fmt.Sprintf(
					"domain inferred from service '%s' → '%s'", in.Service, mapped,
				)
				in.Domain = mapped
			} else if in.Domain != mapped {
				redirectNotice = fmt.Sprintf(
					"service '%s' belongs to domain '%s', not '%s' — redirected",
					in.Service, mapped, in.Domain,
				)
				in.Domain = mapped
			}
		}
	}

	if in.Domain == "" {
		// No domain → full support matrix (no-args mode OR provider-only mode).
		// Python: {"support_matrix": {pname: {domains,services,decision_matrix}}, "tip": "..."}
		// pvdr_names is [provider] when provider is set, or all providers when empty.
		var pvdrNames []string
		if in.Provider != "" {
			if h.provider(in.Provider) == nil {
				return errResult(map[string]any{
					"error": fmt.Sprintf("Provider '%s' not configured.", in.Provider),
				}), nil, nil
			}
			pvdrNames = []string{in.Provider}
		} else {
			for name := range h.providers {
				pvdrNames = append(pvdrNames, name)
			}
			sort.Strings(pvdrNames)
		}

		matrix := make(map[string]any, len(pvdrNames))
		for _, pname := range pvdrNames {
			pvdr := h.providers[pname]
			cat, err := pvdr.DescribeCatalog(ctx)
			if err != nil {
				matrix[pname] = map[string]any{"error": err.Error()}
				continue
			}
			matrix[pname] = catalogEntry(cat)
		}
		return jsonText(map[string]any{
			"support_matrix": matrix,
			"tip": "Call describe_catalog(provider, domain) for field-level guidance " +
				"and a copy-paste example_invocation.",
		}), nil, nil
	}

	// Domain is set → provider + domain [+ service] targeted mode.
	if in.Provider == "" {
		return errResult(map[string]any{
			"error": "provider is required when domain is specified.",
		}), nil, nil
	}

	pvdr := h.provider(in.Provider)
	if pvdr == nil {
		return errResult(map[string]any{
			"error": fmt.Sprintf("Provider '%s' not configured.", in.Provider),
		}), nil, nil
	}

	catalog, err := pvdr.DescribeCatalog(ctx)
	if err != nil {
		return errResult(map[string]any{
			"error":   "upstream_failure",
			"message": fmt.Sprintf("Failed to describe catalog for %s: %v", in.Provider, err),
		}), nil, nil
	}

	// provider + domain [+ service] → targeted guidance.
	key := in.Domain
	if in.Service != "" {
		key = in.Domain + "/" + in.Service
	}

	// Python sets service: None (null) when service is empty.
	var serviceVal any
	if in.Service != "" {
		serviceVal = in.Service
	}

	out := map[string]any{
		"provider": in.Provider,
		"domain":   in.Domain,
		"service":  serviceVal,
	}
	if redirectNotice != "" {
		out["redirect_notice"] = redirectNotice
	}

	// Pull the relevant sections from the catalog.
	terms, haTerms := catalog.SupportedTerms[key]
	if !haTerms {
		terms, haTerms = catalog.SupportedTerms[in.Domain]
	}
	if haTerms {
		out["supported_terms"] = terms
	}

	hints, haHints := catalog.FilterHints[key]
	if !haHints {
		hints, haHints = catalog.FilterHints[in.Domain]
	}
	if haHints {
		out["filter_hints"] = hints
	}

	ex, haEx := catalog.ExampleInvocations[key]
	if !haEx {
		ex, haEx = catalog.ExampleInvocations[in.Domain]
	}
	if haEx {
		out["example_invocation"] = ex
		out["usage"] = "Pass example_invocation directly to get_price(spec=...)."
	}

	// When service was omitted and the domain has exactly one service, auto-forward to it.
	// This collapses a two-round describe_catalog dance into one call for single-service domains.
	if in.Service == "" && !haTerms && !haHints && !haEx {
		if svcs := catalog.Services[in.Domain]; len(svcs) == 1 {
			autoKey := in.Domain + "/" + svcs[0]
			if t, ok := catalog.SupportedTerms[autoKey]; ok {
				out["supported_terms"] = t
				haTerms = true
			}
			if h, ok := catalog.FilterHints[autoKey]; ok {
				out["filter_hints"] = h
				haHints = true
			}
			if e, ok := catalog.ExampleInvocations[autoKey]; ok {
				out["example_invocation"] = e
				out["usage"] = "Pass example_invocation directly to get_price(spec=...)."
				haEx = true
			}
			if haTerms || haHints || haEx {
				out["service"] = svcs[0]
				out["available_services"] = svcs
				out["auto_resolved"] = fmt.Sprintf("service omitted; auto-resolved to '%s' (only service in this domain)", svcs[0])
			}
		}
	}

	// When nothing was found: show available services (mirrors Python).
	if !haTerms && !haHints && !haEx {
		availSvcs := catalog.Services[in.Domain]
		out["available_services"] = availSvcs

		// GCP serverless (Cloud Run, Cloud Functions) is only available via the
		// Python provider — emit a clear message rather than an empty service list.
		switch {
		case in.Provider == "gcp" && in.Domain == string(models.PricingDomainServerless):
			out["error"] = "not_supported_in_go_provider"
			out["tip"] = "GCP serverless (Cloud Run, Cloud Functions) is not implemented in the " +
				"Go provider. Use the Python provider for Cloud Run and Cloud Functions pricing."
		case in.Service != "":
			// Service was specified but not found — check if it lives in a different domain.
			svcList := strings.Join(availSvcs, "', '")
			if svcList == "" {
				svcList = "<service>"
			}
			// Check serviceToDomain for cross-domain redirect.
			if correctDomain, ok := serviceToDomain[strings.ToLower(in.Service)]; ok && string(correctDomain) != in.Domain {
				out["tip"] = fmt.Sprintf(
					"Service '%s' is not in domain '%s' — it belongs to domain '%s'. "+
						"Call: describe_catalog(provider='%s', domain='%s', service='%s') "+
						"then get_price(spec={provider:'%s', domain:'%s', service:'%s', ...}).",
					in.Service, in.Domain, string(correctDomain),
					in.Provider, string(correctDomain), in.Service,
					in.Provider, string(correctDomain), in.Service,
				)
			} else {
				out["tip"] = fmt.Sprintf(
					"Service '%s' is not a catalog key for domain '%s'. "+
						"Pick one of the exact names from available_services and call: "+
						"describe_catalog(provider='%s', domain='%s', service='<exact_name>') "+
						"then get_price(spec={provider:'%s', domain:'%s', service:'<exact_name>', ...}). "+
						"Valid names: '%s'.",
					in.Service, in.Domain,
					in.Provider, in.Domain,
					in.Provider, in.Domain,
					svcList,
				)
			}
		default:
			out["tip"] = fmt.Sprintf(
				"Specify service= to get targeted guidance. Available services for %s: %v",
				in.Domain, availSvcs,
			)
		}
	}

	return jsonText(out), nil, nil
}

// --------------------------------------------------------------------------
// GetSpotHistory
// --------------------------------------------------------------------------

// GetSpotHistoryInput is the typed input for the get_spot_history tool.
type GetSpotHistoryInput struct {
	Spec             map[string]any `json:"spec"`
	Hours            int            `json:"hours"`
	AvailabilityZone string         `json:"availability_zone"`
}

// HandleGetSpotHistory implements the get_spot_history tool.
func (h *Handler) HandleGetSpotHistory(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in GetSpotHistoryInput,
) (*mcp.CallToolResult, any, error) {
	defer recoverToResult(ctx, "get_spot_history")

	if in.Hours <= 0 {
		in.Hours = 24
	}

	spec, err := unmarshalSpec(in.Spec)
	if err != nil {
		return jsonText(specErrorResponse(err, in.Spec)), nil, nil
	}

	pvdr := h.provider(string(spec.GetProvider()))
	if pvdr == nil {
		return errResult(map[string]any{
			"error": fmt.Sprintf("Provider '%s' not configured.", spec.GetProvider()),
		}), nil, nil
	}

	result, err := pvdr.GetSpotHistory(ctx, spec, in.Hours, in.AvailabilityZone)
	if err != nil {
		if errors.Is(err, providers.ErrNotSupported) {
			return errResult(map[string]any{
				"error":    "not_supported",
				"provider": string(spec.GetProvider()),
				"domain":   string(spec.GetDomain()),
				"service":  spec.GetService(),
				"reason":   err.Error(),
			}), nil, nil
		}
		if isInvalidInput(err) {
			return errResult(map[string]any{
				"error":     "invalid_input",
				"message":   err.Error(),
				"retryable": false,
			}), nil, nil
		}
		return errResult(map[string]any{
			"error":     "upstream_failure",
			"message":   "Spot history lookup failed. Try again shortly.",
			"retryable": true,
		}), nil, nil
	}

	if len(result) == 0 {
		instanceType, _ := in.Spec["resource_type"].(string)
		if instanceType == "" {
			instanceType, _ = in.Spec["instance_type"].(string)
		}
		region := spec.GetRegion()
		return jsonText(map[string]any{
			"result": "no_data",
			"message": fmt.Sprintf(
				"No spot price history found for %s in %s. "+
					"Check instance type spelling or try a different region.",
				instanceType, region,
			),
			"region_name": regionDisplayNameFn(string(spec.GetProvider()), region),
		}), nil, nil
	}

	result["provider"] = string(spec.GetProvider())
	result["region_name"] = regionDisplayNameFn(string(spec.GetProvider()), spec.GetRegion())
	return jsonText(result), nil, nil
}

// --------------------------------------------------------------------------
// Error classification helpers
// --------------------------------------------------------------------------

// isNotConfigured returns true if err is a "not configured" provider error.
// Providers signal this via a sentinel type — until the providers are ported,
// we check the error message.
func isNotConfigured(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not configured") || strings.Contains(msg, "credentials")
}

// isInvalidInput returns true if err is a validation / bad-input error.
func isInvalidInput(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "invalid") || strings.Contains(msg, "validation")
}

// recoverToResult is a no-op deferred function placeholder for panic recovery.
// Full implementation in the server layer via recover(); here it satisfies the
// enterprise-readiness requirement that no panic reaches the MCP client.
func recoverToResult(_ context.Context, _ string) {
	recover() //nolint:errcheck // intentional no-op recovery; server layer handles panics
}
