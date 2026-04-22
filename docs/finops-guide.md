# OpenCloudCosts — FinOps Usage Guide

Practical examples of how an AI assistant uses the OpenCloudCosts server for common FinOps activities.

All pricing calls go through `get_price(spec={...})`. Use `describe_catalog(provider, domain)` to discover exact field names and copy-paste `example_invocation` values.

---

## 1. Regional Price Comparison

**Use case:** "What's the price delta for an m5.xlarge between us-east-1 and ap-southeast-2?"

```
compare_prices(
  spec={"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge", "region": "us-east-1"},
  regions=["us-east-1", "ap-southeast-2"],
  baseline_region="us-east-1"
)
```

Response includes prices sorted cheapest first, % delta, and monthly estimates.

**Typical output:**
> "An m5.xlarge in us-east-1 costs $0.192/hr ($140.16/mo). In Sydney (ap-southeast-2) it costs $0.278/hr ($202.94/mo) — 44.8% more expensive. Over a year that's $755 additional per instance."

---

## 2. Finding the Cheapest Region

**Use case:** "Where should I deploy to minimise compute costs for m5.xlarge?"

```
find_cheapest_region(
  spec={"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge", "region": "us-east-1"}
)
```

Returns all major regions sorted cheapest first. Pass `regions=["all"]` for an exhaustive search across every available region.

---

## 3. TCO Estimation from a Bill of Materials

**Use case:** "What's the monthly cost for this architecture: 3 API servers (m5.xlarge), 2 DB servers (r5.2xlarge), and 500GB EBS (gp3) in us-east-1?"

```
estimate_bom(items=[
  {"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge",
   "region": "us-east-1", "quantity": 3, "description": "API servers"},
  {"provider": "aws", "domain": "compute", "resource_type": "r5.2xlarge",
   "region": "us-east-1", "quantity": 2, "description": "DB servers"},
  {"provider": "aws", "domain": "storage", "storage_type": "gp3",
   "size_gb": 500, "region": "us-east-1", "description": "EBS volume"}
])
```

**Typical output:**
> "Total monthly: $1,196.32 / Annual: $14,355.84 (on-demand, Linux)"

The response also includes a `not_included` list of hidden costs (egress, load balancers, monitoring) with ready-to-use `get_price` calls for each.

---

## 4. Unit Economics

**Use case:** "If I build this for 50,000 MAUs, what's my cost per user?"

```
estimate_unit_economics(
  items=[...same BoM as above...],
  units_per_month=50000,
  unit_label="user"
)
```

**Typical output:**
> "At 50,000 MAUs, infrastructure costs $1,196.32/mo = **$0.024 per user per month** ($0.29/user/year)."

For margin analysis: if you charge $5/user/month, infrastructure is ~0.5% of revenue.

---

## 5. Reserved Instance vs On-Demand Comparison

**Use case:** "How much would I save moving these 3 API servers to 1-year reserved?"

Call `get_price` twice — once for on-demand, once for reserved:

```
get_price(spec={"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge",
                "region": "us-east-1", "term": "on_demand"})

get_price(spec={"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge",
                "region": "us-east-1", "term": "reserved_1yr"})
```

Then compute the delta: on-demand ~$140.16/mo vs 1yr reserved ~$91/mo = ~35% savings = **$588/yr per instance**, or $1,764/yr across 3 instances.

---

## 6. Analysing Existing Discounts

**Use case:** "What discounts do we currently have and are we using them efficiently?"

```
get_discount_summary(provider="aws")
```

Returns:
- All active Savings Plans: type, commitment $/hr, utilization %
- All active Reserved Instances: instance type, count, days remaining
- Cost Explorer utilization metrics for last month (unrealized savings, coverage %)

**AI interpretation example:**
> "You have 2 active Compute Savings Plans with $1.50/hr commitment, running at 92% utilization (efficient). You also have 8 active RIs across m5.xlarge and c5.xlarge, but RI utilization is only 73% — 3 RIs appear underused and may indicate over-provisioned reservations. Unrealized RI savings this month: $55."

Requires AWS credentials + `OCC_AWS_ENABLE_COST_EXPLORER=true`.

---

## 7. Architecture Design Costing

**Use case:** During system design, evaluate cost of different architecture options.

**Option A: Monolith**
```
estimate_bom(items=[
  {"provider": "aws", "domain": "compute", "resource_type": "m5.4xlarge",
   "region": "us-east-1", "quantity": 2}
])
```

**Option B: Microservices**
```
estimate_bom(items=[
  {"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge",
   "region": "us-east-1", "quantity": 6},
  {"provider": "aws", "domain": "compute", "resource_type": "t3.medium",
   "region": "us-east-1", "quantity": 4}
])
```

The AI can compare both and recommend based on cost, scalability, and operational overhead.

---

## 8. Multi-Region Deployment Costing

**Use case:** "What's the cost delta for active-active deployment in us-east-1 + ap-southeast-2 vs single region?"

