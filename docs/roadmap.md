# OpenCloudCosts — Roadmap & Implementation Tasks

Tracks all known gaps and planned features. Tasks are grouped by phase and parallelism.
Status: `pending` | `in_progress` | `done`

Each task has a detailed plan file in `docs/plans/`:

| Task | Plan file | Status |
|------|-----------|--------|
| T18 — AWS spot pricing | [T18-aws-spot-pricing.md](plans/T18-aws-spot-pricing.md) | **done** |
| T19 — RI upfront options | [T19-ri-upfront-options.md](plans/T19-ri-upfront-options.md) | **done** |
| T20 — os field in BoM | [T20-os-field-bom.md](plans/T20-os-field-bom.md) | **done** |
| T21 — refresh_cache metadata | [T21-refresh-cache-metadata.md](plans/T21-refresh-cache-metadata.md) | **done** |
| T22 — GCP major regions | [T22-gcp-major-regions.md](plans/T22-gcp-major-regions.md) | **done** |
| T23 — Phase 4 error messages | [T23-phase4-error-messages.md](plans/T23-phase4-error-messages.md) | **done** |
| T24 — Test coverage expansion | [T24-test-coverage.md](plans/T24-test-coverage.md) | **done** |
| T25 — No-results hints | [T25-no-results-hints.md](plans/T25-no-results-hints.md) | **done** |
| T26 — GCP effective pricing (BigQuery) | [T26-gcp-effective-pricing.md](plans/T26-gcp-effective-pricing.md) | pending |
| T27 — get_database_price tool | [T27-get-database-price-tool.md](plans/T27-get-database-price-tool.md) | **done** (AWS RDS + GCP Cloud SQL) |
| T28 — Azure provider | [T28-azure-provider.md](plans/T28-azure-provider.md) | **done** |
| T29 — HTTP/SSE transport | [T29-http-transport.md](plans/T29-http-transport.md) | **done** |
| T30 — Spot price history tool | [T30-spot-price-history.md](plans/T30-spot-price-history.md) | **done** |
| T31 — GCP Windows pricing | [T31-gcp-windows-pricing.md](plans/T31-gcp-windows-pricing.md) | **done** |
| T32 — GCP GKE + Memorystore pricing | — | **done** (v0.7.2) |
| T33 — GCP GCS + Cloud SQL pricing | — | **done** (v0.7.3) |
| T34 — GCP BigQuery pricing | — | **done** (v0.7.4) |
| T35 — GCP Vertex AI + Gemini pricing | — | **done** (v0.7.5) |
| T36 — GCP Cloud LB / CDN / NAT pricing | — | **done** (v0.7.6) |
| T37 — GCP Cloud Armor + Monitoring pricing | — | **done** (v0.7.7) |
| T38 — Azure reserved pricing unit fix | — | **done** (v0.7.8) |
| T39 — CDN SKU filter + harness GCS/SQL coverage | — | **done** (v0.7.9) |
| v0.8.0 — Tool consolidation + provider protocol | [roadmap_0_8_0.md](roadmap_0_8_0.md) | **done** — 31 tools → 15 tools, unified `get_price(spec)`, `describe_catalog`, discriminated-union `PricingSpec`; harness 123/123 (100%) |
| T41 — get_price_by_sku (AWS SKU/usage-type lookup) | [T41-sku-lookup.md](plans/T41-sku-lookup.md) | **done** |

---

## Quick wins — can build in parallel immediately

### T20 · Add `os` field to `estimate_bom` / `estimate_unit_economics`
**Status:** done  
**Effort:** Small (2 files)  
**Files:** `src/opencloudcosts/tools/bom.py`, `docs/tools.md`

`estimate_bom` hardcodes `"Linux"` for all compute items (`bom.py:69`, `bom.py:162`). Windows instances cost 30–40% more — not accounting for this skews TCO estimates.

**Implementation:**
- Read `os` from each item dict, default `"Linux"`. Pass to `get_compute_price()` in both tools.
- Update the tool docstring and `docs/tools.md` BoM line item fields table to document `os`.
- Add test: BoM with a Windows instance returns a higher price than the same Linux instance.

---

