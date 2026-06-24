// gcp_catalog.go — GCP provider catalog, BOM advisories, and Part 3 pricing dispatch.
//
// Implements:
//   - DescribeCatalog: full ProviderCatalog for all GCP domains and services
//   - BOMAdvisories:   egress cost advisory when data-transfer services appear in BoM
//   - getPart3Price:   dispatch for networking, observability, and inter_region_egress
//
// All methods are on *Provider defined in gcp.go (Part 1).
package gcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// --------------------------------------------------------------------------
// DescribeCatalog
// --------------------------------------------------------------------------

// gcpDescribeCatalog returns the full provider catalog describing every GCP
// domain and service this provider can price, along with filter hints,
// example invocations, and a decision matrix for LLM tool routing.
func gcpDescribeCatalog() *models.ProviderCatalog {
	return &models.ProviderCatalog{
		Provider: models.CloudProviderGCP,
		Domains: []models.PricingDomain{
			models.PricingDomainCompute,
			models.PricingDomainStorage,
			models.PricingDomainDatabase,
			models.PricingDomainContainer,
			models.PricingDomainAI,
			models.PricingDomainAnalytics,
			models.PricingDomainNetwork,
			models.PricingDomainObservability,
			models.PricingDomainInterRegionEgress,
		},
		Services: map[string][]string{
			"compute":             {"compute_engine"},
			"storage":             {"gcs", "persistent_disk"},
			"database":            {"cloud_sql", "memorystore"},
			"container":           {"gke"},
			"ai":                  {"vertex", "gemini"},
			"analytics":           {"bigquery"},
			"network":             {"cloud_lb", "cloud_cdn", "cloud_nat", "cloud_armor", "egress"},
			"observability":       {"cloud_monitoring"},
			"inter_region_egress": {},
		},
		SupportedTerms: map[string][]string{
			"compute/compute_engine":         {"on_demand", "spot", "cud_1yr", "cud_3yr", "sud", "flex_cud"},
			"storage/gcs":                    {"on_demand"},
			"storage/persistent_disk":        {"on_demand"},
			"database/cloud_sql":             {"on_demand"},
			"database/memorystore":           {"on_demand"},
			"container/gke":                  {"on_demand"},
			"ai/vertex":                      {"on_demand"},
			"ai/gemini":                      {"on_demand"},
			"analytics/bigquery":             {"on_demand"},
			"network/cloud_lb":               {"on_demand"},
			"network/cloud_cdn":              {"on_demand"},
			"network/cloud_nat":              {"on_demand"},
			"network/cloud_armor":            {"on_demand"},
			"network/egress":                 {"on_demand"},
			"observability/cloud_monitoring": {"on_demand"},
			"inter_region_egress":            {"on_demand"},
		},
		FilterHints: map[string]map[string]any{
			"compute/compute_engine": {
				"resource_type": "GCP machine type e.g. 'n1-standard-4', 'e2-medium', 'c2-standard-8'",
				"os":            "'Linux' (default) or 'Windows' (N1/N2/N2D/C2 only)",
				"term":          "on_demand | spot | cud_1yr | cud_3yr | sud | flex_cud",
				"sud_note":      "SUD (Sustained Use Discount) is a billing-engine discount — it is NOT a catalog SKU and will NOT appear in search_pricing results. To get SUD pricing call get_price with term='sud' directly; the response includes the per-tier breakdown and blended effective rate. Eligible families: n1, n2, n2d, e2, c2, c2d, c3, t2d, t2a, m1, m2, m3. GPU families (a2, a3, g2) are not eligible.",
				"flex_cud_note": "Flex CUD (Flexible Committed Use Discount, usageType=CmtCudPremium) — no minimum commitment term; discount falls between on_demand and cud_1yr. Eligible families: n2, n2d, c2, c2d, e2. Use term='flex_cud'.",
			},
			"storage/gcs": {
				"storage_type": "standard | nearline | coldline | archive",
			},
			"storage/persistent_disk": {
				"storage_type": "pd-ssd | pd-balanced | pd-standard | pd-extreme",
				"size_gb":      "Disk size in GiB for monthly cost estimate",
			},
			"database/cloud_sql": {
				"resource_type": "Cloud SQL instance type e.g. 'db-n1-standard-4', 'db-custom-2-3840'",
				"engine":        "MySQL | PostgreSQL | SQL Server",
				"deployment":    "single-az (zonal) | ha (regional/HA)",
			},
			"database/memorystore": {
				"capacity_gb": "Provisioned capacity in GiB (positive float)",
				"deployment":  "basic | standard (HA with cross-zone replication)",
				"service":     "memorystore",
			},
			"container/gke": {
				"mode":       "standard (cluster management fee) | autopilot (per vCPU/memory)",
				"vcpu":       "Pod vCPU requests (Autopilot mode)",
				"memory_gb":  "Pod memory GiB (Autopilot mode)",
				"node_count": "Worker node count (Standard mode; add node costs via compute)",
			},
			"ai/gemini": {
				"model":         "gemini-1.5-flash | gemini-1.5-pro | gemini-1.0-pro",
				"input_tokens":  "Estimated input tokens (optional, for cost estimate)",
				"output_tokens": "Estimated output tokens (optional, for cost estimate)",
				"service":       "gemini",
			},
			"ai/vertex": {
				"machine_type":   "GCP machine type for training/prediction e.g. 'n1-standard-8'",
				"task":           "training | prediction | inference",
				"training_hours": "Hours for cost estimate",
				"service":        "vertex",
			},
			"analytics/bigquery": {
				"query_tb":            "TB of data scanned per month (optional, for cost estimate)",
				"active_storage_gb":   "Active storage GB (optional, for cost estimate)",
				"longterm_storage_gb": "Long-term storage GB (optional, for cost estimate)",
				"streaming_gb":        "Streaming inserts GB (optional, for cost estimate)",
			},
			"network/cloud_lb": {
				"lb_type":    "https | tcp | ssl | network | internal",
				"rule_count": "Number of forwarding rules",
				"data_gb":    "GB of data processed per month",
				"service":    "cloud_lb",
			},
			"network/cloud_cdn": {
				"egress_gb":     "GB egressed from CDN to users per month",
				"cache_fill_gb": "GB filled from origin into CDN per month",
				"service":       "cloud_cdn",
			},
			"network/cloud_nat": {
				"gateway_count": "Number of Cloud NAT gateways",
				"data_gb":       "GB processed through NAT per month",
				"service":       "cloud_nat",
			},
			"network/cloud_armor": {
				"policy_count":              "Number of security policies",
				"monthly_requests_millions": "Millions of requests evaluated per month",
				"service":                   "cloud_armor",
			},
			"network/egress": {
				"source_region":      "GCP origin region e.g. 'us-central1', 'europe-west1'",
				"destination_type":   "internet | cross_region | cross_az",
				"destination_region": "target GCP region for cross_region (optional)",
				"data_gb_per_month":  "monthly data volume in GB for blended-rate estimate",
				"network_tier":       "premium (default) | standard",
			},
			"observability/cloud_monitoring": {
				"ingestion_mib": "MiB of custom/external metrics ingested per month (150 MiB free tier)",
				"service":       "cloud_monitoring",
			},
			"inter_region_egress": {
				"source_region": "GCP source region e.g. 'us-central1', 'europe-west1'",
				"dest_region":   "GCP destination region (omit for internet egress)",
				"data_gb":       "GB to transfer per month",
			},
		},
		ExampleInvocations: map[string]map[string]any{
			"compute/compute_engine": {
				"provider":      "gcp",
				"domain":        "compute",
				"resource_type": "n1-standard-4",
				"region":        "us-central1",
				"os":            "Linux",
				"term":          "on_demand",
			},
			"compute/compute_engine/sud": {
				"provider":      "gcp",
				"domain":        "compute",
				"resource_type": "n1-standard-4",
				"region":        "us-central1",
				"os":            "Linux",
				"term":          "sud",
			},
			"compute/compute_engine/flex_cud": {
				"provider":      "gcp",
				"domain":        "compute",
				"resource_type": "n2-standard-8",
				"region":        "us-central1",
				"os":            "Linux",
				"term":          "flex_cud",
			},
			"storage/gcs": {
				"provider":     "gcp",
				"domain":       "storage",
				"storage_type": "standard",
				"region":       "us-central1",
			},
			"storage/persistent_disk": {
				"provider":     "gcp",
				"domain":       "storage",
				"service":      "persistent_disk",
				"storage_type": "pd-ssd",
				"size_gb":      500,
				"region":       "us-central1",
			},
			"database/cloud_sql": {
				"provider":      "gcp",
				"domain":        "database",
				"service":       "cloud_sql",
				"resource_type": "db-n1-standard-4",
				"engine":        "MySQL",
				"deployment":    "single-az",
				"region":        "us-central1",
			},
			"database/memorystore": {
				"provider":    "gcp",
				"domain":      "database",
				"service":     "memorystore",
				"capacity_gb": 10.0,
				"deployment":  "standard",
				"region":      "us-central1",
			},
			"container/gke": {
				"provider":   "gcp",
				"domain":     "container",
				"service":    "gke",
				"mode":       "standard",
				"node_count": 3,
				"region":     "us-central1",
			},
			"ai/gemini": {
				"provider":      "gcp",
				"domain":        "ai",
				"service":       "gemini",
				"model":         "gemini-1.5-flash",
				"region":        "us-central1",
				"input_tokens":  1000000,
				"output_tokens": 1000000,
			},
			"ai/vertex": {
				"provider":       "gcp",
				"domain":         "ai",
				"service":        "vertex",
				"machine_type":   "n1-standard-8",
				"task":           "training",
				"training_hours": 10.0,
				"region":         "us-central1",
			},
			"analytics/bigquery": {
				"provider":          "gcp",
				"domain":            "analytics",
				"service":           "bigquery",
				"query_tb":          10.0,
				"active_storage_gb": 100.0,
				"region":            "us",
			},
			"network/cloud_lb": {
				"provider":   "gcp",
				"domain":     "network",
				"service":    "cloud_lb",
				"lb_type":    "https",
				"rule_count": 1,
				"data_gb":    100.0,
				"region":     "us-central1",
			},
			"network/cloud_cdn": {
				"provider":      "gcp",
				"domain":        "network",
				"service":       "cloud_cdn",
				"egress_gb":     1000.0,
				"cache_fill_gb": 100.0,
				"region":        "us-central1",
			},
			"observability/cloud_monitoring": {
				"provider":      "gcp",
				"domain":        "observability",
				"service":       "cloud_monitoring",
				"ingestion_mib": 1000.0,
				"region":        "us-central1",
			},
			"inter_region_egress": {
				"provider":      "gcp",
				"domain":        "inter_region_egress",
				"source_region": "us-central1",
				"data_gb":       1000.0,
			},
			"network/egress": {
				"provider":          "gcp",
				"domain":            "network",
				"service":           "egress",
				"source_region":     "us-central1",
				"destination_type":  "internet",
				"data_gb_per_month": 1024.0,
				"network_tier":      "premium",
			},
			"network/egress/cross_region": {
				"provider":           "gcp",
				"domain":             "network",
				"service":            "egress",
				"source_region":      "us-central1",
				"destination_type":   "cross_region",
				"destination_region": "europe-west1",
				"data_gb_per_month":  1024.0,
			},
		},
		DecisionMatrix: map[string]string{
			"Cloud Storage":            "storage/gcs",
			"GCS":                      "storage/gcs",
			"Compute Engine":           "compute/compute_engine",
			"GCE":                      "compute/compute_engine",
			"Cloud SQL":                "database/cloud_sql",
			"Memorystore":              "database/memorystore",
			"Redis":                    "database/memorystore",
			"GKE":                      "container/gke",
			"Google Kubernetes Engine": "container/gke",
			"Vertex AI":                "ai/vertex",
			"Gemini":                   "ai/gemini",
			"BigQuery":                 "analytics/bigquery",
			"Cloud Load Balancing":     "network/cloud_lb",
			"Cloud CDN":                "network/cloud_cdn — GCP-native CDN (use provider='gcp', service='cloud_cdn')",
			"CDN (GCP)":                "network/cloud_cdn — use provider='gcp', service='cloud_cdn', NOT provider='aws'",
			"Cloud NAT":                "network/cloud_nat",
			"Cloud Armor":              "network/cloud_armor",
			"Cloud Monitoring":         "observability/cloud_monitoring",
			"GCP Egress":               "inter_region_egress — set source_region and data_gb",
			"GCP Data Transfer":        "inter_region_egress — set source_region, dest_region (optional), data_gb",
			"GCP Internet Egress":      "inter_region_egress — set source_region, data_gb (no dest_region)",
			"GCP internet egress with tier breakdown":       "network/egress — set destination_type=internet + data_gb_per_month",
			"GCP inter-region transfer with tier breakdown": "network/egress — set destination_type=cross_region + destination_region",
		},
	}
}

