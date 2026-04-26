#!/usr/bin/env python3
"""
LLM Judge for OpenCloudCosts test harness.

Evaluates factual accuracy of model answers against tool results.
Complements the heuristic analyse.py — outputs judge.json per run.

Usage:
    uv run local-test-harness/judge.py [results-dir]
    uv run local-test-harness/judge.py [results-dir] --ids C1,X1
    uv run local-test-harness/judge.py [results-dir] --parallel 4

Configuration (from .env or environment):
    OCC_JUDGE_BASE_URL   Judge LLM base URL (falls back to OCC_LLM_BASE_URL)
    OCC_JUDGE_MODEL      Judge model identifier (falls back to OCC_LLM_MODEL)
    OCC_JUDGE_API_KEY    Judge API key (falls back to OCC_LLM_API_KEY)
"""
# /// script
# requires-python = ">=3.11"
# dependencies = ["httpx"]
# ///

import argparse
import asyncio
import json
import os
import re
import sys
from pathlib import Path

import httpx

HARNESS_DIR = Path(__file__).parent
RESULTS_ROOT = HARNESS_DIR / "results"
# Total character budget for tool results across all calls in one judge prompt
TOOL_RESULT_TOTAL_BUDGET = 6000


def _load_dotenv(env_file: Path) -> None:
    if not env_file.exists():
        return
    for line in env_file.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, _, value = line.partition("=")
        key = key.strip()
        value = value.strip().strip('"').strip("'")
        if key and key not in os.environ:
            os.environ[key] = value


_load_dotenv(HARNESS_DIR / ".env")

# Judge can use a different model from the test model — falls back to test config
JUDGE_BASE_URL = os.environ.get("OCC_JUDGE_BASE_URL") or os.environ.get("OCC_LLM_BASE_URL", "")
JUDGE_MODEL = os.environ.get("OCC_JUDGE_MODEL") or os.environ.get("OCC_LLM_MODEL", "")
JUDGE_API_KEY = os.environ.get("OCC_JUDGE_API_KEY") or os.environ.get("OCC_LLM_API_KEY", "")

# Import TEST_PROMPTS so we can look up question text for run_matrix.py traces
# (which omit the `prompt` field in the trace)
try:
    sys.path.insert(0, str(HARNESS_DIR))
    from run_tests import TEST_PROMPTS as _TEST_PROMPTS
except ImportError:
    _TEST_PROMPTS: dict = {}

JUDGE_SYSTEM = (
    "You are a factual accuracy judge for cloud pricing AI responses. "
    "Your only job is to check whether the assistant's answer is grounded in "
    "the tool results shown. Do NOT use your own cloud pricing knowledge — "
    "the tool results are the sole source of truth. "
    "Respond ONLY with valid JSON and nothing else."
)

_JUDGE_TEMPLATE = """\
TASK: Judge whether the ASSISTANT'S ANSWER is factually correct based on the TOOL RESULTS below.

RULES:
1. Every price/cost figure in the answer must appear in (or be directly derivable from)
   the tool results.
   - Acceptable: rounding within 2% ($0.384 → $0.38), or simple arithmetic
     (hourly × 730 = monthly, price × quantity, difference between two tool prices).
   - NOT acceptable: figures absent from tool results and not arithmetically derivable.
2. The answer must address all parts of the QUESTION (completeness).
3. If all tool calls returned credential/API-key errors, the correct response is to
   acknowledge the limitation — that counts as correct behaviour, not a failure.
4. Hedging ("approximately") is only acceptable when the tool result itself lacks
   a precise number.

VERDICT:
  "pass"    — all figures grounded, question fully addressed, no material omissions
  "partial" — ≤1 minor ungrounded figure OR minor omission (question not fully answered)
  "fail"    — major hallucinated/ungrounded figure, key question part skipped,
              or answer uses training-data prices rather than tool results

Respond with ONLY this JSON structure (no text before or after):
{{
  "verdict": "pass",
  "score": 9,
  "issues": [],
  "summary": "All prices from tool results; question fully answered."
}}

QUESTION:
{question}

TOOL RESULTS (source of truth — {n_calls} tool call(s)):
{tool_results}

ASSISTANT'S ANSWER:
{answer}
"""


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _get_prompt(trace: dict) -> str:
    """Return question text from trace or fall back to TEST_PROMPTS lookup."""
    if trace.get("prompt"):
        return trace["prompt"]
    return _TEST_PROMPTS.get(trace.get("prompt_id", ""), "[question not found]")


