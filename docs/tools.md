# OpenCloudCosts — Tool Reference

## Provider Notes

### AWS
- Instance types: `m5.xlarge`, `c6g.2xlarge`, `r5.4xlarge`, etc.
- Regions: `us-east-1`, `ap-southeast-2`, `eu-west-1`, etc. (use `list_regions` for full list)
- Public pricing works with no credentials. Cost Explorer requires `OCC_AWS_ENABLE_COST_EXPLORER=true`.

### GCP
- Instance types: `n2-standard-4`, `e2-standard-8`, `c2-standard-16`, etc.
- Regions: `us-central1`, `us-east1`, `europe-west1`, `asia-southeast1`, etc.
- Requires a GCP API key (`OCC_GCP_API_KEY`) or Application Default Credentials (`gcloud auth application-default login`).
- Pricing works by combining per-vCPU and per-GB-RAM SKUs: `total = vcpus × cpu_price + memory_gb × ram_price`
- CUD (Committed Use Discount) pricing available via `term="cud_1yr"` or `term="cud_3yr"`
- Supported machine families: `e2`, `n1`, `n2`, `n2d`, `c2`, `c2d`, `t2d`, `t2a`, `m1`
- Supported storage types: `pd-standard`, `pd-ssd`, `pd-balanced`, `pd-extreme`

