// gcp_sku_lookup.go implements get_price_by_sku's GCP counterpart to AWS's
// raw usage-type/SKU lookup (internal/providers/aws/aws_sku_lookup.go): given
// a raw GCP Cloud Billing Catalog "skuId" string (e.g. "0055-9F63-3A4D"),
// find its price in a list of target regions.
//
// Unlike AWS's usage-type/SKU strings, a GCP skuId does not encode a region
// at all — the same opaque ID is either region-invariant (GLOBAL), scoped to
// exactly one region (REGIONAL), or scoped to a named multi-region
// (MULTI_REGIONAL), as declared on the SKU's own geoTaxonomy field. There is
// also no service-inference heuristic analogous to AWS's usage-type pattern
// matching: a skuId does not by itself hint which of the 13 onboarded GCP
// service catalogs it lives in, so an omitted service hint means scanning
// every one of them (bounded concurrency, see below) rather than guessing.
//
// REGION-ATTRIBUTION RULE (geoTaxonomy-first, serviceRegions-fallback):
// This file's per-region matching logic is grounded in two prior, live-
// verified findings elsewhere in this package, not invented fresh here:
//   - gcp_kms.go: every in-scope Cloud KMS SKU has geoTaxonomy.type=="GLOBAL"
//     and is deliberately reported as Region="global" regardless of the
//     region the caller asked about, bypassing serviceRegions entirely — a
//     precedent this file follows for any matched SKU whose geoTaxonomy.type
//     is GLOBAL (or absent, historically treated as GLOBAL by isGlobalSKU).
//   - gcp_firestore.go: Cloud Firestore's serviceRegions is the literal
//     ["global"] on every SKU regardless of true scope, making serviceRegions
//     membership useless for that service — geoTaxonomy.type/regions is the
//     only usable signal, and MULTI_REGIONAL SKUs (nam5/nam7's overlapping
//     constituent-region lists) require description-based short-name parsing
//     (skuFirestoreMultiRegion) rather than trusting geoTaxonomy.regions
//     directly.
//
// A dedicated research pass (see issue RC3-015 planning notes) confirmed
// Firestore is the ONLY onboarded service with evidence of MULTI_REGIONAL
// SKUs; every other service either uses literal serviceRegions membership or
// is pure GLOBAL. So this file's MULTI_REGIONAL case reuses
// skuFirestoreMultiRegion as-is rather than generalizing it — see that
// research's recommendation for why a generalized parser is not (yet)
// justified. If a future live catalog check surfaces a non-Firestore
// MULTI_REGIONAL SKU with an unusable serviceRegions, lifting
// skuFirestoreMultiRegion's substring loop into a shared
// skuMultiRegionShortName(descLower, knownNames) helper is a small,
// contained refactor, not a redesign.
//
// Because of all this, region attribution here is, in priority order:
//  1. geoTaxonomy.type == "GLOBAL": matches every requested region.
//  2. geoTaxonomy.type == "REGIONAL": matches iff geoTaxonomy.regions is
//     exactly one entry equal to the requested region string.
//  3. geoTaxonomy.type == "MULTI_REGIONAL": matches iff
//     skuFirestoreMultiRegion(descLower) equals the requested region string
//     (Firestore's own convention — callers pass "nam5"/"nam7"/"eur3" as the
//     region).
//  4. geoTaxonomy absent/unrecognized: fall back to the pre-Firestore
//     serviceRegions + skuMatchesRegion membership check used by every other
//     onboarded GCP domain file.
//
// All methods are on *Provider defined in gcp.go (Part 1).
package gcp

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/skulookup"
)

// gcpSKUMaxLength bounds the raw skuId string. Mirrors aws.maxSKULength's
// rationale (real skuId values are short hex-hyphenated tokens, far under
// this cap; the cap exists only to reject pathological/abusive input before
// it's echoed into error messages, not to constrain any real skuId shape).
// aws.maxSKULength is unexported, so this is a separate, identically-valued
// constant rather than a shared one.
const gcpSKUMaxLength = 1024

// gcpSKUMaxLookupRegions bounds the regions list, mirroring
// aws.maxSKULookupRegions's rationale: each candidate service catalog is
// fetched once regardless of region count (fetchSKUs is not region-scoped
// for GCP), but an unbounded regions list still means unbounded per-region
// work building the response, so it is capped defensively.
const gcpSKUMaxLookupRegions = 30

