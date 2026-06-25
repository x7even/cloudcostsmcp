// Package azure implements the Azure cloud pricing provider for OpenCloudCosts.
//
// It uses the Azure Retail Prices REST API — fully public, no credentials needed.
// Endpoint: https://prices.azure.com/api/retail/prices
// Filter fields: armSkuName, armRegionName, priceType, reservationTerm
// priceType: "Consumption" = on-demand/spot, "Reservation" = reserved
// Instance format: Standard_D4s_v3, Standard_E8s_v3, Standard_B2ms
// Region format: eastus, westeurope, southeastasia (ARM region names, lowercase)
package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/cache"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers"
)

const (
	azurePricesBase = "https://prices.azure.com/api/retail/prices"
	apiVersion      = "2023-01-01-preview"
	sourceURL       = "https://prices.azure.com/api/retail/prices"
)

// azureStorageMap maps our storage_type keys to Azure productName substrings.
var azureStorageMap = map[string]string{
	"premium-ssd":     "Premium SSD Managed Disks",
	"standard-ssd":    "Standard SSD Managed Disks",
	"standard-hdd":    "Standard HDD Managed Disks",
	"ultra-ssd":       "Ultra Disks",
	"blob":            "Blob Storage",
	"gp3":             "Premium SSD Managed Disks",
	"gp2":             "Standard SSD Managed Disks",
	"io1":             "Premium SSD Managed Disks",
	"pd-ssd":          "Premium SSD Managed Disks",
	"pd-balanced":     "Standard SSD Managed Disks",
	"pd-standard":     "Standard HDD Managed Disks",
	"standard":        "Standard SSD Managed Disks",
	"standardssd_lrs": "Standard SSD Managed Disks",
	"premium_lrs":     "Premium SSD Managed Disks",
	"standard_lrs":    "Standard HDD Managed Disks",
	"ultrassd_lrs":    "Ultra Disks",
}

// premiumSSDTier pairs a tier name with its GiB capacity.
type premiumSSDTier struct {
	name     string
	capacity int
}

// premiumSSDTiers lists Azure Premium SSD P-series tier sizes.
// Each tier's price is a flat monthly fee covering disks up to that capacity.
var premiumSSDTiers = []premiumSSDTier{
	{"P1", 4}, {"P2", 8}, {"P3", 16}, {"P4", 32}, {"P6", 64},
	{"P10", 128}, {"P15", 256}, {"P20", 512}, {"P30", 1024},
	{"P40", 2048}, {"P50", 4096}, {"P60", 8192}, {"P70", 16384}, {"P80", 32767},
}

// selectPremiumSSDTier returns the smallest P-series tier name that covers sizeGB.
func selectPremiumSSDTier(sizeGB float64) string {
	for _, t := range premiumSSDTiers {
		if float64(t.capacity) >= sizeGB {
			return t.name
		}
	}
	return "P80"
}

// majorRegions is the curated major-region list used by fan-out tools.
var majorRegions = []string{
	"eastus", "eastus2", "westus", "westus2", "westus3", "centralus",
	"northeurope", "westeurope", "uksouth",
	"eastasia", "southeastasia", "australiaeast",
}

// azureRegionDisplay maps ARM region codes to friendly display names.
var azureRegionDisplay = map[string]string{
	"eastus":             "East US",
	"eastus2":            "East US 2",
	"westus":             "West US",
	"westus2":            "West US 2",
	"westus3":            "West US 3",
	"centralus":          "Central US",
	"northcentralus":     "North Central US",
	"southcentralus":     "South Central US",
	"westcentralus":      "West Central US",
	"canadacentral":      "Canada Central",
	"canadaeast":         "Canada East",
	"brazilsouth":        "Brazil South",
	"northeurope":        "North Europe",
	"westeurope":         "West Europe",
	"uksouth":            "UK South",
	"ukwest":             "UK West",
	"francecentral":      "France Central",
	"germanywestcentral": "Germany West Central",
	"norwayeast":         "Norway East",
	"switzerlandnorth":   "Switzerland North",
	"eastasia":           "East Asia",
	"southeastasia":      "Southeast Asia",
	"japaneast":          "Japan East",
	"japanwest":          "Japan West",
	"australiaeast":      "Australia East",
	"australiasoutheast": "Australia Southeast",
	"centralindia":       "Central India",
	"southindia":         "South India",
	"westindia":          "West India",
	"koreacentral":       "Korea Central",
	"southafricanorth":   "South Africa North",
	"uaenorth":           "UAE North",
}

// listAzureRegions returns a sorted slice of all known Azure region codes.
func listAzureRegions() []string {
	out := make([]string, 0, len(azureRegionDisplay))
	for k := range azureRegionDisplay {
		out = append(out, k)
	}
	// sort for determinism
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// azureEgressZone maps source armRegionName to billing zone (unlisted → zone1).
var azureEgressZone = map[string]string{
	// Zone 2: Asia Pacific
	"eastasia": "zone2", "southeastasia": "zone2", "japaneast": "zone2",
	"japanwest": "zone2", "australiaeast": "zone2", "australiasoutheast": "zone2",
	"centralindia": "zone2", "southindia": "zone2", "westindia": "zone2",
	"koreacentral": "zone2", "koreasouth": "zone2",
	// Zone 3: Middle East + Africa
	"uaenorth": "zone3", "uaecentral": "zone3",
	"southafricanorth": "zone3", "southafricawest": "zone3",
}

// azureEgressBaseRate gives per-GB rates by billing zone.
var azureEgressBaseRate = map[string]float64{
	"zone1": 0.087,
	"zone2": 0.16,
	"zone3": 0.181,
}

// egressTier describes a single tiered price band for egress.
type egressTier struct {
	thresholdGB float64
	rate        float64
	label       string
}

// azureInternetEgressTiers defines tiered egress per zone.
var azureInternetEgressTiers = map[string][]egressTier{
	"zone1": {
		{0, 0.000, "0-5 GB (free)"},
		{5, 0.087, "5 GB-10 TB"},
		{10240, 0.083, "10-50 TB"},
		{51200, 0.070, "50-150 TB"},
		{153600, 0.050, "150-500 TB"},
		{512000, 0.050, ">500 TB"},
	},
	"zone2": {
		{0, 0.000, "0-5 GB (free)"},
		{5, 0.120, "5 GB-10 TB"},
		{10240, 0.085, "10-50 TB"},
		{51200, 0.082, "50-150 TB"},
		{153600, 0.080, "150-500 TB"},
		{512000, 0.080, ">500 TB"},
	},
	"zone3": {
		{0, 0.000, "0-5 GB (free)"},
		{5, 0.181, "5 GB-10 TB"},
		{10240, 0.175, "10-50 TB"},
		{51200, 0.170, "50-150 TB"},
		{153600, 0.160, "150-500 TB"},
		{512000, 0.160, ">500 TB"},
	},
}

// computeTieredCost computes blended egress cost across ordered tiers.
func computeTieredCost(tiers []egressTier, dataGB float64) map[string]any {
	remaining := math.Max(0, dataGB)
	total := 0.0
	type tierSplit struct {
		Label string  `json:"label"`
		GB    float64 `json:"gb"`
		Rate  string  `json:"rate"`
		Cost  string  `json:"cost"`
	}
	var splits []tierSplit

	for i, tier := range tiers {
		if remaining <= 0 {
			break
		}
		var used float64
		if i+1 < len(tiers) {
			tierCap := tiers[i+1].thresholdGB - tier.thresholdGB
			if remaining < tierCap {
				used = remaining
			} else {
				used = tierCap
			}
		} else {
			used = remaining
		}
		cost := used * tier.rate
		total += cost
		splits = append(splits, tierSplit{
			Label: tier.label,
			GB:    used,
			Rate:  fmt.Sprintf("%.4f", tier.rate),
			Cost:  fmt.Sprintf("%.4f", cost),
		})
		remaining -= used
	}

	blended := 0.0
	if dataGB > 0 {
		blended = total / dataGB
	}
	return map[string]any{
		"total_cost":          fmt.Sprintf("%.4f", total),
		"blended_rate_per_gb": fmt.Sprintf("%.4f", blended),
		"data_gb":             dataGB,
		"tiers":               splits,
	}
}

// azureCrossRegionRate gives inter-region flat rates by zone.
var azureCrossRegionRate = map[string]float64{
	"zone1": 0.02,
	"zone2": 0.08,
	"zone3": 0.16,
}

const azureCrossAZRate = 0.01

const azureEgressSourceURL = "https://azure.microsoft.com/en-us/pricing/details/bandwidth/"

// sqlTierMap maps resource_type tier keywords to API skuName prefixes.
var sqlTierMap = map[string]string{
	"general purpose":   "GP",
	"gp":                "GP",
	"business critical": "BC",
	"bc":                "BC",
	"hyperscale":        "HS",
	"hs":                "HS",
}

// sqlResourceTypeToArmSkuName converts a human-readable resource_type into
// the armSkuName used by the Azure SQL Database (engine=SQL) Retail Prices API.
//
// "General Purpose 4 vCores"  → "SQLDB_GP_Compute_Gen5_4"
// "Business Critical 8 vCores" → "SQLDB_BC_Compute_Gen5_8"
// Returns "" when the tier or vCore count cannot be determined.
func sqlResourceTypeToArmSkuName(resourceType string) string {
	rt := strings.ToLower(resourceType)
	var tier string
	switch {
	case strings.Contains(rt, "general purpose") || strings.Contains(rt, "general_purpose") || strings.Contains(rt, " gp"):
		tier = "GP"
	case strings.Contains(rt, "business critical") || strings.Contains(rt, "business_critical") || strings.Contains(rt, " bc"):
		tier = "BC"
	case strings.Contains(rt, "hyperscale") || strings.Contains(rt, " hs"):
		tier = "HS"
	default:
		return ""
	}
	// Extract the vCore count: first integer token in the string.
	fields := strings.Fields(resourceType)
	for i, f := range fields {
		if strings.EqualFold(f, "vCores") || strings.EqualFold(f, "vCore") {
			if i > 0 {
				return "SQLDB_" + tier + "_Compute_Gen5_" + fields[i-1]
			}
		}
	}
	return ""
}

// pgResourceTypeToArmSkuName converts a human-readable resource_type into
// the armSkuName used by Azure Database for PostgreSQL Flexible Server.
//
// Only General Purpose (Ddsv5 series) is supported; Memory Optimized and
// Burstable tiers use different SKU patterns not covered here.
//
// "General Purpose 4 vCores" → "Standard_D4ds_v5"
// Returns "" when the tier is not General Purpose or the vCore count is absent.
func pgResourceTypeToArmSkuName(resourceType string) string {
	rt := strings.ToLower(resourceType)
	if !strings.Contains(rt, "general purpose") && !strings.Contains(rt, "general_purpose") && !strings.Contains(rt, " gp") {
		return ""
	}
	fields := strings.Fields(resourceType)
	for i, f := range fields {
		if strings.EqualFold(f, "vCores") || strings.EqualFold(f, "vCore") {
			if i > 0 {
				return "Standard_D" + fields[i-1] + "ds_v5"
			}
		}
	}
	return ""
}

// Static fallback rates.
const (
	functionsExecRate   = 0.0000002 // per execution
	functionsGBSecRate  = 0.000016  // per GB-second
	aksPremiumRate      = 0.10      // per cluster/hr (Standard tier)
	// sqlGPVCoreRate is the published on-demand rate for Azure SQL Database
	// General Purpose Gen5 in us-east regions (per vCore per hour).
	// Used only as a static fallback when the Retail Prices API returns no rows.
	sqlGPVCoreRate = 0.1770 // per vCore-hour, GP Gen5
	monitorLogRate      = 2.76      // per GB Analytics Logs
	monitorBasicLogRate = 0.50      // per GB Basic Logs
	monitorMetricsRate  = 0.16      // per 10M metric samples
	monitorAlertRate    = 0.10      // per rule/month after 10 free
	monitorFreeLogGB    = 5.0       // 5 GB/month free tier
	monitorFreeAlerts   = 10        // first 10 metric alert rules free

	cosmosProvisionedRate = 0.00008 // per 100 RU/s per hour (Provisioned/Autoscale)
	cosmosServerlessRate  = 0.25    // per million Request Units consumed
	cosmosStorageRate     = 0.25    // per GB-month storage

	frontDoorBaseFee = 35.0 // per month Standard tier base fee
)

// openAIStaticRates maps model names to (input, output) per-1K-token rates.
type openAIRate struct{ input, output float64 }

var openAIStaticRates = map[string]openAIRate{
	"gpt-4o":                 {0.005, 0.015},
	"gpt-4o-mini":            {0.00015, 0.0006},
	"gpt-4":                  {0.03, 0.06},
	"gpt-4-32k":              {0.06, 0.12},
	"gpt-35-turbo":           {0.0005, 0.0015},
	"gpt-35-turbo-16k":       {0.001, 0.002},
	"o1":                     {0.015, 0.06},
	"o1-mini":                {0.003, 0.012},
	"text-embedding-ada-002": {0.0001, 0.0001},
	"text-embedding-3-small": {0.00002, 0.00002},
	"text-embedding-3-large": {0.00013, 0.00013},
}

// cdnZone maps region to CDN billing zone (default Zone 1).
var cdnZone = map[string]string{
	"eastasia": "Zone 2", "southeastasia": "Zone 2", "japaneast": "Zone 2",
	"japanwest": "Zone 2", "koreacentral": "Zone 2", "koreasouth": "Zone 2",
	"uaenorth": "Zone 2", "uaecentral": "Zone 2",
	"centralindia": "Zone 3", "southindia": "Zone 3", "westindia": "Zone 3",
	"brazilsouth":   "Zone 4",
	"australiaeast": "Zone 5", "australiasoutheast": "Zone 5",
}

// CDN static rates per GB by zone.
var cdnStaticRates = map[string]float64{
	"Zone 1": 0.081,
	"Zone 2": 0.163,
	"Zone 3": 0.163,
	"Zone 4": 0.200,
	"Zone 5": 0.220,
}

// Front Door static data transfer rates per GB by zone.
var frontDoorStaticRates = map[string]float64{
	"Zone 1": 0.0825,
	"Zone 2": 0.160,
	"Zone 3": 0.160,
	"Zone 4": 0.195,
	"Zone 5": 0.210,
}

// Front Door static request rates per 10K requests by zone.
var frontDoorReqStaticRates = map[string]float64{
	"Zone 1": 0.009,
	"Zone 2": 0.012,
	"Zone 3": 0.012,
	"Zone 4": 0.014,
	"Zone 5": 0.015,
}

// azureRetailItem mirrors one "Items" entry from the Azure Retail Prices API.
type azureRetailItem struct {
	RetailPrice      float64 `json:"retailPrice"`
	SkuName          string  `json:"skuName"`
	ArmSkuName       string  `json:"armSkuName"`
	ProductName      string  `json:"productName"`
	MeterName        string  `json:"meterName"`
	ServiceName      string  `json:"serviceName"`
	ServiceFamily    string  `json:"serviceFamily"`
	MeterID          string  `json:"meterId"`
	ArmRegionName    string  `json:"armRegionName"`
	UnitOfMeasure    string  `json:"unitOfMeasure"`
	TierMinimumUnits float64 `json:"tierMinimumUnits"`
	Type             string  `json:"type"`
}

// azureRetailResponse is the top-level API response.
type azureRetailResponse struct {
	Items        []azureRetailItem `json:"Items"`
	NextPageLink string            `json:"NextPageLink"`
}

// cachedPriceSlice is stored in the cache (prices + fetched_at).
type cachedPriceSlice struct {
	FetchedAt time.Time                `json:"fetched_at"`
	Prices    []models.NormalizedPrice `json:"prices"`
}

// Provider is the Azure pricing provider implementation.
type Provider struct {
	cache      *cache.CacheManager
	cacheTTL   time.Duration
	metaTTL    time.Duration
	httpClient *http.Client
	baseURL    string // overridable for tests
}

// azureAllowedHost is the only domain the Azure HTTP client may follow redirects to.
const azureAllowedHost = "prices.azure.com"

// azureCheckRedirect blocks redirects that leave the prices.azure.com domain.
func azureCheckRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Hostname() != azureAllowedHost {
		return fmt.Errorf("azure: redirect to off-domain host %q blocked", req.URL.Hostname())
	}
	if len(via) >= 10 {
		return fmt.Errorf("azure: stopped after 10 redirects")
	}
	return nil
}

