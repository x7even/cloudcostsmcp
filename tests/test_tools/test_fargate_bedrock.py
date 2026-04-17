"""Tests for get_fargate_price and get_bedrock_price tools."""
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


def _make_price(
    price: str,
    unit: PriceUnit = PriceUnit.PER_HOUR,
    description: str = "test",
) -> NormalizedPrice:
    return NormalizedPrice(
        provider=CloudProvider.AWS,
        service="test",
        sku_id="TESTSKU",
        product_family="Test",
        description=description,
        region="us-east-1",
        attributes={},
        pricing_term=PricingTerm.ON_DEMAND,
        price_per_unit=Decimal(price),
        unit=unit,
    )


@pytest.fixture
def lookup_tools():
    capture = _ToolCapture()
    register_lookup_tools(capture)
    return capture


# ---------------------------------------------------------------------------
# get_fargate_price tests
# ---------------------------------------------------------------------------


async def test_get_fargate_price_basic(lookup_tools):
    """Fargate: correct hourly and monthly totals from two rate fetches."""
    vcpu_price = _make_price("0.04048")
    mem_price = _make_price("0.004445")

    pvdr = MagicMock()
    pvdr.get_service_price = AsyncMock(
        side_effect=[
            [vcpu_price],  # first call: vCPU rate
            [mem_price],   # second call: memory rate
        ]
    )

    ctx = _make_ctx({"aws": pvdr})
    fn = lookup_tools["get_fargate_price"]

    result = await fn(ctx, region="us-east-1", vcpu=1.0, memory_gb=2.0)

    assert result.get("provider") == "aws"
    assert result.get("region") == "us-east-1"
    assert result.get("vcpu") == 1.0
    assert result.get("memory_gb") == 2.0
    assert "vcpu_rate_per_hour" in result
    assert "memory_rate_per_gb_hour" in result
    assert "hourly_total" in result
    assert "monthly_total" in result
    assert "formula" in result

    # hourly = 1.0 * 0.04048 + 2.0 * 0.004445 = 0.04048 + 0.00889 = 0.04937
    hourly = Decimal("1.0") * Decimal("0.04048") + Decimal("2.0") * Decimal("0.004445")
    expected_hourly = f"${hourly:.6f}"
    assert result["hourly_total"] == expected_hourly

    # monthly = hourly * 730
    monthly = hourly * Decimal("730")
    expected_monthly = f"${monthly:.2f}"
    assert result["monthly_total"] == expected_monthly


async def test_get_fargate_price_custom_hours(lookup_tools):
    """Fargate: hours_per_month is correctly applied."""
    vcpu_price = _make_price("0.04048")
    mem_price = _make_price("0.004445")

    pvdr = MagicMock()
    pvdr.get_service_price = AsyncMock(
        side_effect=[[vcpu_price], [mem_price]]
    )

    ctx = _make_ctx({"aws": pvdr})
    fn = lookup_tools["get_fargate_price"]

    result = await fn(ctx, region="us-east-1", vcpu=2.0, memory_gb=4.0, hours_per_month=100.0)

    hourly = Decimal("2.0") * Decimal("0.04048") + Decimal("4.0") * Decimal("0.004445")
    monthly = hourly * Decimal("100.0")
    assert result["monthly_total"] == f"${monthly:.2f}"


async def test_get_fargate_price_windows_passes_os_filter(lookup_tools):
    """Fargate: Windows OS adds operatingSystem filter to vCPU request."""
    vcpu_price = _make_price("0.09098")
    mem_price = _make_price("0.009997")

    pvdr = MagicMock()
    pvdr.get_service_price = AsyncMock(
        side_effect=[[vcpu_price], [mem_price]]
    )

    ctx = _make_ctx({"aws": pvdr})
    fn = lookup_tools["get_fargate_price"]

    result = await fn(ctx, region="us-east-1", vcpu=1.0, memory_gb=2.0, os="Windows")

    assert result.get("os") == "Windows"
    # Verify first call (vCPU) included operatingSystem filter
    first_call_args = pvdr.get_service_price.call_args_list[0]
    filters_arg = first_call_args[0][2]  # positional: service, region, filters
    assert filters_arg.get("operatingSystem") == "Windows"


