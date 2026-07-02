package aws

// testhooks.go exports narrow, explicitly-named test-only seams onto this
// package's otherwise-unexported globals (bulkPricingBaseURL in aws_bulk.go,
// skuCatalogCache in aws_sku_lookup.go).
//
// WHY THIS FILE EXISTS (as opposed to an export_test.go): Go's
// export_test.go convention only exposes unexported identifiers to that same
// package's own external test binary (package aws_test) — it is NOT visible
// to a different package's tests (e.g. internal/tools's tools_test package,
// which needs to drive HandleGetPriceBySKU end-to-end against a mocked bulk
// endpoint without a real network call). Exposing a deliberately narrow,
// test-labelled seam in a regular (non-_test.go) file is the standard way to
// let another package's tests reach across the package boundary without
// exporting the underlying production variable itself. Everything here is a
// thin, obviously-test-only wrapper; no production behavior changes.

// SetBulkPricingBaseURLForTesting overrides the package-level bulk-pricing
// base URL (normally "https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws")
// for the duration of a test. Call the returned restore func (e.g. via
// t.Cleanup) to put the original value back. Mirrors overrideBulkBaseURL in
// aws_bulk_test.go, which serves the same purpose for this package's own
// (in-package) tests.
func SetBulkPricingBaseURLForTesting(url string) (restore func()) {
	orig := bulkPricingBaseURL
	bulkPricingBaseURL = url
	return func() { bulkPricingBaseURL = orig }
}

// ResetSKUCatalogCacheForTesting clears the process-lifetime (service,region)
// catalog memoization cache used by get_price_by_sku's core logic
// (skuCatalogCache in aws_sku_lookup.go). Without this, a cache entry
// populated by one test (keyed only on "service|region", not on which mock
// server was live at the time) would silently leak into a later test that
// points bulkPricingBaseURL at a different mock server, causing stale reads
// instead of a fresh fetch.
func ResetSKUCatalogCacheForTesting() {
	skuCatalogCache.mu.Lock()
	defer skuCatalogCache.mu.Unlock()
	skuCatalogCache.entries = make(map[string]*skuCatalogEntry)
}
