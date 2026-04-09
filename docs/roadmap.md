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
| T27 — get_database_price tool | [T27-get-database-price-tool.md](plans/T27-get-database-price-tool.md) | **done** |
| T28 — Azure provider | [T28-azure-provider.md](plans/T28-azure-provider.md) | **done** |
| T29 — HTTP/SSE transport | [T29-http-transport.md](plans/T29-http-transport.md) | **done** |
| T30 — Spot price history tool | [T30-spot-price-history.md](plans/T30-spot-price-history.md) | **done** |
| T31 — GCP Windows pricing | [T31-gcp-windows-pricing.md](plans/T31-gcp-windows-pricing.md) | **done** |

---

## Quick wins — can build in parallel immediately

### T20 · Add `os` field to `estimate_bom` / `estimate_unit_economics`
**Status:** pending  
**Effort:** Small (2 files)  
**Files:** `src/opencloudcosts/tools/bom.py`, `docs/tools.md`

`estimate_bom` hardcodes `"Linux"` for all compute items (`bom.py:69`, `bom.py:162`). Windows instances cost 30–40% more — not accounting for this skews TCO estimates.

**Implementation:**
- Read `os` from each item dict, default `"Linux"`. Pass to `get_compute_price()` in both tools.
- Update the tool docstring and `docs/tools.md` BoM line item fields table to document `os`.
- Add test: BoM with a Windows instance returns a higher price than the same Linux instance.

---

### T21 · Fix `refresh_cache` to also clear metadata entries for a provider
**Status:** pending  
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
**Status:** pending  
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
**Status:** pending  
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
**Status:** pending  
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
**Status:** pending  
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
**Status:** pending  
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
**Status:** pending  
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
**Status:** pending  
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
**Status:** pending  
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
**Status:** pending  
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
**Status:** pending  
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
**Status:** pending  
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
