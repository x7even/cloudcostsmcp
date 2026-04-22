# OpenCloudCosts — Strategic Review (post-v0.8.0)

_Written April 2026 after the v0.8.0 tool-consolidation release._

---

## Current State

v0.8.0 shipped a unified 15-tool MCP surface across AWS, GCP, and Azure — down from 31
provider-coupled tools. Key achievements:

- **Unified `get_price(spec)`** dispatcher with a discriminated-union `PricingSpec` replacing 21 domain-specific tools
- **`describe_catalog`** discovery with copy-paste `example_invocation` per (provider, domain, service)
- **Capability-based provider protocol** (`supports()`, `get_price()`, `describe_catalog()`) — zero provider-branching in the tool layer
- **123/123 harness pass rate** (100%) on a 123-prompt test suite across three clouds

The product surface is in good shape. The engineering foundation and OSS hygiene need attention before pushing adoption.

---

## Findings

### Theme 1 — Engineering Foundation

**Critical: contract tests are skipped.** `tests/test_providers/test_provider_contract.py:222-250`
contains the 5 correctness invariants for v0.8.0's unified dispatcher, all marked
`@pytest.mark.skip(reason="Activates on v0.8.0-prep after provider migration")`. v0.8.0 shipped
with its safety net disabled.

**No test CI.** `.github/workflows/workflow.yml` only publishes to PyPI on tag. Tests,
linting, and type-checking are not run on push or PR — changes can break undetected.

**Cache wipes on every startup.** `server.py:35` calls `await cache.clear_all()` with comment
"Always start fresh — avoids stale entries across code changes." This defeats the SQLite cache
for every Docker restart, Claude Desktop restart, or `uv run opencloudcosts` invocation.
Replace with a schema-version check that only purges when the DB schema changes.

**No retry or backoff on upstream HTTP.** `aws.py:319`, `gcp.py:204`, and `azure.py:125` are
bare `httpx.get` calls. A single transient 5xx or 429 from AWS Pricing / GCP Cloud Billing /
Azure Retail Prices raises an exception that propagates as `{"error": "API error: <raw exception>"}` to the LLM.

**Raw exception text leaks to LLM.** `tools/lookup.py:121` returns `{"error": f"API error: {e}"}`.
For AWS boto3 errors this can include stack frames and IAM policy details. Should be a structured
envelope with a safe message; log the trace server-side.

**String-typed money at every tool boundary.** Every tool serialises prices as `"$0.192000"`-style
strings. `get_prices_batch` even sorts results lexically on these strings (`lookup.py:180`).
This works for well-formed positive prices but is brittle and forces the LLM to re-parse values
before arithmetic.

**Provider-specific branches in the tool layer.** `tools/bom.py:136` hard-codes
`if li.provider.value == "aws":` for `not_included` hints. `tools/lookup.py:412` accesses
`pvdr._get_spot_history(...)` — a private method from the tool layer, bypassing the
public protocol surface. Both will rot as providers gain parity.

**Dual dev-dependency blocks with conflicting pytest versions.** `pyproject.toml` has both
`[project.optional-dependencies].dev` (pytest ≥ 8.0) and `[dependency-groups].dev`
(pytest ≥ 9.0.3). The `gcp` extra declares `google-cloud-billing` which the GCP provider
never imports (it uses raw `httpx`).

### Theme 2 — Product Coverage

Coverage across providers is significantly uneven:

| Provider | Capability cells | Notable gaps |
|----------|-----------------|--------------|
| AWS | 21 | DynamoDB, Aurora, API Gateway, OpenSearch, Step Functions, MSK, Trainium/Inferentia |
| GCP | 18 | Cloud Run, Cloud Functions, Spanner, Bigtable, Firestore, Pub/Sub, Dataflow, TPU SKUs |
| **Azure** | **3** | Azure SQL, Cosmos DB, AKS, Functions, App Service, Azure OpenAI, Monitor, CDN/Front Door |

Azure's 3-cell coverage (compute/vm, storage/managed_disks, storage/blob) makes three-way
cost comparisons misleading for any service beyond basic VMs. If the project markets
tri-cloud parity, this is the largest product truth gap.