// gcpSKUFetchConcurrency bounds how many candidate service catalogs are
// fetched concurrently for one lookup. A cold-cache, no-service-hint lookup
// has up to 13 candidate services (every onboarded GCP domain), each a
// paginated Cloud Billing Catalog fetch — fetching all 13 serially risks
// exceeding the default 60s per-tool-call context.WithTimeout
// (internal/config/config.go RequestTimeout, applied in
// internal/server/server.go), so this bounds worst-case latency the same way
// LookupSKUAcrossRegions (aws_sku_lookup.go) bounds its own region fan-out
// with a semaphore.
const gcpSKUFetchConcurrency = 5

// --------------------------------------------------------------------------
// Service-hint resolution
// --------------------------------------------------------------------------

// gcpSKULookupServiceOrder is the fixed, deterministic scan order for a
// no-service-hint lookup (every onboarded GCP service), and the canonical
// name used to report ServiceUsed/AttemptedServices in the response — chosen
// so a caller can plug ServiceUsed straight back in as service= on a
// follow-up call, mirroring how AWS's ServiceUsed (a servicecode) is itself
// a valid service= input.
var gcpSKULookupServiceOrder = []string{
	"compute", "gcs", "cloudsql", "gke", "memorystore", "kms", "dns",
	"firestore", "pubsub", "vertex", "bigquery", "monitoring", "armor",
}

// gcpSKULookupServiceIDs maps a canonical service name (and a couple of
// obvious aliases) to its GCP Cloud Billing Catalog service ID. Every
// canonical (non-alias) key has a corresponding entry in
// gcpSKULookupServiceOrder.
var gcpSKULookupServiceIDs = map[string]string{
	"compute":      computeServiceID,
	"gcs":          gcsServiceID,
	"cloudstorage": gcsServiceID, // alias
	"cloudsql":     cloudSQLServiceID,
	"gke":          gkeServiceID,
	"memorystore":  memorystoreServiceID,
	"kms":          kmsServiceID,
	"cloudkms":     kmsServiceID, // alias
	"dns":          dnsServiceID,
	"clouddns":     dnsServiceID, // alias
	"firestore":    firestoreServiceID,
	"pubsub":       pubsubServiceID,
	"vertex":       vertexServiceID,
	"bigquery":     bigqueryServiceID,
	"monitoring":   cloudMonitoringServiceID,
	"armor":        cloudArmorServiceID,
}

// gcpSKULookupServiceIDToName reverse-maps a service ID back to its
// canonical name (built once from gcpSKULookupServiceOrder, so it only ever
// contains canonical names, never aliases).
var gcpSKULookupServiceIDToName = func() map[string]string {
	m := make(map[string]string, len(gcpSKULookupServiceOrder))
	for _, name := range gcpSKULookupServiceOrder {
		m[gcpSKULookupServiceIDs[name]] = name
	}
	return m
}()

// resolveGCPSKUServiceCandidates resolves serviceHint to the list of
// candidate service IDs to scan, in gcpSKULookupServiceOrder's order. An
// empty hint means "scan everything"; a non-empty, unrecognized hint is a
// validation error rather than silently falling back to a full scan.
func resolveGCPSKUServiceCandidates(serviceHint string) ([]string, *skulookup.SKULookupError) {
	if serviceHint == "" {
		ids := make([]string, len(gcpSKULookupServiceOrder))
		for i, name := range gcpSKULookupServiceOrder {
			ids[i] = gcpSKULookupServiceIDs[name]
		}
		return ids, nil
	}
	id, ok := gcpSKULookupServiceIDs[strings.ToLower(serviceHint)]
	if !ok {
		return nil, &skulookup.SKULookupError{
			Code: skulookup.SKUErrInvalidService,
			Message: fmt.Sprintf(
				"service %q is not a recognized GCP service for raw-SKU lookup — known values: %s",
				serviceHint, strings.Join(gcpSKULookupServiceOrder, ", "),
			),
		}
	}
	return []string{id}, nil
}