### T21 · Fix `refresh_cache` to also clear metadata entries for a provider
**Status:** done  
**Effort:** Small (1 file)  
**Files:** `src/opencloudcosts/cache.py`

`refresh_cache(provider="aws")` only deletes from the `prices` table. Metadata (service lists, instance type indexes, GCP SKU catalogs) persists stale after a cache clear.

**Investigation:** The metadata table has no provider column. Two options:
- A) Add a `provider` column to the metadata table (migration needed).
- B) Prefix all metadata keys with `provider:` (e.g. `aws:services_index`, `gcp:sku_list`) and use a `LIKE 'aws:%'` DELETE.

Option B is simpler and avoids schema migration. Ensure `list_services()`, `list_instance_types()`, and GCP SKU catalog results are all stored with provider-prefixed keys.

**Implementation:**
- Audit every `cache.set_metadata()` call — add `{provider}:` prefix where missing.
- Update `clear_provider()` to also `DELETE FROM metadata WHERE key LIKE '{provider}:%'`.
- Add test: store metadata for aws, call `refresh_cache("aws")`, verify metadata is cleared. GCP metadata should be unaffected.

---

### T22 · Add GCP major-regions default (mirror `_AWS_MAJOR_REGIONS`)
**Status:** done  
**Effort:** Small (1 file)  
**Files:** `src/opencloudcosts/tools/availability.py`

`find_cheapest_region` and `find_available_regions` default to 12 major regions for AWS but query ALL 30+ GCP regions. This causes cold-cache timeout risk and inconsistent UX.

**Implementation:**
```python
_GCP_MAJOR_REGIONS = [
    "us-central1",         # Iowa
    "us-east1",            # South Carolina
    "us-west1",            # Oregon
    "us-west2",            # Los Angeles
    "europe-west1",        # Belgium
    "europe-west2",        # London
    "europe-west3",        # Frankfurt
    "europe-west4",        # Netherlands
    "asia-east1",          # Taiwan
    "asia-northeast1",     # Tokyo
    "asia-southeast1",     # Singapore
    "australia-southeast1",# Sydney
]
```
Apply the same scoped-search logic: default to major regions for GCP, `regions=["all"]` for exhaustive. Add `note` field to response when scoped. Update `docs/tools.md`.

---

### T23 · Explicit error messages for Phase 4 features
**Status:** done  
**Effort:** Small (1 file)  
**Files:** `src/opencloudcosts/tools/lookup.py`

`get_effective_price(provider="gcp")` and `get_discount_summary(provider="gcp")` currently fail at runtime with opaque errors. They should fail fast at the tool layer.

**Implementation:**
- In `get_effective_price`: if `provider="gcp"` return:
  ```json
  {"error": "GCP effective pricing via BigQuery billing export is planned for Phase 4 and not yet available. Use get_compute_price with term='cud_1yr' or 'cud_3yr' for committed-use discount list prices."}
  ```
- Same for `get_discount_summary` — point to Phase 4 roadmap.
- Update `docs/tools.md` to note which tools are AWS-only currently.

---

### T25 · `search_pricing` and `get_service_price`: helpful no-results response
**Status:** done  
**Effort:** Small (2 files)  
**Files:** `src/opencloudcosts/tools/lookup.py`, `src/opencloudcosts/providers/aws.py`

When called with an invalid/unknown service code or filters that match nothing, both tools return an empty list with no explanation. LLMs retry blindly.

**Implementation:**
- Standardise no-results response shape: `{"result": "no_results", "message": "...", "tip": "Use list_services() to discover valid service codes. Use search_pricing(service=...) to explore attribute names before filtering."}`.
- In `search_pricing`: if results empty, return the above + the resolved service code so the LLM can verify the alias resolution.
- In `get_service_price`: if results empty, include the filters that were applied so the LLM can adjust.

---

## Medium complexity — parallel

### T18 · AWS spot pricing via EC2 SpotPrice API
**Status:** done  
**Effort:** Medium  
**Files:** `src/opencloudcosts/providers/aws.py`, `tests/test_providers/test_aws.py`

The AWS bulk pricing API doesn't include spot prices — they're time-varying and fetched via the EC2 `DescribeSpotPriceHistory` API.

