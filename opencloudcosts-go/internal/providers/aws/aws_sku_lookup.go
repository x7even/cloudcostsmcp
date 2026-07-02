package aws

// aws_sku_lookup.go implements get_price_by_sku's core logic: given a raw AWS
// usage-type/SKU string exactly as it appears in a Cost & Usage Report (CUR)
// export — e.g. "CAN1-BoxUsage:r5a.8xlarge" — find its price in a list of
// target regions.
//
// WHY NO REGION-PREFIX REVERSE-LOOKUP TABLE:
// AWS usage-type strings are built as "{regionPrefix-or-blank}{operation}",
// where regionPrefix is a short token like "CAN1", "USW2", "EUC1" — but the
// mapping from prefix token to region code (e.g. "CAN1" -> "ca-central-1") is
// undocumented, inconsistent in shape (see the "EU" exception below), and
// grows every time AWS launches a region. Hard-coding that table would mean
// this tool silently returns wrong/empty results for any region AWS adds
// after this code is written. Instead, this file never tries to interpret
// what a prefix token *means* — it only strips it by *shape* to get a
// prefix-independent "suffix" (the part of the usage-type string that is
// stable across regions, e.g. "BoxUsage:r5a.8xlarge"), and compares that
// suffix against the same shape-stripped suffix computed from each candidate
// product row fetched from the *target* region's own offer file. Two usage
// types denote "the same thing in different regions" iff their suffixes are
// byte-equal — regardless of what either prefix token happens to encode.
//
// WHY THE "EU" SPECIAL CASE:
// Every AWS region-prefix token verified live matches the shape
// ^[A-Z]{2,4}[0-9]{1,2}$ (2-4 uppercase letters, then 1-2 digits) — except
// eu-west-1, whose prefix is the bare literal "EU" with no digit at all
// (verified against the AWSELB and AmazonEC2 offer files for eu-west-1 on
// 2026-07-02). This is a hard-coded historical AWS quirk, not a pattern, so
// it is handled as a literal special case rather than folded into the shape
// regex.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// --------------------------------------------------------------------------
// Usage-type prefix stripping
// --------------------------------------------------------------------------

// usageTypePrefixTokenRe matches one or more concatenated
// (2-4 uppercase letters + 1-2 digits) groups anchored at the start of the
// string, followed by a literal "-". A single repetition captures ordinary
// region prefixes ("CAN1-", "USW2-", "EUC1-"...). Multiple repetitions with
// no separating hyphen capture the compound Wavelength-style tokens seen in
// AWSDataTransfer usage types (e.g. "USE1WL1ATL1-" = "USE1"+"WL1"+"ATL1"),
// so the same regex serves both the primary strip and the compound-detection
// re-check below without needing a second pattern.
var usageTypePrefixTokenRe = regexp.MustCompile(`^(?:[A-Z]{2,4}[0-9]{1,2})+-`)

// stripUsageTypeToken attempts to strip exactly one leading region-prefix
// token (by shape, or the literal "EU" exception) from usageType. ok is false
// when no prefix token is present at all, which is the normal, expected shape
// for us-east-1 usage types (they have no prefix token whatsoever).
func stripUsageTypeToken(usageType string) (token, rest string, ok bool) {
	if strings.HasPrefix(usageType, "EU-") {
		return "EU", usageType[len("EU-"):], true
	}
	if m := usageTypePrefixTokenRe.FindString(usageType); m != "" {
		return strings.TrimSuffix(m, "-"), usageType[len(m):], true
	}
	return "", usageType, false
}

// stripUsageTypePrefix canonicalizes an AWS usage-type string into a
// region-independent suffix, per the shape rules documented at the top of
// this file. prefix is "" when usageType has no prefix token (us-east-1
// shape) or the literal region-prefix token otherwise ("EU", "CAN1", ...).
//
// looksLikeCompoundTransfer is true when, after stripping one prefix token,
// the remaining suffix *itself* begins with something matching the same
// prefix shape followed by "-". This flags the AWSDataTransfer
// inter-region/wavelength usage types that encode two region-shaped tokens
// (e.g. "CAN1-DEN1-AWS-Out-Bytes", "USE1WL1ATL1-CAN1-AWS-Out-Bytes"). This
// tool's single-prefix-strip model does not fully resolve those SKUs to a
// clean, comparable suffix — callers must be warned rather than silently
// given a possibly-wrong or empty match.
func stripUsageTypePrefix(usageType string) (prefix, suffix string, looksLikeCompoundTransfer bool) {
	token, rest, ok := stripUsageTypeToken(usageType)
	if !ok {
		return "", usageType, false
	}
	_, _, compoundOK := stripUsageTypeToken(rest)
	return token, rest, compoundOK
}

