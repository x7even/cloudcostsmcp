// compare_bom.go implements the compare_bom MCP tool, which prices a
// cloud-agnostic workload spec across multiple providers in a single call.
//
// Design intent: CCR2-type queries ("compare 3-tier stack across all 3 clouds")
// required 6 sequential estimate_bom calls (3 providers × 2 terms), accumulating
// 12+ rounds of context before the model could summarise. compare_bom reduces
// this to 1 call by running all (provider × term) combinations concurrently
// and returning a unified side-by-side comparison.
//
// Limitations disclosed in the tool output:
//   - Instance type selection is a nearest-fit from a static lookup table;
//     exact matches vary per provider.
//   - Database and cache types map to the closest equivalent service
//     (RDS MySQL / Cloud SQL / Azure SQL).
package tools

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// --------------------------------------------------------------------------
// Input types
// --------------------------------------------------------------------------

// WorkloadItem describes one cloud-agnostic resource in the workload spec.
type WorkloadItem struct {
	// Type is the resource category: "compute", "storage", "database", "cache".
	Type string `json:"type"`
	// VCPUs is the number of virtual CPUs (compute / database / cache).
	VCPUs float64 `json:"vcpus"`
	// MemoryGB is the memory in GB (compute / database / cache).
	MemoryGB float64 `json:"memory_gb"`
	// Quantity is the number of instances (default 1). Fractional values are
	// supported (e.g. partial-month averages, fractional replica counts).
	Quantity float64 `json:"quantity"`
	// Engine is the database/cache engine (default "mysql").
	Engine string `json:"engine"`
	// StorageGB is the storage size in GB (storage / database).
	StorageGB float64 `json:"storage_gb"`
	// StorageType is the storage category: "ssd" (default), "hdd", "ssd-provisioned-iops".
	StorageType string `json:"storage_type"`
	// IOPS is the provisioned IOPS count for storage types that support it (e.g. ssd-provisioned-iops).
	IOPS int `json:"iops"`
	// ThroughputMBPS is the provisioned throughput in MB/s for storage types that support it.
	ThroughputMBPS float64 `json:"throughput_mbps"`
	// OS is the operating system for compute: "linux" (default) or "windows".
	OS string `json:"os"`
}

// CompareBOMInput is the typed input for the compare_bom tool.
type CompareBOMInput struct {
	// Providers lists which providers to include. Defaults to all three.
	Providers []string `json:"providers"`
	// RegionPreference selects the region tier: "us" (default), "eu", "apac".
	RegionPreference string `json:"region_preference"`
	// Workload is a map of logical name → workload item.
	Workload map[string]WorkloadItem `json:"workload"`
	// Terms lists pricing terms to compare. Defaults to ["on_demand", "reserved_1yr"].
	Terms []string `json:"terms"`
}

// --------------------------------------------------------------------------
// Static lookup tables
// --------------------------------------------------------------------------

// compareBOMRegionMap maps (provider, region_preference) → region code.
var compareBOMRegionMap = map[string]map[string]string{
	"aws": {
		"us":   "us-east-1",
		"eu":   "eu-west-1",
		"apac": "ap-southeast-1",
	},
	"gcp": {
		"us":   "us-central1",
		"eu":   "europe-west1",
		"apac": "asia-southeast1",
	},
	"azure": {
		"us":   "eastus",
		"eu":   "westeurope",
		"apac": "southeastasia",
	},
}

// compareBOMTermMap maps a canonical term to a per-provider term string.
// If no entry exists, the canonical term is passed through unchanged.
var compareBOMTermMap = map[string]map[string]string{
	"reserved_1yr": {
		"aws":   "reserved_1yr",
		"gcp":   "cud_1yr",
		"azure": "reserved_1yr",
	},
	"reserved_3yr": {
		"aws":   "reserved_3yr",
		"gcp":   "cud_3yr",
		"azure": "reserved_3yr",
	},
	"cud_1yr": {
		"aws":   "reserved_1yr",
		"gcp":   "cud_1yr",
		"azure": "reserved_1yr",
	},
	"cud_3yr": {
		"aws":   "reserved_3yr",
		"gcp":   "cud_3yr",
		"azure": "reserved_3yr",
	},
}

