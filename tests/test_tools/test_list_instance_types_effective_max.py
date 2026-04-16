"""Tests for list_instance_types effective_max / spec-filter cap behaviour."""
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
    """Return n fake AWS InstanceTypeInfo objects all meeting vcpu/memory_gb."""
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
# effective_max bumped to 100 for spec-filtered queries
# ------------------------------------------------------------------

async def test_min_vcpu_bumps_effective_max_to_100():
    """When min_vcpu is set and max_results < 100, effective_max should be 100."""
    tool_fn = _get_tool_fn()
    # 120 instances all meeting vcpu >= 4 — more than the default max_results=50
    # but also more than 100, so we expect truncation at 100.
    instances = _make_aws_instances(120, vcpu=4, memory_gb=16.0)
    mock_pvdr = MagicMock()
    mock_pvdr.list_instance_types = AsyncMock(return_value=instances)
    ctx = _make_ctx({"aws": mock_pvdr})

    result = await tool_fn(
        ctx, provider="aws", region="us-east-1", min_vcpu=4, max_results=20
    )

    # Should have returned 100, not 20
    assert result["count"] == 100
    assert result["truncated"] is True
    # The truncation message should cite 100, not 20
    truncation_msg = next(
        (s for s in result["next_steps"] if "Result truncated" in s), None
    )
    assert truncation_msg is not None
    assert "returned 100 of 120+" in truncation_msg


async def test_min_memory_gb_bumps_effective_max_to_100():
    """When min_memory_gb is set and max_results < 100, effective_max should be 100."""
    tool_fn = _get_tool_fn()
    instances = _make_aws_instances(110, vcpu=8, memory_gb=64.0)
    mock_pvdr = MagicMock()
    mock_pvdr.list_instance_types = AsyncMock(return_value=instances)
    ctx = _make_ctx({"aws": mock_pvdr})

    result = await tool_fn(
        ctx, provider="aws", region="us-east-1", min_memory_gb=16.0, max_results=50
    )

    assert result["count"] == 100
    assert result["truncated"] is True
    truncation_msg = next(
        (s for s in result["next_steps"] if "Result truncated" in s), None
    )
    assert truncation_msg is not None
    assert "returned 100 of 110+" in truncation_msg


async def test_both_spec_filters_bump_effective_max():
    """Both min_vcpu and min_memory_gb together should also bump effective_max."""
    tool_fn = _get_tool_fn()
    instances = _make_aws_instances(105, vcpu=4, memory_gb=32.0)
    mock_pvdr = MagicMock()
    mock_pvdr.list_instance_types = AsyncMock(return_value=instances)
    ctx = _make_ctx({"aws": mock_pvdr})

    result = await tool_fn(
        ctx, provider="aws", region="us-east-1",
        min_vcpu=4, min_memory_gb=16.0, max_results=20
    )

    assert result["count"] == 100
    assert result["truncated"] is True


# ------------------------------------------------------------------
# explicit max_results >= 100 is always honoured
# ------------------------------------------------------------------

async def test_explicit_max_results_200_is_honoured():
    """Callers who explicitly pass max_results=200 should get up to 200 results."""
    tool_fn = _get_tool_fn()
    instances = _make_aws_instances(250, vcpu=4, memory_gb=16.0)
    mock_pvdr = MagicMock()
    mock_pvdr.list_instance_types = AsyncMock(return_value=instances)
    ctx = _make_ctx({"aws": mock_pvdr})

    result = await tool_fn(
        ctx, provider="aws", region="us-east-1", min_vcpu=4, max_results=200
    )

    assert result["count"] == 200
    assert result["truncated"] is True
    truncation_msg = next(
        (s for s in result["next_steps"] if "Result truncated" in s), None
    )
    assert truncation_msg is not None
    assert "returned 200 of 250+" in truncation_msg


async def test_explicit_max_results_100_is_honoured():
    """max_results=100 should not be further bumped (already at the floor)."""
    tool_fn = _get_tool_fn()
    instances = _make_aws_instances(150, vcpu=4, memory_gb=16.0)
    mock_pvdr = MagicMock()
    mock_pvdr.list_instance_types = AsyncMock(return_value=instances)
    ctx = _make_ctx({"aws": mock_pvdr})

    result = await tool_fn(
        ctx, provider="aws", region="us-east-1", min_vcpu=4, max_results=100
    )

    assert result["count"] == 100
    assert result["truncated"] is True


# ------------------------------------------------------------------
# unfiltered call keeps its own max_results (no bump)
# ------------------------------------------------------------------

async def test_unfiltered_call_keeps_max_results():
    """Without spec filters, max_results should not be bumped to 100."""
    tool_fn = _get_tool_fn()
    # 80 instances — more than default max_results=50, less than 100
    instances = _make_aws_instances(80, vcpu=2, memory_gb=8.0)
    mock_pvdr = MagicMock()
    mock_pvdr.list_instance_types = AsyncMock(return_value=instances)
    ctx = _make_ctx({"aws": mock_pvdr})

    result = await tool_fn(
        ctx, provider="aws", region="us-east-1", max_results=50
    )

    # No spec filters → effective_max stays at 50
    assert result["count"] == 50
    assert result["truncated"] is True


# ------------------------------------------------------------------
# no truncation when total_found <= effective_max
# ------------------------------------------------------------------

async def test_no_truncation_when_results_fit_within_effective_max():
    """When spec-filtered results fit within effective_max, truncated should be False."""
    tool_fn = _get_tool_fn()
    instances = _make_aws_instances(30, vcpu=4, memory_gb=16.0)
    mock_pvdr = MagicMock()
    mock_pvdr.list_instance_types = AsyncMock(return_value=instances)
    ctx = _make_ctx({"aws": mock_pvdr})

    result = await tool_fn(
        ctx, provider="aws", region="us-east-1", min_vcpu=4, max_results=20
    )

    # 30 results fit within effective_max=100
    assert result["count"] == 30
    assert result["truncated"] is False
    # No truncation message in next_steps
    truncation_msg = next(
        (s for s in result["next_steps"] if "Result truncated" in s), None
    )
    assert truncation_msg is None
