# Changelog

All notable changes to OpenCloudCosts are documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

## [1.3.0] — 2026-07-09

### Added
- **Raw-SKU BoM line items** — `estimate_bom` and `compare_bom_regions` now accept raw
  usage-type/SKU dicts (`{"sku": ..., "provider": ..., "region": ..., "quantity": ...}`) as
  BoM line items alongside PricingSpec dicts, resolved via the same per-provider SKU resolver
  `get_price_by_sku` already uses. `compare_bom_regions`' region fan-out reports a raw-SKU
  item with an unsupported provider once in `not_supported`, not once per compared region.
  (RC3-004 / #31, #96)
- **GCP raw-SKU lookup parity** — `get_price_by_sku`, `get_prices_by_sku`, and raw-SKU BoM
  line items now accept `provider="gcp"`, resolving GCP Cloud Billing Catalog `skuId` strings
  the same way AWS CUR usage-type strings already resolve. GCP region attribution is
  geoTaxonomy-first with a serviceRegions fallback, since some SKUs (observed in KMS) carry a
  restrictive `serviceRegions` list despite being globally priced. Introduces a shared
  `internal/skulookup` package (`SKULookupProvider` interface) so AWS and GCP implement one
  contract instead of AWS-only types being reused by convention. (RC3-015 / #35, #97)
- **Azure raw-SKU lookup parity** — the same raw-SKU lookup surface (`get_price_by_sku`,
  `get_prices_by_sku`, BoM raw-SKU line items) now also accepts `provider="azure"`, resolving
  Azure Retail Prices API `meterId` strings. (RC3-015 / #35, #97)
- **GCP catalog coverage** — five new GCP services added to `describe_catalog`/`get_price`,
  closing the remaining zero-coverage gaps from the customer-feedback catalog sweep:
  - External IP pricing — standard and spot/preemptible VM-attached rates. (RC3-023a / #76, #91)
  - Cloud KMS pricing — 27 SKUs covering per-key-version and per-operation rates, software/HSM/
    external key versions, tiered HSM-asymmetric volume discounts, and Autokey allowances.
    (RC3-023b / #77, #92)
  - Cloud DNS pricing — tiered managed-zone monthly fees and tiered query volume.
    (RC3-023c / #78, #93)
  - Cloud Pub/Sub pricing — message throughput (basic + per-destination delivery + Schema/
    Message Transform) and storage, sourced from 77 live-verified SKUs. (RC3-023d / #79, #94)
  - Cloud Firestore pricing — document reads/writes/deletes, stored data, PITR storage, backups,
    and restore/clone, region-dispatched across ~41 regions plus multi-region groups.
    (RC3-023e / #80, #95)
- **Published per-tool output schemas** — every tool now advertises a JSON output schema via
  the MCP `tools/list` response (`mcp.Tool.OutputSchema`), checked into
  `schemas/tools-output-snapshot.json` with a parity test. (RC3-032 sub-part e / #30, #90)

### Fixed
- **MCP `structuredContent` now populated on every tool response** — every tool declares an
  `OutputSchema`, but responses only ever set the unstructured `Content` field. Strict MCP
  clients (including the official Python SDK) reject a tool call outright when a schema is
  declared but `structuredContent` is absent. Also fixes two response fields that could
  marshal to JSON `null` against a non-nullable array schema
  (`no_mapping_in[].attempted_services`, `upstream_failure`'s `regions`).
- **Raw-SKU BoM items require an explicit `provider`** — a raw-SKU BoM item omitting
  `provider` was previously defaulted to `"aws"`, risking a non-AWS SKU being misrouted into
  AWS's usage-type resolver when a BoM mixes items from multiple providers in one call. Now
  returns a clear "provider is required" error instead of guessing. (part of #97)
- **`price_per_unit` / `size_gb` / GCP breakdown consistency (RC3-032 remainder)** — BoM line
  items now build `price_per_unit` and `totals` via the shared `priceDict`/`moneyDict` helpers
  (fixing missing `currency`/`display` on `totals.monthly`/`totals.annual`); `get_price` now
  scales `monthly_estimate` for `per_gb_month` storage prices by `size_gb`, matching how
  `per_hour`/`per_month` prices already scale; GCP LB/CDN/NAT/Armor cost breakdowns use the
  same currency-typed map shape as the rest of the codebase. (#30, #89)
- **GCP egress price unit label** — internet/inter-region egress was labeled `per_gb_month`
  (implying a recurring monthly rate) when it's actually a flat `per_gb` transfer price;
  corrected to match CDN/NAT/LB data-processing prices in the same file. Note: this also
  changes how a GCP egress BoM line item computes `monthly_cost` (see #88 for the
  `bomMonthlyCost()` side effect this was reviewed against). (RC3-032 sub-part d / #30, #88)

### Security
- Go toolchain bumped to 1.25.12, resolving GO-2026-5856 (an Encrypted Client Hello privacy
  leak in `crypto/tls`, fixed upstream in 1.25.12).

## [1.2.0] — 2026-07-05

### Changed
- **BREAKING:** `get_price`, `get_prices_batch`, `get_price_by_sku`, `get_prices_by_sku`,
  and `effective_price` now return the unit-price sub-object under `price_per_unit`
  instead of `price`, matching the key `compare_prices` already used. This unifies the
  price field name across every tool that returns a price (RC3-032 / #30). Callers that
  parse `price.amount` programmatically (e.g. a CUR-reconciliation script rather than an
  LLM re-reading the tool schema each call) need to switch to `price_per_unit.amount`.

### Added
- **get_coverage** — new tool reporting structural catalog coverage (`catalog`/`absent`
  per domain/service) for a provider, answering "what does this server know about" without
  a live per-region fetch. (RC3-038 / #38)
- **compare_bom_regions** — new tool comparing a Bill of Materials' total monthly cost
  across multiple AWS regions, returning `regions[]` sorted cheapest-first with resolved
  line items and per-region errors. v1 scope: AWS-only, PricingSpec-dict items; non-AWS
  items are reported under `not_supported`. (RC3-004 / #31)

## [1.1.0] — 2026-07-04

### Added
- **warm_cache** — new tool that pre-populates the pricing cache for a provider across a
  set of regions (and, optionally, services) before a large sweep (e.g. a multi-region
  `compare_prices` or `get_prices_batch` call), so the sweep hits a warm cache instead of
  paying fetch latency on every combination. `cache_stats` now also reports a per-provider
  entry-count breakdown. (#63)
- **get_price_by_sku** — new AWS-only tool that resolves a raw Cost & Usage Report (CUR)
  usage-type/SKU string (e.g. `CAN1-BoxUsage:r5a.8xlarge`) to a price across one or more
  regions, without requiring the caller to know the resource_type/domain spec. Strips the
  region-prefix token by shape (no hardcoded region table), infers the AWS servicecode from
  the usage-type pattern when not supplied, and falls back to the inferred servicecode with a
  `service_mismatch` flag when an explicit `service` hint finds no match. Flags compound
  inter-region/Wavelength `AWSDataTransfer` SKUs with a warning rather than guessing silently.
- **get_prices_by_sku** — batch form of `get_price_by_sku` for resolving many raw AWS
  usage-type/SKU strings (e.g. every distinct line item in a CUR export) against the same set
  of target regions in one call, up to 25 SKUs per batch. Fans out across SKUs with bounded
  concurrency, reusing the same per-SKU resolution logic and process-lifetime catalog cache as
  `get_price_by_sku` so repeated `(service, region)` fetches across SKUs in a batch are paid
  once. Successful SKUs land in `results` in input order (not price-sorted, since distinct SKUs
  commonly price in incomparable units); SKUs that fail outright are reported in a top-level
  `errors` map keyed by SKU, mirroring `get_prices_batch`'s per-item error shape.

### Fixed

- **get_price_by_sku: ambiguous multi-product matches no longer silently default to the
  cheapest alternate** — a usage-type suffix that maps to more than one distinct billable
  product (e.g. ELB's `LCUUsage` spans Application/Network/Gateway load balancer pricing; RDS's
  `InstanceUsage:<type>` spans every database engine on that instance type) was previously
  resolved by picking whichever candidate row happened to be cheapest, with no indication to
  the caller that the price was a guess. Added optional `operation`/`product_family`
  disambiguating hint fields (the same CUR columns that accompany a usage-type/SKU value) so
  callers can resolve these up front. Regions that remain ambiguous after hint-based and
  canonical-default narrowing are now excluded entirely from `all_regions_sorted`,
  `cheapest_price`, `most_expensive_price`, and baseline-delta calculations, and are instead
  surfaced under a new `ambiguous_in` field with every candidate row listed so the caller must
  explicitly disambiguate rather than trust a silently-guessed number.
- **get_price_by_sku: fixed a 100% timeout on EC2 `BoxUsage` and EBS `VolumeUsage` lookups** —
  term assembly was re-deriving each SKU's on-demand/reserved terms via an O(M) linear
  `strings.HasPrefix` scan over the full terms map per SKU, which against AmazonEC2's ~120K
  products/terms bulk offer file made the overall lookup O(P×M) and reliably exceeded the
  request timeout. `collectTermsForSKUs` now preserves the SKU-level grouping it already
  produces while streaming the offer file, so term assembly is a direct O(1) `map[sku]` lookup
  instead.
- **Tool error responses now echo the requested region(s)** — `warm_cache` and
  `find_available_regions` error paths (unconfigured provider, upstream catalog failure)
  were dropping the caller's `region`/`regions` input from the error payload, making it hard
  to correlate a failure with its request in a batch context. All error branches now include it.
- **estimate_bom: fixed a cross-service SKU collision** — Azure SKU lookups could resolve a
  fallback price from an unrelated service when two services shared an ambiguous SKU token;
  resolution is now scoped per-service so a line item can no longer silently pick up another
  service's price.
- **describe_catalog: `ai/bedrock` and `ai/sagemaker` `FilterHints` now include a `note`**
  explaining that `get_price` currently returns `not_supported` for these services (live
  AWS Cost Explorer pricing API required, not yet implemented), instead of leaving callers
  to discover this only after a failed call.
- **AWS EKS control-plane pricing** — fixed a dispatcher gap that prevented EKS pricing from
  resolving. (#71)
- **GCP `a3-ultragpu-8g`** — added the missing catalog entry for this instance type. (#71)
- **Harness: `reason` field** — fixed a bug that could surface an incorrect explanation on
  certain failures. (#71)
- **search_pricing** — reworded the deprecated stub's message to a clearer deprecation
  framing. (#64)
- **get_price** — a non-nil but empty `PricingResult` is now treated as `no_results` instead
  of surfacing an ambiguous partial success. (#67)
- **find_available_regions** — tool description now leads with a spec-wrap example to reduce
  malformed calls. (#68)
- **RDS** — `aurora_postgresql` accepted as a `get_price` service alias for RDS. (#69)
- **list_instance_types (GCP)** — stopped excluding GPU families when the `gpu` flag is
  omitted. (#66)
- **describe_catalog (AWS)** — filled catalog gaps for `inter_region_egress`, `database`,
  and network LB/NAT/WAF services. (#70)
- **get_price / get_prices_batch** — now emit a `not_available_in` hint when a spec isn't
  available in the requested region/provider. (#61)
- **estimate_bom / compare_bom** — fractional quantities are now allowed in line items. (#59)
- **estimate_bom** — `Attributes[fallback]` is now surfaced on line items so callers can see
  when a fallback price was used. (#60)
- **compare_prices** — results are now grouped by SKU instead of interleaved across regions.
  (#56)
- **AWS SKU hints** — a stored-empty `operation` attribute is no longer treated as
  exclusionary when matching SKU hints. (#58)
- **AWS EBS** — gp3/gp2 pricing now tries a live lookup before falling back to static rates,
  and surfaces a fallback warning when it does. (#55)
- **AWS ALB** — `GetALBPrice` now fetches real region-varying rates instead of a flat
  estimate. (#57)
- **AWS DynamoDB** — stopped mis-inferring `AmazonDynamoDB` from the S3-shared
  `TimedStorage-ByteHrs` usage-type suffix. (#54)
- **GCP/Azure providers** — wired retry/backoff into HTTP fetches for transient failures.
  (#52)
- **GCP instance catalog** — added C4/Z3 families; unsupported families now classified as
  `not_supported` rather than erroring. (#53)
- **compare_prices** — transient errors are now classified distinctly and the baseline
  degrades gracefully instead of failing the whole call. (#51)

### Docs

- **README: published canonical server instructions text** — added a "Server instructions"
  subsection to the Security section with the verbatim `Instructions` string the server sends
  to MCP clients (from `server.go`), a written commitment that these instructions stay minimal
  and pricing-only (any future behavioral "you must..." addition is review-blocking), and a
  note that other locally-installed harness plugins may render their own hook output adjacent
  to this block, which is not this server's output. Closes a documentation gap identified after
  a false-attribution concern (a third-party plugin hook mistaken for this server's own
  instructions) was investigated and resolved.

---

## [1.0.0] — 2026-06-27

### Summary

v1.0.0 marks the production release of **opencloudcosts-go** — a complete Go rewrite of
the MCP server reaching full parity with the Python implementation and then surpassing it
in concurrency, reliability, and deployment simplicity. The gate: ≥99% pass rate with zero
XML hallucinations on the 234-prompt LLM grounding harness. Final: **234/234 (100%)**.

### Added

**Go MCP server (opencloudcosts-go)**
- **16 MCP tools** across four categories: lookup (get_price, get_prices_batch,
  compare_prices, describe_catalog, search_pricing), FinOps (estimate_bom,
  estimate_unit_economics, compare_bom, get_discount_summary), cache (refresh_cache,
  cache_stats), and discovery (list_regions, list_instance_types, find_cheapest_region,
  find_available_regions).
- **compare_bom** — new tool that prices a cloud-agnostic workload spec across AWS, GCP,
  and Azure simultaneously with per-provider, per-term cost breakdown and savings analysis.
  Workload types: compute, storage, database, cache. Fan-out: 8 concurrent provider calls.
- **Concurrent region fan-out** — `find_cheapest_region` and `find_available_regions` run
  up to 32 goroutines in parallel (errgroup + semaphore). `compare_prices` fans across
  regions with a semaphore of 10. `get_prices_batch` parallelises across instance types.
- **Static single binary** — `CGO_ENABLED=0`, ~15 MB distroless Docker image (scratch
  base), no runtime dependencies.
- **Dual transport** — stdio (local Claude Code / Claude Desktop) and streamable HTTP
  (remote/container/Kubernetes) in the same binary.
- **Production HTTP features** — token-bucket rate limiting (`golang.org/x/time/rate`),
  bearer-token auth (`OCC_API_KEY`), liveness (`/healthz`) and readiness (`/readyz`)
  probes, graceful SIGTERM drain (`OCC_SHUTDOWN_TIMEOUT`).
- **AWS public pricing HTTP fallback** — fetches bulk pricing files over HTTPS when
  no AWS credentials are present; no IAM configuration required for list prices.
- **GCP multi-source auth** — service account JSON (inline or base64), Workload Identity
  Federation external account, short-lived access token, ADC file path, API key.

**Harness (234 prompts, up from 199)**
- 35 new harness scenarios: `AIMOD1–5` (AI model pricing), `BOM_RES1–2` (BOM resilience),
  `GCPDB3`, `GCPEGR3`, `AV6`, `AV11`, `AZFN3`, `AZFN6`, `AZAI4`, `AZAI6`, `GC3`, `GC4`,
  `GK4`, `GK5`, `GSQL5`, `PDISK`, `NET_STACK1`, `CL1`, `AURO1`, `FCR1–2`, `EGRESS_HV`,
  `GPU2`, `GCX2`, and others.
- **Retry on Type-A transient errors** — harness retries once on clean connection failures
  (empty body, no `response` attribute) with 3s backoff.
- **resp=None reset per round** — fixes resp-leak bug where a prior round's response
  leaked into the error handler and bypassed retry for Type-A transients.
- **XML hallucination fixes** — `function_name` JSON key parsed correctly in
  `_extract_xml_tool_calls`; `<parameter=spec>` malformed XML format stripped.
- **Loop-break hardening** — forced `tool_choice=none` turn injects prose conclusion
  instead of emitting XML tool calls.
- MCP tool timeout raised 45s → 90s for long-running fan-out tools.
- `_repair_json` truncates over-closed JSON; request timeout raised 120s → 240s.

### Fixed

- **Azure o1-mini SKU contamination** — `prodPattern` boundary `(?:[^\w.]|$)` allows a
  trailing space, causing "o1" to match "o1 mini" SKUs. New `excludeMini` regex explicitly
  excludes any SKU whose model segment continues with ` mini` or `-mini`.
- **Azure word-boundary regex** — prevents `gpt-4` from matching `gpt-4o`, `gpt-4.1`, etc.
  Applied to product name, meter name, and compact SKU fields.
- **GCP cloud_armor domain routing** — remapped from `network` to correct dispatch path.
- Various linter issues resolved (golangci-lint v2, data race in schema snapshot test).

### Changed

- CI deployment pipeline switched from Python MCP server to Go binary for all environments.
- `estimate_bom` line-item responses compacted to reduce LLM context pressure.

### Notes

- Python implementation (`src/opencloudcosts/`) remains in the repo and continues to work;
  it is no longer the primary deployment target.
- `search_pricing` is a deprecated redirect stub (was `get_search_pricing` in v0.8.x).
- `get_spot_history` is not implemented in the Go server; the tool is registered and returns
  a structured "not available" response.
- Harness validated at 234/234 (100%) on `qwen3.6-35b-128k` via llama-swap.

---

## [0.9.2] — 2026-06-22

### Added
- **Harness: HTTP MCP transport** — `run_tests.py` now supports `OCC_MCP_TRANSPORT=http` /
  `OCC_MCP_BASE_URL` to drive a `--transport http` server, enabling multi-client and
  remote deployment testing without stdio process management.

### Fixed
- **Azure OpenAI model matching** — word-boundary regex (`(?![a-z.])` lookahead) prevents
  prefix collisions where `gpt-4` matched `gpt-4o` and `gpt-4.1` matched `gpt-4` model
  IDs in both compact (`gpt4o`) and normalised (`gpt-4o`) forms. Also applied to product
  and meter name fields.
- **Azure OpenAI standard-SKU filter** — deployment-type classifier now excludes audio,
  realtime, batch, and cached variants, reducing lookup noise from ~14 to ~4 SKUs.
- **Azure Functions Consumption plan** — fixed incorrect GB-second rate parsing that was
  returning the provisioned-plan rate instead of the consumption-plan rate.
- **`list_instance_types` default cap** — capped at 25 results by default to prevent
  context overflow on large regions; `max_results` parameter overrides.
- **Harness: reasoning content stripping** — prior assistant turns now have
  `reasoning_content` blocks removed before re-sending to the model, preventing context
  accumulation with thinking-mode models.
- **Harness: MCP restart resilience** — `session.initialize()` now has a 30s timeout;
  restart retry extended to 6 attempts (~7.5 min window) to survive pod redeployment.
- **Harness: tool-call robustness** — XML `arg=obj` syntax fixed; name-less tool calls
  inferred from context; large tool results truncated before injecting into conversation;
  dedup cache prevents duplicate calls within a turn.
- **Harness: AZAI2 prompt** — replaced retired GPT-3.5-Turbo reference with GPT-4o-mini.

### Performance
- **AWS bulk pricing** — `ijson` streaming parser replaces full in-memory JSON decode for
  bulk price files; singleflight lock prevents duplicate concurrent downloads.

### Changed
- **Harness: example configs** — placeholder URLs replace LAN IP examples.
- **CI** — `ruff format --check` extended to `local-test-harness/` and `tests/`; 15 lint
  issues resolved across harness scripts and test files.
- **Project tagline** — "Anchor AI FinOps to real, live cloud pricing" applied consistently
  to README and `pyproject.toml`.
- **Harness suite** — expanded from 169 to 199 prompts; new categories: `NET_EGR1-8`
  (egress tiering), `TRUST1-2` (trust metadata), `EGR_X1-3` (cross-cloud egress),
  `AZAKS4`, `AZFN6`, `AZAI6`, `FCR1-2`, `BOM_RES1-2`, `REC1`.

---

## [0.9.1] — 2026-04-26

### Added
- **GCP egress contract pricing** — `_effective_price_egress` follows the same
  `_find_sku_name` + `_fetch_contract_price` + `_make_effective_price` pattern as
  storage/database/network. Internet egress SKUs from `_COMPUTE_SERVICE_ID` are matched
  by continent (`americas` / `emea` / `apac`), skipping China/Australia destination rows.
  Returns `[]` for intra-GCP inter-region specs (those rates are rarely EDP-discounted).
- Wired into `get_price()` alongside compute, storage, database, and network paths.
- 4 new unit tests: no-billing-account guard, inter-region skipped, contract discount,
  end-to-end via `get_price()`.

### Fixed
- `PricingResult.source` Literal now includes `"catalog+billing_api"` — the value set
  by `get_price()` when effective pricing is active was missing from the model, causing
  a Pydantic validation error on any end-to-end effective pricing call through `get_price()`.

---

## [0.9.0] — 2026-04-26

### Summary

Minor release consolidating the v0.8.11–v0.8.14 GCP and Azure additions plus
harness robustness improvements. All three clouds now have full effective/contract
pricing support and cross-cloud egress comparison.

### Added
- **GCP storage contract pricing** (v0.8.11) — `effective_price` on `StoragePricingSpec`
  for GCS and Persistent Disk when `OCC_GCP_BILLING_ACCOUNT_ID` is configured
- **GCP database contract pricing** (v0.8.11) — `effective_price` on `DatabasePricingSpec`
  for Cloud SQL (all engines/sizes/HA) and Memorystore
- **Azure egress pricing** (v0.8.12) — `inter_region_egress` domain: internet and
  inter-region outbound transfer, zone-keyed rates from Retail Prices API, 5 GB/month
  free tier, monthly estimate in response
- **GCP network contract pricing** (v0.8.13) — `effective_price` on `NetworkPricingSpec`
  for Cloud LB, Cloud CDN, Cloud NAT, and Cloud Armor
- **GCP inter-region egress** (v0.8.14) — `inter_region_egress` domain: continent-based
  internet egress rates from Compute Engine SKU catalog with static fallbacks; intra-GCP
  inter-region at $0.01/GB (same continent) or $0.08/GB (cross-continent)
- **Cross-cloud egress comparison** — `compare_prices` now works across AWS, GCP, and
  Azure for the `inter_region_egress` domain
- **Harness loop detection** — replaces the arbitrary 30-round cap with sliding-window
  fingerprint detection (window=6); on loop: injects a nudge message and forces
  `tool_choice=none` to obtain a prose conclusion rather than a hard stop

### Changed
- `MAX_TOOL_ROUNDS` raised from 30 to 150 (true last-resort safety cap; loop detection
  fires well before this in practice)

### Notes
- Harness validated: 169/169 scenarios pass on `qwen/qwen3.6-35b-a3b`; 166/166 on
  first-pass completion (3 loop hits resolved by detection, 6 API errors transient)

---

## [0.8.14] — 2026-04-25

### Added
- **GCP inter-region egress** — `get_price` now handles `EgressPricingSpec` for GCP.
  - Internet egress (no `dest_region`): source-continent base rate from Compute Engine
    SKU catalog with fallback to documented list prices (Americas $0.08/GB, EMEA $0.085/GB,
    APAC $0.12/GB).
  - Intra-GCP inter-region (both regions set): $0.01/GB (same continent) or $0.08/GB
    (cross-continent) based on source/destination region prefix mapping.
  - `_gcp_egress_continent` static method maps any GCP region to `americas` / `emea` / `apac`.
  - `_fetch_internet_egress_rate` live-fetches the SKU-catalog rate and caches it; skips
    China/Australia destination rows to return the general worldwide base rate.
- **Cross-cloud egress comparison** — `compare_prices` now works across AWS, GCP, and Azure
  for the `inter_region_egress` domain (all three providers support it).
- 5 new unit tests: Americas internet egress, same-continent intra, cross-continent,
  APAC fallback, `get_price` dispatch.
- 3 new harness scenarios: `GCPEGR1–3` (GCP internet egress, GCP cross-region, cross-cloud
  egress comparison).

### Notes
- GCP internet egress China/Australia destinations are higher-priced separate SKUs; this
  implementation returns the worldwide base rate. China/Australia support is deferred.
- Harness: 169/169 (3 new scenarios).

---

## [0.8.13] — 2026-04-25

### Added
- **GCP network contract pricing** — `get_price` for `NetworkPricingSpec` now enriches
  responses with `effective_price` when `OCC_GCP_BILLING_ACCOUNT_ID` is set.
  Covers all four GCP network services:
  - **Cloud LB** (HTTPS, TCP, SSL, Network, Internal): contract rate on the forwarding-rule component
  - **Cloud CDN**: contract rates on both cache-egress and cache-fill components (up to 2 `EffectivePrice` entries)
  - **Cloud NAT**: contract rates on gateway-uptime and data-processing components
  - **Cloud Armor**: contract rates on security-policy and request-evaluation components
- New method: `_effective_price_network(spec: NetworkPricingSpec)` following the same
  `_find_sku_name` + `_fetch_contract_price` + `_make_effective_price` pattern as v0.8.11.
- 4 new unit tests: no-billing-account guard, LB rule discount, CDN two-component discount,
  Cloud Armor policy discount.
- 3 new harness scenarios: `GCPNET1–3` (LB, CDN, NAT public pricing paths).

### Notes
- Cloud LB contract pricing covers the rule component only; the data-processed rate uses
  the public list price (it is volume-tiered and less likely to be contracted separately).
- CDN and NAT return up to 2 `EffectivePrice` entries (one per pricing component).
- Harness: 166/166 (3 new scenarios, all public-path — effective pricing is opt-in).

---

## [0.8.12] — 2026-04-25

### Added
- **Azure egress pricing** (`inter_region_egress` domain) via the Azure Retail Prices API
  (`serviceName eq 'Bandwidth'`). Covers internet egress and inter-region transfers.
  First 5 GB/month free (Zone 1: Americas + Europe); rate fetched live from the API with
  a $0.087/GB static fallback.
- `get_egress_price(source_region, dest_region, data_gb)` on `AzureProvider` — returns
  a `NormalizedPrice` with `monthly_estimate` in attributes when `data_gb` is provided.
- `_price_egress` dispatch method; `INTER_REGION_EGRESS` added to `_AZURE_CAPABILITIES`.
- `describe_catalog()` updated with `filter_hints`, `example_invocations`, and
  `decision_matrix` entries for the new domain.
- 6 new unit tests: API rate fetch, inter-region dest attribute, fallback rate, free-tier
  threshold, supports() capability check, get_price() dispatch.
- 3 new harness scenarios: `AZEGR1–3` (internet egress rate, inter-region cost, AWS/GCP/Azure
  egress comparison).

### Notes
- Azure bandwidth is billed per zone, not per region pair. Zone 1 covers Americas and Europe.
- The egress rate is cached for `OCC_CACHE_TTL_HOURS` (default 24h).
- Harness: 163/163 (3 new scenarios added).

---

## [0.8.11] — 2026-04-25

### Added
- **GCP storage contract pricing** — `get_price` for `StoragePricingSpec` now enriches
  responses with an `effective_price` block when `OCC_GCP_BILLING_ACCOUNT_ID` is set.
  Covers GCS storage classes (Standard, Nearline, Coldline, Archive) and Persistent Disk
  types (pd-ssd, pd-standard, pd-balanced, pd-extreme).
- **GCP database contract pricing** — `get_price` for `DatabasePricingSpec` also supports
  effective pricing. Covers Cloud SQL (MySQL, PostgreSQL, SQL Server — all instance sizes,
  zonal and regional HA) and Memorystore for Redis (Basic/Standard, M2–M5 tiers).
- New internal methods: `_effective_price_storage`, `_effective_price_database`,
  `_make_effective_price` — consistent with the compute path introduced in v0.8.9.
- 6 new harness scenarios (`GCPSTO1–3`, `GCPDB1–3`) covering GCS, PD, Cloud SQL, and
  Memorystore public pricing paths (effective pricing is opt-in, harness uses public).
- 6 new unit tests covering the new methods: no-billing-account guard, GCS contract
  discount, no-discount (list == contract), Cloud SQL contract, Memorystore contract,
  and database no-billing-account guard.

### Notes
- `get_effective_price("storage", ...)` / `get_effective_price("database", ...)` routes
  through `_effective_price_storage` / `_effective_price_database` respectively.
- Contract pricing is best-effort: SKU lookup miss or API 401/403 falls back to public
  prices gracefully (no raise, no error surfaced to the LLM).
- Harness: 157/157 (6 new scenarios, all public-path — effective pricing opt-in).

---

## [0.8.10] — 2026-04-25

### Added
- **`GcpAuthProvider`** — multi-source OAuth resolver for GCP contract pricing.
  Replaces the hardcoded `gcloud`-ADC-only path with a proper priority chain:
  `OCC_GCP_SERVICE_ACCOUNT_JSON_B64` → `OCC_GCP_SERVICE_ACCOUNT_JSON` →
  `OCC_GCP_EXTERNAL_ACCOUNT_JSON_B64` / `OCC_GCP_EXTERNAL_ACCOUNT_JSON` (Workload
  Identity Federation) → `GOOGLE_APPLICATION_CREDENTIALS` / GCP metadata server /
  local ADC → `OCC_GCP_ACCESS_TOKEN` (debug escape hatch, warns loudly).
- `google-auth[requests]>=2.38` added as optional `[gcp]` extra —
  `pip install opencloudcosts[gcp]`. Public pricing users are unaffected.

### Fixed
- `creds.refresh()` now runs via `asyncio.to_thread()` — was blocking the event loop.
- `asyncio.Lock` guards concurrent credential refresh to prevent race condition.
- `_get_http` (catalog client) rebuilds per-call for Bearer auth — token no longer
  goes stale in long-running containers. API-key path unchanged.
- 401/403 responses from the billing API are no longer cached — auth and IAM errors
  must not block credential rotation.
- ADC `NotConfiguredError` no longer includes filesystem paths from `google.auth`
  exceptions, preventing path disclosure in MCP tool responses.
- Billing account ID removed from catch-all HTTP error log.
- `priceReason.type` capped at 64 chars before caching/returning.
- `_decode_json_b64`: narrow `except` clause; 64 KiB size guard.
- Three unused variable assignments removed from `azure.py` (ruff F841).

---

## [0.8.9] — 2026-04-24

### Added
- **GCP effective / contract pricing** via Cloud Billing Pricing API v1beta.
  When `OCC_GCP_BILLING_ACCOUNT_ID` is set and ADC credentials are available,
  `get_price` for GCP compute now returns an `effective_price` block containing
  the negotiated contract rate, discount percentage, and `priceReason` type
  (e.g. `floating-discount`, `fixed-price`). Requires
  `billing.billingAccountPrice.get` IAM permission on the billing account.
- `OCC_GCP_BILLING_ACCOUNT_ID` config variable — the bare billing account ID
  (e.g. `012345-567890-ABCDEF`). Absent → effective pricing skipped, public
  prices returned unchanged (no regression for unauthenticated users).
- New internal helpers: `_get_billing_http`, `_find_sku_name`,
  `_fetch_contract_price` — contract prices cached at `effective_price_ttl_hours`
  (default 1 h); 403 responses are logged and fall back to public prices gracefully.

### Fixed
- `get_effective_price` for GCP no longer raises `NotConfiguredError` when called
  without a billing account — it returns `[]` silently.

### Notes
- API key auth is rejected for v1beta billing-account endpoints (GCP limitation);
  ADC Bearer token (via `gcloud auth application-default login`) is required.
- Storage and database contract pricing deferred to v0.8.10+.
- Harness: 151/151 (no change — effective pricing is opt-in).

---

## [0.8.8] — 2026-04-24

### Added
- **Azure SQL Database** (`database/sql`) — vCore model pricing for General Purpose,
  Business Critical, and Hyperscale tiers; supports MySQL and PostgreSQL engines via
  "Azure Database for MySQL/PostgreSQL" serviceName; single-az and HA (ZRS) deployment;
  on-demand, 1-year, and 3-year reserved terms.
- **Azure Cosmos DB** (`database/cosmos`) — provisioned throughput (per 100 RU/s),
  serverless (per 1M RUs), and autoscale modes; multi-region write flag.
- **AKS** (`container/aks`) — cluster management fee: Standard tier ($0.10/hr, Uptime SLA)
  or Free tier ($0); notes worker nodes are priced separately via compute.
- **Azure Functions** (`serverless/azure_functions`) — Consumption plan GB-second and
  execution-count pricing with free-tier deduction (400K GB-s, 1M executions/month) and
  optional monthly cost estimate when `gb_seconds`/`requests_millions` are provided.
- **Azure OpenAI** (`ai/openai`) — per-1K-token input/output pricing for gpt-4o,
  gpt-4o-mini, gpt-4, gpt-35-turbo, o1, o1-mini, and embedding models; static fallback
  table for all major models; optional cost estimate when token volumes provided.
- `spec_infer._SERVICE_TO_DOMAIN` extended: `sql`, `cosmos`, `aks`, `openai`,
  `azure_functions`, `cloud_functions`, `cloud_run`, `sagemaker`, `elasticache`.

### Changed
- Azure `_AZURE_CAPABILITIES` expanded from 5 entries (compute + storage) to 13.
- `get_price()` router extended with `_price_database`, `_price_container`,
  `_price_serverless`, `_price_ai` dispatch methods.
- `describe_catalog()` updated with `filter_hints` and `example_invocations` for all
  5 new services; `decision_matrix` covers all supported Azure services.
- Not-supported error message updated to list all 7 supported Azure services.

### Fixed
- Harness: 123/123 on gemma-4-26b-a4b (no regression from breadth expansion).

---

## [0.8.5] — 2026-04-24

### Added
- `src/opencloudcosts/utils/spec_infer.py` — `fill_domain(spec)` infers the required
  `domain` field from `service`, `storage_type`, and `resource_type` before
  discriminated-union validation, eliminating the most common `invalid_spec` failure class.
  Covers: service-keyed lookup (rds→database, bigquery→analytics, gke→container, etc.),
  `storage_type` present → storage, `db.`/`cache.` resource_type prefix → database,
  dotted/dashed/Standard_/Basic_/Premium_ instance names → compute.
- `spec_error_response(err, spec)` in `spec_infer.py` — structured `invalid_spec` error
  dict with targeted hints: lists valid `domain` values when discriminator tag is missing,
  lists valid `PricingTerm` strings when a bad term is supplied.
- `get_price` docstring extended with `INTER_REGION_EGRESS` example invocation.
- Azure `describe_catalog` decision_matrix now includes an explicit "NOT SUPPORTED" note
  for database domain, stopping models from looping on futile database queries.

### Changed
- `fill_domain` applied in all tool entry points (`get_price`, `compare_prices`,
  `get_spot_history`, `find_cheapest_region`, `find_available_regions`, `estimate_bom`,
  `estimate_unit_economics`) before `validate_python`.
- AWS spot pricing: `NoCredentialsError` / `PartialCredentialsError` now returns an empty
  `public_prices` list instead of raising, so the tool layer reports "no pricing found"
  rather than an `upstream_failure`.
- GCP Cloud Monitoring breakdown rates (`tier1_rate_per_mib`, `tier2_rate_per_mib`,
  `tier3_rate_per_mib`, `estimated_monthly_cost`) now formatted as `"$X.XXXX/MiB"` strings
  so the grounding regex in `analyse.py` can extract them from tool output.

### Fixed
- Harness: 123/123 (100%) on gemma-4-26b-a4b, up from 104/123 (84.6%) in v0.8.4.
  Eliminated: invalid_spec from domain omission on BOM/service specs, false hallucination
  flags on Cloud Monitoring tier rates, Azure database 12-round futile search, AWS spot
  upstream_failure.

---

## [0.8.4] — 2026-04-23

### Added
- `src/opencloudcosts/utils/money.py` — `_price(amount, unit)` and `_money(amount, label)`
  helpers that return structured dicts `{amount, unit, currency, display}` instead of
  formatted strings. Consumers can now do arithmetic on `price.amount` without parsing.

### Changed
- All string-money `f"${p.price_per_unit:.6f}/{p.unit.value}"` formatting eliminated from
  the tool layer (`lookup.py`, `availability.py`, `bom.py`) and `NormalizedPrice.summary()`.
  Every price field in tool responses is now a structured dict with a `display` key for
  human readability (e.g. `"$0.192000/per_hour"`) alongside the numeric `amount`.
- `apply_baseline_deltas` (`utils/baseline.py`) now accepts both string `"$X.XX/unit"`
  and new dict `{amount: X.XX}` values for `price_per_unit` / `monthly_estimate`.
- Harness `analyse.py` requires no changes: the embedded `display` field keeps existing
  `$` regex grounding checks working on serialised tool output.

---

## [0.8.3] — 2026-04-23

### Added
- Trust metadata on `NormalizedPrice`: `fetched_at`, `source_url`, `cache_age_seconds`
  fields (all optional). Populated from cache hits via `_apply_cache_trust()` on
  `ProviderBase`; emitted in `NormalizedPrice.summary()` when set.
- `CacheManager.get_prices_with_meta()` — returns `(data, fetched_at_datetime)` so
  providers can compute `cache_age_seconds` at read time without breaking existing callers.
- `INTER_REGION_EGRESS` pricing domain and `EgressPricingSpec` discriminated union variant
  (`source_region`, `dest_region`, `data_gb`).
- AWS inter-region egress pricing via `AWSDataTransfer` bulk pricing SKUs, filterable
  by `fromRegionCode` / `toRegionCode`. Exposed through `get_price` with the new domain.
- Egress entries added to `AWSProvider.describe_catalog()`.

---

## [0.8.2] — 2026-04-23

### Added
- Three new `ProviderBase` protocol methods with sensible defaults:
  - `major_regions() -> list[str]` — provider's curated region shortlist for fan-out tools
  - `default_region() -> str` — provider's primary region when none is specified
  - `bom_advisories(services, sample_region) -> list[dict]` — provider-specific BOM
    advisory rows (data transfer, support costs, etc.)
- All three providers implement the new methods; AWS `bom_advisories` carries the
  60-line advisory block previously hard-coded in `bom.py`.

### Changed
- All `if provider == "aws"` / provider-string conditionals removed from the tool layer.
  `find_cheapest_region` and `find_available_regions` call `pvdr.major_regions()`;
  `get_price` uses `pvdr.default_region()`; `estimate_bom` calls `pvdr.bom_advisories()`.
- `get_spot_history` tool is now provider-agnostic: removed the hard AWS guard; catches
  `NotSupportedError` from `pvdr.get_spot_history()` and returns a structured envelope.
- `_AWS_MAJOR_REGIONS` / `_GCP_MAJOR_REGIONS` module-level constants removed from
  `availability.py`; `_DEFAULT_REGIONS` dict removed from `lookup.py`.

---

## [0.8.1] — 2026-04-23

### Added
- HTTP retry/backoff on all upstream pricing API calls (AWS, GCP, Azure) using tenacity:
  exponential back-off 1–30s, 3 attempts, retry on 429/5xx and transient network errors.
- Schema-version gate on startup: cache is only purged when the DB schema changes,
  not on every restart. Pricing data now survives Docker/Claude Desktop/`uv run` restarts.
- Test + lint CI workflow (`.github/workflows/tests.yml`): runs pytest, ruff, and mypy
  across Python 3.11/3.12/3.13 on every push and pull request.
- Docker image published to `ghcr.io/x7even/opencloudcosts` via GHCR workflow.
- PyPI/uvx quickstart in README: `uvx opencloudcosts` and `pip install opencloudcosts`.
- CONTRIBUTING.md, SECURITY.md, GitHub issue templates, PR template.
- CHANGELOG.md (this file).
- Multi-model LLM harness matrix runner (`local-test-harness/run_matrix.py`) with
  `--parallel-models N` flag; 123/123 (100%) pass rate across gemma-4-26b-a4b,
  qwen3.6-35b-a3b, and qwen3.5-122b-a10b@q6_k.

### Changed
- `list_instance_types` response compacted: `network_performance` dropped, GPU fields
  (`gpu_count`, `gpu_type`) only included when non-null. Reduces token use in large regions.

### Fixed
- Structured error envelopes at all tool boundaries: raw exception text (including potential
  boto3 stack frames) no longer leaks into LLM context. Full tracebacks are logged
  server-side.
- 5 provider contract invariant tests were `@pytest.mark.skip` — they now run (35 tests).
- Stale test fixtures for gp3 IOPS add-ons, Azure Premium SSD unit, BigQuery storage
  two-tier format, and Cloud SQL per-instance-size descriptions.
- Duplicate `[dependency-groups].dev` block in `pyproject.toml` with conflicting pytest
  versions removed. Unused `google-cloud-billing` extra dropped.

### Migration from v0.8.0
No tool API changes. Cache will be purged once on first startup (schema version write).

---

## [0.8.0] — 2026-04-21

Breaking change: 31 provider-specific tools consolidated to a 15-tool surface.

### Added
- `get_price(spec)` — unified dispatcher using a discriminated-union `PricingSpec`.
  Replaces 21 domain-specific tools.
- `describe_catalog(provider, domain, service)` — returns the full support matrix with
  `example_invocation` copy-paste snippets for `get_price`.
- Capability-based provider protocol: `supports()`, `get_price()`, `describe_catalog()`
  on each cloud provider — zero provider-branching in the tool layer.
- MAX_TOOL_ROUNDS=30 for agentic BOM/TCO prompts (previously 20).
- 123/123 (100%) harness pass rate on a 123-prompt test suite across AWS, GCP, Azure.

### Removed (migration required)
The following tools no longer exist. Use `get_price(spec={...})` instead:

| Old tool | Replacement |
|----------|-------------|
| `get_compute_price` | `get_price({"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge", ...})` |
| `compare_compute_prices` | `compare_prices(spec={...}, regions=[...])` |
| `get_service_price` | `get_price({"provider": "gcp", "domain": "database", "service": "cloud_sql", ...})` |
| `check_availability` | `find_available_regions(spec={...})` |
| 17 others | See `describe_catalog()` for equivalent invocations |

### Fixed
- gp3 EBS add-on pricing (IOPS/throughput tiers correctly returned alongside base rate).
- Azure Premium SSD disk unit corrected to `per_month` (flat tier fee, not per-GB).
- BigQuery active/long-term storage rates now use the paid tier, not the free-quota tier.
- Cloud SQL SKU matching updated to GCP's per-instance-size description format.
- Memorystore Standard SKU: excludes Redis Cluster node SKUs to return correct HA rate.
- Lambda pricing: dedicated `_price_lambda` for request + duration SKUs.
- Monthly estimate shown for `PER_MONTH` unit in tool responses.

---

## [0.7.x] — 2025–2026

Phase-based rollout:
- **Phase 1**: AWS public pricing (EC2, EBS, list instances)
- **Phase 2**: AWS effective pricing (Cost Explorer, Savings Plans, Reserved Instances)
- **Phase 3**: GCP public pricing (Compute Engine, Persistent Disk, CUDs)
- **Phase 4**: Azure public pricing (Retail Prices API); streamable-HTTP transport; Dockerfile
- **Phase 5**: GCP managed services (GKE, Memorystore, BigQuery, Vertex AI, Gemini,
  Cloud LB/CDN/NAT/Armor/Monitoring, Cloud SQL); Azure reserved pricing

[Unreleased]: https://github.com/x7even/cloudcostsmcp/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/x7even/cloudcostsmcp/compare/v0.9.2...v1.0.0
[0.9.2]: https://github.com/x7even/cloudcostsmcp/compare/v0.9.1...v0.9.2
[0.9.1]: https://github.com/x7even/cloudcostmcp/compare/v0.9.0...v0.9.1
[0.9.0]: https://github.com/x7even/cloudcostmcp/compare/v0.8.14...v0.9.0
[0.8.14]: https://github.com/x7even/cloudcostmcp/compare/v0.8.13...v0.8.14
[0.8.13]: https://github.com/x7even/cloudcostmcp/compare/v0.8.12...v0.8.13
[0.8.12]: https://github.com/x7even/cloudcostmcp/compare/v0.8.11...v0.8.12
[0.8.11]: https://github.com/x7even/cloudcostmcp/compare/v0.8.10...v0.8.11
[0.8.10]: https://github.com/x7even/cloudcostmcp/compare/v0.8.9...v0.8.10
[0.8.9]: https://github.com/x7even/cloudcostmcp/compare/v0.8.8...v0.8.9
[0.8.8]: https://github.com/x7even/cloudcostmcp/compare/v0.8.5...v0.8.8
[0.8.5]: https://github.com/x7even/cloudcostmcp/compare/v0.8.4...v0.8.5
[0.8.4]: https://github.com/x7even/cloudcostmcp/compare/v0.8.3...v0.8.4
[0.8.3]: https://github.com/x7even/cloudcostmcp/compare/v0.8.2...v0.8.3
[0.8.2]: https://github.com/x7even/cloudcostmcp/compare/v0.8.1...v0.8.2
[0.8.1]: https://github.com/x7even/cloudcostmcp/releases/tag/v0.8.1
[0.8.0]: https://github.com/x7even/cloudcostmcp/releases/tag/v0.8.0
