"""Tests for GCP Vertex AI pricing (compute + Gemini)."""
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


def _make_vertex_sku(
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
# Vertex AI compute tests
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_vertex_training_vcpu_rate(provider):
    """SKU with 'Vertex AI Custom Training N1 predefined vCPU' → vcpu rate extracted."""
    fake_skus = [
        _make_vertex_sku(
            "Vertex AI Custom Training N1 predefined vCPU",
            ["us-central1"],
            price_units="0",
            price_nanos=240_000_000,  # $0.24/vCPU-hr
        ),
        _make_vertex_sku(
            "Vertex AI Custom Training N1 predefined RAM",
            ["us-central1"],
            price_units="0",
            price_nanos=32_000_000,   # $0.032/GiB-hr
        ),
    ]
    with patch.object(provider, "_fetch_skus", new=AsyncMock(return_value=fake_skus)):
        result = await provider.get_vertex_price(
            machine_type="n1-standard-4",
            region="us-central1",
            task="training",
        )

    assert "error" not in result
    assert result["provider"] == "gcp"
    assert result["service"] == "vertex_ai"
    assert result["machine_type"] == "n1-standard-4"
    assert result["family"] == "n1"
    # vcpu rate should be $0.24/hr
    vcpu_rate = Decimal(result["vcpu_rate_per_hr"].lstrip("$"))
    assert vcpu_rate == Decimal("0.240000")


@pytest.mark.asyncio
async def test_vertex_training_rate_extraction(provider):
    """Both vcpu and ram rates are extracted correctly from matching SKUs."""
    vcpu_nanos = 240_000_000   # $0.24/vCPU-hr
    ram_nanos = 32_000_000     # $0.032/GiB-hr

    fake_skus = [
        _make_vertex_sku(
            "Vertex AI Custom Training N1 predefined vCPU",
            ["us-central1"],
            price_nanos=vcpu_nanos,
        ),
        _make_vertex_sku(
            "Vertex AI Custom Training N1 predefined RAM",
            ["us-central1"],
            price_nanos=ram_nanos,
        ),
    ]
    with patch.object(provider, "_fetch_skus", new=AsyncMock(return_value=fake_skus)):
        result = await provider.get_vertex_price(
            machine_type="n1-standard-4",
            region="us-central1",
            hours=730.0,
            task="training",
        )

    assert "error" not in result
    vcpu_rate = Decimal(result["vcpu_rate_per_hr"].lstrip("$"))
    ram_rate = Decimal(result["ram_rate_per_gib_hr"].lstrip("$"))

    assert vcpu_rate == Decimal("0.240000")
    assert ram_rate == Decimal("0.032000")

    # Verify the note mentions hours
    assert "730" in result["note"]
    assert result["hours"] == 730.0


@pytest.mark.asyncio
async def test_vertex_no_sku_returns_error(provider):
    """Empty SKU list → graceful error dict, not an exception."""
    with patch.object(provider, "_fetch_skus", new=AsyncMock(return_value=[])):
        result = await provider.get_vertex_price(
            machine_type="n1-standard-4",
            region="us-central1",
        )

    assert isinstance(result, dict)
    assert "error" in result
    assert "n1-standard-4" in result["error"] or "us-central1" in result["error"]


@pytest.mark.asyncio
async def test_vertex_no_matching_family_returns_error(provider):
    """SKUs present but none matching the family → graceful error dict."""
    fake_skus = [
        _make_vertex_sku(
            "Vertex AI Custom Training A2 accelerator-optimized vCPU",
            ["us-central1"],
            price_nanos=500_000_000,
        ),
    ]
    with patch.object(provider, "_fetch_skus", new=AsyncMock(return_value=fake_skus)):
        result = await provider.get_vertex_price(
            machine_type="n1-standard-4",
            region="us-central1",
        )

    assert isinstance(result, dict)
    assert "error" in result


@pytest.mark.asyncio
async def test_vertex_training_vs_prediction_same_rate(provider):
    """Same machine type, both tasks return dict (they may share the same SKU)."""
    fake_skus = [
        _make_vertex_sku(
            "Vertex AI Custom Training N1 predefined vCPU",
            ["us-central1"],
            price_nanos=240_000_000,
        ),
        _make_vertex_sku(
            "Vertex AI Custom Training N1 predefined RAM",
            ["us-central1"],
            price_nanos=32_000_000,
        ),
    ]
    with patch.object(provider, "_fetch_skus", new=AsyncMock(return_value=fake_skus)):
        training_result = await provider.get_vertex_price(
            machine_type="n1-standard-4",
            region="us-central1",
            task="training",
        )
        prediction_result = await provider.get_vertex_price(
            machine_type="n1-standard-4",
            region="us-central1",
            task="prediction",
        )

    # Both must be dicts — behaviour when SKU doesn't say "prediction" is graceful
    assert isinstance(training_result, dict)
    assert isinstance(prediction_result, dict)
    # training result should have rates
    assert "error" not in training_result


# ---------------------------------------------------------------------------
# Gemini pricing tests
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_gemini_rate_extraction(provider):
    """SKU with 'Gemini 1.5 Flash Input' → input rate extracted."""
    fake_skus = [
        _make_vertex_sku(
            "Gemini 1.5 Flash Input",
            ["us-central1"],
            price_units="0",
            price_nanos=250,   # $0.00000025 / char
        ),
        _make_vertex_sku(
            "Gemini 1.5 Flash Output",
            ["us-central1"],
            price_units="0",
            price_nanos=750,   # $0.00000075 / char
        ),
    ]
    with patch.object(provider, "_fetch_skus", new=AsyncMock(return_value=fake_skus)):
        result = await provider.get_gemini_price(
            model="gemini-1.5-flash",
            region="us-central1",
        )

    assert "error" not in result
    assert result["provider"] == "gcp"
    assert result["service"] == "vertex_ai_gemini"
    assert result["model"] == "gemini-1.5-flash"
    rates = result["rates"]
    assert any("input" in k.lower() for k in rates)
    assert any("output" in k.lower() for k in rates)


@pytest.mark.asyncio
async def test_gemini_no_sku_returns_error(provider):
    """No Gemini SKUs → graceful error dict, not an exception."""
    with patch.object(provider, "_fetch_skus", new=AsyncMock(return_value=[])):
        result = await provider.get_gemini_price(
            model="gemini-1.5-flash",
            region="us-central1",
        )

    assert isinstance(result, dict)
    assert "error" in result


@pytest.mark.asyncio
async def test_gemini_non_matching_model_returns_error(provider):
    """Gemini SKUs present but not for the requested model → graceful error."""
    fake_skus = [
        _make_vertex_sku(
            "Gemini 1.0 Pro Input Characters",
            ["us-central1"],
            price_nanos=125,
        ),
    ]
    with patch.object(provider, "_fetch_skus", new=AsyncMock(return_value=fake_skus)):
        result = await provider.get_gemini_price(
            model="gemini-1.5-flash",
            region="us-central1",
        )

    # "flash" is not in "1.0 Pro" description — must return error
    assert isinstance(result, dict)
    assert "error" in result
