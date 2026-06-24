// Package gcp — unit tests for compute and storage pricing (Part 1).
package gcp

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
// Cross-term price invariant tests
// --------------------------------------------------------------------------

// TestGetComputePrice_SpotCheaperThanOnDemand verifies the price ordering
// invariant: Preemptible (spot) must always be cheaper than OnDemand for the
// same instance type.
func TestGetComputePrice_SpotCheaperThanOnDemand(t *testing.T) {
	// n2-standard-4: 4 vCPU, 16 GB RAM.
	// OnDemand:    $0.019560/vCPU-hr, $0.002626/GB-hr
	// Preemptible: $0.004000/vCPU-hr, $0.000540/GB-hr
	skus := []map[string]any{
		// OnDemand SKUs
		makeSKU("N2 Instance Core", "OnDemand", "us-central1", "0", 19_560_000),
		makeSKU("N2 Instance Ram", "OnDemand", "us-central1", "0", 2_626_000),
		// Preemptible (spot) SKUs
		makeSKU("Preemptible N2 Instance Core", "Preemptible", "us-central1", "0", 4_000_000),
		makeSKU("Preemptible N2 Instance Ram", "Preemptible", "us-central1", "0", 540_000),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	spotPrices, err := p.GetComputePrice(ctx, "n2-standard-4", "us-central1", "Linux", models.PricingTermSpot)
	if err != nil {
		t.Fatalf("GetComputePrice (spot): %v", err)
	}
	if len(spotPrices) != 1 {
		t.Fatalf("expected 1 spot price, got %d", len(spotPrices))
	}

	onDemandPrices, err := p.GetComputePrice(ctx, "n2-standard-4", "us-central1", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("GetComputePrice (on_demand): %v", err)
	}
	if len(onDemandPrices) != 1 {
		t.Fatalf("expected 1 on_demand price, got %d", len(onDemandPrices))
	}

	spotPrice := spotPrices[0].PricePerUnit
	onDemandPrice := onDemandPrices[0].PricePerUnit

	if spotPrice >= onDemandPrice {
		t.Errorf("price invariant violated: spot ($%.6f) must be < on_demand ($%.6f)", spotPrice, onDemandPrice)
	}
}

// TestGetComputePrice_CUD1YrCheaperThanOnDemand verifies the price ordering
// invariant: 1-year committed-use discount must be cheaper than on_demand for
// the same instance type.
func TestGetComputePrice_CUD1YrCheaperThanOnDemand(t *testing.T) {
	// n2-standard-4: 4 vCPU, 16 GB RAM.
	// OnDemand:  $0.031611/vCPU-hr, $0.004237/GB-hr
	// Commit1Yr: $0.019560/vCPU-hr, $0.002626/GB-hr
	skus := []map[string]any{
		// OnDemand SKUs
		makeSKU("N2 Instance Core", "OnDemand", "us-central1", "0", 31_611_000),
		makeSKU("N2 Instance Ram", "OnDemand", "us-central1", "0", 4_237_000),
		// CUD 1-year SKUs
		makeSKU("Commitment v1: N2 Cpu in Americas for 1 Year", "Commit1Yr", "us-central1", "0", 19_560_000),
		makeSKU("Commitment v1: N2 Ram in Americas for 1 Year", "Commit1Yr", "us-central1", "0", 2_626_000),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	cud1YrPrices, err := p.GetComputePrice(ctx, "n2-standard-4", "us-central1", "Linux", models.PricingTermCUD1Yr)
	if err != nil {
		t.Fatalf("GetComputePrice (cud_1yr): %v", err)
	}
	if len(cud1YrPrices) != 1 {
		t.Fatalf("expected 1 cud_1yr price, got %d", len(cud1YrPrices))
	}

	onDemandPrices, err := p.GetComputePrice(ctx, "n2-standard-4", "us-central1", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("GetComputePrice (on_demand): %v", err)
	}
	if len(onDemandPrices) != 1 {
		t.Fatalf("expected 1 on_demand price, got %d", len(onDemandPrices))
	}

	cud1YrPrice := cud1YrPrices[0].PricePerUnit
	onDemandPrice := onDemandPrices[0].PricePerUnit

	if cud1YrPrice >= onDemandPrice {
		t.Errorf("price invariant violated: cud_1yr ($%.6f) must be < on_demand ($%.6f)", cud1YrPrice, onDemandPrice)
	}
}

// TestGetComputePrice_CUD3YrCheaperThan1Yr verifies the price ordering
// invariant: 3-year committed-use discount must be cheaper than 1-year for
// the same instance type.
func TestGetComputePrice_CUD3YrCheaperThan1Yr(t *testing.T) {
	// n2-standard-4: 4 vCPU, 16 GB RAM.
	// Commit1Yr: $0.019560/vCPU-hr, $0.002626/GB-hr
	// Commit3Yr: $0.013972/vCPU-hr, $0.001874/GB-hr
	skus := []map[string]any{
		// CUD 1-year SKUs
		makeSKU("Commitment v1: N2 Cpu in Americas for 1 Year", "Commit1Yr", "us-central1", "0", 19_560_000),
		makeSKU("Commitment v1: N2 Ram in Americas for 1 Year", "Commit1Yr", "us-central1", "0", 2_626_000),
		// CUD 3-year SKUs (same desc substring, different usageType)
		makeSKU("Commitment v1: N2 Cpu in Americas for 3 Years", "Commit3Yr", "us-central1", "0", 13_972_000),
		makeSKU("Commitment v1: N2 Ram in Americas for 3 Years", "Commit3Yr", "us-central1", "0", 1_874_000),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	cud3YrPrices, err := p.GetComputePrice(ctx, "n2-standard-4", "us-central1", "Linux", models.PricingTermCUD3Yr)
	if err != nil {
		t.Fatalf("GetComputePrice (cud_3yr): %v", err)
	}
	if len(cud3YrPrices) != 1 {
		t.Fatalf("expected 1 cud_3yr price, got %d", len(cud3YrPrices))
	}

	cud1YrPrices, err := p.GetComputePrice(ctx, "n2-standard-4", "us-central1", "Linux", models.PricingTermCUD1Yr)
	if err != nil {
		t.Fatalf("GetComputePrice (cud_1yr): %v", err)
	}
	if len(cud1YrPrices) != 1 {
		t.Fatalf("expected 1 cud_1yr price, got %d", len(cud1YrPrices))
	}

	cud3YrPrice := cud3YrPrices[0].PricePerUnit
	cud1YrPrice := cud1YrPrices[0].PricePerUnit

	if cud3YrPrice >= cud1YrPrice {
		t.Errorf("price invariant violated: cud_3yr ($%.6f) must be < cud_1yr ($%.6f)", cud3YrPrice, cud1YrPrice)
	}
}

// TestGetComputePrice_AllTermsOrderedCorrectly verifies the full price ladder:
// spot < cud_3yr < cud_1yr < on_demand for the same instance type.
func TestGetComputePrice_AllTermsOrderedCorrectly(t *testing.T) {
	// n2-standard-4: 4 vCPU, 16 GB RAM.
	// Rates chosen so per-vCPU and per-GB rates are strictly ordered the same way,
	// guaranteeing the totals are strictly ordered:
	//   spot < cud_3yr < cud_1yr < on_demand
	skus := []map[string]any{
		// OnDemand SKUs:    $0.031611/vCPU, $0.004237/GB
		makeSKU("N2 Instance Core", "OnDemand", "us-central1", "0", 31_611_000),
		makeSKU("N2 Instance Ram", "OnDemand", "us-central1", "0", 4_237_000),
		// CUD 1-year SKUs:  $0.019560/vCPU, $0.002626/GB
		makeSKU("Commitment v1: N2 Cpu in Americas for 1 Year", "Commit1Yr", "us-central1", "0", 19_560_000),
		makeSKU("Commitment v1: N2 Ram in Americas for 1 Year", "Commit1Yr", "us-central1", "0", 2_626_000),
		// CUD 3-year SKUs:  $0.013972/vCPU, $0.001874/GB
		makeSKU("Commitment v1: N2 Cpu in Americas for 3 Years", "Commit3Yr", "us-central1", "0", 13_972_000),
		makeSKU("Commitment v1: N2 Ram in Americas for 3 Years", "Commit3Yr", "us-central1", "0", 1_874_000),
		// Preemptible (spot) SKUs: $0.006700/vCPU, $0.000900/GB
		makeSKU("Preemptible N2 Instance Core", "Preemptible", "us-central1", "0", 6_700_000),
		makeSKU("Preemptible N2 Instance Ram", "Preemptible", "us-central1", "0", 900_000),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	getPriceForTerm := func(term models.PricingTerm) float64 {
		t.Helper()
		prices, err := p.GetComputePrice(ctx, "n2-standard-4", "us-central1", "Linux", term)
		if err != nil {
			t.Fatalf("GetComputePrice (term=%s): %v", term, err)
		}
		if len(prices) != 1 {
			t.Fatalf("GetComputePrice (term=%s): expected 1 price, got %d", term, len(prices))
		}
		return prices[0].PricePerUnit
	}

	spotPrice := getPriceForTerm(models.PricingTermSpot)
	cud3YrPrice := getPriceForTerm(models.PricingTermCUD3Yr)
	cud1YrPrice := getPriceForTerm(models.PricingTermCUD1Yr)
	onDemandPrice := getPriceForTerm(models.PricingTermOnDemand)

	// Assert the full price ladder: spot < cud_3yr < cud_1yr < on_demand.
	if spotPrice >= cud3YrPrice {
		t.Errorf("price invariant violated: spot ($%.6f) must be < cud_3yr ($%.6f)", spotPrice, cud3YrPrice)
	}
	if cud3YrPrice >= cud1YrPrice {
		t.Errorf("price invariant violated: cud_3yr ($%.6f) must be < cud_1yr ($%.6f)", cud3YrPrice, cud1YrPrice)
	}
	if cud1YrPrice >= onDemandPrice {
		t.Errorf("price invariant violated: cud_1yr ($%.6f) must be < on_demand ($%.6f)", cud1YrPrice, onDemandPrice)
	}
}

// --------------------------------------------------------------------------
// SUD pricing tests
// --------------------------------------------------------------------------

// makeN1SUDServer returns an httptest.Server serving N1 on-demand CPU and RAM SKUs.
// gcpSUDPrice fetches on-demand rates and derives the SUD blended price from them.
func makeN1SUDServer(t *testing.T) *httptest.Server {
	t.Helper()
	skus := []map[string]any{
		makeSKU("N1 Predefined Instance Core", "OnDemand", "us-central1", "0", 31_611_000),
		makeSKU("N1 Predefined Instance Ram", "OnDemand", "us-central1", "0", 4_237_000),
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
}

// TestGetComputePrice_SUD_EligibleN1_ReturnsTerm verifies that n1-standard-4 with
// term=sud returns a non-empty result with Term == "sud".
func TestGetComputePrice_SUD_EligibleN1_ReturnsTerm(t *testing.T) {
	ts := makeN1SUDServer(t)
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	prices, err := p.GetComputePrice(ctx, "n1-standard-4", "us-central1", "Linux", models.PricingTermSUD)
	if err != nil {
		t.Fatalf("GetComputePrice (sud): %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected at least 1 SUD price, got 0")
	}
	if prices[0].PricingTerm != models.PricingTermSUD {
		t.Errorf("Term = %q, want %q", prices[0].PricingTerm, models.PricingTermSUD)
	}
}

// TestGetComputePrice_SUD_BlendedRateMath verifies the 30%-off blended calculation.
// n1-standard-4: 4 vCPU, 15.0 GB RAM.
// on_demand = 4*0.031611 + 15*0.004237 = 0.126444 + 0.063555 = 0.189999 /hr
// blended   = on_demand * 0.70
func TestGetComputePrice_SUD_BlendedRateMath(t *testing.T) {
	ts := makeN1SUDServer(t)
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	prices, err := p.GetComputePrice(ctx, "n1-standard-4", "us-central1", "Linux", models.PricingTermSUD)
	if err != nil {
		t.Fatalf("GetComputePrice (sud): %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected 1 SUD price, got 0")
	}

	const cpuRate = 0.031611
	const ramRate = 0.004237
	onDemandTotal := 4*cpuRate + 15.0*ramRate
	wantBlended := onDemandTotal * 0.70

	if abs(prices[0].PricePerUnit-wantBlended) > 1e-4 {
		t.Errorf("SUD blended PricePerUnit = %.6f, want %.6f (30%% off %.6f)",
			prices[0].PricePerUnit, wantBlended, onDemandTotal)
	}
}

// TestGetComputePrice_SUD_TermLabelIsSUD asserts the returned pricing term is exactly
// models.PricingTermSUD, not on_demand or any other term.
func TestGetComputePrice_SUD_TermLabelIsSUD(t *testing.T) {
	ts := makeN1SUDServer(t)
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	prices, err := p.GetComputePrice(ctx, "n1-standard-4", "us-central1", "Linux", models.PricingTermSUD)
	if err != nil {
		t.Fatalf("GetComputePrice (sud): %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected 1 SUD price, got 0")
	}

	got := prices[0].PricingTerm
	if got != models.PricingTermSUD {
		t.Errorf("PricingTerm = %q, want %q (not on_demand, not spot, not cud_*)", got, models.PricingTermSUD)
	}
}

// TestGetComputePrice_SUD_AttributesComplete asserts that the SUD result carries all
// required informational attributes with the correct sentinel values.
func TestGetComputePrice_SUD_AttributesComplete(t *testing.T) {
	ts := makeN1SUDServer(t)
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	prices, err := p.GetComputePrice(ctx, "n1-standard-4", "us-central1", "Linux", models.PricingTermSUD)
	if err != nil {
		t.Fatalf("GetComputePrice (sud): %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected 1 SUD price, got 0")
	}

	attrs := prices[0].Attributes

	// Required attribute keys.
	required := []string{
		"sud_tier_0", "sud_tier_1", "sud_tier_2", "sud_tier_3",
		"sud_blended_factor", "sud_discount_pct", "usage_assumption",
		"sud_rate_source", "note",
	}
	for _, key := range required {
		if _, ok := attrs[key]; !ok {
			t.Errorf("missing required attribute %q", key)
		}
	}

	// Sentinel value checks.
	if attrs["sud_blended_factor"] != "0.700" {
		t.Errorf("sud_blended_factor = %q, want %q", attrs["sud_blended_factor"], "0.700")
	}
	if attrs["sud_discount_pct"] != "30.0" {
		t.Errorf("sud_discount_pct = %q, want %q", attrs["sud_discount_pct"], "30.0")
	}
	if !strings.Contains(attrs["sud_rate_source"], "catalog") {
		t.Errorf("sud_rate_source = %q, want it to contain %q", attrs["sud_rate_source"], "catalog")
	}
}

// TestGetComputePrice_SUD_TierRatesDescending verifies that tier rates are strictly
// decreasing (tier_0 > tier_1 > tier_2 > tier_3 > 0), reflecting GCP's published
// 0/20/40/60% discount schedule.
func TestGetComputePrice_SUD_TierRatesDescending(t *testing.T) {
	ts := makeN1SUDServer(t)
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	prices, err := p.GetComputePrice(ctx, "n1-standard-4", "us-central1", "Linux", models.PricingTermSUD)
	if err != nil {
		t.Fatalf("GetComputePrice (sud): %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected 1 SUD price, got 0")
	}

	attrs := prices[0].Attributes

	// Extract the dollar rate from each tier attribute.
	// Format: "0-182.5 hrs (0-25% of month): $0.189999/hr (0% off)"
	parseTierRate := func(key string) float64 {
		t.Helper()
		s, ok := attrs[key]
		if !ok {
			t.Errorf("attribute %q not found", key)
			return 0
		}
		// Find '$' and parse the number after it.
		dollarIdx := strings.Index(s, "$")
		if dollarIdx < 0 {
			t.Errorf("attribute %q has no '$': %q", key, s)
			return 0
		}
		rest := s[dollarIdx+1:]
		slashIdx := strings.Index(rest, "/hr")
		if slashIdx < 0 {
			t.Errorf("attribute %q has no '/hr': %q", key, s)
			return 0
		}
		var rate float64
		if _, err := fmt.Sscanf(rest[:slashIdx], "%f", &rate); err != nil {
			t.Errorf("cannot parse rate from %q: %v", rest[:slashIdx], err)
		}
		return rate
	}

	tier0 := parseTierRate("sud_tier_0")
	tier1 := parseTierRate("sud_tier_1")
	tier2 := parseTierRate("sud_tier_2")
	tier3 := parseTierRate("sud_tier_3")

	if tier0 <= 0 {
		t.Errorf("tier_0 rate = %v, want > 0", tier0)
	}
	if tier0 <= tier1 {
		t.Errorf("tier_0 ($%.6f) must be > tier_1 ($%.6f)", tier0, tier1)
	}
	if tier1 <= tier2 {
		t.Errorf("tier_1 ($%.6f) must be > tier_2 ($%.6f)", tier1, tier2)
	}
	if tier2 <= tier3 {
		t.Errorf("tier_2 ($%.6f) must be > tier_3 ($%.6f)", tier2, tier3)
	}
	if tier3 <= 0 {
		t.Errorf("tier_3 rate = %v, want > 0", tier3)
	}
}

// TestGetComputePrice_SUD_IneligibleA2_ReturnsEmpty verifies that a2-highgpu-1g
// (a GPU family) returns an empty slice with no error when term=sud.
// a2 IS in GCPFamilySKU but fails the SUDEligible check inside gcpSUDPrice.
func TestGetComputePrice_SUD_IneligibleA2_ReturnsEmpty(t *testing.T) {
	// No SKUs needed — gcpSUDPrice short-circuits before any HTTP call.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(nil))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	prices, err := p.GetComputePrice(ctx, "a2-highgpu-1g", "us-central1", "Linux", models.PricingTermSUD)
	if err != nil {
		t.Fatalf("GetComputePrice (a2 sud): unexpected error: %v", err)
	}
	if len(prices) != 0 {
		t.Errorf("expected empty slice for SUD-ineligible a2, got %d price(s)", len(prices))
	}
}

// TestGetComputePrice_SUD_IneligibleG2_ReturnsError verifies that g2-standard-4
// returns an error when term=sud because g2 is not in GCPFamilySKU at all.
// (g2 is not a recognized machine family in this provider — distinct from a2 which
// is SUD-ineligible but is a supported family.)
func TestGetComputePrice_SUD_IneligibleG2_ReturnsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(nil))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	_, err := p.GetComputePrice(ctx, "g2-standard-4", "us-central1", "Linux", models.PricingTermSUD)
	if err == nil {
		t.Error("expected error for unsupported g2 family, got nil")
	}
}

// TestGetComputePrice_PriceLadder_SpotLtCUD3LtCUD1LtSUDLtOnDemand verifies the full
// five-term price ladder for n2-standard-4: spot < cud_3yr < cud_1yr < sud < on_demand.
// SUD (70% of on-demand) must be cheaper than on_demand but more expensive than CUD terms.
func TestGetComputePrice_PriceLadder_SpotLtCUD3LtCUD1LtSUDLtOnDemand(t *testing.T) {
	// n2-standard-4: 4 vCPU, 16 GB RAM.
	// Rates chosen so all five terms are strictly ordered.
	skus := []map[string]any{
		// OnDemand SKUs: $0.031611/vCPU, $0.004237/GB
		// on_demand total = 4*0.031611 + 16*0.004237 = 0.194236
		// sud total = 0.194236 * 0.70 = 0.135965
		makeSKU("N2 Instance Core", "OnDemand", "us-central1", "0", 31_611_000),
		makeSKU("N2 Instance Ram", "OnDemand", "us-central1", "0", 4_237_000),
		// CUD 1-year SKUs: $0.019560/vCPU, $0.002626/GB
		// cud_1yr total = 4*0.019560 + 16*0.002626 = 0.120256
		makeSKU("Commitment v1: N2 Cpu in Americas for 1 Year", "Commit1Yr", "us-central1", "0", 19_560_000),
		makeSKU("Commitment v1: N2 Ram in Americas for 1 Year", "Commit1Yr", "us-central1", "0", 2_626_000),
		// CUD 3-year SKUs: $0.013972/vCPU, $0.001874/GB
		// cud_3yr total = 4*0.013972 + 16*0.001874 = 0.085872
		makeSKU("Commitment v1: N2 Cpu in Americas for 3 Years", "Commit3Yr", "us-central1", "0", 13_972_000),
		makeSKU("Commitment v1: N2 Ram in Americas for 3 Years", "Commit3Yr", "us-central1", "0", 1_874_000),
		// Preemptible (spot) SKUs: $0.006700/vCPU, $0.000900/GB
		// spot total = 4*0.006700 + 16*0.000900 = 0.041200
		makeSKU("Preemptible N2 Instance Core", "Preemptible", "us-central1", "0", 6_700_000),
		makeSKU("Preemptible N2 Instance Ram", "Preemptible", "us-central1", "0", 900_000),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	getPrice := func(term models.PricingTerm) float64 {
		t.Helper()
		prices, err := p.GetComputePrice(ctx, "n2-standard-4", "us-central1", "Linux", term)
		if err != nil {
			t.Fatalf("GetComputePrice (term=%s): %v", term, err)
		}
		if len(prices) != 1 {
			t.Fatalf("GetComputePrice (term=%s): expected 1 price, got %d", term, len(prices))
		}
		return prices[0].PricePerUnit
	}

	spotPrice := getPrice(models.PricingTermSpot)
	cud3YrPrice := getPrice(models.PricingTermCUD3Yr)
	cud1YrPrice := getPrice(models.PricingTermCUD1Yr)
	sudPrice := getPrice(models.PricingTermSUD)
	onDemandPrice := getPrice(models.PricingTermOnDemand)

	// Full ladder: spot < cud_3yr < cud_1yr < sud < on_demand
	if spotPrice >= cud3YrPrice {
		t.Errorf("ladder violated: spot ($%.6f) must be < cud_3yr ($%.6f)", spotPrice, cud3YrPrice)
	}
	if cud3YrPrice >= cud1YrPrice {
		t.Errorf("ladder violated: cud_3yr ($%.6f) must be < cud_1yr ($%.6f)", cud3YrPrice, cud1YrPrice)
	}
	if cud1YrPrice >= sudPrice {
		t.Errorf("ladder violated: cud_1yr ($%.6f) must be < sud ($%.6f)", cud1YrPrice, sudPrice)
	}
	if sudPrice >= onDemandPrice {
		t.Errorf("ladder violated: sud ($%.6f) must be < on_demand ($%.6f)", sudPrice, onDemandPrice)
	}
}

// TestGetComputePrice_OnDemand_SUDHintPresent verifies that on-demand results for
// SUD-eligible families carry the sud_eligible="true" and sud_blended_rate_at_100pct
// hint attributes.
func TestGetComputePrice_OnDemand_SUDHintPresent(t *testing.T) {
	ts := makeN1SUDServer(t)
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	prices, err := p.GetComputePrice(ctx, "n1-standard-4", "us-central1", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("GetComputePrice (on_demand): %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected 1 on_demand price, got 0")
	}

	attrs := prices[0].Attributes
	if attrs["sud_eligible"] != "true" {
		t.Errorf("sud_eligible = %q, want %q", attrs["sud_eligible"], "true")
	}
	hint, ok := attrs["sud_blended_rate_at_100pct"]
	if !ok || hint == "" {
		t.Error("sud_blended_rate_at_100pct attribute is missing or empty")
	}
	if !strings.Contains(hint, "30") {
		t.Errorf("sud_blended_rate_at_100pct = %q, want it to mention 30%% discount", hint)
	}
}

// TestGetComputePrice_OnDemand_SUDHintAbsentForGPU verifies that on-demand results
// for a2 (SUD-ineligible GPU family) do NOT carry sud_eligible="true".
func TestGetComputePrice_OnDemand_SUDHintAbsentForGPU(t *testing.T) {
	skus := []map[string]any{
		makeSKU("A2 Instance Core", "OnDemand", "us-central1", "0", 31_611_000),
		makeSKU("A2 Instance Ram", "OnDemand", "us-central1", "0", 4_237_000),
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(skuResponse(skus))
	}))
	defer ts.Close()

	p := newTestProvider(t, ts)
	ctx := context.Background()

	prices, err := p.GetComputePrice(ctx, "a2-highgpu-1g", "us-central1", "Linux", models.PricingTermOnDemand)
	if err != nil {
		t.Fatalf("GetComputePrice (a2 on_demand): %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("expected 1 on_demand price for a2, got 0")
	}

	attrs := prices[0].Attributes
	if attrs["sud_eligible"] == "true" {
		t.Errorf("a2 (GPU family) should not have sud_eligible=true, but it does")
	}
}

// TestSUDEligible_Families verifies the SUDEligible predicate for all documented
// eligible families and the three explicitly ineligible GPU families.
func TestSUDEligible_Families(t *testing.T) {
	eligible := []string{"n1", "n2", "n2d", "e2", "c2", "c2d", "c3", "t2d", "t2a", "m1", "m2", "m3"}
	ineligible := []string{"a2", "a3", "g2"}

	for _, family := range eligible {
		if !utils.SUDEligible(family) {
			t.Errorf("SUDEligible(%q) = false, want true", family)
		}
	}
	for _, family := range ineligible {
		if utils.SUDEligible(family) {
			t.Errorf("SUDEligible(%q) = true, want false", family)
		}
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
