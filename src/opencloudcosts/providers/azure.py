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
import re
from decimal import Decimal
from typing import Any

import httpx

from opencloudcosts.cache import CacheManager
from opencloudcosts.config import Settings
from opencloudcosts.models import (
    AiPricingSpec,
    CloudProvider,
    ComputePricingSpec,
    ContainerPricingSpec,
    DatabasePricingSpec,
    EffectivePrice,
    EgressPricingSpec,
    InstanceTypeInfo,
    NormalizedPrice,
    PriceUnit,
    PricingDomain,
    PricingResult,
    PricingSpec,
    PricingTerm,
    ProviderCatalog,
    ServerlessPricingSpec,
    StoragePricingSpec,
)
from opencloudcosts.providers.base import NotConfiguredError, NotSupportedError, ProviderBase
from opencloudcosts.utils.http_retry import sync_retry
from opencloudcosts.utils.regions import list_azure_regions

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
    (PricingDomain.DATABASE.value, None): True,
    (PricingDomain.DATABASE.value, "sql"): True,
    (PricingDomain.DATABASE.value, "cosmos"): True,
    (PricingDomain.CONTAINER.value, None): True,
    (PricingDomain.CONTAINER.value, "aks"): True,
    (PricingDomain.SERVERLESS.value, None): True,
    (PricingDomain.SERVERLESS.value, "azure_functions"): True,
    (PricingDomain.AI.value, None): True,
    (PricingDomain.AI.value, "openai"): True,
    (PricingDomain.INTER_REGION_EGRESS.value, None): True,
}

# Azure outbound bandwidth: base-tier per-GB rates by billing zone.
# Source: https://azure.microsoft.com/pricing/details/bandwidth/ (as of 2026-04)
# Zone 1: Americas + Europe. Zone 2: Asia Pacific. Zone 3: Middle East + Africa.
_AZURE_EGRESS_BASE_RATE: dict[str, Decimal] = {
    "zone1": Decimal("0.087"),   # 5 GB – 10 TB
    "zone2": Decimal("0.16"),    # 5 GB – 10 TB
    "zone3": Decimal("0.181"),   # 5 GB – 10 TB
}

# Map source armRegionName → billing zone (unlisted regions default to zone1)
_AZURE_EGRESS_ZONE: dict[str, str] = {
    # Zone 2: Asia Pacific
    "eastasia": "zone2", "southeastasia": "zone2",
    "japaneast": "zone2", "japanwest": "zone2",
    "australiaeast": "zone2", "australiasoutheast": "zone2",
    "centralindia": "zone2", "southindia": "zone2", "westindia": "zone2",
    "koreacentral": "zone2", "koreasouth": "zone2",
    "swedencentral": "zone2",
    # Zone 3: Middle East + Africa
    "uaenorth": "zone3", "uaecentral": "zone3",
    "southafricanorth": "zone3", "southafricawest": "zone3",
}

# Azure SQL tier keyword → API skuName prefix
_SQL_TIER_MAP: dict[str, str] = {
    "general purpose": "GP",
    "gp": "GP",
    "business critical": "BC",
    "bc": "BC",
    "hyperscale": "HS",
    "hs": "HS",
}

# Static fallback rates for Azure Functions Consumption plan (per Azure published pricing)
_FUNCTIONS_EXEC_RATE = Decimal("0.0000002")    # per execution
_FUNCTIONS_GB_SEC_RATE = Decimal("0.000016")   # per GB-second

# AKS control plane: Standard tier free, Premium (Uptime SLA) $0.10/cluster/hr
_AKS_PREMIUM_RATE = Decimal("0.10")