// NewProvider creates an AzureProvider.
func NewProvider(cm *cache.CacheManager, cacheTTL, metaTTL time.Duration) *Provider {
	return &Provider{
		cache:    cm,
		cacheTTL: cacheTTL,
		metaTTL:  metaTTL,
		httpClient: &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: azureCheckRedirect,
		},
		baseURL: azurePricesBase,
	}
}

// Compile-time guard: Provider must implement providers.Provider.
var _ providers.Provider = (*Provider)(nil)

// SetBaseURL overrides the Azure Retail Prices API base URL (for testing).
func (p *Provider) SetBaseURL(u string) { p.baseURL = u }

// SetHTTPClient overrides the HTTP client (for testing with httptest.Server).
func (p *Provider) SetHTTPClient(c *http.Client) { p.httpClient = c }

// --------------------------------------------------------------------------
// Identity methods
// --------------------------------------------------------------------------

// Name returns "azure".
func (p *Provider) Name() models.CloudProvider { return models.CloudProviderAzure }

// DefaultRegion returns "eastus".
func (p *Provider) DefaultRegion() string { return "eastus" }

// MajorRegions returns the curated list of major Azure regions.
func (p *Provider) MajorRegions() []string { return majorRegions }

// Supports reports whether Azure can price the given domain/service pair.
func (p *Provider) Supports(domain models.PricingDomain, service string) bool {
	// capabilities set — includes canonical names and accepted aliases
	type key struct{ domain, service string }
	caps := map[key]bool{
		{string(models.PricingDomainCompute), ""}:                    true,
		{string(models.PricingDomainCompute), "vm"}:                  true,
		{string(models.PricingDomainStorage), ""}:                    true,
		{string(models.PricingDomainStorage), "managed_disks"}:       true,
		{string(models.PricingDomainStorage), "blob"}:                true,
		{string(models.PricingDomainDatabase), ""}:                   true,
		{string(models.PricingDomainDatabase), "sql"}:                true,
		{string(models.PricingDomainDatabase), "cosmos"}:             true,
		{string(models.PricingDomainDatabase), "cosmos_db"}:          true,
		{string(models.PricingDomainDatabase), "cosmosdb"}:           true,
		{string(models.PricingDomainContainer), ""}:                  true,
		{string(models.PricingDomainContainer), "aks"}:               true,
		{string(models.PricingDomainServerless), ""}:                 true,
		{string(models.PricingDomainServerless), "azure_functions"}:  true,
		{string(models.PricingDomainAI), ""}:                         true,
		{string(models.PricingDomainAI), "openai"}:                   true,
		{string(models.PricingDomainNetwork), ""}:                    true,
		{string(models.PricingDomainNetwork), "egress"}:              true,
		{string(models.PricingDomainNetwork), "azure_cdn"}:           true,
		{string(models.PricingDomainNetwork), "cdn"}:                 true,
		{string(models.PricingDomainNetwork), "azure_front_door"}:    true,
		{string(models.PricingDomainNetwork), "frontdoor"}:           true,
		{string(models.PricingDomainNetwork), "front_door"}:          true,
		{string(models.PricingDomainInterRegionEgress), ""}:          true,
		{string(models.PricingDomainObservability), ""}:              true,
		{string(models.PricingDomainObservability), "azure_monitor"}: true,
		{string(models.PricingDomainObservability), "monitor"}:       true,
		{string(models.PricingDomainObservability), "log_analytics"}:  true,
	}
	return caps[key{string(domain), service}]
}

// SupportedTerms returns pricing terms for the given domain/service.
func (p *Provider) SupportedTerms(domain models.PricingDomain, service string) []models.PricingTerm {
	if domain == models.PricingDomainCompute {
		return []models.PricingTerm{
			models.PricingTermOnDemand,
			models.PricingTermSpot,
			models.PricingTermReserved1Yr,
			models.PricingTermReserved3Yr,
		}
	}
	return []models.PricingTerm{models.PricingTermOnDemand}
}

// --------------------------------------------------------------------------
// HTTP helpers
// --------------------------------------------------------------------------

// fetchPrices calls the Azure Retail Prices API with the given filters and
// follows pagination until maxResults are collected.
func (p *Provider) fetchPrices(ctx context.Context, filters map[string]string, maxResults int) ([]azureRetailItem, error) {
	parts := make([]string, 0, len(filters))
	for k, v := range filters {
		parts = append(parts, fmt.Sprintf("%s eq '%s'", k, v))
	}
	filterStr := strings.Join(parts, " and ")

	top := maxResults
	if top > 100 {
		top = 100
	}
	rawURL := fmt.Sprintf("%s?api-version=%s&$filter=%s&$top=%d",
		p.baseURL, apiVersion, url.QueryEscape(filterStr), top)

	var results []azureRetailItem
	nextURL := rawURL

	for nextURL != "" && len(results) < maxResults {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, nextURL, nil)
		if err != nil {
			return nil, fmt.Errorf("azure: build request: %w", err)
		}

		resp, err := p.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("azure: http get: %w", err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("azure: read body: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("azure: api returned %d: %s", resp.StatusCode, string(body))
		}

		var data azureRetailResponse
		if err := json.Unmarshal(body, &data); err != nil {
			return nil, fmt.Errorf("azure: unmarshal: %w", err)
		}
		results = append(results, data.Items...)
		nextURL = data.NextPageLink
	}
	if len(results) > maxResults {
		results = results[:maxResults]
	}
	return results, nil
}

// itemToPrice converts a single Azure Retail Prices API item to a NormalizedPrice.
// Returns nil if the price is zero or invalid.
func itemToPrice(item azureRetailItem, region string, term models.PricingTerm, service string) *models.NormalizedPrice {
	if item.RetailPrice == 0 {
		return nil
	}
	skuName := item.SkuName
	if skuName == "" {
		skuName = item.MeterName
	}
	skuID := item.MeterID
	if skuID == "" {
		skuID = item.ArmSkuName
	}
	return &models.NormalizedPrice{
		Provider:      models.CloudProviderAzure,
		Service:       service,
		SKUID:         skuID,
		ProductFamily: item.ServiceFamily,
		Description:   skuName,
		Region:        region,
		PricingTerm:   term,
		PricePerUnit:  item.RetailPrice,
		Unit:          models.PriceUnitPerHour,
		Currency:      "USD",
		Attributes: map[string]string{
			"armSkuName":    item.ArmSkuName,
			"productName":   item.ProductName,
			"meterName":     item.MeterName,
			"serviceName":   item.ServiceName,
			"unitOfMeasure": item.UnitOfMeasure,
		},
	}
}

// --------------------------------------------------------------------------
// Cache helpers
// --------------------------------------------------------------------------

// cacheKey constructs a cache key for a pricing request.
func cacheKey(domain, region string, extras map[string]string) string {
	b := strings.Builder{}
	b.WriteString("azure:")
	b.WriteString(domain)
	b.WriteString(":")
	b.WriteString(region)
	// append sorted extras
	type kv struct{ k, v string }
	var pairs []kv
	for k, v := range extras {
		pairs = append(pairs, kv{k, v})
	}
	for i := 1; i < len(pairs); i++ {
		for j := i; j > 0 && pairs[j].k < pairs[j-1].k; j-- {
			pairs[j], pairs[j-1] = pairs[j-1], pairs[j]
		}
	}
	for _, p := range pairs {
		b.WriteString(":")
		b.WriteString(p.k)
		b.WriteString("=")
		b.WriteString(p.v)
	}
	return b.String()
}

// getCachedPrices retrieves prices from cache and applies cache-age annotation.
func (p *Provider) getCachedPrices(key string) ([]models.NormalizedPrice, bool) {
	raw, ok := p.cache.Get(key)
	if !ok {
		return nil, false
	}
	var cs cachedPriceSlice
	if err := json.Unmarshal(raw, &cs); err != nil {
		return nil, false
	}
	age := int(time.Since(cs.FetchedAt).Seconds())
	for i := range cs.Prices {
		cs.Prices[i].CacheAgeSeconds = &age
		cs.Prices[i].SourceURL = sourceURL
		t := cs.FetchedAt
		cs.Prices[i].FetchedAt = &t
	}
	return cs.Prices, true
}

// setCachedPrices stores prices in cache.
func (p *Provider) setCachedPrices(key string, prices []models.NormalizedPrice, ttl time.Duration) {
	cs := cachedPriceSlice{FetchedAt: time.Now(), Prices: prices}
	raw, err := json.Marshal(cs)
	if err != nil {
		return
	}
	p.cache.Set(key, raw, ttl)
}

// annotateFresh marks each price as freshly fetched (cache_age_seconds=0).
func annotateFresh(prices []models.NormalizedPrice, src string) []models.NormalizedPrice {
	now := time.Now()
	zero := 0
	for i := range prices {
		t := now
		prices[i].FetchedAt = &t
		prices[i].SourceURL = src
		prices[i].CacheAgeSeconds = &zero
	}
	return prices
}

// --------------------------------------------------------------------------
// GetComputePrice
// --------------------------------------------------------------------------

