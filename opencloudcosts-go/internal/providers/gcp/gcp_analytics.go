// gcp_analytics.go — GCP analytics and observability pricing helpers.
//
// Covered domains:
//   - analytics/bigquery    : BigQuery on-demand analysis, active storage,
//     long-term storage, streaming inserts
//   - observability/cloud_monitoring: Custom metric ingestion (tiered MiB pricing)
//
// Note: BigQuery SKU fetching and index building is implemented in gcp_ai.go
// (buildBigQueryIndex / getBigQueryPrice). This file provides the _price_analytics
// and _price_observability dispatch wrappers consumed by GetPrice in gcp_catalog.go.
//
// All methods are on *Provider defined in gcp.go (Part 1).
package gcp

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// GCP service ID for Cloud Monitoring.
const cloudMonitoringServiceID = "58CD-E7C3-72CA"

// --------------------------------------------------------------------------
// BigQuery analytics pricing
// --------------------------------------------------------------------------

// priceAnalytics returns BigQuery pricing for the given AnalyticsPricingSpec.
//
// It delegates to getBigQueryPrice (defined in gcp_ai.go), which handles
// region fallback and index building. This wrapper produces the canonical
// NormalizedPrice slice and breakdown map expected by GetPrice.
func (p *Provider) priceAnalytics(
	ctx context.Context,
	spec *models.AnalyticsPricingSpec,
) ([]models.NormalizedPrice, map[string]any, error) {
	region := spec.Region
	if region == "" {
		region = "us"
	}

	// Delegate to getBigQueryPrice defined in gcp_ai.go.
	prices, breakdown, err := p.getBigQueryPrice(
		ctx,
		region,
		spec.QueryTB,
		spec.ActiveStorageGB,
		spec.LongtermStorageGB,
		spec.StreamingGB,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("gcp analytics: %w", err)
	}

	// Build composite workload_total price if multiple components are requested.
	var totalCost float64
	var components []string
	if ec, ok := breakdown["estimated_query_cost"].(float64); ok {
		totalCost += ec
		if spec.QueryTB != nil {
			components = append(components, fmt.Sprintf("%.3fTiB queries", *spec.QueryTB))
		}
	}
	if ec, ok := breakdown["estimated_active_storage_cost"].(float64); ok {
		totalCost += ec
		if spec.ActiveStorageGB != nil {
			components = append(components, fmt.Sprintf("%.0fGB active storage", *spec.ActiveStorageGB))
		}
	}
	if ec, ok := breakdown["estimated_longterm_storage_cost"].(float64); ok {
		totalCost += ec
		if spec.LongtermStorageGB != nil {
			components = append(components, fmt.Sprintf("%.0fGB long-term storage", *spec.LongtermStorageGB))
		}
	}
	if ec, ok := breakdown["estimated_streaming_cost"].(float64); ok {
		totalCost += ec
		if spec.StreamingGB != nil {
			components = append(components, fmt.Sprintf("%.0fGB streaming inserts", *spec.StreamingGB))
		}
	}

	if totalCost > 0 && len(components) > 0 {
		breakdown["monthly_total"] = totalCost
		// Prepend composite price so estimate_bom picks it up as the unit cost.
		composite := models.NormalizedPrice{
			Provider:      models.CloudProviderGCP,
			Service:       "analytics",
			SKUID:         fmt.Sprintf("gcp:bigquery:%s:workload_total", region),
			ProductFamily: "BigQuery",
			Description:   "BigQuery workload total: " + strings.Join(components, ", "),
			Region:        region,
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  totalCost,
			Unit:          models.PriceUnitPerUnit,
			Currency:      "USD",
			Attributes:    map[string]string{"billing_dimension": "workload_total"},
		}
		prices = append([]models.NormalizedPrice{composite}, prices...)
	}

	return annotateFresh(prices), breakdown, nil
}

// --------------------------------------------------------------------------
// Cloud Monitoring observability pricing
// --------------------------------------------------------------------------

// cloudMonitoringTier holds constants for Cloud Monitoring's three pricing tiers.
// All values are per MiB per month.
const (
	cloudMonitoringFreeTierMiB = 150.0
	cloudMonitoringT1Rate      = 0.258 // 0–100K MiB/month
	cloudMonitoringT2Rate      = 0.151 // 100K–250K MiB/month
	cloudMonitoringT3Rate      = 0.062 // >250K MiB/month
	cloudMonitoringT1Limit     = 100000.0
	cloudMonitoringT2Limit     = 250000.0
)

