// gcp_networking.go — GCP networking pricing helpers.
//
// Covered domains:
//   - network/cloud_lb     : Cloud Load Balancing (forwarding rules + data processed)
//   - network/cloud_cdn    : Cloud CDN (cache egress + cache fill)
//   - network/cloud_nat    : Cloud NAT (gateway hours + data processed)
//   - network/cloud_armor  : Cloud Armor (policy fee + request evaluation)
//   - network/egress       : Internet and inter-region egress with tiered rates
//   - inter_region_egress  : Simple GetEgressPrice (base rate, no tiering)
//
// All methods are on *Provider defined in gcp.go (Part 1).
package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/cache"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// GCP service ID for Cloud Armor.
const cloudArmorServiceID = "E3D3-8838-A232"

// egressSourceURL is the canonical source URL for egress pricing pages.
const egressSourceURL = "https://cloud.google.com/vpc/network-pricing"

// --------------------------------------------------------------------------
// Egress continent classification and rate tables
// --------------------------------------------------------------------------

// gcpEgressContinent maps a GCP region prefix to its continent bucket.
// Americas, EMEA, or APAC — used to select the correct internet-egress rate.
// Falls back to "americas" when no prefix matches.
func gcpEgressContinent(region string) string {
	r := strings.ToLower(region)
	type prefixEntry struct {
		prefix    string
		continent string
	}
	// Ordered so longer/more-specific prefixes are checked before shorter ones.
	entries := []prefixEntry{
		{"northamerica-", "americas"},
		{"southamerica-", "americas"},
		{"australia-", "apac"},
		{"europe-", "emea"},
		{"africa-", "emea"},
		{"us-", "americas"},
		{"nam-", "americas"},
		{"eu-", "emea"},
		{"me-", "emea"},
		{"asia-", "apac"},
	}
	for _, e := range entries {
		if strings.HasPrefix(r, e.prefix) {
			return e.continent
		}
	}
	return "americas"
}

// egressTier represents one pricing tier in an internet-egress tier table.
type egressTier struct {
	thresholdGB float64 // minimum GB to enter this tier
	rate        float64 // USD per GB
	label       string
}

// gcpInternetEgressTiers holds the published tiered internet-egress rates per continent.
// Tier breakpoints are in GiB (1 TiB = 1024 GiB).
var gcpInternetEgressTiers = map[string][]egressTier{
	"americas": {
		{0, 0.08, "0–1 TB"},
		{1024, 0.065, "1–10 TB"},
		{10240, 0.045, ">10 TB"},
	},
	"emea": {
		{0, 0.085, "0–1 TB"},
		{1024, 0.065, "1–10 TB"},
		{10240, 0.045, ">10 TB"},
	},
	"apac": {
		{0, 0.12, "0–1 TB"},
		{1024, 0.085, "1–10 TB"},
		{10240, 0.065, ">10 TB"},
	},
}

// gcpInternetEgressBaseRate is the published first-tier rate per continent.
// Used as fallback when the live SKU fetch fails.
var gcpInternetEgressBaseRate = map[string]float64{
	"americas": 0.08,
	"emea":     0.085,
	"apac":     0.12,
}

// gcpIntraEgressRate is the rate for intra-GCP inter-region traffic (same or cross continent).
var gcpIntraEgressRate = map[string]float64{
	"same":  0.01,
	"cross": 0.08,
}

// gcpCrossZoneRate is the flat rate for cross-zone traffic within a single region.
const gcpCrossZoneRate = 0.01

// --------------------------------------------------------------------------
// Tiered egress cost calculation
// --------------------------------------------------------------------------

// tieredCostResult contains the output of computeTieredCost.
type tieredCostResult struct {
	TotalCost        float64            `json:"total_cost"`
	BlendedRatePerGB float64            `json:"blended_rate_per_gb"`
	TierBreakdown    []tierBreakdownRow `json:"tier_breakdown"`
}

type tierBreakdownRow struct {
	Tier    string  `json:"tier"`
	GBs     float64 `json:"gbs"`
	RateUSD float64 `json:"rate_usd"`
	Cost    float64 `json:"cost"`
}

