# T40 — Go Rewrite of OpenCloudCosts MCP Server

**Status:** planning  
**Effort:** Large (multi-phase, parallel agents)  
**Goal:** Full Go port of the Python server with 100% functional parity, validated by the local harness passing at parity with the current Python baseline (186 prompts).

---

## 1. Why Go: Honest Evaluation

### 1.1 Valid Reasons

**Distribution (primary driver)**  
The current Python server requires Python 3.11+, `uv`, and a correctly resolved virtual environment. Every MCP client that runs the server as a subprocess inherits that dependency chain. A Go binary ships as a single statically-linked executable with zero runtime dependencies — installable via `brew install`, `curl | sh`, or a direct binary download. This eliminates the #1 onboarding friction point for self-hosted use.

**Cold-start latency (strong secondary)**  
The stdio transport starts a new process per Claude Code session. Python + boto3 + pydantic cold-start is ~800ms–1.5s. A Go binary cold-starts in ~30–80ms. For stdio, every conversation pays this tax — Go is ~10–20× faster here.

**Concurrency model**  
`find_cheapest_region` and `find_available_regions` fan out to 12–30 regions concurrently. In Python this is `asyncio.gather` over an event loop. In Go it's goroutines with `sync.WaitGroup` or `errgroup` — simpler to reason about, no event loop to block, and naturally bounded with `semaphore`. The Go model is a better fit for this access pattern.

**Container image size**  
Current Docker image: ~400MB (Python slim + dependencies). Go scratch image: ~20MB. Relevant for Kubernetes sidecar deployments or shared team servers.

**Type safety at compile time**  
The Python codebase runs mypy in strict mode, but mypy catches errors at lint time, not build time. Go's type system catches mismatches at `go build`. No runtime `AttributeError` surprises in provider SKU mapping code.

**Single binary for CI and deployment**  
`go build` produces a self-contained binary. No `uv sync`, no venv activation, no `pyproject.toml` resolution step in CI.

### 1.2 Weaker / Not Justified Reasons

**"Performance"** — The hot path (pricing lookups) is I/O bound: AWS Pricing API, GCP Billing Catalog REST, Azure Retail Prices REST. Go will not meaningfully speed up individual pricing queries. This is not a reason for the rewrite.

**"Go is simpler"** — FastMCP abstracts all MCP protocol handling. The Python code is well-structured and type-annotated. Rewriting in Go does not simplify the implementation; it trades one kind of complexity for another.

### 1.3 Honest Costs

- **11,612 lines of hard-won provider logic** must be ported exactly. T18–T39 represent accumulated SKU quirks, region normalization edge cases, discriminated-union `PricingSpec` routing, and multi-method GCP auth. This is where parity breaks.
- **Lifespan context injection differs** — FastMCP's `yield {"providers": ..., "cache": ...}` dict has no direct Go equivalent. The Go SDK handles schema generation, transport, and tool dispatch, but shared state (providers, cache) is injected via closure binding (initialize a `Server` struct, register its methods as handlers). This is idiomatic Go, not a missing SDK feature. The `modelcontextprotocol/go-sdk` v1.6.1 auto-generates JSON schemas from Go struct types via reflection (same spirit as FastMCP's Pydantic-based generation) — a 15-tool server is ~21 lines of boilerplate per tool, not from-scratch protocol work.
- **Python `Decimal` vs Go `float64`** — use `float64`. The operations in this codebase are: parse an API price string → multiply by a fixed constant (730, 24, a quantity) → marshal to JSON. `float64` has 15–17 significant digits; Go's JSON serializer uses shortest-repr (`strconv.FormatFloat`, `-1` precision), so `0.192 * 730` marshals as `"140.16"`, not `"140.16000000000001"`. No concrete failing case exists for this math. `shopspring/decimal` adds a dependency without a demonstrated necessity — convention and "might accumulate" are not sufficient justification. If a specific harness prompt fails numeric grounding and root-cause analysis confirms float64 divergence (not schema drift or missing tool calls), that is the trigger to revisit. Not before.
- **Feature freeze** — T26 (GCP effective pricing via BigQuery) and all subsequent roadmap items are deferred until post-Go. No new Python work. Building T26 in Python only to immediately rewrite it is wasted toil — it goes straight to Go after cutover.
- **MCP tool JSON schemas must be byte-equivalent** — the LLM's behavior in the harness depends on parameter names and descriptions. Any schema drift will change harness outcomes for reasons unrelated to pricing correctness.

### 1.4 Go/No-Go Recommendation

**Proceed.** The distribution and cold-start benefits are real and compounding. The risk is manageable with (a) a systematic phase-by-phase port, (b) the 186-prompt harness as the parity oracle, and (c) a parallel run period before cutover. The rewrite is not a weekend project — budget 6–8 weeks of focused parallel work — but the end state is a materially better artifact for users.

---

## 2. Parity Definition

### 2.1 What "100% parity" means

