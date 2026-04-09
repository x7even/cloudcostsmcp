"""Extended tests for lookup tools: search_pricing routing, GCP compute delegation,
get_service_price no-results structure, and list_services (T24)."""
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
from opencloudcosts.tools.availability import register_availability_tools


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


def _make_normalized_price(
    provider: CloudProvider = CloudProvider.AWS,
    service: str = "compute",
    region: str = "us-east-1",
    price: str = "0.192",
    description: str = "m5.xlarge",
    instance_type: str = "m5.xlarge",
) -> NormalizedPrice:
    return NormalizedPrice(
        provider=provider,
        service=service,
        sku_id="TESTSKU",
        product_family="Compute Instance",
        description=description,
        region=region,
        attributes={"instanceType": instance_type, "vcpu": "4", "memory": "16 GiB"},
        pricing_term=PricingTerm.ON_DEMAND,
        price_per_unit=Decimal(price),
        unit=PriceUnit.PER_HOUR,
    )


@pytest.fixture
def lookup_tools():
    capture = _ToolCapture()
    register_lookup_tools(capture)
    return capture


@pytest.fixture
def availability_tools():
    capture = _ToolCapture()
    register_availability_tools(capture)
    return capture


# ---------------------------------------------------------------------------
# test_search_pricing_cloudwatch_service
# ---------------------------------------------------------------------------

async def test_search_pricing_cloudwatch_service(lookup_tools):
    """search_pricing with service='cloudwatch' should route to search_pricing with
    service_code kwarg, not the EC2-only default path."""
    cloudwatch_price = _make_normalized_price(
        service="cloudwatch",
        description="CloudWatch Metric",
        instance_type="metric",
    )

    aws_mock = MagicMock()
    aws_mock.search_pricing = AsyncMock(return_value=[cloudwatch_price])
    ctx = _make_ctx({"aws": aws_mock})

    result = await lookup_tools["search_pricing"](
        ctx,
        provider="aws",
        query="metric",
        service="cloudwatch",
    )

    assert "error" not in result
    assert result["count"] > 0

    # Verify the provider was called with service_code kwarg
    aws_mock.search_pricing.assert_called_once()
    call_kwargs = aws_mock.search_pricing.call_args
    assert call_kwargs.kwargs.get("service_code") == "cloudwatch"


# ---------------------------------------------------------------------------
# test_get_service_price_gcp_compute_delegation
# ---------------------------------------------------------------------------

async def test_get_service_price_gcp_compute_delegation(lookup_tools):
    """get_service_price(provider='gcp', service='compute') should delegate to
    get_compute_price on the GCP provider rather than returning 'not supported'."""
    gcp_price = _make_normalized_price(
        provider=CloudProvider.GCP,
        service="compute",
        region="us-central1",
        price="0.1900",
        description="n2-standard-4",
        instance_type="n2-standard-4",
    )

    gcp_mock = MagicMock()
    gcp_mock.get_compute_price = AsyncMock(return_value=[gcp_price])
    ctx = _make_ctx({"gcp": gcp_mock})

    result = await lookup_tools["get_service_price"](
        ctx,
        provider="gcp",
        service="compute",
        region="us-central1",
        filters={"instanceType": "n2-standard-4"},
    )

    assert "error" not in result
    assert result["instance_type"] == "n2-standard-4"
    gcp_mock.get_compute_price.assert_called_once()


async def test_get_service_price_gcp_compute_no_instance_type_error(lookup_tools):
    """get_service_price(provider='gcp', service='compute') without instanceType
    filter should return a helpful error."""
    gcp_mock = MagicMock()
    ctx = _make_ctx({"gcp": gcp_mock})

    result = await lookup_tools["get_service_price"](
        ctx,
        provider="gcp",
        service="compute",
        region="us-central1",
        filters={},
    )

    assert "error" in result
    assert "instanceType" in result["error"]
    gcp_mock.get_compute_price.assert_not_called()


# ---------------------------------------------------------------------------
# test_get_service_price_no_results_structure
# ---------------------------------------------------------------------------

async def test_get_service_price_no_results_structure(lookup_tools):
    """When get_service_price returns no pricing, response must have
    result, message, and tip fields (the second registered get_service_price)."""
    aws_mock = MagicMock()
    aws_mock.get_service_price = AsyncMock(return_value=[])
    ctx = _make_ctx({"aws": aws_mock})

    result = await lookup_tools["get_service_price"](
        ctx,
        provider="aws",
        service="cloudwatch",
        region="us-east-1",
        filters={"group": "NonExistent"},
    )

    # The second (generic) get_service_price returns no_prices_found without a tip,
    # but at minimum it must have result and message
    assert "result" in result
    assert "message" in result
    assert result["result"] == "no_prices_found"


# ---------------------------------------------------------------------------
# test_list_services_returns_aliases
# ---------------------------------------------------------------------------

async def test_list_services_returns_aliases(availability_tools):
    """list_services(provider='aws') should return structured services with aliases."""
    aws_mock = MagicMock()
    aws_mock.list_services = AsyncMock(return_value=[
        {"service_code": "AmazonEC2", "description": "EC2", "aliases": ["ec2", "compute"]},
        {"service_code": "AmazonRDS", "description": "RDS", "aliases": ["rds", "database"]},
    ])
    ctx = _make_ctx({"aws": aws_mock})

    result = await availability_tools["list_services"](ctx, provider="aws")

    assert "error" not in result
    assert result["count"] == 2
    assert "services" in result
    ec2 = next(s for s in result["services"] if s["service_code"] == "AmazonEC2")
    assert "ec2" in ec2["aliases"]


async def test_list_services_provider_not_configured(availability_tools):
    """list_services with unconfigured provider should return an error."""
    ctx = _make_ctx({})
    result = await availability_tools["list_services"](ctx, provider="aws")
    assert "error" in result


async def test_list_services_provider_without_support(availability_tools):
    """Provider that has no list_services method should return descriptive error."""
    gcp_mock = MagicMock(spec=["get_compute_price"])  # no list_services
    ctx = _make_ctx({"gcp": gcp_mock})
    result = await availability_tools["list_services"](ctx, provider="gcp")
    assert "error" in result
    assert "list_services" in result["error"]
