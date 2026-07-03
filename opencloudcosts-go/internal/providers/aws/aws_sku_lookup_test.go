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

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
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
		{"DynamoDB ReadCapacityUnit-Hrs", "CAN1-ReadCapacityUnit-Hrs", "AmazonDynamoDB", true},
		// Regression for RC3-008: TimedStorage-ByteHrs is NOT unique to
		// DynamoDB storage -- S3 storage usage types share the identical
		// literal suffix (e.g. "CAN1-TimedStorage-ByteHrs" for S3 Standard
		// storage in ca-central-1), and the two services' storage prices
		// differ by ~11x. This suffix alone must not resolve to any single
		// confident service inference.
		{"ambiguous TimedStorage-ByteHrs (S3 vs DynamoDB)", "CAN1-TimedStorage-ByteHrs", "", false},
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
	result, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "BoxUsage:r6id.24xlarge", "AmazonEC2", []string{"us-east-1"}, "", "")
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
	result, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "BoxUsage:r6id.24xlarge", "AmazonEC2", []string{"us-east-1"}, "", "")
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
	result, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "BoxUsage:r6id.24xlarge", "AmazonEC2", []string{"us-east-1"}, "", "")
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
	result, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "BoxUsage:r6id.24xlarge", "AmazonEC2", []string{"us-east-1"}, "", "")
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
		"", "",
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
	_, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "BoxUsage:r6id.24xlarge", "AmazonEC2", nil, "", "")
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
	_, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "BoxUsage:r6id.24xlarge", "AmazonEC2", regions, "", "")
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
	_, err := p.LookupSKUAcrossRegions(context.Background(), "gcp", "BoxUsage:r6id.24xlarge", "AmazonEC2", []string{"us-east-1"}, "", "")
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
	result, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "AWS-Out-Bytes", "AmazonEC2", []string{"us-east-1"}, "", "")
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

// TestLookupSKUAcrossRegions_HintTriedAgainstFallbackServiceCatalog verifies
// the RC2 review's finding-3 fix: when an explicit service= hint disagrees
// with the heuristically inferred service (candidates = [serviceHint,
// inferredService], as in
// TestLookupSKUAcrossRegions_ExplicitHintFailsThenInferredFallbackMatches
// above), and the FIRST candidate's catalog does contain a row for this
// suffix but that row contradicts a supplied operation/product_family hint,
// the lookup must not give up right there and report hint_no_match against
// only that first candidate — it must also try the second candidate's
// catalog, which may be where the hint-matching row actually lives. Before
// this fix, lookupSKUInRegion returned as soon as ANY candidate's catalog
// contained the suffix at all, regardless of whether the hint matched
// anything in it, silently starving later candidates of a chance to
// resolve the hint.
func TestLookupSKUAcrossRegions_HintTriedAgainstFallbackServiceCatalog(t *testing.T) {
	resetSKUCatalogCache(t)

	mux := http.NewServeMux()
	// AmazonEC2 (the explicit, "wrong" service hint here) happens to also
	// have a row for the "LCUUsage" suffix, but for an unrelated operation
	// that the hint does not match.
	mux.HandleFunc("/AmazonEC2/current/us-east-1/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(skuBulkJSONMulti([]multiProductSpec{
			{
				sku: "SKU-EC2-DECOY", usageType: "LCUUsage", location: "US East (N. Virginia)",
				priceUSD:   "0.0010000000",
				extraAttrs: map[string]string{"operation": "SomeUnrelatedEC2Operation"},
			},
		})))
	})
	// AWSELB (the inferred fallback service) has the actual hint-matching row.
	mux.HandleFunc("/AWSELB/current/us-east-1/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(skuBulkJSONMulti([]multiProductSpec{
			{
				sku: "SKU-ALB", usageType: "LCUUsage", location: "US East (N. Virginia)",
				priceUSD:   "0.0080000000",
				extraAttrs: map[string]string{"operation": "LoadBalancing:Application"},
			},
		})))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}
	result, err := p.LookupSKUAcrossRegions(
		context.Background(), "aws", "LCUUsage", "AmazonEC2", []string{"us-east-1"},
		"LoadBalancing:Application", "",
	)
	if err != nil {
		t.Fatalf("LookupSKUAcrossRegions returned error: %v", err)
	}
	if result.InferredService != "AWSELB" {
		t.Fatalf("expected inferred_service=AWSELB, got %q", result.InferredService)
	}

	rr := result.Regions[0]
	if rr.Ambiguous {
		t.Fatalf("expected the hint to resolve via the fallback AWSELB catalog, got ambiguous result: %+v", rr)
	}
	if rr.HintStatus != HintStatusResolved {
		t.Errorf("hint_status = %q, want %q", rr.HintStatus, HintStatusResolved)
	}
	if rr.ServiceUsed != "AWSELB" {
		t.Errorf("ServiceUsed = %q, want AWSELB (the fallback candidate that actually matched the hint)", rr.ServiceUsed)
	}
	if !rr.ServiceMismatch {
		t.Errorf("expected ServiceMismatch=true (explicit hint AmazonEC2 didn't match ServiceUsed AWSELB)")
	}
	if len(rr.Prices) != 1 || rr.Prices[0].SKUID != "SKU-ALB" {
		t.Fatalf("expected the single Application LB price from the fallback catalog, got %+v", rr.Prices)
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
			result, err := p.LookupSKUAcrossRegions(context.Background(), "aws", sku, "AWSDataTransfer", []string{"us-east-1"}, "", "")
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
	_, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "SomeMadeUpUsageTypeXYZ123", "", []string{"us-east-1"}, "", "")
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

