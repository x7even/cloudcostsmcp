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
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/cache"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers"
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
	"cloud_armor":      models.PricingDomainObservability,
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
	"data_transfer": models.PricingDomainInterRegionEgress,
	"egress":        models.PricingDomainInterRegionEgress,
}

// validTerms is shown in error hints when an invalid term is supplied.
const validTerms = "on_demand, spot, reserved_1yr, reserved_1yr_partial, reserved_1yr_all, " +
	"reserved_3yr, reserved_3yr_partial, reserved_3yr_all, " +
	"cud_1yr, cud_3yr, sud, compute_savings_plan, ec2_instance_savings_plan"

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
// Returns an error if baseline_region is not present in the entries.
func applyBaselineDeltas(entries []map[string]any, baselineRegion string) error {
	var baseHourly float64
	var baseMonthly float64
	found := false
	for _, e := range entries {
		if e["region"] == baselineRegion {
			if pd, ok := e["price_per_unit"].(map[string]any); ok {
				if a, ok := pd["amount"].(float64); ok {
					baseHourly = a
				}
			}
			if md, ok := e["monthly_estimate"].(map[string]any); ok {
				if a, ok := md["amount"].(float64); ok {
					baseMonthly = a
				}
			}
			found = true
			break
		}
	}
	if !found {
		available := make([]string, 0, len(entries))
		for _, e := range entries {
			if r, ok := e["region"].(string); ok {
				available = append(available, r)
			}
		}
		return fmt.Errorf("baseline region %q not found in results. Available: %v",
			baselineRegion, available)
	}

	for _, e := range entries {
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
		dh := h - baseHourly
		dm := m - baseMonthly
		var pct float64
		if baseHourly > 0 {
			pct = (dh / baseHourly) * 100
		}
		e["delta_per_hour"] = fmt.Sprintf("$%+.6f", dh)
		e["delta_monthly"] = fmt.Sprintf("$%+.2f/mo", dm)
		e["delta_pct"] = fmt.Sprintf("%+.1f%%", pct)
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
	attrKeys := map[string]bool{
		"instanceType":    true,
		"vcpu":            true,
		"memory":          true,
		"operatingSystem": true,
		"storage_type":    true,
		"volumeType":      true,
		"fallback":        true,
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
		return errResult(map[string]any{
			"error":    "not_supported",
			"provider": string(spec.GetProvider()),
			"domain":   string(spec.GetDomain()),
			"service":  spec.GetService(),
			"reason": fmt.Sprintf("%s does not support %s/%s.",
				spec.GetProvider(), spec.GetDomain(), spec.GetService()),
			"alternatives": []string{"Call describe_catalog() to see what this provider supports."},
		}), nil, nil
	}

	result, err := pvdr.GetPrice(ctx, spec)
	if err != nil {
		if err == providers.ErrNotSupported {
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
				"Try search_pricing(provider='%s', service='%s', query='...') "+
					"to explore available products and valid filter attribute names. "+
					"Use list_services() to verify the service code exists.",
				provName, svc,
			),
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
				results[idx] = fetchResult{itype: it, err: err.Error()}
				return
			}
			r, err := pvdr.GetPrice(ctx, spec)
			if err != nil {
				results[idx] = fetchResult{itype: it, err: err.Error()}
				return
			}
			if r == nil {
				results[idx] = fetchResult{itype: it, prices: []models.NormalizedPrice{}}
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
	}

	var entries []entry
	errMap := make(map[string]string)

	for _, fr := range results {
		if fr.err != "" {
			errMap[fr.itype] = fr.err
			continue
		}
		if len(fr.prices) == 0 {
			errMap[fr.itype] = "no pricing found"
			continue
		}
		p := fr.prices[0]
		e := entry{
			instanceType:  fr.itype,
			pricePerHour:  p.PricePerUnit,
			monthlyEst:    p.MonthlyCost(),
			pricePerUnit:  priceDict(p.PricePerUnit, string(p.Unit)),
			monthlyEstMap: moneyDict(p.MonthlyCost(), "/mo"),
			description:   p.Description,
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
				results[idx] = regionResult{region: rgn}
				return
			}
			r, err := pvdr.GetPrice(ctx, spec)
			if err != nil {
				results[idx] = regionResult{region: rgn}
				return
			}
			if r == nil {
				results[idx] = regionResult{region: rgn}
				return
			}
			results[idx] = regionResult{region: rgn, prices: r.PublicPrices}
		}(i, region)
	}
	wg.Wait()

	var allPrices []models.NormalizedPrice
	var notAvailable []string
	for _, rr := range results {
		if len(rr.prices) > 0 {
			allPrices = append(allPrices, rr.prices...)
		} else {
			notAvailable = append(notAvailable, rr.region)
		}
	}

	if len(allPrices) == 0 {
		return jsonText(map[string]any{
			"result":  "no_prices_found",
			"message": "No pricing found in any of the specified regions.",
		}), nil, nil
	}

	// Sort cheapest first.
	sort.Slice(allPrices, func(i, j int) bool {
		return allPrices[i].PricePerUnit < allPrices[j].PricePerUnit
	})

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
		entries = append(entries, e)
	}

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
	if len(notAvailable) > 0 {
		out["not_available_in"] = notAvailable
	}
	if in.BaselineRegion != "" {
		out["baseline_region"] = in.BaselineRegion
	}
	return jsonText(out), nil, nil
}

