// Package aws — FinOps methods for the AWS provider.
//
// This file implements the five FinOps interface methods that require live
// AWS credentials (Cost Explorer, Savings Plans, EC2 Reserved Instances):
//
//   - GetEffectivePrice  — Cost Explorer GetCostAndUsage, manual NextPageToken loop.
//   - GetSpotHistory     — EC2 DescribeSpotPriceHistoryPaginator.
//   - GetDiscountSummary — Savings Plans DescribeSavingsPlans (manual pagination) +
//     EC2 DescribeReservedInstances + CE utilisation queries.
//   - DescribeCatalog    — static manifest of AWS capabilities.
//   - BOMAdvisories      — advisory rows for common hidden cost lines.
//
// All methods are on *Provider (struct defined in aws.go).
package aws

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/savingsplans"
	sptypes "github.com/aws/aws-sdk-go-v2/service/savingsplans/types"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers"
)

// errCENotConfigured is returned when Cost Explorer auth is not configured.
// It wraps providers.ErrNotSupported so callers can use errors.Is.
var errCENotConfigured = fmt.Errorf(
	"effective pricing requires Cost Explorer, which is disabled — set "+
		"OCC_AWS_ENABLE_COST_EXPLORER=true to enable it (note: each API call "+
		"costs $0.01): %w",
	providers.ErrNotSupported,
)

// requireCE returns errCENotConfigured when ceClient is nil.
func (p *Provider) requireCE() error {
	if p.ceClient == nil {
		return errCENotConfigured
	}
	return nil
}

// serviceToCE maps internal service names to CE SERVICE dimension values.
var serviceToCE = map[string]string{
	"compute":  "Amazon Elastic Compute Cloud - Compute",
	"storage":  "Amazon Elastic Block Store",
	"database": "Amazon Relational Database Service",
	"s3":       "Amazon Simple Storage Service",
}

func lookupCEService(service string) string {
	if v, ok := serviceToCE[strings.ToLower(service)]; ok {
		return v
	}
	return service
}

// --------------------------------------------------------------------------
// GetEffectivePrice
// --------------------------------------------------------------------------

// GetEffectivePrice implements providers.Provider.
//
// Queries Cost Explorer GetCostAndUsage for the last full calendar month and
// returns the net amortised hourly rate for the compute instance described by
// spec. Only *models.ComputePricingSpec with a non-empty ResourceType is
// enriched; other spec types return ErrNotSupported.
//
// Note: GetCostAndUsage has NO SDK paginator helper in aws-sdk-go-v2.
// We loop on result.NextPageToken manually until it is nil.
func (p *Provider) GetEffectivePrice(ctx context.Context, spec models.PricingSpec) ([]models.EffectivePrice, error) {
	if err := p.requireCE(); err != nil {
		return nil, err
	}

	cs, ok := spec.(*models.ComputePricingSpec)
	if !ok || cs.ResourceType == "" {
		return nil, fmt.Errorf(
			"GetEffectivePrice: spec must be a *ComputePricingSpec with resource_type set: %w",
			providers.ErrNotSupported,
		)
	}

	// Last full calendar month.
	now := time.Now().UTC()
	end := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	start := end.AddDate(0, -1, 0)
	period := &cetypes.DateInterval{
		Start: aws.String(start.Format("2006-01-02")),
		End:   aws.String(end.Format("2006-01-02")),
	}

	svcName := lookupCEService(cs.GetService())

	filter := &cetypes.Expression{
		And: []cetypes.Expression{
			{Dimensions: &cetypes.DimensionValues{
				Key:    cetypes.DimensionService,
				Values: []string{svcName},
			}},
			{Dimensions: &cetypes.DimensionValues{
				Key:    cetypes.DimensionRegion,
				Values: []string{cs.GetRegion()},
			}},
			{Dimensions: &cetypes.DimensionValues{
				Key:    cetypes.DimensionInstanceType,
				Values: []string{cs.ResourceType},
			}},
		},
	}

	input := &costexplorer.GetCostAndUsageInput{
		TimePeriod:  period,
		Granularity: cetypes.GranularityMonthly,
		Metrics:     []string{"NetAmortizedCost", "UsageQuantity"},
		Filter:      filter,
	}

	// GetCostAndUsage has no paginator helper — loop manually.
	var resultsByTime []cetypes.ResultByTime
	for {
		out, err := p.ceClient.GetCostAndUsage(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("aws GetEffectivePrice: GetCostAndUsage: %w", err)
		}
		resultsByTime = append(resultsByTime, out.ResultsByTime...)
		if out.NextPageToken == nil || *out.NextPageToken == "" {
			break
		}
		input.NextPageToken = out.NextPageToken
	}

	// Fetch the on-demand base price so we can compute discount_pct.
	basePrices, _ := p.GetComputePrice(ctx, cs.ResourceType, cs.GetRegion(), cs.OS, models.PricingTermOnDemand)

	var effective []models.EffectivePrice
	for _, rbt := range resultsByTime {
		netCostStr := ""
		if mv, ok := rbt.Total["NetAmortizedCost"]; ok && mv.Amount != nil {
			netCostStr = *mv.Amount
		}
		usageStr := "1"
		if mv, ok := rbt.Total["UsageQuantity"]; ok && mv.Amount != nil && *mv.Amount != "" {
			usageStr = *mv.Amount
		}

		netCost, err := strconv.ParseFloat(netCostStr, 64)
		if err != nil || netCost == 0 {
			continue
		}
		usage, err := strconv.ParseFloat(usageStr, 64)
		if err != nil || usage == 0 {
			usage = 1
		}

		effectiveRate := netCost / usage

		var base models.NormalizedPrice
		discountPct := 0.0
		if len(basePrices) > 0 {
			base = basePrices[0]
			if base.PricePerUnit > 0 {
				raw := (base.PricePerUnit - effectiveRate) / base.PricePerUnit * 100
				discountPct = math.Round(raw*100) / 100
			}
		} else {
			base = models.NormalizedPrice{
				Provider:    models.CloudProviderAWS,
				Service:     cs.GetService(),
				Region:      cs.GetRegion(),
				PricingTerm: models.PricingTermOnDemand,
				Description: cs.ResourceType,
			}
		}

		effective = append(effective, models.EffectivePrice{
			BasePrice:             base,
			EffectivePricePerUnit: effectiveRate,
			DiscountType:          "Blended (RI/SP/EDP)",
			DiscountPct:           discountPct,
			Source:                "cost_explorer",
		})
	}
	return effective, nil
}

