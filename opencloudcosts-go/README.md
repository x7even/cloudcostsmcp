# opencloudcosts-go

Go implementation of the OpenCloudCosts MCP server. Functional replacement for the
Python implementation in `src/opencloudcosts/`. Same 15 MCP tools, same environment
variables, same wire protocol — drop-in swap at the transport layer.

## Quick start

```bash
# Build (static binary, no CGO, no system dependencies)
CGO_ENABLED=0 go build -o opencloudcosts ./cmd/opencloudcosts

# stdio (default — for local MCP clients like Claude Code)
./opencloudcosts

# HTTP transport (for remote/container deployments)
./opencloudcosts --transport http --host 0.0.0.0 --port 8080
```

## Binary flags

| Flag | Default | Env override | Description |
|------|---------|-------------|-------------|
| `--transport` | `stdio` | — | `stdio` or `http` |
| `--host` | `127.0.0.1` | `OCC_HTTP_HOST` | HTTP bind address |
| `--port` | `8080` | `OCC_HTTP_PORT` | HTTP port |

## Environment variables

All variables are identical to the Python implementation. The Go binary reads them
with the same defaults.

### Caching

| Variable | Default | Description |
|----------|---------|-------------|
| `OCC_CACHE_DIR` | `~/.cache/opencloudcosts` | Cache file directory |
| `OCC_CACHE_TTL_HOURS` | `24` | Price cache TTL (hours) |
| `OCC_METADATA_TTL_DAYS` | `7` | Region/instance metadata TTL (days) |
| `OCC_EFFECTIVE_PRICE_TTL_HOURS` | `1` | Effective/contracted price TTL (hours) |
| `OCC_SPOT_CACHE_TTL_MINUTES` | `5` | Spot price history TTL (minutes) |

### General

| Variable | Default | Description |
|----------|---------|-------------|
| `OCC_DEFAULT_CURRENCY` | `USD` | Response currency |
| `OCC_DEFAULT_REGIONS` | `us-east-1,us-west-2` | Comma-separated default region list |
| `OCC_MAX_RESULTS` | `20` | Max results for list/search tools |
| `OCC_LOG_LEVEL` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |
| `OCC_API_KEY` | _(none)_ | Bearer token for HTTP transport auth |
| `OCC_RATE_LIMIT` | `10` | Requests/second per client |
| `OCC_REQUEST_TIMEOUT` | `30s` | Per-request deadline |
| `OCC_PROVIDER_TIMEOUT` | `15s` | Per-provider API call deadline |
| `OCC_SHUTDOWN_TIMEOUT` | `10s` | Graceful shutdown drain period (SIGTERM) |

### AWS

| Variable | Default | Description |
|----------|---------|-------------|
| `OCC_AWS_REGION` | `us-east-1` | AWS SDK default region |
| `OCC_AWS_PROFILE` | _(none)_ | AWS credentials profile name |
| `OCC_AWS_ENABLE_COST_EXPLORER` | `false` | Enable Cost Explorer (paid API, $0.01/call) |
| `AWS_ACCESS_KEY_ID` | _(none)_ | Standard AWS credential env var |
| `AWS_SECRET_ACCESS_KEY` | _(none)_ | Standard AWS credential env var |

AWS public pricing (EC2, EBS, RDS list prices) works without any credentials.
Credentials are only needed for `get_effective_price` and `get_discount_summary`.

### GCP

| Variable | Default | Description |
|----------|---------|-------------|
| `OCC_GCP_PROJECT_ID` | _(none)_ | GCP project ID |
| `OCC_GCP_API_KEY` | _(none)_ | API key (catalog prices only, no effective pricing) |
| `OCC_GCP_SERVICE_ACCOUNT_JSON` | _(none)_ | Service account JSON inline |
| `OCC_GCP_SERVICE_ACCOUNT_JSON_B64` | _(none)_ | Service account JSON, base64-encoded |
| `OCC_GCP_EXTERNAL_ACCOUNT_JSON` | _(none)_ | Workload Identity / external account JSON inline |
| `OCC_GCP_EXTERNAL_ACCOUNT_JSON_B64` | _(none)_ | Same, base64-encoded |
| `OCC_GCP_ACCESS_TOKEN` | _(none)_ | Short-lived access token |
| `OCC_GCP_ACCESS_TOKEN_EXPIRES_AT` | _(none)_ | RFC3339 expiry for the access token |
| `OCC_GCP_BILLING_ACCOUNT_ID` | _(none)_ | Billing account for CUD summaries |
| `OCC_GCP_BILLING_DATASET` | _(none)_ | BigQuery dataset for billing export |
| `GOOGLE_APPLICATION_CREDENTIALS` | _(none)_ | Standard ADC path (SA JSON file) |

GCP public pricing (Compute Engine, Cloud Storage, etc.) works with just `OCC_GCP_API_KEY`.

### Azure

No credentials required — Azure pricing uses the public Retail Prices API.

## Health check (HTTP transport only)

```
GET /healthzz  →  200 OK   (liveness — always up while process is running)
GET /readyz   →  200 OK   (readiness — 503 until cache and providers are ready)
```

Use `/healthz` for liveness probes and `/readyz` for readiness probes.
`GET /` is owned by the MCP handler and returns 400 for plain HTTP requests.

## Docker

Multi-stage build — final image is distroless scratch, ~15 MB.

The committed `Dockerfile` uses `COPY . .` so the build context must be the
`opencloudcosts-go/` directory (where `go.mod` lives):

```bash
# Build from within the opencloudcosts-go/ subdirectory
cd opencloudcosts-go
docker build -t opencloudcosts-go:local .
docker run --rm -p 8080:8080 opencloudcosts-go:local

# Or from the repo root, pass the subdirectory as the build context
docker build -f opencloudcosts-go/Dockerfile -t opencloudcosts-go:local opencloudcosts-go/
```

Dockerfile contents (for reference):

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

## Kubernetes

Same manifests as the Python deployment (`deploy/kubernetes/`). Only the image tag
changes. Replace the `image:` line in `deployment.yaml`:

```yaml
image: ghcr.io/x7even/opencloudcosts-go:latest   # Go binary
# was:
# image: ghcr.io/x7even/opencloudcosts:latest    # Python
```

Credentials via environment variables or Kubernetes Secrets are identical — same
variable names, same semantics.

## Differences from the Python implementation

| | Python | Go |
|-|--------|-----|
| Runtime | `uv run` / uvicorn | Single static binary |
| Stdio | ✓ | ✓ |
| Streamable HTTP | ✓ | ✓ |
| CGO | — | `CGO_ENABLED=0` always |
| Cache file | `~/.cache/opencloudcosts/cache.json` | same path |
| Log format | structlog JSON | `log/slog` JSON (same schema) |
| `--version` flag | ✓ | ✓ |
| Graceful SIGTERM | ✓ | ✓ (configurable via `OCC_SHUTDOWN_TIMEOUT`) |

## Module path

```
github.com/x7even/cloudcostsmcp/opencloudcosts-go
```

Source lives at `opencloudcosts-go/` inside the `cloudcostmcp` monorepo.
