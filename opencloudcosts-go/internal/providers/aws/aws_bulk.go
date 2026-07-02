package aws

// aws_bulk.go implements the public bulk HTTP pricing fallback for when AWS
// credentials are unavailable. The AWS Pricing API requires credentials, but
// the same data is available as public JSON files at:
//   https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/{service}/current/{region}/index.json
//
// These files can be 100-200 MB for large services (e.g. AmazonEC2), so we
// stream them with json.Decoder rather than loading them entirely into memory.

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"context"

	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
)

// bulkPricingBaseURL is the base URL for the AWS public bulk pricing endpoint.
// It is a package-level variable so tests can override it with an httptest.Server.
var bulkPricingBaseURL = "https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws"

// attrFilter is a parsed TERM_MATCH filter used when matching bulk product entries.
type attrFilter struct {
	field string
	value string
}

// awsLocationToRegion maps AWS pricing API display names (as produced by
// _regionToLocation in aws_pricing.go) to region codes. Built by inverting the
// existing map so the two can never drift.
var awsLocationToRegion = func() map[string]string {
	m := make(map[string]string, len(_regionToLocation))
	for code, loc := range _regionToLocation {
		m[loc] = code
	}
	return m
}()

// extractRegionFromFilters finds the "location" filter value and maps it to a
// region code. Falls back to "us-east-1" if not found.
func extractRegionFromFilters(filters []pricingtypes.Filter) string {
	for _, f := range filters {
		if f.Field != nil && *f.Field == "location" && f.Value != nil {
			if r, ok := awsLocationToRegion[*f.Value]; ok {
				return r
			}
		}
	}
	return "us-east-1"
}

