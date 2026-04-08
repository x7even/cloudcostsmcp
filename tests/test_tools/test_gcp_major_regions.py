"""Tests for T22: GCP major-regions default in find_cheapest_region and find_available_regions."""
from __future__ import annotations

from decimal import Decimal
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from opencloudcosts.models import CloudProvider, NormalizedPrice, PriceUnit, PricingTerm
from opencloudcosts.tools.availability import _GCP_MAJOR_REGIONS, _AWS_MAJOR_REGIONS


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _make_gcp_price(region: str, price: str) -> NormalizedPrice:
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


def _make_ctx(provider_name: str, pvdr_mock):
    """Build a minimal MCP Context mock wired to a provider mock."""
    ctx = MagicMock()
    ctx.request_context.lifespan_context = {"providers": {provider_name: pvdr_mock}}
    return ctx


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
    pvdr = MagicMock()
    # list_regions returns all 30+ GCP regions (we only care that it is NOT used
    # to build the search list when no regions param is passed)
    all_gcp = _GCP_MAJOR_REGIONS + ["asia-east2", "asia-northeast2", "northamerica-northeast1"]
    pvdr.list_regions = AsyncMock(return_value=all_gcp)
    return pvdr


async def test_find_cheapest_region_gcp_defaults_to_major_regions(gcp_pvdr):
    """GCP find_cheapest_region without regions param must query only _GCP_MAJOR_REGIONS."""
    queried_regions: list[str] = []

    async def mock_get_compute_price(instance_type, region, os="Linux", term=PricingTerm.ON_DEMAND):
        queried_regions.append(region)
        return [_make_gcp_price(region, "0.1900")]

    gcp_pvdr.get_compute_price = mock_get_compute_price

    # Import and call the tool function directly by registering it onto a mock mcp
    from opencloudcosts.tools.availability import register_availability_tools

    registered: dict = {}

    class FakeMCP:
        def tool(self):
            def decorator(fn):
                registered[fn.__name__] = fn
                return fn
            return decorator

    register_availability_tools(FakeMCP())
    fn = registered["find_cheapest_region"]

    ctx = _make_ctx("gcp", gcp_pvdr)
    result = await fn(ctx, provider="gcp", instance_type="n2-standard-4")

    assert set(queried_regions) == set(_GCP_MAJOR_REGIONS)
    assert "note" in result
    assert "GCP" in result["note"]
    assert "12" in result["note"]


async def test_find_cheapest_region_gcp_all_regions(gcp_pvdr):
    """GCP find_cheapest_region with regions=['all'] must query all available regions."""
    queried_regions: list[str] = []
    all_gcp = _GCP_MAJOR_REGIONS + ["asia-east2", "asia-northeast2", "northamerica-northeast1"]
    gcp_pvdr.list_regions = AsyncMock(return_value=all_gcp)

    async def mock_get_compute_price(instance_type, region, os="Linux", term=PricingTerm.ON_DEMAND):
        queried_regions.append(region)
        return [_make_gcp_price(region, "0.1900")]

    gcp_pvdr.get_compute_price = mock_get_compute_price

    from opencloudcosts.tools.availability import register_availability_tools

    registered: dict = {}

    class FakeMCP:
        def tool(self):
            def decorator(fn):
                registered[fn.__name__] = fn
                return fn
            return decorator

    register_availability_tools(FakeMCP())
    fn = registered["find_cheapest_region"]

    ctx = _make_ctx("gcp", gcp_pvdr)
    result = await fn(ctx, provider="gcp", instance_type="n2-standard-4", regions=["all"])

    assert set(queried_regions) == set(all_gcp)
    assert "note" not in result


