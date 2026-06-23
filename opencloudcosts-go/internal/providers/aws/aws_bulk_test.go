package aws

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
	awssdk "github.com/aws/aws-sdk-go-v2/aws"
)

// ---------------------------------------------------------------------------
// Minimal bulk JSON fixtures
// ---------------------------------------------------------------------------

// minimalBulkJSON is a single-product bulk JSON fixture with one OnDemand term.
const minimalBulkJSON = `{"products":{"SKU1":{"sku":"SKU1","productFamily":"Compute Instance","attributes":{"instanceType":"m5.xlarge","operatingSystem":"Linux","location":"US East (N. Virginia)","tenancy":"Shared","capacitystatus":"Used"}}},"terms":{"OnDemand":{"SKU1.JRTCKXETXF":{"offerTermCode":"JRTCKXETXF","priceDimensions":{"SKU1.JRTCKXETXF.6YS6EN2CT7":{"unit":"Hrs","pricePerUnit":{"USD":"0.1920000000"}}}}},"Reserved":{}}}`

// multiProductBulkJSON has three matching products to test maxResults.
const multiProductBulkJSON = `{"products":{"SKU1":{"sku":"SKU1","productFamily":"Compute Instance","attributes":{"instanceType":"m5.xlarge","operatingSystem":"Linux","location":"US East (N. Virginia)","tenancy":"Shared","capacitystatus":"Used"}},"SKU2":{"sku":"SKU2","productFamily":"Compute Instance","attributes":{"instanceType":"m5.xlarge","operatingSystem":"Linux","location":"US East (N. Virginia)","tenancy":"Shared","capacitystatus":"Used"}},"SKU3":{"sku":"SKU3","productFamily":"Compute Instance","attributes":{"instanceType":"m5.xlarge","operatingSystem":"Linux","location":"US East (N. Virginia)","tenancy":"Shared","capacitystatus":"Used"}}},"terms":{"OnDemand":{"SKU1.JRTCKXETXF":{"offerTermCode":"JRTCKXETXF","priceDimensions":{"SKU1.JRTCKXETXF.6YS6EN2CT7":{"unit":"Hrs","pricePerUnit":{"USD":"0.1920000000"}}}},"SKU2.JRTCKXETXF":{"offerTermCode":"JRTCKXETXF","priceDimensions":{"SKU2.JRTCKXETXF.6YS6EN2CT7":{"unit":"Hrs","pricePerUnit":{"USD":"0.1920000000"}}}},"SKU3.JRTCKXETXF":{"offerTermCode":"JRTCKXETXF","priceDimensions":{"SKU3.JRTCKXETXF.6YS6EN2CT7":{"unit":"Hrs","pricePerUnit":{"USD":"0.1920000000"}}}}},"Reserved":{}}}`

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
			name:    "no location filter falls back to us-east-1",
			filters: []pricingtypes.Filter{mkTestFilter("instanceType", "m5.xlarge")},
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