// computeTieredCost calculates the total egress cost for dataGB over the given tiers.
func computeTieredCost(tiers []egressTier, dataGB float64) tieredCostResult {
	if dataGB <= 0 || len(tiers) == 0 {
		rate := 0.0
		if len(tiers) > 0 {
			rate = tiers[0].rate
		}
		return tieredCostResult{BlendedRatePerGB: rate}
	}

	var rows []tierBreakdownRow
	remaining := dataGB
	totalCost := 0.0

	for i, tier := range tiers {
		if remaining <= 0 {
			break
		}
		var tierGB float64
		if i+1 < len(tiers) {
			nextThreshold := tiers[i+1].thresholdGB
			tierCap := nextThreshold - tier.thresholdGB
			tierGB = math.Min(remaining, tierCap)
		} else {
			tierGB = remaining
		}
		cost := tierGB * tier.rate
		rows = append(rows, tierBreakdownRow{
			Tier:    tier.label,
			GBs:     tierGB,
			RateUSD: tier.rate,
			Cost:    cost,
		})
		totalCost += cost
		remaining -= tierGB
	}

	blended := 0.0
	if dataGB > 0 {
		blended = totalCost / dataGB
	}
	return tieredCostResult{
		TotalCost:        totalCost,
		BlendedRatePerGB: blended,
		TierBreakdown:    rows,
	}
}

// --------------------------------------------------------------------------
// Internet egress rate — live SKU fetch with fallback
// --------------------------------------------------------------------------

// fetchInternetEgressRate returns the live first-tier internet-egress rate for
// the given continent, caching the result. Falls back to the published rate if
// the SKU fetch fails or returns nothing.
func (p *Provider) fetchInternetEgressRate(ctx context.Context, continent string) float64 {
	cacheKey := fmt.Sprintf("gcp:egress:internet:%s:rate", continent)
	if raw, ok := p.cache.GetMetadata(cacheKey); ok {
		var m map[string]float64
		if err := json.Unmarshal(raw, &m); err == nil {
			if r, ok2 := m["rate"]; ok2 {
				return r
			}
		}
	}

	var rate float64
	skus, err := p.fetchSKUs(ctx, computeServiceID)
	if err == nil {
		continentLabel := strings.ToLower(continent)
		for _, sku := range skus {
			desc, _ := sku["description"].(string)
			descLower := strings.ToLower(desc)
			if !strings.Contains(descLower, "internet egress") {
				continue
			}
			if !strings.Contains(descLower, "from "+continentLabel) {
				continue
			}
			// Skip China/Australia/Oceania destination rows.
			if strings.Contains(descLower, "china") ||
				strings.Contains(descLower, "australia") ||
				strings.Contains(descLower, "oceania") {
				continue
			}
			r := skuPrice(sku)
			if r > 0 {
				rate = r
				break
			}
		}
	}
	if rate == 0 {
		rate = gcpInternetEgressBaseRate[continent]
		if rate == 0 {
			rate = 0.08
		}
	}

	if raw, err := json.Marshal(map[string]float64{"rate": rate}); err == nil {
		p.cache.SetMetadata(cacheKey, raw, p.cfg.MetadataTTL())
	}
	return rate
}

// --------------------------------------------------------------------------
// Networking price index — LB / CDN / NAT SKUs
// --------------------------------------------------------------------------

// networkingPriceIndex maps lowercase SKU description to unit price.
type networkingPriceIndex map[string]float64

