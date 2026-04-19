# v0.8.0 — Tool Consolidation & Provider Protocol Rework

## Context

`opencloudcosts` has grown to 31 MCP tools as new GCP services were added. The v0.7.10 harness run on a large LLM hit 108/109 (99%), but the surface has three structural problems that will bite as we scale:

1. **Tool sprawl** — 31 tools is past the ~15-tool threshold where LLM tool-selection accuracy noticeably degrades. Smaller models will regress.
2. **Provider coupling leaks into tool names** — `get_bedrock_price` (AWS-only), `get_gke_price` (GCP-only), `get_gemini_price` (GCP-only) force the LLM to memorise provider ↔ service mappings. Observed failure: MP2 prompt routed an AWS instance type (`db.r5.2xlarge`) into a GCP Cloud SQL call.
3. **Under-specified provider Protocol** — many provider methods (GCP's managed-service pricers, AWS's discount APIs) live outside the `CloudPricingProvider` Protocol. Tool layer branches with `if provider == "gcp": ...`, blocking clean OCI/Alibaba onboarding.

**Intended outcome:** a consolidated 15-tool surface that grounds the LLM in cloud pricing as ground truth — public catalog prices for every supported service, effective post-discount prices enriched into the same response where credentials exist — with a unified provider protocol that scales to 5+ hyperscalers and future FinOps work. Released as **v0.8.0 with a hard break** from old tool names.

**User priorities:** quality, reliability, factual grounding of both public and effective costs in a single answer.

---

## Core design decision — enriched PricingResult (revised after review)

A single `get_price(spec)` call returns a progressively-richer answer depending on auth:

```python
class PricingResult(BaseModel):
    public_prices: list[NormalizedPrice]           # always — catalog list rates
    contracted_prices: list[NormalizedPrice] = []  # applicable SP/RI/CUD rates for this spec, if auth
    effective_price: EffectivePrice | None = None  # blended rate for this resource, if auth
    auth_available: bool = False                   # LLM sees what was possible
    breakdown: dict[str, Any] = {}                 # category-specific composite math
    note: str | None = None
    source: Literal["catalog", "fallback", "mixed"] = "catalog"
    schema_version: Literal["1"] = "1"
```

**No separate `get_effective_price` tool.** One call = one financial picture for that resource. The LLM is grounded in catalog truth + contractual truth simultaneously, with clear `auth_available` signalling when the contractual layer is unavailable.

Retrospective/account-wide analysis (time-bucketed CE data, SP/RI utilisation) stays in `get_discount_summary(provider)` — that's a different use case, not a spec-based query.

---

## Final tool surface — 15 tools

### Pricing (4)
1. **`get_price(spec)`** — unified pricing dispatcher. Returns `PricingResult` with public + contracted + effective where auth permits. Replaces 21 of the 31 current tools (including old `get_effective_price`).
2. **`get_prices_batch(provider, instance_types, region, os, term)`** — concurrent batch compute pricing; input shape differs meaningfully from `PricingSpec`.
3. **`compare_prices(spec, regions, baseline_region)`** — multi-region comparison over any category.
4. **`search_pricing(provider, query, category, region, max_results)`** — free-text catalog search fallback.

### Discovery (3)
5. **`list_regions(provider, category)`** — region codes + display names.
6. **`list_instance_types(provider, region, filters)`** — filterable instance-type catalog.
7. **`describe_catalog(provider?, domain?, service?)`** — dual-purpose discovery + validation tool. No args returns full support matrix; with args returns targeted spec guidance (`required_fields`, `usage_keys_required`, `supported_terms`, `example_invocation` that can be passed verbatim to `get_price`). Replaces `list_services` and `check_availability`.

### Analysis (3)
8. **`find_cheapest_region(spec, regions)`** — cheapest region for any category.
9. **`find_available_regions(spec, regions)`** — regions where a service exists, cheapest first.
10. **`get_spot_history(spec, hours)`** — AWS spot volatility/recommendation.

