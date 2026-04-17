# OpenCloudCosts MCP

An open source MCP server that gives AI assistants accurate cloud pricing data for AWS, GCP, and Azure.

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Supports both **public list pricing** (no credentials needed for AWS and Azure; GCP requires a free API key) and **effective/bespoke pricing** (post-discount: Reserved Instances, Savings Plans, CUDs, EDPs).

## Key Use Cases

- "What is the on-demand price of an m5.xlarge in us-east-1 vs ap-southeast-2, and what's the % delta?"
- "Give me a TCO estimate for this architecture: 3x m5.xlarge + 1x 500GB gp3 EBS in us-east-1"
- "What's the cost per user if I run this stack for 50,000 MAUs?"
- "List all c6g instances in eu-west-1 with >= 8 vCPUs"
- "What's my effective hourly rate on m5.xlarge after Savings Plans?"

## Tools

| Tool | Description |
|------|-------------|
**Pricing Lookup**

| Tool | Description |
|------|-------------|
| `get_compute_price` | Price for a specific instance type in a region |
| `get_storage_price` | EBS/S3/GCS storage pricing |
| `get_service_price` | **Generic pricing for any AWS service** — CloudWatch, data transfer, RDS, Lambda, ELB, Route53, DynamoDB, EFS, and 250+ others |
| `get_prices_batch` | Prices for multiple instance types in one call (concurrent) |
| `compare_compute_prices` | Compare same instance across multiple regions with optional baseline deltas |
| `search_pricing` | Search pricing catalog by keyword — any service, not just EC2 |

**Effective & Discount Pricing**

| Tool | Description |
|------|-------------|
| `get_effective_price` | Effective rate after account discounts (requires credentials) |
| `get_discount_summary` | All active RIs and Savings Plans with utilization % |

**Discovery**

| Tool | Description |
|------|-------------|
| `list_services` | All 260+ AWS services with pricing data |
| `list_regions` | All regions with friendly names |
| `list_instance_types` | Available instance types with vCPU/memory filters |
| `check_availability` | Is a SKU available in a region? |

**Region Analysis**

| Tool | Description |
|------|-------------|
| `find_cheapest_region` | Cheapest region for an instance type with optional baseline deltas |
| `find_available_regions` | Every region where an instance exists — prices, region names, deltas |

**Cost Estimation**

| Tool | Description |
|------|-------------|
| `estimate_bom` | TCO for a Bill of Materials |
| `estimate_unit_economics` | Cost per user/request/transaction |

**Cache**

| Tool | Description |
|------|-------------|
| `refresh_cache` | Invalidate pricing cache |
| `cache_stats` | Cache entry counts and DB size |

See [docs/tools.md](docs/tools.md) for full parameter reference and [docs/finops-guide.md](docs/finops-guide.md) for usage examples.

## Setup

### Prerequisites
- [uv](https://docs.astral.sh/uv/) installed (`curl -LsSf https://astral.sh/uv/install.sh | sh`)
- AWS credentials (optional — public pricing works without them)

### Clone & configure

```bash
git clone https://github.com/x7even/cloudcostsmcp opencloudcosts
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
docker build -t opencloudcosts .
docker run -p 8080:8080 \
  -e OCC_GCP_API_KEY=AIza... \
  -v ~/.aws:/root/.aws:ro \
  opencloudcosts
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
| `OCC_AWS_ENABLE_COST_EXPLORER` | false | Enable effective pricing (costs $0.01/call) |
| `OCC_DEFAULT_REGIONS` | us-east-1,us-west-2 | Default regions |
| `AWS_PROFILE` | (default chain) | AWS credentials profile |

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

## Azure Setup

Azure pricing is fully public — no credentials, API key, or subscription needed.

```bash
# No configuration needed — works out of the box
uv run opencloudcosts
```

**Azure instance type format:** ARM SKU names e.g. `Standard_D4s_v3`, `Standard_E8s_v3`, `Standard_B2ms`

**Azure pricing terms:** `on_demand` (default), `reserved_1yr`, `reserved_3yr`, `spot`

**Azure regions:** ARM region names e.g. `eastus`, `westeurope`, `southeastasia` (use `list_regions` for full list)

**GCP pricing terms:** `on_demand` (default), `spot` (preemptible), `cud_1yr`, `cud_3yr`

## Phases

- **Phase 1** ✅ AWS public pricing (EC2, EBS, list instances)
- **Phase 2** ✅ AWS effective pricing (Cost Explorer, Savings Plans, Reserved Instances)
- **Phase 3** ✅ GCP public pricing (Compute Engine families, Persistent Disk, CUDs)
- **Phase 4** ✅ Azure public pricing (Retail Prices API, no credentials)
- **Phase 4** ✅ Streamable-HTTP transport (`--transport http`), Dockerfile
- **Phase 4** ✅ Spot price history tool (`get_spot_history`), GCP Windows pricing
- **Phase 5**: GCP effective pricing (BigQuery billing export)