// buildNetworkingIndex fetches Compute Engine SKUs and builds a price index for
// networking-related SKUs (load balancing, CDN, NAT).
func (p *Provider) buildNetworkingIndex(ctx context.Context, region string) (networkingPriceIndex, error) {
	cacheKey := fmt.Sprintf("gcp:networking_price_index:%s", region)
	if raw, ok := p.cache.GetMetadata(cacheKey); ok {
		var m map[string]float64
		if err := json.Unmarshal(raw, &m); err == nil {
			return networkingPriceIndex(m), nil
		}
		slog.Warn("gcp: failed to unmarshal networking index from cache")
	}

	skus, err := p.fetchSKUs(ctx, computeServiceID)
	if err != nil {
		return nil, fmt.Errorf("gcp networking: fetch SKUs: %w", err)
	}

	networkingKeywords := []string{
		"load bal",
		"tcp proxy",
		"ssl proxy",
		"network cdn",
		"cdn cache",
		"cloud nat",
		"nat gateway",
		"nat data",
	}

	idx := make(networkingPriceIndex)
	for _, sku := range skus {
		regions, _ := sku["serviceRegions"].([]any)
		if !skuMatchesRegion(regions, region) {
			continue
		}
		desc, _ := sku["description"].(string)
		descLower := strings.ToLower(desc)
		matched := false
		for _, kw := range networkingKeywords {
			if strings.Contains(descLower, kw) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		price := skuPrice(sku)
		if price > 0 {
			idx[descLower] = price
		}
	}

	if raw, err := json.Marshal(idx); err == nil {
		p.cache.SetMetadata(cacheKey, raw, cache.DefaultMetadataTTL)
	}
	return idx, nil
}

// --------------------------------------------------------------------------
// Network egress (domain=network, service=egress)
// --------------------------------------------------------------------------

// priceNetworkEgress handles the network/egress domain with tiered pricing.
func (p *Provider) priceNetworkEgress(
	ctx context.Context,
	spec *models.NetworkPricingSpec,
) ([]models.NormalizedPrice, map[string]any, error) {
	src := spec.SourceRegion
	if src == "" {
		src = spec.Region
	}
	if src == "" {
		src = "us-central1"
	}
	destType := strings.ToLower(spec.DestinationType)
	if destType == "" {
		destType = "internet"
	}
	destRegion := spec.DestinationRegion
	dataGB := spec.DataGBPerMonth
	if dataGB <= 0 {
		dataGB = spec.EgressGB
	}
	networkTier := strings.ToLower(spec.NetworkTier)
	if networkTier == "" {
		networkTier = "premium"
	}

	srcContinent := gcpEgressContinent(src)

	switch destType {
	case "same_zone":
		tiers := []egressTier{{0, 0.0, "Same-zone within region (free)"}}
		tierResult := computeTieredCost(tiers, dataGB)
		price := models.NormalizedPrice{
			Provider:      models.CloudProviderGCP,
			Service:       "egress",
			SKUID:         fmt.Sprintf("gcp:same_zone:%s", src),
			ProductFamily: "Networking",
			Description:   fmt.Sprintf("GCP same-zone traffic within %s (free)", src),
			Region:        src,
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  0,
			Unit:          models.PriceUnitPerGB,
			Currency:      "USD",
			Attributes: map[string]string{
				"source_region":    src,
				"destination_type": "same_zone",
				"note":             "Traffic between VMs in the same GCP zone is free.",
			},
		}
		breakdown := map[string]any{
			"total_cost":          tierResult.TotalCost,
			"blended_rate_per_gb": tierResult.BlendedRatePerGB,
			"tier_breakdown":      tierResult.TierBreakdown,
		}
		return annotateFreshWithURL([]models.NormalizedPrice{price}, egressSourceURL), breakdown, nil

	case "cross_az":
		tiers := []egressTier{{0, gcpCrossZoneRate, fmt.Sprintf("Cross-zone within same region ($%.2f/GB)", gcpCrossZoneRate)}}
		tierResult := computeTieredCost(tiers, dataGB)
		price := models.NormalizedPrice{
			Provider:      models.CloudProviderGCP,
			Service:       "egress",
			SKUID:         fmt.Sprintf("gcp:cross_zone:%s", src),
			ProductFamily: "Networking",
			Description:   fmt.Sprintf("GCP cross-zone traffic within %s region", src),
			Region:        src,
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  gcpCrossZoneRate,
			Unit:          models.PriceUnitPerGB,
			Currency:      "USD",
			Attributes: map[string]string{
				"source_region":    src,
				"destination_type": "cross_az",
				"note":             "Flat $0.01/GB between zones within the same GCP region.",
			},
		}
		breakdown := map[string]any{
			"total_cost":          tierResult.TotalCost,
			"blended_rate_per_gb": tierResult.BlendedRatePerGB,
			"tier_breakdown":      tierResult.TierBreakdown,
		}
		return annotateFreshWithURL([]models.NormalizedPrice{price}, egressSourceURL), breakdown, nil

	case "cross_region", "cross_continent":
		dstContinent := srcContinent
		if destRegion != "" {
			dstContinent = gcpEgressContinent(destRegion)
		}
		same := srcContinent == dstContinent
		rateKey := "cross"
		if same {
			rateKey = "same"
		}
		rate := gcpIntraEgressRate[rateKey]
		var label string
		if same {
			label = fmt.Sprintf("Same-continent inter-region ($%.2f/GB)", rate)
		} else {
			label = fmt.Sprintf("Cross-continent inter-region ($%.2f/GB)", rate)
		}
		tiers := []egressTier{{0, rate, label}}
		tierResult := computeTieredCost(tiers, dataGB)

		desc := fmt.Sprintf("GCP inter-region data transfer %s", src)
		if destRegion != "" {
			desc += " to " + destRegion
		}
		skuSuffix := dstContinent
		if destRegion != "" {
			skuSuffix = destRegion
		}
		attrs := map[string]string{
			"source_region":    src,
			"destination_type": destType,
			"continent":        srcContinent,
		}
		if destRegion != "" {
			attrs["dest_region"] = destRegion
		}
		price := models.NormalizedPrice{
			Provider:      models.CloudProviderGCP,
			Service:       "egress",
			SKUID:         fmt.Sprintf("gcp:inter_region:%s:%s", src, skuSuffix),
			ProductFamily: "Networking",
			Description:   desc,
			Region:        src,
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  rate,
			Unit:          models.PriceUnitPerGB,
			Currency:      "USD",
			Attributes:    attrs,
		}
		breakdown := map[string]any{
			"total_cost":          tierResult.TotalCost,
			"blended_rate_per_gb": tierResult.BlendedRatePerGB,
			"tier_breakdown":      tierResult.TierBreakdown,
		}
		return annotateFreshWithURL([]models.NormalizedPrice{price}, egressSourceURL), breakdown, nil

	default:
		// Internet egress with tiering
		tiers := gcpInternetEgressTiers[srcContinent]
		if len(tiers) == 0 {
			tiers = gcpInternetEgressTiers["americas"]
		}

		// Attempt live rate fetch to verify first-tier value.
		liveRate := p.fetchInternetEgressRate(ctx, srcContinent)
		if len(tiers) > 0 && liveRate > 0 && liveRate != tiers[0].rate {
			modified := make([]egressTier, len(tiers))
			copy(modified, tiers)
			modified[0].rate = liveRate
			tiers = modified
		}

		tierResult := computeTieredCost(tiers, dataGB)
		blended := tiers[0].rate
		if dataGB > 0 {
			blended = tierResult.BlendedRatePerGB
		}

		desc := fmt.Sprintf(
			"GCP internet egress from %s (%s, %s tier, tiered)",
			src, strings.ToUpper(srcContinent), networkTier,
		)
		if dataGB > 0 {
			desc += fmt.Sprintf(
				": %.0f GB/month — $%.4f ($%.4f/GB blended)",
				dataGB, tierResult.TotalCost, blended,
			)
		}
		price := models.NormalizedPrice{
			Provider:      models.CloudProviderGCP,
			Service:       "egress",
			SKUID:         fmt.Sprintf("gcp:internet_egress:%s:%s", src, networkTier),
			ProductFamily: "Networking",
			Description:   desc,
			Region:        src,
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  blended,
			Unit:          models.PriceUnitPerGB,
			Currency:      "USD",
			Attributes: map[string]string{
				"source_region":    src,
				"destination_type": "internet",
				"continent":        srcContinent,
				"network_tier":     networkTier,
				"note": fmt.Sprintf(
					"Tiered rates for %s internet egress. China and Australia destinations billed at higher rates.",
					strings.ToUpper(srcContinent),
				),
			},
		}
		breakdown := map[string]any{
			"total_cost":          tierResult.TotalCost,
			"blended_rate_per_gb": tierResult.BlendedRatePerGB,
			"tier_breakdown":      tierResult.TierBreakdown,
		}
		return annotateFreshWithURL([]models.NormalizedPrice{price}, egressSourceURL), breakdown, nil
	}
}

// --------------------------------------------------------------------------
// GetEgressPrice — simple egress pricing for inter_region_egress domain
// --------------------------------------------------------------------------

// GetEgressPrice returns GCP outbound data transfer pricing.
//
// For internet egress (destRegion empty), returns the source-continent base rate
// (first 1 TB/month). For intra-GCP inter-region traffic, returns $0.01/GB
// (same continent) or $0.08/GB (cross-continent).
func (p *Provider) GetEgressPrice(
	ctx context.Context,
	sourceRegion string,
	destRegion string,
	dataGB float64,
) ([]models.NormalizedPrice, error) {
	srcContinent := gcpEgressContinent(sourceRegion)

	var rate float64
	var egressType, desc, note, skuSuffix string
	attrs := map[string]string{
		"source_region": sourceRegion,
		"continent":     srcContinent,
	}

	if destRegion == "" {
		rate = p.fetchInternetEgressRate(ctx, srcContinent)
		egressType = "internet"
		desc = fmt.Sprintf("GCP internet egress from %s (%s)", sourceRegion, strings.ToUpper(srcContinent))
		note = fmt.Sprintf(
			"Base rate for %s internet egress (0–1 TB/month). China and Australia destinations are billed at higher rates.",
			strings.ToUpper(srcContinent),
		)
		skuSuffix = fmt.Sprintf("internet:%s", srcContinent)
	} else {
		dstContinent := gcpEgressContinent(destRegion)
		same := srcContinent == dstContinent
		rateKey := "cross"
		if same {
			rateKey = "same"
		}
		rate = gcpIntraEgressRate[rateKey]
		egressType = "inter-region"
		desc = fmt.Sprintf("GCP inter-region egress from %s to %s", sourceRegion, destRegion)
		if same {
			note = fmt.Sprintf(
				"GCP VPC inter-region egress: %s → %s. Same-continent rate ($0.01/GB).",
				strings.ToUpper(srcContinent), strings.ToUpper(dstContinent),
			)
		} else {
			note = fmt.Sprintf(
				"GCP VPC inter-region egress: %s → %s. Cross-continent rate ($0.08/GB).",
				strings.ToUpper(srcContinent), strings.ToUpper(dstContinent),
			)
		}
		skuSuffix = fmt.Sprintf("intraregion:%s:%s", srcContinent, dstContinent)
		attrs["dest_region"] = destRegion
	}
	attrs["egress_type"] = egressType
	attrs["note"] = note

	safeGB := dataGB
	if math.IsInf(dataGB, 0) || math.IsNaN(dataGB) {
		safeGB = 0
	}
	if safeGB < 0 {
		safeGB = 0
	}
	if safeGB > 0 {
		attrs["monthly_estimate"] = fmt.Sprintf("$%.4f for %.1f GB", safeGB*rate, dataGB)
	}

	price := models.NormalizedPrice{
		Provider:      models.CloudProviderGCP,
		Service:       "inter_region_egress",
		SKUID:         fmt.Sprintf("gcp:egress:%s:%s", sourceRegion, skuSuffix),
		ProductFamily: "Networking",
		Description:   desc,
		Region:        sourceRegion,
		PricingTerm:   models.PricingTermOnDemand,
		PricePerUnit:  rate,
		Unit:          models.PriceUnitPerGB,
		Currency:      "USD",
		Attributes:    attrs,
	}
	return annotateFresh([]models.NormalizedPrice{price}), nil
}

// breakdownMoney wraps a dollar amount in a currency-typed display map,
// mirroring internal/tools/lookup.go's moneyDict shape for cross-codebase
// consistency (this package cannot import internal/tools due to layering).
func breakdownMoney(amount float64, label string) map[string]any {
	return map[string]any{
		"amount":   amount,
		"currency": "USD",
		"display":  fmt.Sprintf("$%.2f%s", amount, label),
	}
}

// --------------------------------------------------------------------------
// Cloud Load Balancing
// --------------------------------------------------------------------------

// priceNetworkLB prices Cloud Load Balancing forwarding rules and data processed.
func (p *Provider) priceNetworkLB(
	ctx context.Context,
	spec *models.NetworkPricingSpec,
) ([]models.NormalizedPrice, map[string]any, error) {
	region := spec.Region
	if region == "" {
		region = "us-central1"
	}
	lbType := strings.ToLower(spec.LBType)
	if lbType == "" {
		lbType = "https"
	}

	idx, err := p.buildNetworkingIndex(ctx, region)
	if err != nil {
		return nil, nil, fmt.Errorf("gcp cloud_lb: %w", err)
	}

	var ruleRate, dataRate float64
	for desc, price := range idx {
		if ruleRate == 0 {
			switch lbType {
			case "https", "http":
				if (strings.Contains(desc, "external http") || strings.Contains(desc, "https load balancing rule")) &&
					strings.Contains(desc, "rule") {
					ruleRate = price
				}
			case "tcp":
				if strings.Contains(desc, "tcp proxy") && strings.Contains(desc, "rule") {
					ruleRate = price
				}
			case "ssl":
				if strings.Contains(desc, "ssl proxy") && strings.Contains(desc, "rule") {
					ruleRate = price
				}
			case "network", "internal":
				if strings.Contains(desc, "network load balancing") && strings.Contains(desc, "forwarding rule") {
					ruleRate = price
				}
			}
		}
		if dataRate == 0 && strings.Contains(desc, "data processed by") && strings.Contains(desc, "load bal") {
			dataRate = price
		}
	}

	fallback := ruleRate == 0
	if ruleRate == 0 {
		ruleRate = 0.008
	}
	if dataRate == 0 {
		dataRate = 0.008
	}

	prices := []models.NormalizedPrice{
		{
			Provider:      models.CloudProviderGCP,
			Service:       "network",
			SKUID:         fmt.Sprintf("gcp:cloud_lb:%s:%s:rule", lbType, region),
			ProductFamily: "Cloud Load Balancing",
			Description:   fmt.Sprintf("Cloud LB (%s) forwarding rule per hour", lbType),
			Region:        region,
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  ruleRate,
			Unit:          models.PriceUnitPerHour,
			Currency:      "USD",
			Attributes:    map[string]string{"lb_type": lbType, "component": "forwarding_rule"},
		},
		{
			Provider:      models.CloudProviderGCP,
			Service:       "network",
			SKUID:         fmt.Sprintf("gcp:cloud_lb:%s:%s:data", lbType, region),
			ProductFamily: "Cloud Load Balancing",
			Description:   "Cloud LB data processed per GB",
			Region:        region,
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  dataRate,
			Unit:          models.PriceUnitPerGB,
			Currency:      "USD",
			Attributes:    map[string]string{"lb_type": lbType, "component": "data_processed"},
		},
	}

	ruleCount := float64(spec.RuleCount)
	if ruleCount <= 0 {
		ruleCount = 1
	}
	hoursPerMonth := spec.HoursPerMonth
	if hoursPerMonth <= 0 {
		hoursPerMonth = 730
	}
	ruleMonthlyCost := ruleRate * ruleCount * hoursPerMonth
	dataCost := dataRate * spec.DataGB
	breakdown := map[string]any{
		"lb_type":            lbType,
		"rule_count":         spec.RuleCount,
		"monthly_rule_cost":  breakdownMoney(ruleMonthlyCost, "/mo"),
		"monthly_data_cost":  breakdownMoney(dataCost, "/mo"),
		"monthly_total":      breakdownMoney(ruleMonthlyCost+dataCost, "/mo"),
		"monthly_total_note": "monthly_total = monthly_rule_cost + monthly_data_cost.",
	}
	if fallback {
		breakdown["fallback"] = true
	}
	return annotateFresh(prices), breakdown, nil
}

// --------------------------------------------------------------------------
// Cloud CDN
// --------------------------------------------------------------------------

// priceNetworkCDN prices Cloud CDN cache egress and cache fill.
func (p *Provider) priceNetworkCDN(
	ctx context.Context,
	spec *models.NetworkPricingSpec,
) ([]models.NormalizedPrice, map[string]any, error) {
	region := spec.Region
	if region == "" {
		region = "us-central1"
	}

	idx, err := p.buildNetworkingIndex(ctx, region)
	if err != nil {
		return nil, nil, fmt.Errorf("gcp cloud_cdn: %w", err)
	}

	var egressRate, fillRate float64
	for desc, price := range idx {
		if fillRate == 0 && strings.Contains(desc, "cdn cache fill") {
			fillRate = price
		}
		if egressRate == 0 && strings.Contains(desc, "cdn") && strings.Contains(desc, "egress") {
			egressRate = price
		}
	}

	fallback := egressRate == 0 || fillRate == 0
	if egressRate == 0 {
		egressRate = 0.02
	}
	if fillRate == 0 {
		fillRate = 0.01
	}

	prices := []models.NormalizedPrice{
		{
			Provider:      models.CloudProviderGCP,
			Service:       "network",
			SKUID:         fmt.Sprintf("gcp:cloud_cdn:%s:egress", region),
			ProductFamily: "Cloud CDN",
			Description:   "Cloud CDN cache egress per GB",
			Region:        region,
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  egressRate,
			Unit:          models.PriceUnitPerGB,
			Currency:      "USD",
		},
		{
			Provider:      models.CloudProviderGCP,
			Service:       "network",
			SKUID:         fmt.Sprintf("gcp:cloud_cdn:%s:cache_fill", region),
			ProductFamily: "Cloud CDN",
			Description:   "Cloud CDN cache fill per GB",
			Region:        region,
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  fillRate,
			Unit:          models.PriceUnitPerGB,
			Currency:      "USD",
		},
	}

	breakdown := map[string]any{
		"monthly_egress_cost":     breakdownMoney(egressRate*spec.EgressGB, "/mo"),
		"monthly_cache_fill_cost": breakdownMoney(fillRate*spec.CacheFillGB, "/mo"),
		"monthly_total":           breakdownMoney(egressRate*spec.EgressGB+fillRate*spec.CacheFillGB, "/mo"),
		"monthly_total_note":      "monthly_total = monthly_egress_cost + monthly_cache_fill_cost.",
	}
	if fallback {
		breakdown["fallback"] = true
	}
	return annotateFresh(prices), breakdown, nil
}

// --------------------------------------------------------------------------
// Cloud NAT
// --------------------------------------------------------------------------

// priceNetworkNAT prices Cloud NAT gateways and data processed.
func (p *Provider) priceNetworkNAT(
	ctx context.Context,
	spec *models.NetworkPricingSpec,
) ([]models.NormalizedPrice, map[string]any, error) {
	region := spec.Region
	if region == "" {
		region = "us-central1"
	}

	idx, err := p.buildNetworkingIndex(ctx, region)
	if err != nil {
		return nil, nil, fmt.Errorf("gcp cloud_nat: %w", err)
	}

	var gatewayRate, dataRate float64
	for desc, price := range idx {
		if gatewayRate == 0 && strings.Contains(desc, "cloud nat gateway") {
			gatewayRate = price
		}
		if dataRate == 0 && strings.Contains(desc, "cloud nat") &&
			(strings.Contains(desc, "data processed") || strings.Contains(desc, "nat gateway data")) {
			dataRate = price
		}
	}

	fallback := gatewayRate == 0
	if gatewayRate == 0 {
		gatewayRate = 0.044
	}
	if dataRate == 0 {
		dataRate = 0.045
	}

	prices := []models.NormalizedPrice{
		{
			Provider:      models.CloudProviderGCP,
			Service:       "network",
			SKUID:         fmt.Sprintf("gcp:cloud_nat:%s:gateway", region),
			ProductFamily: "Cloud NAT",
			Description:   "Cloud NAT gateway per hour",
			Region:        region,
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  gatewayRate,
			Unit:          models.PriceUnitPerHour,
			Currency:      "USD",
		},
		{
			Provider:      models.CloudProviderGCP,
			Service:       "network",
			SKUID:         fmt.Sprintf("gcp:cloud_nat:%s:data", region),
			ProductFamily: "Cloud NAT",
			Description:   "Cloud NAT data processed per GB",
			Region:        region,
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  dataRate,
			Unit:          models.PriceUnitPerGB,
			Currency:      "USD",
		},
	}

	gatewayCount := float64(spec.GatewayCount)
	if gatewayCount <= 0 {
		gatewayCount = 1
	}
	hoursPerMonth := spec.HoursPerMonth
	if hoursPerMonth <= 0 {
		hoursPerMonth = 730
	}
	gwMonthlyCost := gatewayRate * gatewayCount * hoursPerMonth
	dataCost := dataRate * spec.DataGB
	breakdown := map[string]any{
		"gateway_count":        spec.GatewayCount,
		"monthly_gateway_cost": breakdownMoney(gwMonthlyCost, "/mo"),
		"monthly_data_cost":    breakdownMoney(dataCost, "/mo"),
		"monthly_total":        breakdownMoney(gwMonthlyCost+dataCost, "/mo"),
		"monthly_total_note":   "monthly_total = monthly_gateway_cost + monthly_data_cost.",
	}
	if fallback {
		breakdown["fallback"] = true
	}
	return annotateFresh(prices), breakdown, nil
}

// --------------------------------------------------------------------------
// Cloud Armor
// --------------------------------------------------------------------------

// priceNetworkArmor prices Cloud Armor security policies and request evaluation.
func (p *Provider) priceNetworkArmor(
	ctx context.Context,
	spec *models.NetworkPricingSpec,
) ([]models.NormalizedPrice, map[string]any, error) {
	var policyRate, requestRate float64
	fallback := false

	skus, err := p.fetchSKUs(ctx, cloudArmorServiceID)
	if err != nil {
		slog.Warn("gcp cloud_armor: fetch SKUs failed", "err", err)
		fallback = true
	} else {
		for _, sku := range skus {
			desc, _ := sku["description"].(string)
			descLower := strings.ToLower(desc)
			if !strings.Contains(descLower, "cloud armor") {
				continue
			}
			if policyRate == 0 && (strings.Contains(descLower, "policy") || strings.Contains(descLower, "security policy")) {
				pr := skuPrice(sku)
				if pr > 0 {
					policyRate = pr
				}
			}
			if requestRate == 0 && strings.Contains(descLower, "request") {
				pr := skuPrice(sku)
				if pr > 0 {
					requestRate = pr
				}
			}
		}
	}

	fallback = fallback || policyRate == 0 || requestRate == 0
	if policyRate == 0 {
		policyRate = 0.75
	}
	if requestRate == 0 {
		requestRate = 0.75
	}

	region := spec.Region
	if region == "" {
		region = "global"
	}

	prices := []models.NormalizedPrice{
		{
			Provider:      models.CloudProviderGCP,
			Service:       "network",
			SKUID:         "gcp:cloud_armor:policy",
			ProductFamily: "Cloud Armor",
			Description:   "Cloud Armor security policy per month",
			Region:        region,
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  policyRate,
			Unit:          models.PriceUnitPerMonth,
			Currency:      "USD",
		},
		{
			Provider:      models.CloudProviderGCP,
			Service:       "network",
			SKUID:         "gcp:cloud_armor:requests",
			ProductFamily: "Cloud Armor",
			Description:   "Cloud Armor request evaluation per million requests",
			Region:        region,
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  requestRate,
			Unit:          models.PriceUnitPerUnit,
			Currency:      "USD",
			Attributes:    map[string]string{"billing_dimension": "per_million_requests"},
		},
	}

	policyCost := policyRate * float64(spec.PolicyCount)
	reqCost := requestRate * spec.MonthlyRequestsMillions
	breakdown := map[string]any{
		"monthly_policy_cost":  breakdownMoney(policyCost, "/mo"),
		"monthly_request_cost": breakdownMoney(reqCost, "/mo"),
		"monthly_total":        breakdownMoney(policyCost+reqCost, "/mo"),
		"monthly_total_note":   "monthly_total = monthly_policy_cost + monthly_request_cost.",
	}

	var zeroDims []string
	if spec.PolicyCount == 0 {
		zeroDims = append(zeroDims, "policy_count")
	}
	if spec.MonthlyRequestsMillions <= 0 {
		zeroDims = append(zeroDims, "monthly_requests_millions")
	}
	if len(zeroDims) > 0 {
		rates := fmt.Sprintf(
			"Policy: $%.4f/policy/month; Requests: $%.4f/million requests.",
			policyRate, requestRate,
		)
		breakdown["note"] = fmt.Sprintf(
			"Cost for %s not shown — set to a non-zero value to include it in the estimate. %s",
			strings.Join(zeroDims, " and "), rates,
		)
	}
	if fallback {
		breakdown["fallback"] = true
		breakdown["fallback_note"] = "Using hardcoded fallback rates ($0.75/policy/month, $0.75/million requests); live SKU catalog unavailable or returned no match. Verify current rates at https://cloud.google.com/armor/pricing."
	}
	return annotateFresh(prices), breakdown, nil
}

// --------------------------------------------------------------------------
// priceNetwork — dispatcher for domain=network
// --------------------------------------------------------------------------

// priceNetwork dispatches to the appropriate sub-method based on spec.Service.
func (p *Provider) priceNetwork(
	ctx context.Context,
	spec *models.NetworkPricingSpec,
) ([]models.NormalizedPrice, map[string]any, error) {
	svc := strings.ToLower(spec.Service)
	switch svc {
	case "egress":
		return p.priceNetworkEgress(ctx, spec)
	case "cloud_cdn":
		return p.priceNetworkCDN(ctx, spec)
	case "cloud_nat":
		return p.priceNetworkNAT(ctx, spec)
	case "cloud_armor":
		return p.priceNetworkArmor(ctx, spec)
	default:
		// Default to Cloud LB
		return p.priceNetworkLB(ctx, spec)
	}
}

// --------------------------------------------------------------------------
// annotateFreshWithURL — variant that allows a custom source URL
// --------------------------------------------------------------------------

// annotateFreshWithURL stamps freshness metadata with a custom source URL.
func annotateFreshWithURL(prices []models.NormalizedPrice, srcURL string) []models.NormalizedPrice {
	out := annotateFresh(prices)
	for i := range out {
		out[i].SourceURL = srcURL
	}
	return out
}