### FinOps (3)
11. **`get_discount_summary(provider)`** — retrospective SP + RI + CUD utilisation (account-wide, time-bucketed).
12. **`estimate_bom(items)`** — BoM/TCO. `items[*]` is a `PricingSpec` + `quantity`.
13. **`estimate_unit_economics(items, units_per_month, unit_label)`** — cost-per-unit synthesis.

### Utility (2)
14. **`cache_stats()`** — cache entry counts + DB size.
15. **`refresh_cache(provider)`** — invalidate cache for a provider.

### Removed (16 folded into `get_price` or `describe_catalog`)
`get_compute_price`, `get_storage_price`, `get_database_price`, `get_fargate_price`, `get_bedrock_price`, `get_gke_price`, `get_memorystore_price`, `get_bigquery_price`, `get_vertex_price`, `get_gemini_price`, `get_cloud_lb_price`, `get_cloud_cdn_price`, `get_cloud_nat_price`, `get_cloud_armor_price`, `get_cloud_monitoring_price`, `get_effective_price`, `list_services`, `check_availability`, `compare_compute_prices` (generalised).

---

## Type system (`src/opencloudcosts/models.py`)

**Rename** `PricingCategory` → `PricingDomain` and reserve `PricingShape` for the pricing-model axis (addresses review S2).

```python
class PricingDomain(str, Enum):
    COMPUTE = "compute"           # VMs, Fargate, ECS-on-Fargate
    STORAGE = "storage"           # block + object
    DATABASE = "database"         # RDS, Cloud SQL, Memorystore, ElastiCache, Azure SQL
    CONTAINER = "container"       # EKS, GKE, AKS control planes
    AI = "ai"                     # Bedrock, Vertex, Gemini, SageMaker, Azure OpenAI
    SERVERLESS = "serverless"     # Lambda, Cloud Run, Cloud Functions, Azure Functions
    ANALYTICS = "analytics"       # BigQuery, Redshift, Athena, Synapse
    NETWORK = "network"           # LB, CDN, NAT, egress, Cloud Armor, WAF
    OBSERVABILITY = "observability"  # CloudWatch metrics/logs, Cloud Monitoring, Azure Monitor

class PricingShape(str, Enum):
    RATE_PER_HOUR = "rate_per_hour"
    RATE_PER_GB_MONTH = "rate_per_gb_month"
    RATE_PER_TOKEN = "rate_per_token"
    RATE_PER_REQUEST = "rate_per_request"
    RATE_PER_QUERY = "rate_per_query"
    TIERED_USAGE = "tiered_usage"
    COMPOSITE = "composite"

# Extended terms cover missing commitment SKUs (addresses review S3)
class PricingTerm(str, Enum):
    ON_DEMAND = "on_demand"
    RESERVED_1YR = "reserved_1yr"
    RESERVED_3YR = "reserved_3yr"
    RESERVED_1YR_PARTIAL = "reserved_1yr_partial"
    RESERVED_1YR_ALL = "reserved_1yr_all"
    RESERVED_3YR_PARTIAL = "reserved_3yr_partial"
    RESERVED_3YR_ALL = "reserved_3yr_all"
    SPOT = "spot"
    COMPUTE_SP = "compute_savings_plan"
    EC2_INSTANCE_SP = "ec2_instance_savings_plan"
    SAGEMAKER_SP = "sagemaker_savings_plan"
    CUD_1YR = "cud_1yr"
    CUD_3YR = "cud_3yr"
    FLEX_CUD = "flex_cud"
    SUD = "sustained_use_discount"
    PTU = "provisioned_throughput_units"  # Azure OpenAI
```

### Spec shape — discriminated-union registry (addresses review S1)

Rather than one flat `PricingSpec` with loose `usage: dict[str, float]` and cross-field validators, we register a **typed spec class per (domain, service) pair**:

