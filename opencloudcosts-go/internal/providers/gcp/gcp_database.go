// gcp_database.go — GCP database, cache, and container pricing helpers (Part 2).
//
// Covered domains:
//   - database/cloud_sql  : Cloud SQL for MySQL and PostgreSQL
//   - database/memorystore: Memorystore for Redis (basic and standard tiers)
//   - container/gke       : GKE standard (cluster management fee) and autopilot
//     (per-mCPU-hour + per-GiB-hour pod resource rates)
//
// All methods are on *Provider, which is defined in gcp.go (Part 1).
package gcp

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/utils"
)

// GCP Cloud Billing Catalog service IDs for Part 2 domains.
const (
	cloudSQLServiceID    = "9662-B51E-5089"
	gkeServiceID         = "CCD8-9BF1-090E"
	memorystoreServiceID = "5AF5-2C11-D467"
)

// cloudSQLEngineNames normalises the user-supplied engine string to the
// canonical form used in Cloud SQL SKU descriptions.
var cloudSQLEngineNames = map[string]string{
	"mysql":      "MySQL",
	"postgresql": "PostgreSQL",
	"postgres":   "PostgreSQL",
	"pg":         "PostgreSQL",
	"sqlserver":  "SQL Server",
	"mssql":      "SQL Server",
}

// --------------------------------------------------------------------------
// Cloud SQL pricing
// --------------------------------------------------------------------------

// buildCloudSQLIndex fetches Cloud SQL SKUs and builds a (desc, usageType) → price index.
// Results are cached under the key gcp:cloud_sql_index:<region>.
func (p *Provider) buildCloudSQLIndex(ctx context.Context, region string) (priceIndex, error) {
	cacheKey := "gcp:cloud_sql_index:" + region
	if idx, ok := p.loadPriceIndex(cacheKey); ok {
		return idx, nil
	}

	skus, err := p.fetchSKUs(ctx, cloudSQLServiceID)
	if err != nil {
		return nil, fmt.Errorf("gcp cloud sql: fetch SKUs: %w", err)
	}

	idx := make(priceIndex)
	for _, sku := range skus {
		regions, _ := sku["serviceRegions"].([]any)
		if !skuMatchesRegion(regions, region) {
			continue
		}
		desc, _ := sku["description"].(string)
		cat, _ := sku["category"].(map[string]any)
		usageType := "OnDemand"
		if cat != nil {
			if u, ok := cat["usageType"].(string); ok && u != "" {
				usageType = u
			}
		}
		price := skuPrice(sku)
		if price > 0 {
			idx[[2]string{desc, usageType}] = price
		}
	}

	p.savePriceIndex(cacheKey, idx, p.cfg.MetadataTTL())
	return idx, nil
}

// getCloudSQLPrice returns pricing for a Cloud SQL instance.
//
// Parameters:
//   - instanceType: Cloud SQL instance tier, e.g. "db-n1-standard-4"
//   - region:       GCP region, e.g. "us-central1"
//   - engine:       "mysql", "postgresql", or "postgres"
//   - ha:           true → Regional (HA) deployment, false → Zonal
func (p *Provider) getCloudSQLPrice(
	ctx context.Context,
	instanceType, region, engine string,
	ha bool,
) ([]models.NormalizedPrice, error) {
	specs, ok := utils.CloudSQLInstanceSpecs[strings.ToLower(instanceType)]
	if !ok {
		return nil, fmt.Errorf("unknown Cloud SQL instance type %q", instanceType)
	}
	vcpus := specs[0]
	memoryGB := specs[1]

	engineNorm := cloudSQLEngineNames[strings.ToLower(engine)]
	if engineNorm == "" {
		engineNorm = engine
	}

	zoneType := "Zonal"
	haLabel := "Zonal"
	if ha {
		zoneType = "Regional"
		haLabel = "HA/Regional"
	}

	// Build the SKU description prefix and size pattern to match.
	// GCP SKU format: "Cloud SQL for MySQL: Zonal - 4 vCPU + 15GB RAM in Americas"
	enginePrefix := strings.ToLower(fmt.Sprintf("cloud sql for %s: %s -", engineNorm, zoneType))

	// Format vCPU and RAM: drop trailing ".0" for whole numbers.
	vcpuStr := strFloat(vcpus)
	ramStr := strFloat(memoryGB)
	sizePattern := strings.ToLower(fmt.Sprintf("%s vcpu + %sgb ram", vcpuStr, ramStr))

	idx, err := p.buildCloudSQLIndex(ctx, region)
	if err != nil {
		return nil, err
	}

	var totalPrice float64
	found := false
	for k, price := range idx {
		desc, utype := k[0], k[1]
		if utype != "OnDemand" {
			continue
		}
		dl := strings.ToLower(desc)
		if strings.Contains(dl, enginePrefix) && strings.Contains(dl, sizePattern) {
			totalPrice = price
			found = true
			break
		}
	}

	if !found {
		slog.Warn("gcp: Cloud SQL SKU not found",
			"instanceType", instanceType,
			"engine", engineNorm,
			"region", region,
			"ha", ha,
		)
		return nil, nil
	}

	haStr := "false"
	haKey := "zonal"
	if ha {
		haStr = "true"
		haKey = "ha"
	}
	result := models.NormalizedPrice{
		Provider:      models.CloudProviderGCP,
		Service:       "database",
		SKUID:         fmt.Sprintf("gcp:cloud_sql:%s:%s:%s:%s", engineNorm, instanceType, region, haKey),
		ProductFamily: "Cloud SQL",
		Description:   fmt.Sprintf("Cloud SQL %s %s (%s)", engineNorm, instanceType, haLabel),
		Region:        region,
		Attributes: map[string]string{
			"instanceType": instanceType,
			"engine":       engineNorm,
			"ha":           haStr,
		},
		PricingTerm:  models.PricingTermOnDemand,
		PricePerUnit: totalPrice,
		Unit:         models.PriceUnitPerHour,
		Currency:     "USD",
	}
	return annotateFresh([]models.NormalizedPrice{result}), nil
}