// priceObservability returns Cloud Monitoring pricing for the given spec.
//
// Cloud Monitoring uses tiered per-MiB ingestion pricing with a 150 MiB/month
// free tier. The live SKU rate is accepted only when >= $0.01/MiB to avoid
// mistaking a per-byte rate for a per-MiB rate.
func (p *Provider) priceObservability(
	ctx context.Context,
	spec *models.ObservabilityPricingSpec,
) ([]models.NormalizedPrice, map[string]any, error) {
	tier1Rate := cloudMonitoringT1Rate
	fallback := false
	var rejectedRate float64

	skus, err := p.fetchSKUs(ctx, cloudMonitoringServiceID)
	if err != nil {
		slog.Warn("gcp cloud_monitoring: fetch SKUs failed", "err", err)
		fallback = true
	} else {
		matched := false
		for _, sku := range skus {
			desc, _ := sku["description"].(string)
			descLower := strings.ToLower(desc)
			if !(strings.Contains(descLower, "monitoring") || strings.Contains(descLower, "workload metric")) {
				continue
			}
			if !(strings.Contains(descLower, "metric") || strings.Contains(descLower, "ingest")) {
				continue
			}
			price := skuPrice(sku)
			// Per-byte SKUs are ~$2.4e-7; per-MiB SKUs are >= $0.10.
			// Accept only values >= $0.01 as a plausible per-MiB rate.
			if price >= 0.01 {
				tier1Rate = price
				matched = true
				break
			} else if price > 0 {
				rejectedRate = price // per-byte rate — keep scanning
			}
		}
		if !matched {
			fallback = true
		}
	}

	region := spec.Region
	if region == "" {
		region = "global"
	}

	prices := []models.NormalizedPrice{
		{
			Provider:      models.CloudProviderGCP,
			Service:       "observability",
			SKUID:         "gcp:cloud_monitoring:ingestion",
			ProductFamily: "Cloud Monitoring",
			Description:   "Cloud Monitoring custom metric ingestion per MiB (tier 1: 0–100K MiB/mo)",
			Region:        region,
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  tier1Rate,
			Unit:          models.PriceUnitPerUnit,
			Currency:      "USD",
			Attributes: map[string]string{
				"billing_dimension": "per_mib",
				"tier2_rate":        fmt.Sprintf("%.3f", cloudMonitoringT2Rate),
				"tier3_rate":        fmt.Sprintf("%.3f", cloudMonitoringT3Rate),
				"free_tier_mib":     "150",
			},
		},
	}

	breakdown := map[string]any{
		"free_tier_mib":      cloudMonitoringFreeTierMiB,
		"tier1_rate_per_mib": fmt.Sprintf("$%.4f/MiB", tier1Rate),
		"tier2_rate_per_mib": fmt.Sprintf("$%.4f/MiB", cloudMonitoringT2Rate),
		"tier3_rate_per_mib": fmt.Sprintf("$%.4f/MiB", cloudMonitoringT3Rate),
	}

	if spec.IngestionMiB > 0 {
		total := spec.IngestionMiB
		billable := total - cloudMonitoringFreeTierMiB
		if billable < 0 {
			billable = 0
		}
		var cost float64
		rem := billable

		// Tier 1: 0 to 100K MiB
		t1 := rem
		if t1 > cloudMonitoringT1Limit {
			t1 = cloudMonitoringT1Limit
		}
		cost += t1 * tier1Rate
		rem -= t1

		// Tier 2: 100K to 250K MiB
		if rem > 0 {
			t2Limit := cloudMonitoringT2Limit - cloudMonitoringT1Limit
			t2 := rem
			if t2 > t2Limit {
				t2 = t2Limit
			}
			cost += t2 * cloudMonitoringT2Rate
			rem -= t2
		}

		// Tier 3: >250K MiB
		if rem > 0 {
			cost += rem * cloudMonitoringT3Rate
		}

		breakdown["estimated_monthly_cost"] = fmt.Sprintf("$%.4f/month", cost)
	}

	if rejectedRate > 0 && fallback {
		slog.Warn("GCP Cloud Monitoring: SKU rate is not in plausible per-MiB range; using published fallback",
			"rejected_rate", rejectedRate,
			"fallback_rate", cloudMonitoringT1Rate,
		)
		breakdown["note"] = fmt.Sprintf(
			"Live SKU found ($%.2e/unit, appears to be per-byte not per-MiB); using published fallback rates.",
			rejectedRate,
		)
	}
	if fallback {
		breakdown["fallback"] = true
	}
	return annotateFresh(prices), breakdown, nil
}
