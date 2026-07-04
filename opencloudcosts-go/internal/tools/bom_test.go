// bom_test.go tests the estimate_bom, estimate_unit_economics, and
// get_discount_summary tool handlers.
package tools_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/tools"
)

// --------------------------------------------------------------------------
// Helpers shared by bom tests
// --------------------------------------------------------------------------

// callEstimateBOM invokes HandleEstimateBOM and decodes the JSON response.
func callEstimateBOM(t *testing.T, h *tools.Handler, items []map[string]any) map[string]any {
	t.Helper()
	in := tools.EstimateBOMInput{Items: items}
	result, _, err := h.HandleEstimateBOM(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("HandleEstimateBOM returned err: %v", err)
	}
	return decodeResult(t, result)
}

// callEstimateUnitEconomics invokes HandleEstimateUnitEconomics and decodes the response.
func callEstimateUnitEconomics(
	t *testing.T,
	h *tools.Handler,
	items []map[string]any,
	unitsPerMonth float64,
	unitLabel string,
) map[string]any {
	t.Helper()
	in := tools.EstimateUnitEconomicsInput{
		Items:         items,
		UnitsPerMonth: unitsPerMonth,
		UnitLabel:     unitLabel,
	}
	result, _, err := h.HandleEstimateUnitEconomics(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("HandleEstimateUnitEconomics returned err: %v", err)
	}
	return decodeResult(t, result)
}

// callGetDiscountSummary invokes HandleGetDiscountSummary and decodes the response.
func callGetDiscountSummary(t *testing.T, h *tools.Handler, provider string) map[string]any {
	t.Helper()
	in := tools.GetDiscountSummaryInput{Provider: provider}
	result, _, err := h.HandleGetDiscountSummary(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("HandleGetDiscountSummary returned err: %v", err)
	}
	return decodeResult(t, result)
}

// makeComputePrice returns a NormalizedPrice for a compute instance.
func makeComputePrice(provider, region, instanceType string, pricePerUnit float64) models.NormalizedPrice {
	return models.NormalizedPrice{
		Provider:     models.CloudProvider(provider),
		Service:      "compute",
		SKUID:        "TEST-SKU",
		Description:  instanceType + " Linux",
		Region:       region,
		PricingTerm:  models.PricingTermOnDemand,
		PricePerUnit: pricePerUnit,
		Unit:         models.PriceUnitPerHour,
		Currency:     "USD",
	}
}

// makeDatabasePrice returns a NormalizedPrice for a database instance.
func makeDatabasePrice(provider, region, instanceType string, pricePerUnit float64) models.NormalizedPrice {
	return models.NormalizedPrice{
		Provider:     models.CloudProvider(provider),
		Service:      "rds",
		SKUID:        "DB-SKU",
		Description:  instanceType,
		Region:       region,
		PricingTerm:  models.PricingTermOnDemand,
		PricePerUnit: pricePerUnit,
		Unit:         models.PriceUnitPerHour,
		Currency:     "USD",
	}
}

// makeStoragePrice returns a NormalizedPrice for storage.
func makeStoragePrice(provider, region string, pricePerGBMonth float64) models.NormalizedPrice {
	return models.NormalizedPrice{
		Provider:     models.CloudProvider(provider),
		Service:      "storage",
		SKUID:        "STOR-SKU",
		Description:  "gp3 storage",
		Region:       region,
		PricingTerm:  models.PricingTermOnDemand,
		PricePerUnit: pricePerGBMonth,
		Unit:         models.PriceUnitPerGBMonth,
		Currency:     "USD",
	}
}

// captureSpecProvider is a mockProvider that captures the PricingSpec it receives.
type captureSpecProvider struct {
	name      string
	region    string
	captured  models.PricingSpec
	returnErr error
	prices    []models.NormalizedPrice
}

