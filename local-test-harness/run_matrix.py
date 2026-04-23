#!/usr/bin/env python3
"""
Multi-model harness matrix runner for OpenCloudCosts MCP.

Runs the full test suite against multiple models on an LMStudio-compatible
server and writes a pass/fail matrix to docs/harness-results.md (or --output).

Usage:
    uv run local-test-harness/run_matrix.py \\
        --base-url http://192.168.50.230:1234 \\
        --models "qwen3.5-122b-a10b@q6_k,unsloth/qwen3.6-35b-a3b,google/gemma-4-26b-a4b" \\
        [--ids all] \\
        [--parallel 2] \\
        [--output docs/harness-results.md]
"""
# /// script
# requires-python = ">=3.11"
# dependencies = ["httpx", "mcp"]
# ///

import argparse
import asyncio
import json
import os
import re
import sys
from datetime import datetime, timezone
from pathlib import Path

import httpx
from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client

# ---------------------------------------------------------------------------
# Import shared test data from run_tests.py
# ---------------------------------------------------------------------------
_HARNESS_DIR = Path(__file__).parent
sys.path.insert(0, str(_HARNESS_DIR))

from run_tests import (  # noqa: E402
    MAX_TOOL_ROUNDS,
    SYSTEM_PROMPT,
    TEST_PROMPTS,
    _preview,
    mcp_tool_to_openai,
)

_PROJECT_DIR = _HARNESS_DIR.parent
_MCP_COMMAND = "uv"
_MCP_ARGS = ["run", "--directory", str(_PROJECT_DIR), "opencloudcosts"]

# ---------------------------------------------------------------------------
# Category labels derived from test ID prefix
# ---------------------------------------------------------------------------
_CATEGORY_MAP: dict[str, str] = {
    "C":    "Common",
    "M":    "Multi-cloud",
    "X":    "Complex BoM",
    "AS":   "AWS Simple",
    "GS":   "GCP Simple",
    "MP":   "AWS vs GCP",
    "MR":   "Multi-region",
    "CX":   "Complex TCO",
    "AZX":  "Azure Complex",
    "MRS":  "Multi-region Stack",
    "CCR":  "Cross-Cloud",
    "AZ":   "Azure Simple",
    "AA":   "Advanced AWS",
    "MC":   "3-Cloud Compare",
    "GK":   "GCP GKE",
    "GM":   "GCP Memorystore",
    "GB":   "GCP BigQuery",
    "GV":   "GCP Vertex AI",
    "GN":   "GCP Networking",
    "GC":   "GCP Cloud Armor",
    "GCX":  "GCP Complex",
    "GGCS": "GCP Cloud Storage",
    "GSQL": "GCP Cloud SQL",
}


def _category(test_id: str) -> str:
    # Match longest prefix first
    for prefix in sorted(_CATEGORY_MAP, key=len, reverse=True):
        if re.match(rf"^{re.escape(prefix)}\d", test_id) or test_id == prefix:
            return _CATEGORY_MAP[prefix]
    return "Other"


def _short_model(model: str) -> str:
    """Shorten a model identifier for use as a column header."""
    # Strip registry prefix (unsloth/, google/, etc.)
    name = model.split("/")[-1]
    # Strip quantisation suffix (@q6_k, etc.)
    name = name.split("@")[0]
    # Truncate long names at 22 chars
    return name if len(name) <= 22 else name[:19] + "…"


# ---------------------------------------------------------------------------
# Single-test runner (model-parameterised)
# ---------------------------------------------------------------------------