// TestLookupSKUAcrossRegions_TimedStorageByteHrsIsUndeterminable is the
// direct regression test for RC3-008: previously,
// get_price_by_sku(sku="CAN1-TimedStorage-ByteHrs", regions=[us-east-1])
// with no service hint silently returned a single confidently-priced
// AmazonDynamoDB "Database Storage" row (/bin/bash.25/GB-month) when the usage
// type is equally consistent with S3 Standard storage (/bin/bash.023/GB-month) --
// an ~11x pricing error with no forced signal to the caller. It must now
// come back as a structured SKUErrServiceUndeterminable error instead of a
// confident (and possibly wrong) single-service answer.
func TestLookupSKUAcrossRegions_TimedStorageByteHrsIsUndeterminable(t *testing.T) {
	resetSKUCatalogCache(t)
	overrideBulkBaseURL(t, "http://127.0.0.1:1/unreachable")

	p := &Provider{}
	_, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "CAN1-TimedStorage-ByteHrs", "", []string{"us-east-1"}, "", "")
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
	_, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "BoxUsage:r6id.24xlarge", "AmazonEC2", []string{"us-east-1"}, "", "")
	if err != nil {
		t.Fatalf("first LookupSKUAcrossRegions returned error: %v", err)
	}
	// Second lookup: a different SKU string, same (service, region) pair —
	// this one has no matching row (NoMapping), but must still reuse the
	// memoized catalog rather than re-fetching.
	_, err = p.LookupSKUAcrossRegions(context.Background(), "aws", "BoxUsage:completely-different.xlarge", "AmazonEC2", []string{"us-east-1"}, "", "")
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
	result, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "BoxUsage:r6id.24xlarge", "AmazonEC2", []string{"us-east-1"}, "", "")
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
	result, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "InstanceUsage:db.r5.large", "AmazonRDS", []string{"us-east-1"}, "", "")
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
	result, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "BoxUsage:r6id.24xlarge", "AmazonEC2", []string{"us-east-1"}, "", "")
	if err != nil {
		t.Fatalf("LookupSKUAcrossRegions returned error: %v", err)
	}
	if result.Regions[0].Ambiguous {
		t.Errorf("expected a single matching row to never be flagged ambiguous")
	}
}

// --------------------------------------------------------------------------
// resolveSKUCandidates — operation/product_family hint disambiguation
// (RC2 Bug 1 fix: ambiguous matches must never silently resolve to
// "cheapest"; a hint must deterministically select the correct alternate,
// and fail closed — remain ambiguous — when the hint matches nothing.)
// --------------------------------------------------------------------------

// elbLCUCandidates reproduces the RC1 field-test's Example A ground truth:
// the bare "LCUUsage" suffix is shared by Application/Network/Gateway load
// balancer capacity pricing, distinguished only by the "operation" attribute
// and top-level productFamily. Gateway is deliberately the cheapest of the
// three (it is what the pre-fix code silently picked) and Application is
// deliberately NOT the cheapest, so a test that merely reproduces "cheapest"
// cannot pass by accident.
func elbLCUCandidates() []models.NormalizedPrice {
	return []models.NormalizedPrice{
		{
			Service: "AWSELB", ProductFamily: "Load Balancer-Application", PricePerUnit: 0.0080,
			Attributes: map[string]string{"operation": "LoadBalancing:Application", "usagetype": "LCUUsage"},
			SKUID:      "SKU-ALB",
		},
		{
			Service: "AWSELB", ProductFamily: "Load Balancer-Network", PricePerUnit: 0.0060,
			Attributes: map[string]string{"operation": "LoadBalancing:Network", "usagetype": "LCUUsage"},
			SKUID:      "SKU-NLB",
		},
		{
			Service: "AWSELB", ProductFamily: "Load Balancer-Gateway", PricePerUnit: 0.0040,
			Attributes: map[string]string{"operation": "LoadBalancing:Gateway", "usagetype": "LCUUsage"},
			SKUID:      "SKU-GWLB",
		},
	}
}

