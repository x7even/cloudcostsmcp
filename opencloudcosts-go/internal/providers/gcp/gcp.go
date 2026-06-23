// Package gcp implements the GCP cloud pricing provider.
// It ports the Python GCPProvider from providers/gcp.py.
// Part 1: Provider struct, constructor, identity, HTTP/auth helpers, SKU catalog helpers,
//
//	and price-index building used by compute and storage pricing.
//
// Part 2 (gcp_database.go): database, container pricing.
// Part 3 (future): networking, egress, observability pricing.
package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/cache"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/config"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/providers"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/utils"
)

// Compile-time interface check.
var _ providers.Provider = (*Provider)(nil)

const (
	// sourceURL is the canonical source for GCP public billing catalog data.
	sourceURL = "https://cloud.google.com/billing/v1/services"

	// catalogBase is the base URL for the GCP Cloud Billing Catalog REST API.
	catalogBase = "https://cloudbilling.googleapis.com/v1"

	// computeServiceID is the GCP service ID for Compute Engine.
	computeServiceID = "6F81-5844-456A"

	// gcsServiceID is the GCP service ID for Cloud Storage.
	gcsServiceID = "95FF-2EF5-5EA1"

	// schemaVersion is the current schema version for PricingResult.
	schemaVersion = "1"
)

// Provider implements the GCP cloud pricing provider.
type Provider struct {
	cfg        *config.Config
	cache      *cache.CacheManager
	auth       *gcpAuthProvider
	httpClient *http.Client
	baseURL    string
}

// NewProvider constructs a new GCP Provider.
// It wires auth, an HTTP client with a 30-second timeout, and the billing catalog base URL.
func NewProvider(cfg *config.Config, cm *cache.CacheManager) (*Provider, error) {
	auth := newGCPAuthProvider(cfg)
	p := &Provider{
		cfg:        cfg,
		cache:      cm,
		auth:       auth,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    catalogBase,
	}
	return p, nil
}

// --------------------------------------------------------------------------
// Provider identity (providers.Provider)
// --------------------------------------------------------------------------

// Name returns "gcp".
func (p *Provider) Name() providers.CloudProvider {
	return models.CloudProviderGCP
}

// DefaultRegion returns the GCP default region.
func (p *Provider) DefaultRegion() string {
	return "us-central1"
}

// MajorRegions returns the 12-region curated list.
func (p *Provider) MajorRegions() []string {
	return utils.GCPMajorRegions
}

// --------------------------------------------------------------------------
// Catalog HTTP helpers
// --------------------------------------------------------------------------