async def _run_single(
    prompt_id: str,
    prompt: str,
    mcp_session: ClientSession,
    openai_tools: list[dict],
    base_url: str,
    model: str,
    api_key: str,
    run_dir: Path,
) -> dict:
    print(f"  [{prompt_id}] {prompt[:80]}")

    messages = [
        {"role": "system", "content": SYSTEM_PROMPT},
        {"role": "user", "content": prompt},
    ]

    trace: dict = {
        "prompt_id": prompt_id,
        "model": model,
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "rounds": 0,
        "tool_calls": [],
        "final_answer": None,
        "error": None,
    }

    async with httpx.AsyncClient(timeout=300.0) as client:
        for round_num in range(MAX_TOOL_ROUNDS):
            trace["rounds"] = round_num + 1
            payload = {
                "model": model,
                "messages": messages,
                "tools": openai_tools,
                "tool_choice": "auto",
                "temperature": 0.3,
                "max_tokens": 16384,
                "enable_thinking": False,
            }
            headers = {"Authorization": f"Bearer {api_key}"} if api_key else {}
            try:
                resp = await client.post(
                    f"{base_url}/v1/chat/completions",
                    json=payload,
                    headers=headers,
                )
                resp.raise_for_status()
                data = resp.json()
            except Exception as e:
                trace["error"] = f"API error round {round_num + 1}: {e}"
                print(f"    ERROR: {e}")
                break

            choice = data["choices"][0]
            assistant_msg = choice["message"]
            finish_reason = choice.get("finish_reason", "")
            content = assistant_msg.get("content") or ""
            reasoning = assistant_msg.get("reasoning_content") or ""
            thinking_only = finish_reason == "stop" and not content.strip() and reasoning

            if thinking_only:
                stub = {**assistant_msg, "content": ""}
                messages.append(stub)
                recovery = {"role": "user", "content": "Please write your final answer now."}
                messages.append(recovery)
                try:
                    r = await client.post(
                        f"{base_url}/v1/chat/completions",
                        json={**payload, "messages": messages, "tools": []},
                        headers=headers,
                    )
                    r.raise_for_status()
                    rd = r.json()
                    if rd.get("choices"):
                        assistant_msg = rd["choices"][0]["message"]
                        finish_reason = rd["choices"][0].get("finish_reason", "")
                        content = assistant_msg.get("content") or ""
                        thinking_only = not content.strip()
                except Exception:
                    pass

            if thinking_only:
                assistant_msg = {**assistant_msg, "content": f"[thinking only]\n{reasoning[:300]}"}

            messages.append(assistant_msg)
            tool_calls = assistant_msg.get("tool_calls") or []

            if not tool_calls or finish_reason in ("stop", "length"):
                trace["final_answer"] = assistant_msg.get("content") or reasoning
                if finish_reason == "length":
                    trace["error"] = f"max_tokens at round {round_num + 1}"
                break

            for tc in tool_calls:
                fn = tc["function"]
                tool_name = fn["name"]
                try:
                    tool_args = json.loads(fn.get("arguments") or "{}")
                except json.JSONDecodeError:
                    tool_args = {}
                print(f"    → {tool_name}({_preview(tool_args, 80)})")
                try:
                    mcp_result = await mcp_session.call_tool(tool_name, tool_args)
                    text_parts = [c.text for c in mcp_result.content if hasattr(c, "text")]
                    raw = "".join(text_parts)
                    try:
                        tool_result = json.loads(raw)
                    except json.JSONDecodeError:
                        tool_result = {"text": raw}
                except Exception as e:
                    tool_result = {"error": f"MCP error: {e}"}
                print(f"       ← {_preview(tool_result, 100)}")
                trace["tool_calls"].append({
                    "round": round_num,
                    "tool": tool_name,
                    "args": tool_args,
                    "result": tool_result,
                })
                messages.append({
                    "role": "tool",
                    "tool_call_id": tc["id"],
                    "content": json.dumps(tool_result),
                })
        else:
            trace["error"] = f"max rounds ({MAX_TOOL_ROUNDS}) exhausted"
            print(f"    ✗ max rounds")

    status = "✅" if not trace["error"] else "❌"
    print(f"    {status} {round_num + 1} round(s), {len(trace['tool_calls'])} tool call(s)"
          + (f" — {trace['error']}" if trace["error"] else ""))

    # Save per-test trace
    safe_model = re.sub(r"[^a-zA-Z0-9_-]", "_", model)
    trace_file = run_dir / f"{safe_model}_{prompt_id}_trace.json"
    trace_file.write_text(json.dumps(trace, indent=2, default=str))
    return trace


# ---------------------------------------------------------------------------
# Per-model runner (all tests in sequence, sharing one MCP session)
# ---------------------------------------------------------------------------

