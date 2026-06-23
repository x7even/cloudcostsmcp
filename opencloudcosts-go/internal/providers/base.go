// Package providers defines the Provider interface all cloud pricing
// providers must implement. Methods that do not apply to a given provider
// return (nil, ErrNotSupported). The interface mirrors the CloudPricingProvider
// Protocol and ProviderBase mixin defined in Python's providers/base.py.
package providers

import (
	"context"
	"errors"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// ErrNotSupported is returned by providers for methods they do not implement.
var ErrNotSupported = errors.New("not supported by this provider")

// Provider is the common interface all cloud pricing providers must implement.
//
// The interface is the union of methods present in CloudPricingProvider (Protocol)
// and ProviderBase (mixin) in providers/base.py, plus the provider-identity helpers
// (Name, DefaultRegion, MajorRegions) and the unified get_price entry point added
// in v0.8.0.
//
// The following methods are in all three providers but are internal dispatch helpers
// called by GetPrice — they are NOT part of this interface:
// AWS:   GetServicePrice, ListServices, GetActiveSavingsPlans, GetSavingsPlanRates,
//
//	GetActiveReservedInstances
//
// GCP:   GetCloudSQLPrice, GetMemostorePrice, GetBigQueryPrice, GetGKEPrice,
//
//	GetVertexPrice, GetGeminiPrice, GetCloudLBPrice, GetCloudCDNPrice,
//	GetCloudNATPrice, GetCloudArmorPrice, GetCloudMonitoringPrice,
//	GetEgressPrice, Close
//
// Azure: GetSQLPrice, GetCosmosPrice, GetAKSPrice, GetFunctionsPrice,
//
//	GetOpenAIPrice, GetEgressPrice, GetMonitorPrice, GetCDNPrice,
//	GetFrontDoorPrice
type Provider interface {
	// Identity

	// Name returns the canonical provider name (e.g. "aws", "gcp", "azure").
	Name() CloudProvider

	// DefaultRegion returns the provider's default region when none is specified.
	DefaultRegion() string

	// MajorRegions returns the provider's curated major-region list used by
	// fan-out tools (find_cheapest_region, find_available_regions).
	MajorRegions() []string

	// Supports reports whether the provider can price the given domain/service pair.
	Supports(domain models.PricingDomain, service string) bool

	// SupportedTerms returns the pricing terms supported for the given domain/service.
	SupportedTerms(domain models.PricingDomain, service string) []models.PricingTerm

	// Unified pricing entry point (v0.8.0+)

	// GetPrice is the primary entry point. It accepts a typed PricingSpec (with
	// discriminated domain field) and returns the full PricingResult including
	// public prices, contracted rates (when auth is present), and effective price.
	GetPrice(ctx context.Context, spec models.PricingSpec) (*models.PricingResult, error)

	// Legacy per-domain entry points (called directly by some tool layers)

	// GetComputePrice returns pricing for a specific compute instance type in a region.
	GetComputePrice(
		ctx context.Context,
		instanceType string,
		region string,
		os string,
		term models.PricingTerm,
	) ([]models.NormalizedPrice, error)

	// GetStoragePrice returns pricing for block or object storage.
	GetStoragePrice(
		ctx context.Context,
		storageType string,
		region string,
		sizeGB float64,
	) ([]models.NormalizedPrice, error)

	// SearchPricing performs a free-text search across the pricing catalog.
	SearchPricing(
		ctx context.Context,
		query string,
		region string,
		maxResults int,
	) ([]models.NormalizedPrice, error)

	// Discovery

	// ListRegions returns all regions where a service is available.
	ListRegions(ctx context.Context, service string) ([]string, error)

	// ListInstanceTypes returns available instance types matching the given filters.
	ListInstanceTypes(
		ctx context.Context,
		region string,
		family string,
		minVCPUs int,
		minMemoryGB float64,
		gpu bool,
	) ([]models.InstanceTypeInfo, error)

	// CheckAvailability reports whether the given SKU/instance type is available
	// in the region. Returns (available, alternateSuggestions, error).
	CheckAvailability(
		ctx context.Context,
		service string,
		skuOrType string,
		region string,
	) (bool, []string, error)

	// FinOps

	// GetEffectivePrice returns effective/bespoke pricing reflecting account-level
	// discounts (Reserved Instances, Savings Plans, CUDs, etc.).
	// Returns ErrNotSupported if the provider is not authenticated or does not
	// support effective pricing.
	GetEffectivePrice(ctx context.Context, spec models.PricingSpec) ([]models.EffectivePrice, error)

	// GetSpotHistory returns historical spot instance prices.
	// Returns ErrNotSupported for providers that do not offer spot pricing
	// (GCP, Azure).
	GetSpotHistory(
		ctx context.Context,
		spec models.PricingSpec,
		hours int,
		availabilityZone string,
	) (map[string]any, error)

	// GetDiscountSummary returns a summary of active account-level discounts
	// (Savings Plans, Reserved Instances, CUDs, EDPs).
	// Returns ErrNotSupported if the provider is not authenticated or does not
	// support discount summaries.
	GetDiscountSummary(ctx context.Context) (map[string]any, error)

	// DescribeCatalog returns a structured catalog of what the provider supports
	// and example invocations for each domain/service cell. This is the LLM's
	// O(1) discovery tool.
	DescribeCatalog(ctx context.Context) (*models.ProviderCatalog, error)

	// BOMAdvisories returns provider-specific advisory rows for services not
	// included in estimate_bom (e.g. egress, support costs).
	// services is the set of service keys appearing in the BoM; sampleRegion is
	// any region from the BoM for regional advisory targeting.
	BOMAdvisories(ctx context.Context, services []string, sampleRegion string) ([]map[string]string, error)
}

// CloudProvider is an alias for models.CloudProvider for use within this package.
type CloudProvider = models.CloudProvider
