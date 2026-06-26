// compare_bom_test.go tests the compare_bom tool handler.
package tools_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/tools"
)

// callCompareBOM invokes HandleCompareBOM and decodes the JSON response.
func callCompareBOM(t *testing.T, h *tools.Handler, in tools.CompareBOMInput) map[string]any {
	t.Helper()
	result, _, err := h.HandleCompareBOM(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("HandleCompareBOM returned err: %v", err)
	}
	return decodeResult(t, result)
}

// --------------------------------------------------------------------------
// Test 1: Basic 2-provider compute comparison returns both providers.
// --------------------------------------------------------------------------

// TestCompareBOM_TwoProviders verifies that requesting AWS and GCP returns
// both providers in the comparison map, each with on_demand totals.
func TestCompareBOM_TwoProviders(t *testing.T) {
	awsPvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		supportsFunc:  func(_ models.PricingDomain, _ string) bool { return true },
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			price := makeComputePrice("aws", spec.GetRegion(), "m5.xlarge", 0.192)
			return &models.PricingResult{PublicPrices: []models.NormalizedPrice{price}}, nil
		},
	}
	gcpPvdr := &mockProvider{
		name:          "gcp",
		defaultRegion: "us-central1",
		supportsFunc:  func(_ models.PricingDomain, _ string) bool { return true },
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			price := makeComputePrice("gcp", spec.GetRegion(), "n2-standard-4", 0.170)
			return &models.PricingResult{PublicPrices: []models.NormalizedPrice{price}}, nil
		},
	}
	h := tools.New(map[string]tools.Provider{
		"aws": awsPvdr,
		"gcp": gcpPvdr,
	})

	resp := callCompareBOM(t, h, tools.CompareBOMInput{
		Providers: []string{"aws", "gcp"},
		Workload: map[string]tools.WorkloadItem{
			"web_server": {Type: "compute", VCPUs: 4, MemoryGB: 16, Quantity: 1},
		},
		Terms: []string{"on_demand"},
	})

	if _, ok := resp["error"]; ok {
		t.Fatalf("expected success, got error: %v", resp["error"])
	}

	cmp, ok := resp["comparison"].(map[string]any)
	if !ok {
		t.Fatalf("expected 'comparison' map, got %T", resp["comparison"])
	}

	// Both providers must be present.
	for _, prov := range []string{"aws", "gcp"} {
		provData, ok := cmp[prov].(map[string]any)
		if !ok {
			t.Errorf("expected provider '%s' in comparison, got %T", prov, cmp[prov])
			continue
		}

		odData, ok := provData["on_demand"].(map[string]any)
		if !ok {
			t.Errorf("provider '%s': expected 'on_demand' term data, got %T", prov, provData["on_demand"])
			continue
		}

		totalMonthly, _ := odData["total_monthly"].(float64)
		if totalMonthly <= 0 {
			t.Errorf("provider '%s': expected positive total_monthly, got %v", prov, totalMonthly)
		}

		breakdown, ok := odData["breakdown"].(map[string]any)
		if !ok || breakdown == nil {
			t.Errorf("provider '%s': expected breakdown map, got %T", prov, odData["breakdown"])
		} else if _, ok := breakdown["web_server"]; !ok {
			t.Errorf("provider '%s': expected 'web_server' in breakdown", prov)
		}
	}
}

// --------------------------------------------------------------------------
// Test 2: Summary string mentions the cheapest provider.
// --------------------------------------------------------------------------

// TestCompareBOM_SummaryMentionsCheapest verifies that the summary string
// names the provider with the lowest total monthly cost.
func TestCompareBOM_SummaryMentionsCheapest(t *testing.T) {
	// GCP is cheaper (0.100/hr) than AWS (0.200/hr).
	awsPvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		supportsFunc:  func(_ models.PricingDomain, _ string) bool { return true },
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			price := makeComputePrice("aws", spec.GetRegion(), "m5.xlarge", 0.200)
			return &models.PricingResult{PublicPrices: []models.NormalizedPrice{price}}, nil
		},
	}
	gcpPvdr := &mockProvider{
		name:          "gcp",
		defaultRegion: "us-central1",
		supportsFunc:  func(_ models.PricingDomain, _ string) bool { return true },
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			price := makeComputePrice("gcp", spec.GetRegion(), "n2-standard-4", 0.100)
			return &models.PricingResult{PublicPrices: []models.NormalizedPrice{price}}, nil
		},
	}
	h := tools.New(map[string]tools.Provider{
		"aws": awsPvdr,
		"gcp": gcpPvdr,
	})

	resp := callCompareBOM(t, h, tools.CompareBOMInput{
		Providers: []string{"aws", "gcp"},
		Workload: map[string]tools.WorkloadItem{
			"app": {Type: "compute", VCPUs: 4, MemoryGB: 16, Quantity: 1},
		},
		Terms: []string{"on_demand"},
	})

	summary, _ := resp["summary"].(string)
	if summary == "" {
		t.Fatal("expected non-empty summary")
	}

	// Summary should mention GCP as cheapest.
	if !strings.Contains(strings.ToUpper(summary), "GCP") {
		t.Errorf("expected summary to mention 'GCP' as cheapest, got: %q", summary)
	}
}

// --------------------------------------------------------------------------
// Test 3: on_demand vs reserved_1yr both appear when requested.
// --------------------------------------------------------------------------