```
estimate_bom(items=[
  {"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge",
   "region": "us-east-1", "quantity": 3}
])

estimate_bom(items=[
  {"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge",
   "region": "ap-southeast-2", "quantity": 3}
])
```

Sydney m5.xlarge ($0.278/hr) vs Virginia ($0.192/hr) = 44.8% premium for AU capacity. For 3 instances, active-active costs ~$756/mo more than single-region.

---

## 9. Discovering Available Instance Types

**Use case:** "What instances are available in Sydney with at least 8 vCPUs and 32GB RAM?"

```
list_instance_types(
  provider="aws",
  region="ap-southeast-2",
  min_vcpu=8,
  min_memory_gb=32
)
```

Returns matching instances sorted by size. Then call `get_price` on candidates to compare costs.

---

## 10. Pre-Commit Architecture Review

Before committing to an architecture, use OpenCloudCosts to validate assumptions:

1. `list_instance_types` — confirm chosen instances are available in target regions
2. `find_available_regions(spec={...})` — verify a specific SKU exists across planned regions
3. `get_price(spec={..., "term": "reserved_1yr"})` — model committed spend
4. `estimate_bom` — get full TCO including storage and supporting services
5. `estimate_unit_economics` — validate business unit economics at target scale
6. `get_discount_summary` — check if existing RIs/SPs can absorb the new workload

---

## 11. AWS vs GCP Cross-Cloud Comparison

**Use case:** "Compare AWS m5.xlarge vs GCP n2-standard-4 (both 4 vCPU / 16 GB) in US regions."

```
get_price(spec={"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge", "region": "us-east-1"})
get_price(spec={"provider": "gcp", "domain": "compute", "resource_type": "n2-standard-4", "region": "us-central1"})
```

GCP n2-standard-4 US on-demand ~$0.19/hr vs AWS m5.xlarge ~$0.192/hr — roughly equivalent.

But GCP offers 3-year CUDs (~55% off):
```
get_price(spec={"provider": "gcp", "domain": "compute", "resource_type": "n2-standard-4",
                "region": "us-central1", "term": "cud_3yr"})
```
GCP 3yr CUD ~$0.086/hr vs AWS 3yr reserved ~$0.091/hr — GCP typically edges out AWS on committed pricing for general-purpose workloads.

---

## 12. AWS vs GCP vs Azure 3-Way Comparison

**Use case:** "Compare the same 4-vCPU / 16-GB general-purpose instance across all three clouds."

```
get_price(spec={"provider": "aws",   "domain": "compute", "resource_type": "m5.xlarge",       "region": "us-east-1"})
get_price(spec={"provider": "gcp",   "domain": "compute", "resource_type": "n2-standard-4",   "region": "us-central1"})
get_price(spec={"provider": "azure", "domain": "compute", "resource_type": "Standard_D4s_v3", "region": "eastus"})
```

All three are approximately 4 vCPU / 16 GB general-purpose instances. Compare on-demand hourly rates and reserved/committed-use rates:

```
# 1-year reserved / committed
get_price(spec={"provider": "aws",   "domain": "compute", "resource_type": "m5.xlarge",       "region": "us-east-1",   "term": "reserved_1yr"})
get_price(spec={"provider": "gcp",   "domain": "compute", "resource_type": "n2-standard-4",   "region": "us-central1", "term": "cud_1yr"})
get_price(spec={"provider": "azure", "domain": "compute", "resource_type": "Standard_D4s_v3", "region": "eastus",      "term": "reserved_1yr"})
```

**Typical output:**
> "On-demand: AWS $0.192/hr, GCP ~$0.190/hr, Azure $0.192/hr — all roughly equivalent.
> 1-year reserved: AWS ~$0.118/hr (38% off), GCP ~$0.128/hr (33% off), Azure ~$0.116/hr (40% off).
> Azure 1yr reserved edges out slightly at list pricing; GCP 3yr CUDs often win at maximum commitment."

Azure requires no credentials — pricing is fetched from the public Azure Retail Prices API.

---

## 13. GCP Region Pricing Variation

**Use case:** "Where is the cheapest GCP region for n2-standard-8?"

```
find_cheapest_region(
  spec={"provider": "gcp", "domain": "compute", "resource_type": "n2-standard-8", "region": "us-central1"}
)
```

Returns all GCP major regions sorted by price. US regions (us-central1, us-east1) are typically cheapest; APAC and South America command a premium.

---

## 14. Non-Compute Service Pricing

### Database Pricing

**Use case:** "What's the on-demand cost for a MySQL db.r5.large in us-east-1 Single-AZ?"

```
get_price(spec={
  "provider": "aws", "domain": "database", "resource_type": "db.r5.large",
  "engine": "MySQL", "deployment": "single-az", "region": "us-east-1"
})
```

To compare Multi-AZ:
```
get_price(spec={
  "provider": "aws", "domain": "database", "resource_type": "db.r5.large",
  "engine": "MySQL", "deployment": "multi-az", "region": "us-east-1"
})
```

