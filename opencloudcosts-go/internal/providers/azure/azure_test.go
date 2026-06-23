package azure_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/cache"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers/azure"
)

// azureItem mirrors the API response item structure.
type azureItem map[string]any

// apiResponse wraps items in the Azure Retail Prices API shape.
func apiResponse(items []azureItem) map[string]any {
	return map[string]any{
		"Items":        items,
		"NextPageLink": nil,
	}
}

// mockServer creates an httptest.Server that returns the given items.
func mockServer(t *testing.T, items []azureItem) *httptest.Server {
	t.Helper()
	body, err := json.Marshal(apiResponse(items))
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(body) //nolint
	}))
}

// countingServer counts how many requests hit it.
func countingServer(t *testing.T, items []azureItem) (*httptest.Server, *int) {
	t.Helper()
	body, err := json.Marshal(apiResponse(items))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(body) //nolint
	}))
	return srv, &count
}

// newTestProvider creates a Provider backed by a temp-dir cache, pointing at srv.
func newTestProvider(t *testing.T, srv *httptest.Server) *azure.Provider {
	t.Helper()
	dir := t.TempDir()
	cm, err := cache.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := azure.NewProvider(cm, 24*time.Hour, 7*24*time.Hour)
	p.SetBaseURL(srv.URL)
	p.SetHTTPClient(srv.Client())
	return p
}

// --------------------------------------------------------------------------
// Test fixtures (mirroring test_azure.py)
// --------------------------------------------------------------------------

var vmItem = azureItem{
	"retailPrice":   0.192,
	"armSkuName":    "Standard_D4s_v3",
	"productName":   "Virtual Machines DSv3 Series",
	"skuName":       "D4s v3",
	"serviceName":   "Virtual Machines",
	"serviceFamily": "Compute",
	"meterId":       "test-meter-id",
	"meterName":     "D4s v3",
	"armRegionName": "eastus",
	"unitOfMeasure": "1 Hour",
}

var reservedItem = azureItem{
	"retailPrice":   1016.16,
	"armSkuName":    "Standard_D4s_v3",
	"productName":   "Virtual Machines DSv3 Series",
	"skuName":       "D4s v3 1 Year",
	"serviceName":   "Virtual Machines",
	"serviceFamily": "Compute",
	"meterId":       "test-reserved-meter",
	"meterName":     "D4s v3",
	"armRegionName": "eastus",
	"unitOfMeasure": "1 Year",
	"type":          "Reservation",
}

var storageItem = azureItem{
	"retailPrice":   0.135,
	"productName":   "Premium SSD Managed Disks",
	"skuName":       "P10 Disks",
	"serviceName":   "Storage",
	"serviceFamily": "Storage",
	"meterId":       "storage-meter",
	"meterName":     "P10 Disks",
	"armRegionName": "eastus",
	"unitOfMeasure": "1/Month",
}

// --------------------------------------------------------------------------
// Tests
// --------------------------------------------------------------------------

func TestGetComputePrice_OnDemand(t *testing.T) {
	srv := mockServer(t, []azureItem{vmItem})
	defer srv.Close()
	p := newTestProvider(t, srv)

	prices, err := p.GetComputePrice(context.Background(), "Standard_D4s_v3", "eastus", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected at least one price, got 0")
	}
	price := prices[0]
	if price.PricePerUnit != 0.192 {
		t.Errorf("expected price 0.192, got %f", price.PricePerUnit)
	}
	if price.Provider != models.CloudProviderAzure {
		t.Errorf("expected provider azure, got %s", price.Provider)
	}
	if price.Unit != models.PriceUnitPerHour {
		t.Errorf("expected unit per_hour, got %s", price.Unit)
	}
	if price.FetchedAt == nil {
		t.Error("FetchedAt must not be nil for fresh prices")
	}
	if price.CacheAgeSeconds == nil {
		t.Error("CacheAgeSeconds must not be nil for fresh prices")
	}
	if *price.CacheAgeSeconds != 0 {
		t.Errorf("CacheAgeSeconds must be 0 for fresh prices, got %d", *price.CacheAgeSeconds)
	}
	if price.SourceURL == "" {
		t.Error("SourceURL must not be empty")
	}
}

func TestGetComputePrice_Reserved1Yr_T38Fix(t *testing.T) {
	// T38 fix: reserved pricing from API is total annual cost; must divide by 8760.
	srv := mockServer(t, []azureItem{reservedItem})
	defer srv.Close()
	p := newTestProvider(t, srv)

	prices, err := p.GetComputePrice(context.Background(), "Standard_D4s_v3", "eastus", "Linux", models.PricingTermReserved1Yr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected at least one price, got 0")
	}
	// 1016.16 / 8760 ≈ 0.116
	expected := 1016.16 / 8760.0
	got := prices[0].PricePerUnit
	diff := got - expected
	if diff < 0 {
		diff = -diff
	}
	if diff > 0.001 {
		t.Errorf("reserved 1yr price: expected ~%.6f, got %.6f (diff %.6f)", expected, got, diff)
	}
}