func (c *captureSpecProvider) Name() models.CloudProvider                     { return models.CloudProvider(c.name) }
func (c *captureSpecProvider) DefaultRegion() string                          { return c.region }
func (c *captureSpecProvider) MajorRegions() []string                         { return nil }
func (c *captureSpecProvider) Supports(_ models.PricingDomain, _ string) bool { return true }
func (c *captureSpecProvider) SupportedTerms(_ models.PricingDomain, _ string) []models.PricingTerm {
	return []models.PricingTerm{models.PricingTermOnDemand}
}
func (c *captureSpecProvider) GetPrice(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
	c.captured = spec
	if c.returnErr != nil {
		return nil, c.returnErr
	}
	return &models.PricingResult{PublicPrices: c.prices}, nil
}
func (c *captureSpecProvider) GetComputePrice(_ context.Context, _, _, _ string, _ models.PricingTerm) ([]models.NormalizedPrice, error) {
	return nil, providers.ErrNotSupported
}
func (c *captureSpecProvider) GetStoragePrice(_ context.Context, _, _ string, _ float64) ([]models.NormalizedPrice, error) {
	return nil, providers.ErrNotSupported
}
func (c *captureSpecProvider) SearchPricing(_ context.Context, _, _ string, _ int) ([]models.NormalizedPrice, error) {
	return nil, providers.ErrNotSupported
}
func (c *captureSpecProvider) ListRegions(_ context.Context, _ string) ([]string, error) {
	return nil, providers.ErrNotSupported
}
func (c *captureSpecProvider) ListInstanceTypes(_ context.Context, _, _ string, _ int, _ float64, _ bool) ([]models.InstanceTypeInfo, error) {
	return nil, providers.ErrNotSupported
}
func (c *captureSpecProvider) CheckAvailability(_ context.Context, _, _, _ string) (bool, []string, error) {
	return false, nil, providers.ErrNotSupported
}
func (c *captureSpecProvider) GetEffectivePrice(_ context.Context, _ models.PricingSpec) ([]models.EffectivePrice, error) {
	return nil, providers.ErrNotSupported
}
func (c *captureSpecProvider) GetSpotHistory(_ context.Context, _ models.PricingSpec, _ int, _ string) (map[string]any, error) {
	return nil, providers.ErrNotSupported
}
func (c *captureSpecProvider) GetDiscountSummary(_ context.Context) (map[string]any, error) {
	return nil, providers.ErrNotSupported
}
func (c *captureSpecProvider) DescribeCatalog(_ context.Context) (*models.ProviderCatalog, error) {
	return &models.ProviderCatalog{Provider: models.CloudProvider(c.name)}, nil
}
func (c *captureSpecProvider) BOMAdvisories(_ context.Context, _ []string, _ string) ([]map[string]string, error) {
	return nil, nil
}

// --------------------------------------------------------------------------
// T20: OS field passthrough tests
// --------------------------------------------------------------------------

