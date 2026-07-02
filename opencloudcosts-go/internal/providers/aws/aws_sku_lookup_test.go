package aws

// aws_sku_lookup_test.go tests get_price_by_sku's core logic: the
// prefix-stripping/canonicalization function, the service-inference
// heuristic, and the end-to-end per-region lookup orchestration
// (LookupSKUAcrossRegions / lookupSKUInRegion), including the process-lifetime
// catalog memoization cache.
//
// HTTP mocking follows the existing pattern in aws_bulk_test.go: an
// httptest.Server whose URL replaces the package-level bulkPricingBaseURL for
// the duration of a test (overrideBulkBaseURL), serving a bulk-JSON fixture
// shaped exactly like the real
// https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/{service}/current/{region}/index.json
// files.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// resetSKUCatalogCache clears the process-lifetime (service,region) catalog
// memoization cache between tests. Without this, a cache entry populated by
// one test would leak into a later test pointing bulkPricingBaseURL at a
// different mock server (the cache key is "service|region" only — it does
// not include the base URL), causing stale reads or a memoization test that
// starts with an already-warm cache.
func resetSKUCatalogCache(t *testing.T) {
	t.Helper()
	skuCatalogCache.mu.Lock()
	skuCatalogCache.entries = make(map[string]*skuCatalogEntry)
	skuCatalogCache.mu.Unlock()
}

// --------------------------------------------------------------------------
// stripUsageTypePrefix
// --------------------------------------------------------------------------

func TestStripUsageTypePrefix(t *testing.T) {
	tests := []struct {
		name         string
		usageType    string
		wantPrefix   string
		wantSuffix   string
		wantCompound bool
	}{
		{
			name:         "us-east-1 no prefix",
			usageType:    "BoxUsage:r6id.24xlarge",
			wantPrefix:   "",
			wantSuffix:   "BoxUsage:r6id.24xlarge",
			wantCompound: false,
		},
		{
			name:         "eu-west-1 literal EU exception",
			usageType:    "EU-BoxUsage:c8g.xlarge",
			wantPrefix:   "EU",
			wantSuffix:   "BoxUsage:c8g.xlarge",
			wantCompound: false,
		},
		{
			name:         "ca-central-1 CAN1 prefix",
			usageType:    "CAN1-BoxUsage:c6in.xlarge",
			wantPrefix:   "CAN1",
			wantSuffix:   "BoxUsage:c6in.xlarge",
			wantCompound: false,
		},
		{
			name:         "us-west-2 USW2 prefix, LCUUsage",
			usageType:    "USW2-LCUUsage",
			wantPrefix:   "USW2",
			wantSuffix:   "LCUUsage",
			wantCompound: false,
		},
		{
			name:         "ap-south-2 APS5 prefix (2-letter+1-digit, double-digit index)",
			usageType:    "APS5-LCUUsage",
			wantPrefix:   "APS5",
			wantSuffix:   "LCUUsage",
			wantCompound: false,
		},
		{
			name:         "IA modifier is not stripped as a second prefix",
			usageType:    "CAN1-IA-ReadCapacityUnit-Hrs",
			wantPrefix:   "CAN1",
			wantSuffix:   "IA-ReadCapacityUnit-Hrs",
			wantCompound: false,
		},
		{
			name:         "compound inter-region data-transfer SKU is flagged",
			usageType:    "CAN1-DEN1-AWS-Out-Bytes",
			wantPrefix:   "CAN1",
			wantSuffix:   "DEN1-AWS-Out-Bytes",
			wantCompound: true,
		},
		{
			name:         "compound wavelength triple-token data-transfer SKU is flagged",
			usageType:    "USE1WL1ATL1-CAN1-AWS-Out-Bytes",
			wantPrefix:   "USE1WL1ATL1",
			wantSuffix:   "CAN1-AWS-Out-Bytes",
			wantCompound: true,
		},
		{
			name:         "simple single-prefix egress SKU is NOT flagged as compound",
			usageType:    "CAN1-AWS-Out-Bytes",
			wantPrefix:   "CAN1",
			wantSuffix:   "AWS-Out-Bytes",
			wantCompound: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prefix, suffix, compound := stripUsageTypePrefix(tc.usageType)
			if prefix != tc.wantPrefix {
				t.Errorf("prefix = %q, want %q", prefix, tc.wantPrefix)
			}
			if suffix != tc.wantSuffix {
				t.Errorf("suffix = %q, want %q", suffix, tc.wantSuffix)
			}
			if compound != tc.wantCompound {
				t.Errorf("looksLikeCompoundTransfer = %v, want %v", compound, tc.wantCompound)
			}
		})
	}
}

