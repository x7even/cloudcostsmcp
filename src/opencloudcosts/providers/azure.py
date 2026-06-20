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
    NetworkPricingSpec,
    NormalizedPrice,
    ObservabilityPricingSpec,
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
from opencloudcosts.utils.egress_tiers import EgressTier, compute_tiered_cost
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
    "gp3": "Premium SSD Managed Disks",  # AWS gp3 → closest Azure equivalent
    "gp2": "Standard SSD Managed Disks",
    "io1": "Premium SSD Managed Disks",
    "pd-ssd": "Premium SSD Managed Disks",  # GCP alias
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
    ("P1", 4),
    ("P2", 8),
    ("P3", 16),
    ("P4", 32),
    ("P6", 64),
    ("P10", 128),
    ("P15", 256),
    ("P20", 512),
    ("P30", 1024),
    ("P40", 2048),
    ("P50", 4096),
    ("P60", 8192),
    ("P70", 16384),
    ("P80", 32767),
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
    (PricingDomain.NETWORK.value, None): True,
    (PricingDomain.NETWORK.value, "egress"): True,
    (PricingDomain.INTER_REGION_EGRESS.value, None): True,
    # Azure Monitor (Observability domain)
    (PricingDomain.OBSERVABILITY.value, None): True,
    (PricingDomain.OBSERVABILITY.value, "azure_monitor"): True,
    # Azure CDN + Front Door (Network domain)
    (PricingDomain.NETWORK.value, None): True,
    (PricingDomain.NETWORK.value, "azure_cdn"): True,
    (PricingDomain.NETWORK.value, "azure_front_door"): True,
}

# Azure outbound bandwidth: base-tier per-GB rates by billing zone.
# Source: https://azure.microsoft.com/pricing/details/bandwidth/ (as of 2026-04)
# Zone 1: Americas + Europe. Zone 2: Asia Pacific. Zone 3: Middle East + Africa.
_AZURE_EGRESS_BASE_RATE: dict[str, Decimal] = {
    "zone1": Decimal("0.087"),  # 5 GB – 10 TB
    "zone2": Decimal("0.16"),  # 5 GB – 10 TB
    "zone3": Decimal("0.181"),  # 5 GB – 10 TB
}

# Azure internet egress tiers by billing zone.
# Source: https://azure.microsoft.com/en-us/pricing/details/bandwidth/
# Tiers: 0-100 GB free, then tiered at 100 GB, ~10 TB, ~50 TB, ~150 TB, ~500 TB.
# Used as static fallback for the tiered network/egress path.
_AZURE_INTERNET_EGRESS_TIERS: dict[str, list[EgressTier]] = {
    "zone1": [
        EgressTier(threshold_gb=0, rate=Decimal("0.000"), label="0-5 GB (free)"),
        EgressTier(threshold_gb=5, rate=Decimal("0.087"), label="5 GB-10 TB"),
        EgressTier(threshold_gb=10_240, rate=Decimal("0.083"), label="10-50 TB"),
        EgressTier(threshold_gb=51_200, rate=Decimal("0.070"), label="50-150 TB"),
        EgressTier(threshold_gb=153_600, rate=Decimal("0.050"), label="150-500 TB"),
        EgressTier(threshold_gb=512_000, rate=Decimal("0.050"), label=">500 TB"),
    ],
    "zone2": [
        EgressTier(threshold_gb=0, rate=Decimal("0.000"), label="0-5 GB (free)"),
        EgressTier(threshold_gb=5, rate=Decimal("0.120"), label="5 GB-10 TB"),
        EgressTier(threshold_gb=10_240, rate=Decimal("0.085"), label="10-50 TB"),
        EgressTier(threshold_gb=51_200, rate=Decimal("0.082"), label="50-150 TB"),
        EgressTier(threshold_gb=153_600, rate=Decimal("0.080"), label="150-500 TB"),
        EgressTier(threshold_gb=512_000, rate=Decimal("0.080"), label=">500 TB"),
    ],
    "zone3": [
        EgressTier(threshold_gb=0, rate=Decimal("0.000"), label="0-5 GB (free)"),
        EgressTier(threshold_gb=5, rate=Decimal("0.181"), label="5 GB-10 TB"),
        EgressTier(threshold_gb=10_240, rate=Decimal("0.175"), label="10-50 TB"),
        EgressTier(threshold_gb=51_200, rate=Decimal("0.170"), label="50-150 TB"),
        EgressTier(threshold_gb=153_600, rate=Decimal("0.160"), label="150-500 TB"),
        EgressTier(threshold_gb=512_000, rate=Decimal("0.160"), label=">500 TB"),
    ],
}

# Azure cross-region inter-region flat rate by zone
_AZURE_CROSS_REGION_RATE: dict[str, Decimal] = {
    "zone1": Decimal("0.02"),
    "zone2": Decimal("0.08"),
    "zone3": Decimal("0.16"),
}

# Azure cross-AZ flat rate
_AZURE_CROSS_AZ_RATE = Decimal("0.01")  # per GB each direction

_AZURE_EGRESS_SOURCE_URL = "https://azure.microsoft.com/en-us/pricing/details/bandwidth/"

