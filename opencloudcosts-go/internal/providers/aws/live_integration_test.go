//go:build integration

package aws

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/cache"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/config"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// TestLiveOnDemand_m5_4xlarge is a live integration test that fetches m5.4xlarge
// on-demand pricing from the real AWS bulk pricing endpoint. It requires network
// access but no AWS credentials (uses the public bulk fallback path).
func TestLiveOnDemand_m5_4xlarge(t *testing.T) {
	cm, _ := cache.New(t.TempDir())
	cfg := &config.Config{CacheTTLHours: 24}

	p, err := NewProvider(cfg, cm)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	spec := &models.ComputePricingSpec{}
	spec.Provider = models.CloudProviderAWS
	spec.Domain = models.PricingDomainCompute
	spec.Region = "us-east-1"
	spec.OS = "Linux"
	spec.Term = models.PricingTermOnDemand
	spec.ResourceType = "m5.4xlarge"

	start := time.Now()
	result, err := p.GetPrice(ctx, spec)
	t.Logf("Elapsed: %v", time.Since(start))

	if err != nil {
		t.Fatalf("GetPrice error: %v", err)
	}
	if len(result.PublicPrices) == 0 {
		t.Fatalf("expected at least one on-demand price for m5.4xlarge, got none (source=%s)", result.Source)
	}

	price := result.PublicPrices[0].PricePerUnit
	// m5.4xlarge us-east-1 Linux on-demand is ~$0.768/hr; allow wide bounds for drift.
	if price < 0.1 || price > 5.0 {
		t.Errorf("m5.4xlarge on-demand price %v is outside expected range [0.10, 5.00]", price)
	}
	fmt.Printf("m5.4xlarge on-demand us-east-1: $%.4f/hr\n", price)
}