| Layer | Python | Go target | Oracle |
|-------|--------|-----------|--------|
| MCP transport | FastMCP stdio + streamable-http | `modelcontextprotocol/go-sdk` v1.6.1+ | Protocol-level: same tool names, schemas, response shapes |
| Tool behavior | 15 tools across 4 categories | Same 15 tools, same input/output contract | Harness pass rate |
| Provider logic | AWS/GCP/Azure pricing | Go ports of all three providers | Unit tests (Go) + harness |
| Cache | SQLite via aiosqlite, schema versioned | In-memory map + JSON file, stdlib only | Cache test suite |
| Config | Pydantic settings, `OCC_` prefix | Env var parsing with same `OCC_` names | Smoke test |
| Auth | boto3 chain, GCP multi-method, Azure public | aws-sdk-go-v2 chain, google-auth-go, Azure public | Auth integration tests |
| Numeric precision | `Decimal` throughout | `shopspring/decimal` throughout | Harness grounding checks |

### 2.2 The harness as parity oracle

The local harness at `local-test-harness/` contains **186 prompts** (note: README says 109, roadmap says 123 — both outdated; the code has 186 as of this writing). It is **not rewritten** as part of this task — it stays Python and drives the Go binary over stdio or HTTP transport identically to how it drives Python today.

Cutover gate: **Go binary must hit ≥ the current Python baseline pass rate on all 186 prompts** before the Python implementation is retired. Run the harness against both binaries in parallel during the transition period to differential-compare failures.

The two test layers distinguished:

| Layer | What gets rewritten | Purpose |
|-------|---------------------|---------|
| `tests/` (pytest, 9.4k lines) | Yes — ported to Go `testing` | Provider/unit correctness |
| `local-test-harness/` (Python, 186 prompts) | No — stays Python | End-to-end parity oracle |

---

## 3. Go Technology Stack

### 3.1 MCP SDK

**Recommendation: `modelcontextprotocol/go-sdk` v1.6.1**

| Capability | Support |
|-----------|---------|
| Stdio transport | ✅ |
| Streamable-HTTP transport (MCP 2025-03-26) | ✅ (added v1.6.0) |
| Tool schema definition | ✅ |
| Tool call handling | ✅ |
| Server lifespan hooks | ✅ |

The official SDK's schema approach (standard Go struct tags + JSON schema generation via reflection) is sufficient for our 15-tool surface.

### 3.2 Dependency Mapping

| Python | Go replacement | Notes |
|--------|---------------|-------|
| `boto3` | `aws-sdk-go-v2`: `service/pricing`, `service/costexplorer`, `service/ec2`, `service/savingsplans` | Official AWS SDK — no alternative. **Gotcha:** `pricing.GetProducts` returns `[]string`; each element is raw JSON requiring manual `json.Unmarshal` and tree navigation. `costexplorer` has no paginator; loop on `NextPageToken` manually. |
| `google-auth` (auth paths) | `golang.org/x/oauth2/google` | Go team extended stdlib — not third-party. Covers all 5 auth paths: ADC, service account JSON (raw or base64), short-lived access token, WIF. Obtain a `TokenSource`, set `Authorization: Bearer <token>` on `net/http` requests manually. |
| `google-auth` (API key) | `net/http` query param | API key auth appends `?key=<key>` to the URL. No library. |
| `google.golang.org/api/option` | — (removed) | Only needed for generated Google API clients. We call the GCP Billing Catalog over plain `net/http` — not required. |
| `httpx` (async HTTP) | `net/http` stdlib | All provider calls are GET with query parameters. No third-party HTTP client justified. Goroutines replace asyncio concurrency. |
| `go-resty/resty/v2` | — (removed) | Not justified. `net/http` covers every call in this codebase. |
| `tenacity` (retry) | stdlib (`time`, `context`) | Retry logic lives in `internal/utils/http_retry.go`: exponential backoff, skip 4xx as permanent, honour context cancellation. ~20 lines. No library justified. |
| `aiosqlite` | `sync.RWMutex` + `encoding/json` (stdlib) | In-memory `map[string]CacheEntry` loaded from a JSON file on startup, flushed on writes via temp-file-then-rename (atomic). O(1) lookup, provider-scoped clear by key prefix, TTL checked on read. No external dependency. Eliminates WAL complexity, schema versioning, and the `modernc.org/sqlite` transpiled-C dependency entirely. |
| `pydantic-settings` | `os.Getenv` + helpers (stdlib) | ~18 `OCC_` fields. Explicit `os.Getenv` with typed helper functions is ~80 lines of clear, testable code. No library justified. |
| `pydantic` models | Plain Go structs + `encoding/json` | Validate at system boundaries. `PricingSpec` discriminated union requires hand-written two-pass `UnmarshalJSON` — see Agent 1A spike. |
| `decimal.Decimal` | `float64` + `encoding/json` (stdlib) | Go's shortest-repr JSON serializer produces correct output for all pricing math here. Introduce `shopspring/decimal` only if a harness failure is root-caused to float64 divergence. |
| `FastMCP` | `modelcontextprotocol/go-sdk` v1.6.1 | Official MCP SDK. Struct-based schema inference, stdio + streamable-http transport, tool dispatch. |
| `pytest` | `testing` stdlib + interface mocks | `testify/mock` not justified — Go interface-based mocks are idiomatic and explicit. `testify/assert` is test-only and borderline; decision deferred. |

---

## 4. Architecture Mapping

### 4.1 Module structure