// GetComputePrice returns pricing for a specific compute instance type in a region.
func (p *Provider) GetComputePrice(
	ctx context.Context,
	instanceType string,
	region string,
	os string,
	term models.PricingTerm,
) ([]models.NormalizedPrice, error) {
	key := cacheKey("compute", region, map[string]string{
		"instance_type": instanceType,
		"os":            os,
		"term":          string(term),
	})
	if cached, ok := p.getCachedPrices(key); ok {
		return cached, nil
	}

	filters := map[string]string{
		"armSkuName":    instanceType,
		"armRegionName": region,
	}
	switch term { //nolint:exhaustive // unsupported terms (savings plans, CUDs) fall back to Consumption
	case models.PricingTermOnDemand:
		filters["priceType"] = "Consumption"
	case models.PricingTermSpot:
		filters["priceType"] = "Consumption"
	case models.PricingTermReserved1Yr:
		filters["priceType"] = "Reservation"
		filters["reservationTerm"] = "1 Year"
	case models.PricingTermReserved3Yr:
		filters["priceType"] = "Reservation"
		filters["reservationTerm"] = "3 Years"
	default:
		filters["priceType"] = "Consumption"
	}

	items, err := p.fetchPrices(ctx, filters, 20)
	if err != nil {
		return nil, err
	}

	var prices []models.NormalizedPrice
	for _, item := range items {
		productName := item.ProductName
		skuName := item.SkuName

		if term == models.PricingTermSpot {
			if !strings.Contains(skuName, "Spot") {
				continue
			}
		} else if term == models.PricingTermOnDemand {
			if strings.Contains(skuName, "Spot") || strings.Contains(skuName, "Low Priority") {
				continue
			}
			if strings.EqualFold(os, "linux") && strings.Contains(productName, "Windows") {
				continue
			}
			if strings.EqualFold(os, "windows") && !strings.Contains(productName, "Windows") {
				continue
			}
		}

		pp := itemToPrice(item, region, term, "compute")
		if pp != nil {
			prices = append(prices, *pp)
		}
	}

	// T38 fix: reserved prices from API are total annual/3yr cost; divide to per-hour.
	switch term { //nolint:exhaustive // only Reserved1Yr/3Yr need adjustment; all other terms keep per-hour prices
	case models.PricingTermReserved1Yr:
		for i := range prices {
			prices[i].PricePerUnit /= 8760.0
		}
	case models.PricingTermReserved3Yr:
		for i := range prices {
			prices[i].PricePerUnit /= 26280.0
		}
	default:
		// On-demand, spot, and other terms: prices are already per-hour.
	}

	// Sort ascending by price.
	sortPrices(prices)

	p.setCachedPrices(key, prices, p.cacheTTL)
	return annotateFresh(prices, sourceURL), nil
}

// sortPrices sorts a slice of NormalizedPrice by PricePerUnit ascending.
func sortPrices(prices []models.NormalizedPrice) {
	for i := 1; i < len(prices); i++ {
		for j := i; j > 0 && prices[j].PricePerUnit < prices[j-1].PricePerUnit; j-- {
			prices[j], prices[j-1] = prices[j-1], prices[j]
		}
	}
}

// --------------------------------------------------------------------------
// GetStoragePrice
// --------------------------------------------------------------------------

// GetStoragePrice returns pricing for block or object storage.
func (p *Provider) GetStoragePrice(
	ctx context.Context,
	storageType string,
	region string,
	sizeGB float64,
) ([]models.NormalizedPrice, error) {
	productName, ok := azureStorageMap[strings.ToLower(storageType)]
	if !ok {
		return nil, fmt.Errorf("azure: unknown storage type %q", storageType)
	}
	isPremiumSSD := productName == "Premium SSD Managed Disks"

	key := cacheKey("storage", region, map[string]string{"storage_type": storageType})
	if cached, ok := p.getCachedPrices(key); ok {
		return filterStorageBySize(cached, isPremiumSSD, sizeGB), nil
	}

	filters := map[string]string{
		"armRegionName": region,
		"priceType":     "Consumption",
		"productName":   productName,
	}
	maxResults := 10
	if isPremiumSSD {
		maxResults = 50
	}
	items, err := p.fetchPrices(ctx, filters, maxResults)
	if err != nil {
		return nil, err
	}

	var prices []models.NormalizedPrice
	for _, item := range items {
		pp := itemToPrice(item, region, models.PricingTermOnDemand, "storage")
		if pp == nil {
			continue
		}
		if isPremiumSSD {
			pp.Unit = models.PriceUnitPerMonth
		} else {
			pp.Unit = models.PriceUnitPerGBMonth
		}
		prices = append(prices, *pp)
	}

	p.setCachedPrices(key, prices, p.cacheTTL)
	result := filterStorageBySize(prices, isPremiumSSD, sizeGB)
	return annotateFresh(result, sourceURL), nil
}

// filterStorageBySize filters Premium SSD prices to the matching P-tier.
func filterStorageBySize(prices []models.NormalizedPrice, isPremiumSSD bool, sizeGB float64) []models.NormalizedPrice {
	if !isPremiumSSD || sizeGB <= 0 {
		return prices
	}
	targetTier := selectPremiumSSDTier(sizeGB)
	tierPrefix := targetTier + " "

	var filtered []models.NormalizedPrice
	for _, p := range prices {
		if strings.HasPrefix(p.Description, tierPrefix) && !strings.Contains(p.Description, "ZRS") {
			filtered = append(filtered, p)
		}
	}
	if len(filtered) == 0 {
		for _, p := range prices {
			if strings.HasPrefix(p.Description, tierPrefix) {
				filtered = append(filtered, p)
			}
		}
	}
	if len(filtered) == 0 {
		return prices
	}
	// Annotate with tier info.
	tierCap := 0
	for _, t := range premiumSSDTiers {
		if t.name == targetTier {
			tierCap = t.capacity
			break
		}
	}
	for i := range filtered {
		filtered[i].Description = fmt.Sprintf("%s (%s: up to %d GiB)", filtered[i].Description, targetTier, tierCap)
		attrs := make(map[string]string, len(filtered[i].Attributes)+3)
		for k, v := range filtered[i].Attributes {
			attrs[k] = v
		}
		attrs["disk_tier"] = targetTier
		attrs["tier_capacity_gib"] = fmt.Sprintf("%d", tierCap)
		attrs["requested_size_gb"] = fmt.Sprintf("%g", sizeGB)
		filtered[i].Attributes = attrs
	}
	return filtered
}

// --------------------------------------------------------------------------
// GetSQLPrice
// --------------------------------------------------------------------------

// GetSQLPrice returns pricing for Azure SQL Database (vCore model).
func (p *Provider) GetSQLPrice(
	ctx context.Context,
	resourceType string,
	region string,
	engine string,
	deployment string,
	term models.PricingTerm,
) ([]models.NormalizedPrice, error) {
	engineLower := strings.ToLower(engine)
	var serviceName string
	switch {
	case strings.Contains(engineLower, "mysql"):
		serviceName = "Azure Database for MySQL"
	case strings.Contains(engineLower, "postgres") || strings.Contains(engineLower, "pg"):
		serviceName = "Azure Database for PostgreSQL"
	default:
		serviceName = "SQL Database"
	}

	key := cacheKey("sql", region, map[string]string{
		"resource_type": resourceType,
		"engine":        engine,
		"deployment":    deployment,
		"term":          string(term),
	})
	if cached, ok := p.getCachedPrices(key); ok {
		return cached, nil
	}

	priceType := "Consumption"
	if term == models.PricingTermReserved1Yr || term == models.PricingTermReserved3Yr {
		priceType = "Reservation"
	}
	filters := map[string]string{
		"armRegionName": region,
		"priceType":     priceType,
		"serviceName":   serviceName,
	}
	if term == models.PricingTermReserved1Yr {
		filters["reservationTerm"] = "1 Year"
	} else if term == models.PricingTermReserved3Yr {
		filters["reservationTerm"] = "3 Years"
	}

	// For Azure SQL Database (engine=SQL) and PostgreSQL, the skuName field does
	// NOT carry the tier prefix (it's "4 vCore", not "GP_Gen5_4"). Instead we
	// filter by armSkuName server-side so that the API returns only matching rows
	// without downloading hundreds of items first.
	//
	// NOTE: Reserved SQL Database meters use a different armSkuName scheme
	// ("SQLDB_GP_Compute_Gen5" without a vCore suffix, priced per-vCore) so we
	// skip the armSkuName filter for reservation terms — the existing post-fetch
	// vcorePattern check handles that path.
	isSQLEngine := serviceName == "SQL Database"
	isPGEngine := serviceName == "Azure Database for PostgreSQL"
	isConsumption := priceType == "Consumption"

	var armSkuFilter string
	if isConsumption {
		if isSQLEngine {
			armSkuFilter = sqlResourceTypeToArmSkuName(resourceType)
		} else if isPGEngine {
			armSkuFilter = pgResourceTypeToArmSkuName(resourceType)
		}
	}
	if armSkuFilter != "" {
		filters["armSkuName"] = armSkuFilter
	}

	items, err := p.fetchPrices(ctx, filters, 100)
	if err != nil {
		return nil, err
	}

	rtLower := strings.ToLower(resourceType)
	var tierPrefix string
	for kw, prefix := range sqlTierMap {
		if strings.Contains(rtLower, kw) {
			tierPrefix = prefix
			break
		}
	}

	// Extract vCore count (first integer in resourceType).
	vcoresStr := ""
	for _, ch := range resourceType {
		if ch >= '0' && ch <= '9' {
			if vcoresStr == "" {
				vcoresStr = string(ch)
			} else {
				vcoresStr += string(ch)
			}
		} else if vcoresStr != "" {
			break
		}
	}

	ha := func() bool {
		d := strings.ToLower(deployment)
		return d == "ha" || d == "zone-redundant" || d == "multi-az" || d == "regional"
	}()

	var vcorePattern *regexp.Regexp
	if vcoresStr != "" {
		vcorePattern = regexp.MustCompile(`(?:^|[^\d])` + regexp.QuoteMeta(vcoresStr) + `(?:[^\d]|$)`)
	}

	var prices []models.NormalizedPrice
	for _, item := range items {
		skuName := item.SkuName
		armSku := item.ArmSkuName

		// Tier-prefix filtering: for SQL and PG engines the skuName does not
		// start with the tier abbreviation (it's "4 vCore", not "GP_Gen5_4").
		// When we have already narrowed results via armSkuName server-side we
		// skip this check; otherwise fall back to checking armSkuName or skuName.
		if tierPrefix != "" && armSkuFilter == "" {
			// Legacy path for MySQL (or future engines without armSkuName mapping).
			// MySQL skuName doesn't use tier prefixes either, so this block is
			// effectively a no-op for well-structured responses.
			if !strings.HasPrefix(skuName, tierPrefix) && !strings.Contains(armSku, "_"+tierPrefix+"_") {
				continue
			}
		}

		// When armSkuFilter is set, the server already narrowed results to the
		// requested vCore count via armSkuName. Applying vcorePattern on top
		// would silently drop rows whose skuName format doesn't match the
		// bare-digit word-boundary (e.g. "4 vCores" would survive, but
		// "GP_Gen5_4 LRS" with an adjacent underscore might not on some patterns).
		if armSkuFilter == "" && vcorePattern != nil && !vcorePattern.MatchString(skuName) {
			continue
		}

		// HA / Zone-Redundancy filtering.
		// For Azure SQL Database the "Zone Redundancy vCore" meter is an incremental
		// uplift charged on top of the base "vCore" meter — it is NOT a replacement
		// full-price row. Returning only the ZR uplift row for ha=true would give a
		// price that is lower than single-az (implausible). We therefore always
		// return the base compute rows and exclude ZR uplift meters.
		// MySQL/PostgreSQL use LRS/ZRS variants; skip ZRS rows for non-ha requests.
		if isSQLEngine {
			if strings.Contains(skuName, "Zone Redundancy") {
				continue
			}
		} else {
			if ha && strings.Contains(skuName, "LRS") {
				continue
			}
			if !ha && strings.Contains(skuName, "ZRS") {
				continue
			}
		}

		pp := itemToPrice(item, region, term, "sql")
		if pp == nil {
			continue
		}
		// T38 fix for SQL reserved pricing.
		switch term { //nolint:exhaustive // only Reserved1Yr/3Yr need adjustment; other terms keep per-hour prices
		case models.PricingTermReserved1Yr:
			pp.PricePerUnit /= 8760.0
		case models.PricingTermReserved3Yr:
			pp.PricePerUnit /= 26280.0
		default:
			// On-demand, spot, and other terms: prices are already per-hour.
		}
		pp.Unit = models.PriceUnitPerHour
		prices = append(prices, *pp)
	}

	// Static fallback: if the API returned no usable rows (e.g. armSkuName
	// mapping is wrong or the region has no pricing rows), return a
	// best-effort estimate so the caller always gets a non-empty result.
	// The fallback is GP Gen5 at the published us-east on-demand rate scaled
	// by the requested vCore count. Non-GP tiers are not modelled here.
	if len(prices) == 0 && vcoresStr != "" && isSQLEngine {
		vcores := 0.0
		if v, err := strconv.ParseFloat(vcoresStr, 64); err == nil {
			vcores = v
		}
		if vcores > 0 {
			totalPerHour := sqlGPVCoreRate * vcores
			fallbackSKU := fmt.Sprintf("azure:sql:%s:%s:static", region, strings.ReplaceAll(strings.ToLower(resourceType), " ", "_"))
			prices = []models.NormalizedPrice{{
				Provider:      models.CloudProviderAzure,
				Service:       "sql",
				SKUID:         fallbackSKU,
				ProductFamily: "Databases",
				Description:   fmt.Sprintf("Azure SQL Database %s — static fallback (%.0f vCores × $%.4f/vCore-hr)", resourceType, vcores, sqlGPVCoreRate),
				Region:        region,
				PricingTerm:   term,
				PricePerUnit:  totalPerHour,
				Unit:          models.PriceUnitPerHour,
				Currency:      "USD",
				Attributes: map[string]string{
					"source": "static_fallback",
					"note":   "armSkuName mapping unverified or API returned no rows; rate is GP Gen5 us-east published price",
				},
			}}
		}
	}

	sortPrices(prices)
	p.setCachedPrices(key, prices, p.cacheTTL)
	return annotateFresh(prices, sourceURL), nil
}

// --------------------------------------------------------------------------
// GetCosmosPrice
// --------------------------------------------------------------------------

