// Package gcp — compute and storage pricing (Part 1).
// Ports _compute_price and _storage_price from Python's gcp.py.
package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/utils"
)

// _TERM_USAGE_TYPE maps PricingTerm → (usageType, cpuDescKey, ramDescKey).
// cpuDescKey / ramDescKey reference the FamilySKU fields.
type termMapping struct {
	usageType string
	cpuKey    string // "cpu" | "preemptCPU" | "cudCPU" | "flexCudCPU"
	ramKey    string // "ram" | "preemptRAM" | "cudRAM" | "flexCudRAM"
}

var termUsageType = map[models.PricingTerm]termMapping{
	models.PricingTermOnDemand: {"OnDemand", "cpu", "ram"},
	models.PricingTermSpot:     {"Preemptible", "preemptCPU", "preemptRAM"},
	models.PricingTermCUD1Yr:   {"Commit1Yr", "cudCPU", "cudRAM"},
	models.PricingTermCUD3Yr:   {"Commit3Yr", "cudCPU", "cudRAM"},
	models.PricingTermFlexCUD:  {"CmtCudPremium", "flexCudCPU", "flexCudRAM"},
}

// sudMonthlyHours is the GCP billing month in hours.
const sudMonthlyHours = 730.0

// sudTier represents one GCP SUD discount tier.
type sudTier struct {
	startHr float64 // inclusive
	endHr   float64 // exclusive
	factor  float64 // price multiplier (1.0 = no discount, 0.4 = 60% off)
}

// sudTiers is GCP's official, published Sustained Use Discount schedule.
// Source: https://cloud.google.com/compute/docs/sustained-use-discounts
var sudTiers = [4]sudTier{
	{0, 182.5, 1.00},   // 0-25% of month: no discount
	{182.5, 365, 0.80}, // 25-50%: 20% off
	{365, 547.5, 0.60}, // 50-75%: 40% off
	{547.5, 730, 0.40}, // 75-100%: 60% off
}

// familySKUDesc returns the CPU / RAM description strings from the spec-based mapping.
func familySKUDesc(fsku utils.FamilySKU, cpuKey, ramKey string) (string, string) {
	switch cpuKey {
	case "preemptCPU":
		return fsku.PreemptCPUDesc, fsku.PreemptRAMDesc
	case "cudCPU":
		return fsku.CUDCPUDesc, fsku.CUDRAMDesc
	case "flexCudCPU":
		return fsku.FlexCUDCPUDesc, fsku.FlexCUDRAMDesc
	default: // "cpu"
		return fsku.CPUDesc, fsku.RAMDesc
	}
}

// --------------------------------------------------------------------------
// Supports / SupportedTerms
// --------------------------------------------------------------------------

// Supports reports whether the GCP provider can price the given domain/service pair.
// Part 1: compute and storage. Part 2: database, container, AI, analytics.
func (p *Provider) Supports(domain models.PricingDomain, service string) bool {
	switch domain { //nolint:exhaustive // PricingDomainServerless is not supported by GCP; falls to return false
	case models.PricingDomainCompute:
		switch service {
		case "", "compute_engine", "vm":
			return true
		}
	case models.PricingDomainStorage:
		switch service {
		case "", "gcs", "persistent_disk":
			return true
		}
	// Part 2 domains — implemented in gcp_database.go and gcp_ai.go.
	case models.PricingDomainDatabase:
		switch service {
		case "", "cloud_sql", "mysql", "postgresql", "postgres", "memorystore", "redis":
			return true
		}
	case models.PricingDomainContainer:
		switch service {
		case "", "gke":
			return true
		}
	case models.PricingDomainAI:
		switch service {
		case "", "vertex", "vertex_ai", "vertexai", "gemini":
			return true
		}
	case models.PricingDomainAnalytics:
		switch service {
		case "", "bigquery":
			return true
		}
	// Part 3 domains — implemented in gcp_networking.go and gcp_analytics.go.
	case models.PricingDomainNetwork:
		switch service {
		case "", "cloud_lb", "cloud_cdn", "cloud_nat", "cloud_armor", "egress", "external_ip":
			return true
		}
	case models.PricingDomainObservability:
		switch service {
		case "", "cloud_monitoring":
			return true
		}
	case models.PricingDomainInterRegionEgress:
		return true
	// Part 3 domain — implemented in gcp_kms.go.
	case models.PricingDomainSecurity:
		switch service {
		case "", "kms":
			return true
		}
	// Part 3 domain — implemented in gcp_dns.go.
	case models.PricingDomainDNS:
		switch service {
		case "", "cloud_dns", "dns", "clouddns":
			return true
		}
	// Part 3 domain — implemented in gcp_pubsub.go.
	case models.PricingDomainMessaging:
		switch service {
		case "", "pubsub", "pub_sub":
			return true
		}
	default:
		return false
	}
	return false
}

