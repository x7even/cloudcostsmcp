"""Tests for GCP major-regions default in find_cheapest_region and find_available_regions (v0.8.0)."""
from __future__ import annotations

from decimal import Decimal
from unittest.mock import AsyncMock, MagicMock

import pytest

from opencloudcosts.models import (
    CloudProvider,
    NormalizedPrice,
    PriceUnit,
    PricingResult,
    PricingTerm,
)
from opencloudcosts.tools.availability import _GCP_MAJOR_REGIONS, _AWS_MAJOR_REGIONS


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _make_gcp_price(region: str, price: str = "0.1900") -> NormalizedPrice:
    return NormalizedPrice(
        provider=CloudProvider.GCP,
        service="compute",
        sku_id="GCPSKU",
        product_family="Compute Instance",
        description="n2-standard-4",
        region=region,
        attributes={"vcpu": "4", "memory": "16 GB"},
        pricing_term=PricingTerm.ON_DEMAND,
        price_per_unit=Decimal(price),
        unit=PriceUnit.PER_HOUR,
    )


def _make_aws_price(region: str, price: str = "0.1920") -> NormalizedPrice:
    return NormalizedPrice(
        provider=CloudProvider.AWS,
        service="compute",
        sku_id="AWSSKU",
        product_family="Compute Instance",
        description="m5.xlarge",
        region=region,
        attributes={},
        pricing_term=PricingTerm.ON_DEMAND,
        price_per_unit=Decimal(price),
        unit=PriceUnit.PER_HOUR,
    )


def _make_ctx(provider_name: str, pvdr_mock):
    ctx = MagicMock()
    ctx.request_context.lifespan_context = {"providers": {provider_name: pvdr_mock}}
    return ctx


def _register_availability():
    from opencloudcosts.tools.availability import register_availability_tools
    registered: dict = {}

    class FakeMCP:
        def tool(self):
            def decorator(fn):
                registered[fn.__name__] = fn
                return fn
            return decorator

    register_availability_tools(FakeMCP())
    return registered


# ---------------------------------------------------------------------------
# _GCP_MAJOR_REGIONS constant
# ---------------------------------------------------------------------------

def test_gcp_major_regions_has_twelve_entries():
    assert len(_GCP_MAJOR_REGIONS) == 12


def test_gcp_major_regions_contains_expected_regions():
    expected = {
        "us-central1", "us-east1", "us-west1", "us-west2",
        "europe-west1", "europe-west2", "europe-west3", "europe-west4",
        "asia-east1", "asia-northeast1", "asia-southeast1", "australia-southeast1",
    }
    assert set(_GCP_MAJOR_REGIONS) == expected


# ---------------------------------------------------------------------------
# find_cheapest_region — GCP defaults to major regions
# ---------------------------------------------------------------------------

@pytest.fixture
def gcp_pvdr():
    all_gcp = _GCP_MAJOR_REGIONS + ["asia-east2", "asia-northeast2", "northamerica-northeast1"]
    pvdr = MagicMock()
    pvdr.list_regions = AsyncMock(return_value=all_gcp)
    pvdr.supports = MagicMock(return_value=True)
    return pvdr


async def test_find_cheapest_region_gcp_defaults_to_major_regions(gcp_pvdr):
    """GCP find_cheapest_region without regions param must query only _GCP_MAJOR_REGIONS."""
    queried_regions: list[str] = []

    async def mock_get_price(spec):
        queried_regions.append(spec.region)
        return PricingResult(public_prices=[_make_gcp_price(spec.region)])

    gcp_pvdr.get_price = AsyncMock(side_effect=mock_get_price)

    registered = _register_availability()
    fn = registered["find_cheapest_region"]

    ctx = _make_ctx("gcp", gcp_pvdr)
    spec = {
        "provider": "gcp",
        "domain": "compute",
        "resource_type": "n2-standard-4",
        "region": "us-central1",
    }
    result = await fn(ctx, spec=spec)

    assert set(queried_regions) == set(_GCP_MAJOR_REGIONS)
    assert "note" in result
    assert "GCP" in result["note"]
    assert "12" in result["note"]


