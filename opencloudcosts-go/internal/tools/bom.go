// bom.go implements the BoM and FinOps MCP tools:
//   - estimate_bom
//   - estimate_unit_economics
//   - get_discount_summary
//
// All monetary values use float64 arithmetic (not shopspring/decimal) per the
// Phase 0 plan decision. The output shape mirrors the Python implementation
// in src/opencloudcosts/tools/bom.py and src/opencloudcosts/tools/lookup.py.
package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers"
)

// --------------------------------------------------------------------------
// Input types (mirrors server/server.go input structs)
// --------------------------------------------------------------------------

// EstimateBOMInput is the typed input for the estimate_bom tool.
type EstimateBOMInput struct {
	Items []map[string]any `json:"items"`
}

// EstimateUnitEconomicsInput is the typed input for the estimate_unit_economics tool.
type EstimateUnitEconomicsInput struct {
	Items         []map[string]any `json:"items"`
	UnitsPerMonth float64          `json:"units_per_month"`
	UnitLabel     string           `json:"unit_label"`
}

// GetDiscountSummaryInput is the typed input for the get_discount_summary tool.
type GetDiscountSummaryInput struct {
	Provider string `json:"provider"`
}

// --------------------------------------------------------------------------
// Monthly cost calculation — mirrors BomLineItem.from_price() in models.py
// --------------------------------------------------------------------------

// bomMonthlyCost computes the monthly cost for a single line item.
// It mirrors the BomLineItem.from_price() method in Python:
//
//	PER_HOUR    → price * hours_per_month * quantity
//	PER_GB_MONTH → price * size_gb * quantity
//	PER_MONTH   → price * quantity
//	else        → price * quantity
func bomMonthlyCost(price models.NormalizedPrice, quantity float64, hoursPerMonth, sizeGB float64) float64 {
	switch price.Unit { //nolint:exhaustive // unlisted units (per_gb, per_request, etc.) use the default per-unit scaling
	case models.PriceUnitPerHour:
		return price.PricePerUnit * hoursPerMonth * quantity
	case models.PriceUnitPerGBMonth:
		return price.PricePerUnit * sizeGB * quantity
	case models.PriceUnitPerMonth:
		return price.PricePerUnit * quantity
	default:
		return price.PricePerUnit * quantity
	}
}

// --------------------------------------------------------------------------
// Description fallback — mirrors Python getattr chain in estimate_bom
// --------------------------------------------------------------------------

// descriptionFromSpec returns a fallback description from the spec.
// Mirrors the Python getattr chain:
//
//	resource_type → storage_type → model → service → domain
//
// Format: "{id} ({region})"
func descriptionFromSpec(spec models.PricingSpec) string {
	var resourceID string
	switch s := spec.(type) {
	case *models.ComputePricingSpec:
		if s.ResourceType != "" {
			resourceID = s.ResourceType
		}
	case *models.StoragePricingSpec:
		if s.StorageType != "" {
			resourceID = s.StorageType
		}
	case *models.DatabasePricingSpec:
		if s.ResourceType != "" {
			resourceID = s.ResourceType
		}
	case *models.AiPricingSpec:
		if s.Model != "" {
			resourceID = s.Model
		}
	}
	if resourceID == "" {
		if svc := spec.GetService(); svc != "" {
			resourceID = svc
		} else {
			resourceID = string(spec.GetDomain())
		}
	}
	return fmt.Sprintf("%s (%s)", resourceID, spec.GetRegion())
}

// --------------------------------------------------------------------------
// Thousands-separator formatter for volume display
// --------------------------------------------------------------------------

// commaInt formats an integer with comma separators (e.g. 10000 → "10,000").
// Go's fmt package has no built-in thousands separator.
func commaInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 {
		s = s[1:]
	}
	// Insert commas from right to left.
	var b strings.Builder
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	if n < 0 {
		return "-" + b.String()
	}
	return b.String()
}

// --------------------------------------------------------------------------
// bomLineItemResult builds the per-line-item map for estimate_bom output.
// --------------------------------------------------------------------------

type bomLineItem struct {
	description string
	provider    string
	service     string
	region      string
	quantity    float64
	unitPrice   models.NormalizedPrice
	monthlyCost float64
	annualCost  float64
}

