// Package aws — network/egress, serverless, and ElastiCache pricing methods.
//
// This file ports the Python _price_network_egress, _price_lambda, and
// _price_database/elasticache methods from aws.py.
package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// --------------------------------------------------------------------------
// Egress continent routing — mirrors _region_continent and _EGRESS_RATES
// --------------------------------------------------------------------------

func regionContinent(region string) string {
	r := strings.ToLower(region)
	switch {
	case strings.HasPrefix(r, "us-") || strings.HasPrefix(r, "ca-") || strings.HasPrefix(r, "mx-"):
		return "us"
	case strings.HasPrefix(r, "eu-"):
		return "eu"
	case strings.HasPrefix(r, "ap-") || strings.HasPrefix(r, "cn-"):
		return "ap"
	case strings.HasPrefix(r, "sa-"):
		return "sa"
	case strings.HasPrefix(r, "me-") || strings.HasPrefix(r, "il-"):
		return "me"
	case strings.HasPrefix(r, "af-"):
		return "af"
	default:
		return "us"
	}
}

// egressRates mirrors Python _EGRESS_RATES — published AWS inter-region rates.
// Key is "src_continent:dst_continent".
var egressRates = map[string]float64{
	"us:us": 0.02,
	"eu:eu": 0.02,
	"ap:ap": 0.09,
	"sa:sa": 0.16,
	"me:me": 0.16,
	"af:af": 0.16,
	"us:eu": 0.02, "eu:us": 0.02,
	"us:ap": 0.09, "ap:us": 0.09,
	"us:sa": 0.16, "sa:us": 0.16,
	"us:me": 0.16, "me:us": 0.16,
	"us:af": 0.16, "af:us": 0.16,
	"eu:ap": 0.09, "ap:eu": 0.09,
	"eu:sa": 0.16, "sa:eu": 0.16,
	"eu:me": 0.16, "me:eu": 0.16,
	"eu:af": 0.16, "af:eu": 0.16,
	"ap:sa": 0.16, "sa:ap": 0.16,
	"ap:me": 0.16, "me:ap": 0.16,
	"ap:af": 0.16, "af:ap": 0.16,
	"sa:me": 0.16, "me:sa": 0.16,
	"sa:af": 0.16, "af:sa": 0.16,
	"me:af": 0.16, "af:me": 0.16,
}

func interRegionRate(src, dst string) float64 {
	srcC := regionContinent(src)
	dstC := regionContinent(dst)
	key := srcC + ":" + dstC
	if r, ok := egressRates[key]; ok {
		return r
	}
	return 0.16 // conservative default
}

// sourceURL is the canonical AWS pricing index URL.
const sourceURL = "https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/index.json"

// --------------------------------------------------------------------------
// GetNetworkPrice — AWSDataTransfer inter-region egress
// --------------------------------------------------------------------------

