"""Tests for list_instance_types Azure specs_unavailable guard and specs_note."""
from __future__ import annotations

from unittest.mock import AsyncMock, MagicMock

import pytest

from opencloudcosts.models import CloudProvider, InstanceTypeInfo


def _make_ctx(providers: dict) -> MagicMock:
    ctx = MagicMock()
    ctx.request_context.lifespan_context = {"providers": providers}
    return ctx


def _get_tool_fn():
    from opencloudcosts.server import create_server as create_mcp_server
    mcp = create_mcp_server()
    for tool in mcp._tool_manager.list_tools():
        if tool.name == "list_instance_types":
            return mcp._tool_manager._tools[tool.name].fn
    raise RuntimeError("list_instance_types tool not found")


def _make_azure_instance(name: str) -> InstanceTypeInfo:
    return InstanceTypeInfo(
        provider=CloudProvider.AZURE,
        instance_type=name,
        vcpu=0,
        memory_gb=0.0,
        region="eastus",
    )


# ------------------------------------------------------------------
# Part 2: min_vcpus filter returns specs_unavailable for Azure
# ------------------------------------------------------------------

async def test_azure_min_vcpus_returns_specs_unavailable():
    """Calling list_instance_types for Azure with min_vcpus should return specs_unavailable."""
    tool_fn = _get_tool_fn()
    mock_pvdr = MagicMock()
    mock_pvdr.list_instance_types = AsyncMock(return_value=[])
    ctx = _make_ctx({"azure": mock_pvdr})

    result = await tool_fn(ctx, provider="azure", region="eastus", min_vcpus=4)

    assert result["result"] == "specs_unavailable"
    assert result["provider"] == "azure"
    assert "suggestion" in result
    assert "docs" in result
    # The provider method should NOT have been called
    mock_pvdr.list_instance_types.assert_not_called()


async def test_azure_min_memory_gb_returns_specs_unavailable():
    """Calling list_instance_types for Azure with min_memory_gb should return specs_unavailable."""
    tool_fn = _get_tool_fn()
    mock_pvdr = MagicMock()
    mock_pvdr.list_instance_types = AsyncMock(return_value=[])
    ctx = _make_ctx({"azure": mock_pvdr})

    result = await tool_fn(ctx, provider="azure", region="eastus", min_memory_gb=16.0)

    assert result["result"] == "specs_unavailable"
    assert "suggestion" in result
    mock_pvdr.list_instance_types.assert_not_called()


async def test_azure_both_filters_returns_specs_unavailable():
    """Both min_vcpus and min_memory_gb together should also return specs_unavailable."""
    tool_fn = _get_tool_fn()
    mock_pvdr = MagicMock()
    mock_pvdr.list_instance_types = AsyncMock(return_value=[])
    ctx = _make_ctx({"azure": mock_pvdr})

    result = await tool_fn(
        ctx, provider="azure", region="eastus", min_vcpus=4, min_memory_gb=16.0
    )

    assert result["result"] == "specs_unavailable"
    mock_pvdr.list_instance_types.assert_not_called()


# ------------------------------------------------------------------
# Part 1: Normal Azure listing includes specs_note
# ------------------------------------------------------------------

async def test_azure_list_includes_specs_note():
    """A normal Azure list_instance_types response should include a specs_note."""
    tool_fn = _get_tool_fn()
    mock_pvdr = MagicMock()
    mock_pvdr.list_instance_types = AsyncMock(
        return_value=[
            _make_azure_instance("Standard_D4s_v3"),
            _make_azure_instance("Standard_D8s_v3"),
        ]
    )
    ctx = _make_ctx({"azure": mock_pvdr})

    result = await tool_fn(ctx, provider="azure", region="eastus")

    assert "specs_note" in result
    assert "Retail Prices API" in result["specs_note"]
    assert result["count"] == 2


async def test_non_azure_list_no_specs_note():
    """Non-Azure providers should NOT have a specs_note in the response."""
    tool_fn = _get_tool_fn()
    from opencloudcosts.models import InstanceTypeInfo
    mock_pvdr = MagicMock()
    mock_pvdr.list_instance_types = AsyncMock(
        return_value=[
            InstanceTypeInfo(
                provider=CloudProvider.AWS,
                instance_type="m5.xlarge",
                vcpu=4,
                memory_gb=16.0,
                region="us-east-1",
            )
        ]
    )
    ctx = _make_ctx({"aws": mock_pvdr})

    result = await tool_fn(ctx, provider="aws", region="us-east-1")

    assert "specs_note" not in result


# ------------------------------------------------------------------
# Part 2: suggestion field contains useful family examples
# ------------------------------------------------------------------

async def test_specs_unavailable_suggestion_contains_examples():
    """The suggestion field should mention Standard_D4s and other common families."""
    tool_fn = _get_tool_fn()
    mock_pvdr = MagicMock()
    mock_pvdr.list_instance_types = AsyncMock(return_value=[])
    ctx = _make_ctx({"azure": mock_pvdr})

    result = await tool_fn(ctx, provider="azure", region="eastus", min_vcpus=4)

    suggestion = result["suggestion"]
    assert "Standard_D4s" in suggestion
    assert "Standard_E4s" in suggestion