```python
class BasePricingSpec(BaseModel):
    provider: CloudProvider
    domain: PricingDomain
    service: str | None = None
    region: str
    term: PricingTerm = PricingTerm.ON_DEMAND
    schema_version: Literal["1"] = "1"

class ComputePricingSpec(BasePricingSpec):
    domain: Literal[PricingDomain.COMPUTE]
    resource_type: str | None = None        # instance_type
    os: str = "Linux"
    vcpu: float | None = None               # for Fargate-style
    memory_gb: float | None = None
    hours_per_month: float = 730.0

class AiPricingSpec(BasePricingSpec):
    domain: Literal[PricingDomain.AI]
    model: str                              # required
    input_tokens: int | None = None
    output_tokens: int | None = None
    training_hours: float | None = None
    machine_type: str | None = None         # Vertex training

class DatabasePricingSpec(BasePricingSpec):
    domain: Literal[PricingDomain.DATABASE]
    resource_type: str                      # db instance type — required
    engine: str = "MySQL"
    deployment: str = "single-az"
    storage_gb: float | None = None

# ...one class per (domain, service) cell, maintained in PRICING_SCHEMAS registry
PRICING_SCHEMAS: dict[tuple[PricingDomain, str | None], type[BasePricingSpec]] = {
    (PricingDomain.COMPUTE, None): ComputePricingSpec,
    (PricingDomain.AI, "bedrock"): BedrockSpec,
    (PricingDomain.AI, "vertex"): VertexSpec,
    (PricingDomain.AI, "gemini"): GeminiSpec,
    ...
}

PricingSpec = Annotated[
    Union[ComputePricingSpec, AiPricingSpec, DatabasePricingSpec, ...],
    Field(discriminator="domain"),
]
```

**Why**: typed spec produces real JSON Schema output to the MCP client; invalid keys (e.g. `input_token` vs `input_tokens`) fail at parse time with a clear field-level error, not runtime dict-sniffing. Extending to OCI/Alibaba = add new spec classes, register in `PRICING_SCHEMAS`, no validator changes.

### Other new types
```python
class PricingResult(BaseModel):            # see Core design decision above
    ...

class ProviderCatalog(BaseModel):
    provider: CloudProvider
    domains: list[PricingDomain]
    services: dict[PricingDomain, list[str]]
    supported_terms: dict[tuple[PricingDomain, str], list[PricingTerm]]
    filter_hints: dict[tuple[PricingDomain, str], dict[str, Any]]
    decision_matrix: dict[str, str]  # "Cloud Run" → "serverless/cloud_run"; "ECS on Fargate" → "compute/fargate" (S2)

class NotSupportedError(Exception):
    def to_response(self) -> dict:
        return {
            "error": "not_supported",
            "provider": ...,
            "domain": ...,
            "service": ...,
            "reason": ...,
            "alternatives": [...],
            "example_invocation": {...},  # ready-to-copy PricingSpec dict (addresses S4)
        }
```

Keep `NormalizedPrice`, `EffectivePrice`, `BomLineItem`, `BomEstimate`, `InstanceTypeInfo`, `PriceComparison` unchanged.

---

## Provider protocol (`src/opencloudcosts/providers/base.py`)

```python
@runtime_checkable
class CloudPricingProvider(Protocol):
    provider: CloudProvider

    def supports(self, domain: PricingDomain, service: str | None = None) -> bool: ...
    def supported_terms(self, domain: PricingDomain, service: str | None) -> list[PricingTerm]: ...
    async def get_price(self, spec: PricingSpec) -> PricingResult: ...
    async def search(self, query: str, *, domain=None, service=None, region=None, max_results=20) -> list[NormalizedPrice]: ...
    async def list_regions(self, domain: PricingDomain = PricingDomain.COMPUTE) -> list[str]: ...
    async def list_instance_types(self, region: str, **filters) -> list[InstanceTypeInfo]: ...
    async def describe_catalog(self) -> ProviderCatalog: ...
    async def get_discount_summary(self) -> dict: ...
    async def get_spot_history(self, spec: PricingSpec, hours: int) -> dict: ...
    # Internal helper expected by get_price when auth is present:
    async def _applicable_commitments(self, spec: PricingSpec) -> list[EffectivePrice]: ...
```

