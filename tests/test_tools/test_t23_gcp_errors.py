"""Tests for T23: explicit Phase 4 error messages for GCP effective pricing tools."""
from __future__ import annotations

from decimal import Decimal
from typing import Any
from unittest.mock import AsyncMock, MagicMock

import pytest

from opencloudcosts.models import (
    CloudProvider,
    EffectivePrice,
    NormalizedPrice,
    PriceUnit,
    PricingTerm,
)
from opencloudcosts.tools.lookup import register_lookup_tools


# ---------------------------------------------------------------------------
# Helpers to capture tool functions registered via @mcp.tool()
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
    """Build a minimal mock Context with the given providers mapping."""
    ctx = MagicMock()
    ctx.request_context.lifespan_context = {"providers": providers}
    return ctx


@pytest.fixture
def lookup_tools():
    capture = _ToolCapture()
    register_lookup_tools(capture)
    return capture


# ---------------------------------------------------------------------------
# get_effective_price — GCP guard
# ---------------------------------------------------------------------------

async def test_get_effective_price_gcp_returns_helpful_error(lookup_tools):
    gcp_mock = MagicMock()
    ctx = _make_ctx({"gcp": gcp_mock})

    result = await lookup_tools["get_effective_price"](
        ctx,
        provider="gcp",
        service="compute",
        instance_type="n2-standard-4",
        region="us-central1",
    )

    assert "error" in result
    assert "Phase 4" in result["error"]
    assert "cud_1yr" in result["error"]
    gcp_mock.get_effective_price.assert_not_called()


async def test_get_effective_price_gcp_not_calls_provider(lookup_tools):
    """Regression: provider method must never be invoked for GCP."""
    gcp_mock = MagicMock()
    ctx = _make_ctx({"gcp": gcp_mock})

    await lookup_tools["get_effective_price"](
        ctx,
        provider="gcp",
        service="storage",
        instance_type="pd-ssd",
        region="europe-west1",
    )

    gcp_mock.get_effective_price.assert_not_called()


# ---------------------------------------------------------------------------
# get_discount_summary — GCP guard
# ---------------------------------------------------------------------------

async def test_get_discount_summary_gcp_returns_helpful_error(lookup_tools):
    gcp_mock = MagicMock()
    ctx = _make_ctx({"gcp": gcp_mock})

    result = await lookup_tools["get_discount_summary"](ctx, provider="gcp")

    assert "error" in result
    assert "Phase 4" in result["error"]
    assert "cud_1yr" in result["error"]
    gcp_mock.get_discount_summary.assert_not_called()


# ---------------------------------------------------------------------------
# AWS path is unchanged (no regression)
# ---------------------------------------------------------------------------

async def test_get_effective_price_aws_not_intercepted(lookup_tools):
    """AWS must still reach the provider (mock returns a value, not an early error)."""
    base_price = NormalizedPrice(
        provider=CloudProvider.AWS,
        service="compute",
        sku_id="SKU1",
        product_family="Compute Instance",
        description="m5.xlarge",
        region="us-east-1",
        attributes={},
        pricing_term=PricingTerm.ON_DEMAND,
        price_per_unit=Decimal("0.192"),
        unit=PriceUnit.PER_HOUR,
    )
    ep = EffectivePrice(
        base_price=base_price,
        effective_price_per_unit=Decimal("0.128"),
        discount_type="Blended",
        discount_pct=33.3,
        commitment_term=None,
        source="cost_explorer",
    )

    aws_mock = MagicMock()
    aws_mock.get_effective_price = AsyncMock(return_value=[ep])
    ctx = _make_ctx({"aws": aws_mock})

    result = await lookup_tools["get_effective_price"](
        ctx,
        provider="aws",
        service="compute",
        instance_type="m5.xlarge",
        region="us-east-1",
    )

    assert "error" not in result
    assert result["provider"] == "aws"
    aws_mock.get_effective_price.assert_called_once()
