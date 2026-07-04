package tools_test

import (
	"context"
	"errors"
	"testing"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/tools"
)

func callGetCoverage(t *testing.T, h *tools.Handler, in tools.GetCoverageInput) map[string]any {
	t.Helper()
	result, _, err := h.HandleGetCoverage(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("HandleGetCoverage returned err: %v", err)
	}
	return decodeResult(t, result)
}

func TestGetCoverage_ProviderNotConfigured(t *testing.T) {
	h := tools.New(nil)
	resp := callGetCoverage(t, h, tools.GetCoverageInput{Provider: "aws"})
	if resp["error"] == nil {
		t.Errorf("expected error, got: %v", resp)
	}
}

func TestGetCoverage_SingleProvider(t *testing.T) {
	catalog := &models.ProviderCatalog{
		Provider: models.CloudProviderAWS,
		Domains: []models.PricingDomain{
			models.PricingDomainCompute,
			models.PricingDomainInterRegionEgress,
		},
		Services: map[string][]string{
			"compute":             {"ec2", "fargate"},
			"inter_region_egress": {},
		},
	}
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		describeCatFunc: func(_ context.Context) (*models.ProviderCatalog, error) {
			return catalog, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetCoverage(t, h, tools.GetCoverageInput{Provider: "aws"})

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	if resp["as_of"] == nil {
		t.Error("expected as_of timestamp")
	}
	domains, ok := resp["domains"].(map[string]any)
	if !ok {
		t.Fatalf("expected domains map, got: %v", resp)
	}

	compute, ok := domains["compute"].(map[string]any)
	if !ok {
		t.Fatalf("expected compute entry, got: %v", domains)
	}
	if compute["status"] != "catalog" {
		t.Errorf("compute status: got %v, want catalog", compute["status"])
	}
	services, ok := compute["services"].([]any)
	if !ok || len(services) != 2 {
		t.Errorf("compute services: got %v, want [ec2 fargate]", compute["services"])
	}

	// A domain with an empty Services[] entry (e.g. inter_region_egress on
	// GCP/Azure) is still functional/parameterized — must not be reported as
	// absent (RC3-020-style misrepresentation at the get_coverage level).
	ire, ok := domains["inter_region_egress"].(map[string]any)
	if !ok {
		t.Fatalf("expected inter_region_egress entry, got: %v", domains)
	}
	if ire["status"] != "catalog" {
		t.Errorf("inter_region_egress status: got %v, want catalog", ire["status"])
	}
	if ire["note"] == nil {
		t.Error("expected a note explaining the parameterized-domain case")
	}
}

func TestGetCoverage_NoArgs_AllProviders(t *testing.T) {
	catalog := &models.ProviderCatalog{
		Provider: models.CloudProviderAWS,
		Domains:  []models.PricingDomain{models.PricingDomainCompute},
		Services: map[string][]string{"compute": {"ec2"}},
	}
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		describeCatFunc: func(_ context.Context) (*models.ProviderCatalog, error) {
			return catalog, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetCoverage(t, h, tools.GetCoverageInput{})

	coverage, ok := resp["coverage"].(map[string]any)
	if !ok {
		t.Fatalf("expected coverage map, got: %v", resp)
	}
	if _, ok := coverage["aws"]; !ok {
		t.Error("expected aws in coverage")
	}
}

func TestGetCoverage_UpstreamError(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		describeCatFunc: func(_ context.Context) (*models.ProviderCatalog, error) {
			return nil, errors.New("catalog unavailable")
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetCoverage(t, h, tools.GetCoverageInput{Provider: "aws"})
	if resp["error"] == nil {
		t.Errorf("expected error, got: %v", resp)
	}
}
