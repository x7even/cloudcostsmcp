# T41 · `get_price_by_sku` — AWS raw usage-type/SKU lookup

**Status:** done
**Depends on:** none (AWS-only, additive)
**Branch:** worktree-sku-lookup-tool

## Overview

Cost & Usage Report (CUR) exports and other AWS billing data identify line items by a raw
`UsageType`/SKU string (e.g. `"CAN1-BoxUsage:r5a.8xlarge"`, `"CAN1-AWS-Out-Bytes"`) rather than the
`resource_type`/domain shape the rest of this server's tools expect. Reconciling a billing export
against current public pricing today requires a human to manually decode the region-prefix
convention and guess the AWS servicecode. `get_price_by_sku` automates that: given a raw usage-type
string and a target region list, it resolves the matching public price(s), distinguishing
"checked, no catalog entry" from "fetch failed" and surfacing service-inference/mismatch
provenance so callers can trust or double-check the result.

## Files changed

- `internal/providers/aws/aws_sku_lookup.go` — core matching logic: prefix-stripping, service
  inference, per-region catalog fetch + memoization, multi-row disambiguation
  (`resolveSKUCandidates`), `LookupSKUAcrossRegions` entry point.
- `internal/tools/sku_lookup.go` — `GetPriceBySKUInput`, `HandleGetPriceBySKU`; type-asserts the
  provider-agnostic `providers.Provider` to `*aws.Provider` to reach the AWS-only core function.
- `internal/server/server.go` — `schemaGetPriceBySKU`, `descGetPriceBySKU`, tool registration
  (`mcp.AddTool` for `"get_price_by_sku"`).
- `internal/providers/aws/aws_sku_lookup_test.go`, `internal/tools/sku_lookup_test.go` — unit and
  handler-level tests (see "Test coverage" below).
- `internal/providers/aws/testhooks.go` — narrow test-only seams (`SetBulkPricingBaseURLForTesting`,
  `ResetSKUCatalogCacheForTesting`) letting `internal/tools`' tests drive the handler end-to-end
  against a mocked bulk-pricing endpoint without a real network call.
- `docs/tools.md`, `docs/roadmap.md`, `CHANGELOG.md`, `server.json` — documentation and version bump.

## Design decisions

### No hardcoded region → prefix table

AWS usage-type strings encode the region as a short prefix token (`"CAN1-BoxUsage:..."`), but the
mapping from region code to prefix is **not derivable from the region code itself** and is not
documented anywhere authoritative. Live verification against
`https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/{service}/current/{region}/index.json`
turned up two cases that would have broken a table built from memorized/assumed conventions:

- `ca-west-1` → `"CAN2"`, not the more "obvious" guess `"CAW1"`.
- `mx-central-1` → `"MXC1"`, not `"MXO1"`.
- `eu-west-1` → the bare literal `"EU"` (two letters, **zero** digits) — the only prefix that
  doesn't match the otherwise-universal `^[A-Z]{2,4}[0-9]{1,2}$` shape.
- `us-east-1` → **no prefix token at all**; usage types start directly with the operation name
  (`"BoxUsage:r6id.24xlarge"`).

Given that a static table would (a) require manual upkeep as AWS adds regions, and (b) has already
been shown to produce wrong answers from "reasonable" guesses, the design instead:

1. Takes `regions` as **required, explicit input** (real AWS region codes) — the tool never needs
   to reverse a prefix back into a region code.
2. Strips the prefix token from both the input SKU and every candidate catalog row **by shape**
   (regex `^(?:[A-Z]{2,4}[0-9]{1,2})+-`, plus the literal `"EU"` exception), producing a
   region-independent "suffix".
3. Matches two usage-type strings by comparing their stripped suffixes for byte-equality.

This is forward-compatible with new AWS regions without a code change, and correctly handles the
`eu-west-1`/`us-east-1` exceptions because it never has to know what a given prefix "means" — only
whether a token has prefix-like shape.

### Service-code inference and mismatch-tolerant fallback

The optional `service` param lets a caller supply a known AWS servicecode (e.g. `"AmazonEC2"`).
When omitted, the servicecode is inferred from the usage-type pattern (see the table in
`inferServiceFromUsageType`) — this is a heuristic over ~6 known usage-type families, not
exhaustive, so the response always marks `service_source: "inferred"` in that case plus a warning
suggesting the caller pass `service=` explicitly for anything ambiguous.

