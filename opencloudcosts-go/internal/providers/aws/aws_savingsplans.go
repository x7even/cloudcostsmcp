package aws

// aws_savingsplans.go implements AWS Savings Plans pricing via the public bulk
// JSON API at https://pricing.us-east-1.amazonaws.com/savingsPlan/v1.0/
//
// Key API facts:
//   - Base URL uses singular "savingsPlan" (NOT plural "savingsPlans" — that returns 404)
//   - ISP and CSP pricing are in the SAME file (AWSComputeSavingsPlan)
//   - products[] is an ARRAY (unlike the EC2 bulk file where it's a SKU-keyed map)
//   - terms.savingsPlan[] is also an ARRAY
//   - The per-region files can be extremely large (us-east-1 = ~411 MB)
//   - HTTP Range requests are NOT supported; stream-parse with json.Decoder

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// spIndexBaseURL is the base URL for the SP pricing index.
// It is a package-level variable so tests can override it with an httptest.Server.
var spIndexBaseURL = "https://pricing.us-east-1.amazonaws.com/savingsPlan/v1.0/aws/AWSComputeSavingsPlan"

// spHTTPTimeout is the HTTP timeout for SP bulk file fetches.
// The us-east-1 file is ~411 MB so needs a long timeout.
const spHTTPTimeout = 300 * time.Second

// --------------------------------------------------------------------------
// Internal types for stream-parsing the SP bulk JSON
// --------------------------------------------------------------------------

// spRegionIndexEntry is one entry in the region_index.json file.
type spRegionIndexEntry struct {
	RegionCode string `json:"regionCode"`
	VersionURL string `json:"versionUrl"`
}

// spRegionIndex is the top-level structure of the region_index.json file.
type spRegionIndexFile struct {
	Regions []spRegionIndexEntry `json:"regions"`
}

// spProduct represents one entry from the products[] array in the SP bulk file.
type spProduct struct {
	SKU           string            `json:"sku"`
	ProductFamily string            `json:"productFamily"`
	Attributes    map[string]string `json:"attributes"`
}

// spRateDiscount is the nested DiscountedRate within a rate entry.
type spRateDiscount struct {
	Price    string `json:"price"`
	Currency string `json:"currency"`
}

// spRate represents one rate entry within a term.
type spRate struct {
	DiscountedSku          string         `json:"discountedSku"`
	DiscountedUsageType    string         `json:"discountedUsageType"`
	DiscountedOperation    string         `json:"discountedOperation"`
	DiscountedServiceCode  string         `json:"discountedServiceCode"`
	RateCode               string         `json:"rateCode"`
	Unit                   string         `json:"unit"`
	DiscountedRate         spRateDiscount `json:"discountedRate"`
	DiscountedRegionCode   string         `json:"discountedRegionCode"`
	DiscountedInstanceType string         `json:"discountedInstanceType"`
}

// spLeaseLength holds the lease contract length info within a term.
type spLeaseLength struct {
	Duration int    `json:"duration"`
	Unit     string `json:"unit"`
}

// spTerm represents one entry from the terms.savingsPlan[] array.
type spTerm struct {
	SKU                  string        `json:"sku"`
	Description          string        `json:"description"`
	EffectiveDate        string        `json:"effectiveDate"`
	LeaseContractLength  spLeaseLength `json:"leaseContractLength"`
	Rates                []spRate      `json:"rates"`
}

// spProductMeta holds the classification data extracted from a product entry.
type spProductMeta struct {
	spType        string // "csp" or "isp"
	purchaseOption string
	purchaseTerm  string // "1yr" or "3yr"
	instanceFamily string // ISP only (e.g. "m7gd" from attribute instanceType)
	productFamily string
}

