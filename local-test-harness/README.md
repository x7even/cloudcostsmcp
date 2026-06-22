# OpenCloudCosts — LLM Test Harness

An end-to-end quality harness that drives an LLM through the full MCP tool-call loop and automatically grades every response against four failure criteria. Works with any OpenAI-compatible LLM server.

## What it tests

The harness sends **199 curated prompts** covering every tool and provider combination:

| Category | IDs | What's tested |
|----------|-----|---------------|
| Common | C1–C4 | Basic AWS compute + storage |
| Multi-cloud | M1–M3 | AWS vs GCP vs Azure comparisons |
| Complex BoM/TCO | CX1–CX14, CX_BOM | Bill of Materials, unit economics, complex TCO |
| AWS Simple | AS1–AS10 | EC2, EBS, RDS, Lambda, ElastiCache, NAT |
| GCP Simple | GS1–GS10 | GCE, Persistent Disk, CUDs |
| GCP GKE | GK1–GK5 | Standard and Autopilot cluster pricing |
| GCP Memorystore | GM1–GM5 | Redis Basic and Standard tiers |
| GCP BigQuery | GB1–GB5 | Storage, queries, streaming inserts |
| GCP Vertex AI + Gemini | GV1–GV5 | Custom training, Gemini token pricing |
| GCP Networking | GN1–GN5 | Cloud LB, CDN, NAT |
| GCP Armor + Monitoring | GC1–GC5 | Cloud Armor, Cloud Monitoring |
| GCP Complex Stacks | GCX1–GCX5 | Multi-service GCP architectures |
| GCP Cloud Storage | GGCS1–GGCS5 | GCS storage classes |
| GCP Cloud SQL | GSQL1–GSQL5 | Cloud SQL MySQL and PostgreSQL |
| GCP Effective Pricing | GCPSTO1–3, GCPDB1–3, GCPNET1–3, GCPEGR1–3 | Contract/effective pricing paths |
| Multi-product AWS vs GCP | MP1–MP5 | Side-by-side price comparisons |
| Multi-region | MR1–MR5 | Cross-region price comparisons |
| Multi-region Stack | MRS1–MRS3 | Multi-region multi-service stacks |
| Cross-Cloud | CCR1–CCR3 | Cross-cloud comparisons |
| Azure Simple | AZ1–AZ5 | Azure VM, storage, reserved pricing |
| Azure Complex | AZX1–AZX3 | Multi-service Azure architectures |
| Azure SQL | AZSQL1–AZSQL5 | Azure SQL Database vCore tiers, HA, reserved |
| Azure Cosmos DB | AZCOS1–AZCOS5 | Provisioned, serverless, autoscale |
| Azure AKS | AZAKS1–AZAKS4 | AKS cluster management, spot nodes |
| Azure Functions | AZFN1–AZFN6 | Consumption plan, free tier deduction |
| Azure OpenAI | AZAI1–AZAI6 | GPT-4o, GPT-4o-mini, GPT-4, o1 token pricing |
| Azure Monitor + CDN | AZMON1–3, AZCDN1, AZFD1–2 | Log Analytics, CDN, Front Door |
| Azure New Services | NSV1–NSV6 | SQL, Functions, AKS, OpenAI, Monitor, CDN spot-checks |
| Advanced AWS | AA1–AA5 | Lambda, S3, RDS, reserved instances |
| Multi-cloud 3-way | MC1–MC5 | AWS + GCP + Azure |
| Inter-region Egress | EGR1–EGR5 | AWS, GCP, Azure inter-region egress |
| Azure Egress | AZEGR1–AZEGR3 | Azure bandwidth pricing |
| Network Egress (tiered) | NET_EGR1–NET_EGR8 | Tiered egress costs, blended rates, cross-region |
| Cross-cloud Egress | EGR_X1–EGR_X3 | Multi-provider egress comparison, large volumes |
| Trust Metadata | TRUST1–TRUST2 | `as_of`, `source_url`, `cache_age_seconds` fields |
| FinOps Cross-region | FCR1–FCR2 | Cheapest-region queries, spot vs reserved |
| BoM Resources | BOM_RES1–BOM_RES2 | BoM with mixed resource types |
| Recommendations | REC1 | Basic pricing lookup smoke test |

## Failure criteria

Each response is automatically graded. A test **passes** unless one of:

| Flag | Meaning |
|------|---------|
| `HALLUCINATION` | Dollar amount in the answer that cannot be derived from tool results (direct match, arithmetic, or combination) |
| `TRUNCATION` | Region/instance list looks suspiciously short; `list_instance_types` hit its cap |
| `UNANCHORED` | Answer omits region/instance/OS when the prompt specified them and tools returned data |
| `MISSING_DATA` | Public pricing was available but no tool call was made to retrieve it |
| `NO_ANSWER` | Empty or thinking-only response |
| `API_KEY` | GCP/AWS credential error — expected in environments without keys configured (not counted as failure) |