// getProductsBulk fetches pricing data from the public AWS bulk pricing endpoint
// and returns JSON strings in the same format as the Pricing SDK's GetProducts.
//
// The bulk JSON has this top-level structure:
//
//	{
//	  "products": {
//	    "{sku}": {"sku":"...","productFamily":"...","attributes":{...}}
//	  },
//	  "terms": {
//	    "OnDemand": { "{sku}.{termCode}": {...} },
//	    "Reserved": { "{sku}.{termCode}": {...} }
//	  }
//	}
//
// We make one HTTP request, then stream the body in two logical passes over the
// already-buffered bytes: first to collect matching products, then to collect
// their terms.
func (p *Provider) getProductsBulk(
	ctx context.Context,
	serviceCode string,
	filters []pricingtypes.Filter,
	maxResults int32,
	region string,
) ([]string, error) {
	url := fmt.Sprintf("%s/%s/current/%s/index.json", bulkPricingBaseURL, serviceCode, region)

	// Use a longer timeout for large files (100-200 MB).
	bulkClient := &http.Client{Timeout: 120 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("aws bulk: build request: %w", err)
	}
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := bulkClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("aws bulk: fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("aws bulk: unexpected status %d for %s", resp.StatusCode, url)
	}

	var bodyReader io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("aws bulk: gzip reader: %w", err)
		}
		defer func() { _ = gz.Close() }()
		bodyReader = gz
	}

	// Build attribute filters (exclude "location" — it selects the URL, not an attr).
	var attrFilters []attrFilter
	for _, f := range filters {
		if f.Field == nil || f.Value == nil {
			continue
		}
		if *f.Field == "location" {
			continue
		}
		attrFilters = append(attrFilters, attrFilter{field: *f.Field, value: *f.Value})
	}

	// --- Pass 1: stream through "products", collect matching SKUs. ---
	//
	// The bulk JSON structure at the products level is:
	//   { "products": { "SKUABC": { "sku":"SKUABC","productFamily":"...","attributes":{...} }, ... }, ... }
	//
	// We stream token-by-token to avoid loading the whole file. We capture each
	// product as json.RawMessage and decode only the fields we need to filter.
	// We must drain ALL products regardless of maxResults — we need to reach the
	// "terms" section that follows in the byte stream.

	dec := json.NewDecoder(bodyReader)

	// Advance past the opening "{" of the top-level object.
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("aws bulk: expected top-level object: %v", err)
	}

	// rawProducts maps sku -> raw product JSON (only for matched SKUs).
	rawProducts := make(map[string]json.RawMessage)
	// rawOnDemand/rawReserved map sku -> (termKey -> raw term JSON). Keeping the
	// SKU-level grouping that collectTermsForSKUs already produces (rather than
	// flattening into one big map keyed by termKey) lets the assembly pass below
	// do an O(1) per-SKU lookup instead of an O(M) linear scan per SKU, which
	// matters for large offer files like AmazonEC2's (~120K products/terms) —
	// see collectTermsForSKUs for details.
	var rawOnDemand map[string]map[string]json.RawMessage
	var rawReserved map[string]map[string]json.RawMessage

	for dec.More() {
		// Read the top-level key (e.g. "formatVersion", "disclaimer", "products", "terms").
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("aws bulk: reading top-level key: %w", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("aws bulk: expected string key, got %T", keyTok)
		}

		switch key {
		case "products":
			// Advance past opening "{" of products object.
			if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
				return nil, fmt.Errorf("aws bulk: expected products object: %v", err)
			}
			for dec.More() {
				// SKU key (e.g. "ABC123DEF").
				skuTok, err := dec.Token()
				if err != nil {
					return nil, fmt.Errorf("aws bulk: reading sku key: %w", err)
				}
				sku, ok := skuTok.(string)
				if !ok {
					return nil, fmt.Errorf("aws bulk: expected string sku key, got %T", skuTok)
				}

				var raw json.RawMessage
				if err := dec.Decode(&raw); err != nil {
					return nil, fmt.Errorf("aws bulk: decoding product %s: %w", sku, err)
				}

				// Only parse and match if we still need more results.
				// We must still Decode above to advance the stream regardless.
				if len(rawProducts) >= int(maxResults) {
					continue
				}

				if matchesProduct(raw, attrFilters) {
					rawProducts[sku] = raw
				}
			}
			// Consume closing "}" of products.
			if _, err := dec.Token(); err != nil {
				return nil, fmt.Errorf("aws bulk: closing products: %w", err)
			}

		case "terms":
			// terms: { "OnDemand": { ... }, "Reserved": { ... } }
			if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
				return nil, fmt.Errorf("aws bulk: expected terms object: %v", err)
			}
			for dec.More() {
				termTypeTok, err := dec.Token()
				if err != nil {
					return nil, fmt.Errorf("aws bulk: reading term type: %w", err)
				}
				termType, ok := termTypeTok.(string)
				if !ok {
					return nil, fmt.Errorf("aws bulk: expected string term type, got %T", termTypeTok)
				}

				termEntries, err := collectTermsForSKUs(dec, rawProducts)
				if err != nil {
					return nil, fmt.Errorf("aws bulk: collecting %s terms: %w", termType, err)
				}

				switch termType {
				case "OnDemand":
					rawOnDemand = termEntries
				case "Reserved":
					rawReserved = termEntries
				}
			}
			// Consume closing "}" of terms.
			if _, err := dec.Token(); err != nil {
				return nil, fmt.Errorf("aws bulk: closing terms: %w", err)
			}

		default:
			// Skip unknown top-level fields (formatVersion, disclaimer, etc.).
			var discard json.RawMessage
			if err := dec.Decode(&discard); err != nil {
				return nil, fmt.Errorf("aws bulk: skipping field %q: %w", key, err)
			}
		}
	}

	// --- Assemble per-SKU result JSON matching parsedSKU format. ---
	//
	// Output format:
	//   {"product":{...},"terms":{"OnDemand":{...},"Reserved":{...}}}
	//
	// This is exactly what callers json.Unmarshal into parsedSKU.

	type termsJSON struct {
		OnDemand map[string]json.RawMessage `json:"OnDemand,omitempty"`
		Reserved map[string]json.RawMessage `json:"Reserved,omitempty"`
	}
	type skuJSON struct {
		Product json.RawMessage `json:"product"`
		Terms   termsJSON       `json:"terms"`
	}

	results := make([]string, 0, len(rawProducts))
	for sku, productRaw := range rawProducts {
		terms := termsJSON{}

		// Direct O(1) per-SKU lookup instead of scanning the whole term map —
		// see the rawOnDemand/rawReserved comment above.
		if odTerms := rawOnDemand[sku]; len(odTerms) > 0 {
			terms.OnDemand = odTerms
		}
		if resTerms := rawReserved[sku]; len(resTerms) > 0 {
			terms.Reserved = resTerms
		}

		out, err := json.Marshal(skuJSON{Product: productRaw, Terms: terms})
		if err != nil {
			continue
		}
		results = append(results, string(out))
	}

	return results, nil
}

