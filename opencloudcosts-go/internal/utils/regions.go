// Package utils provides shared utilities for cloud pricing providers.
// This file ports regions.py: region code <-> display name mappings for all
// three cloud providers, plus normalization and listing helpers.
package utils

import (
	"fmt"
	"sort"
)

// AWSRegionDisplay maps AWS region codes to the display names used by the Pricing API.
var AWSRegionDisplay = map[string]string{
	// North America
	"us-east-1":    "US East (N. Virginia)",
	"us-east-2":    "US East (Ohio)",
	"us-west-1":    "US West (N. California)",
	"us-west-2":    "US West (Oregon)",
	"ca-central-1": "Canada (Central)",
	"ca-west-1":    "Canada West (Calgary)",
	"mx-central-1": "Mexico (Central)",
	// South America
	"sa-east-1": "South America (Sao Paulo)",
	// Europe
	"eu-west-1":    "Europe (Ireland)",
	"eu-west-2":    "Europe (London)",
	"eu-west-3":    "Europe (Paris)",
	"eu-central-1": "EU (Frankfurt)",
	"eu-central-2": "Europe (Zurich)",
	"eu-north-1":   "Europe (Stockholm)",
	"eu-south-1":   "Europe (Milan)",
	"eu-south-2":   "Europe (Spain)",
	// Asia Pacific
	"ap-east-1":      "Asia Pacific (Hong Kong)",
	"ap-south-1":     "Asia Pacific (Mumbai)",
	"ap-south-2":     "Asia Pacific (Hyderabad)",
	"ap-northeast-1": "Asia Pacific (Tokyo)",
	"ap-northeast-2": "Asia Pacific (Seoul)",
	"ap-northeast-3": "Asia Pacific (Osaka)",
	"ap-southeast-1": "Asia Pacific (Singapore)",
	"ap-southeast-2": "Asia Pacific (Sydney)",
	"ap-southeast-3": "Asia Pacific (Jakarta)",
	"ap-southeast-4": "Asia Pacific (Melbourne)",
	"ap-southeast-5": "Asia Pacific (Malaysia)",
	// Middle East & Africa
	"me-south-1":   "Middle East (Bahrain)",
	"me-central-1": "Middle East (UAE)",
	"af-south-1":   "Africa (Cape Town)",
	"il-central-1": "Israel (Tel Aviv)",
	// GovCloud
	"us-gov-east-1": "AWS GovCloud (US-East)",
	"us-gov-west-1": "AWS GovCloud (US)",
}

// awsDisplayRegion is the reverse: display name → region code.
var awsDisplayRegion map[string]string

// GCPRegionDisplay maps GCP region codes to human-readable display names.
var GCPRegionDisplay = map[string]string{
	// Americas
	"us-east1":                "US East (South Carolina)",
	"us-east4":                "US East (Northern Virginia)",
	"us-east5":                "US East (Columbus)",
	"us-central1":             "US Central (Iowa)",
	"us-west1":                "US West (Oregon)",
	"us-west2":                "US West (Los Angeles)",
	"us-west3":                "US West (Salt Lake City)",
	"us-west4":                "US West (Las Vegas)",
	"us-south1":               "US South (Dallas)",
	"northamerica-northeast1": "Canada (Montréal)",
	"northamerica-northeast2": "Canada (Toronto)",
	"northamerica-south1":     "Mexico (Querétaro)",
	"southamerica-east1":      "South America (São Paulo)",
	"southamerica-west1":      "South America (Santiago)",
	// Europe
	"europe-west1":      "Europe (Belgium)",
	"europe-west2":      "Europe (London)",
	"europe-west3":      "Europe (Frankfurt)",
	"europe-west4":      "Europe (Netherlands)",
	"europe-west6":      "Europe (Zürich)",
	"europe-west8":      "Europe (Milan)",
	"europe-west9":      "Europe (Paris)",
	"europe-west10":     "Europe (Berlin)",
	"europe-west12":     "Europe (Turin)",
	"europe-north1":     "Europe (Finland)",
	"europe-central2":   "Europe (Warsaw)",
	"europe-southwest1": "Europe (Madrid)",
	// Asia Pacific
	"asia-east1":           "Asia Pacific (Taiwan)",
	"asia-east2":           "Asia Pacific (Hong Kong)",
	"asia-northeast1":      "Asia Pacific (Tokyo)",
	"asia-northeast2":      "Asia Pacific (Osaka)",
	"asia-northeast3":      "Asia Pacific (Seoul)",
	"asia-south1":          "Asia Pacific (Mumbai)",
	"asia-south2":          "Asia Pacific (Delhi)",
	"asia-southeast1":      "Asia Pacific (Singapore)",
	"asia-southeast2":      "Asia Pacific (Jakarta)",
	"australia-southeast1": "Australia (Sydney)",
	"australia-southeast2": "Australia (Melbourne)",
	// Middle East & Africa
	"me-west1":      "Middle East (Tel Aviv)",
	"me-central1":   "Middle East (Doha)",
	"me-central2":   "Middle East (Dammam)",
	"africa-south1": "Africa (Johannesburg)",
}

