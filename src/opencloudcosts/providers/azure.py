"""
Azure cloud pricing provider.

Uses the Azure Retail Prices REST API — fully public, no credentials needed.

Endpoint: https://prices.azure.com/api/retail/prices
Filter fields: armSkuName, armRegionName, priceType, reservationTerm
priceType: "Consumption" = on-demand/spot, "Reservation" = reserved
Instance format: Standard_D4s_v3, Standard_E8s_v3, Standard_B2ms
Region format: eastus, westeurope, southeastasia (ARM region names, lowercase)
"""
from __future__ import annotations

import asyncio
import logging
from decimal import Decimal
from typing import Any

import httpx

from opencloudcosts.cache import CacheManager
from opencloudcosts.config import Settings
from opencloudcosts.models import (
    CloudProvider,
    EffectivePrice,
    InstanceTypeInfo,
    NormalizedPrice,
    PriceUnit,
    PricingTerm,
)
from opencloudcosts.providers.base import NotConfiguredError
from opencloudcosts.utils.regions import AZURE_REGION_DISPLAY, list_azure_regions

logger = logging.getLogger(__name__)

_AZURE_PRICES_BASE = "https://prices.azure.com/api/retail/prices"
_API_VERSION = "2023-01-01-preview"

# Map our storage_type keys to Azure productName substrings
_AZURE_STORAGE_MAP: dict[str, str] = {
    "premium-ssd": "Premium SSD Managed Disks",
    "standard-ssd": "Standard SSD Managed Disks",
    "standard-hdd": "Standard HDD Managed Disks",
    "ultra-ssd": "Ultra Disks",
    "blob": "Blob Storage",
}