// TestStripUsageTypePrefix_CrossRegionSuffixesMatch confirms two different
// regions' full usage-type strings for the same logical operation produce
// byte-equal suffixes — the core invariant the whole suffix-matching design
// depends on.
func TestStripUsageTypePrefix_CrossRegionSuffixesMatch(t *testing.T) {
	_, suffix1, _ := stripUsageTypePrefix("CAN1-EBS:VolumeUsage.gp3")
	_, suffix2, _ := stripUsageTypePrefix("APS1-EBS:VolumeUsage.gp3")
	if suffix1 != suffix2 {
		t.Fatalf("expected equal suffixes across regions, got %q vs %q", suffix1, suffix2)
	}
	if suffix1 != "EBS:VolumeUsage.gp3" {
		t.Fatalf("unexpected suffix value: %q", suffix1)
	}
}

// --------------------------------------------------------------------------
// inferServiceFromUsageType
// --------------------------------------------------------------------------

func TestInferServiceFromUsageType(t *testing.T) {
	tests := []struct {
		name        string
		usageType   string
		wantService string
		wantOK      bool
	}{
		{"EC2 BoxUsage", "CAN1-BoxUsage:r5a.8xlarge", "AmazonEC2", true},
		{"EC2 EBS volume usage", "CAN1-EBS:VolumeUsage.gp3", "AmazonEC2", true},
		{"ELB LCUUsage", "CAN1-LCUUsage", "AWSELB", true},
		{"RDS InstanceUsage:db.", "CAN1-InstanceUsage:db.r8g.2xl", "AmazonRDS", true},
		{"RDS InstanceUsageIOOptimized:db.", "CAN1-InstanceUsageIOOptimized:db.r5.xl", "AmazonRDS", true},
		{"DynamoDB WriteRequestUnits", "CAN1-WriteRequestUnits", "AmazonDynamoDB", true},
		{"ElastiCache NodeUsage:cache.", "CAN1-NodeUsage:cache.m6g.2xlarge", "AmazonElastiCache", true},
		{"DataTransfer AWS-Out-Bytes", "CAN1-AWS-Out-Bytes", "AWSDataTransfer", true},
		{"unrecognizable usage-type cannot be inferred", "CAN1-SomeMadeUpUsageTypeXYZ123", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, ok := inferServiceFromUsageType(tc.usageType)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (svc=%q)", ok, tc.wantOK, svc)
			}
			if ok && svc != tc.wantService {
				t.Errorf("service = %q, want %q", svc, tc.wantService)
			}
		})
	}
}

// --------------------------------------------------------------------------
// End-to-end per-region lookup / LookupSKUAcrossRegions
// --------------------------------------------------------------------------

// skuBulkJSON builds a minimal single-product AWS bulk offer-file fixture
// whose product carries a "usagetype" attribute (required — fetchSKUCatalog
// skips any row with usagetype == "") and a non-zero OnDemand USD price
// (required — skuToNormalizedPrice returns nil for a zero price).
func skuBulkJSON(sku, usageType, location, priceUSD string) string {
	return fmt.Sprintf(
		`{"products":{%q:{"sku":%q,"productFamily":"Compute Instance","attributes":{"usagetype":%q,"instanceType":"r6id.24xlarge","location":%q,"operatingSystem":"Linux","tenancy":"Shared","capacitystatus":"Used"}}},`+
			`"terms":{"OnDemand":{%q:{%q:{"offerTermCode":"JRTCKXETXF","priceDimensions":{%q:{"unit":"Hrs","pricePerUnit":{"USD":%q}}}}}},"Reserved":{}}}`,
		sku, sku, usageType, location,
		sku, sku+".JRTCKXETXF",
		sku+".JRTCKXETXF.6YS6EN2CT7", priceUSD,
	)
}

// skuBulkJSONEmpty is a fixture with zero matching products (empty products
// map, still well-formed JSON) — used for the "no mapping found" case.
const skuBulkJSONEmptyProducts = `{"products":{},"terms":{"OnDemand":{},"Reserved":{}}}`

func TestLookupSKUInRegion_Match(t *testing.T) {
	resetSKUCatalogCache(t)
	body := skuBulkJSON("SKU1", "BoxUsage:r6id.24xlarge", "US East (N. Virginia)", "0.5000000000")
	server := newBulkTestServer(t, []byte(body), map[string]string{"Content-Type": "application/json"}, http.StatusOK)
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}
	result, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "BoxUsage:r6id.24xlarge", "AmazonEC2", []string{"us-east-1"})
	if err != nil {
		t.Fatalf("LookupSKUAcrossRegions returned error: %v", err)
	}
	if len(result.Regions) != 1 {
		t.Fatalf("expected 1 region result, got %d", len(result.Regions))
	}
	rr := result.Regions[0]
	if rr.Error != "" {
		t.Fatalf("unexpected error: %s", rr.Error)
	}
	if rr.NoMapping {
		t.Fatalf("expected a match, got NoMapping=true")
	}
	if len(rr.Prices) != 1 {
		t.Fatalf("expected 1 price, got %d", len(rr.Prices))
	}
	if rr.Prices[0].PricePerUnit != 0.5 {
		t.Errorf("expected price 0.5, got %v", rr.Prices[0].PricePerUnit)
	}
	if rr.ServiceUsed != "AmazonEC2" {
		t.Errorf("expected ServiceUsed=AmazonEC2, got %q", rr.ServiceUsed)
	}
}

