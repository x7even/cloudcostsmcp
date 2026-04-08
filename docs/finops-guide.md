# OpenCloudCosts — FinOps Usage Guide

Practical examples of how an AI assistant uses the OpenCloudCosts server for common FinOps activities.

---

## 1. Regional Price Comparison

**Use case:** "What's the price delta for an m5.xlarge between us-east-1 and ap-southeast-2?"

The AI calls `compare_compute_prices` with both regions:

```
compare_compute_prices(
  provider="aws",
  instance_type="m5.xlarge",
  regions=["us-east-1", "ap-southeast-2"]
)
```

Response includes: prices sorted cheapest first, % delta, monthly estimates.

**Typical output:**
> "An m5.xlarge in us-east-1 costs $0.192/hr ($140.16/mo). In Sydney (ap-southeast-2) it costs $0.278/hr ($202.94/mo) — 44.8% more expensive. Over a year that's $755 additional per instance."

---

## 2. Finding the Cheapest Region

**Use case:** "Where should I deploy to minimise compute costs for m5.xlarge?"

```
find_cheapest_region(
  provider="aws",
  instance_type="m5.xlarge"
)
```

Returns all regions sorted cheapest first. The AI can then factor in latency requirements, data residency, and other constraints.

---

## 3. TCO Estimation from a Bill of Materials

**Use case:** "What's the monthly cost for this architecture: 3 API servers (m5.xlarge), 2 DB servers (r5.2xlarge), and 500GB EBS (gp3) in us-east-1?"

```
estimate_bom(items=[
  {"provider": "aws", "service": "compute", "type": "m5.xlarge",
   "region": "us-east-1", "quantity": 3, "description": "API servers"},
  {"provider": "aws", "service": "compute", "type": "r5.2xlarge",
   "region": "us-east-1", "quantity": 2, "description": "DB servers"},
  {"provider": "aws", "service": "storage", "type": "gp3",
   "region": "us-east-1", "quantity": 1, "size_gb": 500, "description": "EBS"}
])
```

**Typical output:**
> "Total monthly: $1,196.32 / Annual: $14,355.84 (on-demand, Linux)"

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

Call `get_compute_price` twice — once for on-demand, once for reserved:

```
get_compute_price(provider="aws", instance_type="m5.xlarge",
                  region="us-east-1", term="on_demand")

get_compute_price(provider="aws", instance_type="m5.xlarge",
                  region="us-east-1", term="reserved_1yr")
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

---

## 7. Architecture Design Costing

**Use case:** During system design, evaluate cost of different architecture options.

**Option A: Monolith**
```
estimate_bom(items=[
  {"provider": "aws", "service": "compute", "type": "m5.4xlarge",
   "region": "us-east-1", "quantity": 2}
])
```

**Option B: Microservices**
```
estimate_bom(items=[
  {"provider": "aws", "service": "compute", "type": "m5.xlarge",
   "region": "us-east-1", "quantity": 6},
  {"provider": "aws", "service": "compute", "type": "t3.medium",
   "region": "us-east-1", "quantity": 4}
])
```

The AI can compare both and recommend based on cost, scalability, and operational overhead.

---

## 8. Multi-Region Deployment Costing

**Use case:** "What's the cost delta for active-active deployment in us-east-1 + ap-southeast-2 vs single region?"

```
# US pricing
estimate_bom(items=[
  {"provider": "aws", "service": "compute", "type": "m5.xlarge",
   "region": "us-east-1", "quantity": 3}
])

# AU pricing
estimate_bom(items=[
  {"provider": "aws", "service": "compute", "type": "m5.xlarge",
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
  min_vcpus=8,
  min_memory_gb=32
)
```

Returns matching instances sorted by size. Then call `get_compute_price` on candidates to compare.

---

## 10. Pre-Commit Architecture Review

Before committing to an architecture, use CloudCost to validate assumptions:

1. `list_instance_types` — confirm chosen instances are available in target regions
2. `check_availability` — verify specific SKUs exist before scripting deployment
3. `get_compute_price` with `term="reserved_1yr"` — model committed spend
4. `estimate_bom` — get full TCO including storage and supporting services
5. `estimate_unit_economics` — validate business unit economics at target scale
6. `get_discount_summary` — check if existing RIs/SPs can absorb the new workload

---

---

## 11. AWS vs GCP Cross-Cloud Comparison

**Use case:** "Compare AWS m5.xlarge vs GCP n2-standard-4 (both 4 vCPU / 16 GB) in US regions."

```
get_compute_price(provider="aws", instance_type="m5.xlarge", region="us-east-1")
get_compute_price(provider="gcp", instance_type="n2-standard-4", region="us-east1")
```

GCP n2-standard-4 US on-demand ~$0.19/hr vs AWS m5.xlarge ~$0.192/hr — roughly equivalent.

But GCP offers 3-year CUDs (~55% off):
```
get_compute_price(provider="gcp", instance_type="n2-standard-4",
                  region="us-east1", term="cud_3yr")
```
GCP 3yr CUD ~$0.086/hr vs AWS 3yr reserved ~$0.091/hr — GCP typically edges out AWS on committed pricing for general-purpose workloads.

---

## 12. GCP Region Pricing Variation

**Use case:** "Where is the cheapest GCP region for n2-standard-8?"

```
find_cheapest_region(provider="gcp", instance_type="n2-standard-8")
```

Returns all GCP regions sorted by price. US regions (us-central1, us-east1) are typically cheapest; APAC and South America command a premium.

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
        "ec2:DescribeReservedInstances"
      ],
      "Resource": "*"
    }
  ]
}
```