// SupportedTerms returns the pricing terms the provider supports for a given domain/service.
func (p *Provider) SupportedTerms(domain models.PricingDomain, service string) []models.PricingTerm {
	switch domain { //nolint:exhaustive // unlisted domains fall through to on-demand default
	case models.PricingDomainCompute:
		return []models.PricingTerm{
			models.PricingTermOnDemand,
			models.PricingTermSpot,
			models.PricingTermCUD1Yr,
			models.PricingTermCUD3Yr,
			models.PricingTermSUD,
			models.PricingTermFlexCUD,
		}
	case models.PricingDomainStorage:
		return []models.PricingTerm{models.PricingTermOnDemand}
	}
	return []models.PricingTerm{models.PricingTermOnDemand}
}

// --------------------------------------------------------------------------
// GetPrice — unified entry point
// --------------------------------------------------------------------------

// GetPrice is the primary entry point. Dispatches to domain implementations.
func (p *Provider) GetPrice(ctx context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
	if !p.Supports(spec.GetDomain(), spec.GetService()) {
		return nil, providers.ErrNotSupported
	}

	switch s := spec.(type) {
	case *models.ComputePricingSpec:
		resourceType := s.ResourceType
		if resourceType == "" {
			resourceType = "n1-standard-4"
		}
		osType := s.OS
		if osType == "" {
			osType = "Linux"
		}
		prices, err := p.GetComputePrice(ctx, resourceType, s.Region, osType, s.Term)
		if err != nil {
			return nil, err
		}
		return buildResult(prices, nil), nil

	case *models.StoragePricingSpec:
		storageType := s.StorageType
		if storageType == "" {
			storageType = "standard"
		}
		stLower := strings.ToLower(storageType)
		sizeGB := 0.0
		if s.SizeGB != nil {
			sizeGB = *s.SizeGB
		}
		prices, err := p.GetStoragePrice(ctx, storageType, s.Region, sizeGB)
		if err != nil {
			return nil, err
		}
		// For hyperdisk-extreme: if the live GCP catalog lookup returned no
		// results (SKU pattern may not yet be indexed), fall back to the
		// published static rate of $0.125/GB-month.
		if len(prices) == 0 && stLower == "hyperdisk-extreme" {
			now := time.Now().UTC()
			prices = []models.NormalizedPrice{{
				Provider:      models.CloudProviderGCP,
				Service:       "storage",
				SKUID:         fmt.Sprintf("gcp:storage:hyperdisk-extreme:%s:static", s.Region),
				ProductFamily: "Storage",
				Description:   "GCP Hyperdisk Extreme storage — static rate ($0.125/GB-month)",
				Region:        s.Region,
				PricingTerm:   models.PricingTermOnDemand,
				PricePerUnit:  0.125,
				Unit:          models.PriceUnitPerGBMonth,
				Currency:      "USD",
				Attributes: map[string]string{
					"fallback":     "true",
					"storage_type": "hyperdisk-extreme",
				},
				FetchedAt: &now,
			}}
		}
		// For pd-extreme and hyperdisk-extreme with provisioned IOPS, append a
		// per_iops_month price entry. Published GCP rate: $0.080/IOPS-month.
		if s.IOPS != nil && *s.IOPS > 0 &&
			(stLower == "pd-extreme" || stLower == "hyperdisk-extreme") {
			iopsCount := *s.IOPS
			now := time.Now().UTC()
			prices = append(prices, models.NormalizedPrice{
				Provider:      models.CloudProviderGCP,
				Service:       "storage",
				SKUID:         fmt.Sprintf("gcp:storage:%s:%s:iops:static", stLower, s.Region),
				ProductFamily: "Storage",
				Description:   fmt.Sprintf("GCP %s provisioned IOPS — static rate ($0.080/IOPS-month)", storageType),
				Region:        s.Region,
				PricingTerm:   models.PricingTermOnDemand,
				PricePerUnit:  0.080,
				Unit:          models.PriceUnitPerIOPSMonth,
				Currency:      "USD",
				Attributes: map[string]string{
					"fallback":     "true",
					"storage_type": stLower,
					"iops":         fmt.Sprintf("%d", iopsCount),
				},
				FetchedAt: &now,
			})
		}
		return buildResult(prices, nil), nil
	}

	// Part 2: database and container domains (gcp_database.go).
	if result, err := p.getPart2Price(ctx, spec); result != nil || err != nil {
		return result, err
	}

	// Part 2: AI and analytics domains (gcp_ai.go).
	if result, err := p.getAIPart2Price(ctx, spec); result != nil || err != nil {
		return result, err
	}

	// Part 3: networking, observability, and inter_region_egress (gcp_catalog.go / gcp_networking.go).
	if result, err := p.getPart3Price(ctx, spec); result != nil || err != nil {
		return result, err
	}

	return nil, providers.ErrNotSupported
}

// --------------------------------------------------------------------------
// GetComputePrice — full implementation
// --------------------------------------------------------------------------