// GetNetworkPrice prices network/egress or inter_region_egress specs.
// It queries AWSDataTransfer with transferType="InterRegion Outbound", optionally
// filtered by fromRegionCode (src). Falls back to the static rate table when the
// Pricing API returns no matching SKUs.
//
// Mirrors Python _price_egress.
func (p *Provider) GetNetworkPrice(
	ctx context.Context,
	spec models.PricingSpec,
	region string,
) ([]models.NormalizedPrice, error) {
	// Internet egress — tiered pricing distinct from inter-region transfer.
	if ns, ok := spec.(*models.NetworkPricingSpec); ok && strings.EqualFold(ns.DestinationType, "internet") {
		return internetEgressPrices(region, ns.DataGBPerMonth, time.Now()), nil
	}

	// Resolve src/dst from either EgressPricingSpec or NetworkPricingSpec.
	var src, dst string
	switch s := spec.(type) {
	case *models.EgressPricingSpec:
		src = s.SourceRegion
		if src == "" {
			src = region
		}
		dst = s.DestRegion
	case *models.NetworkPricingSpec:
		src = s.SourceRegion
		if src == "" {
			src = region
		}
		dst = s.DestinationRegion
	default:
		// For any other spec type, just use the region as source.
		src = region
	}
	if src == "" {
		src = "us-east-1"
	}

	// When no destination region is specified, the caller expects internet egress
	// tiered pricing — not inter-region transfer. Route to internetEgressPrices so
	// the server behavior matches the documented promise.
	if dst == "" {
		dataGB := 0.0
		if es, ok := spec.(*models.EgressPricingSpec); ok {
			dataGB = es.DataGB
		}
		return internetEgressPrices(src, dataGB, time.Now()), nil
	}

	cacheKey := fmt.Sprintf("aws:network_egress:%s:%s", src, dst)
	if cached, ok := p.cache.Get(cacheKey); ok {
		var prices []models.NormalizedPrice
		if err := json.Unmarshal(cached, &prices); err == nil {
			return prices, nil
		}
	}

	filters := []pricingtypes.Filter{
		mkFilter("transferType", "InterRegion Outbound"),
	}
	if src != "" {
		filters = append(filters, mkFilter("fromRegionCode", src))
	}

	var rawItems []string
	if p.pricingClient != nil || p.bulkFallback {
		var apiErr error
		rawItems, apiErr = p.GetProducts(ctx, "AWSDataTransfer", filters, 30)
		if apiErr != nil {
			// Don't fail hard — fall back to static rates.
			rawItems = nil
		}
	}

	var prices []models.NormalizedPrice
	now := time.Now()
	for _, raw := range rawItems {
		var sku parsedSKU
		if err2 := json.Unmarshal([]byte(raw), &sku); err2 != nil {
			continue
		}
		attrs := sku.Product.Attributes
		toRegion := attrs["toRegionCode"]
		// If a specific destination was requested, filter to exact match.
		if dst != "" && toRegion != "" && toRegion != dst {
			continue
		}
		price, unitStr := extractOnDemandPrice(sku)
		if price == 0 {
			continue
		}
		np := &models.NormalizedPrice{
			Provider:      models.CloudProviderAWS,
			Service:       "inter_region_egress",
			SKUID:         sku.Product.SKU,
			ProductFamily: sku.Product.ProductFamily,
			Description:   fmt.Sprintf("AWS inter-region data transfer %s → %s", src, toRegion),
			Region:        src,
			Attributes: map[string]string{
				"fromRegionCode": attrs["fromRegionCode"],
				"toRegionCode":   toRegion,
				"transferType":   attrs["transferType"],
			},
			PricingTerm:  models.PricingTermOnDemand,
			PricePerUnit: price,
			Unit:         parseUnit(unitStr),
			Currency:     "USD",
			FetchedAt:    &now,
			SourceURL:    sourceURL,
		}
		// Override unit to per_gb (data transfer is billed per GB)
		if np.Unit == models.PriceUnitPerUnit {
			np.Unit = models.PriceUnitPerGB
		}
		prices = append(prices, *np)
		if len(prices) >= 10 {
			break
		}
	}

	// Static fallback when the Pricing API returned nothing.
	if len(prices) == 0 {
		prices = egressStaticFallback(src, dst, now)
	}

	// When the caller specified a volume, append a computed monthly total so the
	// model can read the answer directly without multiplying per-unit rate × volume.
	if es, ok := spec.(*models.EgressPricingSpec); ok && es.DataGB > 0 && len(prices) > 0 {
		rate := prices[0].PricePerUnit
		monthly := rate * es.DataGB
		prices = append(prices, models.NormalizedPrice{
			Provider:      models.CloudProviderAWS,
			Service:       "inter_region_egress",
			SKUID:         fmt.Sprintf("aws:data_transfer:%s:%s:monthly_total", src, dst),
			ProductFamily: "Data Transfer",
			Description: fmt.Sprintf(
				"AWS inter-region %s → %s monthly total: %.0f GB × $%.6f/GB = $%.2f/month",
				src, dst, es.DataGB, rate, monthly,
			),
			Region:       src,
			PricingTerm:  models.PricingTermOnDemand,
			PricePerUnit: monthly,
			Unit:         models.PriceUnitPerMonth,
			Currency:     "USD",
			FetchedAt:    &now,
			Attributes: map[string]string{
				"fromRegionCode":    src,
				"toRegionCode":      dst,
				"data_gb_per_month": fmt.Sprintf("%.0f", es.DataGB),
				"monthly_total_usd": fmt.Sprintf("%.2f", monthly),
			},
		})
	}

	if len(prices) > 0 {
		if data, err2 := json.Marshal(prices); err2 == nil {
			ttl := time.Duration(p.cfg.CacheTTLHours) * time.Hour
			p.cache.Set(cacheKey, data, ttl)
		}
	}
	return prices, nil
}