// spRateKey is the lookup key for the in-memory SP rate index.
// It is serialised to a string (via spRateKeyString) for JSON-safe map keys.
type spRateKey struct {
	SPType        string // "csp" or "isp"
	Years         int    // 1 or 3
	PaymentOption string // "No Upfront", "Partial Upfront", "All Upfront"
	Operation     string // e.g. "RunInstances", "RunInstances:0010"
	InstanceType  string // specific instance type (e.g. "m5.xlarge") — populated for both CSP and ISP
}

// spRateKeyString serialises a spRateKey to a string suitable for use as a
// JSON map key. Format: "{spType}|{years}|{paymentOption}|{operation}|{instanceType}".
func spRateKeyString(k spRateKey) string {
	return fmt.Sprintf("%s|%d|%s|%s|%s", k.SPType, k.Years, k.PaymentOption, k.Operation, k.InstanceType)
}

// spIndex is the in-memory cache type for SP rates. String keys are JSON-safe
// (unlike struct keys which cannot be marshalled to JSON objects).
type spIndex = map[string]spIndexEntry

// spIndexEntry holds the data stored per rate in the in-memory index.
type spIndexEntry struct {
	Price          float64   `json:"price"`
	DiscountedSku  string    `json:"discounted_sku"`
	Currency       string    `json:"currency"`
	EffectiveDate  time.Time `json:"effective_date"`
	SourceURL      string    `json:"source_url"`
	ProductFamily  string    `json:"product_family"`
}

// --------------------------------------------------------------------------
// URL resolution
// --------------------------------------------------------------------------

// fetchSPRegionIndex fetches the SP region index and returns the URL of the
// per-region file for the given region. Returns an error if the region is not found.
func fetchSPRegionIndex(ctx context.Context, region string, client *http.Client) (string, error) {
	indexURL := spIndexBaseURL + "/current/region_index.json"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, indexURL, nil)
	if err != nil {
		return "", fmt.Errorf("aws sp: build region index request: %w", err)
	}
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("aws sp: fetch region index: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("aws sp: region index returned status %d", resp.StatusCode)
	}

	var bodyReader io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, gzErr := gzip.NewReader(resp.Body)
		if gzErr != nil {
			return "", fmt.Errorf("aws sp: gzip region index: %w", gzErr)
		}
		defer func() { _ = gz.Close() }()
		bodyReader = gz
	}

	var idx spRegionIndexFile
	if err := json.NewDecoder(bodyReader).Decode(&idx); err != nil {
		return "", fmt.Errorf("aws sp: decode region index: %w", err)
	}

	for _, entry := range idx.Regions {
		if entry.RegionCode == region {
			vURL := entry.VersionURL
			// If the versionUrl is relative, prepend the base pricing host.
			if !strings.HasPrefix(vURL, "http") {
				vURL = "https://pricing.us-east-1.amazonaws.com" + vURL
			}
			return vURL, nil
		}
	}
	return "", fmt.Errorf("aws sp: region %q not found in SP region index (105 regions listed)", region)
}

// --------------------------------------------------------------------------
// SP index build — stream-parse the large per-region file
// --------------------------------------------------------------------------

