package aws

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
)

// ---------------------------------------------------------------------------
// Minimal bulk JSON fixtures
// ---------------------------------------------------------------------------

// minimalBulkJSON is a single-product bulk JSON fixture with one OnDemand term.
// Terms use the real AWS bulk file two-level nesting: outer key = SKU, inner key = "SKU.termCode".
const minimalBulkJSON = `{"products":{"SKU1":{"sku":"SKU1","productFamily":"Compute Instance","attributes":{"instanceType":"m5.xlarge","operatingSystem":"Linux","location":"US East (N. Virginia)","tenancy":"Shared","capacitystatus":"Used"}}},"terms":{"OnDemand":{"SKU1":{"SKU1.JRTCKXETXF":{"offerTermCode":"JRTCKXETXF","priceDimensions":{"SKU1.JRTCKXETXF.6YS6EN2CT7":{"unit":"Hrs","pricePerUnit":{"USD":"0.1920000000"}}}}}},"Reserved":{}}}`

// multiProductBulkJSON has three matching products to test maxResults.
const multiProductBulkJSON = `{"products":{"SKU1":{"sku":"SKU1","productFamily":"Compute Instance","attributes":{"instanceType":"m5.xlarge","operatingSystem":"Linux","location":"US East (N. Virginia)","tenancy":"Shared","capacitystatus":"Used"}},"SKU2":{"sku":"SKU2","productFamily":"Compute Instance","attributes":{"instanceType":"m5.xlarge","operatingSystem":"Linux","location":"US East (N. Virginia)","tenancy":"Shared","capacitystatus":"Used"}},"SKU3":{"sku":"SKU3","productFamily":"Compute Instance","attributes":{"instanceType":"m5.xlarge","operatingSystem":"Linux","location":"US East (N. Virginia)","tenancy":"Shared","capacitystatus":"Used"}}},"terms":{"OnDemand":{"SKU1":{"SKU1.JRTCKXETXF":{"offerTermCode":"JRTCKXETXF","priceDimensions":{"SKU1.JRTCKXETXF.6YS6EN2CT7":{"unit":"Hrs","pricePerUnit":{"USD":"0.1920000000"}}}}},"SKU2":{"SKU2.JRTCKXETXF":{"offerTermCode":"JRTCKXETXF","priceDimensions":{"SKU2.JRTCKXETXF.6YS6EN2CT7":{"unit":"Hrs","pricePerUnit":{"USD":"0.1920000000"}}}}},"SKU3":{"SKU3.JRTCKXETXF":{"offerTermCode":"JRTCKXETXF","priceDimensions":{"SKU3.JRTCKXETXF.6YS6EN2CT7":{"unit":"Hrs","pricePerUnit":{"USD":"0.1920000000"}}}}}},"Reserved":{}}}`

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newBulkTestServer creates an httptest.Server that serves the given body bytes.
// The caller must call server.Close() when done.
func newBulkTestServer(t *testing.T, body []byte, headers map[string]string, statusCode int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(statusCode)
		w.Write(body) //nolint:errcheck
	}))
}

// overrideBulkBaseURL saves bulkPricingBaseURL, sets it to serverURL, and
// registers cleanup to restore the original.
func overrideBulkBaseURL(t *testing.T, serverURL string) {
	t.Helper()
	orig := bulkPricingBaseURL
	bulkPricingBaseURL = serverURL
	t.Cleanup(func() { bulkPricingBaseURL = orig })
}