**Missing pricing models across the board:**
- Inter-region egress is not a first-class domain on any cloud — the biggest "hidden cost" miss in real architecture estimates
- No free-tier awareness (BigQuery 150 MiB partial; no AWS/Azure free-tier model)
- Tiered volume discounts (S3, BigQuery query first-1TB, CloudFront) not modelled
- EDPs / Private Pricing Agreements unsupported
- All pricing hardcoded to USD (`models.py:91,214`) — no FX conversion, no tax/GST/VAT caveat

### Theme 3 — Trust Signals

Every `NormalizedPrice` is missing metadata that an agent needs to cite a source:

- No `as_of` timestamp — LLM cannot say "as of <date>"
- No `source_url` — no link to the vendor pricing page for verification
- No `cache_age_seconds` — LLM can't warn when data is stale
- No confidence score for search-fallback results

Every provider test mocks the HTTP layer (`_get_products`, `_fetch_skus`, etc.), meaning a
silent upstream schema change at AWS / GCP / Azure would pass every unit test. No recorded
real-response fixtures exist to catch drift.

### Theme 4 — OSS Hygiene

**No CHANGELOG.** v0.8.0 was a breaking-change release (31 → 15 tools) with no migration
documentation. Users upgrading from v0.7.x have no written record of what changed.

**No community health files.** Missing: `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`,
`SECURITY.md`, `.github/ISSUE_TEMPLATE/`, `.github/PULL_REQUEST_TEMPLATE.md`.
GitHub does not show community-health checkmarks; first-time contributors have no guidance.

**README install path is wrong.** The README only shows `git clone` despite the project
being on PyPI. No `pip install opencloudcosts` or `uvx opencloudcosts` path is documented.
The clone URL also has a typo: `cloudcostsmcp` should be `cloudcostmcp`.

**No Docker image in a registry.** A `Dockerfile` exists but no workflow publishes it to
GHCR or Docker Hub. Users must `docker build` locally.

**Single badge on README.** Only the MIT license badge is present. Missing: PyPI version,
Python versions, CI status, Docker pulls.

**No examples directory.** MCP client snippets only cover Claude Code; no Cursor,
Continue.dev, Cline, Windsurf, or Zed examples.

**No comparison or positioning section.** The README does not answer "why this vs Infracost,
AWS Pricing Calculator, Vantage, or cloud-native cost tools?".

---

## Three-Horizon Roadmap

### Horizon 1 — v0.8.1: Stabilize the Foundation (1–2 weeks)

Ten items that reduce operational risk and lower the first-contributor bar.
No behaviour changes to the pricing surface; safe to ship as a patch release.

| # | Item | Files |
|---|------|-------|
| 1 | Un-skip provider contract tests | `tests/test_providers/test_provider_contract.py` |
| 2 | Add test + lint CI workflow | `.github/workflows/tests.yml` (new) |
| 3 | Fix startup cache wipe | `server.py:35`, `cache.py` |
| 4 | Retry/backoff on upstream HTTP | `aws.py:319`, `gcp.py:204`, `azure.py:125` |
| 5 | Structured error envelope | `tools/lookup.py:121` |
| 6 | README typo + PyPI/uvx quickstart | `README.md` |
| 7 | `CHANGELOG.md` with v0.8.0 migration note | `CHANGELOG.md` (new) |
| 8 | Community health files | `CONTRIBUTING.md`, `SECURITY.md`, `.github/ISSUE_TEMPLATE/`, `.github/PULL_REQUEST_TEMPLATE.md` |
| 9 | Publish Docker image to GHCR | `.github/workflows/docker.yml` (new) |
| 10 | Repo cleanup | `pyproject.toml`, `.env.example`, stale branches |

### Horizon 2 — v0.9: Coverage & Trust (1–3 months)

| Priority | Item |
|----------|------|
| 1 | Azure service expansion: SQL, Cosmos DB, AKS, Functions, App Service, Azure OpenAI, Monitor |
| 2 | Trust metadata on `NormalizedPrice`: `as_of`, `source_url`, `cache_age_seconds` |
| 3 | Inter-region egress as a first-class domain (all three clouds) |
| 4 | Numeric price fields alongside formatted strings in tool responses |
| 5 | Provider interface cleanup: `suggest_unbilled()` replaces `bom.py` hard-coded AWS branch; `spot_history()` on public surface replaces private method access |
| 6 | Multi-currency + explicit pre-tax caveat |
| 7 | Examples directory with MCP client snippets for Cursor, Continue.dev, Cline, Windsurf, Zed |
| 8 | Recording-mode tests: capture real provider responses under `tests/fixtures/`; replay in CI |
| 9 | README comparison section vs Infracost, AWS Pricing Calculator, Vantage |
| 10 | Structured/JSON logging + OpenTelemetry-compatible hooks |