// computeInstanceTable holds (vcpu → instance_type) for each provider.
// Key: vcpu count. Values cover the general-purpose tier.
// Memory-optimized (>6 GB/vCPU) uses a separate highmem table.
var computeInstanceTableGP = map[string]map[int]string{
	"aws": {
		1:  "t3.small",
		2:  "m5.large",
		4:  "m5.xlarge",
		8:  "m5.2xlarge",
		16: "m5.4xlarge",
		32: "m5.8xlarge",
		48: "m5.12xlarge",
		64: "m5.16xlarge",
	},
	"gcp": {
		1:  "n2-standard-2", // GCP minimum for n2-standard is 2 vCPU
		2:  "n2-standard-2",
		4:  "n2-standard-4",
		8:  "n2-standard-8",
		16: "n2-standard-16",
		32: "n2-standard-32",
		48: "n2-standard-48",
		64: "n2-standard-64",
	},
	"azure": {
		1:  "Standard_D2s_v3",
		2:  "Standard_D2s_v3",
		4:  "Standard_D4s_v3",
		8:  "Standard_D8s_v3",
		16: "Standard_D16s_v3",
		32: "Standard_D32s_v3",
		48: "Standard_D48s_v3",
		64: "Standard_D64s_v3",
	},
}

// computeInstanceTableHM holds highmem instance types (>6 GB/vCPU).
var computeInstanceTableHM = map[string]map[int]string{
	"aws": {
		1:  "r5.large",
		2:  "r5.large",
		4:  "r5.xlarge",
		8:  "r5.2xlarge",
		16: "r5.4xlarge",
		32: "r5.8xlarge",
		48: "r5.12xlarge",
		64: "r5.16xlarge",
	},
	"gcp": {
		1:  "n2-highmem-2",
		2:  "n2-highmem-2",
		4:  "n2-highmem-4",
		8:  "n2-highmem-8",
		16: "n2-highmem-16",
		32: "n2-highmem-32",
		48: "n2-highmem-48",
		64: "n2-highmem-64",
	},
	"azure": {
		1:  "Standard_E2s_v3",
		2:  "Standard_E2s_v3",
		4:  "Standard_E4s_v3",
		8:  "Standard_E8s_v3",
		16: "Standard_E16s_v3",
		32: "Standard_E32s_v3",
		48: "Standard_E48s_v3",
		64: "Standard_E64s_v3",
	},
}

// dbInstanceTable holds (vcpu → db_instance_type) for each provider.
var dbInstanceTable = map[string]map[int]string{
	"aws": {
		1:  "db.t4g.micro",
		2:  "db.t4g.medium",
		4:  "db.m5.xlarge",
		8:  "db.m5.2xlarge",
		16: "db.m5.4xlarge",
		32: "db.m5.8xlarge",
	},
	"gcp": {
		1:  "db-n1-standard-2", // Cloud SQL minimum is 1 vCPU, but use standard-2 for clarity
		2:  "db-n1-standard-2",
		4:  "db-n1-standard-4",
		8:  "db-n1-standard-8",
		16: "db-n1-standard-16",
		32: "db-n1-standard-32",
	},
	"azure": {
		1:  "GP_Gen5_2",
		2:  "GP_Gen5_2",
		4:  "GP_Gen5_4",
		8:  "GP_Gen5_8",
		16: "GP_Gen5_16",
		32: "GP_Gen5_32",
	},
}

// dbServiceTable maps provider → service name for relational databases.
var dbServiceTable = map[string]string{
	"aws":   "rds",
	"gcp":   "cloud_sql",
	"azure": "sql",
}

// storageTypeTable maps (storage_type → provider → storage_type_code).
var storageTypeTable = map[string]map[string]string{
	"ssd": {
		"aws":   "gp3",
		"gcp":   "pd-ssd",
		"azure": "premium-ssd",
	},
	"hdd": {
		"aws":   "sc1",
		"gcp":   "pd-standard",
		"azure": "standard-hdd",
	},
	"ssd-provisioned-iops": {
		"aws":   "io1",
		"gcp":   "pd-ssd",
		"azure": "ultra-ssd",
	},
}

// --------------------------------------------------------------------------
// Helper functions
// --------------------------------------------------------------------------