// rdsEngineCandidates reproduces the RC1 field-test's Example B ground
// truth: the "InstanceUsage:db.r8g.2xl" suffix is shared by every DB engine
// on that instance type. Plain PostgreSQL is deliberately the cheapest of
// the five (it is what the pre-fix code silently picked); Aurora PostgreSQL
// — identified by operation "CreateDBInstance:0021" — is deliberately NOT
// the cheapest, matching the report's worked numbers ($0.956 vs $1.104).
func rdsEngineCandidates() []models.NormalizedPrice {
	return []models.NormalizedPrice{
		{
			Service: "AmazonRDS", ProductFamily: "Database Instance", PricePerUnit: 1.010,
			Attributes: map[string]string{"operation": "CreateDBInstance:0002", "databaseEngine": "MySQL"},
			SKUID:      "SKU-MYSQL",
		},
		{
			Service: "AmazonRDS", ProductFamily: "Database Instance", PricePerUnit: 0.956,
			Attributes: map[string]string{"operation": "CreateDBInstance:0014", "databaseEngine": "PostgreSQL"},
			SKUID:      "SKU-POSTGRES",
		},
		{
			Service: "AmazonRDS", ProductFamily: "Database Instance", PricePerUnit: 1.030,
			Attributes: map[string]string{"operation": "CreateDBInstance:0009", "databaseEngine": "MariaDB"},
			SKUID:      "SKU-MARIADB",
		},
		{
			Service: "AmazonRDS", ProductFamily: "Database Instance", PricePerUnit: 1.080,
			Attributes: map[string]string{"operation": "CreateDBInstance:0020", "databaseEngine": "Aurora MySQL"},
			SKUID:      "SKU-AURORA-MYSQL",
		},
		{
			Service: "AmazonRDS", ProductFamily: "Database Instance", PricePerUnit: 1.104,
			Attributes: map[string]string{"operation": "CreateDBInstance:0021", "databaseEngine": "Aurora PostgreSQL"},
			SKUID:      "SKU-AURORA-POSTGRES",
		},
	}
}