// --------------------------------------------------------------------------
// GetSpotHistory
// --------------------------------------------------------------------------

// GetSpotHistory implements providers.Provider.
//
// Returns historical spot price statistics for the EC2 instance type described
// by spec (must be *models.ComputePricingSpec). The lookback window is capped
// at 720 hours per AWS API limits.
// Uses DescribeSpotPriceHistoryPaginator (SDK-managed pagination).
func (p *Provider) GetSpotHistory(
	ctx context.Context,
	spec models.PricingSpec,
	hours int,
	availabilityZone string,
) (map[string]any, error) {
	cs, ok := spec.(*models.ComputePricingSpec)
	if !ok || cs.ResourceType == "" {
		return nil, fmt.Errorf(
			"GetSpotHistory: spec must be a *ComputePricingSpec with resource_type set: %w",
			providers.ErrNotSupported,
		)
	}
	if p.ec2Client == nil {
		return nil, fmt.Errorf("GetSpotHistory: EC2 client not initialised: %w", providers.ErrNotSupported)
	}

	if hours <= 0 {
		hours = 24
	}
	if hours > 720 {
		hours = 720
	}

	startTime := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)

	osStr := cs.OS
	if osStr == "" {
		osStr = "Linux"
	}
	productDesc := "Linux/UNIX"
	if strings.EqualFold(osStr, "Windows") {
		productDesc = "Windows"
	}

	spotInput := &awsec2.DescribeSpotPriceHistoryInput{
		InstanceTypes:       []ec2types.InstanceType{ec2types.InstanceType(cs.ResourceType)},
		ProductDescriptions: []string{productDesc},
		StartTime:           &startTime,
	}
	if availabilityZone != "" {
		spotInput.AvailabilityZone = &availabilityZone
	}

	paginator := awsec2.NewDescribeSpotPriceHistoryPaginator(p.ec2Client, spotInput)

	type pricePoint struct {
		price float64
		az    string
	}
	var allPoints []pricePoint
	byAZ := map[string][]float64{}

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("GetSpotHistory: DescribeSpotPriceHistory: %w", err)
		}
		for _, sp := range page.SpotPriceHistory {
			az := ""
			if sp.AvailabilityZone != nil {
				az = *sp.AvailabilityZone
			}
			priceStr := ""
			if sp.SpotPrice != nil {
				priceStr = *sp.SpotPrice
			}
			price, err := strconv.ParseFloat(priceStr, 64)
			if err != nil || price == 0 {
				continue
			}
			allPoints = append(allPoints, pricePoint{price: price, az: az})
			byAZ[az] = append(byAZ[az], price)
		}
	}

	if len(allPoints) == 0 {
		return map[string]any{}, nil
	}

	// Compute overall stats.
	minP := allPoints[0].price
	maxP := allPoints[0].price
	sumP := 0.0
	for _, pt := range allPoints {
		if pt.price < minP {
			minP = pt.price
		}
		if pt.price > maxP {
			maxP = pt.price
		}
		sumP += pt.price
	}
	n := float64(len(allPoints))
	avgP := sumP / n

	// Volatility = stddev / mean.
	sumSq := 0.0
	for _, pt := range allPoints {
		diff := pt.price - avgP
		sumSq += diff * diff
	}
	stddev := 0.0
	if n > 1 {
		stddev = math.Sqrt(sumSq / n)
	}
	volatilityRatio := 0.0
	if avgP > 0 {
		volatilityRatio = stddev / avgP
	}

	stability := "stable"
	recommendation := "Low interruption risk. Good candidate for fault-tolerant batch workloads."
	switch {
	case volatilityRatio >= 0.15:
		stability = "volatile"
		recommendation = "High volatility. Consider on-demand or reserved instances for reliable workloads."
	case volatilityRatio >= 0.05:
		stability = "moderate"
		recommendation = "Moderate price variation. Use with checkpointing for long-running jobs."
	}

	// Per-AZ stats.
	azStats := map[string]any{}
	for az, prices := range byAZ {
		azMin := prices[0]
		azMax := prices[0]
		azSum := 0.0
		for _, price := range prices {
			if price < azMin {
				azMin = price
			}
			if price > azMax {
				azMax = price
			}
			azSum += price
		}
		azAvg := azSum / float64(len(prices))
		azStats[az] = map[string]any{
			"current":      fmt.Sprintf("$%.6f", prices[0]), // API returns newest first
			"min":          fmt.Sprintf("$%.6f", azMin),
			"max":          fmt.Sprintf("$%.6f", azMax),
			"avg":          fmt.Sprintf("$%.6f", azAvg),
			"sample_count": len(prices),
		}
	}

	return map[string]any{
		"instance_type":    cs.ResourceType,
		"region":           cs.GetRegion(),
		"os":               osStr,
		"lookback_hours":   hours,
		"stability":        stability,
		"volatility_ratio": fmt.Sprintf("%.4f", volatilityRatio),
		"overall": map[string]any{
			"min":          fmt.Sprintf("$%.6f", minP),
			"max":          fmt.Sprintf("$%.6f", maxP),
			"avg":          fmt.Sprintf("$%.6f", avgP),
			"sample_count": len(allPoints),
		},
		"by_availability_zone": azStats,
		"recommendation":       recommendation,
	}, nil
}