// compareBOMRegion returns the provider-specific region for a region preference.
func compareBOMRegion(provider, regionPref string) string {
	if provMap, ok := compareBOMRegionMap[provider]; ok {
		if region, ok := provMap[regionPref]; ok {
			return region
		}
	}
	// Fallback defaults.
	switch provider {
	case "aws":
		return "us-east-1"
	case "gcp":
		return "us-central1"
	case "azure":
		return "eastus"
	default:
		return "us-east-1"
	}
}

// translateTerm converts a canonical term to the provider-specific term.
// If no mapping exists, the canonical term is returned unchanged.
func translateTerm(canonTerm, provider string) string {
	if provMap, ok := compareBOMTermMap[canonTerm]; ok {
		if mapped, ok := provMap[provider]; ok {
			return mapped
		}
	}
	return canonTerm
}

// closestVCPU returns the closest vCPU count from a table to the requested count.
// It rounds up to the nearest available key, or uses the largest available.
func closestVCPU(table map[int]string, vcpu float64) int {
	requested := int(math.Ceil(vcpu))
	if requested <= 0 {
		requested = 1
	}

	// Collect and sort all available keys.
	keys := make([]int, 0, len(table))
	for k := range table {
		keys = append(keys, k)
	}
	sort.Ints(keys)

	// Find the smallest key >= requested, or return the largest key.
	for _, k := range keys {
		if k >= requested {
			return k
		}
	}
	return keys[len(keys)-1]
}

// pickComputeInstance returns the best-fit instance type for the workload.
// Uses the highmem table when memory_gb/vcpu > 6 GB/vCPU.
func pickComputeInstance(provider string, vcpus, memGB float64) string {
	useHM := false
	if vcpus > 0 && memGB/vcpus > 6.0 {
		useHM = true
	}

	var table map[int]string
	if useHM {
		table = computeInstanceTableHM[provider]
	} else {
		table = computeInstanceTableGP[provider]
	}
	if table == nil {
		table = computeInstanceTableGP[provider]
	}

	vcpu := closestVCPU(table, vcpus)
	if it, ok := table[vcpu]; ok {
		return it
	}
	// Hardcoded fallbacks per provider.
	switch provider {
	case "aws":
		return "m5.xlarge"
	case "gcp":
		return "n2-standard-4"
	default:
		return "Standard_D4s_v3"
	}
}

// pickDBInstance returns the best-fit database instance type.
func pickDBInstance(provider string, vcpus float64) string {
	table := dbInstanceTable[provider]
	if table == nil {
		return ""
	}
	vcpu := closestVCPU(table, vcpus)
	if it, ok := table[vcpu]; ok {
		return it
	}
	switch provider {
	case "aws":
		return "db.m5.xlarge"
	case "gcp":
		return "db-n1-standard-4"
	default:
		return "GP_Gen5_4"
	}
}

// pickStorageType returns the provider-specific storage type code.
func pickStorageType(provider, storageType string) string {
	st := strings.ToLower(storageType)
	if st == "" {
		st = "ssd"
	}
	if stMap, ok := storageTypeTable[st]; ok {
		if code, ok := stMap[provider]; ok {
			return code
		}
	}
	// If the caller supplied a provider-specific type code (gp3, io2, sc1, pd-ssd,
	// pd-extreme, hyperdisk-extreme, etc.), pass it through unchanged rather than
	// silently mapping to an SSD default and returning a wrong price.
	// The check is provider-scoped so that AWS type codes are never forwarded to
	// GCP/Azure in compare_bom's multi-provider fan-out (and vice versa).
	providerTypes := map[string]map[string]bool{
		"aws": {
			"gp2": true, "gp3": true, "io1": true, "io2": true,
			"sc1": true, "st1": true, "standard": true,
		},
		"gcp": {
			"pd-standard": true, "pd-balanced": true, "pd-ssd": true,
			"pd-extreme": true, "hyperdisk-extreme": true, "hyperdisk-balanced": true,
			"hyperdisk-throughput": true,
		},
		"azure": {
			"premium-ssd": true, "standard-hdd": true, "ultra-ssd": true,
		},
	}
	if pt, ok := providerTypes[provider]; ok && pt[st] {
		return st
	}
	// Final fallback to SSD defaults for completely unknown types.
	switch provider {
	case "aws":
		return "gp3"
	case "gcp":
		return "pd-ssd"
	default:
		return "premium-ssd"
	}
}