# Map source armRegionName → billing zone (unlisted regions default to zone1)
_AZURE_EGRESS_ZONE: dict[str, str] = {
    # Zone 2: Asia Pacific
    "eastasia": "zone2",
    "southeastasia": "zone2",
    "japaneast": "zone2",
    "japanwest": "zone2",
    "australiaeast": "zone2",
    "australiasoutheast": "zone2",
    "centralindia": "zone2",
    "southindia": "zone2",
    "westindia": "zone2",
    "koreacentral": "zone2",
    "koreasouth": "zone2",
    # Zone 3: Middle East + Africa
    "uaenorth": "zone3",
    "uaecentral": "zone3",
    "southafricanorth": "zone3",
    "southafricawest": "zone3",
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
_FUNCTIONS_EXEC_RATE = Decimal("0.0000002")  # per execution
_FUNCTIONS_GB_SEC_RATE = Decimal("0.000016")  # per GB-second

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

# Azure Monitor static fallback rates (eastus, as of 2026-04)
# Source: https://azure.microsoft.com/en-us/pricing/details/monitor/
_MONITOR_LOG_ANALYTICS_RATE = Decimal("2.30")  # per GB Analytics Logs (after 5 GB free)
_MONITOR_BASIC_LOG_RATE = Decimal("0.50")  # per GB Basic Logs
_MONITOR_METRICS_RATE = Decimal("0.16")  # per 10M metric samples
_MONITOR_ALERT_METRIC_RATE = Decimal("0.10")  # per rule/month after 10 free
_MONITOR_FREE_LOG_GB = Decimal("5")  # 5 GB/month free tier
_MONITOR_FREE_ALERT_RULES = 10  # first 10 metric alert rules free

# Azure CDN / Front Door billing zone by source region.
# CDN: Zone 1=NA+EU, Zone 2=APAC+ME, Zone 3=India/South Asia,
#       Zone 4=South America, Zone 5=Australia/Pacific
_CDN_ZONE: dict[str, str] = {
    # Zone 2: APAC + Middle East
    "eastasia": "Zone 2",
    "southeastasia": "Zone 2",
    "japaneast": "Zone 2",
    "japanwest": "Zone 2",
    "koreacentral": "Zone 2",
    "koreasouth": "Zone 2",
    "uaenorth": "Zone 2",
    "uaecentral": "Zone 2",
    # Zone 3: India/South Asia
    "centralindia": "Zone 3",
    "southindia": "Zone 3",
    "westindia": "Zone 3",
    # Zone 4: South America
    "brazilsouth": "Zone 4",
    # Zone 5: Australia/Pacific
    "australiaeast": "Zone 5",
    "australiasoutheast": "Zone 5",
}
# Default (all Americas + Europe) = "Zone 1"

# Static fallback rates per GB for Azure CDN Standard (Microsoft), by billing zone.
# Zone 1: Americas + Europe. Zone 2: APAC + ME. Zone 3: India/South Asia.
# Zone 4: South America. Zone 5: Australia/Pacific.
# Source: https://azure.microsoft.com/en-us/pricing/details/cdn/
_CDN_STATIC_RATE_ZONE1 = Decimal("0.081")   # Zone 1, 0-10 TB tier
_CDN_STATIC_RATE_ZONE2 = Decimal("0.163")   # Zone 2
_CDN_STATIC_RATE_ZONE3 = Decimal("0.163")   # Zone 3
_CDN_STATIC_RATE_ZONE4 = Decimal("0.200")   # Zone 4
_CDN_STATIC_RATE_ZONE5 = Decimal("0.220")   # Zone 5
_CDN_STATIC_RATES: dict[str, Decimal] = {
    "Zone 1": _CDN_STATIC_RATE_ZONE1,
    "Zone 2": _CDN_STATIC_RATE_ZONE2,
    "Zone 3": _CDN_STATIC_RATE_ZONE3,
    "Zone 4": _CDN_STATIC_RATE_ZONE4,
    "Zone 5": _CDN_STATIC_RATE_ZONE5,
}
_FRONTDOOR_STATIC_RATE_ZONE1 = Decimal("0.0825")  # Zone 1, 0-10 TB tier Standard
_FRONTDOOR_STATIC_RATES: dict[str, Decimal] = {
    "Zone 1": Decimal("0.0825"),
    "Zone 2": Decimal("0.160"),
    "Zone 3": Decimal("0.160"),
    "Zone 4": Decimal("0.195"),
    "Zone 5": Decimal("0.210"),
}
_FRONTDOOR_REQ_STATIC_RATE_ZONE1 = Decimal("0.009")  # per 10K requests
_FRONTDOOR_REQ_STATIC_RATES: dict[str, Decimal] = {
    "Zone 1": Decimal("0.009"),
    "Zone 2": Decimal("0.012"),
    "Zone 3": Decimal("0.012"),
    "Zone 4": Decimal("0.014"),
    "Zone 5": Decimal("0.015"),
}


class AzureProvider(ProviderBase):
    provider = CloudProvider.AZURE

    def __init__(self, settings: Settings, cache: CacheManager) -> None:
        self._settings = settings
        self._cache = cache

    # ------------------------------------------------------------------
    # Internal HTTP helpers (synchronous — wrapped in asyncio.to_thread)
    # ------------------------------------------------------------------

    def _fetch_prices(
        self, filters: dict[str, str], max_results: int = 100
    ) -> list[dict[str, Any]]:
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
        cached_meta = await self._cache.get_prices_with_meta(
            "azure", "compute", region, cache_extras
        )
        if cached_meta is not None:
            cached_data, fetched_at = cached_meta
            return self._apply_cache_trust(
                [NormalizedPrice.model_validate(p) for p in cached_data],
                fetched_at,
                self._SOURCE_URL,
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
            prices = [
                p.model_copy(update={"price_per_unit": p.price_per_unit / hours}) for p in prices
            ]
        elif term == PricingTerm.RESERVED_3YR:
            hours = Decimal("26280")
            prices = [
                p.model_copy(update={"price_per_unit": p.price_per_unit / hours}) for p in prices
            ]

        # Sort by price ascending so the cheapest canonical result appears first
        prices.sort(key=lambda p: p.price_per_unit)

        await self._cache.set_prices(
            "azure",
            "compute",
            region,
            cache_extras,
            [p.model_dump(mode="json") for p in prices],
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return self._annotate_fresh(prices, self._SOURCE_URL)

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
        cached_meta = await self._cache.get_prices_with_meta(
            "azure", "storage", region, cache_extras
        )
        if cached_meta is not None:
            cached_data, fetched_at = cached_meta
            all_prices = self._apply_cache_trust(
                [NormalizedPrice.model_validate(p) for p in cached_data],
                fetched_at,
                self._SOURCE_URL,
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
            "azure",
            "storage",
            region,
            cache_extras,
            [p.model_dump(mode="json") for p in prices],
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return self._annotate_fresh(
            self._filter_storage_by_size(prices, is_premium_ssd, size_gb),
            self._SOURCE_URL,
        )

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
            p
            for p in prices
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
            p.model_copy(
                update={
                    "description": f"{p.description} ({target_tier}: up to {tier_gb} GiB)",
                    "attributes": {
                        **p.attributes,
                        "disk_tier": target_tier,
                        "tier_capacity_gib": str(tier_gb),
                        "requested_size_gb": str(size_gb),
                    },
                }
            )
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

        cache_extras = {
            "resource_type": resource_type,
            "engine": engine,
            "deployment": deployment,
            "term": term.value,
        }
        cached_meta = await self._cache.get_prices_with_meta("azure", "sql", region, cache_extras)
        if cached_meta is not None:
            cached_data, fetched_at = cached_meta
            return self._apply_cache_trust(
                [NormalizedPrice.model_validate(p) for p in cached_data],
                fetched_at,
                self._SOURCE_URL,
            )

        price_type = (
            "Reservation"
            if term in (PricingTerm.RESERVED_1YR, PricingTerm.RESERVED_3YR)
            else "Consumption"
        )
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
            if vcores_str and not re.search(rf"(?<!\d){re.escape(vcores_str)}(?!\d)", sku_name):
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
                price_obj = price_obj.model_copy(
                    update={"price_per_unit": price_obj.price_per_unit / Decimal("8760")}
                )
            elif term == PricingTerm.RESERVED_3YR:
                price_obj = price_obj.model_copy(
                    update={"price_per_unit": price_obj.price_per_unit / Decimal("26280")}
                )
            price_obj = price_obj.model_copy(update={"unit": unit})
            prices.append(price_obj)

        prices.sort(key=lambda p: p.price_per_unit)
        await self._cache.set_prices(
            "azure",
            "sql",
            region,
            cache_extras,
            [p.model_dump(mode="json") for p in prices],
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return self._annotate_fresh(prices, self._SOURCE_URL)

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
        cached_meta = await self._cache.get_prices_with_meta(
            "azure", "cosmos", region, cache_extras
        )
        if cached_meta is not None:
            cached_data, fetched_at = cached_meta
            return self._apply_cache_trust(
                [NormalizedPrice.model_validate(p) for p in cached_data],
                fetched_at,
                self._SOURCE_URL,
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
            "azure",
            "cosmos",
            region,
            cache_extras,
            [p.model_dump(mode="json") for p in prices],
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return self._annotate_fresh(prices, self._SOURCE_URL)

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
                fetched_at,
                self._SOURCE_URL,
            )

        prices: list[NormalizedPrice] = []

        if mode == "free":
            # Free tier — zero control-plane cost; return informational $0 price
            prices = [
                NormalizedPrice(
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
                )
            ]
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
                    prices.append(
                        p.model_copy(
                            update={
                                "description": "AKS Standard tier — cluster management fee (Uptime SLA)",
                                "attributes": {
                                    **p.attributes,
                                    "note": "Worker node VMs billed separately via compute pricing",
                                },
                            }
                        )
                    )

            if not prices:
                # Static fallback: $0.10/cluster/hr (published rate)
                prices = [
                    NormalizedPrice(
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
                    )
                ]

        await self._cache.set_prices(
            "azure",
            "aks",
            region,
            cache_extras,
            [p.model_dump(mode="json") for p in prices],
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return self._annotate_fresh(prices, self._SOURCE_URL)

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
        cached_meta = await self._cache.get_prices_with_meta(
            "azure", "azure_functions", region, cache_extras
        )
        if cached_meta is not None:
            cached_data, fetched_at = cached_meta
            prices = self._apply_cache_trust(
                [NormalizedPrice.model_validate(p) for p in cached_data],
                fetched_at,
                self._SOURCE_URL,
            )
        else:
            filters: dict[str, str] = {
                "armRegionName": region,
                "priceType": "Consumption",
                "serviceName": "Functions",
            }
            items = await asyncio.to_thread(self._fetch_prices, filters, 30)

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
                elif "vcpu" in meter:
                    # Premium plan: per-vCPU-hour
                    p = p.model_copy(
                        update={
                            "unit": PriceUnit.PER_HOUR,
                            "attributes": {
                                **p.attributes,
                                "plan": "premium",
                                "dimension": "vcpu_hour",
                            },
                        }
                    )
                elif "memory" in meter and "duration" in meter:
                    # Premium plan: per-GiB-hour
                    p = p.model_copy(
                        update={
                            "unit": PriceUnit.PER_HOUR,
                            "attributes": {
                                **p.attributes,
                                "plan": "premium",
                                "dimension": "gib_hour",
                            },
                        }
                    )
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
                        attributes={
                            "free_tier": "1M executions/month",
                            "source": "static_fallback",
                        },
                    ),
                ]

            await self._cache.set_prices(
                "azure",
                "azure_functions",
                region,
                cache_extras,
                [p.model_dump(mode="json") for p in prices],
                ttl_hours=self._settings.cache_ttl_hours,
            )
            prices = self._annotate_fresh(prices, self._SOURCE_URL)

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
                synthetic = NormalizedPrice(
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
                )
                # Inherit trust metadata from first component price
                if prices:
                    ref = prices[0]
                    synthetic = synthetic.model_copy(
                        update={
                            "fetched_at": ref.fetched_at,
                            "source_url": ref.source_url,
                            "cache_age_seconds": ref.cache_age_seconds,
                        }
                    )
                prices.append(synthetic)

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
        cached_meta = await self._cache.get_prices_with_meta(
            "azure", "openai", region, cache_extras
        )
        if cached_meta is not None:
            cached_data, fetched_at = cached_meta
            prices = self._apply_cache_trust(
                [NormalizedPrice.model_validate(p) for p in cached_data],
                fetched_at,
                self._SOURCE_URL,
            )
        else:
            filters: dict[str, str] = {
                "armRegionName": region,
                "priceType": "Consumption",
                "serviceName": "Foundry Models",
            }
            items = await asyncio.to_thread(self._fetch_prices, filters, 100)

            prices = []
            for item in items:
                meter = item.get("meterName", "").lower()
                product = item.get("productName", "").lower()
                sku = item.get("skuName", "").lower()
                model_normalized = model_lower.replace("-", " ")
                model_compact = model_lower.replace("-", "").replace(" ", "")
                if (
                    model_normalized not in product
                    and model_normalized not in meter
                    and model_compact not in sku.replace("-", "").replace(" ", "")
                ):
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
                "azure",
                "openai",
                region,
                cache_extras,
                [p.model_dump(mode="json") for p in prices],
                ttl_hours=self._settings.cache_ttl_hours,
            )
            prices = self._annotate_fresh(prices, self._SOURCE_URL)

        # Cost estimate if token volumes provided
        if prices and (input_tokens or output_tokens):
            in_price = next(
                (
                    p
                    for p in prices
                    if p.attributes.get("token_type") == "input" or "input" in p.description.lower()
                ),
                None,
            )
            out_price = next(
                (
                    p
                    for p in prices
                    if p.attributes.get("token_type") == "output"
                    or "output" in p.description.lower()
                ),
                None,
            )
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
                synthetic = NormalizedPrice(
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
                )
                # Inherit trust metadata from first component price
                if prices:
                    ref = prices[0]
                    synthetic = synthetic.model_copy(
                        update={
                            "fetched_at": ref.fetched_at,
                            "source_url": ref.source_url,
                            "cache_age_seconds": ref.cache_age_seconds,
                        }
                    )
                prices.append(synthetic)

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
            is_gpu = any(arm_sku.startswith(f"Standard_{prefix}") for prefix in ("NC", "ND", "NV"))
            if gpu and not is_gpu:
                continue
            if not gpu and is_gpu:
                continue

            instances.append(
                InstanceTypeInfo(
                    provider=CloudProvider.AZURE,
                    instance_type=arm_sku,
                    vcpu=0,  # Not available from Retail Prices API
                    memory_gb=0.0,  # Not available from Retail Prices API
                    gpu_count=1 if is_gpu else 0,
                    region=region,
                )
            )

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

            if (
                query_lower not in meter_name
                and query_lower not in product_name
                and query_lower not in sku_name
                and query_lower not in arm_sku
            ):
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
        "eastus",
        "eastus2",
        "westus",
        "westus2",
        "westus3",
        "centralus",
        "northeurope",
        "westeurope",
        "uksouth",
        "eastasia",
        "southeastasia",
        "australiaeast",
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
                    "ai (openai), inter_region_egress, observability (azure_monitor), "
                    "network (azure_cdn, azure_front_door)."
                ),
                alternatives=["Use describe_catalog(provider='azure') to see supported services."],
                example_invocation={
                    "provider": "azure",
                    "domain": "compute",
                    "resource_type": "Standard_D4s_v3",
                    "region": "eastus",
                },
            )
        breakdown: dict = {}
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
        elif isinstance(spec, ObservabilityPricingSpec):
            public_prices = await self._price_observability(spec)
        elif isinstance(spec, NetworkPricingSpec):
            public_prices, breakdown = await self._price_network(spec)
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
            breakdown=breakdown,
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
            dep = spec.deployment.lower()
            # Pass the requested deployment mode through to get_cosmos_price.
            # "ha" and "multi-region" variants mean multi-region provisioned writes.
            multi = dep in ("multi-az", "ha", "regional", "multi-region")
            cosmos_deployment = dep if dep in ("serverless", "autoscale") else "provisioned"
            return await self.get_cosmos_price(
                spec.region, deployment=cosmos_deployment, multi_region=multi
            )
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

    async def _price_network(self, spec: NetworkPricingSpec) -> tuple[list[NormalizedPrice], dict]:
        """Dispatch for domain=network specs. Supports egress, azure_cdn, azure_front_door."""
        svc = (spec.service or "").lower()
        if svc == "egress":
            return await self._price_network_egress(spec)
        if svc == "azure_cdn":
            prices = await self.get_cdn_price(
                region=spec.region,
                data_gb=spec.data_gb or spec.egress_gb,
            )
            return prices, {}
        if svc == "azure_front_door":
            prices = await self.get_front_door_price(
                region=spec.region,
                data_gb=spec.data_gb or spec.egress_gb,
                monthly_requests_millions=spec.monthly_requests_millions,
            )
            return prices, {}
        raise NotSupportedError(
            provider=self.provider,
            domain=spec.domain,
            service=spec.service,
            reason=(
                f"Azure NETWORK service '{spec.service}' not yet supported. "
                "Supported: egress, azure_cdn, azure_front_door."
            ),
            alternatives=["Use domain=inter_region_egress for legacy egress queries."],
        )

    async def _price_network_egress(
        self, spec: NetworkPricingSpec
    ) -> tuple[list[NormalizedPrice], dict]:
        """Tiered internet/cross-region/cross-AZ egress for domain=network, service=egress."""
        src = spec.source_region or spec.region or "eastus"
        dest_type = (spec.destination_type or "internet").lower()
        dest_region = spec.destination_region or ""
        data_gb = spec.data_gb_per_month if spec.data_gb_per_month > 0 else spec.egress_gb

        zone = _AZURE_EGRESS_ZONE.get(src.lower(), "zone1")

        if dest_type == "cross_az":
            tiers = [EgressTier(0, _AZURE_CROSS_AZ_RATE, "Cross-AZ within same region ($0.01/GB)")]
            tier_result = compute_tiered_cost(tiers, data_gb)
            price = NormalizedPrice(
                provider=CloudProvider.AZURE,
                service="egress",
                sku_id=f"azure:cross_az:{src}",
                product_family="Bandwidth",
                description=f"Azure cross-AZ traffic within {src}",
                region=src,
                pricing_term=PricingTerm.ON_DEMAND,
                price_per_unit=_AZURE_CROSS_AZ_RATE,
                unit=PriceUnit.PER_GB,
                attributes={
                    "source_region": src,
                    "destination_type": "cross_az",
                    "zone": zone,
                    "note": "Flat $0.01/GB each direction between availability zones.",
                },
            )
            return self._annotate_fresh([price], _AZURE_EGRESS_SOURCE_URL), tier_result

        if dest_type == "cross_region":
            rate = _AZURE_CROSS_REGION_RATE.get(zone, Decimal("0.02"))
            tiers = [EgressTier(0, rate, f"Inter-region within Azure ({zone}, flat)")]
            tier_result = compute_tiered_cost(tiers, data_gb)
            desc = f"Azure inter-region data transfer from {src}"
            if dest_region:
                desc += f" to {dest_region}"
            price = NormalizedPrice(
                provider=CloudProvider.AZURE,
                service="egress",
                sku_id=f"azure:inter_region:{src}:{dest_region or zone}",
                product_family="Bandwidth",
                description=desc,
                region=src,
                pricing_term=PricingTerm.ON_DEMAND,
                price_per_unit=rate,
                unit=PriceUnit.PER_GB,
                attributes={
                    "source_region": src,
                    "destination_type": "cross_region",
                    "zone": zone,
                    **({"dest_region": dest_region} if dest_region else {}),
                },
            )
            return self._annotate_fresh([price], _AZURE_EGRESS_SOURCE_URL), tier_result

        # Validate destination_type before falling through to internet egress
        if dest_type not in ("internet", "cross_az", "cross_region"):
            raise NotSupportedError(
                provider=self.provider,
                domain=spec.domain,
                service=spec.service,
                reason=(
                    f"Azure: unknown destination_type={dest_type!r}. "
                    "Valid values: 'internet', 'cross_region', 'cross_az'."
                ),
                alternatives=["Use destination_type='internet' for internet egress."],
            )

        # Default: internet egress with tiering
        tiers = _AZURE_INTERNET_EGRESS_TIERS.get(zone, _AZURE_INTERNET_EGRESS_TIERS["zone1"])

        # Attempt live fetch to update first paid tier rate
        try:
            zone_label = zone.replace("zone", "zone ")
            items = await asyncio.to_thread(
                self._fetch_prices,
                {"serviceName": "Bandwidth"},
                max_results=200,
            )
            tier_rows: list[tuple[float, Decimal]] = []
            for item in items:
                meter = item.get("meterName", "").lower()
                if "out" in meter and zone_label in meter:
                    tier_min = float(item.get("tierMinimumUnits", 0))
                    r = Decimal(str(item.get("retailPrice", 0)))
                    if r >= 0:
                        tier_rows.append((tier_min, r))
            if tier_rows:
                tier_rows.sort(key=lambda x: x[0])
                live_tiers = [
                    EgressTier(
                        threshold_gb=int(t[0]),
                        rate=t[1],
                        label=f"From {int(t[0])} GB (live)",
                    )
                    for t in tier_rows
                ]
                if live_tiers:
                    tiers = live_tiers
        except Exception as exc:
            logger.warning("Azure: tiered egress fetch failed: %s", exc)

        tier_result = compute_tiered_cost(tiers, data_gb)
        blended = (
            Decimal(tier_result["blended_rate_per_gb"])
            if data_gb > 0
            else tiers[1].rate
            if len(tiers) > 1
            else tiers[0].rate
        )
        desc = f"Azure internet egress from {src} ({zone.upper()}, tiered, first 5 GB free)"
        if data_gb > 0:
            total_cost = tier_result["total_cost"]
            desc += f": {data_gb:.0f} GB/month — ${total_cost} (${float(blended):.4f}/GB blended)"
        price = NormalizedPrice(
            provider=CloudProvider.AZURE,
            service="egress",
            sku_id=f"azure:internet_egress:{src}:{zone}",
            product_family="Bandwidth",
            description=desc,
            region=src,
            pricing_term=PricingTerm.ON_DEMAND,
            price_per_unit=blended,
            unit=PriceUnit.PER_GB_MONTH,
            attributes={
                "source_region": src,
                "destination_type": "internet",
                "zone": zone,
                "free_tier_gb": "5",
                "note": f"First 5 GB/month free. {zone.upper()} rates.",
            },
        )
        return self._annotate_fresh([price], _AZURE_EGRESS_SOURCE_URL), tier_result

    async def _price_egress(self, spec: EgressPricingSpec) -> list[NormalizedPrice]:
        src = spec.source_region or spec.region or "eastus"
        return await self.get_egress_price(src, spec.dest_region, spec.data_gb)

    async def _price_observability(self, spec: ObservabilityPricingSpec) -> list[NormalizedPrice]:
        svc = (spec.service or "azure_monitor").lower()
        if svc == "azure_monitor":
            return await self.get_monitor_price(
                region=spec.region,
                log_gb=spec.log_gb,
                metrics_millions=spec.ingestion_mib,  # repurposed: millions of samples
                alert_rules=spec.metrics_count,  # repurposed: alert rule count
            )
        raise NotSupportedError(
            provider=self.provider,
            domain=spec.domain,
            service=spec.service,
            reason=(
                f"Azure observability service '{spec.service}' not supported. Use 'azure_monitor'."
            ),
        )

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
                cache_key,
                {"rate": str(rate)},
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

        price = NormalizedPrice(
            provider=CloudProvider.AZURE,
            service="egress",
            sku_id=f"azure:egress:{source_region}:{dest_region}:{zone}",
            product_family="Bandwidth",
            description=desc,
            region=source_region,
            attributes=attrs,
            pricing_term=PricingTerm.ON_DEMAND,
            price_per_unit=rate,
            unit=PriceUnit.PER_GB_MONTH,
        )
        return self._annotate_fresh([price], self._SOURCE_URL)

    async def get_monitor_price(
        self,
        region: str,
        log_gb: float = 0.0,
        metrics_millions: float = 0.0,
        alert_rules: int = 0,
    ) -> list[NormalizedPrice]:
        """
        Price Azure Monitor.

        log_gb: Analytics Logs ingestion volume in GB/month (after 5 GB free).
        metrics_millions: Custom metrics ingestion in millions of samples (per 10M billed).
        alert_rules: Number of metric alert rules (first 10 free, then $0.10/rule/month).
        Free tier: 5 GB/month Analytics Logs. Log types: Analytics ($2.30/GB),
        Basic ($0.50/GB), Auxiliary ($0.05/GB).
        """
        cache_extras: dict[str, str] = {}
        cached_meta = await self._cache.get_prices_with_meta(
            "azure", "azure_monitor", region, cache_extras
        )
        if cached_meta is not None:
            cached_data, fetched_at = cached_meta
            prices = self._apply_cache_trust(
                [NormalizedPrice.model_validate(p) for p in cached_data],
                fetched_at,
                self._SOURCE_URL,
            )
        else:
            filters: dict[str, str] = {
                "armRegionName": region,
                "priceType": "Consumption",
                "serviceName": "Azure Monitor",
            }
            items = await asyncio.to_thread(self._fetch_prices, filters, 300)

            prices: list[NormalizedPrice] = []
            for item in items:
                meter = item.get("meterName", "").lower()

                # Match only log ingestion, metrics, and alert meters that are
                # billed on a volume/count basis.
                # Note: Analytics Logs ($2.30/GB) are not exposed via this API
                # endpoint; the method falls back to static rates for that tier.
                if "basic logs data ingestion" in meter:
                    unit = PriceUnit.PER_GB
                elif "auxiliary logs data ingestion" in meter:
                    unit = PriceUnit.PER_GB
                elif "metrics ingestion" in meter and "metric samples" in meter:
                    unit = PriceUnit.PER_UNIT  # per 10M samples
                elif meter == "alerts metric monitored":
                    unit = PriceUnit.PER_UNIT  # per rule/month
                else:
                    continue

                p = self._item_to_price(item, region, PricingTerm.ON_DEMAND, "azure_monitor")
                if p is None:
                    continue
                prices.append(
                    p.model_copy(
                        update={
                            "unit": unit,
                            "attributes": {
                                **p.attributes,
                                "monitor_dimension": meter,
                            },
                        }
                    )
                )

            if not prices:
                prices = [
                    NormalizedPrice(
                        provider=CloudProvider.AZURE,
                        service="azure_monitor",
                        sku_id="monitor-analytics-logs",
                        product_family="Management and Governance",
                        description="Azure Monitor — Analytics Logs ingestion (after 5 GB free)",
                        region=region,
                        pricing_term=PricingTerm.ON_DEMAND,
                        price_per_unit=_MONITOR_LOG_ANALYTICS_RATE,
                        unit=PriceUnit.PER_GB,
                        attributes={
                            "free_tier": "5 GB/month",
                            "source": "static_fallback",
                        },
                    ),
                    NormalizedPrice(
                        provider=CloudProvider.AZURE,
                        service="azure_monitor",
                        sku_id="monitor-metrics",
                        product_family="Management and Governance",
                        description="Azure Monitor — Metrics ingestion",
                        region=region,
                        pricing_term=PricingTerm.ON_DEMAND,
                        price_per_unit=_MONITOR_METRICS_RATE,
                        unit=PriceUnit.PER_UNIT,
                        attributes={
                            "unit_label": "per 10M metric samples",
                            "source": "static_fallback",
                        },
                    ),
                ]

            await self._cache.set_prices(
                "azure",
                "azure_monitor",
                region,
                cache_extras,
                [p.model_dump(mode="json") for p in prices],
                ttl_hours=self._settings.cache_ttl_hours,
            )
            prices = self._annotate_fresh(prices, self._SOURCE_URL)

        # Synthetic cost estimate if volume provided
        if log_gb > 0 or metrics_millions > 0 or alert_rules > 0:
            # Analytics Logs rate is not in live API (no "Analytics Logs Data
            # Ingestion" meter); always use the static fallback rate.
            log_rate = _MONITOR_LOG_ANALYTICS_RATE  # $2.30/GB
            metrics_price = next(
                (
                    p
                    for p in prices
                    if "metrics ingestion" in p.attributes.get("monitor_dimension", "")
                ),
                None,
            )
            if metrics_price is None:
                metrics_price = next(
                    (
                        p
                        for p in prices
                        if p.unit == PriceUnit.PER_UNIT and "metric" in p.description.lower()
                    ),
                    None,
                )
            alert_price = next(
                (
                    p
                    for p in prices
                    if p.attributes.get("monitor_dimension", "") == "alerts metric monitored"
                ),
                None,
            )

            total = Decimal("0")
            breakdown: list[str] = []

            if log_gb > 0:
                billable = max(Decimal("0"), Decimal(str(log_gb)) - _MONITOR_FREE_LOG_GB)
                cost = log_rate * billable
                total += cost
                breakdown.append(
                    f"{float(billable):.1f} GB × ${float(log_rate):.2f}/GB (Analytics Logs)"
                    f" = ${float(cost):.4f}"
                )
            if metrics_price and metrics_millions > 0:
                cost = metrics_price.price_per_unit * Decimal(str(metrics_millions / 10))
                total += cost
                breakdown.append(
                    f"{metrics_millions:.1f}M samples × "
                    f"${float(metrics_price.price_per_unit):.2f}/10M = ${float(cost):.4f}"
                )
            if alert_rules > 0:
                billable_rules = max(0, alert_rules - _MONITOR_FREE_ALERT_RULES)
                rate = alert_price.price_per_unit if alert_price else _MONITOR_ALERT_METRIC_RATE
                cost = rate * Decimal(str(billable_rules))
                total += cost
                breakdown.append(
                    f"{billable_rules} rules × ${float(rate):.2f}/mo = ${float(cost):.4f}"
                )

            if breakdown:
                synthetic = NormalizedPrice(
                    provider=CloudProvider.AZURE,
                    service="azure_monitor",
                    sku_id="monitor-estimate",
                    product_family="Management and Governance",
                    description="Azure Monitor — estimated monthly cost",
                    region=region,
                    pricing_term=PricingTerm.ON_DEMAND,
                    price_per_unit=total,
                    unit=PriceUnit.PER_UNIT,
                    attributes={
                        "breakdown": "; ".join(breakdown),
                        "note": (
                            "Analytics Logs free tier: 5 GB/month. "
                            "Metric alerts: first 10 rules free."
                        ),
                    },
                )
                if prices:
                    ref = prices[0]
                    synthetic = synthetic.model_copy(
                        update={
                            "fetched_at": ref.fetched_at,
                            "source_url": ref.source_url,
                            "cache_age_seconds": ref.cache_age_seconds,
                        }
                    )
                prices.append(synthetic)

        return prices

    async def get_cdn_price(
        self,
        region: str,
        data_gb: float = 0.0,
        sku: str = "standard",  # "standard" | "premium"
    ) -> list[NormalizedPrice]:
        """
        Price Azure CDN (Content Delivery Network).

        data_gb: Monthly outbound data transfer in GB for cost estimate.
        sku: "standard" (default) or "premium".
        CDN pricing is zone-based (Zone 1: Americas+Europe, Zone 2: APAC+ME).
        Rate at 0-10 TB: $0.081/GB (Zone 1).
        Note: CDN API prices are zone-based, not region-based. armRegionName is NOT
        passed to the API filter; region is used only for zone lookup.
        """
        zone_label = _CDN_ZONE.get(region.lower(), "Zone 1")
        cache_extras = {"sku": sku, "zone": zone_label}
        cached_meta = await self._cache.get_prices_with_meta(
            "azure", "azure_cdn", region, cache_extras
        )
        if cached_meta is not None:
            cached_data, fetched_at = cached_meta
            prices = self._apply_cache_trust(
                [NormalizedPrice.model_validate(p) for p in cached_data],
                fetched_at,
                self._SOURCE_URL,
            )
        else:
            # CDN pricing is zone-based — do NOT include armRegionName in filter
            filters: dict[str, str] = {
                "priceType": "Consumption",
                "serviceName": "Content Delivery Network",
            }
            items = await asyncio.to_thread(self._fetch_prices, filters, 700)

            prices = []
            for item in items:
                meter = item.get("meterName", "").lower()
                sku_name = item.get("skuName", "").lower()
                product = item.get("productName", "").lower()

                # Accept only data transfer out meters for the target zone
                if "data transfer" not in meter:
                    continue
                # SKU filter
                if sku == "premium" and "premium" not in sku_name:
                    continue
                if sku == "standard" and "standard" not in sku_name:
                    continue
                # Restrict to Microsoft CDN (skip Akamai/Verizon products)
                # CDN API returns no zone label in meterName; zones are implicit in
                # the price points. We collect all rows and pick by price tier.
                if "akamai" in product or "verizon" in product:
                    continue

                p = self._item_to_price(item, region, PricingTerm.ON_DEMAND, "azure_cdn")
                if p is None:
                    continue
                prices.append(
                    p.model_copy(
                        update={
                            "unit": PriceUnit.PER_GB,
                            "attributes": {
                                **p.attributes,
                                "cdn_zone": zone_label,
                                "cdn_sku": sku,
                            },
                        }
                    )
                )

            if not prices:
                cdn_static_rate = _CDN_STATIC_RATES.get(zone_label, _CDN_STATIC_RATE_ZONE1)
                prices = [
                    NormalizedPrice(
                        provider=CloudProvider.AZURE,
                        service="azure_cdn",
                        sku_id=f"cdn-{zone_label.lower().replace(' ', '')}-standard-dt",
                        product_family="Networking",
                        description=f"Azure CDN Standard — Data Transfer Out ({zone_label})",
                        region=region,
                        pricing_term=PricingTerm.ON_DEMAND,
                        price_per_unit=cdn_static_rate,
                        unit=PriceUnit.PER_GB,
                        attributes={
                            "cdn_zone": zone_label,
                            "cdn_sku": sku,
                            "source": "static_fallback",
                            "note": (
                                f"{zone_label} base tier (0-10 TB). Prices decrease at higher volumes."
                            ),
                        },
                    )
                ]

            await self._cache.set_prices(
                "azure",
                "azure_cdn",
                region,
                cache_extras,
                [p.model_dump(mode="json") for p in prices],
                ttl_hours=self._settings.cache_ttl_hours,
            )
            prices = self._annotate_fresh(prices, self._SOURCE_URL)

        # Synthetic estimate if data_gb provided
        if data_gb > 0 and prices:
            # Pick base rate (highest $/GB = first volume tier)
            base = max(prices, key=lambda p: p.price_per_unit)
            cost = base.price_per_unit * Decimal(str(data_gb))
            synthetic = NormalizedPrice(
                provider=CloudProvider.AZURE,
                service="azure_cdn",
                sku_id="cdn-estimate",
                product_family="Networking",
                description=f"Azure CDN — estimated monthly cost for {data_gb:.1f} GB",
                region=region,
                pricing_term=PricingTerm.ON_DEMAND,
                price_per_unit=cost,
                unit=PriceUnit.PER_UNIT,
                attributes={
                    "breakdown": (
                        f"{data_gb:.1f} GB × ${float(base.price_per_unit):.4f}/GB"
                        f" = ${float(cost):.4f}"
                    ),
                    "cdn_zone": zone_label,
                    "note": "Base tier rate. Volume discounts apply above 10 TB/month.",
                },
            )
            ref = prices[0]
            synthetic = synthetic.model_copy(
                update={
                    "fetched_at": ref.fetched_at,
                    "source_url": ref.source_url,
                    "cache_age_seconds": ref.cache_age_seconds,
                }
            )
            prices.append(synthetic)

        return prices

    async def get_front_door_price(
        self,
        region: str,
        data_gb: float = 0.0,
        monthly_requests_millions: float = 0.0,
        sku: str = "standard",  # "standard" | "premium"
    ) -> list[NormalizedPrice]:
        """
        Price Azure Front Door.

        data_gb: Monthly data transfer out in GB for cost estimate.
        monthly_requests_millions: Monthly request count in millions for cost estimate.
        sku: "standard" (default) or "premium".
        Front Door has 8 pricing zones. Standard SKU Zone 1 base: $0.0825/GB,
        $0.009/10K requests.
        Note: Front Door API prices are zone-based — armRegionName is NOT passed to
        the API filter.
        """
        zone_label = _CDN_ZONE.get(region.lower(), "Zone 1")
        cache_extras = {"sku": sku, "zone": zone_label}
        cached_meta = await self._cache.get_prices_with_meta(
            "azure", "azure_front_door", region, cache_extras
        )
        if cached_meta is not None:
            cached_data, fetched_at = cached_meta
            prices = self._apply_cache_trust(
                [NormalizedPrice.model_validate(p) for p in cached_data],
                fetched_at,
                self._SOURCE_URL,
            )
        else:
            # Front Door pricing is zone-based — do NOT include armRegionName in filter
            filters: dict[str, str] = {
                "priceType": "Consumption",
                "serviceName": "Azure Front Door Service",
            }
            items = await asyncio.to_thread(self._fetch_prices, filters, 500)

            prices = []
            for item in items:
                meter = item.get("meterName", "").lower()
                sku_name = item.get("skuName", "").lower()
                uom = item.get("unitOfMeasure", "").lower()
                product = item.get("productName", "").lower()

                # SKU filter
                if sku == "premium" and "premium" not in sku_name:
                    continue
                if sku == "standard" and "standard" not in sku_name:
                    continue
                # Front Door API returns no zone label in meterName; zones are
                # implicit in the price points. Restrict to Front Door Service product.
                if "front door service" not in product and "front door" not in product:
                    continue

                if "data transfer out" in meter:
                    unit = PriceUnit.PER_GB
                elif "requests" in meter and "10k" in uom:
                    unit = PriceUnit.PER_UNIT  # per 10K requests
                else:
                    continue

                p = self._item_to_price(item, region, PricingTerm.ON_DEMAND, "azure_front_door")
                if p is None:
                    continue
                attrs = {**p.attributes, "cdn_zone": zone_label, "front_door_sku": sku}
                if unit == PriceUnit.PER_UNIT:
                    attrs["unit_label"] = "per 10K requests"
                prices.append(p.model_copy(update={"unit": unit, "attributes": attrs}))

            if not prices:
                fd_dt_rate = _FRONTDOOR_STATIC_RATES.get(zone_label, _FRONTDOOR_STATIC_RATE_ZONE1)
                fd_req_rate = _FRONTDOOR_REQ_STATIC_RATES.get(
                    zone_label, _FRONTDOOR_REQ_STATIC_RATE_ZONE1
                )
                zone_slug = zone_label.lower().replace(" ", "")
                prices = [
                    NormalizedPrice(
                        provider=CloudProvider.AZURE,
                        service="azure_front_door",
                        sku_id=f"frontdoor-{zone_slug}-standard-dt",
                        product_family="Networking",
                        description=(
                            f"Azure Front Door Standard — Data Transfer Out ({zone_label})"
                        ),
                        region=region,
                        pricing_term=PricingTerm.ON_DEMAND,
                        price_per_unit=fd_dt_rate,
                        unit=PriceUnit.PER_GB,
                        attributes={
                            "cdn_zone": zone_label,
                            "front_door_sku": sku,
                            "source": "static_fallback",
                        },
                    ),
                    NormalizedPrice(
                        provider=CloudProvider.AZURE,
                        service="azure_front_door",
                        sku_id=f"frontdoor-{zone_slug}-standard-req",
                        product_family="Networking",
                        description=(f"Azure Front Door Standard — Requests ({zone_label})"),
                        region=region,
                        pricing_term=PricingTerm.ON_DEMAND,
                        price_per_unit=fd_req_rate,
                        unit=PriceUnit.PER_UNIT,
                        attributes={
                            "cdn_zone": zone_label,
                            "front_door_sku": sku,
                            "unit_label": "per 10K requests",
                            "source": "static_fallback",
                        },
                    ),
                ]

            await self._cache.set_prices(
                "azure",
                "azure_front_door",
                region,
                cache_extras,
                [p.model_dump(mode="json") for p in prices],
                ttl_hours=self._settings.cache_ttl_hours,
            )
            prices = self._annotate_fresh(prices, self._SOURCE_URL)

        # Synthetic estimate if volumes provided
        if data_gb > 0 or monthly_requests_millions > 0:
            dt_price = next(
                (p for p in prices if p.unit == PriceUnit.PER_GB and p.price_per_unit > 0),
                None,
            )
            req_price = next(
                (
                    p
                    for p in prices
                    if p.unit == PriceUnit.PER_UNIT
                    and p.attributes.get("unit_label") == "per 10K requests"
                ),
                None,
            )
            total = Decimal("0")
            breakdown: list[str] = []

            if dt_price and data_gb > 0:
                cost = dt_price.price_per_unit * Decimal(str(data_gb))
                total += cost
                breakdown.append(
                    f"{data_gb:.1f} GB × ${float(dt_price.price_per_unit):.4f}/GB"
                    f" = ${float(cost):.4f}"
                )
            if req_price and monthly_requests_millions > 0:
                # req_price is per 10K requests; convert millions to 10K units
                units_10k = monthly_requests_millions * 1_000_000 / 10_000
                cost = req_price.price_per_unit * Decimal(str(units_10k))
                total += cost
                breakdown.append(
                    f"{monthly_requests_millions:.1f}M requests × "
                    f"${float(req_price.price_per_unit):.4f}/10K = ${float(cost):.4f}"
                )

            if breakdown:
                synthetic = NormalizedPrice(
                    provider=CloudProvider.AZURE,
                    service="azure_front_door",
                    sku_id="frontdoor-estimate",
                    product_family="Networking",
                    description="Azure Front Door — estimated monthly cost",
                    region=region,
                    pricing_term=PricingTerm.ON_DEMAND,
                    price_per_unit=total,
                    unit=PriceUnit.PER_UNIT,
                    attributes={
                        "breakdown": "; ".join(breakdown),
                        "cdn_zone": zone_label,
                    },
                )
                ref = prices[0]
                synthetic = synthetic.model_copy(
                    update={
                        "fetched_at": ref.fetched_at,
                        "source_url": ref.source_url,
                        "cache_age_seconds": ref.cache_age_seconds,
                    }
                )
                prices.append(synthetic)

        return prices

    async def describe_catalog(self) -> ProviderCatalog:
        return ProviderCatalog(
            provider=CloudProvider.AZURE,
            domains=[
                PricingDomain.COMPUTE,
                PricingDomain.STORAGE,
                PricingDomain.DATABASE,
                PricingDomain.CONTAINER,
                PricingDomain.SERVERLESS,
                PricingDomain.AI,
                PricingDomain.NETWORK,
                PricingDomain.INTER_REGION_EGRESS,
                PricingDomain.OBSERVABILITY,
                PricingDomain.NETWORK,
            ],
            services={
                "compute": ["vm"],
                "storage": ["managed_disks", "blob"],
                "database": ["sql", "cosmos"],
                "container": ["aks"],
                "serverless": ["azure_functions"],
                "ai": ["openai"],
                "network": ["egress"],
                "inter_region_egress": [],
                "observability": ["azure_monitor"],
                "network": ["azure_cdn", "azure_front_door"],
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
                "network/egress": ["on_demand"],
                "inter_region_egress": ["on_demand"],
                "observability/azure_monitor": ["on_demand"],
                "network/azure_cdn": ["on_demand"],
                "network/azure_front_door": ["on_demand"],
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
                    "deployment": (
                        "'provisioned' (per 100 RU/s, default) | 'serverless' (per 1M RUs) | 'autoscale' | "
                        "'ha'/'multi-region' (provisioned with multi-region writes)"
                    ),
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
                "observability/azure_monitor": {
                    "log_gb": (
                        "Analytics Logs ingestion volume in GB/month (after 5 GB free tier)"
                    ),
                    "ingestion_mib": (
                        "Custom metrics ingestion in millions of samples "
                        "(e.g. 100 for 100M samples/month)"
                    ),
                    "metrics_count": (
                        "Number of metric alert rules (first 10 free, then $0.10/rule/month)"
                    ),
                    "note": (
                        "Free tier: 5 GB/month Analytics Logs. "
                        "Log types: Analytics ($2.30/GB), Basic ($0.50/GB), "
                        "Auxiliary ($0.05/GB)."
                    ),
                },
                "network/azure_cdn": {
                    "data_gb": "Monthly outbound data transfer in GB for cost estimate",
                    "note": (
                        "CDN pricing is zone-based "
                        "(Zone 1: Americas+Europe, Zone 2: APAC+ME). "
                        "Rate at 0-10 TB: $0.081/GB (Zone 1)."
                    ),
                },
                "network/azure_front_door": {
                    "data_gb": "Monthly data transfer out in GB",
                    "monthly_requests_millions": (
                        "Monthly request count in millions for cost estimate"
                    ),
                    "note": (
                        "Front Door has 8 pricing zones. "
                        "Standard SKU Zone 1 base: $0.0825/GB, $0.009/10K requests."
                    ),
                },
                "network/egress": {
                    "source_region": "Origin Azure ARM region e.g. 'eastus', 'westeurope'",
                    "destination_type": "internet | cross_region | cross_az",
                    "destination_region": "Target region for cross_region (optional)",
                    "data_gb_per_month": "Monthly data volume in GB for tiered cost estimate",
                    "note": "First 100 GB/month free. Zone 1 rates for Americas + Europe.",
                },
            },
            example_invocations={
                "compute/vm": {
                    "provider": "azure",
                    "domain": "compute",
                    "resource_type": "Standard_D4s_v3",
                    "region": "eastus",
                    "os": "Linux",
                    "term": "on_demand",
                },
                "storage/managed_disks": {
                    "provider": "azure",
                    "domain": "storage",
                    "storage_type": "premium-ssd",
                    "region": "eastus",
                    "size_gb": 128,
                },
                "database/sql": {
                    "provider": "azure",
                    "domain": "database",
                    "service": "sql",
                    "resource_type": "General Purpose 4 vCores",
                    "engine": "SQL",
                    "deployment": "single-az",
                    "region": "eastus",
                },
                "database/cosmos": {
                    "provider": "azure",
                    "domain": "database",
                    "service": "cosmos",
                    "deployment": "provisioned",
                    "region": "eastus",
                },
                "container/aks": {
                    "provider": "azure",
                    "domain": "container",
                    "service": "aks",
                    "mode": "standard",
                    "region": "eastus",
                },
                "serverless/azure_functions": {
                    "provider": "azure",
                    "domain": "serverless",
                    "service": "azure_functions",
                    "gb_seconds": 500000,
                    "requests_millions": 5,
                    "region": "eastus",
                },
                "ai/openai": {
                    "provider": "azure",
                    "domain": "ai",
                    "service": "openai",
                    "model": "gpt-4o",
                    "input_tokens": 1000000,
                    "output_tokens": 500000,
                    "region": "eastus",
                },
                "inter_region_egress": {
                    "provider": "azure",
                    "domain": "inter_region_egress",
                    "source_region": "eastus",
                    "dest_region": "westeurope",
                    "data_gb": 1000,
                },
                "observability/azure_monitor": {
                    "provider": "azure",
                    "domain": "observability",
                    "service": "azure_monitor",
                    "region": "eastus",
                    "log_gb": 50.0,
                    "metrics_count": 20,
                },
                "network/azure_cdn": {
                    "provider": "azure",
                    "domain": "network",
                    "service": "azure_cdn",
                    "region": "eastus",
                    "data_gb": 5000,
                },
                "network/azure_front_door": {
                    "provider": "azure",
                    "domain": "network",
                    "service": "azure_front_door",
                    "region": "eastus",
                    "data_gb": 1000,
                    "monthly_requests_millions": 100,
                },
                "network/egress": {
                    "provider": "azure",
                    "domain": "network",
                    "service": "egress",
                    "source_region": "eastus",
                    "destination_type": "internet",
                    "data_gb_per_month": 1024.0,
                },
                "network/egress/cross_region": {
                    "provider": "azure",
                    "domain": "network",
                    "service": "egress",
                    "source_region": "eastus",
                    "destination_type": "cross_region",
                    "destination_region": "westeurope",
                    "data_gb_per_month": 1024.0,
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
                "Azure Monitor": "observability/azure_monitor",
                "Log Analytics": "observability/azure_monitor — set log_gb for ingestion volume",
                "Azure Monitor Logs": "observability/azure_monitor — set log_gb for Analytics Logs cost",
                "Azure Monitor Metrics": "observability/azure_monitor — set ingestion_mib for custom metrics millions",
                "Azure Monitor Alerts": "observability/azure_monitor — set metrics_count for alert rule count",
                "Azure CDN": "network/azure_cdn — set data_gb for monthly transfer estimate",
                "Content Delivery Network": "network/azure_cdn — set data_gb for monthly transfer estimate",
                "Azure Front Door": "network/azure_front_door — set data_gb and monthly_requests_millions",
                "Azure Front Door Standard": "network/azure_front_door — set data_gb, monthly_requests_millions",
                "Azure Front Door Premium": "network/azure_front_door — Premium WAF rules cost additional",
                "Azure internet egress with tier breakdown (first 100 GB free)": "network/egress — set destination_type=internet + data_gb_per_month",
                "Azure inter-region transfer with blended cost": "network/egress — set destination_type=cross_region + destination_region",
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