// --------------------------------------------------------------------------
// GetDiscountSummary
// --------------------------------------------------------------------------

// GetDiscountSummary implements providers.Provider.
//
// Returns a structured map containing:
//   - "savings_plans":      active Savings Plans (DescribeSavingsPlans, manual pagination).
//   - "reserved_instances": active EC2 Reserved Instances (state=active).
//   - "utilisation":        CE utilisation summary for SP and RI for the last full month.
//   - "sp_count" / "ri_count": convenience counts.
func (p *Provider) GetDiscountSummary(ctx context.Context) (map[string]any, error) {
	if err := p.requireCE(); err != nil {
		return nil, err
	}

	spSummary, err := p.activeSavingsPlans(ctx)
	if err != nil {
		return nil, fmt.Errorf("GetDiscountSummary: savings plans: %w", err)
	}

	riSummary, err := p.activeReservedInstances(ctx)
	if err != nil {
		return nil, fmt.Errorf("GetDiscountSummary: reserved instances: %w", err)
	}

	utilisation, err := p.ceUtilisation(ctx)
	if err != nil {
		// Non-fatal — include the error string in the response.
		utilisation = map[string]any{"error": err.Error()}
	}

	return map[string]any{
		"savings_plans":      spSummary,
		"reserved_instances": riSummary,
		"utilisation":        utilisation,
		"sp_count":           len(spSummary),
		"ri_count":           len(riSummary),
	}, nil
}

