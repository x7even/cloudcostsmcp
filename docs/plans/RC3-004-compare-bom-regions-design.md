# RC3-004 / #31 — `compare_bom_regions` design note

## v1 scope: AWS-only

Per the issue's own DACI alternative (a): v1 ships AWS-only, synchronous
per-line region fan-out. GCP/Azure line items are reported with
`"source": "not_supported"` rather than guessed or dropped silently.

## Why AWS-only v1 does not lock in an AWS-specific schema

The input/output contract is already provider-agnostic, because
`compare_bom_regions` is composed entirely from existing cross-provider
machinery rather than new AWS-specific plumbing:

- **Line-item spec shape** — reuses `estimate_bom`'s `processBOMItems`
  (`bom.go`), which already accepts open PricingSpec-dict items for
  `aws`/`gcp`/`azure` today via `provider` on each item. Nothing about the
  dict shape changes for AWS-only v1.
- **Region fan-out** — reuses `compare_prices`' `Regions []string` +
  `BaselineRegion` + `applyBaselineDeltas` pattern (`lookup.go`), which is
  provider-agnostic: it fans out over `(region)` for whatever `PricingSpec`
  the item resolves to, regardless of provider.
- **Raw-SKU line items** — the one AWS-specific piece is reusing
  `get_price_by_sku`'s CUR usage-type resolver (`sku_lookup.go`) for line
  items given as a raw SKU string instead of a spec dict. This is an
  *optional* input variant (`{"sku": "..."}` vs `{"provider": ..., ...}`),
  not the core mechanism — GCP/Azure line items simply use the spec-dict
  form until an equivalent raw-SKU resolver exists for those providers
  (tracked separately, e.g. #35 for GCP).

**Consequence:** adding GCP/Azure support later is enabling those
providers in the same `processBOMItems` dispatch + region fan-out — not a
schema migration or a v2 rewrite. The `providers?` filter parameter in the
tool signature already anticipates this: v1 accepts it but only AWS
resolves to real prices, GCP/Azure entries in the filter (if requested)
come back tagged `not_supported` rather than being rejected outright.

## Response shape

Uses the canonical `price_per_unit` field established in #30 (PR #74) from
day one — this tool did not exist before #30 landed, so there is no legacy
shape to migrate.

## Dependencies incorporated

- RC3-006 (fallback passthrough) and RC3-005 (float weight) — assumed
  landed in the input contract this tool builds on.
- RC3-002's partial-result reliability contract — a missing baseline
  region or an unresolvable line item degrades to a partial payload with
  explicitly-nulled delta fields, not a hard top-level error.