// --------------------------------------------------------------------------
// Memorystore for Redis pricing
// --------------------------------------------------------------------------

// buildMemstoreIndex fetches Memorystore SKUs and builds a (desc, usageType) → price index.
func (p *Provider) buildMemstoreIndex(ctx context.Context, region string) (priceIndex, error) {
	cacheKey := "gcp:memorystore_index_v2:" + region
	if idx, ok := p.loadPriceIndex(cacheKey); ok {
		return idx, nil
	}

	skus, err := p.fetchSKUs(ctx, memorystoreServiceID)
	if err != nil {
		return nil, fmt.Errorf("gcp memorystore: fetch SKUs: %w", err)
	}

	idx := make(priceIndex)
	for _, sku := range skus {
		regions, _ := sku["serviceRegions"].([]any)
		if !skuMatchesRegion(regions, region) {
			continue
		}
		desc, _ := sku["description"].(string)
		cat, _ := sku["category"].(map[string]any)
		usageType := "OnDemand"
		if cat != nil {
			if u, ok := cat["usageType"].(string); ok && u != "" {
				usageType = u
			}
		}
		price := skuPrice(sku)
		if price > 0 {
			idx[[2]string{desc, usageType}] = price
		}
	}

	p.savePriceIndex(cacheKey, idx, p.cfg.MetadataTTL())
	return idx, nil
}

