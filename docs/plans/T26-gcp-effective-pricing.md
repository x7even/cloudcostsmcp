# T26 · GCP effective pricing via BigQuery billing export

**Status:** pending  
**Branch:** task/T26-gcp-effective-pricing

## Overview
Implement `get_effective_price()` and `get_discount_summary()` for GCP using BigQuery billing export. This is the only practical way to get post-discount effective rates on GCP (unlike AWS which has Cost Explorer).

## Prerequisites (user setup)
1. GCP Billing export must be enabled: Console → Billing → Billing export → BigQuery export
2. Standard usage cost export (not detailed resources — standard is sufficient)
3. BigQuery dataset will be something like: `my-project.billing_export.gcp_billing_export_v1_XXXXXX_XXXXXX_XXXXXX`

## New config variable
`src/opencloudcosts/config.py`:
```python
gcp_billing_dataset: str = Field(
    default="",
    description="BigQuery dataset for GCP billing export, e.g. 'project.dataset.table'"
)
```
Env var: `OCC_GCP_BILLING_DATASET`

## Files to change
- `src/opencloudcosts/providers/gcp.py` — implement `get_effective_price()`, `get_discount_summary()`
- `src/opencloudcosts/config.py` — add `gcp_billing_dataset` field
- `pyproject.toml` — add `google-cloud-bigquery` as optional dependency
- `README.md` — add BigQuery export setup instructions to GCP Setup section
- `docs/tools.md` — remove Phase 4 caveat once implemented

## Implementation

### Dependency
```toml
[project.optional-dependencies]
gcp-effective = ["google-cloud-bigquery>=3.0"]
```

Or add to main dependencies if BigQuery client is acceptable as a required dep.

### `get_effective_price()` query
```sql
SELECT
  service.description AS service,
  sku.description AS sku,
  SUM(cost) AS total_cost,
  SUM(usage.amount) AS total_usage,
  SUM(cost) / NULLIF(SUM(usage.amount), 0) AS effective_rate,
  usage.unit AS unit
FROM `{dataset}`
WHERE
  usage_start_time >= TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 30 DAY)
  AND usage_end_time <= CURRENT_TIMESTAMP()
  AND project.id IS NOT NULL
  AND sku.description LIKE '%{instance_family}%'
  AND location.region = '{region}'
GROUP BY 1, 2, 6
ORDER BY total_cost DESC
LIMIT 10
```

Instance family extraction: from `n2-standard-4` → `N2 Instance` (for SKU matching).

### `get_discount_summary()` query
```sql
SELECT
  service.description,
  SUM(cost) AS billed_cost,
  SUM(credits.amount) AS total_credits,
  credit.type AS credit_type
FROM `{dataset}`,
  UNNEST(credits) AS credit
WHERE
  usage_start_time >= TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 30 DAY)
  AND credit.type IN ('COMMITTED_USAGE_DISCOUNT', 'COMMITTED_USAGE_DISCOUNT_DOLLAR_BASE')
GROUP BY 1, 4
```

### Error handling
If `OCC_GCP_BILLING_DATASET` is not set:
```json
{
  "error": "GCP effective pricing requires OCC_GCP_BILLING_DATASET to be set. See README for BigQuery billing export setup."
}
```

If BigQuery credentials not available:
```json
{
  "error": "BigQuery access requires Google credentials. Run 'gcloud auth application-default login' or set GOOGLE_APPLICATION_CREDENTIALS."
}
```

## README additions
Under "GCP Setup", add a new section:

### GCP Effective Pricing (Phase 4)
1. Enable billing export in GCP Console: Billing → Billing export → BigQuery export
2. Note your dataset path (shown after export is enabled)
3. Set the env var:
```bash
export OCC_GCP_BILLING_DATASET="my-project.billing_dataset.gcp_billing_export_v1_..."
```
4. Ensure BigQuery access: `gcloud auth application-default login`

Minimum IAM: `roles/bigquery.dataViewer` on the billing dataset.

## Tests
These will need mocked BigQuery clients.
```python
async def test_get_effective_price_gcp_no_dataset():
    # OCC_GCP_BILLING_DATASET not set
    result = await call_get_effective_price(provider="gcp", ...)
    assert "OCC_GCP_BILLING_DATASET" in result["error"]

async def test_get_effective_price_gcp_with_dataset(mock_bq_client):
    # Mock BigQuery query results
    result = await call_get_effective_price(
        provider="gcp", service="compute",
        instance_type="n2-standard-4", region="us-central1"
    )
    assert "effective_rate" in result or "price" in result
```

## Acceptance criteria
- `get_effective_price(provider="gcp")` returns actual billing-derived rate when dataset configured
- `get_discount_summary(provider="gcp")` returns CUD credit breakdown
- Clear error when dataset not configured
- README has complete setup instructions
