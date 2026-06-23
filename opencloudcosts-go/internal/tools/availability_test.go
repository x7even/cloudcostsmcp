// availability_test.go tests the 4 availability tools.
//
// Test patterns mirror test_gcp_major_regions.py:
//   - no regions arg → defaults to major regions, note field present with count
//   - regions=["all"] → all regions, no note field
//   - explicit regions list → used as-is, no note
//   - context cancellation aborts all in-flight fetches
//   - -race flag: goroutines write only to their own index slot
package tools_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/tools"
)

// --------------------------------------------------------------------------
// Helpers for availability tests
// --------------------------------------------------------------------------

// mockProviderAvail extends mockProvider with ListRegions, ListInstanceTypes,
// and configurable major regions, without duplicating the full mock.
type mockProviderAvail struct {
	mockProvider
	listRegionsFunc       func(ctx context.Context, service string) ([]string, error)
	listInstanceTypesFunc func(ctx context.Context, region, family string, minVCPUs int, minMemGB float64, gpu bool) ([]models.InstanceTypeInfo, error)
}

func (m *mockProviderAvail) ListRegions(ctx context.Context, service string) ([]string, error) {
	if m.listRegionsFunc != nil {
		return m.listRegionsFunc(ctx, service)
	}
	return nil, providers.ErrNotSupported
}

func (m *mockProviderAvail) ListInstanceTypes(ctx context.Context, region, family string, minVCPUs int, minMemGB float64, gpu bool) ([]models.InstanceTypeInfo, error) {
	if m.listInstanceTypesFunc != nil {
		return m.listInstanceTypesFunc(ctx, region, family, minVCPUs, minMemGB, gpu)
	}
	return nil, providers.ErrNotSupported
}

// callListRegions invokes HandleListRegions and decodes the response.
func callListRegions(t *testing.T, h *tools.Handler, in tools.ListRegionsInput) map[string]any {
	t.Helper()
	result, _, err := h.HandleListRegions(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("HandleListRegions returned err: %v", err)
	}
	return decodeResult(t, result)
}

// callListInstanceTypes invokes HandleListInstanceTypes and decodes the response.
func callListInstanceTypes(t *testing.T, h *tools.Handler, in tools.ListInstanceTypesInput) map[string]any {
	t.Helper()
	result, _, err := h.HandleListInstanceTypes(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("HandleListInstanceTypes returned err: %v", err)
	}
	return decodeResult(t, result)
}

// callFindCheapestRegion invokes HandleFindCheapestRegion and decodes the response.
func callFindCheapestRegion(t *testing.T, h *tools.Handler, in tools.FindCheapestRegionInput) map[string]any {
	t.Helper()
	result, _, err := h.HandleFindCheapestRegion(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("HandleFindCheapestRegion returned err: %v", err)
	}
	return decodeResult(t, result)
}

// callFindCheapestRegionCtx is like callFindCheapestRegion but uses a custom context.
func callFindCheapestRegionCtx(t *testing.T, ctx context.Context, h *tools.Handler, in tools.FindCheapestRegionInput) map[string]any {
	t.Helper()
	result, _, err := h.HandleFindCheapestRegion(ctx, nil, in)
	if err != nil {
		t.Fatalf("HandleFindCheapestRegion returned err: %v", err)
	}
	return decodeResult(t, result)
}

// callFindAvailableRegions invokes HandleFindAvailableRegions and decodes the response.
func callFindAvailableRegions(t *testing.T, h *tools.Handler, in tools.FindAvailableRegionsInput) map[string]any {
	t.Helper()
	result, _, err := h.HandleFindAvailableRegions(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("HandleFindAvailableRegions returned err: %v", err)
	}
	return decodeResult(t, result)
}

// callFindAvailableRegionsCtx is like callFindAvailableRegions but uses a custom context.
func callFindAvailableRegionsCtx(t *testing.T, ctx context.Context, h *tools.Handler, in tools.FindAvailableRegionsInput) map[string]any {
	t.Helper()
	result, _, err := h.HandleFindAvailableRegions(ctx, nil, in)
	if err != nil {
		t.Fatalf("HandleFindAvailableRegions returned err: %v", err)
	}
	return decodeResult(t, result)
}

// gcpMajorRegions matches the 12 GCP major regions from utils.GCPMajorRegions.
var gcpMajorRegions = []string{
	"us-central1",
	"us-east1",
	"us-west1",
	"us-west2",
	"europe-west1",
	"europe-west2",
	"europe-west3",
	"europe-west4",
	"asia-east1",
	"asia-northeast1",
	"asia-southeast1",
	"australia-southeast1",
}

// allGCPRegions is a superset used to simulate "list all" returning more regions.
var allGCPRegions = append([]string{
	"us-east4",
	"us-east5",
	"us-west3",
	"us-west4",
	"us-south1",
	"northamerica-northeast1",
	"europe-west6",
	"europe-central2",
	"asia-south1",
	"asia-southeast2",
}, gcpMajorRegions...)

// makeGCPMockProvider creates a mock GCP provider with major regions and configurable
// GetPrice and ListRegions behavior.
func makeGCPMockProvider(
	getPriceFn func(ctx context.Context, spec models.PricingSpec) (*models.PricingResult, error),
	listRegionsFn func(ctx context.Context, service string) ([]string, error),
) *mockProviderAvail {
	if listRegionsFn == nil {
		listRegionsFn = func(_ context.Context, _ string) ([]string, error) {
			return allGCPRegions, nil
		}
	}
	return &mockProviderAvail{
		mockProvider: mockProvider{
			name:          "gcp",
			defaultRegion: "us-central1",
			majorRegions:  gcpMajorRegions,
			getPriceFunc:  getPriceFn,
		},
		listRegionsFunc: listRegionsFn,
	}
}