func (li bomLineItem) toMap() map[string]any {
	m := map[string]any{
		"description": li.description,
		"provider":    li.provider,
		"service":     li.service,
		"region":      li.region,
		"quantity":       li.quantity,
		"price_per_unit": priceDict(li.unitPrice.PricePerUnit, string(li.unitPrice.Unit)),
		"monthly_cost":   moneyDict(li.monthlyCost, "/mo"),
	}

	// Surface the provider's fallback flag (mirrors compare_prices in
	// lookup.go, which encodes this as the string "true" rather than a
	// bool — matched here so callers can treat the "fallback" key
	// identically across tool outputs) so callers can tell a
	// static/published rate — which may not reflect the requested region
	// — from a live catalog lookup.
	if li.unitPrice.Attributes["fallback"] == "true" {
		m["fallback"] = "true"
		if note := li.unitPrice.Attributes["fallback_note"]; note != "" {
			m["fallback_note"] = note
		}
	}

	return m
}

// --------------------------------------------------------------------------
// processBOMItems is the shared item-processing loop for estimate_bom and
// estimate_unit_economics. Returns (lineItems, errors).
// --------------------------------------------------------------------------

func processBOMItems(
	ctx context.Context,
	provs map[string]Provider,
	items []map[string]any,
) ([]bomLineItem, []string) {
	var lineItems []bomLineItem
	var errs []string

	for idx, item := range items {
		label := fmt.Sprintf("Item %d", idx+1)

		// Extract BoM-only fields.
		quantity := 1.0
		if v, ok := item["quantity"]; ok {
			switch n := v.(type) {
			case float64:
				quantity = n
			case int:
				quantity = float64(n)
			case int64:
				quantity = float64(n)
			}
		}
		hoursPerMonth := 730.0
		if v, ok := item["hours_per_month"]; ok {
			if n, ok := v.(float64); ok {
				hoursPerMonth = n
			}
		}
		sizeGB := 100.0
		if v, ok := item["size_gb"]; ok {
			if n, ok := v.(float64); ok {
				sizeGB = n
			}
		}
		description, _ := item["description"].(string)

		// Build clean spec dict (remove BoM-only fields).
		specDict := make(map[string]any, len(item))
		for k, v := range item {
			if k == "quantity" || k == "hours_per_month" || k == "description" {
				continue
			}
			specDict[k] = v
		}

		// Infer domain.
		enriched := fillDomain(specDict)

		// Inject hours_per_month / size_gb into spec for relevant domains.
		if d, _ := enriched["domain"].(string); d == "compute" {
			if _, has := enriched["hours_per_month"]; !has {
				enriched["hours_per_month"] = hoursPerMonth
			}
		}
		if d, _ := enriched["domain"].(string); d == "storage" {
			if _, has := enriched["size_gb"]; !has && sizeGB > 0 {
				enriched["size_gb"] = sizeGB
			}
		}

		// Unmarshal to typed PricingSpec.
		parsed, err := unmarshalSpec(enriched)
		if err != nil {
			// parsed does not exist yet on this branch, so region info is
			// only available as a raw, unvalidated field on the input map
			// (best-effort — the spec itself failed validation).
			if rawRegion, ok := enriched["region"].(string); ok && rawRegion != "" {
				errs = append(errs, fmt.Sprintf("%s: invalid spec (region '%s') — %v", label, rawRegion, err))
			} else {
				errs = append(errs, fmt.Sprintf("%s: invalid spec — %v", label, err))
			}
			continue
		}

		// Look up provider.
		pvdrName := string(parsed.GetProvider())
		pvdr := provs[pvdrName]
		if pvdr == nil {
			errs = append(errs, fmt.Sprintf("%s: provider '%s' not configured for region '%s'",
				label, pvdrName, parsed.GetRegion()))
			continue
		}

		// Check support.
		if !pvdr.Supports(parsed.GetDomain(), parsed.GetService()) {
			errs = append(errs, fmt.Sprintf("%s: %s does not support %s/%s in region '%s'",
				label, pvdrName, parsed.GetDomain(), parsed.GetService(), parsed.GetRegion()))
			continue
		}

		// Fetch price.
		result, err := pvdr.GetPrice(ctx, parsed)
		if err != nil {
			if err == providers.ErrNotSupported {
				errs = append(errs, fmt.Sprintf("%s: %s does not support this spec in region '%s'",
					label, pvdrName, parsed.GetRegion()))
			} else {
				errs = append(errs, fmt.Sprintf("%s: %v (region '%s')", label, err, parsed.GetRegion()))
			}
			continue
		}
		if result == nil || len(result.PublicPrices) == 0 {
			errs = append(errs, fmt.Sprintf("%s: no pricing found for spec in region '%s'",
				label, parsed.GetRegion()))
			continue
		}

		// Build description if not provided.
		if description == "" {
			description = descriptionFromSpec(parsed)
		}

		// Extract provisioned IOPS count from StoragePricingSpec so that
		// per_iops_month prices are multiplied correctly.
		var iopsCount int
		if stoSpec, ok := parsed.(*models.StoragePricingSpec); ok && stoSpec.IOPS != nil {
			iopsCount = *stoSpec.IOPS
		}

		// Iterate all price components — providers such as AWS io2 return two
		// entries: one per_gb_month (storage) and one per_iops_month (IOPS).
		// Taking only PublicPrices[0] would drop the IOPS charge entirely.
		for _, price := range result.PublicPrices {
			var monthly float64
			switch price.Unit {
			case models.PriceUnitPerIOPSMonth:
				// Multiply by provisioned IOPS count, not by storage size.
				monthly = price.PricePerUnit * float64(iopsCount) * quantity
			default:
				monthly = bomMonthlyCost(price, quantity, hoursPerMonth, sizeGB)
			}
			annual := monthly * 12

			// Suffix the description for multi-component line items so the
			// model can present separate rows (e.g. "storage capacity" vs
			// "provisioned IOPS") as required by the PDISK prompt.
			lineDesc := description
			if len(result.PublicPrices) > 1 {
				lineDesc = fmt.Sprintf("%s — %s", description, price.Description)
			}

			lineItems = append(lineItems, bomLineItem{
				description: lineDesc,
				provider:    string(price.Provider),
				service:     price.Service,
				region:      price.Region,
				quantity:    quantity,
				unitPrice:   price,
				monthlyCost: monthly,
				annualCost:  annual,
			})
		}
	}

	return lineItems, errs
}

