# OpenCloudCosts MCP — Tool Reference

## Overview

15 tools across 5 categories. Start with `describe_catalog` if unsure of field names, then call `get_price`.

---

## Provider Notes

### AWS
- Instance types: `m5.xlarge`, `c6g.2xlarge`, `r5.4xlarge`, etc.
- Regions: `us-east-1`, `ap-southeast-2`, `eu-west-1` (use `list_regions` for full list)
- Public pricing requires no credentials. Cost Explorer (effective pricing) requires `OCC_AWS_ENABLE_COST_EXPLORER=true`.

### GCP
- Instance types: `n2-standard-4`, `e2-standard-8`, `c2-standard-16`, `a2-highgpu-1g`, etc.
- Regions: `us-central1`, `us-east1`, `europe-west1`, `asia-southeast1`, etc.
- Requires `OCC_GCP_API_KEY` or Application Default Credentials (`gcloud auth application-default login`).
- CUD terms: `cud_1yr`, `cud_3yr`
- **Contract / effective pricing (v0.8.9):** Set `OCC_GCP_BILLING_ACCOUNT_ID=<id>` + ADC to retrieve negotiated rates via Cloud Billing Pricing API v1beta. When configured, `get_price` responses include an `effective_price` field with your discounted hourly rate and the `priceReason` type (`floating-discount`, `fixed-price`, etc.). Requires `billing.billingAccountPrice.get` IAM permission. API keys are NOT accepted by this endpoint.

### Azure
- Instance types use ARM SKU names: `Standard_D4s_v3`, `Standard_E8s_v3`, `Standard_NC6s_v3` (GPU), etc.
- Regions: `eastus`, `eastus2`, `westeurope`, `southeastasia`, `australiaeast`, etc.
- Public pricing — no credentials needed. Uses Azure Retail Prices API.
- Supported storage types: `premium-ssd`, `standard-ssd`, `standard-hdd`, `ultra-ssd`, `blob`
- **Supported services:** compute/vm, storage/managed_disks, storage/blob, database/sql, database/cosmos, container/aks, serverless/azure_functions, ai/openai, inter_region_egress
- Azure SQL `resource_type` examples: `"General Purpose 4 vCores"`, `"Business Critical 8 vCores"`, `"Hyperscale 2 vCores"`
- Azure OpenAI models: `gpt-4o`, `gpt-4o-mini`, `gpt-4`, `gpt-35-turbo`, `o1`, `o1-mini`, `text-embedding-3-small`, `text-embedding-3-large`

---

All tools return JSON. Errors are returned as `{"error": "..."}` so the LLM can reason about them.

---

## Pricing Lookup

### `get_price`

Unified pricing entry point. Replaces all domain-specific tools (compute, storage, database, AI, networking, etc.).

Pass a `spec` dict with at minimum: `provider`, `domain`, `region`. Add domain-specific fields as needed.
Use `describe_catalog(provider, domain)` to see required fields and a copy-paste `example_invocation`.

**Common spec shapes:**

| domain | Key fields | Example |
|--------|-----------|---------|
| `compute` | `resource_type` (instance type), `os`, `term` | `{"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge", "region": "us-east-1"}` |
| `storage` | `storage_type`, `size_gb` | `{"provider": "gcp", "domain": "storage", "storage_type": "pd-ssd", "size_gb": 500, "region": "us-central1"}` |
| `database` | `resource_type` (DB instance type), `engine`, `deployment` | `{"provider": "aws", "domain": "database", "resource_type": "db.r6g.large", "engine": "MySQL", "deployment": "single-az", "region": "us-east-1"}` |
| `ai` | `service` (`bedrock`/`vertex`/`gemini`), `model`, `input_tokens`, `output_tokens` | `{"provider": "aws", "domain": "ai", "service": "bedrock", "model": "amazon.titan-text-express-v1", "region": "us-east-1"}` |
| `serverless` | `service` (`lambda`/`cloud_run`), usage fields | `{"provider": "aws", "domain": "serverless", "service": "lambda", "region": "us-east-1"}` |
| `analytics` | `service` (`bigquery`/`redshift`), query/storage fields | `{"provider": "gcp", "domain": "analytics", "service": "bigquery", "region": "us-central1", "query_tb": 10, "active_storage_gb": 200}` |
| `network` | `service` (`lb`/`cdn`/`nat`/`data_transfer`/`cloud_armor`) | `{"provider": "gcp", "domain": "network", "service": "lb", "region": "us-central1"}` |
| `observability` | `service` (`cloudwatch`/`cloud_monitoring`) | `{"provider": "aws", "domain": "observability", "service": "cloudwatch", "region": "us-east-1"}` |
| `container` | `service` (`gke`/`eks`/`aks`) | `{"provider": "gcp", "domain": "container", "service": "gke", "region": "us-central1"}` |
| `inter_region_egress` | `source_region`, `dest_region` (optional), `data_gb` (optional) | `{"provider": "aws", "domain": "inter_region_egress", "source_region": "us-east-1", "dest_region": "eu-west-1", "data_gb": 1000}` |