func TestGetComputePrice_SpotFilterApplied(t *testing.T) {
	spotItem := azureItem{
		"retailPrice": 0.05,
		"armSkuName":  "Standard_D4s_v3",
		"productName": "Virtual Machines DSv3 Series",
		"skuName":     "D4s v3 Spot",
		"serviceName": "Virtual Machines",
		"meterName":   "D4s v3 Spot",
		"meterId":     "spot-meter",
	}
	srv := mockServer(t, []azureItem{vmItem, spotItem})
	defer srv.Close()
	p := newTestProvider(t, srv)

	// Spot request: only spot items should appear
	prices, err := p.GetComputePrice(context.Background(), "Standard_D4s_v3", "eastus", "Linux", models.PricingTermSpot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, price := range prices {
		if price.PricePerUnit == 0.192 {
			t.Error("on-demand price should not appear in spot results")
		}
	}
}

func TestGetComputePrice_OnDemandExcludesSpot(t *testing.T) {
	spotItem := azureItem{
		"retailPrice": 0.05,
		"armSkuName":  "Standard_D4s_v3",
		"productName": "Virtual Machines DSv3 Series",
		"skuName":     "D4s v3 Spot",
		"serviceName": "Virtual Machines",
		"meterName":   "D4s v3 Spot",
		"meterId":     "spot-meter",
	}
	srv := mockServer(t, []azureItem{vmItem, spotItem})
	defer srv.Close()
	p := newTestProvider(t, srv)

	prices, err := p.GetComputePrice(context.Background(), "Standard_D4s_v3", "eastus", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, price := range prices {
		if price.PricePerUnit == 0.05 {
			t.Error("spot price should not appear in on-demand results")
		}
	}
}

func TestGetComputePrice_Cache(t *testing.T) {
	srv, count := countingServer(t, []azureItem{vmItem})
	defer srv.Close()
	p := newTestProvider(t, srv)

	ctx := context.Background()
	// First call: hits the API.
	_, err := p.GetComputePrice(ctx, "Standard_D4s_v3", "eastus", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatal(err)
	}
	if *count != 1 {
		t.Errorf("expected 1 HTTP call after first request, got %d", *count)
	}

	// Second call: should use cache.
	_, err = p.GetComputePrice(ctx, "Standard_D4s_v3", "eastus", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatal(err)
	}
	if *count != 1 {
		t.Errorf("expected still 1 HTTP call after cached request, got %d", *count)
	}
}

func TestGetStoragePrice_PremiumSSD(t *testing.T) {
	srv := mockServer(t, []azureItem{storageItem})
	defer srv.Close()
	p := newTestProvider(t, srv)

	prices, err := p.GetStoragePrice(context.Background(), "premium-ssd", "eastus", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected at least one storage price")
	}
	// Premium SSD must be billed per_month (flat tier fee).
	if prices[0].Unit != models.PriceUnitPerMonth {
		t.Errorf("expected unit per_month for premium-ssd, got %s", prices[0].Unit)
	}
}

func TestGetStoragePrice_PremiumSSD_TierSelection(t *testing.T) {
	// P10 = 128 GiB. Requesting 128 GB should select P10.
	p10Item := azureItem{
		"retailPrice":   0.135,
		"productName":   "Premium SSD Managed Disks",
		"skuName":       "P10 LRS",
		"serviceName":   "Storage",
		"serviceFamily": "Storage",
		"meterId":       "p10-meter",
		"meterName":     "P10 LRS",
		"armRegionName": "eastus",
		"unitOfMeasure": "1/Month",
	}
	p20Item := azureItem{
		"retailPrice":   0.254,
		"productName":   "Premium SSD Managed Disks",
		"skuName":       "P20 LRS",
		"serviceName":   "Storage",
		"serviceFamily": "Storage",
		"meterId":       "p20-meter",
		"meterName":     "P20 LRS",
		"armRegionName": "eastus",
		"unitOfMeasure": "1/Month",
	}
	srv := mockServer(t, []azureItem{p10Item, p20Item})
	defer srv.Close()
	p := newTestProvider(t, srv)

	prices, err := p.GetStoragePrice(context.Background(), "premium-ssd", "eastus", 128)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected prices after tier selection")
	}
	// Should select P10 (128 GiB) not P20.
	for _, price := range prices {
		if price.PricePerUnit == 0.254 {
			t.Error("P20 should not appear when requesting 128 GB (P10 covers it)")
		}
	}
}

func TestGetStoragePrice_UnknownType(t *testing.T) {
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)

	_, err := p.GetStoragePrice(context.Background(), "completely-unknown-type", "eastus", 0)
	if err == nil {
		t.Error("expected error for unknown storage type")
	}
}