// GetCosmosPrice returns pricing for Azure Cosmos DB.
func (p *Provider) GetCosmosPrice(
	ctx context.Context,
	region string,
	deployment string,
	multiRegion bool,
) ([]models.NormalizedPrice, error) {
	multiStr := "false"
	if multiRegion {
		multiStr = "true"
	}
	key := cacheKey("cosmos", region, map[string]string{
		"deployment":   deployment,
		"multi_region": multiStr,
	})
	if cached, ok := p.getCachedPrices(key); ok {
		return cached, nil
	}

	filters := map[string]string{
		"armRegionName": region,
		"priceType":     "Consumption",
		"serviceName":   "Azure Cosmos DB",
	}
	items, err := p.fetchPrices(ctx, filters, 50)
	if err != nil {
		return nil, err
	}

	var prices []models.NormalizedPrice
	for _, item := range items {
		meter := strings.ToLower(item.MeterName)
		product := strings.ToLower(item.ProductName)
		var unit models.PriceUnit

		switch deployment {
		case "serverless":
			if !strings.Contains(meter, "serverless") && !strings.Contains(product, "serverless") {
				continue
			}
			unit = models.PriceUnitPerUnit
		case "autoscale":
			if !strings.Contains(meter, "autoscale") && !strings.Contains(product, "autoscale") {
				continue
			}
			unit = models.PriceUnitPerUnit
		default: // provisioned
			if !strings.Contains(meter, "100 ru/s") && !strings.Contains(product, "throughput") {
				continue
			}
			if multiRegion && strings.Contains(meter, "single") {
				continue
			}
			if !multiRegion && (strings.Contains(meter, "multi") || strings.Contains(meter, "write region")) {
				continue
			}
			unit = models.PriceUnitPerUnit
		}

		pp := itemToPrice(item, region, models.PricingTermOnDemand, "cosmos")
		if pp != nil {
			pp.Unit = unit
			prices = append(prices, *pp)
		}
	}

	// Static fallback when API returns no results.
	if len(prices) == 0 {
		switch deployment {
		case "serverless":
			prices = []models.NormalizedPrice{
				{
					Provider:      models.CloudProviderAzure,
					Service:       "cosmos",
					SKUID:         "cosmos-serverless-ru",
					ProductFamily: "Databases",
					Description:   "Azure Cosmos DB Serverless — per million Request Units",
					Region:        region,
					PricingTerm:   models.PricingTermOnDemand,
					PricePerUnit:  cosmosServerlessRate,
					Unit:          models.PriceUnitPerUnit,
					Currency:      "USD",
					Attributes:    map[string]string{"unit_label": "per million RUs", "source": "static_fallback"},
				},
				{
					Provider:      models.CloudProviderAzure,
					Service:       "cosmos",
					SKUID:         "cosmos-serverless-storage",
					ProductFamily: "Databases",
					Description:   "Azure Cosmos DB Serverless — Storage",
					Region:        region,
					PricingTerm:   models.PricingTermOnDemand,
					PricePerUnit:  cosmosStorageRate,
					Unit:          models.PriceUnitPerGBMonth,
					Currency:      "USD",
					Attributes:    map[string]string{"cosmos_dimension": "storage", "source": "static_fallback"},
				},
			}
		case "autoscale":
			prices = []models.NormalizedPrice{
				{
					Provider:      models.CloudProviderAzure,
					Service:       "cosmos",
					SKUID:         "cosmos-autoscale-ru",
					ProductFamily: "Databases",
					Description:   "Azure Cosmos DB Autoscale — per 100 RU/s per hour",
					Region:        region,
					PricingTerm:   models.PricingTermOnDemand,
					PricePerUnit:  cosmosProvisionedRate,
					Unit:          models.PriceUnitPerUnit,
					Currency:      "USD",
					Attributes:    map[string]string{"unit_label": "per 100 RU/s per hour", "source": "static_fallback"},
				},
				{
					Provider:      models.CloudProviderAzure,
					Service:       "cosmos",
					SKUID:         "cosmos-autoscale-storage",
					ProductFamily: "Databases",
					Description:   "Azure Cosmos DB Autoscale — Storage",
					Region:        region,
					PricingTerm:   models.PricingTermOnDemand,
					PricePerUnit:  cosmosStorageRate,
					Unit:          models.PriceUnitPerGBMonth,
					Currency:      "USD",
					Attributes:    map[string]string{"cosmos_dimension": "storage", "source": "static_fallback"},
				},
			}
		default: // provisioned
			prices = []models.NormalizedPrice{
				{
					Provider:      models.CloudProviderAzure,
					Service:       "cosmos",
					SKUID:         "cosmos-provisioned-ru",
					ProductFamily: "Databases",
					Description:   "Azure Cosmos DB Provisioned — per 100 RU/s per hour",
					Region:        region,
					PricingTerm:   models.PricingTermOnDemand,
					PricePerUnit:  cosmosProvisionedRate,
					Unit:          models.PriceUnitPerUnit,
					Currency:      "USD",
					Attributes:    map[string]string{"unit_label": "per 100 RU/s per hour", "source": "static_fallback"},
				},
				{
					Provider:      models.CloudProviderAzure,
					Service:       "cosmos",
					SKUID:         "cosmos-provisioned-storage",
					ProductFamily: "Databases",
					Description:   "Azure Cosmos DB — Storage",
					Region:        region,
					PricingTerm:   models.PricingTermOnDemand,
					PricePerUnit:  cosmosStorageRate,
					Unit:          models.PriceUnitPerGBMonth,
					Currency:      "USD",
					Attributes:    map[string]string{"cosmos_dimension": "storage", "source": "static_fallback"},
				},
			}
		}
	}

	sortPrices(prices)
	p.setCachedPrices(key, prices, p.cacheTTL)
	return annotateFresh(prices, sourceURL), nil
}

// --------------------------------------------------------------------------
// GetAKSPrice
// --------------------------------------------------------------------------

// GetAKSPrice returns pricing for AKS cluster management fee.
func (p *Provider) GetAKSPrice(
	ctx context.Context,
	region string,
	mode string,
) ([]models.NormalizedPrice, error) {
	key := cacheKey("aks", region, map[string]string{"mode": mode})
	if cached, ok := p.getCachedPrices(key); ok {
		return cached, nil
	}

	var prices []models.NormalizedPrice

	if mode == "free" {
		zero := 0.0
		prices = []models.NormalizedPrice{{
			Provider:      models.CloudProviderAzure,
			Service:       "aks",
			SKUID:         "aks-free-tier",
			ProductFamily: "Containers",
			Description:   "AKS Free tier — control plane included (no uptime SLA)",
			Region:        region,
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  zero,
			Unit:          models.PriceUnitPerHour,
			Currency:      "USD",
			Attributes: map[string]string{
				"note": "Worker node VMs billed separately via compute pricing",
				"sla":  "No uptime SLA",
			},
		}}
	} else {
		filters := map[string]string{
			"armRegionName": region,
			"priceType":     "Consumption",
			"serviceName":   "Azure Kubernetes Service",
		}
		items, err := p.fetchPrices(ctx, filters, 20)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			meter := strings.ToLower(item.MeterName)
			if !strings.Contains(meter, "uptime sla") && !strings.Contains(meter, "standard") {
				continue
			}
			pp := itemToPrice(item, region, models.PricingTermOnDemand, "aks")
			if pp != nil {
				pp.Description = "AKS Standard tier — cluster management fee (Uptime SLA)"
				pp.Attributes["note"] = "Worker node VMs billed separately via compute pricing"
				prices = append(prices, *pp)
			}
		}
		if len(prices) == 0 {
			prices = []models.NormalizedPrice{{
				Provider:      models.CloudProviderAzure,
				Service:       "aks",
				SKUID:         "aks-standard-tier",
				ProductFamily: "Containers",
				Description:   "AKS Standard tier — cluster management fee (Uptime SLA)",
				Region:        region,
				PricingTerm:   models.PricingTermOnDemand,
				PricePerUnit:  aksPremiumRate,
				Unit:          models.PriceUnitPerHour,
				Currency:      "USD",
				Attributes: map[string]string{
					"note":   "Worker node VMs billed separately via compute pricing",
					"source": "static_fallback",
				},
			}}
		}
	}

	p.setCachedPrices(key, prices, p.cacheTTL)
	return annotateFresh(prices, sourceURL), nil
}

// --------------------------------------------------------------------------
// GetFunctionsPrice
// --------------------------------------------------------------------------

// GetFunctionsPrice returns pricing for Azure Functions Consumption plan.
func (p *Provider) GetFunctionsPrice(
	ctx context.Context,
	region string,
	gbSeconds float64,
	requestsMillions float64,
) ([]models.NormalizedPrice, error) {
	key := cacheKey("azure_functions", region, nil)
	var prices []models.NormalizedPrice
	var fromCache bool

	if cached, ok := p.getCachedPrices(key); ok {
		prices = cached
		fromCache = true
	} else {
		filters := map[string]string{
			"armRegionName": region,
			"priceType":     "Consumption",
			"serviceName":   "Functions",
			// Restrict to classic Consumption plan; excludes Flex Consumption and Premium
			"productName": "Functions",
		}
		items, err := p.fetchPrices(ctx, filters, 30)
		if err != nil {
			return nil, err
		}
		// Further narrow to Standard SKU (Consumption plan); skip Flex/Premium leftovers
		var standardItems []azureRetailItem
		for _, item := range items {
			if strings.EqualFold(item.SkuName, "standard") {
				standardItems = append(standardItems, item)
			}
		}
		items = standardItems
		for _, item := range items {
			meter := strings.ToLower(item.MeterName)
			uom := strings.ToLower(item.UnitOfMeasure)
			rawUOM := strings.TrimSpace(item.UnitOfMeasure)
			pp := itemToPrice(item, region, models.PricingTermOnDemand, "azure_functions")
			if pp == nil {
				continue
			}
			switch {
			// Check GB-second BEFORE execution: "Standard Execution Time" has uom="1 GB Second"
			// and would otherwise match "execution in meter" first (wrong unit).
			case strings.Contains(uom, "gb") && strings.Contains(uom, "second"):
				pp.Unit = models.PriceUnitPerGBSecond
			case strings.Contains(meter, "execution") || strings.Contains(meter, "invocation"):
				// API prices executions per N (e.g. unitOfMeasure="10 Million"); normalise to per-1.
				if parts := strings.Fields(rawUOM); len(parts) > 0 {
					if denom, err := strconv.ParseFloat(parts[0], 64); err == nil && denom > 1 {
						pp.PricePerUnit /= denom
					}
				}
				pp.Unit = models.PriceUnitPerRequest
			case strings.Contains(meter, "vcpu"):
				pp.Unit = models.PriceUnitPerHour
				pp.Attributes["plan"] = "premium"
				pp.Attributes["dimension"] = "vcpu_hour"
			case strings.Contains(meter, "memory") && strings.Contains(meter, "duration"):
				pp.Unit = models.PriceUnitPerHour
				pp.Attributes["plan"] = "premium"
				pp.Attributes["dimension"] = "gib_hour"
			default:
				continue
			}
			prices = append(prices, *pp)
		}

		if len(prices) == 0 {
			prices = []models.NormalizedPrice{
				{
					Provider:      models.CloudProviderAzure,
					Service:       "azure_functions",
					SKUID:         "functions-gb-second",
					ProductFamily: "Serverless",
					Description:   "Azure Functions — compute duration (Consumption plan)",
					Region:        region,
					PricingTerm:   models.PricingTermOnDemand,
					PricePerUnit:  functionsGBSecRate,
					Unit:          models.PriceUnitPerGBSecond,
					Currency:      "USD",
					Attributes:    map[string]string{"free_tier": "400,000 GB-s/month", "source": "static_fallback"},
				},
				{
					Provider:      models.CloudProviderAzure,
					Service:       "azure_functions",
					SKUID:         "functions-executions",
					ProductFamily: "Serverless",
					Description:   "Azure Functions — execution count (Consumption plan)",
					Region:        region,
					PricingTerm:   models.PricingTermOnDemand,
					PricePerUnit:  functionsExecRate,
					Unit:          models.PriceUnitPerRequest,
					Currency:      "USD",
					Attributes:    map[string]string{"free_tier": "1M executions/month", "source": "static_fallback"},
				},
			}
		}
		p.setCachedPrices(key, prices, p.cacheTTL)
		prices = annotateFresh(prices, sourceURL)
	}
	_ = fromCache

	// Cost estimate if volumes provided.
	if gbSeconds > 0 || requestsMillions > 0 {
		var gbSecPrice, reqPrice *models.NormalizedPrice
		for i := range prices {
			if prices[i].Unit == models.PriceUnitPerGBSecond {
				p2 := prices[i]
				gbSecPrice = &p2
			} else if prices[i].Unit == models.PriceUnitPerRequest {
				p2 := prices[i]
				reqPrice = &p2
			}
		}

		total := 0.0
		var breakdownParts []string
		if gbSecPrice != nil && gbSeconds > 0 {
			billable := math.Max(0, gbSeconds-400_000)
			cost := gbSecPrice.PricePerUnit * billable
			total += cost
			breakdownParts = append(breakdownParts, fmt.Sprintf("%.0f GB-s × $%.6f = $%.4f", billable, gbSecPrice.PricePerUnit, cost))
		}
		if reqPrice != nil && requestsMillions > 0 {
			reqCount := requestsMillions * 1_000_000
			billableReq := math.Max(0, reqCount-1_000_000)
			cost := reqPrice.PricePerUnit * billableReq
			total += cost
			breakdownParts = append(breakdownParts, fmt.Sprintf("%.0f executions × $%.8f = $%.4f", billableReq, reqPrice.PricePerUnit, cost))
		}
		if len(breakdownParts) > 0 {
			synthetic := models.NormalizedPrice{
				Provider:      models.CloudProviderAzure,
				Service:       "azure_functions",
				SKUID:         "functions-estimate",
				ProductFamily: "Serverless",
				Description:   "Azure Functions — estimated monthly cost",
				Region:        region,
				PricingTerm:   models.PricingTermOnDemand,
				PricePerUnit:  total,
				Unit:          models.PriceUnitPerUnit,
				Currency:      "USD",
				Attributes: map[string]string{
					"breakdown": strings.Join(breakdownParts, "; "),
					"note":      "After free tier deduction (400K GB-s, 1M executions/month)",
				},
			}
			if len(prices) > 0 {
				ref := prices[0]
				synthetic.FetchedAt = ref.FetchedAt
				synthetic.SourceURL = ref.SourceURL
				synthetic.CacheAgeSeconds = ref.CacheAgeSeconds
			}
			prices = append(prices, synthetic)
		}
	}
	return prices, nil
}