func TestLookupSKUInRegion_NoMapping(t *testing.T) {
	resetSKUCatalogCache(t)
	// Catalog fetches successfully but contains a usage-type that does not
	// match the requested SKU's suffix.
	body := skuBulkJSON("SKU1", "BoxUsage:m5.xlarge", "US East (N. Virginia)", "0.2000000000")
	server := newBulkTestServer(t, []byte(body), map[string]string{"Content-Type": "application/json"}, http.StatusOK)
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}
	result, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "BoxUsage:r6id.24xlarge", "AmazonEC2", []string{"us-east-1"})
	if err != nil {
		t.Fatalf("LookupSKUAcrossRegions returned error: %v", err)
	}
	rr := result.Regions[0]
	if rr.Error != "" {
		t.Fatalf("expected no error (catalog fetch succeeded), got: %s", rr.Error)
	}
	if len(rr.Prices) != 0 {
		t.Fatalf("expected no prices, got %d", len(rr.Prices))
	}
	if !rr.NoMapping {
		t.Fatalf("expected NoMapping=true (checked, not found), got false")
	}
}

func TestLookupSKUInRegion_EmptyProductsAlsoNoMapping(t *testing.T) {
	resetSKUCatalogCache(t)
	server := newBulkTestServer(t, []byte(skuBulkJSONEmptyProducts), map[string]string{"Content-Type": "application/json"}, http.StatusOK)
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}
	result, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "BoxUsage:r6id.24xlarge", "AmazonEC2", []string{"us-east-1"})
	if err != nil {
		t.Fatalf("LookupSKUAcrossRegions returned error: %v", err)
	}
	rr := result.Regions[0]
	if !rr.NoMapping || rr.Error != "" || len(rr.Prices) != 0 {
		t.Fatalf("expected clean NoMapping result, got %+v", rr)
	}
}

func TestLookupSKUInRegion_FetchFailureIsError(t *testing.T) {
	resetSKUCatalogCache(t)
	// A 500 status makes getProductsBulk return an error for every candidate
	// service — this must surface as rr.Error, not rr.NoMapping, since we
	// never actually got to check the catalog.
	server := newBulkTestServer(t, []byte(`{}`), nil, http.StatusInternalServerError)
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}
	result, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "BoxUsage:r6id.24xlarge", "AmazonEC2", []string{"us-east-1"})
	if err != nil {
		t.Fatalf("LookupSKUAcrossRegions returned error: %v", err)
	}
	rr := result.Regions[0]
	if rr.Error == "" {
		t.Fatalf("expected a non-empty Error, got NoMapping=%v Prices=%v", rr.NoMapping, rr.Prices)
	}
	if rr.NoMapping {
		t.Fatalf("a fetch failure must not be reported as NoMapping")
	}
}

// TestLookupSKUAcrossRegions_PerRegionValidation_NonFatal verifies that a
// malformed/unsupported region is rejected per-region (surfaced as that
// region's Error), without aborting the whole multi-region request.
func TestLookupSKUAcrossRegions_PerRegionValidation_NonFatal(t *testing.T) {
	resetSKUCatalogCache(t)
	body := skuBulkJSON("SKU1", "BoxUsage:r6id.24xlarge", "US East (N. Virginia)", "0.5000000000")
	server := newBulkTestServer(t, []byte(body), map[string]string{"Content-Type": "application/json"}, http.StatusOK)
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}
	result, err := p.LookupSKUAcrossRegions(
		context.Background(), "aws", "BoxUsage:r6id.24xlarge", "AmazonEC2",
		[]string{"us-east-1", "cn-north-1", "not-a-region"},
	)
	if err != nil {
		t.Fatalf("LookupSKUAcrossRegions returned error: %v", err)
	}
	if len(result.Regions) != 3 {
		t.Fatalf("expected 3 region results (order-preserving), got %d", len(result.Regions))
	}

	good, cn, bad := result.Regions[0], result.Regions[1], result.Regions[2]
	if good.Region != "us-east-1" || len(good.Prices) == 0 {
		t.Errorf("expected us-east-1 to match, got %+v", good)
	}
	if cn.Region != "cn-north-1" || cn.Error == "" {
		t.Errorf("expected cn-north-1 (China partition) to be rejected with an error, got %+v", cn)
	}
	if bad.Region != "not-a-region" || bad.Error == "" {
		t.Errorf("expected malformed region to be rejected with an error, got %+v", bad)
	}
}

// TestLookupSKUAcrossRegions_EmptyRegionsRejected verifies request-level
// rejection happens before any network call — the mock server below is
// intentionally never hit (bulkPricingBaseURL is left pointing at an
// unroutable placeholder to make an accidental network attempt fail loudly
// rather than silently succeed against the real AWS endpoint).
func TestLookupSKUAcrossRegions_EmptyRegionsRejected(t *testing.T) {
	resetSKUCatalogCache(t)
	overrideBulkBaseURL(t, "http://127.0.0.1:1/unreachable")

	p := &Provider{}
	_, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "BoxUsage:r6id.24xlarge", "AmazonEC2", nil)
	if err == nil {
		t.Fatal("expected an error for empty regions, got nil")
	}
	skuErr, ok := err.(*SKULookupError)
	if !ok {
		t.Fatalf("expected *SKULookupError, got %T: %v", err, err)
	}
	if skuErr.Code != SKUErrRegionsRequired {
		t.Errorf("expected code %q, got %q", SKUErrRegionsRequired, skuErr.Code)
	}
}