// mkTestFilter is a convenience wrapper for creating a TERM_MATCH filter.
func mkTestFilter(field, value string) pricingtypes.Filter {
	return pricingtypes.Filter{
		Type:  pricingtypes.FilterTypeTermMatch,
		Field: awssdk.String(field),
		Value: awssdk.String(value),
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestGetProductsBulk_BasicFilter verifies that a matching product is returned
// in parsedSKU-compatible JSON format with the correct price.
func TestGetProductsBulk_BasicFilter(t *testing.T) {
	server := newBulkTestServer(t, []byte(minimalBulkJSON), map[string]string{"Content-Type": "application/json"}, http.StatusOK)
	defer server.Close()

	// The URL pattern is: {base}/{service}/current/{region}/index.json
	// We set base to server.URL so the path appended is /AmazonEC2/current/us-east-1/index.json.
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}
	filters := []pricingtypes.Filter{
		mkTestFilter("instanceType", "m5.xlarge"),
		mkTestFilter("operatingSystem", "Linux"),
	}

	results, err := p.getProductsBulk(context.Background(), "AmazonEC2", filters, 10, "us-east-1")
	if err != nil {
		t.Fatalf("getProductsBulk returned error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	// Unmarshal into parsedSKU to validate structure.
	var sku parsedSKU
	if err := json.Unmarshal([]byte(results[0]), &sku); err != nil {
		t.Fatalf("failed to unmarshal result as parsedSKU: %v", err)
	}

	if sku.Product.SKU != "SKU1" {
		t.Errorf("expected SKU=SKU1, got %q", sku.Product.SKU)
	}
	if sku.Product.Attributes["instanceType"] != "m5.xlarge" {
		t.Errorf("expected instanceType=m5.xlarge, got %q", sku.Product.Attributes["instanceType"])
	}

	// Verify the price is parseable and correct.
	price, unit := extractOnDemandPrice(sku)
	if unit != "Hrs" {
		t.Errorf("expected unit=Hrs, got %q", unit)
	}
	const wantPrice = 0.192
	if price < wantPrice-1e-9 || price > wantPrice+1e-9 {
		t.Errorf("expected price=%.10f, got %.10f", wantPrice, price)
	}
}

// TestGetProductsBulk_NoMatch verifies that a filter matching no products
// returns an empty slice (not an error).
func TestGetProductsBulk_NoMatch(t *testing.T) {
	server := newBulkTestServer(t, []byte(minimalBulkJSON), map[string]string{"Content-Type": "application/json"}, http.StatusOK)
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}
	filters := []pricingtypes.Filter{
		mkTestFilter("instanceType", "c5.4xlarge"), // no such instance in fixture
	}

	results, err := p.getProductsBulk(context.Background(), "AmazonEC2", filters, 10, "us-east-1")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected empty results, got %d items", len(results))
	}
}

// TestGetProductsBulk_MaxResults verifies that when more products match than
// maxResults, only maxResults items are returned.
func TestGetProductsBulk_MaxResults(t *testing.T) {
	server := newBulkTestServer(t, []byte(multiProductBulkJSON), map[string]string{"Content-Type": "application/json"}, http.StatusOK)
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}
	// All three products match instanceType=m5.xlarge, but we cap at 2.
	filters := []pricingtypes.Filter{
		mkTestFilter("instanceType", "m5.xlarge"),
	}
	const maxResults = int32(2)

	results, err := p.getProductsBulk(context.Background(), "AmazonEC2", filters, maxResults, "us-east-1")
	if err != nil {
		t.Fatalf("getProductsBulk returned error: %v", err)
	}
	if int32(len(results)) != maxResults {
		t.Errorf("expected %d results, got %d", maxResults, len(results))
	}

	// Each result should still be valid parsedSKU JSON.
	for i, r := range results {
		var sku parsedSKU
		if err := json.Unmarshal([]byte(r), &sku); err != nil {
			t.Errorf("result[%d] is not valid parsedSKU JSON: %v", i, err)
		}
	}
}

// TestGetProductsBulk_GzipResponse verifies that a gzip-compressed response
// body is decompressed transparently.
func TestGetProductsBulk_GzipResponse(t *testing.T) {
	// Compress the fixture JSON.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(minimalBulkJSON)); err != nil {
		t.Fatalf("gzip.Write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip.Close: %v", err)
	}
	compressed := buf.Bytes()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(compressed) //nolint:errcheck
	}))
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}
	filters := []pricingtypes.Filter{
		mkTestFilter("instanceType", "m5.xlarge"),
	}

	results, err := p.getProductsBulk(context.Background(), "AmazonEC2", filters, 10, "us-east-1")
	if err != nil {
		t.Fatalf("getProductsBulk returned error on gzip response: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result from gzip response, got %d", len(results))
	}

	var sku parsedSKU
	if err := json.Unmarshal([]byte(results[0]), &sku); err != nil {
		t.Fatalf("failed to unmarshal gzip result: %v", err)
	}
	if sku.Product.SKU != "SKU1" {
		t.Errorf("expected SKU=SKU1 after gzip decompression, got %q", sku.Product.SKU)
	}
}

// TestGetProductsBulk_HTTPError verifies that a non-200 HTTP status causes an error.
func TestGetProductsBulk_HTTPError(t *testing.T) {
	server := newBulkTestServer(t, []byte("Internal Server Error"), nil, http.StatusInternalServerError)
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}
	_, err := p.getProductsBulk(context.Background(), "AmazonEC2", nil, 10, "us-east-1")
	if err == nil {
		t.Fatal("expected error for HTTP 500, got nil")
	}
}

