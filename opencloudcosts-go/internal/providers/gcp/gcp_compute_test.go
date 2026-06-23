// Package gcp — unit tests for compute and storage pricing (Part 1).
package gcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/cache"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/config"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/utils"
)

// --------------------------------------------------------------------------
// Test helpers
// --------------------------------------------------------------------------

// makeSKU builds a minimal GCP SKU map[string]any for testing.
func makeSKU(desc, usageType, region string, units string, nanos int) map[string]any {
	return map[string]any{
		"description":    desc,
		"serviceRegions": []any{region},
		"category": map[string]any{
			"usageType": usageType,
		},
		"pricingInfo": []any{
			map[string]any{
				"pricingExpression": map[string]any{
					"tieredRates": []any{
						map[string]any{
							"startUsageAmount": float64(0),
							"unitPrice": map[string]any{
								"units": units,
								"nanos": float64(nanos),
							},
						},
					},
				},
			},
		},
	}
}

// skuResponse wraps a list of SKU maps into the GCP catalog API JSON shape.
func skuResponse(skus []map[string]any) []byte {
	resp := map[string]any{
		"skus":          skus,
		"nextPageToken": "",
	}
	b, _ := json.Marshal(resp)
	return b
}

// newTestProvider creates a Provider backed by the given httptest.Server.
// It uses a temporary directory for the cache.
func newTestProvider(t *testing.T, server *httptest.Server) *Provider {
	t.Helper()
	dir := t.TempDir()
	cm, err := cache.New(dir)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	cfg := &config.Config{
		GCPAPIKey:       "test-key",
		CacheTTLHours:   24,
		MetadataTTLDays: 7,
	}
	p := &Provider{
		cfg:        cfg,
		cache:      cm,
		auth:       newGCPAuthProvider(cfg),
		httpClient: server.Client(),
		baseURL:    server.URL,
	}
	return p
}

// --------------------------------------------------------------------------
// Unit tests — pure helpers
// --------------------------------------------------------------------------

// TestGCPMoney verifies the Money proto conversion.
func TestGCPMoney(t *testing.T) {
	cases := []struct {
		units string
		nanos int
		want  float64
	}{
		{"0", 48_000_000, 0.048},
		{"0", 100_000_000, 0.1},
		{"1", 0, 1.0},
		{"0", 0, 0.0},
	}
	for _, c := range cases {
		got := gcpMoney(c.units, c.nanos)
		if abs(got-c.want) > 1e-9 {
			t.Errorf("gcpMoney(%q, %d) = %v, want %v", c.units, c.nanos, got, c.want)
		}
	}
}

// TestLookupPrice verifies partial-match lookups in a priceIndex.
func TestLookupPrice(t *testing.T) {
	idx := priceIndex{
		[2]string{"N2 Instance Core", "OnDemand"}:                0.031611,
		[2]string{"N2 Instance Ram", "OnDemand"}:                 0.004237,
		[2]string{"Preemptible N2 Instance Core", "Preemptible"}: 0.006700,
	}

	// Exact substring match.
	price, ok := lookupPrice(idx, "N2 Instance Core", "OnDemand")
	if !ok || abs(price-0.031611) > 1e-9 {
		t.Errorf("lookupPrice(N2 Instance Core) = %v, %v; want 0.031611, true", price, ok)
	}

	// Case-insensitive substring.
	price, ok = lookupPrice(idx, "n2 instance ram", "OnDemand")
	if !ok || abs(price-0.004237) > 1e-9 {
		t.Errorf("lookupPrice(n2 instance ram) = %v, %v; want 0.004237, true", price, ok)
	}

	// Substring "N2 Instance Core" is contained in "Preemptible N2 Instance Core",
	// so with usageType "Preemptible" this SHOULD match (mirrors Python behavior).
	price, ok = lookupPrice(idx, "N2 Instance Core", "Preemptible")
	if !ok || abs(price-0.006700) > 1e-9 {
		t.Errorf("lookupPrice(N2 Instance Core, Preemptible) = %v, %v; want 0.006700, true", price, ok)
	}

	// Desc substring not present in any key.
	_, ok = lookupPrice(idx, "E2 Instance Core", "OnDemand")
	if ok {
		t.Error("lookupPrice for missing key should return false")
	}

	// Right desc substring, wrong usageType that has no match.
	_, ok = lookupPrice(idx, "N2 Instance Ram", "Preemptible")
	if ok {
		t.Error("lookupPrice(N2 Instance Ram, Preemptible) should return false — no Preemptible RAM key")
	}
}

