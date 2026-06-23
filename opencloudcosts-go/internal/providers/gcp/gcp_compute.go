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
	cpuKey    string // "cpu" | "preemptCPU" | "cudCPU"
	ramKey    string // "ram" | "preemptRAM" | "cudRAM"
}

var termUsageType = map[models.PricingTerm]termMapping{
	models.PricingTermOnDemand: {"OnDemand", "cpu", "ram"},
	models.PricingTermSpot:     {"Preemptible", "preemptCPU", "preemptRAM"},
	models.PricingTermCUD1Yr:   {"Commit1Yr", "cudCPU", "cudRAM"},
	models.PricingTermCUD3Yr:   {"Commit3Yr", "cudCPU", "cudRAM"},
}

// familySKUDesc returns the CPU / RAM description strings from the spec-based mapping.
func familySKUDesc(fsku utils.FamilySKU, cpuKey, ramKey string) (string, string) {
	switch cpuKey {
	case "preemptCPU":
		return fsku.PreemptCPUDesc, fsku.PreemptRAMDesc
	case "cudCPU":
		return fsku.CUDCPUDesc, fsku.CUDRAMDesc
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
		case "", "vertex_ai", "gemini":
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
		case "", "cloud_lb", "cloud_cdn", "cloud_nat", "cloud_armor", "egress":
			return true
		}
	case models.PricingDomainObservability:
		switch service {
		case "", "cloud_monitoring":
			return true
		}
	case models.PricingDomainInterRegionEgress:
		return true
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
		sizeGB := 0.0
		if s.SizeGB != nil {
			sizeGB = *s.SizeGB
		}
		prices, err := p.GetStoragePrice(ctx, storageType, s.Region, sizeGB)
		if err != nil {
			return nil, err
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
		return nil, fmt.Errorf("gcp: machine family %q is not supported; supported: %v",
			family, sortedFamilyKeys())
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

	totalPrice := float64(vcpus)*cpuPrice + memGB*ramPrice

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
	gpuFamilies := map[string]bool{"a2": true}

	var results []models.InstanceTypeInfo
	for itype, spec := range utils.GCPInstanceSpecs {
		fam := utils.GetMachineFamily(itype)
		isGPU := gpuFamilies[fam]

		if gpu && !isGPU {
			continue
		}
		if !gpu && isGPU {
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
		if isGPU {
			gpuCount = 1
		}
		results = append(results, models.InstanceTypeInfo{
			Provider:     models.CloudProviderGCP,
			InstanceType: itype,
			VCPU:         spec.VCPU,
			MemoryGB:     spec.MemoryGB,
			GPUCount:     gpuCount,
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
