package gcp

import (
	"net/http"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/cache"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/config"
)

// testhooks.go exports a narrow, explicitly-named test-only seam for
// constructing a *Provider wired to a fake HTTP server, mirroring
// internal/providers/aws/testhooks.go's rationale: a regular (non-_test.go)
// file is required (not export_test.go) because a *different* package's
// tests need this — internal/tools's tools_test package, which drives
// raw-SKU tools (get_price_by_sku, estimate_bom, compare_bom_regions) against
// a real *gcpprovider.Provider without a live network call. Everything here
// is a thin, obviously-test-only wrapper; NewProvider's production behavior
// is unchanged.

// NewProviderForTesting constructs a *Provider identical to NewProvider
// except its Cloud Billing Catalog base URL and HTTP client are overridden
// to point at a caller-supplied fake server (typically an httptest.Server).
func NewProviderForTesting(cfg *config.Config, cm *cache.CacheManager, baseURL string, httpClient *http.Client) *Provider {
	return &Provider{
		cfg:        cfg,
		cache:      cm,
		auth:       newGCPAuthProvider(cfg),
		httpClient: httpClient,
		baseURL:    baseURL,
	}
}
