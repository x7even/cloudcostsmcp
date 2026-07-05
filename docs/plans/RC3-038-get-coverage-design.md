# RC3-038 / #38 — `get_coverage` design note

## Scope fork resolved: `fallback` is runtime, not static-per-service

The issue proposed composing coverage "from the same per-price `fallback`
attribute already computed today, fanned out across ... the region list."

Checked where `fallback` is actually set:
`opencloudcosts-go/internal/providers/aws/aws.go:79` (`bulkFallback`,
flipped when the AWS bulk pricing offer file fails to fetch) and
`opencloudcosts-go/internal/providers/gcp/gcp_analytics.go:250,414`
(BigQuery breakdown fallback). Both are **live, per-request outcomes** —
whether a fetch degraded just now — not a fixed property of
(domain, service, region) knowable ahead of time.

A true per-region fan-out would mean issuing a live `get_price` call for
every (domain × service × region) combination on every `get_coverage`
call — unbounded cost that scales with region-list size per provider
(dozens of regions × dozens of services), with no caching story. That is
out of scope for v1.

## v1 scope: structural coverage only

`get_coverage(provider)` reports, per domain/service, one of:
- `"catalog"` — service is present in `DescribeCatalog().Services[domain]`
- `"absent"` — service is not in the catalog for this provider (e.g. the
  known GCP gaps found during RC3 triage)

Region is **not** fanned out in v1. The `as_of` field is the catalog
snapshot time, not a promise of live per-region freshness. The tool
docstring says explicitly: live fallback/degraded status for a specific
region is still only observable by calling `get_price` for that spec —
`get_coverage` answers "does this server know about this service at
all," not "is today's live price for this region a real catalog rate or
a fallback constant."

This matches DACI alternative (a) — a thin wrapper over
`DescribeCatalog()` rather than new AWS-FinOps-specific plumbing — and
keeps the response contract cheap to reason about ahead of owner
sign-off on the full per-region contract.