// TestResolveSKUCandidates_TableDriven is the core unit-level coverage for
// the hint-based disambiguation logic added in RC2 (resolveSKUCandidates):
// no exhaustive HTTP mocking is required here, since resolveSKUCandidates
// operates purely on []models.NormalizedPrice.
func TestResolveSKUCandidates_TableDriven(t *testing.T) {
	tests := []struct {
		name              string
		prices            []models.NormalizedPrice
		operationHint     string
		productFamilyHint string

		wantAmbiguous     bool
		wantHintStatus    string
		wantChosenCount   int
		wantChosenSKUID   string // only checked when wantChosenCount == 1
		wantChosenPriceIs float64
		checkChosenPrice  bool
	}{
		// --- ELB LCUUsage: bare suffix, no hint => must NOT silently pick
		// Gateway (the cheapest); must be flagged ambiguous with all 3 kept.
		{
			name:            "ELB no hint stays ambiguous with all 3 candidates",
			prices:          elbLCUCandidates(),
			wantAmbiguous:   true,
			wantHintStatus:  HintStatusNoHint,
			wantChosenCount: 3,
		},
		// --- ELB with a correct operation hint => resolves deterministically
		// to Application LB, matching the report's worked example.
		{
			name:              "ELB operation hint resolves to Application LB",
			prices:            elbLCUCandidates(),
			operationHint:     "LoadBalancing:Application",
			wantAmbiguous:     false,
			wantHintStatus:    HintStatusResolved,
			wantChosenCount:   1,
			wantChosenSKUID:   "SKU-ALB",
			checkChosenPrice:  true,
			wantChosenPriceIs: 0.0080,
		},
		// --- ELB with a correct operation hint, case-insensitively supplied.
		{
			name:              "ELB operation hint is case-insensitive",
			prices:            elbLCUCandidates(),
			operationHint:     "loadbalancing:application",
			wantAmbiguous:     false,
			wantHintStatus:    HintStatusResolved,
			wantChosenCount:   1,
			wantChosenSKUID:   "SKU-ALB",
			checkChosenPrice:  true,
			wantChosenPriceIs: 0.0080,
		},
		// --- ELB with a correct product_family hint (instead of operation)
		// resolves the same way.
		{
			name:              "ELB product_family hint resolves to Application LB",
			prices:            elbLCUCandidates(),
			productFamilyHint: "Load Balancer-Application",
			wantAmbiguous:     false,
			wantHintStatus:    HintStatusResolved,
			wantChosenCount:   1,
			wantChosenSKUID:   "SKU-ALB",
			checkChosenPrice:  true,
			wantChosenPriceIs: 0.0080,
		},
		// --- ELB with a hint matching zero rows fails closed: still
		// ambiguous, does NOT fall through to picking any default (cheapest
		// or otherwise), and the full original candidate set is preserved.
		{
			name:            "ELB hint matching zero rows fails closed",
			prices:          elbLCUCandidates(),
			operationHint:   "LoadBalancing:ClassicDoesNotExist",
			wantAmbiguous:   true,
			wantHintStatus:  HintStatusNoMatch,
			wantChosenCount: 3,
		},
		// --- RDS engine collision, no hint => ambiguous, all 5 kept.
		{
			name:            "RDS no hint stays ambiguous with all 5 candidates",
			prices:          rdsEngineCandidates(),
			wantAmbiguous:   true,
			wantHintStatus:  HintStatusNoHint,
			wantChosenCount: 5,
		},
		// --- RDS with the operation code from the report's worked example
		// resolves deterministically to Aurora PostgreSQL — NOT plain
		// PostgreSQL (the cheapest, and what the pre-fix code silently
		// picked).
		{
			name:              "RDS operation hint resolves to Aurora PostgreSQL",
			prices:            rdsEngineCandidates(),
			operationHint:     "CreateDBInstance:0021",
			wantAmbiguous:     false,
			wantHintStatus:    HintStatusResolved,
			wantChosenCount:   1,
			wantChosenSKUID:   "SKU-AURORA-POSTGRES",
			checkChosenPrice:  true,
			wantChosenPriceIs: 1.104,
		},
		// --- RDS with a hint matching zero rows fails closed.
		{
			name:            "RDS hint matching zero rows fails closed",
			prices:          rdsEngineCandidates(),
			operationHint:   "CreateDBInstance:9999",
			wantAmbiguous:   true,
			wantHintStatus:  HintStatusNoMatch,
			wantChosenCount: 5,
		},
		// --- Genuinely single-row match, no hint supplied: never flagged
		// ambiguous (regression guard).
		{
			name: "single row is never ambiguous when no hint is supplied",
			prices: []models.NormalizedPrice{
				{Service: "AmazonDynamoDB", PricePerUnit: 0.25, SKUID: "SKU-ONLY"},
			},
			wantAmbiguous:   false,
			wantHintStatus:  HintStatusNoHint,
			wantChosenCount: 1,
			wantChosenSKUID: "SKU-ONLY",
		},
		// --- Single-row match, hint supplied and it actually matches the
		// row: resolved_by_hint, not just "no_hint_supplied" — the hint did
		// real (if trivial) confirming work and callers should be able to
		// see that.
		{
			name: "single row matching a supplied hint resolves by hint",
			prices: []models.NormalizedPrice{
				{
					Service: "AWSELB", ProductFamily: "Load Balancer-Application", PricePerUnit: 0.0080,
					Attributes: map[string]string{"operation": "LoadBalancing:Application"},
					SKUID:      "SKU-ALB",
				},
			},
			operationHint:     "LoadBalancing:Application",
			wantAmbiguous:     false,
			wantHintStatus:    HintStatusResolved,
			wantChosenCount:   1,
			wantChosenSKUID:   "SKU-ALB",
			checkChosenPrice:  true,
			wantChosenPriceIs: 0.0080,
		},
		// --- Single-row match, hint supplied but it CONTRADICTS the sole
		// row (e.g. the suffix happens to be unique in this region's
		// catalog, but the caller's operation/product_family hint names a
		// different product than the one row actually present). This is the
		// RC2 correctness/security review finding: resolveSKUCandidates must
		// not silently return that one row as a confident match just because
		// there was "nothing to disambiguate" among rows — the hint itself
		// must still be honored and fail closed, exactly like the
		// zero-hint-match case for a genuinely multi-row suffix. Silently
		// ignoring a contradicting hint here would let the wrong product's
		// price feed the headline result with no flag at all.
		{
			name: "single row contradicting a supplied hint fails closed, not silently accepted",
			prices: []models.NormalizedPrice{
				{
					Service: "AWSELB", ProductFamily: "Load Balancer-Gateway", PricePerUnit: 0.0040,
					Attributes: map[string]string{"operation": "LoadBalancing:Gateway"},
					SKUID:      "SKU-GWLB",
				},
			},
			operationHint:  "LoadBalancing:Application",
			wantAmbiguous:  true,
			wantHintStatus: HintStatusNoMatch,
			// The fail-closed path returns the original, unfiltered prices
			// slice (here just the one rejected Gateway-LB row) so a caller
			// still sees it via alternate_matches — but wantAmbiguous=true is
			// the load-bearing signal here, not this SKUID: the row is
			// present-but-rejected, not a confirmed match.
			wantChosenCount: 1,
			wantChosenSKUID: "SKU-GWLB",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chosen, ambiguous, hintStatus := resolveSKUCandidates(tc.prices, tc.operationHint, tc.productFamilyHint)
			if ambiguous != tc.wantAmbiguous {
				t.Errorf("ambiguous = %v, want %v", ambiguous, tc.wantAmbiguous)
			}
			if hintStatus != tc.wantHintStatus {
				t.Errorf("hintStatus = %q, want %q", hintStatus, tc.wantHintStatus)
			}
			if len(chosen) != tc.wantChosenCount {
				t.Fatalf("len(chosen) = %d, want %d (chosen=%+v)", len(chosen), tc.wantChosenCount, chosen)
			}
			if tc.wantChosenCount == 1 {
				if chosen[0].SKUID != tc.wantChosenSKUID {
					t.Errorf("chosen[0].SKUID = %q, want %q", chosen[0].SKUID, tc.wantChosenSKUID)
				}
				if tc.checkChosenPrice && chosen[0].PricePerUnit != tc.wantChosenPriceIs {
					t.Errorf("chosen[0].PricePerUnit = %v, want %v", chosen[0].PricePerUnit, tc.wantChosenPriceIs)
				}
			}
		})
	}
}