// TestGetProductsBulk_LocationFilter verifies that the location filter is applied
// via the URL region path and NOT as an attribute filter. This means:
//  1. The request URL path includes the mapped region code.
//  2. A product is still returned when only a location filter is provided
//     (location is excluded from attrFilters, so matchesProduct returns true).
func TestGetProductsBulk_LocationFilter(t *testing.T) {
	var capturedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(minimalBulkJSON)) //nolint:errcheck
	}))
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}
	// Pass only a location filter (no other attribute filters).
	// extractRegionFromFilters will map "US East (N. Virginia)" -> "us-east-1".
	// The location filter must be excluded from attrFilters so matchesProduct passes.
	filters := []pricingtypes.Filter{
		mkTestFilter("location", "US East (N. Virginia)"),
	}

	region := extractRegionFromFilters(filters)
	results, err := p.getProductsBulk(context.Background(), "AmazonEC2", filters, 10, region)
	if err != nil {
		t.Fatalf("getProductsBulk returned error: %v", err)
	}

	// Verify the URL path encodes the region (not the location display name).
	wantPath := "/AmazonEC2/current/us-east-1/index.json"
	if capturedPath != wantPath {
		t.Errorf("expected URL path %q, got %q", wantPath, capturedPath)
	}

	// Location-only filter means no attribute filtering; the product must be returned.
	if len(results) != 1 {
		t.Errorf("expected 1 result (location filter does not restrict attributes), got %d", len(results))
	}
}

// TestGetProductsBulk_ReservedTermsCollected verifies that Reserved terms are
// collected and included in per-SKU output when OnDemand terms are absent.
// Mirrors Python test_get_products_bulk_reserved_terms_collected.
func TestGetProductsBulk_ReservedTermsCollected(t *testing.T) {
	const bulkSKU = "JRTCKXETXF8Z6NMQ"
	bulkJSON := `{
		"products": {
			"` + bulkSKU + `": {
				"sku": "` + bulkSKU + `",
				"productFamily": "Compute Instance",
				"attributes": {
					"instanceType": "m5.xlarge",
					"operatingSystem": "Linux",
					"tenancy": "Shared",
					"preInstalledSw": "NA",
					"capacitystatus": "Used"
				}
			}
		},
		"terms": {
			"OnDemand": {},
			"Reserved": {
				"` + bulkSKU + `": {
					"` + bulkSKU + `.RESERVED": {
						"priceDimensions": {
							"` + bulkSKU + `.R.DIM": {
								"unit": "Hrs",
								"pricePerUnit": {"USD": "0.0500000000"},
								"description": "Reserved hourly"
							}
						},
						"termAttributes": {
							"LeaseContractLength": "1yr",
							"PurchaseOption": "No Upfront",
							"OfferingClass": "standard"
						}
					}
				}
			}
		}
	}`

	server := newBulkTestServer(t, []byte(bulkJSON), map[string]string{"Content-Type": "application/json"}, http.StatusOK)
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}
	filters := []pricingtypes.Filter{
		mkTestFilter("instanceType", "m5.xlarge"),
	}

	results, err := p.getProductsBulk(context.Background(), "AmazonEC2", filters, 10, "us-east-1")
	if err != nil {
		t.Fatalf("getProductsBulk returned error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	var sku parsedSKU
	if err := json.Unmarshal([]byte(results[0]), &sku); err != nil {
		t.Fatalf("failed to unmarshal result as parsedSKU: %v", err)
	}

	if len(sku.Terms.OnDemand) != 0 {
		t.Errorf("expected empty OnDemand terms, got %d", len(sku.Terms.OnDemand))
	}
	if len(sku.Terms.Reserved) == 0 {
		t.Fatalf("expected Reserved terms to be collected, got empty map")
	}
	for termKey := range sku.Terms.Reserved {
		if !strings.HasPrefix(termKey, bulkSKU) {
			t.Errorf("Reserved term key %q does not have SKU prefix %q", termKey, bulkSKU)
		}
	}
}

// TestGetProductsBulk_ProductFamilyFilter verifies that productFamily filter
// matches the top-level productFamily field (not inside attributes).
// Mirrors Python test_get_products_bulk_productfamily_filter.
func TestGetProductsBulk_ProductFamilyFilter(t *testing.T) {
	const bulkJSON = `{
		"products": {
			"STORAGE_SKU": {
				"sku": "STORAGE_SKU",
				"productFamily": "Storage",
				"attributes": {
					"volumeApiName": "gp3"
				}
			},
			"COMPUTE_SKU": {
				"sku": "COMPUTE_SKU",
				"productFamily": "Compute Instance",
				"attributes": {
					"instanceType": "m5.xlarge"
				}
			}
		},
		"terms": {
			"OnDemand": {
				"STORAGE_SKU": {
					"STORAGE_SKU.TERM": {
						"priceDimensions": {
							"STORAGE_SKU.DIM": {
								"unit": "GB-Mo",
								"pricePerUnit": {"USD": "0.0800000000"},
								"description": "$0.08 per GB-month"
							}
						},
						"termAttributes": {}
					}
				}
			},
			"Reserved": {}
		}
	}`

	server := newBulkTestServer(t, []byte(bulkJSON), map[string]string{"Content-Type": "application/json"}, http.StatusOK)
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}
	filters := []pricingtypes.Filter{
		mkTestFilter("productFamily", "Storage"),
	}

	results, err := p.getProductsBulk(context.Background(), "AmazonEC2", filters, 10, "us-east-1")
	if err != nil {
		t.Fatalf("getProductsBulk returned error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result (only Storage SKU), got %d", len(results))
	}

	var sku parsedSKU
	if err := json.Unmarshal([]byte(results[0]), &sku); err != nil {
		t.Fatalf("failed to unmarshal result as parsedSKU: %v", err)
	}
	if sku.Product.SKU != "STORAGE_SKU" {
		t.Errorf("expected SKU=STORAGE_SKU, got %q", sku.Product.SKU)
	}
	if sku.Product.ProductFamily != "Storage" {
		t.Errorf("expected productFamily=Storage, got %q", sku.Product.ProductFamily)
	}
}

