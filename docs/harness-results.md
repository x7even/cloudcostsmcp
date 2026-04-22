# OpenCloudCosts — LLM Harness Results

This file is updated by running the multi-model matrix harness locally.

## How to update

```bash
# Run all 123 tests against all three models (sequentially — one large model at a time):
uv run local-test-harness/run_matrix.py \
    --base-url http://192.168.50.230:1234 \
    --models "qwen3.5-122b-a10b@q6_k,unsloth/qwen3.6-35b-a3b,google/gemma-4-26b-a4b" \
    --ids all \
    --parallel 2 \
    --output docs/harness-results.md

# Run the two smaller models in parallel (35b + 26b fit together):
uv run local-test-harness/run_matrix.py \
    --base-url http://192.168.50.230:1234 \
    --models "unsloth/qwen3.6-35b-a3b,google/gemma-4-26b-a4b" \
    --ids all \
    --parallel 2 \
    --parallel-models 2 \
    --output docs/harness-results.md
```

> **Note:** Per-test trace JSON files are written to `local-test-harness/results/matrix_<timestamp>/`.

---

_No results recorded yet. Run the harness to populate this file._