```
opencloudcosts-go/
├── cmd/
│   └── opencloudcosts/
│       └── main.go              # Entry point: CLI flags, create server, run transport
├── internal/
│   ├── config/
│   │   └── config.go            # OCC_ env vars → Config struct (mirrors config.py)
│   ├── models/
│   │   └── models.go            # CloudProvider, PricingSpec, NormalizedPrice, etc.
│   ├── cache/
│   │   └── cache.go             # CacheManager: sync.RWMutex map + JSON file persistence
│   ├── providers/
│   │   ├── base.go              # CloudPricingProvider interface + ProviderBase
│   │   ├── aws/
│   │   │   └── aws.go           # AWSProvider: pricing, CE, EC2, SP APIs
│   │   ├── gcp/
│   │   │   ├── gcp.go           # GCPProvider: Billing Catalog REST
│   │   │   └── auth.go          # GcpAuthProvider: ADC, SA, access token, WIF
│   │   └── azure/
│   │       └── azure.go         # AzureProvider: Retail Prices REST API
│   ├── tools/
│   │   ├── lookup.go            # get_price, get_prices_batch, compare_prices, search_pricing, describe_catalog
│   │   ├── availability.go      # list_regions, list_instance_types, find_cheapest_region, find_available_regions
│   │   ├── bom.go               # estimate_bom, estimate_unit_economics
│   │   └── cache.go             # refresh_cache, cache_stats
│   ├── utils/
│   │   ├── money.go             # Decimal arithmetic, currency formatting
│   │   ├── regions.go           # Region normalization
│   │   ├── spec_infer.go        # PricingSpec inference
│   │   ├── baseline.go          # Regional comparison baseline
│   │   ├── egress_tiers.go      # Egress pricing tiers
│   │   ├── gcp_specs.go         # GCP spec mapping
│   │   ├── http_retry.go        # HTTP retry with exponential backoff + jitter
│   │   └── units.go             # Unit conversions
│   └── server/
│       └── server.go            # MCP server wiring, tool registration, lifespan
└── go.mod
```

### 4.2 Transport wiring

```go
// cmd/opencloudcosts/main.go
func main() {
    // flags: --transport stdio|http, --host, --port
    // identical OCC_HTTP_HOST / OCC_HTTP_PORT env overrides
    srv := server.New(cfg)
    if transport == "http" {
        srv.RunHTTP(host, port)  // streamable-http via go-sdk
    } else {
        srv.RunStdio()           // stdio via go-sdk
    }
}
```

### 4.3 Tool registration pattern

```go
// internal/server/server.go
func (s *Server) registerTools(mcpServer *mcp.Server) {
    // Each tool: name, description, input schema (JSON Schema struct), handler func
    mcpServer.AddTool(mcp.Tool{
        Name:        "get_price",
        Description: "...",  // MUST match Python exactly
        InputSchema: getPriceSchema(),
    }, s.handleGetPrice)
}
```

**Critical:** Tool `Name`, `Description`, and parameter names must be byte-identical to the Python implementation. The harness relies on the LLM calling tools by name based on descriptions — any drift changes harness behavior.

### 4.4 Decimal arithmetic

All price values use `shopspring/decimal.Decimal`, not `float64`:

```go
// internal/utils/money.go
type Money = decimal.Decimal

func MonthlyEstimate(hourlyRate decimal.Decimal) decimal.Decimal {
    return hourlyRate.Mul(decimal.NewFromInt(730))
}
```

The Python implementation uses `decimal.Decimal`. The Go port uses `float64` — the operations (parse API string, multiply by fixed constant, marshal to JSON) do not require a decimal library. Use `strconv.ParseFloat` to ingest API price strings. If during Phase 4 harness runs a numeric grounding failure is traced to float64 divergence, add `shopspring/decimal` at that point with the failing test as justification.

---

## 5. Implementation Phases and Agent Fan-Out

The implementation is structured in four phases with maximum parallelism within each phase. Each numbered "Agent" below maps to a parallel subagent that can be dispatched simultaneously with its peers.

### Phase 0: Prerequisites (sequential, ~3 days)

**Before any porting begins:**

1. **Feature freeze**: Python is frozen now. T26 and all subsequent roadmap items are deferred to Go — no new Python work of any kind before or during the port.
2. **Establish baseline**: Run the full 186-prompt harness against the current Python server and record the pass count. This is the cutover gate.
3. **Go module init**: Create `opencloudcosts-go/` directory, `go.mod`, CI workflow.
4. **Schema snapshot**: Export all 15 MCP tool JSON schemas from the Python server (via `mcp.list_tools()` or direct FastMCP introspection). This is the source of truth for tool schema parity.

---

### Phase 1: Foundation (parallel, ~1 week)

Three agents run in parallel. They share no files.

**Agent 1A — Config + Models (PricingSpec spike first)**  
Port `config.py` and `models.py` to Go. Begin with a mandatory spike on `PricingSpec` deserialization before any other model work.

