# OpenCloudCosts — LLM Test Harness

An end-to-end quality harness that drives an LLM through the full MCP tool-call loop and automatically grades every response against four failure criteria. Works with any OpenAI-compatible LLM server.

## What it tests

The harness sends **109 curated prompts** covering every tool and provider combination:

| Category | IDs | What's tested |
|----------|-----|---------------|
| Common | C1–C4 | Basic AWS compute + storage |
| Multi-cloud | M1–M3 | AWS vs GCP vs Azure comparisons |
| Complex BoM/TCO | CX1–CX10 | Bill of Materials, unit economics |
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
| Multi-product AWS vs GCP | MP1–MP5 | Side-by-side price comparisons |
| Multi-region | MR1–MR5 | Cross-region price comparisons |
| Azure Simple | AZ1–AZ5 | Azure VM, storage, reserved pricing |
| Advanced AWS | AA1–AA5 | Lambda, S3, RDS, reserved instances |
| Multi-cloud 3-way | MC1–MC5 | AWS + GCP + Azure |

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
# All 109 prompts
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

```bash
# Analyse most recent run
uv run local-test-harness/analyse.py

# Analyse a specific run directory
uv run local-test-harness/analyse.py local-test-harness/results/20260418_042813
```

The analyser prints a per-category pass/fail report, failure breakdown, and writes:
- `analysis.json` — structured results per prompt
- `improvement_plan.md` — recommended fixes grouped by failure type

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