// TestGetProductsBulk_MalformedJSON verifies that malformed bulk JSON returns an
// error gracefully without panicking.
func TestGetProductsBulk_MalformedJSON(t *testing.T) {
	const malformed = `{"products":{invalid json here`

	server := newBulkTestServer(t, []byte(malformed), map[string]string{"Content-Type": "application/json"}, http.StatusOK)
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}
	_, err := p.getProductsBulk(context.Background(), "AmazonEC2", nil, 10, "us-east-1")
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

// TestExtractRegionFromFilters tests the helper that maps location display
// names to AWS region codes.
func TestExtractRegionFromFilters(t *testing.T) {
	tests := []struct {
		name       string
		filters    []pricingtypes.Filter
		wantRegion string
	}{
		{
			name:       "US East N Virginia",
			filters:    []pricingtypes.Filter{mkTestFilter("location", "US East (N. Virginia)")},
			wantRegion: "us-east-1",
		},
		{
			name:       "US West Oregon",
			filters:    []pricingtypes.Filter{mkTestFilter("location", "US West (Oregon)")},
			wantRegion: "us-west-2",
		},
		{
			name:       "Europe Frankfurt",
			filters:    []pricingtypes.Filter{mkTestFilter("location", "Europe (Frankfurt)")},
			wantRegion: "eu-central-1",
		},
		{
			name:       "Asia Pacific Tokyo",
			filters:    []pricingtypes.Filter{mkTestFilter("location", "Asia Pacific (Tokyo)")},
			wantRegion: "ap-northeast-1",
		},
		{
			name:       "no location filter falls back to us-east-1",
			filters:    []pricingtypes.Filter{mkTestFilter("instanceType", "m5.xlarge")},
			wantRegion: "us-east-1",
		},
		{
			name:       "unknown location falls back to us-east-1",
			filters:    []pricingtypes.Filter{mkTestFilter("location", "Narnia (Nowhere)")},
			wantRegion: "us-east-1",
		},
		{
			name:       "empty filters fall back to us-east-1",
			filters:    []pricingtypes.Filter{},
			wantRegion: "us-east-1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractRegionFromFilters(tc.filters)
			if got != tc.wantRegion {
				t.Errorf("extractRegionFromFilters(%v) = %q, want %q", tc.filters, got, tc.wantRegion)
			}
		})
	}
}

