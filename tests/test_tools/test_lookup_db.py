"""Tests for get_database_price tool (T27)."""
from __future__ import annotations

from decimal import Decimal
from unittest.mock import AsyncMock, MagicMock

import pytest

from opencloudcosts.models import (
    CloudProvider,
    NormalizedPrice,
    PriceUnit,
    PricingTerm,
)


def _make_rds_price(region: str = "us-east-1", price: str = "0.240") -> NormalizedPrice:
    return NormalizedPrice(
        provider=CloudProvider.AWS,
        service="rds",
        sku_id="RDS-TESTSKU",
        product_family="Database Instance",
        description="db.r5.large MySQL Single-AZ",
        region=region,
        attributes={
            "instanceType": "db.r5.large",
            "databaseEngine": "MySQL",
            "deploymentOption": "Single-AZ",
        },
        pricing_term=PricingTerm.ON_DEMAND,
        price_per_unit=Decimal(price),
        unit=PriceUnit.PER_HOUR,
    )


@pytest.fixture
def mock_aws_provider():
    pvdr = MagicMock()
    pvdr.get_service_price = AsyncMock(return_value=[_make_rds_price()])
    return pvdr


@pytest.fixture
def mock_gcp_provider():
    pvdr = MagicMock(spec=[])  # no get_service_price attribute
    return pvdr


def _make_ctx(providers: dict) -> MagicMock:
    ctx = MagicMock()
    ctx.request_context.lifespan_context = {"providers": providers}
    return ctx


def _get_tool_fn():
    from opencloudcosts.server import create_server as create_mcp_server
    mcp = create_mcp_server()
    for tool in mcp._tool_manager.list_tools():
        if tool.name == "get_database_price":
            return mcp._tool_manager._tools[tool.name].fn
    raise RuntimeError("get_database_price tool not found")


# ------------------------------------------------------------------
# Basic MySQL lookup
# ------------------------------------------------------------------

async def test_get_database_price_mysql(mock_aws_provider):
    tool_fn = _get_tool_fn()
    ctx = _make_ctx({"aws": mock_aws_provider})

    result = await tool_fn(ctx, provider="aws", instance_type="db.r5.large", region="us-east-1")

    assert "price_per_hour" in result
    assert result["engine"] == "MySQL"
    assert result["deployment"] == "single-az"
    assert result["provider"] == "aws"
    assert result["instance_type"] == "db.r5.large"
    assert result["region"] == "us-east-1"
    assert "monthly_estimate" in result
    assert result["price_per_hour"] == "$0.240000"


# ------------------------------------------------------------------
# Engine name normalization (case-insensitive)
# ------------------------------------------------------------------

@pytest.mark.parametrize("engine_input,expected", [
    ("mysql", "MySQL"),
    ("MySQL", "MySQL"),
    ("MYSQL", "MySQL"),
    ("postgresql", "PostgreSQL"),
    ("PostgreSQL", "PostgreSQL"),
    ("postgres", "PostgreSQL"),
    ("mariadb", "MariaDB"),
    ("oracle", "Oracle"),
    ("sqlserver", "SQL Server"),
    ("aurora-mysql", "Aurora MySQL"),
    ("Aurora-MySQL", "Aurora MySQL"),
    ("aurora-postgresql", "Aurora PostgreSQL"),
    ("aurora-postgres", "Aurora PostgreSQL"),
    ("Aurora-PostgreSQL", "Aurora PostgreSQL"),
])
async def test_engine_normalization(mock_aws_provider, engine_input, expected):
    tool_fn = _get_tool_fn()
    ctx = _make_ctx({"aws": mock_aws_provider})

    result = await tool_fn(
        ctx, provider="aws", instance_type="db.r5.large",
        region="us-east-1", engine=engine_input
    )

    assert result["engine"] == expected
    # Confirm the normalized engine was passed to get_service_price
    call_kwargs = mock_aws_provider.get_service_price.call_args
    filters_passed = call_kwargs[0][2]  # positional: service, region, filters
    assert filters_passed["databaseEngine"] == expected