- **PricingSpec spike (do this before anything else):** Since v0.8.0, `get_price(spec)` is the unified entry point and every tool call flows through a discriminated-union `PricingSpec`. Pydantic auto-deserializes JSON into the correct variant using a `domain` discriminator field. Go has no equivalent — requires hand-written two-pass `UnmarshalJSON`: peek the `domain` field into a `json.RawMessage`, then `json.Unmarshal` into the concrete type. This must be byte-compatible with Pydantic's discriminated union behavior. Write table-driven round-trip tests covering all spec variants (`ComputeSpec`, `StorageSpec`, `DatabaseSpec`, `NetworkSpec`, etc.) before any provider code depends on `PricingSpec`. If deserialization diverges, every tool call fails and failures look like pricing bugs, not serialization bugs.
- `internal/config/config.go`: All `OCC_` env vars parsed with `os.Getenv` and typed helper functions (`envString`, `envInt`, `envDuration`, `envBool`). `Config.Validate()` method checks constraints after loading. Exact same field names and defaults as `config.py`. No external library.
- `internal/models/models.go`: All enums (`CloudProvider`, `PricingTerm`, `PricingDomain`, `PriceUnit`), `PricingSpec` (Go interface + concrete types with correct `UnmarshalJSON`), `NormalizedPrice`, `EffectivePrice`, `InstanceTypeInfo`, `ProviderCatalog`, `PricingResult`.
- Deliverable: `go test ./internal/config/... ./internal/models/...` passes, including PricingSpec round-trip tests for all spec variants.

**Agent 1B — Cache Layer**  
Reimplement `cache.py` in Go — this is a clean redesign, not a schema port.
- `internal/cache/cache.go`: `CacheManager` — an in-memory `map[string]cacheEntry` protected by `sync.RWMutex`. `cacheEntry` holds `Value []byte`, `FetchedAt time.Time`, `TTL time.Duration`.
- Persistence: on startup, load from `~/.cache/opencloudcosts/cache.json` if the file exists. On every write, flush the full map to a temp file then `os.Rename` (atomic). No partial-write corruption possible.
- Same TTLs: prices 24h, metadata 7 days, effective prices 1h — enforced on `Get` (check `FetchedAt + TTL > now`).
- Same operations: `Get(key)`, `Set(key, value, ttl)`, `GetMetadata(key)`, `SetMetadata(key, value, ttl)`, `ClearProvider(prefix)`, `Stats()` (entry count + file size), `Close()` (final flush).
- Provider-scoped clear: iterate map, delete all keys where `strings.HasPrefix(key, provider+":")`.
- No schema versioning needed — JSON is self-describing; old entries with unknown fields are silently ignored on load.
- No external dependency. No CGO. No WAL.
- **Note for cutover:** the Go cache file (`cache.json`) is a fresh start — existing Python SQLite caches are not migrated. Users get a cold cache on first run of the Go binary, which is acceptable (pricing data refetches within seconds).
- Deliverable: `go test ./internal/cache/...` passes (same behavioural coverage as `test_cache.py`).

**Agent 1C — MCP Server Scaffold**  
Wire the `modelcontextprotocol/go-sdk` server, transport, and shared state injection.
- `internal/server/server.go`: `AppServer` struct holding providers map + cache. Methods on this struct become tool handlers — closures replace FastMCP's lifespan context dict.
- `New(cfg *config.Config) *AppServer`: initializes cache and provider clients; sets up the `mcp.Server` and calls `mcp.AddTool` for each of the 15 tools pointing to receiver methods.
- `RunStdio()`: `server.Run(ctx, &mcp.StdioTransport{})` — 1 line.
- `RunHTTP(host, port)`: `http.ListenAndServe(addr, mcp.NewStreamableHTTPHandler(...))` — 3 lines.
- Tool schemas are **auto-generated by the SDK** from input struct types via reflection — no manual JSON Schema writing.
- Stub tool handlers return `{"status": "not implemented"}` initially (providers not yet ported).
- Deliverable: `go run ./cmd/opencloudcosts` starts, connects to Claude Code over stdio, lists all 15 tools with names and schemas that match the Phase 0 Python schema snapshot.

---

### Phase 2: Providers (parallel, ~2 weeks)

Three agents run in parallel. They share `internal/models/` and `internal/cache/` from Phase 1 (read-only).

**Agent 2A — AWS Provider**  
Port `providers/aws.py` to Go.
- `internal/providers/aws/aws.go`: `AWSProvider` struct.
- Dependencies: `aws-sdk-go-v2`: `pricing`, `costexplorer`, `ec2`, `savingsplans`.
- **AWS SDK gotchas to handle explicitly:**
  - `pricing.GetProducts` returns `[]string` — each element is raw JSON. Must `json.Unmarshal` each string and navigate the nested `product.attributes` / `terms.OnDemand` / `terms.Reserved` tree manually (no typed structs).
  - `costexplorer.GetCostAndUsage` has no paginator helper — loop manually on `NextPageToken` (field name differs from the standard `NextToken`). All monetary amounts come back as strings; parse with `shopspring/decimal.NewFromString`.
  - `savingsplans` pagination is also manual (`result.NextToken != nil`).
  - `ec2.DescribeSpotPriceHistory` has a proper paginator (`DescribeSpotPriceHistoryPaginator`). Results include one price from before `StartTime` — filter appropriately.
  - Cost Explorer endpoint is hardcoded to `us-east-1` regardless of configured region.
