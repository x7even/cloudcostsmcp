#!/usr/bin/env python3
"""
Analyse a completed test run against the 4 failure criteria:
  1. HALLUCINATION  — cost/spec figure not from a tool result
  2. TRUNCATION     — incomplete result set (should have had more data)
  3. UNANCHORED     — answer missing key params (region, OS, term, instance)
  4. MISSING_DATA   — public-API pricing not fetched when it should have been
  5. NO_ANSWER      — empty or thinking-only final answer
  6. API_KEY        — failed only because GCP/credentials not configured (expected)

Usage:
    uv run local-test-harness/analyse.py [results-dir]
    # omit results-dir to use latest
"""
# /// script
# requires-python = ">=3.11"
# dependencies = []
# ///

import itertools
import json
import re
import sys
from pathlib import Path

RESULTS_ROOT = Path(__file__).parent / "results"

# Keywords that suggest a figure was estimated rather than retrieved
# (kept as a secondary signal for answers with no dollar amounts)
ESTIMATE_PHRASES = [
    "typically", "usually", "approximately", "roughly", "around",
    "estimate", "estimated", "generally", "varies", "may cost",
    "could cost", "would cost", "from memory", "based on my knowledge",
    "based on general", "range from", "range of",
]

# Multipliers that represent legitimate unit conversions in cloud pricing:
#   instance counts (2–10), hours/month (730), hours/year (8760),
#   months (12, 24, 36, 60=5yr), days (24, 30), GB quantities (1000=1TB)
_ARITH_MULTIPLIERS = [2, 3, 4, 5, 6, 7, 8, 9, 10, 12, 24, 30, 36, 60, 100, 120, 730, 1000, 8760,
                      10_000, 100_000, 1_000_000, 10_000_000]

_DOLLAR_RE = re.compile(r'\$[\d,]+(?:\.\d+)?(?:[eE][+-]?\d+)?')

# Keywords that suggest the LLM acknowledged a missing data problem
MISSING_DATA_PHRASES = [
    "requires api key", "requires credentials", "requires authentication",
    "not available", "couldn't retrieve", "could not retrieve",
    "unable to retrieve", "failed to fetch",
]

ANCHOR_FIELDS = ["region", "instance", "os", "linux", "windows", "term",
                 "on.demand", "reserved", "per.hour", "per hour"]

NO_ANSWER_MARKERS = ["[thinking only", "no final answer", ""]


def load_traces(run_dir: Path) -> dict:
    traces = {}
    for f in sorted(run_dir.glob("*_trace.json")):
        pid = f.stem.replace("_trace", "")
        traces[pid] = json.loads(f.read_text())
    return traces


def _extract_dollar_amounts(text: str) -> list[float]:
    """Return all positive dollar amounts found in text as floats."""
    result = []
    for m in _DOLLAR_RE.findall(text):
        try:
            v = float(m.replace(",", "").lstrip("$"))
            if v > 0:
                result.append(v)
        except ValueError:
            pass
    return result