# ------------------------------------------------------------------
# Multi-AZ deployment
# ------------------------------------------------------------------

async def test_get_database_price_multi_az(mock_aws_provider):
    tool_fn = _get_tool_fn()
    ctx = _make_ctx({"aws": mock_aws_provider})

    result = await tool_fn(
        ctx, provider="aws", instance_type="db.r5.large",
        region="us-east-1", deployment="multi-az"
    )

    assert result["deployment"] == "multi-az"
    call_kwargs = mock_aws_provider.get_service_price.call_args
    filters_passed = call_kwargs[0][2]
    assert filters_passed["deploymentOption"] == "Multi-AZ"


async def test_get_database_price_single_az_default(mock_aws_provider):
    tool_fn = _get_tool_fn()
    ctx = _make_ctx({"aws": mock_aws_provider})

    result = await tool_fn(
        ctx, provider="aws", instance_type="db.r5.large", region="us-east-1"
    )

    call_kwargs = mock_aws_provider.get_service_price.call_args
    filters_passed = call_kwargs[0][2]
    assert filters_passed["deploymentOption"] == "Single-AZ"


# ------------------------------------------------------------------
# GCP returns Phase 4 error
# ------------------------------------------------------------------

async def test_get_database_price_gcp_phase4_error(mock_gcp_provider):
    tool_fn = _get_tool_fn()
    ctx = _make_ctx({"gcp": mock_gcp_provider})

    result = await tool_fn(
        ctx, provider="gcp", instance_type="db.r5.large", region="us-central1"
    )

    assert "error" in result
    assert "Phase 4" in result["error"]
    assert "get_compute_price" in result["error"]


# ------------------------------------------------------------------
# Unknown provider
# ------------------------------------------------------------------

async def test_get_database_price_unknown_provider():
    tool_fn = _get_tool_fn()
    ctx = _make_ctx({})

    result = await tool_fn(
        ctx, provider="azure", instance_type="db.r5.large", region="eastus"
    )

    assert "error" in result
    assert "azure" in result["error"]


# ------------------------------------------------------------------
# No prices found
# ------------------------------------------------------------------

async def test_get_database_price_no_results():
    tool_fn = _get_tool_fn()
    pvdr = MagicMock()
    pvdr.get_service_price = AsyncMock(return_value=[])
    ctx = _make_ctx({"aws": pvdr})

    result = await tool_fn(
        ctx, provider="aws", instance_type="db.r99.obscure",
        region="us-east-1", engine="MySQL"
    )

    assert result["result"] == "no_prices_found"
    assert "tip" in result
    assert "db.r99.obscure" in result["message"]


# ------------------------------------------------------------------
# Reserved term adds term filters
# ------------------------------------------------------------------

async def test_get_database_price_reserved_1yr(mock_aws_provider):
    tool_fn = _get_tool_fn()
    ctx = _make_ctx({"aws": mock_aws_provider})

    result = await tool_fn(
        ctx, provider="aws", instance_type="db.r5.large",
        region="us-east-1", term="reserved_1yr"
    )

    assert result["term"] == "reserved_1yr"
    call_kwargs = mock_aws_provider.get_service_price.call_args
    filters_passed = call_kwargs[0][2]
    assert filters_passed.get("termType") == "Reserved"
    assert filters_passed.get("leaseContractLength") == "1yr"


async def test_get_database_price_reserved_3yr(mock_aws_provider):
    tool_fn = _get_tool_fn()
    ctx = _make_ctx({"aws": mock_aws_provider})

    await tool_fn(
        ctx, provider="aws", instance_type="db.r5.large",
        region="us-east-1", term="reserved_3yr"
    )

    call_kwargs = mock_aws_provider.get_service_price.call_args
    filters_passed = call_kwargs[0][2]
    assert filters_passed.get("leaseContractLength") == "3yr"