Multi-AZ doubles the instance cost but provides automatic failover — worth factoring into production architecture TCO.

### Serverless Pricing

**Use case:** "What does Lambda cost for 10M requests and 500GB-seconds/month?"

```
get_price(spec={
  "provider": "aws", "domain": "serverless", "service": "lambda",
  "region": "us-east-1", "requests": 10000000, "duration_gb_seconds": 500
})
```

Lambda's first 1M requests/month and 400,000 GB-seconds/month are free. For serverless architecture TCO, factor both duration and request counts.

### AI / LLM Token Pricing

**Use case:** "How much does Bedrock Claude Sonnet 3.5 cost for 50M input + 10M output tokens?"

```
get_price(spec={
  "provider": "aws", "domain": "ai", "service": "bedrock",
  "model": "anthropic.claude-3-5-sonnet-20241022-v2:0",
  "input_tokens": 50000000, "output_tokens": 10000000,
  "region": "us-east-1"
})
```

The response includes per-token rates and a pre-computed workload total so you don't need to apply scientific-notation rates manually.

### Cloud Storage Pricing

**Use case:** "What does 1TB of GCS Standard storage cost in us-central1?"

```
get_price(spec={
  "provider": "gcp", "domain": "storage", "storage_type": "gcs",
  "storage_class": "standard", "size_gb": 1024, "region": "us-central1"
})
```

### Network / CDN Pricing

**Use case:** "What does CloudFront CDN cost for 10TB egress?"

```
get_price(spec={
  "provider": "aws", "domain": "network", "service": "cdn",
  "egress_gb": 10000, "region": "us-east-1"
})
```

For GCP Cloud CDN:
```
get_price(spec={
  "provider": "gcp", "domain": "network", "service": "cloud_cdn",
  "egress_gb": 10000, "region": "us-central1"
})
```

### Load Balancer Pricing

**Use case:** "What does an Application Load Balancer cost in us-east-1?"

```
get_price(spec={
  "provider": "aws", "domain": "network", "service": "lb",
  "lb_type": "application", "region": "us-east-1"
})
```

### Observability / CloudWatch Pricing

**Use case:** "How much does CloudWatch log ingestion cost for 500GB/month?"

```
get_price(spec={
  "provider": "aws", "domain": "observability", "service": "cloudwatch",
  "log_ingestion_gb": 500, "region": "us-east-1"
})
```

### Discovering Catalog Fields

When unsure of valid field names or values for a service, use `describe_catalog` first:

```
describe_catalog(provider="aws", domain="database", service="rds")
```

Returns `required_fields`, `filter_hints`, `supported_terms`, and a working `example_invocation` you can pass directly to `get_price`.

For free-text search across the catalog:
```
search_pricing(provider="aws", query="egress", domain="network")
```

---

## 15. Full Production Stack TCO

**Use case:** "What's the total monthly cost for our production stack?"

Use `estimate_bom` with all components. The response includes a line-item table, totals, and a `not_included` list of common hidden costs:

```
estimate_bom(items=[
  # Compute
  {"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge",    "region": "us-east-1", "quantity": 3, "description": "API servers"},
  {"provider": "aws", "domain": "compute", "resource_type": "m5.2xlarge",   "region": "us-east-1", "quantity": 2, "description": "Worker nodes"},
  # Database
  {"provider": "aws", "domain": "database", "resource_type": "db.r6g.large",
   "engine": "MySQL", "deployment": "multi-az", "region": "us-east-1", "description": "Primary RDS"},
  # Storage
  {"provider": "aws", "domain": "storage", "storage_type": "gp3", "size_gb": 1000,
   "region": "us-east-1", "description": "EBS volumes"},
  # Serverless
  {"provider": "aws", "domain": "serverless", "service": "lambda",
   "requests": 5000000, "region": "us-east-1", "description": "Event processing"}
])
```

---

## Configuration for Different FinOps Scenarios

### Public pricing only (no credentials)
```bash
# No AWS credentials needed — public list prices work out of the box
uv run opencloudcosts
```

### With effective pricing (requires credentials)
```bash
export AWS_PROFILE=finops-readonly
export OCC_AWS_ENABLE_COST_EXPLORER=true
uv run opencloudcosts
```

### GCP with API key (public pricing)
```bash
export OCC_GCP_API_KEY=AIza...
uv run opencloudcosts
```

### GCP with Application Default Credentials
```bash
gcloud auth application-default login
uv run opencloudcosts
```

### IAM policy for full FinOps access
```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "pricing:GetProducts",
        "pricing:DescribeServices",
        "pricing:GetAttributeValues",
        "ce:GetCostAndUsage",
        "ce:GetSavingsPlansUtilization",
        "ce:GetReservationUtilization",
        "savingsplans:DescribeSavingsPlans",
        "savingsplans:DescribeSavingsPlanRates",
        "ec2:DescribeReservedInstances",
        "ec2:DescribeSpotPriceHistory"
      ],
      "Resource": "*"
    }
  ]
}
```
