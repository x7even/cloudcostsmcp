# T40 — Go Rewrite Execution Workplan (Ultracode Edition)

How we use Claude to execute the Go rewrite defined in [T40-go-rewrite.md](T40-go-rewrite.md). This document is about the *execution strategy* — which Claude capabilities are used, when, and why — not the technical design (that lives in the plan).

---

## Environment

| Item | Value |
|------|-------|
| Repo | `github.com/x7even/cloudcostsmcp` |
| Go subdirectory | `opencloudcosts-go/` (subdirectory of existing repo) |
| Go module path | `github.com/x7even/cloudcostsmcp/opencloudcosts-go` |
| Go version | 1.25.8 (`/home/xin/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.8.linux-amd64/bin/go`) |
| Python project | `/home/xin/cloudcostmcp/` |
| Harness | `/home/xin/cloudcostmcp/local-test-harness/run_tests.py` (186 prompts) |
| Workflow scripts | `/home/xin/cloudcostmcp/.claude/workflows/occ-go-phase*.js` |

---

## Ultracode Execution Patterns

Every Workflow in this plan uses these patterns. Understanding them explains the structure of each phase.

### pipeline() — not barriers between stages

All phases use `pipeline(agents, stageA, stageB, ...)`. This means Agent A's reviewer fires as soon as A completes — it does not wait for Agents B and C to finish. Wall-clock time equals the slowest single-agent chain, not the sum of all stages.

```
Agent A ──► Review A ──► (merge)
Agent B ──────────────► Review B ──► (merge)     ← B is slower; its reviewer fires last
Agent C ──► Review C ──► (merge)                 ← C's reviewer fires independently
```

Barriers (`parallel()`) are used only within the review stage of a single agent — when we genuinely need all three reviewer perspectives on that one piece of code at once.

### Adversarial review — every agent gets one

Every implementation agent is reviewed by three agents in parallel:

| Reviewer | Stance | Focus |
|---------|--------|-------|
| Correctness | Neutral | Go idioms, error handling, interface compliance |
| Adversarial | Hostile — default to finding bugs | Off-by-one, nil panics, race conditions, wrong response shapes |
| Domain-specific | Neutral | Test quality (Phase 1), security (Phase 2), harness parity (Phase 3) |

Adversarial reviewers are explicitly framed: *"Default to finding issues. Only approve if you genuinely cannot find a problem after a thorough search."*

### Completeness critic — end of every phase

After all agents and reviewers complete, a completeness critic agent reads the actual filesystem state and checks what the next phase needs. It returns `{ phase_N_ready: bool, gaps: [...] }`. The main session gates phase transitions on this output.

### Schema-validated outputs

All review and critique agents use `schema:` — their responses are JSON-validated at the tool layer. This prevents reviewers from returning unstructured text that the Workflow cannot interpret.

### Security as adversarial

Phase 2 security reviewers are per-provider and adversarial (default to finding issues). Phase 4 runs four independent security reviewers in parallel, each assigned a single attack surface (credentials, network, cache integrity, supply chain). A finding from ANY reviewer blocks cutover.

---

## Claude Capabilities Used

| Capability | Role |
|-----------|------|
| `Workflow` tool | Orchestrates Phases 1–4 — fans out parallel agents, pipelines review stages, runs completeness critics |
| `agent()` in Workflow | Implementation agents + all reviewer agents + completeness critics |
| `pipeline()` in Workflow | Primary pattern — review fires immediately per agent, no cross-agent barrier |
| `parallel()` in Workflow | Multi-perspective review panel within a single agent's review stage |
| `schema:` on reviewer calls | Forces structured JSON output; model retries on mismatch |
| `effort: 'high'` | Applied to adversarial and security reviewers; lower for mechanical stages |
| `advisor` | Called before Phase 0 → 1 transition and before cutover declaration |
| Main session | Merges worktrees, reads Workflow results, gates phase transitions, applies patches |

---

## Phase 0 — Scaffold (Main Session, Sequential)

**No Workflow. Main session does all of this. Blocking: nothing in Phase 1 can start until Phase 0 is complete.**

### Steps