// egressStaticFallback returns static inter-region egress prices when the
// Pricing API returns empty. Mirrors Python _egress_static_fallback.
func egressStaticFallback(src, dst string, now time.Time) []models.NormalizedPrice {
	note := "static published rate; Pricing API returned no match for this route"
	srcURL := "https://aws.amazon.com/ec2/pricing/on-demand/#Data_Transfer"

	if dst != "" {
		rate := interRegionRate(src, dst)
		return []models.NormalizedPrice{{
			Provider:      models.CloudProviderAWS,
			Service:       "inter_region_egress",
			SKUID:         fmt.Sprintf("aws:data_transfer:%s:%s:fallback", src, dst),
			ProductFamily: "Data Transfer",
			Description:   fmt.Sprintf("AWS inter-region data transfer %s → %s (%s)", src, dst, note),
			Region:        src,
			Attributes: map[string]string{
				"fromRegionCode": src,
				"toRegionCode":   dst,
				"transferType":   "InterRegion Outbound",
				"fallback":       "true",
			},
			PricingTerm:  models.PricingTermOnDemand,
			PricePerUnit: rate,
			Unit:         models.PriceUnitPerGB,
			Currency:     "USD",
			FetchedAt:    &now,
			SourceURL:    srcURL,
		}}
	}

	// No dest — return one entry per destination continent from src.
	srcC := regionContinent(src)
	seen := map[string]bool{}
	var results []models.NormalizedPrice
	for key, rate := range egressRates {
		parts := strings.SplitN(key, ":", 2)
		if len(parts) != 2 {
			continue
		}
		sc, dc := parts[0], parts[1]
		if sc != srcC || seen[dc] {
			continue
		}
		seen[dc] = true
		results = append(results, models.NormalizedPrice{
			Provider:      models.CloudProviderAWS,
			Service:       "inter_region_egress",
			SKUID:         fmt.Sprintf("aws:data_transfer:%s:%s-star:fallback", src, dc),
			ProductFamily: "Data Transfer",
			Description:   fmt.Sprintf("AWS inter-region data transfer %s → %s regions (%s)", src, dc, note),
			Region:        src,
			Attributes: map[string]string{
				"fromRegionCode": src,
				"toRegionCode":   dc + "-*",
				"transferType":   "InterRegion Outbound",
				"fallback":       "true",
			},
			PricingTerm:  models.PricingTermOnDemand,
			PricePerUnit: rate,
			Unit:         models.PriceUnitPerGB,
			Currency:     "USD",
			FetchedAt:    &now,
			SourceURL:    srcURL,
		})
	}
	return results
}

// --------------------------------------------------------------------------
// GetLambdaPrice — AWSLambda serverless pricing
// --------------------------------------------------------------------------