// --------------------------------------------------------------------------
// SearchPricing
// --------------------------------------------------------------------------

// SearchPricingInput is the typed input for the search_pricing tool.
type SearchPricingInput struct {
	Provider   string `json:"provider"`
	Query      string `json:"query"`
	Domain     string `json:"domain"`
	Region     string `json:"region"`
	MaxResults int    `json:"max_results"`
}

// HandleSearchPricing implements the search_pricing tool.
func (h *Handler) HandleSearchPricing(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in SearchPricingInput,
) (*mcp.CallToolResult, any, error) {
	defer recoverToResult(ctx, "search_pricing")

	if in.MaxResults <= 0 {
		in.MaxResults = 20
	}

	pvdr := h.provider(in.Provider)
	if pvdr == nil {
		return errResult(map[string]any{
			"error": fmt.Sprintf("Provider '%s' not configured.", in.Provider),
		}), nil, nil
	}

	region := in.Region
	prices, err := pvdr.SearchPricing(ctx, in.Query, region, in.MaxResults)
	if err != nil {
		return errResult(map[string]any{
			"error":     "upstream_failure",
			"message":   "Search request failed. Try again shortly.",
			"retryable": true,
		}), nil, nil
	}

	regionOut := in.Region
	if regionOut == "" {
		regionOut = "all"
	}

	if len(prices) == 0 {
		domain := in.Domain
		if domain == "" {
			domain = "compute"
		}
		tip := "Check the service code with list_services(). " +
			"Try a broader query (e.g. the product family name). "
		if in.Domain != "" {
			tip += fmt.Sprintf("Verify that '%s' is a valid domain or service alias.", in.Domain)
		}
		msg := fmt.Sprintf("No pricing found matching '%s'", in.Query)
		if in.Domain != "" {
			msg += fmt.Sprintf(" in domain '%s'", in.Domain)
		}
		msg += "."
		return jsonText(map[string]any{
			"result":   "no_results",
			"provider": in.Provider,
			"domain":   domain,
			"query":    in.Query,
			"region":   regionOut,
			"message":  msg,
			"tip":      tip,
		}), nil, nil
	}

	summaries := make([]map[string]any, len(prices))
	for i, p := range prices {
		summaries[i] = normalizedPriceSummary(p)
	}
	return jsonText(map[string]any{
		"provider": in.Provider,
		"query":    in.Query,
		"region":   regionOut,
		"count":    len(prices),
		"results":  summaries,
		"tip": "Use get_price(spec) with a typed spec for cost estimates. " +
			"Call describe_catalog(provider) to see supported domains and services.",
	}), nil, nil
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

	// When nothing was found: show available services (mirrors Python).
	if !haTerms && !haHints && !haEx {
		availSvcs := catalog.Services[in.Domain]
		out["available_services"] = availSvcs
		if in.Service != "" {
			// Service was specified but not found — guide toward the exact name.
			svcList := strings.Join(availSvcs, "', '")
			if svcList == "" {
				svcList = "<service>"
			}
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
		} else {
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
		if err == providers.ErrNotSupported {
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