// --------------------------------------------------------------------------
// Perf fix regression: collectTermsForSKUs / getProductsBulk assembly join
// (RC2 Bug 2 — get_price_by_sku's EC2/EBS timeouts).
//
// The pre-fix bug: the final assembly loop in getProductsBulk did, for every
// matched product, a full linear scan of a *flattened* map of every term key
// in the whole offer file, checking strings.HasPrefix(termKey, sku+".") — an
// O(P×M) join. For the real AmazonEC2 offer file (P≈M≈120K), that is what
// caused get_price_by_sku's EC2/EBS lookups (fetchSKUCatalog calls
// getProductsBulk with no attribute filters, i.e. P≈M≈the whole file) to
// time out 100% of the time. The fix keeps collectTermsForSKUs' natural
// SKU-level grouping (sku -> termKey -> raw term) instead of flattening it,
// so the assembly loop can do an O(1) map[sku] lookup per product instead —
// turning the join into O(P+M).
// --------------------------------------------------------------------------

// TestCollectTermsForSKUs_GroupsBySKUAndSkipsUnmatched is a direct unit test
// of the grouping mechanism the perf fix relies on: collectTermsForSKUs must
// return terms grouped by their owning SKU (map[sku]map[termKey]raw) — NOT a
// flat map[termKey]raw — and must skip (not collect) terms for any outer key
// not present in the caller-supplied matched-SKU set, both of which the
// O(1)-lookup assembly loop in getProductsBulk depends on.
func TestCollectTermsForSKUs_GroupsBySKUAndSkipsUnmatched(t *testing.T) {
	// Term-type object shaped exactly like the real bulk file's
	// "OnDemand"/"Reserved" value: outer key = SKU, inner key = "SKU.termCode".
	const termTypeJSON = `{
		"SKU-MATCHED": {
			"SKU-MATCHED.TERM1": {"offerTermCode": "TERM1", "note": "first rate"},
			"SKU-MATCHED.TERM2": {"offerTermCode": "TERM2", "note": "second rate"}
		},
		"SKU-UNMATCHED": {
			"SKU-UNMATCHED.TERM1": {"offerTermCode": "TERM1"}
		}
	}`

	dec := json.NewDecoder(strings.NewReader(termTypeJSON))
	// Only "SKU-MATCHED" is in the matched-product set — mirrors
	// getProductsBulk passing rawProducts (already filtered by matchesProduct).
	matchedSKUs := map[string]json.RawMessage{"SKU-MATCHED": json.RawMessage(`{}`)}

	grouped, err := collectTermsForSKUs(dec, matchedSKUs)
	if err != nil {
		t.Fatalf("collectTermsForSKUs returned error: %v", err)
	}

	if len(grouped) != 1 {
		t.Fatalf("expected exactly 1 SKU group (unmatched SKU's terms must be skipped), got %d: %+v", len(grouped), grouped)
	}
	inner, ok := grouped["SKU-MATCHED"]
	if !ok {
		t.Fatalf("expected a group for SKU-MATCHED, got keys %v", func() []string {
			ks := make([]string, 0, len(grouped))
			for k := range grouped {
				ks = append(ks, k)
			}
			return ks
		}())
	}
	if len(inner) != 2 {
		t.Fatalf("expected 2 term entries grouped under SKU-MATCHED, got %d: %+v", len(inner), inner)
	}
	if _, ok := inner["SKU-MATCHED.TERM1"]; !ok {
		t.Errorf("expected inner map to contain key SKU-MATCHED.TERM1, got %+v", inner)
	}
	if _, ok := inner["SKU-MATCHED.TERM2"]; !ok {
		t.Errorf("expected inner map to contain key SKU-MATCHED.TERM2, got %+v", inner)
	}
	if _, ok := grouped["SKU-UNMATCHED"]; ok {
		t.Errorf("expected SKU-UNMATCHED to be entirely absent from the grouped result, got %+v", grouped["SKU-UNMATCHED"])
	}
}