// GetLambdaPrice returns Lambda request and duration prices for the given region.
// It queries the AWSLambda service, extracting the standard x86 request rate
// (group="AWS-Lambda-Requests") and duration rate (group="AWS-Lambda-Duration",
// usagetype="Lambda-GB-Second"). Mirrors Python _price_lambda.
func (p *Provider) GetLambdaPrice(
	ctx context.Context,
	region string,
) ([]models.NormalizedPrice, error) {
	cacheKey := fmt.Sprintf("aws:lambda:%s", region)
	if cached, ok := p.cache.Get(cacheKey); ok {
		var prices []models.NormalizedPrice
		if err := json.Unmarshal(cached, &prices); err == nil {
			return prices, nil
		}
	}

	location, err := regionToLocation(region)
	if err != nil {
		return nil, err
	}

	filters := []pricingtypes.Filter{
		mkFilter("location", location),
	}

	rawItems, err := p.GetProducts(ctx, "AWSLambda", filters, 500)
	if err != nil {
		return nil, fmt.Errorf("aws: GetLambdaPrice: %w", err)
	}

	var requestPrice, durationPrice float64
	now := time.Now()

	for _, raw := range rawItems {
		var sku parsedSKU
		if err2 := json.Unmarshal([]byte(raw), &sku); err2 != nil {
			continue
		}
		attrs := sku.Product.Attributes
		group := attrs["group"]
		usagetype := attrs["usagetype"]

		priceVal, _ := extractOnDemandPrice(sku)
		if priceVal == 0 {
			continue
		}

		// Standard x86 request rate (not ARM, not Edge, not managed instances).
		if group == "AWS-Lambda-Requests" &&
			!strings.Contains(usagetype, "ARM") &&
			!strings.Contains(usagetype, "Edge") &&
			requestPrice == 0 {
			requestPrice = priceVal
		}
		// Standard x86 duration rate (first tier at startUsageAmount=0).
		if group == "AWS-Lambda-Duration" &&
			usagetype == "Lambda-GB-Second" &&
			durationPrice == 0 {
			durationPrice = priceVal
		}
	}

	var prices []models.NormalizedPrice

	if requestPrice > 0 {
		perMillion := requestPrice * 1_000_000
		prices = append(prices, models.NormalizedPrice{
			Provider:      models.CloudProviderAWS,
			Service:       "serverless",
			SKUID:         fmt.Sprintf("aws:lambda:%s:requests", region),
			ProductFamily: "AWS Lambda",
			Description:   fmt.Sprintf("Lambda requests — $%.2f per 1M requests", perMillion),
			Region:        region,
			Attributes: map[string]string{
				"billing_dimension":    "requests",
				"per_million_requests": fmt.Sprintf("$%.4f", perMillion),
			},
			PricingTerm:  models.PricingTermOnDemand,
			PricePerUnit: requestPrice,
			Unit:         models.PriceUnitPerRequest,
			Currency:     "USD",
			FetchedAt:    &now,
			SourceURL:    sourceURL,
		})
	}

	if durationPrice > 0 {
		prices = append(prices, models.NormalizedPrice{
			Provider:      models.CloudProviderAWS,
			Service:       "serverless",
			SKUID:         fmt.Sprintf("aws:lambda:%s:duration", region),
			ProductFamily: "AWS Lambda",
			Description:   "Lambda duration (per GB-second)",
			Region:        region,
			Attributes: map[string]string{
				"billing_dimension": "gb_second",
			},
			PricingTerm:  models.PricingTermOnDemand,
			PricePerUnit: durationPrice,
			Unit:         models.PriceUnitPerGBSecond,
			Currency:     "USD",
			FetchedAt:    &now,
			SourceURL:    sourceURL,
		})
	}

	if len(prices) > 0 {
		if data, err2 := json.Marshal(prices); err2 == nil {
			ttl := time.Duration(p.cfg.CacheTTLHours) * time.Hour
			p.cache.Set(cacheKey, data, ttl)
		}
	}
	return prices, nil
}

// --------------------------------------------------------------------------
// GetElastiCachePrice — AmazonElastiCache pricing
// --------------------------------------------------------------------------

// GetElastiCachePrice returns pricing for an ElastiCache node type.
// It queries AmazonElastiCache filtered by instanceType (the cache node type,
// e.g. "cache.r6g.large"). Mirrors Python _price_database/elasticache.
func (p *Provider) GetElastiCachePrice(
	ctx context.Context,
	nodeType string,
	region string,
) ([]models.NormalizedPrice, error) {
	location, err := regionToLocation(region)
	if err != nil {
		return nil, err
	}

	cacheKey := fmt.Sprintf("aws:elasticache:%s:%s", region, nodeType)
	if cached, ok := p.cache.Get(cacheKey); ok {
		var prices []models.NormalizedPrice
		if err2 := json.Unmarshal(cached, &prices); err2 == nil {
			return prices, nil
		}
	}

	filters := []pricingtypes.Filter{
		mkFilter("location", location),
	}
	if nodeType != "" {
		filters = append(filters, mkFilter("instanceType", nodeType))
	}

	rawItems, apiErr := p.GetProducts(ctx, "AmazonElastiCache", filters, 10)
	if apiErr != nil {
		// Don't fail hard — fall back to static rates if the API is unavailable.
		rawItems = nil
	}

	var prices []models.NormalizedPrice
	for _, raw := range rawItems {
		np := skuToNormalizedPrice(raw, region, models.PricingTermOnDemand, "database")
		if np != nil {
			prices = append(prices, *np)
		}
	}

	if len(prices) > 0 {
		if data, err2 := json.Marshal(prices); err2 == nil {
			ttl := time.Duration(p.cfg.CacheTTLHours) * time.Hour
			p.cache.Set(cacheKey, data, ttl)
		}
	}

	// Static fallback for when the Pricing API is unavailable.
	// Covers current-gen node types from us-east-1 published pricing.
	if len(prices) == 0 {
		prices = elasticacheStaticFallback(nodeType, region, time.Now())
	}

	return prices, nil
}

