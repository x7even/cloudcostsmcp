# opencloudcosts-go

**Real cloud pricing, grounded in data, delivered at scale.**

opencloudcosts-go is a [Model Context Protocol (MCP)](https://modelcontextprotocol.io) server that gives AI assistants accurate, real-time pricing data across AWS, GCP, and Azure — so they can answer infrastructure cost questions with precision instead of hallucinating numbers.

16 tools. Three clouds. Zero credentials needed for AWS and Azure public pricing. 234/234 on the LLM grounding harness at v1.0.0.

---

## Why this exists

Cost questions are one of the most common things you ask an AI about infrastructure — and one of the most likely to go wrong. Without grounded tooling, models approximate, confabulate, or recall stale training data. opencloudcosts-go fixes that: it gives the model a live pricing API it can actually call, with structured responses it can reason from.

**Key strengths:**

- **Grounded, not hallucinated.** Every number comes from a live API call to the cloud provider. Pricing is as fresh as the cache TTL (default 24h, configurable to zero).
- **Concurrent by design.** Region fan-outs run up to 32 goroutines in parallel (configurable). Cross-provider BOM comparisons fan across all three clouds simultaneously. Batch queries parallelize across instance types. You get results fast even when asking across many regions.
- **Zero-credential public pricing.** AWS public catalog (EC2, EBS, RDS list prices) and all Azure pricing (via the public Retail Prices API) require no credentials at all. GCP catalog pricing needs only an API key. Credentials unlock contracted/effective pricing on top.
- **16 purpose-built tools.** From single-resource spot checks to full multi-cloud BOM comparisons with unit economics. The model picks the right tool; you don't need to direct it.
- **Static single binary.** No runtime dependencies, no Python venv, no Node version pinning. Build once, run anywhere: bare metal, container, Kubernetes, or as a local stdio process for Claude Code.
- **Production-ready HTTP transport.** Token-bucket rate limiting, bearer-token auth, liveness/readiness probes, graceful SIGTERM drain — ready to run as a sidecar or standalone service.

---

## Install

### Option A: Homebrew (macOS / Linux)

```bash
brew tap x7even/opencloudcosts
brew install opencloudcosts
```

### Option B: Build from source

Requires Go 1.25.11+.

```bash
git clone https://github.com/x7even/cloudcostsmcp
cd cloudcostsmcp/opencloudcosts-go
CGO_ENABLED=0 go build -o opencloudcosts ./cmd/opencloudcosts
```

The result is a fully static binary with no system dependencies.

### Option C: Docker

Multi-stage build produces a ~15 MB distroless image (scratch base, no shell, no OS).

```bash
cd opencloudcosts-go
docker build -t opencloudcosts-go:latest .

# Or from the repo root:
docker build -f opencloudcosts-go/Dockerfile -t opencloudcosts-go:latest opencloudcosts-go/
```

Run it:

```bash
docker run --rm -p 8080:8080 \
  -e OCC_API_KEY=your-secret-token \
  opencloudcosts-go:latest
```

---

## Updating

### From source

```bash
cd cloudcostsmcp
git pull
cd opencloudcosts-go
CGO_ENABLED=0 go build -o opencloudcosts ./cmd/opencloudcosts
```

### Docker

```bash
docker build -t opencloudcosts-go:latest .
# Restart your container with the new image
```

Cache is stored outside the binary (`OCC_CACHE_DIR`, default `~/.cache/opencloudcosts`), so updates never reset cached pricing data.

---

## Quick start

```bash
# stdio (local — for Claude Code, Claude Desktop)
./opencloudcosts

# HTTP transport (for remote clients, containers, Kubernetes)
./opencloudcosts --transport http --host 0.0.0.0 --port 8080
```

### Connect to Claude Code (stdio)

Add to your Claude Code MCP config (`~/.claude/settings.json` or project `.mcp.json`):

```json
{
  "mcpServers": {
    "opencloudcosts": {
      "command": "/path/to/opencloudcosts",
      "args": [],
      "env": {
        "OCC_GCP_API_KEY": "your-gcp-api-key"
      }
    }
  }
}
```

AWS and Azure public pricing work immediately with no credentials. The `OCC_GCP_API_KEY` unlocks GCP catalog prices.

### Connect to Claude Desktop

Add to `~/Library/Application Support/Claude/claude_desktop_config.json` (macOS) or `%APPDATA%\Claude\claude_desktop_config.json` (Windows):

```json
{
  "mcpServers": {
    "opencloudcosts": {
      "command": "/path/to/opencloudcosts",
      "args": []
    }
  }
}
```

### Connect via HTTP (remote clients)

```bash
# Start with auth token
OCC_API_KEY=my-secret-token ./opencloudcosts --transport http --host 0.0.0.0 --port 8080

# MCP clients connect to the root endpoint
# The server listens at / — configure your MCP client to use:
#   http://localhost:8080/
```

---

## Configuration

### Binary flags

| Flag | Default | Env override | Description |
|------|---------|-------------|-------------|
| `--transport` | `stdio` | — | `stdio` or `http` |
| `--host` | `127.0.0.1` | `OCC_HTTP_HOST` | HTTP bind address |
| `--port` | `8080` | `OCC_HTTP_PORT` | HTTP port |

### Caching

| Variable | Default | Description |
|----------|---------|-------------|
| `OCC_CACHE_DIR` | `~/.cache/opencloudcosts` | Cache file directory |
| `OCC_CACHE_TTL_HOURS` | `24` | Price cache TTL (hours) |
| `OCC_METADATA_TTL_DAYS` | `7` | Region/instance metadata TTL (days) |
| `OCC_EFFECTIVE_PRICE_TTL_HOURS` | `1` | Contracted/effective price TTL (hours) |
| `OCC_SPOT_CACHE_TTL_MINUTES` | `5` | Spot price history TTL (minutes) |

Set `OCC_CACHE_TTL_HOURS=0` to disable price caching entirely (always-fresh, higher latency).

### General

| Variable | Default | Description |
|----------|---------|-------------|
| `OCC_DEFAULT_CURRENCY` | `USD` | Response currency |
| `OCC_DEFAULT_REGIONS` | `us-east-1,us-west-2` | Comma-separated default region list |
| `OCC_MAX_RESULTS` | `20` | Max results for list/search tools |
| `OCC_LOG_LEVEL` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |
| `OCC_API_KEY` | _(none)_ | Bearer token for HTTP transport auth |
| `OCC_RATE_LIMIT` | `10` | Requests/second (token bucket, HTTP transport) |
| `OCC_REQUEST_TIMEOUT` | `30s` | Per-request deadline |
| `OCC_PROVIDER_TIMEOUT` | `15s` | Per-provider API call deadline |
| `OCC_SHUTDOWN_TIMEOUT` | `10s` | Graceful shutdown drain period (SIGTERM) |

### AWS

| Variable | Default | Description |
|----------|---------|-------------|
| `OCC_AWS_REGION` | `us-east-1` | AWS SDK default region |
| `OCC_AWS_PROFILE` | _(none)_ | AWS credentials profile name |
| `OCC_AWS_ENABLE_COST_EXPLORER` | `false` | Enable Cost Explorer (paid API — $0.01/call) |
| `AWS_ACCESS_KEY_ID` | _(none)_ | Standard AWS credential env var |
| `AWS_SECRET_ACCESS_KEY` | _(none)_ | Standard AWS credential env var |

**No credentials required for AWS public pricing.** EC2, EBS, and RDS list prices are fetched from the public AWS Pricing API. Credentials are only needed for `get_discount_summary` (Savings Plans / Reserved Instance summary) and Cost Explorer data.

### GCP

| Variable | Default | Description |
|----------|---------|-------------|
| `OCC_GCP_PROJECT_ID` | _(none)_ | GCP project ID |
| `OCC_GCP_API_KEY` | _(none)_ | API key — enables catalog prices without a service account |
| `OCC_GCP_SERVICE_ACCOUNT_JSON` | _(none)_ | Service account JSON (inline) |
| `OCC_GCP_SERVICE_ACCOUNT_JSON_B64` | _(none)_ | Service account JSON (base64-encoded) |
| `OCC_GCP_EXTERNAL_ACCOUNT_JSON` | _(none)_ | Workload Identity / external account JSON (inline) |
| `OCC_GCP_EXTERNAL_ACCOUNT_JSON_B64` | _(none)_ | Workload Identity JSON (base64-encoded) |
| `OCC_GCP_ACCESS_TOKEN` | _(none)_ | Short-lived access token |
| `OCC_GCP_ACCESS_TOKEN_EXPIRES_AT` | _(none)_ | RFC3339 expiry for the access token |
| `OCC_GCP_BILLING_ACCOUNT_ID` | _(none)_ | Billing account for CUD summaries |
| `OCC_GCP_BILLING_DATASET` | _(none)_ | BigQuery dataset for billing export |
| `GOOGLE_APPLICATION_CREDENTIALS` | _(none)_ | Standard ADC path (service account JSON file) |

GCP catalog pricing (Compute Engine, Cloud Storage, etc.) works with just `OCC_GCP_API_KEY`. A service account is only needed for contracted/effective prices or billing analysis.

### Azure

No credentials required. Azure pricing uses the public [Retail Prices API](https://learn.microsoft.com/en-us/rest/api/cost-management/retail-prices/azure-retail-prices).

---

## Tools reference

### Lookup (single-resource pricing)

| Tool | Description |
|------|-------------|
| `get_price` | Look up pricing for any compute, storage, database, AI, container, analytics, network, or inter-region egress resource. Returns public catalog rates; also returns contracted/effective prices when credentials are available. |
| `get_prices_batch` | Get prices for multiple compute instance types in a single region in one parallel call. |
| `compare_prices` | Compare pricing for any service across multiple regions concurrently. |
| `describe_catalog` | Discover what each provider supports and get example invocations for `get_price`. |
| `get_spot_history` | _Not implemented_ — registered for compatibility; returns a structured "not available" response. |
| `search_pricing` | _Deprecated_ — redirects to the correct tool. Kept for backward compatibility. |

#### `get_price` supported domains

`get_price` covers the full breadth of cloud services:

| Domain | Providers | Examples |
|--------|-----------|---------|
| `compute` | AWS, GCP, Azure | EC2, Compute Engine, Azure VMs, Fargate |
| `storage` | AWS, GCP, Azure | EBS (gp3/io2/sc1), GCS, Azure Disk |
| `database` | AWS, GCP, Azure | RDS, Cloud SQL, Azure SQL, ElastiCache, Memorystore |
| `ai` | AWS, GCP | Bedrock (Claude, Llama), Vertex AI, Gemini |
| `container` | AWS, GCP | EKS, GKE (Standard + Autopilot) |
| `analytics` | GCP | BigQuery (query + storage) |
| `network` | GCP | Cloud LB, CDN, NAT, Cloud Armor |
| `observability` | AWS, GCP | CloudWatch, Cloud Monitoring |
| `inter_region_egress` | AWS, GCP | Cross-region and internet egress |

### FinOps (cost estimation and comparison)

| Tool | Description |
|------|-------------|
| `estimate_bom` | Estimate total monthly and annual cost for a multi-resource infrastructure stack (compute + storage + database + AI) in a single call. |
| `estimate_unit_economics` | Estimate per-unit economics — cost per user, per request, per transaction. |
| `compare_bom` | Price the same workload across AWS, GCP, and Azure simultaneously and return a side-by-side comparison with savings analysis. |
| `get_discount_summary` | Return a summary of active discounts for the authenticated account (Savings Plans, Reserved Instances, CUDs). Requires AWS credentials + `OCC_AWS_ENABLE_COST_EXPLORER=true`. |

### Cache management

| Tool | Description |
|------|-------------|
| `refresh_cache` | Invalidate the pricing cache to force fresh data on the next request. |
| `cache_stats` | Return statistics about the local pricing cache (size, age, hit rate). |

### Discovery

| Tool | Description |
|------|-------------|
| `list_regions` | List all regions where a cloud service is available for the given provider. |
| `list_instance_types` | List available compute instance types matching vCPU, memory, GPU, and family filters. |
| `find_cheapest_region` | Find the cheapest region for any cloud service (concurrent fan-out across all regions). |
| `find_available_regions` | Find all regions where a specific service/instance type is available, sorted cheapest-first. |

---

## Example tool calls

### Single instance price

```json
{
  "tool": "get_price",
  "arguments": {
    "spec": {
      "provider": "aws",
      "domain": "compute",
      "resource_type": "m5.xlarge",
      "region": "us-east-1",
      "os": "Linux",
      "term": "on_demand"
    }
  }
}
```

### Cross-cloud BOM comparison

```json
{
  "tool": "compare_bom",
  "arguments": {
    "workload": {
      "app_servers": {"type": "compute", "vcpus": 4, "memory_gb": 16, "quantity": 3},
      "database":    {"type": "database", "vcpus": 2, "memory_gb": 8, "engine": "mysql"},
      "block_store": {"type": "storage", "storage_type": "ssd", "storage_gb": 500}
    },
    "region_preference": "us",
    "providers": ["aws", "gcp", "azure"],
    "terms": ["on_demand", "reserved_1yr"]
  }
}
```

`workload` is a named map — each key is a logical component. `region_preference` picks the region tier ("us", "eu", or "apac"); the tool selects the closest matching region per provider automatically.

Returns a side-by-side cost breakdown per provider per term, with total monthly cost and savings vs on-demand.

### Cheapest region for a workload

```json
{
  "tool": "find_cheapest_region",
  "arguments": {
    "spec": {
      "provider": "aws",
      "domain": "compute",
      "resource_type": "p3.2xlarge",
      "os": "Linux",
      "term": "on_demand"
    }
  }
}
```

Fans out concurrently across all available regions (up to 32 in parallel) and returns results sorted cheapest-first. Pass `regions: [...]` to limit the search to specific regions.

---

## Health check (HTTP transport)

```
GET /healthz  →  200 OK  (liveness — process is running)
GET /readyz   →  200 OK  (readiness — 503 until providers are initialized)
```

Use `/healthz` for liveness probes and `/readyz` for readiness probes in Kubernetes or Docker health checks.

---

## Docker deployment

The Dockerfile produces a minimal distroless image (~15 MB):

```dockerfile
FROM golang:1.25.11-alpine AS builder
WORKDIR /src
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
      -ldflags="-X main.version=${VERSION}" \
      -o /opencloudcosts ./cmd/opencloudcosts

FROM scratch
COPY --from=builder /opencloudcosts /opencloudcosts
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
EXPOSE 8080
ENTRYPOINT ["/opencloudcosts"]
CMD ["--transport", "http", "--host", "0.0.0.0", "--port", "8080"]
```

Run with credentials via environment:

```bash
docker run --rm -p 8080:8080 \
  -e OCC_API_KEY=your-secret-token \
  -e OCC_GCP_API_KEY=your-gcp-key \
  -v /path/to/cache:/root/.cache/opencloudcosts \
  opencloudcosts-go:latest
```

### Kubernetes

```yaml
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      containers:
        - name: opencloudcosts
          image: your-registry/opencloudcosts-go:v1.0.0
          ports:
            - containerPort: 8080
          env:
            - name: OCC_API_KEY
              valueFrom:
                secretKeyRef:
                  name: opencloudcosts-secrets
                  key: api-key
            - name: OCC_GCP_API_KEY
              valueFrom:
                secretKeyRef:
                  name: opencloudcosts-secrets
                  key: gcp-api-key
          livenessProbe:
            httpGet:
              path: /healthz
              port: 8080
          readinessProbe:
            httpGet:
              path: /readyz
              port: 8080
```

---

## Reliability

opencloudcosts-go v1.0.0 achieves **234/234 (100%) on the LLM grounding test harness** — 234 realistic pricing questions across all three clouds, zero XML hallucinations, tested against a 35B reasoning model. The harness covers spot checks, region comparisons, BOM estimates, multi-cloud comparisons, unit economics, AI model pricing, database pricing, storage pricing, discount summaries, and availability queries.

The 108 Go unit tests in `TEST_PARITY.md` verify behavioral parity across all tools.

---

## Module

```
github.com/x7even/cloudcostsmcp/opencloudcosts-go
```

Source lives at `opencloudcosts-go/` inside the `cloudcostmcp` monorepo.