def _format_tool_results(tool_calls: list[dict]) -> str:
    """Compact, budget-capped representation of all tool calls for the judge prompt."""
    if not tool_calls:
        return "(no tool calls made)"
    n = len(tool_calls)
    per_call = max(200, TOOL_RESULT_TOTAL_BUDGET // n)
    lines = []
    for i, tc in enumerate(tool_calls, 1):
        name = tc.get("tool", "?")
        args_str = json.dumps(tc.get("args", {}))[:100]
        result_str = json.dumps(tc.get("result", {}))
        if len(result_str) > per_call:
            result_str = result_str[:per_call] + "…"
        lines.append(f"[{i}] {name}({args_str})")
        lines.append(f"    → {result_str}")
    return "\n".join(lines)


def _parse_judge_json(text: str) -> dict | None:
    """Extract verdict JSON from judge response, tolerating markdown fences."""
    text = re.sub(r"^```(?:json)?\s*", "", text.strip(), flags=re.MULTILINE)
    text = re.sub(r"\s*```\s*$", "", text.strip(), flags=re.MULTILINE)
    m = re.search(r'\{.*\}', text, flags=re.DOTALL)
    if not m:
        return None
    try:
        d = json.loads(m.group())
        if d.get("verdict") in ("pass", "partial", "fail", "error"):
            return d
    except json.JSONDecodeError:
        pass
    return None


async def _llm_call(messages: list[dict], client: httpx.AsyncClient) -> str:
    headers = {"Authorization": f"Bearer {JUDGE_API_KEY}"} if JUDGE_API_KEY else {}
    resp = await client.post(
        f"{JUDGE_BASE_URL}/v1/chat/completions",
        json={
            "model": JUDGE_MODEL,
            "messages": messages,
            "temperature": 0.0,
            # Thinking models (e.g. Qwen3) spend many tokens on reasoning before content —
            # 4096 is enough to complete both the reasoning chain and the JSON response.
            "max_tokens": 4096,
            "enable_thinking": False,
        },
        headers=headers,
    )
    resp.raise_for_status()
    msg = resp.json()["choices"][0]["message"]
    # Prefer content; fall back to reasoning_content for models that separate them
    return msg.get("content") or msg.get("reasoning_content") or ""


# ---------------------------------------------------------------------------
# Core judge logic
# ---------------------------------------------------------------------------

async def judge_trace(trace: dict, client: httpx.AsyncClient) -> dict:
    answer = trace.get("final_answer") or ""
    prompt = _get_prompt(trace)
    tool_calls = trace.get("tool_calls", [])

    # Fast-path failures that don't need an LLM call
    if not answer.strip() or answer.strip().startswith("[thinking only"):
        return {
            "verdict": "fail",
            "score": 0,
            "issues": ["No final answer produced"],
            "summary": "Model produced no answer.",
        }

    if not tool_calls:
        return {
            "verdict": "fail",
            "score": 1,
            "issues": ["No tool calls made — answer not grounded in any tool results"],
            "summary": "Answer produced without tool calls (likely hallucination).",
        }

    tool_results_str = _format_tool_results(tool_calls)
    user_content = _JUDGE_TEMPLATE.format(
        question=prompt,
        tool_results=tool_results_str,
        n_calls=len(tool_calls),
        answer=answer[:3000],
    )
    messages = [
        {"role": "system", "content": JUDGE_SYSTEM},
        {"role": "user", "content": user_content},
    ]

    for attempt in range(2):
        try:
            content = await _llm_call(messages, client)
            parsed = _parse_judge_json(content)
            if parsed:
                return parsed
            # Retry once with a nudge if JSON was unparseable
            messages.append({"role": "assistant", "content": content})
            messages.append({
                "role": "user",
                "content": (
                    "Your response was not valid JSON. "
                    "Reply with ONLY the JSON object in the exact format specified — "
                    "no explanation, no markdown, just the JSON."
                ),
            })
        except Exception as e:
            if attempt == 1:
                return {
                    "verdict": "error",
                    "score": 0,
                    "issues": [f"Judge LLM call failed: {e}"],
                    "summary": f"Judge error: {e}",
                }

    return {
        "verdict": "error",
        "score": 0,
        "issues": ["Invalid JSON after retry"],
        "summary": "Judge returned unparseable response after retry.",
    }


# ---------------------------------------------------------------------------
# Trace loading
# ---------------------------------------------------------------------------

def load_traces(run_dir: Path, ids: list[str] | None) -> dict[str, dict]:
    traces: dict[str, dict] = {}
    for f in sorted(run_dir.glob("*_trace.json")):
        stem = f.stem.replace("_trace", "")
        m = re.search(r'_([A-Z][A-Z0-9_]*)$', stem)
        pid = m.group(1) if m else stem
        if ids and pid not in ids:
            continue
        traces[pid] = json.loads(f.read_text())
    return traces


# ---------------------------------------------------------------------------
# Main runner
# ---------------------------------------------------------------------------

async def run_judge(run_dir: Path, ids: list[str] | None, parallel: int) -> dict:
    traces = load_traces(run_dir, ids)
    if not traces:
        print("No traces found in this directory.")
        return {}

    print(f"Judging {len(traces)} trace(s) — model: {JUDGE_MODEL}")
    print(f"Run dir: {run_dir}\n")

    results: dict[str, dict] = {}
    sem = asyncio.Semaphore(parallel)
    print_lock = asyncio.Lock()

    async def judge_one(pid: str, trace: dict) -> None:
        async with sem:
            async with httpx.AsyncClient(timeout=120.0) as client:
                verdict = await judge_trace(trace, client)
        results[pid] = {
            "verdict": verdict.get("verdict", "error"),
            "score": verdict.get("score", 0),
            "issues": verdict.get("issues", []),
            "summary": verdict.get("summary", ""),
            "rounds": trace.get("rounds", 0),
            "tool_calls": len(trace.get("tool_calls", [])),
            "answer_preview": (trace.get("final_answer") or "")[:200],
        }
        v = results[pid]["verdict"]
        icon = "✓" if v == "pass" else ("~" if v == "partial" else "✗")
        async with print_lock:
            print(
                f"  {icon} [{pid:8s}] {v:7s} score={results[pid]['score']:2d}"
                f" — {results[pid]['summary'][:80]}"
            )

    await asyncio.gather(*[judge_one(pid, trace) for pid, trace in sorted(traces.items())])

    # Persist results
    out_file = run_dir / "judge.json"
    out_file.write_text(json.dumps(results, indent=2))

    pass_c = sum(1 for r in results.values() if r["verdict"] == "pass")
    partial_c = sum(1 for r in results.values() if r["verdict"] == "partial")
    fail_c = sum(1 for r in results.values() if r["verdict"] in ("fail", "error"))
    total = len(results)

    print(f"\n{'='*64}")
    print(f"JUDGE SUMMARY — {run_dir.name}")
    print(f"{'='*64}")
    print(f"  Pass:    {pass_c}/{total}  ({100 * pass_c // total if total else 0}%)")
    print(f"  Partial: {partial_c}/{total}")
    print(f"  Fail:    {fail_c}/{total}")

    # Cross-reference with heuristic analyse.py output when available
    analysis_file = run_dir / "analysis.json"
    if analysis_file.exists():
        analysis = json.loads(analysis_file.read_text())
        agree = disagree_stricter = disagree_lenient = 0
        disagreements = []
        for pid in sorted(results):
            jr = results[pid]
            ar = analysis.get(pid)
            if ar is None:
                continue
            heuristic_ok = ar.get("pass", True)
            judge_ok = jr["verdict"] in ("pass", "partial")
            if heuristic_ok == judge_ok:
                agree += 1
            elif heuristic_ok and not judge_ok:
                disagree_stricter += 1
                disagreements.append((pid, "heuristic=pass", f"judge={jr['verdict']}", jr["summary"][:60]))
            else:
                disagree_lenient += 1
                disagreements.append((pid, "heuristic=fail", f"judge={jr['verdict']}", jr["summary"][:60]))

        total_compared = agree + disagree_stricter + disagree_lenient
        print(f"\n── vs heuristic analyse.py ({total_compared} compared) ──")
        if disagreements:
            for pid, h, j, note in disagreements:
                print(f"  [{pid}] {h} / {j} — {note}")
        pct = 100 * agree // total_compared if total_compared else 0
        print(f"  Agreement: {agree}/{total_compared} ({pct}%)  "
              f"judge stricter: {disagree_stricter}  judge more lenient: {disagree_lenient}")

    print(f"\nResults written to: {out_file}")
    return results


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

def main() -> None:
    parser = argparse.ArgumentParser(
        description="LLM judge for OpenCloudCosts harness",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=(
            "Configuration via .env or environment variables:\n"
            "  OCC_JUDGE_BASE_URL   Judge LLM base URL (falls back to OCC_LLM_BASE_URL)\n"
            "  OCC_JUDGE_MODEL      Judge model (falls back to OCC_LLM_MODEL)\n"
            "  OCC_JUDGE_API_KEY    Judge API key (falls back to OCC_LLM_API_KEY)\n\n"
            "Examples:\n"
            "  # Judge latest run (all traces):\n"
            "  uv run local-test-harness/judge.py\n\n"
            "  # Judge specific tests from a named run directory:\n"
            "  uv run local-test-harness/judge.py results/matrix_20260424_044537 --ids C1,X1,M1\n\n"
            "  # Run 8 judge calls concurrently:\n"
            "  uv run local-test-harness/judge.py --parallel 8\n"
        ),
    )
    parser.add_argument(
        "run_dir",
        nargs="?",
        help="Results directory to judge (default: latest in results/)",
    )
    parser.add_argument(
        "--ids",
        default="",
        help="Comma-separated prompt IDs to judge (default: all traces in the directory)",
    )
    parser.add_argument(
        "--parallel",
        type=int,
        default=4,
        help="Number of concurrent judge LLM calls (default: 4)",
    )
    args = parser.parse_args()

    if args.run_dir:
        run_dir = Path(args.run_dir)
    else:
        dirs = [d for d in sorted(RESULTS_ROOT.iterdir()) if d.is_dir()]
        if not dirs:
            print("No results found. Run run_tests.py or run_matrix.py first.")
            sys.exit(1)
        run_dir = dirs[-1]

    if not run_dir.exists():
        print(f"Directory not found: {run_dir}")
        sys.exit(1)

    if not JUDGE_BASE_URL:
        print("Error: OCC_LLM_BASE_URL (or OCC_JUDGE_BASE_URL) not set in .env or environment.")
        sys.exit(1)
    if not JUDGE_MODEL:
        print("Error: OCC_LLM_MODEL (or OCC_JUDGE_MODEL) not set in .env or environment.")
        sys.exit(1)

    ids = [x.strip() for x in args.ids.split(",") if x.strip()] if args.ids else None
    asyncio.run(run_judge(run_dir, ids, args.parallel))


if __name__ == "__main__":
    main()
