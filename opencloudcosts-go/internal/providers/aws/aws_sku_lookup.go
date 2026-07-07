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
	"time"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/skulookup"
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
		strings.Contains(usageType, "ReadCapacityUnit-Hrs"):
		return "AmazonDynamoDB", true

	// NOTE: "TimedStorage-ByteHrs" is intentionally NOT bucketed here even
	// though DynamoDB storage usage types do carry that literal suffix -- S3
	// standard/other-tier storage usage types (e.g. "TimedStorage-ByteHrs",
	// "TimedStorage-INT-FA-ByteHrs") use the identical suffix, and DynamoDB
	// storage pricing ($0.25/GB-month) differs from S3 Standard storage
	// pricing ($0.023/GB-month) by ~11x. Confidently inferring either service
	// from this suffix alone would silently return a wrong-product price for
	// the other; callers must pass service= explicitly for storage usage
	// types with this suffix (falls through to SKUErrServiceUndeterminable
	// when no hint is given -- see LookupSKUAcrossRegions).

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
var awsRegionCodeShapeRe = regexp.MustCompile(`^[a-z]{2}(-gov)?-[a-z]{2,20}-[0-9]{1,2}$`)

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
//
// These were originally declared locally here; they now alias the canonical
// definitions in internal/skulookup so a second provider (GCP) can share them
// without importing this package. Every existing call site that spells them
// as awsprovider.SKUErrSKURequired, etc. continues to compile unchanged — see
// internal/skulookup's package doc for why.
const (
	SKUErrUnsupportedProvider   = skulookup.SKUErrUnsupportedProvider
	SKUErrSKURequired           = skulookup.SKUErrSKURequired
	SKUErrSKUTooLong            = skulookup.SKUErrSKUTooLong
	SKUErrRegionsRequired       = skulookup.SKUErrRegionsRequired
	SKUErrTooManyRegions        = skulookup.SKUErrTooManyRegions
	SKUErrInvalidService        = skulookup.SKUErrInvalidService
	SKUErrServiceUndeterminable = skulookup.SKUErrServiceUndeterminable
	SKUErrHintTooLong           = skulookup.SKUErrHintTooLong
)

// maxSKULength bounds the raw sku string. Real AWS usage-type strings are at
// most a couple hundred characters (the longest observed compound-transfer
// tokens are well under 100); this generous cap exists only to reject
// pathological/abusive input before it is echoed back into log lines, error
// messages, and (via stripUsageTypePrefix) regex matching, not to constrain
// any real usage-type shape.
const maxSKULength = 1024

// maxHintLength bounds operationHint/productFamilyHint. Real AWS "operation"
// and "productFamily" attribute values are short (well under 100 characters
// for every value observed in the offer files this tool reads). Unlike sku,
// these hints are never interpolated into a URL path or echoed into a log
// line/error message — they are only ever compared via strings.EqualFold
// against catalog attribute values (see resolveSKUCandidates) — so this cap
// exists purely for input-validation parity with sku/service rather than to
// close any injection vector, and is generous for the same reason maxSKULength
// is.
const maxHintLength = 256

// SKULookupError aliases skulookup.SKULookupError — see that package's docs.
type SKULookupError = skulookup.SKULookupError

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

// skuCatalogEntryTTL bounds how long a fetched (service, region) catalog is
// reused before being treated as stale and re-fetched. Without this, a
// long-running server process would hold every (service, region) catalog it
// has ever fetched in memory forever, and would never pick up AWS pricing
// changes for the rest of its lifetime. Falls back to
// defaultSKUCatalogEntryTTL when the provider has no configured cache TTL
// (p.cfg == nil, e.g. in unit tests that construct &Provider{} directly).
const defaultSKUCatalogEntryTTL = 24 * time.Hour

// maxSKUCatalogCacheEntries hard-caps the number of distinct (service,
// region) entries held at once. Combined with the TTL above, this bounds the
// cache's worst-case memory footprint instead of letting it grow unbounded
// for the lifetime of the process (a single reconciliation run against
// maxSKULookupRegions regions with a service-hint/inferred-service mismatch
// can populate up to 2*maxSKULookupRegions=60 entries by itself, so the cap
// must comfortably exceed that for normal usage while still being a real
// bound). When full, the oldest completed entry is evicted to make room.
const maxSKUCatalogCacheEntries = 128