// TestLookupSKUAcrossRegions_TooManyRegionsRejected verifies the >30 regions
// cap is enforced before any network call.
func TestLookupSKUAcrossRegions_TooManyRegionsRejected(t *testing.T) {
	resetSKUCatalogCache(t)
	overrideBulkBaseURL(t, "http://127.0.0.1:1/unreachable")

	regions := make([]string, 31)
	for i := range regions {
		regions[i] = "us-east-1"
	}

	p := &Provider{}
	_, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "BoxUsage:r6id.24xlarge", "AmazonEC2", regions)
	if err == nil {
		t.Fatal("expected an error for >30 regions, got nil")
	}
	skuErr, ok := err.(*SKULookupError)
	if !ok {
		t.Fatalf("expected *SKULookupError, got %T: %v", err, err)
	}
	if skuErr.Code != SKUErrTooManyRegions {
		t.Errorf("expected code %q, got %q", SKUErrTooManyRegions, skuErr.Code)
	}
}

// TestLookupSKUAcrossRegions_WrongProviderRejected verifies provider != "aws"
// is rejected with a clear, structured error, before any network call.
func TestLookupSKUAcrossRegions_WrongProviderRejected(t *testing.T) {
	resetSKUCatalogCache(t)
	overrideBulkBaseURL(t, "http://127.0.0.1:1/unreachable")

	p := &Provider{}
	_, err := p.LookupSKUAcrossRegions(context.Background(), "gcp", "BoxUsage:r6id.24xlarge", "AmazonEC2", []string{"us-east-1"})
	if err == nil {
		t.Fatal("expected an error for provider=gcp, got nil")
	}
	skuErr, ok := err.(*SKULookupError)
	if !ok {
		t.Fatalf("expected *SKULookupError, got %T: %v", err, err)
	}
	if skuErr.Code != SKUErrUnsupportedProvider {
		t.Errorf("expected code %q, got %q", SKUErrUnsupportedProvider, skuErr.Code)
	}
}

// TestLookupSKUAcrossRegions_ExplicitHintFailsThenInferredFallbackMatches
// covers the mismatch-tolerant fallback: the caller's explicit service hint
// (AmazonEC2, matching the real-world "AWS Product" column for this usage
// type) has no matching row, but the inferred service (AWSDataTransfer, the
// verified real servicecode) does — the result must report a match with
// ServiceMismatch=true rather than silently failing under the wrong hint.
func TestLookupSKUAcrossRegions_ExplicitHintFailsThenInferredFallbackMatches(t *testing.T) {
	resetSKUCatalogCache(t)

	mux := http.NewServeMux()
	// AmazonEC2 catalog: no AWS-Out-Bytes row at all -> the hint's candidate
	// catalog fetches fine but has no match for this suffix.
	mux.HandleFunc("/AmazonEC2/current/us-east-1/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(skuBulkJSON("SKU1", "BoxUsage:m5.xlarge", "US East (N. Virginia)", "0.2000000000")))
	})
	// AWSDataTransfer catalog: has the matching row.
	mux.HandleFunc("/AWSDataTransfer/current/us-east-1/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(skuBulkJSON("SKU2", "AWS-Out-Bytes", "US East (N. Virginia)", "0.0900000000")))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}
	result, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "AWS-Out-Bytes", "AmazonEC2", []string{"us-east-1"})
	if err != nil {
		t.Fatalf("LookupSKUAcrossRegions returned error: %v", err)
	}
	if result.ServiceSource != "explicit" {
		t.Errorf("expected service_source=explicit, got %q", result.ServiceSource)
	}
	if result.InferredService != "AWSDataTransfer" {
		t.Errorf("expected inferred_service=AWSDataTransfer, got %q", result.InferredService)
	}

	rr := result.Regions[0]
	if len(rr.Prices) != 1 {
		t.Fatalf("expected a match via the inferred fallback, got %+v", rr)
	}
	if rr.ServiceUsed != "AWSDataTransfer" {
		t.Errorf("expected ServiceUsed=AWSDataTransfer, got %q", rr.ServiceUsed)
	}
	if !rr.ServiceMismatch {
		t.Errorf("expected ServiceMismatch=true (hint AmazonEC2 didn't match, fallback did)")
	}
}