// GetComputePrice returns the hourly on-demand / preemptible / CUD price for a
// GCP Compute Engine instance type in the given region.
//
// Algorithm (mirrors Python get_compute_price):
//  1. Parse instance type → (vcpus, memoryGB, family).
//  2. Look up FamilySKU → CPU/RAM description substrings for the requested term.
//  3. Build/load the Compute Engine price index for the region.
//  4. Look up CPU price and RAM price by description substring.
//  5. Total = vcpus×cpuPrice + memGB×ramPrice.
//  6. For Windows: add Windows Server license on top (OnDemand always).
func (p *Provider) GetComputePrice(
	ctx context.Context,
	instanceType string,
	region string,
	os string,
	term models.PricingTerm,
) ([]models.NormalizedPrice, error) {
	vcpus, memGB, ok := utils.ParseInstanceType(instanceType)
	if !ok {
		return nil, fmt.Errorf("gcp: unknown instance type %q — use ListInstanceTypes to discover valid types", instanceType)
	}
	family := utils.GetMachineFamily(instanceType)

	fsku, hasFSKU := utils.GCPFamilySKU[family]
	if !hasFSKU {
		// Wrap providers.ErrNotSupported (rather than a bare fmt.Errorf) so the
		// caller (lookup.go) classifies this as a clean not_supported response
		// instead of a retryable upstream_failure -- there is nothing to retry,
		// this machine family is simply absent from the local catalog.
		return nil, fmt.Errorf("gcp: machine family %q is not in the supported catalog (supported: %v): %w",
			family, sortedFamilyKeys(), providers.ErrNotSupported)
	}

	// SUD is computed from on-demand rates + published tier schedule, not from catalog SKUs.
	if term == models.PricingTermSUD {
		return p.gcpSUDPrice(ctx, instanceType, region, os)
	}

	// Flex CUD is spend-based (not resource-based) — computed from on-demand × 0.72 (28% off).
	if term == models.PricingTermFlexCUD {
		return p.gcpFlexCUDPrice(ctx, instanceType, region, os)
	}

	tm, hasTerm := termUsageType[term]
	if !hasTerm {
		return nil, fmt.Errorf("gcp: unsupported term %q for compute pricing", term)
	}

	cpuDesc, ramDesc := familySKUDesc(fsku, tm.cpuKey, tm.ramKey)

	// Empty desc means this family does not support the term (e.g. N1 CUD, T2A CUD).
	if cpuDesc == "" || ramDesc == "" {
		slog.Info("gcp: family does not support term — returning empty",
			"family", family, "term", term)
		return []models.NormalizedPrice{}, nil
	}

	// Check cache.
	cacheKey := fmt.Sprintf("gcp:compute:%s:%s:%s:%s", instanceType, region, os, string(term))
	if cached, ok := p.cache.Get(cacheKey); ok {
		var prices []models.NormalizedPrice
		if err := json.Unmarshal(cached, &prices); err == nil {
			return stampCacheAge(prices), nil
		}
	}

	idx, err := p.buildComputePriceIndex(ctx, region)
	if err != nil {
		return nil, err
	}

	cpuPrice, found := lookupPrice(idx, cpuDesc, tm.usageType)
	if !found {
		slog.Warn("gcp: CPU SKU not found", "desc", cpuDesc, "usage_type", tm.usageType,
			"instance", instanceType, "region", region, "term", term)
		return []models.NormalizedPrice{}, nil
	}
	ramPrice, found := lookupPrice(idx, ramDesc, tm.usageType)
	if !found {
		slog.Warn("gcp: RAM SKU not found", "desc", ramDesc, "usage_type", tm.usageType,
			"instance", instanceType, "region", region, "term", term)
		return []models.NormalizedPrice{}, nil
	}

	cpuRamTotal := float64(vcpus)*cpuPrice + memGB*ramPrice
	totalPrice := cpuRamTotal

	// GPU accelerator cost: only for on-demand term.
	// Spot/CUD GPU pricing uses different SKU patterns not yet implemented.
	var gpuAttrs map[string]string
	if term == models.PricingTermOnDemand {
		if gpuInfo, hasGPU := utils.GCPInstanceGPU[instanceType]; hasGPU {
			gpuPrice, gpuFound := lookupPrice(idx, gpuInfo.OnDemand, "OnDemand")
			if gpuFound {
				gpuCost := gpuPrice * float64(gpuInfo.Count)
				totalPrice += gpuCost
				gpuAttrs = map[string]string{
					"gpu_count":          fmt.Sprintf("%d", gpuInfo.Count),
					"gpu_model":          gpuInfo.Model,
					"gpu_rate_per_hour":  fmt.Sprintf("$%.6f", gpuPrice),
					"gpu_cost_per_hour":  fmt.Sprintf("$%.6f", gpuCost),
					"pricing_components": "cpu+ram+gpu",
				}
			} else {
				slog.Warn("gcp: GPU SKU not found — returning CPU+RAM only",
					"instance", instanceType, "gpu_desc", gpuInfo.OnDemand, "region", region)
			}
		}
	}

	// Windows licensing: add per-vCPU and per-GB-RAM Windows Server cost.
	// Windows SKUs are always looked up at usageType "OnDemand" regardless of term.
	if strings.EqualFold(os, "windows") {
		winCPUDesc, winRAMDesc := utils.WindowsSKU(family)
		if winCPUDesc == "" {
			slog.Warn("gcp: Windows pricing not supported for machine family", "family", family)
			return []models.NormalizedPrice{}, nil
		}
		winCPUPrice, ok := lookupPrice(idx, winCPUDesc, "OnDemand")
		if !ok {
			slog.Warn("gcp: Windows CPU SKU not found", "desc", winCPUDesc, "region", region)
			return []models.NormalizedPrice{}, nil
		}
		winRAMPrice, ok := lookupPrice(idx, winRAMDesc, "OnDemand")
		if !ok {
			slog.Warn("gcp: Windows RAM SKU not found", "desc", winRAMDesc, "region", region)
			return []models.NormalizedPrice{}, nil
		}
		windowsLicense := float64(vcpus)*winCPUPrice + memGB*winRAMPrice
		totalPrice += windowsLicense
	}

	price := models.NormalizedPrice{
		Provider:      models.CloudProviderGCP,
		Service:       "compute",
		SKUID:         fmt.Sprintf("gcp:%s:%s:%s", family, region, string(term)),
		ProductFamily: "Compute Instance",
		Description:   instanceType,
		Region:        region,
		Attributes: map[string]string{
			"instanceType":    instanceType,
			"vcpu":            fmt.Sprintf("%d", vcpus),
			"memory":          fmt.Sprintf("%.4g GB", memGB),
			"operatingSystem": os,
			"machineFamily":   family,
		},
		PricingTerm:  term,
		PricePerUnit: totalPrice,
		Unit:         models.PriceUnitPerHour,
		Currency:     "USD",
	}

	// Merge GPU attributes if present.
	for k, v := range gpuAttrs {
		price.Attributes[k] = v
	}

	// For on-demand requests on SUD-eligible families, annotate a hint with the
	// blended SUD rate. Use the base CPU+RAM total (before Windows license) so
	// the SUD hint reflects the VM resource cost, not the license.
	if term == models.PricingTermOnDemand && utils.SUDEligible(family) {
		price.Attributes["sud_eligible"] = "true"
		price.Attributes["sud_blended_rate_at_100pct"] = fmt.Sprintf(
			"$%.6f/hr (30%% off on-demand; use term='sud' for full tier breakdown)",
			cpuRamTotal*0.7,
		)
	}

	result := annotateFresh([]models.NormalizedPrice{price})

	if raw, err := json.Marshal(result); err == nil {
		p.cache.Set(cacheKey, raw, p.cfg.CacheTTL())
	}
	return result, nil
}

