"""Tests for list_instance_types effective_max / spec-filter cap behaviour.

effective_max is 200 when min_vcpu or min_memory_gb is set and max_results < 200.
"""
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


def _make_aws_instances(n: int, vcpu: int = 4, memory_gb: float = 16.0) -> list[InstanceTypeInfo]:
    return [
        InstanceTypeInfo(
            provider=CloudProvider.AWS,
            instance_type=f"m5.{i}xlarge",
            vcpu=vcpu,
            memory_gb=memory_gb,
            region="us-east-1",
        )
        for i in range(n)
    ]


# ------------------------------------------------------------------
# spec-filtered queries: effective_max bumped to 200
# ------------------------------------------------------------------

async def test_min_vcpu_bumps_effective_max_to_200():
    """When min_vcpu is set and max_results=20, effective_max becomes 200.
    With 120 instances all fitting within 200, none are truncated."""
    tool_fn = _get_tool_fn()
    instances = _make_aws_instances(120, vcpu=4, memory_gb=16.0)
    mock_pvdr = MagicMock()
    mock_pvdr.list_instance_types = AsyncMock(return_value=instances)
    ctx = _make_ctx({"aws": mock_pvdr})

    result = await tool_fn(
        ctx, provider="aws", region="us-east-1", min_vcpu=4, max_results=20
    )

    # All 120 fit within effective_max=200
    assert result["count"] == 120
    assert result["truncated"] is False
    assert result["total_found"] == 120


async def test_min_memory_gb_bumps_effective_max_to_200():
    """min_memory_gb bumps effective_max to 200; 110 instances all fit."""
    tool_fn = _get_tool_fn()
    instances = _make_aws_instances(110, vcpu=8, memory_gb=64.0)
    mock_pvdr = MagicMock()
    mock_pvdr.list_instance_types = AsyncMock(return_value=instances)
    ctx = _make_ctx({"aws": mock_pvdr})

    result = await tool_fn(
        ctx, provider="aws", region="us-east-1", min_memory_gb=16.0, max_results=50
    )

    assert result["count"] == 110
    assert result["truncated"] is False
    assert result["total_found"] == 110


async def test_both_spec_filters_bump_effective_max():
    """Both filters together bump effective_max to 200; 105 instances all fit."""
    tool_fn = _get_tool_fn()
    instances = _make_aws_instances(105, vcpu=4, memory_gb=32.0)
    mock_pvdr = MagicMock()
    mock_pvdr.list_instance_types = AsyncMock(return_value=instances)
    ctx = _make_ctx({"aws": mock_pvdr})

    result = await tool_fn(
        ctx, provider="aws", region="us-east-1",
        min_vcpu=4, min_memory_gb=16.0, max_results=20
    )

    assert result["count"] == 105
    assert result["truncated"] is False


async def test_spec_filter_with_250_instances_still_truncates():
    """With 250 instances and min_vcpu set, effective_max=200 so 50 are truncated."""
    tool_fn = _get_tool_fn()
    instances = _make_aws_instances(250, vcpu=4, memory_gb=16.0)
    mock_pvdr = MagicMock()
    mock_pvdr.list_instance_types = AsyncMock(return_value=instances)
    ctx = _make_ctx({"aws": mock_pvdr})

    result = await tool_fn(
        ctx, provider="aws", region="us-east-1", min_vcpu=4, max_results=20
    )

    assert result["count"] == 200
    assert result["truncated"] is True
    assert result["total_found"] == 250
    # Spec-filtered truncation message uses "Spec filters applied" format
    truncation_msg = next(
        (s for s in result["next_steps"] if "Spec filters applied" in s), None
    )
    assert truncation_msg is not None
    assert "250 total matches" in truncation_msg
    assert "showing 200" in truncation_msg
    assert "50 more instances" in truncation_msg


async def test_explicit_max_results_300_honoured():
    """Callers who pass max_results=300 (> 200 floor) get that value honoured."""
    tool_fn = _get_tool_fn()
    instances = _make_aws_instances(350, vcpu=4, memory_gb=16.0)
    mock_pvdr = MagicMock()
    mock_pvdr.list_instance_types = AsyncMock(return_value=instances)
    ctx = _make_ctx({"aws": mock_pvdr})

    result = await tool_fn(
        ctx, provider="aws", region="us-east-1", min_vcpu=4, max_results=300
    )

    assert result["count"] == 300
    assert result["truncated"] is True
    assert result["total_found"] == 350


async def test_explicit_max_results_100_bumped_to_200():
    """max_results=100 with spec filter is bumped to 200; 150 instances all fit."""
    tool_fn = _get_tool_fn()
    instances = _make_aws_instances(150, vcpu=4, memory_gb=16.0)
    mock_pvdr = MagicMock()
    mock_pvdr.list_instance_types = AsyncMock(return_value=instances)
    ctx = _make_ctx({"aws": mock_pvdr})

    result = await tool_fn(
        ctx, provider="aws", region="us-east-1", min_vcpu=4, max_results=100
    )

    # Bumped to 200; all 150 fit
    assert result["count"] == 150
    assert result["truncated"] is False


# ------------------------------------------------------------------
# unfiltered call keeps its own max_results (no bump)
# ------------------------------------------------------------------

async def test_unfiltered_call_keeps_max_results():
    """Without spec filters, max_results is not bumped to 200."""
    tool_fn = _get_tool_fn()
    instances = _make_aws_instances(80, vcpu=2, memory_gb=8.0)
    mock_pvdr = MagicMock()
    mock_pvdr.list_instance_types = AsyncMock(return_value=instances)
    ctx = _make_ctx({"aws": mock_pvdr})

    result = await tool_fn(
        ctx, provider="aws", region="us-east-1", max_results=50
    )

    assert result["count"] == 50
    assert result["truncated"] is True
    assert result["total_found"] == 80


# ------------------------------------------------------------------
# no truncation when total_found <= effective_max
# ------------------------------------------------------------------

async def test_no_truncation_when_results_fit_within_effective_max():
    """30 spec-filtered results fit within effective_max=200; no truncation."""
    tool_fn = _get_tool_fn()
    instances = _make_aws_instances(30, vcpu=4, memory_gb=16.0)
    mock_pvdr = MagicMock()
    mock_pvdr.list_instance_types = AsyncMock(return_value=instances)
    ctx = _make_ctx({"aws": mock_pvdr})

    result = await tool_fn(
        ctx, provider="aws", region="us-east-1", min_vcpu=4, max_results=20
    )

    assert result["count"] == 30
    assert result["truncated"] is False
    assert result["total_found"] == 30
    # No truncation message in next_steps
    spec_msg = next(
        (s for s in result["next_steps"] if "Spec filters applied" in s), None
    )
    assert spec_msg is None