// makeAWSMockProvider creates a mock AWS provider with 12 major regions.
func makeAWSMockProvider(
	getPriceFn func(ctx context.Context, spec models.PricingSpec) (*models.PricingResult, error),
	listRegionsFn func(ctx context.Context, service string) ([]string, error),
) *mockProviderAvail {
	awsMajorRegions := []string{
		"us-east-1", "us-east-2", "us-west-1", "us-west-2",
		"ca-central-1", "eu-west-1", "eu-west-2", "eu-central-1",
		"ap-southeast-1", "ap-southeast-2", "ap-northeast-1", "ap-south-1",
	}
	if listRegionsFn == nil {
		listRegionsFn = func(_ context.Context, _ string) ([]string, error) {
			return append(awsMajorRegions, "ap-east-1", "sa-east-1", "me-south-1"), nil
		}
	}
	return &mockProviderAvail{
		mockProvider: mockProvider{
			name:          "aws",
			defaultRegion: "us-east-1",
			majorRegions:  awsMajorRegions,
			getPriceFunc:  getPriceFn,
		},
		listRegionsFunc: listRegionsFn,
	}
}

// makeGCPPriceResult returns a single NormalizedPrice for a GCP region.
func makeGCPPriceResult(region string, pricePerHour float64) *models.PricingResult {
	return makePriceResult(models.NormalizedPrice{
		Provider:     models.CloudProviderGCP,
		Service:      "compute",
		SKUID:        "test-sku-" + region,
		Description:  "n1-standard-1 in " + region,
		Region:       region,
		PricingTerm:  models.PricingTermOnDemand,
		PricePerUnit: pricePerHour,
		Unit:         models.PriceUnitPerHour,
		Currency:     "USD",
	})
}

// --------------------------------------------------------------------------
// Tests: list_regions
// --------------------------------------------------------------------------

func TestListRegions_ProviderNotConfigured(t *testing.T) {
	h := tools.New(nil)
	resp := callListRegions(t, h, tools.ListRegionsInput{Provider: "gcp"})
	if resp["error"] == nil {
		t.Errorf("expected error, got: %v", resp)
	}
}

func TestListRegions_DefaultDomainIsCompute(t *testing.T) {
	var capturedDomain string
	pvdr := makeGCPMockProvider(nil, func(_ context.Context, service string) ([]string, error) {
		capturedDomain = service
		return gcpMajorRegions, nil
	})
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})
	resp := callListRegions(t, h, tools.ListRegionsInput{Provider: "gcp"}) // no domain
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	if capturedDomain != "compute" {
		t.Errorf("domain passed to ListRegions: got %q, want %q", capturedDomain, "compute")
	}
}

func TestListRegions_ResponseShape(t *testing.T) {
	pvdr := makeGCPMockProvider(nil, func(_ context.Context, _ string) ([]string, error) {
		return gcpMajorRegions, nil
	})
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})
	resp := callListRegions(t, h, tools.ListRegionsInput{Provider: "gcp", Domain: "compute"})

	if resp["provider"] != "gcp" {
		t.Errorf("provider: got %v, want gcp", resp["provider"])
	}
	if resp["domain"] != "compute" {
		t.Errorf("domain: got %v, want compute", resp["domain"])
	}
	if resp["count"] != float64(12) {
		t.Errorf("count: got %v, want 12", resp["count"])
	}
	regions, ok := resp["regions"].([]any)
	if !ok || len(regions) != 12 {
		t.Fatalf("regions: expected 12, got %v", resp["regions"])
	}
	r0 := regions[0].(map[string]any)
	if r0["code"] == nil {
		t.Error("regions[0] missing code")
	}
	if r0["name"] == nil {
		t.Error("regions[0] missing name")
	}
}

func TestListRegions_ListRegionsError(t *testing.T) {
	pvdr := makeGCPMockProvider(nil, func(_ context.Context, _ string) ([]string, error) {
		return nil, errors.New("catalog unavailable")
	})
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})
	resp := callListRegions(t, h, tools.ListRegionsInput{Provider: "gcp"})
	if resp["error"] == nil {
		t.Errorf("expected error, got: %v", resp)
	}
}

// --------------------------------------------------------------------------
// Tests: list_instance_types
// --------------------------------------------------------------------------

func TestListInstanceTypes_ProviderNotConfigured(t *testing.T) {
	h := tools.New(nil)
	resp := callListInstanceTypes(t, h, tools.ListInstanceTypesInput{
		Provider: "gcp", Region: "us-central1",
	})
	if resp["error"] == nil {
		t.Errorf("expected error, got: %v", resp)
	}
}