// --------------------------------------------------------------------------
// gcpSUDPrice — Sustained Use Discount pricing
// --------------------------------------------------------------------------

// gcpSUDPrice computes the blended Sustained Use Discount rate for a GCP
// Compute Engine instance assuming 100% monthly (continuous 24/7) usage.
//
// SUD is a billing-time discount — there are no "SUD" SKUs in the catalog.
// We fetch the on-demand rate and apply GCP's published contractual tier schedule:
//   - 0-25%  of month (0-182.5 hrs):   0% off   (factor 1.00)
//   - 25-50% of month (182.5-365 hrs): 20% off   (factor 0.80)
//   - 50-75% of month (365-547.5 hrs): 40% off   (factor 0.60)
//   - 75-100% of month (547.5-730 hrs): 60% off  (factor 0.40)
//     Blended for 100% usage = on_demand × 0.700 (30% off)
//
// Returns an empty slice (not an error) when the family is SUD-ineligible
// (e.g. a2, a3, g2 GPU families).
func (p *Provider) gcpSUDPrice(
	ctx context.Context,
	instanceType string,
	region string,
	os string,
) ([]models.NormalizedPrice, error) {
	vcpus, memGB, ok := utils.ParseInstanceType(instanceType)
	if !ok {
		return nil, fmt.Errorf("gcp: unknown instance type %q — use ListInstanceTypes to discover valid types", instanceType)
	}
	family := utils.GetMachineFamily(instanceType)

	// SUD eligibility check — GPU/accelerator families are not eligible.
	if !utils.SUDEligible(family) {
		slog.Info("gcp: SUD not eligible for family", "family", family)
		return []models.NormalizedPrice{}, nil
	}

	fsku, hasFSKU := utils.GCPFamilySKU[family]
	if !hasFSKU {
		// Wrap providers.ErrNotSupported (rather than a bare fmt.Errorf) so the
		// caller (lookup.go) classifies this as a clean not_supported response
		// instead of a retryable upstream_failure -- there is nothing to retry,
		// this machine family is simply absent from the local catalog.
		return nil, fmt.Errorf("gcp: machine family %q is not in the supported catalog (supported: %v): %w",
			family, sortedFamilyKeys(), providers.ErrNotSupported)
	}

	// Get on-demand CPU/RAM description strings for this family.
	cpuDesc, ramDesc := familySKUDesc(fsku, "cpu", "ram")
	if cpuDesc == "" || ramDesc == "" {
		slog.Info("gcp: no on-demand SKU desc for SUD family", "family", family)
		return []models.NormalizedPrice{}, nil
	}

	// Check cache.
	cacheKey := fmt.Sprintf("gcp:compute:%s:%s:%s:sud", instanceType, region, os)
	if cached, ok := p.cache.Get(cacheKey); ok {
		var prices []models.NormalizedPrice
		if err := json.Unmarshal(cached, &prices); err == nil {
			return stampCacheAge(prices), nil
		}
	}

	idx, err := p.buildComputePriceIndex(ctx, region)
	if err != nil {
		return nil, err
	}

	cpuPrice, found := lookupPrice(idx, cpuDesc, "OnDemand")
	if !found {
		slog.Warn("gcp: SUD — CPU SKU not found", "desc", cpuDesc, "instance", instanceType, "region", region)
		return []models.NormalizedPrice{}, nil
	}
	ramPrice, found := lookupPrice(idx, ramDesc, "OnDemand")
	if !found {
		slog.Warn("gcp: SUD — RAM SKU not found", "desc", ramDesc, "instance", instanceType, "region", region)
		return []models.NormalizedPrice{}, nil
	}

	// on-demand hourly total for this instance.
	onDemandTotal := float64(vcpus)*cpuPrice + memGB*ramPrice

	// Compute per-tier rates (full instance, not per-component).
	tier0rate := onDemandTotal * sudTiers[0].factor
	tier1rate := onDemandTotal * sudTiers[1].factor
	tier2rate := onDemandTotal * sudTiers[2].factor
	tier3rate := onDemandTotal * sudTiers[3].factor

	// Blended rate for 100% monthly usage.
	// Each tier spans sudMonthlyHours/4 = 182.5 hrs.
	// = (182.5×1.00 + 182.5×0.80 + 182.5×0.60 + 182.5×0.40) × onDemandTotal / sudMonthlyHours
	// = (182.5 × 2.80) / sudMonthlyHours × onDemandTotal = 0.70 × onDemandTotal
	blendedRate := onDemandTotal * (sudMonthlyHours / 4) * (1.00 + 0.80 + 0.60 + 0.40) / sudMonthlyHours

	price := models.NormalizedPrice{
		Provider:      models.CloudProviderGCP,
		Service:       "compute",
		SKUID:         "gcp-sud-" + strings.ToLower(instanceType),
		ProductFamily: "Compute Instance",
		Description:   fmt.Sprintf("%s — SUD blended rate (100%% monthly, 30%% off on-demand)", instanceType),
		Region:        region,
		Attributes: map[string]string{
			"instanceType":       instanceType,
			"vcpu":               fmt.Sprintf("%d", vcpus),
			"memory":             fmt.Sprintf("%.4g GB", memGB),
			"operatingSystem":    os,
			"sud_rate_source":    "gcp_catalog_ondemand+published_sud_schedule",
			"sud_tier_0":         fmt.Sprintf("0-182.5 hrs (0-25%% of month): $%.6f/hr (0%% off)", tier0rate),
			"sud_tier_1":         fmt.Sprintf("182.5-365 hrs (25-50%% of month): $%.6f/hr (20%% off)", tier1rate),
			"sud_tier_2":         fmt.Sprintf("365-547.5 hrs (50-75%% of month): $%.6f/hr (40%% off)", tier2rate),
			"sud_tier_3":         fmt.Sprintf("547.5-730 hrs (75-100%% of month): $%.6f/hr (60%% off)", tier3rate),
			"sud_blended_factor": "0.700",
			"sud_discount_pct":   "30.0",
			"usage_assumption":   "730 hours/month (100% continuous)",
			"note":               "SUD is applied automatically by GCP for qualifying VMs. No commitment required. Effective rate varies with actual monthly usage — this rate assumes continuous 24/7 operation.",
		},
		PricingTerm:  models.PricingTermSUD,
		PricePerUnit: blendedRate,
		Unit:         models.PriceUnitPerHour,
		Currency:     "USD",
	}

	result := annotateFresh([]models.NormalizedPrice{price})

	if raw, err := json.Marshal(result); err == nil {
		p.cache.Set(cacheKey, raw, p.cfg.CacheTTL())
	}
	return result, nil
}

