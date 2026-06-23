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
	if p.pricingClient != nil {
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

	rawItems, err := p.GetProducts(ctx, "AmazonElastiCache", filters, 10)
	if err != nil {
		return nil, fmt.Errorf("aws: GetElastiCachePrice: %w", err)
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
	return prices, nil
}
