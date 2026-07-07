package tools_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/config"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers"
	awsprovider "github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers/aws"
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

// TestCompareBOMRegions_RawSKUItem verifies a raw-SKU BoM item (issue #31,
// RC3-004) resolves per region against a real *awsprovider.Provider — same
// fixture/mocking pattern as TestHandleGetPriceBySKU_HappyPath in
// sku_lookup_test.go, since resolveBOMSKUItem type-asserts the concrete AWS
// provider rather than going through the mockProvider interface.
func TestCompareBOMRegions_RawSKUItem(t *testing.T) {
	awsprovider.ResetSKUCatalogCacheForTesting()

	mux := http.NewServeMux()
	mux.HandleFunc("/AmazonEC2/current/us-east-1/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(skuFixtureJSON("SKU1", "BoxUsage:r6id.24xlarge", "US East (N. Virginia)", "0.5000000000")))
	})
	mux.HandleFunc("/AmazonEC2/current/us-west-2/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(skuFixtureJSON("SKU2", "USW2-BoxUsage:r6id.24xlarge", "US West (Oregon)", "0.6000000000")))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	restore := awsprovider.SetBulkPricingBaseURLForTesting(server.URL)
	defer restore()

	realAWS, err := awsprovider.NewProvider(&config.Config{}, nil)
	if err != nil {
		t.Fatalf("awsprovider.NewProvider: %v", err)
	}
	h := tools.New(map[string]tools.Provider{"aws": realAWS})

	resp := callCompareBOMRegions(t, h, tools.CompareBOMRegionsInput{
		Items: []map[string]any{
			{"sku": "BoxUsage:r6id.24xlarge", "service": "AmazonEC2", "quantity": float64(2)},
		},
		Regions: []string{"us-east-1", "us-west-2"},
	})

	regions, ok := resp["regions"].([]any)
	if !ok || len(regions) != 2 {
		t.Fatalf("expected 2 region entries, got: %v", resp["regions"])
	}

	first := regions[0].(map[string]any)
	if first["region"] != "us-east-1" {
		t.Errorf("expected cheapest region us-east-1 first, got %v", first["region"])
	}
	lineItems, ok := first["line_items"].([]any)
	if !ok || len(lineItems) != 1 {
		t.Fatalf("expected 1 line item for us-east-1, got: %v", first["line_items"])
	}
	li := lineItems[0].(map[string]any)
	if li["sku"] != "BoxUsage:r6id.24xlarge" {
		t.Errorf("expected sku field populated, got %v", li["sku"])
	}
	monthly := li["monthly_cost"].(map[string]any)
	// 0.50/hr * 730 hrs/mo (default) * quantity 2 = $730.00/mo.
	if monthly["display"] != "$730.00/mo" {
		t.Errorf("expected monthly_cost $730.00/mo, got %v", monthly["display"])
	}

	last := regions[1].(map[string]any)
	if last["region"] != "us-west-2" {
		t.Errorf("expected us-west-2 second (more expensive), got %v", last["region"])
	}
}

// TestCompareBOMRegions_RawSKUNonAWSProviderReportedOnce verifies a raw-SKU
// item with an explicit unsupported (non-aws, non-gcp, non-azure) provider is
// reported once in not_supported (Finding 1 fix), not duplicated once per
// compared region.
//
// NOTE: this test previously used provider="gcp", then provider="azure", as
// its "unsupported" example. As of RC3-015 (GCP raw-SKU parity) and this
// step's Azure raw-SKU wiring, both "gcp" and "azure" are legitimately
// accepted at the partition step above (HandleCompareBOMRegions), so neither
// exercises the not_supported path anymore — see
// TestCompareBOMRegions_GCPRawSKUItem and TestCompareBOMRegions_AzureRawSKUItem
// below for their new (resolvable) behavior. This test now uses a
// fictitious provider name so it continues to guard the not_supported path —
// and doubles as the regression check that widening acceptance to
// aws/gcp/azure didn't accidentally start accepting arbitrary providers too.
func TestCompareBOMRegions_RawSKUNonAWSProviderReportedOnce(t *testing.T) {
	pvdr := newRegionPricedProvider(map[string]float64{"us-east-1": 0.192, "us-west-2": 0.150})
	h := tools.New(map[string]tools.Provider{"aws": pvdr})

	resp := callCompareBOMRegions(t, h, tools.CompareBOMRegionsInput{
		Items: []map[string]any{
			{"sku": "BoxUsage:m5.xlarge", "provider": "oraclecloud", "service": "AmazonEC2"},
		},
		Regions: []string{"us-east-1", "us-west-2"},
	})

	notSupported, ok := resp["not_supported"].([]any)
	if !ok || len(notSupported) != 1 {
		t.Fatalf("expected exactly 1 not_supported entry, got: %v", resp["not_supported"])
	}
	entry := notSupported[0].(map[string]any)
	if entry["provider"] != "oraclecloud" {
		t.Errorf("expected oraclecloud in not_supported entry, got %v", entry)
	}

	regions := resp["regions"].([]any)
	for _, r := range regions {
		region := r.(map[string]any)
		if errs, ok := region["errors"].([]any); ok && len(errs) > 0 {
			t.Errorf("expected no per-region errors for the unsupported-provider raw-SKU item (should be reported once at top level), got: %v in region %v", errs, region["region"])
		}
	}
}