// activeSavingsPlans pages through DescribeSavingsPlans with state=active.
// The Savings Plans API uses manual NextToken pagination (no SDK paginator).
func (p *Provider) activeSavingsPlans(ctx context.Context) ([]map[string]any, error) {
	if p.spClient == nil {
		return nil, fmt.Errorf("savings plans client not initialised: %w", providers.ErrNotSupported)
	}

	var result []map[string]any
	var nextToken *string

	for {
		out, err := p.spClient.DescribeSavingsPlans(ctx, &savingsplans.DescribeSavingsPlansInput{
			States:    []sptypes.SavingsPlanState{sptypes.SavingsPlanStateActive},
			NextToken: nextToken,
		})
		if err != nil {
			return nil, err
		}

		for _, sp := range out.SavingsPlans {
			termYears := "1"
			if sp.TermDurationInSeconds > 94_000_000 {
				termYears = "3"
			}
			row := map[string]any{
				"id":                      derefStr(sp.SavingsPlanId),
				"type":                    string(sp.SavingsPlanType),
				"payment_option":          string(sp.PaymentOption),
				"commitment_usd_per_hour": derefStr(sp.Commitment),
				"term_years":              termYears,
				"start":                   derefStr(sp.Start),
				"end":                     derefStr(sp.End),
				"state":                   string(sp.State),
			}
			result = append(result, row)
		}

		if out.NextToken == nil || *out.NextToken == "" {
			break
		}
		nextToken = out.NextToken
	}
	return result, nil
}

// activeReservedInstances calls DescribeReservedInstances with state=active.
func (p *Provider) activeReservedInstances(ctx context.Context) ([]map[string]any, error) {
	if p.ec2Client == nil {
		return nil, fmt.Errorf("EC2 client not initialised: %w", providers.ErrNotSupported)
	}

	out, err := p.ec2Client.DescribeReservedInstances(ctx, &awsec2.DescribeReservedInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("state"), Values: []string{"active"}},
		},
	})
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	region := ""
	if p.cfg != nil {
		region = p.cfg.AWSRegion
	}

	var result []map[string]any
	for _, ri := range out.ReservedInstances {
		daysRemaining := 0
		if ri.End != nil && ri.End.After(now) {
			daysRemaining = int(ri.End.Sub(now).Hours() / 24)
		}

		termYears := "1"
		if ri.Duration != nil && *ri.Duration > 94_000_000 {
			termYears = "3"
		}

		count := 0
		if ri.InstanceCount != nil {
			count = int(*ri.InstanceCount)
		}
		fixedPrice := 0.0
		if ri.FixedPrice != nil {
			fixedPrice = float64(*ri.FixedPrice)
		}
		usagePrice := 0.0
		if ri.UsagePrice != nil {
			usagePrice = float64(*ri.UsagePrice)
		}

		result = append(result, map[string]any{
			"instance_type":       string(ri.InstanceType),
			"region":              region,
			"count":               count,
			"offering_type":       string(ri.OfferingType),
			"duration_years":      termYears,
			"days_remaining":      daysRemaining,
			"fixed_price":         fixedPrice,
			"usage_price":         usagePrice,
			"product_description": string(ri.ProductDescription),
			"state":               string(ri.State),
		})
	}
	return result, nil
}

// ceUtilisation fetches CE Savings Plans + RI utilisation for the last full calendar month.
func (p *Provider) ceUtilisation(ctx context.Context) (map[string]any, error) {
	now := time.Now().UTC()
	endDate := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	startDate := endDate.AddDate(0, -1, 0)
	period := &cetypes.DateInterval{
		Start: aws.String(startDate.Format("2006-01-02")),
		End:   aws.String(endDate.Format("2006-01-02")),
	}

	result := map[string]any{}

	// Savings Plans utilisation.
	spOut, spErr := p.ceClient.GetSavingsPlansUtilization(ctx, &costexplorer.GetSavingsPlansUtilizationInput{
		TimePeriod: period,
	})
	if spErr == nil && spOut.Total != nil {
		netSavings := ""
		if spOut.Total.Savings != nil {
			netSavings = derefStr(spOut.Total.Savings.NetSavings)
		}
		totalCommit := ""
		unusedCommit := ""
		utilizationPct := ""
		if u := spOut.Total.Utilization; u != nil {
			totalCommit = derefStr(u.TotalCommitment)
			unusedCommit = derefStr(u.UnusedCommitment)
			utilizationPct = derefStr(u.UtilizationPercentage)
		}
		result["savings_plans"] = map[string]any{
			"total_commitment":  totalCommit,
			"unused_commitment": unusedCommit,
			"utilization_pct":   utilizationPct,
			"net_savings":       netSavings,
		}
	} else if spErr != nil {
		result["savings_plans_error"] = spErr.Error()
	}

	// Reserved Instances utilisation.
	riOut, riErr := p.ceClient.GetReservationUtilization(ctx, &costexplorer.GetReservationUtilizationInput{
		TimePeriod:  period,
		Granularity: cetypes.GranularityMonthly,
	})
	if riErr == nil && riOut.Total != nil {
		t := riOut.Total
		result["reserved_instances"] = map[string]any{
			"utilization_pct":            derefStr(t.UtilizationPercentage),
			"on_demand_cost_of_ri_hours": derefStr(t.OnDemandCostOfRIHoursUsed),
			"net_ri_savings":             derefStr(t.NetRISavings),
			"purchased_hours":            derefStr(t.PurchasedHours),
		}
	} else if riErr != nil {
		result["reserved_instances_error"] = riErr.Error()
	}

	return result, nil
}