def _is_grounded(amount: float, tool_floats: list[float], tol: float = 0.03) -> bool:
    """
    Return True if *amount* can be explained by the tool result numbers.

    Checks (in order):
    1. Direct match within *tol* relative tolerance.
    2. amount ≈ tool_price × n  for n in _ARITH_MULTIPLIERS
       (covers per-unit price × quantity, e.g. $0.02/GB × 1000 GB = $20).
    3. amount ≈ |t1 − t2|  for any pair in tool_floats
       (covers savings / difference calculations).
    4. amount ≈ sum of any subset of up to 5 tool_floats
       (covers totals assembled from multiple line items).
    """
    if amount == 0.0:
        return True

    # 1. Direct match
    for tf in tool_floats:
        if abs(tf - amount) / max(abs(amount), 0.001) <= tol:
            return True

    # 2. Multiply by common integer
    for tf in tool_floats:
        for n in _ARITH_MULTIPLIERS:
            if abs(tf * n - amount) / max(abs(amount), 0.001) <= tol:
                return True

    # 3. Absolute difference of any pair
    for t1, t2 in itertools.combinations(tool_floats, 2):
        diff = abs(t1 - t2)
        if diff > 0 and abs(diff - amount) / max(abs(amount), 0.001) <= tol:
            return True

    # 4. Subset sum (up to 5 elements)
    for size in range(2, min(6, len(tool_floats) + 1)):
        for combo in itertools.combinations(tool_floats, size):
            s = sum(combo)
            if s > 0 and abs(s - amount) / max(abs(amount), 0.001) <= tol:
                return True

    # 5. Two-multiplier chain: tf × n1 × n2
    for tf in tool_floats:
        for n1 in _ARITH_MULTIPLIERS:
            for n2 in _ARITH_MULTIPLIERS:
                if abs(tf * n1 * n2 - amount) / max(abs(amount), 0.001) <= tol:
                    return True

    # 6. Tool value divided by (another tool value × multiplier)
    #    Covers: monthly_cost / (n_requests_millions × 1000) = cost_per_1000_requests
    for t1 in tool_floats:
        for t2 in tool_floats:
            if t1 == t2 or t2 == 0:
                continue
            for n in _ARITH_MULTIPLIERS:
                denom = t2 * n
                if denom > 0 and abs(t1 / denom - amount) / max(abs(amount), 0.001) <= tol:
                    return True

    # 7. Tool value divided by a single multiplier
    for tf in tool_floats:
        for n in _ARITH_MULTIPLIERS:
            if n > 0 and abs(tf / n - amount) / max(abs(amount), 0.001) <= tol:
                return True

    # 8. Linear combination of two tool values: tf1*n1 + tf2*n2 ≈ amount
    #    Covers multi-component pricing like Lambda (request cost + duration cost)
    #    or Fargate (vCPU cost + memory cost).
    for tf1 in tool_floats:
        for tf2 in tool_floats:
            if tf1 == tf2:
                continue
            for n1 in _ARITH_MULTIPLIERS:
                v1 = tf1 * n1
                if v1 > amount * 10:  # prune: partial sum already too large
                    break
                for n2 in _ARITH_MULTIPLIERS:
                    if abs(v1 + tf2 * n2 - amount) / max(abs(amount), 0.001) <= tol:
                        return True

    return False


def flag_hallucination(trace: dict) -> tuple[bool, str]:
    """
    Check if the final answer contains dollar amounts not grounded in tool results.

    Primary check (numeric grounding):
      Extract all $X.XX amounts from the answer and from tool results.
      Flag only if at least one answer amount cannot be explained as a direct
      match, simple arithmetic (multiply/difference/sum) of tool result numbers.

    Secondary check (hedging language, no dollar amounts):
      If the answer has no dollar amounts but uses estimation language that does
      not appear in tool results, flag with a note that it may be hedging around
      data the LLM retrieved from training rather than tools.
    """
    answer = trace.get("final_answer") or ""
    if not answer:
        return False, ""

    tool_results_text = json.dumps([tc["result"] for tc in trace["tool_calls"]])
    ans_amounts = _extract_dollar_amounts(answer)
    tool_floats = list(set(_extract_dollar_amounts(tool_results_text)))

    if ans_amounts:
        ungrounded = []
        seen: set[float] = set()
        for av in ans_amounts:
            if av not in seen and not _is_grounded(av, tool_floats):
                seen.add(av)
                ungrounded.append(f"${av:g}")
        if ungrounded:
            # If the provider wasn't configured (expected API_KEY failure), any dollar
            # amounts the LLM provides are inherently estimates — not hallucinations in
            # the tool-grounding sense. Suppress to avoid double-penalising the expected
            # configuration gap on top of the API_KEY flag.
            api_key_fired, _ = flag_api_key(trace)
            if api_key_fired:
                return False, ""
            return True, (
                f"Ungrounded amount(s) in answer (not derivable from tool results): "
                f"{', '.join(ungrounded[:5])}"
            )
        return False, ""

    # No dollar amounts — fall back to hedging-language check
    # Skip if the answer already explicitly acknowledges unavailability
    # (e.g. GCP API key not configured) — hedge phrases in that context
    # are descriptive, not estimates from training data.
    answer_lower = answer.lower()
    tool_lower = tool_results_text.lower()
    _UNAVAIL_PHRASES = ["api key", "credentials", "cannot retrieve", "unable to retrieve",
                        "not configured", "requires authentication"]
    answer_acknowledges_unavail = any(p in answer_lower for p in _UNAVAIL_PHRASES)
    for phrase in ESTIMATE_PHRASES:
        if phrase in answer_lower and phrase not in tool_lower:
            if answer_acknowledges_unavail:
                continue  # don't flag — LLM is being descriptive, not estimating
            return True, (
                f"Hedging phrase '{phrase}' in answer (no dollar amounts to verify — "
                f"may be estimating from training data)"
            )
    return False, ""