// TestLookupSKUAcrossRegions_CompoundTransferWarning verifies both compound
// fixture SKUs from the ground-truth data produce the compound-transfer
// warning text.
func TestLookupSKUAcrossRegions_CompoundTransferWarning(t *testing.T) {
	for _, sku := range []string{"CAN1-DEN1-AWS-Out-Bytes", "USE1WL1ATL1-CAN1-AWS-Out-Bytes"} {
		t.Run(sku, func(t *testing.T) {
			resetSKUCatalogCache(t)
			server := newBulkTestServer(t, []byte(skuBulkJSONEmptyProducts), map[string]string{"Content-Type": "application/json"}, http.StatusOK)
			defer server.Close()
			overrideBulkBaseURL(t, server.URL)

			p := &Provider{}
			result, err := p.LookupSKUAcrossRegions(context.Background(), "aws", sku, "AWSDataTransfer", []string{"us-east-1"})
			if err != nil {
				t.Fatalf("LookupSKUAcrossRegions returned error: %v", err)
			}
			found := false
			for _, w := range result.Warnings {
				if containsCompoundWarning(w) {
					found = true
				}
			}
			if !found {
				t.Errorf("expected a compound/wavelength warning, got warnings=%v", result.Warnings)
			}
		})
	}
}

func containsCompoundWarning(s string) bool {
	return len(s) > 0 && (indexOfSubstr(s, "multi-region") >= 0 || indexOfSubstr(s, "wavelength") >= 0)
}

// indexOfSubstr is a tiny local helper so this test file doesn't need to pull
// in strings.Contains under a different name; kept trivial on purpose.
func indexOfSubstr(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// TestLookupSKUAcrossRegions_ServiceUndeterminable verifies that when no
// service hint is given and the usage-type pattern cannot be inferred, the
// function returns a structured error rather than guessing.
func TestLookupSKUAcrossRegions_ServiceUndeterminable(t *testing.T) {
	resetSKUCatalogCache(t)
	overrideBulkBaseURL(t, "http://127.0.0.1:1/unreachable")

	p := &Provider{}
	_, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "SomeMadeUpUsageTypeXYZ123", "", []string{"us-east-1"})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	skuErr, ok := err.(*SKULookupError)
	if !ok {
		t.Fatalf("expected *SKULookupError, got %T: %v", err, err)
	}
	if skuErr.Code != SKUErrServiceUndeterminable {
		t.Errorf("expected code %q, got %q", SKUErrServiceUndeterminable, skuErr.Code)
	}
}

// TestGetOrFetchSKUCatalog_MemoizesAcrossCalls verifies that two lookups
// against the same (service, region) — even for different input SKUs — only
// fetch the underlying offer file once, via a request counter in the mock
// HTTP handler. atomic.Int32 is used because the region fan-out inside
// LookupSKUAcrossRegions calls this from multiple goroutines, and this test
// runs under -race.
func TestGetOrFetchSKUCatalog_MemoizesAcrossCalls(t *testing.T) {
	resetSKUCatalogCache(t)

	var requestCount atomic.Int32
	body := skuBulkJSON("SKU1", "BoxUsage:r6id.24xlarge", "US East (N. Virginia)", "0.5000000000")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}

	// First lookup: SKU with a prefix that strips to the fixture's suffix.
	_, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "BoxUsage:r6id.24xlarge", "AmazonEC2", []string{"us-east-1"})
	if err != nil {
		t.Fatalf("first LookupSKUAcrossRegions returned error: %v", err)
	}
	// Second lookup: a different SKU string, same (service, region) pair —
	// this one has no matching row (NoMapping), but must still reuse the
	// memoized catalog rather than re-fetching.
	_, err = p.LookupSKUAcrossRegions(context.Background(), "aws", "BoxUsage:completely-different.xlarge", "AmazonEC2", []string{"us-east-1"})
	if err != nil {
		t.Fatalf("second LookupSKUAcrossRegions returned error: %v", err)
	}

	if got := requestCount.Load(); got != 1 {
		t.Errorf("expected exactly 1 underlying HTTP fetch (memoized), got %d", got)
	}
}

// --------------------------------------------------------------------------
// Multi-row-per-suffix disambiguation (resolveSKUCandidates)
// --------------------------------------------------------------------------

// multiProductSpec describes one product row within a multi-row bulk
// offer-file fixture (skuBulkJSONMulti below): several rows can share the
// same usageType (and therefore the same stripped suffix) while differing in
// their other attributes and price — exactly the shape that exercises
// resolveSKUCandidates.
type multiProductSpec struct {
	sku        string
	usageType  string
	location   string
	priceUSD   string
	extraAttrs map[string]string
}