// getMemstorePrice returns pricing for a Memorystore for Redis instance.
//
// Memorystore is billed per GiB-hour of provisioned capacity. The M-tier
// (m2/m3/m4/m5) is selected automatically based on capacity:
//
//	< 4 GB   → m2
//	< 12 GB  → m3
//	< 100 GB → m4
//	≥ 100 GB → m5
func (p *Provider) getMemstorePrice(
	ctx context.Context,
	capacityGB float64,
	region string,
	tier string, // "basic" or "standard"
	hoursPerMonth float64,
) ([]models.NormalizedPrice, error) {
	if capacityGB <= 0 {
		return nil, fmt.Errorf("capacityGB must be positive, got %v", capacityGB)
	}
	tierLower := strings.ToLower(tier)
	if tierLower != "basic" && tierLower != "standard" {
		return nil, fmt.Errorf("invalid Memorystore tier %q: must be 'basic' or 'standard'", tier)
	}

	// Select preferred M-tier by capacity.
	var preferredM string
	switch {
	case capacityGB < 4:
		preferredM = "m2"
	case capacityGB < 12:
		preferredM = "m3"
	case capacityGB < 100:
		preferredM = "m4"
	default:
		preferredM = "m5"
	}

	idx, err := p.buildMemstoreIndex(ctx, region)
	if err != nil {
		return nil, err
	}

	// Try preferred M-tier first, then fall back to others.
	allMTiers := []string{"m2", "m3", "m4", "m5"}
	mTierOrder := []string{preferredM}
	for _, m := range allMTiers {
		if m != preferredM {
			mTierOrder = append(mTierOrder, m)
		}
	}

	var rawRate float64
	found := false
	for _, mTier := range mTierOrder {
		for k, price := range idx {
			desc, utype := k[0], k[1]
			if utype != "OnDemand" {
				continue
			}
			dl := strings.ToLower(desc)
			if tierLower == "basic" {
				if strings.Contains(dl, "redis capacity basic "+mTier) {
					rawRate = price
					found = true
					break
				}
			} else {
				// Exclude Redis Cluster ("redis standard node capacity") — match only
				// the classic HA tier "redis capacity standard mX".
				if strings.Contains(dl, "redis capacity standard "+mTier) {
					rawRate = price
					found = true
					break
				}
			}
		}
		if found {
			break
		}
	}

	if !found {
		slog.Warn("gcp: Memorystore SKU not found",
			"capacityGB", capacityGB,
			"tier", tier,
			"region", region,
		)
		return nil, nil
	}

	// Memorystore is billed per GiB-hour × capacity.
	hourlyRate := rawRate * capacityGB

	tierTitle := strings.ToUpper(tierLower[:1]) + tierLower[1:]
	result := models.NormalizedPrice{
		Provider:      models.CloudProviderGCP,
		Service:       "database",
		SKUID:         fmt.Sprintf("gcp:memorystore:%s:%.3f:%s", tierLower, capacityGB, region),
		ProductFamily: "Memorystore for Redis",
		Description:   fmt.Sprintf("Memorystore Redis %s %.0fGB in %s", tierTitle, capacityGB, region),
		Region:        region,
		Attributes: map[string]string{
			"capacity_gb":     fmt.Sprintf("%v", capacityGB),
			"tier":            tierLower,
			"rate_per_gib_hr": fmt.Sprintf("%v", rawRate),
		},
		PricingTerm:  models.PricingTermOnDemand,
		PricePerUnit: hourlyRate,
		Unit:         models.PriceUnitPerHour,
		Currency:     "USD",
	}
	return annotateFresh([]models.NormalizedPrice{result}), nil
}

// --------------------------------------------------------------------------
// GKE (Google Kubernetes Engine) pricing
// --------------------------------------------------------------------------

// buildGKEIndex fetches GKE SKUs and builds a (desc, usageType) → price index.
func (p *Provider) buildGKEIndex(ctx context.Context, region string) (priceIndex, error) {
	cacheKey := "gcp:gke_index:" + region
	if idx, ok := p.loadPriceIndex(cacheKey); ok {
		return idx, nil
	}

	skus, err := p.fetchSKUs(ctx, gkeServiceID)
	if err != nil {
		return nil, fmt.Errorf("gcp gke: fetch SKUs: %w", err)
	}

	idx := make(priceIndex)
	for _, sku := range skus {
		regions, _ := sku["serviceRegions"].([]any)
		if !skuMatchesRegion(regions, region) {
			continue
		}
		desc, _ := sku["description"].(string)
		cat, _ := sku["category"].(map[string]any)
		usageType := "OnDemand"
		if cat != nil {
			if u, ok := cat["usageType"].(string); ok && u != "" {
				usageType = u
			}
		}
		price := skuPrice(sku)
		if price > 0 {
			idx[[2]string{desc, usageType}] = price
		}
	}

	p.savePriceIndex(cacheKey, idx, p.cfg.MetadataTTL())
	return idx, nil
}

// getGKEPrice returns GKE pricing info for either standard or autopilot mode.
//
// Standard mode returns NormalizedPrice for the cluster management fee.
// Autopilot mode returns NormalizedPrices for per-vCPU-hr and per-GiB-hr rates.
// A breakdown map is always returned for additional context.
func (p *Provider) getGKEPrice(
	ctx context.Context,
	region string,
	mode string,
	nodeType string,
	nodeCount int,
	vcpu float64,
	memoryGB float64,
	hoursPerMonth float64,
) ([]models.NormalizedPrice, map[string]any, error) {
	idx, err := p.buildGKEIndex(ctx, region)
	if err != nil {
		return nil, nil, err
	}

	if strings.ToLower(mode) == "autopilot" {
		return p.gkeAutopilotPrice(idx, region, vcpu, memoryGB, hoursPerMonth)
	}
	return p.gkeStandardPrice(idx, region, nodeType, nodeCount, hoursPerMonth)
}

