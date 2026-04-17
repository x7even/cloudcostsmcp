"""Tests for GCP GKE (Google Kubernetes Engine) pricing."""
from __future__ import annotations

from decimal import Decimal
from pathlib import Path
from unittest.mock import AsyncMock, patch

import pytest

from opencloudcosts.cache import CacheManager
from opencloudcosts.config import Settings
from opencloudcosts.providers.gcp import GCPProvider
from opencloudcosts.utils.units import gcp_money_to_decimal


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _make_gke_sku(
    description: str,
    regions: list[str],
    price_units: str = "0",
    price_nanos: int = 0,
    usage_type: str = "OnDemand",
) -> dict:
    return {
        "description": description,
        "serviceRegions": regions,
        "category": {"usageType": usage_type},
        "pricingInfo": [{"pricingExpression": {"tieredRates": [
            {"startUsageAmount": 0, "unitPrice": {"units": price_units, "nanos": price_nanos}}
        ]}}],
    }


@pytest.fixture
async def provider(tmp_path: Path):
    settings = Settings(
        cache_dir=tmp_path / "cache",
        cache_ttl_hours=1,
        gcp_api_key="AIzaFakeKey",
    )
    cache = CacheManager(settings.cache_dir)
    await cache.initialize()
    pvdr = GCPProvider(settings, cache)
    yield pvdr
    await cache.close()
    await pvdr.close()


# ---------------------------------------------------------------------------
# Standard mode tests
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_gke_standard_management_fee(provider):
    """Standard mode: SKU with 'Kubernetes Engine' in desc should be used as management fee."""
    fake_skus = [
        _make_gke_sku(
            "Kubernetes Engine Cluster Management Fee",
            ["us-central1"],
            price_units="0", price_nanos=100_000_000,  # $0.10
        ),
    ]
    with patch.object(provider, "_fetch_skus", new=AsyncMock(return_value=fake_skus)):
        result = await provider.get_gke_price(region="us-central1", mode="standard")

    assert result["mode"] == "standard"
    assert result["cluster_management_fee_per_hr"] == "$0.100000"
    assert "cluster_management_monthly" in result
    assert result["region"] == "us-central1"


@pytest.mark.asyncio
async def test_gke_standard_fallback_rate(provider):
    """Standard mode: when no matching SKU found, fallback to $0.10/hr."""
    # Only Autopilot SKUs — no management fee SKU
    fake_skus = [
        _make_gke_sku(
            "Autopilot Balanced Pod mCPU Requests (us-central1)",
            ["us-central1"],
            price_units="0", price_nanos=64_000,
        ),
    ]
    with patch.object(provider, "_fetch_skus", new=AsyncMock(return_value=fake_skus)):
        result = await provider.get_gke_price(region="us-central1", mode="standard")

    assert result["mode"] == "standard"
    assert result["cluster_management_fee_per_hr"] == "$0.100000"
    assert result["cluster_management_monthly"] == "$73.00"  # 0.10 * 730


@pytest.mark.asyncio
async def test_gke_standard_includes_node_hint(provider):
    """Standard mode: result must reference node_type and node_count in the hint."""
    with patch.object(provider, "_fetch_skus", new=AsyncMock(return_value=[])):
        result = await provider.get_gke_price(
            region="us-east1",
            mode="standard",
            node_count=5,
            node_type="n2-standard-8",
        )

    assert "n2-standard-8" in result["node_pricing_hint"]
    assert "5" in result["node_pricing_hint"]
    assert "5" in result["total_monthly_note"]


@pytest.mark.asyncio
async def test_gke_standard_excludes_autopilot_sku(provider):
    """Standard mode: Autopilot SKUs must NOT be selected as management fee."""
    fake_skus = [
        # Autopilot SKU — should be ignored by standard mode search
        _make_gke_sku(
            "Kubernetes Engine Autopilot Balanced Pod mCPU Requests",
            ["us-central1"],
            price_units="0", price_nanos=64_000,
        ),
    ]
    with patch.object(provider, "_fetch_skus", new=AsyncMock(return_value=fake_skus)):
        result = await provider.get_gke_price(region="us-central1", mode="standard")

    # Should fall back to $0.10 since autopilot SKU must be excluded
    assert result["cluster_management_fee_per_hr"] == "$0.100000"


# ---------------------------------------------------------------------------
# Autopilot mode tests
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_gke_autopilot_rates(provider):
    """Autopilot mode: correct rate extraction from SKUs."""
    # mCPU rate: 64_000 nanos = $0.000064/mCPU-hr
    # mem rate: 9_982_000 nanos = $0.009982/GiB-hr
    fake_skus = [
        _make_gke_sku(
            "Autopilot Balanced Pod mCPU Requests (us-east1)",
            ["us-east1"],
            price_units="0", price_nanos=64_000,
        ),
        _make_gke_sku(
            "Autopilot Balanced Pod Memory Requests (us-east1)",
            ["us-east1"],
            price_units="0", price_nanos=9_982_000,
        ),
    ]
    with patch.object(provider, "_fetch_skus", new=AsyncMock(return_value=fake_skus)):
        result = await provider.get_gke_price(
            region="us-east1", mode="autopilot", vcpu=0.0, memory_gb=0.0
        )

    assert result["mode"] == "autopilot"
    # vcpu_rate = mCPU_rate * 1000 = 0.000064 * 1000 = 0.064
    assert result["vcpu_rate_per_hr"] == "$0.064000"
    assert result["memory_rate_per_gib_hr"] == "$0.009982"
    assert result["region"] == "us-east1"