async def run_model(
    model: str,
    base_url: str,
    api_key: str,
    tests: dict[str, str],
    run_dir: Path,
    parallel: int = 1,
) -> dict[str, dict]:
    """Run all tests for one model. Returns {prompt_id: trace}."""
    print(f"\n{'━' * 68}")
    print(f"  Model: {model}")
    print(f"{'━' * 68}")

    server_params = StdioServerParameters(
        command=_MCP_COMMAND, args=_MCP_ARGS, env={**os.environ}
    )

    results: dict[str, dict] = {}
    items = list(tests.items())

    async def _worker(bucket: list[tuple[str, str]]) -> None:
        async with stdio_client(server_params) as (read, write):
            async with ClientSession(read, write) as session:
                await session.initialize()
                tools_resp = await session.list_tools()
                openai_tools = [mcp_tool_to_openai(t) for t in tools_resp.tools]
                for pid, prompt in bucket:
                    try:
                        trace = await _run_single(
                            pid, prompt, session, openai_tools,
                            base_url, model, api_key, run_dir,
                        )
                        results[pid] = trace
                    except Exception as e:
                        print(f"  FATAL [{pid}]: {e}")
                        results[pid] = {
                            "prompt_id": pid, "model": model,
                            "error": f"fatal: {e}", "rounds": 0,
                            "tool_calls": [], "final_answer": None,
                        }

    buckets: list[list[tuple[str, str]]] = [[] for _ in range(max(1, min(parallel, len(items))))]
    for i, item in enumerate(items):
        buckets[i % len(buckets)].append(item)

    await asyncio.gather(*[_worker(b) for b in buckets if b])
    return results


# ---------------------------------------------------------------------------
# Markdown matrix generator
# ---------------------------------------------------------------------------

def _build_markdown(
    models: list[str],
    tests: dict[str, str],
    all_results: dict[str, dict[str, dict]],
    base_url: str,
    run_ts: str,
) -> str:
    short_names = [_short_model(m) for m in models]

    lines = [
        "# OpenCloudCosts — LLM Harness Results",
        "",
        f"**Run date:** {run_ts}  ",
        f"**Server:** `{base_url}`  ",
        f"**Tests:** {len(tests)}  ",
        "",
        "## Summary",
        "",
        "| Model | Pass | Fail | Pass Rate |",
        "|-------|-----:|-----:|----------:|",
    ]

    model_pass: dict[str, int] = {}
    model_fail: dict[str, int] = {}
    for model in models:
        results = all_results.get(model, {})
        p = sum(1 for pid in tests if not (results.get(pid) or {}).get("error"))
        f = len(tests) - p
        model_pass[model] = p
        model_fail[model] = f
        rate = f"{p / len(tests) * 100:.1f}%" if tests else "—"
        lines.append(f"| {model} | {p} | {f} | {rate} |")

    # Per-test matrix
    header = "| ID | Category | " + " | ".join(short_names) + " |"
    sep = "|:---|:---------|" + "|".join([":---:"] * len(models)) + "|"
    lines += ["", "## Per-test matrix", "", header, sep]

    for pid in tests:
        cat = _category(pid)
        cells = []
        for model in models:
            trace = (all_results.get(model) or {}).get(pid)
            if trace is None:
                cells.append("—")
            elif trace.get("error"):
                err = str(trace["error"])[:60].replace("|", "\\|")
                cells.append(f"❌ `{err}`" if len(err) < 30 else "❌")
            else:
                rounds = trace.get("rounds", "?")
                cells.append(f"✅ ({rounds}r)")
        lines.append(f"| {pid} | {cat} | " + " | ".join(cells) + " |")

    # Footer totals
    totals = []
    for model in models:
        p = model_pass[model]
        totals.append(f"**{p}/{len(tests)}**")
    lines.append(f"| **Total** | | " + " | ".join(totals) + " |")

    lines += [
        "",
        "---",
        "",
        "## Model details",
        "",
    ]
    for model, short in zip(models, short_names):
        p = model_pass[model]
        f = model_fail[model]
        lines.append(f"- **{short}** (`{model}`): {p} passed, {f} failed")

    lines += [
        "",
        f"_Generated by `local-test-harness/run_matrix.py` on {run_ts}_",
        "",
    ]
    return "\n".join(lines)


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

