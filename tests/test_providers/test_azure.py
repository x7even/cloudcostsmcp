"""Tests for the Azure provider, mocking httpx.get to avoid live API calls."""

from __future__ import annotations

from decimal import Decimal
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

from opencloudcosts.cache import CacheManager
from opencloudcosts.config import Settings
from opencloudcosts.models import CloudProvider, PriceUnit, PricingTerm
from opencloudcosts.providers.azure import AzureProvider

# ---------------------------------------------------------------------------
# Shared fixtures / test data
# ---------------------------------------------------------------------------

_AZURE_VM_ITEM = {
    "retailPrice": 0.192,
    "unitPrice": 0.192,
    "armRegionName": "eastus",
    "armSkuName": "Standard_D4s_v3",
    "productName": "Virtual Machines DSv3 Series",
    "skuName": "D4s v3",
    "meterName": "D4s v3",
    "serviceName": "Virtual Machines",
    "serviceFamily": "Compute",
    "unitOfMeasure": "1 Hour",
    "type": "Consumption",
    "currencyCode": "USD",
    "meterId": "test-meter-id",
}

_AZURE_API_RESPONSE = {"Items": [_AZURE_VM_ITEM], "NextPageLink": None}

_AZURE_RESERVED_ITEM = {
    "retailPrice": 1016.16,  # annual total (API returns total reservation cost, not hourly)
    "unitPrice": 1016.16,
    "armRegionName": "eastus",
    "armSkuName": "Standard_D4s_v3",
    "productName": "Virtual Machines DSv3 Series",
    "skuName": "D4s v3 1 Year",
    "meterName": "D4s v3",
    "serviceName": "Virtual Machines",
    "serviceFamily": "Compute",
    "unitOfMeasure": "1 Year",
    "type": "Reservation",
    "currencyCode": "USD",
    "meterId": "test-meter-reserved-id",
}

_AZURE_STORAGE_ITEM = {
    "retailPrice": 0.135,
    "unitPrice": 0.135,
    "armRegionName": "eastus",
    "armSkuName": "",
    "productName": "Premium SSD Managed Disks",
    "skuName": "P10 Disks",
    "meterName": "P10 Disks",
    "serviceName": "Storage",
    "serviceFamily": "Storage",
    "unitOfMeasure": "1/Month",
    "type": "Consumption",
    "currencyCode": "USD",
    "meterId": "test-storage-meter-id",
}


def _make_mock_response(data: dict) -> MagicMock:
    mock_resp = MagicMock()
    mock_resp.raise_for_status = MagicMock()
    mock_resp.json.return_value = data
    return mock_resp


@pytest.fixture
async def azure_provider(tmp_path: Path):
    settings = Settings(
        cache_dir=tmp_path / "cache",
        cache_ttl_hours=1,
        aws_enable_cost_explorer=False,
    )
    cache = CacheManager(settings.cache_dir)
    await cache.initialize()
    provider = AzureProvider(settings, cache)
    yield provider
    await cache.close()


# ---------------------------------------------------------------------------
# get_compute_price
# ---------------------------------------------------------------------------


async def test_azure_get_compute_price(azure_provider: AzureProvider):
    with patch("httpx.get", return_value=_make_mock_response(_AZURE_API_RESPONSE)):
        prices = await azure_provider.get_compute_price("Standard_D4s_v3", "eastus")

    assert len(prices) == 1
    p = prices[0]
    assert p.provider == CloudProvider.AZURE
    assert p.pricing_term == PricingTerm.ON_DEMAND
    assert p.price_per_unit == Decimal("0.192")
    assert p.unit == PriceUnit.PER_HOUR
    assert p.region == "eastus"
    assert p.attributes["armSkuName"] == "Standard_D4s_v3"
    assert p.attributes["serviceName"] == "Virtual Machines"


async def test_azure_get_compute_price_cached(azure_provider: AzureProvider):
    """Second call should hit the cache — httpx.get called exactly once."""
    with patch("httpx.get", return_value=_make_mock_response(_AZURE_API_RESPONSE)) as mock_get:
        await azure_provider.get_compute_price("Standard_D4s_v3", "eastus")
        await azure_provider.get_compute_price("Standard_D4s_v3", "eastus")

    assert mock_get.call_count == 1