Add `ProviderBase` abstract mixin with default `NotSupportedError`-raising implementations for optional methods.

---

## Validation strategy — four layers

1. **Pydantic discriminated-union parse** — invalid keys / missing required fields fail at spec construction with field-level errors. (Replaces cross-field validator swamp per review S1.)
2. **Capability pre-check** — `provider.supports(domain, service)` → structured `NotSupportedError.to_response()` with alternatives + `example_invocation`.
3. **Provider-level semantic validation** — e.g. AWS-style `resource_type="db.r5.xxx"` routed to GCP → rejected with "Did you mean provider='aws'? GCP Cloud SQL types look like 'db-n1-standard-4'".
4. **Rich error responses from `get_price`** — on any internal failure, return structured response with `required_fields`, `usage_keys_expected`, `example_invocation`, `alternatives`.

### Proactive guidance for small LLMs (addresses S4)
- `server.py` `instructions` block includes **3-5 `get_price` example invocations** covering compute / AI-token / database / storage. Models pattern-match against examples more reliably than they call discovery tools.
- Every `NotSupportedError.to_response()` includes `example_invocation` — a complete, ready-to-pass spec dict.
- `describe_catalog(provider, domain, service)` targeted mode returns the same `example_invocation` shape.

---

## Critical discovery from review — GCP is a rewrite, not a wrapper (B1)

`providers/gcp.py` has 10 managed-service methods returning **heterogeneous formatted dicts** with string-money fields (e.g. `"analysis_rate_per_tib": "$5.000000/TiB"`), not `list[NormalizedPrice]`. Lines 790, 1301, 1420, 1511, 1587, 1661, 1744, 635, 513. Each must be re-engineered to:
- Emit `NormalizedPrice` instances with correct `PriceUnit` + `price_per_unit: Decimal`
- Preserve composite estimates in `PricingResult.breakdown`
- Maintain free-tier hints, multi-region fallbacks (`us-central1` → `us`), tiered rates

**Scope: full sprint for GCP migration. Mandatory economic-equivalence contract test** (see Test strategy).

---

## Tool layer (`src/opencloudcosts/tools/`)

Every tool is a thin validator + router. Example:

```python
@mcp.tool()
async def get_price(ctx, spec_dict: dict) -> dict:
    spec = parse_spec(spec_dict)  # Layer 1 — discriminated-union parse
    pvdr = _providers(ctx)[spec.provider]
    if not pvdr.supports(spec.domain, spec.service):  # Layer 2
        return NotSupportedError(
            provider=spec.provider, domain=spec.domain, service=spec.service,
            reason=..., alternatives=..., example_invocation=...,
        ).to_response()
    try:
        result = await pvdr.get_price(spec)
        return result.model_dump()
    except NotSupportedError as e:
        return e.to_response()
    except NotConfiguredError as e:
        return {"error": "not_configured", "reason": str(e)}
```

Zero `if provider == "x"` branches in any tool file.

### Hard-break landmines (addresses review B2)

Hardcoded old tool names exist in:
- `tools/lookup.py:625-749` (`_SERVICE_HINTS`)
- `tools/bom.py:161-194` (`how_to_price` strings)
- `server.py:82-87` (`instructions` block)
- `local-test-harness/run_tests.py:83` (`TEST_PROMPTS`)
- `docs/tools.md`, `docs/roadmap.md`, `README.md`

All must be rewritten before merge. **CI gate:**
```bash
rg '(get_\w+_price|list_services|check_availability|get_service_price)\b' src/ docs/ README.md local-test-harness/
# must return zero matches
```

---

## File-level changes