// TestEstimateBOM_T20_WindowsOS verifies that "os": "Windows" in the item dict
// flows through to the ComputePricingSpec passed to GetPrice.
func TestEstimateBOM_T20_WindowsOS(t *testing.T) {
	pvdr := &captureSpecProvider{
		name:   "aws",
		region: "us-east-1",
		prices: []models.NormalizedPrice{makeComputePrice("aws", "us-east-1", "m5.xlarge", 0.192)},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	item := map[string]any{
		"provider":      "aws",
		"domain":        "compute",
		"resource_type": "m5.xlarge",
		"region":        "us-east-1",
		"os":            "Windows",
		"quantity":      float64(1),
	}
	resp := callEstimateBOM(t, h, []map[string]any{item})

	if _, ok := resp["error"]; ok {
		t.Fatalf("expected success, got error: %v", resp["error"])
	}

	// Verify the captured spec has OS = "Windows" (T20).
	csSpec, ok := pvdr.captured.(*models.ComputePricingSpec)
	if !ok {
		t.Fatalf("expected *ComputePricingSpec, got %T", pvdr.captured)
	}
	if csSpec.OS != "Windows" {
		t.Errorf("T20: expected OS=Windows, got %q", csSpec.OS)
	}

	// Verify line_items returned.
	lineItems, ok := resp["line_items"].([]any)
	if !ok || len(lineItems) != 1 {
		t.Fatalf("expected 1 line item, got %v", resp["line_items"])
	}
}

// TestEstimateBOM_T20_DefaultLinux verifies that omitting "os" defaults to "Linux".
func TestEstimateBOM_T20_DefaultLinux(t *testing.T) {
	pvdr := &captureSpecProvider{
		name:   "aws",
		region: "us-east-1",
		prices: []models.NormalizedPrice{makeComputePrice("aws", "us-east-1", "t3.small", 0.023)},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	item := map[string]any{
		"provider":      "aws",
		"domain":        "compute",
		"resource_type": "t3.small",
		"region":        "us-east-1",
	}
	callEstimateBOM(t, h, []map[string]any{item})

	csSpec, ok := pvdr.captured.(*models.ComputePricingSpec)
	if !ok {
		t.Fatalf("expected *ComputePricingSpec, got %T", pvdr.captured)
	}
	if csSpec.OS != "Linux" {
		t.Errorf("T20 default: expected OS=Linux, got %q", csSpec.OS)
	}
}

// --------------------------------------------------------------------------
// T27: Database routing test
// --------------------------------------------------------------------------

// TestEstimateBOM_T27_DatabaseRouting verifies that a database item is correctly
// routed through GetPrice (not a separate compute path) and returns a valid
// line item with the database instance type in the description.
func TestEstimateBOM_T27_DatabaseRouting(t *testing.T) {
	pvdr := &captureSpecProvider{
		name:   "aws",
		region: "us-east-1",
		prices: []models.NormalizedPrice{makeDatabasePrice("aws", "us-east-1", "db.t4g.micro", 0.016)},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	item := map[string]any{
		"provider":      "aws",
		"domain":        "database",
		"service":       "rds",
		"resource_type": "db.t4g.micro",
		"engine":        "MySQL",
		"deployment":    "single-az",
		"region":        "us-east-1",
	}
	resp := callEstimateBOM(t, h, []map[string]any{item})

	if _, ok := resp["error"]; ok {
		t.Fatalf("expected success, got error: %v", resp["error"])
	}

	// Verify routing: captured spec must be DatabasePricingSpec.
	dbSpec, ok := pvdr.captured.(*models.DatabasePricingSpec)
	if !ok {
		t.Fatalf("T27: expected *DatabasePricingSpec, got %T", pvdr.captured)
	}
	if dbSpec.ResourceType != "db.t4g.micro" {
		t.Errorf("T27: expected resource_type=db.t4g.micro, got %q", dbSpec.ResourceType)
	}

	// Verify the description contains "db.t4g.micro".
	lineItems, ok := resp["line_items"].([]any)
	if !ok || len(lineItems) != 1 {
		t.Fatalf("expected 1 line item, got %v", resp["line_items"])
	}
	li, ok := lineItems[0].(map[string]any)
	if !ok {
		t.Fatalf("line item is not a map")
	}
	desc, _ := li["description"].(string)
	if !strings.Contains(desc, "db.t4g.micro") {
		t.Errorf("T27: expected description to contain 'db.t4g.micro', got %q", desc)
	}
}

// --------------------------------------------------------------------------
// Mixed 3-item BoM
// --------------------------------------------------------------------------

// TestEstimateBOM_MixedItems verifies a 3-item BoM returns 3 line items and
// correct totals.
func TestEstimateBOM_MixedItems(t *testing.T) {
	// compute: 0.192 $/hr × 730 hr × 2 qty = 280.32
	// database: 0.016 $/hr × 730 hr × 1 qty = 11.68
	// storage: 0.08 $/GB-month × 100 GB × 1 qty = 8.00
	// total monthly = 300.00
	callCount := 0
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		supportsFunc:  func(_ models.PricingDomain, _ string) bool { return true },
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			callCount++
			var price models.NormalizedPrice
			switch s := spec.(type) {
			case *models.ComputePricingSpec:
				price = makeComputePrice("aws", s.GetRegion(), s.ResourceType, 0.192)
			case *models.DatabasePricingSpec:
				price = makeDatabasePrice("aws", s.GetRegion(), s.ResourceType, 0.016)
			case *models.StoragePricingSpec:
				price = makeStoragePrice("aws", s.GetRegion(), 0.08)
			default:
				return nil, errors.New("unexpected spec type")
			}
			return &models.PricingResult{PublicPrices: []models.NormalizedPrice{price}}, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	items := []map[string]any{
		{
			"provider":      "aws",
			"domain":        "compute",
			"resource_type": "m5.xlarge",
			"region":        "us-east-1",
			"quantity":      float64(2),
		},
		{
			"provider":      "aws",
			"domain":        "database",
			"service":       "rds",
			"resource_type": "db.t4g.micro",
			"engine":        "MySQL",
			"deployment":    "single-az",
			"region":        "us-east-1",
		},
		{
			"provider":     "aws",
			"domain":       "storage",
			"storage_type": "gp3",
			"region":       "us-east-1",
		},
	}
	resp := callEstimateBOM(t, h, items)

	if _, ok := resp["error"]; ok {
		t.Fatalf("expected success, got error: %v", resp["error"])
	}

	lineItems, ok := resp["line_items"].([]any)
	if !ok {
		t.Fatalf("line_items is not a slice: %T", resp["line_items"])
	}
	if len(lineItems) != 3 {
		t.Errorf("expected 3 line items, got %d", len(lineItems))
	}

	totals, ok := resp["totals"].(map[string]any)
	if !ok {
		t.Fatalf("totals is not a map: %T", resp["totals"])
	}
	monthly, ok := totals["monthly"].(map[string]any)
	if !ok {
		t.Fatalf("totals.monthly is not a map")
	}
	// Expected monthly = 0.192*730*2 + 0.016*730*1 + 0.08*100*1
	//                  = 280.32 + 11.68 + 8.00 = 300.00
	amount, _ := monthly["amount"].(float64)
	if amount < 299.99 || amount > 300.01 {
		t.Errorf("expected monthly total ~300.00, got %.4f", amount)
	}
	if callCount != 3 {
		t.Errorf("expected 3 GetPrice calls, got %d", callCount)
	}
}

// --------------------------------------------------------------------------
// estimate_unit_economics basic test
// --------------------------------------------------------------------------

// TestEstimateUnitEconomics_Basic verifies the volume formatting and per-unit
// cost calculation for 10,000 users/month.
func TestEstimateUnitEconomics_Basic(t *testing.T) {
	// 0.10 $/hr × 730 hr × 1 qty = 73.00/month → 73/10000 = 0.0073 per user
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		supportsFunc:  func(_ models.PricingDomain, _ string) bool { return true },
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			price := models.NormalizedPrice{
				Provider:     models.CloudProviderAWS,
				Service:      "compute",
				SKUID:        "SKU",
				Region:       spec.GetRegion(),
				PricingTerm:  models.PricingTermOnDemand,
				PricePerUnit: 0.10,
				Unit:         models.PriceUnitPerHour,
				Currency:     "USD",
			}
			return &models.PricingResult{PublicPrices: []models.NormalizedPrice{price}}, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	items := []map[string]any{
		{
			"provider":      "aws",
			"domain":        "compute",
			"resource_type": "t3.small",
			"region":        "us-east-1",
		},
	}
	resp := callEstimateUnitEconomics(t, h, items, 10000, "user")

	if _, ok := resp["error"]; ok {
		t.Fatalf("expected success, got error: %v", resp["error"])
	}

	// Check volume format: "10,000 users/month"
	volume, _ := resp["volume"].(string)
	if volume != "10,000 users/month" {
		t.Errorf("expected volume='10,000 users/month', got %q", volume)
	}

	// Check infrastructure_monthly amount.
	infra, ok := resp["infrastructure_monthly"].(map[string]any)
	if !ok {
		t.Fatalf("infrastructure_monthly is not a map")
	}
	infraAmt, _ := infra["amount"].(float64)
	// 0.10 * 730 * 1 = 73.00
	if infraAmt < 72.99 || infraAmt > 73.01 {
		t.Errorf("expected infrastructure_monthly ~73.00, got %.4f", infraAmt)
	}

	// Check cost_per_unit amount.
	cpu, ok := resp["cost_per_unit"].(map[string]any)
	if !ok {
		t.Fatalf("cost_per_unit is not a map")
	}
	cpuAmt, _ := cpu["amount"].(float64)
	// 73.00 / 10000 = 0.0073
	if cpuAmt < 0.0072 || cpuAmt > 0.0074 {
		t.Errorf("expected cost_per_unit ~0.0073, got %.6f", cpuAmt)
	}

	// Check cost_per_unit display contains " per user".
	cpuDisplay, _ := cpu["display"].(string)
	if !strings.Contains(cpuDisplay, "per user") {
		t.Errorf("expected display to contain 'per user', got %q", cpuDisplay)
	}

	// Check important field mentions the region.
	important, _ := resp["important"].(string)
	if !strings.Contains(important, "us-east-1") {
		t.Errorf("expected important to mention region, got %q", important)
	}
}

// TestEstimateUnitEconomics_DefaultLabel verifies "user" is the default unit_label.
func TestEstimateUnitEconomics_DefaultLabel(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		supportsFunc:  func(_ models.PricingDomain, _ string) bool { return true },
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			price := models.NormalizedPrice{
				Provider:     models.CloudProviderAWS,
				Service:      "compute",
				SKUID:        "SKU",
				Region:       "us-east-1",
				PricePerUnit: 0.05,
				Unit:         models.PriceUnitPerHour,
				Currency:     "USD",
			}
			return &models.PricingResult{PublicPrices: []models.NormalizedPrice{price}}, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	items := []map[string]any{
		{
			"provider":      "aws",
			"domain":        "compute",
			"resource_type": "t3.micro",
			"region":        "us-east-1",
		},
	}
	resp := callEstimateUnitEconomics(t, h, items, 1000, "") // empty label → "user"

	volume, _ := resp["volume"].(string)
	if !strings.Contains(volume, "users/month") {
		t.Errorf("expected default label 'users', got %q", volume)
	}
}

// --------------------------------------------------------------------------
// get_discount_summary — GCP not_supported
// --------------------------------------------------------------------------

// TestGetDiscountSummary_GCPNotSupported verifies that GCP returns the structured
// not_supported error when GetDiscountSummary returns ErrNotSupported.
func TestGetDiscountSummary_GCPNotSupported(t *testing.T) {
	pvdr := &mockProvider{
		name:          "gcp",
		defaultRegion: "us-central1",
		discountFunc: func(_ context.Context) (map[string]any, error) {
			return nil, providers.ErrNotSupported
		},
	}
	h := tools.New(map[string]tools.Provider{"gcp": pvdr})

	resp := callGetDiscountSummary(t, h, "gcp")

	errVal, _ := resp["error"].(string)
	if errVal != "not_supported" {
		t.Errorf("expected error='not_supported', got %q", errVal)
	}
	provVal, _ := resp["provider"].(string)
	if provVal != "gcp" {
		t.Errorf("expected provider='gcp', got %q", provVal)
	}
	reason, _ := resp["reason"].(string)
	if reason == "" {
		t.Error("expected non-empty reason in not_supported response")
	}
	alts, _ := resp["alternatives"].([]any)
	if len(alts) == 0 {
		t.Error("expected non-empty alternatives in not_supported response")
	}
}

// TestGetDiscountSummary_ProviderNotConfigured verifies the error when the
// requested provider is not in the map.
func TestGetDiscountSummary_ProviderNotConfigured(t *testing.T) {
	h := tools.New(nil)
	resp := callGetDiscountSummary(t, h, "aws")

	errVal, _ := resp["error"].(string)
	if errVal == "" {
		t.Error("expected error for unconfigured provider")
	}
}

// TestGetDiscountSummary_AWSSuccess verifies that a successful AWS discount
// summary is returned verbatim.
func TestGetDiscountSummary_AWSSuccess(t *testing.T) {
	awsResult := map[string]any{
		"savings_plans":      []any{},
		"reserved_instances": []any{},
		"status":             "ok",
	}
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		discountFunc: func(_ context.Context) (map[string]any, error) {
			return awsResult, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	resp := callGetDiscountSummary(t, h, "aws")

	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("expected success, got error: %v", resp["error"])
	}
	if resp["status"] != "ok" {
		t.Errorf("expected status=ok, got %v", resp["status"])
	}
}

// --------------------------------------------------------------------------
// estimate_bom — advisory rows (BOMAdvisories)
// --------------------------------------------------------------------------

// TestEstimateBOM_Advisories verifies that BOMAdvisories rows are included
// in not_included when the provider returns them.
func TestEstimateBOM_Advisories(t *testing.T) {
	advisory := map[string]string{
		"item":         "Egress",
		"estimate":     "variable",
		"how_to_price": `call get_price({"provider":"aws","domain":"inter_region_egress",...})`,
	}
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		supportsFunc:  func(_ models.PricingDomain, _ string) bool { return true },
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			price := makeComputePrice("aws", spec.GetRegion(), "m5.xlarge", 0.192)
			return &models.PricingResult{PublicPrices: []models.NormalizedPrice{price}}, nil
		},
	}
	// Override BOMAdvisories via the embedded mock field.
	pvdrWithAdvisory := &mockProviderWithAdvisories{
		mockProvider: pvdr,
		advisories:   []map[string]string{advisory},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdrWithAdvisory})

	items := []map[string]any{
		{
			"provider":      "aws",
			"domain":        "compute",
			"resource_type": "m5.xlarge",
			"region":        "us-east-1",
		},
	}
	resp := callEstimateBOM(t, h, items)

	if _, ok := resp["error"]; ok {
		t.Fatalf("expected success, got: %v", resp)
	}

	notIncluded := resp["not_included"]
	if notIncluded == nil {
		t.Fatal("expected not_included to be set when advisories are present")
	}
	rows, ok := notIncluded.([]any)
	if !ok || len(rows) == 0 {
		t.Fatalf("expected non-empty not_included slice, got: %v", notIncluded)
	}
	notIncludedAction, _ := resp["not_included_action"].(string)
	if !strings.Contains(notIncludedAction, "SUPPLEMENTARY") {
		t.Errorf("expected not_included_action to contain 'SUPPLEMENTARY', got %q", notIncludedAction)
	}
	if strings.Contains(notIncludedAction, "REQUIRED") {
		t.Errorf("not_included_action must not contain 'REQUIRED', got %q", notIncludedAction)
	}
}