**0.1 — Directory scaffold and `go.mod`**
Main session creates:
```
opencloudcosts-go/
├── cmd/opencloudcosts/main.go       (stub — prints version, exits 0)
├── internal/
│   ├── config/
│   ├── models/
│   ├── cache/
│   ├── providers/
│   │   ├── base.go                  (Provider interface + constructor registry)
│   │   ├── aws/
│   │   ├── gcp/
│   │   └── azure/
│   ├── tools/
│   ├── utils/
│   └── server/
├── schemas/
├── go.mod                           (module + decided deps only)
├── go.sum
└── .github/workflows/ci.yml         (build, test -race, vet, lint, govulncheck)
```

`go.mod` declares only the three external deps decided in the plan:
- `github.com/aws/aws-sdk-go-v2` (+ service sub-modules)
- `golang.org/x/oauth2`
- `github.com/modelcontextprotocol/go-sdk`
- `golang.org/x/sync` (for errgroup in Phase 3C)
- `golang.org/x/time` (for rate limiter in Phase 4)

**0.2 — Tool schema snapshot**
Start Python MCP server via stdio, issue `tools/list`, save response to `schemas/tools-snapshot.json`. This is the byte-level ground truth every Phase 1C and Phase 3 agent diffs against.

**0.3 — Provider interface**
Write `internal/providers/base.go` — the `Provider` interface with all methods Phase 2 must implement. Phase 1C imports this. Phase 2 agents depend on it being stable before they start.

**0.4 — Harness baseline** *(human action)*
```bash
cd /home/xin/cloudcostmcp
uv run local-test-harness/run_tests.py
```
Record result in `schemas/harness-baseline.json`:
```json
{ "total": 186, "passed": N, "failed": M, "date": "YYYY-MM-DD" }
```

**0.5 — Advisor call**
Before Phase 1, call advisor to confirm scaffold is correct and phase gates are well-defined.

### Phase 0 Output
- `opencloudcosts-go/` committed, `go build ./...` green on the stub
- `schemas/tools-snapshot.json` — 15 tool schemas
- `schemas/harness-baseline.json` — Python pass count, set by human
- `internal/providers/base.go` — Provider interface final

---

## Phase 1 — Foundation

**Workflow script:** `.claude/workflows/occ-go-phase1-foundation.js`

```bash
# Run from main session:
Workflow({ scriptPath: '/home/xin/cloudcostmcp/.claude/workflows/occ-go-phase1-foundation.js' })
```

### What the Workflow does

Three agents run in parallel, each writing to a non-overlapping subdirectory. The review pipeline fires independently per agent the moment each implementation completes.

```
1A: Config + Models  ──►  correctness:1a  ─┐
                    ──►  adversarial:1a  ──┤  (parallel reviews, fire immediately)
                    ──►  test-quality:1a ─┘

1B: Cache            ──►  correctness:1b  ─┐
                    ──►  adversarial:1b  ──┤
                    ──►  test-quality:1b ─┘

1C: MCP Scaffold     ──►  correctness:1c  ─┐
                    ──►  adversarial:1c  ──┤
                    ──►  test-quality:1c ─┘

[all above: pipeline() — no barrier between agents]

Completeness Critic  ──►  { phase2Ready: bool, gaps: [...] }
```

### Agents

**1A — Config + Models**
- Targets: `internal/config/`, `internal/models/`
- Critical path: PricingSpec spike FIRST — two-pass `UnmarshalJSON`, round-trip table tests for all variants, tests pass before any other model code is written
- Config: `os.Getenv` + typed helpers only, no library
- All enums exact string matches to Python

**1B — Cache**
- Target: `internal/cache/`
- Design: `sync.RWMutex` map + JSON file persistence; atomic writes via temp-file+rename; TTL enforced on Get
- No SQLite, no external dep
- Tests: concurrent reads, TTL expiry (1ms), ClearProvider isolation, corrupt-file recovery

**1C — MCP Scaffold**
- Targets: `cmd/opencloudcosts/main.go`, `internal/server/`
- All 15 tools registered with STUB handlers; schemas auto-generated via SDK reflection; schema parity test diffs against `schemas/tools-snapshot.json`
- `/healthz` + `/readyz` endpoints wired

### Review Panel (per agent, all fire in parallel)