// gcpServiceIDsToNames converts a slice of raw GCP service IDs to their
// canonical names (see gcpSKULookupServiceIDToName), for building
// human-readable warnings/errors and the AttemptedServices field. An ID with
// no canonical name (should not occur — every candidate ID originates from
// gcpSKULookupServiceIDs) is passed through verbatim as a defensive
// fallback.
func gcpServiceIDsToNames(ids []string) []string {
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		if n, ok := gcpSKULookupServiceIDToName[id]; ok {
			names = append(names, n)
		} else {
			names = append(names, id)
		}
	}
	return names
}

// --------------------------------------------------------------------------
// Region-attribution and price-extraction helpers
// --------------------------------------------------------------------------

// gcpSKUMatchesRequestedRegion reports whether a matched raw SKU applies to
// requestedRegion, per the geoTaxonomy-first/serviceRegions-fallback rule
// documented at the top of this file. regionType is returned alongside for
// the caller to distinguish the GLOBAL case (which needs stampGlobalScope
// applied to the resulting price) from the others.
func gcpSKUMatchesRequestedRegion(sku map[string]any, requestedRegion string) (matches bool, regionType string) {
	// Region codes in the raw GCP Cloud Billing Catalog JSON (geoTaxonomy.
	// regions, serviceRegions) and this package's own multi-region short
	// names (firestoreMultiRegions) are always lowercase, but requestedRegion
	// arrives here straight from the caller with no case normalization
	// applied anywhere upstream (see sku_lookup.go/bom.go). Lowercase it once
	// here, mirroring gcp_firestore.go's firestoreRegionKey / gcp_networking.
	// go's strings.ToLower(region) precedent for exactly this comparison, so
	// a caller passing e.g. "NAM5" or "US-CENTRAL1" still matches.
	requestedRegion = strings.ToLower(requestedRegion)
	regionType, geoRegions := skuGeoTaxonomy(sku)
	switch regionType {
	case "GLOBAL":
		return true, regionType
	case "REGIONAL":
		return len(geoRegions) == 1 && geoRegions[0] == requestedRegion, regionType
	case "MULTI_REGIONAL":
		desc, _ := sku["description"].(string)
		short := skuFirestoreMultiRegion(strings.ToLower(desc))
		if short == "" {
			// Unlike Firestore's own fetchFirestoreRates (gcp_firestore.go),
			// which slog.Warns and skips a MULTI_REGIONAL SKU whose
			// description carries no recognized multi-region short name,
			// this domain-agnostic path had no equivalent diagnostic —
			// silently reporting NoMapping for every requested region with
			// no way to distinguish "genuinely out of scope" from "we
			// couldn't parse the multi-region name". Warn so this is at
			// least visible/debuggable.
			skuID, _ := sku["skuId"].(string)
			slog.Warn("gcp raw-sku lookup: MULTI_REGIONAL SKU with no recognized multi-region name in description",
				"sku_id", skuID, "description", desc)
			return false, regionType
		}
		return short == requestedRegion, regionType
	default:
		// geoTaxonomy absent, or a type this codebase has never observed —
		// fall back to plain serviceRegions membership, the pre-Firestore
		// convention every other onboarded domain file uses.
		regionsAny, _ := sku["serviceRegions"].([]any)
		matches = skuMatchesRegion(regionsAny, requestedRegion)
		if matches && serviceRegionsContainsGlobal(regionsAny) {
			// isGlobalSKU's precedent (this file's own header comment)
			// treats absent/unrecognized geoTaxonomy as GLOBAL whenever the
			// SKU is otherwise region-invariant. skuMatchesRegion's match
			// here can come from either a literal requestedRegion entry or
			// the "global" sentinel; only the latter actually means
			// region-invariant, so only report regionType="GLOBAL" (which
			// triggers stampGlobalScope on the caller side) when
			// serviceRegions itself contains "global" — not merely because
			// requestedRegion happened to also be listed.
			return true, "GLOBAL"
		}
		return matches, regionType
	}
}

// serviceRegionsContainsGlobal reports whether a SKU's raw serviceRegions
// slice contains the literal sentinel "global" (as opposed to matching only
// because it lists the specific requestedRegion).
func serviceRegionsContainsGlobal(regions []any) bool {
	for _, r := range regions {
		if s, _ := r.(string); s == "global" {
			return true
		}
	}
	return false
}