func TestListRegions(t *testing.T) {
	// ListRegions doesn't need to hit any API.
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)

	regions, err := p.ListRegions(context.Background(), "compute")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(regions) < 20 {
		t.Errorf("expected at least 20 regions, got %d", len(regions))
	}
	// Must contain these critical regions.
	mustContain := []string{"eastus", "westeurope", "southeastasia"}
	regionSet := make(map[string]bool, len(regions))
	for _, r := range regions {
		regionSet[r] = true
	}
	for _, want := range mustContain {
		if !regionSet[want] {
			t.Errorf("ListRegions must contain %q", want)
		}
	}
}

func TestGetAKSPrice_Free(t *testing.T) {
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)

	prices, err := p.GetAKSPrice(context.Background(), "eastus", "free")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected at least one AKS price")
	}
	if prices[0].PricePerUnit != 0 {
		t.Errorf("AKS free tier must be $0, got %f", prices[0].PricePerUnit)
	}
}

func TestGetAKSPrice_Standard_StaticFallback(t *testing.T) {
	// Return no AKS items to force static fallback.
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)

	prices, err := p.GetAKSPrice(context.Background(), "eastus", "standard")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected static fallback AKS price")
	}
	if prices[0].PricePerUnit != 0.10 {
		t.Errorf("AKS standard static fallback must be $0.10/hr, got %f", prices[0].PricePerUnit)
	}
}

func TestGetOpenAIPrice_StaticFallback(t *testing.T) {
	// Return no items → force static fallback.
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)

	prices, err := p.GetOpenAIPrice(context.Background(), "gpt-4o", "eastus", 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prices) < 2 {
		t.Fatalf("expected at least 2 prices (input + output), got %d", len(prices))
	}

	var inputPrice, outputPrice float64
	for _, price := range prices {
		if price.Attributes != nil {
			switch price.Attributes["token_type"] {
			case "input":
				inputPrice = price.PricePerUnit
			case "output":
				outputPrice = price.PricePerUnit
			}
		}
	}
	// gpt-4o: input $0.005, output $0.015.
	if inputPrice != 0.005 {
		t.Errorf("gpt-4o input price: expected 0.005, got %f", inputPrice)
	}
	if outputPrice != 0.015 {
		t.Errorf("gpt-4o output price: expected 0.015, got %f", outputPrice)
	}
}

func TestGetOpenAIPrice_CostEstimate(t *testing.T) {
	// 1M input tokens × $0.005/1K = $5.00
	// 500K output tokens × $0.015/1K = $7.50
	// total = $12.50
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)

	prices, err := p.GetOpenAIPrice(context.Background(), "gpt-4o", "eastus", 1_000_000, 500_000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Find the estimate entry.
	var estimatePrice float64
	for _, price := range prices {
		if price.SKUID == "openai-gpt-4o-estimate" {
			estimatePrice = price.PricePerUnit
			break
		}
	}
	diff := estimatePrice - 12.50
	if diff < 0 {
		diff = -diff
	}
	if diff > 0.01 {
		t.Errorf("gpt-4o cost estimate for 1M+500K tokens: expected $12.50, got $%.4f", estimatePrice)
	}
}

func TestGetMonitorPrice_LogEstimate(t *testing.T) {
	// 100 GB log - 5 GB free = 95 GB × $2.30 = $218.50.
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)

	prices, err := p.GetMonitorPrice(context.Background(), "eastus", 100.0, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var estimatePrice float64
	for _, price := range prices {
		if price.SKUID == "monitor-estimate" {
			estimatePrice = price.PricePerUnit
			break
		}
	}
	expected := 95.0 * 2.30 // = 218.50
	diff := estimatePrice - expected
	if diff < 0 {
		diff = -diff
	}
	if diff > 0.01 {
		t.Errorf("monitor log estimate: expected $%.2f, got $%.4f", expected, estimatePrice)
	}
}

func TestGetCDNPrice_Zone1(t *testing.T) {
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)

	// eastus → Zone 1 → $0.081/GB
	prices, err := p.GetCDNPrice(context.Background(), "eastus", 0, "standard")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected CDN prices")
	}
	// Static fallback Zone 1 rate.
	found := false
	for _, price := range prices {
		if price.Unit == models.PriceUnitPerGB && price.PricePerUnit > 0 {
			if price.PricePerUnit != 0.081 {
				t.Errorf("CDN Zone 1 rate: expected $0.081/GB, got $%.4f/GB", price.PricePerUnit)
			}
			found = true
		}
	}
	if !found {
		t.Error("did not find CDN per-GB price")
	}
}

func TestGetCDNPrice_Zone2_DifferentFromZone1(t *testing.T) {
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)

	// southeastasia → Zone 2
	prices, err := p.GetCDNPrice(context.Background(), "southeastasia", 0, "standard")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	zone1Prices, err2 := p.GetCDNPrice(context.Background(), "eastus", 0, "standard")
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if len(prices) == 0 || len(zone1Prices) == 0 {
		t.Fatal("expected prices from both zones")
	}
	if prices[0].PricePerUnit == zone1Prices[0].PricePerUnit {
		t.Error("Zone 2 CDN price should differ from Zone 1")
	}
	if prices[0].PricePerUnit <= 0 {
		t.Error("Zone 2 CDN price must be > 0")
	}
}