| Reviewer | Instruction |
|---------|-------------|
| Correctness | Error wrapping, no global state, ctx propagation, RWMutex correctness (1B), schema diff (1C) |
| Adversarial | Default hostile: off-by-one, nil panics, PricingSpec variant fails, TTL race, schema field drift |
| Test quality | Coverage, table-driven subtests, no time.Sleep, round-trip tests for all PricingSpec variants (1A) |

### Phase 1 Gate (checked by Completeness Critic)
- Provider interface importable by Phase 2 agents
- PricingSpec round-trips for all domains
- `go build ./...` succeeds
- `go test -race ./...` green
- Schema parity test passes

---

## Phase 2 — Providers

**Workflow script:** `.claude/workflows/occ-go-phase2-providers.js`

```bash
Workflow({ scriptPath: '/home/xin/cloudcostmcp/.claude/workflows/occ-go-phase2-providers.js' })
```

### What the Workflow does

Three provider agents in parallel. Immediately on each completion: three reviewers fire simultaneously (pipeline). Security is adversarial and provider-specific. Completeness critic checks cross-provider interface consistency.

```
2A: AWS    ──►  correctness:aws  ─┐
           ──►  adversarial:aws  ──┤  (parallel, fire immediately on 2A complete)
           ──►  security:aws    ─┘

2B: GCP    ──►  correctness:gcp  ─┐
           ──►  adversarial:gcp  ──┤
           ──►  security:gcp    ─┘

2C: Azure  ──►  correctness:azure ─┐
           ──►  adversarial:azure ──┤
           ──►  security:azure   ─┘

Completeness Critic ──► { phase3Ready: bool, securityFailures: N, gaps: [...] }
```

### Agents

**2A — AWS**
- All Provider interface methods: GetPrice, GetComputePrice, GetStoragePrice, GetDatabasePrice, SearchPricing, ListRegions, ListInstanceTypes, CheckAvailability, GetEffectivePrice, GetSpotHistory, GetDiscountSummary, DescribeCatalog, BOMAdvisories, MajorRegions, DefaultRegion
- Critical: `pricing.GetProducts` returns `[]string` — each raw JSON requiring tree navigation
- All SKU filter maps ported exactly from Python

**2B — GCP**
- All 5 auth paths from `gcp_auth.py`: API key, SA JSON, base64 SA JSON, short-lived token, ADC/WIF
- `golang.org/x/oauth2/google` only — no `google.golang.org/api/option`
- `net/http` for Billing Catalog API calls
- Windows pricing (T31), GCP major regions (T22)

**2C — Azure**
- Pure `net/http` — Azure Retail Prices API is public, no credentials
- T38 fix: reserved pricing unit normalisation
- All region and instance type format handling

### Review Panel (per provider, all fire in parallel)

| Reviewer | Instruction |
|---------|-------------|
| Correctness | All interface methods present, pagination correct, error shapes structured JSON |
| Adversarial | Missing pages, stale auth tokens, SKU filter drift from Python, nil panics on unexpected API responses |
| Security | Adversarial: no credential logging, no InsecureSkipVerify, SSRF-safe URL construction, all HTTP clients have timeouts |

### Phase 2 Gate
- All three providers implement identical interface
- Security: zero `fail` findings across all three providers (warns documented, non-blocking)
- `go test -race ./...` green
- Completeness critic: `phase3Ready: true`

---

## Phase 3 — Utilities + Tools

**Workflow script:** `.claude/workflows/occ-go-phase3-tools.js`

```bash
Workflow({ scriptPath: '/home/xin/cloudcostmcp/.claude/workflows/occ-go-phase3-tools.js' })
```

### What the Workflow does

Four agents in parallel, each owning a non-overlapping scope. Three reviewers per agent fire immediately in pipeline. The completeness critic is the harness-aware gate: it reads all 186 harness prompts and assesses whether the tools would satisfy them.

```
3A: Utilities      ──►  correctness:utils   ─┐
                   ──►  adversarial:utils   ──┤
                   ──►  harness-parity:utils─┘

3B: Lookup tools   ──►  correctness:lookup  ─┐
                   ──►  adversarial:lookup  ──┤
                   ──►  harness-parity:lookup─┘

3C: Availability   ──►  correctness:avail  ─┐
                   ──►  adversarial:avail  ──┤
                   ──►  harness-parity:avail─┘

3D: FinOps+Cache   ──►  correctness:finops ─┐
                   ──►  adversarial:finops ──┤
                   ──►  harness-parity:finops─┘

Completeness Critic ──► { toolsPresent: [...], toolsMissing: [...], phase4Ready: bool, harnessRisks: [...] }
```