// skuBulkJSONMulti builds a multi-product AWS bulk offer-file fixture from
// specs, each becoming one product row plus one OnDemand price term. Unlike
// skuBulkJSON (a single, fixed-attribute product), this lets a test
// construct several rows that collide on stripped usage-type suffix but
// differ in other attributes/price, to exercise the multi-row
// disambiguation path.
func skuBulkJSONMulti(specs []multiProductSpec) string {
	products := make(map[string]any, len(specs))
	onDemand := make(map[string]any, len(specs))
	for _, s := range specs {
		attrs := map[string]string{
			"usagetype":    s.usageType,
			"instanceType": "r6id.24xlarge",
			"location":     s.location,
		}
		for k, v := range s.extraAttrs {
			attrs[k] = v
		}
		products[s.sku] = map[string]any{
			"sku":           s.sku,
			"productFamily": "Compute Instance",
			"attributes":    attrs,
		}
		termCode := s.sku + ".JRTCKXETXF"
		rateCode := termCode + ".6YS6EN2CT7"
		onDemand[s.sku] = map[string]any{
			termCode: map[string]any{
				"offerTermCode": "JRTCKXETXF",
				"priceDimensions": map[string]any{
					rateCode: map[string]any{
						"unit":         "Hrs",
						"pricePerUnit": map[string]string{"USD": s.priceUSD},
					},
				},
			},
		}
	}
	doc := map[string]any{
		"products": products,
		"terms": map[string]any{
			"OnDemand": onDemand,
			"Reserved": map[string]any{},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err) // test-only helper; a marshal failure here is a test bug
	}
	return string(b)
}

// TestLookupSKUInRegion_MultiRowResolvesToCanonicalDefault verifies the
// critical-finding fix: when a usage-type suffix matches multiple product
// rows that differ only in operatingSystem/tenancy, the Linux/Shared
// canonical-default row is chosen — even though it is deliberately the MOST
// expensive of the three candidate rows here. This is the test that
// distinguishes the fix from the old "pick the cheapest of the bucket"
// behavior: if the assertion below passed merely because Linux/Shared
// happened to be cheapest, it would not prove anything.
func TestLookupSKUInRegion_MultiRowResolvesToCanonicalDefault(t *testing.T) {
	resetSKUCatalogCache(t)
	body := skuBulkJSONMulti([]multiProductSpec{
		{
			sku: "SKU-DEDICATED", usageType: "BoxUsage:r6id.24xlarge", location: "US East (N. Virginia)",
			priceUSD: "0.1000000000", // cheapest overall, but Dedicated tenancy — must NOT be silently chosen
			extraAttrs: map[string]string{
				"operatingSystem": "Linux", "tenancy": "Dedicated", "preInstalledSw": "NA", "capacitystatus": "Used",
			},
		},
		{
			sku: "SKU-WINDOWS", usageType: "BoxUsage:r6id.24xlarge", location: "US East (N. Virginia)",
			priceUSD: "0.3000000000", // cheaper than the canonical row, but Windows — must NOT be silently chosen
			extraAttrs: map[string]string{
				"operatingSystem": "Windows", "tenancy": "Shared", "preInstalledSw": "NA", "capacitystatus": "Used",
			},
		},
		{
			sku: "SKU-LINUX-SHARED", usageType: "BoxUsage:r6id.24xlarge", location: "US East (N. Virginia)",
			priceUSD: "0.9000000000", // the canonical Linux/Shared row — deliberately the MOST expensive
			extraAttrs: map[string]string{
				"operatingSystem": "Linux", "tenancy": "Shared", "preInstalledSw": "NA", "capacitystatus": "Used",
			},
		},
	})
	server := newBulkTestServer(t, []byte(body), map[string]string{"Content-Type": "application/json"}, http.StatusOK)
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}
	result, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "BoxUsage:r6id.24xlarge", "AmazonEC2", []string{"us-east-1"})
	if err != nil {
		t.Fatalf("LookupSKUAcrossRegions returned error: %v", err)
	}
	rr := result.Regions[0]
	if rr.Ambiguous {
		t.Fatalf("expected canonical-default narrowing to resolve this unambiguously, got Ambiguous=true, prices=%+v", rr.Prices)
	}
	if len(rr.Prices) != 1 {
		t.Fatalf("expected exactly 1 resolved price, got %d: %+v", len(rr.Prices), rr.Prices)
	}
	got := rr.Prices[0]
	if got.PricePerUnit != 0.9 {
		t.Errorf("expected the Linux/Shared canonical-default row (0.9) to be chosen despite not being "+
			"cheapest, got price=%v attrs=%v", got.PricePerUnit, got.Attributes)
	}
}

// TestLookupSKUInRegion_MultiRowGenuinelyAmbiguousIsFlagged verifies that a
// suffix collision the canonical-default attributes cannot resolve (RDS rows
// differing only by databaseEngine, which no usage-type string encodes) is
// surfaced as Ambiguous=true with every candidate row kept, rather than
// silently picking one.
func TestLookupSKUInRegion_MultiRowGenuinelyAmbiguousIsFlagged(t *testing.T) {
	resetSKUCatalogCache(t)
	body := skuBulkJSONMulti([]multiProductSpec{
		{
			sku: "SKU-MYSQL", usageType: "InstanceUsage:db.r5.large", location: "US East (N. Virginia)",
			priceUSD:   "0.5000000000",
			extraAttrs: map[string]string{"databaseEngine": "MySQL", "deploymentOption": "Single-AZ"},
		},
		{
			sku: "SKU-POSTGRES", usageType: "InstanceUsage:db.r5.large", location: "US East (N. Virginia)",
			priceUSD:   "0.4000000000",
			extraAttrs: map[string]string{"databaseEngine": "PostgreSQL", "deploymentOption": "Single-AZ"},
		},
	})
	server := newBulkTestServer(t, []byte(body), map[string]string{"Content-Type": "application/json"}, http.StatusOK)
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}
	result, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "InstanceUsage:db.r5.large", "AmazonRDS", []string{"us-east-1"})
	if err != nil {
		t.Fatalf("LookupSKUAcrossRegions returned error: %v", err)
	}
	rr := result.Regions[0]
	if !rr.Ambiguous {
		t.Fatalf("expected the genuinely-ambiguous RDS engine collision to be flagged Ambiguous=true, got %+v", rr)
	}
	if len(rr.Prices) != 2 {
		t.Fatalf("expected both candidate rows to be kept when ambiguous, got %d: %+v", len(rr.Prices), rr.Prices)
	}
}

