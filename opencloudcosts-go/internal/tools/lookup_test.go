package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	azureprovider "github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers/azure"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/tools"
)

// --------------------------------------------------------------------------
// Mock provider
// --------------------------------------------------------------------------

// mockProvider is a minimal Provider implementation for testing.
// Each call-able method delegates to an injected function, defaulting to
// ErrNotSupported so tests only need to fill in what they exercise.
type mockProvider struct {
	name            string
	defaultRegion   string
	majorRegions    []string
	supportsFunc    func(domain models.PricingDomain, service string) bool
	getPriceFunc    func(ctx context.Context, spec models.PricingSpec) (*models.PricingResult, error)
	searchFunc      func(ctx context.Context, query, region string, max int) ([]models.NormalizedPrice, error)
	spotHistFunc    func(ctx context.Context, spec models.PricingSpec, hours int, az string) (map[string]any, error)
	describeCatFunc func(ctx context.Context) (*models.ProviderCatalog, error)
	discountFunc    func(ctx context.Context) (map[string]any, error)
}

func (m *mockProvider) Name() models.CloudProvider { return models.CloudProvider(m.name) }
func (m *mockProvider) DefaultRegion() string      { return m.defaultRegion }
func (m *mockProvider) MajorRegions() []string     { return m.majorRegions }

func (m *mockProvider) Supports(domain models.PricingDomain, service string) bool {
	if m.supportsFunc != nil {
		return m.supportsFunc(domain, service)
	}
	return true
}

func (m *mockProvider) SupportedTerms(domain models.PricingDomain, service string) []models.PricingTerm {
	return []models.PricingTerm{models.PricingTermOnDemand}
}

func (m *mockProvider) GetPrice(ctx context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
	if m.getPriceFunc != nil {
		return m.getPriceFunc(ctx, spec)
	}
	return nil, providers.ErrNotSupported
}

func (m *mockProvider) GetComputePrice(ctx context.Context, instanceType, region, os string, term models.PricingTerm) ([]models.NormalizedPrice, error) {
	return nil, providers.ErrNotSupported
}

func (m *mockProvider) GetStoragePrice(ctx context.Context, storageType, region string, sizeGB float64) ([]models.NormalizedPrice, error) {
	return nil, providers.ErrNotSupported
}

func (m *mockProvider) SearchPricing(ctx context.Context, query, region string, max int) ([]models.NormalizedPrice, error) {
	if m.searchFunc != nil {
		return m.searchFunc(ctx, query, region, max)
	}
	return nil, providers.ErrNotSupported
}

func (m *mockProvider) ListRegions(ctx context.Context, service string) ([]string, error) {
	return nil, providers.ErrNotSupported
}

func (m *mockProvider) ListInstanceTypes(ctx context.Context, region, family string, minVCPUs int, minMemGB float64, gpu bool) ([]models.InstanceTypeInfo, error) {
	return nil, providers.ErrNotSupported
}

func (m *mockProvider) CheckAvailability(ctx context.Context, service, skuOrType, region string) (bool, []string, error) {
	return false, nil, providers.ErrNotSupported
}

func (m *mockProvider) GetEffectivePrice(ctx context.Context, spec models.PricingSpec) ([]models.EffectivePrice, error) {
	return nil, providers.ErrNotSupported
}

func (m *mockProvider) GetSpotHistory(ctx context.Context, spec models.PricingSpec, hours int, az string) (map[string]any, error) {
	if m.spotHistFunc != nil {
		return m.spotHistFunc(ctx, spec, hours, az)
	}
	return nil, providers.ErrNotSupported
}

func (m *mockProvider) GetDiscountSummary(ctx context.Context) (map[string]any, error) {
	if m.discountFunc != nil {
		return m.discountFunc(ctx)
	}
	return nil, providers.ErrNotSupported
}

func (m *mockProvider) DescribeCatalog(ctx context.Context) (*models.ProviderCatalog, error) {
	if m.describeCatFunc != nil {
		return m.describeCatFunc(ctx)
	}
	return &models.ProviderCatalog{
		Provider: models.CloudProvider(m.name),
		Domains:  []models.PricingDomain{models.PricingDomainCompute},
	}, nil
}

func (m *mockProvider) BOMAdvisories(ctx context.Context, services []string, sampleRegion string) ([]map[string]string, error) {
	return nil, nil
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// callGetPrice invokes the handler and returns the decoded response map.
func callGetPrice(t *testing.T, h *tools.Handler, specMap map[string]any) map[string]any {
	t.Helper()
	in := tools.GetPriceInput{Spec: specMap}
	result, _, err := h.HandleGetPrice(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("HandleGetPrice returned err: %v", err)
	}
	return decodeResult(t, result)
}

func callGetPricesBatch(t *testing.T, h *tools.Handler, in tools.GetPricesBatchInput) map[string]any {
	t.Helper()
	result, _, err := h.HandleGetPricesBatch(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("HandleGetPricesBatch returned err: %v", err)
	}
	return decodeResult(t, result)
}

func callComparePrices(t *testing.T, h *tools.Handler, in tools.ComparePricesInput) map[string]any {
	t.Helper()
	result, _, err := h.HandleComparePrices(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("HandleComparePrices returned err: %v", err)
	}
	return decodeResult(t, result)
}

func callSearchPricing(t *testing.T, h *tools.Handler, in tools.SearchPricingInput) map[string]any {
	t.Helper()
	result, _, err := h.HandleSearchPricing(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("HandleSearchPricing returned err: %v", err)
	}
	return decodeResult(t, result)
}

func callDescribeCatalog(t *testing.T, h *tools.Handler, in tools.DescribeCatalogInput) map[string]any {
	t.Helper()
	result, _, err := h.HandleDescribeCatalog(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("HandleDescribeCatalog returned err: %v", err)
	}
	return decodeResult(t, result)
}

func callGetSpotHistory(t *testing.T, h *tools.Handler, in tools.GetSpotHistoryInput) map[string]any {
	t.Helper()
	result, _, err := h.HandleGetSpotHistory(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("HandleGetSpotHistory returned err: %v", err)
	}
	return decodeResult(t, result)
}

// decodeResult extracts the JSON from the first TextContent block of a CallToolResult.
func decodeResult(t *testing.T, r *mcp.CallToolResult) map[string]any {
	t.Helper()
	if r == nil || len(r.Content) == 0 {
		t.Fatal("empty CallToolResult")
	}
	text, ok := r.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want *TextContent", r.Content[0])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text.Text), &out); err != nil {
		t.Fatalf("response is not JSON: %v\nraw: %s", err, text.Text)
	}
	return out
}