// TestLookupSKUInRegion_HintResolvesELBAmbiguity is an end-to-end (HTTP-mocked)
// wiring check that operationHint/productFamilyHint actually reach
// resolveSKUCandidates through LookupSKUAcrossRegions/lookupSKUInRegion — the
// table-driven test above exercises resolveSKUCandidates directly and does
// not prove the hint parameters are threaded through the full call chain.
func TestLookupSKUInRegion_HintResolvesELBAmbiguity(t *testing.T) {
	resetSKUCatalogCache(t)
	specs := make([]multiProductSpec, 0, 3)
	for _, c := range []struct {
		sku, op, family, price string
	}{
		{"SKU-ALB", "LoadBalancing:Application", "Load Balancer-Application", "0.0080000000"},
		{"SKU-NLB", "LoadBalancing:Network", "Load Balancer-Network", "0.0060000000"},
		{"SKU-GWLB", "LoadBalancing:Gateway", "Load Balancer-Gateway", "0.0040000000"},
	} {
		specs = append(specs, multiProductSpec{
			sku: c.sku, usageType: "LCUUsage", location: "US East (N. Virginia)", priceUSD: c.price,
			extraAttrs: map[string]string{"operation": c.op},
		})
	}
	body := skuBulkJSONMultiWithProductFamily(specs, map[string]string{
		"SKU-ALB": "Load Balancer-Application", "SKU-NLB": "Load Balancer-Network", "SKU-GWLB": "Load Balancer-Gateway",
	})
	server := newBulkTestServer(t, []byte(body), map[string]string{"Content-Type": "application/json"}, http.StatusOK)
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}

	t.Run("no hint stays ambiguous", func(t *testing.T) {
		result, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "LCUUsage", "AWSELB", []string{"us-east-1"}, "", "")
		if err != nil {
			t.Fatalf("LookupSKUAcrossRegions returned error: %v", err)
		}
		rr := result.Regions[0]
		if !rr.Ambiguous || len(rr.Prices) != 3 || rr.HintStatus != HintStatusNoHint {
			t.Fatalf("expected ambiguous=true, 3 prices, hint_status=no_hint_supplied, got %+v", rr)
		}
	})

	t.Run("operation hint resolves to Application LB", func(t *testing.T) {
		result, err := p.LookupSKUAcrossRegions(
			context.Background(), "aws", "LCUUsage", "AWSELB", []string{"us-east-1"},
			"LoadBalancing:Application", "",
		)
		if err != nil {
			t.Fatalf("LookupSKUAcrossRegions returned error: %v", err)
		}
		rr := result.Regions[0]
		if rr.Ambiguous {
			t.Fatalf("expected ambiguous=false once resolved by hint, got %+v", rr)
		}
		if rr.HintStatus != HintStatusResolved {
			t.Errorf("hint_status = %q, want %q", rr.HintStatus, HintStatusResolved)
		}
		if len(rr.Prices) != 1 || rr.Prices[0].PricePerUnit != 0.008 {
			t.Fatalf("expected the single Application LB price (0.008), got %+v", rr.Prices)
		}
	})

	t.Run("hint matching zero rows fails closed", func(t *testing.T) {
		result, err := p.LookupSKUAcrossRegions(
			context.Background(), "aws", "LCUUsage", "AWSELB", []string{"us-east-1"},
			"LoadBalancing:ClassicDoesNotExist", "",
		)
		if err != nil {
			t.Fatalf("LookupSKUAcrossRegions returned error: %v", err)
		}
		rr := result.Regions[0]
		if !rr.Ambiguous || rr.HintStatus != HintStatusNoMatch || len(rr.Prices) != 3 {
			t.Fatalf("expected fail-closed ambiguous result with all 3 candidates preserved, got %+v", rr)
		}
	})
}

