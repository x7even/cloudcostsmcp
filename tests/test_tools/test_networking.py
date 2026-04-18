"""Tests for get_cloud_lb_price, get_cloud_cdn_price, get_cloud_nat_price MCP tools."""
from __future__ import annotations

from typing import Any
from unittest.mock import AsyncMock, MagicMock

import pytest

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


_LB_RESULT = {
    "provider": "gcp",
    "region": "us-central1",
    "service": "cloud_load_balancing",
    "lb_type": "https",
    "rule_count": 1,
    "rule_rate_per_hr": "$0.008000/hr",
    "data_processed_rate_per_gb": "$0.008000/GB",
    "monthly_rule_cost": "$5.84",
    "note": "Cloud Load Balancing: forwarding rules are billed per hour.",
}

_CDN_RESULT = {
    "provider": "gcp",
    "region": "us-central1",
    "service": "cloud_cdn",
    "egress_rate_per_gb": "$0.020000/GB",
    "cache_fill_rate_per_gb": "$0.010000/GB",
    "note": "Cloud CDN egress rates vary by destination region.",
}

_NAT_RESULT = {
    "provider": "gcp",
    "region": "us-central1",
    "service": "cloud_nat",
    "gateway_count": 1,
    "gateway_rate_per_hr": "$0.044000/hr",
    "data_processed_rate_per_gb": "$0.045000/GB",
    "monthly_gateway_cost": "$32.12",
    "note": "Cloud NAT gateway uptime is billed per hour.",
}


# ---------------------------------------------------------------------------
# Load Balancing tool tests
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_get_lb_price_happy():
    """Happy path: mock provider, verify tool returns provider dict."""
    pvdr = MagicMock()
    pvdr.get_cloud_lb_price = AsyncMock(return_value=_LB_RESULT)

    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_cloud_lb_price"]

    ctx = _make_ctx({"gcp": pvdr})
    result = await tool_fn(ctx, region="us-central1", lb_type="https", rule_count=1)

    assert result["provider"] == "gcp"
    assert result["service"] == "cloud_load_balancing"
    assert "rule_rate_per_hr" in result

    pvdr.get_cloud_lb_price.assert_called_once_with(
        region="us-central1",
        lb_type="https",
        rule_count=1,
        data_gb=0.0,
        hours_per_month=730.0,
    )


@pytest.mark.asyncio
async def test_get_lb_no_provider():
    """Returns error dict when GCP provider is not configured."""
    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_cloud_lb_price"]

    ctx = _make_ctx({})
    result = await tool_fn(ctx)

    assert "error" in result
    assert "GCP provider is not configured" in result["error"]


# ---------------------------------------------------------------------------
# Cloud CDN tool tests
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_get_cdn_price_happy():
    """Happy path: mock provider, tool passes through."""
    pvdr = MagicMock()
    pvdr.get_cloud_cdn_price = AsyncMock(return_value=_CDN_RESULT)

    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_cloud_cdn_price"]

    ctx = _make_ctx({"gcp": pvdr})
    result = await tool_fn(ctx, region="us-central1", egress_gb=1000.0, cache_fill_gb=100.0)

    assert result["provider"] == "gcp"
    assert result["service"] == "cloud_cdn"
    assert "egress_rate_per_gb" in result

    pvdr.get_cloud_cdn_price.assert_called_once_with(
        region="us-central1",
        egress_gb=1000.0,
        cache_fill_gb=100.0,
    )


@pytest.mark.asyncio
async def test_get_cdn_no_provider():
    """Returns error dict when GCP provider is not configured."""
    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_cloud_cdn_price"]

    ctx = _make_ctx({})
    result = await tool_fn(ctx)

    assert "error" in result
    assert "GCP provider is not configured" in result["error"]


# ---------------------------------------------------------------------------
# Cloud NAT tool tests
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_get_nat_price_happy():
    """Happy path: mock provider, tool passes through."""
    pvdr = MagicMock()
    pvdr.get_cloud_nat_price = AsyncMock(return_value=_NAT_RESULT)

    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_cloud_nat_price"]

    ctx = _make_ctx({"gcp": pvdr})
    result = await tool_fn(ctx, region="us-central1", gateway_count=2, data_gb=500.0)

    assert result["provider"] == "gcp"
    assert result["service"] == "cloud_nat"
    assert "gateway_rate_per_hr" in result

    pvdr.get_cloud_nat_price.assert_called_once_with(
        region="us-central1",
        gateway_count=2,
        data_gb=500.0,
        hours_per_month=730.0,
    )


@pytest.mark.asyncio
async def test_get_nat_no_provider():
    """Returns error dict when GCP provider is not configured."""
    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_cloud_nat_price"]

    ctx = _make_ctx({})
    result = await tool_fn(ctx)

    assert "error" in result
    assert "GCP provider is not configured" in result["error"]