// gcpSKUProductFamily extracts a matched SKU's category.resourceFamily (the
// GCP Cloud Billing Catalog field documented for this purpose — e.g.
// "Compute", "Storage", "ApplicationServices" — but never read by any
// existing per-domain file in this package, which only ever read
// category.resourceGroup/usageType for their own domain-specific narrowing;
// there is no established convention to reuse here). Falls back to
// category.resourceGroup (the field every other file does read) if
// resourceFamily is absent, since either is a reasonable best-effort
// "product family" label for a domain-agnostic lookup with no a priori
// knowledge of which is more meaningful for the matched service.
func gcpSKUProductFamily(sku map[string]any) string {
	cat, _ := sku["category"].(map[string]any)
	if cat == nil {
		return ""
	}
	if rf, ok := cat["resourceFamily"].(string); ok && rf != "" {
		return rf
	}
	if rg, ok := cat["resourceGroup"].(string); ok && rg != "" {
		return rg
	}
	return ""
}

// gcpSKUUnit maps a matched SKU's raw pricingInfo[0].pricingExpression.
// usageUnit code to the closest models.PriceUnit constant. This is
// deliberately best-effort/generic — unlike every existing per-domain file
// in this package, which hardcodes the correct unit because it already knows
// the domain (e.g. gcp_kms.go always knows a key-version rate is
// per-key-version-month), a domain-agnostic raw-SKU lookup has no a priori
// knowledge of which unit fits, only the raw GCP unit code string. Falls
// back to models.PriceUnitPerUnit for any code not in this (non-exhaustive)
// table.
func gcpSKUUnit(sku map[string]any) models.PriceUnit {
	expr := gcpPricingExpression(sku)
	if expr == nil {
		return models.PriceUnitPerUnit
	}
	raw, _ := expr["usageUnit"].(string)
	switch raw {
	case "h":
		return models.PriceUnitPerHour
	case "mo":
		return models.PriceUnitPerMonth
	case "GiBy.mo":
		return models.PriceUnitPerGBMonth
	case "GiBy":
		return models.PriceUnitPerGB
	case "requests":
		return models.PriceUnitPerRequest
	case "count", "1":
		return models.PriceUnitPerUnit
	default:
		return models.PriceUnitPerUnit
	}
}

// copyStringMap returns a shallow copy of m (nil in, nil out), so per-region
// NormalizedPrice clones built from one shared matched-SKU template never
// alias the same Attributes map (stampGlobalScope, called per matching
// region for a GLOBAL SKU, mutates its price's Attributes map in place).
func copyStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// gcpBuildMatchedPrices builds one models.NormalizedPrice per priced tier of
// a matched raw SKU (region-independent — Region is left unset; callers
// clone and set it per matching requested region). Returns a non-empty
// errMsg instead of prices when the matched SKU has no priceable rate at
// all, which the caller must surface as a region Error rather than a
// silently-zero price.
func gcpBuildMatchedPrices(sku map[string]any, serviceName string) (prices []models.NormalizedPrice, tiered bool, errMsg string) {
	tiers := skuTierList(sku)
	if len(tiers) == 0 {
		skuID, _ := sku["skuId"].(string)
		return nil, false, fmt.Sprintf(
			"matched GCP SKU %q has no priceable rate (no tieredRates found in pricingInfo) — this is an anomaly, not a zero price", skuID)
	}
	skuID, _ := sku["skuId"].(string)
	desc, _ := sku["description"].(string)
	productFamily := gcpSKUProductFamily(sku)
	unit := gcpSKUUnit(sku)
	tiered = len(tiers) > 1

	prices = make([]models.NormalizedPrice, 0, len(tiers))
	for _, t := range tiers {
		var attrs map[string]string
		if tiered {
			attrs = map[string]string{"tier_start_usage": fmt.Sprintf("%g", t.start)}
		}
		prices = append(prices, newGCPBasePrice(serviceName, productFamily, skuID, desc, t.price, unit, attrs))
	}
	return prices, tiered, ""
}