**Pricing terms (valid `term` values):**

| Value | Description |
|-------|-------------|
| `on_demand` | On-demand (default) |
| `reserved_1yr` | 1-year No Upfront RI |
| `reserved_1yr_partial` | 1-year Partial Upfront RI |
| `reserved_1yr_all` | 1-year All Upfront RI (effective hourly) |
| `reserved_3yr` | 3-year No Upfront RI |
| `reserved_3yr_partial` | 3-year Partial Upfront RI |
| `reserved_3yr_all` | 3-year All Upfront RI (effective hourly) |
| `spot` | Spot/preemptible (AWS requires credentials) |
| `cud_1yr` | GCP 1-year Committed Use Discount |
| `cud_3yr` | GCP 3-year Committed Use Discount |

**Response:**
```json
{
  "public_prices": [
    {"region": "us-east-1", "price_per_unit": {"amount": 0.192, "unit": "per_hour", "currency": "USD", "display": "$0.192000/per_hour"}, "monthly_estimate": "$140.16/mo", "vcpu": "4", "memory": "16 GiB"}
  ],
  "auth_available": false,
  "source": "catalog"
}
```

When credentials are present, `contracted_prices` and `effective_price` are also populated.

`price_per_unit` is the canonical price field across every tool that returns a
price (`get_price`, `get_prices_batch`, `get_price_by_sku`, `compare_prices`,
`effective_price`, etc.) — always a per-unit rate, never pre-multiplied by
`size_gb`/quantity. Check `price_per_unit.unit` (e.g. `per_gb_month`,
`per_hour`) to know what to multiply by for a total; use `estimate_bom` /
`estimate_unit_economics` if you want the multiplication done for you.

---

### `get_prices_batch`

Get prices for multiple instance types in a single region concurrently. Returns sorted cheapest first.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"`, `"gcp"`, or `"azure"` |
| `instance_types` | list[string] | ✓ | e.g. `["m5.xlarge", "c5.xlarge", "r5.large"]` |
| `region` | string | ✓ | Region code |
| `os` | string | | `"Linux"` (default) or `"Windows"` |
| `term` | string | | `"on_demand"` (default), `"reserved_1yr"`, `"spot"` |

---

### `compare_prices`

Compare a spec across multiple regions concurrently. Returns sorted cheapest first with delta vs most expensive.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `spec` | dict | ✓ | PricingSpec dict (same as `get_price`). The `region` field is overridden per comparison. |
| `regions` | list[string] | | Region codes to compare. Omit for major regions. Pass `["all"]` for all available regions. |
| `baseline_region` | string | | Region for delta comparison, e.g. `"us-east-1"` |

---

### `get_price_by_sku`

Resolve a raw AWS usage-type/SKU string — exactly as it appears in a Cost & Usage Report (CUR) export,
e.g. `"CAN1-BoxUsage:r5a.8xlarge"` — to a price, across one or more regions. **AWS only.**

Use this instead of `get_price`/`compare_prices` when starting from a raw billing export line item
(a CUR `UsageType`/`SKU` value) rather than a known `resource_type`/domain spec. The region-prefix
token (e.g. `"CAN1-"`, `"EU-"`, or no prefix at all for `us-east-1`) is stripped from the usage-type
string to get a region-independent suffix, which is then matched against each target region's
pricing catalog.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | | `"aws"` (default and only supported value) |
| `sku` | string | ✓ | Raw usage-type/SKU string, e.g. `"CAN1-BoxUsage:r5a.8xlarge"` |
| `service` | string | | AWS servicecode hint, e.g. `"AmazonEC2"`, `"AWSELB"`, `"AmazonRDS"`, `"AmazonDynamoDB"`, `"AmazonElastiCache"`, `"AWSDataTransfer"`. Inferred from the usage-type pattern if omitted. |
| `regions` | list[string] | ✓ | AWS region codes to check, e.g. `["ca-central-1", "us-east-1"]`. Max 30. |
| `baseline_region` | string | | Region for delta comparison, e.g. `"us-east-1"` |
| `operation` | string | | Optional disambiguating hint — the AWS product `operation` attribute (e.g. `"CreateDBInstance:0021"` identifies Aurora PostgreSQL among RDS engines on the same instance type), matched case-insensitively. Use when a region comes back in `ambiguous_in`. |
| `product_family` | string | | Optional disambiguating hint — the AWS top-level `productFamily` (e.g. `"Load Balancer-Application"` for an ALB vs. NLB/GLB), matched case-insensitively. Use when a region comes back in `ambiguous_in`. |

