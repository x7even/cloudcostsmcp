# OpenCloudCosts MCP — Tool Reference (v0.8.8)

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
- **Supported services (v0.8.8):** compute/vm, storage/managed_disks, storage/blob, database/sql, database/cosmos, container/aks, serverless/azure_functions, ai/openai
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
    {"region": "us-east-1", "price": "$0.192000 per_hour", "monthly_estimate": "$140.16/mo", "vcpu": "4", "memory": "16 GiB"}
  ],
  "auth_available": false,
  "source": "catalog"
}
```

When credentials are present, `contracted_prices` and `effective_price` are also populated.

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
| `domain` | string | | `"compute"`, `"storage"`, `"database"`, `"ai"`, `"container"`, `"serverless"`, `"analytics"`, `"network"`, `"observability"`. Empty = all. |
| `service` | string | | e.g. `"bedrock"`, `"rds"`, `"gke"`, `"bigquery"`. Empty = all. |

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
