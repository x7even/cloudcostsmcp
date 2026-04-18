# OpenCloudCosts MCP — Tool Reference

## Provider Notes

### AWS
- Instance types: `m5.xlarge`, `c6g.2xlarge`, `r5.4xlarge`, etc.
- Regions: `us-east-1`, `ap-southeast-2`, `eu-west-1`, etc. (use `list_regions` for full list)
- Public pricing works with **no credentials**. Cost Explorer requires `OCC_AWS_ENABLE_COST_EXPLORER=true`.
- Non-compute services (CloudWatch, data transfer, RDS, Lambda, etc.) available via `get_service_price`.

### GCP
- Instance types: `n2-standard-4`, `e2-standard-8`, `c2-standard-16`, etc.
- Regions: `us-central1`, `us-east1`, `europe-west1`, `asia-southeast1`, etc.
- Requires `OCC_GCP_API_KEY` or Application Default Credentials (`gcloud auth application-default login`).
- Pricing: per-vCPU + per-GB-RAM SKUs combined — `total = vcpus × cpu_price + memory_gb × ram_price`
- CUD terms: `cud_1yr`, `cud_3yr`

### Azure
- Instance types use ARM SKU names: `Standard_D4s_v3`, `Standard_E8s_v3`, `Standard_B2ms`, `Standard_NC6s_v3` (GPU), etc.
- Regions use ARM region names (lowercase, no spaces): `eastus`, `eastus2`, `westeurope`, `southeastasia`, `australiaeast`, etc.
- **Public pricing — no credentials needed.** Uses the Azure Retail Prices REST API.
- Pricing terms: `on_demand` (default), `reserved_1yr`, `reserved_3yr`, `spot`
- Windows pricing: pass `os="Windows"` to `get_compute_price`
- Supported storage types: `premium-ssd`, `standard-ssd`, `standard-hdd`, `ultra-ssd`, `blob`
- Note: `list_instance_types` returns instance names only — vCPU/memory metadata is not available from the Retail Prices API. Use [Azure VM sizes docs](https://learn.microsoft.com/en-us/azure/virtual-machines/sizes) for specs.

---

All tools return JSON. Errors are returned as `{"error": "..."}` so the LLM can reason about them.

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
| `term` | string | | Pricing term — see table below (default: `"on_demand"`) |

**Valid `term` values for `get_compute_price`:**

| Value | Description |
|-------|-------------|
| `on_demand` | On-demand (default) |
| `reserved_1yr` | 1-year No Upfront RI |
| `reserved_1yr_partial` | 1-year Partial Upfront RI |
| `reserved_1yr_all` | 1-year All Upfront RI (normalised to effective hourly) |
| `reserved_3yr` | 3-year No Upfront RI |
| `reserved_3yr_partial` | 3-year Partial Upfront RI |
| `reserved_3yr_all` | 3-year All Upfront RI (normalised to effective hourly) |
| `spot` | Spot instance (requires credentials) |

**Example response:**
```json
{
  "provider": "aws", "instance_type": "m5.xlarge", "region": "us-east-1",
  "prices": [{
    "region": "us-east-1", "region_name": "US East (N. Virginia)",
    "price": "$0.192000 per_hour", "monthly_estimate": "$140.16/mo",
    "vcpu": "4", "memory": "16 GiB"
  }]
}
```

---

### `get_spot_history`

Get spot price history and stability analysis. **Requires AWS credentials.**

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"` |
| `instance_type` | string | ✓ | e.g. `"m5.xlarge"` |
| `region` | string | ✓ | Region code |
| `availability_zone` | string | | Filter to specific AZ, e.g. `"us-east-1a"`. Empty = all AZs. |
| `os` | string | | `"Linux"` (default) or `"Windows"` |
| `hours` | int | | Lookback window in hours (default `24`, max `720`) |

---

### `get_storage_price`

Get block or object storage pricing in a region.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"`, `"gcp"`, or `"azure"` |
| `storage_type` | string | ✓ | AWS: `"gp3"`, `"gp2"`, `"io1"`, `"io2"`, `"st1"`, `"sc1"`, `"s3"`. GCP Persistent Disk: `"pd-ssd"`, `"pd-balanced"`, `"pd-standard"`, `"pd-extreme"`. GCP Cloud Storage (GCS): `"standard"`, `"nearline"`, `"coldline"`, `"archive"`. Azure: `"premium-ssd"`, `"standard-ssd"`, `"standard-hdd"`, `"ultra-ssd"`, `"blob"` |
| `region` | string | ✓ | Region code |
| `size_gb` | float | | Size for monthly cost estimate (default `100`) |

---

### `get_database_price`

Get pricing for a managed database instance — AWS RDS or GCP Cloud SQL.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"` or `"gcp"` |
| `instance_type` | string | ✓ | AWS: e.g. `"db.r5.large"`, `"db.t4g.micro"`. GCP Cloud SQL: e.g. `"db-n1-standard-4"`, `"db-n1-standard-8"` |
| `region` | string | ✓ | Region code |
| `engine` | string | | AWS: `"MySQL"` (default), `"PostgreSQL"`, `"MariaDB"`, `"Oracle"`, `"SQLServer"`, `"Aurora-MySQL"`, `"Aurora-PostgreSQL"`. GCP: `"MySQL"` (default), `"PostgreSQL"`, `"SQL Server"` |
| `deployment` | string | | AWS: `"single-az"` (default) or `"multi-az"`. GCP: `"single-zone"` (default), `"ha"`, or `"regional"` (HA = cross-zone replication, ~2× cost) |
| `term` | string | | `"on_demand"` (default), `"reserved_1yr"`, `"reserved_3yr"` (AWS only) |

**Example response:**
```json
{
  "provider": "aws",
  "instance_type": "db.r5.large",
  "engine": "MySQL",
  "deployment": "single-az",
  "term": "on_demand",
  "region": "us-east-1",
  "region_name": "US East (N. Virginia)",
  "price_per_hour": "$0.240000",
  "monthly_estimate": "$175.20/mo"
}
```

---

### `get_service_price`

**Generic pricing for any AWS service** — CloudWatch, data transfer, RDS, Lambda, ELB, CloudFront, Route53, DynamoDB, EFS, ElastiCache, and 250+ others.

Use `list_services()` to discover service codes, and `search_pricing(service=...)` to explore product attributes before filtering.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"` |
| `service` | string | ✓ | Service code or alias (see table below) |
| `region` | string | ✓ | Region code, e.g. `"us-east-1"` |
| `filters` | object | | Attribute key/value pairs to narrow results |
| `max_results` | int | | Max results (default `20`) |

**Common service aliases:**

| Alias | Canonical code | Example filters |
|-------|---------------|-----------------|
| `cloudwatch` | AmazonCloudWatch | `{"group": "Metric"}` |
| `data_transfer` | AWSDataTransfer | `{"transferType": "AWS Outbound"}` |
| `rds` | AmazonRDS | `{"databaseEngine": "MySQL", "instanceType": "db.r5.large"}` |
| `lambda` | AWSLambda | `{"group": "AWS-Lambda-Duration"}` |
| `elb` | AWSELB | `{"productFamily": "Load Balancer"}` |
| `cloudfront` | AmazonCloudFront | `{"productFamily": "Data Transfer"}` |
| `route53` | AmazonRoute53 | `{"productFamily": "DNS Query"}` |
| `dynamodb` | AmazonDynamoDB | `{"group": "DDB-WriteUnits"}` |
| `efs` | AmazonEFS | `{"productFamily": "Storage"}` |
| `elasticache` | AmazonElastiCache | `{"cacheEngine": "Redis"}` |
| `sqs` | AmazonSQS | `{"productFamily": "Queue"}` |
| `redshift` | AmazonRedshift | `{"instanceType": "dc2.large"}` |

**Example — CloudWatch metric pricing:**
```
get_service_price(provider="aws", service="cloudwatch",
                  region="us-east-1", filters={"group": "Metric"})
```

**Example — Data transfer us-east-1 to eu-west-1:**
```
get_service_price(provider="aws", service="data_transfer", region="us-east-1",
                  filters={"fromRegionCode": "us-east-1", "toRegionCode": "eu-west-1"})
```

**Example — RDS MySQL db.r5.large:**
```
get_service_price(provider="aws", service="rds", region="us-east-1",
                  filters={"databaseEngine": "MySQL", "instanceType": "db.r5.large",
                            "deploymentOption": "Single-AZ"})
```

---

### `get_prices_batch`

Get prices for multiple instance types in a single region in one call. Fetches concurrently, returns sorted cheapest first.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"`, `"gcp"`, or `"azure"` |
| `instance_types` | list[string] | ✓ | e.g. `["m5.xlarge", "c5.xlarge", "r5.large"]` |
| `region` | string | ✓ | Region code |
| `os` | string | | `"Linux"` (default) or `"Windows"` |
| `term` | string | | `"on_demand"` (default), `"reserved_1yr"`, `"spot"` |

---

### `compare_compute_prices`

Compare the same compute instance type across multiple regions. Fetches concurrently, returns sorted cheapest first.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"`, `"gcp"`, or `"azure"` |
| `instance_type` | string | ✓ | e.g. `"m5.xlarge"` |
| `regions` | list[string] | ✓ | Region codes to compare |
| `os` | string | | `"Linux"` (default) or `"Windows"` |
| `term` | string | | `"on_demand"` (default) |
| `baseline_region` | string | | Region for delta comparison, e.g. `"us-east-1"` |

---

### `search_pricing`

Search the pricing catalog by keyword. Defaults to EC2 compute — set `service` to search any other AWS service.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"`, `"gcp"`, or `"azure"` |
| `query` | string | ✓ | Keyword, e.g. `"m5"`, `"metric"`, `"MySQL"`, `"egress"`, `"Standard_D"` |
| `region` | string | | Filter to a specific region |
| `service` | string | | AWS service to search (default: `"ec2"`). Use `list_services()` to discover options. |
| `max_results` | int | | Max results (default `10`, max `50`) |

---

## GCP-Specific Services

### `get_gke_price`

Get GKE (Google Kubernetes Engine) cluster pricing.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `region` | string | ✓ | GCP region, e.g. `"us-central1"` |
| `mode` | string | | `"standard"` (default) or `"autopilot"` |
| `node_count` | int | | Standard mode: number of worker nodes (default `3`) |
| `node_type` | string | | Standard mode: node instance type, e.g. `"n2-standard-4"` (default `"n1-standard-4"`) |
| `vcpu` | float | | Autopilot mode: total vCPUs requested across all pods |
| `memory_gb` | float | | Autopilot mode: total memory GiB requested across all pods |
| `hours_per_month` | float | | Hours active per month (default `730` = always-on) |

**Standard mode** charges a flat $0.10/hr cluster management fee plus normal Compute Engine rates for worker nodes. **Autopilot mode** bills per vCPU-hour and per GiB-RAM-hour of pod requests (no separate node cost).

---

### `get_memorystore_price`

Get GCP Memorystore for Redis pricing.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `capacity_gb` | float | ✓ | Provisioned cache size in GB |
| `region` | string | ✓ | GCP region, e.g. `"us-central1"` |
| `tier` | string | | `"basic"` (single-zone) or `"standard"` (HA, default) |
| `hours_per_month` | float | | Hours active per month (default `730`) |

Standard tier includes cross-zone replication and typically costs ~1.3–2× Basic.

---

### `get_bigquery_price`

Get BigQuery pricing for storage, on-demand queries, and streaming inserts.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `region` | string | | Location — multi-region `"us"` (default), `"eu"`, or a single region e.g. `"us-central1"` |
| `query_tb` | float | | TiB of data scanned by on-demand queries per month |
| `active_storage_gb` | float | | GB of active table storage (modified in last 90 days) |
| `longterm_storage_gb` | float | | GB of long-term table storage (unmodified 90+ days, ~50% cheaper) |
| `streaming_gb` | float | | GB of data inserted via streaming API per month |

First 1 TiB of queries and 10 GiB of storage are free each month.

---

### `get_vertex_price`

Get Vertex AI custom training or prediction compute pricing.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `machine_type` | string | ✓ | GCP machine type, e.g. `"n1-standard-4"`, `"n1-highmem-8"`, `"a2-highgpu-1g"` |
| `region` | string | | GCP region (default `"us-central1"`) |
| `hours` | float | | Hours of compute time (default `730`) |
| `task` | string | | `"training"` (default) or `"prediction"` |

Returns per-vCPU-hour and per-GiB-RAM-hour rates for the machine family.

---

### `get_gemini_price`

Get Gemini generative model token pricing via the Vertex AI catalog.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `model` | string | | Model name substring: `"gemini-1.5-flash"` (default), `"gemini-1.5-pro"`, `"gemini-1.0-pro"` |
| `region` | string | | GCP region (default `"us-central1"`) |

Returns input and output token rates (per 1,000 tokens). Most Gemini SKUs are global so region has limited effect on price.

---

### `get_cloud_lb_price`

Get GCP Cloud Load Balancing pricing for forwarding rules and data processed.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `region` | string | | GCP region (default `"us-central1"`) |
| `lb_type` | string | | `"https"` (External HTTP(S), default), `"tcp"` (TCP Proxy), `"ssl"` (SSL Proxy), `"network"` (L4), `"internal"` |
| `rule_count` | int | | Number of forwarding rules (default `1`) |
| `data_gb` | float | | GB of data processed per month (default `0`) |
| `hours_per_month` | float | | Hours active (default `730`) |

Forwarding rules are billed per hour. Data processed (ingress + egress through LB) billed per GB.

---

### `get_cloud_cdn_price`

Get GCP Cloud CDN pricing for cache egress and cache fill.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `region` | string | | GCP region for CDN PoP (default `"us-central1"`) |
| `egress_gb` | float | | GB served from CDN to end users per month (default `0`) |
| `cache_fill_gb` | float | | GB pulled from origin into CDN cache per month (default `0`) |

Egress rates vary by destination (North America / EU cheapest; APAC / LATAM / ME / Africa higher). First 10 GiB/month egress is free.

---

### `get_cloud_nat_price`

Get GCP Cloud NAT pricing for gateway uptime and data processing.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `region` | string | | GCP region (default `"us-central1"`) |
| `gateway_count` | int | | Number of Cloud NAT gateways (default `1`) |
| `data_gb` | float | | GB of data processed through NAT per month (default `0`) |
| `hours_per_month` | float | | Hours active (default `730`) |

Gateway uptime charged per hour regardless of traffic. Data processing charged per GB (both directions). Intra-region traffic is not charged.

---

### `get_cloud_armor_price`

Get GCP Cloud Armor Standard pricing for security policies and request evaluation.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `policy_count` | int | | Number of enforced security policies (default `1`) |
| `monthly_requests_millions` | float | | Millions of requests evaluated per month (default `0`) |

Standard tier: ~$0.75/policy/month + ~$0.75/million requests evaluated. Cloud Armor Enterprise ($3,000/month) is a separate SKU not returned by this tool.

---

### `get_cloud_monitoring_price`

Get GCP Cloud Monitoring pricing for custom and external metric ingestion.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `ingestion_mib` | float | | MiB of custom/external metrics ingested per month (default `0`) |

First 150 MiB/month per billing account is free. Tiered pricing above that: ~$0.258/MiB up to 100,000 MiB, then lower rates for higher volumes.

---

## Effective & Discount Pricing

### `get_effective_price`

Get effective rate after account discounts (Reserved Instances, Savings Plans, CUDs, EDPs).

**Requires:** AWS credentials + `OCC_AWS_ENABLE_COST_EXPLORER=true` (costs $0.01/call)

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"` or `"gcp"` |
| `service` | string | ✓ | `"compute"`, `"storage"`, `"database"` |
| `instance_type` | string | ✓ | e.g. `"m5.xlarge"` |
| `region` | string | ✓ | Region code |

---

### `get_discount_summary`

List all active Savings Plans and Reserved Instances with utilization metrics.

**Requires:** AWS credentials + `OCC_AWS_ENABLE_COST_EXPLORER=true`

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | | `"aws"` (default) |

---

## Discovery

### `list_services`

List all AWS services that have pricing data (260+ services). Returns service codes and short aliases for use with `get_service_price`.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | | `"aws"` (default) |

---

### `list_regions`

List all regions for a provider/service with friendly display names.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"`, `"gcp"`, or `"azure"` |
| `service` | string | | `"compute"` (default), `"storage"`, `"database"` |

**Example response:**
```json
{
  "regions": [
    {"code": "us-east-1", "name": "US East (N. Virginia)"},
    {"code": "eu-west-1", "name": "Europe (Ireland)"}
  ]
}
```

---

### `list_instance_types`

List available compute instance types in a region with optional filters.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"`, `"gcp"`, or `"azure"` |
| `region` | string | ✓ | Region code |
| `family` | string | | Instance family prefix, e.g. `"m5"`, `"c6g"`, `"n2"`, `"Standard_D"` |
| `min_vcpu` | int | | Minimum vCPU count. When set alongside `min_memory_gb`, auto-expands the result cap to 200 so spec-filtered queries don't silently truncate. |
| `min_memory_gb` | float | | Minimum memory in GB |
| `gpu` | bool | | If `true`, only return GPU instances |
| `max_results` | int | | Max results (default `50`). Auto-expanded to 200 when `min_vcpu` or `min_memory_gb` is set and the given value is less than 200. |

---

### `check_availability`

Check if a specific instance type or storage SKU is available in a region.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"`, `"gcp"`, or `"azure"` |
| `service` | string | ✓ | `"compute"` or `"storage"` |
| `sku_or_type` | string | ✓ | e.g. `"c6a.xlarge"`, `"gp3"`, `"Standard_D4s_v3"` |
| `region` | string | ✓ | Region code |

---

## Region Analysis

### `find_cheapest_region`

Find the cheapest region for an instance type across all (or specified) regions. Queries concurrently, returns sorted cheapest first.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"`, `"gcp"`, or `"azure"` |
| `instance_type` | string | ✓ | e.g. `"m5.xlarge"` |
| `os` | string | | `"Linux"` (default) or `"Windows"` |
| `term` | string | | `"on_demand"` (default), `"reserved_1yr"`, `"reserved_1yr_partial"`, `"reserved_1yr_all"`, `"reserved_3yr"`, `"reserved_3yr_partial"`, `"reserved_3yr_all"` |
| `regions` | list[string] | | Regions to compare. Omit to use 12 major regions for AWS or GCP (faster on first run). Pass `["all"]` for exhaustive search across all available regions (slower without cache). |
| `baseline_region` | string | | Region for delta comparison, e.g. `"us-east-1"` |

---

### `find_available_regions`

Find every region where an instance type is available, with prices, region names, and optional baseline deltas. Designed to answer "where can I run X and what does it cost?" in a single call.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | ✓ | `"aws"`, `"gcp"`, or `"azure"` |
| `instance_type` | string | ✓ | e.g. `"c6a.xlarge"`, `"n2-standard-4"` |
| `os` | string | | `"Linux"` (default) or `"Windows"` |
| `term` | string | | `"on_demand"` (default), `"reserved_1yr"`, `"spot"` |
| `include_prices` | bool | | Include per-hour price (default `true`) |
| `regions` | list[string] | | Regions to search. Omit to use 12 major regions for AWS or GCP (faster on first run). Pass `["all"]` for exhaustive search across all available regions (slower without cache). |
| `baseline_region` | string | | Region for delta comparison, e.g. `"us-east-1"` |

**Example — find all regions for c6a.xlarge with us-east-1 deltas:**
```
find_available_regions(provider="aws", instance_type="c6a.xlarge",
                       regions=["all"], baseline_region="us-east-1")
```

---

## Cost Estimation

### `estimate_bom`

Calculate Total Cost of Ownership for a Bill of Materials.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `items` | list[object] | ✓ | List of BoM line items |

**BoM line item fields:**

| Field | Default | Description |
|-------|---------|-------------|
| `provider` | `"aws"` | Cloud provider |
| `service` | | `"compute"` or `"storage"` |
| `type` | | Instance/storage type, e.g. `"m5.xlarge"`, `"gp3"` |
| `region` | `"us-east-1"` | Region code |
| `quantity` | `1` | Number of units |
| `hours_per_month` | `730` | Hours/month (730 = always-on) |
| `term` | `"on_demand"` | Pricing term |
| `description` | | Human-readable label |
| `size_gb` | `100` | For storage items |
| `os` | `"Linux"` | Operating system — `"Linux"` (default) or `"Windows"` |

**Example response:**
```json
{
  "totals": {"monthly": "$1196.32", "annual": "$14355.84"},
  "line_items": [
    {"description": "API servers", "quantity": 3, "monthly_cost": "$420.48"},
    {"description": "DB servers",  "quantity": 2, "monthly_cost": "$735.84"},
    {"description": "EBS storage", "quantity": 1, "monthly_cost": "$40.00"}
  ]
}
```

---

### `estimate_unit_economics`

Estimate cost per user, request, or transaction given a BoM and monthly volume.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `items` | list[object] | ✓ | Same format as `estimate_bom` |
| `units_per_month` | float | ✓ | Monthly volume, e.g. `50000` |
| `unit_label` | string | | `"user"`, `"request"`, `"transaction"` (default `"user"`) |

---

## Cache Management

### `refresh_cache`

Invalidate the pricing cache to force fresh data on next request.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `provider` | string | | Provider to clear (`"aws"`, `"gcp"`). Empty = purge all expired entries. |

### `cache_stats`

Return cache statistics: entry counts and SQLite DB size.

Returns: `{"price_entries": 142, "metadata_entries": 8, "db_size_mb": 0.45}`