async def test_azure_get_compute_price_reserved_1yr(azure_provider: AzureProvider):
    api_resp = {"Items": [_AZURE_RESERVED_ITEM], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_compute_price(
            "Standard_D4s_v3", "eastus", term=PricingTerm.RESERVED_1YR
        )

    assert len(prices) == 1
    assert prices[0].pricing_term == PricingTerm.RESERVED_1YR
    # 1016.16 annual ÷ 8760 hr/yr ≈ $0.116/hr
    assert abs(prices[0].price_per_unit - Decimal("0.116")) < Decimal("0.001")


async def test_azure_get_compute_price_zero_price_filtered(azure_provider: AzureProvider):
    """Items with retailPrice=0 should be filtered out."""
    zero_item = {**_AZURE_VM_ITEM, "retailPrice": 0}
    api_resp = {"Items": [zero_item], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_compute_price("Standard_D4s_v3", "eastus")

    assert prices == []


async def test_azure_get_compute_price_spot_filtered(azure_provider: AzureProvider):
    """Spot term should only include items with 'Spot' in skuName."""
    spot_item = {**_AZURE_VM_ITEM, "skuName": "D4s v3 Spot", "retailPrice": 0.04}
    non_spot_item = {**_AZURE_VM_ITEM, "skuName": "D4s v3", "retailPrice": 0.192}
    api_resp = {"Items": [spot_item, non_spot_item], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_compute_price(
            "Standard_D4s_v3", "eastus", term=PricingTerm.SPOT
        )

    assert len(prices) == 1
    assert prices[0].price_per_unit == Decimal("0.04")


# ---------------------------------------------------------------------------
# OS filtering and price sorting (T28 fix)
# ---------------------------------------------------------------------------

_AZURE_LINUX_ITEM = {
    "retailPrice": 0.384,
    "unitPrice": 0.384,
    "armRegionName": "eastus",
    "armSkuName": "Standard_D8s_v3",
    "productName": "Virtual Machines DSv3 Series",
    "skuName": "D8s v3",
    "meterName": "D8s v3",
    "serviceName": "Virtual Machines",
    "serviceFamily": "Compute",
    "unitOfMeasure": "1 Hour",
    "type": "Consumption",
    "currencyCode": "USD",
    "meterId": "linux-meter-id",
}

_AZURE_WINDOWS_ITEM = {
    "retailPrice": 0.752,
    "unitPrice": 0.752,
    "armRegionName": "eastus",
    "armSkuName": "Standard_D8s_v3",
    "productName": "Virtual Machines DSv3 Series Windows",
    "skuName": "D8s v3",
    "meterName": "D8s v3",
    "serviceName": "Virtual Machines",
    "serviceFamily": "Compute",
    "unitOfMeasure": "1 Hour",
    "type": "Consumption",
    "currencyCode": "USD",
    "meterId": "windows-meter-id",
}

_AZURE_SPOT_ITEM = {
    "retailPrice": 0.060,
    "unitPrice": 0.060,
    "armRegionName": "eastus",
    "armSkuName": "Standard_D8s_v3",
    "productName": "Virtual Machines DSv3 Series",
    "skuName": "D8s v3 Spot",
    "meterName": "D8s v3 Spot",
    "serviceName": "Virtual Machines",
    "serviceFamily": "Compute",
    "unitOfMeasure": "1 Hour",
    "type": "Consumption",
    "currencyCode": "USD",
    "meterId": "spot-meter-id",
}

_AZURE_LINUX_ITEM_CHEAPER = {
    **_AZURE_LINUX_ITEM,
    "retailPrice": 0.300,
    "meterId": "linux-meter-cheaper",
}


async def test_azure_linux_excludes_windows_skus(azure_provider: AzureProvider):
    """Linux on-demand results must not include Windows productName SKUs."""
    api_resp = {"Items": [_AZURE_LINUX_ITEM, _AZURE_WINDOWS_ITEM], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_compute_price("Standard_D8s_v3", "eastus", os="Linux")

    assert len(prices) == 1
    assert prices[0].price_per_unit == Decimal("0.384")
    for p in prices:
        assert "Windows" not in p.attributes.get("productName", ""), (
            f"Linux result should not contain Windows SKU: {p.attributes['productName']}"
        )


async def test_azure_windows_excludes_linux_skus(azure_provider: AzureProvider):
    """Windows on-demand results must only include Windows productName SKUs."""
    api_resp = {"Items": [_AZURE_LINUX_ITEM, _AZURE_WINDOWS_ITEM], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_compute_price("Standard_D8s_v3", "eastus", os="Windows")

    assert len(prices) == 1
    assert prices[0].price_per_unit == Decimal("0.752")
    for p in prices:
        assert "Windows" in p.attributes.get("productName", ""), (
            f"Windows result should contain Windows SKU: {p.attributes['productName']}"
        )


async def test_azure_compute_price_sorted_cheapest_first(azure_provider: AzureProvider):
    """Results must be sorted by price ascending (cheapest first)."""
    # Return items out of order: expensive first, cheap second
    api_resp = {
        "Items": [_AZURE_LINUX_ITEM, _AZURE_LINUX_ITEM_CHEAPER],
        "NextPageLink": None,
    }
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_compute_price("Standard_D8s_v3", "eastus", os="Linux")

    assert len(prices) == 2
    assert prices[0].price_per_unit <= prices[1].price_per_unit, (
        "Prices must be sorted cheapest first"
    )
    assert prices[0].price_per_unit == Decimal("0.300")
    assert prices[1].price_per_unit == Decimal("0.384")


async def test_azure_linux_on_demand_excludes_spot(azure_provider: AzureProvider):
    """On-demand Linux results must exclude Spot SKUs even when API returns them."""
    api_resp = {
        "Items": [_AZURE_LINUX_ITEM, _AZURE_SPOT_ITEM],
        "NextPageLink": None,
    }
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_compute_price("Standard_D8s_v3", "eastus", os="Linux")

    assert len(prices) == 1
    assert prices[0].price_per_unit == Decimal("0.384")
    for p in prices:
        assert "Spot" not in p.description, (
            f"On-demand result should not contain Spot SKU: {p.description}"
        )


# ---------------------------------------------------------------------------
# get_storage_price
# ---------------------------------------------------------------------------


async def test_azure_get_storage_price(azure_provider: AzureProvider):
    api_resp = {"Items": [_AZURE_STORAGE_ITEM], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_storage_price("premium-ssd", "eastus")

    assert len(prices) == 1
    p = prices[0]
    assert p.provider == CloudProvider.AZURE
    assert p.unit == PriceUnit.PER_MONTH  # Premium SSD tiers are flat monthly fees
    assert p.price_per_unit == Decimal("0.135")


async def test_azure_get_storage_price_unknown_type(azure_provider: AzureProvider):
    with pytest.raises(ValueError, match="Unknown Azure storage type"):
        await azure_provider.get_storage_price("nvme-ssd", "eastus")


# ---------------------------------------------------------------------------
# list_regions
# ---------------------------------------------------------------------------


async def test_azure_list_regions(azure_provider: AzureProvider):
    regions = await azure_provider.list_regions("compute")
    assert "eastus" in regions
    assert "westeurope" in regions
    assert "southeastasia" in regions
    assert len(regions) >= 20


# ---------------------------------------------------------------------------
# check_availability
# ---------------------------------------------------------------------------


async def test_azure_check_availability_true(azure_provider: AzureProvider):
    with patch("httpx.get", return_value=_make_mock_response(_AZURE_API_RESPONSE)):
        available = await azure_provider.check_availability("compute", "Standard_D4s_v3", "eastus")
    assert available is True


async def test_azure_check_availability_false(azure_provider: AzureProvider):
    empty_resp = {"Items": [], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(empty_resp)):
        available = await azure_provider.check_availability("compute", "Standard_D4s_v3", "eastus")
    assert available is False


# ---------------------------------------------------------------------------
# search_pricing
# ---------------------------------------------------------------------------


async def test_azure_search_pricing(azure_provider: AzureProvider):
    api_resp = {"Items": [_AZURE_VM_ITEM], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        results = await azure_provider.search_pricing("D4s", region="eastus")

    assert len(results) >= 1
    assert any(
        "D4s" in r.description or "D4s" in r.attributes.get("meterName", "") for r in results
    )


async def test_azure_search_pricing_no_match(azure_provider: AzureProvider):
    """Query that doesn't match any item should return empty list."""
    api_resp = {"Items": [_AZURE_VM_ITEM], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        results = await azure_provider.search_pricing("ZZZNOMATCH", region="eastus")

    assert results == []


# ---------------------------------------------------------------------------
# list_instance_types
# ---------------------------------------------------------------------------


async def test_azure_list_instance_types(azure_provider: AzureProvider):
    vm_items = [
        {**_AZURE_VM_ITEM, "armSkuName": "Standard_D4s_v3", "skuName": "D4s v3"},
        {**_AZURE_VM_ITEM, "armSkuName": "Standard_E8s_v3", "skuName": "E8s v3"},
        {
            **_AZURE_VM_ITEM,
            "armSkuName": "Standard_D4s_v3",
            "skuName": "D4s v3 Spot",
        },  # duplicate + spot
    ]
    api_resp = {"Items": vm_items, "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        instances = await azure_provider.list_instance_types("eastus")

    # Should deduplicate and exclude Spot variant
    instance_types = [i.instance_type for i in instances]
    assert "Standard_D4s_v3" in instance_types
    assert "Standard_E8s_v3" in instance_types
    assert len(instance_types) == len(set(instance_types))  # no duplicates


# ---------------------------------------------------------------------------
# region_display_name integration
# ---------------------------------------------------------------------------


def test_region_display_name_azure():
    from opencloudcosts.utils.regions import region_display_name

    assert region_display_name("azure", "eastus") == "East US"
    assert region_display_name("azure", "westeurope") == "West Europe"
    assert region_display_name("azure", "southeastasia") == "Southeast Asia"
    # Unknown region falls back to code
    assert region_display_name("azure", "unknownregion") == "unknownregion"


def test_normalize_region_azure():
    from opencloudcosts.utils.regions import normalize_region

    assert normalize_region("azure", "eastus") == "eastus"
    assert normalize_region("azure", "East US") == "eastus"
    assert normalize_region("azure", "West Europe") == "westeurope"


# ---------------------------------------------------------------------------
# v0.8.12 — Azure egress pricing tests
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_egress_internet_uses_api_rate(azure_provider: AzureProvider):
    """get_egress_price returns the Zone 1 base-tier rate (max of all Zone 1 rows)."""
    # API returns two tier rows — base ($0.087) and volume discount ($0.083)
    bw_items = [
        {
            "retailPrice": 0.083,
            "serviceName": "Bandwidth",
            "meterName": "Data Transfer Out Zone 1",
            "skuName": "Data Transfer Out Zone 1",
            "armRegionName": "global",
            "meterId": "bw-zone1-vol",
            "serviceFamily": "Networking",
            "productName": "Bandwidth",
            "unitOfMeasure": "1 GB",
        },
        {
            "retailPrice": 0.087,
            "serviceName": "Bandwidth",
            "meterName": "Data Transfer Out Zone 1",
            "skuName": "Data Transfer Out Zone 1",
            "armRegionName": "global",
            "meterId": "bw-zone1-base",
            "serviceFamily": "Networking",
            "productName": "Bandwidth",
            "unitOfMeasure": "1 GB",
        },
    ]
    api_resp = {"Items": bw_items, "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        results = await azure_provider.get_egress_price("eastus", data_gb=100.0)

    assert len(results) == 1
    r = results[0]
    # Base tier (highest price) must be selected
    assert r.price_per_unit == Decimal("0.087")
    assert r.unit == PriceUnit.PER_GB_MONTH
    assert r.attributes["egress_type"] == "internet"
    assert r.attributes["zone"] == "zone1"
    assert "monthly_estimate" in r.attributes
    # 100 GB - 5 GB free = 95 GB chargeable
    expected_monthly = Decimal("0.087") * Decimal("95")
    assert f"${float(expected_monthly):.4f}" in r.attributes["monthly_estimate"]


@pytest.mark.asyncio
async def test_egress_inter_region_sets_dest(azure_provider: AzureProvider):
    """When dest_region is set, egress_type is 'inter-region' and dest_region appears in attrs."""
    bw_items = [
        {
            "retailPrice": 0.087,
            "serviceName": "Bandwidth",
            "meterName": "Data Transfer Out Zone 1",
            "skuName": "Data Transfer Out Zone 1",
            "armRegionName": "global",
            "meterId": "bw-zone1",
            "serviceFamily": "Networking",
            "productName": "Bandwidth",
            "unitOfMeasure": "1 GB",
        }
    ]
    api_resp = {"Items": bw_items, "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        results = await azure_provider.get_egress_price("eastus", "westeurope", data_gb=1000.0)

    assert len(results) == 1
    r = results[0]
    assert r.attributes["egress_type"] == "inter-region"
    assert r.attributes["dest_region"] == "westeurope"
    assert "eastus" in r.description
    assert "westeurope" in r.description


@pytest.mark.asyncio
async def test_egress_fallback_rate_when_api_returns_no_zone1(azure_provider: AzureProvider):
    """Falls back to $0.087/GB static rate when API has no Zone 1 bandwidth items."""
    api_resp = {"Items": [], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        results = await azure_provider.get_egress_price("eastus", data_gb=10.0)

    assert len(results) == 1
    assert results[0].price_per_unit == Decimal("0.087")


@pytest.mark.asyncio
async def test_egress_free_tier_5gb(azure_provider: AzureProvider):
    """For data_gb <= 5, monthly estimate is $0 (all within free tier)."""
    api_resp = {"Items": [], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        results = await azure_provider.get_egress_price("eastus", data_gb=3.0)

    assert len(results) == 1
    assert "$0.0000" in results[0].attributes.get("monthly_estimate", "")


@pytest.mark.asyncio
async def test_egress_zone2_uses_zone2_rate(azure_provider: AzureProvider):
    """Asia Pacific regions use Zone 2 rate, not Zone 1."""
    # API returns Zone 2 items with $0.16 base rate
    bw_items = [
        {
            "retailPrice": 0.16,
            "serviceName": "Bandwidth",
            "meterName": "Data Transfer Out Zone 2",
            "skuName": "Data Transfer Out Zone 2",
            "armRegionName": "global",
            "meterId": "bw-zone2-base",
            "serviceFamily": "Networking",
            "productName": "Bandwidth",
            "unitOfMeasure": "1 GB",
        },
    ]
    api_resp = {"Items": bw_items, "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        results = await azure_provider.get_egress_price("eastasia", data_gb=100.0)

    assert len(results) == 1
    r = results[0]
    assert r.attributes["zone"] == "zone2"
    assert r.price_per_unit == Decimal("0.16")


@pytest.mark.asyncio
async def test_egress_supports_capability(azure_provider: AzureProvider):
    """AzureProvider.supports returns True for inter_region_egress domain."""
    from opencloudcosts.models import PricingDomain

    assert azure_provider.supports(PricingDomain.INTER_REGION_EGRESS)


@pytest.mark.asyncio
async def test_egress_get_price_dispatch(azure_provider: AzureProvider):
    """get_price with EgressPricingSpec returns a PricingResult with egress prices."""
    from opencloudcosts.models import EgressPricingSpec, PricingDomain

    spec = EgressPricingSpec(
        provider="azure",
        domain=PricingDomain.INTER_REGION_EGRESS,
        source_region="eastus",
        dest_region="westeurope",
        data_gb=500.0,
    )
    api_resp = {"Items": [], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        result = await azure_provider.get_price(spec)

    assert len(result.public_prices) == 1
    assert result.public_prices[0].service == "egress"


# ---------------------------------------------------------------------------
# network/egress (tiered internet egress path) [new in this version]
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_network_egress_internet_returns_breakdown(azure_provider: AzureProvider):
    """get_price with domain=network service=egress returns PricingResult with tier breakdown."""
    from opencloudcosts.models import NetworkPricingSpec, PricingDomain

    spec = NetworkPricingSpec(
        provider="azure",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="eastus",
        destination_type="internet",
        data_gb_per_month=1000.0,
    )
    api_resp = {"Items": [], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        result = await azure_provider.get_price(spec)

    assert len(result.public_prices) == 1
    p = result.public_prices[0]
    assert p.provider.value == "azure"
    assert p.service == "egress"
    assert "tiers" in result.breakdown
    assert result.breakdown["data_gb"] == pytest.approx(1000.0)
    # Zone 1: 5 GB free, 995 GB at $0.087 = $86.565
    assert float(result.breakdown["total_cost"]) == pytest.approx(86.565, rel=1e-3)


@pytest.mark.asyncio
async def test_network_egress_cross_region(azure_provider: AzureProvider):
    """Cross-region egress via network/egress uses zone1 flat rate ($0.02/GB)."""
    from opencloudcosts.models import NetworkPricingSpec, PricingDomain

    spec = NetworkPricingSpec(
        provider="azure",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="eastus",
        destination_type="cross_region",
        destination_region="westeurope",
        data_gb_per_month=500.0,
    )
    api_resp = {"Items": [], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        result = await azure_provider.get_price(spec)

    assert len(result.public_prices) == 1
    p = result.public_prices[0]
    assert p.service == "egress"
    # Zone 1 cross-region = $0.02/GB
    assert float(p.price_per_unit) == pytest.approx(0.02, rel=1e-3)


@pytest.mark.asyncio
async def test_network_egress_100gb_all_free(azure_provider: AzureProvider):
    """100 GB: first 5 GB free, remaining 95 GB at $0.087/GB = $8.265."""
    from opencloudcosts.models import NetworkPricingSpec, PricingDomain

    spec = NetworkPricingSpec(
        provider="azure",
        domain=PricingDomain.NETWORK,
        service="egress",
        source_region="eastus",
        destination_type="internet",
        data_gb_per_month=100.0,
    )
    api_resp = {"Items": [], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        result = await azure_provider.get_price(spec)

    # 5 GB free, 95 GB × $0.087 = $8.265
    assert float(result.breakdown["total_cost"]) == pytest.approx(8.265, rel=1e-3)


@pytest.mark.asyncio
async def test_network_egress_supports_capability(azure_provider: AzureProvider):
    """AzureProvider.supports returns True for NETWORK/egress."""
    from opencloudcosts.models import PricingDomain

    assert azure_provider.supports(PricingDomain.NETWORK, "egress")


# ===========================================================================
# New service tests — Azure SQL, Azure Functions, AKS, Azure OpenAI,
# Azure Monitor, Azure CDN, Azure Cosmos DB, Azure Front Door
# Added to cover the 6 new services and regression tests for critical fixes.
# ===========================================================================

# ---------------------------------------------------------------------------
# Azure SQL (get_sql_price)
# ---------------------------------------------------------------------------

_AZURE_SQL_ITEM = {
    "retailPrice": 0.3812,
    "unitPrice": 0.3812,
    "armRegionName": "eastus",
    "armSkuName": "",
    "productName": "SQL Database Vcore",
    "skuName": "GP_Gen5_4 LRS",
    "meterName": "GP_Gen5_4",
    "serviceName": "SQL Database",
    "serviceFamily": "Databases",
    "unitOfMeasure": "1 Hour",
    "type": "Consumption",
    "currencyCode": "USD",
    "meterId": "sql-gp-4vcores-lrs",
}


async def test_sql_price_gp_4vcores(azure_provider: AzureProvider):
    """SQL General Purpose 4 vCores returns correct price and unit."""
    api_resp = {"Items": [_AZURE_SQL_ITEM], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_sql_price("General Purpose 4 vCores", "eastus")

    assert len(prices) == 1
    p = prices[0]
    assert p.provider == CloudProvider.AZURE
    assert p.price_per_unit == Decimal("0.3812")
    assert p.unit == PriceUnit.PER_HOUR
    assert p.service == "sql"
    assert p.fetched_at is not None
    assert p.source_url is not None
    assert p.cache_age_seconds == 0


async def test_sql_price_vcores_word_boundary(azure_provider: AzureProvider):
    """SQL vCore filter must not match 14 or 40 vCore items when requesting 4 vCores."""
    items = [
        # 4-vCore item — should match
        {**_AZURE_SQL_ITEM, "skuName": "GP_Gen5_4 LRS", "meterId": "sql-gp-4"},
        # 14-vCore item — must NOT match (false positive fixed by issue #3)
        {**_AZURE_SQL_ITEM, "skuName": "GP_Gen5_14 LRS", "retailPrice": 1.34, "meterId": "sql-gp-14"},
        # 40-vCore item — must NOT match
        {**_AZURE_SQL_ITEM, "skuName": "GP_Gen5_40 LRS", "retailPrice": 3.05, "meterId": "sql-gp-40"},
    ]
    api_resp = {"Items": items, "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_sql_price("General Purpose 4 vCores", "eastus")

    # Only the 4-vCore item should be returned
    assert len(prices) == 1
    assert prices[0].price_per_unit == Decimal("0.3812")


async def test_sql_price_empty_response(azure_provider: AzureProvider):
    """Empty API response returns empty list (no exception, no fallback for SQL)."""
    api_resp = {"Items": [], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_sql_price("General Purpose 4 vCores", "eastus")

    assert prices == []


async def test_sql_price_cache_hit(azure_provider: AzureProvider):
    """Second call to get_sql_price hits cache; httpx.get called only once."""
    api_resp = {"Items": [_AZURE_SQL_ITEM], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)) as mock_get:
        await azure_provider.get_sql_price("General Purpose 4 vCores", "eastus")
        prices = await azure_provider.get_sql_price("General Purpose 4 vCores", "eastus")

    assert mock_get.call_count == 1
    # Cache hit: cache_age_seconds >= 0 (may be 0 in fast test)
    assert all(p.cache_age_seconds is not None for p in prices)


# ---------------------------------------------------------------------------
# Azure Functions (get_functions_price)
# ---------------------------------------------------------------------------

_AZURE_FUNCTIONS_GB_SEC_ITEM = {
    "retailPrice": 0.000016,
    "unitPrice": 0.000016,
    "armRegionName": "eastus",
    "armSkuName": "",
    "productName": "Azure Functions",
    "skuName": "Standard",
    "meterName": "Standard GB-s",
    "serviceName": "Functions",
    "serviceFamily": "Compute",
    "unitOfMeasure": "1 GB Second",
    "type": "Consumption",
    "currencyCode": "USD",
    "meterId": "functions-gbsec",
}

_AZURE_FUNCTIONS_EXEC_ITEM = {
    "retailPrice": 0.0000002,
    "unitPrice": 0.0000002,
    "armRegionName": "eastus",
    "armSkuName": "",
    "productName": "Azure Functions",
    "skuName": "Standard",
    "meterName": "Standard Execution",
    "serviceName": "Functions",
    "serviceFamily": "Compute",
    "unitOfMeasure": "1 Execution",
    "type": "Consumption",
    "currencyCode": "USD",
    "meterId": "functions-exec",
}


async def test_functions_price_basic(azure_provider: AzureProvider):
    """Functions price returns gb-second and per-request items."""
    api_resp = {
        "Items": [_AZURE_FUNCTIONS_GB_SEC_ITEM, _AZURE_FUNCTIONS_EXEC_ITEM],
        "NextPageLink": None,
    }
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_functions_price("eastus")

    units = {p.unit for p in prices}
    assert PriceUnit.PER_GB_SECOND in units
    assert PriceUnit.PER_REQUEST in units
    assert all(p.fetched_at is not None for p in prices)
    assert all(p.source_url is not None for p in prices)
    assert all(p.cache_age_seconds == 0 for p in prices)


async def test_functions_price_with_estimate(azure_provider: AzureProvider):
    """Providing gb_seconds and requests_millions adds a monthly estimate entry."""
    api_resp = {
        "Items": [_AZURE_FUNCTIONS_GB_SEC_ITEM, _AZURE_FUNCTIONS_EXEC_ITEM],
        "NextPageLink": None,
    }
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_functions_price(
            "eastus",
            gb_seconds=5_000_000.0,  # 5M GB-s (after 400K free = 4.6M billable)
            requests_millions=10.0,  # 10M req (after 1M free = 9M billable)
        )

    estimate = next((p for p in prices if "estimate" in p.sku_id), None)
    assert estimate is not None
    assert estimate.price_per_unit > Decimal("0")
    assert "breakdown" in estimate.attributes


async def test_functions_price_empty_uses_static_fallback(azure_provider: AzureProvider):
    """Empty API response returns static fallback rates (not exception)."""
    api_resp = {"Items": [], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_functions_price("eastus")

    assert len(prices) >= 2
    sources = {p.attributes.get("source") for p in prices}
    assert "static_fallback" in sources


# ---------------------------------------------------------------------------
# AKS (get_aks_price)
# ---------------------------------------------------------------------------

_AZURE_AKS_ITEM = {
    "retailPrice": 0.10,
    "unitPrice": 0.10,
    "armRegionName": "eastus",
    "armSkuName": "",
    "productName": "Azure Kubernetes Service",
    "skuName": "Standard",
    "meterName": "Standard Uptime SLA",
    "serviceName": "Azure Kubernetes Service",
    "serviceFamily": "Compute",
    "unitOfMeasure": "1 Hour",
    "type": "Consumption",
    "currencyCode": "USD",
    "meterId": "aks-standard-sla",
}


async def test_aks_price_standard(azure_provider: AzureProvider):
    """AKS Standard tier returns the cluster management fee."""
    api_resp = {"Items": [_AZURE_AKS_ITEM], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_aks_price("eastus", mode="standard")

    assert len(prices) >= 1
    p = prices[0]
    assert p.provider == CloudProvider.AZURE
    assert p.price_per_unit > Decimal("0")
    assert p.unit == PriceUnit.PER_HOUR
    assert p.fetched_at is not None
    assert p.source_url is not None
    assert p.cache_age_seconds == 0


async def test_aks_price_free_tier(azure_provider: AzureProvider):
    """AKS free tier returns $0 control plane cost."""
    prices = await azure_provider.get_aks_price("eastus", mode="free")

    assert len(prices) == 1
    assert prices[0].price_per_unit == Decimal("0")


async def test_aks_price_empty_api_uses_static_fallback(azure_provider: AzureProvider):
    """Empty API response for standard tier uses static fallback rate ($0.10/hr)."""
    api_resp = {"Items": [], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_aks_price("eastus", mode="standard")

    assert len(prices) >= 1
    assert prices[0].price_per_unit == Decimal("0.10")
    assert prices[0].attributes.get("source") == "static_fallback"


# ---------------------------------------------------------------------------
# Azure OpenAI (get_openai_price)
# ---------------------------------------------------------------------------


async def test_openai_price_static_fallback_gpt4o(azure_provider: AzureProvider):
    """gpt-4o returns static fallback input+output prices when API returns nothing."""
    api_resp = {"Items": [], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_openai_price("gpt-4o", "eastus")

    token_types = {p.attributes.get("token_type") for p in prices}
    assert "input" in token_types
    assert "output" in token_types
    in_p = next(p for p in prices if p.attributes.get("token_type") == "input")
    out_p = next(p for p in prices if p.attributes.get("token_type") == "output")
    assert in_p.price_per_unit == Decimal("0.005")
    assert out_p.price_per_unit == Decimal("0.015")
    assert all(p.fetched_at is not None for p in prices)
    assert all(p.cache_age_seconds == 0 for p in prices)


async def test_openai_price_with_token_estimate(azure_provider: AzureProvider):
    """Providing input_tokens and output_tokens adds a cost estimate entry."""
    api_resp = {"Items": [], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_openai_price(
            "gpt-4o",
            "eastus",
            input_tokens=1_000_000,
            output_tokens=500_000,
        )

    estimate = next((p for p in prices if "estimate" in p.sku_id), None)
    assert estimate is not None
    # 1M input tokens × $0.005/1K = $5.00; 500K output × $0.015/1K = $7.50 → $12.50
    assert estimate.price_per_unit == Decimal("12.50")


async def test_openai_price_unknown_model_returns_empty(azure_provider: AzureProvider):
    """Unknown model not in static rates returns empty list (no exception)."""
    api_resp = {"Items": [], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_openai_price("unknown-model-xyz", "eastus")

    assert prices == []


# ---------------------------------------------------------------------------
# Azure Monitor (get_monitor_price)
# ---------------------------------------------------------------------------

_AZURE_MONITOR_BASIC_LOG_ITEM = {
    "retailPrice": 0.50,
    "unitPrice": 0.50,
    "armRegionName": "eastus",
    "armSkuName": "",
    "productName": "Log Analytics",
    "skuName": "Basic Logs",
    "meterName": "Basic Logs Data Ingestion",
    "serviceName": "Azure Monitor",
    "serviceFamily": "Management and Governance",
    "unitOfMeasure": "1 GB",
    "type": "Consumption",
    "currencyCode": "USD",
    "meterId": "monitor-basic-logs",
}


async def test_monitor_price_basic_logs(azure_provider: AzureProvider):
    """Monitor returns Basic Logs ingestion price from API."""
    api_resp = {"Items": [_AZURE_MONITOR_BASIC_LOG_ITEM], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_monitor_price("eastus")

    assert len(prices) >= 1
    assert any(p.price_per_unit == Decimal("0.50") for p in prices)
    assert all(p.fetched_at is not None for p in prices)
    assert all(p.cache_age_seconds == 0 for p in prices)


async def test_monitor_price_log_estimate(azure_provider: AzureProvider):
    """get_monitor_price with log_gb adds a cost estimate entry using static Analytics rate."""
    api_resp = {"Items": [], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_monitor_price("eastus", log_gb=100.0)

    estimate = next((p for p in prices if "estimate" in p.sku_id), None)
    assert estimate is not None
    # 100 - 5 = 95 GB billable × $2.30 = $218.50
    assert estimate.price_per_unit == Decimal("218.50")


async def test_monitor_price_empty_uses_static_fallback(azure_provider: AzureProvider):
    """Empty API response returns static fallback Analytics Logs and Metrics prices."""
    api_resp = {"Items": [], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_monitor_price("eastus")

    assert len(prices) >= 1
    sources = {p.attributes.get("source") for p in prices}
    assert "static_fallback" in sources


# ---------------------------------------------------------------------------
# Azure CDN (get_cdn_price)
# ---------------------------------------------------------------------------


async def test_cdn_price_zone1_static_fallback(azure_provider: AzureProvider):
    """Empty API response for Zone 1 region returns static Zone 1 rate ($0.081/GB)."""
    api_resp = {"Items": [], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_cdn_price("eastus")

    assert len(prices) >= 1
    p = prices[0]
    assert p.price_per_unit == Decimal("0.081")
    assert p.attributes.get("cdn_zone") == "Zone 1"
    assert p.attributes.get("source") == "static_fallback"


async def test_cdn_price_zone2_non_zone1_rate(azure_provider: AzureProvider):
    """Zone 2 region (e.g. southeastasia) returns Zone 2 rate, not Zone 1."""
    api_resp = {"Items": [], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_cdn_price("southeastasia")

    assert len(prices) >= 1
    p = prices[0]
    assert p.attributes.get("cdn_zone") == "Zone 2"
    # Zone 2 rate must differ from Zone 1 ($0.081)
    assert p.price_per_unit != Decimal("0.081")
    assert p.price_per_unit > Decimal("0")


async def test_cdn_price_with_estimate(azure_provider: AzureProvider):
    """Providing data_gb adds a monthly cost estimate entry."""
    api_resp = {"Items": [], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_cdn_price("eastus", data_gb=1000.0)

    estimate = next((p for p in prices if "estimate" in p.sku_id), None)
    assert estimate is not None
    assert estimate.price_per_unit > Decimal("0")
    assert "breakdown" in estimate.attributes


# ---------------------------------------------------------------------------
# Azure Cosmos DB (get_cosmos_price) — regression for critical fix #1
# ---------------------------------------------------------------------------

_AZURE_COSMOS_PROVISIONED_ITEM = {
    "retailPrice": 0.008,
    "unitPrice": 0.008,
    "armRegionName": "eastus",
    "armSkuName": "",
    "productName": "Azure Cosmos DB",
    "skuName": "100 RU/s",
    "meterName": "100 RU/s",
    "serviceName": "Azure Cosmos DB",
    "serviceFamily": "Databases",
    "unitOfMeasure": "1 Hour",
    "type": "Consumption",
    "currencyCode": "USD",
    "meterId": "cosmos-provisioned",
}

_AZURE_COSMOS_SERVERLESS_ITEM = {
    "retailPrice": 0.25,
    "unitPrice": 0.25,
    "armRegionName": "eastus",
    "armSkuName": "",
    "productName": "Azure Cosmos DB Serverless",
    "skuName": "1M RUs",
    "meterName": "Serverless Request Units",
    "serviceName": "Azure Cosmos DB",
    "serviceFamily": "Databases",
    "unitOfMeasure": "1M",
    "type": "Consumption",
    "currencyCode": "USD",
    "meterId": "cosmos-serverless",
}


async def test_cosmos_serverless_via_get_price(azure_provider: AzureProvider):
    """get_price with deployment='serverless' routes to serverless pricing (fix #1 regression)."""
    from opencloudcosts.models import DatabasePricingSpec, PricingDomain

    spec = DatabasePricingSpec(
        provider="azure",
        domain=PricingDomain.DATABASE,
        service="cosmos",
        region="eastus",
        deployment="serverless",
    )
    api_resp = {
        "Items": [_AZURE_COSMOS_PROVISIONED_ITEM, _AZURE_COSMOS_SERVERLESS_ITEM],
        "NextPageLink": None,
    }
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        result = await azure_provider.get_price(spec)

    prices = result.public_prices
    # With deployment=serverless, only the serverless item should match
    assert len(prices) >= 1
    descriptions = [p.description.lower() for p in prices]
    meter_names = [p.attributes.get("meterName", "").lower() for p in prices]
    # At least one price should come from a serverless meter
    assert any("serverless" in d or "serverless" in m for d, m in zip(descriptions, meter_names))


async def test_cosmos_autoscale_via_get_price(azure_provider: AzureProvider):
    """get_price with deployment='autoscale' routes to autoscale pricing (fix #1 regression)."""
    from opencloudcosts.models import DatabasePricingSpec, PricingDomain

    _COSMOS_AUTOSCALE_ITEM = {
        **_AZURE_COSMOS_PROVISIONED_ITEM,
        "meterName": "Autoscale 100 RU/s",
        "productName": "Azure Cosmos DB Autoscale",
        "meterId": "cosmos-autoscale",
    }

    spec = DatabasePricingSpec(
        provider="azure",
        domain=PricingDomain.DATABASE,
        service="cosmos",
        region="eastus",
        deployment="autoscale",
    )
    api_resp = {
        "Items": [_AZURE_COSMOS_PROVISIONED_ITEM, _COSMOS_AUTOSCALE_ITEM],
        "NextPageLink": None,
    }
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        result = await azure_provider.get_price(spec)

    prices = result.public_prices
    assert len(prices) >= 1
    # Autoscale item should appear; provisioned-only item should be filtered out
    meter_names = [p.attributes.get("meterName", "").lower() for p in prices]
    assert any("autoscale" in m for m in meter_names)


async def test_cosmos_price_provisioned_direct(azure_provider: AzureProvider):
    """Direct get_cosmos_price with deployment='provisioned' returns the RU/s rate."""
    api_resp = {
        "Items": [_AZURE_COSMOS_PROVISIONED_ITEM],
        "NextPageLink": None,
    }
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_cosmos_price("eastus", deployment="provisioned")

    assert len(prices) >= 1
    assert prices[0].price_per_unit == Decimal("0.008")
    assert prices[0].fetched_at is not None
    assert prices[0].source_url is not None
    assert prices[0].cache_age_seconds == 0


# ---------------------------------------------------------------------------
# Azure Front Door (get_front_door_price) — regression for critical fix #2
# ---------------------------------------------------------------------------


async def test_front_door_zone1_static_fallback(azure_provider: AzureProvider):
    """Empty API for Zone 1 region returns $0.0825/GB fallback rate."""
    api_resp = {"Items": [], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_front_door_price("eastus")

    dt_price = next(p for p in prices if p.unit == PriceUnit.PER_GB)
    assert dt_price.price_per_unit == Decimal("0.0825")
    assert dt_price.attributes.get("cdn_zone") == "Zone 1"


async def test_front_door_zone2_returns_zone2_rate(azure_provider: AzureProvider):
    """Zone 2 region returns a different (higher) rate than Zone 1."""
    api_resp = {"Items": [], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_front_door_price("southeastasia")

    dt_price = next(p for p in prices if p.unit == PriceUnit.PER_GB)
    assert dt_price.attributes.get("cdn_zone") == "Zone 2"
    # Zone 2 rate must differ from Zone 1
    assert dt_price.price_per_unit != Decimal("0.0825")
    assert dt_price.price_per_unit > Decimal("0")