// --------------------------------------------------------------------------
// GetNATPrice — AWS NAT Gateway pricing
// --------------------------------------------------------------------------

// GetNATPrice returns AWS NAT Gateway pricing for the given region.
// Rates: $0.045/hr per NAT Gateway + $0.045/GB data processed (us-east-1).
// Other regions have slightly higher rates; we use us-east-1 as the baseline.
// Source: https://aws.amazon.com/vpc/pricing/
func (p *Provider) GetNATPrice(ctx context.Context, region string) ([]models.NormalizedPrice, error) {
	cacheKey := fmt.Sprintf("aws:nat:%s", region)
	if cached, ok := p.cache.Get(cacheKey); ok {
		var prices []models.NormalizedPrice
		if err := json.Unmarshal(cached, &prices); err == nil {
			return prices, nil
		}
	}
	now := time.Now()
	srcURL := "https://aws.amazon.com/vpc/pricing/"
	// Published NAT Gateway rates — hourly and per-GB data processing.
	// These are identical across major regions; slight regional variation is omitted.
	prices := []models.NormalizedPrice{
		{
			Provider: models.CloudProviderAWS, Service: "nat",
			SKUID:         fmt.Sprintf("aws:nat:%s:hourly", region),
			ProductFamily: "NAT Gateway",
			Description:   "NAT Gateway hourly charge",
			Region:        region,
			Attributes:    map[string]string{"billing_dimension": "hourly"},
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  0.045, Unit: models.PriceUnitPerHour,
			Currency: "USD", FetchedAt: &now, SourceURL: srcURL,
		},
		{
			Provider: models.CloudProviderAWS, Service: "nat",
			SKUID:         fmt.Sprintf("aws:nat:%s:data_processing", region),
			ProductFamily: "NAT Gateway",
			Description:   "NAT Gateway data processing (per GB)",
			Region:        region,
			Attributes:    map[string]string{"billing_dimension": "data_processing_gb"},
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  0.045, Unit: models.PriceUnitPerGB,
			Currency: "USD", FetchedAt: &now, SourceURL: srcURL,
		},
	}
	if data, err := json.Marshal(prices); err == nil {
		ttl := time.Duration(p.cfg.CacheTTLHours) * time.Hour
		p.cache.Set(cacheKey, data, ttl)
	}
	return prices, nil
}

// --------------------------------------------------------------------------
// GetALBPrice — AWS Application Load Balancer pricing
// --------------------------------------------------------------------------