**Service inference and mismatch fallback.** If `service` is omitted, the AWS servicecode is inferred
from the usage-type pattern (e.g. `BoxUsage:` → `AmazonEC2`, `LCUUsage` → `AWSELB`), and
`service_source` in the response is set to `"inferred"` (vs. `"explicit"` when `service` was
supplied). Real CUR exports aren't always internally consistent — e.g. `AWSDataTransfer` usage types
sometimes appear against an `"AWS Product"` column of `"AmazonEC2"`. If an explicit `service` hint
finds no match in a region but the inferred servicecode does, the tool falls back to the inferred
match rather than reporting no result, and flags `service_mismatch: true` on that region's entry.

**No-match vs. fetch-failure vs. ambiguous.** Regions where the catalog was fetched successfully but
no row matches the resolved suffix are listed in `no_mapping_in` (checked, not modeled). Regions
where the catalog fetch itself failed are listed separately in `errors_in`. Regions that matched more
than one distinct product row with no unambiguous resolution are listed in `ambiguous_in` (see
below). This three-way distinction lets callers tell "priced but not available here", "we couldn't
check", and "matched but needs a hint to disambiguate" apart.

**Ambiguous multi-row matches.** A usage-type suffix does not always map to a single priced product
row — e.g. an EC2 `BoxUsage:<instanceType>` suffix matches one row per
operatingSystem/tenancy/preInstalledSw/capacitystatus combination (Linux, Windows, RHEL, Shared vs.
Dedicated tenancy, etc.); ELB's `LCUUsage` suffix matches Application/Network/Gateway load balancer
pricing alike (distinct products, not variants); RDS's `InstanceUsage:<type>` suffix matches every
database engine on that instance type. Resolution is tried in order:

1. If `operation` and/or `product_family` were supplied, candidates are filtered to rows matching
   the hint(s) (case-insensitive). Exactly one match resolves the region (`hint_status:
   "resolved_by_hint"`). Zero matches fails closed — the hint is never silently ignored, the region
   stays ambiguous (`hint_status: "hint_no_match"`). More than one match falls through to step 2 on
   the hint-filtered subset.
2. Candidates are narrowed to the same canonical-default attributes (`operatingSystem=Linux`,
   `tenancy=Shared`, `preInstalledSw=NA`, `capacitystatus=Used`) the rest of this server uses. If
   that narrows to exactly one row, it resolves the region (`hint_status: "no_hint_supplied"` when no
   hint was involved).

**"Cheapest" is never used as a tie-breaker.** If neither step resolves to exactly one row, the
region is *excluded* from `all_regions_sorted`/`cheapest_price`/`most_expensive_price`/baseline
deltas and instead appears in its own top-level `ambiguous_in` bucket, with `alternate_match_count`
and every remaining candidate row in `alternate_matches` (via `description`/`product_family`/
`attributes`/`sku_id`) and no chosen price — the caller must pick the correct row and retry with
`operation`/`product_family` set. If every requested region ends up in `ambiguous_in`/`no_mapping_in`/
`errors_in`, `result: "no_prices_found"` is returned rather than a guessed price. A top-level warning
is also added to `warnings` whenever any region hits this case.

**Known limitation — compound/wavelength data-transfer SKUs.** Some `AWSDataTransfer` usage types
carry *two* region-shaped tokens, e.g. `"USE1WL1ATL1-CAN1-AWS-Out-Bytes"` (inter-region or AWS
Wavelength transfer). This tool's single-prefix-strip matching model does not fully resolve these —
it still attempts a match and infers `AWSDataTransfer`, but adds a warning to the response noting the
result may be inaccurate and should be verified manually, rather than silently returning a wrong or
empty match.

---

### `get_prices_by_sku`

Batch form of `get_price_by_sku`: resolve many raw AWS usage-type/SKU strings — each exactly as it
appears in a Cost & Usage Report (CUR) export — against the same set of target regions in one call.
**AWS only.**

