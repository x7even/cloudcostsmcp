// gcp_ai.go — GCP AI/ML and analytics pricing helpers (Part 2).
//
// Covered domains:
//   - ai/vertex_ai  : Vertex AI custom training / prediction (per-vCPU-hr + per-GiB-hr)
//   - ai/gemini     : Vertex AI Gemini model token / character pricing
//   - analytics/bigquery: BigQuery on-demand query, storage, and streaming pricing
//
// All methods are on *Provider, which is defined in gcp.go (Part 1).
package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// GCP Cloud Billing Catalog service IDs for AI / analytics.
const (
	vertexServiceID   = "C7E2-9256-1C43"
	bigqueryServiceID = "24E6-581D-38E5"
)

// --------------------------------------------------------------------------
// Vertex AI SKU index
// --------------------------------------------------------------------------

// buildVertexIndex fetches Vertex AI SKUs and builds a (desc, usageType) → price index.
func (p *Provider) buildVertexIndex(ctx context.Context, region string) (priceIndex, error) {
	cacheKey := "gcp:vertex_index:" + region
	if idx, ok := p.loadPriceIndex(cacheKey); ok {
		return idx, nil
	}

	skus, err := p.fetchSKUs(ctx, vertexServiceID)
	if err != nil {
		return nil, fmt.Errorf("gcp vertex ai: fetch SKUs: %w", err)
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

// --------------------------------------------------------------------------
// Vertex AI compute pricing
// --------------------------------------------------------------------------

// getVertexPrice returns Vertex AI custom training / prediction pricing for the
// given machine type and task.
//
// Returns prices (per-vCPU-hr and per-GiB-RAM-hr) plus a Breakdown map.
func (p *Provider) getVertexPrice(
	ctx context.Context,
	machineType string,
	region string,
	hours float64,
	task string,
) ([]models.NormalizedPrice, map[string]any, error) {
	// Extract family prefix: "n1" from "n1-standard-4", "a2" from "a2-highgpu-1g".
	var family string
	if idx := strings.IndexByte(machineType, '-'); idx >= 0 {
		family = strings.ToLower(machineType[:idx])
	} else {
		family = strings.ToLower(machineType)
	}

	idx, err := p.buildVertexIndex(ctx, region)
	if err != nil {
		return nil, nil, err
	}

	if len(idx) == 0 {
		breakdown := map[string]any{
			"error": fmt.Sprintf(
				"no Vertex AI pricing found for machine_type=%q in region=%q. No SKUs returned from Vertex AI catalog.",
				machineType, region,
			),
		}
		return nil, breakdown, nil
	}

	taskLower := strings.ToLower(task)
	taskKeyword := "prediction"
	if strings.Contains(taskLower, "train") {
		taskKeyword = "training"
	}

	var vcpuRate, ramRate float64

	for k, price := range idx {
		desc := k[0]
		dl := strings.ToLower(desc)

		if !strings.Contains(dl, family) {
			continue
		}
		// If desc explicitly mentions the other task, skip.
		if taskKeyword == "training" && strings.Contains(dl, "prediction") && !strings.Contains(dl, "training") {
			continue
		}
		if taskKeyword == "prediction" && strings.Contains(dl, "training") && !strings.Contains(dl, "prediction") {
			continue
		}

		isVCPU := strings.Contains(dl, "vcpu") || strings.Contains(dl, "core")
		isRAM := strings.Contains(dl, "ram") || strings.Contains(dl, "memory")

		if isVCPU && vcpuRate == 0 {
			vcpuRate = price
		} else if isRAM && ramRate == 0 {
			ramRate = price
		}
		if vcpuRate != 0 && ramRate != 0 {
			break
		}
	}

	if vcpuRate == 0 || ramRate == 0 {
		breakdown := map[string]any{
			"error": fmt.Sprintf(
				"no Vertex AI SKUs matched machine_type=%q (family=%q), task=%q, region=%q. "+
					"The SKU catalog may use different naming — try a broader family prefix.",
				machineType, family, task, region,
			),
			"hint": "Available SKU descriptions can be inspected via search_pricing(service='vertexai').",
		}
		return nil, breakdown, nil
	}

	breakdown := map[string]any{
		"provider":            "gcp",
		"service":             "vertex_ai",
		"machine_type":        machineType,
		"family":              family,
		"task":                task,
		"region":              region,
		"hours":               hours,
		"vcpu_rate_per_hr":    fmt.Sprintf("$%.6f", vcpuRate),
		"ram_rate_per_gib_hr": fmt.Sprintf("$%.6f", ramRate),
		"note": fmt.Sprintf(
			"Rates are per vCPU-hour and per GiB-RAM-hour. "+
				"To estimate total cost: (vcpus × vcpu_rate + ram_gib × ram_rate) × %.0f hours. "+
				"Use list_instance_types(provider='gcp', region='%s') to get specs.",
			hours, region,
		),
	}

	prices := []models.NormalizedPrice{
		{
			Provider:      models.CloudProviderGCP,
			Service:       "ai",
			SKUID:         fmt.Sprintf("gcp:vertex:vcpu:%s:%s:%s", family, task, region),
			ProductFamily: "Vertex AI",
			Description:   fmt.Sprintf("Vertex AI %s %s vCPU in %s", family, task, region),
			Region:        region,
			Attributes: map[string]string{
				"machine_type": machineType,
				"family":       family,
				"task":         task,
				"resource":     "vcpu",
			},
			PricingTerm:  models.PricingTermOnDemand,
			PricePerUnit: vcpuRate,
			Unit:         models.PriceUnitPerHour,
			Currency:     "USD",
		},
		{
			Provider:      models.CloudProviderGCP,
			Service:       "ai",
			SKUID:         fmt.Sprintf("gcp:vertex:ram:%s:%s:%s", family, task, region),
			ProductFamily: "Vertex AI",
			Description:   fmt.Sprintf("Vertex AI %s %s RAM in %s", family, task, region),
			Region:        region,
			Attributes: map[string]string{
				"machine_type": machineType,
				"family":       family,
				"task":         task,
				"resource":     "ram",
			},
			PricingTerm:  models.PricingTermOnDemand,
			PricePerUnit: ramRate,
			Unit:         models.PriceUnitPerHour,
			Currency:     "USD",
		},
	}
	return annotateFresh(prices), breakdown, nil
}

// --------------------------------------------------------------------------
// Gemini token pricing
// --------------------------------------------------------------------------

// getGeminiPrice returns Vertex AI Gemini model token / character pricing.
//
// Searches Vertex AI SKUs for entries whose description contains "Gemini"
// and the given model name substring. Returns input and output rates found in
// the catalog in a Breakdown map.
func (p *Provider) getGeminiPrice(
	ctx context.Context,
	model string,
	region string,
) ([]models.NormalizedPrice, map[string]any, error) {
	idx, err := p.buildVertexIndex(ctx, region)
	if err != nil {
		return nil, nil, err
	}

	if len(idx) == 0 {
		breakdown := map[string]any{
			"error": fmt.Sprintf(
				"no Vertex AI SKUs found for region=%q. "+
					"Gemini pricing may be global — try region='global' or 'us-central1'.",
				region,
			),
		}
		return nil, breakdown, nil
	}

	modelLower := strings.ToLower(model)
	// Extract meaningful parts from slug like "gemini-1.5-flash" → ["1.5", "flash"]
	slug := strings.TrimPrefix(modelLower, "gemini-")
	slug = strings.TrimPrefix(slug, "gemini")
	var modelParts []string
	for _, part := range strings.Split(slug, "-") {
		if len(part) > 1 {
			modelParts = append(modelParts, part)
		}
	}

	rates := make(map[string]string)
	var prices []models.NormalizedPrice

	for k, price := range idx {
		desc := k[0]
		dl := strings.ToLower(desc)

		if !strings.Contains(dl, "gemini") {
			continue
		}
		// Match on model name parts if we have any.
		allMatch := true
		for _, part := range modelParts {
			if !strings.Contains(dl, part) {
				allMatch = false
				break
			}
		}
		if !allMatch {
			continue
		}

		// Determine input vs output direction.
		direction := "other"
		if strings.Contains(dl, "input") {
			direction = "input"
		} else if strings.Contains(dl, "output") {
			direction = "output"
		}

		safeKey := fmt.Sprintf("%s:%s", direction, desc)
		rates[safeKey] = fmt.Sprintf("$%.8f", price)

		skuID := fmt.Sprintf("gcp:gemini:%s:%s:%s", model, direction, region)
		prices = append(prices, models.NormalizedPrice{
			Provider:      models.CloudProviderGCP,
			Service:       "ai",
			SKUID:         skuID,
			ProductFamily: "Vertex AI Gemini",
			Description:   desc,
			Region:        region,
			Attributes: map[string]string{
				"model":     model,
				"direction": direction,
			},
			PricingTerm:  models.PricingTermOnDemand,
			PricePerUnit: price,
			Unit:         models.PriceUnitPerUnit,
			Currency:     "USD",
		})
	}

	if len(rates) == 0 {
		breakdown := map[string]any{
			"error": fmt.Sprintf(
				"no Gemini SKUs matched model=%q in region=%q. "+
					"The model name may differ from the SKU catalog. "+
					"Try model='gemini-1.5-flash', 'gemini-1.0-pro', or 'gemini-1.5-pro'.",
				model, region,
			),
			"region": region,
			"model":  model,
		}
		return nil, breakdown, nil
	}

	breakdown := map[string]any{
		"provider": "gcp",
		"service":  "vertex_ai_gemini",
		"model":    model,
		"region":   region,
		"rates":    rates,
		"note":     "Rates are per character or per token depending on the SKU. Check the 'rates' keys for input/output direction and unit from SKU description.",
	}
	return annotateFresh(prices), breakdown, nil
}

// --------------------------------------------------------------------------
// BigQuery analytics pricing
// --------------------------------------------------------------------------

// bigqueryPriceIndex maps SKU description → unit price.
// (simpler than priceIndex: no usageType dimension needed)
type bigqueryPriceIndex map[string]float64

// buildBigQueryIndex fetches BigQuery SKUs and builds a desc → price index.
// BigQuery uses multi-region identifiers ("us", "eu") as well as single-region codes.
func (p *Provider) buildBigQueryIndex(ctx context.Context, region string) (bigqueryPriceIndex, error) {
	cacheKey := "gcp:bigquery_index_v2:" + region
	if raw, ok := p.cache.GetMetadata(cacheKey); ok {
		var m bigqueryPriceIndex
		if err := json.Unmarshal(raw, &m); err == nil {
			return m, nil
		}
		slog.Warn("gcp: failed to unmarshal BigQuery index from cache; re-fetching", "region", region)
	}

	skus, err := p.fetchSKUs(ctx, bigqueryServiceID)
	if err != nil {
		return nil, fmt.Errorf("gcp bigquery: fetch SKUs: %w", err)
	}

	idx := make(bigqueryPriceIndex)
	for _, sku := range skus {
		regions, _ := sku["serviceRegions"].([]any)
		// BigQuery SKUs use named regions or multi-region codes ("us", "eu", "asia") —
		// no "global" SKUs, so we match only exact region strings.
		matched := false
		for _, r := range regions {
			if s, _ := r.(string); s == region {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}

		desc, _ := sku["description"].(string)

		// BigQuery Analysis, Active Storage, and Long-term Storage SKUs have a
		// free-quota tier at startUsageAmount=0 followed by the actual charged rate.
		// Use skuPaidPrice to skip the free tier for these SKUs.
		var price float64
		if (strings.Contains(desc, "Analysis") && !strings.Contains(desc, "Streaming")) ||
			strings.Contains(desc, "Active Logical Storage") ||
			strings.Contains(desc, "Long Term Logical Storage") {
			price = skuPaidPrice(sku)
		} else {
			price = skuPrice(sku)
		}
		if price > 0 {
			idx[desc] = price
		}
	}

	if raw, err := json.Marshal(idx); err == nil {
		p.cache.SetMetadata(cacheKey, raw, p.cfg.MetadataTTL())
	}
	return idx, nil
}

// skuPaidPrice extracts the first paid-tier unit price (startUsageAmount > 0) from a SKU.
// Used for BigQuery SKUs that have a free-quota tier at startUsageAmount == 0.
func skuPaidPrice(sku map[string]any) float64 {
	pi, _ := sku["pricingInfo"].([]any)
	if len(pi) == 0 {
		return 0
	}
	pe, _ := pi[0].(map[string]any)
	if pe == nil {
		return 0
	}
	expr, _ := pe["pricingExpression"].(map[string]any)
	if expr == nil {
		return 0
	}
	tiers, _ := expr["tieredRates"].([]any)
	for _, t := range tiers {
		tier, _ := t.(map[string]any)
		if tier == nil {
			continue
		}
		start, _ := tier["startUsageAmount"].(float64)
		if start <= 0 {
			continue
		}
		up, _ := tier["unitPrice"].(map[string]any)
		if up == nil {
			continue
		}
		units, _ := up["units"].(string)
		nanos, _ := up["nanos"].(float64)
		return gcpMoney(units, int(nanos))
	}
	return 0
}

// getBigQueryPrice returns BigQuery on-demand pricing for the given region and
// optional usage volumes.
//
// BigQuery uses a multi-region / single-region model. Pass region="us" for the
// US multi-region, or region="us-central1" for a specific region. If the exact
// region yields no SKUs, the corresponding multi-region prefix is tried.
func (p *Provider) getBigQueryPrice(
	ctx context.Context,
	region string,
	queryTB *float64,
	activeStorageGB *float64,
	longtermStorageGB *float64,
	streamingGB *float64,
) ([]models.NormalizedPrice, map[string]any, error) {
	idx, err := p.buildBigQueryIndex(ctx, region)
	if err != nil {
		return nil, nil, err
	}

	// Fallback to multi-region prefix if no SKUs for exact region.
	if len(idx) == 0 {
		fallback := bigQueryMultiRegion(region)
		if fallback != "" && fallback != region {
			idx, err = p.buildBigQueryIndex(ctx, fallback)
			if err != nil {
				return nil, nil, err
			}
		}
	}

	var analysisRate, activeStorageRate, longtermStorageRate, streamingRate float64
	for desc, price := range idx {
		switch {
		case strings.Contains(desc, "Active Logical Storage") && activeStorageRate == 0:
			activeStorageRate = price
		case strings.Contains(desc, "Long Term Logical Storage") && longtermStorageRate == 0:
			longtermStorageRate = price
		case strings.Contains(desc, "Analysis") && !strings.Contains(desc, "Streaming") && analysisRate == 0:
			analysisRate = price
		case strings.Contains(desc, "Streaming Insert") && streamingRate == 0:
			streamingRate = price
		}
	}

	result := map[string]any{
		"region":                           region,
		"provider":                         "gcp",
		"service":                          "bigquery",
		"analysis_rate_per_tib":            fmt.Sprintf("$%.6f/TiB", analysisRate),
		"active_storage_rate_per_gib_mo":   fmt.Sprintf("$%.6f/GiB-mo", activeStorageRate),
		"longterm_storage_rate_per_gib_mo": fmt.Sprintf("$%.6f/GiB-mo", longtermStorageRate),
		"streaming_insert_rate_per_gib":    fmt.Sprintf("$%.6f/GiB", streamingRate),
		"note": "BigQuery free tier: first 1 TiB/month of query processing is free; " +
			"first 10 GiB/month of active storage is free. " +
			"Rates shown are for usage above the free tier.",
	}

	if queryTB != nil {
		result["estimated_query_cost"] = fmt.Sprintf("$%.2f", (*queryTB)*analysisRate)
	}
	if activeStorageGB != nil {
		result["estimated_active_storage_cost"] = fmt.Sprintf("$%.2f", (*activeStorageGB)/1024*activeStorageRate)
	}
	if longtermStorageGB != nil {
		result["estimated_longterm_storage_cost"] = fmt.Sprintf("$%.2f", (*longtermStorageGB)/1024*longtermStorageRate)
	}
	if streamingGB != nil {
		result["estimated_streaming_cost"] = fmt.Sprintf("$%.2f", (*streamingGB)/1024*streamingRate)
	}

	// Build NormalizedPrices for each rate found.
	var prices []models.NormalizedPrice
	if analysisRate > 0 {
		prices = append(prices, models.NormalizedPrice{
			Provider:      models.CloudProviderGCP,
			Service:       "analytics",
			SKUID:         fmt.Sprintf("gcp:bigquery:analysis:%s", region),
			ProductFamily: "BigQuery",
			Description:   fmt.Sprintf("BigQuery On-Demand Analysis in %s", region),
			Region:        region,
			Attributes:    map[string]string{"query_type": "on_demand"},
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  analysisRate,
			Unit:          models.PriceUnitPerUnit, // per TiB
			Currency:      "USD",
		})
	}
	if activeStorageRate > 0 {
		prices = append(prices, models.NormalizedPrice{
			Provider:      models.CloudProviderGCP,
			Service:       "analytics",
			SKUID:         fmt.Sprintf("gcp:bigquery:active_storage:%s", region),
			ProductFamily: "BigQuery",
			Description:   fmt.Sprintf("BigQuery Active Logical Storage in %s", region),
			Region:        region,
			Attributes:    map[string]string{"storage_type": "active"},
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  activeStorageRate,
			Unit:          models.PriceUnitPerGBMonth,
			Currency:      "USD",
		})
	}

	return annotateFresh(prices), result, nil
}

// bigQueryMultiRegion returns the multi-region prefix for a given GCP region.
func bigQueryMultiRegion(region string) string {
	switch {
	case strings.HasPrefix(region, "us-") || region == "us":
		return "us"
	case strings.HasPrefix(region, "europe-") || region == "eu":
		return "eu"
	case strings.HasPrefix(region, "asia-") || strings.HasPrefix(region, "australia-"):
		return "asia"
	}
	return ""
}

// --------------------------------------------------------------------------
// GetPrice dispatch for Part 2 AI/analytics domains
// --------------------------------------------------------------------------

// getAIPart2Price dispatches AI and analytics domain specs.
//
// Analytics specs are routed to priceAnalytics (defined in gcp_analytics.go),
// which adds a workload_total composite NormalizedPrice that estimate_bom
// consumes. AI specs are dispatched directly to the Vertex / Gemini helpers.
func (p *Provider) getAIPart2Price(ctx context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
	region := spec.GetRegion()
	if region == "" {
		region = p.DefaultRegion()
	}

	switch s := spec.(type) {
	case *models.AiPricingSpec:
		return p.dispatchAIPrice(ctx, s, region)

	case *models.AnalyticsPricingSpec:
		// Delegate to priceAnalytics (gcp_analytics.go) — it wraps getBigQueryPrice
		// and prepends the composite workload_total price for estimate_bom.
		prices, breakdown, err := p.priceAnalytics(ctx, s)
		if err != nil {
			return nil, err
		}
		return buildResult(prices, breakdown), nil
	}
	return nil, nil
}

func (p *Provider) dispatchAIPrice(ctx context.Context, s *models.AiPricingSpec, region string) (*models.PricingResult, error) {
	svc := strings.ToLower(s.Service)
	switch svc {
	case "gemini":
		prices, breakdown, err := p.getGeminiPrice(ctx, s.Model, region)
		if err != nil {
			return nil, err
		}
		return buildResult(prices, breakdown), nil

	default: // vertex, vertex_ai, training, prediction, ""
		prices, breakdown, err := p.priceVertexAI(ctx, s)
		if err != nil {
			return nil, err
		}
		return buildResult(prices, breakdown), nil
	}
}