def flag_truncation(trace: dict) -> tuple[bool, str]:
    """Check if results look suspiciously small for the question asked."""
    for tc in trace["tool_calls"]:
        result = tc["result"]
        if not isinstance(result, dict):
            continue
        tool = tc["tool"]
        # find_cheapest_region / find_available_regions returning very few results
        if tool in ("find_cheapest_region", "find_available_regions"):
            results = result.get("results") or result.get("regions") or []
            if isinstance(results, list) and 0 < len(results) < 4:
                return True, f"{tool} returned only {len(results)} region(s) — may be truncated"
        # list_instance_types hitting max_results
        if tool == "list_instance_types":
            truncated = result.get("truncated", False)
            if truncated:
                # If the answer has grounded dollar amounts, the truncation was benign —
                # the LLM still produced a correct, tool-backed answer despite the cap.
                # Only flag if the answer lacks grounding (may have been harmed by truncation).
                answer = trace.get("final_answer") or ""
                tool_results_text = json.dumps([tc2["result"] for tc2 in trace["tool_calls"]])
                ans_amounts = _extract_dollar_amounts(answer)
                tool_floats = list(set(_extract_dollar_amounts(tool_results_text)))
                if ans_amounts and all(_is_grounded(a, tool_floats) for a in ans_amounts):
                    continue  # Benign truncation — answer is grounded in tool results
                return True, "list_instance_types result was truncated (hit max_results)"
    return False, ""


def flag_unanchored(trace: dict) -> tuple[bool, str]:
    """Check if the final answer is missing key anchoring parameters."""
    answer = (trace.get("final_answer") or "").lower()
    if not answer or len(answer) < 50:
        return False, ""  # too short to judge

    if not trace["tool_calls"]:
        return False, ""

    # Condition 1: Only flag UNANCHORED if the prompt itself mentions a region.
    # Unit-economics questions (e.g. "cost per user") typically don't specify a
    # region, so the LLM has no obligation to echo one.
    prompt = trace.get("prompt", "").lower()
    prompt_has_region = any(term in prompt for term in [
        "us-", "eu-", "ap-", "eastus", "us-east", "us-west", "eu-west",
        "central", "europe", "asia", "region",
    ])
    if not prompt_has_region:
        return False, ""

    # Condition 2: Only flag if at least one tool result actually contains a
    # region identifier. If all tool calls returned errors (e.g. GCP API key not
    # configured) the LLM never received a region to echo.
    tool_results_text = json.dumps([tc["result"] for tc in trace["tool_calls"]])
    results_have_region = any(term in tool_results_text.lower() for term in [
        "us-east", "us-west", "eu-west", "ap-", "eastus", "us-central",
    ])
    if not results_have_region:
        return False, ""

    # Check that the answer mentions at least one region keyword.
    if "region" not in answer and "us-" not in answer \
            and "eu-" not in answer and "ap-" not in answer and "eastus" not in answer \
            and "central" not in answer and "europe" not in answer and "asia" not in answer:
        return True, "Answer mentions no region despite tool calls being made"
    return False, ""


def flag_missing_data(trace: dict) -> tuple[bool, str]:
    """Check if pricing that should have been fetched was skipped."""
    answer = (trace.get("final_answer") or "").lower()
    prompt = trace.get("prompt", "").lower()

    # If no tool calls at all for a pricing question
    if not trace["tool_calls"] and trace.get("final_answer"):
        for phrase in ESTIMATE_PHRASES:
            if phrase in answer:
                return True, "No tool calls made — answer appears estimated from memory"

    # Collect tools that returned successful pricing data (used to suppress
    # estimate_bom/estimate_unit_economics fallback failures).
    successful_pricing_tools = {
        tc2["tool"] for tc2 in trace["tool_calls"]
        if isinstance(tc2["result"], dict)
        and "error" not in tc2["result"]
        and ("prices" in tc2["result"] or "line_items" in tc2["result"]
             or "infrastructure_monthly" in tc2["result"])
    }

    # Check for tool errors on public APIs (not GCP/credentials)
    for tc in trace["tool_calls"]:
        result = tc["result"]
        if isinstance(result, dict) and "error" in result:
            err = (result["error"] or "").lower()
            # Expected: GCP auth errors, spot-price auth, cost-explorer auth,
            # and any error explicitly telling the LLM not to estimate
            if "gcp" not in err and "api key" not in err and "credentials" not in err \
                    and "spot" not in err and "cost explorer" not in err \
                    and "do not estimate" not in err \
                    and "not supported for azure" not in err \
                    and "not supported for" not in err \
                    and "does not support" not in err \
                    and "not yet implemented" not in err:
                # If estimate_bom/estimate_unit_economics failed but the LLM already
                # retrieved pricing via get_service_price / get_compute_price etc.,
                # the failed aggregator call is a dead-end attempt, not missing data.
                if tc["tool"] in ("estimate_bom", "estimate_unit_economics") \
                        and successful_pricing_tools:
                    continue
                return True, f"Tool {tc['tool']} returned unexpected error: {result['error'][:100]}"

    return False, ""