// TestLookupSKUInRegion_SingleRowNeverFlaggedAmbiguous is a guard against
// resolveSKUCandidates regressing the overwhelmingly common single-match
// case (the existing TestLookupSKUInRegion_Match already covers this
// end-to-end; this asserts the Ambiguous field specifically).
func TestLookupSKUInRegion_SingleRowNeverFlaggedAmbiguous(t *testing.T) {
	resetSKUCatalogCache(t)
	body := skuBulkJSON("SKU1", "BoxUsage:r6id.24xlarge", "US East (N. Virginia)", "0.5000000000")
	server := newBulkTestServer(t, []byte(body), map[string]string{"Content-Type": "application/json"}, http.StatusOK)
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}
	result, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "BoxUsage:r6id.24xlarge", "AmazonEC2", []string{"us-east-1"})
	if err != nil {
		t.Fatalf("LookupSKUAcrossRegions returned error: %v", err)
	}
	if result.Regions[0].Ambiguous {
		t.Errorf("expected a single matching row to never be flagged ambiguous")
	}
}

// --------------------------------------------------------------------------
// Case-insensitive service hint (falls back to canonical-cased inference)
// --------------------------------------------------------------------------

// TestLookupSKUAcrossRegions_CaseInsensitiveHintUsesCanonicalCasing verifies
// that a service hint differing from the inferred servicecode only by
// casing (e.g. "amazonec2" vs "AmazonEC2") is resolved using the
// canonically-cased inferred servicecode, since the bulk-pricing URL path
// this candidate feeds into is case-sensitive.
func TestLookupSKUAcrossRegions_CaseInsensitiveHintUsesCanonicalCasing(t *testing.T) {
	resetSKUCatalogCache(t)
	body := skuBulkJSON("SKU1", "BoxUsage:r6id.24xlarge", "US East (N. Virginia)", "0.5000000000")

	var requestedPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPaths = append(requestedPaths, r.URL.Path)
		if r.URL.Path == "/AmazonEC2/current/us-east-1/index.json" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
			return
		}
		// Any other (wrongly-cased) path 404s, mirroring the real bulk
		// endpoint's case-sensitive URL matching (see aws_bulk.go).
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}
	result, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "BoxUsage:r6id.24xlarge", "amazonec2", []string{"us-east-1"})
	if err != nil {
		t.Fatalf("LookupSKUAcrossRegions returned error: %v", err)
	}
	rr := result.Regions[0]
	if rr.Error != "" {
		t.Fatalf("expected a match via canonical-cased fallback, got error: %s (paths requested: %v)", rr.Error, requestedPaths)
	}
	if len(rr.Prices) != 1 {
		t.Fatalf("expected 1 price, got %d (paths requested: %v)", len(rr.Prices), requestedPaths)
	}
	if rr.ServiceUsed != "AmazonEC2" {
		t.Errorf("expected ServiceUsed=AmazonEC2 (canonical casing), got %q", rr.ServiceUsed)
	}
	if rr.ServiceMismatch {
		t.Errorf("expected ServiceMismatch=false (a casing-only difference is not a real mismatch), got true")
	}
}

// --------------------------------------------------------------------------
// Input-length / region-shape bounds (security review findings)
// --------------------------------------------------------------------------

// TestLookupSKUAcrossRegions_SKUTooLongRejected verifies an oversized sku
// string is rejected before any network call, rather than being processed
// (regex-matched, echoed into error/log strings) unbounded.
func TestLookupSKUAcrossRegions_SKUTooLongRejected(t *testing.T) {
	resetSKUCatalogCache(t)
	overrideBulkBaseURL(t, "http://127.0.0.1:1/unreachable")

	longSKU := strings.Repeat("A", maxSKULength+1)
	p := &Provider{}
	_, err := p.LookupSKUAcrossRegions(context.Background(), "aws", longSKU, "AmazonEC2", []string{"us-east-1"})
	if err == nil {
		t.Fatal("expected an error for an over-length sku, got nil")
	}
	skuErr, ok := err.(*SKULookupError)
	if !ok {
		t.Fatalf("expected *SKULookupError, got %T: %v", err, err)
	}
	if skuErr.Code != SKUErrSKUTooLong {
		t.Errorf("expected code %q, got %q", SKUErrSKUTooLong, skuErr.Code)
	}
}

