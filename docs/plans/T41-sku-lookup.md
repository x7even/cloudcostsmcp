# T41 · `get_price_by_sku` — AWS raw usage-type/SKU lookup

**Status:** done (RC2)
**Depends on:** none (AWS-only, additive)
**Branch:** worktree-sku-lookup-tool

## RC2 revision (post field-test fixes)

RC1 field-testing against 10 real CUR SKUs across 10 regions found two bugs, both fixed in RC2:

1. **Silent-wrong-price on ambiguous matches (data accuracy, HIGH).** Ambiguous multi-product
   matches (e.g. ELB `LCUUsage` spanning Application/Network/Gateway LB pricing, RDS
   `InstanceUsage:<type>` spanning every DB engine on that instance type) were flagged
   `ambiguous: true` correctly, but the headline `price_per_unit` and the top-level
   `cheapest_price`/`most_expensive_price` summary were still computed by picking the cheapest
   alternate — a different product at a different price, not a confirmed match. Fixed by: (a) adding
   optional `operation`/`product_family` disambiguating hints (matched against the AWS product
   `operation` attribute and top-level `productFamily`, case-insensitively) that a caller can supply
   from the adjacent CUR column to deterministically resolve the correct row, and (b) when a region
   is still ambiguous after hint/canonical-default narrowing, excluding it entirely from
   `all_regions_sorted`/`cheapest_price`/`most_expensive_price`/baseline deltas and surfacing it only
   in a dedicated `ambiguous_in` bucket with every candidate row and no chosen price — "cheapest" is
   never used as an ambiguity tie-breaker. See "Multi-row-per-suffix disambiguation" and "Response
   shape" below for the current (post-fix) behavior.
2. **EC2/EBS SKU lookups timing out (availability, CRITICAL).** `get_price_by_sku`'s per-SKU term
   assembly re-scanned the *entire* flattened terms map with a `strings.HasPrefix` check for every
   product SKU (O(P×M) over the offer file's product count P and term count M) to re-derive a
   sku→terms grouping that the streaming decoder already had for free. For AmazonEC2's offer file
   (~120K products/terms, EBS pricing bundled into the same file) this made every lookup effectively
   unusable regardless of timeout. Fixed in `aws_bulk.go`'s `collectTermsForSKUs` / `getProductsBulk`
   by preserving the sku→termKey→raw grouping the decoder produces while streaming, turning the
   per-SKU assembly into an O(1) map lookup instead of a linear rescan — this is the same
   `getProductsBulk` function both `get_price_by_sku` and the existing `get_price`/`get_prices_batch`
   fast paths share, so the fix applies to both. Verified empirically post-fix: a cold-cache
   `CAN1-BoxUsage:r5a.8xlarge` lookup across `["us-east-1","ca-central-1"]` completes in ~11s, and a
   cold-cache `CAN1-EBS:VolumeUsage.gp3` lookup completes in ~9s — both well inside the 60s default
   `OCC_REQUEST_TIMEOUT` and the 120s per-HTTP-fetch client timeout in `aws_bulk.go`.

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

**RC2 additions:** `internal/providers/aws/aws_bulk.go` (`collectTermsForSKUs`/`getProductsBulk` —
the O(P×M)→O(1) per-SKU term-lookup fix behind BUG 2, shared with `get_price`/`get_prices_batch`)
and `internal/providers/aws/aws_bulk_test.go`, in addition to further changes across the files
above for the `operation`/`product_family` hints, `hint_status`, and `ambiguous_in` behind BUG 1 —
see "RC2 revision" at the top of this document.

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
another with the same shape. The clearest examples are EC2's `BoxUsage:<instanceType>` suffix
(matches one row per operatingSystem × tenancy × preInstalledSw × capacitystatus combination),
ELB's `LCUUsage` suffix (matches Application/Network/Gateway LB capacity pricing alike — distinct
products, not variants of one product), and RDS's `InstanceUsage:<type>` suffix (matches every
database engine on that instance type). Silently returning "the cheapest of these" is not just
imprecise but can be a materially wrong *product* — RC1 field-testing found this picked Gateway LB
(roughly half the correct Application LB price) and plain PostgreSQL instead of Aurora PostgreSQL
(15-25% under the correct price) — so `resolveSKUCandidates` never uses "cheapest" as a tie-breaker.
Resolution is tried in this order:

