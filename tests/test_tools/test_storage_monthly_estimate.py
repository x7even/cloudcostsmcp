"""Tests that get_storage_price returns a non-null monthly_estimate when size_gb is provided.

Covers both AWS (PER_GB_MONTH) and Azure (PER_GB_MONTH) storage SKUs.
"""
from __future__ import annotations

from decimal import Decimal
from typing import Any
from unittest.mock import AsyncMock, MagicMock

from opencloudcosts.models import (
    CloudProvider,
    NormalizedPrice,
    PriceUnit,
    PricingTerm,
)
from opencloudcosts.tools.lookup import register_lookup_tools


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

class _ToolCapture:
    """Minimal mock MCP object that captures registered tool functions by name."""

    def __init__(self) -> None:
        self._tools: dict[str, Any] = {}

    def tool(self):
        def decorator(fn):
            self._tools[fn.__name__] = fn
            return fn
        return decorator

    def __getitem__(self, name: str):
        return self._tools[name]


def _make_ctx(providers: dict[str, Any]) -> MagicMock:
    ctx = MagicMock()
    ctx.request_context.lifespan_context = {"providers": providers}
    return ctx


def _make_storage_price(
    provider: CloudProvider = CloudProvider.AZURE,
    region: str = "eastus",
    price_per_gb_month: str = "0.10",
    description: str = "Premium SSD P10",
    storage_type: str = "premium-ssd",
) -> NormalizedPrice:
    return NormalizedPrice(
        provider=provider,
        service="storage",
        sku_id="STORAGE-TESTSKU",
        product_family="Storage",
        description=description,
        region=region,
        attributes={"storage_type": storage_type},
        pricing_term=PricingTerm.ON_DEMAND,
        price_per_unit=Decimal(price_per_gb_month),
        unit=PriceUnit.PER_GB_MONTH,
    )


def _get_storage_tool():
    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    return mcp["get_storage_price"]


# ---------------------------------------------------------------------------
# Azure: monthly_estimate must be non-null when size_gb is provided
# ---------------------------------------------------------------------------

async def test_azure_storage_monthly_estimate_non_null():
    """Azure get_storage_price with size_gb=100 must return monthly_estimate != null."""
    price = _make_storage_price(
        provider=CloudProvider.AZURE,
        price_per_gb_month="0.10",
    )
    pvdr = MagicMock()
    pvdr.get_storage_price = AsyncMock(return_value=[price])
    ctx = _make_ctx({"azure": pvdr})

    tool_fn = _get_storage_tool()
    result = await tool_fn(ctx, provider="azure", storage_type="premium-ssd", region="eastus", size_gb=100.0)

    assert "prices" in result, result
    assert len(result["prices"]) == 1
    p = result["prices"][0]
    assert p["monthly_estimate"] is not None, "monthly_estimate must not be null for PER_GB_MONTH storage"
    assert p["monthly_estimate"] == "$10.00/mo"


async def test_azure_storage_monthly_estimate_calculation():
    """monthly_estimate should be price_per_unit * size_gb formatted correctly."""
    price = _make_storage_price(
        provider=CloudProvider.AZURE,
        price_per_gb_month="0.0513",  # Azure premium SSD realistic price
    )
    pvdr = MagicMock()
    pvdr.get_storage_price = AsyncMock(return_value=[price])
    ctx = _make_ctx({"azure": pvdr})

    tool_fn = _get_storage_tool()
    result = await tool_fn(ctx, provider="azure", storage_type="premium-ssd", region="eastus", size_gb=200.0)

    p = result["prices"][0]
    # 0.0513 * 200 = 10.26
    assert p["monthly_estimate"] == "$10.26/mo"
    # monthly_estimate_for_size should also be present and consistent
    assert "monthly_estimate_for_size" in p
    assert "$10.26/mo" in p["monthly_estimate_for_size"]


# ---------------------------------------------------------------------------
# AWS: monthly_estimate must be non-null when size_gb is provided
# ---------------------------------------------------------------------------

async def test_aws_storage_monthly_estimate_non_null():
    """AWS get_storage_price with size_gb=50 must return monthly_estimate != null."""
    price = _make_storage_price(
        provider=CloudProvider.AWS,
        region="us-east-1",
        price_per_gb_month="0.08",
        description="gp3 Storage",
        storage_type="gp3",
    )
    pvdr = MagicMock()
    pvdr.get_storage_price = AsyncMock(return_value=[price])
    ctx = _make_ctx({"aws": pvdr})

    tool_fn = _get_storage_tool()
    result = await tool_fn(ctx, provider="aws", storage_type="gp3", region="us-east-1", size_gb=50.0)

    assert "prices" in result
    p = result["prices"][0]
    assert p["monthly_estimate"] is not None, "monthly_estimate must not be null for PER_GB_MONTH storage"
    # 0.08 * 50 = 4.00
    assert p["monthly_estimate"] == "$4.00/mo"


# ---------------------------------------------------------------------------
# monthly_estimate_for_size key also present and consistent
# ---------------------------------------------------------------------------

async def test_monthly_estimate_for_size_consistent_with_monthly_estimate():
    """monthly_estimate_for_size should start with the same dollar amount as monthly_estimate."""
    price = _make_storage_price(price_per_gb_month="0.10")
    pvdr = MagicMock()
    pvdr.get_storage_price = AsyncMock(return_value=[price])
    ctx = _make_ctx({"azure": pvdr})

    tool_fn = _get_storage_tool()
    result = await tool_fn(ctx, provider="azure", storage_type="premium-ssd", region="eastus", size_gb=100.0)

    p = result["prices"][0]
    assert p["monthly_estimate"] == "$10.00/mo"
    assert p["monthly_estimate_for_size"].startswith("$10.00/mo")


# ---------------------------------------------------------------------------
# No prices: returns no_prices_found without crashing
# ---------------------------------------------------------------------------

async def test_storage_no_prices_found():
    pvdr = MagicMock()
    pvdr.get_storage_price = AsyncMock(return_value=[])
    ctx = _make_ctx({"azure": pvdr})

    tool_fn = _get_storage_tool()
    result = await tool_fn(ctx, provider="azure", storage_type="premium-ssd", region="eastus", size_gb=100.0)

    assert result["result"] == "no_prices_found"


# ---------------------------------------------------------------------------
# Unknown provider returns an error without crashing
# ---------------------------------------------------------------------------

async def test_storage_unknown_provider():
    tool_fn = _get_storage_tool()
    ctx = _make_ctx({})

    result = await tool_fn(ctx, provider="gcp", storage_type="pd-ssd", region="us-central1", size_gb=100.0)

    assert "error" in result