async def test_find_cheapest_region_aws_behaviour_unchanged():
    """AWS find_cheapest_region without regions param must still query _AWS_MAJOR_REGIONS."""
    queried_regions: list[str] = []

    all_aws = _AWS_MAJOR_REGIONS + ["ap-east-1", "me-south-1"]
    pvdr = MagicMock()
    pvdr.list_regions = AsyncMock(return_value=all_aws)

    async def mock_get_compute_price(instance_type, region, os="Linux", term=PricingTerm.ON_DEMAND):
        queried_regions.append(region)
        from opencloudcosts.models import NormalizedPrice, CloudProvider, PriceUnit, PricingTerm as PT
        return [NormalizedPrice(
            provider=CloudProvider.AWS,
            service="compute",
            sku_id="AWSSKU",
            product_family="Compute Instance",
            description="m5.xlarge",
            region=region,
            attributes={},
            pricing_term=PT.ON_DEMAND,
            price_per_unit=Decimal("0.192"),
            unit=PriceUnit.PER_HOUR,
        )]

    pvdr.get_compute_price = mock_get_compute_price

    from opencloudcosts.tools.availability import register_availability_tools

    registered: dict = {}

    class FakeMCP:
        def tool(self):
            def decorator(fn):
                registered[fn.__name__] = fn
                return fn
            return decorator

    register_availability_tools(FakeMCP())
    fn = registered["find_cheapest_region"]

    ctx = _make_ctx("aws", pvdr)
    result = await fn(ctx, provider="aws", instance_type="m5.xlarge")

    assert set(queried_regions) == set(_AWS_MAJOR_REGIONS)
    assert "note" in result
    assert "AWS" in result["note"]


# ---------------------------------------------------------------------------
# find_available_regions — GCP defaults to major regions
# ---------------------------------------------------------------------------

async def test_find_available_regions_gcp_defaults_to_major_regions(gcp_pvdr):
    """GCP find_available_regions without regions param must query only _GCP_MAJOR_REGIONS."""
    queried_regions: list[str] = []

    async def mock_get_compute_price(instance_type, region, os="Linux", term=PricingTerm.ON_DEMAND):
        queried_regions.append(region)
        return [_make_gcp_price(region, "0.1900")]

    gcp_pvdr.get_compute_price = mock_get_compute_price

    from opencloudcosts.tools.availability import register_availability_tools

    registered: dict = {}

    class FakeMCP:
        def tool(self):
            def decorator(fn):
                registered[fn.__name__] = fn
                return fn
            return decorator

    register_availability_tools(FakeMCP())
    fn = registered["find_available_regions"]

    ctx = _make_ctx("gcp", gcp_pvdr)
    result = await fn(ctx, provider="gcp", instance_type="n2-standard-4")

    assert set(queried_regions) == set(_GCP_MAJOR_REGIONS)
    assert "note" in result
    assert "GCP" in result["note"]
    assert "12" in result["note"]


async def test_find_available_regions_gcp_all_regions(gcp_pvdr):
    """GCP find_available_regions with regions=['all'] must query all available regions."""
    queried_regions: list[str] = []
    all_gcp = _GCP_MAJOR_REGIONS + ["asia-east2", "asia-northeast2", "northamerica-northeast1"]
    gcp_pvdr.list_regions = AsyncMock(return_value=all_gcp)

    async def mock_get_compute_price(instance_type, region, os="Linux", term=PricingTerm.ON_DEMAND):
        queried_regions.append(region)
        return [_make_gcp_price(region, "0.1900")]

    gcp_pvdr.get_compute_price = mock_get_compute_price

    from opencloudcosts.tools.availability import register_availability_tools

    registered: dict = {}

    class FakeMCP:
        def tool(self):
            def decorator(fn):
                registered[fn.__name__] = fn
                return fn
            return decorator

    register_availability_tools(FakeMCP())
    fn = registered["find_available_regions"]

    ctx = _make_ctx("gcp", gcp_pvdr)
    result = await fn(ctx, provider="gcp", instance_type="n2-standard-4", regions=["all"])

    assert set(queried_regions) == set(all_gcp)
    assert "note" not in result