**Design:**
- Add `_get_spot_price(instance_type, region, os) -> list[NormalizedPrice]` method.
- Use `boto3 ec2.describe_spot_price_history(InstanceTypes=[...], AvailabilityZone=..., MaxResults=5)`.
- Returns the most recent price per AZ; pick the cheapest AZ or expose all.
- Requires credentials — return a clear `{"error": "Spot pricing requires AWS credentials. Set AWS_PROFILE or AWS_ACCESS_KEY_ID."}` when unauthenticated.
- Map `PricingTerm.SPOT` to this path in `get_compute_price()`.
- `get_prices_batch`, `find_cheapest_region`, `find_available_regions` already pass `term` through — spot will work automatically once the provider handles it.

**Tests:** Mock `boto3.client('ec2').describe_spot_price_history`. Test: returns price, test: no credentials returns clear error.

---

### T19 · AWS Reserved Instance upfront options (Partial/All Upfront)
**Status:** done  
**Effort:** Medium  
**Files:** `src/opencloudcosts/models.py`, `src/opencloudcosts/providers/aws.py`, `docs/tools.md`

`_RESERVED_FILTERS` hardcodes `"PurchaseOption": "No Upfront"`. Partial and All Upfront are typically cheaper (lower hourly rate in exchange for upfront payment) and should be exposed.

**Design:**
- Add to `PricingTerm` enum: `reserved_1yr_partial`, `reserved_1yr_all`, `reserved_3yr_partial`, `reserved_3yr_all`.
- Extend `_RESERVED_FILTERS` dict to map each term to `(LeaseContractLength, PurchaseOption)` pairs.
- `get_compute_price()` already routes via `_RESERVED_FILTERS` — no structural change needed, just add entries.
- For All Upfront: the price is in a `Quantity` dimension (one-time), not `Hrs`. Handle the unit difference — either normalise to hourly equivalent or return as a separate `upfront_cost` field alongside `price_per_unit`.
- Update `docs/tools.md` `term` parameter table.

**Tests:** Mock `_get_products` returning a partial-upfront item. Assert price and term are correct.

---

### T24 · Expand test coverage
**Status:** done  
**Effort:** Medium  
**Files:** `tests/test_tools/` (new files)

Current coverage has no tests for: BoM database items, region analysis tools, cache tools, list_services, or the GCP compute delegation in get_service_price.

**Tests to add:**
1. `estimate_bom` with `service="database"` (AWS RDS db.t4g.micro) — returns a price
2. `estimate_bom` with `service="database"` for GCP — returns graceful error
3. `find_cheapest_region` — mocked concurrent region fetch, verifies sorted output + baseline deltas
4. `find_available_regions` — verifies `_sort_price` key is popped, baseline deltas present
5. `cache_stats` — returns `price_entries`, `db_size_mb`
6. `refresh_cache` — clears prices; next fetch goes back to provider
7. `list_services` — returns list with `service_code` and `aliases` fields
8. `search_pricing(service="cloudwatch")` — routes to generic path, not EC2 path
9. `get_service_price(provider="gcp", service="compute", filters={"instanceType": "n2-standard-4"})` — delegates to `get_compute_price`
10. `estimate_unit_economics` — end-to-end with a simple compute BoM

---

### T31 · GCP Windows pricing support
**Status:** done  
**Effort:** Medium (requires catalog research)  
**Files:** `src/opencloudcosts/providers/gcp.py`, `tests/test_providers/test_gcp.py`

GCP accepts `os="Windows"` but ignores it — all pricing returns Linux rates. GCP does charge a per-vCPU/hour Windows Server license fee on top of compute.

**Investigation required:**
- Call GCP Cloud Billing Catalog for an n2-standard-4 and look for SKUs with "Windows" in the description to understand the exact SKU naming.
- Likely SKU pattern: `"N2 Instance Core running Windows"` and `"N2 Instance Ram running Windows"` — separate per-vCPU and per-GB-RAM rates.

**Design:**
- In `get_compute_price()`, if `os="Windows"`, look up Windows-specific SKUs for the instance family.
- Add Windows license price to base compute price, or return as a separate `os_license_per_hour` field.
- If no Windows SKUs found for a family (e.g. e2 preemptible), return an error explaining it.
- Add test: `os="Windows"` returns higher price than `os="Linux"` for `n2-standard-4`.

