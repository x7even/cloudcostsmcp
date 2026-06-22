"""Tests for opencloudcosts.utils.egress_tiers."""

from __future__ import annotations

from decimal import Decimal

import pytest

from opencloudcosts.utils.egress_tiers import EgressTier, compute_tiered_cost

# ---------------------------------------------------------------------------
# EgressTier dataclass
# ---------------------------------------------------------------------------


def test_egress_tier_frozen():
    tier = EgressTier(threshold_gb=0, rate=Decimal("0.09"), label="test")
    with pytest.raises(Exception):
        tier.rate = Decimal("0.10")  # type: ignore[misc]


# ---------------------------------------------------------------------------
# compute_tiered_cost — basic cases
# ---------------------------------------------------------------------------


def test_compute_tiered_cost_zero_data_gb():
    tiers = [
        EgressTier(threshold_gb=0, rate=Decimal("0.09"), label="all"),
    ]
    result = compute_tiered_cost(tiers, 0.0)
    assert result["total_cost"] == "0.0000"
    assert result["blended_rate_per_gb"] == "0.0000"
    assert result["data_gb"] == 0.0
    assert result["tiers"] == []


def test_compute_tiered_cost_single_tier():
    """A single tier with no boundary uses all remaining GB."""
    tiers = [
        EgressTier(threshold_gb=0, rate=Decimal("0.09"), label="all"),
    ]
    result = compute_tiered_cost(tiers, 100.0)
    assert result["total_cost"] == "9.0000"
    assert result["blended_rate_per_gb"] == "0.0900"
    assert len(result["tiers"]) == 1
    assert result["tiers"][0]["gb"] == pytest.approx(100.0)
    assert result["tiers"][0]["rate"] == "0.09"


def test_compute_tiered_cost_free_first_tier():
    """Free first tier (rate=0) then paid tier: only paid portion costs."""
    tiers = [
        EgressTier(threshold_gb=0, rate=Decimal("0.000"), label="0-100 GB (free)"),
        EgressTier(threshold_gb=100, rate=Decimal("0.090"), label="100 GB+"),
    ]
    # 150 GB: 100 free + 50 paid at $0.09
    result = compute_tiered_cost(tiers, 150.0)
    assert result["total_cost"] == "4.5000"  # 50 * 0.09
    assert len(result["tiers"]) == 2
    assert result["tiers"][0]["gb"] == pytest.approx(100.0)
    assert result["tiers"][0]["cost"] == "0.0000"
    assert result["tiers"][1]["gb"] == pytest.approx(50.0)
    assert result["tiers"][1]["cost"] == "4.5000"


def test_compute_tiered_cost_all_in_free_tier():
    """When data is entirely within the free tier, total cost is $0."""
    tiers = [
        EgressTier(threshold_gb=0, rate=Decimal("0.000"), label="0-100 GB (free)"),
        EgressTier(threshold_gb=100, rate=Decimal("0.090"), label="100 GB+"),
    ]
    result = compute_tiered_cost(tiers, 50.0)
    assert result["total_cost"] == "0.0000"
    assert result["blended_rate_per_gb"] == "0.0000"
    assert len(result["tiers"]) == 1
    assert result["tiers"][0]["gb"] == pytest.approx(50.0)


def test_compute_tiered_cost_three_tiers():
    """Validates correct split across three tiers."""
    tiers = [
        EgressTier(threshold_gb=0, rate=Decimal("0.000"), label="0-100 GB (free)"),
        EgressTier(threshold_gb=100, rate=Decimal("0.090"), label="100 GB-10 TB"),
        EgressTier(threshold_gb=10_335, rate=Decimal("0.085"), label="10-50 TB"),
    ]
    # Price exactly 10335 GB: 100 free + 10235 at $0.09
    data_gb = 10_335.0
    result = compute_tiered_cost(tiers, data_gb)
    expected_cost = Decimal("0") + Decimal("10235") * Decimal("0.090")
    assert float(Decimal(result["total_cost"])) == pytest.approx(float(expected_cost), rel=1e-6)


def test_compute_tiered_cost_crosses_tier_boundary():
    """Crossing a boundary correctly splits the volume."""
    tiers = [
        EgressTier(threshold_gb=0, rate=Decimal("0.000"), label="0-100 GB (free)"),
        EgressTier(threshold_gb=100, rate=Decimal("0.090"), label="100 GB-1 TB"),
        EgressTier(threshold_gb=1_024, rate=Decimal("0.065"), label="1-10 TB"),
    ]
    # 2048 GB: 100 free + 924 at $0.09 + 1024 at $0.065
    result = compute_tiered_cost(tiers, 2048.0)
    expected = Decimal("924") * Decimal("0.090") + Decimal("1024") * Decimal("0.065")
    assert float(Decimal(result["total_cost"])) == pytest.approx(float(expected), rel=1e-6)
    assert len(result["tiers"]) == 3


def test_compute_tiered_cost_blended_rate():
    """Blended rate = total_cost / data_gb."""
    tiers = [
        EgressTier(threshold_gb=0, rate=Decimal("0.000"), label="0-100 GB (free)"),
        EgressTier(threshold_gb=100, rate=Decimal("0.090"), label="100 GB+"),
    ]
    data_gb = 200.0
    result = compute_tiered_cost(tiers, data_gb)
    # 100 free + 100 at $0.09 = $9.00
    # blended = 9.00 / 200 = 0.045
    assert result["total_cost"] == "9.0000"
    assert result["blended_rate_per_gb"] == "0.0450"


def test_compute_tiered_cost_large_volume():
    """AWS-style tiers: 5 TB = 5120 GB crosses from 100 into the 10 TB tier."""
    from opencloudcosts.providers.aws import _AWS_INTERNET_EGRESS_TIERS

    result = compute_tiered_cost(_AWS_INTERNET_EGRESS_TIERS, 5120.0)
    # 100 free + 5020 at $0.09
    expected_cost = Decimal("5020") * Decimal("0.090")
    assert float(Decimal(result["total_cost"])) == pytest.approx(float(expected_cost), rel=1e-4)
    # blended = total / 5120
    blended = float(expected_cost) / 5120.0
    assert float(result["blended_rate_per_gb"]) == pytest.approx(blended, rel=1e-3)