// fetchSKUs returns all SKUs for the given GCP service ID as raw maps.
// Each SKU is a map[string]any matching the JSON shape from the Billing Catalog API.
// Results are cached using the metadata TTL.
func (p *Provider) fetchSKUs(ctx context.Context, serviceID string) ([]map[string]any, error) {
	cacheKey := "gcp:skus:" + serviceID
	if raw, ok := p.cache.GetMetadata(cacheKey); ok {
		var skus []map[string]any
		if err := json.Unmarshal(raw, &skus); err == nil {
			return skus, nil
		}
	}

	var all []map[string]any
	pageToken := ""
	for {
		url := fmt.Sprintf("%s/services/%s/skus?pageSize=5000", p.baseURL, serviceID)
		if pageToken != "" {
			url += "&pageToken=" + pageToken
		}
		if p.cfg.GCPAPIKey != "" {
			url += "&key=" + p.cfg.GCPAPIKey
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("gcp: build SKU request: %w", err)
		}

		// Bearer auth when no API key is configured.
		if p.cfg.GCPAPIKey == "" {
			if err := p.auth.addBearerHeader(ctx, req); err != nil {
				return nil, fmt.Errorf("gcp: auth: %w", err)
			}
		}

		resp, err := p.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("gcp: SKU fetch: %w", err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("gcp: SKU read body: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("gcp: SKU catalog HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
		}

		var page struct {
			SKUs          []map[string]any `json:"skus"`
			NextPageToken string           `json:"nextPageToken"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("gcp: parse SKU page: %w", err)
		}
		all = append(all, page.SKUs...)
		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}

	if raw, err := json.Marshal(all); err == nil {
		p.cache.SetMetadata(cacheKey, raw, p.cfg.MetadataTTL())
	}
	slog.Info("gcp: fetched SKUs", "service_id", serviceID, "count", len(all))
	return all, nil
}

// gcpMoney converts a GCP Money proto (units string, nanos int) to float64.
// This duplicates utils.GCPMoney for use within the package without an import cycle.
func gcpMoney(units string, nanos int) float64 {
	return utils.GCPMoney(units, nanos)
}

// --------------------------------------------------------------------------
// Price index helpers
// --------------------------------------------------------------------------

// priceIndex is a map of (description, usageType) → price.
// We serialise it to the cache as "desc|usageType" → float64.
type priceIndex map[[2]string]float64

// buildComputePriceIndex builds a price index for Compute Engine SKUs in a region.
// The index is keyed by (sku_description, usageType).
func (p *Provider) buildComputePriceIndex(ctx context.Context, region string) (priceIndex, error) {
	cacheKey := "gcp:price_index:" + region
	if idx, ok := p.loadPriceIndex(cacheKey); ok {
		return idx, nil
	}

	skus, err := p.fetchSKUs(ctx, computeServiceID)
	if err != nil {
		return nil, err
	}

	idx := make(priceIndex)
	for _, sku := range skus {
		regions, _ := sku["serviceRegions"].([]any)
		if !skuMatchesRegion(regions, region) {
			continue
		}
		desc, _ := sku["description"].(string)
		cat, _ := sku["category"].(map[string]any)
		usageType := "OnDemand"
		if cat != nil {
			if u, ok := cat["usageType"].(string); ok && u != "" {
				usageType = u
			}
		}
		price := skuPrice(sku)
		if price > 0 {
			idx[[2]string{desc, usageType}] = price
		}
	}

	p.savePriceIndex(cacheKey, idx, p.cfg.CacheTTL())
	return idx, nil
}

// buildGCSPriceIndex builds a price index for Cloud Storage SKUs in a region.
func (p *Provider) buildGCSPriceIndex(ctx context.Context, region string) (priceIndex, error) {
	cacheKey := "gcp:gcs_price_index:" + region
	if idx, ok := p.loadPriceIndex(cacheKey); ok {
		return idx, nil
	}

	skus, err := p.fetchSKUs(ctx, gcsServiceID)
	if err != nil {
		return nil, err
	}

	gcsExcludeKeywords := []string{"operations", "retrieval", "early delete", "metadata", "list"}

	idx := make(priceIndex)
	for _, sku := range skus {
		regions, _ := sku["serviceRegions"].([]any)
		if !skuMatchesRegion(regions, region) {
			continue
		}
		desc, _ := sku["description"].(string)
		descLower := strings.ToLower(desc)
		excluded := false
		for _, kw := range gcsExcludeKeywords {
			if strings.Contains(descLower, kw) {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}
		cat, _ := sku["category"].(map[string]any)
		usageType := "OnDemand"
		if cat != nil {
			if u, ok := cat["usageType"].(string); ok && u != "" {
				usageType = u
			}
		}
		price := skuPrice(sku)
		if price > 0 {
			idx[[2]string{desc, usageType}] = price
		}
	}

	p.savePriceIndex(cacheKey, idx, p.cfg.CacheTTL())
	return idx, nil
}

// lookupPrice finds a price in the index by partial description match (case-insensitive).
func lookupPrice(idx priceIndex, descSubstring, usageType string) (float64, bool) {
	sub := strings.ToLower(descSubstring)
	for k, price := range idx {
		if k[1] == usageType && strings.Contains(strings.ToLower(k[0]), sub) {
			return price, true
		}
	}
	return 0, false
}

// loadPriceIndex attempts to load a priceIndex from the cache.
func (p *Provider) loadPriceIndex(cacheKey string) (priceIndex, bool) {
	raw, ok := p.cache.GetMetadata(cacheKey)
	if !ok {
		return nil, false
	}
	var flat map[string]float64
	if err := json.Unmarshal(raw, &flat); err != nil {
		return nil, false
	}
	idx := make(priceIndex, len(flat))
	for k, v := range flat {
		sep := strings.LastIndex(k, "|")
		if sep < 0 {
			continue
		}
		desc, utype := k[:sep], k[sep+1:]
		idx[[2]string{desc, utype}] = v
	}
	return idx, true
}

// savePriceIndex serialises a priceIndex to the cache.
func (p *Provider) savePriceIndex(cacheKey string, idx priceIndex, ttl time.Duration) {
	flat := make(map[string]float64, len(idx))
	for k, v := range idx {
		flat[k[0]+"|"+k[1]] = v
	}
	if raw, err := json.Marshal(flat); err == nil {
		p.cache.SetMetadata(cacheKey, raw, ttl)
	}
}

// --------------------------------------------------------------------------
// Result helpers (mirror Python's _annotate_fresh / _apply_cache_trust)
// --------------------------------------------------------------------------

// annotateFresh stamps freshness metadata on a slice of NormalizedPrice.
func annotateFresh(prices []models.NormalizedPrice) []models.NormalizedPrice {
	now := time.Now().UTC()
	age := 0
	out := make([]models.NormalizedPrice, len(prices))
	for i, pr := range prices {
		pr.FetchedAt = &now
		pr.CacheAgeSeconds = &age
		pr.SourceURL = sourceURL
		out[i] = pr
	}
	return out
}

// buildResult wraps a slice of NormalizedPrice into the standard PricingResult.
func buildResult(prices []models.NormalizedPrice, breakdown map[string]any) *models.PricingResult {
	source := "catalog"
	return &models.PricingResult{
		PublicPrices:  prices,
		AuthAvailable: false,
		Breakdown:     breakdown,
		Source:        source,
		SchemaVersion: schemaVersion,
	}
}

// --------------------------------------------------------------------------
// Shared SKU helpers used by gcp_ai.go and gcp_networking.go
// --------------------------------------------------------------------------

// skuMatchesRegion reports whether a SKU's serviceRegions slice (from raw JSON)
// includes the given region or the literal string "global".
func skuMatchesRegion(regions []any, region string) bool {
	for _, r := range regions {
		s, _ := r.(string)
		if s == region || s == "global" {
			return true
		}
	}
	return false
}

// skuPrice extracts the first-tier unit price (startUsageAmount == 0) from a raw
// GCP SKU map[string]any returned by the Billing Catalog API.
func skuPrice(sku map[string]any) float64 {
	pi, _ := sku["pricingInfo"].([]any)
	if len(pi) == 0 {
		return 0
	}
	pe, _ := pi[0].(map[string]any)
	if pe == nil {
		return 0
	}
	expr, _ := pe["pricingExpression"].(map[string]any)
	if expr == nil {
		return 0
	}
	tiers, _ := expr["tieredRates"].([]any)
	for _, t := range tiers {
		tier, _ := t.(map[string]any)
		if tier == nil {
			continue
		}
		start, _ := tier["startUsageAmount"].(float64)
		if start != 0 {
			continue
		}
		up, _ := tier["unitPrice"].(map[string]any)
		if up == nil {
			continue
		}
		units, _ := up["units"].(string)
		nanos, _ := up["nanos"].(float64)
		price := gcpMoney(units, int(nanos))
		if price > 0 {
			return price
		}
	}
	return 0
}

// --------------------------------------------------------------------------
// Utility
// --------------------------------------------------------------------------

// truncate limits a string to n bytes for error messages.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// unused suppresses "imported and not used" errors for cache import
var _ = cache.DefaultMetadataTTL