// --------------------------------------------------------------------------
// gcpFlexCUDPrice — Flexible CUD pricing (spend-based, computed from on-demand)
// --------------------------------------------------------------------------

// flexCUD1YrDiscount is the GCP Flexible CUD 1-year discount rate (28% off on-demand).
// Source: https://cloud.google.com/blog/products/compute/save-money-with-the-new-compute-engine-flexible-cuds
const flexCUD1YrDiscount = 0.28

// gcpFlexCUDPrice computes the Flex CUD 1-year hourly price for a GCP Compute Engine instance.
//
// Flex CUD is a spend-based commitment — there are no dedicated catalog SKUs. The price is
// computed as on_demand × (1 - 0.28) = 72% of on-demand. Eligible families: N1, N2, N2D,
// E2, C2, C2D. GPU/accelerator and Arm families are not eligible.
func (p *Provider) gcpFlexCUDPrice(
	ctx context.Context,
	instanceType string,
	region string,
	os string,
) ([]models.NormalizedPrice, error) {
	family := utils.GetMachineFamily(instanceType)

	if !utils.FlexCUDEligible(family) {
		slog.Info("gcp: Flex CUD not eligible for family", "family", family)
		return []models.NormalizedPrice{}, nil
	}

	// Check cache.
	cacheKey := fmt.Sprintf("gcp:compute:%s:%s:%s:flex_cud", instanceType, region, os)
	if cached, ok := p.cache.Get(cacheKey); ok {
		var prices []models.NormalizedPrice
		if err := json.Unmarshal(cached, &prices); err == nil {
			return stampCacheAge(prices), nil
		}
	}

	// Get on-demand price as the base.
	onDemandPrices, err := p.GetComputePrice(ctx, instanceType, region, os, models.PricingTermOnDemand)
	if err != nil {
		return nil, err
	}
	if len(onDemandPrices) == 0 {
		return []models.NormalizedPrice{}, nil
	}

	vcpus, memGB, _ := utils.ParseInstanceType(instanceType)
	onDemand := onDemandPrices[0]
	flexPrice := onDemand.PricePerUnit * (1.0 - flexCUD1YrDiscount)

	price := models.NormalizedPrice{
		Provider:      models.CloudProviderGCP,
		Service:       "compute",
		SKUID:         fmt.Sprintf("gcp-flex-cud-%s-%s", strings.ToLower(instanceType), region),
		ProductFamily: "Compute Instance",
		Description:   instanceType,
		Region:        region,
		Attributes: map[string]string{
			"instanceType":      instanceType,
			"vcpu":              fmt.Sprintf("%d", vcpus),
			"memory":            fmt.Sprintf("%.4g GB", memGB),
			"operatingSystem":   os,
			"machineFamily":     family,
			"flex_cud_term":     "1_year",
			"flex_cud_discount": "28%",
			"on_demand_rate":    fmt.Sprintf("$%.6f/hr", onDemand.PricePerUnit),
			"note":              "Flex CUD is spend-based. No commitment to a specific VM type or region. 3-year Flex CUD gives 46% off on-demand.",
		},
		PricingTerm:  models.PricingTermFlexCUD,
		PricePerUnit: flexPrice,
		Unit:         models.PriceUnitPerHour,
		Currency:     "USD",
	}

	result := annotateFresh([]models.NormalizedPrice{price})

	if raw, err := json.Marshal(result); err == nil {
		p.cache.Set(cacheKey, raw, p.cfg.CacheTTL())
	}
	return result, nil
}