// fetchAndBuildSPIndex fetches and stream-parses the per-region SP bulk file,
// returning an in-memory index. Scope: payment_option='No Upfront' only for v1.
// The file structure is:
//
//	{ "products": [ {...}, ... ], "terms": { "savingsPlan": [ {...}, ... ] } }
//
// products is an ARRAY (unlike EC2 bulk where it is a map).
func fetchAndBuildSPIndex(ctx context.Context, regionURL string, client *http.Client) (spIndex, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, regionURL, nil)
	if err != nil {
		return nil, fmt.Errorf("aws sp: build per-region request: %w", err)
	}
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("aws sp: fetch per-region file: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("aws sp: per-region file returned status %d", resp.StatusCode)
	}

	var bodyReader io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, gzErr := gzip.NewReader(resp.Body)
		if gzErr != nil {
			return nil, fmt.Errorf("aws sp: gzip per-region: %w", gzErr)
		}
		defer func() { _ = gz.Close() }()
		bodyReader = gz
	}

	dec := json.NewDecoder(bodyReader)

	// Advance past the opening "{" of the top-level object.
	if tok, tokErr := dec.Token(); tokErr != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("aws sp: expected top-level object")
	}

	// productMeta maps SKU -> spProductMeta built from the products array.
	productMeta := make(map[string]spProductMeta)
	index := make(spIndex)

	for dec.More() {
		keyTok, keyErr := dec.Token()
		if keyErr != nil {
			return nil, fmt.Errorf("aws sp: reading top-level key: %w", keyErr)
		}
		topKey, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("aws sp: expected string key, got %T", keyTok)
		}

		switch topKey {
		case "products":
			// products is an ARRAY of product objects (not a map like EC2 bulk).
			if tok, tokErr := dec.Token(); tokErr != nil || tok != json.Delim('[') {
				return nil, fmt.Errorf("aws sp: expected products array")
			}
			for dec.More() {
				var prod spProduct
				if decErr := dec.Decode(&prod); decErr != nil {
					return nil, fmt.Errorf("aws sp: decoding product: %w", decErr)
				}
				meta := classifyProduct(prod)
				if meta != nil {
					productMeta[prod.SKU] = *meta
				}
			}
			// Consume closing "]" of products array.
			if _, closErr := dec.Token(); closErr != nil {
				return nil, fmt.Errorf("aws sp: closing products array: %w", closErr)
			}

		case "terms":
			// terms: { "savingsPlan": [ {...}, ... ] }
			if tok, tokErr := dec.Token(); tokErr != nil || tok != json.Delim('{') {
				return nil, fmt.Errorf("aws sp: expected terms object")
			}
			for dec.More() {
				termTypeTok, ttErr := dec.Token()
				if ttErr != nil {
					return nil, fmt.Errorf("aws sp: reading term type key: %w", ttErr)
				}
				termType, ok := termTypeTok.(string)
				if !ok {
					return nil, fmt.Errorf("aws sp: expected string term type, got %T", termTypeTok)
				}

				if termType == "savingsPlan" {
					// savingsPlan: array of term objects.
					if tok, tokErr := dec.Token(); tokErr != nil || tok != json.Delim('[') {
						return nil, fmt.Errorf("aws sp: expected savingsPlan array")
					}
					for dec.More() {
						var term spTerm
						if decErr := dec.Decode(&term); decErr != nil {
							return nil, fmt.Errorf("aws sp: decoding term: %w", decErr)
						}
						meta, hasMeta := productMeta[term.SKU]
						if !hasMeta {
							continue
						}
						effectiveDate := parseEffectiveDate(term.EffectiveDate)
						years := termToYears(term.LeaseContractLength)
						if years == 0 {
							continue
						}
						for _, rate := range term.Rates {
							// Skip dedicated-tenancy entries to prevent collision with
							// shared-tenancy entries that share the same
							// (SPType, Years, PaymentOption, Operation, InstanceType) key.
							// In the AWS SP bulk file, DedicatedUsage:m5.xlarge and
							// BoxUsage:m5.xlarge are separate rate entries but map to the
							// same index key; keeping only BoxUsage (shared tenancy) entries
							// ensures callers always receive the shared-tenancy rate.
							if strings.Contains(rate.DiscountedUsageType, "Dedicated") {
								continue
							}
							price, parseErr := strconv.ParseFloat(rate.DiscountedRate.Price, 64)
							if parseErr != nil {
								continue
							}
							key := spRateKeyString(spRateKey{
								SPType:        meta.spType,
								Years:         years,
								PaymentOption: meta.purchaseOption,
								Operation:     rate.DiscountedOperation,
								InstanceType:  rate.DiscountedInstanceType,
							})
							index[key] = spIndexEntry{
								Price:         price,
								DiscountedSku: rate.DiscountedSku,
								Currency:      rate.DiscountedRate.Currency,
								EffectiveDate: effectiveDate,
								SourceURL:     regionURL,
								ProductFamily: meta.productFamily,
							}
						}
					}
					// Consume closing "]" of savingsPlan array.
					if _, closErr := dec.Token(); closErr != nil {
						return nil, fmt.Errorf("aws sp: closing savingsPlan array: %w", closErr)
					}
				} else {
					// Skip other term types.
					var discard json.RawMessage
					if decErr := dec.Decode(&discard); decErr != nil {
						return nil, fmt.Errorf("aws sp: skipping term type %q: %w", termType, decErr)
					}
				}
			}
			// Consume closing "}" of terms object.
			if _, closErr := dec.Token(); closErr != nil {
				return nil, fmt.Errorf("aws sp: closing terms object: %w", closErr)
			}

		default:
			// Skip other top-level fields (version, disclaimer, etc.).
			var discard json.RawMessage
			if decErr := dec.Decode(&discard); decErr != nil {
				return nil, fmt.Errorf("aws sp: skipping top-level field %q: %w", topKey, decErr)
			}
		}
	}

	return index, nil
}

