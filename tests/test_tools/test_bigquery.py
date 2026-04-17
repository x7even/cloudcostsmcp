"""Tests for get_bigquery_price MCP tool."""
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


_RATES_ONLY_RESULT = {
    "region": "us",
    "provider": "gcp",
    "service": "bigquery",
    "analysis_rate_per_tib": "$6.250000/TiB",
    "active_storage_rate_per_gib_mo": "$0.023000/GiB-mo",
    "longterm_storage_rate_per_gib_mo": "$0.010000/GiB-mo",
    "streaming_insert_rate_per_gib": "$0.010000/GiB",
    "note": (
        "BigQuery free tier: first 1 TiB/month of query processing is free; "
        "first 10 GiB/month of active storage is free. "
        "Rates shown are for usage above the free tier."
    ),
}

_WITH_COSTS_RESULT = {
    **_RATES_ONLY_RESULT,
    "estimated_query_cost": "$31.25",
    "estimated_active_storage_cost": "$0.00",
}


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_get_bigquery_price_rates_only():
    """Happy path: no quantities provided, returns rates only."""
    pvdr = MagicMock()
    pvdr.get_bigquery_price = AsyncMock(return_value=_RATES_ONLY_RESULT)

    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_bigquery_price"]

    ctx = _make_ctx({"gcp": pvdr})
    result = await tool_fn(ctx, region="us")

    assert result["provider"] == "gcp"
    assert result["service"] == "bigquery"
    assert result["region"] == "us"
    assert "analysis_rate_per_tib" in result
    assert "active_storage_rate_per_gib_mo" in result
    assert "longterm_storage_rate_per_gib_mo" in result
    assert "streaming_insert_rate_per_gib" in result
    assert "note" in result
    # No cost estimates since no quantities passed
    assert "estimated_query_cost" not in result
    assert "estimated_active_storage_cost" not in result

    pvdr.get_bigquery_price.assert_called_once_with(
        region="us",
        query_tb=None,
        active_storage_gb=None,
        longterm_storage_gb=None,
        streaming_gb=None,
    )


@pytest.mark.asyncio
async def test_get_bigquery_no_provider():
    """Returns error dict when GCP provider is not configured."""
    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_bigquery_price"]

    ctx = _make_ctx({})  # no providers configured
    result = await tool_fn(ctx, region="us")

    assert "error" in result
    assert "GCP provider is not configured" in result["error"]


@pytest.mark.asyncio
async def test_get_bigquery_with_costs():
    """With quantities, tool passes them through and returns cost estimates."""
    pvdr = MagicMock()
    pvdr.get_bigquery_price = AsyncMock(return_value=_WITH_COSTS_RESULT)

    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_bigquery_price"]

    ctx = _make_ctx({"gcp": pvdr})
    result = await tool_fn(
        ctx,
        region="us",
        query_tb=5.0,
        active_storage_gb=100.0,
    )

    assert "estimated_query_cost" in result
    assert "estimated_active_storage_cost" in result

    pvdr.get_bigquery_price.assert_called_once_with(
        region="us",
        query_tb=5.0,
        active_storage_gb=100.0,
        longterm_storage_gb=None,
        streaming_gb=None,
    )