| File | Change | Pattern |
|---|---|---|
| `src/opencloudcosts/models.py` | **Extend** | Add `PricingDomain`, `PricingShape`, extended `PricingTerm`, per-(domain,service) spec classes, `PRICING_SCHEMAS` registry, `PricingSpec` discriminated union, `PricingResult`, `ProviderCatalog`. Keep `NormalizedPrice`, `EffectivePrice`, etc. |
| `src/opencloudcosts/providers/base.py` | **Rewrite** | New capability-based Protocol, `NotSupportedError` with `example_invocation`, `ProviderBase` mixin. |
| `src/opencloudcosts/providers/aws.py` | **Refactor** | Add `get_price(spec)` dispatcher + `supports()` + `supported_terms()` + `describe_catalog()` + `_applicable_commitments()` (folds old `get_effective_price` logic + SP/RI enrichment). Pull Fargate + Bedrock logic from `tools/lookup.py`. Keep existing methods as internals. |
| `src/opencloudcosts/providers/gcp.py` | **Rewrite (not wrapper)** | Re-engineer 10 managed-service methods to emit `NormalizedPrice` + `breakdown`. Add dispatcher + capability surface. Economic-equivalence contract test required. |
| `src/opencloudcosts/providers/azure.py` | **Extend** | Add dispatcher (compute + storage today). Add `_applicable_commitments()` stub raising `NotSupportedError` (PTU support follow-up). |
| `src/opencloudcosts/tools/lookup.py` | **Rewrite** | Delete 23 old tools. Ship `get_price`, `get_prices_batch`, `search_pricing`, `get_discount_summary`, `get_spot_history`, `refresh_cache`. ~300 LoC from ~1800. Delete `_SERVICE_HINTS` entirely. |
| `src/opencloudcosts/tools/availability.py` | **Refactor** | Delete `list_services` + `check_availability` (folded into `describe_catalog`). Update `find_cheapest_region` / `find_available_regions` to accept `PricingSpec`. Add `describe_catalog` tool. |
| `src/opencloudcosts/tools/bom.py` | **Refactor** | `items[*]` → `PricingSpec` + `quantity`. Rewrite all `how_to_price` strings. |
| `src/opencloudcosts/server.py` | **Extend** | Rewrite `instructions` block with 3-5 `get_price` examples. Remove old-tool registration. Add `OCC_DEBUG_PRICING` structured logging hook (M3). |
| `src/opencloudcosts/providers/__init__.py` | **Minimal** | Export new public types. |
| `docs/tools.md`, `docs/roadmap.md`, `README.md` | **Rewrite** | Full documentation refresh for 15-tool surface. |
| `local-test-harness/run_tests.py`, harness prompts | **Update** | Rewrite prompts that name old tools; preserve prompt semantics for baseline comparability (M1). |

---

## Migration sequence — branch-based (addresses review S6)

Avoid broken-middle on `main` by sequencing provider migrations on a branch.

**On `main`:**
1. Models + Protocol + `PRICING_SCHEMAS` registry (pure additions, no behaviour change).
2. Contract test skeleton (`tests/test_providers/test_provider_contract.py`) — passes trivially on old surface.

**On branch `v0.8.0-prep`:**
3. AWS provider dispatcher + capability surface + `_applicable_commitments`. Passes contract test.
4. GCP provider **rewrite** (per B1). Economic-equivalence test must pass before proceeding.
5. Azure provider dispatcher. Passes contract test.
6. Tool layer rewrite + landmine CI gate.
7. Harness prompt rewrites.
8. Docs refresh (tools.md, roadmap.md, README.md).

**Merge branch → main** only after step 8, when full surface is consistent.

**Harness regression checkpoints:** steps 3, 4, 5, 6 each run the harness against the large LLM; any regression blocks the next step.

---

## Test strategy

### Contract test invariants (addresses review S5)
`tests/test_providers/test_provider_contract.py` — parameterised across every (provider, domain, service) in the support matrix:

1. **Supports/dispatch agreement** — `supports() == True` ⟺ `get_price()` does not raise `NotSupportedError`.
2. **Shape invariant** — every `PricingResult.public_prices[*]` has `price_per_unit > 0`, non-empty `region`, valid `PriceUnit`.
3. **Region echo** — `result.public_prices[*].region == spec.region` after normalisation.
4. **Example actually works** — `describe_catalog(p, d, s).example_invocation` passed back to `get_price` succeeds. **Highest-value test** — guarantees the LLM's learning path is correct.
5. **Breakdown arithmetic** — where `breakdown.monthly_total` exists, it equals sum of `prices` × quantity within 0.01 USD.