Real CUR exports are not always internally consistent with the pricing API's own servicecode
values: the field brief's own worked example, `"CAN1-AWS-Out-Bytes"`, is commonly seen in exports
against an `"AWS Product"` column of `AmazonEC2`, but the verified live servicecode for that
usage-type shape is actually `AWSDataTransfer`. Rather than let an explicit-but-wrong `service` hint
produce a hard "no match", the lookup tries the hint first and, if it fails, also tries the inferred
servicecode (when it differs from the hint) — and returns the inferred match with
`service_mismatch: true` if that succeeds. Both are attempted before giving up.

### Catalog memoization

Each `(service, region)` bulk-pricing offer file fetch is memoized for the process lifetime — offer
files are large, and multiple regions/services are frequently checked in the same request
(`AWSDataTransfer` is tried as a fallback candidate on nearly every lookup with a hint). Plain
`sync.Once` per key was rejected because it would permanently pin a single transient network
failure into the cache for the rest of the process's life; the implementation instead evicts the
cache entry on fetch failure so a later retry can succeed, while still de-duplicating concurrent
in-flight successful fetches for the same key.

### Multi-row-per-suffix disambiguation

A stripped usage-type suffix is not always unique within a (service, region) catalog — AWS
usage-type strings do not encode every attribute that distinguishes one priced product row from
another with the same shape. The clearest example is EC2's `BoxUsage:<instanceType>` suffix, which
matches one row per operatingSystem × tenancy × preInstalledSw × capacitystatus combination
(Linux/Windows/RHEL, Shared/Dedicated/Host, with/without SQL Server, on-demand/reserved-capacity),
all sharing the identical usage-type suffix. Silently returning "the cheapest of these" can be
materially wrong (the cheapest row for a given suffix is very often Dedicated-tenancy or
Windows-with-SQL, not the plain Linux/Shared row a caller reconciling a CUR export almost always
means).

`resolveSKUCandidates` narrows multi-row matches down to the same canonical-default attribute set
(`operatingSystem=Linux`, `tenancy=Shared`, `preInstalledSw=NA`, `capacitystatus=Used`) that
`computeFilters` in `aws_pricing.go` already uses for `get_price`/`search_pricing`, reusing that
existing convention rather than inventing a second one. A row missing a given key entirely (e.g. an
RDS row has no `tenancy` attribute) is left unfiltered on that key, not excluded — so narrowing only
ever disambiguates EC2-shaped rows and has no effect on services that don't carry these attributes.
If narrowing resolves to exactly one row, it is returned as the sole match. If it resolves to zero or
more than one row (the suffix genuinely doesn't disambiguate them — RDS `databaseEngine` variants are
not resolvable this way), the entry is flagged `ambiguous: true` with every original candidate row
surfaced in `alternate_matches`, rather than arbitrarily picking one and reporting it as *the* price.

### Compound / Wavelength `AWSDataTransfer` SKUs — known limitation

Some `AWSDataTransfer` usage types carry **two** region-shaped tokens rather than one, e.g.
`"CAN1-DEN1-AWS-Out-Bytes"` (inter-region transfer) or `"USE1WL1ATL1-CAN1-AWS-Out-Bytes"` (AWS
Wavelength, where `USE1WL1ATL1` is itself three concatenated sub-tokens). The tool's single-prefix
matching model strips only the first token; for these compound cases, the remaining string can
still resemble `"{prefix}-suffix"` after one strip, meaning it does not reliably reduce to a
region-independent form. Detecting the case is done by shape (checking whether the remaining suffix
*itself* starts with another prefix-shaped token followed by `-`), and rather than either silently
producing a wrong match or refusing to answer, the tool still attempts the lookup (inferring
`AWSDataTransfer`) but adds an explicit warning: *"sku looks like a multi-region/wavelength
data-transfer usage type; result may be inaccurate — verify manually."*

## Tool signature

