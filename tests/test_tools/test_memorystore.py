"""Tests for get_memorystore_price MCP tool."""
from __future__ import annotations

from decimal import Decimal
from typing import Any
from unittest.mock import AsyncMock, MagicMock

import pytest

from opencloudcosts.models import CloudProvider, NormalizedPrice, PriceUnit, PricingTerm
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


def _make_price(
    capacity_gb: float = 10.0,
    tier: str = "standard",
    rate_per_gib_hr: str = "0.027",
    region: str = "us-central1",
) -> NormalizedPrice:
    hourly = Decimal(rate_per_gib_hr) * Decimal(str(capacity_gb))
    return NormalizedPrice(
        provider=CloudProvider.GCP,
        service="database",
        sku_id=f"gcp:memorystore:{tier}:{capacity_gb}:{region}",
        product_family="Memorystore for Redis",
        description=f"Memorystore Redis {tier.capitalize()} {capacity_gb}GB in {region}",
        region=region,
        attributes={
            "capacity_gb": str(capacity_gb),
            "tier": tier,
            "rate_per_gib_hr": rate_per_gib_hr,
        },
        pricing_term=PricingTerm.ON_DEMAND,
        price_per_unit=hourly,
        unit=PriceUnit.PER_HOUR,
    )


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_get_memorystore_price_tool():
    """Happy path: tool returns expected keys and values."""
    fake_price = _make_price(capacity_gb=10.0, tier="standard", rate_per_gib_hr="0.027")
    pvdr = MagicMock()
    pvdr.get_memorystore_price = AsyncMock(return_value=[fake_price])

    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_memorystore_price"]

    ctx = _make_ctx({"gcp": pvdr})
    result = await tool_fn(ctx, capacity_gb=10.0, region="us-central1", tier="standard")

    assert result["provider"] == "gcp"
    assert result["service"] == "memorystore_redis"
    assert result["tier"] == "standard"
    assert result["capacity_gb"] == 10.0
    assert result["region"] == "us-central1"
    assert "rate_per_gib_hr" in result
    assert "hourly_cost" in result
    assert "monthly_cost" in result
    assert "note" in result
    # Verify math: 10 GB * $0.027/GiB-hr = $0.27/hr; monthly = $0.27 * 730 = $197.10
    assert result["hourly_cost"] == "$0.2700/hr"
    assert result["monthly_cost"] == "$197.10/mo"

    pvdr.get_memorystore_price.assert_called_once_with(
        capacity_gb=10.0,
        region="us-central1",
        tier="standard",
        hours_per_month=730.0,
    )


@pytest.mark.asyncio
async def test_get_memorystore_no_provider():
    """Returns error when GCP provider is not configured."""
    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_memorystore_price"]

    ctx = _make_ctx({})  # no providers configured
    result = await tool_fn(ctx, capacity_gb=10.0, region="us-central1")

    assert "error" in result
    assert "GCP provider is not configured" in result["error"]


@pytest.mark.asyncio
async def test_get_memorystore_invalid_tier():
    """ValueError from provider is propagated as error dict."""
    pvdr = MagicMock()
    pvdr.get_memorystore_price = AsyncMock(
        side_effect=ValueError("Unknown Memorystore tier: 'premium'.")
    )

    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_memorystore_price"]

    ctx = _make_ctx({"gcp": pvdr})
    result = await tool_fn(ctx, capacity_gb=10.0, region="us-central1", tier="premium")

    assert "error" in result
    assert "premium" in result["error"]