---

## Phase 4 — larger features

### T27 · Dedicated `get_database_price` tool
**Status:** done  
**Effort:** Medium  
**Files:** `src/opencloudcosts/tools/lookup.py`, `docs/tools.md`

`estimate_bom` routes `service="database"` through `get_service_price("rds", ...)` but using it directly requires knowing RDS filter keys. A dedicated tool is much more LLM-friendly.

**Design:**
```python
get_database_price(
    provider: str,           # "aws" (gcp coming later)
    instance_type: str,      # "db.r5.large", "db.t4g.micro"
    region: str,             # "us-east-1"
    engine: str = "MySQL",   # MySQL, PostgreSQL, MariaDB, Oracle, SQLServer
    deployment: str = "single-az",  # "single-az", "multi-az"
    term: str = "on_demand"  # "on_demand", "reserved_1yr", "reserved_3yr"
)
```

- AWS: pre-build RDS filters from the above params, call `get_service_price("rds", ...)`.
- Return structured response matching `get_compute_price` format: `price_per_hour`, `monthly_estimate`, engine, deployment.
- Update `estimate_bom` to call `get_database_price` instead of the inline RDS lookup.
- Add `docs/tools.md` entry under Pricing Lookup.
- Add tests.

---

### T26 · GCP effective pricing via BigQuery billing export
**Status:** pending  
**Effort:** Large  
**Files:** `src/opencloudcosts/providers/gcp.py`, `src/opencloudcosts/config.py`, `README.md`, `docs/tools.md`

Implement GCP `get_effective_price()` and `get_discount_summary()` using BigQuery billing export.

**Prerequisites:** User must have billing export to BigQuery enabled in GCP Console.

**Design:**
- Add `OCC_GCP_BILLING_DATASET` env var: BigQuery dataset reference e.g. `myproject.billing_export.gcp_billing_export_resource_v1_...`.
- Query pattern for effective rate:
  ```sql
  SELECT service.description, sku.description,
         SUM(cost) / SUM(usage.amount) as effective_rate
  FROM `{dataset}`
  WHERE usage_start_time >= TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 30 DAY)
    AND service.description LIKE '%Compute%'
    AND sku.description LIKE '%{instance_family}%'
  GROUP BY 1, 2
  ```
- `get_discount_summary()` for GCP: aggregate CUD savings from billing data.
- Add `google-cloud-bigquery` to `pyproject.toml` as optional dependency.
- Update `README.md` GCP Setup with BigQuery export setup steps.
- Remove Phase 4 caveat from `docs/tools.md` once done.

---

## Phase 5 — new providers & transport

### T28 · Azure provider (compute + storage public pricing)
**Status:** done  
**Effort:** Large  
**Files:** `src/opencloudcosts/providers/azure.py` (new), `src/opencloudcosts/server.py`, `README.md`, `docs/tools.md`, `docs/finops-guide.md`

Azure Retail Prices API requires no credentials — fully public REST endpoint.

**API:** `https://prices.azure.com/api/retail/prices?api-version=2023-01-01-preview&$filter=...`

**Key filter fields:** `armSkuName`, `armRegionName`, `priceType` (`"Consumption"` = on-demand, `"Reservation"` = reserved), `serviceName`, `productName`.

**Instance type format:** `Standard_D4s_v3`, `Standard_E8s_v3`, `Standard_NC6s_v3` (GPU).  
**Region format:** `eastus`, `westeurope`, `southeastasia`.

**Methods to implement** (matching CloudProvider interface):
- `get_compute_price()` — filter by `armSkuName` + `armRegionName` + `priceType`
- `get_storage_price()` — filter `productName contains "Managed Disks"` or `"Blob Storage"`
- `list_regions()` — fetch distinct `armRegionName` values for a service
- `list_instance_types()` — filter by `priceType="Consumption"` + family prefix
- `check_availability()` — attempt price lookup, return bool
- `search_pricing()` — text search across `meterName` + `productName`

**Config:** No credentials needed for public pricing. Add `OCC_AZURE_SUBSCRIPTION_ID` for effective pricing later (Phase 5+).