// --------------------------------------------------------------------------
// DescribeCatalog
// --------------------------------------------------------------------------

// DescribeCatalog implements providers.Provider.
//
// Returns a static *models.ProviderCatalog describing all AWS domains, services,
// supported terms, filter hints, and example invocations. This is the LLM's
// O(1) discovery tool — it is a pure static response, never fetches data.
func (p *Provider) DescribeCatalog(ctx context.Context) (*models.ProviderCatalog, error) {
	return &models.ProviderCatalog{
		Provider: models.CloudProviderAWS,
		Domains: []models.PricingDomain{
			models.PricingDomainCompute,
			models.PricingDomainStorage,
			models.PricingDomainDatabase,
			models.PricingDomainAI,
			models.PricingDomainServerless,
			models.PricingDomainAnalytics,
			models.PricingDomainNetwork,
			models.PricingDomainObservability,
			models.PricingDomainContainer,
			models.PricingDomainInterRegionEgress,
		},
		Services: map[string][]string{
			"compute":             {"ec2", "fargate"},
			"storage":             {"ebs", "s3"},
			"database":            {"rds", "elasticache", "aurora_postgresql"},
			"ai":                  {"bedrock", "sagemaker"},
			"serverless":          {"lambda"},
			"analytics":           {"redshift", "athena"},
			"network":             {"lb", "cdn", "nat", "waf", "data_transfer", "egress"},
			"observability":       {"cloudwatch"},
			"container":           {"eks"},
			"inter_region_egress": {},
		},
		SupportedTerms: map[string][]string{
			"compute": {
				string(models.PricingTermOnDemand),
				string(models.PricingTermSpot),
				string(models.PricingTermReserved1Yr),
				string(models.PricingTermReserved1YrPartial),
				string(models.PricingTermReserved1YrAll),
				string(models.PricingTermReserved3Yr),
				string(models.PricingTermReserved3YrPartial),
				string(models.PricingTermReserved3YrAll),
				string(models.PricingTermComputeSP),
				string(models.PricingTermEC2InstanceSP),
			},
			"database": {
				string(models.PricingTermOnDemand),
				string(models.PricingTermReserved1Yr),
				string(models.PricingTermReserved3Yr),
				string(models.PricingTermReserved1YrPartial),
				string(models.PricingTermReserved1YrAll),
			},
			"ai/bedrock": {
				string(models.PricingTermOnDemand),
				string(models.PricingTermSavingsPlan),
			},
		},
		FilterHints: map[string]map[string]any{
			"compute": {
				"resource_type":    "EC2 instance type, e.g. 'm5.xlarge'",
				"os":               "'Linux' or 'Windows'",
				"term":             "pricing term; for 'compute_savings_plan' or 'ec2_instance_savings_plan' also pass commitment_years",
				"commitment_years": "1 (default) or 3 — applies only to compute_savings_plan and ec2_instance_savings_plan terms",
			},
			"compute/savings_plan": {
				"resource_type":    "EC2 instance type, e.g. 'm5.xlarge'",
				"os":               "'Linux' (default) | 'Windows' | 'RHEL' | 'SUSE'",
				"term":             "'compute_savings_plan' (CSP) or 'ec2_instance_savings_plan' (ISP)",
				"payment_option":   "'No Upfront' (default) | 'Partial Upfront' | 'All Upfront'. All Upfront is paid entirely upfront; the returned hourly rate will be $0.00 (no ongoing charge).",
				"commitment_years": "1 (default) or 3. WARNING: 3yr commitments have a ratchet clause — commitment cannot decrease year-over-year; choose 3yr only when utilisation is stable.",
				"edp_discount_pct": "optional: your EDP/PPA discount as a fraction 0.0–1.0 (e.g. 0.15 for 15%). EDP is a confidential negotiated rate — supply only if you have a PPA/EDP contract. EDP eligibility: ~$1M+ annual spend, AWS Enterprise Support required, contact your AWS account team or TAM to negotiate.",
				"note":             "CSP applies to EC2+Fargate+Lambda; ISP is EC2-family-specific and cheaper for same family. Data sourced from AWS SP bulk pricing API (live rates). All payment options (No Upfront, Partial Upfront, All Upfront) are indexed.",
			},
			"compute/fargate": {
				"vcpu":      "vCPU count (0.25–16)",
				"memory_gb": "memory in GB",
				"os":        "'Linux' or 'Windows'",
			},
			"storage": {
				"storage_type": "'gp3', 'io1', 'st1', 'sc1', 's3-standard'",
				"size_gb":      "size for monthly estimate",
			},
			"database/rds": {
				"resource_type": "DB instance type e.g. 'db.r5.large'",
				"engine":        "MySQL/PostgreSQL/MariaDB/Oracle/SQLServer",
				"deployment":    "'single-az' or 'multi-az'",
			},
			"database/elasticache": {
				"resource_type": "cache.r6g.large",
				"service":       "elasticache",
			},
			"database/aurora_postgresql": {
				"service":       "'rds' (Aurora uses the RDS service path)",
				"engine":        "'aurora-postgresql' or 'aurora-mysql'",
				"resource_type": "DB instance type e.g. 'db.r6g.large', 'db.r6g.2xlarge'",
				"deployment":    "'single-az' (Aurora manages its own HA across 3 AZs; do not use 'multi-az')",
				"note":          "Aurora storage, I/O requests, and backup pricing are not in this catalog. Aurora Standard storage: $0.10/GB-month; I/O-Optimized: $0.225/GB-month; I/O requests (Standard only): $0.20/million; backup beyond 1-day free: $0.021/GB-month.",
			},
			"ai/bedrock": {
				"model": "e.g. 'claude-3-5-sonnet', 'nova-pro', 'llama-3-1-70b'",
				"mode":  "'on_demand' or 'batch'",
			},
			"ai/sagemaker": {
				"machine_type": "ml instance type e.g. 'ml.g5.xlarge'",
			},
			"serverless/lambda": {
				"service":             "lambda",
				"gb_seconds":          "compute time in GB-seconds per month (memory_gb × duration_seconds × invocations); omit for raw rate",
				"requests_millions":   "number of invocations in millions per month; omit for raw rate",
				"region":              "AWS region e.g. 'us-east-1'",
			},
			"analytics/redshift":    {"service": "redshift"},
			"analytics/athena":      {"service": "athena"},
			"network/lb":            {"service": "lb", "note": "also accepts 'cloud_lb'"},
			"network/cdn": {
				"service":           "cdn",
				"data_gb_per_month": "monthly data transfer volume in GB (used for blended-rate calculation)",
				"region":            "origin region e.g. 'us-east-1'",
			},
			"network/nat":           {"service": "nat", "note": "also accepts 'cloud_nat'"},
			"network/data_transfer": {"service": "data_transfer"},
			"network/egress": {
				"source_region":      "origin region e.g. 'us-east-1'",
				"destination_type":   "internet | cross_region | cross_az | cross_continent",
				"destination_region": "target region for cross_region (optional; empty = internet)",
				"data_gb_per_month":  "monthly data volume in GB for blended-rate estimate",
			},
			"observability/cloudwatch": {"service": "cloudwatch"},
			"container/eks":            {"service": "eks"},
			"inter_region_egress": {
				"source_region": "origin region e.g. 'us-east-1'",
				"dest_region":   "destination region e.g. 'eu-west-1'; empty = internet egress",
			},
		},
		ExampleInvocations: map[string]map[string]any{
			"compute": {
				"provider":      "aws",
				"domain":        "compute",
				"resource_type": "m5.xlarge",
				"region":        "us-east-1",
				"os":            "Linux",
				"term":          "on_demand",
			},
			"compute/csp": {
				"provider":         "aws",
				"domain":           "compute",
				"resource_type":    "m5.xlarge",
				"region":           "us-east-1",
				"os":               "Linux",
				"term":             "compute_savings_plan",
				"commitment_years": 1,
				"payment_option":   "No Upfront",
			},
			"compute/csp/edp": {
				"provider":         "aws",
				"domain":           "compute",
				"resource_type":    "m5.xlarge",
				"region":           "us-east-1",
				"os":               "Linux",
				"term":             "compute_savings_plan",
				"commitment_years": 1,
				"payment_option":   "No Upfront",
				"edp_discount_pct": 0.12,
			},
			"compute/isp": {
				"provider":         "aws",
				"domain":           "compute",
				"resource_type":    "m5.xlarge",
				"region":           "us-east-1",
				"os":               "Linux",
				"term":             "ec2_instance_savings_plan",
				"commitment_years": 1,
				"payment_option":   "No Upfront",
			},
			"compute/fargate": {
				"provider":  "aws",
				"domain":    "compute",
				"service":   "fargate",
				"vcpu":      2.0,
				"memory_gb": 4.0,
				"region":    "us-east-1",
			},
			"storage": {
				"provider":     "aws",
				"domain":       "storage",
				"storage_type": "gp3",
				"region":       "us-east-1",
				"size_gb":      100,
			},
			"database/rds": {
				"provider":      "aws",
				"domain":        "database",
				"service":       "rds",
				"resource_type": "db.r5.large",
				"engine":        "MySQL",
				"deployment":    "single-az",
				"region":        "us-east-1",
			},
			"database/aurora_postgresql": {
				"provider":      "aws",
				"domain":        "database",
				"service":       "rds",
				"engine":        "aurora-postgresql",
				"resource_type": "db.r6g.large",
				"deployment":    "single-az",
				"region":        "us-east-1",
				"term":          "on_demand",
			},
			"ai/bedrock": {
				"provider":      "aws",
				"domain":        "ai",
				"service":       "bedrock",
				"model":         "claude-3-5-sonnet",
				"region":        "us-east-1",
				"input_tokens":  1_000_000,
				"output_tokens": 1_000_000,
			},
			"serverless/lambda": {
				"provider":            "aws",
				"domain":              "serverless",
				"service":             "lambda",
				"region":              "us-east-1",
				"gb_seconds":          100.0,
				"requests_millions":   10.0,
			},
			"observability/cloudwatch": {
				"provider": "aws",
				"domain":   "observability",
				"service":  "cloudwatch",
				"region":   "us-east-1",
			},
			"container/eks": {
				"provider": "aws",
				"domain":   "container",
				"service":  "eks",
				"region":   "us-east-1",
			},
			"inter_region_egress": {
				"provider":      "aws",
				"domain":        "inter_region_egress",
				"source_region": "us-east-1",
				"dest_region":   "eu-west-1",
			},
			"network/egress": {
				"provider":          "aws",
				"domain":            "network",
				"service":           "egress",
				"source_region":     "us-east-1",
				"destination_type":  "internet",
				"data_gb_per_month": 1024.0,
			},
			"network/egress/cross_region": {
				"provider":           "aws",
				"domain":             "network",
				"service":            "egress",
				"source_region":      "us-east-1",
				"destination_type":   "cross_region",
				"destination_region": "eu-west-1",
				"data_gb_per_month":  1024.0,
			},
			"network/cdn": {
				"provider":          "aws",
				"domain":            "network",
				"service":           "cdn",
				"data_gb_per_month": 1000.0,
				"region":            "us-east-1",
			},
		},
		DecisionMatrix: map[string]string{
			"ECS on Fargate":                          "compute/fargate — use vcpu + memory_gb params",
			"ECS tasks (Fargate launch type)":         "compute/fargate",
			"EC2 instances":                           "compute/ec2",
			"Compute Savings Plan (CSP)":              "compute with term=compute_savings_plan — covers EC2+Fargate+Lambda; less discount than ISP but more flexible (not family-locked). Use commitment_years=1 or 3, payment_option='No Upfront'|'Partial Upfront'|'All Upfront'",
			"EC2 Instance Savings Plan (ISP)":         "compute with term=ec2_instance_savings_plan — EC2-family-specific (e.g. m5 family in us-east-1); deeper discount than CSP (~37% vs ~27% for 1yr). Requires resource_type=specific instance type for rate lookup",
			"Savings Plans vs Reserved Instances":     "SP=flexible across family/size/region (CSP) or family/size (ISP); RI=locked to exact instance type+region. SP is generally preferred unless you need cross-region portability guarantees",
			"1yr vs 3yr commitment risk":              "3-year SP/RI commitments include a ratchet clause: the commitment amount cannot decrease year-over-year during the term. If your workload shrinks or migrates away from AWS, you continue paying the full commitment, resulting in wasted spend. Recommended: start with 1yr to prove utilisation; upgrade to 3yr only when growth trajectory is stable and confident.",
			"EDP (Enterprise Discount Program)":       "EDP requires ~$1M+ annual AWS spend commitment (multi-year), enrollment in AWS Enterprise Support, and a PPA (Private Pricing Agreement) negotiated through your AWS account team or TAM. EDP is not self-service — contact your AWS account team to qualify. EDP discounts stack on top of SP/RI savings (unless your contract uses SP bundling, which embeds EDP into SP rates).",
			"Lambda functions":                        "serverless/lambda",
			"EBS volumes":                             "storage/ebs",
			"S3 buckets":                              "storage/s3",
			"RDS instances":                           "database/rds",
			"ElastiCache clusters":                    "database/elasticache",
			"Bedrock inference":                       "ai/bedrock",
			"SageMaker endpoints":                     "ai/sagemaker",
			"EKS clusters":                            "container/eks",
			"CloudWatch metrics":                      "observability/cloudwatch",
			"Application Load Balancer":               "network/lb (also: service='cloud_lb')",
			"NAT Gateway":                             "network/nat (also: service='cloud_nat')",
			"CloudFront CDN":                          "network/cdn — AWS-ONLY; for GCP Cloud CDN use provider='gcp', service='cloud_cdn'",
			"AWS WAF":                                 "network/waf",
			"Data transfer / egress":                  "network/data_transfer",
			"Inter-region data transfer":              "inter_region_egress — use source_region + dest_region",
			"Internet egress with tier breakdown":     "network/egress — set destination_type=internet + data_gb_per_month",
			"Cross-region transfer with blended cost": "network/egress — set destination_type=cross_region + destination_region",
			"Cross-AZ traffic cost":                   "network/egress — set destination_type=cross_az",
		},
	}, nil
}