func TestListInstanceTypes_ResponseShape(t *testing.T) {
	pvdr := makeGCPMockProvider(nil, nil)
	pvdr.listInstanceTypesFunc = func(_ context.Context, region, family string, minVCPUs int, minMemGB float64, gpu bool) ([]models.InstanceTypeInfo, error) {
		return []models.InstanceTypeInfo{
			{Provider: models.CloudProviderGCP, InstanceType: "n1-standard-1", VCPU: 1, MemoryGB: 3.75, Region: region, Available: true},
			{Provider: models.CloudProviderGCP, InstanceType: "n1-standard-4", VCPU: 4, MemoryGB: 15.0, Region: region, Available: true},
		}, nil
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})
	resp := callListInstanceTypes(t, h, tools.ListInstanceTypesInput{
		Provider: "gcp",
		Region:   "us-central1",
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	if resp["provider"] != "gcp" {
		t.Errorf("provider: got %v, want gcp", resp["provider"])
	}
	if resp["region"] != "us-central1" {
		t.Errorf("region: got %v, want us-central1", resp["region"])
	}
	if resp["count"] != float64(2) {
		t.Errorf("count: got %v, want 2", resp["count"])
	}
	types, ok := resp["instance_types"].([]any)
	if !ok || len(types) != 2 {
		t.Fatalf("instance_types: expected 2, got %v", resp["instance_types"])
	}
	it0 := types[0].(map[string]any)
	if it0["instance_type"] != "n1-standard-1" {
		t.Errorf("instance_types[0].instance_type: got %v", it0["instance_type"])
	}
	if it0["vcpu"] != float64(1) {
		t.Errorf("instance_types[0].vcpu: got %v, want 1", it0["vcpu"])
	}
	if it0["memory_gb"] != 3.75 {
		t.Errorf("instance_types[0].memory_gb: got %v, want 3.75", it0["memory_gb"])
	}
}

func TestListInstanceTypes_GPUFieldsOmittedWhenZero(t *testing.T) {
	pvdr := makeGCPMockProvider(nil, nil)
	pvdr.listInstanceTypesFunc = func(_ context.Context, _ string, _ string, _ int, _ float64, _ bool) ([]models.InstanceTypeInfo, error) {
		return []models.InstanceTypeInfo{
			// No GPU — GPUCount=0 and GPUType=""
			{InstanceType: "n1-standard-1", VCPU: 1, MemoryGB: 3.75, Region: "us-central1"},
		}, nil
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})
	resp := callListInstanceTypes(t, h, tools.ListInstanceTypesInput{Provider: "gcp", Region: "us-central1"})
	types, _ := resp["instance_types"].([]any)
	it0 := types[0].(map[string]any)
	if _, ok := it0["gpu_count"]; ok {
		t.Errorf("gpu_count should not be present when zero, got: %v", it0)
	}
	if _, ok := it0["gpu_type"]; ok {
		t.Errorf("gpu_type should not be present when empty, got: %v", it0)
	}
}

func TestListInstanceTypes_GPUFieldsIncludedWhenPresent(t *testing.T) {
	pvdr := makeGCPMockProvider(nil, nil)
	pvdr.listInstanceTypesFunc = func(_ context.Context, _ string, _ string, _ int, _ float64, _ bool) ([]models.InstanceTypeInfo, error) {
		return []models.InstanceTypeInfo{
			{InstanceType: "a2-highgpu-1g", VCPU: 12, MemoryGB: 85.0, GPUCount: 1, GPUType: "nvidia-tesla-a100", Region: "us-central1"},
		}, nil
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})
	resp := callListInstanceTypes(t, h, tools.ListInstanceTypesInput{Provider: "gcp", Region: "us-central1", GPU: true})
	types, _ := resp["instance_types"].([]any)
	it0 := types[0].(map[string]any)
	if it0["gpu_count"] != float64(1) {
		t.Errorf("gpu_count: got %v, want 1", it0["gpu_count"])
	}
	if it0["gpu_type"] != "nvidia-tesla-a100" {
		t.Errorf("gpu_type: got %v, want nvidia-tesla-a100", it0["gpu_type"])
	}
}

