// Package aws implements the AWS cloud pricing provider for OpenCloudCosts.
//
// Public pricing uses the AWS Pricing API (pricing.GetProducts). No credentials
// are required for the pricing client itself when using the public endpoint at
// us-east-1. Cost Explorer and Savings Plans clients are only used by Part 2
// (FinOps) and are held in the struct but stubs return ErrNotSupported here.
package aws

import (
	"context"
	"net/http"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	"github.com/aws/aws-sdk-go-v2/service/savingsplans"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/cache"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/config"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers"
)

// Compile-time interface guard — fails to build if Provider does not satisfy
// the providers.Provider interface.
var _ providers.Provider = (*Provider)(nil)

// pricingRegion is the only region where the AWS Pricing API is available.
const pricingRegion = "us-east-1"

// Provider implements providers.Provider for AWS.
type Provider struct {
	cfg           *config.Config
	cache         *cache.CacheManager
	pricingClient *pricing.Client
	ceClient      *costexplorer.Client
	ec2Client     *ec2.Client
	spClient      *savingsplans.Client
	httpClient    *http.Client
}

// NewProvider constructs a new AWS Provider.
//
// The AWS SDK loads credentials from the standard chain (env vars, ~/.aws/credentials,
// IAM role, etc.). The pricing client is always created at us-east-1 because the
// AWS Pricing API is only available there. The Cost Explorer client is also
// pinned to us-east-1 per AWS documentation. The EC2 client uses the configured
// region (or us-east-1 default) for DescribeRegions / DescribeInstanceTypes.
func NewProvider(cfg *config.Config, cacheManager *cache.CacheManager) (*Provider, error) {
	ctx := context.Background()

	opts := []func(*awsconfig.LoadOptions) error{}
	if cfg.AWSProfile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(cfg.AWSProfile))
	}
	if cfg.AWSRegion != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.AWSRegion))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}

	// The pricing API is only available in us-east-1.
	pricingCfg := awsCfg.Copy()
	pricingCfg.Region = pricingRegion
	pricingClient := pricing.NewFromConfig(pricingCfg)

	// Cost Explorer must also be in us-east-1 regardless of configured region.
	ceCfg := awsCfg.Copy()
	ceCfg.Region = "us-east-1"
	ceClient := costexplorer.NewFromConfig(ceCfg)

	ec2Client := ec2.NewFromConfig(awsCfg)
	spClient := savingsplans.NewFromConfig(awsCfg)

	return &Provider{
		cfg:           cfg,
		cache:         cacheManager,
		pricingClient: pricingClient,
		ceClient:      ceClient,
		ec2Client:     ec2Client,
		spClient:      spClient,
		httpClient:    &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// --------------------------------------------------------------------------
// Identity methods
// --------------------------------------------------------------------------

// Name implements providers.Provider.
func (p *Provider) Name() providers.CloudProvider {
	return models.CloudProviderAWS
}

// DefaultRegion implements providers.Provider.
func (p *Provider) DefaultRegion() string {
	return "us-east-1"
}

// MajorRegions implements providers.Provider — returns the 12 curated major
// AWS regions used by fan-out tools, mirroring _MAJOR_REGIONS in Python.
func (p *Provider) MajorRegions() []string {
	return []string{
		"us-east-1",
		"us-east-2",
		"us-west-1",
		"us-west-2",
		"ca-central-1",
		"eu-west-1",
		"eu-west-2",
		"eu-central-1",
		"ap-southeast-1",
		"ap-southeast-2",
		"ap-northeast-1",
		"ap-south-1",
	}
}

// Supports implements providers.Provider — mirrors _AWS_CAPABILITIES from Python.
func (p *Provider) Supports(domain models.PricingDomain, service string) bool {
	// Domain-level catch-all (service == "")
	switch domain { //nolint:exhaustive // unlisted domains are checked in capabilities map below
	case models.PricingDomainCompute,
		models.PricingDomainStorage,
		models.PricingDomainDatabase,
		models.PricingDomainNetwork:
		if service == "" {
			return true
		}
	}

	type key struct {
		domain  models.PricingDomain
		service string
	}
	capabilities := map[key]bool{
		// compute
		{models.PricingDomainCompute, "ec2"}:     true,
		{models.PricingDomainCompute, "fargate"}: true,
		// storage
		{models.PricingDomainStorage, "ebs"}: true,
		{models.PricingDomainStorage, "s3"}:  true,
		// database
		{models.PricingDomainDatabase, "rds"}:         true,
		{models.PricingDomainDatabase, "elasticache"}: true,
		// network
		{models.PricingDomainNetwork, "lb"}:            true,
		{models.PricingDomainNetwork, "cloud_lb"}:      true,
		{models.PricingDomainNetwork, "cdn"}:           true,
		{models.PricingDomainNetwork, "cloud_cdn"}:     true,
		{models.PricingDomainNetwork, "cloudfront"}:    true,
		{models.PricingDomainNetwork, "nat"}:           true,
		{models.PricingDomainNetwork, "cloud_nat"}:     true,
		{models.PricingDomainNetwork, "waf"}:           true,
		{models.PricingDomainNetwork, "data_transfer"}: true,
		{models.PricingDomainNetwork, "egress"}:        true,
		// finops
		{models.PricingDomainAI, ""}:                      true,
		{models.PricingDomainAI, "bedrock"}:               true,
		{models.PricingDomainAI, "sagemaker"}:             true,
		{models.PricingDomainServerless, ""}:              true,
		{models.PricingDomainServerless, "lambda"}:        true,
		{models.PricingDomainAnalytics, ""}:               true,
		{models.PricingDomainAnalytics, "redshift"}:       true,
		{models.PricingDomainAnalytics, "athena"}:         true,
		{models.PricingDomainObservability, ""}:           true,
		{models.PricingDomainObservability, "cloudwatch"}: true,
		{models.PricingDomainContainer, ""}:               true,
		{models.PricingDomainContainer, "eks"}:            true,
		{models.PricingDomainInterRegionEgress, ""}:       true,
	}
	return capabilities[key{domain, service}]
}

// SupportedTerms implements providers.Provider.
// Mirrors supported_terms() from Python, trimmed to Part 1 domains.
func (p *Provider) SupportedTerms(domain models.PricingDomain, service string) []models.PricingTerm {
	base := []models.PricingTerm{models.PricingTermOnDemand}
	switch domain { //nolint:exhaustive // unlisted domains only support on-demand (the base slice)
	case models.PricingDomainCompute:
		base = append(base,
			models.PricingTermReserved1Yr,
			models.PricingTermReserved1YrPartial,
			models.PricingTermReserved1YrAll,
			models.PricingTermReserved3Yr,
			models.PricingTermReserved3YrPartial,
			models.PricingTermReserved3YrAll,
		)
	case models.PricingDomainDatabase:
		base = append(base,
			models.PricingTermReserved1Yr,
			models.PricingTermReserved3Yr,
			models.PricingTermReserved1YrPartial,
			models.PricingTermReserved1YrAll,
		)
	}
	return base
}