### Horizon 3 — v0.10+: Positioning Choice (quarterly)

These represent a fork in ambition. Both paths build on H1 + H2.

**Path A — Pricing Oracle (deepen, don't widen)**
- IaC-aware pricing: parse Terraform plan / CloudFormation / Bicep → BoM as a first-class tool. This is Infracost's moat; owning an MCP-native version is high leverage in every CI/CD pipeline.
- Deterministic replay: `describe_catalog(p, d, s).example_invocation` guaranteed to return exactly the documented price for a test SKU. Enables embedding in automated quality checks.
- Complete remaining AWS/GCP service cells (DynamoDB, Aurora, Cloud Run, Spanner, etc.).
- *Result*: the most accurate, agent-friendly pricing catalog at the MCP layer.

**Path B — FinOps Suite (expand scope)**
- Real billing ingestion: AWS CUR, GCP billing export, Azure Cost Management. Answers "what did I spend last month?" — the single most common FinOps question.
- Commitment/rightsizing recommendations: "you'd save $X by moving to 1yr partial upfront".
- Anomaly detection on time-series spend.
- *Result*: directly competes with Vantage/Cloudability at the MCP layer; much larger addressable market, much larger scope.

**Recommended sequencing**: stake the pricing-oracle claim (Path A) first — it's completable by one maintainer in 2–4 quarters. Path B requires billing-data access that adds credential complexity and operational burden; open that as a community contribution surface once H1 + H2 are stable.

---

## Key File Index

| File | Concern |
|------|---------|
| `src/opencloudcosts/server.py:35` | Cache wipe on startup |
| `src/opencloudcosts/cache.py` | Schema-version gate needed |
| `src/opencloudcosts/providers/aws.py:319` | Unretried HTTP (bulk pricing) |
| `src/opencloudcosts/providers/gcp.py:204` | Unretried HTTP (catalog API) |
| `src/opencloudcosts/providers/azure.py:125` | Unretried HTTP (retail prices) |
| `src/opencloudcosts/tools/lookup.py:121` | Raw exception leak to LLM |
| `src/opencloudcosts/tools/lookup.py:412` | Private method access from tool layer |
| `src/opencloudcosts/tools/bom.py:136` | Provider branch in tool layer |
| `src/opencloudcosts/models.py:91,214` | Hardcoded USD currency |
| `src/opencloudcosts/providers/azure.py:89-95` | Azure 3-cell capability matrix |
| `tests/test_providers/test_provider_contract.py:222-250` | Skipped correctness invariants |
| `pyproject.toml:38,75` | Dual dev-dep blocks, conflicting pytest versions |
| `.github/workflows/workflow.yml` | Only release CI; no test gate |
| `README.md` | Clone typo, no PyPI quickstart |
| `.env.example` | Stale "CloudCost" name, wrong cache path |

---

## Verification Criteria

### H1 (v0.8.1) complete when:
- Every PR triggers CI: `pytest` + `ruff check` + `mypy src/` pass on Python 3.11/3.12/3.13
- `pytest tests/test_providers/test_provider_contract.py` — zero skipped tests
- `pip install opencloudcosts && opencloudcosts --help` works from a fresh venv
- `docker pull ghcr.io/x7even/opencloudcosts:v0.8.1` succeeds
- Server survives one transient HTTP 429 mid-conversation without surfacing an error to the LLM
- `CHANGELOG.md` on `main` with v0.8.1 and v0.8.0 entries; GitHub Release linked

### H2 (v0.9) complete when:
- `describe_catalog(provider="azure")` returns ≥ 12 capability cells
- Every `NormalizedPrice.summary()` includes `as_of` (ISO timestamp) and `source_url`
- `get_prices_batch` results sort numerically, not lexically
- `examples/` directory contains ≥ 5 MCP client configs (Cursor, Continue.dev, Cline, Windsurf, Zed)
- `uv run pytest --cov=opencloudcosts --cov-report=term-missing` ≥ 70% line coverage

### H3 (v0.10) complete when (Path A):
- New `price_iac(plan_path)` tool parses `terraform show -json` output → calls `estimate_bom` → returns BoM with source line references
- A new harness prompt suite for IaC pricing passes ≥ 95%
