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
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

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

// gcpSKUFetchGroup coalesces concurrent fetchSKUs calls for the same
// serviceID into a single in-flight execution (shared cache-hit unmarshal or
// cache-miss network fetch), the same way AWS's skuCatalogCache uses
// sync.Once per (service, region) key. Without this, callers that fan out
// concurrently over the same candidate service IDs — the raw-SKU-lookup
// region fan-out (compare_bom_regions.go's per-region goroutines) and SKU
// fan-out (get_prices_by_sku's per-SKU goroutines) — can each independently
// re-fetch (on a cold cache) or re-unmarshal (on a warm cache) the same
// multi-thousand-row catalog at the same time instead of sharing one result.
// It is package-level (not per-Provider) since serviceID alone is already
// process-globally unique for this purpose, mirroring skuCatalogCache's own
// process-lifetime scope.
var gcpSKUFetchGroup singleflight.Group

// fetchSKUs returns all SKUs for the given GCP service ID as raw maps.
// Each SKU is a map[string]any matching the JSON shape from the Billing Catalog API.
// Results are cached using the metadata TTL. Concurrent calls for the same
// serviceID are coalesced via gcpSKUFetchGroup — see its doc comment.
func (p *Provider) fetchSKUs(ctx context.Context, serviceID string) ([]map[string]any, error) {
	v, err, _ := gcpSKUFetchGroup.Do(serviceID, func() (any, error) {
		return p.fetchSKUsUncoalesced(ctx, serviceID)
	})
	if err != nil {
		return nil, err
	}
	return v.([]map[string]any), nil
}