// workloadItemToSpec converts a WorkloadItem to a BOM spec map for a
// specific provider, region, and term. Returns an error if the item type
// is not supported for this provider.
func workloadItemToSpec(item WorkloadItem, provider, region, term string) (map[string]any, string, error) {
	switch strings.ToLower(item.Type) {
	case "compute":
		vcpus := item.VCPUs
		if vcpus <= 0 {
			vcpus = 2
		}
		memGB := item.MemoryGB
		if memGB <= 0 {
			memGB = vcpus * 4 // default 4 GB/vCPU
		}
		instanceType := pickComputeInstance(provider, vcpus, memGB)
		os := item.OS
		if os == "" {
			os = "Linux"
		}
		spec := map[string]any{
			"provider":      provider,
			"domain":        "compute",
			"resource_type": instanceType,
			"region":        region,
			"term":          term,
			"os":            os,
		}
		return spec, instanceType, nil

	case "storage":
		sizeGB := item.StorageGB
		if sizeGB <= 0 {
			sizeGB = 100
		}
		stCode := pickStorageType(provider, item.StorageType)
		spec := map[string]any{
			"provider":     provider,
			"domain":       "storage",
			"storage_type": stCode,
			"size_gb":      sizeGB,
			"region":       region,
			"term":         "on_demand", // storage is always on-demand pricing
		}
		// Pass IOPS and throughput when the workload item specifies them so
		// that providers return per_iops_month / per_mbps_month price entries.
		if item.IOPS > 0 {
			spec["iops"] = item.IOPS
		}
		if item.ThroughputMBPS > 0 {
			spec["throughput_mbps"] = item.ThroughputMBPS
		}
		return spec, stCode, nil

	case "database":
		vcpus := item.VCPUs
		if vcpus <= 0 {
			vcpus = 4
		}
		dbType := pickDBInstance(provider, vcpus)
		if dbType == "" {
			return nil, "", fmt.Errorf("no database instance mapping for provider %q", provider)
		}
		svc := dbServiceTable[provider]
		if svc == "" {
			return nil, "", fmt.Errorf("no database service mapping for provider %q", provider)
		}
		engine := item.Engine
		if engine == "" {
			engine = "MySQL"
		}
		deployment := "single-az"
		if provider == "gcp" {
			deployment = "ha" // GCP Cloud SQL uses "ha" for single-region HA
		}
		spec := map[string]any{
			"provider":      provider,
			"domain":        "database",
			"service":       svc,
			"resource_type": dbType,
			"engine":        engine,
			"deployment":    deployment,
			"region":        region,
			"term":          term,
		}
		return spec, dbType, nil

	case "cache":
		// Map cache to provider-specific in-memory service.
		vcpus := item.VCPUs
		if vcpus <= 0 {
			vcpus = 2
		}
		switch provider {
		case "aws":
			// ElastiCache Redis — resource_type is node type, e.g. cache.t4g.micro
			// Simple mapping: use t4g or r6g family based on memory
			nodeType := "cache.t4g.medium"
			if vcpus >= 4 {
				nodeType = "cache.r6g.xlarge"
			}
			if vcpus >= 8 {
				nodeType = "cache.r6g.2xlarge"
			}
			spec := map[string]any{
				"provider":      "aws",
				"domain":        "database",
				"service":       "elasticache",
				"resource_type": nodeType,
				"engine":        "Redis",
				"deployment":    "single-az",
				"region":        region,
				"term":          term,
			}
			return spec, nodeType, nil
		case "gcp":
			// Memorystore Redis — use capacity_gb parameter
			capGB := item.MemoryGB
			if capGB <= 0 {
				capGB = float64(vcpus) * 4
			}
			spec := map[string]any{
				"provider":    "gcp",
				"domain":      "database",
				"service":     "memorystore",
				"capacity_gb": capGB,
				"region":      region,
				"term":        "on_demand", // Memorystore has no committed pricing
			}
			return spec, fmt.Sprintf("memorystore-%.0fgb", capGB), nil
		case "azure":
			// Azure Cache for Redis is not in the current catalog — report as skipped.
			return nil, "", fmt.Errorf(
				"azure cache (Redis) is not in the current catalog; use get_price with service='cosmos' for Azure data stores",
			)
		default:
			return nil, "", fmt.Errorf("unknown provider %q for cache type", provider)
		}

	default:
		return nil, "", fmt.Errorf("unsupported workload type %q (supported: compute, storage, database, cache)", item.Type)
	}
}

