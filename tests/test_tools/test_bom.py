"""Tests for estimate_bom and estimate_unit_economics os/spec handling (v0.8.0)."""
from __future__ import annotations

from decimal import Decimal
from unittest.mock import AsyncMock, MagicMock

import pytest

from opencloudcosts.models import (
    CloudProvider,
    NormalizedPrice,
    PriceUnit,
    PricingResult,
    PricingTerm,
)


def _make_price(region: str = "us-east-1", price: str = "0.192") -> NormalizedPrice:
    return NormalizedPrice(
        provider=CloudProvider.AWS,
        service="compute",
        sku_id="TESTSKU",
        product_family="Compute Instance",
        description="m5.xlarge Linux",
        region=region,
        attributes={"instanceType": "m5.xlarge", "vcpu": "4", "memory": "16 GiB"},
        pricing_term=PricingTerm.ON_DEMAND,
        price_per_unit=Decimal(price),
        unit=PriceUnit.PER_HOUR,
    )


def _make_provider(price: NormalizedPrice | None = None):
    pvdr = MagicMock()
    pvdr.supports = MagicMock(return_value=True)
    result = PricingResult(public_prices=[price or _make_price()])
    pvdr.get_price = AsyncMock(return_value=result)
    return pvdr


def _make_ctx(pvdr):
    ctx = MagicMock()
    ctx.request_context.lifespan_context = {"providers": {"aws": pvdr}}
    return ctx


# ------------------------------------------------------------------
# estimate_bom — os field passed through in spec
# ------------------------------------------------------------------

async def test_estimate_bom_windows_os():
    """Windows BoM item should include os='Windows' in the ComputePricingSpec passed to get_price."""
    from opencloudcosts.server import create_server as create_mcp_server

    pvdr = _make_provider()
    ctx = _make_ctx(pvdr)
    mcp = create_mcp_server()

    tool_fn = None
    for tool in mcp._tool_manager.list_tools():
        if tool.name == "estimate_bom":
            tool_fn = mcp._tool_manager._tools[tool.name].fn
            break

    assert tool_fn is not None, "estimate_bom tool not found"

    items = [
        {
            "provider": "aws",
            "domain": "compute",
            "resource_type": "m5.xlarge",
            "region": "us-east-1",
            "os": "Windows",
        }
    ]
    result = await tool_fn(ctx, items)

    assert "error" not in result or result.get("error") is None
    pvdr.get_price.assert_called_once()
    spec_arg = pvdr.get_price.call_args.args[0]
    assert spec_arg.os == "Windows"


async def test_estimate_bom_os_defaults_to_linux():
    """BoM item without os field should default to Linux."""
    from opencloudcosts.server import create_server as create_mcp_server

    pvdr = _make_provider()
    ctx = _make_ctx(pvdr)
    mcp = create_mcp_server()

    tool_fn = None
    for tool in mcp._tool_manager.list_tools():
        if tool.name == "estimate_bom":
            tool_fn = mcp._tool_manager._tools[tool.name].fn
            break

    assert tool_fn is not None

    items = [
        {
            "provider": "aws",
            "domain": "compute",
            "resource_type": "m5.xlarge",
            "region": "us-east-1",
        }
    ]
    result = await tool_fn(ctx, items)

    assert "error" not in result or result.get("error") is None
    pvdr.get_price.assert_called_once()
    spec_arg = pvdr.get_price.call_args.args[0]
    assert spec_arg.os == "Linux"


# ------------------------------------------------------------------
# estimate_unit_economics — os field passed through in spec
# ------------------------------------------------------------------

async def test_estimate_unit_economics_windows_os():
    """estimate_unit_economics Windows item should pass os='Windows' in spec to get_price."""
    from opencloudcosts.server import create_server as create_mcp_server

    pvdr = _make_provider()
    ctx = _make_ctx(pvdr)
    mcp = create_mcp_server()

    tool_fn = None
    for tool in mcp._tool_manager.list_tools():
        if tool.name == "estimate_unit_economics":
            tool_fn = mcp._tool_manager._tools[tool.name].fn
            break

    assert tool_fn is not None, "estimate_unit_economics tool not found"

    items = [
        {
            "provider": "aws",
            "domain": "compute",
            "resource_type": "m5.xlarge",
            "region": "us-east-1",
            "os": "Windows",
        }
    ]
    result = await tool_fn(ctx, items, units_per_month=1000.0)

    assert "error" not in result or result.get("error") is None
    pvdr.get_price.assert_called_once()
    spec_arg = pvdr.get_price.call_args.args[0]
    assert spec_arg.os == "Windows"


async def test_estimate_unit_economics_os_defaults_to_linux():
    """estimate_unit_economics without os field should default to Linux."""
    from opencloudcosts.server import create_server as create_mcp_server

    pvdr = _make_provider()
    ctx = _make_ctx(pvdr)
    mcp = create_mcp_server()

    tool_fn = None
    for tool in mcp._tool_manager.list_tools():
        if tool.name == "estimate_unit_economics":
            tool_fn = mcp._tool_manager._tools[tool.name].fn
            break

    assert tool_fn is not None

    items = [
        {
            "provider": "aws",
            "domain": "compute",
            "resource_type": "m5.xlarge",
            "region": "us-east-1",
        }
    ]
    result = await tool_fn(ctx, items, units_per_month=1000.0)

    assert "error" not in result or result.get("error") is None
    pvdr.get_price.assert_called_once()
    spec_arg = pvdr.get_price.call_args.args[0]
    assert spec_arg.os == "Linux"
