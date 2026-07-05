package tools_test

import (
	"context"
	"testing"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/tools"
)

func callCompareBOMRegions(t *testing.T, h *tools.Handler, in tools.CompareBOMRegionsInput) map[string]any {
	t.Helper()
	result, _, err := h.HandleCompareBOMRegions(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("HandleCompareBOMRegions returned err: %v", err)
	}
	return decodeResult(t, result)
}

// newRegionPricedProvider returns a mockProvider whose GetPrice varies by the
// region on the incoming spec, so tests can exercise sorting/baseline-delta
// behavior across regions.
func newRegionPricedProvider(pricePerRegion map[string]float64) *mockProvider {
	return &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			price, ok := pricePerRegion[spec.GetRegion()]
			if !ok {
				return nil, providers.ErrNotSupported
			}
			return &models.PricingResult{PublicPrices: []models.NormalizedPrice{
				makeComputePrice("aws", spec.GetRegion(), "m5.xlarge", price),
			}}, nil
		},
	}
}

func TestCompareBOMRegions_MissingRegions(t *testing.T) {
	h := tools.New(nil)
	resp := callCompareBOMRegions(t, h, tools.CompareBOMRegionsInput{
		Items: []map[string]any{{"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge"}},
	})
	if resp["error"] == nil {
		t.Errorf("expected error for missing regions, got: %v", resp)
	}
}

func TestCompareBOMRegions_MissingItems(t *testing.T) {
	h := tools.New(nil)
	resp := callCompareBOMRegions(t, h, tools.CompareBOMRegionsInput{
		Regions: []string{"us-east-1"},
	})
	if resp["error"] == nil {
		t.Errorf("expected error for missing items, got: %v", resp)
	}
}

func TestCompareBOMRegions_SortedCheapestFirstWithBaselineDelta(t *testing.T) {
	pvdr := newRegionPricedProvider(map[string]float64{
		"us-east-1": 0.192,
		"us-west-2": 0.150,
		"eu-west-1": 0.210,
	})
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	resp := callCompareBOMRegions(t, h, tools.CompareBOMRegionsInput{
		Items: []map[string]any{
			{"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge", "quantity": float64(1)},
		},
		Regions:        []string{"us-east-1", "us-west-2", "eu-west-1"},
		BaselineRegion: "us-east-1",
	})

	regions, ok := resp["regions"].([]any)
	if !ok || len(regions) != 3 {
		t.Fatalf("expected 3 region entries, got: %v", resp["regions"])
	}

	first := regions[0].(map[string]any)
	if first["region"] != "us-west-2" {
		t.Errorf("expected cheapest region us-west-2 first, got %v", first["region"])
	}
	last := regions[2].(map[string]any)
	if last["region"] != "eu-west-1" {
		t.Errorf("expected most expensive region eu-west-1 last, got %v", last["region"])
	}

	// us-east-1 is the baseline — its own delta should be $0.00/mo (zero, not omitted).
	var baselineEntry map[string]any
	for _, r := range regions {
		e := r.(map[string]any)
		if e["region"] == "us-east-1" {
			baselineEntry = e
		}
	}
	if baselineEntry == nil {
		t.Fatalf("expected us-east-1 entry in regions")
	}
	if baselineEntry["delta_monthly"] != "$+0.00/mo" {
		t.Errorf("expected baseline delta_monthly of +0.00, got %v", baselineEntry["delta_monthly"])
	}
}

func TestCompareBOMRegions_NonAWSItemReportedNotSupported(t *testing.T) {
	pvdr := newRegionPricedProvider(map[string]float64{"us-east-1": 0.192})
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	resp := callCompareBOMRegions(t, h, tools.CompareBOMRegionsInput{
		Items: []map[string]any{
			{"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge", "quantity": float64(1)},
			{"provider": "gcp", "domain": "compute", "resource_type": "n1-standard-4", "quantity": float64(1)},
		},
		Regions: []string{"us-east-1"},
	})

	notSupported, ok := resp["not_supported"].([]any)
	if !ok || len(notSupported) != 1 {
		t.Fatalf("expected 1 not_supported entry, got: %v", resp["not_supported"])
	}
	entry := notSupported[0].(map[string]any)
	if entry["provider"] != "gcp" {
		t.Errorf("expected gcp in not_supported entry, got %v", entry)
	}

	regions := resp["regions"].([]any)
	region := regions[0].(map[string]any)
	lineItems := region["line_items"].([]any)
	if len(lineItems) != 1 {
		t.Errorf("expected only the aws line item to resolve, got %d line items", len(lineItems))
	}
}

func TestCompareBOMRegions_BaselineRegionNotFound(t *testing.T) {
	pvdr := newRegionPricedProvider(map[string]float64{"us-east-1": 0.192})
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	resp := callCompareBOMRegions(t, h, tools.CompareBOMRegionsInput{
		Items: []map[string]any{
			{"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge", "quantity": float64(1)},
		},
		Regions:        []string{"us-east-1"},
		BaselineRegion: "ap-southeast-1",
	})

	if resp["baseline_region_error"] == nil {
		t.Errorf("expected baseline_region_error, got: %v", resp)
	}
	regions := resp["regions"].([]any)
	region := regions[0].(map[string]any)
	if region["delta_monthly"] != nil {
		t.Errorf("expected nulled delta_monthly when baseline not found, got %v", region["delta_monthly"])
	}
}
