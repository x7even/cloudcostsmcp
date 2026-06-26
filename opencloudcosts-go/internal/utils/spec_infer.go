// Package utils provides shared utilities for cloud pricing providers.
// This file ports spec_infer.py: PricingSpec domain inference and error enrichment.
package utils

import (
	"fmt"
	"strings"
)

// serviceToDomain maps known service identifiers to their canonical PricingDomain value.
var serviceToDomain = map[string]string{
	// database
	"rds":         "database",
	"cloud_sql":   "database",
	"memorystore": "database",
	"sql":         "database",
	"cosmos":      "database",
	"elasticache": "database",
	// analytics
	"bigquery": "analytics",
	// network
	"cloud_nat": "network",
	"cloud_lb":  "network",
	"cloud_cdn": "network",
	"nat":       "network",
	"lb":        "network",
	"cdn":       "network",
	// observability
	"cloud_armor":      "network",
	"cloudwatch":       "observability",
	"cloud_monitoring": "observability",
	// ai
	"bedrock":   "ai",
	"gemini":    "ai",
	"vertex":    "ai",
	"openai":    "ai",
	"sagemaker": "ai",
	// serverless
	"lambda":          "serverless",
	"functions":       "serverless",
	"azure_functions": "serverless",
	"cloud_functions": "serverless",
	"cloud_run":       "serverless",
	// container
	"gke": "container",
	"eks": "container",
	"aks": "container",
	// egress / data transfer
	// "egress" is intentionally absent: it is a valid service in BOTH domain=network
	// (internet egress with tiered pricing via NetworkPricingSpec + destination_type=internet)
	// AND domain=inter_region_egress (flat per-region rates via EgressPricingSpec).
	// Including it here would override an explicitly supplied domain=network during FillDomain,
	// routing the get_price call to EgressPricingSpec instead of NetworkPricingSpec and
	// bypassing the internetEgressPrices() tiered-rate branch. All prompts that use
	// service=egress supply an explicit domain, so FillDomain returns early before this map
	// is consulted anyway.
	"data_transfer": "inter_region_egress",
}

// validTerms is shown in error hints when the model sends an invalid term.
const validTerms = ("on_demand, spot, reserved_1yr, reserved_1yr_partial, reserved_1yr_all, " +
	"reserved_3yr, reserved_3yr_partial, reserved_3yr_all, " +
	"cud_1yr, cud_3yr, flex_cud, sud, compute_savings_plan, ec2_instance_savings_plan")

// FillDomain returns a copy of spec with "domain" added if it can be inferred
// from "service", "storage_type", or "resource_type". If "domain" is already
// present the original map is returned unchanged (identity preserved).
func FillDomain(spec map[string]interface{}) map[string]interface{} {
	if _, ok := spec["domain"]; ok {
		return spec // already set — return same map (identity)
	}

	// 1. Service-keyed lookup (highest precision)
	service := strings.ToLower(fmt.Sprintf("%v", spec["service"]))
	if service != "" && service != "<nil>" {
		if domain, ok := serviceToDomain[service]; ok {
			return copyWith(spec, "domain", domain)
		}
	}

	// 2. storage_type present → storage
	if st, ok := spec["storage_type"]; ok && st != nil && fmt.Sprintf("%v", st) != "" {
		return copyWith(spec, "domain", "storage")
	}

	// 3. resource_type prefix patterns
	rt := strings.ToLower(fmt.Sprintf("%v", spec["resource_type"]))
	if rt != "" && rt != "<nil>" {
		if strings.HasPrefix(rt, "db.") || strings.HasPrefix(rt, "cache.") {
			return copyWith(spec, "domain", "database")
		}
		// Compute: AWS (contains dot, e.g. m5.xlarge), GCP (dash-separated),
		// Azure (starts with standard_ / basic_ / premium_)
		if strings.Contains(rt, ".") ||
			strings.Contains(rt, "-") ||
			strings.HasPrefix(rt, "standard_") ||
			strings.HasPrefix(rt, "basic_") ||
			strings.HasPrefix(rt, "premium_") {
			return copyWith(spec, "domain", "compute")
		}
	}

	return spec
}

// copyWith returns a shallow copy of m with key set to value.
func copyWith(m map[string]interface{}, key string, value interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out[key] = value
	return out
}

// SpecErrorResponse returns a structured invalid_spec error map with targeted hints.
// It mirrors spec_error_response() in spec_infer.py.
func SpecErrorResponse(err error, spec map[string]interface{}) map[string]interface{} {
	msg := err.Error()
	resp := map[string]interface{}{
		"error":  "invalid_spec",
		"reason": msg,
		"hint": "Call describe_catalog(provider, domain, service) to get a valid " +
			"example_invocation for your provider/domain/service combination.",
	}

	msgLower := strings.ToLower(msg)
	_, hasDomain := spec["domain"]

	if strings.Contains(msgLower, "unable to extract tag") ||
		(!hasDomain && strings.Contains(msgLower, "discriminator")) {
		resp["fix"] = ("The 'domain' field is required and must be one of: " +
			"compute, storage, database, ai, container, serverless, " +
			"analytics, network, observability, inter_region_egress")
	} else if strings.Contains(msg, ".term") && strings.Contains(msgLower, "input should be") {
		resp["fix"] = "Valid term values: " + validTerms
	} else if _, hasProvider := spec["provider"]; !hasProvider {
		resp["fix"] = "The 'provider' field is required: 'aws', 'gcp', or 'azure'."
	}

	return resp
}
