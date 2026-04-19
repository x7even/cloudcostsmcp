"""Extended tests for estimate_bom and estimate_unit_economics (v0.8.0)."""
from __future__ import annotations

from decimal import Decimal
from typing import Any
from unittest.mock import AsyncMock, MagicMock

import pytest

from opencloudcosts.models import (
    CloudProvider,
    NormalizedPrice,
    PriceUnit,
    PricingResult,
    PricingTerm,
)
from opencloudcosts.tools.bom import register_bom_tools


def _make_compute_price(
    region: str = "us-east-1",
    price: str = "0.192",
    provider: CloudProvider = CloudProvider.AWS,
    instance_type: str = "m5.xlarge",
) -> NormalizedPrice:
    return NormalizedPrice(
        provider=provider,
        service="compute",
        sku_id="TESTSKU",
        product_family="Compute Instance",
        description=f"{instance_type} Linux",
        region=region,
        attributes={"instanceType": instance_type, "vcpu": "4", "memory": "16 GiB"},
        pricing_term=PricingTerm.ON_DEMAND,
        price_per_unit=Decimal(price),
        unit=PriceUnit.PER_HOUR,
    )


def _make_rds_price(
    region: str = "us-east-1",
    price: str = "0.240",
    instance_type: str = "db.t4g.micro",
) -> NormalizedPrice:
    return NormalizedPrice(
        provider=CloudProvider.AWS,
        service="rds",
        sku_id="RDS-TESTSKU",
        product_family="Database Instance",
        description=f"{instance_type} MySQL Single-AZ",
        region=region,
        attributes={
            "instanceType": instance_type,
            "databaseEngine": "MySQL",
            "deploymentOption": "Single-AZ",
        },
        pricing_term=PricingTerm.ON_DEMAND,
        price_per_unit=Decimal(price),
        unit=PriceUnit.PER_HOUR,
    )


def _make_storage_price(region: str = "us-east-1", price: str = "0.10") -> NormalizedPrice:
    return NormalizedPrice(
        provider=CloudProvider.AWS,
        service="storage",
        sku_id="STORAGETESKU",
        product_family="Storage",
        description="gp3 EBS",
        region=region,
        attributes={"volumeType": "gp3"},
        pricing_term=PricingTerm.ON_DEMAND,
        price_per_unit=Decimal(price),
        unit=PriceUnit.PER_GB_MONTH,
    )


class _ToolCapture:
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


def _make_provider(price: NormalizedPrice) -> MagicMock:
    pvdr = MagicMock()
    pvdr.supports = MagicMock(return_value=True)
    pvdr.get_price = AsyncMock(return_value=PricingResult(public_prices=[price]))
    return pvdr


@pytest.fixture
def bom_tools():
    capture = _ToolCapture()
    register_bom_tools(capture)
    return capture


# ---------------------------------------------------------------------------
# test_estimate_bom_database_aws
# ---------------------------------------------------------------------------

async def test_estimate_bom_database_aws(bom_tools):
    """AWS database item should call get_price and return a valid line item."""
    rds_price = _make_rds_price(instance_type="db.t4g.micro")
    aws_mock = _make_provider(rds_price)
    ctx = _make_ctx({"aws": aws_mock})

    items = [
        {
            "provider": "aws",
            "domain": "database",
            "resource_type": "db.t4g.micro",
            "region": "us-east-1",
        }
    ]
    result = await bom_tools["estimate_bom"](ctx, items)

    assert "error" not in result
    assert result["errors"] is None
    assert len(result["line_items"]) == 1
    assert "db.t4g.micro" in result["line_items"][0]["description"]


# ---------------------------------------------------------------------------
# test_estimate_bom_database_gcp_graceful_error (unsupported domain)
# ---------------------------------------------------------------------------

async def test_estimate_bom_database_gcp_graceful_error(bom_tools):
    """Provider that returns supports()=False should produce a structured error."""
    gcp_mock = MagicMock()
    gcp_mock.supports = MagicMock(return_value=False)
    ctx = _make_ctx({"gcp": gcp_mock})

    items = [
        {
            "provider": "gcp",
            "domain": "database",
            "resource_type": "db-n1-standard-2",
            "region": "us-central1",
        }
    ]
    result = await bom_tools["estimate_bom"](ctx, items)

    assert "error" in result or (result.get("errors") and len(result["errors"]) > 0)


# ---------------------------------------------------------------------------
# test_estimate_bom_mixed_compute_storage_database
# ---------------------------------------------------------------------------

async def test_estimate_bom_mixed_compute_storage_database(bom_tools):
    """A BoM with compute, storage, and database items should produce 3 line items."""
    compute_price = _make_compute_price()
    storage_price = _make_storage_price()
    rds_price = _make_rds_price()

    call_count = 0
    prices_seq = [compute_price, storage_price, rds_price]

    async def get_price_side_effect(spec):
        nonlocal call_count
        p = prices_seq[call_count]
        call_count += 1
        return PricingResult(public_prices=[p])

    aws_mock = MagicMock()
    aws_mock.supports = MagicMock(return_value=True)
    aws_mock.get_price = AsyncMock(side_effect=get_price_side_effect)
    ctx = _make_ctx({"aws": aws_mock})

    items = [
        {
            "provider": "aws",
            "domain": "compute",
            "resource_type": "m5.xlarge",
            "region": "us-east-1",
        },
        {
            "provider": "aws",
            "domain": "storage",
            "storage_type": "gp3",
            "size_gb": 100,
            "region": "us-east-1",
        },
        {
            "provider": "aws",
            "domain": "database",
            "resource_type": "db.t4g.micro",
            "region": "us-east-1",
        },
    ]
    result = await bom_tools["estimate_bom"](ctx, items)

    assert "error" not in result
    assert len(result["line_items"]) == 3


# ---------------------------------------------------------------------------
# test_estimate_unit_economics_basic
# ---------------------------------------------------------------------------

async def test_estimate_unit_economics_basic(bom_tools):
    """estimate_unit_economics should return cost_per_unit, infrastructure_monthly, volume."""
    compute_price = _make_compute_price(instance_type="t3.micro", price="0.0104")

    aws_mock = _make_provider(compute_price)
    ctx = _make_ctx({"aws": aws_mock})

    items = [
        {
            "provider": "aws",
            "domain": "compute",
            "resource_type": "t3.micro",
            "region": "us-east-1",
        }
    ]
    result = await bom_tools["estimate_unit_economics"](
        ctx, items, units_per_month=10000.0, unit_label="user"
    )

    assert "error" not in result
    assert "cost_per_unit" in result
    assert "infrastructure_monthly" in result
    assert "volume" in result
    assert result["volume"] == "10,000 users/month"