- Implement all methods: `get_compute_price`, `get_storage_price`, `get_database_price`, `search_pricing`, `list_regions`, `list_instance_types`, `check_availability`, `get_effective_price`, `get_spot_history`, `get_discount_summary`, `describe_catalog`, `bom_advisories`, `major_regions`, `default_region`.
- Port all SKU filter maps: `_RESERVED_FILTERS`, compute/storage/database attribute sets, spot pricing path.
- Use `internal/utils/http_retry.go` for the AWS bulk pricing endpoint retry: exponential backoff, 4xx treated as permanent (no retry), context cancellation respected.
- Deliverable: `go test ./internal/providers/aws/...` passes (mirrors `test_providers/test_aws.py`).

**Agent 2B — GCP Provider**  
Port `providers/gcp.py` and `providers/gcp_auth.py` to Go.
- `internal/providers/gcp/gcp.go`: `GCPProvider` struct.
- `internal/providers/gcp/auth.go`: `GcpAuthProvider` — all 5 auth methods:
  1. API key (query param)
  2. Service account JSON (raw or base64)
  3. Access token (short-lived, no refresh)
  4. ADC (`google.golang.org/api/option.WithTokenSource(google.DefaultTokenSource(...))`)
  5. Workload Identity Federation (external account JSON via `golang.org/x/oauth2/google`)