async def test_get_fargate_price_no_vcpu_results(lookup_tools):
    """Fargate: returns no_prices_found when vCPU lookup is empty."""
    pvdr = MagicMock()
    pvdr.get_service_price = AsyncMock(side_effect=[[], [_make_price("0.004445")]])

    ctx = _make_ctx({"aws": pvdr})
    fn = lookup_tools["get_fargate_price"]

    result = await fn(ctx, region="ap-southeast-99", vcpu=1.0, memory_gb=2.0)

    assert result.get("result") == "no_prices_found"
    assert "vCPU" in result["message"]


async def test_get_fargate_price_no_mem_results(lookup_tools):
    """Fargate: returns no_prices_found when memory lookup is empty."""
    pvdr = MagicMock()
    pvdr.get_service_price = AsyncMock(side_effect=[[_make_price("0.04048")], []])

    ctx = _make_ctx({"aws": pvdr})
    fn = lookup_tools["get_fargate_price"]

    result = await fn(ctx, region="us-east-1", vcpu=1.0, memory_gb=2.0)

    assert result.get("result") == "no_prices_found"
    assert "memory" in result["message"]


async def test_get_fargate_price_no_aws_provider(lookup_tools):
    """Fargate: returns error when AWS provider is not configured."""
    ctx = _make_ctx({})
    fn = lookup_tools["get_fargate_price"]

    result = await fn(ctx, region="us-east-1", vcpu=1.0, memory_gb=2.0)

    assert "error" in result
    assert "AWS" in result["error"]


# ---------------------------------------------------------------------------
# get_bedrock_price tests
# ---------------------------------------------------------------------------


async def test_get_bedrock_price_basic(lookup_tools):
    """Bedrock: correct monthly costs from input and output token rates."""
    input_price = _make_price("0.003000", unit=PriceUnit.PER_UNIT)
    output_price = _make_price("0.015000", unit=PriceUnit.PER_UNIT)

    pvdr = MagicMock()
    pvdr.get_service_price = AsyncMock(
        side_effect=[
            [input_price],   # first call: input token rate
            [output_price],  # second call: output token rate
        ]
    )

    ctx = _make_ctx({"aws": pvdr})
    fn = lookup_tools["get_bedrock_price"]

    result = await fn(
        ctx, model="claude-3-5-sonnet", region="us-east-1",
        input_tokens=1_000_000, output_tokens=1_000_000
    )

    assert result.get("model") == "Claude 3.5 Sonnet"
    assert result.get("region") == "us-east-1"
    assert result.get("mode") == "on_demand"
    assert result.get("input_rate_per_1k_tokens") == "$0.003000"
    assert result.get("output_rate_per_1k_tokens") == "$0.015000"
    assert result.get("input_tokens") == 1_000_000
    assert result.get("output_tokens") == 1_000_000

    # input_cost = 0.003 * 1_000_000 / 1000 = $3.0000
    assert result.get("monthly_input_cost") == "$3.0000"
    # output_cost = 0.015 * 1_000_000 / 1000 = $15.0000
    assert result.get("monthly_output_cost") == "$15.0000"
    # total = $18.0000
    assert result.get("monthly_total_cost") == "$18.0000"
    assert "note" in result


async def test_get_bedrock_price_alias_resolution(lookup_tools):
    """Bedrock: slug aliases are resolved to AWS catalog names."""
    pvdr = MagicMock()
    pvdr.get_service_price = AsyncMock(
        side_effect=[
            [_make_price("0.001000", unit=PriceUnit.PER_UNIT)],
            [_make_price("0.003000", unit=PriceUnit.PER_UNIT)],
        ]
    )

    ctx = _make_ctx({"aws": pvdr})
    fn = lookup_tools["get_bedrock_price"]

    result = await fn(ctx, model="nova-pro", region="us-east-1")

    assert result.get("model") == "Nova Pro"