// canonicalTermLabel returns a consistent output key for a term, regardless of
// which provider-specific term was actually used. This keeps the output
// cross-provider comparable. The canonical term is what the caller passed in.
func canonicalTermLabel(term string) string {
	return term // already canonical from caller
}

// --------------------------------------------------------------------------
// compareBOMProviderResult is the result for one (provider, term) combination.
// --------------------------------------------------------------------------

type compareBOMTermResult struct {
	totalMonthly float64
	breakdown    map[string]float64
	errs         []string
}

// --------------------------------------------------------------------------
// HandleCompareBOM — compare_bom tool handler
// --------------------------------------------------------------------------

// HandleCompareBOM implements the compare_bom MCP tool.
func (h *Handler) HandleCompareBOM(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in CompareBOMInput,
) (*mcp.CallToolResult, any, error) {
	defer recoverToResult(ctx, "compare_bom")

	// --- Apply defaults ---

	providers := in.Providers
	if len(providers) == 0 {
		providers = []string{"aws", "gcp", "azure"}
	}
	// De-duplicate and sort for deterministic output.
	providers = dedupStrings(providers)
	sort.Strings(providers)

	regionPref := strings.ToLower(in.RegionPreference)
	if regionPref == "" {
		regionPref = "us"
	}

	terms := in.Terms
	if len(terms) == 0 {
		terms = []string{"on_demand", "reserved_1yr"}
	}

	if len(in.Workload) == 0 {
		return jsonText(map[string]any{
			"error": "workload is required and must contain at least one item",
		}), nil, nil
	}

	// Stable ordering of workload keys.
	workloadKeys := make([]string, 0, len(in.Workload))
	for k := range in.Workload {
		workloadKeys = append(workloadKeys, k)
	}
	sort.Strings(workloadKeys)

	// Ensure on_demand is always computed for savings calculation,
	// even if the caller did not request it.
	onDemandRequested := false
	for _, t := range terms {
		if t == "on_demand" {
			onDemandRequested = true
			break
		}
	}
	// internalTerms is the full list of terms we will compute pricing for.
	// It may include "on_demand" even if not in the requested output terms.
	internalTerms := terms
	if !onDemandRequested {
		internalTerms = append([]string{"on_demand"}, terms...)
	}

	// --- Fan-out: (provider × term) concurrently ---

	type taskKey struct {
		provider string
		term     string
	}
	type taskResult struct {
		key    taskKey
		result compareBOMTermResult
	}

	tasks := make([]taskKey, 0, len(providers)*len(internalTerms))
	for _, prov := range providers {
		for _, term := range internalTerms {
			tasks = append(tasks, taskKey{prov, term})
		}
	}

	taskResults := make([]taskResult, len(tasks))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8) // max 8 concurrent provider calls

	for i, tk := range tasks {
		wg.Add(1)
		go func(idx int, key taskKey) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			res := compareBOMTermResult{
				breakdown: make(map[string]float64),
			}

			pvdr := h.providers[key.provider]
			if pvdr == nil {
				res.errs = append(res.errs, fmt.Sprintf("provider '%s' not configured", key.provider))
				taskResults[idx] = taskResult{key: key, result: res}
				return
			}

			region := compareBOMRegion(key.provider, regionPref)
			provTerm := translateTerm(key.term, key.provider)

			for _, wKey := range workloadKeys {
				wItem := in.Workload[wKey]

				specMap, _, err := workloadItemToSpec(wItem, key.provider, region, provTerm)
				if err != nil {
					res.errs = append(res.errs, fmt.Sprintf("%s: %v", wKey, err))
					continue
				}

				spec, err := unmarshalSpec(specMap)
				if err != nil {
					res.errs = append(res.errs, fmt.Sprintf("%s: invalid spec — %v", wKey, err))
					continue
				}

				if !pvdr.Supports(spec.GetDomain(), spec.GetService()) {
					res.errs = append(res.errs, fmt.Sprintf("%s: not supported by %s", wKey, key.provider))
					continue
				}

				pricing, err := pvdr.GetPrice(ctx, spec)
				if err != nil {
					res.errs = append(res.errs, fmt.Sprintf("%s: %v", wKey, err))
					continue
				}
				if pricing == nil || len(pricing.PublicPrices) == 0 {
					res.errs = append(res.errs, fmt.Sprintf("%s: no pricing found", wKey))
					continue
				}

				qty := wItem.Quantity
				if qty <= 0 {
					qty = 1
				}
				sizeGB := wItem.StorageGB
				if sizeGB <= 0 {
					sizeGB = 100
				}
				iopsCount := wItem.IOPS

				// Iterate all price components so that multi-entry results
				// (e.g. io2: storage + IOPS) are fully summed.
				itemMonthly := 0.0
				for _, price := range pricing.PublicPrices {
					var componentCost float64
					switch price.Unit {
					case models.PriceUnitPerIOPSMonth:
						componentCost = price.PricePerUnit * float64(iopsCount) * qty
					default:
						componentCost = bomMonthlyCost(price, qty, 730.0, sizeGB)
					}
					itemMonthly += componentCost
				}
				res.breakdown[wKey] = roundToTwoDecimal(itemMonthly)
				res.totalMonthly += itemMonthly
			}
			res.totalMonthly = roundToTwoDecimal(res.totalMonthly)

			taskResults[idx] = taskResult{key: key, result: res}
		}(i, tk)
	}
	wg.Wait()

	// --- Index results: [provider][term] → compareBOMTermResult ---

	resultIndex := make(map[string]map[string]compareBOMTermResult)
	for _, tr := range taskResults {
		prov := tr.key.provider
		term := tr.key.term
		if resultIndex[prov] == nil {
			resultIndex[prov] = make(map[string]compareBOMTermResult)
		}
		resultIndex[prov][term] = tr.result
	}

	// --- Build comparison output ---

	comparison := make(map[string]any, len(providers))

	for _, prov := range providers {
		region := compareBOMRegion(prov, regionPref)
		pvdr := h.providers[prov]

		provOut := map[string]any{
			"region": region,
		}

		// on_demand baseline for savings calculation.
		odMonthly := 0.0
		if odRes, ok := resultIndex[prov]["on_demand"]; ok {
			odMonthly = odRes.totalMonthly
		}

		for _, canonTerm := range terms {
			termRes, ok := resultIndex[prov][canonTerm]
			if !ok {
				continue
			}

			// Build breakdown map with float amounts.
			breakdownOut := make(map[string]any, len(termRes.breakdown))
			for k, v := range termRes.breakdown {
				breakdownOut[k] = v
			}

			termOut := map[string]any{
				"total_monthly": termRes.totalMonthly,
				"breakdown":     breakdownOut,
			}

			if len(termRes.errs) > 0 {
				termOut["errors"] = termRes.errs
			}

			// Savings vs on-demand for committed terms.
			if canonTerm != "on_demand" && odMonthly > 0 && termRes.totalMonthly > 0 {
				savings := roundToTwoDecimal(odMonthly - termRes.totalMonthly)
				pct := roundToTwoDecimal((odMonthly-termRes.totalMonthly)/odMonthly*100)
				termOut["savings_vs_on_demand"] = map[string]any{
					"amount":  savings,
					"percent": pct,
				}
			}

			provOut[canonicalTermLabel(canonTerm)] = termOut
		}

		// BOM advisories for this provider.
		services := servicesForWorkload(in.Workload)
		if pvdr != nil {
			rows, err := pvdr.BOMAdvisories(ctx, services, region)
			if err == nil && len(rows) > 0 {
				provOut["not_included"] = rows
			} else {
				provOut["not_included"] = nil
			}
		} else {
			provOut["not_included"] = nil
			provOut["error"] = fmt.Sprintf("provider '%s' not configured", prov)
		}

		comparison[prov] = provOut
	}

	// --- Generate summary string ---

	summary := buildCompareSummary(comparison, providers, terms)

	return jsonText(map[string]any{
		"comparison": comparison,
		"summary":    summary,
		"note": "Pricing uses closest available instance types per provider " +
			"(compute: m5/n2-standard/D-series; database: RDS MySQL/Cloud SQL/Azure SQL). " +
			"Exact instance matches may vary. For precise comparisons, use estimate_bom with " +
			"explicit resource_type per provider.",
	}), nil, nil
}