// GetALBPrice returns AWS Application Load Balancer pricing: an hourly base
// charge plus an LCU-hour charge, both of which vary by region. It queries
// the AWSELB service (via the live AWS Pricing API, or its public bulk-
// catalog mirror when no credentials are configured — see GetProducts),
// filtered to productFamily="Load Balancer-Application" and
// operation="LoadBalancing:Application", the same servicecode/attribute
// combination get_price_by_sku resolves ELB LCU SKUs against (e.g.
// "CAN1-LCUUsage" -> $0.0088 in ca-central-1 vs $0.0080 in eu-west-1).
// Falls back to the last known static us-east-1 published rates — tagged
// Attributes["fallback"]="true" — only if the live/bulk fetch yields no
// usable price for the region.
// Fallback source: https://aws.amazon.com/elasticloadbalancing/pricing/
func (p *Provider) GetALBPrice(ctx context.Context, region string) ([]models.NormalizedPrice, error) {
	cacheKey := fmt.Sprintf("aws:alb:%s", region)
	if cached, ok := p.cache.Get(cacheKey); ok {
		var prices []models.NormalizedPrice
		if err := json.Unmarshal(cached, &prices); err == nil {
			return prices, nil
		}
	}

	now := time.Now()
	location, err := regionToLocation(region)
	if err != nil {
		return nil, err
	}

	filters := []pricingtypes.Filter{
		mkFilter("location", location),
		mkFilter("productFamily", "Load Balancer-Application"),
		mkFilter("operation", "LoadBalancing:Application"),
		mkFilter("locationType", "AWS Region"),
	}

	var hourlyPrice, lcuPrice float64
	rawItems, apiErr := p.GetProducts(ctx, "AWSELB", filters, 20)
	if apiErr == nil {
		for _, raw := range rawItems {
			var sku parsedSKU
			if err2 := json.Unmarshal([]byte(raw), &sku); err2 != nil {
				continue
			}
			usagetype := sku.Product.Attributes["usagetype"]
			// productFamily+operation alone still admits Reserved-capacity and
			// Trust Store (mutual TLS) rows, which bill a different dimension
			// than the standard on-demand hourly/LCU charges; Outposts rows
			// are already excluded by the locationType filter above.
			if strings.Contains(usagetype, "Reserved") || strings.Contains(usagetype, "TS-") {
				continue
			}
			priceVal, _ := extractOnDemandPrice(sku)
			if priceVal == 0 {
				continue
			}
			switch {
			case hourlyPrice == 0 && strings.HasSuffix(usagetype, "LoadBalancerUsage"):
				hourlyPrice = priceVal
			case lcuPrice == 0 && strings.HasSuffix(usagetype, "LCUUsage"):
				lcuPrice = priceVal
			}
		}
	}

	var prices []models.NormalizedPrice
	if hourlyPrice > 0 {
		prices = append(prices, models.NormalizedPrice{
			Provider: models.CloudProviderAWS, Service: "lb",
			SKUID:         fmt.Sprintf("aws:alb:%s:hourly", region),
			ProductFamily: "Load Balancer-Application", Description: "Application Load Balancer hourly charge",
			Region:       region,
			Attributes:   map[string]string{"lb_type": "application", "billing_dimension": "hourly"},
			PricingTerm:  models.PricingTermOnDemand,
			PricePerUnit: hourlyPrice, Unit: models.PriceUnitPerHour,
			Currency: "USD", FetchedAt: &now, SourceURL: sourceURL,
		})
	}
	if lcuPrice > 0 {
		prices = append(prices, models.NormalizedPrice{
			Provider: models.CloudProviderAWS, Service: "lb",
			SKUID:         fmt.Sprintf("aws:alb:%s:lcu", region),
			ProductFamily: "Load Balancer-Application", Description: "Application Load Balancer LCU-hour",
			Region:       region,
			Attributes:   map[string]string{"lb_type": "application", "billing_dimension": "lcu_hour"},
			PricingTerm:  models.PricingTermOnDemand,
			PricePerUnit: lcuPrice, Unit: models.PriceUnitPerHour,
			Currency: "USD", FetchedAt: &now, SourceURL: sourceURL,
		})
	}

	if len(prices) > 0 {
		// Only cache real, region-specific prices — never the static
		// fallback below, so a transient fetch failure doesn't pin callers
		// to stale us-east-1 rates for the full cache TTL (mirrors
		// GetElastiCachePrice's fallback-is-never-cached behavior).
		if data, err := json.Marshal(prices); err == nil {
			ttl := time.Duration(p.cfg.CacheTTLHours) * time.Hour
			p.cache.Set(cacheKey, data, ttl)
		}
		return prices, nil
	}

	// Live/bulk fetch failed or returned nothing usable for this region —
	// fall back to the last known published on-demand rates (us-east-1),
	// explicitly flagged so callers know these are not region-specific.
	srcURL := "https://aws.amazon.com/elasticloadbalancing/pricing/"
	return []models.NormalizedPrice{
		{
			Provider: models.CloudProviderAWS, Service: "lb",
			SKUID:         fmt.Sprintf("aws:alb:%s:hourly:fallback", region),
			ProductFamily: "Load Balancer-Application", Description: "Application Load Balancer hourly charge (static fallback)",
			Region: region,
			Attributes: map[string]string{
				"lb_type": "application", "billing_dimension": "hourly",
				"fallback": "true", "note": "static us-east-1 published rate; may vary by region",
			},
			PricingTerm:  models.PricingTermOnDemand,
			PricePerUnit: 0.0225, Unit: models.PriceUnitPerHour,
			Currency: "USD", FetchedAt: &now, SourceURL: srcURL,
		},
		{
			Provider: models.CloudProviderAWS, Service: "lb",
			SKUID:         fmt.Sprintf("aws:alb:%s:lcu:fallback", region),
			ProductFamily: "Load Balancer-Application", Description: "Application Load Balancer LCU-hour (static fallback)",
			Region: region,
			Attributes: map[string]string{
				"lb_type": "application", "billing_dimension": "lcu_hour",
				"fallback": "true", "note": "static us-east-1 published rate; may vary by region",
			},
			PricingTerm:  models.PricingTermOnDemand,
			PricePerUnit: 0.008, Unit: models.PriceUnitPerHour,
			Currency: "USD", FetchedAt: &now, SourceURL: srcURL,
		},
	}, nil
}