async def test_find_cheapest_region_gcp_all_regions(gcp_pvdr):
    """GCP find_cheapest_region with regions=['all'] must query all available regions."""
    all_gcp = _GCP_MAJOR_REGIONS + ["asia-east2", "asia-northeast2", "northamerica-northeast1"]
    gcp_pvdr.list_regions = AsyncMock(return_value=all_gcp)
    queried_regions: list[str] = []

    async def mock_get_price(spec):
        queried_regions.append(spec.region)
        return PricingResult(public_prices=[_make_gcp_price(spec.region)])

    gcp_pvdr.get_price = AsyncMock(side_effect=mock_get_price)

    registered = _register_availability()
    fn = registered["find_cheapest_region"]

    ctx = _make_ctx("gcp", gcp_pvdr)
    spec = {
        "provider": "gcp",
        "domain": "compute",
        "resource_type": "n2-standard-4",
        "region": "us-central1",
    }
    result = await fn(ctx, spec=spec, regions=["all"])

    assert set(queried_regions) == set(all_gcp)
    assert "note" not in result


async def test_find_cheapest_region_aws_behaviour_unchanged():
    """AWS find_cheapest_region without regions param must query _AWS_MAJOR_REGIONS."""
    all_aws = _AWS_MAJOR_REGIONS + ["ap-east-1", "me-south-1"]
    pvdr = MagicMock()
    pvdr.list_regions = AsyncMock(return_value=all_aws)
    pvdr.supports = MagicMock(return_value=True)
    queried_regions: list[str] = []

    async def mock_get_price(spec):
        queried_regions.append(spec.region)
        return PricingResult(public_prices=[_make_aws_price(spec.region)])

    pvdr.get_price = AsyncMock(side_effect=mock_get_price)

    registered = _register_availability()
    fn = registered["find_cheapest_region"]

    ctx = _make_ctx("aws", pvdr)
    spec = {
        "provider": "aws",
        "domain": "compute",
        "resource_type": "m5.xlarge",
        "region": "us-east-1",
    }
    result = await fn(ctx, spec=spec)

    assert set(queried_regions) == set(_AWS_MAJOR_REGIONS)
    assert "note" in result
    assert "AWS" in result["note"]


# ---------------------------------------------------------------------------
# find_available_regions — GCP defaults to major regions
# ---------------------------------------------------------------------------

async def test_find_available_regions_gcp_defaults_to_major_regions(gcp_pvdr):
    """GCP find_available_regions without regions param must query only _GCP_MAJOR_REGIONS."""
    queried_regions: list[str] = []

    async def mock_get_price(spec):
        queried_regions.append(spec.region)
        return PricingResult(public_prices=[_make_gcp_price(spec.region)])

    gcp_pvdr.get_price = AsyncMock(side_effect=mock_get_price)

    registered = _register_availability()
    fn = registered["find_available_regions"]

    ctx = _make_ctx("gcp", gcp_pvdr)
    spec = {
        "provider": "gcp",
        "domain": "compute",
        "resource_type": "n2-standard-4",
        "region": "us-central1",
    }
    result = await fn(ctx, spec=spec)

    assert set(queried_regions) == set(_GCP_MAJOR_REGIONS)
    assert "note" in result
    assert "GCP" in result["note"]
    assert "12" in result["note"]


async def test_find_available_regions_gcp_all_regions(gcp_pvdr):
    """GCP find_available_regions with regions=['all'] must query all available regions."""
    all_gcp = _GCP_MAJOR_REGIONS + ["asia-east2", "asia-northeast2", "northamerica-northeast1"]
    gcp_pvdr.list_regions = AsyncMock(return_value=all_gcp)
    queried_regions: list[str] = []

    async def mock_get_price(spec):
        queried_regions.append(spec.region)
        return PricingResult(public_prices=[_make_gcp_price(spec.region)])

    gcp_pvdr.get_price = AsyncMock(side_effect=mock_get_price)

    registered = _register_availability()
    fn = registered["find_available_regions"]

    ctx = _make_ctx("gcp", gcp_pvdr)
    spec = {
        "provider": "gcp",
        "domain": "compute",
        "resource_type": "n2-standard-4",
        "region": "us-central1",
    }
    result = await fn(ctx, spec=spec, regions=["all"])

    assert set(queried_regions) == set(all_gcp)
    assert "note" not in result