1. **Caller-supplied hint** (`operation` and/or `product_family` — see the tool signature below).
   Candidates are filtered to rows whose `Attributes["operation"]` and/or `ProductFamily` match the
   supplied hint(s) case-insensitively.
   - Exactly one match: resolved (`hint_status: "resolved_by_hint"`), no further narrowing.
   - Zero matches: fails closed — the hint is never silently ignored. The *original, unfiltered*
     candidate set is returned, still ambiguous, `hint_status: "hint_no_match"`.
   - More than one match: canonical-default narrowing (below) is tried on the hint-filtered subset;
     if that resolves to one row, resolved; otherwise still ambiguous,
     `hint_status: "hint_ambiguous"`, with the hint-filtered (not full) subset as candidates.
2. **Canonical-default narrowing** (used when no hint is supplied, or as the second pass within case
   3 above): candidates are filtered down to the same canonical-default attribute set
   (`operatingSystem=Linux`, `tenancy=Shared`, `preInstalledSw=NA`, `capacitystatus=Used`) that
   `computeFilters` in `aws_pricing.go` already uses for `get_price`/`search_pricing`, reusing that
   existing convention rather than inventing a second one. A row missing a given key entirely (e.g.
   an RDS row has no `tenancy` attribute) is left unfiltered on that key, not excluded — so this only
   ever disambiguates EC2-shaped rows and has no effect on services that don't carry these
   attributes. If it resolves to exactly one row, that row is the sole match
   (`hint_status: "no_hint_supplied"` when no hint was involved).