### Agents

**3A — Utilities** (`internal/utils/`)
- 8 files: http_retry, money, regions, spec_infer, baseline, egress_tiers, gcp_specs, units
- `float64` throughout for money — no decimal library

**3B — Lookup tools** (5 tools)
- `get_price`, `get_prices_batch`, `compare_prices`, `search_pricing`, `describe_catalog`
- No-results hint text (T25) byte-identical to Python — harness grades these strings

**3C — Availability tools** (4 tools)
- `list_regions`, `list_instance_types`, `find_cheapest_region`, `find_available_regions`
- `errgroup.WithContext` + semaphore (max 32 concurrent region fetches)
- GCP major-region `note` field with exact wording

**3D — FinOps + Cache tools** (5 tools)
- `estimate_bom` (T20 os field, T27 database routing), `estimate_unit_economics`, `get_discount_summary`, `refresh_cache`, `cache_stats`

### Review Panel (per agent, all fire in parallel)

| Reviewer | Instruction |
|---------|-------------|
| Correctness | Tool param decoding, provider dispatch, error JSON shape, errgroup correct (3C) |
| Adversarial | Handler panics on nil, field names differ by one character, T25 hint text wrong, goroutine leak on cancellation |
| Harness parity | Read `run_tests.py`, assess each prompt category: HALLUCINATION / TRUNCATION / UNANCHORED / MISSING_DATA risks |

### Phase 3 Gate
- All 15 tools present and registered in MCP server
- `go build ./...` succeeds
- `go test -race ./...` green, ≥80% coverage per package
- Completeness critic: `phase4Ready: true`, no high-risk harness items unaddressed

---

## Phase 4 — Integration + Quality

**Workflow script:** `.claude/workflows/occ-go-phase4-integration.js`

```bash
Workflow({ scriptPath: '/home/xin/cloudcostmcp/.claude/workflows/occ-go-phase4-integration.js' })
```

### What the Workflow does

Sequential integration agent, then four parallel adversarial security reviewers (barrier — we need all results before declaring the sweep done), then quality gate agent, then harness preflight analyst.

```
Integration Agent
  ↓ (sequential)
Security Sweep ──► security-sweep:credentials ─┐
               ──► security-sweep:network      ──┤  (parallel — barrier; all four needed)
               ──► security-sweep:cache        ──┤
               ──► security-sweep:supply-chain ─┘
  ↓ (sequential)
Quality Gate Agent  (lint + vulncheck + race + coverage)
  ↓ (sequential)
Harness Preflight Analyst
```

### Step 4.1 — Integration Agent
- Replaces stub handlers in `server.go` with real tool package calls
- Wires provider constructors into `AppServer` with graceful degradation (provider init failure → log + skip)
- Adds: structured `log/slog` JSON logging, request timeout wrapper, rate limiter, SIGTERM drain
- Verifies: `go build ./...`, `go test -race ./...`, tools/list returns 15 tools

### Step 4.2 — Security Sweep (4 parallel adversarial reviewers)

Each reviewer owns one attack surface. All four are framed adversarially: *"Default to finding issues."*

| Reviewer | Surface |
|---------|---------|
| `security-sweep:credentials` | Credential logging, key material in errors, GCP token expiry, AWS credential fallback |
| `security-sweep:network` | InsecureSkipVerify, redirect policy, SSRF via API response data, URL construction from user input, HTTP timeouts |
| `security-sweep:cache-integrity` | Cache key construction (user-controlled?), file permissions (0600), atomic write correctness, symlink vulnerability, cache poisoning |
| `security-sweep:supply-chain` | Dep count vs approved list, govulncheck CVEs, go.sum committed, CGO_ENABLED=0 in CI, reproducible build |

These four run as `parallel()` — a barrier is correct here because we want the full cross-surface picture before declaring the sweep done.

### Step 4.3 — Quality Gate Agent