// mockProviderWithAdvisories wraps mockProvider and overrides BOMAdvisories.
type mockProviderWithAdvisories struct {
	*mockProvider
	advisories []map[string]string
}

func (m *mockProviderWithAdvisories) BOMAdvisories(_ context.Context, _ []string, _ string) ([]map[string]string, error) {
	return m.advisories, nil
}

// --------------------------------------------------------------------------
// estimate_bom — no valid items
// --------------------------------------------------------------------------

// TestEstimateBOM_NoValidItems verifies the error response when no items succeed.
func TestEstimateBOM_NoValidItems(t *testing.T) {
	h := tools.New(nil) // no providers configured
	items := []map[string]any{
		{
			"provider":      "aws",
			"domain":        "compute",
			"resource_type": "m5.xlarge",
			"region":        "us-east-1",
		},
	}
	resp := callEstimateBOM(t, h, items)

	errVal, ok := resp["error"]
	if !ok || errVal == "" {
		t.Error("expected error key when no valid items")
	}
	// bom.go's per-item errors are plain strings, not maps, so the region is
	// embedded in the message text rather than a separate JSON key
	// (region-in-errors fix).
	errStr, _ := errVal.(string)
	if !strings.Contains(errStr, "us-east-1") {
		t.Errorf("expected error message to mention region 'us-east-1', got: %q", errStr)
	}
}