// --------------------------------------------------------------------------
// HandleEstimateBOM — estimate_bom tool handler
// --------------------------------------------------------------------------

// HandleEstimateBOM implements the estimate_bom MCP tool.
func (h *Handler) HandleEstimateBOM(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in EstimateBOMInput,
) (*mcp.CallToolResult, any, error) {
	lineItems, errs := processBOMItems(ctx, h.providers, in.Items)

	if len(lineItems) == 0 {
		msg := "No valid line items."
		if len(errs) > 0 {
			msg += " Errors: " + strings.Join(errs, "; ")
		}
		return jsonText(map[string]any{"error": msg}), nil, nil
	}

	// Compute totals.
	totalMonthly := 0.0
	for _, li := range lineItems {
		totalMonthly += li.monthlyCost
	}
	totalAnnual := totalMonthly * 12

	// Gather advisory rows from each provider in the BoM.
	servicesSet := make(map[string]struct{})
	providersInBoM := make(map[string]bool)
	providerFirstRegion := make(map[string]string)
	for _, li := range lineItems {
		servicesSet[li.service] = struct{}{}
		if !providersInBoM[li.provider] {
			providersInBoM[li.provider] = true
			providerFirstRegion[li.provider] = li.region
		}
	}
	services := make([]string, 0, len(servicesSet))
	for s := range servicesSet {
		services = append(services, s)
	}

	var notIncluded []map[string]string
	for pname := range providersInBoM {
		pvdr := h.providers[pname]
		if pvdr == nil {
			continue
		}
		sampleRegion := providerFirstRegion[pname]
		if sampleRegion == "" {
			sampleRegion = pvdr.DefaultRegion()
		}
		rows, err := pvdr.BOMAdvisories(ctx, services, sampleRegion)
		if err == nil {
			notIncluded = append(notIncluded, rows...)
		}
	}

	// Build line items output.
	lineItemMaps := make([]map[string]any, len(lineItems))
	for i, li := range lineItems {
		lineItemMaps[i] = li.toMap()
	}

	// Build response.
	resp := map[string]any{
		"line_items": lineItemMaps,
		"totals": map[string]any{
			"monthly": moneyDict(totalMonthly, "/mo"),
			"annual":  moneyDict(totalAnnual, "/yr"),
		},
		"not_included":        nil,
		"not_included_action": nil,
		"errors":              nil,
	}

	if len(notIncluded) > 0 {
		resp["not_included"] = notIncluded
		resp["not_included_action"] = "SUPPLEMENTARY: these items are excluded from the baseline estimate above. " +
			"Include them only if the user explicitly asked for comprehensive or total-cost-of-ownership pricing. " +
			"For core infrastructure questions, you may note 'additional costs may apply' and list the items " +
			"without making further tool calls. When the user did ask, price each item using the get_price " +
			"command in its how_to_price field where one is provided, rather than estimating."
	}

	if len(errs) > 0 {
		resp["errors"] = errs
	}

	return jsonText(resp), nil, nil
}

