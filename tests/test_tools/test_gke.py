"""Tests for get_gke_price MCP tool."""
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


def _make_gke_provider(return_value: dict) -> MagicMock:
    pvdr = MagicMock()
    pvdr.get_gke_price = AsyncMock(return_value=return_value)
    # hasattr check — ensure attribute exists
    pvdr.__class__.get_gke_price = pvdr.get_gke_price
    return pvdr


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_get_gke_price_standard_tool():
    """Tool returns dict with expected keys for standard mode."""
    fake_result = {
        "mode": "standard",
        "cluster_management_fee_per_hr": "$0.100000",
        "cluster_management_monthly": "$73.00",
        "node_pricing_hint": "Use get_compute_price(...) for each node. Multiply by 3 nodes.",
        "total_monthly_note": "Total = cluster fee + (3 × node_hourly × 730 hrs)",
        "region": "us-central1",
    }
    pvdr = MagicMock()
    pvdr.get_gke_price = AsyncMock(return_value=fake_result)

    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_gke_price"]

    ctx = _make_ctx({"gcp": pvdr})
    result = await tool_fn(
        ctx,
        region="us-central1",
        mode="standard",
        node_count=3,
        node_type="n1-standard-4",
    )

    assert result["mode"] == "standard"
    assert "cluster_management_fee_per_hr" in result
    assert "cluster_management_monthly" in result
    assert "node_pricing_hint" in result
    pvdr.get_gke_price.assert_called_once()


@pytest.mark.asyncio
async def test_get_gke_price_autopilot_tool():
    """Tool returns dict with expected keys for autopilot mode."""
    fake_result = {
        "mode": "autopilot",
        "vcpu_rate_per_hr": "$0.064000",
        "memory_rate_per_gib_hr": "$0.009982",
        "requested_vcpu": 4.0,
        "requested_memory_gb": 16.0,
        "hourly_cost": "$0.415712",
        "monthly_cost": "$303.47",
        "region": "us-central1",
        "note": "Autopilot charges for actual pod resource requests only",
    }
    pvdr = MagicMock()
    pvdr.get_gke_price = AsyncMock(return_value=fake_result)

    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_gke_price"]

    ctx = _make_ctx({"gcp": pvdr})
    result = await tool_fn(
        ctx,
        region="us-central1",
        mode="autopilot",
        vcpu=4.0,
        memory_gb=16.0,
    )

    assert result["mode"] == "autopilot"
    assert "vcpu_rate_per_hr" in result
    assert "monthly_cost" in result
    assert result["requested_vcpu"] == 4.0


@pytest.mark.asyncio
async def test_get_gke_price_no_provider():
    """Returns error when GCP provider is not configured."""
    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_gke_price"]

    ctx = _make_ctx({})  # no providers at all
    result = await tool_fn(ctx, region="us-central1", mode="standard")

    assert "error" in result
    assert "GCP provider not configured" in result["error"]


@pytest.mark.asyncio
async def test_get_gke_price_invalid_mode():
    """Tool-level mode validation: unknown mode returns error dict."""
    pvdr = MagicMock()
    pvdr.get_gke_price = AsyncMock(return_value={})

    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_gke_price"]

    ctx = _make_ctx({"gcp": pvdr})
    result = await tool_fn(ctx, region="us-central1", mode="invalid_mode")

    assert "error" in result
    assert "invalid_mode" in result["error"]
    # Provider should NOT have been called
    pvdr.get_gke_price.assert_not_called()


@pytest.mark.asyncio
async def test_get_gke_price_provider_exception():
    """Tool returns error dict when provider raises an unexpected exception."""
    pvdr = MagicMock()
    pvdr.get_gke_price = AsyncMock(side_effect=RuntimeError("connection timeout"))

    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_gke_price"]

    ctx = _make_ctx({"gcp": pvdr})
    result = await tool_fn(ctx, region="us-central1", mode="standard")

    assert "error" in result
    assert "Pricing lookup failed" in result["error"]