// gkeStandardPrice handles standard-mode GKE pricing.
func (p *Provider) gkeStandardPrice(
	idx priceIndex,
	region string,
	nodeType string,
	nodeCount int,
	hoursPerMonth float64,
) ([]models.NormalizedPrice, map[string]any, error) {
	const fallbackRate = 0.10 // documented GCP on-demand rate

	var rate float64
	fromFallback := true
	for k, price := range idx {
		desc, utype := k[0], k[1]
		if utype != "OnDemand" {
			continue
		}
		dl := strings.ToLower(desc)
		if strings.Contains(dl, "kubernetes engine") &&
			!strings.Contains(dl, "autopilot") &&
			!strings.Contains(dl, "committed") &&
			!strings.Contains(dl, "cost attribution") {
			rate = price
			fromFallback = false
			break
		}
	}
	if fromFallback {
		rate = fallbackRate
	}

	monthlyFee := rate * hoursPerMonth
	breakdown := map[string]any{
		"mode":                          "standard",
		"cluster_management_fee_per_hr": fmt.Sprintf("$%.6f", rate),
		"cluster_management_monthly":    fmt.Sprintf("$%.2f", monthlyFee),
		"node_pricing_hint": fmt.Sprintf(
			"Use GetComputePrice(provider='gcp', instance_type='%s', region='%s') for each node. Multiply by %d nodes.",
			nodeType, region, nodeCount,
		),
		"total_monthly_note": fmt.Sprintf(
			"Total = cluster fee + (%d × node_hourly × %.0f hrs)",
			nodeCount, hoursPerMonth,
		),
		"region": region,
	}
	if fromFallback {
		breakdown["fallback"] = true
	}

	price := models.NormalizedPrice{
		Provider:      models.CloudProviderGCP,
		Service:       "container",
		SKUID:         fmt.Sprintf("gcp:gke:standard:cluster_mgmt:%s", region),
		ProductFamily: "GKE",
		Description:   fmt.Sprintf("GKE Standard Cluster Management Fee in %s", region),
		Region:        region,
		Attributes: map[string]string{
			"mode":       "standard",
			"node_type":  nodeType,
			"node_count": fmt.Sprintf("%d", nodeCount),
		},
		PricingTerm:  models.PricingTermOnDemand,
		PricePerUnit: rate,
		Unit:         models.PriceUnitPerHour,
		Currency:     "USD",
	}
	return annotateFresh([]models.NormalizedPrice{price}), breakdown, nil
}

// gkeAutopilotPrice handles autopilot-mode GKE pricing.
func (p *Provider) gkeAutopilotPrice(
	idx priceIndex,
	region string,
	vcpu float64,
	memoryGB float64,
	hoursPerMonth float64,
) ([]models.NormalizedPrice, map[string]any, error) {
	if vcpu < 0 {
		return nil, nil, fmt.Errorf("vcpu must be non-negative, got %v", vcpu)
	}
	if memoryGB < 0 {
		return nil, nil, fmt.Errorf("memoryGB must be non-negative, got %v", memoryGB)
	}

	var mcpuRate, memRate float64

	for k, price := range idx {
		desc := strings.ToLower(k[0])
		if strings.Contains(desc, "autopilot balanced pod mcpu requests") && mcpuRate == 0 {
			mcpuRate = price
		}
		if strings.Contains(desc, "autopilot balanced pod memory requests") && memRate == 0 {
			memRate = price
		}
		if mcpuRate != 0 && memRate != 0 {
			break
		}
	}

	// mCPU rate is per milli-CPU — multiply by 1000 to get per-vCPU rate.
	vcpuRate := mcpuRate * 1000

	hourly := vcpu*vcpuRate + memoryGB*memRate
	monthly := hourly * hoursPerMonth

	breakdown := map[string]any{
		"mode":                   "autopilot",
		"vcpu_rate_per_hr":       fmt.Sprintf("$%.6f", vcpuRate),
		"memory_rate_per_gib_hr": fmt.Sprintf("$%.6f", memRate),
		"requested_vcpu":         vcpu,
		"requested_memory_gb":    memoryGB,
		"hourly_cost":            fmt.Sprintf("$%.6f", hourly),
		"monthly_cost":           fmt.Sprintf("$%.2f", monthly),
		"region":                 region,
		"note":                   "Autopilot charges for actual pod resource requests only",
	}

	var prices []models.NormalizedPrice
	if vcpuRate > 0 {
		prices = append(prices, models.NormalizedPrice{
			Provider:      models.CloudProviderGCP,
			Service:       "container",
			SKUID:         fmt.Sprintf("gcp:gke:autopilot:vcpu:%s", region),
			ProductFamily: "GKE Autopilot",
			Description:   fmt.Sprintf("GKE Autopilot Balanced Pod vCPU in %s", region),
			Region:        region,
			Attributes:    map[string]string{"mode": "autopilot", "resource": "vcpu"},
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  vcpuRate,
			Unit:          models.PriceUnitPerHour,
			Currency:      "USD",
		})
	}
	if memRate > 0 {
		prices = append(prices, models.NormalizedPrice{
			Provider:      models.CloudProviderGCP,
			Service:       "container",
			SKUID:         fmt.Sprintf("gcp:gke:autopilot:memory:%s", region),
			ProductFamily: "GKE Autopilot",
			Description:   fmt.Sprintf("GKE Autopilot Balanced Pod Memory in %s", region),
			Region:        region,
			Attributes:    map[string]string{"mode": "autopilot", "resource": "memory"},
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  memRate,
			Unit:          models.PriceUnitPerHour,
			Currency:      "USD",
		})
	}
	return annotateFresh(prices), breakdown, nil
}