@pytest.mark.asyncio
async def test_gke_autopilot_mcpu_to_vcpu_conversion(provider):
    """Autopilot mode: mCPU rate * 1000 must equal the vCPU rate returned."""
    # 64 nanos per mCPU = $0.000000064/mCPU-hr -> *1000 = $0.000064/vCPU-hr
    fake_skus = [
        _make_gke_sku(
            "Autopilot Balanced Pod mCPU Requests (us-central1)",
            ["us-central1"],
            price_units="0", price_nanos=64,  # very small value to test math precisely
        ),
        _make_gke_sku(
            "Autopilot Balanced Pod Memory Requests (us-central1)",
            ["us-central1"],
            price_units="0", price_nanos=1_000_000,
        ),
    ]
    with patch.object(provider, "_fetch_skus", new=AsyncMock(return_value=fake_skus)):
        result = await provider.get_gke_price(
            region="us-central1", mode="autopilot"
        )

    # mCPU_rate = 64 nanos = 0.000000064
    # vcpu_rate = 0.000000064 * 1000 = 0.000064
    vcpu_rate_str = result["vcpu_rate_per_hr"]
    vcpu_rate = Decimal(vcpu_rate_str.lstrip("$"))
    mcpu_rate = vcpu_rate / Decimal("1000")

    # Verify the conversion: vcpu_rate should be exactly 1000x the mCPU rate
    expected_mcpu_rate = gcp_money_to_decimal("0", 64)
    assert mcpu_rate == expected_mcpu_rate


@pytest.mark.asyncio
async def test_gke_autopilot_cost_math(provider):
    """Autopilot mode: cost calculation with vcpu=4, memory_gb=16, 730 hours."""
    # mCPU rate: 64_000 nanos = $0.000064/mCPU-hr  -> vcpu_rate = $0.064/vCPU-hr
    # mem rate:  9_982_000 nanos = $0.009982/GiB-hr
    fake_skus = [
        _make_gke_sku(
            "Autopilot Balanced Pod mCPU Requests (us-central1)",
            ["us-central1"],
            price_units="0", price_nanos=64_000,
        ),
        _make_gke_sku(
            "Autopilot Balanced Pod Memory Requests (us-central1)",
            ["us-central1"],
            price_units="0", price_nanos=9_982_000,
        ),
    ]
    with patch.object(provider, "_fetch_skus", new=AsyncMock(return_value=fake_skus)):
        result = await provider.get_gke_price(
            region="us-central1",
            mode="autopilot",
            vcpu=4.0,
            memory_gb=16.0,
            hours_per_month=730.0,
        )

    assert result["mode"] == "autopilot"
    assert result["requested_vcpu"] == 4.0
    assert result["requested_memory_gb"] == 16.0

    # vcpu_rate = 0.000064 * 1000 = 0.064
    # mem_rate = 0.009982
    # hourly = 4 * 0.064 + 16 * 0.009982 = 0.256 + 0.159712 = 0.415712
    # monthly = 0.415712 * 730 ≈ 303.47
    vcpu_rate = Decimal("0.064000")
    mem_rate = Decimal("0.009982")
    hourly_expected = Decimal("4") * vcpu_rate + Decimal("16") * mem_rate
    monthly_expected = hourly_expected * Decimal("730")

    hourly_actual = Decimal(result["hourly_cost"].lstrip("$"))
    monthly_actual = Decimal(result["monthly_cost"].lstrip("$"))

    assert abs(hourly_actual - hourly_expected) < Decimal("0.000001")
    assert abs(monthly_actual - monthly_expected) < Decimal("0.01")


@pytest.mark.asyncio
async def test_gke_autopilot_zero_vcpu_memory_no_cost(provider):
    """Autopilot mode: when vcpu=0 and memory_gb=0, cost is $0.00 but rates are returned."""
    fake_skus = [
        _make_gke_sku(
            "Autopilot Balanced Pod mCPU Requests (us-central1)",
            ["us-central1"],
            price_units="0", price_nanos=64_000,
        ),
        _make_gke_sku(
            "Autopilot Balanced Pod Memory Requests (us-central1)",
            ["us-central1"],
            price_units="0", price_nanos=9_982_000,
        ),
    ]
    with patch.object(provider, "_fetch_skus", new=AsyncMock(return_value=fake_skus)):
        result = await provider.get_gke_price(
            region="us-central1", mode="autopilot", vcpu=0.0, memory_gb=0.0
        )

    assert result["hourly_cost"] == "$0.000000"
    assert result["monthly_cost"] == "$0.00"
    # Rates should still be populated
    assert result["vcpu_rate_per_hr"] != "$0.000000"


@pytest.mark.asyncio
async def test_gke_invalid_mode(provider):
    """Invalid mode falls through to autopilot path — provider does not validate mode."""
    with patch.object(provider, "_fetch_skus", new=AsyncMock(return_value=[])):
        result = await provider.get_gke_price(
            region="us-central1", mode="unknown_mode"
        )
    # Falls through to autopilot else-branch — returns a dict with autopilot-like keys
    # (the tool layer validates mode before calling the provider)
    assert isinstance(result, dict)
    assert "mode" in result