```go
type GetPriceBySKUInput struct {
    Provider       string   `json:"provider"`
    SKU            string   `json:"sku"`
    Service        string   `json:"service"`
    Regions        []string `json:"regions"`
    BaselineRegion string   `json:"baseline_region"`
}

func (h *Handler) HandleGetPriceBySKU(
    ctx context.Context, req *mcp.CallToolRequest, in GetPriceBySKUInput,
) (*mcp.CallToolResult, any, error)
```

Core AWS-only logic (not on the shared `providers.Provider` interface — raw CUR usage-type/SKU
strings are an AWS-specific billing concept with no GCP/Azure equivalent):

```go
func (p *Provider) LookupSKUAcrossRegions(
    ctx context.Context,
    providerName string,
    sku string,
    serviceHint string,
    regions []string,
) (*SKULookupResult, error)
```

## Response shape

Mirrors `compare_prices`: a cheapest-first sorted per-region list, plus provenance and diagnostic
fields specific to SKU resolution.

```json
{
  "sku": "CAN1-BoxUsage:r5a.8xlarge",
  "usage_type_prefix": "CAN1",
  "usage_type_suffix": "BoxUsage:r5a.8xlarge",
  "service_source": "inferred",
  "inferred_service": "AmazonEC2",
  "all_regions_sorted": [
    {
      "region": "us-east-1",
      "region_name": "us-east-1",
      "price_per_unit": {"amount": 1.5104, "unit": "per_hour", "currency": "USD", "display": "$1.510400/per_hour"},
      "monthly_estimate": {"amount": 1102.59, "currency": "USD", "display": "$1102.59/mo"},
      "service_used": "AmazonEC2",
      "description": "Linux/UNIX",
      "attributes": {"operatingSystem": "Linux", "tenancy": "Shared"},
      "sku_id": "ABCD1234EFGH5678"
    },
    {
      "region": "eu-west-1",
      "region_name": "eu-west-1",
      "price_per_unit": {"amount": 1.55, "unit": "per_hour", "currency": "USD", "display": "$1.550000/per_hour"},
      "service_used": "AmazonEC2",
      "ambiguous": true,
      "alternate_match_count": 2,
      "alternate_matches": [
        {"price_per_unit": {"amount": 1.55, "unit": "per_hour", "currency": "USD", "display": "$1.550000/per_hour"}, "description": "Linux/UNIX (Dedicated)"},
        {"price_per_unit": {"amount": 1.60, "unit": "per_hour", "currency": "USD", "display": "$1.600000/per_hour"}, "description": "Windows"}
      ]
    }
  ],
  "cheapest_region": "us-east-1",
  "cheapest_price": {"amount": 1.5104, "unit": "per_hour", "currency": "USD", "display": "$1.510400/per_hour"},
  "most_expensive_region": "af-south-1",
  "most_expensive_price": {"amount": 1.62, "unit": "per_hour", "currency": "USD", "display": "$1.620000/per_hour"},
  "price_delta_pct": 7.2,
  "no_mapping_in": [
    {"region": "eu-central-1", "attempted_services": ["AmazonEC2"]}
  ],
  "errors_in": [],
  "warnings": []
}
```

Notes:
- The top-level sorted list is named `all_regions_sorted` and each price is shaped via the same
  `priceDict`/`moneyDict` helpers `compare_prices` uses (`{amount, unit, currency, display}` and
  `{amount, currency, display}` respectively) — deliberately matching field names/shapes rather
  than inventing a parallel convention.
- `cheapest_region`/`cheapest_price`, `most_expensive_region`/`most_expensive_price`, and
  `price_delta_pct` mirror `compare_prices`' spread fields, computed over the matched regions only.
- `no_mapping_in` (catalog fetched fine, no matching suffix) is kept distinct from `errors_in` (all
  candidate service fetches for that region genuinely failed) — this lets callers tell
  "priced but not available here" apart from "we couldn't check". These two fields are additions
  specific to this tool's domain (SKU resolution can fail in more granular ways than a plain
  region-availability check), not present on `compare_prices`.
- `service_mismatch: true` is added to a region entry when the match came from the inferred
  servicecode after the supplied `service` hint's own catalog produced no match.
- Each matched entry may also carry `description`/`attributes`/`sku_id` (provenance from the
  underlying product row, when present) to help a caller reconciling a CUR line item confirm the
  match rather than trusting a bare number.