// --------------------------------------------------------------------------
// BOMAdvisories
// --------------------------------------------------------------------------

// gcpBOMAdvisories returns provider-specific advisory rows for services not
// included in estimate_bom, e.g. egress cost when data-transfer services appear
// in the Bill of Materials.
func gcpBOMAdvisories(services []string, sampleRegion string) []map[string]string {
	// Classify the sample region's continent for advisory targeting.
	continent := gcpEgressContinent(sampleRegion)
	baseRate := gcpInternetEgressBaseRate[continent]
	if baseRate == 0 {
		baseRate = 0.08
	}

	// Data-transfer / storage keywords that imply significant egress.
	dataServices := map[string]bool{
		"gcs": true, "storage": true, "bigquery": true, "analytics": true,
		"gke": true, "compute_engine": true, "cloud_sql": true,
	}

	hasDataService := false
	for _, svc := range services {
		if dataServices[strings.ToLower(svc)] {
			hasDataService = true
			break
		}
	}

	if !hasDataService {
		return nil
	}

	return []map[string]string{
		{
			"provider": "gcp",
			"type":     "egress",
			"message": fmt.Sprintf(
				"GCP internet egress from %s (%s) costs $%.3f/GB (first 1 TB/month). "+
					"Use domain='inter_region_egress' with source_region='%s' to estimate data-transfer costs. "+
					"Inter-region same-continent: $0.01/GB; cross-continent: $0.08/GB.",
				sampleRegion, strings.ToUpper(continent), baseRate, sampleRegion,
			),
			"action": "Call GetPrice with domain='inter_region_egress', source_region='" + sampleRegion + "', data_gb=<estimated monthly GB>",
		},
	}
}