async def main(
    base_url: str,
    models: list[str],
    ids: list[str],
    api_key: str,
    parallel: int,
    parallel_models: int,
    output_path: Path,
) -> None:
    run_ts = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M UTC")
    run_dir = _HARNESS_DIR / "results" / f"matrix_{datetime.now(timezone.utc).strftime('%Y%m%d_%H%M%S')}"
    run_dir.mkdir(parents=True, exist_ok=True)

    if "all" in ids:
        tests = TEST_PROMPTS
    else:
        tests = {k: v for k, v in TEST_PROMPTS.items() if k in ids}
        missing = set(ids) - set(tests)
        if missing:
            print(f"Warning: unknown prompt IDs: {missing}")

    if not tests:
        print("No tests selected. Use --ids C1,C2,... or --ids all")
        sys.exit(1)

    print(f"Matrix run: {len(models)} model(s) × {len(tests)} test(s) = {len(models) * len(tests)} total")
    if parallel_models > 1:
        print(f"  Running up to {parallel_models} models in parallel")
    print(f"Run dir: {run_dir}")

    all_results: dict[str, dict[str, dict]] = {}

    if parallel_models <= 1:
        for model in models:
            all_results[model] = await run_model(model, base_url, api_key, tests, run_dir, parallel)
    else:
        # Run models concurrently in groups of parallel_models.
        # Each group finishes before the next starts so the server isn't overloaded.
        sem = asyncio.Semaphore(parallel_models)

        async def _run_with_sem(model: str) -> tuple[str, dict[str, dict]]:
            async with sem:
                return model, await run_model(model, base_url, api_key, tests, run_dir, parallel)

        for result in await asyncio.gather(*[_run_with_sem(m) for m in models]):
            model, results = result
            all_results[model] = results

    # Write markdown
    md = _build_markdown(models, tests, all_results, base_url, run_ts)
    output_path.parent.mkdir(parents=True, exist_ok=True)
    output_path.write_text(md)
    print(f"\n✅ Results written to {output_path}")

    # Print summary
    print(f"\n{'═' * 68}")
    print("SUMMARY")
    print(f"{'═' * 68}")
    for model in models:
        results = all_results[model]
        p = sum(1 for pid in tests if not results.get(pid, {}).get("error"))
        print(f"  {_short_model(model)}: {p}/{len(tests)} passed")
    print(f"\nFull traces in {run_dir}/")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description="Run harness across multiple models and write a markdown results matrix",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=(
            "Examples:\n"
            "  # Run all tests against 3 models:\n"
            "  uv run local-test-harness/run_matrix.py \\\n"
            "      --base-url http://192.168.50.230:1234 \\\n"
            "      --models 'qwen3.5-122b-a10b@q6_k,unsloth/qwen3.6-35b-a3b,google/gemma-4-26b-a4b'\n\n"
            "  # Run a subset of tests:\n"
            "  uv run local-test-harness/run_matrix.py \\\n"
            "      --base-url http://192.168.50.230:1234 \\\n"
            "      --models 'qwen3.5-122b-a10b@q6_k' \\\n"
            "      --ids C1,C2,M1,X1\n"
        ),
    )
    parser.add_argument(
        "--base-url",
        required=True,
        help="OpenAI-compatible LLM server base URL (e.g. http://192.168.50.230:1234)",
    )
    parser.add_argument(
        "--models",
        required=True,
        help="Comma-separated model identifiers as returned by the server",
    )
    parser.add_argument(
        "--ids",
        default="all",
        help="Comma-separated test IDs to run (e.g. C1,C2,M1) or 'all' (default)",
    )
    parser.add_argument(
        "--api-key",
        default="",
        help="Optional API key for the LLM server",
    )
    parser.add_argument(
        "--parallel",
        type=int,
        default=1,
        help="Parallel workers per model (default: 1)",
    )
    parser.add_argument(
        "--parallel-models",
        type=int,
        default=1,
        metavar="N",
        help=(
            "Run up to N models concurrently (default: 1 = sequential). "
            "Use 2 for smaller models that fit together (e.g. 35b+26b); "
            "keep at 1 for large models that need exclusive GPU memory (e.g. 122b)."
        ),
    )
    parser.add_argument(
        "--output",
        default=str(_PROJECT_DIR / "docs" / "harness-results.md"),
        help="Output markdown file (default: docs/harness-results.md)",
    )
    args = parser.parse_args()

    model_list = [m.strip() for m in args.models.split(",") if m.strip()]
    if not model_list:
        print("Error: --models must specify at least one model")
        sys.exit(1)

    id_list = [x.strip() for x in args.ids.split(",") if x.strip()]

    asyncio.run(main(
        base_url=args.base_url,
        models=model_list,
        ids=id_list,
        api_key=args.api_key,
        parallel=args.parallel,
        parallel_models=args.parallel_models,
        output_path=Path(args.output),
    ))