- `ambiguous: true` (plus `alternate_match_count`/`alternate_matches`) is added to an entry when
  `resolveSKUCandidates` cannot narrow multiple product rows sharing the suffix down to a single
  canonical-default match (see "Multi-row-per-suffix disambiguation" above); `price_per_unit` for
  that entry is only the cheapest of the alternatives, not a confirmed match. A top-level warning is
  added to `warnings` whenever any region hits this case.
- `baseline_region`, when supplied, adds the standard delta-vs-baseline fields to each entry
  (same `applyBaselineDeltas` helper used by `compare_prices`/`find_cheapest_region`).
- If no region matches, `result: "no_prices_found"` is returned with a pointer to `no_mapping_in`
  / `errors_in` for diagnosis, rather than an empty/ambiguous response.
- `provider` defaults to `"aws"` in the JSON Schema, but the go-sdk does not apply schema defaults
  to bound Go structs — the handler coalesces an omitted `provider` field to `"aws"` itself, the
  same convention `HandleGetDiscountSummary` uses for its own `default: "aws"` field.

## Acceptance criteria

- `provider` defaults to (and only accepts) `"aws"`; other providers get a structured
  `unsupported_provider` error, not a panic or generic 500.
- `sku` required, non-empty; `regions` required, 1–30 entries — validated before any network I/O.
- Prefix-stripping is shape-based (no hardcoded region table), correctly handles the `eu-west-1`
  `"EU"` exception and the no-prefix `us-east-1` case.
- Explicit `service` hint that yields no match falls back to the inferred servicecode (if
  different) before giving up, and flags `service_mismatch` when that fallback is what matched.
- `no_mapping_in` and `errors_in` are reported separately, never conflated.
- Compound inter-region/Wavelength `AWSDataTransfer` SKUs still produce a best-effort match plus an
  explicit warning, not a silent wrong answer or a hard failure.
- Multi-row-per-suffix matches that can't be narrowed to a single canonical-default row are surfaced
  as `ambiguous: true` with every candidate in `alternate_matches`, never silently collapsed to "the
  cheapest."
- `go build ./...`, `go vet ./...`, `go test -race ./...`, `govulncheck ./...`, and
  `golangci-lint run --enable=errcheck,staticcheck,gosec,gocritic` all pass cleanly.

## Test coverage

A dedicated test suite was added covering both layers:

- `internal/providers/aws/aws_sku_lookup_test.go` — prefix-stripping (including the `EU`/`us-east-1`
  shape exceptions), service inference, per-region match/no-mapping/fetch-failure outcomes,
  request-level validation (empty/too-many regions, wrong provider, SKU length), per-region
  validation that's non-fatal to the rest of the request (China-partition and malformed-region-shape
  rejection alongside a region that still resolves), the explicit-hint-fails →
  inferred-fallback → `service_mismatch: true` path, the compound-transfer warning for both fixture
  SKUs (`"CAN1-DEN1-AWS-Out-Bytes"`, `"USE1WL1ATL1-CAN1-AWS-Out-Bytes"`), the catalog memoization
  cache (including TTL expiry and capacity eviction), and the multi-row disambiguation logic
  (canonical-default narrowing vs. genuine ambiguity).
- `internal/tools/sku_lookup_test.go` — end-to-end `HandleGetPriceBySKU` tests (via
  `SetBulkPricingBaseURLForTesting`/`ResetSKUCatalogCacheForTesting` in `testhooks.go`) covering the
  happy path, ambiguous-match surfacing, no-mapping regions, missing-required-field errors, and the
  `provider` default-to-`"aws"` behavior.

## Not yet covered (next phase)

The current suite exercises the core matching, validation, and disambiguation logic plus the handler
wiring; it does not yet include:

- Golden/snapshot tests pinning the exact `all_regions_sorted` JSON shape (schema drift would
  currently only be caught by manual doc review, as it was here).
- Fixtures built from real (anonymized) CUR export usage-type strings, beyond the hand-written
  examples in the design-decisions section above.
- Load/perf testing of the catalog-memoization cache under the ~1000-SKU × ~10-region reconciliation
  workload the design doc's cache section is sized for.