// TestCompareBOM_BothTermsPresent verifies that when terms=["on_demand","reserved_1yr"]
// is requested, both term keys appear in each provider's output.
func TestCompareBOM_BothTermsPresent(t *testing.T) {
	var callCount atomic.Int64
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		supportsFunc:  func(_ models.PricingDomain, _ string) bool { return true },
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			callCount.Add(1)
			var pricePerHour float64
			if spec.GetTerm() == models.PricingTermOnDemand {
				pricePerHour = 0.192
			} else {
				pricePerHour = 0.120 // reserved is cheaper
			}
			price := makeComputePrice("aws", spec.GetRegion(), "m5.xlarge", pricePerHour)
			return &models.PricingResult{PublicPrices: []models.NormalizedPrice{price}}, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	resp := callCompareBOM(t, h, tools.CompareBOMInput{
		Providers: []string{"aws"},
		Workload: map[string]tools.WorkloadItem{
			"web": {Type: "compute", VCPUs: 4, MemoryGB: 16, Quantity: 1},
		},
		Terms: []string{"on_demand", "reserved_1yr"},
	})

	if _, ok := resp["error"]; ok {
		t.Fatalf("expected success, got: %v", resp["error"])
	}

	cmp, ok := resp["comparison"].(map[string]any)
	if !ok {
		t.Fatalf("expected comparison map, got %T", resp["comparison"])
	}

	awsData, ok := cmp["aws"].(map[string]any)
	if !ok {
		t.Fatal("expected aws in comparison")
	}

	// Both terms must be present.
	for _, term := range []string{"on_demand", "reserved_1yr"} {
		termData, ok := awsData[term].(map[string]any)
		if !ok {
			t.Errorf("expected term '%s' in aws output, got %T", term, awsData[term])
			continue
		}
		totalMonthly, _ := termData["total_monthly"].(float64)
		if totalMonthly <= 0 {
			t.Errorf("term '%s': expected positive total_monthly", term)
		}
	}

	// reserved_1yr must have savings_vs_on_demand.
	r1yr, _ := awsData["reserved_1yr"].(map[string]any)
	if r1yr != nil {
		svo, ok := r1yr["savings_vs_on_demand"].(map[string]any)
		if !ok || svo == nil {
			t.Error("expected savings_vs_on_demand in reserved_1yr")
		} else {
			pct, _ := svo["percent"].(float64)
			if pct <= 0 {
				t.Errorf("expected positive savings percent, got %v", pct)
			}
		}
	}
}

// --------------------------------------------------------------------------
// Test 4: Empty workload returns meaningful error.
// --------------------------------------------------------------------------

// TestCompareBOM_EmptyWorkload verifies that an empty workload returns an error.
func TestCompareBOM_EmptyWorkload(t *testing.T) {
	h := tools.New(nil)

	resp := callCompareBOM(t, h, tools.CompareBOMInput{
		Providers: []string{"aws"},
		Workload:  map[string]tools.WorkloadItem{},
		Terms:     []string{"on_demand"},
	})

	errVal, ok := resp["error"]
	if !ok || errVal == "" {
		t.Error("expected error key for empty workload")
	}
}

// --------------------------------------------------------------------------
// Test 5: Single provider works (providers: ["aws"]).
// --------------------------------------------------------------------------

// TestCompareBOM_SingleProvider verifies that requesting only one provider
// returns a comparison with just that provider.
func TestCompareBOM_SingleProvider(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		supportsFunc:  func(_ models.PricingDomain, _ string) bool { return true },
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			price := makeComputePrice("aws", spec.GetRegion(), "m5.xlarge", 0.192)
			return &models.PricingResult{PublicPrices: []models.NormalizedPrice{price}}, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	resp := callCompareBOM(t, h, tools.CompareBOMInput{
		Providers: []string{"aws"},
		Workload: map[string]tools.WorkloadItem{
			"server": {Type: "compute", VCPUs: 4, MemoryGB: 16, Quantity: 2},
		},
		Terms: []string{"on_demand"},
	})

	if _, ok := resp["error"]; ok {
		t.Fatalf("expected success, got: %v", resp["error"])
	}

	cmp, ok := resp["comparison"].(map[string]any)
	if !ok {
		t.Fatalf("expected comparison map, got %T", resp["comparison"])
	}

	if len(cmp) != 1 {
		t.Errorf("expected 1 provider in comparison, got %d", len(cmp))
	}

	awsData, ok := cmp["aws"].(map[string]any)
	if !ok {
		t.Fatal("expected 'aws' in comparison")
	}

	// Verify region is set.
	region, _ := awsData["region"].(string)
	if region == "" {
		t.Error("expected non-empty region in provider output")
	}

	// Verify quantity is factored in (2 × $0.192/hr × 730hr ≈ $280.32).
	odData, ok := awsData["on_demand"].(map[string]any)
	if !ok {
		t.Fatal("expected on_demand data for aws")
	}
	totalMonthly, _ := odData["total_monthly"].(float64)
	if totalMonthly < 270 || totalMonthly > 290 {
		t.Errorf("expected total_monthly ~280.32 for 2x instances, got %.2f", totalMonthly)
	}
}

// --------------------------------------------------------------------------
// Test 6: note field is present in response.
// --------------------------------------------------------------------------

// TestCompareBOM_NotePresent verifies that the response includes a "note" field
// disclosing the approximate nature of instance type mapping.
func TestCompareBOM_NotePresent(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		supportsFunc:  func(_ models.PricingDomain, _ string) bool { return true },
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			price := makeComputePrice("aws", spec.GetRegion(), "m5.xlarge", 0.192)
			return &models.PricingResult{PublicPrices: []models.NormalizedPrice{price}}, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	resp := callCompareBOM(t, h, tools.CompareBOMInput{
		Providers: []string{"aws"},
		Workload: map[string]tools.WorkloadItem{
			"app": {Type: "compute", VCPUs: 4, MemoryGB: 16},
		},
		Terms: []string{"on_demand"},
	})

	note, _ := resp["note"].(string)
	if note == "" {
		t.Error("expected non-empty 'note' field disclosing instance type approximation")
	}
}
