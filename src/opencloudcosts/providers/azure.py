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
    ComputePricingSpec,
    EffectivePrice,
    InstanceTypeInfo,
    NormalizedPrice,
    PriceUnit,
    PricingDomain,
    PricingResult,
    PricingSpec,
    PricingTerm,
    ProviderCatalog,
    StoragePricingSpec,
)
from opencloudcosts.providers.base import NotConfiguredError, NotSupportedError, ProviderBase
from opencloudcosts.utils.http_retry import sync_retry
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
    # Cross-provider aliases so LLMs can use AWS/GCP names
    "gp3": "Premium SSD Managed Disks",   # AWS gp3 → closest Azure equivalent
    "gp2": "Standard SSD Managed Disks",
    "io1": "Premium SSD Managed Disks",
    "pd-ssd": "Premium SSD Managed Disks",   # GCP alias
    "pd-balanced": "Standard SSD Managed Disks",
    "pd-standard": "Standard HDD Managed Disks",
    "standard": "Standard SSD Managed Disks",
    # ARM SKU-style names
    "standardssd_lrs": "Standard SSD Managed Disks",
    "premium_lrs": "Premium SSD Managed Disks",
    "standard_lrs": "Standard HDD Managed Disks",
    "ultrassd_lrs": "Ultra Disks",
}


# Azure Premium SSD P-series tier sizes (GiB capacity per tier).
# Each tier's price is a flat monthly fee covering disks up to that capacity.
# When size_gb is specified, we select the smallest tier >= size_gb.
_PREMIUM_SSD_TIERS: list[tuple[str, int]] = [
    ("P1", 4), ("P2", 8), ("P3", 16), ("P4", 32), ("P6", 64),
    ("P10", 128), ("P15", 256), ("P20", 512), ("P30", 1024),
    ("P40", 2048), ("P50", 4096), ("P60", 8192), ("P70", 16384), ("P80", 32767),
]

# Storage types that use P-series fixed-size tiers
_PREMIUM_SSD_PRODUCT_NAMES = {"Premium SSD Managed Disks"}


def _select_premium_ssd_tier(size_gb: float) -> str:
    """Return the smallest P-series tier name that covers size_gb."""
    for tier_name, capacity in _PREMIUM_SSD_TIERS:
        if capacity >= size_gb:
            return tier_name
    return "P80"


_AZURE_CAPABILITIES: dict[tuple[str, str | None], bool] = {
    (PricingDomain.COMPUTE.value, None): True,
    (PricingDomain.COMPUTE.value, "vm"): True,
    (PricingDomain.STORAGE.value, None): True,
    (PricingDomain.STORAGE.value, "managed_disks"): True,
    (PricingDomain.STORAGE.value, "blob"): True,
}


