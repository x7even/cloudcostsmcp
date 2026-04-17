"""Tests for GCP Memorystore for Redis pricing."""
from __future__ import annotations

from decimal import Decimal
from pathlib import Path
from unittest.mock import AsyncMock, patch

import pytest

from opencloudcosts.cache import CacheManager
from opencloudcosts.config import Settings
from opencloudcosts.models import PriceUnit, PricingTerm
from opencloudcosts.providers.gcp import GCPProvider


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _make_redis_sku(description: str, regions: list[str], price_nanos: int) -> dict:
    return {
        "description": description,
        "serviceRegions": regions,
        "category": {"usageType": "OnDemand"},
        "pricingInfo": [{"pricingExpression": {"tieredRates": [
            {"startUsageAmount": 0, "unitPrice": {"units": "0", "nanos": price_nanos}}
        ]}}],
    }


# Realistic Memorystore SKUs for us-central1 / europe-west1
MEMORYSTORE_FAKE_SKUS = [
    # Basic tier, various M-tiers in us-central1 (Iowa)
    _make_redis_sku("Redis Capacity Basic M2 Iowa", ["us-central1"], 16_000_000),   # $0.016/GiB-hr
    _make_redis_sku("Redis Capacity Basic M3 Iowa", ["us-central1"], 18_000_000),   # $0.018/GiB-hr
    _make_redis_sku("Redis Capacity Basic M4 Iowa", ["us-central1"], 20_000_000),   # $0.020/GiB-hr
    _make_redis_sku("Redis Capacity Basic M5 Iowa", ["us-central1"], 22_000_000),   # $0.022/GiB-hr
    # Standard (new naming), various M-tiers in us-central1
    _make_redis_sku("Redis Standard Node Capacity M2 Iowa", ["us-central1"], 24_000_000),  # $0.024/GiB-hr
    _make_redis_sku("Redis Standard Node Capacity M3 Iowa", ["us-central1"], 26_000_000),  # $0.026/GiB-hr
    _make_redis_sku("Redis Standard Node Capacity M4 Iowa", ["us-central1"], 28_000_000),  # $0.028/GiB-hr
    _make_redis_sku("Redis Standard Node Capacity M5 Iowa", ["us-central1"], 30_000_000),  # $0.030/GiB-hr
    # Standard (old naming alias)
    _make_redis_sku("Redis Capacity Standard M2 Iowa", ["us-central1"], 24_000_000),
    # Only M4 available in europe-west1 (for fallback test)
    _make_redis_sku("Redis Capacity Basic M4 Netherlands", ["europe-west1"], 21_000_000),  # $0.021/GiB-hr
    _make_redis_sku("Redis Standard Node Capacity M4 Netherlands", ["europe-west1"], 31_000_000),
]


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
# Provider tests
# ---------------------------------------------------------------------------

async def test_memorystore_basic_price(gcp_provider):
    """Basic tier returns correct price with service='database'."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=MEMORYSTORE_FAKE_SKUS)):
        prices = await gcp_provider.get_memorystore_price(5.0, "us-central1", tier="basic")

    assert len(prices) == 1
    p = prices[0]
    assert p.service == "database"
    assert p.product_family == "Memorystore for Redis"
    assert p.unit == PriceUnit.PER_HOUR
    assert p.pricing_term == PricingTerm.ON_DEMAND
    assert p.region == "us-central1"
    assert "basic" in p.description.lower()
    assert p.attributes["tier"] == "basic"
    assert p.attributes["capacity_gb"] == "5.0"


async def test_memorystore_standard_price(gcp_provider):
    """Standard tier returns correct price."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=MEMORYSTORE_FAKE_SKUS)):
        prices = await gcp_provider.get_memorystore_price(5.0, "us-central1", tier="standard")

    assert len(prices) == 1
    p = prices[0]
    assert p.service == "database"
    assert p.product_family == "Memorystore for Redis"
    assert p.attributes["tier"] == "standard"


async def test_memorystore_standard_more_expensive_than_basic(gcp_provider):
    """Standard tier is more expensive than Basic for same capacity and region."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=MEMORYSTORE_FAKE_SKUS)):
        basic = await gcp_provider.get_memorystore_price(5.0, "us-central1", tier="basic")
        standard = await gcp_provider.get_memorystore_price(5.0, "us-central1", tier="standard")

    assert len(basic) == 1
    assert len(standard) == 1
    assert standard[0].price_per_unit > basic[0].price_per_unit


async def test_memorystore_monthly_cost_math(gcp_provider):
    """hourly_rate = capacity × rate_per_gib_hr; monthly = hourly × 730."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=MEMORYSTORE_FAKE_SKUS)):
        # 5 GB in us-central1 basic — M3 tier: $0.018/GiB-hr
        prices = await gcp_provider.get_memorystore_price(5.0, "us-central1", tier="basic")

    assert len(prices) == 1
    p = prices[0]
    raw_rate = Decimal(p.attributes["rate_per_gib_hr"])
    expected_hourly = raw_rate * Decimal("5.0")
    expected_monthly = expected_hourly * Decimal("730")
    assert p.price_per_unit == expected_hourly
    assert p.monthly_cost == expected_monthly


async def test_memorystore_mtier_fallback(gcp_provider):
    """When preferred M-tier not available, falls back to another available tier."""
    # europe-west1 only has M4 SKUs; capacity=5 (prefers M3) should fall back to M4
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=MEMORYSTORE_FAKE_SKUS)):
        prices = await gcp_provider.get_memorystore_price(5.0, "europe-west1", tier="basic")

    assert len(prices) == 1
    p = prices[0]
    # M4 rate for europe-west1 basic = $0.021/GiB-hr
    raw_rate = Decimal(p.attributes["rate_per_gib_hr"])
    assert raw_rate == Decimal("0.021")
    assert p.region == "europe-west1"


async def test_memorystore_zero_capacity_raises(gcp_provider):
    """capacity_gb=0 raises ValueError."""
    with pytest.raises(ValueError, match="capacity_gb must be positive"):
        await gcp_provider.get_memorystore_price(0.0, "us-central1", tier="basic")


async def test_memorystore_unknown_tier(gcp_provider):
    """Raises ValueError for an unsupported tier."""
    with pytest.raises(ValueError, match="Unknown Memorystore tier"):
        await gcp_provider.get_memorystore_price(10.0, "us-central1", tier="premium")


async def test_memorystore_alternate_standard_sku(gcp_provider):
    """Matches 'Redis Capacity Standard Mn' pattern (older naming) for standard tier."""
    # Only provide the old-style SKU, not the new 'Redis Standard Node Capacity' one
    old_style_skus = [
        _make_redis_sku("Redis Capacity Standard M3 Iowa", ["us-central1"], 27_000_000),
    ]
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=old_style_skus)):
        prices = await gcp_provider.get_memorystore_price(5.0, "us-central1", tier="standard")

    assert len(prices) == 1
    p = prices[0]
    assert p.attributes["tier"] == "standard"
    raw_rate = Decimal(p.attributes["rate_per_gib_hr"])
    assert raw_rate == Decimal("0.027")