def flag_no_answer(trace: dict) -> tuple[bool, str]:
    answer = trace.get("final_answer") or ""
    if not answer.strip() or answer.strip().startswith("[thinking only"):
        return True, "Empty or thinking-only final answer"
    if trace.get("error"):
        return True, f"Run error: {trace['error']}"
    return False, ""


def flag_api_key(trace: dict) -> tuple[bool, str]:
    """Flag GCP/credential failures or AWS credentials-required errors as expected."""
    for tc in trace["tool_calls"]:
        result = tc["result"]
        if isinstance(result, dict):
            # Check both structured "error" field and FastMCP text error format
            err = (result.get("error") or result.get("text") or "").lower()
            if "api key" in err or ("gcp" in err and "requires" in err):
                return True, "GCP API key not configured (expected in this environment)"
            # AWS spot/Cost Explorer pricing requires live credentials — expected gap
            if ("spot pricing requires" in err or "aws credentials" in err
                    or "cost explorer" in err or "aws_access_key_id" in err):
                return True, "AWS credentials required for this pricing data (expected in this environment)"
    return False, ""


CHECKS = [
    ("NO_ANSWER",    flag_no_answer),
    ("HALLUCINATION", flag_hallucination),
    ("TRUNCATION",   flag_truncation),
    ("UNANCHORED",   flag_unanchored),
    ("MISSING_DATA", flag_missing_data),
    ("API_KEY",      flag_api_key),
]


def analyse_run(run_dir: Path) -> dict:
    traces = load_traces(run_dir)
    report = {}

    for pid, trace in traces.items():
        flags = []
        for label, fn in CHECKS:
            hit, reason = fn(trace)
            if hit:
                flags.append({"flag": label, "reason": reason})

        report[pid] = {
            "prompt": trace.get("prompt", "")[:100],
            "rounds": trace.get("rounds", 0),
            "tool_calls": len(trace.get("tool_calls", [])),
            "tools_used": list({tc["tool"] for tc in trace.get("tool_calls", [])}),
            "flags": flags,
            "pass": not any(f["flag"] not in ("API_KEY",) for f in flags),
            "answer_preview": (trace.get("final_answer") or "")[:250],
        }

    return report


def print_report(report: dict, run_dir: Path):
    print(f"\n{'='*72}")
    print(f"ANALYSIS — {run_dir.name}")
    print(f"{'='*72}")

    # Group by category prefix
    categories = {}
    for pid, r in report.items():
        prefix = re.match(r"^[A-Z]+", pid).group()
        categories.setdefault(prefix, []).append((pid, r))

    passed = sum(1 for r in report.values() if r["pass"])
    total = len(report)

    for prefix, items in categories.items():
        label = {
            "C": "Original Common",
            "M": "Original Multi-cloud",
            "X": "Original Complex",
            "AS": "AWS Simple",
            "GS": "GCP Simple",
            "MP": "Multi-product AWS vs GCP",
            "MR": "Multi-region",
            "CX": "Complex BoM/TCO",
            "AZ": "Azure Simple",
            "AA": "Advanced AWS",
            "MC": "Multi-Cloud 3-way",
            "GK": "GCP GKE",
            "GM": "GCP Memorystore",
            "GB": "GCP BigQuery",
            "GV": "GCP Vertex AI + Gemini",
            "GN": "GCP Networking",
            "GC": "GCP Armor + Monitoring",
            "GCX": "GCP Complex Stacks",
            "GGCS": "GCP Cloud Storage",
            "GSQL": "GCP Cloud SQL",
        }.get(prefix, prefix)
        print(f"\n── {label} ──")
        for pid, r in sorted(items):
            status = "✓" if r["pass"] else "✗"
            flag_str = ", ".join(f["flag"] for f in r["flags"]) if r["flags"] else "—"
            print(f"  {status} [{pid:4s}] {r['rounds']}r {r['tool_calls']}tc  flags={flag_str}")
            if not r["pass"]:
                for f in r["flags"]:
                    if f["flag"] != "API_KEY":
                        print(f"         → {f['reason']}")

    print(f"\n{'─'*72}")
    print(f"SCORE: {passed}/{total} passed  ({100*passed//total}%)")

    # Failure breakdown
    from collections import Counter
    all_flags = Counter(
        f["flag"] for r in report.values() for f in r["flags"]
        if f["flag"] != "API_KEY"
    )
    if all_flags:
        print("\nFailure breakdown (excl. expected API_KEY):")
        for flag, count in all_flags.most_common():
            print(f"  {flag:15s} {count}")