// buildLargeBulkFixture builds an n-product AWS bulk offer-file fixture with
// one OnDemand term per product, in the same two-level-nesting shape as the
// real endpoint. Every product's usagetype is unique (BoxUsage:type%06d), so
// each also becomes its own suffix bucket if run through fetchSKUCatalog —
// not required for this test, but keeps the fixture realistic.
func buildLargeBulkFixture(n int) []byte {
	products := make(map[string]any, n)
	onDemand := make(map[string]any, n)
	for i := 0; i < n; i++ {
		sku := fmt.Sprintf("SKU%08d", i)
		products[sku] = map[string]any{
			"sku":           sku,
			"productFamily": "Compute Instance",
			"attributes": map[string]string{
				"usagetype":    fmt.Sprintf("BoxUsage:type%06d", i),
				"instanceType": fmt.Sprintf("type%06d.large", i),
				"location":     "US East (N. Virginia)",
			},
		}
		termCode := sku + ".JRTCKXETXF"
		rateCode := termCode + ".6YS6EN2CT7"
		onDemand[sku] = map[string]any{
			termCode: map[string]any{
				"offerTermCode": "JRTCKXETXF",
				"priceDimensions": map[string]any{
					rateCode: map[string]any{
						"unit":         "Hrs",
						"pricePerUnit": map[string]string{"USD": "0.1230000000"},
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
	return b
}

// TestGetProductsBulk_LargeCatalog_CompletesWithoutQuadraticBlowup is the
// regression guard for RC2 Bug 2: it reproduces the shape of the call that
// timed out 100% of the time (getProductsBulk with NO attribute filters and
// maxResults effectively unbounded — exactly how fetchSKUCatalog in
// aws_sku_lookup.go calls it — against a catalog where every product
// matches, mirroring the real unfiltered AmazonEC2/EBS offer file). It
// asserts both correctness (every product's term is attached) and an
// elapsed-time bound generous enough for the fixed O(P+M) join but far
// tighter than the pre-fix O(P×M) join would need at this N.
//
// Calibrated empirically against this repo (not just estimated): N and the
// budget below were chosen by temporarily reintroducing the exact pre-fix
// join (flatten terms into one map, then scan+strings.HasPrefix per
// product) and confirming it fails this test, while the real (fixed) join
// passes with a large margin:
//   - fixed join:   ~150ms  (plain), ~1.1s  (go test -race)
//   - reverted, pre-fix join: ~13.5s at the same N=30000
//
// The budget is set well above the fixed join's observed time and well
// below the pre-fix join's, so a regression back to the O(P×M) join fails
// this test rather than merely being "slower but still green".
func TestGetProductsBulk_LargeCatalog_CompletesWithoutQuadraticBlowup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large-catalog perf regression test in short mode")
	}
	const n = 30000

	body := buildLargeBulkFixture(n)
	server := newBulkTestServer(t, body, map[string]string{"Content-Type": "application/json"}, http.StatusOK)
	defer server.Close()
	overrideBulkBaseURL(t, server.URL)

	p := &Provider{}

	start := time.Now()
	// No filters, maxResults=math.MaxInt32: exactly how fetchSKUCatalog
	// calls getProductsBulk (see aws_sku_lookup.go) — the call shape that
	// actually times out under the pre-fix O(P×M) join.
	results, err := p.getProductsBulk(context.Background(), "AmazonEC2", nil, math.MaxInt32, "us-east-1")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("getProductsBulk returned error: %v", err)
	}

	if len(results) != n {
		t.Fatalf("expected %d results, got %d", n, len(results))
	}

	const budget = 8 * time.Second
	if elapsed > budget {
		t.Errorf("getProductsBulk(%d products, unfiltered) took %v, want under %v — this is the "+
			"regression guard for RC2 Bug 2 (O(P×M) term-join causing EC2/EBS timeouts); an O(P+M) "+
			"join should comfortably clear this bound", n, elapsed, budget)
	}
	t.Logf("getProductsBulk(%d products, unfiltered) took %v", n, elapsed)

	// Spot-check correctness: every result must carry its own OnDemand term
	// (not some other SKU's, and not empty) — the actual defect this fix
	// guards against is a join that returns wrong/missing terms as much as
	// one that is merely slow.
	checked := 0
	for _, raw := range results {
		var sku parsedSKU
		if err := json.Unmarshal([]byte(raw), &sku); err != nil {
			t.Fatalf("failed to unmarshal result: %v", err)
		}
		if len(sku.Terms.OnDemand) != 1 {
			t.Fatalf("expected exactly 1 OnDemand term for SKU %q, got %d", sku.Product.SKU, len(sku.Terms.OnDemand))
		}
		// The join must attach each product's OWN term, not some other
		// product's — the exact defect a broken join (wrong grouping, not
		// just a slow one) would produce.
		for _, offerTerm := range sku.Terms.OnDemand {
			for dimKey := range offerTerm.PriceDimensions {
				if !strings.HasPrefix(dimKey, sku.Product.SKU+".") {
					t.Errorf("SKU %q got a price dimension key %q belonging to a different SKU", sku.Product.SKU, dimKey)
				}
			}
		}
		checked++
		if checked >= 25 {
			break // a full-N unmarshal-and-check pass isn't needed to catch a wrong-term-attached defect
		}
	}
}