// skuCatalogEntry holds the memoized result for one (service, region) pair.
// sync.Once ensures concurrent lookups for the same pair (e.g. the
// per-region fan-out below, or two different SKUs sharing a candidate
// service+region) collapse into a single fetch rather than a stampede.
type skuCatalogEntry struct {
	once     sync.Once
	bySuffix map[string][]models.NormalizedPrice
	err      error

	// fetchedAt is set to time.Now() (under skuCatalogCache.mu, by the sole
	// goroutine that runs once.Do's body) once the fetch completes. It stays
	// the zero Time while a fetch is in flight, which getSKUCatalogEntry and
	// evictOldestLocked both rely on to never treat/evict an in-flight entry
	// as stale/evictable.
	fetchedAt time.Time
}

// getSKUCatalogEntry returns the (possibly new) cache slot for key, creating
// it under the map mutex if absent or stale. The actual fetch happens
// outside this mutex, inside the entry's own sync.Once, so slow fetches for
// different keys never block each other.
func getSKUCatalogEntry(key string, ttl time.Duration) *skuCatalogEntry {
	skuCatalogCache.mu.Lock()
	defer skuCatalogCache.mu.Unlock()

	if e, ok := skuCatalogCache.entries[key]; ok {
		if e.fetchedAt.IsZero() || time.Since(e.fetchedAt) <= ttl {
			return e
		}
		// Stale: drop it and fall through to create a fresh entry below.
		delete(skuCatalogCache.entries, key)
	}

	if len(skuCatalogCache.entries) >= maxSKUCatalogCacheEntries {
		evictOldestLocked()
	}

	e := &skuCatalogEntry{}
	skuCatalogCache.entries[key] = e
	return e
}

// evictOldestLocked removes the completed entry with the oldest fetchedAt
// from skuCatalogCache.entries. Entries still in flight (fetchedAt still
// zero) are never chosen — evicting a slot another goroutine is actively
// fetching into would just cause that goroutine's result to be silently
// discarded. Callers must hold skuCatalogCache.mu.
func evictOldestLocked() {
	var oldestKey string
	var oldestAt time.Time
	for k, e := range skuCatalogCache.entries {
		if e.fetchedAt.IsZero() {
			continue
		}
		if oldestKey == "" || e.fetchedAt.Before(oldestAt) {
			oldestKey, oldestAt = k, e.fetchedAt
		}
	}
	if oldestKey != "" {
		delete(skuCatalogCache.entries, oldestKey)
	}
}

// skuCatalogCacheTTL returns p's configured cache TTL, falling back to
// defaultSKUCatalogEntryTTL when p or p.cfg is nil (unit tests construct
// &Provider{} directly) or when CacheTTLHours is unset/non-positive.
func skuCatalogCacheTTL(p *Provider) time.Duration {
	if p != nil && p.cfg != nil && p.cfg.CacheTTLHours > 0 {
		return time.Duration(p.cfg.CacheTTLHours) * time.Hour
	}
	return defaultSKUCatalogEntryTTL
}

