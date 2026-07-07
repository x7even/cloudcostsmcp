// bom.go implements the BoM and FinOps MCP tools:
//   - estimate_bom
//   - estimate_unit_economics
//   - get_discount_summary
//
// All monetary values use float64 arithmetic (not shopspring/decimal) per the
// Phase 0 plan decision. The output shape mirrors the Python implementation
// in src/opencloudcosts/tools/bom.py and src/opencloudcosts/tools/lookup.py.
//
// processBOMItems additionally resolves raw-SKU line items (issue #31,
// RC3-004; GCP parity RC3-015) via resolveBOMSKUItem, which resolves the
// concrete provider through resolveSKULookupProviderFromMap and calls
// LookupSKUAcrossRegionsGeneric — the same provider-agnostic core
// get_price_by_sku uses (internal/tools/sku_lookup.go). That
// skulookup/provider-specific plumbing is isolated to the resolveBOMSKUItem
// call site for the same reason sku_lookup.go isolates it: the rest of this
// file stays provider-agnostic.
package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/skulookup"
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

// gcpTieredUsageVolume returns the usage volume that a tiered GCP price's
// tier_start_usage thresholds are denominated in, matching bomMonthlyCost's
// own per-unit scaling exactly (PER_HOUR: hoursPerMonth*quantity,
// PER_GB_MONTH: sizeGB*quantity, else: quantity) — tier thresholds must be
// compared against this same scaled volume, not raw quantity, since that is
// what the thresholds mean on the underlying GCP catalog (e.g. Firestore
// storage tiers are denominated in GB/month, not in the BoM item's
// "quantity" of database instances).
func gcpTieredUsageVolume(unit models.PriceUnit, quantity, hoursPerMonth, sizeGB float64) float64 {
	switch unit { //nolint:exhaustive // mirrors bomMonthlyCost's own switch
	case models.PriceUnitPerHour:
		return hoursPerMonth * quantity
	case models.PriceUnitPerGBMonth:
		return sizeGB * quantity
	default:
		return quantity
	}
}

// tierStartUsage parses a tier row's tier_start_usage attribute (set by every
// GCP tiered-price row — see gcp_sku_lookup.go's gcpBuildMatchedPrices). ok
// is false when the attribute is absent or unparseable, so callers can
// gracefully skip a malformed tier rather than mis-bracket the whole
// calculation around it.
func tierStartUsage(t models.NormalizedPrice) (start float64, ok bool) {
	startStr, present := t.Attributes["tier_start_usage"]
	if !present {
		return 0, false
	}
	start, err := strconv.ParseFloat(startStr, 64)
	return start, err == nil
}

// gcpGraduatedTieredCost computes the total monthly cost for a tiered
// (graduated) GCP price across every bracket usageVolume actually spans,
// mirroring the established graduated-billing model this codebase already
// uses elsewhere (gcp_networking.go's computeTieredCost, gcp_dns.go's
// addTieredEstimate): each tier's rate applies only to the portion of
// usageVolume that falls within that tier's own [start, nextStart) bracket,
// not to the entire usageVolume at one flat rate. tiers must be sorted
// ascending by tier_start_usage (LookupSKUAcrossRegionsGeneric's rr.Prices
// already are). Returns the total cost and the marginal (highest-bracket-
// reached) tier row, used only for the line item's displayed
// "price_per_unit" — a graduated rate has no single applicable per-unit
// price, so the marginal rate (the rate on the last unit of usage) is
// reported, the same convention computeTieredCost's own BlendedRatePerQty
// stands in for at its call sites.
func gcpGraduatedTieredCost(tiers []models.NormalizedPrice, usageVolume float64) (monthly float64, marginal models.NormalizedPrice) {
	marginal = tiers[0]
	for i, t := range tiers {
		start, ok := tierStartUsage(t)
		if !ok {
			continue
		}
		if start > usageVolume {
			// Tiers are ascending, so this and every later tier's bracket
			// starts beyond usageVolume — neither is reached.
			break
		}
		marginal = t
		upper := math.Inf(1)
		if i+1 < len(tiers) {
			if nextStart, ok2 := tierStartUsage(tiers[i+1]); ok2 {
				upper = nextStart
			}
		}
		bracketEnd := math.Min(usageVolume, upper)
		if bracketEnd > start {
			monthly += (bracketEnd - start) * t.PricePerUnit
		}
	}
	return monthly, marginal
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
	// sku is set only for line items resolved from a raw-SKU BoM entry
	// (see resolveBOMSKUItem); empty for PricingSpec-dict items.
	sku string
}

