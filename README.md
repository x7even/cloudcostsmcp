# OpenCloudCosts MCP

An open source MCP server that gives AI assistants accurate cloud pricing data for AWS, GCP, and Azure.

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![PyPI version](https://badge.fury.io/py/opencloudcosts.svg)](https://pypi.org/project/opencloudcosts/)
[![Tests](https://github.com/x7even/cloudcostmcp/actions/workflows/tests.yml/badge.svg)](https://github.com/x7even/cloudcostmcp/actions/workflows/tests.yml)
[![Python 3.11+](https://img.shields.io/badge/python-3.11+-blue.svg)](https://www.python.org/downloads/)

Supports both **public list pricing** (no credentials needed for AWS and Azure; GCP requires a free API key) and **effective (custom Private Pricing Agreement) rates** (post-discount: Reserved Instances, Savings Plans, CUDs, EDPs).

## Key Use Cases

- "What is the on-demand price of an m5.xlarge in us-east-1 vs ap-southeast-2, and what's the % delta?"
- "Give me a TCO estimate for this architecture: 3x m5.xlarge + 1x 500GB gp3 EBS in us-east-1"
- "What's the cost per user if I run this stack for 50,000 MAUs?"
- "List all c6g instances in eu-west-1 with >= 8 vCPUs"
- "What's my effective hourly rate on m5.xlarge after Savings Plans?"

## Tools (v0.9.0 — 15 tools)

**Pricing**

| Tool | Description |
|------|-------------|
| `get_price` | Unified pricing dispatcher — compute, storage, database, AI, networking, serverless, analytics, observability |
| `get_prices_batch` | Prices for multiple instance types in one call (concurrent) |
| `compare_prices` | Compare a spec across multiple regions with optional baseline deltas |
| `search_pricing` | Free-text search across the pricing catalog |

**Discovery**

| Tool | Description |
|------|-------------|
| `describe_catalog` | Full support matrix or targeted field guidance + copy-paste `example_invocation` for `get_price` |
| `list_regions` | All regions with friendly names |
| `list_instance_types` | Available instance types with vCPU/memory filters |

**Region Analysis**

| Tool | Description |
|------|-------------|
| `find_cheapest_region` | Cheapest region for any service spec, concurrently |
| `find_available_regions` | Every region where a service is available, sorted by price |
| `get_spot_history` | AWS spot price history and stability analysis (requires credentials) |

**FinOps**

| Tool | Description |
|------|-------------|
| `get_discount_summary` | Account-wide RI / Savings Plan / CUD utilisation (requires credentials) |
| `estimate_bom` | TCO for a multi-resource Bill of Materials |
| `estimate_unit_economics` | Cost per user/request/transaction |

**Cache**

| Tool | Description |
|------|-------------|
| `refresh_cache` | Invalidate pricing cache |
| `cache_stats` | Cache entry counts and DB size |

See [docs/tools.md](docs/tools.md) for full parameter reference and [docs/finops-guide.md](docs/finops-guide.md) for usage examples.

## Setup

### Quick install (recommended)

```bash
# Run directly from PyPI — no clone needed
uvx opencloudcosts

# Or install permanently
pip install opencloudcosts
opencloudcosts
```

### Prerequisites (for development / clone-based install)
- [uv](https://docs.astral.sh/uv/) installed (`curl -LsSf https://astral.sh/uv/install.sh | sh`)
- AWS credentials (optional — public pricing works without them)

### Clone & configure

```bash
git clone https://github.com/x7even/cloudcostmcp opencloudcosts
cd opencloudcosts
cp .env.example .env
# Edit .env if you want effective pricing / Cost Explorer
```

### Test it works

```bash
uv run pytest
```

### Run the server

```bash
uv run opencloudcosts
```

### Run as HTTP server

HTTP transport enables shared/remote deployments — one server, many clients.

```bash
# Localhost only (default)
uv run opencloudcosts --transport http --port 8080

# Bind to all interfaces (e.g. for Docker or remote access)
uv run opencloudcosts --transport http --host 0.0.0.0 --port 8080
```

Environment variable equivalents: `OCC_HTTP_HOST` and `OCC_HTTP_PORT`.

Connect to Claude Code via `.mcp.json`:

```json
{
  "mcpServers": {
    "cloudcost": {
      "transport": "http",
      "url": "http://localhost:8080/mcp/v1"
    }
  }
}
```

### Docker

```bash
# Pull from registry (recommended)
docker pull ghcr.io/x7even/opencloudcosts:latest
docker run -p 8080:8080 \
  -e OCC_GCP_API_KEY=AIza... \
  -v ~/.aws:/root/.aws:ro \
  ghcr.io/x7even/opencloudcosts:latest

# Or build locally
docker build -t opencloudcosts .
docker run -p 8080:8080 opencloudcosts
```

The container starts in HTTP transport mode by default (bound to `0.0.0.0:8080`).
Pass cloud credentials via `-e` flags or mount your AWS credentials directory.

### Connect to Claude Code

Add to your project's `.mcp.json`:

```json
{
  "mcpServers": {
    "cloudcost": {
      "command": "uv",
      "args": ["run", "--directory", "/path/to/opencloudcosts", "opencloudcosts"],
      "env": {
        "AWS_PROFILE": "default",
        "OCC_GCP_API_KEY": "AIza..."
      }
    }
  }
}
```

Or to `~/.claude/settings.json` for global access:

```json
{
  "mcpServers": {
    "cloudcost": {
      "command": "uv",
      "args": ["run", "--directory", "/path/to/opencloudcosts", "opencloudcosts"]
    }
  }
}
```

### Test with MCP Inspector

```bash
npx @modelcontextprotocol/inspector uv run --directory /path/to/opencloudcosts opencloudcosts
```

## AWS Credentials

| Feature | Credentials needed |
|---------|--------------------|
| Public pricing (EC2, EBS, RDS list prices) | None |
| Effective pricing (RI / SP discounts) | AWS credentials + `OCC_AWS_ENABLE_COST_EXPLORER=true` |

Minimal IAM policy for public pricing:
```json
{
  "Effect": "Allow",
  "Action": ["pricing:GetProducts", "pricing:DescribeServices", "pricing:GetAttributeValues"],
  "Resource": "*"
}
```

Add these for effective pricing:
```json
"ce:GetCostAndUsage", "savingsplans:DescribeSavingsPlans", "savingsplans:DescribeSavingsPlanRates"
```

## Configuration

All settings via environment variables (prefix `OCC_`) or `.env` file:

| Variable | Default | Description |
|----------|---------|-------------|
| `OCC_CACHE_TTL_HOURS` | 24 | Public price cache TTL |
| `OCC_AWS_ENABLE_COST_EXPLORER` | false | Enable AWS effective pricing (costs $0.01/call) |
| `OCC_DEFAULT_REGIONS` | us-east-1,us-west-2 | Default regions |
| `AWS_PROFILE` | (default chain) | AWS credentials profile |
| `OCC_GCP_BILLING_ACCOUNT_ID` | (none) | GCP billing account ID for contract/effective pricing |

## Caching

Prices are cached in SQLite at `~/.cache/opencloudcosts/pricing.db`. Public list prices are cached for 24 hours — AWS pricing changes infrequently. Use the `refresh_cache` tool to force a refresh.

## GCP Setup

Unlike AWS (which has public bulk pricing endpoints), GCP's pricing API always requires at least a free API key. No credit card or billing account is needed.

**Option A — Free API key (recommended, 2 min setup):**
1. Go to [console.cloud.google.com/apis/credentials](https://console.cloud.google.com/apis/credentials)
2. Create a Project if you don't have one (free)
3. Click **Create Credentials → API key**
4. Set the key:

```bash
export OCC_GCP_API_KEY=AIza...
```

Or add `OCC_GCP_API_KEY=AIza...` to your `.env` file.

**Option B — Application Default Credentials** (if you already use `gcloud`):
```bash
gcloud auth application-default login
# or set GOOGLE_APPLICATION_CREDENTIALS=/path/to/service-account.json
```

**GCP instance type format:** `{family}-{series}-{vcpus}` e.g. `n2-standard-4`, `e2-highmem-8`, `c2-standard-16`

### GCP Contract / Effective Pricing

If you have a negotiated pricing contract with Google Cloud, you can retrieve your actual discounted rates (EDP, custom pricing) via the Cloud Billing Pricing API v1beta. This requires:

1. ADC credentials: `gcloud auth application-default login`
2. `billing.billingAccountPrice.get` IAM permission on your billing account
3. Your billing account ID:

```bash
export OCC_GCP_BILLING_ACCOUNT_ID=012345-567890-ABCDEF
```

With this configured, `get_price` responses for GCP compute will include an `effective_price` block showing your contract rate and discount percentage. Without it, public list prices are returned unchanged.

## Azure Setup

Azure pricing is fully public — no credentials, API key, or subscription needed.

```bash
# No configuration needed — works out of the box
uv run opencloudcosts
```

**Azure instance type format:** ARM SKU names e.g. `Standard_D4s_v3`, `Standard_E8s_v3`, `Standard_B2ms`

**Azure pricing terms:** `on_demand` (default), `reserved_1yr`, `reserved_3yr`, `spot`

**Azure regions:** ARM region names e.g. `eastus`, `westeurope`, `southeastasia` (use `list_regions` for full list)

**Azure supported services (v0.8.12):**

| Domain | Service | Description |
|--------|---------|-------------|
| compute | vm | Virtual Machines — all families, Linux/Windows, on-demand/spot/reserved |
| storage | managed_disks | Premium SSD, Standard SSD, Standard HDD, Ultra Disk |
| storage | blob | Blob Storage |
| database | sql | Azure SQL Database, Azure DB for MySQL/PostgreSQL — vCore tiers, HA, reserved |
| database | cosmos | Cosmos DB — provisioned (per 100 RU/s), serverless, autoscale |
| container | aks | AKS cluster management fee (free tier or $0.10/hr Standard) |
| serverless | azure_functions | Functions Consumption plan — per GB-second + per execution |
| ai | openai | Azure OpenAI — GPT-4o, GPT-4, GPT-3.5-Turbo, o1, embeddings |
| inter_region_egress | — | Outbound data transfer — internet and inter-region, Zone 1 rates, 5 GB/month free |

**GCP pricing terms:** `on_demand` (default), `spot` (preemptible), `cud_1yr`, `cud_3yr`

## Security

OpenCloudCosts can access sensitive billing data when configured with cloud credentials (AWS Cost Explorer, GCP billing, Azure contract pricing). Follow these guidelines to keep that data safe.

**Credential hygiene**
- Use dedicated, least-privilege credentials — read-only access scoped to pricing and billing APIs only. Never use root, owner, or admin credentials.
- AWS: create an IAM user/role with only `ce:GetCostAndUsage`, `pricing:GetProducts`, and `savingsplans:Describe*` permissions.
- Store credentials in `.env` (see `.env.example`) and never commit that file to version control.

**Transport security**
- The default `stdio` transport is safe — the server runs as a local process with no network exposure.
- If you use `--transport http`, never expose it publicly without a reverse proxy and authentication in front of it. Treat it as an internal service.

**MCP client trust**
- Only add this server to MCP client configs you control.
- Avoid running it alongside untrusted third-party MCP servers — a malicious server can craft prompts that cause the LLM to call your billing tools and relay the results.

**What this server can access**
With credentials configured: actual spend, contract/negotiated pricing, reservation and savings plan data. Understand this before granting access in shared or multi-user environments.

## Phases

- **Phase 1** ✅ AWS public pricing (EC2, EBS, list instances)
- **Phase 2** ✅ AWS effective pricing (Cost Explorer, Savings Plans, Reserved Instances)
- **Phase 3** ✅ GCP public pricing (Compute Engine families, Persistent Disk, CUDs)
- **Phase 4** ✅ Azure public pricing (Retail Prices API, no credentials)
- **Phase 4** ✅ Streamable-HTTP transport (`--transport http`), Dockerfile
- **Phase 4** ✅ Spot price history tool (`get_spot_history`), GCP Windows pricing
- **Phase 5** ✅ GCP managed services — GKE, Memorystore, BigQuery, Vertex AI, Gemini
- **Phase 5** ✅ GCP networking — Cloud LB, CDN, NAT, Cloud Armor, Cloud Monitoring
- **Phase 5** ✅ GCP Cloud SQL; Azure reserved pricing
- **v0.8.0** ✅ Consolidated to 15-tool surface; unified `get_price(spec)` dispatcher; `describe_catalog` discovery; 123/123 (100%) harness pass rate
- **v0.8.2** ✅ Provider protocol cleanup — `major_regions()`, `default_region()`, `bom_advisories()` on all providers; zero provider-string conditionals in tool layer
- **v0.8.3** ✅ Trust metadata on `NormalizedPrice` (`fetched_at`, `source_url`, `cache_age_seconds`); inter-region egress domain + AWS data transfer pricing
- **v0.8.4** ✅ Numeric price fields — structured `{amount, unit, currency, display}` dicts replace formatted strings at all tool boundaries
- **v0.8.5** ✅ Service→domain inference (`fill_domain`); structured `invalid_spec` error hints; 123/123 harness (up from 84%)
- **v0.8.8** ✅ Azure breadth: SQL Database, Cosmos DB, AKS, Azure Functions, Azure OpenAI; 151/151 harness (28 new test scenarios)
- **v0.8.9** ✅ GCP effective/contract pricing via Cloud Billing Pricing API v1beta (`OCC_GCP_BILLING_ACCOUNT_ID`); GCP now at parity with AWS effective pricing
- **v0.8.10** ✅ `GcpAuthProvider` — multi-source OAuth (SA JSON B64, WIF, ADC, metadata server, raw token); `google-auth[requests]` optional `[gcp]` extra; event-loop-safe refresh; no gcloud required in containers
- **v0.8.11** ✅ GCP storage and database contract pricing — GCS, Persistent Disk, Cloud SQL (all engines/sizes/HA), Memorystore; `effective_price` on `StoragePricingSpec` and `DatabasePricingSpec` when billing account configured
- **v0.8.12** ✅ Azure egress pricing (`inter_region_egress` domain) — internet and inter-region outbound transfer, Zone 1 rates from Retail Prices API, 5 GB/month free tier, monthly estimate in response
- **v0.8.13** ✅ GCP network contract pricing — Cloud LB, CDN, NAT, Cloud Armor; `effective_price` on `NetworkPricingSpec` when billing account configured
- **v0.8.14** ✅ GCP internet and inter-region egress (`inter_region_egress` domain) — continent-based rates from SKU catalog with static fallbacks; cross-cloud egress comparison now works across AWS, GCP, and Azure