func TestListInstanceTypes_MaxResultsDefaultIs25(t *testing.T) {
	// Return 30 instance types; with default max_results the response should cap at 25.
	pvdr := makeGCPMockProvider(nil, nil)
	pvdr.listInstanceTypesFunc = func(_ context.Context, region, _ string, _ int, _ float64, _ bool) ([]models.InstanceTypeInfo, error) {
		items := make([]models.InstanceTypeInfo, 30)
		for i := range items {
			items[i] = models.InstanceTypeInfo{
				InstanceType: fmt.Sprintf("n1-standard-%d", i+1),
				VCPU:         i + 1,
				MemoryGB:     float64(i+1) * 3.75,
				Region:       region,
				Available:    true,
			}
		}
		return items, nil
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	// No max_results specified — should default to 25.
	resp := callListInstanceTypes(t, h, tools.ListInstanceTypesInput{
		Provider: "gcp",
		Region:   "us-central1",
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	if resp["count"] != float64(25) {
		t.Errorf("count: got %v, want 25", resp["count"])
	}
	types, ok := resp["instance_types"].([]any)
	if !ok || len(types) != 25 {
		t.Errorf("instance_types length: got %v, want 25", len(types))
	}
	note, ok := resp["note"].(string)
	if !ok || note == "" {
		t.Fatalf("note field missing or empty when results are truncated: %v", resp["note"])
	}
	if !strings.Contains(note, "25") {
		t.Errorf("note should contain '25', got: %q", note)
	}
	if !strings.Contains(note, "30") {
		t.Errorf("note should contain '30' (total), got: %q", note)
	}
}

func TestListInstanceTypes_MaxResultsRespected(t *testing.T) {
	// Return 10 instance types; request max_results=5.
	pvdr := makeGCPMockProvider(nil, nil)
	pvdr.listInstanceTypesFunc = func(_ context.Context, region, _ string, _ int, _ float64, _ bool) ([]models.InstanceTypeInfo, error) {
		items := make([]models.InstanceTypeInfo, 10)
		for i := range items {
			items[i] = models.InstanceTypeInfo{
				InstanceType: fmt.Sprintf("n1-standard-%d", i+1),
				VCPU:         i + 1,
				MemoryGB:     float64(i+1) * 3.75,
				Region:       region,
				Available:    true,
			}
		}
		return items, nil
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callListInstanceTypes(t, h, tools.ListInstanceTypesInput{
		Provider:   "gcp",
		Region:     "us-central1",
		MaxResults: 5,
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	if resp["count"] != float64(5) {
		t.Errorf("count: got %v, want 5", resp["count"])
	}
	types, ok := resp["instance_types"].([]any)
	if !ok || len(types) != 5 {
		t.Errorf("instance_types length: got %v, want 5", len(types))
	}
	note, ok := resp["note"].(string)
	if !ok || note == "" {
		t.Fatalf("note field missing or empty when results are truncated: %v", resp["note"])
	}
	// Verify exact wording matches Python.
	wantNote := "Showing 5 of 10 results — use filters to narrow"
	if note != wantNote {
		t.Errorf("note wording mismatch:\n  got:  %q\n  want: %q", note, wantNote)
	}
}

func TestListInstanceTypes_NoNoteWhenUnderLimit(t *testing.T) {
	// Return fewer results than max_results — no note field should be present.
	pvdr := makeGCPMockProvider(nil, nil)
	pvdr.listInstanceTypesFunc = func(_ context.Context, region, _ string, _ int, _ float64, _ bool) ([]models.InstanceTypeInfo, error) {
		return []models.InstanceTypeInfo{
			{InstanceType: "n1-standard-1", VCPU: 1, MemoryGB: 3.75, Region: region, Available: true},
			{InstanceType: "n1-standard-2", VCPU: 2, MemoryGB: 7.5, Region: region, Available: true},
		}, nil
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callListInstanceTypes(t, h, tools.ListInstanceTypesInput{
		Provider:   "gcp",
		Region:     "us-central1",
		MaxResults: 25,
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	if resp["count"] != float64(2) {
		t.Errorf("count: got %v, want 2", resp["count"])
	}
	if _, ok := resp["note"]; ok {
		t.Errorf("note should not be present when results are under limit, got: %v", resp["note"])
	}
}

func TestListInstanceTypes_MaxResultsExactlyAtLimit(t *testing.T) {
	// Return exactly max_results items — no note should be added.
	pvdr := makeGCPMockProvider(nil, nil)
	pvdr.listInstanceTypesFunc = func(_ context.Context, region, _ string, _ int, _ float64, _ bool) ([]models.InstanceTypeInfo, error) {
		items := make([]models.InstanceTypeInfo, 5)
		for i := range items {
			items[i] = models.InstanceTypeInfo{
				InstanceType: fmt.Sprintf("n1-standard-%d", i+1),
				VCPU:         i + 1,
				MemoryGB:     float64(i+1) * 3.75,
				Region:       region,
				Available:    true,
			}
		}
		return items, nil
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callListInstanceTypes(t, h, tools.ListInstanceTypesInput{
		Provider:   "gcp",
		Region:     "us-central1",
		MaxResults: 5,
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	if resp["count"] != float64(5) {
		t.Errorf("count: got %v, want 5", resp["count"])
	}
	if _, ok := resp["note"]; ok {
		t.Errorf("note should not be present when total == max_results, got: %v", resp["note"])
	}
}

// --------------------------------------------------------------------------
// Tests: find_cheapest_region — provider not configured
// --------------------------------------------------------------------------

func TestFindCheapestRegion_ProviderNotConfigured(t *testing.T) {
	h := tools.New(nil)
	resp := callFindCheapestRegion(t, h, tools.FindCheapestRegionInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
	})
	if resp["error"] == nil {
		t.Errorf("expected error, got: %v", resp)
	}
}

func TestFindCheapestRegion_InvalidSpec(t *testing.T) {
	h := tools.New(nil)
	resp := callFindCheapestRegion(t, h, tools.FindCheapestRegionInput{
		Spec: map[string]any{
			// No provider, no domain, no inferrable fields
			"something": "else",
		},
	})
	if resp["error"] == nil {
		t.Errorf("expected error for invalid spec, got: %v", resp)
	}
}

// --------------------------------------------------------------------------
// Tests: find_cheapest_region — GCP major-region scoping (mirrors T22/T23)
// --------------------------------------------------------------------------

// TestFindCheapestRegion_GCP_DefaultsMajorRegions verifies that omitting
// regions defaults to the 12 GCP major regions and includes a note field
// containing "GCP" and "12". Mirrors test_gcp_major_regions.py.
func TestFindCheapestRegion_GCP_DefaultsMajorRegions(t *testing.T) {
	var mu sync.Mutex
	var queriedRegions []string
	pvdr := makeGCPMockProvider(
		func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			mu.Lock()
			queriedRegions = append(queriedRegions, spec.GetRegion())
			mu.Unlock()
			return makeGCPPriceResult(spec.GetRegion(), 0.05), nil
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callFindCheapestRegion(t, h, tools.FindCheapestRegionInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		// No regions — should default to major
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}

	// Should have queried exactly 12 major GCP regions.
	if len(queriedRegions) != 12 {
		t.Errorf("queried %d regions, want 12", len(queriedRegions))
	}

	// note field must be present.
	note, ok := resp["note"].(string)
	if !ok || note == "" {
		t.Fatalf("note field missing or empty: %v", resp["note"])
	}
	if !strings.Contains(note, "GCP") {
		t.Errorf("note should contain 'GCP', got: %q", note)
	}
	if !strings.Contains(note, "12") {
		t.Errorf("note should contain '12', got: %q", note)
	}
	if !strings.Contains(note, "regions=['all']") {
		t.Errorf("note should mention regions=['all'], got: %q", note)
	}
	// Exact wording check (from Python source).
	wantSuffix := "Pass regions=['all'] to search all available regions (slower on first run)."
	if !strings.Contains(note, wantSuffix) {
		t.Errorf("note wording mismatch, got: %q", note)
	}
}

// TestFindCheapestRegion_GCP_AllRegions verifies that regions=["all"] queries
// ALL available regions and does NOT include a note field.
func TestFindCheapestRegion_GCP_AllRegions(t *testing.T) {
	var mu sync.Mutex
	var queriedRegions []string
	pvdr := makeGCPMockProvider(
		func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			mu.Lock()
			queriedRegions = append(queriedRegions, spec.GetRegion())
			mu.Unlock()
			return makeGCPPriceResult(spec.GetRegion(), 0.05), nil
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callFindCheapestRegion(t, h, tools.FindCheapestRegionInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		Regions: []string{"all"},
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}

	// Should have queried more than 12 regions (all available).
	if len(queriedRegions) <= 12 {
		t.Errorf("expected >12 regions queried with ['all'], got %d", len(queriedRegions))
	}

	// note field must NOT be present.
	if _, ok := resp["note"]; ok {
		t.Errorf("note should not be present when regions=['all'], got: %v", resp["note"])
	}
}

// TestFindCheapestRegion_AWS_DefaultsMajorRegions mirrors the AWS major-region test.
func TestFindCheapestRegion_AWS_DefaultsMajorRegions(t *testing.T) {
	var mu sync.Mutex
	var queriedRegions []string
	pvdr := makeAWSMockProvider(
		func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			mu.Lock()
			queriedRegions = append(queriedRegions, spec.GetRegion())
			mu.Unlock()
			return makePriceResult(makePrice(spec.GetRegion(), 0.192)), nil
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	resp := callFindCheapestRegion(t, h, tools.FindCheapestRegionInput{
		Spec: map[string]any{
			"provider":      "aws",
			"domain":        "compute",
			"resource_type": "m5.xlarge",
			"region":        "us-east-1",
		},
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	if len(queriedRegions) != 12 {
		t.Errorf("queried %d regions, want 12", len(queriedRegions))
	}

	note, ok := resp["note"].(string)
	if !ok || note == "" {
		t.Fatalf("note field missing or empty: %v", resp["note"])
	}
	if !strings.Contains(note, "AWS") {
		t.Errorf("note should contain 'AWS', got: %q", note)
	}
	if !strings.Contains(note, "12") {
		t.Errorf("note should contain '12', got: %q", note)
	}
}

// TestFindCheapestRegion_ExplicitRegions verifies explicit region list is used
// as-is, and no note field is added.
func TestFindCheapestRegion_ExplicitRegions(t *testing.T) {
	var mu sync.Mutex
	var queriedRegions []string
	pvdr := makeGCPMockProvider(
		func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			mu.Lock()
			queriedRegions = append(queriedRegions, spec.GetRegion())
			mu.Unlock()
			return makeGCPPriceResult(spec.GetRegion(), 0.05), nil
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	explicit := []string{"us-central1", "us-east1", "europe-west1"}
	resp := callFindCheapestRegion(t, h, tools.FindCheapestRegionInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		Regions: explicit,
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	if len(queriedRegions) != 3 {
		t.Errorf("queried %d regions, want 3", len(queriedRegions))
	}
	if _, ok := resp["note"]; ok {
		t.Errorf("note should not be present for explicit regions, got: %v", resp["note"])
	}
}

// --------------------------------------------------------------------------
// Tests: find_cheapest_region — response shape and sorting
// --------------------------------------------------------------------------

func TestFindCheapestRegion_SortedCheapestFirst(t *testing.T) {
	regionPrices := map[string]float64{
		"us-central1":  0.050,
		"us-east1":     0.040, // cheapest
		"europe-west1": 0.060, // most expensive
	}
	pvdr := makeGCPMockProvider(
		func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			p, ok := regionPrices[spec.GetRegion()]
			if !ok {
				return makePriceResult(), nil
			}
			return makeGCPPriceResult(spec.GetRegion(), p), nil
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callFindCheapestRegion(t, h, tools.FindCheapestRegionInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		Regions: []string{"europe-west1", "us-central1", "us-east1"},
	})

	if resp["cheapest_region"] != "us-east1" {
		t.Errorf("cheapest_region: got %v, want us-east1", resp["cheapest_region"])
	}
	if resp["most_expensive_region"] != "europe-west1" {
		t.Errorf("most_expensive_region: got %v, want europe-west1", resp["most_expensive_region"])
	}

	sorted, ok := resp["all_regions_sorted"].([]any)
	if !ok || len(sorted) != 3 {
		t.Fatalf("all_regions_sorted: expected 3 entries, got %v", resp["all_regions_sorted"])
	}
	if sorted[0].(map[string]any)["region"] != "us-east1" {
		t.Errorf("sorted[0]: want us-east1, got %v", sorted[0])
	}
}

func TestFindCheapestRegion_ResponseShapeFields(t *testing.T) {
	pvdr := makeGCPMockProvider(
		func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makeGCPPriceResult(spec.GetRegion(), 0.05), nil
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callFindCheapestRegion(t, h, tools.FindCheapestRegionInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		Regions: []string{"us-central1", "us-east1"},
	})

	requiredFields := []string{
		"provider", "domain", "service",
		"cheapest_region", "cheapest_region_name", "cheapest_price",
		"most_expensive_region", "most_expensive_price",
		"price_delta_pct", "all_regions_sorted", "not_available_in",
	}
	for _, f := range requiredFields {
		if _, ok := resp[f]; !ok {
			t.Errorf("response missing field %q", f)
		}
	}
}

func TestFindCheapestRegion_NotAvailableNilWhenEmpty(t *testing.T) {
	// All regions return prices — not_available_in should be nil (not an empty list).
	pvdr := makeGCPMockProvider(
		func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makeGCPPriceResult(spec.GetRegion(), 0.05), nil
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callFindCheapestRegion(t, h, tools.FindCheapestRegionInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		Regions: []string{"us-central1", "us-east1"},
	})

	// nil JSON value (not an empty array).
	if resp["not_available_in"] != nil {
		t.Errorf("not_available_in should be nil when all regions have prices, got: %v", resp["not_available_in"])
	}
}

func TestFindCheapestRegion_SomeRegionsNotAvailable(t *testing.T) {
	pvdr := makeGCPMockProvider(
		func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			if spec.GetRegion() == "europe-west1" {
				return makePriceResult(), nil // not available
			}
			return makeGCPPriceResult(spec.GetRegion(), 0.05), nil
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callFindCheapestRegion(t, h, tools.FindCheapestRegionInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		Regions: []string{"us-central1", "europe-west1"},
	})

	notAvail, ok := resp["not_available_in"].([]any)
	if !ok {
		t.Fatalf("not_available_in should be a list, got: %v", resp["not_available_in"])
	}
	if len(notAvail) != 1 || notAvail[0] != "europe-west1" {
		t.Errorf("not_available_in: got %v, want [europe-west1]", notAvail)
	}
}

func TestFindCheapestRegion_NoPricesFound(t *testing.T) {
	pvdr := makeGCPMockProvider(
		func(_ context.Context, _ models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(), nil // empty prices everywhere
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callFindCheapestRegion(t, h, tools.FindCheapestRegionInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		Regions: []string{"us-central1", "us-east1"},
	})

	if resp["result"] != "no_prices_found" {
		t.Errorf("result: got %v, want no_prices_found", resp["result"])
	}
}

func TestFindCheapestRegion_PriceDeltaPct(t *testing.T) {
	// cheapest=0.040, most_expensive=0.060 → delta=(0.060-0.040)/0.040*100=50.0
	pvdr := makeGCPMockProvider(
		func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			prices := map[string]float64{"us-east1": 0.040, "europe-west1": 0.060}
			p := prices[spec.GetRegion()]
			return makeGCPPriceResult(spec.GetRegion(), p), nil
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callFindCheapestRegion(t, h, tools.FindCheapestRegionInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		Regions: []string{"us-east1", "europe-west1"},
	})

	delta, ok := resp["price_delta_pct"].(float64)
	if !ok {
		t.Fatalf("price_delta_pct: got %T %v", resp["price_delta_pct"], resp["price_delta_pct"])
	}
	if delta < 49.9 || delta > 50.1 {
		t.Errorf("price_delta_pct: got %v, want ~50.0", delta)
	}
}

func TestFindCheapestRegion_PriceDeltaPctNilWhenCheapestIsZero(t *testing.T) {
	pvdr := makeGCPMockProvider(
		func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makeGCPPriceResult(spec.GetRegion(), 0.0), nil // zero price
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callFindCheapestRegion(t, h, tools.FindCheapestRegionInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		Regions: []string{"us-central1"},
	})

	if resp["price_delta_pct"] != nil {
		t.Errorf("price_delta_pct should be nil when cheapest price is 0, got: %v", resp["price_delta_pct"])
	}
}

func TestFindCheapestRegion_BaselineRegion(t *testing.T) {
	pvdr := makeGCPMockProvider(
		func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			prices := map[string]float64{"us-central1": 0.050, "europe-west1": 0.070}
			return makeGCPPriceResult(spec.GetRegion(), prices[spec.GetRegion()]), nil
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callFindCheapestRegion(t, h, tools.FindCheapestRegionInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		Regions:        []string{"us-central1", "europe-west1"},
		BaselineRegion: "us-central1",
	})

	if resp["baseline_region"] != "us-central1" {
		t.Errorf("baseline_region: got %v, want us-central1", resp["baseline_region"])
	}
	if resp["baseline_region_name"] == nil {
		t.Error("baseline_region_name missing")
	}
	sorted, _ := resp["all_regions_sorted"].([]any)
	for _, entry := range sorted {
		e := entry.(map[string]any)
		if _, ok := e["delta_per_hour"]; !ok {
			t.Errorf("entry missing delta_per_hour: %v", e)
		}
		if _, ok := e["delta_monthly"]; !ok {
			t.Errorf("entry missing delta_monthly: %v", e)
		}
		if _, ok := e["delta_pct"]; !ok {
			t.Errorf("entry missing delta_pct: %v", e)
		}
	}
}

func TestFindCheapestRegion_MonthlyEstimateIncludedForHourly(t *testing.T) {
	pvdr := makeGCPMockProvider(
		func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makeGCPPriceResult(spec.GetRegion(), 0.05), nil
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callFindCheapestRegion(t, h, tools.FindCheapestRegionInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		Regions: []string{"us-central1"},
	})

	sorted, _ := resp["all_regions_sorted"].([]any)
	e := sorted[0].(map[string]any)
	if e["monthly_estimate"] == nil {
		t.Errorf("monthly_estimate should be present for per_hour unit, entry: %v", e)
	}
	me, ok := e["monthly_estimate"].(map[string]any)
	if !ok {
		t.Fatalf("monthly_estimate not a map: %v", e["monthly_estimate"])
	}
	// 0.05 * 730 = 36.5
	if me["amount"] != 0.05*730 {
		t.Errorf("monthly_estimate.amount: got %v, want %v", me["amount"], 0.05*730)
	}
}

// --------------------------------------------------------------------------
// Tests: find_cheapest_region — provider_not_configured error path
// --------------------------------------------------------------------------

func TestFindCheapestRegion_ProviderNotConfiguredError(t *testing.T) {
	// Simulate a provider that returns a "not configured" error from GetPrice.
	pvdr := makeGCPMockProvider(
		func(_ context.Context, _ models.PricingSpec) (*models.PricingResult, error) {
			return nil, errors.New("credentials not configured")
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callFindCheapestRegion(t, h, tools.FindCheapestRegionInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		Regions: []string{"us-central1"},
	})

	if resp["result"] != "provider_not_configured" {
		t.Errorf("result: got %v, want provider_not_configured", resp["result"])
	}
	if resp["provider"] != "gcp" {
		t.Errorf("provider: got %v, want gcp", resp["provider"])
	}
}

// --------------------------------------------------------------------------
// Tests: find_available_regions
// --------------------------------------------------------------------------

func TestFindAvailableRegions_ProviderNotConfigured(t *testing.T) {
	h := tools.New(nil)
	resp := callFindAvailableRegions(t, h, tools.FindAvailableRegionsInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
	})
	if resp["error"] == nil {
		t.Errorf("expected error, got: %v", resp)
	}
}

// TestFindAvailableRegions_GCP_DefaultsMajorRegions mirrors T22 for find_available_regions.
func TestFindAvailableRegions_GCP_DefaultsMajorRegions(t *testing.T) {
	var mu sync.Mutex
	var queriedRegions []string
	pvdr := makeGCPMockProvider(
		func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			mu.Lock()
			queriedRegions = append(queriedRegions, spec.GetRegion())
			mu.Unlock()
			return makeGCPPriceResult(spec.GetRegion(), 0.05), nil
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callFindAvailableRegions(t, h, tools.FindAvailableRegionsInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	if len(queriedRegions) != 12 {
		t.Errorf("queried %d regions, want 12", len(queriedRegions))
	}

	note, ok := resp["note"].(string)
	if !ok || note == "" {
		t.Fatalf("note field missing: %v", resp["note"])
	}
	if !strings.Contains(note, "GCP") {
		t.Errorf("note should contain 'GCP', got: %q", note)
	}
	if !strings.Contains(note, "12") {
		t.Errorf("note should contain '12', got: %q", note)
	}
	// Exact wording for find_available_regions differs from find_cheapest_region.
	wantSuffix := "Pass regions=['all'] to check all available regions."
	if !strings.Contains(note, wantSuffix) {
		t.Errorf("note wording mismatch, got: %q", note)
	}
}

// TestFindAvailableRegions_GCP_AllRegions verifies regions=["all"] behavior.
func TestFindAvailableRegions_GCP_AllRegions(t *testing.T) {
	var mu sync.Mutex
	var queriedRegions []string
	pvdr := makeGCPMockProvider(
		func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			mu.Lock()
			queriedRegions = append(queriedRegions, spec.GetRegion())
			mu.Unlock()
			return makeGCPPriceResult(spec.GetRegion(), 0.05), nil
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callFindAvailableRegions(t, h, tools.FindAvailableRegionsInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		Regions: []string{"all"},
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	if len(queriedRegions) <= 12 {
		t.Errorf("expected >12 regions with ['all'], got %d", len(queriedRegions))
	}
	if _, ok := resp["note"]; ok {
		t.Errorf("note should not be present with regions=['all'], got: %v", resp["note"])
	}
}

func TestFindAvailableRegions_ResponseShape(t *testing.T) {
	pvdr := makeGCPMockProvider(
		func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			if spec.GetRegion() == "europe-west1" {
				return makePriceResult(), nil // not available
			}
			return makeGCPPriceResult(spec.GetRegion(), 0.05), nil
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callFindAvailableRegions(t, h, tools.FindAvailableRegionsInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		Regions: []string{"us-central1", "us-east1", "europe-west1"},
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	if resp["provider"] != "gcp" {
		t.Errorf("provider: got %v, want gcp", resp["provider"])
	}
	// 2 available, 1 not.
	if resp["available_in"] != float64(2) {
		t.Errorf("available_in: got %v, want 2", resp["available_in"])
	}
	notAvail, ok := resp["not_available_in"].([]any)
	if !ok || len(notAvail) != 1 || notAvail[0] != "europe-west1" {
		t.Errorf("not_available_in: got %v, want [europe-west1]", resp["not_available_in"])
	}
	entries, ok := resp["regions_sorted_cheapest_first"].([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("regions_sorted_cheapest_first: expected 2, got %v", resp["regions_sorted_cheapest_first"])
	}
}

func TestFindAvailableRegions_SortedCheapestFirst(t *testing.T) {
	// Each region uses only its first price — different prices per region.
	pvdr := makeGCPMockProvider(
		func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			prices := map[string]float64{
				"us-central1":  0.050,
				"us-east1":     0.040, // cheapest
				"europe-west1": 0.070, // most expensive
			}
			p := prices[spec.GetRegion()]
			return makeGCPPriceResult(spec.GetRegion(), p), nil
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callFindAvailableRegions(t, h, tools.FindAvailableRegionsInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		Regions: []string{"europe-west1", "us-central1", "us-east1"},
	})

	entries, _ := resp["regions_sorted_cheapest_first"].([]any)
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[0].(map[string]any)["region"] != "us-east1" {
		t.Errorf("sorted[0]: want us-east1, got %v", entries[0])
	}
}

func TestFindAvailableRegions_NotAvailable(t *testing.T) {
	pvdr := makeGCPMockProvider(
		func(_ context.Context, _ models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(), nil // empty everywhere
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callFindAvailableRegions(t, h, tools.FindAvailableRegionsInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		Regions: []string{"us-central1"},
	})

	if resp["result"] != "not_available" {
		t.Errorf("result: got %v, want not_available", resp["result"])
	}
}

func TestFindAvailableRegions_NotAvailableNilWhenEmpty(t *testing.T) {
	pvdr := makeGCPMockProvider(
		func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makeGCPPriceResult(spec.GetRegion(), 0.05), nil
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callFindAvailableRegions(t, h, tools.FindAvailableRegionsInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		Regions: []string{"us-central1", "us-east1"},
	})

	if resp["not_available_in"] != nil {
		t.Errorf("not_available_in should be nil when all regions available, got: %v", resp["not_available_in"])
	}
}

func TestFindAvailableRegions_OnlyFirstPricePerRegion(t *testing.T) {
	// Provider returns two prices per region; find_available_regions should
	// take only the first. We verify by checking the entry count vs price count.
	pvdr := makeGCPMockProvider(
		func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			// Return two prices per region.
			return &models.PricingResult{
				PublicPrices: []models.NormalizedPrice{
					{Provider: models.CloudProviderGCP, Region: spec.GetRegion(), PricePerUnit: 0.05, Unit: models.PriceUnitPerHour, Currency: "USD"},
					{Provider: models.CloudProviderGCP, Region: spec.GetRegion(), PricePerUnit: 0.08, Unit: models.PriceUnitPerHour, Currency: "USD"},
				},
				Source:        "catalog",
				SchemaVersion: "1",
			}, nil
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callFindAvailableRegions(t, h, tools.FindAvailableRegionsInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		Regions: []string{"us-central1", "us-east1"},
	})

	// Even though 2 prices per region, we should have exactly 2 entries (one per region).
	entries, ok := resp["regions_sorted_cheapest_first"].([]any)
	if !ok || len(entries) != 2 {
		t.Errorf("regions_sorted_cheapest_first: expected 2 (one per region), got %v", resp["regions_sorted_cheapest_first"])
	}
}

func TestFindAvailableRegions_BaselineRegion(t *testing.T) {
	pvdr := makeGCPMockProvider(
		func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			prices := map[string]float64{"us-central1": 0.05, "europe-west1": 0.07}
			return makeGCPPriceResult(spec.GetRegion(), prices[spec.GetRegion()]), nil
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callFindAvailableRegions(t, h, tools.FindAvailableRegionsInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		Regions:        []string{"us-central1", "europe-west1"},
		BaselineRegion: "us-central1",
	})

	if resp["baseline_region"] != "us-central1" {
		t.Errorf("baseline_region: got %v, want us-central1", resp["baseline_region"])
	}
	entries, _ := resp["regions_sorted_cheapest_first"].([]any)
	for _, entry := range entries {
		e := entry.(map[string]any)
		if _, ok := e["delta_per_hour"]; !ok {
			t.Errorf("entry missing delta_per_hour: %v", e)
		}
	}
}

// --------------------------------------------------------------------------
// Tests: context cancellation (concurrent fan-out)
// --------------------------------------------------------------------------

// TestFindCheapestRegion_ContextCancellation verifies that when the parent
// context expires mid-flight, the handler returns an error (not a hang).
// The -race detector validates no data races in the goroutine fan-out.
func TestFindCheapestRegion_ContextCancellation(t *testing.T) {
	blockCh := make(chan struct{})
	var started atomic.Int32

	pvdr := makeGCPMockProvider(
		func(ctx context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			started.Add(1)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-blockCh:
				return makeGCPPriceResult(spec.GetRegion(), 0.05), nil
			}
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	regions := []string{"us-central1", "us-east1", "europe-west1", "asia-east1", "australia-southeast1"}
	resp := callFindCheapestRegionCtx(t, ctx, h, tools.FindCheapestRegionInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		Regions: regions,
	})

	// Unblock goroutines after test returns.
	close(blockCh)

	// Should return context_cancelled error.
	if resp["error"] != "context_cancelled" {
		// Also acceptable: no_prices_found (if all goroutines were cancelled before writing).
		if resp["result"] == "no_prices_found" {
			return // acceptable outcome
		}
		t.Errorf("expected context_cancelled or no_prices_found, got: %v", resp)
	}
}

// TestFindAvailableRegions_ContextCancellation mirrors the above for find_available_regions.
func TestFindAvailableRegions_ContextCancellation(t *testing.T) {
	blockCh := make(chan struct{})

	pvdr := makeGCPMockProvider(
		func(ctx context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-blockCh:
				return makeGCPPriceResult(spec.GetRegion(), 0.05), nil
			}
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	regions := []string{"us-central1", "us-east1", "europe-west1", "asia-east1"}
	resp := callFindAvailableRegionsCtx(t, ctx, h, tools.FindAvailableRegionsInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		Regions: regions,
	})

	close(blockCh)

	if resp["error"] != "context_cancelled" {
		if resp["result"] == "not_available" {
			return // acceptable
		}
		t.Errorf("expected context_cancelled or not_available, got: %v", resp)
	}
}

// TestFindCheapestRegion_RaceFreeHighConcurrency stresses the semaphore and
// index-slot pattern with many regions. The -race detector will catch any
// data races between goroutines writing to results[].
func TestFindCheapestRegion_RaceFreeHighConcurrency(t *testing.T) {
	// 50 regions — exceeds the semaphore size of 32 to exercise queuing.
	manyRegions := make([]string, 50)
	for i := range manyRegions {
		manyRegions[i] = "us-test-region-" + string(rune('a'+i%26)) + "-" + string(rune('0'+i/26))
	}

	pvdr := makeGCPMockProvider(
		func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makeGCPPriceResult(spec.GetRegion(), 0.05), nil
		},
		nil,
	)
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callFindCheapestRegion(t, h, tools.FindCheapestRegionInput{
		Spec: map[string]any{
			"provider":      "gcp",
			"domain":        "compute",
			"resource_type": "n1-standard-1",
			"region":        "us-central1",
		},
		Regions: manyRegions,
	})

	// All 50 regions return prices — should not return error.
	if resp["error"] != nil {
		t.Errorf("unexpected error in high-concurrency test: %v", resp)
	}
	if resp["cheapest_region"] == nil {
		t.Error("cheapest_region missing in high-concurrency test")
	}
}

// Compile-time check that mockProviderAvail satisfies the Provider interface.
var _ providers.Provider = (*mockProviderAvail)(nil)