// --------------------------------------------------------------------------
// Part 3 pricing dispatch
// --------------------------------------------------------------------------

// getPart3Price dispatches networking, observability, and inter_region_egress
// domain specs to their implementations in gcp_networking.go and gcp_analytics.go.
// Returns (nil, nil) for unrecognised specs so GetPrice can fall through.
func (p *Provider) getPart3Price(ctx context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
	switch s := spec.(type) {
	case *models.NetworkPricingSpec:
		prices, breakdown, err := p.priceNetwork(ctx, s)
		if err != nil {
			return nil, err
		}
		return buildResult(prices, breakdown), nil

	case *models.ObservabilityPricingSpec:
		prices, breakdown, err := p.priceObservability(ctx, s)
		if err != nil {
			return nil, err
		}
		return buildResult(prices, breakdown), nil

	case *models.EgressPricingSpec:
		src := s.SourceRegion
		if src == "" {
			src = s.Region
		}
		prices, err := p.GetEgressPrice(ctx, src, s.DestRegion, s.DataGB)
		if err != nil {
			return nil, err
		}
		return buildResult(prices, map[string]any{"source_region": src, "dest_region": s.DestRegion}), nil

	case *models.AnalyticsPricingSpec:
		// Analytics is also handled by getAIPart2Price; this handles any that
		// fall through if the service is explicitly set to a Part 3 context.
		prices, breakdown, err := p.priceAnalytics(ctx, s)
		if err != nil {
			return nil, err
		}
		return buildResult(prices, breakdown), nil
	}
	return nil, nil
}

