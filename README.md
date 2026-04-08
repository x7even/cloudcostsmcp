# CloudCost MCP Server

An MCP server that gives AI assistants accurate cloud pricing data for AWS (and GCP in Phase 3).

Supports both **public list pricing** (no credentials needed) and **effective/bespoke pricing** (post-discount: Reserved Instances, Savings Plans, CUDs, EDPs).

## Key Use Cases

- "What is the on-demand price of an m5.xlarge in us-east-1 vs ap-southeast-2, and what's the % delta?"
- "Give me a TCO estimate for this architecture: 3x m5.xlarge + 1x 500GB gp3 EBS in us-east-1"
- "What's the cost per user if I run this stack for 50,000 MAUs?"
- "List all c6g instances in eu-west-1 with >= 8 vCPUs"
- "What's my effective hourly rate on m5.xlarge after Savings Plans?"

## Tools

| Tool | Description |
|------|-------------|
| `get_compute_price` | Price for a specific instance type in a region |
| `compare_compute_prices` | Compare same instance across multiple regions |
| `find_cheapest_region` | Find cheapest region for an instance type across all regions |
| `get_storage_price` | EBS/S3/GCS storage pricing |
| `search_pricing` | Free-text search across pricing catalog |
| `get_effective_price` | Effective rate after account discounts (requires credentials) |
| `get_discount_summary` | All active RIs and Savings Plans with utilization % |
| `list_regions` | All regions for a provider/service |
| `list_instance_types` | Available instance types with filters |
| `check_availability` | Is a SKU available in a region? |
| `estimate_bom` | TCO for a Bill of Materials |
| `estimate_unit_economics` | Cost per user/request/transaction |
| `refresh_cache` | Invalidate pricing cache |
| `cache_stats` | Cache entry counts and DB size |

See [docs/tools.md](docs/tools.md) for full parameter reference and [docs/finops-guide.md](docs/finops-guide.md) for usage examples.

## Setup

### Prerequisites
- [uv](https://docs.astral.sh/uv/) installed (`curl -LsSf https://astral.sh/uv/install.sh | sh`)
- AWS credentials (optional — public pricing works without them)

### Clone & configure

```bash
git clone <repo> cloudcostmcp
cd cloudcostmcp
cp .env.example .env
# Edit .env if you want effective pricing / Cost Explorer
```

### Test it works

```bash
uv run pytest
```

### Run the server

```bash
uv run cloudcostmcp
```

### Connect to Claude Code

Add to your project's `.mcp.json`:

```json
{
  "mcpServers": {
    "cloudcost": {
      "command": "uv",
      "args": ["run", "--directory", "/path/to/cloudcostmcp", "cloudcostmcp"],
      "env": {
        "AWS_PROFILE": "default"
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
      "args": ["run", "--directory", "/path/to/cloudcostmcp", "cloudcostmcp"]
    }
  }
}
```

### Test with MCP Inspector

```bash
npx @modelcontextprotocol/inspector uv run --directory /path/to/cloudcostmcp cloudcostmcp
```

## AWS Credentials

| Feature | Credentials needed |
|---------|--------------------|
| Public pricing (EC2, EBS, RDS list prices) | None |
| Effective pricing (RI / SP discounts) | AWS credentials + `CLOUDCOSTMCP_AWS_ENABLE_COST_EXPLORER=true` |

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

All settings via environment variables (prefix `CLOUDCOSTMCP_`) or `.env` file:

| Variable | Default | Description |
|----------|---------|-------------|
| `CLOUDCOSTMCP_CACHE_TTL_HOURS` | 24 | Public price cache TTL |
| `CLOUDCOSTMCP_AWS_ENABLE_COST_EXPLORER` | false | Enable effective pricing (costs $0.01/call) |
| `CLOUDCOSTMCP_DEFAULT_REGIONS` | us-east-1,us-west-2 | Default regions |
| `AWS_PROFILE` | (default chain) | AWS credentials profile |

## Caching

Prices are cached in SQLite at `~/.cache/cloudcostmcp/pricing.db`. Public list prices are cached for 24 hours — AWS pricing changes infrequently. Use the `refresh_cache` tool to force a refresh.

## GCP Setup

GCP pricing requires authentication. Either:

**Option A — API key** (simplest, no service account needed):
```bash
# Create an API key in GCP Console -> APIs & Services -> Credentials
export CLOUDCOSTMCP_GCP_API_KEY=AIza...
```

**Option B — Application Default Credentials**:
```bash
gcloud auth application-default login
# or set GOOGLE_APPLICATION_CREDENTIALS=/path/to/service-account.json
```

**GCP instance type format:** `{family}-{series}-{vcpus}` e.g. `n2-standard-4`, `e2-highmem-8`, `c2-standard-16`

**GCP pricing terms:** `on_demand` (default), `spot` (preemptible), `cud_1yr`, `cud_3yr`

## Phases

- **Phase 1** ✅ AWS public pricing (EC2, EBS, list instances)
- **Phase 2** ✅ AWS effective pricing (Cost Explorer, Savings Plans, Reserved Instances)
- **Phase 3** ✅ GCP public pricing (Compute Engine families, Persistent Disk, CUDs)
- **Phase 4**: GCP effective pricing (BigQuery billing export) + RDS/database pricing
- **Phase 5**: Azure, HTTP transport, spot price history
