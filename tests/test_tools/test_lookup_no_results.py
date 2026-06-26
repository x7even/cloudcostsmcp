"""Tests for no-results handling in search_pricing tool."""

from __future__ import annotations

from decimal import Decimal
from typing import Any
from unittest.mock import AsyncMock, MagicMock

import pytest

from opencloudcosts.models import (
    CloudProvider,
    NormalizedPrice,
    PriceUnit,
    PricingTerm,
)
from opencloudcosts.tools.lookup import register_lookup_tools


# ---------------------------------------------------------------------------
# Helpers — mirror the pattern used in test_cache_tools.py
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


def _make_provider(search_results=None) -> MagicMock:
    """Return a mock provider whose search_pricing returns *search_results*."""
    pvdr = MagicMock()
    pvdr.search_pricing = AsyncMock(return_value=search_results if search_results is not None else [])
    return pvdr


def _make_price(region: str = "us-east-1") -> NormalizedPrice:
    return NormalizedPrice(
        provider=CloudProvider.AWS,
        service="compute",
        sku_id="TESTSKU",
        product_family="Compute Instance",
        description="m5.xlarge",
        region=region,
        attributes={"instanceType": "m5.xlarge", "vcpu": "4", "memory": "16 GiB"},
        pricing_term=PricingTerm.ON_DEMAND,
        price_per_unit=Decimal("0.192"),
        unit=PriceUnit.PER_HOUR,
    )


@pytest.fixture
def lookup_tools():
    capture = _ToolCapture()
    register_lookup_tools(capture)
    return capture


# ---------------------------------------------------------------------------
# search_pricing — no-results path
# ---------------------------------------------------------------------------


async def test_search_pricing_no_results_structure(lookup_tools):
    """When the provider returns an empty list, search_pricing must return a
    structured no_results dict with required keys and a tip referencing describe_catalog."""
    pvdr = _make_provider(search_results=[])
    ctx = _make_ctx({"aws": pvdr})

    result = await lookup_tools["search_pricing"](
        ctx, provider="aws", query="nonexistent-xyz"
    )

    assert result["result"] == "no_results"
    assert result["provider"] == "aws"
    assert result["query"] == "nonexistent-xyz"
    assert result["region"] == "any"
    assert "message" in result
    assert "nonexistent-xyz" in result["message"]
    assert "tip" in result
    assert "describe_catalog" in result["tip"]


async def test_search_pricing_no_results_region_empty_string_becomes_any(lookup_tools):
    """Empty region string is normalised to 'any' in no_results."""
    pvdr = _make_provider(search_results=[])
    ctx = _make_ctx({"aws": pvdr})

    result = await lookup_tools["search_pricing"](
        ctx, provider="aws", query="foo", region=""
    )

    assert result["result"] == "no_results"
    assert result["region"] == "any"


async def test_search_pricing_no_results_tip_does_not_reference_list_services(lookup_tools):
    """Tip must reference describe_catalog, not the nonexistent list_services tool."""
    pvdr = _make_provider(search_results=[])
    ctx = _make_ctx({"aws": pvdr})

    result = await lookup_tools["search_pricing"](
        ctx, provider="aws", query="ghost-service"
    )

    assert result["result"] == "no_results"
    assert "describe_catalog" in result["tip"]
    assert "list_services" not in result["tip"]


async def test_search_pricing_non_empty_returns_normal_structure(lookup_tools):
    """Non-empty results must NOT return result='no_results'; standard keys present."""
    price = _make_price()
    pvdr = _make_provider(search_results=[price])
    ctx = _make_ctx({"aws": pvdr})

    result = await lookup_tools["search_pricing"](
        ctx, provider="aws", query="m5.xlarge"
    )

    assert result.get("result") != "no_results"
    assert result["count"] == 1
    assert len(result["results"]) == 1
    assert "tip" in result
    assert "describe_catalog" in result["tip"]


async def test_search_pricing_provider_not_configured(lookup_tools):
    """Unknown provider returns an error dict, not no_results."""
    ctx = _make_ctx({})

    result = await lookup_tools["search_pricing"](
        ctx, provider="unknown", query="foo"
    )

    assert "error" in result
    assert "result" not in result