func (li bomLineItem) toMap() map[string]any {
	m := map[string]any{
		"description":    li.description,
		"provider":       li.provider,
		"service":        li.service,
		"region":         li.region,
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

	if li.sku != "" {
		m["sku"] = li.sku
	}

	return m
}

// --------------------------------------------------------------------------
// Raw-SKU BoM item helpers — shared by processBOMItems (this file) and
// HandleCompareBOMRegions's partition loop (compare_bom_regions.go).
// --------------------------------------------------------------------------

// rawBOMSKU extracts and trims a raw-SKU BoM item's "sku" field, reporting
// whether one was present (a whitespace-only value does not count). Shared
// by processBOMItems and HandleCompareBOMRegions's partition loop
// (compare_bom_regions.go) so both treat "is this a raw-SKU item" — and the
// exact string handed to the resolved (AWS or GCP) SKU lookup provider —
// identically.
func rawBOMSKU(item map[string]any) (string, bool) {
	sku, _ := item["sku"].(string)
	sku = strings.TrimSpace(sku)
	return sku, sku != ""
}

// stringItemField extracts item[key] as a string, distinguishing "absent"
// (returns "", "") from "present but not a string" (returns "", a non-empty
// error message) — a raw-SKU item with e.g. operation as a number/array
// should surface a clear type error rather than silently being treated as
// "field not supplied," which would otherwise produce a misleading
// disambiguation-hint error later.
func stringItemField(item map[string]any, key, label, sku string) (string, string) {
	v, present := item[key]
	if !present {
		return "", ""
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Sprintf("%s: %q must be a string (sku %q)", label, key, sku)
	}
	return s, ""
}

// awsServiceCodeToAdvisoryToken maps a raw AWS Pricing API servicecode (as
// stored on raw-SKU line items' li.service — see resolveBOMSKUItem) to the
// short-form category token BOMAdvisories' svcSet already recognizes for
// PricingSpec-dict line items, so advisory rows (egress, LB, NAT, RDS
// backups, EBS snapshots) aren't silently skipped just because a BoM used
// raw-SKU items instead of PricingSpec dicts for the same AWS service.
var awsServiceCodeToAdvisoryToken = map[string]string{
	"amazonec2":         "ec2",
	"amazonrds":         "rds",
	"amazonelasticache": "elasticache",
	"amazons3":          "s3",
	"amazonebs":         "ebs",
}

// bomAdvisoryServiceToken normalizes a bomLineItem.service value for the
// BOMAdvisories lookup — see awsServiceCodeToAdvisoryToken.
func bomAdvisoryServiceToken(service string) string {
	if tok, ok := awsServiceCodeToAdvisoryToken[strings.ToLower(service)]; ok {
		return tok
	}
	return service
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

		// Raw-SKU items (issue #31, RC3-004; GCP parity RC3-015) bypass the
		// PricingSpec path entirely — they carry a raw provider-native
		// SKU/usage-type string instead of a domain/resource_type spec, so
		// resolve them via the same provider-agnostic SKU lookup
		// get_price_by_sku uses.
		if sku, ok := rawBOMSKU(item); ok {
			li, errMsg := resolveBOMSKUItem(ctx, provs, label, item, sku, quantity, hoursPerMonth, sizeGB, description)
			if errMsg != "" {
				errs = append(errs, errMsg)
				continue
			}
			lineItems = append(lineItems, li)
			continue
		}

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
// resolveBOMSKUItem resolves a raw-SKU BoM line item (issue #31, RC3-004) —
// mirrors resolveSKUPriceEntry's (sku_lookup.go) error-unwrapping and
// Prices/Ambiguous/NoMapping/Error discrimination exactly, but for a single
// region and shaped as a bomLineItem rather than get_price_by_sku's response
// map, since a BoM line item needs exactly one price to cost out.
// --------------------------------------------------------------------------

func resolveBOMSKUItem(
	ctx context.Context,
	provs map[string]Provider,
	label string,
	item map[string]any,
	sku string,
	quantity float64,
	hoursPerMonth float64,
	sizeGB float64,
	description string,
) (bomLineItem, string) {
	region, _ := item["region"].(string)
	if region == "" {
		return bomLineItem{}, fmt.Sprintf("%s: region is required for raw-SKU items (sku %q)", label, sku)
	}

	providerName, errMsg := stringItemField(item, "provider", label, sku)
	if errMsg != "" {
		return bomLineItem{}, errMsg
	}
	if providerName == "" {
		// Mirrors HandleGetPriceBySKU's default: raw usage-type/SKU strings
		// are an AWS CUR concept, so an absent provider means "aws".
		providerName = "aws"
	}

	lookupP, errOut := resolveSKULookupProviderFromMap(provs, providerName, "raw-SKU BoM items")
	if errOut != nil {
		msg, _ := errOut["message"].(string)
		return bomLineItem{}, fmt.Sprintf("%s: %s (sku %q)", label, msg, sku)
	}

	serviceHint, errMsg := stringItemField(item, "service", label, sku)
	if errMsg != "" {
		return bomLineItem{}, errMsg
	}
	operation, errMsg := stringItemField(item, "operation", label, sku)
	if errMsg != "" {
		return bomLineItem{}, errMsg
	}
	productFamily, errMsg := stringItemField(item, "product_family", label, sku)
	if errMsg != "" {
		return bomLineItem{}, errMsg
	}

	result, err := lookupP.LookupSKUAcrossRegionsGeneric(ctx, sku, []string{region}, serviceHint, skulookup.SKUHint{
		OperationHint:     operation,
		ProductFamilyHint: productFamily,
	})
	if err != nil {
		var skuErr *skulookup.SKULookupError
		if errors.As(err, &skuErr) {
			return bomLineItem{}, fmt.Sprintf("%s: [%s] %s (sku %q)", label, skuErr.Code, skuErr.Message, sku)
		}
		return bomLineItem{}, fmt.Sprintf("%s: SKU lookup failed (sku %q)", label, sku)
	}

	rr := result.Regions[0]
	switch classifySKURegionResult(rr) {
	case skuResultAmbiguous:
		return bomLineItem{}, fmt.Sprintf(
			"%s: sku %q is ambiguous in region '%s' (%d matching rows) — supply operation/product_family to disambiguate",
			label, sku, region, len(rr.Prices))
	case skuResultNoMapping:
		return bomLineItem{}, fmt.Sprintf("%s: sku %q has no pricing mapping in region '%s' (tried service(s): %v)",
			label, sku, region, rr.AttemptedServices)
	case skuResultError:
		return bomLineItem{}, fmt.Sprintf("%s: %s (sku %q, region '%s')", label, rr.Error, sku, region)
	case skuResultUnresolved:
		return bomLineItem{}, fmt.Sprintf("%s: sku %q could not be resolved in region '%s'", label, sku, region)
	}

	price := rr.Prices[0]
	// Tiered (GCP only — see skulookup.SKULookupRegionResult.Tiered): rr.Prices
	// holds every usage-volume tier's rate, ascending by usage threshold, each
	// tagged with its own Attributes["tier_start_usage"]. GCP's tiered SKUs are
	// graduated/bracketed billing (the same model as gcp_networking.go's
	// computeTieredCost / gcp_dns.go's addTieredEstimate): each tier's rate
	// applies only to the slice of usage that falls within that tier's own
	// bracket, not to the whole quantity at one flat rate. Tier-threshold
	// comparisons must also use the same usage volume bomMonthlyCost itself
	// scales by (hoursPerMonth*quantity for PER_HOUR, sizeGB*quantity for
	// PER_GB_MONTH), not raw quantity, or a PER_GB_MONTH/PER_HOUR item would
	// select tiers based on a number the customer never actually sees billed.
	var monthly float64
	if rr.Tiered {
		usageVolume := gcpTieredUsageVolume(price.Unit, quantity, hoursPerMonth, sizeGB)
		monthly, price = gcpGraduatedTieredCost(rr.Prices, usageVolume)
		// Report the blended effective rate (monthly / usage volume), not the
		// marginal tier's own rate, in unitPrice.PricePerUnit — this is the
		// same convention gcp_networking.go's computeTieredCost uses
		// (BlendedRatePerGB = totalCost/dataGB). The marginal rate alone would
		// make the line item's displayed price_per_unit * quantity disagree
		// with its own monthly_cost whenever usage spans more than one
		// bracket (e.g. qty=200 across a $0.10/$0.05 two-tier split: marginal
		// $0.05 * 200 = $10 != the correct $15 monthly cost).
		if usageVolume > 0 {
			price.PricePerUnit = monthly / usageVolume
		}
	} else {
		monthly = bomMonthlyCost(price, quantity, hoursPerMonth, sizeGB)
	}
	annual := monthly * 12

	lineDesc := description
	if lineDesc == "" {
		svc := rr.ServiceUsed
		if svc == "" {
			svc = price.Service
		}
		lineDesc = fmt.Sprintf("SKU %s (%s)", sku, svc)
	}

	return bomLineItem{
		description: lineDesc,
		provider:    string(price.Provider),
		service:     price.Service,
		region:      price.Region,
		quantity:    quantity,
		unitPrice:   price,
		monthlyCost: monthly,
		annualCost:  annual,
		sku:         sku,
	}, ""
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
		servicesSet[bomAdvisoryServiceToken(li.service)] = struct{}{}
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
