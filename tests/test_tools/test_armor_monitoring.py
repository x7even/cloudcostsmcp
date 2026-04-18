"""Tests for get_cloud_armor_price and get_cloud_monitoring_price MCP tools."""
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


_ARMOR_RESULT = {
    "provider": "gcp",
    "service": "cloud_armor",
    "policy_rate_per_month": "$0.75/policy/month",
    "request_rate_per_million": "$0.750000/million requests",
    "estimated_policy_cost": "$0.75",
    "estimated_request_cost": "$0.00",
    "estimated_total_monthly": "$0.75",
    "note": "Cloud Armor Standard: ...",
}

_MONITORING_RESULT = {
    "provider": "gcp",
    "service": "cloud_monitoring",
    "free_tier_mib": 150,
    "tier1_rate_per_mib": "$0.258/MiB (0–100,000 MiB/month)",
    "tier2_rate_per_mib": "$0.151/MiB (100,001–250,000 MiB/month)",
    "tier3_rate_per_mib": "$0.062/MiB (above 250,000 MiB/month)",
    "note": "Cloud Monitoring: ...",
}


# ---------------------------------------------------------------------------
# Cloud Armor tool tests
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_get_armor_price_happy():
    """Happy path: mock provider returns armor dict, tool passes it through."""
    pvdr = MagicMock()
    pvdr.get_cloud_armor_price = AsyncMock(return_value=_ARMOR_RESULT)

    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_cloud_armor_price"]

    ctx = _make_ctx({"gcp": pvdr})
    result = await tool_fn(ctx, policy_count=1, monthly_requests_millions=0.0)

    assert result["provider"] == "gcp"
    assert result["service"] == "cloud_armor"
    assert "policy_rate_per_month" in result
    assert "request_rate_per_million" in result

    pvdr.get_cloud_armor_price.assert_called_once_with(
        policy_count=1,
        monthly_requests_millions=0.0,
    )


@pytest.mark.asyncio
async def test_get_armor_no_provider():
    """Returns error dict when GCP provider is not configured."""
    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_cloud_armor_price"]

    ctx = _make_ctx({})  # no providers configured
    result = await tool_fn(ctx)

    assert "error" in result
    assert "GCP provider is not configured" in result["error"]


# ---------------------------------------------------------------------------
# Cloud Monitoring tool tests
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_get_monitoring_price_happy():
    """Happy path: mock provider returns monitoring dict, tool passes it through."""
    pvdr = MagicMock()
    pvdr.get_cloud_monitoring_price = AsyncMock(return_value=_MONITORING_RESULT)

    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_cloud_monitoring_price"]

    ctx = _make_ctx({"gcp": pvdr})
    result = await tool_fn(ctx, ingestion_mib=200.0)

    assert result["provider"] == "gcp"
    assert result["service"] == "cloud_monitoring"
    assert result["free_tier_mib"] == 150
    assert "tier1_rate_per_mib" in result

    pvdr.get_cloud_monitoring_price.assert_called_once_with(
        ingestion_mib=200.0,
    )


@pytest.mark.asyncio
async def test_get_monitoring_no_provider():
    """Returns error dict when GCP provider is not configured."""
    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_cloud_monitoring_price"]

    ctx = _make_ctx({})  # no providers configured
    result = await tool_fn(ctx)

    assert "error" in result
    assert "GCP provider is not configured" in result["error"]