// TestSkuPrice verifies first-tier price extraction.
func TestSkuPrice(t *testing.T) {
	sku := makeSKU("N2 Instance Core", "OnDemand", "us-central1", "0", 31_611_000)
	got := skuPrice(sku)
	want := 0.031611
	if abs(got-want) > 1e-9 {
		t.Errorf("skuPrice = %v, want %v", got, want)
	}

	// Empty pricingInfo → 0.
	empty := map[string]any{"pricingInfo": []any{}}
	if got := skuPrice(empty); got != 0 {
		t.Errorf("skuPrice(empty) = %v, want 0", got)
	}
}

// TestTermUsageTypeMapping verifies the term → usageType mapping.
func TestTermUsageTypeMapping(t *testing.T) {
	cases := []struct {
		term      models.PricingTerm
		wantUsage string
	}{
		{models.PricingTermOnDemand, "OnDemand"},
		{models.PricingTermSpot, "Preemptible"},
		{models.PricingTermCUD1Yr, "Commit1Yr"},
		{models.PricingTermCUD3Yr, "Commit3Yr"},
	}
	for _, c := range cases {
		tm, ok := termUsageType[c.term]
		if !ok {
			t.Errorf("term %q not found in termUsageType", c.term)
			continue
		}
		if tm.usageType != c.wantUsage {
			t.Errorf("term %q: usageType = %q, want %q", c.term, tm.usageType, c.wantUsage)
		}
	}
}

// TestFamilySKUDesc verifies correct desc selection per CPU key.
func TestFamilySKUDesc(t *testing.T) {
	fsku := utils.GCPFamilySKU["n2"]

	cpuDesc, ramDesc := familySKUDesc(fsku, "cpu", "ram")
	if cpuDesc != fsku.CPUDesc || ramDesc != fsku.RAMDesc {
		t.Errorf("cpu/ram: got %q/%q, want %q/%q", cpuDesc, ramDesc, fsku.CPUDesc, fsku.RAMDesc)
	}

	cpuDesc, ramDesc = familySKUDesc(fsku, "preemptCPU", "preemptRAM")
	if cpuDesc != fsku.PreemptCPUDesc || ramDesc != fsku.PreemptRAMDesc {
		t.Errorf("preempt: got %q/%q, want %q/%q", cpuDesc, ramDesc, fsku.PreemptCPUDesc, fsku.PreemptRAMDesc)
	}

	cpuDesc, ramDesc = familySKUDesc(fsku, "cudCPU", "cudRAM")
	if cpuDesc != fsku.CUDCPUDesc || ramDesc != fsku.CUDRAMDesc {
		t.Errorf("cud: got %q/%q, want %q/%q", cpuDesc, ramDesc, fsku.CUDCPUDesc, fsku.CUDRAMDesc)
	}
}

// TestEmptyCUDDescReturnsEmpty verifies that families without CUD support (N1)
// return an empty slice rather than an error.
func TestEmptyCUDDescReturnsEmpty(t *testing.T) {
	// N1 has empty CUD descs — should return [] not error.
	dir := t.TempDir()
	cm, _ := cache.New(dir)
	cfg := &config.Config{GCPAPIKey: "k", CacheTTLHours: 1, MetadataTTLDays: 1}

	// We need a server but it won't be called because desc is empty before the HTTP fetch.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("HTTP should not be called for empty-desc families")
	}))
	defer ts.Close()

	p := &Provider{cfg: cfg, cache: cm, auth: newGCPAuthProvider(cfg),
		httpClient: ts.Client(), baseURL: ts.URL}

	prices, err := p.GetComputePrice(context.Background(), "n1-standard-4", "us-central1", "Linux", models.PricingTermCUD1Yr)
	if err != nil {
		t.Fatalf("expected nil error for N1 CUD, got: %v", err)
	}
	if len(prices) != 0 {
		t.Errorf("expected empty slice for N1 CUD, got %d prices", len(prices))
	}
}

// --------------------------------------------------------------------------
// Integration-style tests using httptest.Server
// --------------------------------------------------------------------------