// --------------------------------------------------------------------------
// GetStoragePrice — full implementation (GCS + Persistent Disk)
// --------------------------------------------------------------------------

// gcsStorageClasses maps GCS storage class name to the SKU description substring.
var gcsStorageClasses = map[string]string{
	"standard": "Standard Storage",
	"nearline": "Nearline Storage",
	"coldline": "Coldline Storage",
	"archive":  "Archive Storage",
}

// GetStoragePrice returns the per-GB-month price for a GCP storage type in a region.
// It handles both GCS storage classes (standard/nearline/coldline/archive) and
// Persistent Disk types (pd-standard, pd-ssd, pd-balanced, pd-extreme).
func (p *Provider) GetStoragePrice(
	ctx context.Context,
	storageType string,
	region string,
	sizeGB float64,
) ([]models.NormalizedPrice, error) {
	stLower := strings.ToLower(storageType)

	// Route GCS storage classes to the GCS index.
	if descSubstring, isGCS := gcsStorageClasses[stLower]; isGCS {
		return p.getGCSStoragePrice(ctx, stLower, descSubstring, region)
	}

	// Persistent Disk: use Compute Engine index.
	skuPatterns, ok := utils.GCPStorageSKU[stLower]
	if !ok {
		return nil, fmt.Errorf(
			"gcp: unknown storage type %q; supported: %v or GCS classes: standard, nearline, coldline, archive",
			storageType, sortedStorageSKUKeys(),
		)
	}

	cacheKey := fmt.Sprintf("gcp:storage:%s:%s", stLower, region)
	if cached, ok := p.cache.Get(cacheKey); ok {
		var prices []models.NormalizedPrice
		if err := json.Unmarshal(cached, &prices); err == nil {
			return stampCacheAge(prices), nil
		}
	}

	idx, err := p.buildComputePriceIndex(ctx, region)
	if err != nil {
		return nil, err
	}

	price, found := lookupPrice(idx, skuPatterns.Desc, "OnDemand")
	if !found && skuPatterns.AltDesc != "" {
		price, found = lookupPrice(idx, skuPatterns.AltDesc, "OnDemand")
	}
	if !found {
		return []models.NormalizedPrice{}, nil
	}

	np := models.NormalizedPrice{
		Provider:      models.CloudProviderGCP,
		Service:       "storage",
		SKUID:         fmt.Sprintf("gcp:storage:%s:%s", stLower, region),
		ProductFamily: "Storage",
		Description:   fmt.Sprintf("%s in %s", storageType, region),
		Region:        region,
		Attributes:    map[string]string{"storage_type": stLower},
		PricingTerm:   models.PricingTermOnDemand,
		PricePerUnit:  price,
		Unit:          models.PriceUnitPerGBMonth,
		Currency:      "USD",
	}

	result := annotateFresh([]models.NormalizedPrice{np})
	if raw, err := json.Marshal(result); err == nil {
		p.cache.Set(cacheKey, raw, p.cfg.CacheTTL())
	}
	return result, nil
}