// fetchSKUsUncoalesced is fetchSKUs' actual body, called through
// gcpSKUFetchGroup so concurrent callers for the same serviceID share one
// execution instead of each independently hitting the cache/network.
func (p *Provider) fetchSKUsUncoalesced(ctx context.Context, serviceID string) ([]map[string]any, error) {
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

		var body []byte
		err := utils.DoWithRetry(ctx, func(ctx context.Context) error {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return fmt.Errorf("gcp: build SKU request: %w", err)
			}

			// Bearer auth when no API key is configured.
			if p.cfg.GCPAPIKey == "" {
				if err := p.auth.addBearerHeader(ctx, req); err != nil {
					return fmt.Errorf("gcp: auth: %w", err)
				}
			}

			resp, err := p.httpClient.Do(req)
			if err != nil {
				return fmt.Errorf("gcp: SKU fetch: %w", err)
			}
			respBody, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				return fmt.Errorf("gcp: SKU read body: %w", err)
			}
			if resp.StatusCode != http.StatusOK {
				return &utils.HTTPStatusError{
					StatusCode: resp.StatusCode,
					Message:    fmt.Sprintf("gcp: SKU catalog HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 200)),
				}
			}
			body = respBody
			return nil
		})
		if err != nil {
			return nil, err
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

// stampGlobalScope marks price as region-invariant: it sets Region="global"
// and Attributes["scope"]="global" (allocating Attributes if it is nil).
// This gives every pricing function for a region-invariant service (e.g.
// Cloud KMS in gcp_kms.go, External IP Charge in gcp_networking.go) a single
// canonical way to tag a price as global-scoped instead of each hand-rolling
// both fields independently.
func stampGlobalScope(price *models.NormalizedPrice) {
	price.Region = "global"
	if price.Attributes == nil {
		price.Attributes = map[string]string{}
	}
	price.Attributes["scope"] = "global"
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

// tierRate is one parsed (startUsageAmount, unitPrice) pair from a raw GCP
// SKU's pricingInfo[0].pricingExpression.tieredRates.
type tierRate struct {
	start float64
	price float64
}

// gcpPricingExpression unwraps a raw GCP SKU's
// pricingInfo[0].pricingExpression object — the single shared JSON-unwrap
// step behind every reader of that object (skuTierList below, and
// gcpSKUUnit in gcp_sku_lookup.go), so a future change to the
// pricingInfo/pricingExpression JSON shape (or a bugfix to the unwrap logic)
// only needs to happen in one place instead of silently drifting between
// independently-reimplemented copies.
func gcpPricingExpression(sku map[string]any) map[string]any {
	pi, _ := sku["pricingInfo"].([]any)
	if len(pi) == 0 {
		return nil
	}
	pe, _ := pi[0].(map[string]any)
	if pe == nil {
		return nil
	}
	expr, _ := pe["pricingExpression"].(map[string]any)
	return expr
}

// skuTierList parses every tiered unit price out of a raw GCP SKU
// (map[string]any as returned by the Billing Catalog API), sorted ascending
// by startUsageAmount (ties keep their original relative order). It is the
// single shared JSON-unwrap step behind skuPrice (below — first zero-start
// tier), skuPaidPrice (gcp_ai.go — first tier with startUsageAmount > 0), and
// skuAllTierRates (gcp_dns.go — every tier, for SKUs with more than two
// tiers); previously each reimplemented this same
// pricingInfo->pricingExpression->tieredRates unwrap independently.
func skuTierList(sku map[string]any) []tierRate {
	expr := gcpPricingExpression(sku)
	if expr == nil {
		return nil
	}
	tiers, _ := expr["tieredRates"].([]any)

	out := make([]tierRate, 0, len(tiers))
	for _, t := range tiers {
		tier, _ := t.(map[string]any)
		if tier == nil {
			continue
		}
		up, _ := tier["unitPrice"].(map[string]any)
		if up == nil {
			continue
		}
		start, _ := tier["startUsageAmount"].(float64)
		units, _ := up["units"].(string)
		nanos, _ := up["nanos"].(float64)
		out = append(out, tierRate{start: start, price: gcpMoney(units, int(nanos))})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

// skuPrice extracts the first-tier unit price (startUsageAmount == 0) from a raw
// GCP SKU map[string]any returned by the Billing Catalog API.
func skuPrice(sku map[string]any) float64 {
	for _, t := range skuTierList(sku) {
		if t.start != 0 {
			continue
		}
		if t.price > 0 {
			return t.price
		}
	}
	return 0
}

// newGCPBasePrice builds the region-less NormalizedPrice base shape shared by
// every GCP price constructor in this package (Provider=GCP,
// PricingTerm=OnDemand, Currency=USD, plus the SKU-identifying fields) —
// previously hand-built independently by newGlobalScopedPrice (below),
// newFirestorePrice (gcp_firestore.go), and gcpBuildMatchedPrices
// (gcp_sku_lookup.go), which risked a future change to this common shape
// (e.g. a new field, or a Currency/PricingTerm default fix) being applied to
// some of those call sites but missed in others. Callers fill in Region (and
// any scope stamping) themselves, since that varies per constructor.
func newGCPBasePrice(service, productFamily, skuID, description string, pricePerUnit float64, unit models.PriceUnit, attrs map[string]string) models.NormalizedPrice {
	return models.NormalizedPrice{
		Provider:      models.CloudProviderGCP,
		Service:       service,
		SKUID:         skuID,
		ProductFamily: productFamily,
		Description:   description,
		PricingTerm:   models.PricingTermOnDemand,
		PricePerUnit:  pricePerUnit,
		Unit:          unit,
		Currency:      "USD",
		Attributes:    attrs,
	}
}

// newGlobalScopedPrice builds a global-scoped (Region="global",
// Attributes["scope"]="global") NormalizedPrice with Provider=GCP,
// PricingTerm=OnDemand, and Currency=USD already filled in — the fields
// shared by every region-invariant GCP pricing domain (Cloud DNS, Cloud
// Pub/Sub, ...). Domain-specific constructors like newDNSPrice/newPubSubPrice
// wrap this with their fixed Service/ProductFamily so call sites keep their
// existing, domain-named entry point.
func newGlobalScopedPrice(service, productFamily, skuID, description string, pricePerUnit float64, unit models.PriceUnit, attrs map[string]string) *models.NormalizedPrice {
	price := newGCPBasePrice(service, productFamily, skuID, description, pricePerUnit, unit, attrs)
	stampGlobalScope(&price)
	return &price
}

// isGlobalSKU reports whether a raw GCP SKU's geoTaxonomy defensively
// qualifies as GLOBAL-scoped: true if geoTaxonomy is absent, or if
// geoTaxonomy.type is empty or exactly "GLOBAL". A SKU whose geoTaxonomy.type
// is present and any other value (e.g. "REGIONAL") is NOT global. Shared by
// every region-invariant pricing function (Cloud DNS, Cloud Pub/Sub, ...)
// that matches SKUs by description substring and defensively guards against
// a regional SKU that happens to share matching wording.
func isGlobalSKU(sku map[string]any) bool {
	geo, ok := sku["geoTaxonomy"].(map[string]any)
	if !ok {
		return true
	}
	geoType, _ := geo["type"].(string)
	return geoType == "" || geoType == "GLOBAL"
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