// matchesProduct returns true if the raw product JSON satisfies all attrFilters.
// Filters are matched against the product's "attributes" map AND the top-level
// "productFamily" field (which is a sibling of "attributes", not inside it).
func matchesProduct(raw json.RawMessage, attrFilters []attrFilter) bool {
	if len(attrFilters) == 0 {
		return true
	}

	// Decode only the fields we need.
	var product struct {
		ProductFamily string            `json:"productFamily"`
		Attributes    map[string]string `json:"attributes"`
	}
	if err := json.Unmarshal(raw, &product); err != nil {
		return false
	}

	// Build a combined lookup: attributes + top-level productFamily.
	lookup := make(map[string]string, len(product.Attributes)+1)
	for k, v := range product.Attributes {
		lookup[k] = v
	}
	if product.ProductFamily != "" {
		lookup["productFamily"] = product.ProductFamily
	}

	for _, f := range attrFilters {
		if lookup[f.field] != f.value {
			return false
		}
	}
	return true
}

// collectTermsForSKUs streams through a term-type object (e.g. the value of
// "OnDemand") and returns only inner offer-term entries whose parent SKU matches
// one of the provided SKUs. The decoder must be positioned immediately before
// the opening "{" of the term-type object.
//
// The AWS bulk file uses two levels of nesting:
//
//	"OnDemand": {
//	  "SKU": {                      ← outer key = SKU
//	    "SKU.termCode": {offerTerm} ← inner key = offer term entry
//	  }
//	}
//
// The returned map preserves this SKU-level grouping (sku -> termKey -> raw
// term JSON) instead of flattening every SKU's terms into one map keyed by
// "SKU.termCode". Flattening would force callers back to an O(M) linear
// "strings.HasPrefix(termKey, sku+\".\")" scan per SKU to re-derive exactly
// the grouping we already have here for free while walking the stream —
// that O(P×M) rejoin is what caused get_price_by_sku's EC2/EBS timeouts
// (~120K products/terms in the AmazonEC2 offer file). Keeping the grouping
// lets callers do a direct O(1) map[sku] lookup instead.
func collectTermsForSKUs(dec *json.Decoder, skus map[string]json.RawMessage) (map[string]map[string]json.RawMessage, error) {
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("expected term-type object")
	}

	result := make(map[string]map[string]json.RawMessage)
	for dec.More() {
		// Outer key = SKU (e.g. "DW64VZC89TS9M2P2").
		outerKeyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("reading outer term key: %w", err)
		}
		outerKey, ok := outerKeyTok.(string)
		if !ok {
			return nil, fmt.Errorf("expected string outer term key, got %T", outerKeyTok)
		}

		_, matched := skus[outerKey]
		if !matched {
			// Not a SKU we care about — skip the entire inner object.
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, fmt.Errorf("aws bulk: skipping terms for sku %s: %w", outerKey, err)
			}
			continue
		}

		// Matched SKU: open the inner object and collect each "SKU.termCode" entry.
		if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
			return nil, fmt.Errorf("aws bulk: expected inner term object for sku %s", outerKey)
		}
		innerTerms := make(map[string]json.RawMessage)
		for dec.More() {
			innerKeyTok, err := dec.Token()
			if err != nil {
				return nil, fmt.Errorf("aws bulk: reading inner term key for sku %s: %w", outerKey, err)
			}
			innerKey, ok := innerKeyTok.(string)
			if !ok {
				return nil, fmt.Errorf("aws bulk: expected string inner term key for sku %s, got %T", outerKey, innerKeyTok)
			}
			var termRaw json.RawMessage
			if err := dec.Decode(&termRaw); err != nil {
				return nil, fmt.Errorf("aws bulk: decoding term %s: %w", innerKey, err)
			}
			innerTerms[innerKey] = termRaw
		}
		if _, err := dec.Token(); err != nil {
			return nil, fmt.Errorf("aws bulk: closing inner term object for sku %s: %w", outerKey, err)
		}
		if len(innerTerms) > 0 {
			result[outerKey] = innerTerms
		}
	}

	// Consume closing "}" of the term-type object.
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("closing term-type object: %w", err)
	}
	return result, nil
}