// makePrice constructs a NormalizedPrice for testing.
func makePrice(region string, pricePerUnit float64) models.NormalizedPrice {
	return models.NormalizedPrice{
		Provider:    models.CloudProviderAWS,
		Service:     "compute",
		SKUID:       "TEST-SKU",
		Description: "m5.xlarge Linux",
		Region:      region,
		Attributes: map[string]string{
			"instanceType": "m5.xlarge",
			"vcpu":         "4",
			"memory":       "16 GiB",
		},
		PricingTerm:  models.PricingTermOnDemand,
		PricePerUnit: pricePerUnit,
		Unit:         models.PriceUnitPerHour,
		Currency:     "USD",
	}
}

func makePriceResult(prices ...models.NormalizedPrice) *models.PricingResult {
	return &models.PricingResult{
		PublicPrices:  prices,
		AuthAvailable: false,
		Source:        "catalog",
		SchemaVersion: "1",
	}
}

// --------------------------------------------------------------------------
// Tests: get_price
// --------------------------------------------------------------------------

func TestGetPrice_ProviderNotConfigured(t *testing.T) {
	h := tools.New(nil)
	resp := callGetPrice(t, h, map[string]any{
		"provider": "aws",
		"domain":   "compute",
		"region":   "us-east-1",
	})
	if resp["error"] == nil {
		t.Errorf("expected error key, got: %v", resp)
	}
	errStr, _ := resp["error"].(string)
	if errStr == "" {
		t.Errorf("expected non-empty error string, got: %v", resp["error"])
	}
}

func TestGetPrice_InvalidSpec_MissingDomain(t *testing.T) {
	h := tools.New(nil)
	resp := callGetPrice(t, h, map[string]any{
		"provider": "aws",
		"region":   "us-east-1",
		// No domain, no fields that allow inference
	})
	// Should return invalid_spec since domain cannot be inferred
	if resp["error"] == nil {
		t.Errorf("expected error, got: %v", resp)
	}
}

