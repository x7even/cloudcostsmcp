"""Tests for GCP BigQuery pricing provider methods."""
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


def _make_sku(
    description: str,
    regions: list[str],
    tiers: list[dict],
) -> dict:
    """Create a minimal BigQuery SKU dict."""
    return {
        "description": description,
        "serviceRegions": regions,
        "category": {"usageType": "OnDemand"},
        "pricingInfo": [
            {
                "pricingExpression": {
                    "tieredRates": [
                        {
                            "startUsageAmount": t["start"],
                            "unitPrice": {
                                "units": str(t.get("units", "0")),
                                "nanos": t.get("nanos", 0),
                            },
                        }
                        for t in tiers
                    ]
                }
            }
        ],
    }


# BigQuery query analysis SKU: tier[0] = free ($0), tier[1] = $6.25/TiB
_ANALYSIS_SKU = _make_sku(
    "BigQuery Analysis",
    ["us"],
    [
        {"start": 0, "units": "0", "nanos": 0},
        {"start": 1024, "units": "6", "nanos": 250_000_000},  # $6.25/TiB
    ],
)

# Active logical storage: tier[0] is the only/real rate
_ACTIVE_STORAGE_SKU = _make_sku(
    "BigQuery Active Logical Storage",
    ["us"],
    [{"start": 0, "units": "0", "nanos": 23_000_000}],  # $0.023/GiB-mo
)

# Long-term logical storage
_LONGTERM_STORAGE_SKU = _make_sku(
    "BigQuery Long Term Logical Storage",
    ["us"],
    [{"start": 0, "units": "0", "nanos": 10_000_000}],  # $0.010/GiB-mo
)

# Streaming Insert SKU: tier[0] is the real rate ($0.01/GB)
_STREAMING_SKU = _make_sku(
    "BigQuery Streaming Insert",
    ["us"],
    [{"start": 0, "units": "0", "nanos": 10_000_000}],  # $0.01/GB
)

FAKE_BQ_SKUS = [
    _ANALYSIS_SKU,
    _ACTIVE_STORAGE_SKU,
    _LONGTERM_STORAGE_SKU,
    _STREAMING_SKU,
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
# Tests
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_bigquery_analysis_rate(gcp_provider):
    """Analysis SKU: tier[1] (paid) rate is extracted, not the free tier[0]=$0."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=FAKE_BQ_SKUS)):
        result = await gcp_provider.get_bigquery_price(region="us")

    assert "analysis_rate_per_tib" in result
    # $6.25 is the paid tier rate
    assert result["analysis_rate_per_tib"] == "$6.250000/TiB"


@pytest.mark.asyncio
async def test_bigquery_active_storage_rate(gcp_provider):
    """Active storage SKU: tier[0] rate is returned ($0.023/GiB-mo)."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=FAKE_BQ_SKUS)):
        result = await gcp_provider.get_bigquery_price(region="us")

    assert result["active_storage_rate_per_gib_mo"] == "$0.023000/GiB-mo"


@pytest.mark.asyncio
async def test_bigquery_longterm_storage_rate(gcp_provider):
    """Long-term storage SKU: tier[0] rate is returned ($0.010/GiB-mo)."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=FAKE_BQ_SKUS)):
        result = await gcp_provider.get_bigquery_price(region="us")

    assert result["longterm_storage_rate_per_gib_mo"] == "$0.010000/GiB-mo"


@pytest.mark.asyncio
async def test_bigquery_streaming_insert_rate(gcp_provider):
    """Streaming Insert SKU: tier[0] rate is used ($0.01/GB)."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=FAKE_BQ_SKUS)):
        result = await gcp_provider.get_bigquery_price(region="us")

    assert result["streaming_insert_rate_per_gib"] == "$0.010000/GiB"


@pytest.mark.asyncio
async def test_bigquery_cost_math(gcp_provider):
    """With query_tb=5.0 and active_storage_gb=100.0, estimated costs are correct."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=FAKE_BQ_SKUS)):
        result = await gcp_provider.get_bigquery_price(
            region="us",
            query_tb=5.0,
            active_storage_gb=100.0,
        )

    # 5 TiB * $6.25/TiB = $31.25
    assert "estimated_query_cost" in result
    assert result["estimated_query_cost"] == "$31.25"

    # 100 GB / 1024 * $0.023/GiB-mo = $0.00224609375 → rounds to $0.00
    # Let's verify the value is present and matches arithmetic
    assert "estimated_active_storage_cost" in result
    expected = Decimal("100") / Decimal("1024") * Decimal("0.023")
    assert result["estimated_active_storage_cost"] == f"${expected:.2f}"


@pytest.mark.asyncio
async def test_bigquery_multiregion_fallback(gcp_provider):
    """SKUs with serviceRegions=['us'] should be found when querying region='us-central1'."""
    # FAKE_BQ_SKUS only have serviceRegions=["us"]
    # When region="us-central1" yields no SKUs, provider falls back to "us"
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=FAKE_BQ_SKUS)):
        result = await gcp_provider.get_bigquery_price(region="us-central1")

    # Fallback to "us" multi-region should have found the analysis SKU
    assert result["analysis_rate_per_tib"] == "$6.250000/TiB"
    assert result["active_storage_rate_per_gib_mo"] == "$0.023000/GiB-mo"


@pytest.mark.asyncio
async def test_bigquery_free_tier_note(gcp_provider):
    """Result dict contains a note mentioning the free tier."""
    with patch.object(gcp_provider, "_fetch_skus", new=AsyncMock(return_value=FAKE_BQ_SKUS)):
        result = await gcp_provider.get_bigquery_price(region="us")

    assert "note" in result
    note = result["note"].lower()
    assert "free" in note
    assert "1 tib" in note or "1 tib/month" in note or "1 tib" in note