// TestEstimateBOM_NotSupported_MessageIncludesRegion verifies the
// "does not support" per-item error message embeds the item's region
// (region-in-errors fix).
func TestEstimateBOM_NotSupported_MessageIncludesRegion(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		supportsFunc:  func(_ models.PricingDomain, _ string) bool { return false },
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	items := []map[string]any{
		{
			"provider":      "aws",
			"domain":        "compute",
			"resource_type": "m5.xlarge",
			"region":        "eu-west-1",
		},
	}
	resp := callEstimateBOM(t, h, items)

	errStr, _ := resp["error"].(string)
	if !strings.Contains(errStr, "eu-west-1") {
		t.Errorf("expected 'does not support' error to mention region 'eu-west-1', got: %q", errStr)
	}
}

// TestEstimateBOM_NoPricingFound_MessageIncludesRegion verifies the
// "no pricing found for spec" per-item error message embeds the item's
// region (region-in-errors fix).
func TestEstimateBOM_NoPricingFound_MessageIncludesRegion(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		supportsFunc:  func(_ models.PricingDomain, _ string) bool { return true },
		getPriceFunc: func(_ context.Context, _ models.PricingSpec) (*models.PricingResult, error) {
			return &models.PricingResult{PublicPrices: nil}, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})
	items := []map[string]any{
		{
			"provider":      "aws",
			"domain":        "compute",
			"resource_type": "m5.xlarge",
			"region":        "ap-south-1",
		},
	}
	resp := callEstimateBOM(t, h, items)

	errStr, _ := resp["error"].(string)
	if !strings.Contains(errStr, "ap-south-1") {
		t.Errorf("expected 'no pricing found' error to mention region 'ap-south-1', got: %q", errStr)
	}
}

