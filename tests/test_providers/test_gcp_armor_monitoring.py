"""Tests for GCP Cloud Armor and Cloud Monitoring pricing provider methods."""
from __future__ import annotations

from decimal import Decimal
from pathlib import Path
from unittest.mock import AsyncMock, patch

import pytest

from opencloudcosts.cache import CacheManager
from opencloudcosts.config import Settings
from opencloudcosts.providers.gcp import GCPProvider


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _make_sku(description: str, units: str, nanos: int) -> dict:
    """Create a minimal SKU dict with a single flat tier."""
    return {
        "description": description,
        "serviceRegions": ["global"],
        "category": {"usageType": "OnDemand"},
        "pricingInfo": [
            {
                "pricingExpression": {
                    "tieredRates": [
                        {
                            "startUsageAmount": 0,
                            "unitPrice": {"units": units, "nanos": nanos},
                        }
                    ]
                }
            }
        ],
    }


# Cloud Armor SKUs
_ARMOR_POLICY_SKU = _make_sku(
    "Cloud Armor Security Policy",
    "0",
    750_000_000,  # $0.75/policy/month
)

_ARMOR_REQUEST_SKU = _make_sku(
    "Cloud Armor Request Evaluation",
    "0",
    750_000_000,  # $0.75/million requests
)

# Cloud Monitoring SKU
_MONITORING_INGESTION_SKU = _make_sku(
    "Cloud Monitoring Metric Ingestion",
    "0",
    258_000_000,  # $0.258/MiB
)


# ---------------------------------------------------------------------------
# Fixture
# ---------------------------------------------------------------------------


@pytest.fixture
async def gcp_provider(tmp_path: Path):
    settings = Settings(
        cache_dir=tmp_path / "cache",
        cache_ttl_hours=1,
        gcp_api_key="fake-api-key",
    )
    cache = CacheManager(settings.cache_dir)
    await cache.initialize()
    provider = GCPProvider(settings, cache)
    yield provider
    await cache.close()
    await provider.close()


# ---------------------------------------------------------------------------
# Cloud Armor tests
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_armor_policy_rate_from_sku(gcp_provider):
    """Policy SKU matching 'Cloud Armor' + 'Policy' is used for policy rate."""
    skus = [_ARMOR_POLICY_SKU, _ARMOR_REQUEST_SKU]
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=skus)):
        result = await gcp_provider.get_cloud_armor_price(policy_count=0)

    assert "policy_rate_per_month" in result
    assert "$0.75" in result["policy_rate_per_month"]


@pytest.mark.asyncio
async def test_armor_request_rate_from_sku(gcp_provider):
    """Request SKU matching 'Cloud Armor' + 'Request' is used for request rate."""
    skus = [_ARMOR_POLICY_SKU, _ARMOR_REQUEST_SKU]
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=skus)):
        result = await gcp_provider.get_cloud_armor_price()

    assert "request_rate_per_million" in result
    assert "$0.75" in result["request_rate_per_million"]


@pytest.mark.asyncio
async def test_armor_cost_math(gcp_provider):
    """policy_count=3, monthly_requests_millions=50.0 → correct arithmetic."""
    skus = [_ARMOR_POLICY_SKU, _ARMOR_REQUEST_SKU]
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=skus)):
        result = await gcp_provider.get_cloud_armor_price(
            policy_count=3,
            monthly_requests_millions=50.0,
        )

    # 3 policies × $0.75 = $2.25
    assert result["estimated_policy_cost"] == "$2.25"
    # 50M requests × $0.75/M = $37.50
    assert result["estimated_request_cost"] == "$37.50"
    # total = $2.25 + $37.50 = $39.75
    assert result["estimated_total_monthly"] == "$39.75"


@pytest.mark.asyncio
async def test_armor_fallback_on_empty_skus(gcp_provider):
    """When _fetch_skus raises an exception, result uses fallback rates and fallback=True."""
    with patch.object(
        gcp_provider,
        "_fetch_skus",
        new=AsyncMock(side_effect=Exception("service not found")),
    ):
        result = await gcp_provider.get_cloud_armor_price(
            policy_count=1,
            monthly_requests_millions=0.0,
        )

    assert result.get("fallback") is True
    assert "$0.75" in result["policy_rate_per_month"]
    assert "$0.75" in result["request_rate_per_million"]


# ---------------------------------------------------------------------------
# Cloud Monitoring tests
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_monitoring_tiered_cost_math(gcp_provider):
    """ingestion_mib=200.0 → 150 free, 50 × $0.258 = $12.90."""
    skus = [_MONITORING_INGESTION_SKU]
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=skus)):
        result = await gcp_provider.get_cloud_monitoring_price(ingestion_mib=200.0)

    assert "estimated_monthly_cost" in result
    # 200 - 150 free = 50 MiB billable × $0.258 = $12.90
    expected = Decimal("50") * Decimal("0.258")
    assert result["estimated_monthly_cost"] == f"${expected:.2f}"


@pytest.mark.asyncio
async def test_monitoring_tiered_cost_above_100k_mib(gcp_provider):
    """ingestion_mib=150000.0 → 150 free, first 100,000 @ $0.258, rest @ $0.151."""
    # Use fallback rates (no catalog hit needed)
    with patch.object(
        gcp_provider,
        "_fetch_skus",
        new=AsyncMock(side_effect=Exception("unavailable")),
    ):
        result = await gcp_provider.get_cloud_monitoring_price(ingestion_mib=150000.0)

    assert "estimated_monthly_cost" in result
    # Billable: 150000 - 150 = 149850 MiB
    # Tier 1: min(149850, 100000) = 100000 × $0.258 = $25800.00
    # Tier 2: 149850 - 100000 = 49850 × $0.151 = $7527.35
    billable = Decimal("149850")
    tier1 = Decimal("100000") * Decimal("0.258")
    tier2 = (billable - Decimal("100000")) * Decimal("0.151")
    expected = tier1 + tier2
    assert result["estimated_monthly_cost"] == f"${expected:.2f}"


@pytest.mark.asyncio
async def test_monitoring_fallback_on_empty_skus(gcp_provider):
    """When _fetch_skus raises an exception, result uses fallback rates and fallback=True."""
    with patch.object(
        gcp_provider,
        "_fetch_skus",
        new=AsyncMock(side_effect=Exception("service not found")),
    ):
        result = await gcp_provider.get_cloud_monitoring_price(ingestion_mib=0.0)

    assert result.get("fallback") is True
    assert "free_tier_mib" in result
    assert result["free_tier_mib"] == 150
    assert "0.258" in result["tier1_rate_per_mib"]
    assert "0.151" in result["tier2_rate_per_mib"]
    assert "0.062" in result["tier3_rate_per_mib"]
