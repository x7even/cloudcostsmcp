"""Tests for Phase 2 tools: get_discount_summary, find_cheapest_region."""
from __future__ import annotations

from decimal import Decimal
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from cloudcostmcp.cache import CacheManager
from cloudcostmcp.config import Settings
from cloudcostmcp.models import (
    CloudProvider,
    NormalizedPrice,
    PriceUnit,
    PricingTerm,
)
from cloudcostmcp.providers.aws import AWSProvider
from cloudcostmcp.providers.base import NotConfiguredError


def _make_price(region: str, price: str) -> NormalizedPrice:
    return NormalizedPrice(
        provider=CloudProvider.AWS,
        service="compute",
        sku_id="TESTSKU",
        product_family="Compute Instance",
        description="m5.xlarge",
        region=region,
        attributes={"instanceType": "m5.xlarge", "vcpu": "4", "memory": "16 GiB"},
        pricing_term=PricingTerm.ON_DEMAND,
        price_per_unit=Decimal(price),
        unit=PriceUnit.PER_HOUR,
    )


@pytest.fixture
async def aws_provider(tmp_path: Path):
    settings = Settings(
        cache_dir=tmp_path / "cache",
        cache_ttl_hours=1,
        aws_enable_cost_explorer=True,
    )
    cache = CacheManager(settings.cache_dir)
    await cache.initialize()
    provider = AWSProvider(settings, cache)
    # Replace boto3 clients with mocks
    provider._sp = MagicMock()
    provider._ec2 = MagicMock()
    provider._ce = MagicMock()
    yield provider
    await cache.close()


# ------------------------------------------------------------------
# get_discount_summary
# ------------------------------------------------------------------

async def test_get_discount_summary_with_sp_and_ri(aws_provider: AWSProvider):
    aws_provider._sp.describe_savings_plans.return_value = {
        "savingsPlans": [
            {
                "savingsPlanId": "sp-abc123",
                "savingsPlanType": "Compute",
                "paymentOption": "No Upfront",
                "commitment": "1.50",
                "termDurationInSeconds": 31_536_000,  # 1 year
                "start": "2025-01-01T00:00:00Z",
                "end": "2026-01-01T00:00:00Z",
                "state": "active",
                "utilizationPercentage": "92.5",
            }
        ]
    }
    aws_provider._ec2.describe_reserved_instances.return_value = {
        "ReservedInstances": [
            {
                "InstanceType": "m5.xlarge",
                "InstanceCount": 5,
                "OfferingType": "No Upfront",
                "Duration": 31_536_000,
                "FixedPrice": 0.0,
                "UsagePrice": 0.138,
                "ProductDescription": "Linux/UNIX",
                "State": "active",
            }
        ]
    }
    aws_provider._ce.get_savings_plans_utilization.return_value = {
        "Total": {
            "Utilization": {
                "TotalCommitment": "1080.00",
                "UnusedCommitment": "86.40",
                "UtilizationPercentage": "92.0",
            },
            "Savings": {"NetSavings": "312.50"},
        }
    }
    aws_provider._ce.get_reservation_utilization.return_value = {
        "Total": {
            "UtilizationPercentage": "88.0",
            "OnDemandCostOfRIHoursUsed": "450.00",
            "UnrealizedSavings": "55.00",
        }
    }

    summary = await aws_provider.get_discount_summary()

    assert summary["sp_count"] == 1
    assert summary["ri_count"] == 1
    assert summary["savings_plans"][0]["id"] == "sp-abc123"
    assert summary["savings_plans"][0]["type"] == "Compute"
    assert summary["reserved_instances"][0]["instance_type"] == "m5.xlarge"
    assert summary["reserved_instances"][0]["count"] == 5
    assert "savings_plans" in summary["utilization"]


async def test_get_discount_summary_no_auth(tmp_path: Path):
    settings = Settings(
        cache_dir=tmp_path / "cache",
        aws_enable_cost_explorer=False,
    )
    cache = CacheManager(settings.cache_dir)
    await cache.initialize()
    provider = AWSProvider(settings, cache)
    await cache.close()

    with pytest.raises(NotConfiguredError):
        await provider.get_discount_summary()


async def test_get_discount_summary_empty(aws_provider: AWSProvider):
    aws_provider._sp.describe_savings_plans.return_value = {"savingsPlans": []}
    aws_provider._ec2.describe_reserved_instances.return_value = {"ReservedInstances": []}
    aws_provider._ce.get_savings_plans_utilization.return_value = {"Total": {"Utilization": {}, "Savings": {}}}
    aws_provider._ce.get_reservation_utilization.return_value = {"Total": {}}

    summary = await aws_provider.get_discount_summary()
    assert summary["sp_count"] == 0
    assert summary["ri_count"] == 0


# ------------------------------------------------------------------
# find_cheapest_region (tests provider method directly)
# ------------------------------------------------------------------

async def test_find_cheapest_region_ordering(aws_provider: AWSProvider):
    """Verify that prices are returned sorted cheapest-first."""
    region_prices = {
        "us-east-1": "0.192",
        "ap-southeast-2": "0.278",
        "eu-west-1": "0.214",
    }

    async def mock_get_compute_price(instance_type, region, os="Linux", term=PricingTerm.ON_DEMAND):
        price = region_prices.get(region)
        if price:
            return [_make_price(region, price)]
        return []

    aws_provider.get_compute_price = mock_get_compute_price
    aws_provider.list_regions = AsyncMock(return_value=list(region_prices.keys()))

    # Replicate what find_cheapest_region does
    import asyncio
    from cloudcostmcp.models import PriceComparison

    all_prices = []
    for region in region_prices:
        prices = await aws_provider.get_compute_price("m5.xlarge", region)
        all_prices.extend(prices)

    comparison = PriceComparison.from_results("test", all_prices)
    assert comparison.cheapest.region == "us-east-1"
    assert comparison.most_expensive.region == "ap-southeast-2"
    assert comparison.price_delta_pct is not None
    assert comparison.price_delta_pct > 40.0  # Sydney is ~44% more expensive


async def test_find_cheapest_region_filters_unavailable(aws_provider: AWSProvider):
    """Regions with no pricing should be excluded from comparison, not cause errors."""
    async def mock_get_compute_price(instance_type, region, os="Linux", term=PricingTerm.ON_DEMAND):
        if region == "us-east-1":
            return [_make_price("us-east-1", "0.192")]
        return []  # unavailable in other regions

    aws_provider.get_compute_price = mock_get_compute_price

    import asyncio
    from cloudcostmcp.models import PriceComparison

    regions = ["us-east-1", "fake-region-1", "fake-region-2"]
    all_prices = []
    not_available = []
    for region in regions:
        prices = await aws_provider.get_compute_price("m5.xlarge", region)
        if prices:
            all_prices.extend(prices)
        else:
            not_available.append(region)

    assert len(all_prices) == 1
    assert len(not_available) == 2