Runs and interprets:
- `golangci-lint run --enable=errcheck,staticcheck,gosec,exhaustive,gocritic`
- `govulncheck ./...`
- `go test -race -coverprofile=coverage.out ./...`
- `go tool cover -func=coverage.out` — per-package coverage, ≥80% threshold
- `go vet ./...`

Applies fixable issues inline. Returns `{ allGreen: bool }`.

### Step 4.4 — Harness Preflight Analyst

Reads all 186 harness prompts and the Go implementation. Predicts:
- Which prompts are likely to fail and why (HALLUCINATION / TRUNCATION / UNANCHORED / MISSING_DATA)
- Predicted pass rate vs baseline
- `readyForHarnessRun: bool`

This runs BEFORE the human runs the harness, so the human knows exactly what to watch for.

---

## Human Checkpoints

Only two human actions are required:

| Checkpoint | Action | Output |
|-----------|--------|--------|
| Phase 0.4 | `uv run local-test-harness/run_tests.py` against Python | Record baseline in `schemas/harness-baseline.json` |
| Post Phase 4 | `./bin/opencloudcosts --transport http --port 9090` + `uv run local-test-harness/run_tests.py` against Go | Compare to baseline; Go must match or exceed |

After Phase 4, a final `advisor` call happens in the main session before cutover is declared. All Workflow failures surface in the main session for the main session to patch.

---

## Workflow Result Handling

Every Workflow returns a structured result object. After each Workflow call, the main session:

1. Checks `phase2Ready` / `phase3Ready` / `phase4Ready` / `cutoverReady` — if `false`, reads the `gaps` / `findings` arrays and patches before re-running
2. Reads `criticalFindings` count — any critical findings block the phase transition
3. For Phase 2: checks `securityFailures === 0` — any security `fail` blocks Phase 3
4. For Phase 4: checks all four `security` areas and `quality.allGreen`

The main session owns all merges. Agents do not push to any branch directly.

---

## Reviewer Roles Summary

| Reviewer | When | Framing |
|---------|------|---------|
| Correctness | After every agent in Phases 1–3 | Neutral — Go idioms, interface compliance |
| Adversarial | After every agent in Phases 1–3 | Hostile — "default to finding bugs" |
| Test quality | Phase 1 agents | Neutral — coverage, table-driven style |
| Security (per-provider) | Phase 2 agents | Adversarial — credential exposure, TLS, SSRF |
| Harness parity | Phase 3 agents | Neutral — predict harness outcome per tool |
| Security sweep (4 lanes) | Phase 4 | Adversarial — each owns one attack surface |
| Quality gate | Phase 4 | Neutral — runs tools, reports numbers |
| Harness preflight | Phase 4 | Analyst — predict failures before human run |
| Completeness critic | End of Phases 1–3 | Neutral — reads filesystem, blocks next phase if gaps |
| Advisor | Phase 0→1 transition, cutover | Independent — full conversation history visible |

---

## Merge Strategy

```
Phase Workflow completes
  ↓
Main session reads { phase_N_ready, gaps, criticalFindings }
  ↓
If clear → main session merges all agent outputs to opencloudcosts-go/
  ↓
If gaps/critical → main session patches inline, re-runs only affected agents (Workflow resume)
  ↓
go test -race ./... on merged tree → must be green before next phase
```

Agents write to designated non-overlapping subdirectories within the current branch. No per-agent worktree is needed since paths don't overlap per phase. If an agent creates a file that conflicts, the Workflow logs it and the main session resolves it.

---

## Cutover

Triggered after human harness run matches baseline and advisor sign-off:
1. Update `examples/` client configs to point to Go binary
2. Replace `Dockerfile` with multi-stage Go build → scratch image
3. Tag `v0.9.x-python` on the final Python commit
4. Update `README.md` installation section
5. Python source remains; add deprecation notice to `src/opencloudcosts/__init__.py`

---

## What We Are Not Doing

- No migration of Python SQLite cache — users start with a cold JSON cache (one cold session)
- No T26 or any roadmap item in Python — Python is frozen; all roadmap items go straight to Go
- No `testify/mock` — interface-based mocks only
- No CGO anywhere — `CGO_ENABLED=0` unconditional
- No half-phase execution — agents write to non-overlapping paths; the phase Workflow is the unit of execution
- No blind iteration on harness failures — root-cause before fix, diagnostic agent required