### Economic-equivalence test (addresses B1)
`tests/test_providers/test_gcp_economic_equivalence.py` — for every GCP managed-service harness prompt, record the v0.7.10 numeric outputs. After GCP rewrite, each must match within ±0.01 USD. Blocks step 4 from completing.

### Other test work
- **Unified pricing test** — `test_get_price_routes_correctly[domain=X, service=Y, provider=Z]` parameterised matrix; `NotSupportedError` paths; `describe_catalog` shape invariants.
- **Validation test** — every invalid-spec scenario (missing required field, AWS type → GCP provider, unknown service) returns structured error with `example_invocation`.
- **Auth-dependent test** — `with_auth → contracted_prices and effective_price populated`; `without_auth → both empty, auth_available=False, note explains what's missing`.
- **Duplicate-tool-name test** (addresses M5) — `assert len({t.name for t in mcp._tools}) == len(mcp._tools)`.

---

## Verification

### Pre-merge checks
1. **Full harness run against large LLM** ≥ 108/109 baseline. No regression.
2. **Contract test suite passes** — all 5 invariants across entire support matrix.
3. **Economic-equivalence test passes** — GCP outputs match v0.7.10 within ±0.01 USD.
4. **MP2 regression check** — "AWS vs GCP DB stack with db.r5.2xlarge" returns structured error with GCP alternatives.
5. **Landmine CI gate** — `rg '(get_\w+_price|list_services|check_availability|get_service_price)\b' src/ docs/ README.md` returns zero matches.
6. **Small-model harness run** — validates consolidation benefit on lower-capability LLM. Compare to v0.7.10 smaller-model baseline (adapter translates tool names only, prompt semantics preserved — M1).

### Manual smoke tests
- `get_price` for Bedrock with auth → returns public + effective + `auth_available=True`.
- `get_price` for Bedrock without auth → returns public only + `auth_available=False` + explanatory note.
- `get_price` for Azure + Gemini → `NotSupportedError` with `example_invocation` pointing to GCP and Azure OpenAI alternatives.
- `describe_catalog(provider=aws, domain=ai, service=bedrock)` → structured guidance + working `example_invocation`.
- `estimate_bom(items=[{...PricingSpec + quantity...}])` works with new item shape.

### Verification checklist (from review M1-M5)
- [ ] **M1** Baseline comparability — same prompts, name-adapter, both surfaces scored.
- [ ] **M2** `PricingSpec.cache_key()` implemented — sorted dict keys, `extras` excluded.
- [ ] **M3** Structured logging via `OCC_DEBUG_PRICING=1`.
- [ ] **M4** `schema_version: Literal["1"]` on all spec classes from day one.
- [ ] **M5** Duplicate-tool-name test in place.

### Post-ship validation
- Tag `v0.8.0`, push to PyPI, verify install + server startup + basic `get_price` call from fresh env.
- Run harness against both large and small LLMs; compare to v0.7.10 baseline. Publish `docs/quality-v0.8.0.md`.

---

## Critical files to reuse
- `src/opencloudcosts/models.py` — `NormalizedPrice`, `EffectivePrice`, `BomLineItem`, `BomEstimate`, `InstanceTypeInfo`, `PriceComparison` survive.
- `src/opencloudcosts/providers/aws.py` — existing boto3/bulk-pricing methods become dispatch targets.
- `src/opencloudcosts/providers/gcp.py` — SKU index builders (`_build_*_price_index`) survive; public methods are rewritten to emit `NormalizedPrice`.
- `src/opencloudcosts/providers/azure.py` — Retail Prices API methods become dispatch targets for compute+storage.
- `src/opencloudcosts/tools/bom.py` — `BomLineItem.from_price` and unit-aware multiplication preserved.
- Cache infrastructure, settings loading, httpx client management — untouched.
