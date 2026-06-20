"""Tier definitions and blended-cost calculator for inter-region egress pricing.

Used by all three providers (AWS, GCP, Azure) for the domain=network, service=egress
path that returns a tiered cost breakdown rather than a single flat rate.
"""

from __future__ import annotations

from dataclasses import dataclass
from decimal import Decimal


@dataclass(frozen=True)
class EgressTier:
    """A single price tier for egress data transfer."""

    threshold_gb: float  # lower bound of this tier (inclusive)
    rate: Decimal  # USD per GB for volume within this tier
    label: str  # human-readable label, e.g. "0-100 GB (free)"


def compute_tiered_cost(tiers: list[EgressTier], data_gb: float) -> dict:
    """Compute blended egress cost across ordered tiers.

    Args:
        tiers: list of EgressTier, ordered by threshold_gb ascending.
               The last tier absorbs all remaining volume beyond its threshold.
        data_gb: total monthly data volume to price. 0 returns rate-only result.

    Returns:
        {
          "total_cost": "87.0000",           # USD string, 4 decimal places
          "blended_rate_per_gb": "0.0870",   # total / data_gb
          "data_gb": 1000.0,
          "tiers": [
            {"label": "0-100 GB (free)", "gb": 100.0, "rate": "0.0000", "cost": "0.0000"},
            {"label": "100 GB-10 TB",    "gb": 900.0, "rate": "0.0900", "cost": "81.0000"},
          ],
        }
    """
    remaining = Decimal(str(max(0.0, data_gb)))
    total = Decimal("0")
    tier_splits: list[dict] = []

    for i, tier in enumerate(tiers):
        if remaining <= 0:
            break
        # Capacity of this tier = next tier's threshold - this tier's threshold
        if i + 1 < len(tiers):
            next_threshold = Decimal(str(tiers[i + 1].threshold_gb))
            current_threshold = Decimal(str(tier.threshold_gb))
            tier_capacity = next_threshold - current_threshold
        else:
            # Last tier: absorbs all remaining volume
            tier_capacity = remaining

        used = min(remaining, tier_capacity)
        cost = used * tier.rate
        total += cost
        tier_splits.append(
            {
                "label": tier.label,
                "gb": float(used),
                "rate": str(tier.rate),
                "cost": str(cost.quantize(Decimal("0.0001"))),
            }
        )
        remaining -= used

    blended = (
        (total / Decimal(str(data_gb))).quantize(Decimal("0.0001"))
        if data_gb > 0
        else Decimal("0").quantize(Decimal("0.0001"))
    )
    return {
        "total_cost": str(total.quantize(Decimal("0.0001"))),
        "blended_rate_per_gb": str(blended),
        "data_gb": data_gb,
        "tiers": tier_splits,
    }