func TestGetPrice_DomainInferredFromService(t *testing.T) {
	// "rds" → database domain. Provider must then be called with database spec.
	var capturedSpec models.PricingSpec
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			capturedSpec = spec
			return makePriceResult(makePrice("us-east-1", 0.192)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPrice(t, h, map[string]any{
		"provider":      "aws",
		"service":       "rds",
		"resource_type": "db.r5.large",
		"engine":        "MySQL",
		"deployment":    "single-az",
		"region":        "us-east-1",
	})
	if _, ok := resp["error"]; ok {
		t.Fatalf("unexpected error: %v", resp)
	}
	if capturedSpec == nil {
		t.Fatal("provider GetPrice was not called")
	}
	if capturedSpec.GetDomain() != models.PricingDomainDatabase {
		t.Errorf("domain: got %q, want %q", capturedSpec.GetDomain(), models.PricingDomainDatabase)
	}
	if _, ok := resp["public_prices"]; !ok {
		t.Errorf("expected public_prices in response, got: %v", resp)
	}
}

func TestGetPrice_DefaultsRegion(t *testing.T) {
	var capturedRegion string
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-west-2",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			capturedRegion = spec.GetRegion()
			return makePriceResult(makePrice(spec.GetRegion(), 0.192)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPrice(t, h, map[string]any{
		"provider":      "aws",
		"domain":        "compute",
		"resource_type": "m5.xlarge",
		// No region — should default to provider's default
	})
	if _, ok := resp["error"]; ok {
		t.Fatalf("unexpected error: %v", resp)
	}
	if capturedRegion != "us-west-2" {
		t.Errorf("region: got %q, want %q", capturedRegion, "us-west-2")
	}
}

func TestGetPrice_ProviderNotSupported(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		supportsFunc:  func(_ models.PricingDomain, _ string) bool { return false },
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPrice(t, h, map[string]any{
		"provider": "aws",
		"domain":   "compute",
		"region":   "us-east-1",
	})
	if resp["error"] == nil {
		t.Errorf("expected error, got: %v", resp)
	}
	if resp["error"] != "not_supported" {
		t.Errorf("error: got %q, want %q", resp["error"], "not_supported")
	}
}

func TestGetPrice_UpstreamFailure(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, _ models.PricingSpec) (*models.PricingResult, error) {
			return nil, errors.New("transient network error")
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPrice(t, h, map[string]any{
		"provider":      "aws",
		"domain":        "compute",
		"resource_type": "m5.xlarge",
		"region":        "us-east-1",
	})
	if resp["error"] != "upstream_failure" {
		t.Errorf("error: got %q, want %q", resp["error"], "upstream_failure")
	}
	if resp["retryable"] != true {
		t.Errorf("retryable: got %v, want true", resp["retryable"])
	}
}

func TestGetPrice_PublicPricesSummaryShape(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, _ models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(makePrice("us-east-1", 0.192)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPrice(t, h, map[string]any{
		"provider":      "aws",
		"domain":        "compute",
		"resource_type": "m5.xlarge",
		"region":        "us-east-1",
	})

	prices, ok := resp["public_prices"].([]any)
	if !ok || len(prices) == 0 {
		t.Fatalf("public_prices missing or empty: %v", resp)
	}
	p0 := prices[0].(map[string]any)

	// Verify required fields.
	for _, key := range []string{"provider", "description", "region", "region_name", "term", "price", "monthly_estimate"} {
		if _, ok := p0[key]; !ok {
			t.Errorf("public_prices[0] missing key %q", key)
		}
	}

	// price sub-dict.
	price, ok := p0["price"].(map[string]any)
	if !ok {
		t.Fatalf("price field is not a map: %v", p0["price"])
	}
	for _, key := range []string{"amount", "unit", "currency", "display"} {
		if _, ok := price[key]; !ok {
			t.Errorf("price missing key %q", key)
		}
	}
	if price["amount"] != 0.192 {
		t.Errorf("price.amount: got %v, want 0.192", price["amount"])
	}
	if price["currency"] != "USD" {
		t.Errorf("price.currency: got %v, want USD", price["currency"])
	}

	// monthly_estimate sub-dict.
	monthly, ok := p0["monthly_estimate"].(map[string]any)
	if !ok {
		t.Fatalf("monthly_estimate is not a map: %v", p0["monthly_estimate"])
	}
	if monthly["currency"] != "USD" {
		t.Errorf("monthly_estimate.currency: got %v, want USD", monthly["currency"])
	}
	// 0.192 * 730 = 140.16
	if monthly["amount"] != 0.192*730 {
		t.Errorf("monthly_estimate.amount: got %v, want %v", monthly["amount"], 0.192*730)
	}

	if resp["auth_available"] != false {
		t.Errorf("auth_available: got %v, want false", resp["auth_available"])
	}
}

func TestGetPrice_NotSupportedError(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, _ models.PricingSpec) (*models.PricingResult, error) {
			return nil, providers.ErrNotSupported
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPrice(t, h, map[string]any{
		"provider":      "aws",
		"domain":        "compute",
		"resource_type": "m5.xlarge",
		"region":        "us-east-1",
	})
	if resp["error"] != "not_supported" {
		t.Errorf("error: got %q, want %q", resp["error"], "not_supported")
	}
}

// --------------------------------------------------------------------------
// Tests: get_prices_batch
// --------------------------------------------------------------------------

func TestGetPricesBatch_ProviderNotConfigured(t *testing.T) {
	h := tools.New(nil)
	resp := callGetPricesBatch(t, h, tools.GetPricesBatchInput{
		Provider:      "aws",
		InstanceTypes: []string{"m5.xlarge"},
		Region:        "us-east-1",
	})
	if resp["error"] == nil {
		t.Errorf("expected error, got: %v", resp)
	}
}

func TestGetPricesBatch_SingleType(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			rt := ""
			if cs, ok := spec.(*models.ComputePricingSpec); ok {
				rt = cs.ResourceType
			}
			return makePriceResult(models.NormalizedPrice{
				Provider:     models.CloudProviderAWS,
				Description:  rt + " Linux",
				Region:       spec.GetRegion(),
				PricingTerm:  models.PricingTermOnDemand,
				PricePerUnit: 0.192,
				Unit:         models.PriceUnitPerHour,
				Currency:     "USD",
				Attributes: map[string]string{
					"vcpu":   "4",
					"memory": "16 GiB",
				},
			}), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPricesBatch(t, h, tools.GetPricesBatchInput{
		Provider:      "aws",
		InstanceTypes: []string{"m5.xlarge"},
		Region:        "us-east-1",
		OS:            "Linux",
		Term:          "on_demand",
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	if resp["count"] != float64(1) {
		t.Errorf("count: got %v, want 1", resp["count"])
	}
	results, ok := resp["results"].([]any)
	if !ok || len(results) == 0 {
		t.Fatalf("results missing or empty: %v", resp)
	}
	r0 := results[0].(map[string]any)
	if r0["instance_type"] != "m5.xlarge" {
		t.Errorf("instance_type: got %v, want m5.xlarge", r0["instance_type"])
	}
	if r0["price_per_hour"] == nil {
		t.Error("price_per_hour missing")
	}
	if r0["monthly_estimate"] == nil {
		t.Error("monthly_estimate missing")
	}
}

func TestGetPricesBatch_SortedCheapestFirst(t *testing.T) {
	prices := map[string]float64{
		"m5.xlarge": 0.192,
		"c5.xlarge": 0.170,
		"r5.xlarge": 0.252,
	}
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			cs, ok := spec.(*models.ComputePricingSpec)
			if !ok {
				return nil, errors.New("expected ComputePricingSpec")
			}
			p := prices[cs.ResourceType]
			return makePriceResult(models.NormalizedPrice{
				Provider:     models.CloudProviderAWS,
				Description:  cs.ResourceType,
				Region:       spec.GetRegion(),
				PricingTerm:  models.PricingTermOnDemand,
				PricePerUnit: p,
				Unit:         models.PriceUnitPerHour,
				Currency:     "USD",
			}), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPricesBatch(t, h, tools.GetPricesBatchInput{
		Provider:      "aws",
		InstanceTypes: []string{"m5.xlarge", "c5.xlarge", "r5.xlarge"},
		Region:        "us-east-1",
	})

	results, _ := resp["results"].([]any)
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	// Should be sorted: c5.xlarge (0.170), m5.xlarge (0.192), r5.xlarge (0.252)
	wantOrder := []string{"c5.xlarge", "m5.xlarge", "r5.xlarge"}
	for i, wantType := range wantOrder {
		r := results[i].(map[string]any)
		if r["instance_type"] != wantType {
			t.Errorf("results[%d].instance_type: got %v, want %s", i, r["instance_type"], wantType)
		}
	}
}

func TestGetPricesBatch_PartialErrors(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			cs, ok := spec.(*models.ComputePricingSpec)
			if !ok || cs.ResourceType == "invalid.type" {
				return nil, errors.New("not found")
			}
			return makePriceResult(makePrice(spec.GetRegion(), 0.192)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPricesBatch(t, h, tools.GetPricesBatchInput{
		Provider:      "aws",
		InstanceTypes: []string{"m5.xlarge", "invalid.type"},
		Region:        "us-east-1",
	})

	if resp["count"] != float64(1) {
		t.Errorf("count: got %v, want 1", resp["count"])
	}
	errs, ok := resp["errors"].(map[string]any)
	if !ok || errs["invalid.type"] == nil {
		t.Errorf("errors map should contain invalid.type, got: %v", resp["errors"])
	}
}

func TestGetPricesBatch_DefaultOSAndTerm(t *testing.T) {
	var capturedOS, capturedTerm string
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			if cs, ok := spec.(*models.ComputePricingSpec); ok {
				capturedOS = cs.OS
				capturedTerm = string(cs.Term)
			}
			return makePriceResult(makePrice(spec.GetRegion(), 0.1)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	// Call with empty OS/Term — should get defaults.
	callGetPricesBatch(t, h, tools.GetPricesBatchInput{
		Provider:      "aws",
		InstanceTypes: []string{"m5.xlarge"},
		Region:        "us-east-1",
		OS:            "",
		Term:          "",
	})
	if capturedOS != "Linux" {
		t.Errorf("OS: got %q, want %q", capturedOS, "Linux")
	}
	if capturedTerm != "on_demand" {
		t.Errorf("term: got %q, want %q", capturedTerm, "on_demand")
	}
}

// --------------------------------------------------------------------------
// Tests: compare_prices
// --------------------------------------------------------------------------

func TestComparePrices_ProviderNotConfigured(t *testing.T) {
	h := tools.New(nil)
	resp := callComparePrices(t, h, tools.ComparePricesInput{
		Spec:    map[string]any{"provider": "aws", "domain": "compute"},
		Regions: []string{"us-east-1", "eu-west-1"},
	})
	if resp["error"] == nil {
		t.Errorf("expected error, got: %v", resp)
	}
}

func TestComparePrices_NoPricesFound(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, _ models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(), nil // empty prices
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callComparePrices(t, h, tools.ComparePricesInput{
		Spec:    map[string]any{"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge"},
		Regions: []string{"us-east-1", "eu-west-1"},
	})
	if resp["result"] != "no_prices_found" {
		t.Errorf("result: got %v, want no_prices_found", resp["result"])
	}
}

func TestComparePrices_SortedCheapestFirst(t *testing.T) {
	regionPrices := map[string]float64{
		"us-east-1":      0.192,
		"eu-west-1":      0.210,
		"ap-northeast-1": 0.230,
	}
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			rgn := spec.GetRegion()
			p, ok := regionPrices[rgn]
			if !ok {
				return makePriceResult(), nil
			}
			return makePriceResult(makePrice(rgn, p)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callComparePrices(t, h, tools.ComparePricesInput{
		Spec:    map[string]any{"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge"},
		Regions: []string{"ap-northeast-1", "eu-west-1", "us-east-1"},
	})

	if resp["cheapest_region"] != "us-east-1" {
		t.Errorf("cheapest_region: got %v, want us-east-1", resp["cheapest_region"])
	}
	if resp["most_expensive_region"] != "ap-northeast-1" {
		t.Errorf("most_expensive_region: got %v, want ap-northeast-1", resp["most_expensive_region"])
	}

	sorted, ok := resp["all_regions_sorted"].([]any)
	if !ok || len(sorted) != 3 {
		t.Fatalf("all_regions_sorted: expected 3 entries, got %v", resp["all_regions_sorted"])
	}
	r0 := sorted[0].(map[string]any)
	if r0["region"] != "us-east-1" {
		t.Errorf("sorted[0].region: got %v, want us-east-1", r0["region"])
	}
}

func TestComparePrices_PriceDeltaPct(t *testing.T) {
	// cheapest=0.192, most_expensive=0.230 → delta = (0.230-0.192)/0.192*100 = 19.791... → rounds to 19.8
	regionPrices := map[string]float64{
		"us-east-1": 0.192,
		"eu-west-1": 0.230,
	}
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(makePrice(spec.GetRegion(), regionPrices[spec.GetRegion()])), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callComparePrices(t, h, tools.ComparePricesInput{
		Spec:    map[string]any{"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge"},
		Regions: []string{"us-east-1", "eu-west-1"},
	})

	delta, ok := resp["price_delta_pct"].(float64)
	if !ok {
		t.Fatalf("price_delta_pct is not float64: %T %v", resp["price_delta_pct"], resp["price_delta_pct"])
	}
	// 19.791... rounds to 19.8 at 1 decimal
	if delta < 19.7 || delta > 19.9 {
		t.Errorf("price_delta_pct: got %v, want ~19.8", delta)
	}
}

func TestComparePrices_BaselineDelta(t *testing.T) {
	regionPrices := map[string]float64{
		"us-east-1": 0.192,
		"eu-west-1": 0.210,
	}
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(makePrice(spec.GetRegion(), regionPrices[spec.GetRegion()])), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callComparePrices(t, h, tools.ComparePricesInput{
		Spec:           map[string]any{"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge"},
		Regions:        []string{"us-east-1", "eu-west-1"},
		BaselineRegion: "us-east-1",
	})

	if resp["baseline_region"] != "us-east-1" {
		t.Errorf("baseline_region: got %v, want us-east-1", resp["baseline_region"])
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

func TestComparePrices_BaselineRegionNotFound(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(makePrice(spec.GetRegion(), 0.192)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callComparePrices(t, h, tools.ComparePricesInput{
		Spec:           map[string]any{"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge"},
		Regions:        []string{"us-east-1"},
		BaselineRegion: "ap-southeast-1", // not in regions
	})
	if resp["error"] == nil {
		t.Errorf("expected error for missing baseline region, got: %v", resp)
	}
}

func TestComparePrices_NotAvailableRegions(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			if spec.GetRegion() == "sa-east-1" {
				return makePriceResult(), nil // empty — not available
			}
			return makePriceResult(makePrice(spec.GetRegion(), 0.192)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callComparePrices(t, h, tools.ComparePricesInput{
		Spec:    map[string]any{"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge"},
		Regions: []string{"us-east-1", "sa-east-1"},
	})

	notAvail, ok := resp["not_available_in"].([]any)
	if !ok {
		t.Fatalf("not_available_in missing or wrong type: %v", resp["not_available_in"])
	}
	found := false
	for _, r := range notAvail {
		if r == "sa-east-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("sa-east-1 should be in not_available_in: %v", notAvail)
	}
}

func TestComparePrices_ResponseFields(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(makePrice(spec.GetRegion(), 0.192)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callComparePrices(t, h, tools.ComparePricesInput{
		Spec:    map[string]any{"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge"},
		Regions: []string{"us-east-1"},
	})

	for _, key := range []string{
		"provider", "domain", "cheapest_region", "cheapest_price",
		"most_expensive_region", "most_expensive_price", "all_regions_sorted",
	} {
		if _, ok := resp[key]; !ok {
			t.Errorf("response missing key %q", key)
		}
	}
}

// --------------------------------------------------------------------------
// Tests: search_pricing
// --------------------------------------------------------------------------

func TestSearchPricing_ProviderNotConfigured(t *testing.T) {
	h := tools.New(nil)
	resp := callSearchPricing(t, h, tools.SearchPricingInput{
		Provider: "aws",
		Query:    "m5",
	})
	if resp["error"] == nil {
		t.Errorf("expected error, got: %v", resp)
	}
}

func TestSearchPricing_ReturnsResults(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		searchFunc: func(_ context.Context, query, region string, max int) ([]models.NormalizedPrice, error) {
			return []models.NormalizedPrice{makePrice("us-east-1", 0.192)}, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callSearchPricing(t, h, tools.SearchPricingInput{
		Provider: "aws",
		Query:    "m5",
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	if resp["count"] != float64(1) {
		t.Errorf("count: got %v, want 1", resp["count"])
	}
	if resp["region"] != "all" {
		t.Errorf("region: got %v, want \"all\"", resp["region"])
	}
	if resp["tip"] == nil {
		t.Error("tip field missing")
	}
	results, ok := resp["results"].([]any)
	if !ok || len(results) != 1 {
		t.Errorf("results: got %v", resp["results"])
	}
}

// TestSearchPricing_NoResultsHint verifies that empty search results return a
// structured no_results response with a tip mentioning list_services.
func TestSearchPricing_NoResultsHint(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		searchFunc: func(_ context.Context, query, region string, max int) ([]models.NormalizedPrice, error) {
			return nil, nil // empty
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callSearchPricing(t, h, tools.SearchPricingInput{
		Provider: "aws",
		Query:    "nonexistent-instance-xyz",
	})

	if resp["result"] != "no_results" {
		t.Errorf("result: got %v, want no_results", resp["result"])
	}
	tip, _ := resp["tip"].(string)
	if !contains(tip, "list_services") {
		t.Errorf("tip should mention list_services, got: %q", tip)
	}
	if resp["query"] != "nonexistent-instance-xyz" {
		t.Errorf("query: got %v, want nonexistent-instance-xyz", resp["query"])
	}
	// With no region specified, region should be "all".
	if resp["region"] != "all" {
		t.Errorf("region: got %v, want all", resp["region"])
	}
}

// TestSearchPricing_NoResultsWithDomain verifies that when a domain is specified
// and search returns empty results, the tip mentions the domain name.
func TestSearchPricing_NoResultsWithDomain(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		searchFunc: func(_ context.Context, _, _ string, _ int) ([]models.NormalizedPrice, error) {
			return nil, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callSearchPricing(t, h, tools.SearchPricingInput{
		Provider: "aws",
		Query:    "metric",
		Domain:   "cloudwatch",
		Region:   "us-east-1",
	})

	if resp["result"] != "no_results" {
		t.Errorf("result: got %v, want no_results", resp["result"])
	}
	// region was specified, so it should be echoed back.
	if resp["region"] != "us-east-1" {
		t.Errorf("region: got %v, want us-east-1", resp["region"])
	}
	tip, _ := resp["tip"].(string)
	if !contains(tip, "cloudwatch") {
		t.Errorf("tip should mention domain 'cloudwatch', got: %q", tip)
	}
	if !contains(tip, "list_services") {
		t.Errorf("tip should mention list_services, got: %q", tip)
	}
}

func TestSearchPricing_RegionFilter(t *testing.T) {
	var capturedRegion string
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		searchFunc: func(_ context.Context, _, region string, _ int) ([]models.NormalizedPrice, error) {
			capturedRegion = region
			return []models.NormalizedPrice{makePrice(region, 0.1)}, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	callSearchPricing(t, h, tools.SearchPricingInput{
		Provider: "aws",
		Query:    "m5",
		Region:   "eu-west-1",
	})
	if capturedRegion != "eu-west-1" {
		t.Errorf("region passed to provider: got %q, want eu-west-1", capturedRegion)
	}
}

func TestSearchPricing_UpstreamFailure(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		searchFunc: func(_ context.Context, _, _ string, _ int) ([]models.NormalizedPrice, error) {
			return nil, errors.New("network error")
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callSearchPricing(t, h, tools.SearchPricingInput{
		Provider: "aws",
		Query:    "m5",
	})
	if resp["error"] != "upstream_failure" {
		t.Errorf("error: got %v, want upstream_failure", resp["error"])
	}
}

// --------------------------------------------------------------------------
// Tests: describe_catalog
// --------------------------------------------------------------------------

func TestDescribeCatalog_NoArgs_AllProviders(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{})

	// Python returns {"support_matrix": {pname: {domains, services, decision_matrix}}, "tip": "..."}
	matrix, ok := resp["support_matrix"].(map[string]any)
	if !ok {
		t.Fatalf("expected support_matrix map, got: %v", resp)
	}
	if _, ok := matrix["aws"]; !ok {
		t.Error("expected aws in support_matrix")
	}
	if resp["tip"] == nil {
		t.Error("tip field missing")
	}
}

func TestDescribeCatalog_ProviderNotConfigured(t *testing.T) {
	h := tools.New(nil)
	resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{
		Provider: "aws",
	})
	if resp["error"] == nil {
		t.Errorf("expected error, got: %v", resp)
	}
}

func TestDescribeCatalog_ProviderOnly(t *testing.T) {
	catalog := &models.ProviderCatalog{
		Provider: models.CloudProviderAWS,
		Domains:  []models.PricingDomain{models.PricingDomainCompute, models.PricingDomainStorage},
		Services: map[string][]string{
			"compute": {"ec2", "fargate"},
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
	resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{
		Provider: "aws",
	})
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	// Python: provider-only (no domain) → same {"support_matrix": {pname: {domains,...}}, "tip": "..."}
	// structure as no-args mode, but limited to the one requested provider.
	matrix, ok := resp["support_matrix"].(map[string]any)
	if !ok {
		t.Fatalf("expected support_matrix, got: %v", resp)
	}
	entry, ok := matrix["aws"].(map[string]any)
	if !ok {
		t.Fatalf("expected aws entry in support_matrix, got: %v", matrix)
	}
	if entry["domains"] == nil {
		t.Error("expected domains in support_matrix entry")
	}
	if entry["services"] == nil {
		t.Error("expected services in support_matrix entry")
	}
}

func TestDescribeCatalog_ProviderAndDomain(t *testing.T) {
	catalog := &models.ProviderCatalog{
		Provider: models.CloudProviderAWS,
		Domains:  []models.PricingDomain{models.PricingDomainCompute},
		Services: map[string][]string{"compute": {"ec2"}},
		SupportedTerms: map[string][]string{
			"compute": {"on_demand", "reserved_1yr"},
		},
		FilterHints: map[string]map[string]any{
			"compute": {"os": "Linux or Windows"},
		},
		ExampleInvocations: map[string]map[string]any{
			"compute": {
				"provider":      "aws",
				"domain":        "compute",
				"resource_type": "m5.xlarge",
				"region":        "us-east-1",
			},
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
	resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{
		Provider: "aws",
		Domain:   "compute",
	})
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	if resp["provider"] != "aws" {
		t.Errorf("provider: got %v, want aws", resp["provider"])
	}
	if resp["domain"] != "compute" {
		t.Errorf("domain: got %v, want compute", resp["domain"])
	}
	if resp["supported_terms"] == nil {
		t.Error("supported_terms missing")
	}
	if resp["example_invocation"] == nil {
		t.Error("example_invocation missing")
	}
}

func TestDescribeCatalog_UpstreamError(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		describeCatFunc: func(_ context.Context) (*models.ProviderCatalog, error) {
			return nil, errors.New("catalog unavailable")
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	// Provider-only (no domain) → support_matrix mode; error captured per-provider.
	resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{
		Provider: "aws",
	})
	// Python does not fail the whole call — it records the error per provider:
	// {"support_matrix": {"aws": {"error": "catalog unavailable"}}, "tip": "..."}
	matrix, ok := resp["support_matrix"].(map[string]any)
	if !ok {
		t.Fatalf("expected support_matrix, got: %v", resp)
	}
	entry, ok := matrix["aws"].(map[string]any)
	if !ok {
		t.Fatalf("expected aws entry in support_matrix, got: %v", matrix)
	}
	if entry["error"] == nil {
		t.Errorf("expected error in aws entry, got: %v", entry)
	}
}

// --------------------------------------------------------------------------
// Tests: describe_catalog — Azure alias regression
// --------------------------------------------------------------------------

// realAzureProvider returns a minimal azure.Provider sufficient for
// DescribeCatalog, which is purely static and requires no cache or HTTP client.
func realAzureProvider() *azureprovider.Provider {
	return azureprovider.NewProvider(nil, 0, 0)
}

// TestDescribeCatalog_AzureCosmosAlias verifies that describe_catalog with
// service=cosmos (canonical name) returns targeted guidance including
// filter_hints, supported_terms, and an example_invocation, and does not
// panic or return an error.
func TestDescribeCatalog_AzureCosmosAlias(t *testing.T) {
	realAzure := realAzureProvider()
	pvdr := &mockProvider{
		name:          "azure",
		defaultRegion: "eastus",
		describeCatFunc: func(ctx context.Context) (*models.ProviderCatalog, error) {
			return realAzure.DescribeCatalog(ctx)
		},
	}
	h := tools.New(map[string]tools.Provider{"azure": pvdr})
	resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{
		Provider: "azure",
		Domain:   "database",
		Service:  "cosmos",
	})
	if resp["error"] != nil {
		t.Fatalf("unexpected error field: %v", resp)
	}
	if resp["filter_hints"] == nil {
		t.Error("expected filter_hints for database/cosmos, got none")
	}
	if resp["supported_terms"] == nil {
		t.Error("expected supported_terms for database/cosmos, got none")
	}
	if resp["example_invocation"] == nil {
		t.Error("expected example_invocation for database/cosmos, got none")
	}
}

// TestDescribeCatalog_AzureFrontDoorAlias verifies that describe_catalog with
// service=front_door (a user-facing alias for the canonical azure_front_door)
// returns a sane non-error response — either targeted guidance or an
// available_services fallback — and does not panic or terminate the session.
func TestDescribeCatalog_AzureFrontDoorAlias(t *testing.T) {
	realAzure := realAzureProvider()
	pvdr := &mockProvider{
		name:          "azure",
		defaultRegion: "eastus",
		describeCatFunc: func(ctx context.Context) (*models.ProviderCatalog, error) {
			return realAzure.DescribeCatalog(ctx)
		},
	}
	h := tools.New(map[string]tools.Provider{"azure": pvdr})
	resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{
		Provider: "azure",
		Domain:   "network",
		Service:  "front_door",
	})
	if resp["error"] != nil {
		t.Fatalf("unexpected error field: %v", resp)
	}
	// Either targeted guidance (filter_hints/supported_terms) or the
	// available_services fallback is acceptable — what matters is no crash.
	hasGuidance := resp["filter_hints"] != nil || resp["supported_terms"] != nil
	hasFallback := resp["available_services"] != nil
	if !hasGuidance && !hasFallback {
		t.Errorf("expected either guidance fields or available_services fallback, got: %v", resp)
	}
}

// --------------------------------------------------------------------------
// Tests: get_spot_history
// --------------------------------------------------------------------------

func TestGetSpotHistory_ProviderNotConfigured(t *testing.T) {
	h := tools.New(nil)
	resp := callGetSpotHistory(t, h, tools.GetSpotHistoryInput{
		Spec: map[string]any{"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge", "region": "us-east-1"},
	})
	if resp["error"] == nil {
		t.Errorf("expected error, got: %v", resp)
	}
}

func TestGetSpotHistory_NotSupported(t *testing.T) {
	pvdr := &mockProvider{
		name:          "gcp",
		defaultRegion: "us-central1",
		spotHistFunc: func(_ context.Context, _ models.PricingSpec, _ int, _ string) (map[string]any, error) {
			return nil, providers.ErrNotSupported
		},
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})
	resp := callGetSpotHistory(t, h, tools.GetSpotHistoryInput{
		Spec: map[string]any{"provider": "gcp", "domain": "compute", "resource_type": "n1-standard-4", "region": "us-central1"},
	})
	if resp["error"] != "not_supported" {
		t.Errorf("error: got %v, want not_supported", resp["error"])
	}
}

func TestGetSpotHistory_NoData(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		spotHistFunc: func(_ context.Context, _ models.PricingSpec, _ int, _ string) (map[string]any, error) {
			return nil, nil // no data
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetSpotHistory(t, h, tools.GetSpotHistoryInput{
		Spec: map[string]any{"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge", "region": "us-east-1"},
	})
	if resp["result"] != "no_data" {
		t.Errorf("result: got %v, want no_data", resp["result"])
	}
	if resp["message"] == nil {
		t.Error("message field missing")
	}
}

func TestGetSpotHistory_ReturnsData(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		spotHistFunc: func(_ context.Context, spec models.PricingSpec, hours int, az string) (map[string]any, error) {
			return map[string]any{
				"az_stats": map[string]any{
					"us-east-1a": map[string]any{"current": 0.050, "min": 0.045, "max": 0.055},
				},
				"volatility_ratio": 0.182,
				"stability_label":  "stable",
				"recommendation":   "Good candidate for spot.",
				"region":           spec.GetRegion(),
				"hours":            hours,
			}, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetSpotHistory(t, h, tools.GetSpotHistoryInput{
		Spec:             map[string]any{"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge", "region": "us-east-1"},
		Hours:            48,
		AvailabilityZone: "us-east-1a",
	})

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	// The handler augments the result with provider and region_name.
	if resp["provider"] != "aws" {
		t.Errorf("provider: got %v, want aws", resp["provider"])
	}
	if resp["region_name"] == nil {
		t.Error("region_name missing")
	}
}

func TestGetSpotHistory_DefaultHours(t *testing.T) {
	var capturedHours int
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		spotHistFunc: func(_ context.Context, _ models.PricingSpec, hours int, _ string) (map[string]any, error) {
			capturedHours = hours
			return map[string]any{"done": true}, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	callGetSpotHistory(t, h, tools.GetSpotHistoryInput{
		Spec:  map[string]any{"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge", "region": "us-east-1"},
		Hours: 0, // should default to 24
	})
	if capturedHours != 24 {
		t.Errorf("hours: got %d, want 24", capturedHours)
	}
}

// --------------------------------------------------------------------------
// Tests: price formatting helpers (unit tests of package internals via export)
// --------------------------------------------------------------------------

func TestPriceFormatting_SmallAmount(t *testing.T) {
	// Values < 5e-7 should use scientific notation.
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, _ models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(models.NormalizedPrice{
				Provider:     models.CloudProviderAWS,
				Description:  "tiny",
				Region:       "us-east-1",
				PricingTerm:  models.PricingTermOnDemand,
				PricePerUnit: 0.0000001, // < 5e-7
				Unit:         models.PriceUnitPerHour,
				Currency:     "USD",
			}), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPrice(t, h, map[string]any{
		"provider":      "aws",
		"domain":        "compute",
		"resource_type": "t1.nano",
		"region":        "us-east-1",
	})
	prices, _ := resp["public_prices"].([]any)
	p0 := prices[0].(map[string]any)
	price := p0["price"].(map[string]any)
	display, _ := price["display"].(string)
	if !contains(display, "e") && !contains(display, "E") {
		t.Errorf("tiny price display should use scientific notation, got: %q", display)
	}
}

func TestPriceFormatting_NormalAmount(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, _ models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(makePrice("us-east-1", 0.192)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPrice(t, h, map[string]any{
		"provider":      "aws",
		"domain":        "compute",
		"resource_type": "m5.xlarge",
		"region":        "us-east-1",
	})
	prices, _ := resp["public_prices"].([]any)
	p0 := prices[0].(map[string]any)
	price := p0["price"].(map[string]any)
	display, _ := price["display"].(string)
	// Should be "$0.192000/per_hour"
	if !contains(display, "0.192000") {
		t.Errorf("display: got %q, expected to contain 0.192000", display)
	}
}

// --------------------------------------------------------------------------
// Tests: fillDomain inference (tested via get_price integration)
// --------------------------------------------------------------------------

func TestFillDomain_StorageTypeInference(t *testing.T) {
	var capturedDomain models.PricingDomain
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			capturedDomain = spec.GetDomain()
			return makePriceResult(models.NormalizedPrice{
				Provider:     models.CloudProviderAWS,
				Description:  "gp3",
				Region:       spec.GetRegion(),
				PricingTerm:  models.PricingTermOnDemand,
				PricePerUnit: 0.08,
				Unit:         models.PriceUnitPerGBMonth,
				Currency:     "USD",
			}), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	callGetPrice(t, h, map[string]any{
		"provider":     "aws",
		"storage_type": "gp3",
		"region":       "us-east-1",
		// no domain — should infer storage
	})
	if capturedDomain != models.PricingDomainStorage {
		t.Errorf("domain: got %q, want storage", capturedDomain)
	}
}

func TestFillDomain_ServiceInference_RDS(t *testing.T) {
	var capturedDomain models.PricingDomain
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			capturedDomain = spec.GetDomain()
			return makePriceResult(makePrice(spec.GetRegion(), 0.192)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	callGetPrice(t, h, map[string]any{
		"provider":      "aws",
		"service":       "rds",
		"resource_type": "db.r5.large",
		"region":        "us-east-1",
		// no domain — should infer database
	})
	if capturedDomain != models.PricingDomainDatabase {
		t.Errorf("domain: got %q, want database", capturedDomain)
	}
}

func TestFillDomain_ResourceTypeDotInference(t *testing.T) {
	var capturedDomain models.PricingDomain
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			capturedDomain = spec.GetDomain()
			return makePriceResult(makePrice(spec.GetRegion(), 0.192)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	callGetPrice(t, h, map[string]any{
		"provider":      "aws",
		"resource_type": "m5.xlarge",
		"region":        "us-east-1",
		// no domain — "m5.xlarge" has a dot → compute
	})
	if capturedDomain != models.PricingDomainCompute {
		t.Errorf("domain: got %q, want compute", capturedDomain)
	}
}

func TestFillDomain_AzureResourceTypeInference(t *testing.T) {
	var capturedDomain models.PricingDomain
	pvdr := &mockProvider{
		name:          "azure",
		defaultRegion: "eastus",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			capturedDomain = spec.GetDomain()
			return makePriceResult(models.NormalizedPrice{
				Provider:     models.CloudProviderAzure,
				Description:  "Standard_D4s_v3",
				Region:       spec.GetRegion(),
				PricingTerm:  models.PricingTermOnDemand,
				PricePerUnit: 0.192,
				Unit:         models.PriceUnitPerHour,
				Currency:     "USD",
			}), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"azure": pvdr})
	callGetPrice(t, h, map[string]any{
		"provider":      "azure",
		"resource_type": "Standard_D4s_v3",
		"region":        "eastus",
		// no domain — "Standard_" prefix → compute
	})
	if capturedDomain != models.PricingDomainCompute {
		t.Errorf("domain: got %q, want compute", capturedDomain)
	}
}

// TestGetPrice_EmptyPublicPrices_NoResultsHint verifies that when GetPrice returns
// nil (provider found no pricing), the tool emits a no_results response with a
// tip pointing to search_pricing and list_services.
func TestGetPrice_EmptyPublicPrices_NoResultsHint(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, _ models.PricingSpec) (*models.PricingResult, error) {
			return nil, nil // provider returned nothing
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPrice(t, h, map[string]any{
		"provider":      "aws",
		"domain":        "compute",
		"resource_type": "m5.xlarge",
		"region":        "us-east-1",
	})

	if resp["result"] != "no_results" {
		t.Errorf("result: got %v, want no_results", resp["result"])
	}
	tip, _ := resp["tip"].(string)
	if !contains(tip, "search_pricing") {
		t.Errorf("tip should mention search_pricing, got: %q", tip)
	}
	if !contains(tip, "list_services") {
		t.Errorf("tip should mention list_services, got: %q", tip)
	}
	if resp["provider"] != "aws" {
		t.Errorf("provider: got %v, want aws", resp["provider"])
	}
}

// TestSearchPricing_NonEmptyUnchanged verifies that non-empty search results
// return the normal {count, results, tip} structure without a no_results sentinel.
func TestSearchPricing_NonEmptyUnchanged(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		searchFunc: func(_ context.Context, _, _ string, _ int) ([]models.NormalizedPrice, error) {
			return []models.NormalizedPrice{makePrice("us-east-1", 0.192)}, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callSearchPricing(t, h, tools.SearchPricingInput{
		Provider: "aws",
		Query:    "m5",
	})

	if resp["result"] == "no_results" {
		t.Errorf("non-empty results should not produce no_results sentinel")
	}
	if resp["count"] != float64(1) {
		t.Errorf("count: got %v, want 1", resp["count"])
	}
	results, ok := resp["results"].([]any)
	if !ok || len(results) != 1 {
		t.Errorf("results: expected 1 item, got %v", resp["results"])
	}
}

// --------------------------------------------------------------------------
// Tests: Handler.New with nil and non-nil provider maps
// --------------------------------------------------------------------------

func TestNew_NilProviders(t *testing.T) {
	// Must not panic; should return "not configured" errors.
	h := tools.New(nil)
	resp := callGetPrice(t, h, map[string]any{
		"provider": "aws",
		"domain":   "compute",
		"region":   "us-east-1",
	})
	if resp["error"] == nil {
		t.Errorf("expected error for nil providers, got: %v", resp)
	}
}

func TestNew_MultipleProviders(t *testing.T) {
	awsPvdr := &mockProvider{name: "aws", defaultRegion: "us-east-1"}
	gcpPvdr := &mockProvider{name: "gcp", defaultRegion: "us-central1"}
	h := tools.New(map[string]tools.Provider{
		"aws": awsPvdr,
		"gcp": gcpPvdr,
	})

	// Both should appear in the support_matrix when no args are given.
	for _, tc := range []struct{ provider, region string }{
		{"aws", "us-east-1"},
		{"gcp", "us-central1"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{})
			matrix, _ := resp["support_matrix"].(map[string]any)
			if _, ok := matrix[tc.provider]; !ok {
				t.Errorf("expected %s in support_matrix", tc.provider)
			}
		})
	}
}

// --------------------------------------------------------------------------
// Helper
// --------------------------------------------------------------------------

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) &&
		(s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Compile-time check that mockProvider satisfies the Provider interface.
var _ providers.Provider = (*mockProvider)(nil)