// --------------------------------------------------------------------------
// Summary generation
// --------------------------------------------------------------------------

// buildCompareSummary generates a natural-language summary identifying the
// cheapest provider and savings vs on-demand for the best committed term.
func buildCompareSummary(
	comparison map[string]any,
	providers []string,
	terms []string,
) string {
	// Find the "best committed" term — the first non-on_demand term, or on_demand.
	bestCommittedTerm := "on_demand"
	for _, t := range terms {
		if t != "on_demand" {
			bestCommittedTerm = t
			break
		}
	}

	type providerCost struct {
		name     string
		monthly  float64
		savings  float64
		savingPct float64
	}

	var costs []providerCost
	for _, prov := range providers {
		provMap, ok := comparison[prov].(map[string]any)
		if !ok {
			continue
		}
		// Pick the best committed term, fallback to on_demand.
		termKey := bestCommittedTerm
		termData, ok := provMap[termKey].(map[string]any)
		if !ok {
			termKey = "on_demand"
			termData, ok = provMap[termKey].(map[string]any)
			if !ok {
				continue
			}
		}
		monthly, _ := termData["total_monthly"].(float64)
		if monthly <= 0 {
			continue
		}

		savings := 0.0
		savingPct := 0.0
		if svo, ok := termData["savings_vs_on_demand"].(map[string]any); ok {
			savings, _ = svo["amount"].(float64)
			savingPct, _ = svo["percent"].(float64)
		}

		costs = append(costs, providerCost{
			name:     prov,
			monthly:  monthly,
			savings:  savings,
			savingPct: savingPct,
		})
	}

	if len(costs) == 0 {
		return "Insufficient pricing data to generate a comparison summary."
	}

	// Sort cheapest first.
	sort.Slice(costs, func(i, j int) bool {
		return costs[i].monthly < costs[j].monthly
	})

	cheapest := costs[0]
	termDesc := bestCommittedTerm
	if termDesc == "on_demand" {
		termDesc = "on-demand"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(
		"%s is cheapest at $%.2f/mo with %s",
		strings.ToUpper(cheapest.name), cheapest.monthly, termDesc,
	))

	// Add deltas vs other providers.
	if len(costs) > 1 {
		var deltas []string
		for _, c := range costs[1:] {
			if c.monthly > 0 {
				pct := (c.monthly - cheapest.monthly) / c.monthly * 100
				deltas = append(deltas, fmt.Sprintf("%.1f%% below %s ($%.2f/mo)",
					pct, strings.ToUpper(c.name), c.monthly))
			}
		}
		if len(deltas) > 0 {
			sb.WriteString(" (")
			sb.WriteString(strings.Join(deltas, ", "))
			sb.WriteString(")")
		}
	}

	// Add savings vs on-demand for cheapest.
	if cheapest.savings > 0 {
		sb.WriteString(fmt.Sprintf(
			". Saving $%.2f/mo (%.1f%%) vs on-demand.",
			cheapest.savings, cheapest.savingPct,
		))
	} else {
		sb.WriteString(".")
	}

	return sb.String()
}

// servicesForWorkload returns the list of service names to pass to BOMAdvisories.
func servicesForWorkload(workload map[string]WorkloadItem) []string {
	serviceSet := make(map[string]struct{})
	for _, item := range workload {
		switch strings.ToLower(item.Type) {
		case "compute":
			serviceSet["compute"] = struct{}{}
		case "storage":
			serviceSet["storage"] = struct{}{}
		case "database":
			serviceSet["database"] = struct{}{}
		case "cache":
			serviceSet["cache"] = struct{}{}
		}
	}
	services := make([]string, 0, len(serviceSet))
	for s := range serviceSet {
		services = append(services, s)
	}
	sort.Strings(services)
	return services
}

// dedupStrings returns a de-duplicated copy of s preserving first-occurrence order.
func dedupStrings(s []string) []string {
	seen := make(map[string]struct{}, len(s))
	out := make([]string, 0, len(s))
	for _, v := range s {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}