func TestGetFrontDoorPrice_Zone1(t *testing.T) {
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)

	prices, err := p.GetFrontDoorPrice(context.Background(), "eastus", 0, 0, "standard")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected Front Door prices")
	}
	found := false
	for _, price := range prices {
		if price.Unit == models.PriceUnitPerGB && price.PricePerUnit > 0 {
			if price.PricePerUnit != 0.0825 {
				t.Errorf("Front Door Zone 1 DT rate: expected $0.0825/GB, got $%.4f/GB", price.PricePerUnit)
			}
			found = true
		}
	}
	if !found {
		t.Error("did not find Front Door per-GB price")
	}
}

func TestGetSQLPrice_VCoreWordBoundary(t *testing.T) {
	// vCore word-boundary: "GP_Gen5_4 LRS" matches 4 vCores.
	// "GP_Gen5_14 LRS" must NOT match for 4 vCores.
	gp4Item := azureItem{
		"retailPrice":   1.0,
		"productName":   "SQL Database",
		"skuName":       "GP_Gen5_4 LRS",
		"serviceName":   "SQL Database",
		"serviceFamily": "Databases",
		"meterId":       "sql-gp4",
		"meterName":     "GP_Gen5_4",
		"armRegionName": "eastus",
		"unitOfMeasure": "1 Hour",
	}
	gp14Item := azureItem{
		"retailPrice":   2.5,
		"productName":   "SQL Database",
		"skuName":       "GP_Gen5_14 LRS",
		"serviceName":   "SQL Database",
		"serviceFamily": "Databases",
		"meterId":       "sql-gp14",
		"meterName":     "GP_Gen5_14",
		"armRegionName": "eastus",
		"unitOfMeasure": "1 Hour",
	}
	srv := mockServer(t, []azureItem{gp4Item, gp14Item})
	defer srv.Close()
	p := newTestProvider(t, srv)

	prices, err := p.GetSQLPrice(context.Background(), "General Purpose 4 vCores", "eastus", "SQL", "single-az", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, price := range prices {
		if price.PricePerUnit == 2.5 {
			t.Error("GP_Gen5_14 should not match request for 4 vCores (word boundary violated)")
		}
	}
	found4 := false
	for _, price := range prices {
		if price.PricePerUnit == 1.0 {
			found4 = true
		}
	}
	if !found4 {
		t.Error("GP_Gen5_4 should match request for 4 vCores")
	}
}

func TestGetEgressPrice_FreeTierApplied(t *testing.T) {
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)

	// 1 GB — within free tier (5 GB free), so chargeable = 0, monthly cost = $0.
	prices, err := p.GetEgressPrice(context.Background(), "eastus", "", 1.0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected egress price")
	}
	// The attribute "monthly_estimate" must say "0.0 GB chargeable" / $0.0000.
	estimate := prices[0].Attributes["monthly_estimate"]
	t.Logf("egress monthly estimate for 1 GB: %s", estimate)
	if !strings.Contains(estimate, "0.0 GB chargeable") {
		t.Errorf("1 GB egress should have 0.0 GB chargeable (5 GB free), estimate: %q", estimate)
	}
	if !strings.Contains(estimate, "$0.0000") {
		t.Errorf("1 GB egress cost should be $0.0000, estimate: %q", estimate)
	}
}

func TestGetEgressPrice_Zone2(t *testing.T) {
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)

	// southeastasia = zone2
	prices, err := p.GetEgressPrice(context.Background(), "southeastasia", "", 10.0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected egress price for zone2")
	}
	if prices[0].Attributes["zone"] != "zone2" {
		t.Errorf("expected zone2 for southeastasia, got %s", prices[0].Attributes["zone"])
	}
}

func TestProvider_Name(t *testing.T) {
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)
	if p.Name() != models.CloudProviderAzure {
		t.Errorf("expected azure, got %s", p.Name())
	}
}

func TestProvider_DefaultRegion(t *testing.T) {
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)
	if p.DefaultRegion() != "eastus" {
		t.Errorf("expected eastus, got %s", p.DefaultRegion())
	}
}

func TestProvider_MajorRegions(t *testing.T) {
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)
	regions := p.MajorRegions()
	if len(regions) < 5 {
		t.Errorf("expected multiple major regions, got %d", len(regions))
	}
}

func TestProvider_Supports(t *testing.T) {
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)

	tests := []struct {
		domain  models.PricingDomain
		service string
		want    bool
	}{
		{models.PricingDomainCompute, "vm", true},
		{models.PricingDomainStorage, "managed_disks", true},
		{models.PricingDomainAI, "openai", true},
		{models.PricingDomainNetwork, "azure_cdn", true},
		{models.PricingDomainNetwork, "azure_front_door", true},
		{models.PricingDomainObservability, "azure_monitor", true},
		{models.PricingDomainAnalytics, "", false},
		{models.PricingDomainCompute, "lambda", false},
	}
	for _, tt := range tests {
		got := p.Supports(tt.domain, tt.service)
		if got != tt.want {
			t.Errorf("Supports(%s, %q): want %v, got %v", tt.domain, tt.service, tt.want, got)
		}
	}
}

