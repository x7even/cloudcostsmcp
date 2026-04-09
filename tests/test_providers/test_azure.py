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
    "retailPrice": 0.116,
    "unitPrice": 0.116,
    "armRegionName": "eastus",
    "armSkuName": "Standard_D4s_v3",
    "productName": "Virtual Machines DSv3 Series",
    "skuName": "D4s v3 1 Year",
    "meterName": "D4s v3",
    "serviceName": "Virtual Machines",
    "serviceFamily": "Compute",
    "unitOfMeasure": "1 Hour",
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
    assert prices[0].price_per_unit == Decimal("0.116")


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
# get_storage_price
# ---------------------------------------------------------------------------

async def test_azure_get_storage_price(azure_provider: AzureProvider):
    api_resp = {"Items": [_AZURE_STORAGE_ITEM], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        prices = await azure_provider.get_storage_price("premium-ssd", "eastus")

    assert len(prices) == 1
    p = prices[0]
    assert p.provider == CloudProvider.AZURE
    assert p.unit == PriceUnit.PER_GB_MONTH
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
        available = await azure_provider.check_availability(
            "compute", "Standard_D4s_v3", "eastus"
        )
    assert available is True


async def test_azure_check_availability_false(azure_provider: AzureProvider):
    empty_resp = {"Items": [], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(empty_resp)):
        available = await azure_provider.check_availability(
            "compute", "Standard_D4s_v3", "eastus"
        )
    assert available is False


# ---------------------------------------------------------------------------
# search_pricing
# ---------------------------------------------------------------------------

async def test_azure_search_pricing(azure_provider: AzureProvider):
    api_resp = {"Items": [_AZURE_VM_ITEM], "NextPageLink": None}
    with patch("httpx.get", return_value=_make_mock_response(api_resp)):
        results = await azure_provider.search_pricing("D4s", region="eastus")

    assert len(results) >= 1
    assert any("D4s" in r.description or "D4s" in r.attributes.get("meterName", "") for r in results)


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
        {**_AZURE_VM_ITEM, "armSkuName": "Standard_D4s_v3", "skuName": "D4s v3 Spot"},  # duplicate + spot
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
