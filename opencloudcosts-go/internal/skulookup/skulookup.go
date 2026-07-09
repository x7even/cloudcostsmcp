// Package skulookup holds the provider-agnostic raw-SKU-lookup types shared
// by every provider that implements a get_price_by_sku-style lookup (AWS's
// CUR usage-type/SKU strings, GCP's Cloud Billing Catalog skuId strings, ...).
//
// These types were originally declared locally in
// internal/providers/aws/aws_sku_lookup.go (the first, and for a while only,
// implementation). This package hoists them out so a second provider (GCP,
// see internal/providers/gcp/gcp_sku_lookup.go) can implement the same
// SKULookupProvider interface without importing the aws package, and so the
// tool-handler layer (internal/tools) can resolve either provider through one
// generic code path instead of hardcoding *awsprovider.Provider everywhere.
//
// internal/providers/aws/aws_sku_lookup.go re-declares every one of these
// names as a type alias / const alias pointing back here, so every existing
// call site that spells them as awsprovider.SKULookupError,
// awsprovider.SKUErrSKURequired, etc. continues to compile unchanged — a type
// alias makes the old and new names identical types, not merely convertible
// ones.
//
// This package imports only internal/models, so it carries no risk of an
// import cycle with internal/providers/aws or internal/providers/gcp.
package skulookup

import (
	"context"

	"github.com/x7even/cloudcostsmcp/opencloudcosts-go/internal/models"
)

// SKUHint bundles the optional AWS-specific disambiguating hints
// (operation/productFamily) that narrow a usage-type suffix matching more
// than one distinct billable product down to a single row. GCP does not use
// either field today (see gcp_sku_lookup.go's doc comment for why) but
// accepts SKUHint for interface conformance so both providers share one
// method signature.
type SKUHint struct {
	OperationHint     string
	ProductFamilyHint string
}

// SKULookupProvider is implemented by every provider that supports raw-SKU
// lookup (today: AWS via *awsprovider.Provider.LookupSKUAcrossRegionsGeneric,
// GCP via *gcpprovider.Provider.LookupSKUAcrossRegionsGeneric). The
// tool-handler layer (internal/tools) resolves a concrete provider to this
// interface once, instead of hardcoding *awsprovider.Provider at every raw-SKU
// call site.
type SKULookupProvider interface {
	LookupSKUAcrossRegionsGeneric(ctx context.Context, sku string, regions []string, serviceHint string, hint SKUHint) (*SKULookupResult, error)
}

// --------------------------------------------------------------------------
// Structured errors
// --------------------------------------------------------------------------