// TestCompareBOMRegions_GCPRawSKUItem verifies a GCP raw-SKU BoM item
// resolves per region against a real *gcpprovider.Provider — the GCP
// counterpart to TestCompareBOMRegions_RawSKUItem above, added for RC3-015.
func TestCompareBOMRegions_GCPRawSKUItem(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(gcpSKUCatalogFixtureJSON(
			"0055-9F63-3A4D", "N1 Predefined Instance Core running in Americas", "us-central1", "0", 40_000_000)))
	}))
	defer server.Close()
	realGCP := newGCPSKUTestProvider(t, server)
	h := tools.New(map[string]tools.Provider{"gcp": realGCP})

	resp := callCompareBOMRegions(t, h, tools.CompareBOMRegionsInput{
		Items: []map[string]any{
			{"sku": "0055-9F63-3A4D", "provider": "gcp", "service": "compute", "quantity": float64(1)},
		},
		Regions: []string{"us-central1"},
	})

	if _, ok := resp["error"]; ok {
		t.Fatalf("expected success, got error: %v", resp["error"])
	}
	if notSupported, ok := resp["not_supported"].([]any); ok && len(notSupported) > 0 {
		t.Fatalf("expected the gcp raw-SKU item to resolve (not not_supported), got: %v", notSupported)
	}

	regions, ok := resp["regions"].([]any)
	if !ok || len(regions) != 1 {
		t.Fatalf("expected 1 region entry, got: %v", resp["regions"])
	}
	region := regions[0].(map[string]any)
	if region["region"] != "us-central1" {
		t.Errorf("expected region us-central1, got %v", region["region"])
	}
	lineItems, ok := region["line_items"].([]any)
	if !ok || len(lineItems) != 1 {
		t.Fatalf("expected 1 line item for us-central1, got: %v", region["line_items"])
	}
	li := lineItems[0].(map[string]any)
	if li["sku"] != "0055-9F63-3A4D" {
		t.Errorf("expected sku field populated, got %v", li["sku"])
	}
	monthly := li["monthly_cost"].(map[string]any)
	// 0.04/hr * 730 hrs/mo (default) * quantity 1 = $29.20/mo.
	if monthly["display"] != "$29.20/mo" {
		t.Errorf("expected monthly_cost $29.20/mo, got %v", monthly["display"])
	}
}

// TestCompareBOMRegions_AzureRawSKUItem verifies an Azure raw-SKU BoM item
// (a Retail Prices API meterId) resolves per region against a real
// *azureprovider.Provider — the Azure counterpart to
// TestCompareBOMRegions_GCPRawSKUItem above.
func TestCompareBOMRegions_AzureRawSKUItem(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(azureSKUFixtureJSON(
			"00000000-0000-0000-0000-000000000000", "eastus", "D4s v3", "Virtual Machines Dsv3 Series", "Virtual Machines", 0.192)))
	}))
	defer server.Close()
	realAzure := newAzureSKUTestProvider(server)
	h := tools.New(map[string]tools.Provider{"azure": realAzure})

	resp := callCompareBOMRegions(t, h, tools.CompareBOMRegionsInput{
		Items: []map[string]any{
			{"sku": "00000000-0000-0000-0000-000000000000", "provider": "azure", "quantity": float64(1)},
		},
		Regions: []string{"eastus"},
	})

	if _, ok := resp["error"]; ok {
		t.Fatalf("expected success, got error: %v", resp["error"])
	}
	if notSupported, ok := resp["not_supported"].([]any); ok && len(notSupported) > 0 {
		t.Fatalf("expected the azure raw-SKU item to resolve (not not_supported), got: %v", notSupported)
	}

	regions, ok := resp["regions"].([]any)
	if !ok || len(regions) != 1 {
		t.Fatalf("expected 1 region entry, got: %v", resp["regions"])
	}
	region := regions[0].(map[string]any)
	if region["region"] != "eastus" {
		t.Errorf("expected region eastus, got %v", region["region"])
	}
	lineItems, ok := region["line_items"].([]any)
	if !ok || len(lineItems) != 1 {
		t.Fatalf("expected 1 line item for eastus, got: %v", region["line_items"])
	}
	li := lineItems[0].(map[string]any)
	if li["sku"] != "00000000-0000-0000-0000-000000000000" {
		t.Errorf("expected sku field populated, got %v", li["sku"])
	}
	monthly := li["monthly_cost"].(map[string]any)
	// 0.192/hr * 730 hrs/mo (default) * quantity 1 = $140.16/mo.
	if monthly["display"] != "$140.16/mo" {
		t.Errorf("expected monthly_cost $140.16/mo, got %v", monthly["display"])
	}
}
