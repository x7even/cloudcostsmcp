"""Tests for estimate_bom and estimate_unit_economics os field (T20)."""
from __future__ import annotations

from decimal import Decimal
from unittest.mock import AsyncMock, MagicMock

import pytest

from opencloudcosts.models import (
    CloudProvider,
    NormalizedPrice,
    PriceUnit,
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


@pytest.fixture
def mock_provider():
    pvdr = MagicMock()
    pvdr.get_compute_price = AsyncMock(return_value=[_make_price()])
    return pvdr


@pytest.fixture
def bom_context(mock_provider):
    ctx = MagicMock()
    ctx.request_context.lifespan_context = {"providers": {"aws": mock_provider}}
    return ctx, mock_provider


# ------------------------------------------------------------------
# estimate_bom — os field
# ------------------------------------------------------------------

async def test_estimate_bom_windows_os(bom_context):
    """Windows BoM item should pass os='Windows' to get_compute_price."""
    from opencloudcosts.server import create_server as create_mcp_server

    ctx, mock_provider = bom_context
    mcp = create_mcp_server()

    # Find the registered tool function
    tool_fn = None
    for tool in mcp._tool_manager.list_tools():
        if tool.name == "estimate_bom":
            tool_fn = mcp._tool_manager._tools[tool.name].fn
            break

    assert tool_fn is not None, "estimate_bom tool not found"

    items = [
        {
            "provider": "aws",
            "service": "compute",
            "type": "m5.xlarge",
            "region": "us-east-1",
            "os": "Windows",
        }
    ]
    result = await tool_fn(ctx, items)

    assert "error" not in result or result.get("error") is None
    mock_provider.get_compute_price.assert_called_once()
    call_args = mock_provider.get_compute_price.call_args
    # Third positional arg (index 2) should be "Windows"
    assert call_args.args[2] == "Windows", (
        f"Expected os='Windows' but got '{call_args.args[2]}'"
    )


async def test_estimate_bom_os_defaults_to_linux(bom_context):
    """BoM item without os field should default to Linux (backwards compatible)."""
    from opencloudcosts.server import create_server as create_mcp_server

    ctx, mock_provider = bom_context
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
            "service": "compute",
            "type": "m5.xlarge",
            "region": "us-east-1",
            # no "os" field — should default to Linux
        }
    ]
    result = await tool_fn(ctx, items)

    assert "error" not in result or result.get("error") is None
    mock_provider.get_compute_price.assert_called_once()
    call_args = mock_provider.get_compute_price.call_args
    assert call_args.args[2] == "Linux", (
        f"Expected default os='Linux' but got '{call_args.args[2]}'"
    )


# ------------------------------------------------------------------
# estimate_unit_economics — os field
# ------------------------------------------------------------------

async def test_estimate_unit_economics_windows_os(bom_context):
    """estimate_unit_economics Windows item should pass os='Windows' to get_compute_price."""
    from opencloudcosts.server import create_server as create_mcp_server

    ctx, mock_provider = bom_context
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
            "service": "compute",
            "type": "m5.xlarge",
            "region": "us-east-1",
            "os": "Windows",
        }
    ]
    result = await tool_fn(ctx, items, units_per_month=1000.0)

    assert "error" not in result or result.get("error") is None
    mock_provider.get_compute_price.assert_called_once()
    call_args = mock_provider.get_compute_price.call_args
    assert call_args.args[2] == "Windows"


async def test_estimate_unit_economics_os_defaults_to_linux(bom_context):
    """estimate_unit_economics without os field should default to Linux."""
    from opencloudcosts.server import create_server as create_mcp_server

    ctx, mock_provider = bom_context
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
            "service": "compute",
            "type": "m5.xlarge",
            "region": "us-east-1",
        }
    ]
    result = await tool_fn(ctx, items, units_per_month=1000.0)

    assert "error" not in result or result.get("error") is None
    mock_provider.get_compute_price.assert_called_once()
    call_args = mock_provider.get_compute_price.call_args
    assert call_args.args[2] == "Linux"
