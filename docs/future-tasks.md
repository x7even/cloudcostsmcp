# Future Tasks

Features deferred for later — fully designed, implementation-ready when needed.

---

## T26 · GCP effective pricing via BigQuery billing export

**Why deferred:** Requires the user to have GCP billing export to BigQuery already configured. Unlike AWS Cost Explorer (which works with just IAM credentials), this feature needs upfront infrastructure setup on the GCP side before it can be tested or used.

**What it enables:**
- `get_effective_price(provider="gcp", ...)` — actual post-CUD effective hourly rate from billing data
- `get_discount_summary(provider="gcp")` — CUD credit breakdown by service over the last 30 days

**User prerequisites:**
1. Enable billing export in GCP Console: Billing → Billing export → BigQuery export
2. Note your dataset path (e.g. `my-project.billing_export.gcp_billing_export_v1_...`)
3. Set env var: `OCC_GCP_BILLING_DATASET=my-project.billing_export.gcp_billing_export_v1_...`
4. Ensure BigQuery access: `gcloud auth application-default login` (or service account)
5. Minimum IAM: `roles/bigquery.dataViewer` on the billing dataset

**Implementation plan:** See [`docs/plans/T26-gcp-effective-pricing.md`](plans/T26-gcp-effective-pricing.md) for full design including SQL queries, config changes, error handling, and tests.

**Files to change:**
- `src/opencloudcosts/providers/gcp.py` — implement `get_effective_price()`, `get_discount_summary()`
- `src/opencloudcosts/config.py` — add `gcp_billing_dataset` field
- `pyproject.toml` — add `google-cloud-bigquery` as optional dependency
- `README.md` — BigQuery export setup instructions
- `docs/tools.md` — remove Phase 4 caveat

**Effort:** Medium-large (mocked BigQuery client needed for tests).