// --------------------------------------------------------------------------
// GetPrice dispatch for Part 2 domains
// --------------------------------------------------------------------------

// getPart2Price dispatches database/container domain specs. Called from GetPrice
// in gcp_compute.go after the compute/storage check.
func (p *Provider) getPart2Price(ctx context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
	region := spec.GetRegion()
	if region == "" {
		region = p.DefaultRegion()
	}

	switch s := spec.(type) {
	case *models.DatabasePricingSpec:
		return p.getDatabasePrice(ctx, s, region)
	case *models.ContainerPricingSpec:
		return p.getContainerPrice(ctx, s, region)
	}
	return nil, nil
}

func (p *Provider) getDatabasePrice(ctx context.Context, s *models.DatabasePricingSpec, region string) (*models.PricingResult, error) {
	svc := strings.ToLower(s.Service)
	switch {
	case svc == "cloud_sql" || svc == "mysql" || svc == "postgresql" || svc == "postgres" || svc == "":
		ha := strings.Contains(strings.ToLower(s.Deployment), "ha") ||
			strings.Contains(strings.ToLower(s.Deployment), "regional") ||
			strings.Contains(strings.ToLower(s.Deployment), "multi")
		engine := s.Engine
		if engine == "" {
			engine = "MySQL"
		}
		prices, err := p.getCloudSQLPrice(ctx, s.ResourceType, region, engine, ha)
		if err != nil {
			return nil, err
		}
		return buildResult(prices, nil), nil

	case svc == "memorystore" || svc == "redis":
		capacityGB := 1.0
		if s.CapacityGB != nil {
			capacityGB = *s.CapacityGB
		}
		tier := "standard"
		if strings.Contains(strings.ToLower(s.Deployment), "basic") {
			tier = "basic"
		}
		prices, err := p.getMemstorePrice(ctx, capacityGB, region, tier, s.HoursPerMonth)
		if err != nil {
			return nil, err
		}
		return buildResult(prices, nil), nil

	default:
		return nil, fmt.Errorf("GCP database service %q not supported", s.Service)
	}
}

func (p *Provider) getContainerPrice(ctx context.Context, s *models.ContainerPricingSpec, region string) (*models.PricingResult, error) {
	var vcpu, memGB float64
	if s.VCPU != nil {
		vcpu = *s.VCPU
	}
	if s.MemoryGB != nil {
		memGB = *s.MemoryGB
	}
	prices, breakdown, err := p.getGKEPrice(ctx, region, s.Mode, s.NodeType, s.NodeCount, vcpu, memGB, s.HoursPerMonth)
	if err != nil {
		return nil, err
	}
	return buildResult(prices, breakdown), nil
}

// --------------------------------------------------------------------------
// Utility
// --------------------------------------------------------------------------

// strFloat formats a float64 as a compact string, dropping trailing ".0".
func strFloat(f float64) string {
	if f == math.Trunc(f) {
		return fmt.Sprintf("%.0f", f)
	}
	return fmt.Sprintf("%g", f)
}