If none of the above resolves to exactly one row, the region is reported in the response's
`ambiguous_in` bucket (not inline in `all_regions_sorted`) with every remaining candidate row in
`alternate_matches` and no chosen price — the region is excluded from
`all_regions_sorted`/`cheapest_price`/`most_expensive_price`/baseline deltas entirely. See "Response
shape" below.

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

    // Operation and ProductFamily are optional disambiguating hints — see
    // "Multi-row-per-suffix disambiguation" above. They correspond to the AWS
    // product "operation" attribute (e.g. "CreateDBInstance:0021") and
    // top-level productFamily (e.g. "Load Balancer-Application") respectively
    // — the same columns a real CUR export carries alongside the usage-type/
    // SKU column.
    Operation     string `json:"operation"`
    ProductFamily string `json:"product_family"`
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
    operationHint string,
    productFamilyHint string,
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
    }
  ],
  "cheapest_region": "us-east-1",
  "cheapest_price": {"amount": 1.5104, "unit": "per_hour", "currency": "USD", "display": "$1.510400/per_hour"},
  "most_expensive_region": "us-east-1",
  "most_expensive_price": {"amount": 1.5104, "unit": "per_hour", "currency": "USD", "display": "$1.510400/per_hour"},
  "ambiguous_in": [
    {
      "region": "eu-west-1",
      "service_used": "AmazonEC2",
      "hint_status": "no_hint_supplied",
      "alternate_match_count": 2,
      "alternate_matches": [
        {"price_per_unit": {"amount": 1.55, "unit": "per_hour", "currency": "USD", "display": "$1.550000/per_hour"}, "description": "Linux/UNIX (Dedicated)", "attributes": {"operatingSystem": "Linux", "tenancy": "Dedicated"}, "sku_id": "SKU-EU-DEDICATED"},
        {"price_per_unit": {"amount": 1.60, "unit": "per_hour", "currency": "USD", "display": "$1.600000/per_hour"}, "description": "Windows", "attributes": {"operatingSystem": "Windows", "tenancy": "Shared"}, "sku_id": "SKU-EU-WINDOWS"}
      ]
    }
  ],
  "no_mapping_in": [
    {"region": "eu-central-1", "attempted_services": ["AmazonEC2"]}
  ],
  "errors_in": [],
  "warnings": [
    "one or more regions matched multiple distinct product rows for this usage-type suffix that could not be narrowed to a single unambiguous match (see ambiguous_in) — those regions are excluded from all_regions_sorted/cheapest_price/most_expensive_price rather than defaulting to a guessed price; supply operation and/or product_family hints (see each ambiguous_in entry's alternate_matches for the candidate values) to resolve them"
  ]
}
```

Notes:
- The top-level sorted list is named `all_regions_sorted` and each price is shaped via the same
  `priceDict`/`moneyDict` helpers `compare_prices` uses (`{amount, unit, currency, display}` and
  `{amount, currency, display}` respectively) — deliberately matching field names/shapes rather
  than inventing a parallel convention.
- `cheapest_region`/`cheapest_price`, `most_expensive_region`/`most_expensive_price`, and
  `price_delta_pct` mirror `compare_prices`' spread fields, computed over the matched (non-ambiguous)
  regions only — a region in `ambiguous_in` never contributes to these.
- `no_mapping_in` (catalog fetched fine, no matching suffix) is kept distinct from `errors_in` (all
  candidate service fetches for that region genuinely failed) and from `ambiguous_in` (matched, but
  more than one distinct product row and not yet resolved) — this lets callers tell "priced but not
  available here", "we couldn't check", and "matched but needs a hint to disambiguate" apart. These
  fields are additions specific to this tool's domain (SKU resolution can fail/branch in more
  granular ways than a plain region-availability check), not present on `compare_prices`.
- `service_mismatch: true` is added to a region entry when the match came from the inferred
  servicecode after the supplied `service` hint's own catalog produced no match.
- Each matched entry may also carry `hint_status` (see below), `description`/`product_family`/
  `attributes`/`sku_id` (provenance from the underlying product row, when present) to help a caller
  reconciling a CUR line item confirm the match rather than trusting a bare number.
- `hint_status`, one of `resolved_by_hint`/`hint_no_match`/`hint_ambiguous`/`no_hint_supplied`
  (`aws.HintStatus*`), explains *why* a region landed where it did. On a matched entry it is only
  present (non-default) when `resolved_by_hint` — i.e. a supplied `operation`/`product_family` hint
  is what did the disambiguating work, not just canonical-default narrowing. On an `ambiguous_in`
  entry it is always present and tells the caller whether no hint was supplied at all
  (`no_hint_supplied`), a supplied hint matched zero candidates (`hint_no_match` — the hint value is
  likely wrong), or a supplied hint narrowed but not to exactly one row (`hint_ambiguous`).
- **`ambiguous_in`**: when `resolveSKUCandidates` cannot narrow multiple distinct product rows
  sharing the suffix down to a single match (via hint and/or canonical-default narrowing — see
  "Multi-row-per-suffix disambiguation" above), that region is *excluded* from
  `all_regions_sorted`/`cheapest_price`/`most_expensive_price`/baseline deltas entirely and instead
  appears as its own entry in `ambiguous_in`, with `alternate_match_count`/`alternate_matches` (every
  remaining candidate row) and no chosen price — "cheapest of several different products" is never
  used as a default. A top-level warning is added to `warnings` whenever any region hits this case.
  Pass `operation` and/or `product_family` (see each `ambiguous_in` entry's `alternate_matches` for
  the candidate values to choose from) on a follow-up call to resolve it.
- `baseline_region`, when supplied, adds the standard delta-vs-baseline fields to each entry
  (same `applyBaselineDeltas` helper used by `compare_prices`/`find_cheapest_region`). If
  `baseline_region` itself is stuck in `ambiguous_in` rather than genuinely absent, a dedicated
  `baseline_region_ambiguous` error is returned instead of a generic "not found" error.
- If no region matches unambiguously, `result: "no_prices_found"` is returned with a pointer to
  `ambiguous_in` (matched but needs a hint), `no_mapping_in` (checked, not modeled), and `errors_in`
  (fetch failed) for diagnosis, rather than an empty/ambiguous response.
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
- Multi-row-per-suffix matches that can't be narrowed to a single row (via `operation`/
  `product_family` hint and/or canonical-default narrowing) are surfaced in `ambiguous_in` with
  every candidate in `alternate_matches`, and are excluded from
  `all_regions_sorted`/`cheapest_price`/`most_expensive_price`/baseline deltas — never silently
  collapsed to "the cheapest."
- A supplied `operation`/`product_family` hint that uniquely identifies one candidate row resolves
  the region deterministically (`hint_status: "resolved_by_hint"`); a hint that matches zero rows
  fails closed (`hint_status: "hint_no_match"`, ambiguity preserved) rather than being silently
  ignored.
- Per-SKU term assembly against large offer files (AmazonEC2/EBS) is O(1) per SKU via the streaming
  decoder's existing sku→terms grouping, not an O(P×M) rescan — verified empirically: cold-cache
  EC2 `BoxUsage` and EBS `VolumeUsage` lookups complete in single-digit seconds against the live
  bulk endpoint, not timing out as they did pre-fix.
- `go build ./...`, `go vet ./...`, `go test -race ./...`, `govulncheck ./...`, and
  `golangci-lint run --enable=errcheck,staticcheck,gosec,gocritic` all pass cleanly.

## Test coverage

A dedicated test suite was added covering both layers:

- `internal/providers/aws/aws_sku_lookup_test.go` — prefix-stripping (including the `EU`/`us-east-1`
  shape exceptions), service inference, per-region match/no-mapping/fetch-failure outcomes,
  request-level validation (empty/too-many regions, wrong provider, SKU length, oversized hint via
  `maxHintLength`), per-region validation that's non-fatal to the rest of the request (China-partition
  and malformed-region-shape rejection alongside a region that still resolves), the explicit-hint-fails
  → inferred-fallback → `service_mismatch: true` path, the compound-transfer warning for both fixture
  SKUs (`"CAN1-DEN1-AWS-Out-Bytes"`, `"USE1WL1ATL1-CAN1-AWS-Out-Bytes"`), the catalog memoization
  cache (including TTL expiry and capacity eviction), and — the RC2 addition —
  `TestResolveSKUCandidates_TableDriven`, a table-driven suite reproducing the RC1 field-test's exact
  worked examples (ELB `elbLCUCandidates()`: Application/Network/Gateway LB rows at $0.008/$0.006/
  $0.004; RDS `rdsEngineCandidates()`: 5 engine rows keyed by realistic `CreateDBInstance:00XX`
  operation codes, Aurora PostgreSQL at $1.104 deliberately more expensive than the cheapest
  alternative, PostgreSQL at $0.956) covering hint-resolves / hint-matches-zero-fails-closed /
  hint-narrows-to-multiple / no-hint-stays-ambiguous / single-row-with-contradicting-hint for both.
- `internal/tools/sku_lookup_test.go` — end-to-end `HandleGetPriceBySKU` tests (via
  `SetBulkPricingBaseURLForTesting`/`ResetSKUCatalogCacheForTesting` in `testhooks.go`) covering the
  happy path, no-mapping regions, missing-required-field errors, the `provider` default-to-`"aws"`
  behavior, and — the RC2 addition — handler-level assertions that an ambiguous region is excluded
  from `all_regions_sorted`/`cheapest_price`/`most_expensive_price` and appears only in
  `ambiguous_in`, that `operation`/`product_family` hints propagate through to resolve a region that
  would otherwise land there, and the `baseline_region_ambiguous` error path.

## Not yet covered (next phase)

The current suite exercises the core matching, validation, and disambiguation logic plus the handler
wiring; it does not yet include:

- Golden/snapshot tests pinning the exact `all_regions_sorted` JSON shape (schema drift would
  currently only be caught by manual doc review, as it was here).
- Fixtures built from real (anonymized) CUR export usage-type strings, beyond the hand-written
  examples in the design-decisions section above.
- Load/perf testing of the catalog-memoization cache under the full ~1000-SKU × ~10-region
  reconciliation workload the design doc's cache section is sized for. RC2's empirical smoke test
  (against the live bulk endpoint, not mocked) confirmed the O(P×M)→O(1) fix on individual
  previously-timing-out SKUs (single-digit-second cold-cache EC2/EBS lookups across 1-2 regions), but
  did not exercise the full 1089-row/10-region scale or concurrent multi-SKU fan-out against the live
  endpoint.