// classifyProduct extracts metadata from a product entry and classifies it as
// CSP or ISP. Returns nil for unrecognized product families.
func classifyProduct(prod spProduct) *spProductMeta {
	pf := prod.ProductFamily
	attrs := prod.Attributes

	// EC2 Instance Savings Plan: productFamily typically contains "EC2 Instance"
	// CSP: productFamily typically "ComputeSavingsPlans" or "Compute Savings Plans"
	pfLower := strings.ToLower(pf)

	var spType string
	switch {
	case strings.Contains(pfLower, "ec2 instance") || strings.Contains(pfLower, "ec2instance"):
		spType = "isp"
	case strings.Contains(pfLower, "compute"):
		spType = "csp"
	default:
		return nil
	}

	purchaseOption := attrs["purchaseOption"]
	if purchaseOption == "" {
		purchaseOption = attrs["PurchaseOption"]
	}
	purchaseTerm := attrs["purchaseTerm"]
	if purchaseTerm == "" {
		purchaseTerm = attrs["PurchaseTerm"]
	}

	instanceFamily := ""
	if spType == "isp" {
		instanceFamily = attrs["instanceType"] // attribute is family name for ISP products
	}

	return &spProductMeta{
		spType:         spType,
		purchaseOption: purchaseOption,
		purchaseTerm:   purchaseTerm,
		instanceFamily: instanceFamily,
		productFamily:  pf,
	}
}

// termToYears converts a spLeaseLength to an integer year count (1 or 3).
// Returns 0 for unrecognized formats.
func termToYears(lease spLeaseLength) int {
	switch {
	case lease.Duration == 1 && strings.EqualFold(lease.Unit, "year"):
		return 1
	case lease.Duration == 3 && strings.EqualFold(lease.Unit, "year"):
		return 3
	default:
		return 0
	}
}

