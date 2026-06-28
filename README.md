# OpenCloudCosts MCP

<!-- mcp-name: io.github.x7even/cloudcostsmcp -->

## Anchor AI FinOps to real, live cloud pricing.

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![PyPI version](https://img.shields.io/pypi/v/opencloudcosts)](https://pypi.org/project/opencloudcosts/)
[![Release](https://img.shields.io/github/v/release/x7even/cloudcostsmcp?include_prereleases)](https://github.com/x7even/cloudcostsmcp/releases/latest)

Your LLM's cloud pricing knowledge was frozen at training cutoff. Cloud pricing wasn't.

**opencloudcosts** is an MCP server that gives your AI assistant live, structured access to AWS, GCP, and Azure pricing — 16 tools it can call directly, with results it can reason over rather than guess at. Ask it to compare a full multi-resource workload across all three clouds simultaneously. Ask it for your actual post-discount effective rates from Reserved Instances, Savings Plans, or Enterprise Discount Programs. Ask it to fan out across every region and return the cheapest option. None of that is possible from training data alone, and none of it is possible from a single-cloud pricing calculator.

## The Problem

Ask any AI assistant what an `m5.2xlarge` costs in `ap-southeast-2`, whether AWS or GCP is cheaper for a three-tier web app, or what your effective hourly rate is after Savings Plans. You will get a confident answer. It will probably be wrong.

Cloud pricing is a poor fit for static model weights: instance families are added and retired, regional pricing diverges (the same instance type can differ 20–40% across regions), spot markets fluctuate, and commitment discounts are by definition unique to each account. A model answering from training data cannot know your negotiated EDP rate. It can barely reliably recall last year's list price. The problem is not that LLMs are bad — it's that real-time, account-specific pricing data is structurally outside what a model can know.

opencloudcosts fixes this by giving your AI assistant 16 MCP tools backed by live provider APIs. Instead of hallucinating numbers, the model calls a tool and gets a precise answer.

## A Concrete Scenario

Your team asks: *"We're evaluating whether to migrate this workload from AWS to GCP. What does the full stack cost on-demand vs. three-year committed, across all three clouds, so I can make the case to leadership?"*

Without this tool, the model interpolates — possibly from pricing that is a year out of date — across three providers, multiple resource types, and at least four pricing tiers. The numbers will be plausible and wrong.

With opencloudcosts, the model calls `compare_bom` with the workload spec (compute + storage + database). The tool fans out **8 concurrent provider calls** across AWS, GCP, and Azure simultaneously, prices each resource category at public and committed rates, and returns a per-provider, per-term breakdown with savings analysis — in a single tool call.

No spreadsheet. No switching between three provider calculators. No manual SKU matching. No training-data approximation.

## Why Not the Obvious Alternatives

**Asking a model without tools** — Training data has a cutoff. Cloud pricing changes constantly, varies 20–40% across regions for the same instance type, and includes account-level commitment discounts that are invisible at inference time. The model will approximate, confabulate, or recall stale numbers. There is no structured output, and the model cannot access your contracted rates under any circumstances.

**Cloud pricing calculators (AWS / GCP / Azure)** — Each covers exactly one cloud. They are UI-only with no API surface callable from an AI assistant. Cross-cloud comparison requires manually reproducing the same architecture three times across three separate calculators and reconciling exports by hand. They have no concept of unit economics and produce no programmatic output for agentic workflows.

**Infracost** — Excellent at estimating cost diffs against Terraform plans. It requires IaC files as input — it cannot answer "what does an n2-standard-8 cost in europe-west4?" without a Terraform plan in hand. It is not an MCP server and is not callable from a conversational AI context.

**Calling provider APIs directly** — AWS bulk pricing files are multi-GB and require targeted API access patterns to avoid downloading them in full. GCP's Cloud Billing Pricing API v1beta requires a multi-source ADC credential chain and provider-specific IAM. Azure's Retail Prices API needs pagination logic and SKU matching. Every provider uses different region naming conventions, SKU formats, and data schemas. opencloudcosts normalizes all of this behind a uniform MCP tool interface and handles credential chains, caching, and retry logic so the model does not have to.

## Capabilities at a Glance

| Capability | LLM (no tools) | Cloud Calculators | Infracost | opencloudcosts |
|---|---|---|---|---|
| Live pricing (not frozen at training cutoff) | No | Manual input only | IaC-bound | Yes — fetched from provider APIs |
| Cross-cloud comparison in one call | No | No | No | Yes — `compare_bom`, 8 concurrent provider calls |
| Effective/contracted rates (RI, SP, EDP, CUD) | No | No | No | Yes — credentials unlock this layer |
| Multi-region concurrent fan-out | No | No | No | Yes — up to 32 goroutines |
| Unit economics (cost/user, cost/request) | Approximate | No | No | Yes — `estimate_unit_economics` |
| MCP tool surface — callable by model | N/A | No | No | Yes — 16 tools |
| AWS + Azure public pricing, zero credentials | N/A | Single-cloud only | Partial | Yes |
| HTTP service for shared/Kubernetes deployments | N/A | N/A | No | Yes — bearer auth, rate limiting, probes |

## Coverage: Three Clouds, 16 Tools

Fourteen tools are fully functional. Two entries are compatibility stubs: `search_pricing` (a deprecated redirect from v0.8.x, kept for backward compatibility) and `get_spot_history` (registered but not implemented in the Go server — returns a structured "not available" response).

**Credential requirements by provider**

| Coverage | Credentials required |
|---|---|
| AWS EC2, EBS (gp3/io2/sc1), RDS, inter-region egress | None |
| Azure VMs, Managed Disks, Blob Storage, Azure SQL/MySQL/PostgreSQL, Cosmos DB, AKS, Azure Functions, Azure OpenAI (GPT-4o, GPT-4, GPT-3.5-Turbo, o1, o1-mini, embeddings) | None — fully public Retail Prices API |
| GCP Compute Engine, Cloud Storage, Persistent Disk, Cloud SQL, Memorystore, GKE, BigQuery, Vertex AI, Gemini, Cloud LB/CDN/NAT/Armor, Cloud Monitoring | Free API key (`OCC_GCP_API_KEY`) — no billing account, no credit card |
| AWS post-discount rates (Reserved Instances, Savings Plans) + `get_discount_summary` | AWS credentials + `OCC_AWS_ENABLE_COST_EXPLORER=true` ($0.01/call to Cost Explorer, opt-in only) |
| GCP committed-use discounts (CUDs) and Enterprise Discount Programs (EDPs) | ADC credentials + `billing.billingAccountPrice.get` IAM + `OCC_GCP_BILLING_ACCOUNT_ID` |

Azure Reserved VM pricing (1-year and 3-year terms) is available via the public Retail Prices API — no credentials needed. `compare_bom` returns committed-term Azure pricing with no setup beyond the binary.

**The 16 tools by category**

| Category | Tools |
|---|---|
| Pricing | `get_price`, `get_prices_batch`, `compare_prices`, `describe_catalog`, `search_pricing`†, `get_spot_history`† |
| FinOps | `estimate_bom`, `estimate_unit_economics`, `compare_bom`, `get_discount_summary` |
| Discovery | `list_regions`, `list_instance_types`, `find_cheapest_region`, `find_available_regions` |
| Cache | `refresh_cache`, `cache_stats` |

† Compatibility stub only — not functional for live data. See [opencloudcosts-go/README.md](opencloudcosts-go/README.md) for full parameter reference.

## Performance and Reliability

**Concurrency** — The analysis tools are not sequential HTTP wrappers:

- `find_cheapest_region` and `find_available_regions`: errgroup + semaphore, **up to 32 goroutines** — queries all available regions in parallel, returns results sorted cheapest-first
- `compare_bom`: **8 concurrent provider calls** across AWS, GCP, and Azure simultaneously
- `compare_prices`: semaphore of **10 concurrent region calls**
- `get_prices_batch`: parallelized across instance types within a region

**Rate limiting and timeouts** — Token-bucket rate limiter at 10 req/s on the HTTP transport (`OCC_RATE_LIMIT`). Per-request deadline: 30s (`OCC_REQUEST_TIMEOUT`). Per-provider API call: 15s (`OCC_PROVIDER_TIMEOUT`). Graceful SIGTERM drain: 10s (`OCC_SHUTDOWN_TIMEOUT`).

**Cache** — Prices are stored in a **concurrent in-memory cache** (read-optimised with `sync.RWMutex`) with atomic JSON persistence at `~/.cache/opencloudcosts/cache.json`. TTLs: public prices 24h (`OCC_CACHE_TTL_HOURS`), region/instance metadata 7 days (`OCC_METADATA_TTL_DAYS`), effective/contracted rates 1h (`OCC_EFFECTIVE_PRICE_TTL_HOURS`). Cache survives binary updates. 401/403 responses from billing APIs are never cached, so credential rotation takes effect immediately.

**AWS pricing** — EC2/EBS/RDS public pricing uses a targeted SKU API path rather than downloading the full multi-GB bulk pricing file, keeping startup fast and avoiding large network payloads.

**Error isolation** — Raw exception text never reaches LLM context. All tool boundaries emit structured error envelopes; full tracebacks are logged server-side only. GCP contract pricing falls back to public list prices on auth failure rather than surfacing an error into the conversation.

## Validated: 234/234

opencloudcosts v1.0.0 achieves **234/234 (100%)** on the LLM grounding harness, with zero XML hallucinations across the full suite.

The harness covers 234 realistic pricing questions across all three clouds: instance spot checks, cross-region comparisons, BOM estimates, multi-cloud comparisons, unit economics, AI model pricing, database pricing, storage pricing, discount summaries, egress pricing, availability queries, and network pricing.

Primary validation model: **`qwen3.6-35b-128k`** running locally via llama-swap — a self-hosted 35B reasoning model with no external API dependency. The harness has also been exercised against `qwen3.6-35b-a3b`, `qwen3.5-122b-a10b@q6_k`, and `gemma-4-26b-a4b`. Because MCP is a protocol rather than a model feature, accuracy comes from the tool returning correct live data — any MCP-capable AI assistant calls the same tool surface and gets the same structured response.

Harness progression: 109/123 (Python v0.8.x) → 169/169 (v0.9.0) → 199/199 (v0.9.2) → **234/234 (v1.0.0)**.

643 Go unit tests across all providers and tools verify behavioral correctness and cross-provider API parity, independent of LLM evaluation.

## Use Cases

**1. Price the same workload across all three clouds at once**

> "Price the following on AWS, GCP, and Azure simultaneously: 4 instances (8 vCPU, 32 GB RAM), 1 managed PostgreSQL database (4 vCPU, 16 GB RAM), and 500 GB block storage. Return on-demand, 1-year committed, and 3-year committed totals for each cloud with monthly and annual figures, and flag which provider is cheapest at each commitment term."

`compare_bom` fans out 8 concurrent provider calls and returns a per-provider, per-term breakdown with savings analysis versus on-demand. No pricing calculator does this across cloud boundaries. AWS and Azure public pricing requires no credentials; GCP requires a free API key.

---

**2. Find the cheapest AWS region for a long-running compute workload**

> "I need to run a c6a.4xlarge continuously. Fan out across all available AWS regions and return the 5 cheapest, sorted by on-demand hourly rate. Show us-east-1 as a baseline."

`find_cheapest_region` uses a 32-goroutine fan-out across every region where the instance type is available and returns results sorted cheapest-first. Regional price deltas for the same instance type routinely exceed 20%. No credentials needed.

---

**3. Determine your effective AWS rate after commitments**

> "I have two m5.xlarge Reserved Instances (1-year, no upfront) in us-east-1 and a Compute Savings Plan covering $500/month of EC2 spend. What is my effective blended hourly rate on m5.xlarge right now, and what percentage am I saving versus on-demand?"

`get_price` with AWS credentials and `OCC_AWS_ENABLE_COST_EXPLORER=true` returns your actual post-discount rate alongside the public list price, pulling live data from Cost Explorer and Savings Plans APIs.

---

**4. Azure serverless vs. always-on: break-even analysis**

> "Our batch processing job runs 1.5 million Azure Function executions per month, each consuming 512 MB for 900ms. What is the total monthly cost on the Consumption plan in West Europe, and what is the monthly cost of a Standard_D2s_v5 VM running continuously in the same region? At what monthly execution count do they break even?"

`estimate_unit_economics` covers the Functions path; `get_price` covers the VM. Azure pricing is fully public — no credentials, no API key, no subscription required.

---

**5. Unit economics for a SaaS product**

> "If I run two m5.large app servers, one db.t3.medium RDS MySQL instance, and 200 GB gp3 in us-east-1, and I have 50,000 monthly active users making 1 million requests per day, what is my infrastructure cost per user and per request?"

`estimate_bom` prices the full stack; `estimate_unit_economics` computes cost per user and per request at that scale. Output is structured for direct use in a margin model or board-level cost discussion. No credentials needed.

---

**6. AI token cost comparison: Vertex AI vs. Azure OpenAI**

> "We process 50 million input tokens and 8 million output tokens per month. Compare the total monthly cost of Gemini 1.5 Pro on Vertex AI versus GPT-4o and GPT-4o-mini on Azure OpenAI. Show cost per million tokens and total monthly bill for each."

`get_price` with `domain: ai` covers both providers. GCP Vertex AI and Gemini pricing requires a free GCP API key (`OCC_GCP_API_KEY`); Azure OpenAI pricing is fully public — no credentials needed.

---

## Setup

### Option 1 — pip (easiest, cross-platform)

The PyPI package wraps the native Go binary — no Go toolchain needed.

```bash
pip install opencloudcosts
opencloudcosts            # stdio mode (for local MCP clients)
opencloudcosts --transport http --host 0.0.0.0 --port 8080  # HTTP mode
```

### Option 2 — binary download

Download the pre-built binary for your platform from the [latest release](https://github.com/x7even/cloudcostsmcp/releases/latest):

```bash
# Linux (amd64)
curl -L https://github.com/x7even/cloudcostsmcp/releases/latest/download/opencloudcosts_linux_amd64.tar.gz | tar xz
./opencloudcosts

# macOS (Apple Silicon)
curl -L https://github.com/x7even/cloudcostsmcp/releases/latest/download/opencloudcosts_darwin_arm64.tar.gz | tar xz
./opencloudcosts
```

### Option 3 — Docker / container

```bash
# Build the image first (no pre-built image is published)
cd opencloudcosts-go
docker build -t opencloudcosts:local .

# Run — HTTP transport, bound to all interfaces
docker run -p 8080:8080 \
  -e OCC_GCP_API_KEY=AIza... \
  -v ~/.aws:/root/.aws:ro \
  opencloudcosts:local
```

The image is ~15 MB (distroless scratch base, static binary). No credentials are
required for AWS and Azure public pricing.

### Option 4 — build from source

```bash
git clone https://github.com/x7even/cloudcostsmcp
cd cloudcostsmcp/opencloudcosts-go
CGO_ENABLED=0 go build -o opencloudcosts ./cmd/opencloudcosts
./opencloudcosts
```

### Connect to Claude Code

**Stdio (local process — recommended for single-user)**

Add to `~/.claude/settings.json` or your project's `.mcp.json`:

```json
{
  "mcpServers": {
    "cloudcost": {
      "command": "opencloudcosts",
      "env": {
        "OCC_GCP_API_KEY": "AIza..."
      }
    }
  }
}
```

**HTTP (shared/remote server — one server, many clients)**

```json
{
  "mcpServers": {
    "cloudcost": {
      "transport": "http",
      "url": "http://localhost:8080/"
    }
  }
}
```

### Kubernetes

See `deploy/kubernetes/` for manifests. Build and push your own image (see Docker section above), then reference it in `deployment.yaml`.
Credentials are passed via environment variables or Kubernetes Secrets — same variable
names as the Docker examples above.

### Test with MCP Inspector

```bash
npx @modelcontextprotocol/inspector opencloudcosts
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

Prices are stored in a **concurrent in-memory cache** (read-optimised with `sync.RWMutex`) with atomic JSON persistence at `~/.cache/opencloudcosts/cache.json`. Public list prices are cached for 24 hours — AWS pricing changes infrequently. Use the `refresh_cache` tool to force a refresh.

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

**Azure supported services:**

| Domain | Service | Description |
|--------|---------|-------------|
| compute | vm | Virtual Machines — all families, Linux/Windows, on-demand/spot/reserved |
| storage | managed_disks | Premium SSD, Standard SSD, Standard HDD, Ultra Disk |
| storage | blob | Blob Storage |
| database | sql | Azure SQL Database, Azure DB for MySQL/PostgreSQL — vCore tiers, HA, reserved |
| database | cosmos | Cosmos DB — provisioned (per 100 RU/s), serverless, autoscale |
| container | aks | AKS cluster management fee (free tier or $0.10/hr Standard) |
| serverless | azure_functions | Functions Consumption plan — per GB-second + per execution |
| ai | openai | Azure OpenAI — GPT-4o, GPT-4, GPT-3.5-Turbo, o1, o1-mini, embeddings |
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

## Recent releases

- **v0.9.1** ✅ GCP egress contract pricing; fix `PricingResult.source` Literal
- **v0.9.2** ✅ Azure OpenAI model matching fix; Azure Functions pricing fix; `list_instance_types` cap; 199-prompt harness suite
- **v1.0.0** ✅ Go rewrite — static binary, dual stdio/HTTP transport, 16 tools, `compare_bom` cross-cloud workload comparison, concurrent region fan-out (32 goroutines), Azure o1-mini SKU fix; **234/234 (100%) LLM grounding harness**
- **v1.0.1** ✅ PyPI package description; CI Trusted Publisher fix; `go install` tag
- **v1.0.2** ✅ README accuracy fixes; cache description updated
