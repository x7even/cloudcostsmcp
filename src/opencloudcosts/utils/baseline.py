"""Baseline comparison utility for pricing tool responses."""
from __future__ import annotations

from decimal import Decimal
from typing import Any


def apply_baseline_deltas(
    results: list[dict[str, Any]],
    baseline_region: str,
    hourly_key: str = "price_per_hour",
    monthly_key: str = "monthly_estimate",
) -> list[dict[str, Any]]:
    """
    Mutate result dicts in-place to add delta fields vs a baseline region.

    Each dict gets three new keys:
      - delta_per_hour:  e.g. "+$0.046800" or "-$0.059500"
      - delta_monthly:   e.g. "+$34.16/mo"
      - delta_pct:       e.g. "+30.6%" or "-38.9%"

    The baseline region entry shows $0.00 / 0.0%.

    Raises ValueError if baseline_region is not present in results.
    """
    baseline = next((r for r in results if r.get("region") == baseline_region), None)
    if baseline is None:
        available = [r.get("region") for r in results]
        raise ValueError(
            f"Baseline region '{baseline_region}' not found in results. "
            f"Available: {available}"
        )

    def parse_hourly(s: str) -> Decimal:
        # Handle both "$0.544000" and "$0.544000/per_hour" formats
        return Decimal(s.lstrip("$").split("/")[0])

    def parse_monthly(s: str) -> Decimal:
        return Decimal(s.lstrip("$").replace("/mo", ""))

    base_h = parse_hourly(baseline[hourly_key])
    base_m = parse_monthly(baseline.get(monthly_key, "$0.00/mo"))

    for r in results:
        h = parse_hourly(r[hourly_key])
        m = parse_monthly(r.get(monthly_key, "$0.00/mo"))
        dh = h - base_h
        dm = m - base_m
        pct = float((dh / base_h) * 100) if base_h > 0 else 0.0
        r["delta_per_hour"] = f"${dh:+.6f}"
        r["delta_monthly"] = f"${dm:+.2f}/mo"
        r["delta_pct"] = f"{pct:+.1f}%"

    return results