**Docs:** Add Azure examples to `docs/finops-guide.md` (3-way AWS/GCP/Azure comparison).

---

### T29 · HTTP/SSE transport support
**Status:** done  
**Effort:** Medium  
**Files:** `src/opencloudcosts/server.py`, `src/opencloudcosts/config.py`, `Dockerfile` (new), `README.md`

Currently only stdio MCP transport is supported. HTTP+SSE enables remote/shared deployments (team server, Docker container, cloud-hosted).

**Design:**
- Add `--transport` CLI flag: `stdio` (default) | `http`.
- Add `OCC_HTTP_PORT` (default `8080`), `OCC_HTTP_HOST` (default `"127.0.0.1"`).
- FastMCP supports HTTP transport natively — should be a small change to `server.py`.
- Add optional `OCC_API_KEY` — if set, require `Authorization: Bearer <key>` on all requests.
- Add `Dockerfile`:
  ```dockerfile
  FROM python:3.12-slim
  RUN pip install uv
  COPY . /app
  WORKDIR /app
  RUN uv sync
  EXPOSE 8080
  CMD ["uv", "run", "opencloudcosts", "--transport", "http"]
  ```
- Update `README.md`: add "Running as HTTP server" and "Docker" sections.
- Verify stdio mode still works unchanged after refactor.

---

### T30 · AWS spot price history tool
**Status:** done  
**Effort:** Medium (depends on T18)  
**Files:** `src/opencloudcosts/tools/lookup.py`, `src/opencloudcosts/providers/aws.py`, `docs/tools.md`

Spot prices vary over time. A history tool adds context for interruption risk and price stability that the basic spot price (T18) doesn't provide.

**Design:**
```python
get_spot_history(
    provider: str,              # "aws"
    instance_type: str,         # "m5.xlarge"
    region: str,                # "us-east-1"
    availability_zone: str = "", # filter to specific AZ
    hours: int = 24             # lookback window
)
```

Returns: current price per AZ, min/max/avg over window, price volatility (stddev), and a stability label (`stable` / `moderate` / `volatile`).

Uses `ec2.describe_spot_price_history(StartTime=now-hours, InstanceTypes=[...])`. Requires credentials. Depends on T18 for the underlying `_get_spot_price()` method.

---

## Dependency map

```
T18 (spot pricing) ──► T30 (spot history)
T23 (Phase 4 errors) ──► T26 (GCP effective pricing) — clears placeholder
T27 (get_database_price) — improves on the inline RDS path added in bom.py
T28 (Azure) — independent, no deps
T29 (HTTP transport) — independent, no deps
```

Quick wins T20, T21, T22, T23, T25 have no dependencies and can be built in any order.

---

## RC3 triage — scope decisions

The RC3 engineering roadmap (`rc3-engineering-roadmap.md`, filed from customer/GCP
feedback reports) produced one GitHub issue per finding, numbered `RC3-001`
through `RC3-033` (issue numbers don't match RC3 IDs — each issue's title
carries its own RC3 ID). Most were scoped bug fixes and shipped directly. A
handful needed an explicit scope call — either narrowed down to an
independently-shippable slice, or deferred pending a dependency or owner
sign-off. Those decisions:

| RC3 ID | Issue | Decision | Rationale |
|--------|-------|----------|-----------|
| RC3-004 | [#31](https://github.com/x7even/cloudcostsmcp/issues/31) (open — v1 shipped, remaining scope tracked here) | **Scoped to AWS-only v1** (PR [#82](https://github.com/x7even/cloudcostsmcp/pull/82), merged in v1.2.0) | v1 reuses `estimate_bom`'s `processBOMItems` and `compare_prices`' region fan-out for AWS-only PricingSpec-dict items; non-AWS items report once under `not_supported`. It omits raw-SKU line items, a `providers?` filter, and a `weight` field — not because these are blocked by unlanded dependencies (RC3-016's batch SKU lookup already shipped as `get_prices_by_sku`; the "weight" field is new work `#31` itself calls out, not an external dependency), but because they're real scope/semantics decisions (how does `weight` affect ranking/totals? is a single-provider filter worth the surface area yet?) that need repo-owner sign-off, and raw-SKU wiring is nontrivial integration work (`LookupSKUAcrossRegions` resolves one SKU across many regions, not many items within one region — adapting it is a real follow-up, not a drop-in call). A separate, broader design note ([#75](https://github.com/x7even/cloudcostsmcp/pull/75)) had proposed the raw-SKU + `providers?` combination; closed as superseded once v1 shipped without it, so that design work is currently untracked pending a fresh proposal. |
| RC3-006 | [#32](https://github.com/x7even/cloudcostsmcp/issues/32) | **Scoped down** — shipped | The roadmap's full item bundled two independent halves: threading `Attributes["fallback"]` through `estimate_bom`'s existing output (shipped), and per-line×region fallback tagging for the not-yet-built `compare_bom_regions` (deferred alongside RC3-004, since that tool didn't exist yet). |
| RC3-014 | [#33](https://github.com/x7even/cloudcostsmcp/issues/33) (open) | **Deferred, blocked on RC3-004** | `compare_bom`'s 4-type enum can't accept CUR-native services (e.g. DynamoDB) without retiring the enum in favor of `estimate_bom`'s open-item substrate — which is exactly RC3-004's job. No independent fix exists; this still resolves whenever RC3-004's full (non-v1) contract lands, which hasn't happened — only the AWS-only v1 slice shipped (see RC3-004 row above). |
| RC3-015 | [#35](https://github.com/x7even/cloudcostsmcp/issues/35) | **Scoped as GCP-first, deferred** | No GCP/Azure equivalent of `get_price_by_sku` exists. XL effort — GCP SKU IDs and Azure meter IDs have no shared shape with AWS's usage-type convention, so the AWS resolver can't be generalized; each provider needs its own design. Recommended starting with GCP (simpler catalog) once live catalog access is available, not fabricated speculatively. |
| RC3-018 | [#37](https://github.com/x7even/cloudcostsmcp/issues/37) | **Scoped down** — shipped | `not_available_in` is only wired into 3 of ~9 fan-out/BoM tools. Scoped to `get_price` and `get_prices_batch` (no dependencies); the remaining tools (`compare_bom`, `estimate_bom`, `get_price_by_sku`) were left for follow-up since some overlap with RC3-004/RC3-019's own output shape work. |
| RC3-019 | [#38](https://github.com/x7even/cloudcostsmcp/issues/38) (closed) | **Built as thin introspection, AWS+GCP+Azure** (PR [#81](https://github.com/x7even/cloudcostsmcp/pull/81), merged in v1.2.0) | New `get_coverage(provider)` endpoint — L effort, new public API surface, so implemented as a draft pending owner sign-off before merging. Composes from the same per-price `fallback` attribute already computed elsewhere, fanned out across `DescribeCatalog()`'s service/domain/region lists — no new data-fabrication risk since it's introspecting the server's own catalog, not calling out to providers. |
| RC3-023 | [#39](https://github.com/x7even/cloudcostsmcp/issues/39) | **Deferred, split into 5 sub-issues** | Five flat/global-rate GCP services (Firestore, Pub/Sub, KMS, Cloud DNS, external IPs) have zero catalog entries. Each has a genuinely different pricing shape (per-key-version, per-zone+query-volume, per-GB throughput, etc.) — not a uniform bucket — so split into per-service issues ([#76](https://github.com/x7even/cloudcostsmcp/issues/76)–[#80](https://github.com/x7even/cloudcostsmcp/issues/80)), each gated on live GCP catalog access to verify SKU shape before implementing. |
| RC3-030 | [#49](https://github.com/x7even/cloudcostsmcp/issues/49) | **WONTFIX** | `get_prices_batch`'s per-item `errors` map on no-pricing-found was reported as a gap, but it's already correct, intentional, working code (`lookup.go` already checks `len(fr.prices)==0`). No functional change; noted as a documentation companion to RC3-027 (which covers the analogous `get_price` gap) instead. |
| RC3-032 | [#30](https://github.com/x7even/cloudcostsmcp/issues/30) (open — remaining sub-parts) | **Partially shipped** (PR [#74](https://github.com/x7even/cloudcostsmcp/pull/74), merged in v1.2.0) | Canonical `price_per_unit` field renamed across `get_price`/`get_price_by_sku`/`get_prices_batch`/`get_prices_by_sku`/`effective_price` for naming consistency (a breaking, intentional change — no compat alias, since these tools are LLM-consumed and re-read the schema per call), plus docs clarifying that `price_per_unit` is always a unit rate, never scaled by `size_gb`/quantity. Three sub-parts explicitly deferred to follow-up: the GCP egress unit-label mislabel (`per_gb_month` vs `per_gb` — entangled with `bom.go`'s cost-scaling branch, risks silently changing BOM totals if fixed without separate verification), `monthly_estimate`/breakdown decomposition for CDN/Cloud LB, and published per-tool JSON schemas. |

Every scoped-down or deferred item above followed the same rule: if a finding's
proposed fix required a new public tool/endpoint, a schema decision, or
speculative design work with no landed dependency to build on, it got a DACI
write-up in its issue (decision, alternatives considered, risk) rather than an
autonomous guess — consistent with this project's standing rule that new
tool contracts need repo-owner sign-off before merge, not just before design.

---

## Path to 1.0.0

### What 1.0.0 means

Version 1.0.0 signals a **stable API contract**: the 15-tool surface, spec shapes,
and response schemas will not break without a major version bump. It also marks the
completion of the Go rewrite (see below) and the last pending Phase 4 feature (T26).

### Remaining pre-1.0.0 work

| Item | Status | Notes |
|------|--------|-------|
| T26 — GCP effective pricing via BigQuery | pending | Last Phase 4 item |
| Go rewrite | planned | See below |
| Windows support in CI test matrix | nice-to-have | — |

### Go rewrite rationale

The Python implementation is sound, but a Go rewrite targets three concrete goals:

1. **Memory footprint.** The Python process idles at ~50–100 MB (interpreter +
   asyncio loop + all provider modules loaded). A Go binary runs at ~5–20 MB. On a
   home-cluster self-hosted deployment this is the dominant difference.

2. **Cold-start latency.** `uv run opencloudcosts` takes 2–3 s to import the
   module tree. A Go binary starts in under 100 ms — matters for on-demand or
   restart-heavy deployments.

3. **Distribution simplicity.** A single statically-linked binary requires no
   Python runtime, no `uv`, no virtual environment. The Docker image drops from
   ~200 MB (`python:3.12-slim` + deps) to ~25 MB (`scratch` + binary). This also
   eliminates Python version compatibility concerns going forward.

**Concurrency** is the weakest argument: asyncio already handles concurrent
provider HTTP fan-out well. Go goroutines would improve throughput for many
simultaneous MCP *client connections*, but the current single-client use case
doesn't feel this bottleneck.

**Go MCP library:** `mark3labs/mcp-go` — actively maintained, mirrors the
Python `fastmcp` API closely enough to be a natural translation target.

### PyPI distribution after the Go rewrite

The PyPI package does **not** need to be retired. The `ruff` and `uv` projects
establish the canonical pattern: Rust (or Go) binaries distributed via
platform-specific PyPI wheels. The same model applies here:

- CI (goreleaser) builds `opencloudcosts` binaries for `linux-x86_64`,
  `linux-aarch64`, `macos-x86_64`, `macos-arm64`, `windows-x86_64`.
- A packaging step wraps each binary into a platform-specific wheel
  (`opencloudcosts-1.0.0-py3-none-linux_x86_64.whl`, etc.). The wheel
  contains no Python code beyond a thin entry-point shim that `exec()`s the binary.
- `pip install opencloudcosts` and `uvx opencloudcosts` continue to work
  unchanged. Existing MCP client configs using `command: uvx` require no updates.

There is no `maturin` equivalent for Go — the wheel build is more bespoke
than the Rust/PyO3 path — but it is well-understood and used in production
by projects of this scale.

### Additional 1.0.0 distribution targets

- `go install github.com/x7even/cloudcostmcp@v1.0.0`
- Homebrew formula (`brew install opencloudcosts`)
- GitHub Releases with pre-built binaries (linux/mac/win, both arches)
- Docker image: `ghcr.io/x7even/opencloudcosts:1.0.0` (~25 MB scratch image)