// --------------------------------------------------------------------------
// estimate_bom — null fields
// --------------------------------------------------------------------------

// TestEstimateBOM_NullFields verifies that not_included, not_included_action,
// and errors are null (not absent, not []) when empty.
func TestEstimateBOM_NullFields(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		supportsFunc:  func(_ models.PricingDomain, _ string) bool { return true },
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			price := makeComputePrice("aws", spec.GetRegion(), "t3.small", 0.023)
			return &models.PricingResult{PublicPrices: []models.NormalizedPrice{price}}, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	items := []map[string]any{
		{
			"provider":      "aws",
			"domain":        "compute",
			"resource_type": "t3.small",
			"region":        "us-east-1",
		},
	}
	resp := callEstimateBOM(t, h, items)

	// These keys must be present and nil.
	for _, key := range []string{"not_included", "not_included_action", "errors"} {
		v, present := resp[key]
		if !present {
			t.Errorf("key %q missing from response", key)
			continue
		}
		if v != nil {
			t.Errorf("key %q should be null, got %v", key, v)
		}
	}
}

// --------------------------------------------------------------------------
// Monthly cost math
// --------------------------------------------------------------------------

// --------------------------------------------------------------------------
// estimate_bom — partial failure
// --------------------------------------------------------------------------

// TestEstimateBOM_PartialFailure verifies that when one item fails pricing and
// another succeeds, the response includes the successful line item AND an errors
// field describing the failed item (not a top-level error).
func TestEstimateBOM_PartialFailure(t *testing.T) {
	callNum := 0
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		supportsFunc:  func(_ models.PricingDomain, _ string) bool { return true },
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			callNum++
			if callNum == 1 {
				// First item succeeds.
				price := makeComputePrice("aws", spec.GetRegion(), "m5.xlarge", 0.192)
				return &models.PricingResult{PublicPrices: []models.NormalizedPrice{price}}, nil
			}
			// Second item fails.
			return nil, errors.New("pricing lookup failed")
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	items := []map[string]any{
		{
			"provider":      "aws",
			"domain":        "compute",
			"resource_type": "m5.xlarge",
			"region":        "us-east-1",
		},
		{
			"provider":      "aws",
			"domain":        "compute",
			"resource_type": "m5.2xlarge",
			"region":        "us-east-1",
		},
	}
	resp := callEstimateBOM(t, h, items)

	// Must NOT have a top-level "error" key — only partial failures.
	if topErr, ok := resp["error"]; ok {
		t.Fatalf("expected no top-level error for partial failure, got: %v", topErr)
	}

	// Must have exactly 1 successful line item.
	lineItems, ok := resp["line_items"].([]any)
	if !ok || len(lineItems) != 1 {
		t.Fatalf("expected 1 line item from partial success, got: %v", resp["line_items"])
	}

	// Must have errors field set (not nil) with one entry.
	errsVal := resp["errors"]
	if errsVal == nil {
		t.Fatal("expected errors field to be set for the failed item, got nil")
	}
	errs, ok := errsVal.([]any)
	if !ok || len(errs) == 0 {
		t.Fatalf("expected non-empty errors slice, got: %v", errsVal)
	}
	// The failed item's message must embed its region (region-in-errors fix).
	errStr, _ := errs[0].(string)
	if !strings.Contains(errStr, "us-east-1") {
		t.Errorf("expected failed item's error to mention region 'us-east-1', got: %q", errStr)
	}
}

// --------------------------------------------------------------------------
// Monthly cost math
// --------------------------------------------------------------------------