def build_improvement_plan(report: dict, run_dir: Path):
    """Write a structured improvement plan based on observed failures."""
    failures = {pid: r for pid, r in report.items() if not r["pass"]}
    if not failures:
        plan = "# Improvement Plan\n\nAll tests passed. No improvements needed from this run.\n"
    else:
        from collections import defaultdict
        by_flag = defaultdict(list)
        for pid, r in failures.items():
            for f in r["flags"]:
                if f["flag"] != "API_KEY":
                    by_flag[f["flag"]].append((pid, f["reason"], r["prompt"]))

        lines = ["# Improvement Plan\n", f"Generated from run: {run_dir.name}\n"]
        priority = ["NO_ANSWER", "HALLUCINATION", "MISSING_DATA", "TRUNCATION", "UNANCHORED"]
        for flag in priority:
            if flag not in by_flag:
                continue
            lines.append(f"\n## {flag} ({len(by_flag[flag])} failures)\n")
            for pid, reason, prompt in by_flag[flag]:
                lines.append(f"- **[{pid}]** {prompt[:80]}")
                lines.append(f"  - Reason: {reason}")
            lines.append("")

        lines.append("\n## Recommended Actions\n")
        if "NO_ANSWER" in by_flag:
            lines.append("- **NO_ANSWER**: Check if `enable_thinking=false` is being applied correctly. "
                         "Inspect traces for `finish_reason=length` — may need higher `max_tokens` or "
                         "simpler tool sequences.\n")
        if "HALLUCINATION" in by_flag:
            lines.append("- **HALLUCINATION**: Add stronger 'see_also' / 'next_steps' hints to tool "
                         "responses so the LLM fetches data rather than filling gaps from training.\n")
        if "MISSING_DATA" in by_flag:
            lines.append("- **MISSING_DATA**: Review which tools the LLM failed to call. Consider adding "
                         "cross-reference hints in docstrings pointing to the right tool for each scenario.\n")
        if "TRUNCATION" in by_flag:
            lines.append("- **TRUNCATION**: Increase default max_results where appropriate, or add "
                         "'truncated' flags with next_steps hints to drive follow-up calls.\n")
        if "UNANCHORED" in by_flag:
            lines.append("- **UNANCHORED**: Enforce that tool responses always echo back key parameters "
                         "(region, OS, term, instance type) so the LLM anchors its answer to them.\n")

    plan_file = run_dir / "improvement_plan.md"
    plan_file.write_text("".join(lines) if isinstance(lines, list) else plan)
    print(f"\nImprovement plan written to: {plan_file}")
    print("\n" + ("".join(lines) if isinstance(lines, list) else plan))


def main():
    if len(sys.argv) > 1:
        run_dir = Path(sys.argv[1])
    else:
        dirs = sorted(RESULTS_ROOT.iterdir())
        if not dirs:
            print("No results found.")
            sys.exit(1)
        run_dir = dirs[-1]

    print(f"Analysing: {run_dir}")
    report = analyse_run(run_dir)

    # Save report JSON
    report_file = run_dir / "analysis.json"
    report_file.write_text(json.dumps(report, indent=2))

    print_report(report, run_dir)
    build_improvement_plan(report, run_dir)


if __name__ == "__main__":
    main()