// --------------------------------------------------------------------------
// GetOpenAIPrice
// --------------------------------------------------------------------------

// GetOpenAIPrice returns pricing for Azure OpenAI model inference.
func (p *Provider) GetOpenAIPrice(
	ctx context.Context,
	model string,
	region string,
	inputTokens int,
	outputTokens int,
) ([]models.NormalizedPrice, error) {
	modelLower := strings.ToLower(model)
	key := cacheKey("openai", region, map[string]string{"model": modelLower})
	var prices []models.NormalizedPrice

	if cached, ok := p.getCachedPrices(key); ok {
		prices = cached
	} else {
		filters := map[string]string{
			"armRegionName": region,
			"priceType":     "Consumption",
			"serviceName":   "Foundry Models",
		}
		items, err := p.fetchPrices(ctx, filters, 100)
		if err != nil {
			return nil, err
		}

		modelNorm := strings.ReplaceAll(modelLower, "-", " ")
		modelCompact := strings.ReplaceAll(strings.ReplaceAll(modelLower, "-", ""), " ", "")

		// Word-boundary patterns — prevent prefix collisions (e.g. "gpt-4" must
		// not match "gpt-4.1" or "gpt-4o" SKUs).  RE2 has no lookahead, so we
		// consume one trailing boundary character instead.
		skuPattern := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(modelCompact) + `(?:[^a-zA-Z0-9.]|$)`)
		prodPattern := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(modelNorm) + `(?:[^\w.]|$)`)

		for _, item := range items {
			meter := strings.ToLower(item.MeterName)
			product := strings.ToLower(item.ProductName)
			skuLower := strings.ToLower(item.SkuName)
			skuCompact := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(skuLower, "-", ""), "_", ""), " ", "")

			if !prodPattern.MatchString(product) &&
				!prodPattern.MatchString(meter) &&
				!skuPattern.MatchString(skuCompact) {
				continue
			}
			pp := itemToPrice(item, region, models.PricingTermOnDemand, "openai")
			if pp != nil {
				pp.Unit = models.PriceUnitPerUnit
				// Classify deployment variant so we can surface standard text pricing.
				combined := meter + " " + skuLower
				var dtype string
				switch {
				case strings.Contains(combined, "realtime") || strings.Contains(combined, "rtime") ||
					strings.Contains(combined, "rt-") || strings.Contains(combined, "-rt-"):
					dtype = "realtime"
				case strings.Contains(combined, "audio") || strings.Contains(combined, "-aud") ||
					strings.Contains(combined, " aud"):
					dtype = "audio"
				case strings.Contains(combined, "batch"):
					dtype = "batch"
				case strings.Contains(combined, "cached") || strings.Contains(combined, "cchd"):
					dtype = "cached"
				default:
					dtype = "standard"
				}
				if pp.Attributes == nil {
					pp.Attributes = make(map[string]string)
				}
				pp.Attributes["deployment_type"] = dtype
				prices = append(prices, *pp)
			}
		}

		// Prefer standard text deployments; non-standard variants (realtime/audio/
		// batch/cached) inflate per-token cost estimates.
		var standardPrices []models.NormalizedPrice
		for _, pr := range prices {
			if pr.Attributes != nil && pr.Attributes["deployment_type"] == "standard" {
				standardPrices = append(standardPrices, pr)
			}
		}
		if len(standardPrices) > 0 {
			prices = standardPrices
		}

		if len(prices) == 0 || len(standardPrices) == 0 {
			// Static fallback — pick the most specific (longest-key) match to
			// avoid "gpt-4" matching "gpt-4o" before "gpt-4o" does.
			var foundRate *openAIRate
			bestLen := 0
			for k, rate := range openAIStaticRates {
				if strings.Contains(modelLower, k) && len(k) > bestLen {
					r := rate
					foundRate = &r
					bestLen = len(k)
				}
			}
			if foundRate != nil {
				prices = []models.NormalizedPrice{
					{
						Provider:      models.CloudProviderAzure,
						Service:       "openai",
						SKUID:         fmt.Sprintf("openai-%s-input", modelLower),
						ProductFamily: "AI + Machine Learning",
						Description:   fmt.Sprintf("Azure OpenAI %s — input tokens", model),
						Region:        region,
						PricingTerm:   models.PricingTermOnDemand,
						PricePerUnit:  foundRate.input,
						Unit:          models.PriceUnitPerUnit,
						Currency:      "USD",
						Attributes: map[string]string{
							"model":      model,
							"token_type": "input",
							"unit_label": "per 1K tokens",
							"source":     "static_fallback",
						},
					},
					{
						Provider:      models.CloudProviderAzure,
						Service:       "openai",
						SKUID:         fmt.Sprintf("openai-%s-output", modelLower),
						ProductFamily: "AI + Machine Learning",
						Description:   fmt.Sprintf("Azure OpenAI %s — output tokens", model),
						Region:        region,
						PricingTerm:   models.PricingTermOnDemand,
						PricePerUnit:  foundRate.output,
						Unit:          models.PriceUnitPerUnit,
						Currency:      "USD",
						Attributes: map[string]string{
							"model":      model,
							"token_type": "output",
							"unit_label": "per 1K tokens",
							"source":     "static_fallback",
						},
					},
				}
			}
		}

		p.setCachedPrices(key, prices, p.cacheTTL)
		prices = annotateFresh(prices, sourceURL)
	}

	// Cost estimate.
	if len(prices) > 0 && (inputTokens > 0 || outputTokens > 0) {
		var inPrice, outPrice *models.NormalizedPrice
		for i := range prices {
			if prices[i].Attributes != nil {
				if prices[i].Attributes["token_type"] == "input" ||
					strings.Contains(strings.ToLower(prices[i].Description), "input") {
					p2 := prices[i]
					inPrice = &p2
				} else if prices[i].Attributes["token_type"] == "output" ||
					strings.Contains(strings.ToLower(prices[i].Description), "output") {
					p2 := prices[i]
					outPrice = &p2
				}
			}
		}
		total := 0.0
		var breakdownParts []string
		if inPrice != nil && inputTokens > 0 {
			cost := inPrice.PricePerUnit * float64(inputTokens) / 1000.0
			total += cost
			breakdownParts = append(breakdownParts, fmt.Sprintf("%d input tokens × $%.5f/1K = $%.4f", inputTokens, inPrice.PricePerUnit, cost))
		}
		if outPrice != nil && outputTokens > 0 {
			cost := outPrice.PricePerUnit * float64(outputTokens) / 1000.0
			total += cost
			breakdownParts = append(breakdownParts, fmt.Sprintf("%d output tokens × $%.5f/1K = $%.4f", outputTokens, outPrice.PricePerUnit, cost))
		}
		if len(breakdownParts) > 0 {
			synthetic := models.NormalizedPrice{
				Provider:      models.CloudProviderAzure,
				Service:       "openai",
				SKUID:         fmt.Sprintf("openai-%s-estimate", modelLower),
				ProductFamily: "AI + Machine Learning",
				Description:   fmt.Sprintf("Azure OpenAI %s — estimated cost", model),
				Region:        region,
				PricingTerm:   models.PricingTermOnDemand,
				PricePerUnit:  total,
				Unit:          models.PriceUnitPerUnit,
				Currency:      "USD",
				Attributes: map[string]string{
					"breakdown": strings.Join(breakdownParts, "; "),
					"model":     model,
				},
			}
			ref := prices[0]
			synthetic.FetchedAt = ref.FetchedAt
			synthetic.SourceURL = ref.SourceURL
			synthetic.CacheAgeSeconds = ref.CacheAgeSeconds
			prices = append(prices, synthetic)
		}
	}
	return prices, nil
}

// --------------------------------------------------------------------------
// GetEgressPrice
// --------------------------------------------------------------------------

// GetEgressPrice returns Azure outbound data transfer pricing.
func (p *Provider) GetEgressPrice(
	ctx context.Context,
	sourceRegion string,
	destRegion string,
	dataGB float64,
) ([]models.NormalizedPrice, error) {
	zone := azureEgressZone[strings.ToLower(sourceRegion)]
	if zone == "" {
		zone = "zone1"
	}

	cacheMetaKey := fmt.Sprintf("azure:egress-meta:%s:rate", zone)
	rate := 0.0

	if raw, ok := p.cache.Get(cacheMetaKey); ok {
		var m map[string]string
		if err := json.Unmarshal(raw, &m); err == nil {
			if r, ok2 := m["rate"]; ok2 {
				fmt.Sscanf(r, "%f", &rate) //nolint
			}
		}
	}

	if rate == 0 {
		zoneLabel := strings.ReplaceAll(zone, "zone", "zone ")
		items, err := p.fetchPrices(ctx, map[string]string{"serviceName": "Bandwidth"}, 100)
		if err == nil {
			// Pick the item with TierMinimumUnits == 0 — that is the base tier
			// (5 GB–10 TB), which has the highest per-unit rate and applies to
			// small amounts.  If no item has TierMinimumUnits == 0, fall through
			// to the static fallback.
			for _, item := range items {
				meter := strings.ToLower(item.MeterName)
				if strings.Contains(meter, "out") && strings.Contains(meter, zoneLabel) &&
					item.RetailPrice > 0 && item.TierMinimumUnits == 0 {
					rate = item.RetailPrice
					break
				}
			}
		}
		if rate == 0 {
			rate = azureEgressBaseRate[zone]
			if rate == 0 {
				rate = 0.087
			}
		}
		metaBytes, _ := json.Marshal(map[string]string{"rate": fmt.Sprintf("%f", rate)})
		p.cache.Set(cacheMetaKey, metaBytes, p.cacheTTL)
	}

	dataVal := math.Max(0, dataGB)
	chargeable := math.Max(0, dataVal-5.0)
	monthly := chargeable * rate

	egressType := "internet"
	if destRegion != "" {
		egressType = "inter-region"
	}
	desc := fmt.Sprintf("Azure outbound data transfer (%s) from %s", egressType, sourceRegion)
	if destRegion != "" {
		desc += " to " + destRegion
	}

	attrs := map[string]string{
		"source_region": sourceRegion,
		"egress_type":   egressType,
		"zone":          zone,
		"free_tier_gb":  "5",
		"note":          fmt.Sprintf("First 5 GB/month free. %s rate (base tier, 5 GB–10 TB).", strings.ToUpper(zone)),
	}
	if destRegion != "" {
		attrs["dest_region"] = destRegion
	}
	if dataGB > 0 {
		attrs["monthly_estimate"] = fmt.Sprintf("$%.4f for %.1f GB (%.1f GB chargeable)", monthly, dataGB, chargeable)
	}

	price := models.NormalizedPrice{
		Provider:      models.CloudProviderAzure,
		Service:       "egress",
		SKUID:         fmt.Sprintf("azure:egress:%s:%s:%s", sourceRegion, destRegion, zone),
		ProductFamily: "Bandwidth",
		Description:   desc,
		Region:        sourceRegion,
		Attributes:    attrs,
		PricingTerm:   models.PricingTermOnDemand,
		PricePerUnit:  rate,
		Unit:          models.PriceUnitPerGBMonth,
		Currency:      "USD",
	}
	return annotateFresh([]models.NormalizedPrice{price}, sourceURL), nil
}

// --------------------------------------------------------------------------
// GetMonitorPrice
// --------------------------------------------------------------------------