- GCP Billing Catalog REST via `net/http` (no GCP Go SDK needed — it's a public REST API).
- Port `utils/gcp_specs.go` (instance family → SKU prefix mapping).
- Port Windows pricing logic (T31: separate per-vCPU Windows license SKU lookup).
- Port GCP major regions list (T22).
- Deliverable: `go test ./internal/providers/gcp/...` passes (mirrors `test_providers/test_gcp.py` + domain-specific tests).

**Agent 2C — Azure Provider**  
Port `providers/azure.py` to Go.
- `internal/providers/azure/azure.go`: `AzureProvider` struct.
- Azure Retail Prices REST API via `net/http` (fully public, no auth).
- Implement all methods: `get_compute_price`, `get_storage_price`, `get_database_price`, `search_pricing`, `list_regions`, `list_instance_types`, `check_availability`, `describe_catalog`, `major_regions`, `default_region`.
- Port reserved pricing term handling and unit normalization (T38 fix: reserved pricing unit conversion).
- Deliverable: `go test ./internal/providers/azure/...` passes (mirrors `test_providers/test_azure.py`).

---

### Phase 3: Utilities + Tools (parallel, ~1.5 weeks)

Phase 3 agents run in parallel after Phase 2 completes. Utility agents can start during Phase 2 if the `models` package is stable.

**Agent 3A — Utilities**  
Port all of `src/opencloudcosts/utils/` to Go.
- `money.go`: `shopspring/decimal` wrappers, currency formatting, monthly estimate (`×730`).
- `regions.go`: Region normalization (AWS `us-east-1` ↔ GCP `us-east1` ↔ Azure `eastus`).
- `spec_infer.go`: `PricingSpec` inference from free-text / partial inputs.
- `baseline.go`: Regional price delta computation against baseline region.
- `egress_tiers.go`: Egress tiered pricing (NET_EGR, GCPEGR, AZEGR categories).
- `gcp_specs.go`: GCP instance family → SKU prefix mapping.
- `http_retry.go`: Exponential backoff + jitter, max attempts, idempotency.
- `units.go`: Unit conversions (GB, TB, per-hour, per-month).
- Deliverable: `go test ./internal/utils/...` passes (mirrors all `test_utils/` tests).

**Agent 3B — Lookup Tools**  
Port `tools/lookup.py` to Go (5 tools).
- `internal/tools/lookup.go`: `get_price`, `get_prices_batch`, `compare_prices`, `search_pricing`, `describe_catalog`.
- Tool handlers: receive `mcp.CallToolRequest`, decode params, dispatch to provider, return `mcp.CallToolResult` with identical JSON response shape.
- No-results hint responses (T25) must be preserved exactly.
- `get_spot_history` also lives here.
- Deliverable: `go test ./internal/tools/...` (lookup subtests pass).

**Agent 3C — Availability Tools**  
Port `tools/availability.py` to Go (4 tools).
- `internal/tools/availability.go`: `list_regions`, `list_instance_types`, `find_cheapest_region`, `find_available_regions`.
- Fan-out concurrency: replace `asyncio.gather` with `errgroup.WithContext` + goroutines for region parallel fetch.
- GCP major-region scoping (T22) preserved.
- `note` field on scoped responses preserved.
- Deliverable: `go test ./internal/tools/...` (availability subtests pass).

**Agent 3D — FinOps + Cache Tools**  
Port `tools/bom.py` + cache tools to Go (5 tools).
- `internal/tools/bom.go`: `estimate_bom`, `estimate_unit_economics`, `get_discount_summary`.
- `internal/tools/cache.go`: `refresh_cache`, `cache_stats`.
- BoM `os` field for Windows pricing (T20) preserved.
- `estimate_bom` database item routing (AWS RDS + GCP Cloud SQL) preserved (T27).
- Deliverable: `go test ./internal/tools/...` (bom/cache subtests pass).

---

### Phase 4: Integration + Harness (sequential, ~1 week)

**Agent 4A — Full Integration**  
Wire all tool handlers into the MCP server, run the full Go test suite.
- Register all 15 tools in `internal/server/server.go`.
- Run `go test ./...` — all unit and provider tests must pass.
- Manual smoke: connect Claude Code to the Go binary via stdio, call each of the 15 tools once.
- Fix any integration-layer bugs.

**Agent 4B — Harness Differential Run**  
Run the 186-prompt harness against the Go binary and compare to the Python baseline.
- Start the Go binary: `./opencloudcosts-go --transport http --port 9090`
- Point harness at `http://localhost:9090`
- Run all 186 prompts: `uv run local-test-harness/run_tests.py`
- Compare pass/fail counts between Go and Python runs.
- Investigate and fix any Go-specific failures (likely: schema drift, Decimal formatting, missing edge cases in SKU filters).
- Cutover gate: Go pass rate ≥ Python baseline.

---

## 6. Security Review

A dedicated security review agent runs **after Phase 3, before cutover**. Scope:

### 6.1 Credential handling
- **AWS auth chain** (`internal/providers/aws/`): Confirm no credentials are logged, error messages never contain key material, the chain falls back gracefully (anonymous → profile → env → instance role).
- **GCP multi-method auth** (`internal/providers/gcp/auth.go`): Five auth paths. Verify service account JSON (including base64 decode path) is never written to disk or logged. Access token short-circuit path must not cache tokens past expiry. WIF external account JSON must be validated before use.
- **OCC_API_KEY bearer token** (HTTP transport, `internal/server/server.go`): Verify constant-time comparison (`subtle.ConstantTimeCompare`), not plain `==`. Verify the key is not echoed in logs or error responses. Verify missing/wrong key returns 401, not a provider error.

### 6.2 HTTP/network security
- **SSRF risk in provider HTTP calls**: Azure and GCP providers construct URLs from config + API responses. Verify no user-controlled input (provider API response field) is used as a URL component without sanitization.
- **TLS verification**: Confirm `net/http` clients do not set `InsecureSkipVerify: true` anywhere.
- **Redirect following**: Confirm HTTP clients do not blindly follow redirects to internal addresses.

### 6.3 Cache security
- **Key construction** (`internal/cache/`): Cache keys are constructed from provider name and pricing spec fields. Verify no provider API response field is used raw as a cache key without sanitization — a malicious API response could poison or enumerate cache entries via crafted key values.

### 6.4 Supply chain
- Verify `go.sum` is committed and pinned.
- Audit `go.mod` — no indirect dependencies pulling in known-CVE packages.
- Run `govulncheck ./...` before merge.

### 6.5 Information disclosure
- Error responses returned to MCP clients must not include stack traces, file paths, or internal config values.
- Log output (stderr) at INFO level must not include credential values.

**Security reviewer deliverable:** A findings report (`docs/plans/T40-security-review.md`) with pass/warn/fail per scope item. All `fail` items must be resolved before cutover.

---

## 7. Enterprise Readiness

A key differentiator for this project is enterprise-grade quality. The Go rewrite is the opportunity to build these in from the start — not bolt them on. Each item below is a first-class requirement, not a post-launch addition. Agents implementing each phase are responsible for the relevant items in their scope; the integration agent (4A) verifies the full set before cutover.

### 7.1 Structured Logging

Use Go 1.21+ stdlib `log/slog` — no external dependency.

- **Structured JSON output** on stderr: every log line is a JSON object with `time`, `level`, `msg`, and context fields (`provider`, `tool`, `region`, `cache_hit`).
- **Log level** controlled via `OCC_LOG_LEVEL` env var (`debug`, `info`, `warn`, `error`). Default: `info`.
- **Tool call tracing** (HTTP transport): log every incoming tool call with name, parameters (redacted of credential fields), and duration. Log at `debug` level for stdio (too noisy otherwise).
- **Provider errors** logged at `warn` with provider name and HTTP status; retries logged at `debug`.
- **No credential values** appear in any log line at any level — enforced by the security reviewer (Section 6.5).
- **No stack traces** in log output at `info`/`warn` — stack traces only at `error`, formatted as a single `"stack"` JSON field, not multi-line.

### 7.2 Observability Endpoints (HTTP Transport)

When running in HTTP mode, expose two additional endpoints alongside the MCP handler:

**`GET /healthz`** — liveness probe. Returns `200 OK` with `{"status":"ok","version":"<semver>"}` as long as the process is running. No dependency checks — used by Kubernetes liveness probes.

**`GET /readyz`** — readiness probe. Returns `200 OK` only when the cache is initialized and at least one provider is available. Returns `503` with `{"status":"not_ready","reason":"<detail>"}` during startup or if all providers have failed. Used by Kubernetes readiness probes and load balancers.

**`GET /metrics`** — Prometheus-format metrics (optional, enabled via `OCC_METRICS=true`). Expose:
- `occ_tool_calls_total{tool, provider, status}` — counter
- `occ_tool_duration_seconds{tool, provider}` — histogram
- `occ_cache_hits_total{provider}` / `occ_cache_misses_total{provider}` — counters
- `occ_provider_errors_total{provider, error_type}` — counter

These endpoints must not be proxied through the MCP handler — they are served on a separate `net/http.ServeMux` so they remain available even if the MCP handler is overloaded.

### 7.3 Graceful Degradation

The server must remain partially functional when a provider is unavailable:

- **Provider init failure**: if AWS, GCP, or Azure fails to initialize (missing credentials, network error), the server starts anyway and serves the remaining providers. The failed provider returns a structured `{"error": "provider_unavailable", "provider": "aws", "reason": "..."}` on any tool call that routes to it — not a panic, not an unhandled error.
- **Provider API failure mid-request**: HTTP 5xx or timeout from a provider API returns a structured error to the MCP client. The cache is checked first; if a cached value exists (even slightly stale), return it with a `"cache_stale": true` flag and log a warning.
- **Cache failure**: if the JSON cache file is unreadable or corrupt on startup, the server starts with an empty in-memory cache and falls through to live provider APIs. Log an error, do not crash. Writes that fail (disk full, permissions) are logged and skipped — the in-memory cache remains valid for the lifetime of the process.
- **No panics reach the MCP client**: a `recover()` at the tool handler boundary catches any unhandled panics, logs them at `error` level with a stack trace, and returns a structured `{"error": "internal_error"}` to the client.

### 7.4 Request Timeouts and Context Propagation

Every outbound network call — AWS Pricing API, GCP Billing Catalog, Azure Retail Prices — must have a deadline propagated from the MCP request context:

- `OCC_PROVIDER_TIMEOUT` env var (default: `30s`) — applied per-provider API call.
- `OCC_REQUEST_TIMEOUT` env var (default: `60s`) — applied to the entire tool call including cache lookup + provider fetch + response serialization.
- Timeouts surface as `{"error": "timeout", "provider": "aws", "elapsed_ms": 30001}` — not a generic 500.
- Fan-out tools (`find_cheapest_region`, `find_available_regions`) use `errgroup.WithContext` — the group context is cancelled when the parent deadline expires, cleanly aborting all in-flight region fetches.

### 7.5 Rate Limiting (HTTP Transport)

When running as a shared team server over HTTP:

- `OCC_RATE_LIMIT` env var: requests per second per client IP (default: `10`). `0` disables rate limiting.
- Implemented via `golang.org/x/time/rate` (stdlib-adjacent, no external dep).
- Rate-limited requests return `429 Too Many Requests` with a `Retry-After` header.
- Rate limit state is in-process (no Redis dep) — acceptable for single-instance deployments. Document the limitation.

### 7.6 Build and Distribution Quality

- **Multi-arch binaries**: CI builds `linux/amd64`, `linux/arm64`, `darwin/arm64` via `GOOS`/`GOARCH` cross-compile. `CGO_ENABLED=0` throughout — the entire dependency graph is pure Go, so this is unconditional.
- **Reproducible builds**: `go build -trimpath -ldflags="-s -w"`. Embed version via `go build -ldflags="-X main.version=$(git describe --tags)"`.
- **Docker**: multi-stage build (`golang:1.24-alpine` build stage → `scratch` or `gcr.io/distroless/static` runtime). Final image ≤ 30MB.
- **SBOM**: generate with `syft` in CI, attach to GitHub release as `opencloudcosts-<version>-sbom.json`.
- **Vulnerability scanning**: `govulncheck ./...` and `trivy image` on the Docker image in CI — both must pass with zero critical/high findings before release.

### 7.7 Code Quality Gates

Applied in CI to every PR and enforced as merge requirements:

- `go vet ./...` — zero warnings.
- `golangci-lint run` with `errcheck`, `staticcheck`, `gosec`, `exhaustive` (enum switch exhaustiveness), `gocritic` enabled. Zero findings.
- `go test -race ./...` — race detector on all tests. Zero races.
- Coverage gate: `go test -coverprofile=coverage.out ./...` — minimum 80% line coverage per package (matching or exceeding Python test suite coverage).
- `govulncheck ./...` — zero known vulnerabilities in the dependency graph.

### 7.8 Operational Hardening

- **SIGTERM handling**: on SIGTERM, stop accepting new MCP requests, finish in-flight tool calls up to `OCC_SHUTDOWN_TIMEOUT` (default: `15s`), then exit cleanly. Critical for Kubernetes rolling deployments.
- **Connection pooling**: HTTP clients for GCP and Azure use a shared `http.Transport` with `MaxIdleConnsPerHost` tuned to the expected concurrency (fan-out tools spawn ~12–30 concurrent requests; set `MaxIdleConnsPerHost=32`).
- **Cache access**: `sync.RWMutex` — multiple concurrent readers, single writer. `RLock` on `Get`; `Lock` on `Set`/`ClearProvider`. Flush to disk is non-blocking from the caller's perspective (write to temp file + rename is fast; if it fails, in-memory state is unaffected).
- **Version header**: HTTP transport responses include `X-OpenCloudCosts-Version: <semver>` on every response. MCP clients can use this for compatibility checks.

---

## 9. Risk Register

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| **PricingSpec discriminated union deserialization mismatch** | High | Critical | Phase 1A spike: prove round-trip before any provider code depends on it. Every tool call flows through PricingSpec — a silent mismatch breaks the whole surface and looks like pricing bugs. |
| SKU filter logic mismatch (AWS/GCP) | High | High | Port tests first (test-driven port), run harness differentially |
| AWS GetProducts `[]string` navigation errors | High | High | Agent 2A must manually unmarshal each JSON string; unit test against real AWS response fixtures from `test_providers/test_aws.py` mocks |
| Decimal precision drift in monthly estimates | Very Low | Low | `float64` serializes correctly via shortest-repr for all pricing math in this codebase. Not a pre-emptive concern — add `shopspring/decimal` only if a specific harness failure proves it necessary. |
| MCP tool schema drift (param names/descriptions) | Medium | High | Snapshot schemas from Python before porting; automated schema diff in CI |
| GCP multi-method auth breakage | Medium | Medium | Port all 5 auth paths, add integration tests with each |
| Cache cold-start on cutover | Low | Low | Go uses a fresh JSON cache file — users get one cold session on first run. Pricing refetches in seconds. Documented in cutover notes. |
| Go MCP SDK API change | Low | Medium | Pin to `v1.6.1`; monitor releases during port |
| Feature creep into Python during port | Very Low | Low | Python is frozen — T26 and beyond go straight to Go post-cutover, no exceptions. |
| Harness pass rate below Python baseline | Medium | High | Differential run in Phase 4; fix loop before cutover |

---

## 10. Feature Freeze Decision

**Python is frozen. No new features, no T26, no exceptions.**

T26 (GCP effective pricing via BigQuery) and all roadmap items beyond it are deferred to Go post-cutover. Building T26 in Python now means writing it twice — once to throw away, once to keep. That toil is not justified. T26 gets designed and implemented fresh in Go after the port is complete and the harness is passing.

---

## 11. Cutover Strategy

1. **Python and Go run in parallel** during Phase 4.
2. Harness baseline established in Phase 0 (Python pass count).
3. Go binary uses a new `cache.json` file — no migration from the Python SQLite cache. Users start with a cold cache on first run of the Go binary (acceptable; pricing data refetches in seconds).
4. Harness runs against Go binary. Go must match or exceed Python pass rate.
5. Once parity confirmed:
   - Update `examples/` config files (replace `uv run opencloudcosts` with `opencloudcosts` binary path).
   - Update `Dockerfile` (multi-stage Go build → scratch image).
   - Archive Python source to `legacy/` or tag `v0.9.x-python` before removal.
   - Update `README.md` installation section.
6. Python runtime dependency removed. Go binary becomes the canonical distribution.

---

## 12. Success Criteria

**Functional parity**
- [ ] `go build ./cmd/opencloudcosts` produces a single statically-linked binary (`CGO_ENABLED=0`)
- [ ] All 15 MCP tools present with schemas identical to the Phase 0 Python snapshot
- [ ] Harness: 186-prompt run against Go binary ≥ Python baseline pass rate

**Quality gates**
- [ ] `go test -race ./...` passes — zero races
- [ ] Line coverage ≥ 80% per package
- [ ] `go vet ./...` — zero warnings
- [ ] `golangci-lint run` — zero findings (`errcheck`, `staticcheck`, `gosec`, `exhaustive`)
- [ ] `govulncheck ./...` — zero known vulnerabilities

**Security**
- [ ] Security reviewer sign-off: zero `fail` findings (Section 6)
- [ ] Bearer token comparison uses `subtle.ConstantTimeCompare`
- [ ] No credential values appear in logs at any level

**Enterprise readiness**
- [ ] Structured JSON logging via `log/slog`; `OCC_LOG_LEVEL` env var respected
- [ ] `/healthz` returns 200 within 10ms when healthy
- [ ] `/readyz` returns 503 during startup, 200 once cache + ≥1 provider ready
- [ ] Provider init failure serves remaining providers — no hard crash
- [ ] `recover()` at tool handler boundary — no panic surfaces to MCP client
- [ ] SIGTERM drains in-flight requests within `OCC_SHUTDOWN_TIMEOUT` before exit
- [ ] `OCC_PROVIDER_TIMEOUT` and `OCC_REQUEST_TIMEOUT` enforced on all outbound calls

**Distribution**
- [ ] Multi-arch CI builds clean: `linux/amd64`, `linux/arm64`, `darwin/arm64`
- [ ] Docker scratch image ≤ 30MB; `trivy image` zero critical/high CVEs
- [ ] SBOM generated and attached to release artifact
- [ ] Cold-start stdio (`time ./opencloudcosts --help`): < 100ms

**Freeze**
- [ ] Python is frozen — no Python features merged after Phase 0 baseline is cut

---

## 13. Files Changed

**New repository (recommended: `opencloudcosts-go/` alongside current Python repo, or new Git repo)**

- `go.mod`, `go.sum`
- `cmd/opencloudcosts/main.go`
- `internal/config/`, `internal/models/`, `internal/cache/`
- `internal/providers/aws/`, `internal/providers/gcp/`, `internal/providers/azure/`
- `internal/tools/lookup.go`, `internal/tools/availability.go`, `internal/tools/bom.go`, `internal/tools/cache.go`
- `internal/utils/` (7 utility modules)
- `internal/server/server.go`
- `Dockerfile` (updated: Go multi-stage build)
- `docs/plans/T40-security-review.md` (output of security agent)
- All `examples/` client configs updated to point to Go binary

**Unchanged:**
- `local-test-harness/` (stays Python, becomes differential oracle)
- `docs/` (updated incrementally as each phase lands)