// gcpDisplayRegion is the reverse: display name → region code.
var gcpDisplayRegion map[string]string

// AzureRegionDisplay maps Azure ARM region names to human-readable display names.
var AzureRegionDisplay = map[string]string{
	// North America
	"eastus":         "East US",
	"eastus2":        "East US 2",
	"westus":         "West US",
	"westus2":        "West US 2",
	"westus3":        "West US 3",
	"centralus":      "Central US",
	"northcentralus": "North Central US",
	"southcentralus": "South Central US",
	"westcentralus":  "West Central US",
	"canadacentral":  "Canada Central",
	"canadaeast":     "Canada East",
	// South America
	"brazilsouth": "Brazil South",
	// Europe
	"northeurope":        "North Europe",
	"westeurope":         "West Europe",
	"uksouth":            "UK South",
	"ukwest":             "UK West",
	"francecentral":      "France Central",
	"germanywestcentral": "Germany West Central",
	"norwayeast":         "Norway East",
	"switzerlandnorth":   "Switzerland North",
	// Asia Pacific
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
	// Middle East & Africa
	"southafricanorth": "South Africa North",
	"uaenorth":         "UAE North",
}

// azureDisplayRegion is the reverse: display name → region code.
var azureDisplayRegion map[string]string

func init() {
	awsDisplayRegion = make(map[string]string, len(AWSRegionDisplay))
	for k, v := range AWSRegionDisplay {
		awsDisplayRegion[v] = k
	}

	gcpDisplayRegion = make(map[string]string, len(GCPRegionDisplay))
	for k, v := range GCPRegionDisplay {
		gcpDisplayRegion[v] = k
	}

	azureDisplayRegion = make(map[string]string, len(AzureRegionDisplay))
	for k, v := range AzureRegionDisplay {
		azureDisplayRegion[v] = k
	}
}

// AWSRegionToDisplay converts an AWS region code to the display name used by the Pricing API.
// Returns an error for unknown region codes.
func AWSRegionToDisplay(regionCode string) (string, error) {
	if display, ok := AWSRegionDisplay[regionCode]; ok {
		return display, nil
	}
	return "", fmt.Errorf("unknown AWS region code: %q", regionCode)
}

// AWSDisplayToRegion converts an AWS Pricing API display name back to a region code.
// Returns an error for unknown display names.
func AWSDisplayToRegion(displayName string) (string, error) {
	if code, ok := awsDisplayRegion[displayName]; ok {
		return code, nil
	}
	return "", fmt.Errorf("unknown AWS region display name: %q", displayName)
}

// NormalizeRegion accepts either a region code or display name and returns the region code.
// provider must be "aws", "gcp", or "azure". Returns an error for unknown values.
func NormalizeRegion(provider, value string) (string, error) {
	switch provider {
	case "aws":
		if _, ok := AWSRegionDisplay[value]; ok {
			return value, nil // already a code
		}
		if code, ok := awsDisplayRegion[value]; ok {
			return code, nil
		}
		return "", fmt.Errorf("unknown AWS region: %q", value)
	case "gcp":
		if _, ok := GCPRegionDisplay[value]; ok {
			return value, nil
		}
		if code, ok := gcpDisplayRegion[value]; ok {
			return code, nil
		}
		return "", fmt.Errorf("unknown GCP region: %q", value)
	case "azure":
		if _, ok := AzureRegionDisplay[value]; ok {
			return value, nil
		}
		if code, ok := azureDisplayRegion[value]; ok {
			return code, nil
		}
		return "", fmt.Errorf("unknown Azure region: %q", value)
	default:
		return value, nil
	}
}

// ListAWSRegions returns a sorted list of known AWS region codes.
func ListAWSRegions() []string {
	return sortedKeys(AWSRegionDisplay)
}

// ListGCPRegions returns a sorted list of known GCP region codes.
func ListGCPRegions() []string {
	return sortedKeys(GCPRegionDisplay)
}

// ListAzureRegions returns a sorted list of known Azure ARM region names.
func ListAzureRegions() []string {
	return sortedKeys(AzureRegionDisplay)
}

// RegionDisplayName returns the friendly display name for a region code,
// or the code itself if it is not recognised.
func RegionDisplayName(provider, regionCode string) string {
	switch provider {
	case "aws":
		if name, ok := AWSRegionDisplay[regionCode]; ok {
			return name
		}
	case "gcp":
		if name, ok := GCPRegionDisplay[regionCode]; ok {
			return name
		}
	case "azure":
		if name, ok := AzureRegionDisplay[regionCode]; ok {
			return name
		}
	}
	return regionCode
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