// --------------------------------------------------------------------------
// GetCloudWatchPrice — AWS CloudWatch observability pricing
// --------------------------------------------------------------------------

// GetCloudWatchPrice returns AWS CloudWatch pricing (metrics, logs, dashboards).
// Source: https://aws.amazon.com/cloudwatch/pricing/
func (p *Provider) GetCloudWatchPrice(ctx context.Context, region string) ([]models.NormalizedPrice, error) {
	cacheKey := fmt.Sprintf("aws:cloudwatch:%s", region)
	if cached, ok := p.cache.Get(cacheKey); ok {
		var prices []models.NormalizedPrice
		if err := json.Unmarshal(cached, &prices); err == nil {
			return prices, nil
		}
	}
	now := time.Now()
	srcURL := "https://aws.amazon.com/cloudwatch/pricing/"
	prices := []models.NormalizedPrice{
		{
			Provider: models.CloudProviderAWS, Service: "cloudwatch",
			SKUID:         fmt.Sprintf("aws:cloudwatch:%s:metrics", region),
			ProductFamily: "CloudWatch Metrics",
			Description:   "CloudWatch custom metrics (per metric/month, first 10k after free tier)",
			Region:        region,
			Attributes:    map[string]string{"billing_dimension": "custom_metrics"},
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  0.30, Unit: models.PriceUnitPerMonth,
			Currency: "USD", FetchedAt: &now, SourceURL: srcURL,
		},
		{
			Provider: models.CloudProviderAWS, Service: "cloudwatch",
			SKUID:         fmt.Sprintf("aws:cloudwatch:%s:logs_ingestion", region),
			ProductFamily: "CloudWatch Logs",
			Description:   "CloudWatch Logs data ingestion (per GB)",
			Region:        region,
			Attributes:    map[string]string{"billing_dimension": "logs_ingestion_gb"},
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  0.50, Unit: models.PriceUnitPerGB,
			Currency: "USD", FetchedAt: &now, SourceURL: srcURL,
		},
		{
			Provider: models.CloudProviderAWS, Service: "cloudwatch",
			SKUID:         fmt.Sprintf("aws:cloudwatch:%s:logs_storage", region),
			ProductFamily: "CloudWatch Logs",
			Description:   "CloudWatch Logs storage (per GB/month)",
			Region:        region,
			Attributes:    map[string]string{"billing_dimension": "logs_storage_gb"},
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  0.03, Unit: models.PriceUnitPerGB,
			Currency: "USD", FetchedAt: &now, SourceURL: srcURL,
		},
	}
	if data, err := json.Marshal(prices); err == nil {
		ttl := time.Duration(p.cfg.CacheTTLHours) * time.Hour
		p.cache.Set(cacheKey, data, ttl)
	}
	return prices, nil
}

// --------------------------------------------------------------------------
// Internet egress tiered pricing
// --------------------------------------------------------------------------

// internetEgressTiers mirrors AWS internet egress tiered pricing.
// Source: https://aws.amazon.com/ec2/pricing/on-demand/#Data_Transfer
var internetEgressTiers = []struct {
	upToGB    float64
	ratePerGB float64
}{
	{10_000, 0.09},
	{50_000, 0.085},
	{150_000, 0.07},
	{0, 0.05}, // 0 = unbounded
}