Use this to reconcile many CUR line items at once (e.g. every distinct usage-type/SKU in a monthly
export) instead of issuing one `get_price_by_sku` call per SKU. Each `sku` is resolved independently
via the same logic `get_price_by_sku` uses, so per-region `ambiguous_in`/`no_mapping_in`/`errors_in`
bucketing and `baseline_region` deltas all apply per SKU exactly as they would in a standalone
`get_price_by_sku` call. Repeated `(service, region)` catalog fetches across SKUs in the same batch
are memoized, so a batch of SKUs that share a service/region only pays the catalog-fetch cost once.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | | `"aws"` (default and only supported value) |
| `skus` | list[string] | ✓ | Raw usage-type/SKU strings, each exactly as it appears in the CUR export. Max 25. |
| `regions` | list[string] | ✓ | AWS region codes to check, e.g. `["ca-central-1", "us-east-1"]`. Max 30, applies to every SKU. |
| `baseline_region` | string | | Region for delta comparison, applied to every SKU, e.g. `"us-east-1"` |

**No per-SKU `service`/`operation`/`product_family` hints.** Unlike `get_price_by_sku`, these hints
are not accepted here — they are inherently per-SKU, and different SKUs in a batch usually resolve to
different services. The AWS servicecode is inferred per SKU from its usage-type pattern. If a
particular SKU needs a hint to resolve an `ambiguous_in` entry, follow up with a single
`get_price_by_sku` call for that SKU, passing `operation`/`product_family`.

**Result ordering and per-SKU failures.** Each successfully-processed SKU appears in `results`, in the
same order as the input `skus` list — NOT re-sorted by price, since distinct SKUs commonly price in
different units (per-hour, per-GB, per-request, ...) that aren't meaningfully comparable. A SKU that
fails outright (e.g. an empty string, or a usage-type pattern no service could be inferred for) is
instead reported in the top-level `errors` map, keyed by that SKU string, with `message`/`status`/
`retryable` fields mirroring `get_prices_batch`'s per-item error shape.

---

### `search_pricing`

Free-text search across the pricing catalog. Useful when you don't know the exact SKU or service name.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"`, `"gcp"`, or `"azure"` |
| `query` | string | ✓ | Keyword, e.g. `"m5"`, `"metric"`, `"MySQL"`, `"egress"` |
| `domain` | string | | Filter by domain, e.g. `"compute"`, `"database"` |
| `service` | string | | Filter by service, e.g. `"rds"`, `"cloudwatch"` |
| `region` | string | | Filter to a specific region |
| `max_results` | int | | Max results (default `20`) |

---

### `get_spot_history`

Spot price history and stability analysis. **Requires AWS credentials.**

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `spec` | dict | ✓ | PricingSpec dict with `provider="aws"`, `domain="compute"`, `resource_type`, `region` |
| `hours` | int | | Lookback window in hours (default `24`, max `720`) |
| `availability_zone` | string | | Filter to specific AZ, e.g. `"us-east-1a"`. Empty = all AZs. |

---

## Discovery

### `describe_catalog`

Discover what each provider supports and how to call `get_price`.

- **No args** → full support matrix across all configured providers.
- **provider only** → all domains/services for that provider.
- **provider + domain [+ service]** → targeted guidance with `required_fields`, `supported_terms`, `filter_hints`, and a ready-to-use `example_invocation` you can pass directly to `get_price`.

Use this before `get_price` when unsure of exact field names or values.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | | `"aws"`, `"gcp"`, or `"azure"`. Empty = all providers. |
| `domain` | string | | `"compute"`, `"storage"`, `"database"`, `"ai"`, `"container"`, `"serverless"`, `"analytics"`, `"network"`, `"observability"`, `"inter_region_egress"`. Empty = all. |
| `service` | string | | e.g. `"bedrock"`, `"rds"`, `"gke"`, `"bigquery"`. Empty = all. |

---

### `get_coverage`

Report which domains/services this server actually covers, per provider.

v1 scope: structural coverage from the catalog only — each domain is reported as `catalog` (with its known services) unless the provider has no entry for it at all. This does **not** fan out a live `get_price` call per region — whether a specific region's live price is a real catalog rate or a degraded fallback constant is only observable by calling `get_price` for that spec and checking its `fallback` field, since that is a live fetch outcome rather than a fixed property of the catalog.

Use this to answer "what does this server know about" before trial-and-error against `describe_catalog` and individual `get_price` calls.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | | `"aws"`, `"gcp"`, or `"azure"`. Empty = all configured providers. |

---

### `list_regions`

List all regions where a cloud service is available.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"`, `"gcp"`, or `"azure"` |
| `domain` | string | | `"compute"` (default), `"storage"`, `"database"` |

---

### `list_instance_types`