class AzureProvider(ProviderBase):
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
            for attempt in sync_retry():
                with attempt:
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

        items = await asyncio.to_thread(self._fetch_prices, filters, 20)

        prices: list[NormalizedPrice] = []
        for item in items:
            product_name = item.get("productName", "")

            # For SPOT term, filter to only SKUs containing "Spot"
            if term == PricingTerm.SPOT:
                sku_name = item.get("skuName", "")
                if "Spot" not in sku_name:
                    continue
            elif term == PricingTerm.ON_DEMAND:
                # Exclude Spot variants from on-demand results
                sku_name = item.get("skuName", "")
                if "Spot" in sku_name or "Low Priority" in sku_name:
                    continue
                # OS filtering: Linux = no "Windows" in productName
                if os.lower() == "linux" and "Windows" in product_name:
                    continue
                # Windows filtering: only include Windows productNames
                if os.lower() == "windows" and "Windows" not in product_name:
                    continue

            p = self._item_to_price(item, region, term, "compute")
            if p:
                prices.append(p)

        # Reservation prices from the API are the total upfront/annual cost, not per-hour.
        # Convert to per-hour so all terms are comparable.
        if term == PricingTerm.RESERVED_1YR:
            hours = Decimal("8760")
            prices = [p.model_copy(update={"price_per_unit": p.price_per_unit / hours}) for p in prices]
        elif term == PricingTerm.RESERVED_3YR:
            hours = Decimal("26280")
            prices = [p.model_copy(update={"price_per_unit": p.price_per_unit / hours}) for p in prices]

        # Sort by price ascending so the cheapest canonical result appears first
        prices.sort(key=lambda p: p.price_per_unit)

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

        is_premium_ssd = product_name in _PREMIUM_SSD_PRODUCT_NAMES

        # Cache the full tier list, not size-specific slices
        cache_extras = {"storage_type": storage_type}
        cached = await self._cache.get_prices("azure", "storage", region, cache_extras)
        if cached is not None:
            all_prices = [NormalizedPrice.model_validate(p) for p in cached]
            return self._filter_storage_by_size(all_prices, is_premium_ssd, size_gb)

        filters: dict[str, str] = {
            "armRegionName": region,
            "priceType": "Consumption",
            "productName": product_name,
        }

        # Fetch enough items to cover all P-series tiers (14 tiers × LRS/ZRS = ~28)
        max_results = 50 if is_premium_ssd else 10
        items = await asyncio.to_thread(self._fetch_prices, filters, max_results)

        prices: list[NormalizedPrice] = []
        for item in items:
            p = self._item_to_price(item, region, PricingTerm.ON_DEMAND, "storage")
            if p is None:
                continue
            if is_premium_ssd:
                # Premium SSD tiers are flat monthly fees — unit is per_month
                p = p.model_copy(update={"unit": PriceUnit.PER_MONTH})
            else:
                p = p.model_copy(update={"unit": PriceUnit.PER_GB_MONTH})
            prices.append(p)

        await self._cache.set_prices(
            "azure", "storage", region, cache_extras,
            [p.model_dump(mode="json") for p in prices],
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return self._filter_storage_by_size(prices, is_premium_ssd, size_gb)

    @staticmethod
    def _filter_storage_by_size(
        prices: list[NormalizedPrice],
        is_premium_ssd: bool,
        size_gb: float | None,
    ) -> list[NormalizedPrice]:
        """For Premium SSD with a known size_gb, return only the matching P-tier."""
        if not is_premium_ssd or size_gb is None:
            return prices
        target_tier = _select_premium_ssd_tier(size_gb)
        # skuName for a Premium SSD tier looks like "P20 LRS" or "P20 ZRS"
        tier_prefix = target_tier + " "
        filtered = [
            p for p in prices
            if p.description.startswith(tier_prefix)
            # LRS is the standard redundancy option
            and "ZRS" not in p.description
        ]
        if not filtered:
            # Fallback: return any matching tier prefix (without ZRS filter)
            filtered = [p for p in prices if p.description.startswith(tier_prefix)]
        if not filtered:
            return prices  # couldn't filter — return all
        # Annotate with tier info
        tier_gb = next(cap for name, cap in _PREMIUM_SSD_TIERS if name == target_tier)
        return [
            p.model_copy(update={
                "description": f"{p.description} ({target_tier}: up to {tier_gb} GiB)",
                "attributes": {
                    **p.attributes,
                    "disk_tier": target_tier,
                    "tier_capacity_gib": str(tier_gb),
                    "requested_size_gb": str(size_gb),
                },
            })
            for p in filtered
        ]

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

    # ------------------------------------------------------------------
    # v0.8.0 capability surface — supports / get_price / describe_catalog
    # ------------------------------------------------------------------

    def supports(self, domain: PricingDomain, service: str | None = None) -> bool:
        key = (domain.value if hasattr(domain, "value") else domain, service)
        return _AZURE_CAPABILITIES.get(key, False)

    def supported_terms(
        self, domain: PricingDomain, service: str | None = None
    ) -> list[PricingTerm]:
        dv = domain.value if hasattr(domain, "value") else domain
        if dv == PricingDomain.COMPUTE.value:
            return [
                PricingTerm.ON_DEMAND,
                PricingTerm.SPOT,
                PricingTerm.RESERVED_1YR,
                PricingTerm.RESERVED_3YR,
            ]
        return [PricingTerm.ON_DEMAND]

    async def get_price(self, spec: PricingSpec) -> PricingResult:
        if not self.supports(spec.domain, spec.service):
            raise NotSupportedError(
                provider=self.provider,
                domain=spec.domain,
                service=spec.service,
                reason=f"Azure does not support {spec.domain.value}/{spec.service}. "
                       "Azure currently covers compute (VMs) and storage (managed disks, blob).",
                alternatives=[
                    "Use describe_catalog(provider='azure') to see supported services.",
                    "For Azure AI services, Azure OpenAI is not yet implemented.",
                ],
                example_invocation={
                    "provider": "azure", "domain": "compute",
                    "resource_type": "Standard_D4s_v3", "region": "eastus",
                },
            )
        if isinstance(spec, ComputePricingSpec):
            public_prices = await self._price_compute(spec)
        elif isinstance(spec, StoragePricingSpec):
            public_prices = await self._price_storage(spec)
        else:
            raise NotSupportedError(
                provider=self.provider,
                domain=spec.domain,
                service=spec.service,
                reason=f"Azure does not implement domain={spec.domain.value} yet.",
            )
        return PricingResult(
            public_prices=public_prices,
            auth_available=False,
            source="catalog",
        )

    async def _price_compute(self, spec: ComputePricingSpec) -> list[NormalizedPrice]:
        resource_type = spec.resource_type or "Standard_D4s_v3"
        os_type = spec.os or "Linux"
        return await self.get_compute_price(resource_type, spec.region, os_type, spec.term)

    async def _price_storage(self, spec: StoragePricingSpec) -> list[NormalizedPrice]:
        storage_type = spec.storage_type or "premium-ssd"
        return await self.get_storage_price(storage_type, spec.region, spec.size_gb)

    async def describe_catalog(self) -> ProviderCatalog:
        return ProviderCatalog(
            provider=CloudProvider.AZURE,
            domains=[PricingDomain.COMPUTE, PricingDomain.STORAGE],
            services={
                "compute": ["vm"],
                "storage": ["managed_disks", "blob"],
            },
            supported_terms={
                "compute/vm": ["on_demand", "spot", "reserved_1yr", "reserved_3yr"],
                "storage/managed_disks": ["on_demand"],
                "storage/blob": ["on_demand"],
            },
            filter_hints={
                "compute/vm": {
                    "resource_type": "Azure VM size e.g. 'Standard_D4s_v3', 'Standard_E8s_v3'",
                    "os": "'Linux' (default) or 'Windows'",
                    "term": "on_demand | spot | reserved_1yr | reserved_3yr",
                },
                "storage/managed_disks": {
                    "storage_type": "premium-ssd | standard-ssd | standard-hdd | ultra-ssd",
                    "size_gb": "Disk size for monthly estimate",
                },
                "storage/blob": {
                    "storage_type": "blob",
                    "size_gb": "Storage volume for monthly estimate",
                },
            },
            example_invocations={
                "compute/vm": {
                    "provider": "azure", "domain": "compute",
                    "resource_type": "Standard_D4s_v3", "region": "eastus",
                    "os": "Linux", "term": "on_demand",
                },
                "storage/managed_disks": {
                    "provider": "azure", "domain": "storage",
                    "storage_type": "premium-ssd", "region": "eastus", "size_gb": 128,
                },
            },
            decision_matrix={
                "Azure VMs": "compute/vm",
                "Virtual Machines": "compute/vm",
                "Azure Managed Disks": "storage/managed_disks",
                "Azure Blob Storage": "storage/blob",
                "Premium SSD": "storage/managed_disks — set storage_type='premium-ssd'",
                "Standard SSD": "storage/managed_disks — set storage_type='standard-ssd'",
            },
        )

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