// getGCSStoragePrice fetches GCS storage pricing using the GCS SKU index.
func (p *Provider) getGCSStoragePrice(
	ctx context.Context,
	storageClass string,
	descSubstring string,
	region string,
) ([]models.NormalizedPrice, error) {
	cacheKey := fmt.Sprintf("gcp:gcs_storage:%s:%s", storageClass, region)
	if cached, ok := p.cache.Get(cacheKey); ok {
		var prices []models.NormalizedPrice
		if err := json.Unmarshal(cached, &prices); err == nil {
			return stampCacheAge(prices), nil
		}
	}

	idx, err := p.buildGCSPriceIndex(ctx, region)
	if err != nil {
		return nil, err
	}

	price, found := lookupPrice(idx, descSubstring, "OnDemand")
	if !found {
		return []models.NormalizedPrice{}, nil
	}

	np := models.NormalizedPrice{
		Provider:      models.CloudProviderGCP,
		Service:       "storage",
		SKUID:         fmt.Sprintf("gcp:gcs:%s:%s", storageClass, region),
		ProductFamily: "Cloud Storage",
		Description:   fmt.Sprintf("GCS %s storage in %s", storageClass, region),
		Region:        region,
		Attributes: map[string]string{
			"storage_type": storageClass,
			"class":        storageClass,
		},
		PricingTerm:  models.PricingTermOnDemand,
		PricePerUnit: price,
		Unit:         models.PriceUnitPerGBMonth,
		Currency:     "USD",
	}

	result := annotateFresh([]models.NormalizedPrice{np})
	if raw, err := json.Marshal(result); err == nil {
		p.cache.Set(cacheKey, raw, p.cfg.CacheTTL())
	}
	return result, nil
}

// --------------------------------------------------------------------------
// SearchPricing
// --------------------------------------------------------------------------

// SearchPricing performs a free-text search across the GCP compute catalog.
// It matches the query against instance type names and fetches on-demand prices.
func (p *Provider) SearchPricing(
	ctx context.Context,
	query string,
	region string,
	maxResults int,
) ([]models.NormalizedPrice, error) {
	if region == "" {
		region = p.DefaultRegion()
	}
	queryLower := strings.ToLower(query)
	var results []models.NormalizedPrice

	for itype := range utils.GCPInstanceSpecs {
		if !strings.Contains(itype, queryLower) {
			continue
		}
		prices, err := p.GetComputePrice(ctx, itype, region, "Linux", models.PricingTermOnDemand)
		if err != nil {
			continue
		}
		results = append(results, prices...)
		if len(results) >= maxResults {
			break
		}
	}
	return results, nil
}

// --------------------------------------------------------------------------
// ListRegions
// --------------------------------------------------------------------------

// ListRegions returns all GCP regions. Uses the static list from gcp_specs.go.
func (p *Provider) ListRegions(ctx context.Context, service string) ([]string, error) {
	// Static list — accurate as of the gcp_specs.go snapshot.
	return utils.GCPRegions, nil
}

// --------------------------------------------------------------------------
// ListInstanceTypes
// --------------------------------------------------------------------------

