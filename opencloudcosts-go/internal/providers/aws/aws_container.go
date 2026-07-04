// Package aws — EKS container/Kubernetes control-plane pricing.
package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// eksTiers maps the AWS Pricing API "operation" attribute to a human-readable
// support-tier label. "CreateOperation" is the standard per-cluster hourly
// fee; "ExtendedSupport" is the surcharge for clusters running Kubernetes
// versions past standard support.
var eksTiers = []struct {
	operation string
	label     string
}{
	{"CreateOperation", "standard support"},
	{"ExtendedSupport", "extended support"},
}

// GetEKSPrice returns AWS EKS control-plane pricing: the standard per-cluster
// hourly fee and, where published for the region, the extended-support
// surcharge. Node/compute costs are priced separately via the compute domain.
// Source: https://aws.amazon.com/eks/pricing/
func (p *Provider) GetEKSPrice(ctx context.Context, region string) ([]models.NormalizedPrice, error) {
	location, err := regionToLocation(region)
	if err != nil {
		return nil, err
	}

	cacheKey := fmt.Sprintf("aws:eks:%s", region)
	if cached, ok := p.cache.Get(cacheKey); ok {
		var prices []models.NormalizedPrice
		if err := json.Unmarshal(cached, &prices); err == nil {
			return prices, nil
		}
	}

	var prices []models.NormalizedPrice
	for _, tier := range eksTiers {
		filters := []pricingtypes.Filter{
			mkFilter("location", location),
			mkFilter("operation", tier.operation),
			mkFilter("locationType", "AWS Region"),
		}
		rawItems, err := p.GetProducts(ctx, "AmazonEKS", filters, 2)
		if err != nil {
			return nil, fmt.Errorf("aws: GetEKSPrice: %w", err)
		}
		for _, raw := range rawItems {
			np := skuToNormalizedPrice(raw, region, models.PricingTermOnDemand, "eks")
			if np != nil {
				np.Description = fmt.Sprintf("Amazon EKS cluster control plane (%s) — per cluster, per hour", tier.label)
				prices = append(prices, *np)
			}
		}
	}

	if len(prices) == 0 {
		return nil, fmt.Errorf("aws: GetEKSPrice: no EKS control-plane pricing found for region %q", region)
	}

	if data, err := json.Marshal(prices); err == nil {
		ttl := time.Duration(p.cfg.CacheTTLHours) * time.Hour
		p.cache.Set(cacheKey, data, ttl)
	}
	return prices, nil
}