# Azure OpenAI model → (input $/1K tokens, output $/1K tokens) static fallback rates
# Used only when the Retail API returns no data for the model
_OPENAI_STATIC_RATES: dict[str, tuple[Decimal, Decimal]] = {
    "gpt-4o": (Decimal("0.005"), Decimal("0.015")),
    "gpt-4o-mini": (Decimal("0.00015"), Decimal("0.0006")),
    "gpt-4": (Decimal("0.03"), Decimal("0.06")),
    "gpt-4-32k": (Decimal("0.06"), Decimal("0.12")),
    "gpt-35-turbo": (Decimal("0.0005"), Decimal("0.0015")),
    "gpt-35-turbo-16k": (Decimal("0.001"), Decimal("0.002")),
    "o1": (Decimal("0.015"), Decimal("0.06")),
    "o1-mini": (Decimal("0.003"), Decimal("0.012")),
    "text-embedding-ada-002": (Decimal("0.0001"), Decimal("0.0001")),
    "text-embedding-3-small": (Decimal("0.00002"), Decimal("0.00002")),
    "text-embedding-3-large": (Decimal("0.00013"), Decimal("0.00013")),
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
        cached_meta = await self._cache.get_prices_with_meta("azure", "compute", region, cache_extras)
        if cached_meta is not None:
            cached_data, fetched_at = cached_meta
            return self._apply_cache_trust(
                [NormalizedPrice.model_validate(p) for p in cached_data],
                fetched_at, self._SOURCE_URL,
            )

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
        cached_meta = await self._cache.get_prices_with_meta("azure", "storage", region, cache_extras)
        if cached_meta is not None:
            cached_data, fetched_at = cached_meta
            all_prices = self._apply_cache_trust(
                [NormalizedPrice.model_validate(p) for p in cached_data],
                fetched_at, self._SOURCE_URL,
            )
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

    # ------------------------------------------------------------------
    # New services: SQL, Cosmos DB, AKS, Azure Functions, Azure OpenAI
    # ------------------------------------------------------------------

    async def get_sql_price(
        self,
        resource_type: str,
        region: str,
        engine: str = "SQL",
        deployment: str = "single-az",
        term: PricingTerm = PricingTerm.ON_DEMAND,
    ) -> list[NormalizedPrice]:
        """
        Price Azure SQL Database (vCore model).

        resource_type examples: "General Purpose 4 vCores", "Business Critical 8 vCores"
        engine: "SQL" (default), "MySQL" → Azure Database for MySQL, "PostgreSQL" → Azure DB for PostgreSQL
        deployment: "single-az" (LRS) | "ha" / "zone-redundant" (ZRS)
        """
        engine_lower = engine.lower()
        if "mysql" in engine_lower:
            service_name = "Azure Database for MySQL"
        elif "postgres" in engine_lower or "pg" in engine_lower:
            service_name = "Azure Database for PostgreSQL"
        else:
            service_name = "SQL Database"

        cache_extras = {"resource_type": resource_type, "engine": engine, "deployment": deployment, "term": term.value}
        cached_meta = await self._cache.get_prices_with_meta("azure", "sql", region, cache_extras)
        if cached_meta is not None:
            cached_data, fetched_at = cached_meta
            return self._apply_cache_trust(
                [NormalizedPrice.model_validate(p) for p in cached_data],
                fetched_at, self._SOURCE_URL,
            )

        price_type = "Reservation" if term in (PricingTerm.RESERVED_1YR, PricingTerm.RESERVED_3YR) else "Consumption"
        filters: dict[str, str] = {
            "armRegionName": region,
            "priceType": price_type,
            "serviceName": service_name,
        }
        if term == PricingTerm.RESERVED_1YR:
            filters["reservationTerm"] = "1 Year"
        elif term == PricingTerm.RESERVED_3YR:
            filters["reservationTerm"] = "3 Years"

        items = await asyncio.to_thread(self._fetch_prices, filters, 100)

        # Detect tier keyword and vCore count from resource_type
        rt_lower = resource_type.lower()
        tier_prefix = None
        for kw, prefix in _SQL_TIER_MAP.items():
            if kw in rt_lower:
                tier_prefix = prefix
                break

        # Extract vCore count (first integer in resource_type)
        vcores_match = re.search(r"\d+", resource_type)
        vcores_str = vcores_match.group() if vcores_match else None

        ha = deployment.lower() in ("ha", "zone-redundant", "multi-az", "regional")

        prices: list[NormalizedPrice] = []
        for item in items:
            sku_name = item.get("skuName", "")

            # Skip items not matching requested tier
            if tier_prefix and not sku_name.startswith(tier_prefix):
                continue
            # Skip items not matching vCore count
            if vcores_str and vcores_str not in sku_name:
                continue
            # Redundancy filter: ZRS = HA, LRS = single-az
            if ha and "ZRS" in sku_name:
                pass  # prefer ZRS for HA
            elif ha and "ZRS" not in sku_name and "LRS" in sku_name:
                continue  # skip LRS when HA requested
            elif not ha and "ZRS" in sku_name:
                continue  # skip ZRS when single-az requested

            unit = PriceUnit.PER_HOUR
            # Reserved items from API are total cost — divide to hourly
            price_obj = self._item_to_price(item, region, term, "sql")
            if price_obj is None:
                continue
            if term == PricingTerm.RESERVED_1YR:
                price_obj = price_obj.model_copy(update={"price_per_unit": price_obj.price_per_unit / Decimal("8760")})
            elif term == PricingTerm.RESERVED_3YR:
                price_obj = price_obj.model_copy(update={"price_per_unit": price_obj.price_per_unit / Decimal("26280")})
            price_obj = price_obj.model_copy(update={"unit": unit})
            prices.append(price_obj)

        prices.sort(key=lambda p: p.price_per_unit)
        await self._cache.set_prices(
            "azure", "sql", region, cache_extras,
            [p.model_dump(mode="json") for p in prices],
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return prices

    async def get_cosmos_price(
        self,
        region: str,
        deployment: str = "provisioned",
        multi_region: bool = False,
    ) -> list[NormalizedPrice]:
        """
        Price Azure Cosmos DB.

        deployment: "provisioned" (per 100 RU/s/hr) | "serverless" (per 1M RUs) | "autoscale"
        multi_region: True adds geo-replication write price.
        """
        cache_extras = {"deployment": deployment, "multi_region": str(multi_region)}
        cached_meta = await self._cache.get_prices_with_meta("azure", "cosmos", region, cache_extras)
        if cached_meta is not None:
            cached_data, fetched_at = cached_meta
            return self._apply_cache_trust(
                [NormalizedPrice.model_validate(p) for p in cached_data],
                fetched_at, self._SOURCE_URL,
            )

        filters: dict[str, str] = {
            "armRegionName": region,
            "priceType": "Consumption",
            "serviceName": "Azure Cosmos DB",
        }
        items = await asyncio.to_thread(self._fetch_prices, filters, 50)

        prices: list[NormalizedPrice] = []
        for item in items:
            meter = item.get("meterName", "").lower()
            product = item.get("productName", "").lower()

            if deployment == "serverless":
                if "serverless" not in meter and "serverless" not in product:
                    continue
                unit = PriceUnit.PER_UNIT
            elif deployment == "autoscale":
                if "autoscale" not in meter and "autoscale" not in product:
                    continue
                unit = PriceUnit.PER_UNIT
            else:
                # provisioned throughput
                if "100 ru/s" not in meter and "throughput" not in product:
                    continue
                # multi-region: include write replica items; single: exclude multi-write meters
                if multi_region and "single" in meter:
                    continue
                if not multi_region and ("multi" in meter or "write region" in meter):
                    continue
                unit = PriceUnit.PER_UNIT

            p = self._item_to_price(item, region, PricingTerm.ON_DEMAND, "cosmos")
            if p:
                prices.append(p.model_copy(update={"unit": unit}))

        prices.sort(key=lambda p: p.price_per_unit)
        await self._cache.set_prices(
            "azure", "cosmos", region, cache_extras,
            [p.model_dump(mode="json") for p in prices],
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return prices

    async def get_aks_price(
        self,
        region: str,
        mode: str = "standard",
    ) -> list[NormalizedPrice]:
        """
        Price AKS cluster management fee.

        mode: "free" (no SLA, free control plane) | "standard" (SLA, $0.10/cluster/hr)
        Worker node costs are separate — pass node instance type to get_compute_price.
        """
        cache_extras = {"mode": mode}
        cached_meta = await self._cache.get_prices_with_meta("azure", "aks", region, cache_extras)
        if cached_meta is not None:
            cached_data, fetched_at = cached_meta
            return self._apply_cache_trust(
                [NormalizedPrice.model_validate(p) for p in cached_data],
                fetched_at, self._SOURCE_URL,
            )

        prices: list[NormalizedPrice] = []

        if mode == "free":
            # Free tier — zero control-plane cost; return informational $0 price
            prices = [NormalizedPrice(
                provider=CloudProvider.AZURE,
                service="aks",
                sku_id="aks-free-tier",
                product_family="Containers",
                description="AKS Free tier — control plane included (no uptime SLA)",
                region=region,
                pricing_term=PricingTerm.ON_DEMAND,
                price_per_unit=Decimal("0"),
                unit=PriceUnit.PER_HOUR,
                attributes={
                    "note": "Worker node VMs billed separately via compute pricing",
                    "sla": "No uptime SLA",
                },
            )]
        else:
            # Try the Retail API first
            filters: dict[str, str] = {
                "armRegionName": region,
                "priceType": "Consumption",
                "serviceName": "Azure Kubernetes Service",
            }
            items = await asyncio.to_thread(self._fetch_prices, filters, 20)

            for item in items:
                meter = item.get("meterName", "").lower()
                if "uptime sla" not in meter and "standard" not in meter:
                    continue
                p = self._item_to_price(item, region, PricingTerm.ON_DEMAND, "aks")
                if p:
                    prices.append(p.model_copy(update={
                        "description": "AKS Standard tier — cluster management fee (Uptime SLA)",
                        "attributes": {
                            **p.attributes,
                            "note": "Worker node VMs billed separately via compute pricing",
                        },
                    }))

            if not prices:
                # Static fallback: $0.10/cluster/hr (published rate)
                prices = [NormalizedPrice(
                    provider=CloudProvider.AZURE,
                    service="aks",
                    sku_id="aks-standard-tier",
                    product_family="Containers",
                    description="AKS Standard tier — cluster management fee (Uptime SLA)",
                    region=region,
                    pricing_term=PricingTerm.ON_DEMAND,
                    price_per_unit=_AKS_PREMIUM_RATE,
                    unit=PriceUnit.PER_HOUR,
                    attributes={
                        "note": "Worker node VMs billed separately via compute pricing",
                        "source": "static_fallback",
                    },
                )]

        await self._cache.set_prices(
            "azure", "aks", region, cache_extras,
            [p.model_dump(mode="json") for p in prices],
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return prices

    async def get_functions_price(
        self,
        region: str,
        gb_seconds: float | None = None,
        requests_millions: float | None = None,
    ) -> list[NormalizedPrice]:
        """
        Price Azure Functions Consumption plan.

        Returns per-GB-second and per-execution prices from the Retail API,
        with static fallback. Free tier: 400K GB-s and 1M executions/month.
        """
        cache_extras: dict[str, str] = {}
        cached_meta = await self._cache.get_prices_with_meta("azure", "azure_functions", region, cache_extras)
        if cached_meta is not None:
            cached_data, fetched_at = cached_meta
            prices = self._apply_cache_trust(
                [NormalizedPrice.model_validate(p) for p in cached_data],
                fetched_at, self._SOURCE_URL,
            )
        else:
            filters: dict[str, str] = {
                "armRegionName": region,
                "priceType": "Consumption",
                "serviceName": "Azure Functions",
            }
            items = await asyncio.to_thread(self._fetch_prices, filters, 20)

            prices = []
            for item in items:
                meter = item.get("meterName", "").lower()
                uom = item.get("unitOfMeasure", "").lower()
                p = self._item_to_price(item, region, PricingTerm.ON_DEMAND, "azure_functions")
                if p is None:
                    continue
                if "execution" in meter or "invocation" in meter:
                    p = p.model_copy(update={"unit": PriceUnit.PER_REQUEST})
                elif "gb" in uom and "second" in uom:
                    p = p.model_copy(update={"unit": PriceUnit.PER_GB_SECOND})
                else:
                    continue
                prices.append(p)

            if not prices:
                # Static fallback from Azure published Consumption plan rates
                prices = [
                    NormalizedPrice(
                        provider=CloudProvider.AZURE,
                        service="azure_functions",
                        sku_id="functions-gb-second",
                        product_family="Serverless",
                        description="Azure Functions — compute duration (Consumption plan)",
                        region=region,
                        pricing_term=PricingTerm.ON_DEMAND,
                        price_per_unit=_FUNCTIONS_GB_SEC_RATE,
                        unit=PriceUnit.PER_GB_SECOND,
                        attributes={"free_tier": "400,000 GB-s/month", "source": "static_fallback"},
                    ),
                    NormalizedPrice(
                        provider=CloudProvider.AZURE,
                        service="azure_functions",
                        sku_id="functions-executions",
                        product_family="Serverless",
                        description="Azure Functions — execution count (Consumption plan)",
                        region=region,
                        pricing_term=PricingTerm.ON_DEMAND,
                        price_per_unit=_FUNCTIONS_EXEC_RATE,
                        unit=PriceUnit.PER_REQUEST,
                        attributes={"free_tier": "1M executions/month", "source": "static_fallback"},
                    ),
                ]

            await self._cache.set_prices(
                "azure", "azure_functions", region, cache_extras,
                [p.model_dump(mode="json") for p in prices],
                ttl_hours=self._settings.cache_ttl_hours,
            )

        # If volume provided, add a cost estimate price entry
        if gb_seconds or requests_millions:
            gb_sec_price = next((p for p in prices if p.unit == PriceUnit.PER_GB_SECOND), None)
            req_price = next((p for p in prices if p.unit == PriceUnit.PER_REQUEST), None)
            total = Decimal("0")
            breakdown: list[str] = []
            if gb_sec_price and gb_seconds:
                billable = max(0.0, gb_seconds - 400_000)
                cost = gb_sec_price.price_per_unit * Decimal(str(billable))
                total += cost
                breakdown.append(
                    f"{billable:,.0f} GB-s × ${float(gb_sec_price.price_per_unit):.6f} = ${float(cost):.4f}"
                )
            if req_price and requests_millions:
                req_count = requests_millions * 1_000_000
                billable_req = max(0.0, req_count - 1_000_000)
                cost = req_price.price_per_unit * Decimal(str(billable_req))
                total += cost
                breakdown.append(
                    f"{billable_req:,.0f} executions × ${float(req_price.price_per_unit):.8f} = ${float(cost):.4f}"
                )
            if breakdown:
                prices.append(NormalizedPrice(
                    provider=CloudProvider.AZURE,
                    service="azure_functions",
                    sku_id="functions-estimate",
                    product_family="Serverless",
                    description="Azure Functions — estimated monthly cost",
                    region=region,
                    pricing_term=PricingTerm.ON_DEMAND,
                    price_per_unit=total,
                    unit=PriceUnit.PER_UNIT,
                    attributes={
                        "breakdown": "; ".join(breakdown),
                        "note": "After free tier deduction (400K GB-s, 1M executions/month)",
                    },
                ))

        return prices

    async def get_openai_price(
        self,
        model: str,
        region: str,
        input_tokens: int | None = None,
        output_tokens: int | None = None,
    ) -> list[NormalizedPrice]:
        """
        Price Azure OpenAI model inference.

        Returns per-1K-token input and output prices.
        Falls back to static rates when the Retail API has no data.
        """
        model_lower = model.lower()
        cache_extras = {"model": model_lower}
        cached_meta = await self._cache.get_prices_with_meta("azure", "openai", region, cache_extras)
        if cached_meta is not None:
            cached_data, fetched_at = cached_meta
            prices = self._apply_cache_trust(
                [NormalizedPrice.model_validate(p) for p in cached_data],
                fetched_at, self._SOURCE_URL,
            )
        else:
            filters: dict[str, str] = {
                "armRegionName": region,
                "priceType": "Consumption",
                "serviceName": "Azure OpenAI",
            }
            items = await asyncio.to_thread(self._fetch_prices, filters, 100)

            prices = []
            for item in items:
                meter = item.get("meterName", "").lower()
                product = item.get("productName", "").lower()
                if model_lower.replace("-", " ") not in product and model_lower.replace("-", " ") not in meter:
                    continue
                p = self._item_to_price(item, region, PricingTerm.ON_DEMAND, "openai")
                if p:
                    prices.append(p.model_copy(update={"unit": PriceUnit.PER_UNIT}))

            if not prices:
                # Static fallback using known published rates
                in_rate, out_rate = None, None
                for key, (ir, or_) in _OPENAI_STATIC_RATES.items():
                    if key in model_lower:
                        in_rate, out_rate = ir, or_
                        break
                if in_rate is not None:
                    prices = [
                        NormalizedPrice(
                            provider=CloudProvider.AZURE,
                            service="openai",
                            sku_id=f"openai-{model_lower}-input",
                            product_family="AI + Machine Learning",
                            description=f"Azure OpenAI {model} — input tokens",
                            region=region,
                            pricing_term=PricingTerm.ON_DEMAND,
                            price_per_unit=in_rate,
                            unit=PriceUnit.PER_UNIT,
                            attributes={
                                "model": model,
                                "token_type": "input",
                                "unit_label": "per 1K tokens",
                                "source": "static_fallback",
                            },
                        ),
                        NormalizedPrice(
                            provider=CloudProvider.AZURE,
                            service="openai",
                            sku_id=f"openai-{model_lower}-output",
                            product_family="AI + Machine Learning",
                            description=f"Azure OpenAI {model} — output tokens",
                            region=region,
                            pricing_term=PricingTerm.ON_DEMAND,
                            price_per_unit=out_rate,
                            unit=PriceUnit.PER_UNIT,
                            attributes={
                                "model": model,
                                "token_type": "output",
                                "unit_label": "per 1K tokens",
                                "source": "static_fallback",
                            },
                        ),
                    ]

            await self._cache.set_prices(
                "azure", "openai", region, cache_extras,
                [p.model_dump(mode="json") for p in prices],
                ttl_hours=self._settings.cache_ttl_hours,
            )

        # Cost estimate if token volumes provided
        if prices and (input_tokens or output_tokens):
            in_price = next((p for p in prices if p.attributes.get("token_type") == "input"
                             or "input" in p.description.lower()), None)
            out_price = next((p for p in prices if p.attributes.get("token_type") == "output"
                              or "output" in p.description.lower()), None)
            total = Decimal("0")
            breakdown: list[str] = []
            if in_price and input_tokens:
                cost = in_price.price_per_unit * Decimal(str(input_tokens)) / Decimal("1000")
                total += cost
                breakdown.append(
                    f"{input_tokens:,} input tokens × ${float(in_price.price_per_unit):.5f}/1K = ${float(cost):.4f}"
                )
            if out_price and output_tokens:
                cost = out_price.price_per_unit * Decimal(str(output_tokens)) / Decimal("1000")
                total += cost
                breakdown.append(
                    f"{output_tokens:,} output tokens × ${float(out_price.price_per_unit):.5f}/1K = ${float(cost):.4f}"
                )
            if breakdown:
                prices.append(NormalizedPrice(
                    provider=CloudProvider.AZURE,
                    service="openai",
                    sku_id=f"openai-{model_lower}-estimate",
                    product_family="AI + Machine Learning",
                    description=f"Azure OpenAI {model} — estimated cost",
                    region=region,
                    pricing_term=PricingTerm.ON_DEMAND,
                    price_per_unit=total,
                    unit=PriceUnit.PER_UNIT,
                    attributes={
                        "breakdown": "; ".join(breakdown),
                        "model": model,
                    },
                ))

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

    _MAJOR_REGIONS = [
        "eastus", "eastus2", "westus", "westus2", "westus3",
        "centralus", "northeurope", "westeurope", "uksouth",
        "eastasia", "southeastasia", "australiaeast",
    ]
    _SOURCE_URL = "https://prices.azure.com/api/retail/prices"

    def major_regions(self) -> list[str]:
        return self._MAJOR_REGIONS

    def default_region(self) -> str:
        return "eastus"

    async def get_price(self, spec: PricingSpec) -> PricingResult:
        if not self.supports(spec.domain, spec.service):
            raise NotSupportedError(
                provider=self.provider,
                domain=spec.domain,
                service=spec.service,
                reason=(
                    f"Azure does not support {spec.domain.value}/{spec.service}. "
                    "Azure supports: compute (vm), storage (managed_disks, blob), "
                    "database (sql, cosmos), container (aks), serverless (azure_functions), "
                    "ai (openai), inter_region_egress."
                ),
                alternatives=["Use describe_catalog(provider='azure') to see supported services."],
                example_invocation={
                    "provider": "azure", "domain": "compute",
                    "resource_type": "Standard_D4s_v3", "region": "eastus",
                },
            )
        if isinstance(spec, ComputePricingSpec):
            public_prices = await self._price_compute(spec)
        elif isinstance(spec, StoragePricingSpec):
            public_prices = await self._price_storage(spec)
        elif isinstance(spec, DatabasePricingSpec):
            public_prices = await self._price_database(spec)
        elif isinstance(spec, ContainerPricingSpec):
            public_prices = await self._price_container(spec)
        elif isinstance(spec, ServerlessPricingSpec):
            public_prices = await self._price_serverless(spec)
        elif isinstance(spec, AiPricingSpec):
            public_prices = await self._price_ai(spec)
        elif isinstance(spec, EgressPricingSpec):
            public_prices = await self._price_egress(spec)
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

    async def _price_database(self, spec: DatabasePricingSpec) -> list[NormalizedPrice]:
        svc = (spec.service or "sql").lower()
        if svc == "cosmos":
            multi = spec.deployment.lower() in ("multi-az", "ha", "regional", "multi-region")
            return await self.get_cosmos_price(spec.region, deployment="provisioned", multi_region=multi)
        # Default: Azure SQL / MySQL / PostgreSQL
        return await self.get_sql_price(
            resource_type=spec.resource_type or "General Purpose 4 vCores",
            region=spec.region,
            engine=spec.engine,
            deployment=spec.deployment,
            term=spec.term,
        )

    async def _price_container(self, spec: ContainerPricingSpec) -> list[NormalizedPrice]:
        mode = spec.mode or "standard"
        return await self.get_aks_price(spec.region, mode=mode)

    async def _price_serverless(self, spec: ServerlessPricingSpec) -> list[NormalizedPrice]:
        return await self.get_functions_price(
            spec.region,
            gb_seconds=spec.gb_seconds,
            requests_millions=spec.requests_millions,
        )

    async def _price_ai(self, spec: AiPricingSpec) -> list[NormalizedPrice]:
        model = spec.model or "gpt-4o"
        return await self.get_openai_price(
            model=model,
            region=spec.region,
            input_tokens=spec.input_tokens,
            output_tokens=spec.output_tokens,
        )

    async def _price_egress(self, spec: EgressPricingSpec) -> list[NormalizedPrice]:
        src = spec.source_region or spec.region or "eastus"
        return await self.get_egress_price(src, spec.dest_region, spec.data_gb)

    async def get_egress_price(
        self,
        source_region: str,
        dest_region: str = "",
        data_gb: float = 1.0,
    ) -> list[NormalizedPrice]:
        """Azure outbound data transfer pricing (internet egress or inter-region).

        Azure bandwidth is billed per zone, not per region pair. Zone 1 covers
        Americas and Europe; Zone 2 covers Asia Pacific; Zone 3 covers Middle East
        and Africa. The first 5 GB/month outbound is free.
        """
        zone = _AZURE_EGRESS_ZONE.get(source_region.lower(), "zone1")
        cache_key = f"azure:egress:{zone}:rate"
        rate: Decimal | None = None

        cached = await self._cache.get_metadata(cache_key)
        if cached is not None:
            try:
                rate = Decimal(cached["rate"])
            except Exception:
                pass

        if rate is None:
            try:
                zone_label = zone.replace("zone", "zone ")  # "zone1" → "zone 1"
                items = await asyncio.to_thread(
                    self._fetch_prices,
                    {"serviceName": "Bandwidth"},
                    max_results=100,
                )
                # The API returns multiple tier rows for each zone; pick the highest
                # per-GB price, which is the base (lowest-volume) tier.
                zone_rates: list[Decimal] = []
                for item in items:
                    meter = item.get("meterName", "").lower()
                    if "out" in meter and zone_label in meter:
                        r = Decimal(str(item.get("retailPrice", 0)))
                        if r > 0:
                            zone_rates.append(r)
                if zone_rates:
                    rate = max(zone_rates)
            except Exception as exc:
                logger.warning("Azure: egress price fetch failed: %s", exc)

            if rate is None:
                rate = _AZURE_EGRESS_BASE_RATE.get(zone, Decimal("0.087"))
            await self._cache.set_metadata(
                cache_key, {"rate": str(rate)},
                ttl_hours=self._settings.cache_ttl_hours,
            )

        data_dec = Decimal(str(max(0.0, data_gb)))
        free_gb = Decimal("5")
        chargeable = max(Decimal("0"), data_dec - free_gb)
        monthly = chargeable * rate

        egress_type = "inter-region" if dest_region else "internet"
        desc = f"Azure outbound data transfer ({egress_type}) from {source_region}"
        if dest_region:
            desc += f" to {dest_region}"

        attrs: dict[str, str] = {
            "source_region": source_region,
            "egress_type": egress_type,
            "zone": zone,
            "free_tier_gb": "5",
            "note": f"First 5 GB/month free. {zone.upper()} rate (base tier, 5 GB–10 TB).",
        }
        if dest_region:
            attrs["dest_region"] = dest_region
        if data_gb > 0:
            attrs["monthly_estimate"] = (
                f"${float(monthly):.4f} for {data_gb:.1f} GB "
                f"({float(chargeable):.1f} GB chargeable)"
            )

        return [NormalizedPrice(
            provider=CloudProvider.AZURE,
            service="inter_region_egress",
            sku_id=f"azure:egress:{source_region}:{dest_region}:{zone}",
            product_family="Bandwidth",
            description=desc,
            region=source_region,
            attributes=attrs,
            pricing_term=PricingTerm.ON_DEMAND,
            price_per_unit=rate,
            unit=PriceUnit.PER_GB_MONTH,
        )]

    async def describe_catalog(self) -> ProviderCatalog:
        return ProviderCatalog(
            provider=CloudProvider.AZURE,
            domains=[
                PricingDomain.COMPUTE, PricingDomain.STORAGE,
                PricingDomain.DATABASE, PricingDomain.CONTAINER,
                PricingDomain.SERVERLESS, PricingDomain.AI,
                PricingDomain.INTER_REGION_EGRESS,
            ],
            services={
                "compute": ["vm"],
                "storage": ["managed_disks", "blob"],
                "database": ["sql", "cosmos"],
                "container": ["aks"],
                "serverless": ["azure_functions"],
                "ai": ["openai"],
                "inter_region_egress": [],
            },
            supported_terms={
                "compute/vm": ["on_demand", "spot", "reserved_1yr", "reserved_3yr"],
                "storage/managed_disks": ["on_demand"],
                "storage/blob": ["on_demand"],
                "database/sql": ["on_demand", "reserved_1yr", "reserved_3yr"],
                "database/cosmos": ["on_demand"],
                "container/aks": ["on_demand"],
                "serverless/azure_functions": ["on_demand"],
                "ai/openai": ["on_demand"],
                "inter_region_egress": ["on_demand"],
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
                "database/sql": {
                    "resource_type": "'General Purpose 4 vCores' | 'Business Critical 8 vCores' | 'Hyperscale 2 vCores'",
                    "engine": "'SQL' (default) | 'MySQL' | 'PostgreSQL'",
                    "deployment": "single-az (default) | ha",
                    "term": "on_demand | reserved_1yr | reserved_3yr",
                },
                "database/cosmos": {
                    "deployment": "'provisioned' (per 100 RU/s) | 'serverless' (per 1M RUs) | 'autoscale'",
                    "note": "Multi-region: set deployment='ha' to include geo-replication write pricing",
                },
                "container/aks": {
                    "mode": "'standard' ($0.10/hr, Uptime SLA) | 'free' (no SLA, control plane free)",
                    "note": "Worker nodes billed separately — add VM compute line items for nodes",
                },
                "serverless/azure_functions": {
                    "gb_seconds": "Execution duration × memory in GB (e.g. 500000 for 500K GB-s/month)",
                    "requests_millions": "Execution count in millions (e.g. 5 for 5M invocations/month)",
                    "note": "Free tier: 400K GB-s and 1M executions/month; Consumption plan only",
                },
                "ai/openai": {
                    "model": "gpt-4o | gpt-4o-mini | gpt-4 | gpt-35-turbo | o1 | o1-mini | text-embedding-3-small",
                    "input_tokens": "Input token count for cost estimate",
                    "output_tokens": "Output token count for cost estimate",
                },
                "inter_region_egress": {
                    "source_region": "Origin Azure region e.g. 'eastus', 'westeurope'",
                    "dest_region": "Destination region (optional); empty = internet egress",
                    "data_gb": "Data volume in GB for monthly cost estimate",
                    "note": "Zone 1 rate (Americas + Europe). First 5 GB/month free.",
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
                "database/sql": {
                    "provider": "azure", "domain": "database", "service": "sql",
                    "resource_type": "General Purpose 4 vCores", "engine": "SQL",
                    "deployment": "single-az", "region": "eastus",
                },
                "database/cosmos": {
                    "provider": "azure", "domain": "database", "service": "cosmos",
                    "deployment": "provisioned", "region": "eastus",
                },
                "container/aks": {
                    "provider": "azure", "domain": "container", "service": "aks",
                    "mode": "standard", "region": "eastus",
                },
                "serverless/azure_functions": {
                    "provider": "azure", "domain": "serverless", "service": "azure_functions",
                    "gb_seconds": 500000, "requests_millions": 5, "region": "eastus",
                },
                "ai/openai": {
                    "provider": "azure", "domain": "ai", "service": "openai",
                    "model": "gpt-4o", "input_tokens": 1000000, "output_tokens": 500000,
                    "region": "eastus",
                },
                "inter_region_egress": {
                    "provider": "azure", "domain": "inter_region_egress",
                    "source_region": "eastus", "dest_region": "westeurope", "data_gb": 1000,
                },
            },
            decision_matrix={
                "Azure VMs": "compute/vm",
                "Virtual Machines": "compute/vm",
                "Azure Managed Disks": "storage/managed_disks",
                "Azure Blob Storage": "storage/blob",
                "Premium SSD": "storage/managed_disks — set storage_type='premium-ssd'",
                "Standard SSD": "storage/managed_disks — set storage_type='standard-ssd'",
                "Azure SQL Database": "database/sql — set engine='SQL'",
                "Azure Database for MySQL": "database/sql — set engine='MySQL'",
                "Azure Database for PostgreSQL": "database/sql — set engine='PostgreSQL'",
                "Azure Cosmos DB": "database/cosmos",
                "AKS / Azure Kubernetes Service": "container/aks",
                "Azure Functions": "serverless/azure_functions",
                "Azure OpenAI": "ai/openai — set model='gpt-4o' or other model",
                "Azure Bandwidth": "inter_region_egress — set source_region and data_gb",
                "Azure Data Transfer": "inter_region_egress — set source_region, dest_region (optional), data_gb",
                "Azure Egress": "inter_region_egress — set source_region, data_gb",
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