// ListInstanceTypes returns GCP instance types matching the given filters.
func (p *Provider) ListInstanceTypes(
	ctx context.Context,
	region string,
	family string,
	minVCPUs int,
	minMemoryGB float64,
	gpu bool,
) ([]models.InstanceTypeInfo, error) {
	gpuFamilies := map[string]bool{"a2": true, "g2": true, "a3": true}

	var results []models.InstanceTypeInfo
	for itype, spec := range utils.GCPInstanceSpecs {
		fam := utils.GetMachineFamily(itype)
		isGPU := gpuFamilies[fam]

		if gpu && !isGPU {
			continue
		}
		if family != "" && !strings.HasPrefix(itype, family) {
			continue
		}
		if minVCPUs > 0 && spec.VCPU < minVCPUs {
			continue
		}
		if minMemoryGB > 0 && spec.MemoryGB < minMemoryGB {
			continue
		}

		gpuCount := 0
		gpuType := ""
		if gpuInfo, ok := utils.GCPInstanceGPU[itype]; ok {
			gpuCount = gpuInfo.Count
			gpuType = gpuInfo.Model
		} else if isGPU {
			gpuCount = 1 // fallback for GPU families without per-instance data
		}
		results = append(results, models.InstanceTypeInfo{
			Provider:     models.CloudProviderGCP,
			InstanceType: itype,
			VCPU:         spec.VCPU,
			MemoryGB:     spec.MemoryGB,
			GPUCount:     gpuCount,
			GPUType:      gpuType,
			Region:       region,
			Available:    true,
		})
	}
	return results, nil
}

// --------------------------------------------------------------------------
// CheckAvailability
// --------------------------------------------------------------------------

// CheckAvailability checks whether an instance type or storage type is available
// in the given region.
func (p *Provider) CheckAvailability(
	ctx context.Context,
	service string,
	skuOrType string,
	region string,
) (bool, []string, error) {
	switch service {
	case "compute":
		prices, err := p.GetComputePrice(ctx, skuOrType, region, "Linux", models.PricingTermOnDemand)
		if err != nil {
			return false, nil, nil //nolint:nilerr
		}
		return len(prices) > 0, nil, nil
	case "storage":
		prices, err := p.GetStoragePrice(ctx, skuOrType, region, 0)
		if err != nil {
			return false, nil, nil //nolint:nilerr
		}
		return len(prices) > 0, nil, nil
	}
	return false, nil, nil
}

// --------------------------------------------------------------------------
// STUB methods — Part 2/3 to implement
// --------------------------------------------------------------------------

// GetEffectivePrice is a stub; Part 2 implements account-level CUD effective prices.
func (p *Provider) GetEffectivePrice(
	ctx context.Context,
	spec models.PricingSpec,
) ([]models.EffectivePrice, error) {
	return nil, providers.ErrNotSupported
}

// GetSpotHistory is a stub; GCP does not offer a spot history API.
func (p *Provider) GetSpotHistory(
	ctx context.Context,
	spec models.PricingSpec,
	hours int,
	availabilityZone string,
) (map[string]any, error) {
	return nil, providers.ErrNotSupported
}

// GetDiscountSummary returns a structured "not_supported" response (mirrors Python base behaviour).
func (p *Provider) GetDiscountSummary(ctx context.Context) (map[string]any, error) {
	return map[string]any{
		"error":    "not_supported",
		"provider": "gcp",
		"reason":   "GCP does not expose a discount summary API. Use GetPrice with term='cud_1yr' or 'cud_3yr' to see committed-use prices.",
	}, nil
}

// DescribeCatalog is a stub; Part 3 implements the full catalog descriptor.
// DescribeCatalog returns the full GCP provider catalog.
// Implementation is in gcp_catalog.go.
func (p *Provider) DescribeCatalog(ctx context.Context) (*models.ProviderCatalog, error) {
	return gcpDescribeCatalog(), nil
}

// BOMAdvisories returns egress advisory rows when data-transfer services appear in the BoM.
// Implementation is in gcp_catalog.go.
func (p *Provider) BOMAdvisories(
	ctx context.Context,
	services []string,
	sampleRegion string,
) ([]map[string]string, error) {
	return gcpBOMAdvisories(services, sampleRegion), nil
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// stampCacheAge sets CacheAgeSeconds on each price relative to FetchedAt.
func stampCacheAge(prices []models.NormalizedPrice) []models.NormalizedPrice {
	now := time.Now().UTC()
	for i := range prices {
		if prices[i].FetchedAt != nil {
			age := int(now.Sub(*prices[i].FetchedAt).Seconds())
			if age < 0 {
				age = 0
			}
			prices[i].CacheAgeSeconds = &age
		}
	}
	return prices
}

// sortedFamilyKeys returns the sorted list of supported machine families.
func sortedFamilyKeys() []string {
	keys := make([]string, 0, len(utils.GCPFamilySKU))
	for k := range utils.GCPFamilySKU {
		keys = append(keys, k)
	}
	// Simple insertion sort for a small map.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// sortedStorageSKUKeys returns the sorted list of supported PD storage types.
func sortedStorageSKUKeys() []string {
	keys := make([]string, 0, len(utils.GCPStorageSKU))
	for k := range utils.GCPStorageSKU {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