// --------------------------------------------------------------------------
// Service inference
// --------------------------------------------------------------------------

// inferServiceFromUsageType implements the service-inference heuristic table:
// it recognizes a handful of well-known usage-type patterns and maps them to
// the AWS Pricing API servicecode that bills them. It is a heuristic, not an
// exhaustive mapping — productFamily/operation strings can collide across
// services in ways not covered here — so callers should always be told to
// pass service= explicitly when this function's result is used, and every
// call site in this file that relies on inference also emits a warning
// saying so.
func inferServiceFromUsageType(usageType string) (service string, inferred bool) {
	switch {
	case strings.Contains(usageType, "BoxUsage") ||
		strings.Contains(usageType, "SpotUsage") ||
		strings.Contains(usageType, "HostBoxUsage") ||
		strings.Contains(usageType, "EBS:VolumeUsage") ||
		strings.Contains(usageType, "EBS:SnapshotUsage"):
		return "AmazonEC2", true

	case strings.Contains(usageType, "LCUUsage") ||
		strings.Contains(usageType, "LoadBalancerUsage") ||
		strings.Contains(usageType, "DataProcessing-Bytes"):
		return "AWSELB", true

	case strings.Contains(usageType, "InstanceUsage:db.") ||
		strings.Contains(usageType, "InstanceUsageIOOptimized:db.") ||
		strings.Contains(usageType, "Multi-AZUsage:db."):
		return "AmazonRDS", true

	case strings.Contains(usageType, "ReadRequestUnits") ||
		strings.Contains(usageType, "WriteRequestUnits") ||
		strings.Contains(usageType, "ReadCapacityUnit-Hrs") ||
		strings.Contains(usageType, "TimedStorage-ByteHrs"):
		return "AmazonDynamoDB", true

	case strings.Contains(usageType, "NodeUsage:cache."):
		return "AmazonElastiCache", true

	case strings.Contains(usageType, "AWS-Out-Bytes") ||
		strings.Contains(usageType, "AWS-In-Bytes"):
		return "AWSDataTransfer", true

	default:
		return "", false
	}
}

// --------------------------------------------------------------------------
// Input validation — servicecode / region shape checks
// --------------------------------------------------------------------------
//
// Both raw strings below are interpolated directly into an outbound HTTPS URL
// path (see bulkPricingBaseURL usage in aws_bulk.go: ".../{service}/current/{region}/index.json").
// They must be validated by shape BEFORE that interpolation, independent of
// correctness/business-logic checks, so a caller cannot smuggle path
// traversal or unexpected segments into the request.