async def test_get_bedrock_price_batch_mode(lookup_tools):
    """Bedrock: batch mode uses batch inferenceType filters."""
    pvdr = MagicMock()
    pvdr.get_service_price = AsyncMock(
        side_effect=[
            [_make_price("0.0015", unit=PriceUnit.PER_UNIT)],
            [_make_price("0.0075", unit=PriceUnit.PER_UNIT)],
        ]
    )

    ctx = _make_ctx({"aws": pvdr})
    fn = lookup_tools["get_bedrock_price"]

    result = await fn(ctx, model="claude-3-5-sonnet", region="us-east-1", mode="batch")

    assert result.get("mode") == "batch"

    # Verify batch inferenceType filters were used
    first_call_filters = pvdr.get_service_price.call_args_list[0][0][2]
    assert first_call_filters.get("inferenceType") == "input tokens batch"
    second_call_filters = pvdr.get_service_price.call_args_list[1][0][2]
    assert second_call_filters.get("inferenceType") == "output tokens batch"


async def test_get_bedrock_price_full_catalog_name_bypasses_alias(lookup_tools):
    """Bedrock: passing a full catalog name (not an alias) is used as-is."""
    pvdr = MagicMock()
    pvdr.get_service_price = AsyncMock(
        side_effect=[
            [_make_price("0.002", unit=PriceUnit.PER_UNIT)],
            [_make_price("0.008", unit=PriceUnit.PER_UNIT)],
        ]
    )

    ctx = _make_ctx({"aws": pvdr})
    fn = lookup_tools["get_bedrock_price"]

    result = await fn(ctx, model="Claude 3.7 Sonnet", region="us-east-1")

    assert result.get("model") == "Claude 3.7 Sonnet"
    first_call_filters = pvdr.get_service_price.call_args_list[0][0][2]
    assert first_call_filters.get("model") == "Claude 3.7 Sonnet"


async def test_get_bedrock_price_no_input_results(lookup_tools):
    """Bedrock: returns no_prices_found when input token lookup is empty."""
    pvdr = MagicMock()
    pvdr.get_service_price = AsyncMock(
        side_effect=[[], [_make_price("0.015", unit=PriceUnit.PER_UNIT)]]
    )

    ctx = _make_ctx({"aws": pvdr})
    fn = lookup_tools["get_bedrock_price"]

    result = await fn(ctx, model="claude-3-5-sonnet", region="eu-west-1")

    assert result.get("result") == "no_prices_found"
    assert "input" in result["message"].lower() or "no" in result["message"].lower()


async def test_get_bedrock_price_no_aws_provider(lookup_tools):
    """Bedrock: returns error when AWS provider is not configured."""
    ctx = _make_ctx({})
    fn = lookup_tools["get_bedrock_price"]

    result = await fn(ctx, model="claude-3-5-sonnet")

    assert "error" in result
    assert "AWS" in result["error"]


async def test_get_bedrock_price_token_volume_scaling(lookup_tools):
    """Bedrock: different token volumes correctly scale costs."""
    input_price = _make_price("0.003000", unit=PriceUnit.PER_UNIT)
    output_price = _make_price("0.015000", unit=PriceUnit.PER_UNIT)

    pvdr = MagicMock()
    pvdr.get_service_price = AsyncMock(
        side_effect=[[input_price], [output_price]]
    )

    ctx = _make_ctx({"aws": pvdr})
    fn = lookup_tools["get_bedrock_price"]

    # 500k input + 100k output
    result = await fn(
        ctx, model="claude-3-5-sonnet", region="us-east-1",
        input_tokens=500_000, output_tokens=100_000
    )

    # input_cost = 0.003 * 500_000 / 1000 = $1.5000
    assert result.get("monthly_input_cost") == "$1.5000"
    # output_cost = 0.015 * 100_000 / 1000 = $1.5000
    assert result.get("monthly_output_cost") == "$1.5000"
    # total = $3.0000
    assert result.get("monthly_total_cost") == "$3.0000"