### Azure
- Instance types use ARM SKU names: `Standard_D4s_v3`, `Standard_E8s_v3`, `Standard_B2ms`, `Standard_NC6s_v3` (GPU), etc.
- Regions use ARM region names (lowercase, no spaces): `eastus`, `eastus2`, `westeurope`, `southeastasia`, `australiaeast`, etc.
- **Public pricing — no credentials needed.** Uses the Azure Retail Prices REST API.
- Pricing terms: `on_demand` (default), `reserved_1yr`, `reserved_3yr`, `spot`
- Windows pricing: pass `os="Windows"` to `get_compute_price`
- Supported storage types: `premium-ssd`, `standard-ssd`, `standard-hdd`, `ultra-ssd`, `blob`
- Note: `list_instance_types` returns instance names only — vCPU/memory metadata is not available from the Retail Prices API. Use [Azure VM sizes docs](https://learn.microsoft.com/en-us/azure/virtual-machines/sizes) for specs.

---

All tools are callable via MCP `tools/call`. Parameters are JSON-typed. All tools return a JSON object; errors are returned as `{"error": "..."}` rather than exceptions so the LLM can reason about them.

---

## Pricing Lookup

### `get_compute_price`

Get the price for a specific compute instance type in a cloud region.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"`, `"gcp"`, or `"azure"` |
| `instance_type` | string | ✓ | e.g. `"m5.xlarge"`, `"c6g.2xlarge"`, `"n2-standard-4"`, `"Standard_D4s_v3"` |
| `region` | string | ✓ | Region code, e.g. `"us-east-1"`, `"ap-southeast-2"` |
| `os` | string | | `"Linux"` (default) or `"Windows"` |
| `term` | string | | `"on_demand"` (default), `"reserved_1yr"`, `"reserved_3yr"` |

**Example response:**
```json
{
  "provider": "aws",
  "instance_type": "m5.xlarge",
  "region": "us-east-1",
  "os": "Linux",
  "term": "on_demand",
  "prices": [
    {
      "provider": "aws",
      "description": "m5.xlarge",
      "region": "us-east-1",
      "term": "on_demand",
      "price": "$0.192000 per_hour",
      "monthly_estimate": "$140.16/mo",
      "instanceType": "m5.xlarge",
      "vcpu": "4",
      "memory": "16 GiB"
    }
  ],
  "count": 1
}
```

---

### `compare_compute_prices`

Compare the same instance type across multiple regions. Returns results sorted cheapest first with % delta.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"` or `"gcp"` |
| `instance_type` | string | ✓ | e.g. `"m5.xlarge"` |
| `regions` | list[string] | ✓ | e.g. `["us-east-1", "ap-southeast-2", "eu-west-1"]` |
| `os` | string | | `"Linux"` (default) or `"Windows"` |
| `term` | string | | `"on_demand"` (default) |

**Example response:**
```json
{
  "provider": "aws",
  "instance_type": "m5.xlarge",
  "term": "on_demand",
  "cheapest_region": "us-east-1",
  "most_expensive_region": "ap-southeast-2",
  "price_delta_pct": 44.79,
  "prices_by_region": [
    {"region": "us-east-1", "price": "$0.192000 per_hour", "monthly_estimate": "$140.16/mo"},
    {"region": "eu-west-1", "price": "$0.214000 per_hour", "monthly_estimate": "$156.22/mo"},
    {"region": "ap-southeast-2", "price": "$0.278000 per_hour", "monthly_estimate": "$202.94/mo"}
  ]
}
```

---

### `get_storage_price`

Get block/object storage pricing in a region.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"` or `"gcp"` |
| `storage_type` | string | ✓ | AWS: `"gp3"`, `"gp2"`, `"io1"`, `"io2"`, `"st1"`, `"sc1"`. GCP: `"pd-ssd"`, `"pd-balanced"`, `"pd-standard"` |
| `region` | string | ✓ | Region code |
| `size_gb` | float | | Size for monthly cost estimate (default `100`) |

---

### `search_pricing`

Free-text search across the pricing catalog. Useful for exploring instance families.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"` or `"gcp"` |
| `query` | string | ✓ | e.g. `"m5"`, `"c6g"`, `"gpu"`, `"r5.2xlarge"` |
| `region` | string | | Filter to a specific region |
| `max_results` | int | | Max results (default `10`, max `50`) |

---

### `get_effective_price`

Get effective pricing reflecting actual account discounts (Reserved Instances, Savings Plans, CUDs, EDPs). Returns the effective hourly rate and % discount vs on-demand.

**Requires:** AWS credentials + `OCC_AWS_ENABLE_COST_EXPLORER=true` (costs $0.01/call)

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"` or `"gcp"` |
| `service` | string | ✓ | `"compute"`, `"storage"`, `"database"` |
| `instance_type` | string | ✓ | e.g. `"m5.xlarge"` |
| `region` | string | ✓ | Region code |

**Example response:**
```json
{
  "provider": "aws",
  "instance_type": "m5.xlarge",
  "region": "us-east-1",
  "effective_prices": [
    {
      "on_demand_rate": "$0.192000/per_hour",
      "effective_rate": "$0.124800/per_hour",
      "discount_type": "Blended (RI/SP/EDP)",
      "discount_pct": "35.0%",
      "savings": "$0.067200/per_hour",
      "commitment": null,
      "source": "cost_explorer"
    }
  ]
}
```

---

### `get_discount_summary`

Summarise all active discounts in the AWS account: Savings Plans, Reserved Instances, and utilization metrics.

**Requires:** AWS credentials + `OCC_AWS_ENABLE_COST_EXPLORER=true`

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | | `"aws"` (default) |

**Example response:**
```json
{
  "sp_count": 2,
  "ri_count": 8,
  "savings_plans": [
    {
      "id": "sp-abc123",
      "type": "Compute",
      "payment_option": "No Upfront",
      "commitment_usd_per_hour": "1.50",
      "term_years": "1",
      "end": "2026-01-01T00:00:00Z",
      "utilization_pct": "92.5"
    }
  ],
  "reserved_instances": [
    {
      "instance_type": "m5.xlarge",
      "count": 5,
      "offering_type": "No Upfront",
      "duration_years": "1",
      "days_remaining": 180
    }
  ],
  "utilization": {
    "savings_plans": {
      "utilization_pct": "92.0",
      "net_savings": "312.50"
    }
  }
}
```

---

## Availability & Discovery

### `list_regions`

List all regions where a service is available.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"` or `"gcp"` |
| `service` | string | | `"compute"` (default), `"storage"`, `"database"` |

---

### `list_instance_types`

List available compute instance types in a region with optional filters.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"` or `"gcp"` |
| `region` | string | ✓ | Region code |
| `family` | string | | Instance family prefix, e.g. `"m5"`, `"c6g"`, `"r5"` |
| `min_vcpus` | int | | Minimum vCPU count |
| `min_memory_gb` | float | | Minimum memory in GB |
| `gpu` | bool | | If `true`, only return GPU instances |
| `max_results` | int | | Max results (default `30`) |

---

### `check_availability`

Check if a specific instance type or SKU is available in a region.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"` or `"gcp"` |
| `service` | string | ✓ | `"compute"`, `"storage"` |
| `sku_or_type` | string | ✓ | e.g. `"m5.xlarge"`, `"gp3"` |
| `region` | string | ✓ | Region code |

---

### `find_cheapest_region`

Find the cheapest region for an instance type by querying all (or specified) regions concurrently.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"` or `"gcp"` |
| `instance_type` | string | ✓ | e.g. `"m5.xlarge"` |
| `os` | string | | `"Linux"` (default) or `"Windows"` |
| `term` | string | | `"on_demand"` (default), `"reserved_1yr"`, `"reserved_3yr"` |
| `regions` | list[string] | | Specific regions to compare. Omit for all regions. |

**Example response:**
```json
{
  "provider": "aws",
  "instance_type": "m5.xlarge",
  "cheapest_region": "us-east-1",
  "cheapest_price": "$0.192000/hr",
  "most_expensive_region": "ap-southeast-2",
  "most_expensive_price": "$0.278000/hr",
  "price_delta_pct": 44.79,
  "all_regions_sorted": [
    {"region": "us-east-1", "price_per_hour": "$0.192000", "monthly_estimate": "$140.16"},
    {"region": "us-east-2", "price_per_hour": "$0.192000", "monthly_estimate": "$140.16"},
    ...
  ]
}
```

---

## TCO & BoM Estimation

### `estimate_bom`

Calculate Total Cost of Ownership for a Bill of Materials. Each item represents a cloud resource.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `items` | list[object] | ✓ | List of BoM line items (see below) |

**BoM line item fields:**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `provider` | string | `"aws"` | Cloud provider |
| `service` | string | | `"compute"` or `"storage"` |
| `type` | string | | Instance/storage type, e.g. `"m5.xlarge"`, `"gp3"` |
| `region` | string | `"us-east-1"` | Region code |
| `quantity` | int | `1` | Number of units |
| `hours_per_month` | float | `730` | Hours/month (730 = always-on) |
| `term` | string | `"on_demand"` | Pricing term |
| `description` | string | | Human-readable label |
| `size_gb` | float | `100` | For storage items only |

**Example request:**
```json
{
  "items": [
    {"provider": "aws", "service": "compute", "type": "m5.xlarge", "region": "us-east-1", "quantity": 3, "description": "API servers"},
    {"provider": "aws", "service": "compute", "type": "r5.2xlarge", "region": "us-east-1", "quantity": 2, "description": "DB servers"},
    {"provider": "aws", "service": "storage", "type": "gp3", "region": "us-east-1", "quantity": 1, "size_gb": 500, "description": "Primary EBS volume"}
  ]
}
```

**Example response:**
```json
{
  "line_items": [
    {"description": "API servers", "quantity": 3, "unit_price": "$0.192000/per_hour", "monthly_cost": "$420.48", "annual_cost": "$5045.76"},
    {"description": "DB servers", "quantity": 2, "unit_price": "$0.504000/per_hour", "monthly_cost": "$735.84", "annual_cost": "$8830.08"},
    {"description": "Primary EBS volume", "quantity": 1, "unit_price": "$0.080000/per_gb_month", "monthly_cost": "$40.00", "annual_cost": "$480.00"}
  ],
  "totals": {
    "monthly": "$1196.32",
    "annual": "$14355.84",
    "currency": "USD"
  }
}
```

---

### `estimate_unit_economics`

Estimate cost per user, per request, or per transaction given a BoM and expected monthly volume.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `items` | list[object] | ✓ | Same format as `estimate_bom` |
| `units_per_month` | float | ✓ | Monthly volume (e.g. `50000` users) |
| `unit_label` | string | | Label for units: `"user"`, `"request"`, `"transaction"` (default `"user"`) |

**Example response:**
```json
{
  "infrastructure_monthly": "$1196.32",
  "infrastructure_annual": "$14355.84",
  "volume": "50,000 users/month",
  "cost_per_unit": "$0.0239 per user",
  "cost_per_unit_annual": "$0.2871 per user/year"
}
```

---

## Cache Management

### `refresh_cache`

Invalidate the pricing cache to force fresh API data on the next request.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | | Provider to clear (`"aws"`, `"gcp"`). Empty = purge all expired entries. |

### `cache_stats`

Return statistics about the local SQLite pricing cache.

Returns: `{"price_entries": 142, "metadata_entries": 8, "db_size_mb": 0.45, "db_path": "..."}`
