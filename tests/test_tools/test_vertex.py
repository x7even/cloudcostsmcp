"""Tests for get_vertex_price and get_gemini_price MCP tools."""
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


def _make_vertex_provider(vertex_result: dict, gemini_result: dict | None = None) -> MagicMock:
    pvdr = MagicMock()
    pvdr.get_vertex_price = AsyncMock(return_value=vertex_result)
    pvdr.get_gemini_price = AsyncMock(return_value=gemini_result or {})
    return pvdr


# ---------------------------------------------------------------------------
# get_vertex_price tool tests
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_get_vertex_price_happy_path():
    """Tool passes through provider result when GCP provider is configured."""
    fake_result = {
        "provider": "gcp",
        "service": "vertex_ai",
        "machine_type": "n1-standard-4",
        "family": "n1",
        "task": "training",
        "region": "us-central1",
        "hours": 730.0,
        "vcpu_rate_per_hr": "$0.240000",
        "ram_rate_per_gib_hr": "$0.032000",
        "note": "Rates are per vCPU-hour and per GiB-RAM-hour.",
    }
    pvdr = _make_vertex_provider(fake_result)

    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_vertex_price"]

    ctx = _make_ctx({"gcp": pvdr})
    result = await tool_fn(
        ctx,
        machine_type="n1-standard-4",
        region="us-central1",
        hours=730.0,
        task="training",
    )

    assert result["provider"] == "gcp"
    assert result["machine_type"] == "n1-standard-4"
    assert "vcpu_rate_per_hr" in result
    assert "ram_rate_per_gib_hr" in result
    pvdr.get_vertex_price.assert_called_once_with(
        machine_type="n1-standard-4",
        region="us-central1",
        hours=730.0,
        task="training",
    )


@pytest.mark.asyncio
async def test_get_vertex_price_no_provider():
    """Returns error dict when GCP provider is not configured."""
    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_vertex_price"]

    ctx = _make_ctx({})  # no providers
    result = await tool_fn(ctx, machine_type="n1-standard-4")

    assert "error" in result
    assert "GCP provider" in result["error"]


@pytest.mark.asyncio
async def test_get_vertex_price_invalid_task():
    """Tool-level validation: unknown task returns error dict without calling provider."""
    pvdr = _make_vertex_provider({})

    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_vertex_price"]

    ctx = _make_ctx({"gcp": pvdr})
    result = await tool_fn(ctx, machine_type="n1-standard-4", task="serving")

    assert "error" in result
    assert "serving" in result["error"]
    pvdr.get_vertex_price.assert_not_called()


@pytest.mark.asyncio
async def test_get_vertex_price_provider_exception():
    """Tool returns error dict when provider raises an unexpected exception."""
    pvdr = MagicMock()
    pvdr.get_vertex_price = AsyncMock(side_effect=RuntimeError("network timeout"))

    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_vertex_price"]

    ctx = _make_ctx({"gcp": pvdr})
    result = await tool_fn(ctx, machine_type="n1-standard-4", region="us-central1")

    assert "error" in result
    assert "Pricing lookup failed" in result["error"]


# ---------------------------------------------------------------------------
# get_gemini_price tool tests
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_get_gemini_price_happy_path():
    """Tool passes through provider result when GCP provider is configured."""
    fake_result = {
        "provider": "gcp",
        "service": "vertex_ai_gemini",
        "model": "gemini-1.5-flash",
        "region": "us-central1",
        "rates": {
            "input:Gemini 1.5 Flash Input": "$0.00000025",
            "output:Gemini 1.5 Flash Output": "$0.00000075",
        },
        "note": "Rates are per character or per token.",
    }
    pvdr = _make_vertex_provider({}, gemini_result=fake_result)

    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_gemini_price"]

    ctx = _make_ctx({"gcp": pvdr})
    result = await tool_fn(ctx, model="gemini-1.5-flash", region="us-central1")

    assert result["provider"] == "gcp"
    assert result["service"] == "vertex_ai_gemini"
    assert "rates" in result
    pvdr.get_gemini_price.assert_called_once_with(
        model="gemini-1.5-flash",
        region="us-central1",
    )


@pytest.mark.asyncio
async def test_get_gemini_price_no_provider():
    """Returns error dict when GCP provider is not configured."""
    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_gemini_price"]

    ctx = _make_ctx({})  # no providers
    result = await tool_fn(ctx, model="gemini-1.5-flash")

    assert "error" in result
    assert "GCP provider" in result["error"]


@pytest.mark.asyncio
async def test_get_gemini_price_provider_exception():
    """Tool returns error dict when provider raises an unexpected exception."""
    pvdr = MagicMock()
    pvdr.get_gemini_price = AsyncMock(side_effect=RuntimeError("catalog unavailable"))

    mcp = _ToolCapture()
    register_lookup_tools(mcp)
    tool_fn = mcp["get_gemini_price"]

    ctx = _make_ctx({"gcp": pvdr})
    result = await tool_fn(ctx, model="gemini-1.5-flash", region="us-central1")

    assert "error" in result
    assert "Pricing lookup failed" in result["error"]