func internetEgressPrices(region string, dataGBPerMonth float64, now time.Time) []models.NormalizedPrice {
	srcURL := "https://aws.amazon.com/ec2/pricing/on-demand/#Data_Transfer"
	var prices []models.NormalizedPrice
	for _, tier := range internetEgressTiers {
		label := fmt.Sprintf("%.0f TB/month", tier.upToGB/1000)
		if tier.upToGB == 0 {
			label = "150+ TB/month"
		}
		prices = append(prices, models.NormalizedPrice{
			Provider: models.CloudProviderAWS, Service: "egress",
			SKUID:         fmt.Sprintf("aws:internet_egress:%s:tier_%.0f", region, tier.ratePerGB*1000),
			ProductFamily: "Data Transfer", Description: fmt.Sprintf("AWS internet egress — %s ($%.4f/GB)", label, tier.ratePerGB),
			Region: region,
			Attributes: map[string]string{
				"destination_type": "internet",
				"tier_up_to_gb":    fmt.Sprintf("%.0f", tier.upToGB),
			},
			PricingTerm:  models.PricingTermOnDemand,
			PricePerUnit: tier.ratePerGB, Unit: models.PriceUnitPerGB,
			Currency: "USD", FetchedAt: &now, SourceURL: srcURL,
		})
	}
	// Also compute blended rate if dataGBPerMonth > 0
	if dataGBPerMonth > 0 {
		total := 0.0
		remaining := dataGBPerMonth
		prevUpToGB := 0.0
		for _, tier := range internetEgressTiers {
			var capacity float64
			if tier.upToGB == 0 {
				// Unbounded tier: consume all remaining GB.
				capacity = remaining
			} else {
				// upToGB is a cumulative ceiling; per-tier capacity is the delta.
				capacity = tier.upToGB - prevUpToGB
				prevUpToGB = tier.upToGB
			}
			chunk := capacity
			if chunk > remaining {
				chunk = remaining
			}
			total += chunk * tier.ratePerGB
			remaining -= chunk
			if remaining <= 0 {
				break
			}
		}
		blended := total / dataGBPerMonth
		now2 := now
		prices = append(prices, models.NormalizedPrice{
			Provider: models.CloudProviderAWS, Service: "egress",
			SKUID:         fmt.Sprintf("aws:internet_egress:%s:blended_%.0f", region, dataGBPerMonth),
			ProductFamily: "Data Transfer",
			Description:   fmt.Sprintf("AWS internet egress blended rate for %.0f GB/month ($%.4f/GB avg, $%.2f/month total)", dataGBPerMonth, blended, total),
			Region:        region,
			Attributes: map[string]string{
				"destination_type":  "internet",
				"data_gb_per_month": fmt.Sprintf("%.0f", dataGBPerMonth),
				"monthly_total_usd": fmt.Sprintf("%.2f", total),
			},
			PricingTerm:  models.PricingTermOnDemand,
			PricePerUnit: blended, Unit: models.PriceUnitPerGB,
			Currency: "USD", FetchedAt: &now2, SourceURL: srcURL,
		})
	}
	return prices
}

// --------------------------------------------------------------------------
// ElastiCache static fallback
// --------------------------------------------------------------------------

// elasticacheHourlyRates covers common current-gen node types (us-east-1 published rates).
// Source: https://aws.amazon.com/elasticache/pricing/
var elasticacheHourlyRates = map[string]float64{
	// r7g — Graviton3, Redis/Memcached
	"cache.r7g.large":   0.166,
	"cache.r7g.xlarge":  0.333,
	"cache.r7g.2xlarge": 0.665,
	"cache.r7g.4xlarge": 1.330,
	"cache.r7g.8xlarge": 2.660,
	// r6g — Graviton2
	"cache.r6g.large":   0.166,
	"cache.r6g.xlarge":  0.333,
	"cache.r6g.2xlarge": 0.665,
	"cache.r6g.4xlarge": 1.330,
	"cache.r6g.8xlarge": 2.660,
	// m7g — general purpose Graviton3
	"cache.m7g.large":   0.101,
	"cache.m7g.xlarge":  0.202,
	"cache.m7g.2xlarge": 0.404,
	// t4g — burstable Graviton2
	"cache.t4g.micro":  0.016,
	"cache.t4g.small":  0.032,
	"cache.t4g.medium": 0.065,
}

func elasticacheStaticFallback(nodeType, region string, now time.Time) []models.NormalizedPrice {
	rate, ok := elasticacheHourlyRates[nodeType]
	if !ok {
		return nil
	}
	return []models.NormalizedPrice{{
		Provider: models.CloudProviderAWS, Service: "database",
		SKUID:         fmt.Sprintf("aws:elasticache:%s:%s:fallback", region, nodeType),
		ProductFamily: "ElastiCache",
		Description:   fmt.Sprintf("ElastiCache %s (static published rate)", nodeType),
		Region:        region,
		Attributes: map[string]string{
			"instanceType": nodeType,
			"fallback":     "true",
			"note":         "static us-east-1 published rate; may vary by region",
		},
		PricingTerm:  models.PricingTermOnDemand,
		PricePerUnit: rate, Unit: models.PriceUnitPerHour,
		Currency: "USD", FetchedAt: &now,
		SourceURL: "https://aws.amazon.com/elasticache/pricing/",
	}}
}