// TestBOMMonthlyCostMath verifies per_hour, per_gb_month, and per_month
// unit routing produces correct monthly costs.
func TestBOMMonthlyCostMath(t *testing.T) {
	tests := []struct {
		name          string
		unit          models.PriceUnit
		pricePerUnit  float64
		qty           float64 // item quantity
		sizeGB        float64
		hoursPerMonth float64
		wantMonthly   float64
	}{
		{
			name:          "per_hour",
			unit:          models.PriceUnitPerHour,
			pricePerUnit:  0.192,
			qty:           2,
			sizeGB:        100,
			hoursPerMonth: 730,
			wantMonthly:   0.192 * 730 * 2,
		},
		{
			name:          "per_gb_month",
			unit:          models.PriceUnitPerGBMonth,
			pricePerUnit:  0.08,
			qty:           1,
			sizeGB:        500,
			hoursPerMonth: 730,
			wantMonthly:   0.08 * 500 * 1,
		},
		{
			name:          "per_month",
			unit:          models.PriceUnitPerMonth,
			pricePerUnit:  20.0,
			qty:           3,
			sizeGB:        100,
			hoursPerMonth: 730,
			wantMonthly:   20.0 * 3,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			pvdr := &mockProvider{
				name:          "aws",
				defaultRegion: "us-east-1",
				supportsFunc:  func(_ models.PricingDomain, _ string) bool { return true },
				getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
					price := models.NormalizedPrice{
						Provider:     models.CloudProviderAWS,
						Service:      "test",
						Region:       "us-east-1",
						PricingTerm:  models.PricingTermOnDemand,
						PricePerUnit: tc.pricePerUnit,
						Unit:         tc.unit,
						Currency:     "USD",
					}
					return &models.PricingResult{PublicPrices: []models.NormalizedPrice{price}}, nil
				},
			}
			h := tools.New(map[string]tools.Provider{"aws": pvdr})

			item := map[string]any{
				"provider":        "aws",
				"domain":          "compute",
				"resource_type":   "m5.xlarge",
				"region":          "us-east-1",
				"quantity":        tc.qty,
				"hours_per_month": tc.hoursPerMonth,
				"size_gb":         tc.sizeGB,
			}
			resp := callEstimateBOM(t, h, []map[string]any{item})

			if _, ok := resp["error"]; ok {
				t.Fatalf("expected success: %v", resp)
			}
			lineItems, _ := resp["line_items"].([]any)
			if len(lineItems) == 0 {
				t.Fatal("no line items")
			}
			li := lineItems[0].(map[string]any)
			mc := li["monthly_cost"].(map[string]any)
			amount, _ := mc["amount"].(float64)
			if amount < tc.wantMonthly-0.01 || amount > tc.wantMonthly+0.01 {
				t.Errorf("expected monthly_cost=%.4f, got %.4f", tc.wantMonthly, amount)
			}
		})
	}
}

// TestEstimateBOM_FractionalQuantityNotTruncated verifies that a fractional
// "quantity" (e.g. 2.9) is honored exactly rather than silently truncated to
// an int. Prior to the fix, processBOMItems extracted quantity via int(n),
// so 2.9 became 2 — both the reported "quantity" field and the computed
// monthly_cost were wrong with no error or warning surfaced to the caller.
func TestEstimateBOM_FractionalQuantityNotTruncated(t *testing.T) {
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

	item := map[string]any{
		"provider":      "aws",
		"domain":        "compute",
		"resource_type": "m5.xlarge",
		"region":        "us-east-1",
		"quantity":      2.9,
	}
	resp := callEstimateBOM(t, h, []map[string]any{item})

	if _, ok := resp["error"]; ok {
		t.Fatalf("expected success: %v", resp)
	}
	lineItems, _ := resp["line_items"].([]any)
	if len(lineItems) != 1 {
		t.Fatalf("expected 1 line item, got: %v", resp["line_items"])
	}
	li := lineItems[0].(map[string]any)

	gotQty, _ := li["quantity"].(float64)
	if gotQty != 2.9 {
		t.Errorf("expected quantity == 2.9 (not truncated to 2), got %v", gotQty)
	}

	mc := li["monthly_cost"].(map[string]any)
	amount, _ := mc["amount"].(float64)
	// 2.9 x $0.192/hr x 730hr = $406.65. Truncation to 2 would give ~$280.32.
	want := 0.192 * 730 * 2.9
	if amount < want-0.01 || amount > want+0.01 {
		t.Errorf("expected monthly_cost ~%.4f (fractional quantity honored), got %.4f", want, amount)
	}
}

// --------------------------------------------------------------------------
// estimate_bom — not_included_action wording regression
// --------------------------------------------------------------------------

// TestEstimateBOM_NotIncludedAction_NoREQUIRED asserts that the not_included_action
// field uses SUPPLEMENTARY framing rather than an unconditional REQUIRED mandate.
// This prevents regression to the old wording that caused context explosion on
// committed-pricing comparison tests (CCR2 pattern).
func TestEstimateBOM_NotIncludedAction_NoREQUIRED(t *testing.T) {
	advisory := map[string]string{
		"item":         "Data transfer (egress)",
		"estimate":     "variable",
		"how_to_price": `get_price({"provider":"aws","domain":"network","service":"data_transfer","region":"us-east-1"})`,
	}
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		supportsFunc:  func(_ models.PricingDomain, _ string) bool { return true },
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			price := makeComputePrice("aws", spec.GetRegion(), "m5.large", 0.096)
			return &models.PricingResult{PublicPrices: []models.NormalizedPrice{price}}, nil
		},
	}
	pvdrWithAdvisory := &mockProviderWithAdvisories{
		mockProvider: pvdr,
		advisories:   []map[string]string{advisory},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdrWithAdvisory})

	items := []map[string]any{
		{
			"provider":      "aws",
			"domain":        "compute",
			"resource_type": "m5.large",
			"region":        "us-east-1",
		},
	}
	resp := callEstimateBOM(t, h, items)

	if _, ok := resp["error"]; ok {
		t.Fatalf("expected success, got: %v", resp)
	}

	// (a) not_included_action must NOT contain "REQUIRED:" — the unconditional mandate.
	action, _ := resp["not_included_action"].(string)
	if action == "" {
		t.Fatal("not_included_action is empty; expected it to be set when advisories are present")
	}
	if strings.Contains(action, "REQUIRED:") {
		t.Errorf("not_included_action must not contain 'REQUIRED:' (unconditional mandate causes context explosion); got %q", action)
	}

	// (b) not_included_action must contain "SUPPLEMENTARY" and "only if" (contextual framing).
	if !strings.Contains(action, "SUPPLEMENTARY") {
		t.Errorf("not_included_action must contain 'SUPPLEMENTARY', got %q", action)
	}
	if !strings.Contains(action, "only if") {
		t.Errorf("not_included_action must contain 'only if' (opt-out framing), got %q", action)
	}

	// (c) not_included items are still present — we changed the action, not removed items.
	notIncluded, ok := resp["not_included"]
	if !ok || notIncluded == nil {
		t.Fatal("not_included must still be set when advisories are present")
	}
	rows, ok := notIncluded.([]any)
	if !ok || len(rows) == 0 {
		t.Errorf("not_included must be a non-empty slice, got: %v", notIncluded)
	}
}