// uniformRegionResults builds one skulookup.SKULookupRegionResult per region,
// each a copy of tmpl with only Region varying. Shared by
// LookupSKUAcrossRegionsGeneric's "incomplete scan" and "complete scan, no
// match" branches, which previously each independently allocated and looped
// to build an identical shape (differing only in whether Error or NoMapping
// was set) — a future field added to SKULookupRegionResult that must be
// populated uniformly is now only ever set in one place instead of two that
// could silently drift apart.
func uniformRegionResults(regions []string, tmpl skulookup.SKULookupRegionResult) []skulookup.SKULookupRegionResult {
	out := make([]skulookup.SKULookupRegionResult, len(regions))
	for i, region := range regions {
		rr := tmpl
		rr.Region = region
		out[i] = rr
	}
	return out
}

// --------------------------------------------------------------------------
// LookupSKUAcrossRegionsGeneric — skulookup.SKULookupProvider conformance
// --------------------------------------------------------------------------

// LookupSKUAcrossRegionsGeneric resolves the price of a raw GCP Cloud
// Billing Catalog skuId string in each of the given regions. hint is
// accepted for skulookup.SKULookupProvider interface conformance but is not
// used: GCP has no confirmed disambiguation axis analogous to AWS's
// operation/productFamily hints today — a matched skuId resolves to exactly
// one catalog row per service, so there is nothing here for
// OperationHint/ProductFamilyHint to narrow. (Open question carried over
// from planning: whether category.usageType could ever need to disambiguate
// two rows sharing one skuId — no evidence of that has been found; revisit
// if it ever is.)
func (p *Provider) LookupSKUAcrossRegionsGeneric(
	ctx context.Context, sku string, regions []string, serviceHint string, hint skulookup.SKUHint,
) (*skulookup.SKULookupResult, error) {
	_ = hint

	if sku == "" {
		return nil, &skulookup.SKULookupError{Code: skulookup.SKUErrSKURequired, Message: "sku must not be empty"}
	}
	if len(sku) > gcpSKUMaxLength {
		return nil, &skulookup.SKULookupError{
			Code: skulookup.SKUErrSKUTooLong,
			Message: fmt.Sprintf(
				"sku must be at most %d characters (got %d) — real GCP skuId values are far shorter",
				gcpSKUMaxLength, len(sku)),
		}
	}
	if len(regions) == 0 {
		return nil, &skulookup.SKULookupError{
			Code:    skulookup.SKUErrRegionsRequired,
			Message: "regions must contain at least one GCP region code",
		}
	}
	if len(regions) > gcpSKUMaxLookupRegions {
		return nil, &skulookup.SKULookupError{
			Code: skulookup.SKUErrTooManyRegions,
			Message: fmt.Sprintf(
				"regions must contain at most %d entries (got %d)", gcpSKUMaxLookupRegions, len(regions)),
		}
	}

	candidateIDs, svcErr := resolveGCPSKUServiceCandidates(serviceHint)
	if svcErr != nil {
		return nil, svcErr
	}

	result := &skulookup.SKULookupResult{
		SKU:         sku,
		ServiceHint: serviceHint,
	}
	if serviceHint != "" {
		result.ServiceSource = "explicit"
	} else {
		// GCP has no AWS-style single-service inference heuristic from the
		// skuId's own shape — an omitted hint means every onboarded service
		// is scanned, not a guessed single candidate.
		result.ServiceSource = "scanned_all"
	}

	// Fan out the candidate service catalog fetches with bounded
	// concurrency — see gcpSKUFetchConcurrency's doc for why.
	type fetchOutcome struct {
		serviceID string
		skus      []map[string]any
		err       error
	}
	outcomes := make([]fetchOutcome, len(candidateIDs))
	sem := make(chan struct{}, gcpSKUFetchConcurrency)
	var wg sync.WaitGroup
	for i, sid := range candidateIDs {
		wg.Add(1)
		go func(idx int, serviceID string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			skus, err := p.fetchSKUs(ctx, serviceID)
			outcomes[idx] = fetchOutcome{serviceID: serviceID, skus: skus, err: err}
		}(i, sid)
	}
	wg.Wait()

	// Scan every successfully-fetched service's SKU list to completion (never
	// short-circuit on the first match) so a duplicate skuId within one
	// service's catalog is always detected, and so every candidate's
	// fetch-success/failure is known before deciding whether "no match" means
	// a genuine no_mapping or an incomplete scan.
	var matchedServiceID string
	var matchedSKU map[string]any
	var succeededIDs, failedIDs []string
	var warnings []string
	duplicateWarned := false

	for _, o := range outcomes {
		if o.err != nil {
			failedIDs = append(failedIDs, o.serviceID)
			continue
		}
		succeededIDs = append(succeededIDs, o.serviceID)

		var localMatches int
		var localFirst map[string]any
		for _, s := range o.skus {
			id, _ := s["skuId"].(string)
			if id == sku {
				localMatches++
				if localMatches == 1 {
					localFirst = s
				}
			}
		}
		if localMatches > 1 && !duplicateWarned {
			warnings = append(warnings, fmt.Sprintf(
				"unexpected: %d catalog rows matched skuId %q within service %q — using the first row; "+
					"the assumed GCP skuId-uniqueness invariant may not hold",
				localMatches, sku, o.serviceID))
			duplicateWarned = true
		}
		if localMatches > 0 {
			if matchedSKU == nil {
				matchedServiceID = o.serviceID
				matchedSKU = localFirst
			} else if o.serviceID != matchedServiceID {
				// A different service ALSO matched this skuId — an asymmetry
				// with the same-service duplicate case above, which does
				// warn. Report it: silently keeping whichever service
				// happened to be first in scan order, with no visibility
				// into the collision, would be a real (if rare) mispricing
				// risk if the assumed cross-service skuId-uniqueness
				// invariant is ever violated.
				warnings = append(warnings, fmt.Sprintf(
					"unexpected: skuId %q also matched a catalog row in service %q; using the first-scanned "+
						"match from service %q — the assumed GCP skuId-uniqueness-across-services invariant may not hold",
					sku, o.serviceID, matchedServiceID))
			}
		}
	}

	switch {
	case matchedSKU != nil:
		if len(failedIDs) > 0 {
			warnings = append(warnings, fmt.Sprintf(
				"a match was found in service %q, but the scan was not fully exhaustive: service(s) %v "+
					"could not be fetched and were not searched",
				gcpSKULookupServiceIDToName[matchedServiceID], gcpServiceIDsToNames(failedIDs)))
		}

		serviceName := gcpSKULookupServiceIDToName[matchedServiceID]
		basePrices, tiered, buildErr := gcpBuildMatchedPrices(matchedSKU, serviceName)

		regionResults := make([]skulookup.SKULookupRegionResult, len(regions))
		for i, region := range regions {
			rr := skulookup.SKULookupRegionResult{Region: region}
			matches, regionType := gcpSKUMatchesRequestedRegion(matchedSKU, region)
			switch {
			case !matches:
				// A matched skuId with a narrower geography than requested
				// is expected/normal for a REGIONAL (or MULTI_REGIONAL) SKU —
				// report it as this region's own no_mapping, not a whole-
				// lookup failure, since other requested regions may still
				// match.
				rr.NoMapping = true
				rr.AttemptedServices = []string{serviceName}
			case buildErr != "":
				rr.Error = buildErr
			default:
				prices := make([]models.NormalizedPrice, len(basePrices))
				for j, bp := range basePrices {
					cp := bp
					cp.Attributes = copyStringMap(bp.Attributes)
					cp.Region = region
					if regionType == "GLOBAL" {
						stampGlobalScope(&cp)
					}
					prices[j] = cp
				}
				rr.ServiceUsed = serviceName
				rr.Prices = prices
				rr.Tiered = tiered
			}
			regionResults[i] = rr
		}
		result.Regions = regionResults

	case len(failedIDs) > 0:
		// No match found, but the scan was incomplete — an incomplete scan
		// is not evidence of absence, so this must NOT be reported as
		// no_mapping.
		attempted := gcpServiceIDsToNames(succeededIDs)
		msg := fmt.Sprintf(
			"could not determine whether sku %q exists: service(s) %v failed to fetch and could not be "+
				"searched; successfully searched: %v", sku, gcpServiceIDsToNames(failedIDs), attempted)
		result.Regions = uniformRegionResults(regions, skulookup.SKULookupRegionResult{
			Error:             msg,
			AttemptedServices: attempted,
		})

	default:
		// A genuine, complete scan found no match anywhere.
		attempted := gcpServiceIDsToNames(succeededIDs)
		result.Regions = uniformRegionResults(regions, skulookup.SKULookupRegionResult{
			NoMapping:         true,
			AttemptedServices: attempted,
		})
	}

	result.Warnings = warnings
	return result, nil
}