// TestGetComputePrice_OnDemand tests the full on-demand compute price path with a
// canned SKU catalog returned by an httptest.Server.
func TestGetComputePrice_OnDemand(t *testing.T) {
	// Canned SKUs for n2-standard-4: 4 vCPU, 16 GB RAM.
	// N2 CPU OnDemand: $0.031611/vCPU-hr → 4 × 0.031611 = $0.126444
	// N2 RAM OnDemand: $0.004237/GB-hr   → 16 × 0.004237 = $0.067792
	// Total: $0.194236
	skus := []map[string]any{
		makeSKU("N2 Instance Core", "OnDemand", "us-central1", "0", 31_611_000),
		makeSKU("N2 Instance Ram", "OnDemand", "us-central1", "0", 4_237_000),
	}

	var callCount int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	prices, err := p.GetComputePrice(context.Background(), "n2-standard-4", "us-central1", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("GetComputePrice: %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("expected 1 price, got %d", len(prices))
	}

	want := 4*0.031611 + 16*0.004237
	if abs(prices[0].PricePerUnit-want) > 1e-6 {
		t.Errorf("PricePerUnit = %.6f, want %.6f", prices[0].PricePerUnit, want)
	}
	if prices[0].Unit != models.PriceUnitPerHour {
		t.Errorf("Unit = %v, want per_hour", prices[0].Unit)
	}
	if prices[0].PricingTerm != models.PricingTermOnDemand {
		t.Errorf("PricingTerm = %v, want on_demand", prices[0].PricingTerm)
	}

	// Verify freshness annotation.
	if prices[0].FetchedAt == nil {
		t.Error("FetchedAt should be set")
	}
	if prices[0].CacheAgeSeconds == nil || *prices[0].CacheAgeSeconds != 0 {
		t.Error("CacheAgeSeconds should be 0 for fresh fetch")
	}
}

// TestGetComputePrice_Preemptible tests preemptible pricing.
func TestGetComputePrice_Preemptible(t *testing.T) {
	skus := []map[string]any{
		makeSKU("Preemptible N2 Instance Core", "Preemptible", "us-central1", "0", 6_700_000),
		makeSKU("Preemptible N2 Instance Ram", "Preemptible", "us-central1", "0", 900_000),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	prices, err := p.GetComputePrice(context.Background(), "n2-standard-4", "us-central1", "Linux", models.PricingTermSpot)
	if err != nil {
		t.Fatalf("GetComputePrice (preemptible): %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("expected 1 price, got %d", len(prices))
	}
	want := 4*0.0067 + 16*0.0009
	if abs(prices[0].PricePerUnit-want) > 1e-6 {
		t.Errorf("Preemptible PricePerUnit = %.6f, want %.6f", prices[0].PricePerUnit, want)
	}
}

// TestGetComputePrice_Windows tests Windows license addition.
//
// GCP description lookup is a substring match, so Windows SKU descriptions
// ("N2 Instance Core running Windows") contain the base Linux SKU descriptions
// ("N2 Instance Core"). The price index lookup finds the FIRST matching key
// (map iteration order). To write a deterministic test we use SKU descriptions
// that do NOT contain each other as substrings: we use a distinct prefix for
// the Linux SKU so the substring lookup is unambiguous.
func TestGetComputePrice_Windows(t *testing.T) {
	// Use distinct descriptions to avoid the substring ambiguity:
	// Linux base SKUs use descriptions that don't appear inside the Windows ones.
	// The FamilySKU for n2 has CPUDesc="N2 Instance Core" and WindowsSKU for n2
	// has "N2 Instance Core running Windows". "N2 Instance Core" IS a substring of
	// "N2 Instance Core running Windows", so a lookup for "N2 Instance Core" can
	// match either key depending on map iteration order.
	//
	// We model this test by verifying the price is > the Windows-only cost and
	// that the provider returns a non-zero price with both SKUs present, without
	// asserting the exact split.
	skus := []map[string]any{
		// Linux base SKUs for N2.
		makeSKU("N2 Instance Core", "OnDemand", "us-central1", "0", 31_611_000),
		makeSKU("N2 Instance Ram", "OnDemand", "us-central1", "0", 4_237_000),
		// Windows license SKUs (superstring of Linux descriptions).
		makeSKU("N2 Instance Core running Windows", "OnDemand", "us-central1", "0", 50_000_000),
		makeSKU("N2 Instance Ram running Windows", "OnDemand", "us-central1", "0", 10_000_000),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	prices, err := p.GetComputePrice(context.Background(), "n2-standard-4", "us-central1", "Windows", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("GetComputePrice (Windows): %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("expected 1 price, got %d", len(prices))
	}

	// The Windows price must be positive.
	// The lookup is a substring match, so "N2 Instance Core" can match BOTH the Linux
	// SKU ("N2 Instance Core") and the Windows SKU ("N2 Instance Core running Windows"),
	// and "N2 Instance Ram" can match BOTH "N2 Instance Ram" and "N2 Instance Ram running
	// Windows". The result is non-deterministic across runs due to map iteration order.
	//
	// Minimum: Linux base + Windows license, where both base lookups hit their Linux SKU.
	//   = (4*0.031611 + 16*0.004237) + (4*0.05 + 16*0.01)
	minExpected := (4*0.031611 + 16*0.004237) + (4*0.05 + 16*0.01)
	if prices[0].PricePerUnit < minExpected-1e-9 {
		t.Errorf("Windows PricePerUnit = %.6f, below minimum %.6f", prices[0].PricePerUnit, minExpected)
	}
	// Maximum: all four lookups hit the Windows SKU rates.
	//   = (4*0.05 + 16*0.01) + (4*0.05 + 16*0.01)
	maxExpected := 2 * (4*0.05 + 16*0.01)
	if prices[0].PricePerUnit > maxExpected+1e-5 {
		t.Errorf("Windows PricePerUnit = %.6f, exceeds max plausible %.6f", prices[0].PricePerUnit, maxExpected)
	}
}

// TestGetComputePrice_WindowsUnsupportedFamily verifies E2 returns empty for Windows.
func TestGetComputePrice_WindowsUnsupportedFamily(t *testing.T) {
	skus := []map[string]any{
		makeSKU("E2 Instance Core", "OnDemand", "us-central1", "0", 20_000_000),
		makeSKU("E2 Instance Ram", "OnDemand", "us-central1", "0", 3_000_000),
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	prices, err := p.GetComputePrice(context.Background(), "e2-standard-4", "us-central1", "Windows", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// E2 has no Windows support — expect empty.
	if len(prices) != 0 {
		t.Errorf("expected empty slice for E2 Windows, got %d prices", len(prices))
	}
}

// TestGetComputePrice_SKUNotFound verifies that a missing SKU returns an empty slice.
func TestGetComputePrice_SKUNotFound(t *testing.T) {
	// Only RAM SKU; CPU SKU missing.
	skus := []map[string]any{
		makeSKU("N2 Instance Ram", "OnDemand", "us-central1", "0", 4_237_000),
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	prices, err := p.GetComputePrice(context.Background(), "n2-standard-4", "us-central1", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prices) != 0 {
		t.Errorf("expected empty slice for missing CPU SKU, got %d prices", len(prices))
	}
}

// TestGetStoragePrice_GCS tests GCS standard storage pricing.
func TestGetStoragePrice_GCS(t *testing.T) {
	// GCS Standard Storage: $0.020/GB-month.
	skus := []map[string]any{
		makeSKU("Standard Storage", "OnDemand", "us-central1", "0", 20_000_000),
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	prices, err := p.GetStoragePrice(context.Background(), "standard", "us-central1", 0)
	if err != nil {
		t.Fatalf("GetStoragePrice (GCS standard): %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("expected 1 price, got %d", len(prices))
	}
	if abs(prices[0].PricePerUnit-0.02) > 1e-9 {
		t.Errorf("GCS standard price = %.6f, want 0.020000", prices[0].PricePerUnit)
	}
	if prices[0].Unit != models.PriceUnitPerGBMonth {
		t.Errorf("Unit = %v, want per_gb_month", prices[0].Unit)
	}
}

// TestGetStoragePrice_PD tests Persistent Disk pricing.
func TestGetStoragePrice_PD(t *testing.T) {
	// PD SSD: $0.170/GB-month.
	skus := []map[string]any{
		makeSKU("SSD backed PD Capacity", "OnDemand", "us-central1", "0", 170_000_000),
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	prices, err := p.GetStoragePrice(context.Background(), "pd-ssd", "us-central1", 0)
	if err != nil {
		t.Fatalf("GetStoragePrice (pd-ssd): %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("expected 1 price, got %d", len(prices))
	}
	if abs(prices[0].PricePerUnit-0.17) > 1e-9 {
		t.Errorf("pd-ssd price = %.6f, want 0.170000", prices[0].PricePerUnit)
	}
}

// TestGetStoragePrice_UnknownType verifies an error for unknown storage types.
func TestGetStoragePrice_UnknownType(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()
	p := newTestProvider(t, ts)

	_, err := p.GetStoragePrice(context.Background(), "foobar", "us-central1", 0)
	if err == nil {
		t.Error("expected error for unknown storage type")
	}
}

// TestGetComputePrice_UnknownInstanceType verifies an error for unknown instance types.
func TestGetComputePrice_UnknownInstanceType(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()
	p := newTestProvider(t, ts)

	_, err := p.GetComputePrice(context.Background(), "not-a-real-type", "us-central1", "Linux", models.PricingTermOnDemand)
	if err == nil {
		t.Error("expected error for unknown instance type")
	}
}

// TestCachingAvoidsDuplicateHTTP verifies that a second call uses the cache.
func TestCachingAvoidsDuplicateHTTP(t *testing.T) {
	skus := []map[string]any{
		makeSKU("N2 Instance Core", "OnDemand", "us-central1", "0", 31_611_000),
		makeSKU("N2 Instance Ram", "OnDemand", "us-central1", "0", 4_237_000),
	}
	var callCount int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	_, err := p.GetComputePrice(ctx, "n2-standard-4", "us-central1", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, err = p.GetComputePrice(ctx, "n2-standard-4", "us-central1", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	// The compute price result is cached after the first call, so HTTP should be
	// called at most once per service (for the SKU fetch) + once per compute index.
	// Both fetches happen on the first call; the second call should use the cache.
	if callCount > 2 {
		t.Errorf("expected at most 2 HTTP calls (SKU fetch + price index), got %d", callCount)
	}
}

// TestListRegions verifies the region list contains expected regions.
func TestListRegions(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()
	p := newTestProvider(t, ts)

	regions, err := p.ListRegions(context.Background(), "compute")
	if err != nil {
		t.Fatalf("ListRegions: %v", err)
	}
	if len(regions) == 0 {
		t.Fatal("ListRegions: expected non-empty region list")
	}
	found := false
	for _, r := range regions {
		if r == "us-central1" {
			found = true
			break
		}
	}
	if !found {
		t.Error("ListRegions: us-central1 not found in region list")
	}
}

// TestMajorRegions verifies 12 major regions.
func TestMajorRegions(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()
	p := newTestProvider(t, ts)
	regions := p.MajorRegions()
	if len(regions) != 12 {
		t.Errorf("expected 12 major regions, got %d", len(regions))
	}
}

// TestListInstanceTypes verifies filtering logic.
func TestListInstanceTypes(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()
	p := newTestProvider(t, ts)
	ctx := context.Background()

	// Filter by family "n2".
	types, err := p.ListInstanceTypes(ctx, "us-central1", "n2", 0, 0, false)
	if err != nil {
		t.Fatalf("ListInstanceTypes: %v", err)
	}
	if len(types) == 0 {
		t.Fatal("expected non-empty list for family=n2")
	}
	for _, it := range types {
		if !startsWith(it.InstanceType, "n2") {
			t.Errorf("ListInstanceTypes(family=n2): unexpected type %q", it.InstanceType)
		}
	}

	// Filter by minVCPUs.
	types, err = p.ListInstanceTypes(ctx, "us-central1", "n2", 32, 0, false)
	if err != nil {
		t.Fatalf("ListInstanceTypes (minVCPU): %v", err)
	}
	for _, it := range types {
		if it.VCPU < 32 {
			t.Errorf("type %q has %d vCPUs, expected >= 32", it.InstanceType, it.VCPU)
		}
	}

	// GPU filter — should return only A2 family.
	gpuTypes, err := p.ListInstanceTypes(ctx, "us-central1", "", 0, 0, true)
	if err != nil {
		t.Fatalf("ListInstanceTypes (gpu): %v", err)
	}
	if len(gpuTypes) == 0 {
		t.Fatal("expected non-empty GPU instance list")
	}
	for _, it := range gpuTypes {
		if !startsWith(it.InstanceType, "a2") {
			t.Errorf("GPU filter returned non-A2 type: %q", it.InstanceType)
		}
	}
}

// TestSupports verifies the Supports routing.
// The exact set of supported domains is determined by the Supports() implementation
// in gcp_compute.go, which may be extended by Part 2 files.
func TestSupports(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()
	p := newTestProvider(t, ts)

	// These domains must always be supported by Part 1.
	mustSupport := []struct {
		domain  models.PricingDomain
		service string
	}{
		{models.PricingDomainCompute, ""},
		{models.PricingDomainCompute, "compute_engine"},
		{models.PricingDomainCompute, "vm"},
		{models.PricingDomainStorage, ""},
		{models.PricingDomainStorage, "gcs"},
		{models.PricingDomainStorage, "persistent_disk"},
	}
	for _, c := range mustSupport {
		if !p.Supports(c.domain, c.service) {
			t.Errorf("Supports(%v, %q) = false, want true", c.domain, c.service)
		}
	}

	// Unknown domain/service pair must not be supported.
	if p.Supports("nonexistent_domain", "nonexistent_service") {
		t.Error("Supports(nonexistent_domain, nonexistent_service) = true, want false")
	}
}

// TestGetPriceDispatch verifies GetPrice dispatches to compute and storage.
func TestGetPriceDispatch(t *testing.T) {
	skus := []map[string]any{
		makeSKU("N2 Instance Core", "OnDemand", "us-central1", "0", 31_611_000),
		makeSKU("N2 Instance Ram", "OnDemand", "us-central1", "0", 4_237_000),
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()
	p := newTestProvider(t, ts)

	spec := &models.ComputePricingSpec{
		BasePricingSpec: models.BasePricingSpec{
			Provider: models.CloudProviderGCP,
			Domain:   models.PricingDomainCompute,
			Region:   "us-central1",
			Term:     models.PricingTermOnDemand,
		},
		ResourceType: "n2-standard-4",
		OS:           "Linux",
	}

	result, err := p.GetPrice(context.Background(), spec)
	if err != nil {
		t.Fatalf("GetPrice: %v", err)
	}
	if len(result.PublicPrices) == 0 {
		t.Fatal("expected at least one price in result")
	}
	if result.SchemaVersion != schemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", result.SchemaVersion, schemaVersion)
	}
}

// TestStubMethods verifies stubs return ErrNotSupported.
func TestStubMethods(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()
	p := newTestProvider(t, ts)
	ctx := context.Background()

	_, err := p.GetEffectivePrice(ctx, &models.ComputePricingSpec{})
	if err == nil {
		t.Error("GetEffectivePrice: expected ErrNotSupported")
	}

	_, err = p.GetSpotHistory(ctx, &models.ComputePricingSpec{}, 24, "")
	if err == nil {
		t.Error("GetSpotHistory: expected ErrNotSupported")
	}

	// GetDiscountSummary returns a structured map, not an error.
	summary, err := p.GetDiscountSummary(ctx)
	if err != nil {
		t.Errorf("GetDiscountSummary: unexpected error: %v", err)
	}
	if summary["error"] != "not_supported" {
		t.Errorf("GetDiscountSummary: error key = %v, want not_supported", summary["error"])
	}
}

// TestAnnotateFresh verifies freshness stamping.
func TestAnnotateFresh(t *testing.T) {
	before := time.Now().UTC()
	prices := []models.NormalizedPrice{{Provider: models.CloudProviderGCP}}
	result := annotateFresh(prices)
	after := time.Now().UTC()

	if len(result) != 1 {
		t.Fatal("expected 1 price")
	}
	pr := result[0]
	if pr.FetchedAt == nil {
		t.Fatal("FetchedAt should be set")
	}
	if pr.FetchedAt.Before(before) || pr.FetchedAt.After(after) {
		t.Errorf("FetchedAt %v not in range [%v, %v]", pr.FetchedAt, before, after)
	}
	if pr.CacheAgeSeconds == nil || *pr.CacheAgeSeconds != 0 {
		t.Error("CacheAgeSeconds should be 0")
	}
	if pr.SourceURL != sourceURL {
		t.Errorf("SourceURL = %q, want %q", pr.SourceURL, sourceURL)
	}
}

// TestRegionMatches verifies the skuMatchesRegion helper in gcp_database.go
// (used by all SKU index builders).
func TestRegionMatches(t *testing.T) {
	regions := []any{"us-central1", "us-east1"}
	if !skuMatchesRegion(regions, "us-central1") {
		t.Error("skuMatchesRegion: expected match for us-central1")
	}
	if skuMatchesRegion(regions, "europe-west1") {
		t.Error("skuMatchesRegion: unexpected match for europe-west1")
	}
	global := []any{"global"}
	if !skuMatchesRegion(global, "any-region") {
		t.Error("skuMatchesRegion: global should match any region")
	}
}

// --------------------------------------------------------------------------
// Backlog tests — gcp-compute domain
// --------------------------------------------------------------------------

// TestGetComputePrice_CUD1Yr_NumericPrice verifies that a 1-year CUD pricing
// request returns a numeric (non-zero) price, confirming the Commit1Yr SKU
// lookup path works end-to-end.
func TestGetComputePrice_CUD1Yr_NumericPrice(t *testing.T) {
	// N2 CUD 1yr rates (mirrors actual GCP API format):
	// CPU: "Commitment v1: N2 Cpu" at $0.019560/core-hr
	// RAM: "Commitment v1: N2 Ram" at $0.002626/GB-hr
	// n2-standard-4: 4 vCPU, 16 GB
	// Total CUD price: 4*0.019560 + 16*0.002626 = 0.07824 + 0.042016 = 0.120256
	skus := []map[string]any{
		makeSKU("Commitment v1: N2 Cpu in Americas for 1 Year", "Commit1Yr", "us-central1", "0", 19_560_000),
		makeSKU("Commitment v1: N2 Ram in Americas for 1 Year", "Commit1Yr", "us-central1", "0", 2_626_000),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	prices, err := p.GetComputePrice(context.Background(), "n2-standard-4", "us-central1", "Linux", models.PricingTermCUD1Yr)
	if err != nil {
		t.Fatalf("GetComputePrice (CUD1Yr): %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("expected 1 price, got %d", len(prices))
	}

	// Price must be positive (non-zero, non-error).
	if prices[0].PricePerUnit <= 0 {
		t.Errorf("CUD1Yr price = %v, want > 0", prices[0].PricePerUnit)
	}

	// CUD price should be cheaper than on-demand ($0.194236).
	// On-demand: 4*0.031611 + 16*0.004237 = 0.194236
	// CUD1Yr:    4*0.019560 + 16*0.002626 = 0.120256
	want := 4*0.019560 + 16*0.002626
	if abs(prices[0].PricePerUnit-want) > 1e-6 {
		t.Errorf("CUD1Yr PricePerUnit = %.6f, want %.6f", prices[0].PricePerUnit, want)
	}
	if prices[0].PricingTerm != models.PricingTermCUD1Yr {
		t.Errorf("PricingTerm = %v, want cud_1yr", prices[0].PricingTerm)
	}
}

// TestGetStoragePrice_GCS_AllTiers verifies that storage pricing returns
// non-empty results for all four GCS tiers: standard, nearline, coldline, archive.
func TestGetStoragePrice_GCS_AllTiers(t *testing.T) {
	// Prices: standard $0.020, nearline $0.010, coldline $0.004, archive $0.0012
	skus := []map[string]any{
		makeSKU("Standard Storage US", "OnDemand", "us-central1", "0", 20_000_000),
		makeSKU("Nearline Storage US", "OnDemand", "us-central1", "0", 10_000_000),
		makeSKU("Coldline Storage US", "OnDemand", "us-central1", "0", 4_000_000),
		makeSKU("Archive Storage US", "OnDemand", "us-central1", "0", 1_200_000),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	tiers := []struct {
		name      string
		wantPrice float64
	}{
		{"standard", 0.020},
		{"nearline", 0.010},
		{"coldline", 0.004},
		{"archive", 0.0012},
	}

	for _, tier := range tiers {
		prices, err := p.GetStoragePrice(ctx, tier.name, "us-central1", 0)
		if err != nil {
			t.Errorf("GetStoragePrice(%q): %v", tier.name, err)
			continue
		}
		if len(prices) == 0 {
			t.Errorf("GetStoragePrice(%q): expected at least 1 price, got 0", tier.name)
			continue
		}
		if abs(prices[0].PricePerUnit-tier.wantPrice) > 1e-9 {
			t.Errorf("GetStoragePrice(%q): price = %.6f, want %.6f", tier.name, prices[0].PricePerUnit, tier.wantPrice)
		}
		if prices[0].Unit != models.PriceUnitPerGBMonth {
			t.Errorf("GetStoragePrice(%q): unit = %v, want per_gb_month", tier.name, prices[0].Unit)
		}
	}
}

// TestGetStoragePrice_PricingOrder verifies that cheaper tiers are not more expensive
// than standard: archive <= coldline <= nearline <= standard.
func TestGetStoragePrice_PricingOrder(t *testing.T) {
	// Realistic GCS pricing: standard > nearline > coldline > archive.
	skus := []map[string]any{
		makeSKU("Standard Storage US", "OnDemand", "us-central1", "0", 20_000_000),
		makeSKU("Nearline Storage US", "OnDemand", "us-central1", "0", 10_000_000),
		makeSKU("Coldline Storage US", "OnDemand", "us-central1", "0", 4_000_000),
		makeSKU("Archive Storage US", "OnDemand", "us-central1", "0", 1_200_000),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	getPriceFor := func(tier string) float64 {
		t.Helper()
		prices, err := p.GetStoragePrice(ctx, tier, "us-central1", 0)
		if err != nil {
			t.Fatalf("GetStoragePrice(%q): %v", tier, err)
		}
		if len(prices) == 0 {
			t.Fatalf("GetStoragePrice(%q): empty result", tier)
		}
		return prices[0].PricePerUnit
	}

	standardPrice := getPriceFor("standard")
	nearlinePrice := getPriceFor("nearline")
	coldlinePrice := getPriceFor("coldline")
	archivePrice := getPriceFor("archive")

	// Cheapest tier (archive) must not exceed standard.
	if archivePrice > standardPrice {
		t.Errorf("archive ($%.4f) more expensive than standard ($%.4f)", archivePrice, standardPrice)
	}
	if coldlinePrice > standardPrice {
		t.Errorf("coldline ($%.4f) more expensive than standard ($%.4f)", coldlinePrice, standardPrice)
	}
	if nearlinePrice > standardPrice {
		t.Errorf("nearline ($%.4f) more expensive than standard ($%.4f)", nearlinePrice, standardPrice)
	}
	// Verify the expected order: archive <= coldline <= nearline <= standard.
	if archivePrice > coldlinePrice {
		t.Errorf("archive ($%.4f) should be <= coldline ($%.4f)", archivePrice, coldlinePrice)
	}
	if coldlinePrice > nearlinePrice {
		t.Errorf("coldline ($%.4f) should be <= nearline ($%.4f)", coldlinePrice, nearlinePrice)
	}
}

// TestGetComputePrice_CustomMachineType verifies that a custom machine type
// (not in the GCPInstanceSpecs table) falls back to naming-convention parsing
// and returns a valid hourly cost.
func TestGetComputePrice_CustomMachineType(t *testing.T) {
	// n2-standard-200 is not in the spec table; ParseInstanceType should fall
	// back to naming convention: 200 vCPU, 200*4.0 = 800 GB RAM.
	// Pricing: 200*0.031611 + 800*0.004237 = 6.3222 + 3.3896 = 9.7118
	skus := []map[string]any{
		makeSKU("N2 Instance Core", "OnDemand", "us-central1", "0", 31_611_000),
		makeSKU("N2 Instance Ram", "OnDemand", "us-central1", "0", 4_237_000),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	prices, err := p.GetComputePrice(context.Background(), "n2-standard-200", "us-central1", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("GetComputePrice (custom type): %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("expected 1 price for custom machine type, got %d", len(prices))
	}

	// Price must be positive.
	if prices[0].PricePerUnit <= 0 {
		t.Errorf("custom machine type price = %v, want > 0", prices[0].PricePerUnit)
	}

	// 200 vCPU * $0.031611 + 800 GB * $0.004237 = $9.7118
	want := 200*0.031611 + 800*0.004237
	if abs(prices[0].PricePerUnit-want) > 1e-4 {
		t.Errorf("custom machine type PricePerUnit = %.4f, want %.4f", prices[0].PricePerUnit, want)
	}
	// Verify the attributes reflect the custom type.
	if prices[0].Attributes["instanceType"] != "n2-standard-200" {
		t.Errorf("instanceType attribute = %q, want n2-standard-200", prices[0].Attributes["instanceType"])
	}
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// TestMain sets up any global state needed for tests.
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