// parseEffectiveDate parses the effectiveDate string from SP JSON.
func parseEffectiveDate(s string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// --------------------------------------------------------------------------
// Cache management for the SP index
// --------------------------------------------------------------------------

// spCacheKeyForRegion returns the cache key for a region's SP index.
func spCacheKeyForRegion(region string) string {
	return "aws:sp_index:" + region
}

// getSPIndex returns the SP rate index for a region, building and caching it
// if necessary. The index maps string keys (via spRateKeyString) -> spIndexEntry.
func (p *Provider) getSPIndex(ctx context.Context, region string) (spIndex, error) {
	cacheKey := spCacheKeyForRegion(region)

	if cached, ok := p.cache.Get(cacheKey); ok {
		var index spIndex
		if err := json.Unmarshal(cached, &index); err == nil && len(index) > 0 {
			return index, nil
		}
	}

	// Dedicated long-timeout client for large SP file fetches.
	spClient := &http.Client{Timeout: spHTTPTimeout}

	regionURL, err := fetchSPRegionIndex(ctx, region, spClient)
	if err != nil {
		return nil, err
	}

	index, err := fetchAndBuildSPIndex(ctx, regionURL, spClient)
	if err != nil {
		return nil, err
	}

	// Marshal and cache with 24h TTL.
	if data, marshalErr := json.Marshal(index); marshalErr == nil {
		ttl := 24 * time.Hour
		p.cache.Set(cacheKey, data, ttl)
	}

	return index, nil
}

// --------------------------------------------------------------------------
// OS to EC2 operation code mapping
// --------------------------------------------------------------------------

// osToOperation maps OS name to EC2 operation code used in SP rate entries.
func osToOperation(os string) string {
	switch strings.ToLower(os) {
	case "windows":
		return "RunInstances:0010"
	case "rhel", "red hat":
		return "RunInstances:000g"
	case "suse", "sles":
		return "RunInstances:0102"
	default:
		// Linux/UNIX — bare "RunInstances" with no suffix.
		return "RunInstances"
	}
}

// --------------------------------------------------------------------------
// GetSavingsPlanPrice — entry point for CSP and ISP pricing
// --------------------------------------------------------------------------

// GetSavingsPlanPrice returns SP pricing (and optional EDP-adjusted pricing)
// for an EC2 instance type. It builds and caches the SP rate index on first call.
//
// Both CSP and ISP rates are looked up by operation + specific instance type.
// Real AWS SP bulk data has discountedInstanceType populated for every rate
// entry, including CSP. The CSP vs ISP distinction is captured by the SPType
// field of the key, not by leaving instanceType empty.
func (p *Provider) GetSavingsPlanPrice(
	ctx context.Context,
	spec *models.ComputePricingSpec,
) (*models.PricingResult, error) {
	region := spec.GetRegion()
	if region == "" {
		region = "us-east-1"
	}
	instanceType := spec.ResourceType
	os := spec.OS
	if os == "" {
		os = "Linux"
	}

	// Determine SP type from term.
	var spType string
	switch spec.GetTerm() {
	case models.PricingTermComputeSP:
		spType = "csp"
	case models.PricingTermEC2InstanceSP:
		spType = "isp"
	default:
		return nil, fmt.Errorf("aws sp: GetSavingsPlanPrice called with non-SP term %q", spec.GetTerm())
	}

	// Resolve payment option and commitment years with defaults.
	paymentOption := "No Upfront"
	if spec.PaymentOption != nil && *spec.PaymentOption != "" {
		normalized, normErr := models.NormalizePaymentOption(*spec.PaymentOption)
		if normErr != nil {
			return nil, normErr
		}
		paymentOption = normalized
	}

	years := 1
	if spec.CommitmentYears != nil {
		years = *spec.CommitmentYears
		if years != 1 && years != 3 {
			return nil, fmt.Errorf("aws sp: commitment_years must be 1 or 3, got %d", years)
		}
	}

	operation := osToOperation(os)

	// Fetch/build the SP index.
	index, err := p.getSPIndex(ctx, region)
	if err != nil {
		return nil, fmt.Errorf("aws sp: getSPIndex: %w", err)
	}

	// Build lookup key.
	// Both CSP and ISP rates are indexed with the specific discountedInstanceType
	// from the SP bulk file. Real AWS SP rates carry discountedInstanceType for
	// every rate entry including CSP (e.g. "m5.xlarge"). The CSP/ISP distinction
	// is captured by SPType; using the instance type in both avoids false matches.
	keyStr := spRateKeyString(spRateKey{
		SPType:        spType,
		Years:         years,
		PaymentOption: paymentOption,
		Operation:     operation,
		InstanceType:  instanceType,
	})

	entry, found := index[keyStr]
	if !found {
		notCoveredNote := fmt.Sprintf(
			"instance type %q with OS %q is not covered by %s in region %q "+
				"(payment_option=%q, commitment_years=%d)",
			instanceType, os, spec.GetTerm(), region, paymentOption, years,
		)
		return &models.PricingResult{
			PublicPrices:  nil,
			AuthAvailable: false,
			Note:          notCoveredNote,
			Source:        "aws_sp_bulk",
			SchemaVersion: "1",
		}, nil
	}

	now := time.Now()
	spRate := entry.Price

	// Build the SP NormalizedPrice.
	spAttrs := map[string]string{
		"sp_type":         spType,
		"commitment_years": strconv.Itoa(years),
		"payment_option":  paymentOption,
		"instance_type":   instanceType,
		"os":              os,
		"operation":       operation,
	}
	if spType == "isp" && instanceType != "" {
		// Extract instance family for informational purposes.
		family := instanceFamily(instanceType)
		spAttrs["instance_family"] = family
	}
	effectiveDate := entry.EffectiveDate
	spPrice := models.NormalizedPrice{
		Provider:      models.CloudProviderAWS,
		Service:       "ec2",
		SKUID:         entry.DiscountedSku,
		ProductFamily: entry.ProductFamily,
		Description:   fmt.Sprintf("%s %d-year %s for %s (%s)", strings.ToUpper(spType), years, paymentOption, instanceType, os),
		Region:        region,
		Attributes:    spAttrs,
		PricingTerm:   spec.GetTerm(),
		PricePerUnit:  spRate,
		Unit:          models.PriceUnitPerHour,
		Currency:      entry.Currency,
		EffectiveDate: &effectiveDate,
		FetchedAt:     &now,
		SourceURL:     entry.SourceURL,
	}

	result := &models.PricingResult{
		PublicPrices:  []models.NormalizedPrice{spPrice},
		AuthAvailable: false,
		Source:        "aws_sp_bulk",
		SchemaVersion: "1",
	}

	// Attempt to get the on-demand rate for savings % breakdown (best-effort).
	var odRate float64
	odPrices, odErr := p.GetComputePrice(ctx, instanceType, region, os, models.PricingTermOnDemand)
	if odErr == nil && len(odPrices) > 0 {
		odRate = odPrices[0].PricePerUnit
	}

	// Build breakdown.
	breakdown := map[string]any{
		"sp_type":         spType,
		"commitment_years": years,
		"payment_option":  paymentOption,
		"sp_rate":         spRate,
		"edp_note": "EDP is a confidential negotiated rate not available via public API. " +
			"Supply edp_discount_pct (0.0-1.0) to calculate your effective rate. " +
			"Market range: ~5% at $1M/yr commitment to ~20% at $50M+/yr. " +
			"Qualification: requires ~$1M+ annual AWS spend commitment, AWS Enterprise Support enrollment, " +
			"and a PPA (Private Pricing Agreement) — contact your AWS account team or TAM to negotiate.",
	}

	// 3-year commitment risk warning — material for enterprise FinOps decisions.
	if years == 3 {
		breakdown["commitment_risk_warning"] = "3-year SP commitments include a ratchet clause: " +
			"your commitment amount cannot decrease year-over-year during the term. " +
			"If your workload shrinks or migrates, you continue paying the full commitment, " +
			"resulting in wasted spend. Ensure you have high confidence in 3-year growth " +
			"before choosing 3yr over 1yr. Consider starting with 1yr and renewing at 3yr " +
			"once utilisation is proven stable."
	}

	if paymentOption == "All Upfront" {
		breakdown["all_upfront_note"] = "All Upfront SP: the full commitment cost is paid upfront; " +
			"the ongoing hourly charge is $0.00. The effective hourly rate represents " +
			"the amortised prepayment (upfront amount ÷ commitment hours)."
	}

	if odRate > 0 && spRate > 0 {
		spDiscount := (odRate - spRate) / odRate
		breakdown["on_demand_rate"] = odRate
		breakdown["sp_discount_pct"] = fmt.Sprintf("%.1f%%", spDiscount*100)
		breakdown["discount_breakdown"] = []map[string]any{
			{
				"type": "savings_plan",
				"from": odRate,
				"to":   spRate,
				"pct":  fmt.Sprintf("%.1f%%", spDiscount*100),
			},
		}
	} else if odRate > 0 && spRate == 0 {
		breakdown["on_demand_rate"] = odRate
	}

	// EDP post-processing — STEP 8.
	if spec.EDPDiscountPct != nil {
		edpPct := *spec.EDPDiscountPct
		if edpPct < 0 || edpPct >= 1 {
			return nil, fmt.Errorf("aws sp: edp_discount_pct must be in range [0.0, 1.0), got %v", edpPct)
		}

		edpRate := spRate * (1.0 - edpPct)

		edpAttrs := make(map[string]string, len(spAttrs)+3)
		for k, v := range spAttrs {
			edpAttrs[k] = v
		}
		edpAttrs["edp_source"] = "user_supplied"
		edpAttrs["edp_pct"] = fmt.Sprintf("%.1f%%", edpPct*100)
		edpAttrs["application_order"] = "savings_plan,edp"

		edpPrice := models.NormalizedPrice{
			Provider:      models.CloudProviderAWS,
			Service:       "ec2",
			SKUID:         entry.DiscountedSku,
			ProductFamily: entry.ProductFamily,
			Description:   fmt.Sprintf("%s %d-year %s + EDP (%.1f%%) for %s (%s)", strings.ToUpper(spType), years, paymentOption, edpPct*100, instanceType, os),
			Region:        region,
			Attributes:    edpAttrs,
			PricingTerm:   spec.GetTerm(),
			PricePerUnit:  edpRate,
			Unit:          models.PriceUnitPerHour,
			Currency:      entry.Currency,
			EffectiveDate: &effectiveDate,
			FetchedAt:     &now,
			SourceURL:     entry.SourceURL,
		}

		result.ContractedPrices = []models.NormalizedPrice{edpPrice}

		// Extended breakdown with EDP tier.
		discountBreakdown := []map[string]any{}
		if odRate > 0 && spRate > 0 {
			spDiscount := (odRate - spRate) / odRate
			discountBreakdown = append(discountBreakdown, map[string]any{
				"type": "savings_plan",
				"from": odRate,
				"to":   spRate,
				"pct":  fmt.Sprintf("%.1f%%", spDiscount*100),
			})
		} else if odRate > 0 && spRate == 0 {
			discountBreakdown = append(discountBreakdown, map[string]any{
				"type": "savings_plan",
				"from": odRate,
				"to":   spRate,
				"note": "All Upfront: $0/hr ongoing charge; cost is prepaid",
			})
		}
		discountBreakdown = append(discountBreakdown, map[string]any{
			"type": "edp",
			"from": spRate,
			"to":   edpRate,
			"pct":  fmt.Sprintf("%.1f%%", edpPct*100),
		})

		breakdown["edp_rate"] = edpRate
		breakdown["edp_pct"] = fmt.Sprintf("%.1f%%", edpPct*100)
		breakdown["discount_breakdown"] = discountBreakdown
		breakdown["discount_application_order"] = []string{"savings_plan", "edp"}
		if odRate > 0 {
			totalDiscount := (odRate - edpRate) / odRate
			breakdown["total_discount_pct"] = fmt.Sprintf("%.1f%%", totalDiscount*100)
		}

		// Warn about SP bundling double-counting risk.
		breakdown["edp_bundling_note"] = "If your PPA/EDP agreement uses SP bundling, the published SP rate " +
			"already embeds EDP. Applying edp_discount_pct on top would double-count. " +
			"Check with your AWS account team whether your contract uses SP bundling."
	}

	result.Breakdown = breakdown
	return result, nil
}

// instanceFamily extracts the instance family from an instance type string.
// E.g. "m5.xlarge" -> "m5", "r7g.4xlarge" -> "r7g".
func instanceFamily(instanceType string) string {
	if idx := strings.Index(instanceType, "."); idx >= 0 {
		return instanceType[:idx]
	}
	return instanceType
}