// GetMonitorPrice returns pricing for Azure Monitor.
func (p *Provider) GetMonitorPrice(
	ctx context.Context,
	region string,
	logGB float64,
	metricsMillions float64,
	alertRules int,
) ([]models.NormalizedPrice, error) {
	key := cacheKey("azure_monitor", region, nil)
	var prices []models.NormalizedPrice

	if cached, ok := p.getCachedPrices(key); ok {
		prices = cached
	} else {
		filters := map[string]string{
			"armRegionName": region,
			"priceType":     "Consumption",
			"serviceName":   "Azure Monitor",
		}
		items, err := p.fetchPrices(ctx, filters, 300)
		if err != nil {
			return nil, err
		}

		for _, item := range items {
			meter := strings.ToLower(item.MeterName)
			var unit models.PriceUnit
			switch {
			case strings.Contains(meter, "basic logs data ingestion"):
				unit = models.PriceUnitPerGB
			case strings.Contains(meter, "auxiliary logs data ingestion"):
				unit = models.PriceUnitPerGB
			case strings.Contains(meter, "metrics ingestion") && strings.Contains(meter, "metric samples"):
				unit = models.PriceUnitPerUnit
			case meter == "alerts metric monitored":
				unit = models.PriceUnitPerUnit
			default:
				continue
			}
			pp := itemToPrice(item, region, models.PricingTermOnDemand, "azure_monitor")
			if pp == nil {
				continue
			}
			pp.Unit = unit
			pp.Attributes["monitor_dimension"] = meter
			prices = append(prices, *pp)
		}

		if len(prices) == 0 {
			prices = []models.NormalizedPrice{
				{
					Provider:      models.CloudProviderAzure,
					Service:       "azure_monitor",
					SKUID:         "monitor-analytics-logs",
					ProductFamily: "Management and Governance",
					Description:   "Azure Monitor — Analytics Logs ingestion (after 5 GB free)",
					Region:        region,
					PricingTerm:   models.PricingTermOnDemand,
					PricePerUnit:  monitorLogRate,
					Unit:          models.PriceUnitPerGB,
					Currency:      "USD",
					Attributes:    map[string]string{"free_tier": "5 GB/month", "source": "static_fallback", "log_tier": "analytics"},
				},
				{
					Provider:      models.CloudProviderAzure,
					Service:       "azure_monitor",
					SKUID:         "monitor-basic-logs",
					ProductFamily: "Management and Governance",
					Description:   "Azure Monitor — Basic Logs ingestion",
					Region:        region,
					PricingTerm:   models.PricingTermOnDemand,
					PricePerUnit:  monitorBasicLogRate,
					Unit:          models.PriceUnitPerGB,
					Currency:      "USD",
					Attributes:    map[string]string{"source": "static_fallback", "log_tier": "basic"},
				},
				{
					Provider:      models.CloudProviderAzure,
					Service:       "azure_monitor",
					SKUID:         "monitor-metrics",
					ProductFamily: "Management and Governance",
					Description:   "Azure Monitor — Metrics ingestion",
					Region:        region,
					PricingTerm:   models.PricingTermOnDemand,
					PricePerUnit:  monitorMetricsRate,
					Unit:          models.PriceUnitPerUnit,
					Currency:      "USD",
					Attributes:    map[string]string{"unit_label": "per 10M metric samples", "source": "static_fallback"},
				},
			}
		}

		p.setCachedPrices(key, prices, p.cacheTTL)
		prices = annotateFresh(prices, sourceURL)
	}

	// Cost estimate.
	if logGB > 0 || metricsMillions > 0 || alertRules > 0 {
		var metricsPrice, alertPrice *models.NormalizedPrice
		for i := range prices {
			dim := prices[i].Attributes["monitor_dimension"]
			if strings.Contains(dim, "metrics ingestion") {
				p2 := prices[i]
				metricsPrice = &p2
			} else if dim == "alerts metric monitored" {
				p2 := prices[i]
				alertPrice = &p2
			}
		}
		if metricsPrice == nil {
			for i := range prices {
				if prices[i].Unit == models.PriceUnitPerUnit && strings.Contains(strings.ToLower(prices[i].Description), "metric") {
					p2 := prices[i]
					metricsPrice = &p2
					break
				}
			}
		}

		total := 0.0
		var breakdownParts []string

		if logGB > 0 {
			billable := math.Max(0, logGB-monitorFreeLogGB)
			cost := monitorLogRate * billable
			total += cost
			breakdownParts = append(breakdownParts, fmt.Sprintf("%.1f GB × $%.2f/GB (Analytics Logs) = $%.4f", billable, monitorLogRate, cost))
		}
		if metricsPrice != nil && metricsMillions > 0 {
			cost := metricsPrice.PricePerUnit * (metricsMillions / 10.0)
			total += cost
			breakdownParts = append(breakdownParts, fmt.Sprintf("%.1fM samples × $%.2f/10M = $%.4f", metricsMillions, metricsPrice.PricePerUnit, cost))
		}
		if alertRules > 0 {
			billableRules := alertRules - monitorFreeAlerts
			if billableRules < 0 {
				billableRules = 0
			}
			rate := monitorAlertRate
			if alertPrice != nil {
				rate = alertPrice.PricePerUnit
			}
			cost := rate * float64(billableRules)
			total += cost
			breakdownParts = append(breakdownParts, fmt.Sprintf("%d rules × $%.2f/mo = $%.4f", billableRules, rate, cost))
		}

		if len(breakdownParts) > 0 {
			synthetic := models.NormalizedPrice{
				Provider:      models.CloudProviderAzure,
				Service:       "azure_monitor",
				SKUID:         "monitor-estimate",
				ProductFamily: "Management and Governance",
				Description:   "Azure Monitor — estimated monthly cost",
				Region:        region,
				PricingTerm:   models.PricingTermOnDemand,
				PricePerUnit:  total,
				Unit:          models.PriceUnitPerUnit,
				Currency:      "USD",
				Attributes: map[string]string{
					"breakdown": strings.Join(breakdownParts, "; "),
					"note":      "Analytics Logs free tier: 5 GB/month. Metric alerts: first 10 rules free.",
				},
			}
			if len(prices) > 0 {
				ref := prices[0]
				synthetic.FetchedAt = ref.FetchedAt
				synthetic.SourceURL = ref.SourceURL
				synthetic.CacheAgeSeconds = ref.CacheAgeSeconds
			}
			prices = append(prices, synthetic)
		}
	}
	return prices, nil
}

// --------------------------------------------------------------------------
// GetCDNPrice
// --------------------------------------------------------------------------

// GetCDNPrice returns pricing for Azure CDN.
func (p *Provider) GetCDNPrice(
	ctx context.Context,
	region string,
	dataGB float64,
	sku string,
) ([]models.NormalizedPrice, error) {
	zoneLabel := cdnZone[strings.ToLower(region)]
	if zoneLabel == "" {
		zoneLabel = "Zone 1"
	}
	key := cacheKey("azure_cdn", region, map[string]string{"sku": sku, "zone": zoneLabel})
	var prices []models.NormalizedPrice

	if cached, ok := p.getCachedPrices(key); ok {
		prices = cached
	} else {
		filters := map[string]string{
			"priceType":   "Consumption",
			"serviceName": "Content Delivery Network",
		}
		items, err := p.fetchPrices(ctx, filters, 700)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			meter := strings.ToLower(item.MeterName)
			skuName := strings.ToLower(item.SkuName)
			product := strings.ToLower(item.ProductName)

			if !strings.Contains(meter, "data transfer") {
				continue
			}
			if sku == "premium" && !strings.Contains(skuName, "premium") {
				continue
			}
			if sku == "standard" && !strings.Contains(skuName, "standard") {
				continue
			}
			if strings.Contains(product, "akamai") || strings.Contains(product, "verizon") {
				continue
			}
			pp := itemToPrice(item, region, models.PricingTermOnDemand, "azure_cdn")
			if pp == nil {
				continue
			}
			pp.Unit = models.PriceUnitPerGB
			pp.Attributes["cdn_zone"] = zoneLabel
			pp.Attributes["cdn_sku"] = sku
			prices = append(prices, *pp)
		}

		if len(prices) == 0 {
			staticRate := cdnStaticRates[zoneLabel]
			if staticRate == 0 {
				staticRate = cdnStaticRates["Zone 1"]
			}
			zoneSlug := strings.ToLower(strings.ReplaceAll(zoneLabel, " ", ""))
			prices = []models.NormalizedPrice{{
				Provider:      models.CloudProviderAzure,
				Service:       "azure_cdn",
				SKUID:         fmt.Sprintf("cdn-%s-standard-dt", zoneSlug),
				ProductFamily: "Networking",
				Description:   fmt.Sprintf("Azure CDN Standard — Data Transfer Out (%s)", zoneLabel),
				Region:        region,
				PricingTerm:   models.PricingTermOnDemand,
				PricePerUnit:  staticRate,
				Unit:          models.PriceUnitPerGB,
				Currency:      "USD",
				Attributes: map[string]string{
					"cdn_zone": zoneLabel,
					"cdn_sku":  sku,
					"source":   "static_fallback",
					"note":     fmt.Sprintf("%s base tier (0-10 TB). Prices decrease at higher volumes.", zoneLabel),
				},
			}}
		}

		p.setCachedPrices(key, prices, p.cacheTTL)
		prices = annotateFresh(prices, sourceURL)
	}

	if dataGB > 0 && len(prices) > 0 {
		// Pick highest $/GB price as base rate.
		base := prices[0]
		for _, p2 := range prices {
			if p2.PricePerUnit > base.PricePerUnit {
				base = p2
			}
		}
		cost := base.PricePerUnit * dataGB
		synthetic := models.NormalizedPrice{
			Provider:      models.CloudProviderAzure,
			Service:       "azure_cdn",
			SKUID:         "cdn-estimate",
			ProductFamily: "Networking",
			Description:   fmt.Sprintf("Azure CDN — estimated monthly cost for %.1f GB", dataGB),
			Region:        region,
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  cost,
			Unit:          models.PriceUnitPerUnit,
			Currency:      "USD",
			Attributes: map[string]string{
				"breakdown": fmt.Sprintf("%.1f GB × $%.4f/GB = $%.4f", dataGB, base.PricePerUnit, cost),
				"cdn_zone":  zoneLabel,
				"note":      "Base tier rate. Volume discounts apply above 10 TB/month.",
			},
		}
		ref := prices[0]
		synthetic.FetchedAt = ref.FetchedAt
		synthetic.SourceURL = ref.SourceURL
		synthetic.CacheAgeSeconds = ref.CacheAgeSeconds
		prices = append(prices, synthetic)
	}
	return prices, nil
}

// --------------------------------------------------------------------------
// GetFrontDoorPrice
// --------------------------------------------------------------------------