// --------------------------------------------------------------------------
// RC3-006: fallback flag passthrough
// --------------------------------------------------------------------------

// TestEstimateBOM_FallbackFlagSurfaced reproduces the issue #32 scenario: a
// storage price returned with Attributes["fallback"]="true" (e.g. AWS's
// us-east-1 static EBS rate served for an unsupported region such as
// eu-central-2) must have that flag — and the accompanying fallback_note —
// surfaced on the estimate_bom line item, not silently dropped.
func TestEstimateBOM_FallbackFlagSurfaced(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		supportsFunc:  func(_ models.PricingDomain, _ string) bool { return true },
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			price := makeStoragePrice("aws", spec.GetRegion(), 0.08)
			price.Attributes = map[string]string{
				"fallback":      "true",
				"fallback_note": "static published rate — verify at https://aws.amazon.com/ebs/pricing/",
			}
			return &models.PricingResult{PublicPrices: []models.NormalizedPrice{price}}, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	items := []map[string]any{
		{
			"provider":     "aws",
			"domain":       "storage",
			"storage_type": "gp3",
			"size_gb":      float64(500),
			"region":       "eu-central-2",
		},
	}
	resp := callEstimateBOM(t, h, items)

	if _, ok := resp["error"]; ok {
		t.Fatalf("expected success, got error: %v", resp["error"])
	}

	lineItems, ok := resp["line_items"].([]any)
	if !ok || len(lineItems) != 1 {
		t.Fatalf("expected 1 line item, got %v", resp["line_items"])
	}
	li, ok := lineItems[0].(map[string]any)
	if !ok {
		t.Fatalf("line item is not a map: %T", lineItems[0])
	}

	fallback, ok := li["fallback"].(string)
	if !ok || fallback != "true" {
		t.Errorf("expected line_items[0][\"fallback\"] == \"true\", got %v (present=%v)", li["fallback"], ok)
	}
	note, _ := li["fallback_note"].(string)
	if note == "" {
		t.Errorf("expected non-empty line_items[0][\"fallback_note\"], got %q", note)
	}
}

// TestEstimateBOM_NoFallbackFlagWhenLive is the control case: a price with no
// "fallback" attribute (a normal live catalog lookup) must NOT have a
// "fallback" key injected into the line item.
func TestEstimateBOM_NoFallbackFlagWhenLive(t *testing.T) {
	pvdr := &mockProvider{
		name:          "aws",
		defaultRegion: "us-east-1",
		supportsFunc:  func(_ models.PricingDomain, _ string) bool { return true },
		getPriceFunc: func(_ context.Context, spec models.PricingSpec) (*models.PricingResult, error) {
			price := makeStoragePrice("aws", spec.GetRegion(), 0.1142)
			return &models.PricingResult{PublicPrices: []models.NormalizedPrice{price}}, nil
		},
	}
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	items := []map[string]any{
		{
			"provider":     "aws",
			"domain":       "storage",
			"storage_type": "gp3",
			"size_gb":      float64(500),
			"region":       "eu-central-2",
		},
	}
	resp := callEstimateBOM(t, h, items)

	if _, ok := resp["error"]; ok {
		t.Fatalf("expected success, got error: %v", resp["error"])
	}

	lineItems, ok := resp["line_items"].([]any)
	if !ok || len(lineItems) != 1 {
		t.Fatalf("expected 1 line item, got %v", resp["line_items"])
	}
	li, ok := lineItems[0].(map[string]any)
	if !ok {
		t.Fatalf("line item is not a map: %T", lineItems[0])
	}

	if _, present := li["fallback"]; present {
		t.Errorf("expected no \"fallback\" key on a live-priced line item, got %v", li["fallback"])
	}
	if _, present := li["fallback_note"]; present {
		t.Errorf("expected no \"fallback_note\" key on a live-priced line item, got %v", li["fallback_note"])
	}
}