// skuBulkJSONMultiWithProductFamily is like skuBulkJSONMulti but lets each
// SKU carry its own top-level productFamily (skuBulkJSONMulti hard-codes
// "Compute Instance" for every row), needed to exercise the product_family
// hint path against realistic ELB-shaped rows.
func skuBulkJSONMultiWithProductFamily(specs []multiProductSpec, productFamilyBySKU map[string]string) string {
	products := make(map[string]any, len(specs))
	onDemand := make(map[string]any, len(specs))
	for _, s := range specs {
		attrs := map[string]string{
			"usagetype": s.usageType,
			"location":  s.location,
		}
		for k, v := range s.extraAttrs {
			attrs[k] = v
		}
		family := productFamilyBySKU[s.sku]
		if family == "" {
			family = "Compute Instance"
		}
		products[s.sku] = map[string]any{
			"sku":           s.sku,
			"productFamily": family,
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
	result, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "BoxUsage:r6id.24xlarge", "amazonec2", []string{"us-east-1"}, "", "")
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
	_, err := p.LookupSKUAcrossRegions(context.Background(), "aws", longSKU, "AmazonEC2", []string{"us-east-1"}, "", "")
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

// TestLookupSKUAcrossRegions_HintTooLongRejected verifies oversized
// operation/product_family hints are rejected before any network call, for
// input-validation parity with sku/service (RC2 security review finding: the
// hints were previously unbounded while sku/service both had shape/length
// checks).
func TestLookupSKUAcrossRegions_HintTooLongRejected(t *testing.T) {
	resetSKUCatalogCache(t)
	overrideBulkBaseURL(t, "http://127.0.0.1:1/unreachable")

	longHint := strings.Repeat("A", maxHintLength+1)
	p := &Provider{}

	t.Run("operation", func(t *testing.T) {
		_, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "LCUUsage", "AWSELB", []string{"us-east-1"}, longHint, "")
		if err == nil {
			t.Fatal("expected an error for an over-length operation hint, got nil")
		}
		skuErr, ok := err.(*SKULookupError)
		if !ok {
			t.Fatalf("expected *SKULookupError, got %T: %v", err, err)
		}
		if skuErr.Code != SKUErrHintTooLong {
			t.Errorf("expected code %q, got %q", SKUErrHintTooLong, skuErr.Code)
		}
	})

	t.Run("product_family", func(t *testing.T) {
		_, err := p.LookupSKUAcrossRegions(context.Background(), "aws", "LCUUsage", "AWSELB", []string{"us-east-1"}, "", longHint)
		if err == nil {
			t.Fatal("expected an error for an over-length product_family hint, got nil")
		}
		skuErr, ok := err.(*SKULookupError)
		if !ok {
			t.Fatalf("expected *SKULookupError, got %T: %v", err, err)
		}
		if skuErr.Code != SKUErrHintTooLong {
			t.Errorf("expected code %q, got %q", SKUErrHintTooLong, skuErr.Code)
		}
	})
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