// Error codes returned via SKULookupError.Code, for the tool-handler layer to
// switch on when building a structured JSON error response. Exact string
// values are unchanged from their original home in aws_sku_lookup.go — every
// existing caller (including any that may compare against the literal
// string, not just the const) keeps working.
const (
	SKUErrUnsupportedProvider   = "unsupported_provider"
	SKUErrSKURequired           = "sku_required"
	SKUErrSKUTooLong            = "sku_too_long"
	SKUErrRegionsRequired       = "regions_required"
	SKUErrTooManyRegions        = "too_many_regions"
	SKUErrInvalidService        = "invalid_service"
	SKUErrServiceUndeterminable = "service_undeterminable"
	SKUErrHintTooLong           = "hint_too_long"
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

// --------------------------------------------------------------------------
// Hint-resolution status codes
// --------------------------------------------------------------------------

// Hint-resolution status codes, surfaced on SKULookupRegionResult.HintStatus
// so a caller can tell *why* a region is still ambiguous rather than just
// seeing "ambiguous: true" again. Exact string values are unchanged from
// their original home in aws_sku_lookup.go.
const (
	// HintStatusNoHint means no disambiguating hint was supplied — resolution
	// fell back to the provider's own default narrowing (or, if that also
	// failed to narrow, plain ambiguity).
	HintStatusNoHint = "no_hint_supplied"
	// HintStatusResolved means a supplied hint narrowed the candidates to
	// exactly one row.
	HintStatusResolved = "resolved_by_hint"
	// HintStatusNoMatch means a hint was supplied but matched zero candidate
	// rows — resolution fails closed: the original unfiltered candidate set
	// is returned, still ambiguous, rather than silently ignoring the hint.
	HintStatusNoMatch = "hint_no_match"
	// HintStatusAmbiguous means a hint was supplied and matched more than one
	// row (canonical-default-style narrowing was then tried on that
	// hint-filtered subset and still could not get to exactly one).
	HintStatusAmbiguous = "hint_ambiguous"
)

// --------------------------------------------------------------------------
// Public result types
// --------------------------------------------------------------------------

// SKULookupRegionResult is the per-region outcome of a SKU lookup. Exactly
// one of the following holds, mirroring how the rest of this codebase
// distinguishes "we looked and found nothing" from "we couldn't look":
//   - len(Prices) > 0: a match was found; ServiceUsed names the service
//     (AWS servicecode, or GCP service ID) whose catalog contained it.
//   - NoMapping == true: every candidate service's catalog was fetched
//     successfully for this region, but no product row matched the input
//     SKU. This is the explicit "no mapping found" result the caller needs
//     to distinguish "priced but not in this region" (or "not modeled by
//     this provider at all") from a transient failure.
//   - Error != "": the region shape check failed, or every candidate
//     service's catalog fetch itself failed (e.g. network/HTTP error) — we
//     don't actually know whether a match exists.
type SKULookupRegionResult struct {
	Region string `json:"region"`

	// ServiceUsed names the service whose catalog produced the match (AWS
	// servicecode, or GCP service ID). Only set when len(Prices) > 0.
	ServiceUsed string `json:"service_used,omitempty"`

	// ServiceMismatch is true when ServiceUsed differs from the caller's
	// explicit service hint — i.e. the hint's catalog had no match, but a
	// fallback catalog did.
	ServiceMismatch bool `json:"service_mismatch,omitempty"`

	// Prices holds the resolved candidate row(s) for this region. In the
	// common case this is a single row. When multiple product rows are
	// legitimate alternates requiring disambiguation, all remaining
	// candidates are kept here and Ambiguous is set, rather than silently
	// picking one and reporting it as *the* price. See Tiered below for the
	// different (non-ambiguous) case of one matched item with more than one
	// usage-volume tier.
	Prices []models.NormalizedPrice `json:"prices,omitempty"`

	// Ambiguous is true when Prices contains more than one row that the
	// provider could not narrow down to a single canonical match — the
	// caller must disambiguate using Prices[i].Attributes / Description /
	// SKUID rather than trusting a single "the" price.
	Ambiguous bool `json:"ambiguous,omitempty"`

	// HintStatus explains *why* Ambiguous is what it is — see the
	// HintStatus* constants above. Only meaningful when there was more than
	// one candidate to disambiguate in the first place.
	HintStatus string `json:"hint_status,omitempty"`

	// Tiered is true when Prices holds multiple genuine usage-volume tiers
	// of ONE matched item (ascending by usage threshold), not alternate
	// candidates requiring disambiguation. AWS never sets this (always
	// false/omitted, since its usage-type suffix model does not surface
	// tiered rate schedules through this path). GCP sets it when a matched
	// SKU has more than one tiered rate (see gcp_sku_lookup.go).
	Tiered bool `json:"tiered,omitempty"`

	NoMapping bool `json:"no_mapping,omitempty"`

	Error string `json:"error,omitempty"`

	// AttemptedServices lists the services searched for this region (AWS
	// servicecodes, or GCP service IDs), in search order, for
	// diagnostic/debugging purposes.
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
	// cross-region matching (see stripUsageTypePrefix in
	// aws_sku_lookup.go). These are AWS-only concepts — a raw AWS
	// usage-type/SKU string encodes a region prefix that other providers'
	// SKU identifiers have no equivalent of — and are left empty ("") by
	// every other provider.
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