## Setup

### 1. Prerequisites

- [uv](https://docs.astral.sh/uv/) installed
- OpenCloudCosts MCP server checked out and working (`uv run pytest` passes)
- An OpenAI-compatible LLM server running locally or remotely

### 2. Configure the LLM endpoint

```bash
cd local-test-harness
cp .env.example .env
# Edit .env — set OCC_LLM_BASE_URL and OCC_LLM_MODEL
```

**LM Studio** (recommended for local runs):
1. Download and install [LM Studio](https://lmstudio.ai)
2. Load a model (≥ 70B parameters recommended for best results; Qwen 2.5 72B or Llama 3.3 70B work well)
3. Start the local server (default port 1234)
4. Set `OCC_LLM_BASE_URL=http://localhost:1234` and `OCC_LLM_MODEL=<identifier from LM Studio>`

**OpenAI / hosted provider:**
```
OCC_LLM_BASE_URL=https://api.openai.com
OCC_LLM_MODEL=gpt-4o
OCC_LLM_API_KEY=sk-...
```

**Ollama:**
```
OCC_LLM_BASE_URL=http://localhost:11434
OCC_LLM_MODEL=qwen2.5:72b
```

### 3. Configure cloud credentials (optional)

GCP pricing requires a free API key — set `OCC_GCP_API_KEY` in the project root `.env`. Without it, all GCP tests will still run but will receive an `API_KEY` flag (expected, not counted as failure).

AWS effective pricing requires credentials with Cost Explorer access. Public pricing (EC2, EBS, RDS list prices) works without credentials.

Azure pricing is fully public — no credentials needed.

### 4. Run

```bash
# All 199 prompts
uv run local-test-harness/run_tests.py

# Specific prompts
uv run local-test-harness/run_tests.py --ids C1,C4,AS1,GS1

# Override LLM endpoint without editing .env
uv run local-test-harness/run_tests.py \
  --llm-base-url http://localhost:1234 \
  --model my-model-name
```

Results are saved to `local-test-harness/results/<timestamp>/`:
- `<PROMPT_ID>_trace.json` — full message history, tool calls, and final answer
- `summary.json` — per-prompt status summary

### 5. Analyse results

Two complementary analysis tools are available:

**Heuristic analyser** (`analyse.py`) — fast, no LLM required. Checks numeric grounding, truncation, missing tool calls, and empty answers.

```bash
# Analyse most recent run
uv run local-test-harness/analyse.py

# Analyse a specific run directory
uv run local-test-harness/analyse.py local-test-harness/results/20260418_042813
```

Writes:
- `analysis.json` — structured results per prompt
- `improvement_plan.md` — recommended fixes grouped by failure type

**LLM judge** (`judge.py`) — semantic evaluation using a language model. Checks whether every figure in the answer is grounded in tool results, whether the answer is complete, and whether it addresses all parts of the question.

```bash
# Judge latest run (uses same LLM as configured in .env)
uv run local-test-harness/judge.py

# Judge a specific run, specific prompts, 8 concurrent calls
uv run local-test-harness/judge.py results/matrix_20260424_044537 --ids C1,X1,M1 --parallel 8
```

The judge can use a separate model — set `OCC_JUDGE_BASE_URL` / `OCC_JUDGE_MODEL` / `OCC_JUDGE_API_KEY` to override the test-model config.

Writes `judge.json` to the run directory. If `analysis.json` is also present, the judge prints an agreement comparison between the two methods.

## LLM recommendations

The harness is model-agnostic but results vary significantly by model capability. For meaningful results:

- **Parameter count**: ≥ 70B parameters strongly recommended. Smaller models tend to hallucinate prices or skip tool calls.
- **Tool-call support**: The model must support OpenAI-style function calling (`tools` + `tool_choice`).
- **Thinking/reasoning**: Extended thinking (`enable_thinking`) is disabled by default — it slows runs significantly. The system prompt is sufficient for most models.
- **Temperature**: Fixed at 0.3 for reproducibility.

## Results directory

Run results are gitignored. The `results/` directory contains a `.gitkeep` placeholder so the directory is tracked without committing any run data.

To share results, export the `analysis.json` from a run:
```bash
cat local-test-harness/results/20260418_042813/analysis.json
```

## Contributing

To add new test prompts, edit the `TEST_PROMPTS` dict in `run_tests.py`. Follow the existing naming convention:

- Use a 2–4 character prefix for the category (e.g. `GK` for GCP GKE)
- Number prompts sequentially within the category
- Keep prompts specific enough to have a deterministic correct answer

To improve the analyser's grounding logic, edit `analyse.py` — the `_is_grounded()` function controls what arithmetic derivations are accepted.
