package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/config"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers"
	awsprovider "github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers/aws"
	azureprovider "github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers/azure"
	gcpprovider "github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers/gcp"
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

// assertStringsField asserts that resp[key] decodes (via JSON round-trip) to
// exactly the given string slice — used to check "regions" echoed back on
// fan-out tool error/result maps (region-in-errors fix).
func assertStringsField(t *testing.T, resp map[string]any, key string, want []string) {
	t.Helper()
	raw, ok := resp[key]
	if !ok {
		t.Fatalf("expected %q key in response, got: %v", key, resp)
	}
	rawSlice, ok := raw.([]any)
	if !ok {
		t.Fatalf("%q: got %T, want []any", key, raw)
	}
	got := make([]string, len(rawSlice))
	for i, v := range rawSlice {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("%q[%d]: got %T, want string", key, i, v)
		}
		got[i] = s
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%q: got %v, want %v", key, got, want)
	}
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

// makeFallbackPrice is makePrice with Attributes["fallback"] set to "true",
// mirroring how AWS provider static-fallback paths (e.g. ebsGBStaticFallback,
// ebsIOPSStaticFallback) tag their results.
func makeFallbackPrice(region string, pricePerUnit float64) models.NormalizedPrice {
	p := makePrice(region, pricePerUnit)
	p.Attributes["fallback"] = "true"
	return p
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
	if resp["region"] != "us-east-1" {
		t.Errorf("region: got %v, want %q", resp["region"], "us-east-1")
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
	if resp["region"] != "us-east-1" {
		t.Errorf("region: got %v, want %q", resp["region"], "us-east-1")
	}
}

// TestGetPrice_NotConfigured_IncludesRegion exercises the isNotConfigured(err)
// branch (distinct from the pvdr==nil branch covered by
// TestGetPrice_ProviderNotConfigured) and asserts the region string that was
// in scope for the lookup is echoed back so a caller has something to quote
// (region-in-errors fix).
func TestGetPrice_NotConfigured_IncludesRegion(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, _ models.PricingSpec) (*models.PricingResult, error) {
			return nil, errors.New("credentials not configured")
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPrice(t, h, map[string]any{
		"provider":      "aws",
		"domain":        "compute",
		"resource_type": "m5.xlarge",
		"region":        "us-east-1",
	})
	if resp["error"] != "not_configured" {
		t.Errorf("error: got %q, want %q", resp["error"], "not_configured")
	}
	if resp["region"] != "us-east-1" {
		t.Errorf("region: got %v, want %q", resp["region"], "us-east-1")
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
	for _, key := range []string{"provider", "description", "region", "region_name", "term", "price_per_unit", "monthly_estimate"} {
		if _, ok := p0[key]; !ok {
			t.Errorf("public_prices[0] missing key %q", key)
		}
	}

	// price_per_unit sub-dict.
	price, ok := p0["price_per_unit"].(map[string]any)
	if !ok {
		t.Fatalf("price_per_unit field is not a map: %v", p0["price_per_unit"])
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

// --------------------------------------------------------------------------
// Tests: get_price / compare_prices — RC3-010 top-level fallback warning
// --------------------------------------------------------------------------

// TestGetPrice_AllFallback_SurfacesWarning covers RC3-010 for get_price:
// when every price in public_prices is static/fallback data, the response
// must carry a top-level "warnings" entry saying so, rather than presenting
// the fallback price with no indication it might be stale/inaccurate.
func TestGetPrice_AllFallback_SurfacesWarning(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(makeFallbackPrice(spec.GetRegion(), 0.08)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPrice(t, h, map[string]any{
		"provider":      "aws",
		"domain":        "storage",
		"resource_type": "gp3",
		"region":        "eu-central-2",
	})

	warnings, ok := resp["warnings"].([]any)
	if !ok || len(warnings) == 0 {
		t.Fatalf("expected non-empty warnings for all-fallback result, got: %v", resp["warnings"])
	}
	found := false
	for _, w := range warnings {
		if s, ok := w.(string); ok && strings.Contains(s, "fallback") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings do not mention fallback/static data: %v", warnings)
	}
}

// TestGetPrice_NotFallback_NoWarning is the negative counterpart: a live
// (non-fallback) price must not trigger the RC3-010 warning.
func TestGetPrice_NotFallback_NoWarning(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(makePrice(spec.GetRegion(), 0.192)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPrice(t, h, map[string]any{
		"provider":      "aws",
		"domain":        "compute",
		"resource_type": "m5.xlarge",
		"region":        "us-east-1",
	})

	if _, ok := resp["warnings"]; ok {
		t.Errorf("expected no warnings key for a live (non-fallback) price, got: %v", resp["warnings"])
	}
}

// TestGetPrice_StorageWithSizeGB_SurfacesMonthlyEstimate covers RC3-032 (#30)
// part (b): a storage spec (per_gb_month unit) with size_gb set must get a
// monthly_estimate scaled by size_gb, exactly mirroring how per_hour/
// per_month prices already surface one — otherwise callers have no way to
// tell whether price_per_unit is a unit rate or a total.
func TestGetPrice_StorageWithSizeGB_SurfacesMonthlyEstimate(t *testing.T) {
	pvdr := &mockProvider{
		name:          "gcp",
		defaultRegion: "us-central1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(makeGCSStoragePrice(spec.GetRegion(), 0.020)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})
	resp := callGetPrice(t, h, map[string]any{
		"provider":      "gcp",
		"domain":        "storage",
		"resource_type": "gcs_standard",
		"region":        "us-central1",
		"size_gb":       500,
	})

	prices, ok := resp["public_prices"].([]any)
	if !ok || len(prices) != 1 {
		t.Fatalf("public_prices: got %v, want a single-element slice", resp["public_prices"])
	}
	p0 := prices[0].(map[string]any)

	monthly, ok := p0["monthly_estimate"].(map[string]any)
	if !ok {
		t.Fatalf("monthly_estimate is not a map: %v", p0["monthly_estimate"])
	}
	if monthly["currency"] != "USD" {
		t.Errorf("monthly_estimate.currency: got %v, want USD", monthly["currency"])
	}
	// price_per_unit (0.020/GB-month) * size_gb (500) = 10.0.
	if monthly["amount"] != 0.020*500 {
		t.Errorf("monthly_estimate.amount: got %v, want %v", monthly["amount"], 0.020*500)
	}
}

// TestGetPrice_StorageWithoutSizeGB_NoMonthlyEstimate is the negative
// counterpart: omitting size_gb from a storage spec must NOT produce a
// monthly_estimate (unchanged from current behavior) — there's nothing to
// scale price_per_unit by.
func TestGetPrice_StorageWithoutSizeGB_NoMonthlyEstimate(t *testing.T) {
	pvdr := &mockProvider{
		name:          "gcp",
		defaultRegion: "us-central1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(makeGCSStoragePrice(spec.GetRegion(), 0.020)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})
	resp := callGetPrice(t, h, map[string]any{
		"provider":      "gcp",
		"domain":        "storage",
		"resource_type": "gcs_standard",
		"region":        "us-central1",
	})

	prices, ok := resp["public_prices"].([]any)
	if !ok || len(prices) != 1 {
		t.Fatalf("public_prices: got %v, want a single-element slice", resp["public_prices"])
	}
	p0 := prices[0].(map[string]any)
	if _, ok := p0["monthly_estimate"]; ok {
		t.Errorf("expected no monthly_estimate when size_gb is omitted, got: %v", p0["monthly_estimate"])
	}
}

// TestGetPrice_StorageWithIOPS_SurfacesMonthlyEstimate covers the
// per_iops_month generalization of RC3-032 (#30) part (b): a storage spec
// with iops set must get a monthly_estimate scaled by iops, the same way
// per_gb_month prices already get one scaled by size_gb — real providers
// (e.g. AWS io2) price provisioned IOPS this way.
func TestGetPrice_StorageWithIOPS_SurfacesMonthlyEstimate(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(models.NormalizedPrice{
				Provider:      models.CloudProviderAWS,
				Service:       "storage",
				SKUID:         fmt.Sprintf("aws:ebs:io2:iops:%s", spec.GetRegion()),
				ProductFamily: "Storage",
				Description:   "io2 Provisioned IOPS SSD IOPS-months",
				Region:        spec.GetRegion(),
				PricingTerm:   models.PricingTermOnDemand,
				PricePerUnit:  0.065,
				Unit:          models.PriceUnitPerIOPSMonth,
				Currency:      "USD",
			}), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPrice(t, h, map[string]any{
		"provider":      "aws",
		"domain":        "storage",
		"resource_type": "io2",
		"region":        "us-east-1",
		"iops":          1000,
	})

	prices, ok := resp["public_prices"].([]any)
	if !ok || len(prices) != 1 {
		t.Fatalf("public_prices: got %v, want a single-element slice", resp["public_prices"])
	}
	p0 := prices[0].(map[string]any)

	monthly, ok := p0["monthly_estimate"].(map[string]any)
	if !ok {
		t.Fatalf("monthly_estimate is not a map: %v", p0["monthly_estimate"])
	}
	if monthly["currency"] != "USD" {
		t.Errorf("monthly_estimate.currency: got %v, want USD", monthly["currency"])
	}
	// price_per_unit (0.065/IOPS-month) * iops (1000) = 65.0.
	if monthly["amount"] != 0.065*1000 {
		t.Errorf("monthly_estimate.amount: got %v, want %v", monthly["amount"], 0.065*1000)
	}
}

// TestGetPrice_StorageWithMBPS_SurfacesMonthlyEstimate covers the
// per_mbps_month generalization of RC3-032 (#30) part (b): a storage spec
// with throughput_mbps set must get a monthly_estimate scaled by
// throughput_mbps — real providers (e.g. GCP Hyperdisk) price provisioned
// throughput this way.
func TestGetPrice_StorageWithMBPS_SurfacesMonthlyEstimate(t *testing.T) {
	pvdr := &mockProvider{
		name:          "gcp",
		defaultRegion: "us-central1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(models.NormalizedPrice{
				Provider:      models.CloudProviderGCP,
				Service:       "storage",
				SKUID:         fmt.Sprintf("gcp:hyperdisk:throughput:%s", spec.GetRegion()),
				ProductFamily: "Persistent Disk",
				Description:   "Hyperdisk Balanced provisioned throughput",
				Region:        spec.GetRegion(),
				PricingTerm:   models.PricingTermOnDemand,
				PricePerUnit:  0.048,
				Unit:          models.PriceUnitPerMBPSMonth,
				Currency:      "USD",
			}), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})
	resp := callGetPrice(t, h, map[string]any{
		"provider":        "gcp",
		"domain":          "storage",
		"resource_type":   "hyperdisk_balanced",
		"region":          "us-central1",
		"throughput_mbps": 250,
	})

	prices, ok := resp["public_prices"].([]any)
	if !ok || len(prices) != 1 {
		t.Fatalf("public_prices: got %v, want a single-element slice", resp["public_prices"])
	}
	p0 := prices[0].(map[string]any)

	monthly, ok := p0["monthly_estimate"].(map[string]any)
	if !ok {
		t.Fatalf("monthly_estimate is not a map: %v", p0["monthly_estimate"])
	}
	if monthly["currency"] != "USD" {
		t.Errorf("monthly_estimate.currency: got %v, want USD", monthly["currency"])
	}
	// price_per_unit (0.048/MBPS-month) * throughput_mbps (250) = 12.0.
	if monthly["amount"] != 0.048*250 {
		t.Errorf("monthly_estimate.amount: got %v, want %v", monthly["amount"], 0.048*250)
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

// TestGetPrice_WrappedNotSupportedError verifies that a provider error which
// *wraps* providers.ErrNotSupported (e.g. via fmt.Errorf("...: %w", ...), as
// GCP's GetComputePrice does for unrecognized machine families) still
// classifies as a clean not_supported response rather than a retryable
// upstream_failure (RC3-013). Before the fix, lookup.go compared errors with
// `err == providers.ErrNotSupported`, which is false for a wrapped error, so
// this would have fallen through to upstream_failure/retryable:true.
func TestGetPrice_WrappedNotSupportedError(t *testing.T) {
	pvdr := &mockProvider{
		name:          "gcp",
		defaultRegion: "us-central1",
		getPriceFunc: func(_ context.Context, _ models.PricingSpec) (*models.PricingResult, error) {
			return nil, fmt.Errorf("gcp: machine family %q is not in the supported catalog: %w", "z9", providers.ErrNotSupported)
		},
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})
	resp := callGetPrice(t, h, map[string]any{
		"provider":      "gcp",
		"domain":        "compute",
		"resource_type": "z9-standard-4",
		"region":        "us-central1",
	})
	if resp["error"] != "not_supported" {
		t.Errorf("error: got %q, want %q", resp["error"], "not_supported")
	}
	if resp["retryable"] == true {
		t.Errorf("retryable: got %v, want not true (not_supported is not retryable)", resp["retryable"])
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
	if resp["region"] != "us-east-1" {
		t.Errorf("region: got %v, want %q", resp["region"], "us-east-1")
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
	if r0["price_per_unit"] == nil {
		t.Error("price_per_unit missing")
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

// TestGetPricesBatch_NotAvailableIn verifies that instance types with no
// pricing data (as opposed to transient/retryable failures) are surfaced in
// a top-level not_available_in list, using the same field name/shape as the
// other fan-out lookup tools (compare_prices, find_cheapest_region,
// find_available_regions), alongside the existing per-item errors map
// (RC3-018 / #37).
func TestGetPricesBatch_NotAvailableIn(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			cs, ok := spec.(*models.ComputePricingSpec)
			if !ok || cs.ResourceType == "no.such.type" {
				return nil, nil // provider found nothing — genuine no-data
			}
			return makePriceResult(makePrice(spec.GetRegion(), 0.192)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPricesBatch(t, h, tools.GetPricesBatchInput{
		Provider:      "aws",
		InstanceTypes: []string{"m5.xlarge", "no.such.type"},
		Region:        "us-east-1",
	})

	if resp["count"] != float64(1) {
		t.Errorf("count: got %v, want 1", resp["count"])
	}
	notAvail, ok := resp["not_available_in"].([]any)
	if !ok || len(notAvail) != 1 {
		t.Fatalf("not_available_in missing or wrong shape: %v", resp["not_available_in"])
	}
	if notAvail[0] != "no.such.type" {
		t.Errorf("not_available_in[0]: got %v, want no.such.type", notAvail[0])
	}
}

// TestGetPricesBatch_AllOK_NoNotAvailableIn verifies that not_available_in
// is absent when every requested instance type resolves successfully.
func TestGetPricesBatch_AllOK_NoNotAvailableIn(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(makePrice(spec.GetRegion(), 0.192)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPricesBatch(t, h, tools.GetPricesBatchInput{
		Provider:      "aws",
		InstanceTypes: []string{"m5.xlarge", "c5.xlarge"},
		Region:        "us-east-1",
	})
	if _, present := resp["not_available_in"]; present {
		t.Errorf("not_available_in should be absent when all types resolve, got: %v", resp["not_available_in"])
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
	assertStringsField(t, resp, "regions", []string{"us-east-1", "eu-west-1"})
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
	assertStringsField(t, resp, "regions", []string{"us-east-1", "eu-west-1"})
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
	// RC3-002: a baseline_region that is absent from the fetched entries must
	// degrade gracefully — the partial payload is still returned (with a
	// baseline_missing warning and null deltas), not replaced by a bare error.
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
	if resp["error"] != nil {
		t.Errorf("expected no top-level error, got: %v", resp["error"])
	}
	if resp["baseline_missing"] != true {
		t.Errorf("baseline_missing: got %v, want true", resp["baseline_missing"])
	}
	if resp["cheapest_region"] != "us-east-1" {
		t.Errorf("cheapest_region: got %v, want us-east-1 (partial payload should survive)", resp["cheapest_region"])
	}
	sorted, ok := resp["all_regions_sorted"].([]any)
	if !ok || len(sorted) != 1 {
		t.Fatalf("all_regions_sorted: expected 1 entry, got %v", resp["all_regions_sorted"])
	}
	e := sorted[0].(map[string]any)
	// Delta fields must be present-and-null, not omitted — use comma-ok so an
	// omitted key (pre-fix shape) is distinguished from an explicit null.
	for _, key := range []string{"delta_per_hour", "delta_monthly", "delta_pct"} {
		v, ok := e[key]
		if !ok {
			t.Errorf("entry missing key %q entirely (want present with null value)", key)
		}
		if v != nil {
			t.Errorf("entry[%q]: got %v, want null", key, v)
		}
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

// TestComparePrices_AllFallback_SurfacesWarning covers RC3-010: when every
// region resolves to fallback/static data (the reported case — compare_prices
// on EBS gp3 across regions all returning $0.08 with fallback="true"),
// cheapest_region/price_delta_pct are still returned (backward compatible)
// but a top-level warnings entry and ranking_low_confidence flag must be
// added so callers don't treat the ranking as a real regional comparison.
func TestComparePrices_AllFallback_SurfacesWarning(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(makeFallbackPrice(spec.GetRegion(), 0.08)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callComparePrices(t, h, tools.ComparePricesInput{
		Spec:    map[string]any{"provider": "aws", "domain": "storage", "resource_type": "gp3"},
		Regions: []string{"us-east-1", "eu-central-1", "eu-central-2", "eu-north-1"},
	})

	warnings, ok := resp["warnings"].([]any)
	if !ok || len(warnings) == 0 {
		t.Fatalf("expected non-empty warnings when all regions are fallback data, got: %v", resp["warnings"])
	}
	found := false
	for _, w := range warnings {
		if s, ok := w.(string); ok && strings.Contains(s, "fallback") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings do not mention fallback/static data: %v", warnings)
	}
	if resp["ranking_low_confidence"] != true {
		t.Errorf("ranking_low_confidence: got %v, want true", resp["ranking_low_confidence"])
	}
	// cheapest_region/price_delta_pct must still be present (not suppressed).
	if resp["cheapest_region"] != "us-east-1" {
		t.Errorf("cheapest_region: got %v, want us-east-1 (still present, just low-confidence)", resp["cheapest_region"])
	}
}

// TestComparePrices_MixedFallbackAndLive_NoWarning is the negative
// counterpart: when at least one region returns live (non-fallback) data,
// the RC3-010 all-fallback warning must not fire.
func TestComparePrices_MixedFallbackAndLive_NoWarning(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			rgn := spec.GetRegion()
			if rgn == "us-east-1" {
				return makePriceResult(makePrice(rgn, 0.192)), nil
			}
			return makePriceResult(makeFallbackPrice(rgn, 0.08)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callComparePrices(t, h, tools.ComparePricesInput{
		Spec:    map[string]any{"provider": "aws", "domain": "storage", "resource_type": "gp3"},
		Regions: []string{"us-east-1", "eu-central-2"},
	})

	if _, ok := resp["warnings"]; ok {
		t.Errorf("expected no warnings key when not all regions are fallback, got: %v", resp["warnings"])
	}
	if _, ok := resp["ranking_low_confidence"]; ok {
		t.Errorf("expected no ranking_low_confidence key when not all regions are fallback, got: %v", resp["ranking_low_confidence"])
	}
}

// --------------------------------------------------------------------------
// Tests: compare_prices — RC3-033 (#29): multi-SKU results must not be
// interleaved by raw price and must never compute a baseline delta across
// two different SKUs.
// --------------------------------------------------------------------------

// makeMultiSKUPrice constructs a two-SKU-per-region NormalizedPrice pair
// mirroring GCP Cloud CDN's cache-egress + cache-fill shape: same region,
// distinct region-independent Description text, distinct price.
func makeMultiSKUPrice(region string, egressPrice, fillPrice float64) []models.NormalizedPrice {
	return []models.NormalizedPrice{
		{
			Provider:      models.CloudProviderGCP,
			Service:       "network",
			SKUID:         fmt.Sprintf("gcp:cloud_cdn:%s:egress", region),
			ProductFamily: "Cloud CDN",
			Description:   "Cloud CDN cache egress per GB",
			Region:        region,
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  egressPrice,
			Unit:          models.PriceUnitPerGB,
			Currency:      "USD",
		},
		{
			Provider:      models.CloudProviderGCP,
			Service:       "network",
			SKUID:         fmt.Sprintf("gcp:cloud_cdn:%s:cache_fill", region),
			ProductFamily: "Cloud CDN",
			Description:   "Cloud CDN cache fill per GB",
			Region:        region,
			PricingTerm:   models.PricingTermOnDemand,
			PricePerUnit:  fillPrice,
			Unit:          models.PriceUnitPerGB,
			Currency:      "USD",
		},
	}
}

// TestComparePrices_MultiSKU_EntriesGroupedNotInterleaved covers RC3-033
// (#29): a spec resolving to two SKUs per region (egress/cache-fill) must
// come back grouped by SKU — every entry of one SKU contiguous before the
// next SKU's entries — rather than interleaved by raw price across SKUs.
// Pre-fix, sorting allPrices purely by price would put egress(r1) between
// fill(r3) and fill(r2) below.
func TestComparePrices_MultiSKU_EntriesGroupedNotInterleaved(t *testing.T) {
	// fill: r1=0.01 (cheapest overall), r3=0.02, r2=0.30 (deliberately
	// pricier than every egress row so a naive flat price sort would
	// interleave it between the egress rows).
	// egress: r1=0.05, r2=0.06, r3=0.07
	prices := map[string][]models.NormalizedPrice{
		"r1": makeMultiSKUPrice("r1", 0.05, 0.01),
		"r2": makeMultiSKUPrice("r2", 0.06, 0.30),
		"r3": makeMultiSKUPrice("r3", 0.07, 0.02),
	}
	pvdr := &mockProvider{
		name:          "gcp",
		defaultRegion: "r1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(prices[spec.GetRegion()]...), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})
	resp := callComparePrices(t, h, tools.ComparePricesInput{
		Spec:    map[string]any{"provider": "gcp", "domain": "network", "service": "cloud_cdn"},
		Regions: []string{"r1", "r2", "r3"},
	})

	if resp["multi_sku"] != true {
		t.Errorf("multi_sku: got %v, want true", resp["multi_sku"])
	}

	sorted, ok := resp["all_regions_sorted"].([]any)
	if !ok || len(sorted) != 6 {
		t.Fatalf("all_regions_sorted: expected 6 entries, got %v", resp["all_regions_sorted"])
	}

	// Every entry must carry an explicit SKU discriminator.
	for i, entry := range sorted {
		e := entry.(map[string]any)
		if e["sku_description"] == nil || e["sku_description"] == "" {
			t.Errorf("entry[%d] missing sku_description: %v", i, e)
		}
	}

	// The three "cache fill" entries must be contiguous (grouped) rather
	// than interleaved with "cache egress" entries. Collect the SKU sequence
	// and confirm it is not intermixed: once a SKU boundary is crossed, that
	// SKU must not reappear.
	var seenBoundary bool
	prevDesc := sorted[0].(map[string]any)["sku_description"]
	for i := 1; i < len(sorted); i++ {
		desc := sorted[i].(map[string]any)["sku_description"]
		if desc != prevDesc {
			if seenBoundary {
				t.Fatalf("entries are interleaved across SKUs: entry[%d] sku_description=%v breaks contiguity", i, desc)
			}
			seenBoundary = true
			prevDesc = desc
		}
	}
	if !seenBoundary {
		t.Fatalf("expected two distinct SKU groups in all_regions_sorted, saw only one")
	}

	// The globally cheapest entry (fill r1 @ 0.01) must lead, and its whole
	// SKU group (fill) must be fully listed before any egress entry appears
	// — i.e. entry[2] (third row) must still be a "fill" row, not egress.
	if sorted[0].(map[string]any)["region"] != "r1" || sorted[0].(map[string]any)["sku_description"] != "Cloud CDN cache fill per GB" {
		t.Errorf("sorted[0]: expected r1 cache-fill entry (cheapest overall), got %v", sorted[0])
	}
	if sorted[2].(map[string]any)["sku_description"] != "Cloud CDN cache fill per GB" {
		t.Errorf("sorted[2]: expected a cache-fill entry (fill group must stay contiguous), got %v", sorted[2])
	}
}

// TestComparePrices_MultiSKU_TopLevelDeltaStaysWithinSameSKU covers RC3-033
// (#29): when multiple SKUs are present, price_delta_pct / most_expensive_*
// must compare the cheapest entry against the most expensive entry within
// the SAME SKU group, never against a different SKU's max price.
func TestComparePrices_MultiSKU_TopLevelDeltaStaysWithinSameSKU(t *testing.T) {
	// fill: r1=0.01 (global cheapest), r2=0.02, r3=0.03 (fill group max)
	// egress: r1=0.05, r2=0.06, r3=0.50 (global max, but a DIFFERENT SKU
	// than the global cheapest — pre-fix this leaked into price_delta_pct).
	prices := map[string][]models.NormalizedPrice{
		"r1": makeMultiSKUPrice("r1", 0.05, 0.01),
		"r2": makeMultiSKUPrice("r2", 0.06, 0.02),
		"r3": makeMultiSKUPrice("r3", 0.50, 0.03),
	}
	pvdr := &mockProvider{
		name:          "gcp",
		defaultRegion: "r1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(prices[spec.GetRegion()]...), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})
	resp := callComparePrices(t, h, tools.ComparePricesInput{
		Spec:    map[string]any{"provider": "gcp", "domain": "network", "service": "cloud_cdn"},
		Regions: []string{"r1", "r2", "r3"},
	})

	if resp["cheapest_region"] != "r1" {
		t.Errorf("cheapest_region: got %v, want r1", resp["cheapest_region"])
	}
	// Pre-fix this would be r3 (the egress row at 0.50, a different SKU).
	if resp["most_expensive_region"] != "r3" {
		t.Errorf("most_expensive_region: got %v, want r3 (must stay within the cheapest's own SKU group, i.e. cache-fill, not egress)", resp["most_expensive_region"])
	}
	mostExpPrice, ok := resp["most_expensive_price"].(map[string]any)
	if !ok {
		t.Fatalf("most_expensive_price missing or wrong type: %v", resp["most_expensive_price"])
	}
	if amt, _ := mostExpPrice["amount"].(float64); amt < 0.0299 || amt > 0.0301 {
		// Pre-fix this would be 0.50 (the egress SKU's max), not 0.03 (the
		// cache-fill SKU's own max).
		t.Errorf("most_expensive_price.amount: got %v, want ~0.03 (same SKU as cheapest)", mostExpPrice["amount"])
	}

	// delta = (0.03-0.01)/0.01*100 = 200.0%, NOT (0.50-0.01)/0.01*100 = 4900.0%.
	delta, ok := resp["price_delta_pct"].(float64)
	if !ok {
		t.Fatalf("price_delta_pct is not float64: %T %v", resp["price_delta_pct"], resp["price_delta_pct"])
	}
	if delta < 199.9 || delta > 200.1 {
		t.Errorf("price_delta_pct: got %v, want ~200.0 (same-SKU delta, not the cross-SKU 4900.0)", delta)
	}
}

// TestComparePrices_MultiSKU_BaselineDeltaPerSKUGroup covers RC3-033 (#29):
// this is the exact bug scenario from the issue report — baseline deltas
// must be computed within each SKU group against that group's OWN
// baseline-region price, never against a different SKU's baseline price.
// Pre-fix, applyBaselineDeltas found the first entry matching the baseline
// region in the (flat, price-sorted) list — which for this data is the
// cheaper "cache fill" SKU — and applied that single value as the baseline
// for every entry, including "cache egress" entries.
func TestComparePrices_MultiSKU_BaselineDeltaPerSKUGroup(t *testing.T) {
	// fill: r1=0.01, r2=0.02 (baseline), r3=0.03
	// egress: r1=0.05, r2=0.06 (baseline), r3=0.50
	prices := map[string][]models.NormalizedPrice{
		"r1": makeMultiSKUPrice("r1", 0.05, 0.01),
		"r2": makeMultiSKUPrice("r2", 0.06, 0.02),
		"r3": makeMultiSKUPrice("r3", 0.50, 0.03),
	}
	pvdr := &mockProvider{
		name:          "gcp",
		defaultRegion: "r1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(prices[spec.GetRegion()]...), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})
	resp := callComparePrices(t, h, tools.ComparePricesInput{
		Spec:           map[string]any{"provider": "gcp", "domain": "network", "service": "cloud_cdn"},
		Regions:        []string{"r1", "r2", "r3"},
		BaselineRegion: "r2",
	})

	sorted, ok := resp["all_regions_sorted"].([]any)
	if !ok {
		t.Fatalf("all_regions_sorted missing or wrong type: %v", resp["all_regions_sorted"])
	}

	var egressR1 map[string]any
	for _, entry := range sorted {
		e := entry.(map[string]any)
		if e["region"] == "r1" && e["sku_description"] == "Cloud CDN cache egress per GB" {
			egressR1 = e
		}
	}
	if egressR1 == nil {
		t.Fatalf("could not find r1 cache-egress entry in %v", sorted)
	}

	// Correct (same-SKU) baseline: egress r2 = 0.06.
	// delta = (0.05-0.06)/0.06*100 = -16.666...% -> "-16.7%"
	// Pre-fix (cross-SKU) baseline would have used cache-fill r2 = 0.02:
	// delta = (0.05-0.02)/0.02*100 = +150.0%.
	gotDelta, _ := egressR1["delta_pct"].(string)
	if gotDelta != "-16.7%" {
		t.Errorf("egress r1 delta_pct: got %q, want \"-16.7%%\" (same-SKU baseline); "+
			"a cross-SKU baseline bug would instead produce \"+150.0%%\"", gotDelta)
	}
}

// makeGCSStoragePrice constructs a single NormalizedPrice mirroring
// gcp_compute.go's real getGCSStoragePrice shape: exactly one SKU per
// region, but with Description AND SKUID built via fmt.Sprintf so the
// region string is embedded directly in both (e.g. "GCS standard storage in
// us-east1"), the same way Cloud SQL/Memorystore/GKE (gcp_database.go),
// Vertex AI/BigQuery (gcp_ai.go), and AWS inter-region transfer
// (aws_network.go) descriptions are built.
func makeGCSStoragePrice(region string, price float64) models.NormalizedPrice {
	return models.NormalizedPrice{
		Provider:      models.CloudProviderGCP,
		Service:       "storage",
		SKUID:         fmt.Sprintf("gcp:gcs:standard:%s", region),
		ProductFamily: "Cloud Storage",
		Description:   fmt.Sprintf("GCS standard storage in %s", region),
		Region:        region,
		PricingTerm:   models.PricingTermOnDemand,
		PricePerUnit:  price,
		Unit:          models.PriceUnitPerGBMonth,
		Currency:      "USD",
	}
}

// TestComparePrices_RegionEmbeddedDescription_StaysSingleSKUGroup covers the
// regression found in review of the RC3-033 (#29) fix: the grouping
// discriminator must NOT be the raw Description string, because many
// provider call sites embed the region directly in Description (GCS
// storage, Cloud SQL/Memorystore/GKE, Vertex AI/BigQuery, AWS inter-region
// transfer, etc). Using raw Description as the key would make the SAME SKU
// priced across regions look like N distinct one-member "SKU groups" — this
// is a single-SKU, multi-region comparison and must behave exactly like the
// pre-#29 single-SKU path: multi_sku absent/false, and price_delta_pct /
// most_expensive_region reflecting the real regional spread rather than
// collapsing to 0 / cheapest_region.
func TestComparePrices_RegionEmbeddedDescription_StaysSingleSKUGroup(t *testing.T) {
	// Single GCS-storage-shaped SKU, price varies by region: r1=0.020
	// (cheapest), r2=0.023, r3=0.026 (most expensive).
	prices := map[string][]models.NormalizedPrice{
		"r1": {makeGCSStoragePrice("r1", 0.020)},
		"r2": {makeGCSStoragePrice("r2", 0.023)},
		"r3": {makeGCSStoragePrice("r3", 0.026)},
	}
	pvdr := &mockProvider{
		name:          "gcp",
		defaultRegion: "r1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(prices[spec.GetRegion()]...), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})
	resp := callComparePrices(t, h, tools.ComparePricesInput{
		Spec:    map[string]any{"provider": "gcp", "domain": "storage", "service": "gcs_storage"},
		Regions: []string{"r1", "r2", "r3"},
	})

	// Pre-fix (raw-Description grouping): each region's Description differs
	// ("GCS standard storage in r1" vs "...r2" vs "...r3"), so this would
	// incorrectly produce three one-member SKU groups and multi_sku=true.
	if v, present := resp["multi_sku"]; present && v == true {
		t.Errorf("multi_sku: got %v, want absent/false — this is a single SKU priced across regions, "+
			"not multiple SKUs (region must not leak into the grouping key)", v)
	}

	if resp["cheapest_region"] != "r1" {
		t.Errorf("cheapest_region: got %v, want r1", resp["cheapest_region"])
	}
	// Pre-fix, most_expensive_region collapses to equal cheapest_region
	// because the "same SKU group as cheapest" candidate slice has exactly
	// one member (the cheapest region's own singleton group).
	if resp["most_expensive_region"] != "r3" {
		t.Errorf("most_expensive_region: got %v, want r3 (real regional spread); "+
			"a region-leaking grouping bug would instead collapse this to r1 (== cheapest_region)", resp["most_expensive_region"])
	}

	// delta = (0.026-0.020)/0.020*100 = 30.0%. Pre-fix this collapses to 0.0
	// because cheapest and "most expensive" would be the same singleton entry.
	delta, ok := resp["price_delta_pct"].(float64)
	if !ok {
		t.Fatalf("price_delta_pct is not float64: %T %v", resp["price_delta_pct"], resp["price_delta_pct"])
	}
	if delta < 29.9 || delta > 30.1 {
		t.Errorf("price_delta_pct: got %v, want ~30.0 (real regional spread); "+
			"a region-leaking grouping bug would instead produce 0.0", delta)
	}
}

// TestComparePrices_RegionEmbeddedDescription_BaselineDeltaNotNulled covers
// the same regression with baseline_region set: pre-fix, every non-baseline
// region's own singleton "SKU group" has no baseline-region member of its
// own, so applyBaselineDeltas nulls out delta_per_hour/delta_monthly/
// delta_pct for every entry except the baseline region itself — silently
// breaking the baseline-delta feature for any domain whose Description
// embeds the region.
func TestComparePrices_RegionEmbeddedDescription_BaselineDeltaNotNulled(t *testing.T) {
	prices := map[string][]models.NormalizedPrice{
		"r1": {makeGCSStoragePrice("r1", 0.020)},
		"r2": {makeGCSStoragePrice("r2", 0.023)}, // baseline
		"r3": {makeGCSStoragePrice("r3", 0.026)},
	}
	pvdr := &mockProvider{
		name:          "gcp",
		defaultRegion: "r1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(prices[spec.GetRegion()]...), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})
	resp := callComparePrices(t, h, tools.ComparePricesInput{
		Spec:           map[string]any{"provider": "gcp", "domain": "storage", "service": "gcs_storage"},
		Regions:        []string{"r1", "r2", "r3"},
		BaselineRegion: "r2",
	})

	sorted, ok := resp["all_regions_sorted"].([]any)
	if !ok {
		t.Fatalf("all_regions_sorted missing or wrong type: %v", resp["all_regions_sorted"])
	}

	var r1Entry, r3Entry map[string]any
	for _, entry := range sorted {
		e := entry.(map[string]any)
		switch e["region"] {
		case "r1":
			r1Entry = e
		case "r3":
			r3Entry = e
		}
	}
	if r1Entry == nil || r3Entry == nil {
		t.Fatalf("could not find r1/r3 entries in %v", sorted)
	}

	// delta(r1) = (0.020-0.023)/0.023*100 = -13.04...% -> "-13.0%"
	if got, _ := r1Entry["delta_pct"].(string); got != "-13.0%" {
		t.Errorf("r1 delta_pct: got %q, want \"-13.0%%\"; "+
			"a region-leaking grouping bug would instead null this out entirely", got)
	}
	// delta(r3) = (0.026-0.023)/0.023*100 = 13.04...% -> "+13.0%"
	if got, _ := r3Entry["delta_pct"].(string); got != "+13.0%" {
		t.Errorf("r3 delta_pct: got %q, want \"+13.0%%\"; "+
			"a region-leaking grouping bug would instead null this out entirely", got)
	}
}

// --------------------------------------------------------------------------
// Tests: compare_prices / get_prices_batch — RC3-001 transient error classification
// --------------------------------------------------------------------------

// TestComparePrices_AllRegionsTransientError_ReturnsUpstreamFailure covers
// RC3-001: when every region's pvdr.GetPrice call fails with a transport
// error (not genuine no-data), the handler must return a retryable
// upstream_failure — matching HandleGetPrice's contract — instead of a fake
// no_prices_found with no indication the failure might resolve on retry.
func TestComparePrices_AllRegionsTransientError_ReturnsUpstreamFailure(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, _ models.PricingSpec) (*models.PricingResult, error) {
			return nil, errors.New("connection reset by peer")
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callComparePrices(t, h, tools.ComparePricesInput{
		Spec:    map[string]any{"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge"},
		Regions: []string{"us-east-1", "eu-west-1"},
	})
	if resp["result"] == "no_prices_found" {
		t.Fatalf("expected upstream_failure, got no_prices_found: %v", resp)
	}
	if resp["error"] != "upstream_failure" {
		t.Errorf("error: got %v, want upstream_failure", resp["error"])
	}
	if resp["retryable"] != true {
		t.Errorf("retryable: got %v, want true", resp["retryable"])
	}
}

// TestComparePrices_MixedNoDataAndTransientError_StaysNoPricesFound covers
// the boundary of RC3-001: when at least one region is genuinely absent
// (no_data) rather than all regions failing transiently, the response stays
// no_prices_found (not every failure is retryable) but still surfaces which
// regions hit a transient error for visibility.
func TestComparePrices_MixedNoDataAndTransientError_StaysNoPricesFound(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			if spec.GetRegion() == "eu-west-1" {
				return nil, errors.New("timeout")
			}
			return makePriceResult(), nil // genuine no-data
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
	transientErrs, ok := resp["transient_errors"].(map[string]any)
	if !ok || transientErrs["eu-west-1"] == nil {
		t.Errorf("transient_errors should contain eu-west-1, got: %v", resp["transient_errors"])
	}
}

// TestGetPricesBatch_TransientErrorClassifiedRetryable covers RC3-001 for
// get_prices_batch: an error map entry from a transport failure must be
// classified retryable:true (status transient_error), distinct from a
// genuine no-data entry which is retryable:false (status no_data).
func TestGetPricesBatch_TransientErrorClassifiedRetryable(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			cs, ok := spec.(*models.ComputePricingSpec)
			if !ok || cs.ResourceType == "flaky.type" {
				return nil, errors.New("503 service unavailable")
			}
			if cs.ResourceType == "bogus.type" {
				return makePriceResult(), nil // genuine no-data
			}
			return makePriceResult(makePrice(spec.GetRegion(), 0.192)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPricesBatch(t, h, tools.GetPricesBatchInput{
		Provider:      "aws",
		InstanceTypes: []string{"m5.xlarge", "flaky.type", "bogus.type"},
		Region:        "us-east-1",
	})

	errs, ok := resp["errors"].(map[string]any)
	if !ok {
		t.Fatalf("errors map missing: %v", resp)
	}

	flaky, ok := errs["flaky.type"].(map[string]any)
	if !ok {
		t.Fatalf("errors[flaky.type] missing or wrong shape: %v", errs["flaky.type"])
	}
	if flaky["status"] != "transient_error" {
		t.Errorf("flaky.type status: got %v, want transient_error", flaky["status"])
	}
	if flaky["retryable"] != true {
		t.Errorf("flaky.type retryable: got %v, want true", flaky["retryable"])
	}

	bogus, ok := errs["bogus.type"].(map[string]any)
	if !ok {
		t.Fatalf("errors[bogus.type] missing or wrong shape: %v", errs["bogus.type"])
	}
	if bogus["status"] != "no_data" {
		t.Errorf("bogus.type status: got %v, want no_data", bogus["status"])
	}
	if bogus["retryable"] != false {
		t.Errorf("bogus.type retryable: got %v, want false", bogus["retryable"])
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
// Real-provider helpers for describe_catalog tests
// --------------------------------------------------------------------------

// realGCPProvider returns a *gcpprovider.Provider sufficient for DescribeCatalog,
// which is a pure static function requiring no HTTP client or API key.
func realGCPProvider(t *testing.T) *gcpprovider.Provider {
	t.Helper()
	p, err := gcpprovider.NewProvider(&config.Config{}, nil)
	if err != nil {
		t.Fatalf("gcpprovider.NewProvider: %v", err)
	}
	return p
}

// realAWSProvider returns a *awsprovider.Provider sufficient for DescribeCatalog.
// AWS DescribeCatalog is purely static; NewProvider is called with an empty
// config so no credentials are required.
func realAWSProvider(t *testing.T) *awsprovider.Provider {
	t.Helper()
	p, err := awsprovider.NewProvider(&config.Config{}, nil)
	if err != nil {
		t.Fatalf("awsprovider.NewProvider: %v", err)
	}
	return p
}

// listContainsStr reports whether the []any (from JSON-decoded arrays) contains s.
func listContainsStr(list []any, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// --------------------------------------------------------------------------
// Tests: describe_catalog — per-provider terms completeness (end-to-end)
// --------------------------------------------------------------------------

// TestDescribeCatalog_GCP_ComputeTermsComplete verifies that the GCP catalog
// includes ALL expected pricing terms for compute/compute_engine when called
// with provider+domain+service.
func TestDescribeCatalog_GCP_ComputeTermsComplete(t *testing.T) {
	realGCP := realGCPProvider(t)
	pvdr := &mockProvider{
		name:          "gcp",
		defaultRegion: "us-central1",
		describeCatFunc: func(ctx context.Context) (*models.ProviderCatalog, error) {
			return realGCP.DescribeCatalog(ctx)
		},
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})
	resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{
		Provider: "gcp",
		Domain:   "compute",
		Service:  "compute_engine",
	})
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}

	terms, ok := resp["supported_terms"].([]any)
	if !ok {
		t.Fatalf("supported_terms missing or wrong type: %T %v", resp["supported_terms"], resp["supported_terms"])
	}

	expectedTerms := []string{"on_demand", "spot", "cud_1yr", "cud_3yr", "sud", "flex_cud"}
	for _, want := range expectedTerms {
		if !listContainsStr(terms, want) {
			t.Errorf("supported_terms missing %q; got: %v", want, terms)
		}
	}
}

// TestDescribeCatalog_GCP_ServiceListNonEmpty verifies that a domain-only GCP
// describe_catalog call returns a non-empty available_services list containing
// "compute_engine".
func TestDescribeCatalog_GCP_ServiceListNonEmpty(t *testing.T) {
	realGCP := realGCPProvider(t)
	pvdr := &mockProvider{
		name:          "gcp",
		defaultRegion: "us-central1",
		describeCatFunc: func(ctx context.Context) (*models.ProviderCatalog, error) {
			return realGCP.DescribeCatalog(ctx)
		},
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})
	// Domain-only — GCP keys are under domain/service, so the handler returns
	// available_services (the fallback branch).
	resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{
		Provider: "gcp",
		Domain:   "compute",
	})
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}

	svcs, ok := resp["available_services"].([]any)
	if !ok || len(svcs) == 0 {
		t.Fatalf("available_services missing or empty: %v", resp["available_services"])
	}
	if !listContainsStr(svcs, "compute_engine") {
		t.Errorf("available_services should contain compute_engine; got: %v", svcs)
	}
}

// TestDescribeCatalog_AWS_DomainsPresent verifies that AWS compute describe_catalog
// returns supported_terms containing on_demand and spot (AWS keys under bare domain).
func TestDescribeCatalog_AWS_DomainsPresent(t *testing.T) {
	realAWS := realAWSProvider(t)
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		describeCatFunc: func(ctx context.Context) (*models.ProviderCatalog, error) {
			return realAWS.DescribeCatalog(ctx)
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

	terms, ok := resp["supported_terms"].([]any)
	if !ok {
		t.Fatalf("supported_terms missing or wrong type: %T %v", resp["supported_terms"], resp["supported_terms"])
	}
	for _, want := range []string{"on_demand", "spot"} {
		if !listContainsStr(terms, want) {
			t.Errorf("AWS supported_terms missing %q; got: %v", want, terms)
		}
	}
}

// TestDescribeCatalog_Azure_DomainsPresent verifies that Azure compute describe_catalog
// returns supported_terms containing on_demand.
func TestDescribeCatalog_Azure_DomainsPresent(t *testing.T) {
	realAzure := realAzureProvider()
	pvdr := &mockProvider{
		name:          "azure",
		defaultRegion: "eastus",
		describeCatFunc: func(ctx context.Context) (*models.ProviderCatalog, error) {
			return realAzure.DescribeCatalog(ctx)
		},
	}
	h := tools.New(map[string]tools.Provider{"azure": pvdr})
	// Azure keys are under compute/vm; pass service to get terms directly.
	resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{
		Provider: "azure",
		Domain:   "compute",
		Service:  "vm",
	})
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}

	terms, ok := resp["supported_terms"].([]any)
	if !ok {
		t.Fatalf("supported_terms missing or wrong type: %T %v", resp["supported_terms"], resp["supported_terms"])
	}
	if !listContainsStr(terms, "on_demand") {
		t.Errorf("Azure supported_terms missing on_demand; got: %v", terms)
	}
}

// TestDescribeCatalog_UnknownProvider_ReturnsError verifies that an unknown provider
// returns an error key in the response.
func TestDescribeCatalog_UnknownProvider_ReturnsError(t *testing.T) {
	h := tools.New(map[string]tools.Provider{})
	resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{
		Provider: "unknown_xyz",
		Domain:   "compute",
	})
	if resp["error"] == nil {
		t.Errorf("expected error key for unknown provider, got: %v", resp)
	}
}

// TestDescribeCatalog_GCP_FilterHintsHasSUDNote verifies that the filter_hints
// for compute/compute_engine contain a sud_note key that mentions get_price and term='sud'.
func TestDescribeCatalog_GCP_FilterHintsHasSUDNote(t *testing.T) {
	realGCP := realGCPProvider(t)
	pvdr := &mockProvider{
		name:          "gcp",
		defaultRegion: "us-central1",
		describeCatFunc: func(ctx context.Context) (*models.ProviderCatalog, error) {
			return realGCP.DescribeCatalog(ctx)
		},
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})
	resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{
		Provider: "gcp",
		Domain:   "compute",
		Service:  "compute_engine",
	})
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}

	hints, ok := resp["filter_hints"].(map[string]any)
	if !ok {
		t.Fatalf("filter_hints missing or wrong type: %T %v", resp["filter_hints"], resp["filter_hints"])
	}
	sudNote, ok := hints["sud_note"].(string)
	if !ok || sudNote == "" {
		t.Fatalf("filter_hints missing sud_note key or empty; hints: %v", hints)
	}
	if !contains(sudNote, "get_price") {
		t.Errorf("sud_note should mention get_price; got: %q", sudNote)
	}
	if !contains(sudNote, "term='sud'") {
		t.Errorf("sud_note should mention term='sud'; got: %q", sudNote)
	}
}

// TestDescribeCatalog_GCP_AllDomainsReachable verifies that each GCP domain
// returns a non-error response with non-empty available_services.
func TestDescribeCatalog_GCP_AllDomainsReachable(t *testing.T) {
	realGCP := realGCPProvider(t)
	pvdr := &mockProvider{
		name:          "gcp",
		defaultRegion: "us-central1",
		describeCatFunc: func(ctx context.Context) (*models.ProviderCatalog, error) {
			return realGCP.DescribeCatalog(ctx)
		},
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	domains := []string{
		"compute", "storage", "database", "container",
		"ai", "analytics", "network", "observability",
	}
	for _, domain := range domains {
		domain := domain
		t.Run(domain, func(t *testing.T) {
			resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{
				Provider: "gcp",
				Domain:   domain,
			})
			if resp["error"] != nil {
				t.Fatalf("domain %q returned error: %v", domain, resp)
			}
			// Domain-only calls for GCP fall into the available_services fallback
			// branch because all GCP catalog keys are "domain/service".
			svcs, ok := resp["available_services"].([]any)
			if !ok || len(svcs) == 0 {
				t.Errorf("domain %q: expected non-empty available_services, got: %v", domain, resp["available_services"])
			}
		})
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
	if resp["region"] != "us-east-1" {
		t.Errorf("region: got %v, want %q", resp["region"], "us-east-1")
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
	if resp["region"] != "us-central1" {
		t.Errorf("region: got %v, want %q", resp["region"], "us-central1")
	}
}

// TestGetSpotHistory_InvalidInput exercises the isInvalidInput(err) branch and
// asserts the region is echoed back (region-in-errors fix).
func TestGetSpotHistory_InvalidInput(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		spotHistFunc: func(_ context.Context, _ models.PricingSpec, _ int, _ string) (map[string]any, error) {
			return nil, errors.New("invalid availability zone")
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetSpotHistory(t, h, tools.GetSpotHistoryInput{
		Spec:             map[string]any{"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge", "region": "us-east-1"},
		AvailabilityZone: "not-a-real-az",
	})
	if resp["error"] != "invalid_input" {
		t.Errorf("error: got %v, want invalid_input", resp["error"])
	}
	if resp["region"] != "us-east-1" {
		t.Errorf("region: got %v, want %q", resp["region"], "us-east-1")
	}
}

// TestGetSpotHistory_UpstreamFailure exercises the generic (retryable)
// upstream_failure branch and asserts the region is echoed back
// (region-in-errors fix).
func TestGetSpotHistory_UpstreamFailure(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		spotHistFunc: func(_ context.Context, _ models.PricingSpec, _ int, _ string) (map[string]any, error) {
			return nil, errors.New("transient network error")
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetSpotHistory(t, h, tools.GetSpotHistoryInput{
		Spec: map[string]any{"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge", "region": "us-east-1"},
	})
	if resp["error"] != "upstream_failure" {
		t.Errorf("error: got %v, want upstream_failure", resp["error"])
	}
	if resp["region"] != "us-east-1" {
		t.Errorf("region: got %v, want %q", resp["region"], "us-east-1")
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
	price := p0["price_per_unit"].(map[string]any)
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
	price := p0["price_per_unit"].(map[string]any)
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

// TestFillDomain_ServiceInference_AuroraPostgresql verifies the RC3-026 / #45
// fix: service="aurora_postgresql" (the documentation-only alias describe_catalog
// advertises under "database") infers domain=database just like service="rds",
// so get_price(service="aurora_postgresql", ...) works without an explicit
// domain field.
func TestFillDomain_ServiceInference_AuroraPostgresql(t *testing.T) {
	var capturedDomain models.PricingDomain
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			capturedDomain = spec.GetDomain()
			return makePriceResult(makePrice(spec.GetRegion(), 0.29)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	callGetPrice(t, h, map[string]any{
		"provider":      "aws",
		"service":       "aurora_postgresql",
		"resource_type": "db.r6g.large",
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
//
// This (nil, nil) shape is a defensive case only — no provider in this codebase
// actually returns it (AWS's GetPrice always wraps its result in a non-nil
// *models.PricingResult; see TestGetPrice_NonNilEmptyResult_NoResultsHint below
// for the shape AWS really produces on a zero-match lookup, RC3-027 / #46).
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
	if !contains(tip, "describe_catalog") {
		t.Errorf("tip should mention describe_catalog, got: %q", tip)
	}
	if resp["provider"] != "aws" {
		t.Errorf("provider: got %v, want aws", resp["provider"])
	}
}

// TestGetPrice_NonNilEmptyResult_NoResultsHint verifies the no_results hint also
// fires for the shape AWS's GetPrice actually returns on a zero-match lookup: a
// non-nil *models.PricingResult with an empty PublicPrices slice, no contracted
// prices, no effective price, and no Note (e.g. db.r8g.2xlarge Aurora PostgreSQL
// in a region with no catalog entry). Before RC3-027 this was returned verbatim
// as {"public_prices":[],...} with no error/no_results marker, indistinguishable
// from a typo'd resource_type (#46).
func TestGetPrice_NonNilEmptyResult_NoResultsHint(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, _ models.PricingSpec) (*models.PricingResult, error) {
			return &models.PricingResult{
				PublicPrices:  []models.NormalizedPrice{},
				AuthAvailable: false,
				Source:        "catalog",
				SchemaVersion: "1",
			}, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPrice(t, h, map[string]any{
		"provider":      "aws",
		"domain":        "database",
		"service":       "rds",
		"resource_type": "db.r8g.2xlarge",
		"engine":        "Aurora PostgreSQL",
		"region":        "us-east-1",
	})

	if resp["result"] != "no_results" {
		t.Fatalf("result: got %v, want no_results (got full response: %v)", resp["result"], resp)
	}
	tip, _ := resp["tip"].(string)
	if !contains(tip, "describe_catalog") {
		t.Errorf("tip should mention describe_catalog, got: %q", tip)
	}
	if resp["provider"] != "aws" {
		t.Errorf("provider: got %v, want aws", resp["provider"])
	}
	if _, present := resp["public_prices"]; present {
		t.Errorf("no_results response should not also carry public_prices, got: %v", resp)
	}
}

// TestGetPrice_NonNilEmptyResultWithNote_NotNoResults verifies the Note-present
// exclusion is preserved: a non-nil *models.PricingResult with empty
// PublicPrices but a non-empty Note (the deliberate spot-price case, where AWS
// has no static spot data but supplies an on-demand-estimate hint instead) must
// NOT be reclassified as no_results.
func TestGetPrice_NonNilEmptyResultWithNote_NotNoResults(t *testing.T) {
	const spotNote = "Spot prices are not available from the static pricing catalog."
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, _ models.PricingSpec) (*models.PricingResult, error) {
			return &models.PricingResult{
				PublicPrices:  []models.NormalizedPrice{},
				AuthAvailable: false,
				Source:        "catalog",
				SchemaVersion: "1",
				Note:          spotNote,
			}, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPrice(t, h, map[string]any{
		"provider":      "aws",
		"domain":        "compute",
		"resource_type": "m5.xlarge",
		"term":          "spot",
		"region":        "us-east-1",
	})

	if resp["result"] == "no_results" {
		t.Fatalf("result should not be no_results when Note is set, got: %v", resp)
	}
	if resp["note"] != spotNote {
		t.Errorf("note: got %v, want %q", resp["note"], spotNote)
	}
	if _, present := resp["public_prices"]; !present {
		t.Errorf("expected public_prices field to be present in the summary response, got: %v", resp)
	}
}

// TestGetPrice_NoResults_NotAvailableIn verifies that get_price's no_results
// response carries a not_available_in list naming the queried region, using
// the same field name/shape as the fan-out lookup tools (compare_prices,
// find_cheapest_region, find_available_regions), so callers can check for
// coverage gaps generically across tools (RC3-018 / #37).
func TestGetPrice_NoResults_NotAvailableIn(t *testing.T) {
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
		"region":        "us-west-2",
	})

	if resp["result"] != "no_results" {
		t.Fatalf("result: got %v, want no_results", resp["result"])
	}
	notAvail, ok := resp["not_available_in"].([]any)
	if !ok || len(notAvail) != 1 {
		t.Fatalf("not_available_in missing or wrong shape: %v", resp["not_available_in"])
	}
	if notAvail[0] != "us-west-2" {
		t.Errorf("not_available_in[0]: got %v, want us-west-2", notAvail[0])
	}
}

// TestGetPrice_Success_NoNotAvailableIn verifies that a successful get_price
// response does not carry a not_available_in field — the key is only
// present when there is an actual coverage gap to disclose.
func TestGetPrice_Success_NoNotAvailableIn(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(makePrice(spec.GetRegion(), 0.192)), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPrice(t, h, map[string]any{
		"provider":      "aws",
		"domain":        "compute",
		"resource_type": "m5.xlarge",
		"region":        "us-east-1",
	})
	if _, ok := resp["error"]; ok {
		t.Fatalf("unexpected error: %v", resp)
	}
	if _, present := resp["not_available_in"]; present {
		t.Errorf("not_available_in should be absent on success, got: %v", resp["not_available_in"])
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
// Tests: describe_catalog — catalog fix regressions (Findings 1–6)
// --------------------------------------------------------------------------

// TestDescribeCatalog_AWS_LambdaServiceInfersDomain verifies Finding 1:
// describe_catalog(provider='aws', service='lambda') with no domain must
// infer domain='serverless' and return serverless/lambda guidance (not the
// full support matrix).
func TestDescribeCatalog_AWS_LambdaServiceInfersDomain(t *testing.T) {
	realAWS := realAWSProvider(t)
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		describeCatFunc: func(ctx context.Context) (*models.ProviderCatalog, error) {
			return realAWS.DescribeCatalog(ctx)
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{
		Provider: "aws",
		Service:  "lambda",
		// Domain intentionally omitted.
	})
	// Must not return the full support_matrix path.
	if _, hasSM := resp["support_matrix"]; hasSM {
		t.Fatal("service='lambda' with no domain should NOT return support_matrix")
	}
	// Must emit a redirect_notice.
	notice, ok := resp["redirect_notice"].(string)
	if !ok || notice == "" {
		t.Errorf("expected redirect_notice, got: %v", resp["redirect_notice"])
	}
	// Must return domain='serverless'.
	if resp["domain"] != "serverless" {
		t.Errorf("expected domain=serverless, got: %v", resp["domain"])
	}
	// Must return example_invocation or filter_hints (serverless/lambda guidance).
	if resp["example_invocation"] == nil && resp["filter_hints"] == nil {
		t.Errorf("expected serverless/lambda guidance, got: %v", resp)
	}
}

// TestDescribeCatalog_AWS_ComputePlusLambdaCrossDomain verifies Finding 2:
// describe_catalog(provider='aws', domain='compute', service='lambda') must
// redirect to serverless domain and NOT return EC2/compute guidance.
func TestDescribeCatalog_AWS_ComputePlusLambdaCrossDomain(t *testing.T) {
	realAWS := realAWSProvider(t)
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		describeCatFunc: func(ctx context.Context) (*models.ProviderCatalog, error) {
			return realAWS.DescribeCatalog(ctx)
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{
		Provider: "aws",
		Domain:   "compute",
		Service:  "lambda",
	})
	// Must emit a redirect_notice.
	notice, ok := resp["redirect_notice"].(string)
	if !ok || notice == "" {
		t.Errorf("expected redirect_notice for cross-domain redirect, got: %v", resp["redirect_notice"])
	}
	// Must redirect to serverless, not compute.
	if resp["domain"] != "serverless" {
		t.Errorf("expected domain=serverless after redirect, got: %v", resp["domain"])
	}
	// Must NOT return an example_invocation that mentions m5.xlarge (EC2 example).
	if ex, ok := resp["example_invocation"].(map[string]any); ok {
		if rt, _ := ex["resource_type"].(string); rt == "m5.xlarge" {
			t.Errorf("cross-domain redirect returned EC2 example (m5.xlarge) — should be serverless")
		}
	}
}

// TestDescribeCatalog_AWS_LambdaFilterHintsHasUsageFields verifies Finding 3/5:
// describe_catalog for serverless/lambda must include gb_seconds and
// requests_millions in filter_hints.
func TestDescribeCatalog_AWS_LambdaFilterHintsHasUsageFields(t *testing.T) {
	realAWS := realAWSProvider(t)
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		describeCatFunc: func(ctx context.Context) (*models.ProviderCatalog, error) {
			return realAWS.DescribeCatalog(ctx)
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{
		Provider: "aws",
		Domain:   "serverless",
		Service:  "lambda",
	})
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	hints, ok := resp["filter_hints"].(map[string]any)
	if !ok {
		t.Fatalf("filter_hints missing or wrong type: %T", resp["filter_hints"])
	}
	for _, field := range []string{"gb_seconds", "requests_millions"} {
		if _, has := hints[field]; !has {
			t.Errorf("filter_hints missing %q; hints: %v", field, hints)
		}
	}
}

// TestDescribeCatalog_AWS_LambdaExampleHasUsageFields verifies Finding 5:
// the serverless/lambda example_invocation must include gb_seconds and
// requests_millions.
func TestDescribeCatalog_AWS_LambdaExampleHasUsageFields(t *testing.T) {
	realAWS := realAWSProvider(t)
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		describeCatFunc: func(ctx context.Context) (*models.ProviderCatalog, error) {
			return realAWS.DescribeCatalog(ctx)
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{
		Provider: "aws",
		Domain:   "serverless",
		Service:  "lambda",
	})
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	ex, ok := resp["example_invocation"].(map[string]any)
	if !ok {
		t.Fatalf("example_invocation missing or wrong type: %T", resp["example_invocation"])
	}
	for _, field := range []string{"gb_seconds", "requests_millions"} {
		if _, has := ex[field]; !has {
			t.Errorf("example_invocation missing %q; ex: %v", field, ex)
		}
	}
}

// TestDescribeCatalog_GCP_CloudRunNotSupported verifies Finding 3:
// describe_catalog for GCP + service='cloud_run' should emit a
// 'not_supported_in_go_provider' message rather than empty guidance.
func TestDescribeCatalog_GCP_CloudRunNotSupported(t *testing.T) {
	realGCP := realGCPProvider(t)
	pvdr := &mockProvider{
		name:          "gcp",
		defaultRegion: "us-central1",
		describeCatFunc: func(ctx context.Context) (*models.ProviderCatalog, error) {
			return realGCP.DescribeCatalog(ctx)
		},
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})
	resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{
		Provider: "gcp",
		Service:  "cloud_run",
	})
	// Must redirect to serverless.
	if resp["domain"] != "serverless" {
		t.Errorf("expected domain=serverless after cloud_run redirect, got: %v", resp["domain"])
	}
	// Must emit the not_supported_in_go_provider error.
	if resp["error"] != "not_supported_in_go_provider" {
		t.Errorf("expected error=not_supported_in_go_provider, got: %v", resp["error"])
	}
	// Must include a helpful tip mentioning Python provider.
	tip, _ := resp["tip"].(string)
	if !contains(tip, "Python") {
		t.Errorf("tip should mention Python provider, got: %q", tip)
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

// --------------------------------------------------------------------------
// TestGetPrice_AuroraStorageFieldDisambiguates verifies that the "storage"
// attribute (which AWS uses to distinguish Aurora Standard from Aurora
// I/O-Optimized SKUs) is surfaced in the get_price response. Without this
// field both Aurora SKUs look identical to the model (same instanceType, vcpu,
// memory, and term) causing it to make additional disambiguating tool calls.
//
// Real AWS pricing API returns:
//   - Aurora Standard:       storage = "EBS Only"
//   - Aurora I/O-Optimized:  storage = "Aurora IO Optimization Mode"
// --------------------------------------------------------------------------

func TestGetPrice_AuroraStorageFieldDisambiguates(t *testing.T) {
	auroraStandard := models.NormalizedPrice{
		Provider:    models.CloudProviderAWS,
		Service:     "database",
		SKUID:       "AURORA-STD-SKU",
		Description: "db.r6g.2xlarge",
		Region:      "us-east-1",
		Attributes: map[string]string{
			"instanceType":   "db.r6g.2xlarge",
			"vcpu":           "8",
			"memory":         "64 GiB",
			"databaseEngine": "Aurora PostgreSQL",
			"storage":        "EBS Only",
		},
		PricingTerm:  models.PricingTermOnDemand,
		PricePerUnit: 1.038,
		Unit:         models.PriceUnitPerHour,
		Currency:     "USD",
	}
	auroraIOOpt := models.NormalizedPrice{
		Provider:    models.CloudProviderAWS,
		Service:     "database",
		SKUID:       "AURORA-IOOPT-SKU",
		Description: "db.r6g.2xlarge",
		Region:      "us-east-1",
		Attributes: map[string]string{
			"instanceType":   "db.r6g.2xlarge",
			"vcpu":           "8",
			"memory":         "64 GiB",
			"databaseEngine": "Aurora PostgreSQL",
			"storage":        "Aurora IO Optimization Mode",
		},
		PricingTerm:  models.PricingTermOnDemand,
		PricePerUnit: 1.349,
		Unit:         models.PriceUnitPerHour,
		Currency:     "USD",
	}

	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		getPriceFunc: func(_ context.Context, _ models.PricingSpec) (*models.PricingResult, error) {
			return makePriceResult(auroraStandard, auroraIOOpt), nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	resp := callGetPrice(t, h, map[string]any{
		"provider":      "aws",
		"domain":        "database",
		"service":       "rds",
		"engine":        "aurora-postgresql",
		"deployment":    "single-az",
		"region":        "us-east-1",
		"term":          "on_demand",
		"resource_type": "db.r6g.2xlarge",
	})

	prices, ok := resp["public_prices"].([]any)
	if !ok || len(prices) != 2 {
		t.Fatalf("expected 2 public_prices, got %v", resp["public_prices"])
	}

	storageVals := make(map[string]bool)
	for _, p := range prices {
		pm := p.(map[string]any)
		storageVal, hasStorage := pm["storage"]
		if !hasStorage {
			t.Errorf("public_prices entry missing 'storage' field: %v", pm)
			continue
		}
		storageVals[storageVal.(string)] = true
	}

	if !storageVals["EBS Only"] {
		t.Errorf("expected one price with storage='EBS Only' (Aurora Standard), got storage values: %v", storageVals)
	}
	if !storageVals["Aurora IO Optimization Mode"] {
		t.Errorf("expected one price with storage='Aurora IO Optimization Mode' (Aurora I/O-Optimized), got storage values: %v", storageVals)
	}
}

// --------------------------------------------------------------------------
// Tests: describe_catalog — RC3-020/021/022/025 fixes
// --------------------------------------------------------------------------

// TestDescribeCatalog_AWS_InterRegionEgressServicesNotEmpty verifies that the
// top-level services map for AWS no longer lists "inter_region_egress" as an
// empty array — the domain is fully functional and should carry a placeholder
// entry instead (RC3-020).
func TestDescribeCatalog_AWS_InterRegionEgressServicesNotEmpty(t *testing.T) {
	realAWS := realAWSProvider(t)
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		describeCatFunc: func(ctx context.Context) (*models.ProviderCatalog, error) {
			return realAWS.DescribeCatalog(ctx)
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	// Provider-only mode returns the raw services map (catalogEntry) so we can
	// inspect it directly without any per-domain fallback logic in play.
	resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{Provider: "aws"})
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	matrix, ok := resp["support_matrix"].(map[string]any)
	if !ok {
		t.Fatalf("support_matrix missing or wrong type: %v", resp)
	}
	awsEntry, ok := matrix["aws"].(map[string]any)
	if !ok {
		t.Fatalf("support_matrix.aws missing or wrong type: %v", matrix)
	}
	services, ok := awsEntry["services"].(map[string]any)
	if !ok {
		t.Fatalf("services missing or wrong type: %v", awsEntry)
	}
	ire, ok := services["inter_region_egress"].([]any)
	if !ok {
		t.Fatalf("services.inter_region_egress missing or wrong type: %v", services["inter_region_egress"])
	}
	if len(ire) == 0 {
		t.Errorf("expected services.inter_region_egress to be non-empty (placeholder entry), got empty array")
	}

	// The domain itself must still work end-to-end: filter_hints and
	// example_invocation are unaffected by the Services placeholder.
	domResp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{
		Provider: "aws",
		Domain:   "inter_region_egress",
	})
	if domResp["filter_hints"] == nil {
		t.Errorf("expected filter_hints for inter_region_egress domain, got: %v", domResp)
	}
	if domResp["example_invocation"] == nil {
		t.Errorf("expected example_invocation for inter_region_egress domain, got: %v", domResp)
	}
}

// TestDescribeCatalog_AWS_Database_ShowsAvailableServices verifies that
// describe_catalog(domain=database) with no service returns available_services
// and a tip, instead of a bare service:null response (RC3-021). "database" has
// a domain-level supported_terms entry but no domain-level filter_hints or
// example_invocation, so the "nothing actionable found" guard must fire based
// on hints/example alone, not terms.
func TestDescribeCatalog_AWS_Database_ShowsAvailableServices(t *testing.T) {
	realAWS := realAWSProvider(t)
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		describeCatFunc: func(ctx context.Context) (*models.ProviderCatalog, error) {
			return realAWS.DescribeCatalog(ctx)
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{
		Provider: "aws",
		Domain:   "database",
	})
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	if resp["service"] != nil {
		t.Errorf("expected service:null, got: %v", resp["service"])
	}
	// supported_terms should still be present (domain-level terms exist).
	if resp["supported_terms"] == nil {
		t.Errorf("expected supported_terms to still be present, got: %v", resp)
	}
	svcs, ok := resp["available_services"].([]any)
	if !ok || len(svcs) == 0 {
		t.Fatalf("available_services missing or empty: %v", resp["available_services"])
	}
	if !listContainsStr(svcs, "rds") {
		t.Errorf("available_services should contain rds; got: %v", svcs)
	}
	if resp["tip"] == nil {
		t.Errorf("expected a tip guiding the caller to specify service=, got: %v", resp)
	}
}

// TestDescribeCatalog_AWS_Network_LBNATWAF_HaveOwnExampleInvocation verifies
// that describe_catalog(domain=network, service=<lb|nat|waf>) returns an
// example_invocation scoped to that service (not the egress-shaped
// domain-level example) (RC3-022/RC3-025).
func TestDescribeCatalog_AWS_Network_LBNATWAF_HaveOwnExampleInvocation(t *testing.T) {
	realAWS := realAWSProvider(t)
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		describeCatFunc: func(ctx context.Context) (*models.ProviderCatalog, error) {
			return realAWS.DescribeCatalog(ctx)
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	for _, service := range []string{"lb", "nat", "waf"} {
		service := service
		t.Run(service, func(t *testing.T) {
			resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{
				Provider: "aws",
				Domain:   "network",
				Service:  service,
			})
			if resp["error"] != nil {
				t.Fatalf("unexpected error: %v", resp)
			}
			ex, ok := resp["example_invocation"].(map[string]any)
			if !ok {
				t.Fatalf("example_invocation missing or wrong type: %v", resp["example_invocation"])
			}
			if got := ex["service"]; got != service {
				t.Errorf("example_invocation.service = %v, want %q (must not be the egress-shaped domain-level example)", got, service)
			}
			// The egress-shaped domain-level example carries a
			// "destination_type" field; none of lb/nat/waf's examples should.
			if _, hasDestType := ex["destination_type"]; hasDestType {
				t.Errorf("example_invocation for service=%q should not carry egress's destination_type field: %v", service, ex)
			}
		})
	}
}

// TestDescribeCatalog_AWS_Network_DataTransferRedirectStillWorks is a
// regression guard for the RC3-022/025 fallback fix: domain=network,
// service=data_transfer must keep redirecting to domain=inter_region_egress
// (via serviceToDomain) and fall back to that domain's service-agnostic
// example, since that fallback path has no mismatched "service" field.
func TestDescribeCatalog_AWS_Network_DataTransferRedirectStillWorks(t *testing.T) {
	realAWS := realAWSProvider(t)
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		describeCatFunc: func(ctx context.Context) (*models.ProviderCatalog, error) {
			return realAWS.DescribeCatalog(ctx)
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{
		Provider: "aws",
		Domain:   "network",
		Service:  "data_transfer",
	})
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	if resp["domain"] != "inter_region_egress" {
		t.Errorf("expected redirect to domain=inter_region_egress, got domain=%v (full resp: %v)", resp["domain"], resp)
	}
	if resp["example_invocation"] == nil {
		t.Errorf("expected example_invocation to still be present after redirect, got: %v", resp)
	}
}

// TestDescribeCatalog_AWS_Compute_DomainLevelExampleStillFallsBack is a
// regression guard for the RC3-022/025 fallback fix: domain=compute,
// service=ec2 has no "compute/ec2" key in ExampleInvocations, so it must still
// fall back to the domain-level "compute" example (which has no "service"
// field and is therefore an unambiguous, safe fallback).
func TestDescribeCatalog_AWS_Compute_DomainLevelExampleStillFallsBack(t *testing.T) {
	realAWS := realAWSProvider(t)
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		describeCatFunc: func(ctx context.Context) (*models.ProviderCatalog, error) {
			return realAWS.DescribeCatalog(ctx)
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	resp := callDescribeCatalog(t, h, tools.DescribeCatalogInput{
		Provider: "aws",
		Domain:   "compute",
		Service:  "ec2",
	})
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp)
	}
	ex, ok := resp["example_invocation"].(map[string]any)
	if !ok {
		t.Fatalf("example_invocation missing or wrong type: %v", resp["example_invocation"])
	}
	if ex["resource_type"] != "m5.xlarge" {
		t.Errorf("expected domain-level compute example to still be used as fallback, got: %v", ex)
	}
}

// Compile-time check that mockProvider satisfies the Provider interface.
var _ providers.Provider = (*mockProvider)(nil)