// --------------------------------------------------------------------------
// BOMAdvisories
// --------------------------------------------------------------------------

// BOMAdvisories implements providers.Provider.
//
// Returns advisory rows for cost lines commonly omitted from BoMs:
// egress, load balancer, NAT gateway, CloudWatch, RDS backups, EBS snapshots.
//
// The "price" field directs the caller to invoke get_price before including
// the figure — it is never pre-fetched here.
func (p *Provider) BOMAdvisories(ctx context.Context, services []string, sampleRegion string) ([]map[string]string, error) {
	if sampleRegion == "" {
		sampleRegion = "us-east-1"
	}

	svcSet := make(map[string]bool, len(services))
	for _, s := range services {
		svcSet[strings.ToLower(s)] = true
	}

	hasCompute := svcSet["compute"] || svcSet["ec2"] || svcSet["fargate"]
	hasDatabase := svcSet["database"] || svcSet["rds"] || svcSet["elasticache"]
	hasStorage := svcSet["storage"] || svcSet["ebs"] || svcSet["s3"]

	const notFetched = "NOT FETCHED — call get_price using the how_to_price command before including this in your answer"

	var advisories []map[string]string

	if hasCompute || hasDatabase {
		advisories = append(advisories,
			map[string]string{
				"item": "Data transfer (egress)",
				"why":  "Outbound traffic to the internet or cross-region — varies by workload",
				"how_to_price": fmt.Sprintf(
					`get_price(spec={"provider":"aws","domain":"network","service":"data_transfer","region":%q})`,
					sampleRegion,
				),
				"price": notFetched,
			},
			map[string]string{
				"item": "Load balancer (ALB/NLB)",
				"why":  "Typically needed in front of compute clusters",
				"how_to_price": fmt.Sprintf(
					`get_price(spec={"provider":"aws","domain":"network","service":"lb","region":%q})`,
					sampleRegion,
				),
				"price": notFetched,
			},
			map[string]string{
				"item": "NAT Gateway",
				"why":  "Required if EC2 instances are in private subnets",
				"how_to_price": fmt.Sprintf(
					`get_price(spec={"provider":"aws","domain":"network","service":"nat","region":%q})`,
					sampleRegion,
				),
				"price": notFetched,
			},
		)
	}

	advisories = append(advisories, map[string]string{
		"item": "CloudWatch monitoring",
		"why":  "Logs, metrics, alarms — scales with number of instances and log volume",
		"how_to_price": fmt.Sprintf(
			`get_price(spec={"provider":"aws","domain":"observability","service":"cloudwatch","region":%q})`,
			sampleRegion,
		),
		"price": "unknown — use the how_to_price call above to get the real figure",
	})

	if hasDatabase {
		advisories = append(advisories, map[string]string{
			"item":  "RDS automated backups",
			"why":   "Free for storage equal to DB size; extra storage charged beyond that",
			"price": "~$0.095/GB-month in us-east-1 — standard AWS published rate; no catalog lookup available for this item",
		})
	}

	if hasStorage {
		advisories = append(advisories, map[string]string{
			"item":  "EBS snapshots",
			"why":   "Point-in-time backups stored in S3 — charged per GB-month",
			"price": "~$0.05/GB-month in us-east-1 — standard AWS published rate; no catalog lookup available for this item",
		})
	}

	return advisories, nil
}

// --------------------------------------------------------------------------
// Internal helpers
// --------------------------------------------------------------------------

// derefStr safely dereferences a *string, returning "" for nil.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