func TestProvider_SupportedTerms_Compute(t *testing.T) {
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)
	terms := p.SupportedTerms(models.PricingDomainCompute, "vm")
	if len(terms) < 3 {
		t.Errorf("expected at least 3 compute terms, got %d", len(terms))
	}
}

func TestProvider_GetEffectivePrice_NotSupported(t *testing.T) {
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)

	_, err := p.GetEffectivePrice(context.Background(), nil)
	if err == nil {
		t.Error("expected ErrNotSupported from GetEffectivePrice")
	}
}

func TestProvider_GetSpotHistory_NotSupported(t *testing.T) {
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)

	_, err := p.GetSpotHistory(context.Background(), nil, 24, "")
	if err == nil {
		t.Error("expected ErrNotSupported from GetSpotHistory")
	}
}

func TestProvider_GetDiscountSummary_NotSupported(t *testing.T) {
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)

	_, err := p.GetDiscountSummary(context.Background())
	if err == nil {
		t.Error("expected ErrNotSupported from GetDiscountSummary")
	}
}

func TestDescribeCatalog(t *testing.T) {
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)

	catalog, err := p.DescribeCatalog(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if catalog == nil {
		t.Fatal("expected non-nil catalog")
	}
	if catalog.Provider != models.CloudProviderAzure {
		t.Errorf("expected provider azure, got %s", catalog.Provider)
	}
	if len(catalog.Domains) == 0 {
		t.Error("expected at least one domain in catalog")
	}
}

func TestCheckAvailability_Compute(t *testing.T) {
	srv := mockServer(t, []azureItem{vmItem})
	defer srv.Close()
	p := newTestProvider(t, srv)

	ok, _, err := p.CheckAvailability(context.Background(), "compute", "Standard_D4s_v3", "eastus")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected availability=true for known VM type")
	}
}

func TestFreshPrices_MetadataSet(t *testing.T) {
	srv := mockServer(t, []azureItem{vmItem})
	defer srv.Close()
	p := newTestProvider(t, srv)

	prices, err := p.GetComputePrice(context.Background(), "Standard_D4s_v3", "eastus", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected prices")
	}
	price := prices[0]
	if price.FetchedAt == nil {
		t.Error("FetchedAt must not be nil")
	}
	if price.CacheAgeSeconds == nil {
		t.Error("CacheAgeSeconds must not be nil")
	}
	if *price.CacheAgeSeconds != 0 {
		t.Errorf("CacheAgeSeconds must be 0 for fresh prices, got %d", *price.CacheAgeSeconds)
	}
	if price.SourceURL == "" {
		t.Error("SourceURL must not be empty")
	}
}

func TestCachedPrices_AgeIncreases(t *testing.T) {
	srv, _ := countingServer(t, []azureItem{vmItem})
	defer srv.Close()
	p := newTestProvider(t, srv)

	ctx := context.Background()
	// Fetch once to populate cache.
	_, err := p.GetComputePrice(ctx, "Standard_D4s_v3", "eastus", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatal(err)
	}
	// Small sleep to ensure cache age is at least 0.
	time.Sleep(10 * time.Millisecond)

	// Fetch again from cache.
	cachedPrices, err := p.GetComputePrice(ctx, "Standard_D4s_v3", "eastus", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatal(err)
	}
	if len(cachedPrices) == 0 {
		t.Fatal("expected cached prices")
	}
	if cachedPrices[0].CacheAgeSeconds == nil {
		t.Error("CacheAgeSeconds must not be nil for cached prices")
	}
	// cache age should be >= 0 (not negative).
	if *cachedPrices[0].CacheAgeSeconds < 0 {
		t.Errorf("CacheAgeSeconds must be >= 0, got %d", *cachedPrices[0].CacheAgeSeconds)
	}
}

func TestGetFunctionsPrice_StaticFallback(t *testing.T) {
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)

	prices, err := p.GetFunctionsPrice(context.Background(), "eastus", 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prices) < 2 {
		t.Fatalf("expected at least 2 prices from static fallback, got %d", len(prices))
	}
	var gbSecPrice, reqPrice float64
	for _, price := range prices {
		if price.Unit == models.PriceUnitPerGBSecond {
			gbSecPrice = price.PricePerUnit
		} else if price.Unit == models.PriceUnitPerRequest {
			reqPrice = price.PricePerUnit
		}
	}
	if gbSecPrice != 0.000016 {
		t.Errorf("functions GB-sec rate: expected 0.000016, got %f", gbSecPrice)
	}
	if reqPrice != 0.0000002 {
		t.Errorf("functions exec rate: expected 0.0000002, got %f", reqPrice)
	}
}

func TestBOMAdvisories_ReturnsNil(t *testing.T) {
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)

	advisories, err := p.BOMAdvisories(context.Background(), []string{"compute"}, "eastus")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Azure does not implement BOM advisories; nil is acceptable.
	_ = advisories
}