class AzureProvider:
    provider = CloudProvider.AZURE

    def __init__(self, settings: Settings, cache: CacheManager) -> None:
        self._settings = settings
        self._cache = cache

    # ------------------------------------------------------------------
    # Internal HTTP helpers (synchronous — wrapped in asyncio.to_thread)
    # ------------------------------------------------------------------

    def _fetch_prices(self, filters: dict[str, str], max_results: int = 100) -> list[dict[str, Any]]:
        """
        Fetch items from the Azure Retail Prices API, following pagination.
        Runs synchronously — always called via asyncio.to_thread.
        """
        filter_str = " and ".join(f"{k} eq '{v}'" for k, v in filters.items())
        url = (
            f"{_AZURE_PRICES_BASE}"
            f"?api-version={_API_VERSION}"
            f"&$filter={filter_str}"
            f"&$top={min(max_results, 100)}"
        )

        results: list[dict[str, Any]] = []
        while url and len(results) < max_results:
            logger.debug("Azure Retail Prices fetch: %s", url)
            resp = httpx.get(url, timeout=30)
            resp.raise_for_status()
            data = resp.json()
            results.extend(data.get("Items", []))
            url = data.get("NextPageLink")

        return results[:max_results]

    def _item_to_price(
        self,
        item: dict[str, Any],
        region: str,
        term: PricingTerm,
        service: str = "compute",
    ) -> NormalizedPrice | None:
        """Convert a single Azure Retail Prices API item to a NormalizedPrice."""
        retail_price = item.get("retailPrice", 0)
        try:
            price = Decimal(str(retail_price))
        except Exception:
            return None

        if price == 0:
            return None

        sku_name = item.get("skuName", item.get("meterName", ""))
        arm_sku = item.get("armSkuName", "")

        return NormalizedPrice(
            provider=CloudProvider.AZURE,
            service=service,
            sku_id=item.get("meterId", arm_sku),
            product_family=item.get("serviceFamily", ""),
            description=sku_name,
            region=region,
            pricing_term=term,
            price_per_unit=price,
            unit=PriceUnit.PER_HOUR,
            attributes={
                "armSkuName": arm_sku,
                "productName": item.get("productName", ""),
                "meterName": item.get("meterName", ""),
                "serviceName": item.get("serviceName", ""),
                "unitOfMeasure": item.get("unitOfMeasure", ""),
            },
        )

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    async def get_compute_price(
        self,
        instance_type: str,
        region: str,
        os: str = "Linux",
        term: PricingTerm = PricingTerm.ON_DEMAND,
    ) -> list[NormalizedPrice]:
        cache_extras = {"instance_type": instance_type, "os": os, "term": term.value}
        cached = await self._cache.get_prices("azure", "compute", region, cache_extras)
        if cached is not None:
            return [NormalizedPrice.model_validate(p) for p in cached]

        filters: dict[str, str] = {
            "armSkuName": instance_type,
            "armRegionName": region,
        }

        if term == PricingTerm.ON_DEMAND:
            filters["priceType"] = "Consumption"
            if os == "Windows":
                # Windows SKUs have "Windows" in productName
                filters["productName"] = "Virtual Machines DSv3 Series Windows"
        elif term == PricingTerm.SPOT:
            # Spot prices are Consumption type with "Spot" in skuName
            # We filter post-fetch since the API doesn't directly filter on skuName
            filters["priceType"] = "Consumption"
        elif term == PricingTerm.RESERVED_1YR:
            filters["priceType"] = "Reservation"
            filters["reservationTerm"] = "1 Year"
        elif term == PricingTerm.RESERVED_3YR:
            filters["priceType"] = "Reservation"
            filters["reservationTerm"] = "3 Years"
        else:
            filters["priceType"] = "Consumption"

        items = await asyncio.to_thread(self._fetch_prices, filters, 10)

        prices: list[NormalizedPrice] = []
        for item in items:
            # For SPOT term, filter to only SKUs containing "Spot"
            if term == PricingTerm.SPOT:
                sku_name = item.get("skuName", "")
                if "Spot" not in sku_name:
                    continue
            p = self._item_to_price(item, region, term, "compute")
            if p:
                prices.append(p)

        await self._cache.set_prices(
            "azure", "compute", region, cache_extras,
            [p.model_dump(mode="json") for p in prices],
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return prices

    async def get_storage_price(
        self,
        storage_type: str,
        region: str,
        size_gb: float | None = None,
    ) -> list[NormalizedPrice]:
        product_name = _AZURE_STORAGE_MAP.get(storage_type.lower())
        if product_name is None:
            raise ValueError(
                f"Unknown Azure storage type: {storage_type!r}. "
                f"Supported: {sorted(_AZURE_STORAGE_MAP)}"
            )

        cache_extras = {"storage_type": storage_type}
        cached = await self._cache.get_prices("azure", "storage", region, cache_extras)
        if cached is not None:
            return [NormalizedPrice.model_validate(p) for p in cached]

        filters: dict[str, str] = {
            "armRegionName": region,
            "priceType": "Consumption",
            "productName": product_name,
        }

        items = await asyncio.to_thread(self._fetch_prices, filters, 10)

        prices: list[NormalizedPrice] = []
        for item in items:
            p = self._item_to_price(item, region, PricingTerm.ON_DEMAND, "storage")
            if p is None:
                continue
            # Storage pricing is per GB/month
            p = p.model_copy(update={"unit": PriceUnit.PER_GB_MONTH})
            prices.append(p)

        await self._cache.set_prices(
            "azure", "storage", region, cache_extras,
            [p.model_dump(mode="json") for p in prices],
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return prices

    async def list_regions(self, service: str = "compute") -> list[str]:
        return list_azure_regions()

    async def list_instance_types(
        self,
        region: str,
        family: str | None = None,
        min_vcpus: int | None = None,
        min_memory_gb: float | None = None,
        gpu: bool = False,
    ) -> list[InstanceTypeInfo]:
        """
        List Azure VM instance types available in a region.
        Note: The Azure Retail Prices API does not include vCPU/memory metadata.
        Returns instance type names only — vCPU and memory_gb will be 0.
        For full specs, see: https://learn.microsoft.com/en-us/azure/virtual-machines/sizes
        """
        cache_key = f"azure:instance_types:{region}:{family}:{min_vcpus}:{min_memory_gb}:{gpu}"
        cached = await self._cache.get_metadata(cache_key)
        if cached is not None:
            return [InstanceTypeInfo.model_validate(i) for i in cached]

        filters: dict[str, str] = {
            "armRegionName": region,
            "priceType": "Consumption",
            "serviceName": "Virtual Machines",
        }

        items = await asyncio.to_thread(self._fetch_prices, filters, 500)

        seen: set[str] = set()
        instances: list[InstanceTypeInfo] = []
        for item in items:
            arm_sku = item.get("armSkuName", "")
            if not arm_sku or arm_sku in seen:
                continue
            # Skip Spot / Low Priority variants
            sku_name = item.get("skuName", "")
            if "Spot" in sku_name or "Low Priority" in sku_name:
                continue
            seen.add(arm_sku)

            # Filter by family prefix if requested
            if family and not arm_sku.startswith(family):
                continue

            # GPU detection: Azure GPU VMs are NC, ND, NV series
            is_gpu = any(
                arm_sku.startswith(f"Standard_{prefix}")
                for prefix in ("NC", "ND", "NV")
            )
            if gpu and not is_gpu:
                continue
            if not gpu and is_gpu:
                continue

            instances.append(InstanceTypeInfo(
                provider=CloudProvider.AZURE,
                instance_type=arm_sku,
                vcpu=0,    # Not available from Retail Prices API
                memory_gb=0.0,  # Not available from Retail Prices API
                gpu_count=1 if is_gpu else 0,
                region=region,
            ))

        await self._cache.set_metadata(
            cache_key,
            [i.model_dump(mode="json") for i in instances],
            ttl_hours=self._settings.metadata_ttl_days * 24,
        )
        return instances

    async def check_availability(
        self,
        service: str,
        sku_or_type: str,
        region: str,
    ) -> bool:
        try:
            if service == "compute":
                prices = await self.get_compute_price(sku_or_type, region)
                return len(prices) > 0
            if service == "storage":
                prices = await self.get_storage_price(sku_or_type, region)
                return len(prices) > 0
        except Exception:
            return False
        return False

    async def search_pricing(
        self,
        query: str,
        region: str | None = None,
        service_code: str | None = None,
        max_results: int = 20,
    ) -> list[NormalizedPrice]:
        """
        Search Azure pricing by query string matching meterName or productName.
        The Azure Retail Prices API does not support substring filtering natively,
        so we fetch a broader result set and filter in-memory.
        """
        filters: dict[str, str] = {
            "priceType": "Consumption",
        }
        if region:
            filters["armRegionName"] = region
        if service_code:
            filters["serviceName"] = service_code

        items = await asyncio.to_thread(self._fetch_prices, filters, max_results * 5)

        query_lower = query.lower()
        results: list[NormalizedPrice] = []
        target_region = region or "eastus"

        for item in items:
            meter_name = item.get("meterName", "").lower()
            product_name = item.get("productName", "").lower()
            sku_name = item.get("skuName", "").lower()
            arm_sku = item.get("armSkuName", "").lower()

            if query_lower not in meter_name and query_lower not in product_name \
                    and query_lower not in sku_name and query_lower not in arm_sku:
                continue

            item_region = item.get("armRegionName", target_region)
            p = self._item_to_price(item, item_region, PricingTerm.ON_DEMAND)
            if p:
                results.append(p)
            if len(results) >= max_results:
                break

        return results

    async def get_effective_price(
        self,
        service: str,
        instance_type: str,
        region: str,
    ) -> list[EffectivePrice]:
        raise NotConfiguredError(
            "Azure effective pricing via billing APIs is not yet implemented. "
            "Use get_compute_price with term='reserved_1yr' or 'reserved_3yr' "
            "to see Azure Reserved Instance rates."
        )