List available compute instance types in a region with optional filters.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"`, `"gcp"`, or `"azure"` |
| `region` | string | ✓ | Region code |
| `family` | string | | Instance family prefix, e.g. `"m5"`, `"c6g"`, `"n2"`, `"Standard_D"` |
| `min_vcpu` | int | | Minimum vCPU count |
| `min_memory_gb` | float | | Minimum memory in GB |
| `gpu` | bool | | If `true`, only return GPU instances |

---

## Region Analysis

### `find_cheapest_region`

Find the cheapest region for any cloud service. Queries concurrently, returns sorted cheapest first.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `spec` | dict | ✓ | PricingSpec dict (same as `get_price`). The `region` field is overridden per comparison. |
| `regions` | list[string] | | Region codes to compare. Omit for major regions (12 for AWS/GCP, faster). Pass `["all"]` for exhaustive search. |
| `baseline_region` | string | | Region for delta comparison, e.g. `"us-east-1"` |

---

### `find_available_regions`

Find all regions where a specific service/instance type is available, sorted cheapest first.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `spec` | dict | ✓ | PricingSpec dict (same as `get_price`). The `region` field is overridden per comparison. |
| `regions` | list[string] | | Region codes to check. Omit for major regions. Pass `["all"]` for exhaustive search. |
| `baseline_region` | string | | Region for delta comparison |

---

## FinOps

### `get_discount_summary`

Retrospective account-wide discount utilization: Savings Plans, Reserved Instances, CUDs.

**Requires:** AWS credentials + `OCC_AWS_ENABLE_COST_EXPLORER=true`

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | | `"aws"` (default) |

---

### `estimate_bom`

Calculate Total Cost of Ownership for a multi-resource stack. Returns per-item and total monthly/annual costs,
plus a `not_included` list with follow-up `get_price` calls for hidden costs (egress, load balancers, monitoring).

**Use this instead of multiple `get_price` calls** for multi-resource questions.

Each item in `items` is a `get_price` spec dict plus these extra fields:

| Field | Default | Description |
|-------|---------|-------------|
| `quantity` | `1` | Number of units |
| `hours_per_month` | `730` | Hours active per month (730 = always-on) |
| `description` | | Optional label for this line item |

**Example items:**
```json
[
  {"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge", "region": "us-east-1", "quantity": 3},
  {"provider": "aws", "domain": "database", "resource_type": "db.r6g.large", "engine": "MySQL", "deployment": "single-az", "region": "us-east-1"},
  {"provider": "aws", "domain": "storage", "storage_type": "gp3", "size_gb": 500, "region": "us-east-1"}
]
```

**Example response:**
```json
{
  "totals": {"monthly": "$1196.32", "annual": "$14355.84"},
  "line_items": [
    {"description": "m5.xlarge (us-east-1)", "quantity": 3, "monthly_cost": "$420.48"},
    {"description": "db.r6g.large (us-east-1)", "quantity": 1, "monthly_cost": "$735.84"},
    {"description": "gp3 (us-east-1)", "quantity": 1, "monthly_cost": "$40.00"}
  ],
  "not_included": [
    {"item": "Data transfer (egress)", "how_to_price": "get_price(spec={...})"}
  ]
}
```

---

### `compare_bom_regions`

Compare a Bill of Materials' total monthly cost across multiple AWS regions. Returns `regions[]` sorted
cheapest-first, each with `total_monthly`, resolved `line_items`, and any per-item errors.

**v1 scope: AWS-only.** Items using other providers are reported once under `not_supported` rather than
guessed or dropped silently — GCP/Azure support is tracked separately. See the
[#31 design discussion](https://github.com/x7even/cloudcostsmcp/issues/31) for scope rationale.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `items` | list[dict] | ✓ | Same format as `estimate_bom`. The `region` field on each item is overridden per comparison. |
| `regions` | list[string] | ✓ | AWS region codes to compare, e.g. `["us-east-1", "eu-west-1"]` |
| `baseline_region` | string | | Region for delta comparison, e.g. `"us-east-1"` |

---

### `estimate_unit_economics`

Estimate cost per user, request, or transaction given a BoM and monthly volume.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `items` | list[dict] | ✓ | Same format as `estimate_bom` |
| `units_per_month` | float | ✓ | Monthly volume, e.g. `50000` |
| `unit_label` | string | | `"user"`, `"request"`, `"transaction"` (default `"user"`) |

---

## Cache Management

### `cache_stats`

Return cache statistics: entry counts and SQLite DB size.

Returns: `{"price_entries": 142, "metadata_entries": 8, "db_size_mb": 0.45}`

---

### `refresh_cache`

Invalidate the pricing cache to force fresh data on the next request.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | | Provider to clear (`"aws"`, `"gcp"`, `"azure"`). Empty = purge all expired entries. |