// GetFrontDoorPrice returns pricing for Azure Front Door.
func (p *Provider) GetFrontDoorPrice(
	ctx context.Context,
	region string,
	dataGB float64,
	monthlyRequestsMillions float64,
	sku string,
) ([]models.NormalizedPrice, error) {
	zoneLabel := cdnZone[strings.ToLower(region)]
	if zoneLabel == "" {
		zoneLabel = "Zone 1"
	}
	key := cacheKey("azure_front_door", region, map[string]string{"sku": sku, "zone": zoneLabel})
	var prices []models.NormalizedPrice

	if cached, ok := p.getCachedPrices(key); ok {
		prices = cached
	} else {
		filters := map[string]string{
			"priceType":   "Consumption",
			"serviceName": "Azure Front Door Service",
		}
		items, err := p.fetchPrices(ctx, filters, 500)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			meter := strings.ToLower(item.MeterName)
			skuName := strings.ToLower(item.SkuName)
			uom := strings.ToLower(item.UnitOfMeasure)
			product := strings.ToLower(item.ProductName)

			if sku == "premium" && !strings.Contains(skuName, "premium") {
				continue
			}
			if sku == "standard" && !strings.Contains(skuName, "standard") {
				continue
			}
			if !strings.Contains(product, "front door service") && !strings.Contains(product, "front door") {
				continue
			}

			var unit models.PriceUnit
			switch {
			case strings.Contains(meter, "data transfer out"):
				unit = models.PriceUnitPerGB
			case strings.Contains(meter, "requests") && strings.Contains(uom, "10k"):
				unit = models.PriceUnitPerUnit
			default:
				continue
			}

			pp := itemToPrice(item, region, models.PricingTermOnDemand, "azure_front_door")
			if pp == nil {
				continue
			}
			pp.Unit = unit
			pp.Attributes["cdn_zone"] = zoneLabel
			pp.Attributes["front_door_sku"] = sku
			if unit == models.PriceUnitPerUnit {
				pp.Attributes["unit_label"] = "per 10K requests"
			}
			prices = append(prices, *pp)
		}

		if len(prices) == 0 {
			fdDTRate := frontDoorStaticRates[zoneLabel]
			if fdDTRate == 0 {
				fdDTRate = frontDoorStaticRates["Zone 1"]
			}
			fdReqRate := frontDoorReqStaticRates[zoneLabel]
			if fdReqRate == 0 {
				fdReqRate = frontDoorReqStaticRates["Zone 1"]
			}
			zoneSlug := strings.ToLower(strings.ReplaceAll(zoneLabel, " ", ""))
			prices = []models.NormalizedPrice{
				{
					Provider:      models.CloudProviderAzure,
					Service:       "azure_front_door",
					SKUID:         fmt.Sprintf("frontdoor-%s-standard-base", zoneSlug),
					ProductFamily: "Networking",
					Description:   "Azure Front Door Standard — Base fee",
					Region:        region,
					PricingTerm:   models.PricingTermOnDemand,
					PricePerUnit:  frontDoorBaseFee,
					Unit:          models.PriceUnitPerMonth,
					Currency:      "USD",
					Attributes:    map[string]string{"cdn_zone": zoneLabel, "front_door_sku": sku, "source": "static_fallback", "note": "Standard tier base fee per month"},
				},
				{
					Provider:      models.CloudProviderAzure,
					Service:       "azure_front_door",
					SKUID:         fmt.Sprintf("frontdoor-%s-standard-dt", zoneSlug),
					ProductFamily: "Networking",
					Description:   fmt.Sprintf("Azure Front Door Standard — Data Transfer Out (%s)", zoneLabel),
					Region:        region,
					PricingTerm:   models.PricingTermOnDemand,
					PricePerUnit:  fdDTRate,
					Unit:          models.PriceUnitPerGB,
					Currency:      "USD",
					Attributes:    map[string]string{"cdn_zone": zoneLabel, "front_door_sku": sku, "source": "static_fallback"},
				},
				{
					Provider:      models.CloudProviderAzure,
					Service:       "azure_front_door",
					SKUID:         fmt.Sprintf("frontdoor-%s-standard-req", zoneSlug),
					ProductFamily: "Networking",
					Description:   fmt.Sprintf("Azure Front Door Standard — Requests (%s)", zoneLabel),
					Region:        region,
					PricingTerm:   models.PricingTermOnDemand,
					PricePerUnit:  fdReqRate,
					Unit:          models.PriceUnitPerUnit,
					Currency:      "USD",
					Attributes:    map[string]string{"cdn_zone": zoneLabel, "front_door_sku": sku, "unit_label": "per 10K requests", "source": "static_fallback"},
				},
			}
		}

		p.setCachedPrices(key, prices, p.cacheTTL)
		prices = annotateFresh(prices, sourceURL)
	}

	if (dataGB > 0 || monthlyRequestsMillions > 0) && len(prices) > 0 {
		var dtPrice, reqPrice *models.NormalizedPrice
		for i := range prices {
			if prices[i].Unit == models.PriceUnitPerGB && prices[i].PricePerUnit > 0 {
				p2 := prices[i]
				dtPrice = &p2
			} else if prices[i].Unit == models.PriceUnitPerUnit &&
				prices[i].Attributes["unit_label"] == "per 10K requests" {
				p2 := prices[i]
				reqPrice = &p2
			}
		}
		total := 0.0
		var breakdownParts []string
		if dtPrice != nil && dataGB > 0 {
			cost := dtPrice.PricePerUnit * dataGB
			total += cost
			breakdownParts = append(breakdownParts, fmt.Sprintf("%.1f GB × $%.4f/GB = $%.4f", dataGB, dtPrice.PricePerUnit, cost))
		}
		if reqPrice != nil && monthlyRequestsMillions > 0 {
			units10k := monthlyRequestsMillions * 1_000_000 / 10_000
			cost := reqPrice.PricePerUnit * units10k
			total += cost
			breakdownParts = append(breakdownParts, fmt.Sprintf("%.1fM requests × $%.4f/10K = $%.4f", monthlyRequestsMillions, reqPrice.PricePerUnit, cost))
		}
		if len(breakdownParts) > 0 {
			synthetic := models.NormalizedPrice{
				Provider:      models.CloudProviderAzure,
				Service:       "azure_front_door",
				SKUID:         "frontdoor-estimate",
				ProductFamily: "Networking",
				Description:   "Azure Front Door — estimated monthly cost",
				Region:        region,
				PricingTerm:   models.PricingTermOnDemand,
				PricePerUnit:  total,
				Unit:          models.PriceUnitPerUnit,
				Currency:      "USD",
				Attributes:    map[string]string{"breakdown": strings.Join(breakdownParts, "; "), "cdn_zone": zoneLabel},
			}
			ref := prices[0]
			synthetic.FetchedAt = ref.FetchedAt
			synthetic.SourceURL = ref.SourceURL
			synthetic.CacheAgeSeconds = ref.CacheAgeSeconds
			prices = append(prices, synthetic)
		}
	}
	return prices, nil
}

// --------------------------------------------------------------------------
// SearchPricing
// --------------------------------------------------------------------------

// SearchPricing performs a free-text search across the Azure pricing catalog.
func (p *Provider) SearchPricing(
	ctx context.Context,
	query string,
	region string,
	maxResults int,
) ([]models.NormalizedPrice, error) {
	filters := map[string]string{"priceType": "Consumption"}
	if region != "" {
		filters["armRegionName"] = region
	}
	items, err := p.fetchPrices(ctx, filters, maxResults*5)
	if err != nil {
		return nil, err
	}

	queryLower := strings.ToLower(query)
	targetRegion := region
	if targetRegion == "" {
		targetRegion = "eastus"
	}

	var results []models.NormalizedPrice
	for _, item := range items {
		if !strings.Contains(strings.ToLower(item.MeterName), queryLower) &&
			!strings.Contains(strings.ToLower(item.ProductName), queryLower) &&
			!strings.Contains(strings.ToLower(item.SkuName), queryLower) &&
			!strings.Contains(strings.ToLower(item.ArmSkuName), queryLower) {
			continue
		}
		itemRegion := item.ArmRegionName
		if itemRegion == "" {
			itemRegion = targetRegion
		}
		pp := itemToPrice(item, itemRegion, models.PricingTermOnDemand, "compute")
		if pp != nil {
			results = append(results, *pp)
		}
		if len(results) >= maxResults {
			break
		}
	}
	return results, nil
}

// --------------------------------------------------------------------------
// ListRegions / ListInstanceTypes / CheckAvailability
// --------------------------------------------------------------------------

// ListRegions returns all Azure regions where a service is available.
func (p *Provider) ListRegions(ctx context.Context, service string) ([]string, error) {
	return listAzureRegions(), nil
}

// ListInstanceTypes returns available Azure VM instance types.
func (p *Provider) ListInstanceTypes(
	ctx context.Context,
	region string,
	family string,
	minVCPUs int,
	minMemoryGB float64,
	gpu bool,
) ([]models.InstanceTypeInfo, error) {
	cacheMetaKey := fmt.Sprintf("azure:instance_types:%s:%s:%d:%g:%v", region, family, minVCPUs, minMemoryGB, gpu)
	if raw, ok := p.cache.GetMetadata(cacheMetaKey); ok {
		var instances []models.InstanceTypeInfo
		if err := json.Unmarshal(raw, &instances); err == nil {
			return instances, nil
		}
	}

	filters := map[string]string{
		"armRegionName": region,
		"priceType":     "Consumption",
		"serviceName":   "Virtual Machines",
	}
	items, err := p.fetchPrices(ctx, filters, 500)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	var instances []models.InstanceTypeInfo
	for _, item := range items {
		armSku := item.ArmSkuName
		if armSku == "" || seen[armSku] {
			continue
		}
		skuName := item.SkuName
		if strings.Contains(skuName, "Spot") || strings.Contains(skuName, "Low Priority") {
			continue
		}
		seen[armSku] = true

		if family != "" && !strings.HasPrefix(armSku, family) {
			continue
		}
		isGPU := strings.HasPrefix(armSku, "Standard_NC") ||
			strings.HasPrefix(armSku, "Standard_ND") ||
			strings.HasPrefix(armSku, "Standard_NV")
		if gpu && !isGPU {
			continue
		}
		if !gpu && isGPU {
			continue
		}
		gpuCount := 0
		if isGPU {
			gpuCount = 1
		}
		instances = append(instances, models.InstanceTypeInfo{
			Provider:     models.CloudProviderAzure,
			InstanceType: armSku,
			VCPU:         0,
			MemoryGB:     0,
			GPUCount:     gpuCount,
			Region:       region,
			Available:    true,
		})
	}

	raw, _ := json.Marshal(instances)
	p.cache.SetMetadata(cacheMetaKey, raw, p.metaTTL)
	return instances, nil
}

// CheckAvailability reports whether the given SKU/instance type is available in the region.
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
			return false, nil, nil
		}
		return len(prices) > 0, nil, nil
	case "storage":
		prices, err := p.GetStoragePrice(ctx, skuOrType, region, 0)
		if err != nil {
			return false, nil, nil
		}
		return len(prices) > 0, nil, nil
	}
	return false, nil, nil
}

// --------------------------------------------------------------------------
// GetPrice (unified entry point)
// --------------------------------------------------------------------------

// GetPrice is the primary entry point dispatching to domain-specific methods.
func (p *Provider) GetPrice(ctx context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
	if !p.Supports(spec.GetDomain(), spec.GetService()) {
		return nil, fmt.Errorf("azure: %w: domain=%s service=%s", providers.ErrNotSupported, spec.GetDomain(), spec.GetService())
	}

	var (
		publicPrices []models.NormalizedPrice
		breakdown    map[string]any
		err          error
	)

	switch s := spec.(type) {
	case *models.ComputePricingSpec:
		rt := s.ResourceType
		if rt == "" {
			rt = "Standard_D4s_v3"
		}
		os := s.OS
		if os == "" {
			os = "Linux"
		}
		publicPrices, err = p.GetComputePrice(ctx, rt, s.Region, os, s.Term)

	case *models.StoragePricingSpec:
		st := s.StorageType
		if st == "" {
			st = "premium-ssd"
		}
		sizeGB := 0.0
		if s.SizeGB != nil {
			sizeGB = *s.SizeGB
		}
		publicPrices, err = p.GetStoragePrice(ctx, st, s.Region, sizeGB)

	case *models.DatabasePricingSpec:
		svc := strings.ToLower(s.Service)
		// cosmos, cosmos_db, and cosmosdb all route to Cosmos DB pricing.
		if svc == "cosmos" || svc == "cosmos_db" || svc == "cosmosdb" {
			dep := strings.ToLower(s.Deployment)
			multi := dep == "multi-az" || dep == "ha" || dep == "regional" || dep == "multi-region"
			cosmosDep := dep
			if cosmosDep != "serverless" && cosmosDep != "autoscale" {
				cosmosDep = "provisioned"
			}
			publicPrices, err = p.GetCosmosPrice(ctx, s.Region, cosmosDep, multi)
		} else {
			rt := s.ResourceType
			if rt == "" {
				rt = "General Purpose 4 vCores"
			}
			publicPrices, err = p.GetSQLPrice(ctx, rt, s.Region, s.Engine, s.Deployment, s.Term)
		}

	case *models.ContainerPricingSpec:
		mode := s.Mode
		if mode == "" {
			mode = "standard"
		}
		publicPrices, err = p.GetAKSPrice(ctx, s.Region, mode)

	case *models.ServerlessPricingSpec:
		gbSec := 0.0
		if s.GBSeconds != nil {
			gbSec = *s.GBSeconds
		}
		reqMil := 0.0
		if s.RequestsMillions != nil {
			reqMil = *s.RequestsMillions
		}
		publicPrices, err = p.GetFunctionsPrice(ctx, s.Region, gbSec, reqMil)

	case *models.AiPricingSpec:
		model := s.Model
		if model == "" {
			model = "gpt-4o"
		}
		inTokens := 0
		if s.InputTokens != nil {
			inTokens = *s.InputTokens
		}
		outTokens := 0
		if s.OutputTokens != nil {
			outTokens = *s.OutputTokens
		}
		publicPrices, err = p.GetOpenAIPrice(ctx, model, s.Region, inTokens, outTokens)

	case *models.EgressPricingSpec:
		src := s.SourceRegion
		if src == "" {
			src = s.Region
		}
		if src == "" {
			src = "eastus"
		}
		publicPrices, err = p.GetEgressPrice(ctx, src, s.DestRegion, s.DataGB)

	case *models.ObservabilityPricingSpec:
		svc := strings.ToLower(s.Service)
		// azure_monitor, monitor, and log_analytics all route to Monitor pricing.
		if svc != "" && svc != "azure_monitor" && svc != "monitor" && svc != "log_analytics" {
			return nil, fmt.Errorf("azure: %w: observability service %q", providers.ErrNotSupported, s.Service)
		}
		publicPrices, err = p.GetMonitorPrice(ctx, s.Region, s.LogGB, s.IngestionMiB, s.MetricsCount)

	case *models.NetworkPricingSpec:
		svc := strings.ToLower(s.Service)
		switch svc {
		case "egress":
			src := s.SourceRegion
			if src == "" {
				src = s.Region
			}
			if src == "" {
				src = "eastus"
			}
			dataGBVal := s.DataGBPerMonth
			if dataGBVal == 0 {
				dataGBVal = s.EgressGB
			}
			publicPrices, breakdown, err = p.priceNetworkEgress(ctx, src, s.DestinationType, s.DestinationRegion, dataGBVal)
		case "azure_cdn", "cdn":
			dataGBVal := s.DataGB
			if dataGBVal == 0 {
				dataGBVal = s.EgressGB
			}
			publicPrices, err = p.GetCDNPrice(ctx, s.Region, dataGBVal, "standard")
		case "azure_front_door", "frontdoor", "front_door":
			dataGBVal := s.DataGB
			if dataGBVal == 0 {
				dataGBVal = s.EgressGB
			}
			publicPrices, err = p.GetFrontDoorPrice(ctx, s.Region, dataGBVal, s.MonthlyRequestsMillions, "standard")
		default:
			return nil, fmt.Errorf("azure: %w: network service %q", providers.ErrNotSupported, s.Service)
		}

	default:
		return nil, fmt.Errorf("azure: %w: unsupported spec type %T", providers.ErrNotSupported, spec)
	}

	if err != nil {
		return nil, err
	}
	return &models.PricingResult{
		PublicPrices:  publicPrices,
		AuthAvailable: false,
		Breakdown:     breakdown,
		Source:        "catalog",
		SchemaVersion: "1",
	}, nil
}