// TestIsValidAWSRegionCodeShape_RealCodesAccepted is a regression test for
// the region-shape regex's bound-tightening (unbounded [a-z]+ -> [a-z]{2,20})
// against every region-code shape referenced in this feature's ground-truth
// verification data, so the security fix cannot silently start rejecting a
// real AWS region.
func TestIsValidAWSRegionCodeShape_RealCodesAccepted(t *testing.T) {
	valid := []string{
		"us-east-1", "us-west-2", "eu-west-1", "eu-central-1",
		"ap-southeast-1", "ap-southeast-2", "ap-northeast-1",
		"ca-central-1", "sa-east-1", "il-central-1",
		"us-gov-west-1", "us-gov-east-1", "me-south-1", "af-south-1",
	}
	for _, r := range valid {
		if !isValidAWSRegionCodeShape(r) {
			t.Errorf("expected %q to be accepted as a valid AWS region code shape", r)
		}
	}
}

// TestIsValidAWSRegionCodeShape_BoundsGeographySegment verifies the
// previously-unbounded geography segment ([a-z]+) is now bounded — a
// pathologically long segment must be rejected rather than accepted and fed
// into a regex match / URL path segment of unbounded length.
func TestIsValidAWSRegionCodeShape_BoundsGeographySegment(t *testing.T) {
	huge := "us-" + strings.Repeat("a", 10000) + "-1"
	if isValidAWSRegionCodeShape(huge) {
		t.Error("expected a pathologically long geography segment to be rejected")
	}
}

// --------------------------------------------------------------------------
// Catalog cache TTL expiry / size-capped eviction (security + simplification
// review findings)
// --------------------------------------------------------------------------

// TestGetSKUCatalogEntry_TTLExpiryTriggersRefetch verifies that once an
// entry's fetchedAt has aged past its TTL, getSKUCatalogEntry returns a
// fresh (different) entry rather than reusing the stale one forever — the
// core rebuttal to "this cache never expires, unbounded over process
// lifetime".
func TestGetSKUCatalogEntry_TTLExpiryTriggersRefetch(t *testing.T) {
	resetSKUCatalogCache(t)
	const key = "AmazonEC2|us-east-1"
	const ttl = 10 * time.Millisecond

	e1 := getSKUCatalogEntry(key, ttl)
	skuCatalogCache.mu.Lock()
	e1.fetchedAt = time.Now().Add(-2 * ttl) // backdate past the TTL
	skuCatalogCache.mu.Unlock()

	e2 := getSKUCatalogEntry(key, ttl)
	if e2 == e1 {
		t.Fatal("expected a fresh entry once the previous one aged past its TTL, got the same stale entry")
	}
}

// TestGetSKUCatalogEntry_InFlightEntryNeverTreatedAsStale verifies an
// in-flight fetch (fetchedAt still zero) is reused rather than replaced,
// even under a zero/expired TTL — otherwise concurrent lookups for the same
// (service, region) would stampede instead of collapsing into one fetch.
func TestGetSKUCatalogEntry_InFlightEntryNeverTreatedAsStale(t *testing.T) {
	resetSKUCatalogCache(t)
	const key = "AmazonEC2|us-east-1"

	e1 := getSKUCatalogEntry(key, 0) // fetchedAt is still zero (in flight)
	e2 := getSKUCatalogEntry(key, 0)
	if e1 != e2 {
		t.Fatal("expected an in-flight entry to be reused regardless of TTL, got a different entry")
	}
}

// TestSKUCatalogCache_EvictsOldestWhenOverCapacity verifies the hard
// entry-count cap: once the cache holds maxSKUCatalogCacheEntries completed
// entries, adding one more evicts the single oldest completed entry rather
// than growing unbounded.
func TestSKUCatalogCache_EvictsOldestWhenOverCapacity(t *testing.T) {
	resetSKUCatalogCache(t)
	const ttl = time.Hour
	base := time.Now()

	for i := 0; i < maxSKUCatalogCacheEntries; i++ {
		key := fmt.Sprintf("svc|region-%d", i)
		e := getSKUCatalogEntry(key, ttl)
		skuCatalogCache.mu.Lock()
		e.fetchedAt = base.Add(time.Duration(i) * time.Second) // region-0 is oldest
		skuCatalogCache.mu.Unlock()
	}

	getSKUCatalogEntry("svc|region-new", ttl)

	skuCatalogCache.mu.Lock()
	_, oldestStillPresent := skuCatalogCache.entries["svc|region-0"]
	_, newEntryPresent := skuCatalogCache.entries["svc|region-new"]
	count := len(skuCatalogCache.entries)
	skuCatalogCache.mu.Unlock()

	if oldestStillPresent {
		t.Error("expected the oldest completed entry to be evicted once over capacity")
	}
	if !newEntryPresent {
		t.Error("expected the newly-created entry to be present")
	}
	if count != maxSKUCatalogCacheEntries {
		t.Errorf("expected exactly %d entries after eviction, got %d", maxSKUCatalogCacheEntries, count)
	}
}