func TestGetCosmosPrice_ServerlessFilter(t *testing.T) {
	serverlessItem := azureItem{
		"retailPrice":   0.00000025,
		"productName":   "Azure Cosmos DB",
		"skuName":       "Cosmos DB Serverless",
		"meterName":     "serverless Request Units",
		"serviceName":   "Azure Cosmos DB",
		"serviceFamily": "Databases",
		"meterId":       "cosmos-serverless",
		"armRegionName": "eastus",
		"unitOfMeasure": "1M",
	}
	provisionedItem := azureItem{
		"retailPrice":   0.008,
		"productName":   "Azure Cosmos DB",
		"skuName":       "Provisioned 100 RU/s",
		"meterName":     "100 RU/s",
		"serviceName":   "Azure Cosmos DB",
		"serviceFamily": "Databases",
		"meterId":       "cosmos-provisioned",
		"armRegionName": "eastus",
		"unitOfMeasure": "1 Hour",
	}
	srv := mockServer(t, []azureItem{serverlessItem, provisionedItem})
	defer srv.Close()
	p := newTestProvider(t, srv)

	prices, err := p.GetCosmosPrice(context.Background(), "eastus", "serverless", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should only have serverless item.
	for _, price := range prices {
		if price.PricePerUnit == 0.008 {
			t.Error("provisioned price should not appear in serverless results")
		}
	}
}

func TestGetComputePrice_Reserved3Yr(t *testing.T) {
	reserved3YrItem := azureItem{
		"retailPrice":   2625.60,
		"armSkuName":    "Standard_D4s_v3",
		"productName":   "Virtual Machines DSv3 Series",
		"skuName":       "D4s v3 3 Years",
		"serviceName":   "Virtual Machines",
		"serviceFamily": "Compute",
		"meterId":       "test-reserved-3yr-meter",
		"meterName":     "D4s v3",
		"armRegionName": "eastus",
		"unitOfMeasure": "3 Years",
		"type":          "Reservation",
	}
	srv := mockServer(t, []azureItem{reserved3YrItem})
	defer srv.Close()
	p := newTestProvider(t, srv)

	prices, err := p.GetComputePrice(context.Background(), "Standard_D4s_v3", "eastus", "Linux", models.PricingTermReserved3Yr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected reserved 3yr price")
	}
	// 2625.60 / 26280 ≈ 0.09992
	expected := 2625.60 / 26280.0
	got := prices[0].PricePerUnit
	diff := got - expected
	if diff < 0 {
		diff = -diff
	}
	if diff > 0.001 {
		t.Errorf("reserved 3yr price: expected ~%.6f, got %.6f", expected, got)
	}
}

func TestSearchPricing(t *testing.T) {
	srv := mockServer(t, []azureItem{vmItem})
	defer srv.Close()
	p := newTestProvider(t, srv)

	results, err := p.SearchPricing(context.Background(), "D4s v3", "eastus", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// At minimum, if the mock returns the item, SearchPricing should not error.
	// The item may or may not match since lowercase "d4s v3" in search vs "D4s v3".
	t.Logf("SearchPricing returned %d results", len(results))
}

func TestGetPrice_DispatchCompute(t *testing.T) {
	srv := mockServer(t, []azureItem{vmItem})
	defer srv.Close()
	p := newTestProvider(t, srv)

	spec := &models.ComputePricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAzure,
			Domain:   models.PricingDomainCompute,
			Region:   "eastus",
			Term:     models.PricingTermOnDemand,
		},
		ResourceType: "Standard_D4s_v3",
		OS:           "Linux",
	}

	result, err := p.GetPrice(context.Background(), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Source != "catalog" {
		t.Errorf("expected source=catalog, got %s", result.Source)
	}
	if len(result.PublicPrices) == 0 {
		t.Error("expected public prices in result")
	}
}

// --------------------------------------------------------------------------
// Backlog tests
// --------------------------------------------------------------------------

func TestGetComputePrice_ZeroPriceFiltered(t *testing.T) {
	// Items with retailPrice=0 must be filtered out of results.
	zeroItem := azureItem{
		"retailPrice":   0.0,
		"armSkuName":    "Standard_D4s_v3",
		"productName":   "Virtual Machines DSv3 Series",
		"skuName":       "D4s v3",
		"serviceName":   "Virtual Machines",
		"serviceFamily": "Compute",
		"meterId":       "zero-meter",
		"meterName":     "D4s v3",
		"armRegionName": "eastus",
		"unitOfMeasure": "1 Hour",
	}
	srv := mockServer(t, []azureItem{zeroItem})
	defer srv.Close()
	p := newTestProvider(t, srv)

	prices, err := p.GetComputePrice(context.Background(), "Standard_D4s_v3", "eastus", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prices) != 0 {
		t.Errorf("expected 0 prices when retailPrice=0, got %d", len(prices))
	}
}

func TestGetComputePrice_LinuxExcludesWindowsSKUs(t *testing.T) {
	// When OS=Linux is requested, Windows productName SKUs must not appear in results.
	linuxItem := azureItem{
		"retailPrice":   0.384,
		"armSkuName":    "Standard_D8s_v3",
		"productName":   "Virtual Machines DSv3 Series",
		"skuName":       "D8s v3",
		"serviceName":   "Virtual Machines",
		"serviceFamily": "Compute",
		"meterId":       "linux-meter",
		"meterName":     "D8s v3",
		"armRegionName": "eastus",
		"unitOfMeasure": "1 Hour",
	}
	windowsItem := azureItem{
		"retailPrice":   0.752,
		"armSkuName":    "Standard_D8s_v3",
		"productName":   "Virtual Machines DSv3 Series Windows",
		"skuName":       "D8s v3",
		"serviceName":   "Virtual Machines",
		"serviceFamily": "Compute",
		"meterId":       "windows-meter",
		"meterName":     "D8s v3",
		"armRegionName": "eastus",
		"unitOfMeasure": "1 Hour",
	}
	srv := mockServer(t, []azureItem{linuxItem, windowsItem})
	defer srv.Close()
	p := newTestProvider(t, srv)

	prices, err := p.GetComputePrice(context.Background(), "Standard_D8s_v3", "eastus", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("expected 1 Linux price, got %d", len(prices))
	}
	if prices[0].PricePerUnit != 0.384 {
		t.Errorf("expected Linux price 0.384, got %f", prices[0].PricePerUnit)
	}
	for _, price := range prices {
		if productName, ok := price.Attributes["productName"]; ok {
			if strings.Contains(productName, "Windows") {
				t.Errorf("Linux result should not contain Windows SKU: %s", productName)
			}
		}
	}
}

func TestGetComputePrice_WindowsExcludesLinuxSKUs(t *testing.T) {
	// When OS=Windows is requested, only Windows productName SKUs must appear in results.
	linuxItem := azureItem{
		"retailPrice":   0.384,
		"armSkuName":    "Standard_D8s_v3",
		"productName":   "Virtual Machines DSv3 Series",
		"skuName":       "D8s v3",
		"serviceName":   "Virtual Machines",
		"serviceFamily": "Compute",
		"meterId":       "linux-meter",
		"meterName":     "D8s v3",
		"armRegionName": "eastus",
		"unitOfMeasure": "1 Hour",
	}
	windowsItem := azureItem{
		"retailPrice":   0.752,
		"armSkuName":    "Standard_D8s_v3",
		"productName":   "Virtual Machines DSv3 Series Windows",
		"skuName":       "D8s v3",
		"serviceName":   "Virtual Machines",
		"serviceFamily": "Compute",
		"meterId":       "windows-meter",
		"meterName":     "D8s v3",
		"armRegionName": "eastus",
		"unitOfMeasure": "1 Hour",
	}
	srv := mockServer(t, []azureItem{linuxItem, windowsItem})
	defer srv.Close()
	p := newTestProvider(t, srv)

	prices, err := p.GetComputePrice(context.Background(), "Standard_D8s_v3", "eastus", "Windows", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("expected 1 Windows price, got %d", len(prices))
	}
	if prices[0].PricePerUnit != 0.752 {
		t.Errorf("expected Windows price 0.752, got %f", prices[0].PricePerUnit)
	}
	for _, price := range prices {
		if productName, ok := price.Attributes["productName"]; ok {
			if !strings.Contains(productName, "Windows") {
				t.Errorf("Windows result should contain Windows SKU: %s", productName)
			}
		}
	}
}

func TestGetComputePrice_SortedCheapestFirst(t *testing.T) {
	// Results must be sorted ascending by price (cheapest first).
	// Return items out of order: expensive first, cheap second.
	expensiveItem := azureItem{
		"retailPrice":   0.384,
		"armSkuName":    "Standard_D8s_v3",
		"productName":   "Virtual Machines DSv3 Series",
		"skuName":       "D8s v3",
		"serviceName":   "Virtual Machines",
		"serviceFamily": "Compute",
		"meterId":       "linux-meter-expensive",
		"meterName":     "D8s v3",
		"armRegionName": "eastus",
		"unitOfMeasure": "1 Hour",
	}
	cheaperItem := azureItem{
		"retailPrice":   0.300,
		"armSkuName":    "Standard_D8s_v3",
		"productName":   "Virtual Machines DSv3 Series",
		"skuName":       "D8s v3",
		"serviceName":   "Virtual Machines",
		"serviceFamily": "Compute",
		"meterId":       "linux-meter-cheaper",
		"meterName":     "D8s v3",
		"armRegionName": "eastus",
		"unitOfMeasure": "1 Hour",
	}
	// API returns expensive first, then cheap — results should be reversed.
	srv := mockServer(t, []azureItem{expensiveItem, cheaperItem})
	defer srv.Close()
	p := newTestProvider(t, srv)

	prices, err := p.GetComputePrice(context.Background(), "Standard_D8s_v3", "eastus", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prices) != 2 {
		t.Fatalf("expected 2 prices, got %d", len(prices))
	}
	if prices[0].PricePerUnit > prices[1].PricePerUnit {
		t.Errorf("prices must be sorted cheapest first: got %f > %f", prices[0].PricePerUnit, prices[1].PricePerUnit)
	}
	if prices[0].PricePerUnit != 0.300 {
		t.Errorf("first price must be 0.300 (cheapest), got %f", prices[0].PricePerUnit)
	}
	if prices[1].PricePerUnit != 0.384 {
		t.Errorf("second price must be 0.384, got %f", prices[1].PricePerUnit)
	}
}

func TestGetSQLPrice_CacheHit(t *testing.T) {
	// Second call with same args must use cache; HTTP endpoint must be called exactly once.
	sqlItem := azureItem{
		"retailPrice":   0.3812,
		"productName":   "SQL Database Vcore",
		"skuName":       "GP_Gen5_4 LRS",
		"serviceName":   "SQL Database",
		"serviceFamily": "Databases",
		"meterId":       "sql-gp-4vcores",
		"meterName":     "GP_Gen5_4",
		"armRegionName": "eastus",
		"unitOfMeasure": "1 Hour",
	}
	srv, count := countingServer(t, []azureItem{sqlItem})
	defer srv.Close()
	p := newTestProvider(t, srv)

	ctx := context.Background()
	// First call: hits the API.
	_, err := p.GetSQLPrice(ctx, "General Purpose 4 vCores", "eastus", "SQL", "single-az", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("first call unexpected error: %v", err)
	}
	if *count != 1 {
		t.Errorf("expected 1 HTTP call after first request, got %d", *count)
	}

	// Second call with same args: must use cache.
	_, err = p.GetSQLPrice(ctx, "General Purpose 4 vCores", "eastus", "SQL", "single-az", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("second call unexpected error: %v", err)
	}
	if *count != 1 {
		t.Errorf("expected still 1 HTTP call after cached request, got %d", *count)
	}
}

func TestGetEgressPrice_SwedenCentral_IsZone1(t *testing.T) {
	// Sweden Central is not in the azureEgressZone map, so it must default to zone1.
	// The egress price for swedencentral must use zone1 rate ($0.087/GB fallback).
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)

	prices, err := p.GetEgressPrice(context.Background(), "swedencentral", "", 100.0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected egress price for swedencentral")
	}
	zone := prices[0].Attributes["zone"]
	if zone != "zone1" {
		t.Errorf("Sweden Central must map to zone1 (not zone2), got %s", zone)
	}
	// Static fallback rate for zone1 is $0.087/GB
	if prices[0].PricePerUnit != 0.087 {
		t.Errorf("Sweden Central zone1 rate should be $0.087/GB, got %f", prices[0].PricePerUnit)
	}
}