// priceNetworkEgress handles tiered internet/cross-region/cross-AZ egress.
func (p *Provider) priceNetworkEgress(
	ctx context.Context,
	src, destType, destRegion string,
	dataGB float64,
) ([]models.NormalizedPrice, map[string]any, error) {
	zone := azureEgressZone[strings.ToLower(src)]
	if zone == "" {
		zone = "zone1"
	}

	if strings.ToLower(destType) == "cross_az" {
		tiers := []egressTier{{0, azureCrossAZRate, "Cross-AZ within same region ($0.01/GB)"}}
		tierResult := computeTieredCost(tiers, dataGB)
		price := models.NormalizedPrice{
			Provider:      models.CloudProviderAzure,
			Service:       "egress",
			SKUID:         fmt.Sprintf("azure:cross_az:%s", src),
			ProductFamily: "Bandwidth",
			Description:   fmt.Sprintf("Azure cross-AZ traffic within %s", src),
			Region:        src,
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  azureCrossAZRate,
			Unit:          models.PriceUnitPerGB,
			Currency:      "USD",
			Attributes: map[string]string{
				"source_region":    src,
				"destination_type": "cross_az",
				"zone":             zone,
				"note":             "Flat $0.01/GB each direction between availability zones.",
			},
		}
		return annotateFresh([]models.NormalizedPrice{price}, azureEgressSourceURL), tierResult, nil
	}

	if strings.ToLower(destType) == "cross_region" {
		rate := azureCrossRegionRate[zone]
		if rate == 0 {
			rate = 0.02
		}
		tiers := []egressTier{{0, rate, fmt.Sprintf("Inter-region within Azure (%s, flat)", zone)}}
		tierResult := computeTieredCost(tiers, dataGB)
		desc := fmt.Sprintf("Azure inter-region data transfer from %s", src)
		if destRegion != "" {
			desc += " to " + destRegion
		}
		price := models.NormalizedPrice{
			Provider: models.CloudProviderAzure,
			Service:  "egress",
			SKUID: fmt.Sprintf("azure:inter_region:%s:%s", src, func() string {
				if destRegion != "" {
					return destRegion
				}
				return zone
			}()),
			ProductFamily: "Bandwidth",
			Description:   desc,
			Region:        src,
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  rate,
			Unit:          models.PriceUnitPerGB,
			Currency:      "USD",
			Attributes: map[string]string{
				"source_region":    src,
				"destination_type": "cross_region",
				"zone":             zone,
			},
		}
		if destRegion != "" {
			price.Attributes["dest_region"] = destRegion
		}
		return annotateFresh([]models.NormalizedPrice{price}, azureEgressSourceURL), tierResult, nil
	}

	// Default: internet egress with tiering.
	tiers, ok := azureInternetEgressTiers[zone]
	if !ok {
		tiers = azureInternetEgressTiers["zone1"]
	}

	// Try live fetch.
	zoneLabel := strings.ReplaceAll(zone, "zone", "zone ")
	liveItems, lerr := p.fetchPrices(ctx, map[string]string{"serviceName": "Bandwidth"}, 200)
	if lerr == nil {
		type tierRow struct {
			min  float64
			rate float64
		}
		var rows []tierRow
		for _, item := range liveItems {
			meter := strings.ToLower(item.MeterName)
			if strings.Contains(meter, "out") && strings.Contains(meter, zoneLabel) && item.RetailPrice >= 0 {
				rows = append(rows, tierRow{item.TierMinimumUnits, item.RetailPrice})
			}
		}
		if len(rows) > 0 {
			// sort by min
			for i := 1; i < len(rows); i++ {
				for j := i; j > 0 && rows[j].min < rows[j-1].min; j-- {
					rows[j], rows[j-1] = rows[j-1], rows[j]
				}
			}
			liveTiers := make([]egressTier, len(rows))
			for i, r := range rows {
				liveTiers[i] = egressTier{r.min, r.rate, fmt.Sprintf("From %.0f GB (live)", r.min)}
			}
			tiers = liveTiers
		}
	}

	tierResult := computeTieredCost(tiers, dataGB)
	blended := 0.0
	switch {
	case dataGB > 0:
		_, _ = fmt.Sscanf(tierResult["blended_rate_per_gb"].(string), "%f", &blended)
	case len(tiers) > 1:
		blended = tiers[1].rate
	case len(tiers) > 0:
		blended = tiers[0].rate
	}

	desc := fmt.Sprintf("Azure internet egress from %s (%s, tiered, first 5 GB free)", src, strings.ToUpper(zone))
	if dataGB > 0 {
		totalCost := tierResult["total_cost"].(string)
		desc += fmt.Sprintf(": %.0f GB/month — $%s ($%.4f/GB blended)", dataGB, totalCost, blended)
	}
	price := models.NormalizedPrice{
		Provider:      models.CloudProviderAzure,
		Service:       "egress",
		SKUID:         fmt.Sprintf("azure:internet_egress:%s:%s", src, zone),
		ProductFamily: "Bandwidth",
		Description:   desc,
		Region:        src,
		PricingTerm:   models.PricingTermOnDemand,
		PricePerUnit:  blended,
		Unit:          models.PriceUnitPerGBMonth,
		Currency:      "USD",
		Attributes: map[string]string{
			"source_region":    src,
			"destination_type": "internet",
			"zone":             zone,
			"free_tier_gb":     "5",
			"note":             fmt.Sprintf("First 5 GB/month free. %s rates.", strings.ToUpper(zone)),
		},
	}
	return annotateFresh([]models.NormalizedPrice{price}, azureEgressSourceURL), tierResult, nil
}

// --------------------------------------------------------------------------
// FinOps methods (not supported)
// --------------------------------------------------------------------------

// GetEffectivePrice returns ErrNotSupported (Azure billing APIs not implemented).
func (p *Provider) GetEffectivePrice(ctx context.Context, spec models.PricingSpec) ([]models.EffectivePrice, error) {
	return nil, providers.ErrNotSupported
}

// GetSpotHistory returns ErrNotSupported (Azure does not offer spot history API).
func (p *Provider) GetSpotHistory(
	ctx context.Context,
	spec models.PricingSpec,
	hours int,
	availabilityZone string,
) (map[string]any, error) {
	return nil, providers.ErrNotSupported
}

// GetDiscountSummary returns ErrNotSupported.
func (p *Provider) GetDiscountSummary(ctx context.Context) (map[string]any, error) {
	return nil, providers.ErrNotSupported
}

// BOMAdvisories returns provider-specific advisory rows.
func (p *Provider) BOMAdvisories(ctx context.Context, services []string, sampleRegion string) ([]map[string]string, error) {
	return nil, nil
}

// --------------------------------------------------------------------------
// DescribeCatalog
// --------------------------------------------------------------------------

// DescribeCatalog returns a structured catalog of what Azure supports.
func (p *Provider) DescribeCatalog(ctx context.Context) (*models.ProviderCatalog, error) {
	return &models.ProviderCatalog{
		Provider: models.CloudProviderAzure,
		Domains: []models.PricingDomain{
			models.PricingDomainCompute,
			models.PricingDomainStorage,
			models.PricingDomainDatabase,
			models.PricingDomainContainer,
			models.PricingDomainServerless,
			models.PricingDomainAI,
			models.PricingDomainNetwork,
			models.PricingDomainInterRegionEgress,
			models.PricingDomainObservability,
		},
		Services: map[string][]string{
			"compute":             {"vm"},
			"storage":             {"managed_disks", "blob"},
			"database":            {"sql", "cosmos"},
			"container":           {"aks"},
			"serverless":          {"azure_functions"},
			"ai":                  {"openai"},
			"inter_region_egress": {},
			"observability":       {"azure_monitor"},
			"network":             {"azure_cdn", "azure_front_door"},
		},
		SupportedTerms: map[string][]string{
			"compute/vm":                  {"on_demand", "spot", "reserved_1yr", "reserved_3yr"},
			"storage/managed_disks":       {"on_demand"},
			"storage/blob":                {"on_demand"},
			"database/sql":                {"on_demand", "reserved_1yr", "reserved_3yr"},
			"database/cosmos":             {"on_demand"},
			"container/aks":               {"on_demand"},
			"serverless/azure_functions":  {"on_demand"},
			"ai/openai":                   {"on_demand"},
			"network/egress":              {"on_demand"},
			"inter_region_egress":         {"on_demand"},
			"observability/azure_monitor": {"on_demand"},
			"network/azure_cdn":           {"on_demand"},
			"network/azure_front_door":    {"on_demand"},
		},
		FilterHints: map[string]map[string]any{
			"compute/vm": {
				"resource_type": "Azure VM size e.g. 'Standard_D4s_v3', 'Standard_E8s_v3'",
				"os":            "'Linux' (default) or 'Windows'",
				"term":          "on_demand | spot | reserved_1yr | reserved_3yr",
			},
			"storage/managed_disks": {
				"storage_type": "premium-ssd | standard-ssd | standard-hdd | ultra-ssd",
				"size_gb":      "Disk size for monthly estimate",
			},
			"database/sql": {
				"resource_type": "'General Purpose 4 vCores' | 'Business Critical 8 vCores' | 'General Purpose 8 vCores'",
				"engine":        "'SQL' (default, Azure SQL Database Gen5 vCore) | 'MySQL' | 'PostgreSQL'",
				"deployment":    "single-az (default) | ha",
				"term":          "on_demand | reserved_1yr | reserved_3yr",
				"note_sql":      "engine=SQL: resource_type must be '<Tier> <N> vCores' e.g. 'General Purpose 4 vCores'. Maps to armSkuName SQLDB_GP_Compute_Gen5_N / SQLDB_BC_Compute_Gen5_N.",
				"note_pg":       "engine=PostgreSQL: General Purpose maps to Standard_D<N>ds_v5 Flexible Server. Memory Optimized / Burstable tiers not supported via resource_type.",
			},
			"database/cosmos": {
				"deployment": "'provisioned' (per 100 RU/s, default) | 'serverless' | 'autoscale' | 'ha'",
			},
			"container/aks": {
				"mode": "'standard' ($0.10/hr, Uptime SLA) | 'free' (no SLA, control plane free)",
				"note": "Worker nodes billed separately",
			},
			"serverless/azure_functions": {
				"gb_seconds":        "Execution duration × memory in GB",
				"requests_millions": "Execution count in millions",
				"note":              "Free tier: 400K GB-s and 1M executions/month",
			},
			"ai/openai": {
				"model":         "gpt-4o | gpt-4o-mini | gpt-4 | gpt-35-turbo | o1 | o1-mini",
				"input_tokens":  "Input token count for cost estimate",
				"output_tokens": "Output token count for cost estimate",
			},
			"network/egress": {
				"source_region":      "Origin Azure ARM region",
				"destination_type":   "internet | cross_region | cross_az",
				"destination_region": "Target region for cross_region (optional)",
				"data_gb_per_month":  "Monthly data volume in GB for tiered cost estimate",
			},
			"observability/azure_monitor": {
				"log_gb":        "Analytics Logs ingestion volume in GB/month (after 5 GB free tier)",
				"ingestion_mib": "Custom metrics ingestion in millions of samples",
				"metrics_count": "Number of metric alert rules (first 10 free)",
			},
			"network/azure_cdn": {
				"data_gb": "Monthly outbound data transfer in GB for cost estimate",
			},
			"network/azure_front_door": {
				"data_gb":                   "Monthly data transfer out in GB",
				"monthly_requests_millions": "Monthly request count in millions",
			},
		},
		ExampleInvocations: map[string]map[string]any{
			"compute/vm": {
				"provider":      "azure",
				"domain":        "compute",
				"resource_type": "Standard_D4s_v3",
				"region":        "eastus",
				"os":            "Linux",
				"term":          "on_demand",
			},
			"database/sql": {
				"provider":      "azure",
				"domain":        "database",
				"service":       "sql",
				"resource_type": "General Purpose 4 vCores",
				"engine":        "SQL",
				"deployment":    "single-az",
				"region":        "eastus",
			},
			"database/cosmos": {
				"provider":   "azure",
				"domain":     "database",
				"service":    "cosmos",
				"deployment": "provisioned",
				"region":     "eastus",
			},
			"ai/openai": {
				"provider":      "azure",
				"domain":        "ai",
				"service":       "openai",
				"model":         "gpt-4o",
				"input_tokens":  1000000,
				"output_tokens": 500000,
				"region":        "eastus",
			},
		},
		DecisionMatrix: map[string]string{
			"Azure VMs":           "compute/vm",
			"Azure Managed Disks": "storage/managed_disks",
			"Azure Blob Storage":  "storage/blob",
			"Azure SQL Database":  "database/sql",
			"Azure Cosmos DB":     "database/cosmos",
			"AKS":                 "container/aks",
			"Azure Functions":     "serverless/azure_functions",
			"Azure OpenAI":        "ai/openai",
			"Azure CDN":           "network/azure_cdn",
			"Azure Front Door":    "network/azure_front_door",
			"Azure Monitor":       "observability/azure_monitor",
			"Azure Bandwidth":     "inter_region_egress",
		},
	}, nil
}