// --------------------------------------------------------------------------
// HandleEstimateUnitEconomics — estimate_unit_economics tool handler
// --------------------------------------------------------------------------

// HandleEstimateUnitEconomics implements the estimate_unit_economics MCP tool.
func (h *Handler) HandleEstimateUnitEconomics(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in EstimateUnitEconomicsInput,
) (*mcp.CallToolResult, any, error) {
	unitLabel := in.UnitLabel
	if unitLabel == "" {
		unitLabel = "user"
	}

	lineItems, errs := processBOMItems(ctx, h.providers, in.Items)

	if len(lineItems) == 0 {
		msg := "No valid items."
		if len(errs) > 0 {
			msg += " Errors: " + strings.Join(errs, "; ")
		}
		return jsonText(map[string]any{"error": msg}), nil, nil
	}

	// Compute totals.
	totalMonthly := 0.0
	for _, li := range lineItems {
		totalMonthly += li.monthlyCost
	}
	totalAnnual := totalMonthly * 12

	// Cost per unit.
	var costPerUnit float64
	if in.UnitsPerMonth > 0 {
		costPerUnit = totalMonthly / in.UnitsPerMonth
	}

	// Sample region from first line item.
	sampleRegion := "us-east-1"
	if len(lineItems) > 0 {
		sampleRegion = lineItems[0].region
	}

	// Volume string: Python uses {:,.0f} which gives comma-grouped integer.
	volumeStr := fmt.Sprintf("%s %ss/month", commaInt(int64(in.UnitsPerMonth)), unitLabel)

	resp := map[string]any{
		"pricing_region":         sampleRegion,
		"infrastructure_monthly": moneyDict(totalMonthly, ""),
		"infrastructure_annual":  moneyDict(totalAnnual, ""),
		"volume":                 volumeStr,
		"cost_per_unit":          moneyDict(costPerUnit, fmt.Sprintf(" per %s", unitLabel)),
		"cost_per_unit_annual":   moneyDict(costPerUnit*12, fmt.Sprintf(" per %s/year", unitLabel)),
		"errors":                 nil,
		"important": fmt.Sprintf(
			"Prices are for %s — always state the region in your answer "+
				"as unit economics vary significantly by region.",
			sampleRegion,
		),
	}

	if len(errs) > 0 {
		resp["errors"] = errs
	}

	return jsonText(resp), nil, nil
}

// --------------------------------------------------------------------------
// HandleGetDiscountSummary — get_discount_summary tool handler
// --------------------------------------------------------------------------

// HandleGetDiscountSummary implements the get_discount_summary MCP tool.
// For providers that return ErrNotSupported (e.g. GCP), it returns a structured
// not_supported response matching the Python NotSupportedError.to_response() shape.
func (h *Handler) HandleGetDiscountSummary(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in GetDiscountSummaryInput,
) (*mcp.CallToolResult, any, error) {
	providerName := in.Provider
	if providerName == "" {
		providerName = "aws"
	}

	pvdr := h.providers[providerName]
	if pvdr == nil {
		return jsonText(map[string]any{
			"error": fmt.Sprintf("Provider '%s' not configured.", providerName),
		}), nil, nil
	}

	result, err := pvdr.GetDiscountSummary(ctx)
	if err != nil {
		if err == providers.ErrNotSupported {
			// Mirror Python NotSupportedError.to_response() for not_supported cases.
			return jsonText(map[string]any{
				"error":        "not_supported",
				"provider":     providerName,
				"domain":       "compute",
				"service":      nil,
				"reason":       fmt.Sprintf("%s does not support discount summaries.", providerName),
				"alternatives": []string{"Use get_price with the relevant term (reserved_1yr, cud_1yr, etc.)"},
			}), nil, nil
		}
		return jsonText(map[string]any{
			"error":     "upstream_failure",
			"message":   "Discount summary lookup failed. Try again shortly.",
			"retryable": true,
		}), nil, nil
	}

	return jsonText(result), nil, nil
}