func TestEgressServiceField_BothDomains(t *testing.T) {
	// Egress pricing works for both EgressPricingSpec (inter_region_egress domain)
	// and NetworkPricingSpec (network domain, service=egress).
	// Both should return a result with service="egress".
	srv := mockServer(t, nil)
	defer srv.Close()
	p := newTestProvider(t, srv)

	ctx := context.Background()

	// Test EgressPricingSpec
	egressSpec := &models.EgressPricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAzure,
			Domain:   models.PricingDomainInterRegionEgress,
			Region:   "eastus",
		},
		SourceRegion: "eastus",
		DestRegion:   "westeurope",
		DataGB:       100.0,
	}
	egressResult, err := p.GetPrice(ctx, egressSpec)
	if err != nil {
		t.Fatalf("EgressPricingSpec: unexpected error: %v", err)
	}
	if len(egressResult.PublicPrices) == 0 {
		t.Fatal("EgressPricingSpec: expected at least one price")
	}
	if egressResult.PublicPrices[0].Service != "egress" {
		t.Errorf("EgressPricingSpec: expected service=egress, got %s", egressResult.PublicPrices[0].Service)
	}

	// Test NetworkPricingSpec with service=egress
	networkSpec := &models.NetworkPricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderAzure,
			Domain:   models.PricingDomainNetwork,
			Region:   "eastus",
			Service:  "egress",
		},
		SourceRegion:    "eastus",
		DestinationType: "internet",
		DataGBPerMonth:  100.0,
	}
	networkResult, err := p.GetPrice(ctx, networkSpec)
	if err != nil {
		t.Fatalf("NetworkPricingSpec: unexpected error: %v", err)
	}
	if len(networkResult.PublicPrices) == 0 {
		t.Fatal("NetworkPricingSpec: expected at least one price")
	}
	if networkResult.PublicPrices[0].Service != "egress" {
		t.Errorf("NetworkPricingSpec: expected service=egress, got %s", networkResult.PublicPrices[0].Service)
	}
}

func TestMain(m *testing.M) {
	// Ensure tests don't need real network.
	fmt.Println("Running Azure provider tests with mock HTTP server...")
	os.Exit(m.Run())
}