// getOrFetchSKUCatalog returns the memoized suffix->prices index for
// (service, region), fetching it via fetchSKUCatalog on first use or once
// the previous fetch has aged past skuCatalogCacheTTL.
//
// A failed fetch is intentionally NOT memoized permanently: sync.Once only
// runs its function once regardless of outcome, so on error this evicts the
// entry before returning, letting the next caller for the same key retry
// from scratch instead of being stuck with a transient network failure (e.g.
// a timeout) for the rest of the entry's TTL.
func (p *Provider) getOrFetchSKUCatalog(ctx context.Context, service, region string) (map[string][]models.NormalizedPrice, error) {
	key := service + "|" + region
	entry := getSKUCatalogEntry(key, skuCatalogCacheTTL(p))
	entry.once.Do(func() {
		entry.bySuffix, entry.err = fetchSKUCatalog(ctx, p, service, region)
		skuCatalogCache.mu.Lock()
		entry.fetchedAt = time.Now()
		skuCatalogCache.mu.Unlock()
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

// SKULookupRegionResult aliases skulookup.SKULookupRegionResult — see that
// package's docs. It was originally declared locally here; AWS never sets
// its additive Tiered field (see skulookup's docs for why — AWS's usage-type
// suffix model does not surface tiered rate schedules through this path).
type SKULookupRegionResult = skulookup.SKULookupRegionResult

// SKULookupResult aliases skulookup.SKULookupResult — see that package's
// docs. UsageTypePrefix/UsageTypeSuffix (the stripped region-prefix token and
// region-independent remainder, see stripUsageTypePrefix) are AWS-only
// concepts populated only by this file.
type SKULookupResult = skulookup.SKULookupResult

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
//
// operationHint and productFamilyHint, if non-empty, disambiguate a usage-
// type suffix that matches more than one distinct billable product (e.g.
// ELB's LCUUsage suffix spans Application/Network/Gateway load balancer
// pricing; RDS's InstanceUsage:<type> suffix spans every database engine on
// that instance type). They correspond to the AWS product "operation"
// attribute and top-level productFamily respectively — see
// resolveSKUCandidates for exactly how they're applied.
func (p *Provider) LookupSKUAcrossRegions(
	ctx context.Context,
	providerName string,
	sku string,
	serviceHint string,
	regions []string,
	operationHint string,
	productFamilyHint string,
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
	if len(sku) > maxSKULength {
		return nil, &SKULookupError{
			Code: SKUErrSKUTooLong,
			Message: fmt.Sprintf(
				"sku must be at most %d characters (got %d) — real AWS usage-type strings are far shorter",
				maxSKULength, len(sku),
			),
		}
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
	if len(operationHint) > maxHintLength {
		return nil, &SKULookupError{
			Code: SKUErrHintTooLong,
			Message: fmt.Sprintf(
				"operation must be at most %d characters (got %d) — real AWS \"operation\" attribute "+
					"values are far shorter", maxHintLength, len(operationHint),
			),
		}
	}
	if len(productFamilyHint) > maxHintLength {
		return nil, &SKULookupError{
			Code: SKUErrHintTooLong,
			Message: fmt.Sprintf(
				"product_family must be at most %d characters (got %d) — real AWS productFamily "+
					"values are far shorter", maxHintLength, len(productFamilyHint),
			),
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
	case serviceHint != "" && inferred && serviceHint != inferredService:
		// serviceHint and inferredService are the same servicecode modulo
		// casing (e.g. hint "amazonec2" vs canonical "AmazonEC2") — not a
		// genuine mismatch, but the bulk-pricing URL path this candidate is
		// interpolated into (see aws_bulk.go) is case-sensitive, so using the
		// caller's mis-cased hint verbatim would silently fail to match
		// AWS's actual servicecode path segment. Use the canonically-cased
		// inferredService as the sole candidate instead of retrying the same
		// (wrong-cased) servicecode twice.
		candidates = []string{inferredService}
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
			regionResults[idx] = p.lookupSKUInRegion(ctx, rgn, suffix, candidates, serviceHint, operationHint, productFamilyHint)
		}(i, region)
	}
	wg.Wait()

	result.Regions = regionResults
	return result, nil
}

// --------------------------------------------------------------------------
// Multi-row-per-suffix disambiguation
// --------------------------------------------------------------------------
//
// A stripped usage-type suffix (see stripUsageTypePrefix) is NOT always
// unique within a (service, region) catalog: AWS usage-type strings do not
// encode every attribute that distinguishes one priced product row from
// another with the same "operation" shape. The clearest example is EC2's
// "BoxUsage:<instanceType>" suffix, which matches one row per
// operatingSystem x tenancy x preInstalledSw x capacitystatus combination
// (Linux/Windows/RHEL/SUSE, Shared/Dedicated/Host, with/without SQL Server,
// on-demand/reserved-capacity) — all sharing the identical usage-type suffix
// in the raw offer file. Silently picking "the cheapest of these" (as an
// earlier version of this file did) can return a materially wrong price: the
// cheapest row for a given suffix is very often a Dedicated-tenancy or
// Windows-with-SQL row, not the plain Linux/Shared row a caller reconciling
// a CUR export almost always means.
//
// canonicalDefaultAttrs mirrors the canonical-default attribute set this
// codebase already uses elsewhere for exactly this purpose — computeFilters
// in aws_pricing.go (used by GetComputePrice/SearchPricing) narrows EC2
// candidate rows down to operatingSystem=Linux, tenancy=Shared,
// preInstalledSw=NA, capacitystatus=Used. That existing convention is reused
// here rather than inventing a second one, and deliberately kept to exactly
// those four keys (no more): a candidate row that lacks a given key entirely
// (e.g. an RDS row has no "tenancy" attribute at all) is left unfiltered on
// that key rather than excluded, so this narrowing only ever disambiguates
// EC2-shaped rows and has no effect — narrowing or otherwise — on services
// that don't carry these attributes (RDS's databaseEngine collisions, for
// instance, are not resolvable this way and are correctly left ambiguous
// below).
var canonicalDefaultAttrs = map[string]string{
	"operatingSystem": "Linux",
	"tenancy":         "Shared",
	"preInstalledSw":  "NA",
	"capacitystatus":  "Used",
}

// Hint-resolution status codes, surfaced on SKULookupRegionResult.HintStatus
// so a caller can tell *why* a region is still ambiguous rather than just
// seeing "ambiguous: true" again. See resolveSKUCandidates.
//
// These alias the canonical definitions in internal/skulookup — see that
// package's docs.
const (
	HintStatusNoHint    = skulookup.HintStatusNoHint
	HintStatusResolved  = skulookup.HintStatusResolved
	HintStatusNoMatch   = skulookup.HintStatusNoMatch
	HintStatusAmbiguous = skulookup.HintStatusAmbiguous
)

// resolveSKUCandidates narrows prices (all rows sharing one stripped
// usage-type suffix) down to a single match, using an optional caller-
// supplied disambiguating hint before falling back to the existing
// canonicalDefaultAttrs narrowing.
//
// If prices has at most one row to begin with AND no hint was supplied, it
// is returned as-is (not ambiguous, hint status "no_hint_supplied" — hints
// are irrelevant when there is nothing to disambiguate). Critically, this
// short-circuit does NOT apply when a hint IS supplied: a single-row match
// still goes through the hint-filtering block below, so a hint that
// contradicts the sole candidate row fails closed (HintStatusNoMatch,
// ambiguous=true) instead of silently returning that row as if it were a
// confident match. Without this, a caller-supplied hint naming a different
// product than the one row actually present would be ignored entirely,
// reintroducing a silent-wrong-price failure mode for the single-row case.
//
// When operationHint and/or productFamilyHint are non-empty, prices is first
// filtered to rows where (operationHint == "" || Attributes["operation"] ==
// "" || matches Attributes["operation"] case-insensitively) AND
// (productFamilyHint == "" || matches ProductFamily case-insensitively). A
// row with a stored-empty operation attribute (true for services like S3,
// CloudWatch, and Lambda, which don't carry that dimension) is never
// excluded by operationHint — an empty stored value cannot conflict with a
// supplied hint, mirroring how narrowByCanonicalDefaults treats a missing
// key as non-exclusionary:
//   - Exactly one match: resolved by hint — returned immediately as the sole,
//     non-ambiguous match. Canonical-default narrowing is not applied to it.
//   - Zero matches: fails closed. A supplied-but-nonmatching hint must never
//     be silently ignored, so the *original, unfiltered* prices are returned
//     with ambiguous=true and hint status "hint_no_match".
//   - More than one match: canonicalDefaultAttrs narrowing is applied to
//     that hint-filtered subset (not the original full set). If that narrows
//     to exactly one row, it is returned as the sole match. Otherwise the
//     hint-filtered subset (still a real improvement over the full set) is
//     returned with ambiguous=true and hint status "hint_ambiguous".
//
// With no hints supplied, behavior is unchanged from before hints existed:
// rows are filtered by canonicalDefaultAttrs (a row's Attributes value must
// equal the canonical default for every key canonicalDefaultAttrs and that
// row both have — a row missing a given key is not excluded by it, since
// absence of an attribute is not a mismatch, just a service that doesn't
// carry that dimension). If that narrowing leaves exactly one row, it is
// returned as the sole, non-ambiguous match. Otherwise the *original,
// unfiltered* candidate set is returned with ambiguous=true, so the caller
// sees every real candidate rather than an arbitrarily-narrowed subset it
// can't make sense of.
func resolveSKUCandidates(
	prices []models.NormalizedPrice,
	operationHint, productFamilyHint string,
) (chosen []models.NormalizedPrice, ambiguous bool, hintStatus string) {
	hasHint := operationHint != "" || productFamilyHint != ""

	if len(prices) <= 1 && !hasHint {
		return prices, false, HintStatusNoHint
	}

	if hasHint {
		var hintFiltered []models.NormalizedPrice
		for _, p := range prices {
			if operationHint != "" && p.Attributes["operation"] != "" && !strings.EqualFold(p.Attributes["operation"], operationHint) {
				continue
			}
			if productFamilyHint != "" && !strings.EqualFold(p.ProductFamily, productFamilyHint) {
				continue
			}
			hintFiltered = append(hintFiltered, p)
		}

		switch len(hintFiltered) {
		case 0:
			// Fail closed: never silently fall through to the unfiltered set
			// as if the hint had not been supplied.
			return prices, true, HintStatusNoMatch
		case 1:
			return hintFiltered, false, HintStatusResolved
		default:
			narrowed := narrowByCanonicalDefaults(hintFiltered)
			if len(narrowed) == 1 {
				return narrowed, false, HintStatusResolved
			}
			return hintFiltered, true, HintStatusAmbiguous
		}
	}

	narrowed := narrowByCanonicalDefaults(prices)
	if len(narrowed) == 1 {
		return narrowed, false, HintStatusNoHint
	}
	return prices, true, HintStatusNoHint
}

// narrowByCanonicalDefaults filters prices down to rows matching
// canonicalDefaultAttrs on every key both the row and the map carry. See
// canonicalDefaultAttrs' docs for why a missing key on a row is not treated
// as a mismatch.
func narrowByCanonicalDefaults(prices []models.NormalizedPrice) []models.NormalizedPrice {
	var narrowed []models.NormalizedPrice
	for _, p := range prices {
		matchesAll := true
		for key, want := range canonicalDefaultAttrs {
			if got, ok := p.Attributes[key]; ok && got != want {
				matchesAll = false
				break
			}
		}
		if matchesAll {
			narrowed = append(narrowed, p)
		}
	}
	return narrowed
}

// lookupSKUInRegion searches candidates (in order) for a product row whose
// usage-type suffix equals suffix, in a single region. See
// SKULookupRegionResult's docs for how Prices / NoMapping / Error are
// distinguished. operationHint/productFamilyHint are passed through to
// resolveSKUCandidates to disambiguate multi-row matches — see that
// function's docs.
func (p *Provider) lookupSKUInRegion(
	ctx context.Context,
	region string,
	suffix string,
	candidates []string,
	explicitHint string,
	operationHint string,
	productFamilyHint string,
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
	// hintNoMatchFallback remembers the first candidate service whose catalog
	// contained the suffix but whose rows all failed a supplied hint
	// (HintStatusNoMatch). A hint should be given every candidate service's
	// catalog a chance to resolve it — not just whichever candidate happens
	// to be tried first — before this region is reported hint_no_match: e.g.
	// an explicit service= hint that disagrees with the heuristically
	// inferred service (candidates = [serviceHint, inferredService]) may
	// happen to also carry the suffix, but for an unrelated product that the
	// operation/product_family hint correctly rejects — the *inferred*
	// service's catalog is where the real match lives. If no later candidate
	// does better, this fallback is what gets returned, so behavior for a
	// single-candidate call (the overwhelmingly common case) is unchanged.
	var hintNoMatchFallback *SKULookupRegionResult
	for _, svc := range candidates {
		bySuffix, err := p.getOrFetchSKUCatalog(ctx, svc, region)
		if err != nil {
			lastErr = err
			continue
		}
		anyFetchSucceeded = true

		prices, ok := bySuffix[suffix]
		if !ok || len(prices) == 0 {
			continue
		}

		candidateResult := rr
		candidateResult.ServiceUsed = svc
		candidateResult.Prices, candidateResult.Ambiguous, candidateResult.HintStatus =
			resolveSKUCandidates(prices, operationHint, productFamilyHint)
		if explicitHint != "" && !strings.EqualFold(svc, explicitHint) {
			candidateResult.ServiceMismatch = true
		}

		if candidateResult.HintStatus == HintStatusNoMatch {
			if hintNoMatchFallback == nil {
				hintNoMatchFallback = &candidateResult
			}
			continue
		}

		return candidateResult
	}

	if hintNoMatchFallback != nil {
		return *hintNoMatchFallback
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

// --------------------------------------------------------------------------
// skulookup.SKULookupProvider conformance
// --------------------------------------------------------------------------

// LookupSKUAcrossRegionsGeneric adapts LookupSKUAcrossRegions to the
// provider-agnostic skulookup.SKULookupProvider interface, so the
// tool-handler layer (internal/tools) can resolve AWS and GCP raw-SKU lookups
// through one generic code path instead of hardcoding *Provider. It does not
// replace or change the behavior of LookupSKUAcrossRegions — it is a
// different method name delegating to the exact same logic, with
// providerName hardcoded to "aws" (this method only ever makes sense for an
// AWS *Provider instance).
func (p *Provider) LookupSKUAcrossRegionsGeneric(
	ctx context.Context, sku string, regions []string, serviceHint string, hint skulookup.SKUHint,
) (*skulookup.SKULookupResult, error) {
	return p.LookupSKUAcrossRegions(ctx, "aws", sku, serviceHint, regions, hint.OperationHint, hint.ProductFamilyHint)
}