// awsServiceCodeRe matches plausible AWS Pricing API servicecode shapes
// (e.g. "AmazonEC2", "AWSELB", "AWSDataTransfer", "AmazonDynamoDB"): an
// upper/lower alphanumeric token starting with a letter.
var awsServiceCodeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{1,49}$`)

// isValidAWSServiceCode reports whether s is shaped like a real AWS Pricing
// API servicecode. This is a shape check, not a membership check against a
// fixed list — AWS adds new servicecodes over time and this tool should not
// need a code change to support them.
func isValidAWSServiceCode(s string) bool {
	return awsServiceCodeRe.MatchString(s)
}

// awsRegionCodeShapeRe matches the general AWS region-code shape: two
// lowercase letters, an optional "-gov" segment, a lowercase geography word,
// and a trailing 1-2 digit number (e.g. "us-east-1", "ap-southeast-1",
// "us-gov-west-1", "il-central-1").
//
// The rest of this package (regionToLocation in aws_pricing.go,
// utils.AWSRegionDisplay) validates region codes via a fixed allowlist map
// instead of a shape regex. That is intentional there: those call sites need
// the *display name* the SDK-based Pricing API expects, which only exists
// for regions already in the map. This function does NOT reuse that map: the
// bulk-HTTP catalog fetch this file uses needs only the region *code* (it is
// used directly as a URL path segment, see aws_bulk.go), and the whole point
// of the suffix-matching design in this file is to work for AWS regions this
// codebase has never seen before. A fixed allowlist would defeat that; a
// shape check does not.
var awsRegionCodeShapeRe = regexp.MustCompile(`^[a-z]{2}(-gov)?-[a-z]+-[0-9]{1,2}$`)

// isValidAWSRegionCodeShape reports whether region is shaped like a real AWS
// region code. It does not check whether the region actually exists.
func isValidAWSRegionCodeShape(region string) bool {
	return awsRegionCodeShapeRe.MatchString(region)
}

// isChinaPartitionRegion reports whether region belongs to the AWS China
// partition (cn-north-1, cn-northwest-1). Those regions are shaped like valid
// AWS region codes but are NOT reachable via the pricing.us-east-1.amazonaws.com
// public bulk endpoint this file uses (verified empirically: that endpoint
// belongs to the standard "aws" partition only, not "aws-cn") — so they must
// be rejected explicitly rather than attempted and left to fail with a
// confusing HTTP error.
func isChinaPartitionRegion(region string) bool {
	return strings.HasPrefix(region, "cn-")
}

// --------------------------------------------------------------------------
// Structured errors
// --------------------------------------------------------------------------

// Error codes returned via SKULookupError.Code, for the tool-handler layer to
// switch on when building a structured JSON error response (mirroring how
// the rest of this codebase distinguishes error kinds, e.g. "not_supported"
// vs "not_configured" in tools/lookup.go).
const (
	SKUErrUnsupportedProvider   = "unsupported_provider"
	SKUErrSKURequired           = "sku_required"
	SKUErrRegionsRequired       = "regions_required"
	SKUErrTooManyRegions        = "too_many_regions"
	SKUErrInvalidService        = "invalid_service"
	SKUErrServiceUndeterminable = "service_undeterminable"
)

// SKULookupError is returned for request-level failures that apply to the
// whole lookup (bad input, unsupported provider) as opposed to a single
// region's result, which is instead represented as a non-error entry inside
// SKULookupResult.Regions (see that type's docs for why).
type SKULookupError struct {
	Code    string
	Message string
}

func (e *SKULookupError) Error() string { return e.Message }

// maxSKULookupRegions caps the regions list. Each requested region can
// trigger a full multi-hundred-MB offer-file download per candidate service
// (mitigated by the memoization cache below, but only after the first call
// for a given (service,region) pays that cost) — an unbounded regions list is
// a resource-exhaustion vector, so it is rejected up front, before any
// network call is made.
const maxSKULookupRegions = 30

// --------------------------------------------------------------------------
// Process-lifetime catalog memoization
// --------------------------------------------------------------------------
//
// WHY THIS CACHE EXISTS: a single AWS usage-type suffix cannot be turned into
// a server-side TERM_MATCH filter, because the region-prefix token that would
// need to prepend it in the target region's offer file is exactly the thing
// this design deliberately does not know (see the top-of-file doc comment).
// So every SKU lookup for a given (service, region) pair requires streaming
// and bucketing *every* product row in that offer file by its own
// shape-stripped suffix — there is no way to ask AWS's bulk endpoint for just
// the one row we want. The real motivating use case for get_price_by_sku is
// reconciling a CUR export line-by-line — potentially ~1000 distinct SKUs
// against ~10 target regions. Without memoization that is up to 10,000
// redundant downloads and re-parses of multi-hundred-MB JSON files that,
// per (service, region), are byte-identical across every one of those 1000
// lookups. This cache makes each (service, region) pay that cost exactly
// once per process lifetime.
//
// It is deliberately NOT layered on top of cache.CacheManager: that cache
// does a whole-file JSON rewrite (encode the entire map, write a temp file,
// rename) on every Set call, and is designed for a modest number of small
// entries (individual priced SKUs), not a small number of very large ones
// (a full per-service-per-region offer-file index, potentially hundreds of
// thousands of price entries). Persisting that volume through CacheManager
// would make every write pathologically slow and balloon the on-disk cache
// file. This cache is in-memory only and is intentionally lost on restart.
var skuCatalogCache = struct {
	mu      sync.Mutex
	entries map[string]*skuCatalogEntry
}{entries: make(map[string]*skuCatalogEntry)}

// skuCatalogEntry holds the memoized result for one (service, region) pair.
// sync.Once ensures concurrent lookups for the same pair (e.g. the
// per-region fan-out below, or two different SKUs sharing a candidate
// service+region) collapse into a single fetch rather than a stampede.
type skuCatalogEntry struct {
	once     sync.Once
	bySuffix map[string][]models.NormalizedPrice
	err      error
}

// getSKUCatalogEntry returns the (possibly new) cache slot for key, creating
// it under the map mutex if absent. The actual fetch happens outside this
// mutex, inside the entry's own sync.Once, so slow fetches for different
// keys never block each other.
func getSKUCatalogEntry(key string) *skuCatalogEntry {
	skuCatalogCache.mu.Lock()
	defer skuCatalogCache.mu.Unlock()
	e, ok := skuCatalogCache.entries[key]
	if !ok {
		e = &skuCatalogEntry{}
		skuCatalogCache.entries[key] = e
	}
	return e
}

// getOrFetchSKUCatalog returns the memoized (fetch-once-per-process)
// suffix->prices index for (service, region), fetching it via fetchSKUCatalog
// on first use.
//
// A failed fetch is intentionally NOT memoized permanently: sync.Once only
// runs its function once regardless of outcome, so on error this evicts the
// entry before returning, letting the next caller for the same key retry
// from scratch instead of being stuck with a transient network failure (e.g.
// a timeout) for the rest of the process's lifetime.
func (p *Provider) getOrFetchSKUCatalog(ctx context.Context, service, region string) (map[string][]models.NormalizedPrice, error) {
	key := service + "|" + region
	entry := getSKUCatalogEntry(key)
	entry.once.Do(func() {
		entry.bySuffix, entry.err = fetchSKUCatalog(ctx, p, service, region)
	})
	if entry.err != nil {
		skuCatalogCache.mu.Lock()
		if skuCatalogCache.entries[key] == entry {
			delete(skuCatalogCache.entries, key)
		}
		skuCatalogCache.mu.Unlock()
	}
	return entry.bySuffix, entry.err
}

// fetchSKUCatalog streams the full (service, region) offer file via the
// existing bulk-pricing infrastructure (getProductsBulk in aws_bulk.go) with
// no attribute filters — we cannot filter server-side by usage-type suffix,
// only bucket client-side after fetching everything — and buckets every
// priced product row by its shape-stripped usage-type suffix.
//
// maxResults is set to math.MaxInt32 (effectively unbounded): getProductsBulk
// otherwise stops *collecting* matches past that cap (while still streaming
// the rest of the file to reach the terms section), which would silently
// drop candidate rows this function needs — the whole point here is
// completeness, not a capped sample.
func fetchSKUCatalog(ctx context.Context, p *Provider, service, region string) (map[string][]models.NormalizedPrice, error) {
	raw, err := p.getProductsBulk(ctx, service, nil, math.MaxInt32, region)
	if err != nil {
		return nil, fmt.Errorf("aws sku lookup: fetch %s/%s catalog: %w", service, region, err)
	}

	bySuffix := make(map[string][]models.NormalizedPrice)
	for _, r := range raw {
		var parsed parsedSKU
		if jsonErr := json.Unmarshal([]byte(r), &parsed); jsonErr != nil {
			continue
		}
		usageType := parsed.Product.Attributes["usagetype"]
		if usageType == "" {
			continue
		}
		_, suffix, _ := stripUsageTypePrefix(usageType)

		// service (the AWS servicecode, e.g. "AmazonEC2") is passed through as
		// NormalizedPrice.Service here rather than a domain bucket label
		// ("compute"/"storage"/...) like the rest of this package uses. This
		// tool is explicitly about raw AWS billing/servicecode identity — a
		// CUR line item's "AWS Product" — so surfacing the literal servicecode
		// is more useful to a caller reconciling billing data than a coarser
		// domain label would be.
		np := skuToNormalizedPrice(r, region, models.PricingTermOnDemand, service)
		if np == nil {
			continue
		}
		bySuffix[suffix] = append(bySuffix[suffix], *np)
	}
	return bySuffix, nil
}

// --------------------------------------------------------------------------
// Public result types
// --------------------------------------------------------------------------

// SKULookupRegionResult is the per-region outcome of a SKU lookup. Exactly
// one of the following holds, mirroring how the rest of this codebase
// distinguishes "we looked and found nothing" from "we couldn't look":
//   - len(Prices) > 0: a match was found; ServiceUsed names the AWS
//     servicecode whose catalog contained it.
//   - NoMapping == true: every candidate service's catalog was fetched
//     successfully for this region, but no product row's usage-type suffix
//     matched the input SKU's suffix. This is the explicit "no mapping
//     found" result the caller needs to distinguish "priced but not in this
//     region" (or "not modeled by AWS at all") from a transient failure.
//   - Error != "": the region code or China-partition check failed, or every
//     candidate service's catalog fetch itself failed (e.g. network/HTTP
//     error) — we don't actually know whether a match exists.
type SKULookupRegionResult struct {
	Region string `json:"region"`

	// ServiceUsed is the AWS servicecode whose catalog produced the match.
	// Only set when len(Prices) > 0.
	ServiceUsed string `json:"service_used,omitempty"`

	// ServiceMismatch is true when ServiceUsed differs from the caller's
	// explicit service hint — i.e. the hint's catalog had no match, but the
	// inferred-service fallback catalog did. See LookupSKUAcrossRegions docs.
	ServiceMismatch bool `json:"service_mismatch,omitempty"`

	Prices []models.NormalizedPrice `json:"prices,omitempty"`

	NoMapping bool `json:"no_mapping,omitempty"`

	Error string `json:"error,omitempty"`

	// AttemptedServices lists the AWS servicecodes searched for this region,
	// in search order, for diagnostic/debugging purposes.
	AttemptedServices []string `json:"attempted_services,omitempty"`
}

// SKULookupResult is the full result of a get_price_by_sku lookup: the
// canonicalized form of the input SKU, service-resolution provenance, and
// one SKULookupRegionResult per requested region (in the same order as the
// input regions list — the tool-handler layer is responsible for any
// cheapest-first sorting, mirroring how compare_prices sorts after fan-out).
type SKULookupResult struct {
	SKU string `json:"sku"`

	// UsageTypePrefix is the stripped region-prefix token ("CAN1", "EU", "")
	// and UsageTypeSuffix is the region-independent remainder used for
	// cross-region matching. See stripUsageTypePrefix.
	UsageTypePrefix string `json:"usage_type_prefix"`
	UsageTypeSuffix string `json:"usage_type_suffix"`

	ServiceHint     string `json:"service_hint,omitempty"`
	InferredService string `json:"inferred_service,omitempty"`
	// ServiceSource is "explicit" when ServiceHint was supplied and used as
	// the primary candidate, or "inferred" when no hint was given and
	// InferredService was derived from the usage-type pattern.
	ServiceSource string `json:"service_source"`

	Warnings []string `json:"warnings,omitempty"`

	Regions []SKULookupRegionResult `json:"regions"`
}

// --------------------------------------------------------------------------
// LookupSKUAcrossRegions — main entry point
// --------------------------------------------------------------------------

// LookupSKUAcrossRegions resolves the price of a raw AWS usage-type/SKU
// string (as it appears verbatim in a Cost & Usage Report) in each of the
// given regions, fanning out concurrently across regions (bounded by a
// semaphore of 10, matching the pattern used by compare_prices in
// tools/lookup.go).
//
// providerName is validated against "aws" as defense-in-depth: this whole
// feature is AWS-only (raw usage-type strings are an AWS CUR concept with no
// GCP/Azure equivalent), and the caller — the tool-handler layer added in a
// later phase — is expected to reject non-AWS providers before ever reaching
// this AWS-package method, but this function does not assume that happened.
//
// serviceHint, if non-empty, is tried first for every region. If the
// usage-type pattern also allows inferring a servicecode (see
// inferServiceFromUsageType) and that inferred servicecode differs from
// serviceHint, it is tried as a fallback per region: real CUR exports are
// not always internally consistent (e.g. AWSDataTransfer usage types
// sometimes appear against an "AWS Product" column of "AmazonEC2"), so a
// supplied hint that yields no match is not treated as fatal — the inferred
// servicecode is tried too, and if that produces a match, ServiceMismatch is
// set on the result so the caller can see the hint didn't hold. If
// serviceHint is empty and no inference is possible, this returns a
// SKULookupError with Code == SKUErrServiceUndeterminable rather than
// guessing.
func (p *Provider) LookupSKUAcrossRegions(
	ctx context.Context,
	providerName string,
	sku string,
	serviceHint string,
	regions []string,
) (*SKULookupResult, error) {
	if !strings.EqualFold(providerName, "aws") {
		return nil, &SKULookupError{
			Code: SKUErrUnsupportedProvider,
			Message: fmt.Sprintf(
				"get_price_by_sku only supports provider=\"aws\" (got %q) — raw AWS usage-type/SKU "+
					"strings are an AWS Cost & Usage Report concept with no GCP/Azure equivalent",
				providerName,
			),
		}
	}
	if sku == "" {
		return nil, &SKULookupError{Code: SKUErrSKURequired, Message: "sku must not be empty"}
	}
	if len(regions) == 0 {
		return nil, &SKULookupError{
			Code:    SKUErrRegionsRequired,
			Message: "regions must contain at least one AWS region code",
		}
	}
	if len(regions) > maxSKULookupRegions {
		return nil, &SKULookupError{
			Code: SKUErrTooManyRegions,
			Message: fmt.Sprintf(
				"regions must contain at most %d entries (got %d) — this endpoint fetches a full "+
					"per-region pricing catalog per candidate service, so unbounded region lists risk "+
					"excessive memory and bandwidth use",
				maxSKULookupRegions, len(regions),
			),
		}
	}
	if serviceHint != "" && !isValidAWSServiceCode(serviceHint) {
		return nil, &SKULookupError{
			Code:    SKUErrInvalidService,
			Message: fmt.Sprintf("service %q is not shaped like a valid AWS servicecode", serviceHint),
		}
	}

	prefix, suffix, compound := stripUsageTypePrefix(sku)
	inferredService, inferred := inferServiceFromUsageType(sku)

	result := &SKULookupResult{
		SKU:             sku,
		UsageTypePrefix: prefix,
		UsageTypeSuffix: suffix,
		ServiceHint:     serviceHint,
	}
	if compound {
		result.Warnings = append(result.Warnings,
			"sku looks like a multi-region/wavelength data-transfer usage type; "+
				"result may be inaccurate — verify manually")
	}

	var candidates []string
	switch {
	case serviceHint != "" && inferred && !strings.EqualFold(serviceHint, inferredService):
		candidates = []string{serviceHint, inferredService}
		result.ServiceSource = "explicit"
		result.InferredService = inferredService
	case serviceHint != "":
		candidates = []string{serviceHint}
		result.ServiceSource = "explicit"
	case inferred:
		candidates = []string{inferredService}
		result.ServiceSource = "inferred"
		result.InferredService = inferredService
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"service was inferred as %q from the usage-type pattern — this is a heuristic, not an "+
				"exhaustive mapping; pass service=%q explicitly to confirm or override it",
			inferredService, inferredService,
		))
	default:
		return nil, &SKULookupError{
			Code: SKUErrServiceUndeterminable,
			Message: fmt.Sprintf(
				"service is required — could not infer AWS servicecode for usage-type %q; "+
					"supply service= explicitly", sku,
			),
		}
	}

	regionResults := make([]SKULookupRegionResult, len(regions))
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup
	for i, region := range regions {
		wg.Add(1)
		go func(idx int, rgn string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			regionResults[idx] = p.lookupSKUInRegion(ctx, rgn, suffix, candidates, serviceHint)
		}(i, region)
	}
	wg.Wait()

	result.Regions = regionResults
	return result, nil
}

// lookupSKUInRegion searches candidates (in order) for a product row whose
// usage-type suffix equals suffix, in a single region. See
// SKULookupRegionResult's docs for how Prices / NoMapping / Error are
// distinguished.
func (p *Provider) lookupSKUInRegion(
	ctx context.Context,
	region string,
	suffix string,
	candidates []string,
	explicitHint string,
) SKULookupRegionResult {
	rr := SKULookupRegionResult{Region: region, AttemptedServices: candidates}

	if !isValidAWSRegionCodeShape(region) {
		rr.Error = fmt.Sprintf("region %q does not look like a valid AWS region code", region)
		return rr
	}
	if isChinaPartitionRegion(region) {
		rr.Error = fmt.Sprintf(
			"region %q is unsupported: AWS China partition regions are not reachable via the "+
				"public pricing.us-east-1.amazonaws.com endpoint this tool uses", region,
		)
		return rr
	}

	var lastErr error
	anyFetchSucceeded := false
	for _, svc := range candidates {
		bySuffix, err := p.getOrFetchSKUCatalog(ctx, svc, region)
		if err != nil {
			lastErr = err
			continue
		}
		anyFetchSucceeded = true

		if prices, ok := bySuffix[suffix]; ok && len(prices) > 0 {
			rr.ServiceUsed = svc
			rr.Prices = prices
			if explicitHint != "" && !strings.EqualFold(svc, explicitHint) {
				rr.ServiceMismatch = true
			}
			return rr
		}
	}

	if !anyFetchSucceeded && lastErr != nil {
		// Every candidate's catalog fetch itself failed — we don't know
		// whether a match exists, so this must not be reported as NoMapping
		// (which asserts "we checked and there is no such usage-type here").
		rr.Error = lastErr.Error()
		return rr
	}

	rr.NoMapping = true
	return rr
}
