"""
GCP cloud pricing provider.

Uses the GCP Cloud Billing Catalog REST API to fetch public pricing.
Auth: API key (simplest, no service account needed for catalog data) or
      Application Default Credentials (ADC) via google-auth if installed.

GCP pricing differs from AWS: there is no single per-instance-type SKU.
Instead, each machine family has separate per-vCPU and per-GB-RAM SKUs.
We fetch all Compute Engine SKUs once per region, build an index, then
compute instance prices as: vcpus * cpu_price + memory_gb * ram_price.

Windows pricing (T31):
    GCP charges an additional Windows Server license fee on top of base Linux compute.
    Windows SKUs have descriptions like "N2 Instance Core running Windows" (per vCPU)
    and "N2 Instance Ram running Windows" (per GB RAM).
    Total Windows price = base_linux_price + windows_license_price.
    Not all families support Windows; E2 and families without Windows SKU mappings
    return empty results rather than silently returning Linux price.
"""
from __future__ import annotations

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
from opencloudcosts.utils.gcp_specs import (
    CLOUD_SQL_INSTANCE_SPECS,
    GCP_FAMILY_SKU,
    GCP_INSTANCE_SPECS,
    GCP_STORAGE_SKU,
    get_machine_family,
    parse_instance_type,
)
from opencloudcosts.utils.regions import list_gcp_regions
from opencloudcosts.utils.units import gcp_money_to_decimal

logger = logging.getLogger(__name__)

_CATALOG_BASE = "https://cloudbilling.googleapis.com/v1"
_COMPUTE_SERVICE_ID = "6F81-5844-456A"   # Compute Engine
_GCS_SERVICE_ID = "95FF-2EF5-5EA1"       # Cloud Storage
_CLOUD_SQL_SERVICE_ID = "9662-B51E-5089" # Cloud SQL
_GKE_SERVICE_ID = "CCD8-9BF1-090E"       # Google Kubernetes Engine
_MEMORYSTORE_SERVICE_ID = "5AF5-2C11-D467"  # Memorystore for Redis
_VERTEX_SERVICE_ID = "C7E2-9256-1C43"       # Vertex AI
_BIGQUERY_SERVICE_ID = "24E6-581D-38E5"     # BigQuery
_CLOUD_ARMOR_SERVICE_ID = "E3D3-8838-A232"  # Cloud Armor (best-effort)
_CLOUD_MONITORING_SERVICE_ID = "58CD-E7C3-72CA"  # Cloud Monitoring (best-effort)

# GCS storage class -> description substring in SKU catalog
_GCS_STORAGE_CLASSES: dict[str, str] = {
    "standard": "Standard Storage",
    "nearline": "Nearline Storage",
    "coldline": "Coldline Storage",
    "archive":  "Archive Storage",
}

# Words in SKU description that indicate non-capacity SKUs to exclude
_GCS_EXCLUDE_KEYWORDS = ("operations", "retrieval", "early delete", "metadata", "list")

# Cloud SQL engine name normalization
_CLOUD_SQL_ENGINE_NAMES: dict[str, str] = {
    "mysql": "MySQL",
    "postgresql": "PostgreSQL",
    "postgres": "PostgreSQL",
    "sqlserver": "SQL Server",
    "sql server": "SQL Server",
}

# Map PricingTerm -> (usageType value in SKU category, description key in GCP_FAMILY_SKU)
_TERM_USAGE_TYPE: dict[PricingTerm, tuple[str, str, str]] = {
    PricingTerm.ON_DEMAND:   ("OnDemand",   "cpu_desc",          "ram_desc"),
    PricingTerm.SPOT:        ("Preemptible", "preempt_cpu_desc",  "preempt_ram_desc"),
    PricingTerm.CUD_1YR:     ("Commit1Yr",  "cud_cpu_desc",      "cud_ram_desc"),
    PricingTerm.CUD_3YR:     ("Commit3Yr",  "cud_cpu_desc",      "cud_ram_desc"),
}


def _windows_sku_suffix(family: str) -> tuple[str, str] | None:
    """
    Return (cpu_desc_fragment, ram_desc_fragment) for Windows SKU lookup, or None if unsupported.

    GCP Windows pricing adds a per-vCPU and per-GB-RAM Windows Server license cost
    on top of the base Linux compute price. These descriptions match the GCP Billing
    Catalog API SKU descriptions for Windows license charges.

    Families without Windows support (E2, C2D, T2D, T2A, M1, A2, etc.) return None.
    """
    _MAP: dict[str, tuple[str, str]] = {
        "n1":  ("N1 Predefined Instance Core running Windows",  "N1 Predefined Instance Ram running Windows"),
        "n2":  ("N2 Instance Core running Windows",              "N2 Instance Ram running Windows"),
        "n2d": ("N2D AMD Instance Core running Windows",         "N2D AMD Instance Ram running Windows"),
        "c2":  ("Compute optimized Core running Windows",        "Compute optimized Ram running Windows"),
        # E2: cost-optimised, no Windows support
        # C2D, T2D, T2A, M1, A2: no Windows support
    }
    return _MAP.get(family.lower())


class GCPProvider:
    provider = CloudProvider.GCP

    def __init__(self, settings: Settings, cache: CacheManager) -> None:
        self._settings = settings
        self._cache = cache
        api_key = settings.gcp_api_key
        # OAuth access tokens (ya29.*) are short-lived and need Bearer auth, not ?key=
        # API keys always start with "AIza" — catch the common mistake early.
        if api_key and api_key.startswith("ya29."):
            raise NotConfiguredError(
                "OCC_GCP_API_KEY looks like an OAuth access token (starts with 'ya29.'), "
                "not an API key. OAuth tokens are short-lived and must be sent as a Bearer "
                "header — they won't work as a ?key= parameter.\n\n"
                "Create a proper API key instead:\n"
                "  1. console.cloud.google.com/apis/credentials\n"
                "  2. Create Credentials → API key\n"
                "  3. The key will start with 'AIza...'"
            )
        self._api_key = api_key
        self._http: httpx.AsyncClient | None = None

    async def _get_http(self) -> httpx.AsyncClient:
        if self._http is None or self._http.is_closed:
            headers: dict[str, str] = {}
            if not self._api_key:
                # Try ADC via google-auth (optional dep)
                try:
                    import google.auth
                    import google.auth.transport.requests
                    creds, _ = google.auth.default(
                        scopes=["https://www.googleapis.com/auth/cloud-billing.readonly"]
                    )
                    request = google.auth.transport.requests.Request()
                    creds.refresh(request)
                    headers["Authorization"] = f"Bearer {creds.token}"
                except ImportError:
                    raise NotConfiguredError(
                        "IMPORTANT: GCP pricing is unavailable — do NOT estimate or approximate "
                        "any GCP prices. State that GCP pricing is unavailable and explain why.\n\n"
                        "GCP pricing requires a free API key (unlike AWS, GCP has no "
                        "unauthenticated public pricing endpoint).\n\n"
                        "Quickest setup (2 min, no credit card needed):\n"
                        "  1. Go to https://console.cloud.google.com/apis/credentials\n"
                        "  2. Create Project (free) if you don't have one\n"
                        "  3. Click 'Create Credentials' → 'API key'\n"
                        "  4. Set OCC_GCP_API_KEY=<your-key> in your environment or .env\n\n"
                        "Alternative: install google-auth and run 'gcloud auth application-default login'"
                    )
            self._http = httpx.AsyncClient(
                base_url=_CATALOG_BASE,
                timeout=30.0,
                headers=headers,
            )
        return self._http

    def _auth_params(self) -> dict[str, str]:
        if self._api_key:
            return {"key": self._api_key}
        return {}

    # ------------------------------------------------------------------
    # Catalog API helpers
    # ------------------------------------------------------------------

    async def _fetch_skus(self, service_id: str) -> list[dict[str, Any]]:
        """Fetch all SKUs for a GCP service, paginating through results."""
        cache_key = f"gcp:skus:{service_id}"
        cached = await self._cache.get_metadata(cache_key)
        if cached is not None:
            return cached

        http = await self._get_http()
        skus: list[dict[str, Any]] = []
        params: dict[str, Any] = {"pageSize": 5000, **self._auth_params()}
        url = f"/services/{service_id}/skus"

        while True:
            resp = await http.get(url, params=params)
            resp.raise_for_status()
            data = resp.json()
            skus.extend(data.get("skus", []))
            next_token = data.get("nextPageToken")
            if not next_token:
                break
            params["pageToken"] = next_token

        await self._cache.set_metadata(
            cache_key,
            skus,
            ttl_hours=self._settings.metadata_ttl_days * 24,
        )
        logger.info("Fetched %d GCP SKUs for service %s", len(skus), service_id)
        return skus

    @staticmethod
    def _sku_price(sku: dict[str, Any]) -> Decimal:
        """Extract the first-tier unit price from a SKU's pricingInfo."""
        try:
            pricing_info = sku.get("pricingInfo", [])
            if not pricing_info:
                return Decimal("0")
            tiers = pricing_info[0]["pricingExpression"]["tieredRates"]
            # Use the first tier with startUsageAmount == 0
            for tier in tiers:
                if tier.get("startUsageAmount", 0) == 0:
                    up = tier["unitPrice"]
                    return gcp_money_to_decimal(
                        up.get("units", "0"),
                        up.get("nanos", 0),
                    )
        except (KeyError, IndexError, TypeError):
            pass
        return Decimal("0")

    @staticmethod
    def _sku_paid_price(sku: dict[str, Any]) -> Decimal:
        """Extract the first paid-tier unit price from a SKU's pricingInfo.

        Some SKUs (e.g. BigQuery query analysis) have a free-quota tier at
        startUsageAmount=0 followed by the actual charged rate at a higher
        startUsageAmount. This method skips the free tier and returns the
        first tier where startUsageAmount > 0.
        """
        try:
            pricing_info = sku.get("pricingInfo", [])
            if not pricing_info:
                return Decimal("0")
            tiers = pricing_info[0]["pricingExpression"]["tieredRates"]
            for tier in tiers:
                if tier.get("startUsageAmount", 0) > 0:
                    up = tier["unitPrice"]
                    return gcp_money_to_decimal(
                        up.get("units", "0"),
                        up.get("nanos", 0),
                    )
        except (KeyError, IndexError, TypeError):
            pass
        return Decimal("0")

    async def _build_price_index(
        self, region: str
    ) -> dict[tuple[str, str], Decimal]:
        """
        Build a price index for Compute Engine in a given region.

        Returns dict keyed by (sku_description_substring, usage_type) -> price_per_unit.
        We key on description substring so matching is robust to minor SKU wording changes.
        """
        cache_key = f"gcp:price_index:{region}"
        cached = await self._cache.get_metadata(cache_key)
        if cached is not None:
            # Re-hydrate Decimal values from cached strings
            return {(k.split("|")[0], k.split("|")[1]): Decimal(v) for k, v in cached.items()}

        skus = await self._fetch_skus(_COMPUTE_SERVICE_ID)

        index: dict[tuple[str, str], Decimal] = {}
        for sku in skus:
            service_regions = sku.get("serviceRegions", [])
            # SKU applies globally or to our specific region
            if region not in service_regions and "global" not in service_regions:
                continue

            desc = sku.get("description", "")
            category = sku.get("category", {})
            usage_type = category.get("usageType", "OnDemand")
            price = self._sku_price(sku)
            if price > 0:
                index[(desc, usage_type)] = price

        # Cache as JSON-serialisable dict
        serialisable = {f"{k[0]}|{k[1]}": str(v) for k, v in index.items()}
        await self._cache.set_metadata(
            cache_key,
            serialisable,
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return index

    def _lookup_price(
        self,
        index: dict[tuple[str, str], Decimal],
        desc_substring: str,
        usage_type: str,
    ) -> Decimal | None:
        """Find a price in the index by partial description match."""
        for (desc, utype), price in index.items():
            if usage_type == utype and desc_substring.lower() in desc.lower():
                return price
        return None

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
        specs = parse_instance_type(instance_type)
        if specs is None:
            raise ValueError(
                f"Unknown GCP instance type: {instance_type!r}. "
                "Use list_instance_types to discover available types."
            )
        vcpus, memory_gb = specs
        family = get_machine_family(instance_type)

        family_skus = GCP_FAMILY_SKU.get(family)
        if family_skus is None:
            raise ValueError(
                f"GCP machine family '{family}' is not yet supported. "
                f"Supported families: {sorted(GCP_FAMILY_SKU)}"
            )

        if term not in _TERM_USAGE_TYPE:
            raise ValueError(
                f"Unsupported pricing term for GCP: {term.value}. "
                f"Supported: {[t.value for t in _TERM_USAGE_TYPE]}"
            )
        usage_type, cpu_key, ram_key = _TERM_USAGE_TYPE[term]

        # For CUDs, usage_type is the same for 1yr and 3yr — disambiguate by desc
        cud_year = None
        if term == PricingTerm.CUD_1YR:
            cud_year = "1yr"
        elif term == PricingTerm.CUD_3YR:
            cud_year = "3yr"

        cache_key_extras = {
            "instance_type": instance_type, "os": os, "term": term.value
        }
        cached = await self._cache.get_prices("gcp", "compute", region, cache_key_extras)
        if cached:
            return [NormalizedPrice.model_validate(p) for p in cached]

        index = await self._build_price_index(region)

        cpu_desc = family_skus[cpu_key]
        ram_desc = family_skus[ram_key]

        # For CUDs, the usageType in the SKU catalog is "Commit1Yr" / "Commit3Yr"
        if cud_year:
            lookup_usage = f"Commit{cud_year.replace('yr', 'Yr')}"
        else:
            lookup_usage = usage_type

        cpu_price = self._lookup_price(index, cpu_desc, lookup_usage)
        ram_price = self._lookup_price(index, ram_desc, lookup_usage)

        if cpu_price is None or ram_price is None:
            logger.warning(
                "GCP: could not find %s/%s SKU for %s in %s (term=%s). "
                "CPU found: %s, RAM found: %s",
                cpu_desc, ram_desc, instance_type, region, term.value,
                cpu_price is not None, ram_price is not None,
            )
            return []

        total_price = (
            Decimal(str(vcpus)) * cpu_price
            + Decimal(str(memory_gb)) * ram_price
        )

        # Windows pricing: add Windows Server license cost on top of base Linux price
        if os == "Windows":
            win_skus = _windows_sku_suffix(family)
            if win_skus is None:
                logger.warning(
                    "GCP: Windows pricing not supported for machine family '%s'. "
                    "Supported Windows families: n1, n2, n2d, c2.",
                    family,
                )
                return []
            win_cpu_desc, win_ram_desc = win_skus
            win_cpu_price = self._lookup_price(index, win_cpu_desc, "OnDemand")
            win_ram_price = self._lookup_price(index, win_ram_desc, "OnDemand")
            if win_cpu_price is None or win_ram_price is None:
                logger.warning(
                    "GCP: could not find Windows SKU for %s in %s. "
                    "Win CPU found: %s, Win RAM found: %s",
                    instance_type, region,
                    win_cpu_price is not None, win_ram_price is not None,
                )
                return []
            windows_license = (
                Decimal(str(vcpus)) * win_cpu_price
                + Decimal(str(memory_gb)) * win_ram_price
            )
            total_price = total_price + windows_license

        price = NormalizedPrice(
            provider=CloudProvider.GCP,
            service="compute",
            sku_id=f"gcp:{family}:{region}:{term.value}",
            product_family="Compute Instance",
            description=instance_type,
            region=region,
            attributes={
                "instanceType": instance_type,
                "vcpu": str(vcpus),
                "memory": f"{memory_gb} GB",
                "operatingSystem": os,
                "machineFamily": family,
            },
            pricing_term=term,
            price_per_unit=total_price,
            unit=PriceUnit.PER_HOUR,
        )

        await self._cache.set_prices(
            "gcp", "compute", region, cache_key_extras,
            [price.model_dump(mode="json")],
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return [price]

    # ------------------------------------------------------------------
    # GCS (Cloud Storage) pricing
    # ------------------------------------------------------------------

    async def _build_gcs_price_index(
        self, region: str
    ) -> dict[tuple[str, str], Decimal]:
        """Build price index for Cloud Storage SKUs in a given region."""
        cache_key = f"gcp:gcs_price_index:{region}"
        cached = await self._cache.get_metadata(cache_key)
        if cached is not None:
            return {(k.split("|")[0], k.split("|")[1]): Decimal(v) for k, v in cached.items()}

        skus = await self._fetch_skus(_GCS_SERVICE_ID)

        index: dict[tuple[str, str], Decimal] = {}
        for sku in skus:
            service_regions = sku.get("serviceRegions", [])
            if region not in service_regions and "global" not in service_regions:
                continue

            desc = sku.get("description", "")
            desc_lower = desc.lower()

            # Exclude non-capacity SKUs (operations, retrieval, etc.)
            if any(kw in desc_lower for kw in _GCS_EXCLUDE_KEYWORDS):
                continue

            category = sku.get("category", {})
            usage_type = category.get("usageType", "OnDemand")
            price = self._sku_price(sku)
            if price > 0:
                index[(desc, usage_type)] = price

        serialisable = {f"{k[0]}|{k[1]}": str(v) for k, v in index.items()}
        await self._cache.set_metadata(
            cache_key,
            serialisable,
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return index

    # ------------------------------------------------------------------
    # Cloud SQL pricing
    # ------------------------------------------------------------------

    async def _build_cloud_sql_price_index(
        self, region: str
    ) -> dict[tuple[str, str], Decimal]:
        """Build price index for Cloud SQL SKUs in a given region."""
        cache_key = f"gcp:cloud_sql_price_index:{region}"
        cached = await self._cache.get_metadata(cache_key)
        if cached is not None:
            return {(k.split("|")[0], k.split("|")[1]): Decimal(v) for k, v in cached.items()}

        skus = await self._fetch_skus(_CLOUD_SQL_SERVICE_ID)

        index: dict[tuple[str, str], Decimal] = {}
        for sku in skus:
            service_regions = sku.get("serviceRegions", [])
            if region not in service_regions and "global" not in service_regions:
                continue

            desc = sku.get("description", "")
            category = sku.get("category", {})
            usage_type = category.get("usageType", "OnDemand")
            price = self._sku_price(sku)
            if price > 0:
                index[(desc, usage_type)] = price

        serialisable = {f"{k[0]}|{k[1]}": str(v) for k, v in index.items()}
        await self._cache.set_metadata(
            cache_key,
            serialisable,
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return index

    async def get_cloud_sql_price(
        self,
        instance_type: str,
        region: str,
        engine: str = "MySQL",
        ha: bool = False,
    ) -> list[NormalizedPrice]:
        """Price a Cloud SQL instance by vCPU + RAM rate lookup."""
        specs = CLOUD_SQL_INSTANCE_SPECS.get(instance_type.lower())
        if specs is None:
            raise ValueError(
                f"Unknown Cloud SQL instance type: {instance_type!r}. "
                f"Supported types: {sorted(CLOUD_SQL_INSTANCE_SPECS)}"
            )
        vcpus, memory_gb = specs

        # Normalize engine name
        engine_norm = _CLOUD_SQL_ENGINE_NAMES.get(engine.lower(), engine)

        cache_key_extras = {"instance_type": instance_type, "engine": engine_norm, "ha": str(ha)}
        cached = await self._cache.get_prices("gcp", "cloud_sql", region, cache_key_extras)
        if cached:
            return [NormalizedPrice.model_validate(p) for p in cached]

        index = await self._build_cloud_sql_price_index(region)

        # Build SKU description substrings based on engine and HA/Zonal
        zone_type = "Regional" if ha else "Zonal"
        cpu_desc = f"Cloud SQL for {engine_norm}: {zone_type} - Standard"
        # RAM SKU contains "Standard Memory" — CPU SKU contains just "Standard" but not "Memory"
        # We look up CPU first (without "Memory") then RAM (with "Memory")
        ram_desc = f"Cloud SQL for {engine_norm}: {zone_type} - Standard Memory"

        # Find CPU price: matches cpu_desc but NOT "Memory"
        cpu_price: Decimal | None = None
        ram_price: Decimal | None = None
        for (desc, utype), price in index.items():
            if utype != "OnDemand":
                continue
            desc_lower = desc.lower()
            cpu_desc_lower = cpu_desc.lower()
            ram_desc_lower = ram_desc.lower()
            if ram_desc_lower in desc_lower and ram_price is None:
                ram_price = price
            elif cpu_desc_lower in desc_lower and "memory" not in desc_lower and cpu_price is None:
                cpu_price = price

        if cpu_price is None or ram_price is None:
            logger.warning(
                "GCP Cloud SQL: could not find %s/%s SKU for %s %s in %s (ha=%s). "
                "CPU found: %s, RAM found: %s",
                cpu_desc, ram_desc, engine_norm, instance_type, region, ha,
                cpu_price is not None, ram_price is not None,
            )
            return []

        total_price = (
            Decimal(str(vcpus)) * cpu_price
            + Decimal(str(memory_gb)) * ram_price
        )

        ha_label = "HA/Regional" if ha else "Zonal"
        result = NormalizedPrice(
            provider=CloudProvider.GCP,
            service="database",
            sku_id=f"gcp:cloud_sql:{engine_norm}:{instance_type}:{region}:{'ha' if ha else 'zonal'}",
            product_family="Cloud SQL",
            description=f"Cloud SQL {engine_norm} {instance_type} ({ha_label})",
            region=region,
            attributes={
                "instanceType": instance_type,
                "engine": engine_norm,
                "ha": str(ha),
            },
            pricing_term=PricingTerm.ON_DEMAND,
            price_per_unit=total_price,
            unit=PriceUnit.PER_HOUR,
        )

        await self._cache.set_prices(
            "gcp", "cloud_sql", region, cache_key_extras,
            [result.model_dump(mode="json")],
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return [result]

    # ------------------------------------------------------------------
    # Memorystore for Redis pricing
    # ------------------------------------------------------------------

    async def _build_memorystore_price_index(
        self, region: str
    ) -> dict[tuple[str, str], Decimal]:
        """Build price index for Memorystore for Redis SKUs in a given region."""
        cache_key = f"gcp:memorystore_price_index:{region}"
        cached = await self._cache.get_metadata(cache_key)
        if cached is not None:
            return {(k.split("|")[0], k.split("|")[1]): Decimal(v) for k, v in cached.items()}

        skus = await self._fetch_skus(_MEMORYSTORE_SERVICE_ID)

        index: dict[tuple[str, str], Decimal] = {}
        for sku in skus:
            service_regions = sku.get("serviceRegions", [])
            if region not in service_regions and "global" not in service_regions:
                continue

            desc = sku.get("description", "")
            category = sku.get("category", {})
            usage_type = category.get("usageType", "OnDemand")
            price = self._sku_price(sku)
            if price > 0:
                index[(desc, usage_type)] = price

        serialisable = {f"{k[0]}|{k[1]}": str(v) for k, v in index.items()}
        await self._cache.set_metadata(
            cache_key,
            serialisable,
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return index

    async def get_memorystore_price(
        self,
        capacity_gb: float,
        region: str,
        tier: str = "standard",
        hours_per_month: float = 730.0,
    ) -> list[NormalizedPrice]:
        """
        Get Memorystore for Redis pricing for the given capacity and tier.

        Memorystore is billed per GiB-hour of provisioned capacity. Two tiers:
          - basic: single zone, no HA
          - standard: HA with cross-zone replication
        """
        if capacity_gb <= 0:
            raise ValueError(f"capacity_gb must be positive, got {capacity_gb}")

        tier_lower = tier.lower()
        if tier_lower not in ("basic", "standard"):
            raise ValueError(
                f"Unknown Memorystore tier: {tier!r}. Valid values: 'basic', 'standard'."
            )

        # Determine target M-tier string from capacity_gb
        # M2: < 4 GB, M3: 4–11 GB, M4: 12–99 GB, M5: ≥ 100 GB
        if capacity_gb < 4:
            preferred_m = "m2"
        elif capacity_gb < 12:
            preferred_m = "m3"
        elif capacity_gb < 100:
            preferred_m = "m4"
        else:
            preferred_m = "m5"

        cache_key_extras = {
            "capacity_gb": str(capacity_gb),
            "tier": tier_lower,
            "hours_per_month": str(hours_per_month),
        }
        cached = await self._cache.get_prices("gcp", "memorystore", region, cache_key_extras)
        if cached:
            return [NormalizedPrice.model_validate(p) for p in cached]

        index = await self._build_memorystore_price_index(region)

        # Build ordered list of M-tiers to try (preferred first, then fallbacks)
        all_m_tiers = ["m2", "m3", "m4", "m5"]
        m_tier_order = [preferred_m] + [m for m in all_m_tiers if m != preferred_m]

        raw_rate: Decimal | None = None
        matched_desc: str = ""

        for m_tier in m_tier_order:
            for (desc, utype), price in index.items():
                if utype != "OnDemand":
                    continue
                desc_lower = desc.lower()
                if tier_lower == "basic":
                    if f"redis capacity basic {m_tier}" in desc_lower:
                        raw_rate = price
                        matched_desc = desc
                        break
                else:  # standard
                    if (
                        f"redis standard node capacity {m_tier}" in desc_lower
                        or f"redis capacity standard {m_tier}" in desc_lower
                    ):
                        raw_rate = price
                        matched_desc = desc
                        break
            if raw_rate is not None:
                break

        if raw_rate is None:
            logger.warning(
                "GCP Memorystore: could not find Redis %s SKU for capacity=%.1f GB in %s",
                tier, capacity_gb, region,
            )
            return []

        hourly_rate = raw_rate * Decimal(str(capacity_gb))

        result = NormalizedPrice(
            provider=CloudProvider.GCP,
            service="database",
            sku_id=f"gcp:memorystore:{tier_lower}:{capacity_gb}:{region}",
            product_family="Memorystore for Redis",
            description=f"Memorystore Redis {tier.capitalize()} {capacity_gb}GB in {region}",
            region=region,
            attributes={
                "capacity_gb": str(capacity_gb),
                "tier": tier_lower,
                "rate_per_gib_hr": str(raw_rate),
            },
            pricing_term=PricingTerm.ON_DEMAND,
            price_per_unit=hourly_rate,
            unit=PriceUnit.PER_HOUR,
        )

        await self._cache.set_prices(
            "gcp", "memorystore", region, cache_key_extras,
            [result.model_dump(mode="json")],
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return [result]

    # ------------------------------------------------------------------
    # BigQuery pricing
    # ------------------------------------------------------------------

    async def _build_bigquery_price_index(
        self, region: str
    ) -> dict[str, Decimal]:
        """Build price index for BigQuery SKUs in a given region.

        BigQuery has multi-region identifiers in serviceRegions (e.g. "us",
        "eu") as well as single-region codes. We store SKUs matched by the
        given region string directly; callers that want multi-region fallback
        must pass the multi-region code (e.g. "us") explicitly.

        Returns a dict keyed by SKU description.
        """
        cache_key = f"gcp:bigquery_price_index:{region}"
        cached = await self._cache.get_metadata(cache_key)
        if cached is not None:
            return {k: Decimal(v) for k, v in cached.items()}

        skus = await self._fetch_skus(_BIGQUERY_SERVICE_ID)

        index: dict[str, Decimal] = {}
        for sku in skus:
            service_regions = sku.get("serviceRegions", [])
            # BigQuery SKUs use named regions or multi-region codes ("us", "eu", "asia")
            # — no "global" SKUs exist, so no global fallback is needed here.
            if region not in service_regions:
                continue

            desc = sku.get("description", "")
            # Use paid-price tier for Analysis SKUs (tier[0] is free quota = $0)
            if "Analysis" in desc and "Streaming" not in desc:
                price = self._sku_paid_price(sku)
            else:
                price = self._sku_price(sku)

            if price > 0:
                index[desc] = price

        serialisable = {k: str(v) for k, v in index.items()}
        await self._cache.set_metadata(
            cache_key,
            serialisable,
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return index

    async def get_bigquery_price(
        self,
        region: str = "us",
        query_tb: float | None = None,
        active_storage_gb: float | None = None,
        longterm_storage_gb: float | None = None,
        streaming_gb: float | None = None,
    ) -> dict:
        """Return BigQuery pricing for storage, analysis queries, and streaming inserts.

        BigQuery uses a multi-region / single-region model. Pass ``region="us"``
        for the US multi-region, ``region="us-central1"`` for a specific region.
        When the exact region yields no SKUs we fall back to the corresponding
        multi-region prefix (e.g. "us-central1" → "us", "europe-west1" → "eu").
        """
        # Try the exact region first; fall back to multi-region prefix if empty
        index = await self._build_bigquery_price_index(region)
        if not index:
            # Derive a multi-region prefix from the single-region code
            if region.startswith("us-") or region == "us":
                fallback = "us"
            elif region.startswith("europe-") or region == "eu":
                fallback = "eu"
            elif region.startswith("asia-") or region.startswith("australia-"):
                fallback = "asia"
            else:
                fallback = ""

            if fallback and fallback != region:
                index = await self._build_bigquery_price_index(fallback)

        # Extract individual rates from index
        analysis_rate: Decimal = Decimal("0")
        active_storage_rate: Decimal = Decimal("0")
        longterm_storage_rate: Decimal = Decimal("0")
        streaming_rate: Decimal = Decimal("0")

        for desc, price in index.items():
            if "Active Logical Storage" in desc and active_storage_rate == 0:
                active_storage_rate = price
            elif "Long Term Logical Storage" in desc and longterm_storage_rate == 0:
                longterm_storage_rate = price
            elif "Analysis" in desc and "Streaming" not in desc and analysis_rate == 0:
                analysis_rate = price
            elif "Streaming Insert" in desc and streaming_rate == 0:
                streaming_rate = price

        result: dict[str, Any] = {
            "region": region,
            "provider": "gcp",
            "service": "bigquery",
            "analysis_rate_per_tib": f"${analysis_rate:.6f}/TiB",
            "active_storage_rate_per_gib_mo": f"${active_storage_rate:.6f}/GiB-mo",
            "longterm_storage_rate_per_gib_mo": f"${longterm_storage_rate:.6f}/GiB-mo",
            "streaming_insert_rate_per_gib": f"${streaming_rate:.6f}/GiB",
        }

        if query_tb is not None:
            cost = Decimal(str(query_tb)) * analysis_rate
            result["estimated_query_cost"] = f"${cost:.2f}"

        if active_storage_gb is not None:
            cost = Decimal(str(active_storage_gb)) / Decimal("1024") * active_storage_rate
            result["estimated_active_storage_cost"] = f"${cost:.2f}"

        if longterm_storage_gb is not None:
            cost = Decimal(str(longterm_storage_gb)) / Decimal("1024") * longterm_storage_rate
            result["estimated_longterm_storage_cost"] = f"${cost:.2f}"

        if streaming_gb is not None:
            cost = Decimal(str(streaming_gb)) * streaming_rate
            result["estimated_streaming_cost"] = f"${cost:.2f}"

        result["note"] = (
            "BigQuery free tier: first 1 TiB/month of query processing is free; "
            "first 10 GiB/month of active storage is free. "
            "Rates shown are for usage above the free tier."
        )
        return result

    # ------------------------------------------------------------------
    # GKE (Google Kubernetes Engine) pricing
    # ------------------------------------------------------------------

    async def _build_gke_price_index(
        self, region: str
    ) -> dict[tuple[str, str], Decimal]:
        """Build price index for GKE SKUs in a given region."""
        cache_key = f"gcp:gke_price_index:{region}"
        cached = await self._cache.get_metadata(cache_key)
        if cached is not None:
            return {(k.split("|")[0], k.split("|")[1]): Decimal(v) for k, v in cached.items()}

        skus = await self._fetch_skus(_GKE_SERVICE_ID)

        index: dict[tuple[str, str], Decimal] = {}
        for sku in skus:
            service_regions = sku.get("serviceRegions", [])
            if region not in service_regions and "global" not in service_regions:
                continue

            desc = sku.get("description", "")
            category = sku.get("category", {})
            usage_type = category.get("usageType", "OnDemand")
            price = self._sku_price(sku)
            if price > 0:
                index[(desc, usage_type)] = price

        serialisable = {f"{k[0]}|{k[1]}": str(v) for k, v in index.items()}
        await self._cache.set_metadata(
            cache_key,
            serialisable,
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return index

    async def get_gke_price(
        self,
        region: str,
        mode: str = "standard",
        node_count: int = 3,
        node_type: str = "n1-standard-4",
        vcpu: float = 0.0,
        memory_gb: float = 0.0,
        hours_per_month: float = 730.0,
    ) -> dict:
        """
        Returns GKE pricing info (not a NormalizedPrice since structure differs by mode).

        Standard mode: flat cluster management fee + pointer to get_compute_price for nodes.
        Autopilot mode: per-mCPU-hour + per-GiB-hour rates for pods.
        """
        if vcpu < 0 or memory_gb < 0:
            raise ValueError("vcpu and memory_gb must be non-negative")

        index = await self._build_gke_price_index(region)

        if mode == "standard":
            # Search for cluster management fee SKU — OnDemand only to avoid CUD variants
            rate: Decimal | None = None
            for (desc, utype), price in index.items():
                desc_lower = desc.lower()
                if (
                    utype == "OnDemand"
                    and "kubernetes engine" in desc_lower
                    and "autopilot" not in desc_lower
                    and "committed" not in desc_lower
                    and "cost attribution" not in desc_lower
                ):
                    rate = price
                    break

            # Fall back to documented on-demand rate if SKU not found
            if rate is None:
                rate = Decimal("0.10")

            return {
                "mode": "standard",
                "cluster_management_fee_per_hr": f"${rate:.6f}",
                "cluster_management_monthly": f"${rate * Decimal(str(hours_per_month)):.2f}",
                "node_pricing_hint": (
                    f"Use get_compute_price(provider='gcp', instance_type='{node_type}', "
                    f"region='{region}') for each node. Multiply by {node_count} nodes."
                ),
                "total_monthly_note": (
                    f"Total = cluster fee + ({node_count} × node_hourly × {hours_per_month:.0f} hrs)"
                ),
                "region": region,
            }

        else:  # autopilot
            # Search for Autopilot Balanced Pod mCPU SKU (per milli-CPU)
            mcpu_rate: Decimal | None = None
            mem_rate: Decimal | None = None
            for (desc, utype), price in index.items():
                desc_lower = desc.lower()
                if "autopilot balanced pod mcpu requests" in desc_lower and mcpu_rate is None:
                    mcpu_rate = price
                elif "autopilot balanced pod memory requests" in desc_lower and mem_rate is None:
                    mem_rate = price
                if mcpu_rate is not None and mem_rate is not None:
                    break

            # mCPU rate is per milli-CPU — multiply by 1000 to get per-vCPU rate
            vcpu_rate = mcpu_rate * Decimal("1000") if mcpu_rate is not None else Decimal("0")
            mem_rate_val = mem_rate if mem_rate is not None else Decimal("0")

            result: dict = {
                "mode": "autopilot",
                "vcpu_rate_per_hr": f"${vcpu_rate:.6f}",
                "memory_rate_per_gib_hr": f"${mem_rate_val:.6f}",
                "requested_vcpu": vcpu,
                "requested_memory_gb": memory_gb,
                "region": region,
                "note": "Autopilot charges for actual pod resource requests only",
            }

            if vcpu > 0 or memory_gb > 0:
                hourly = (
                    Decimal(str(vcpu)) * vcpu_rate
                    + Decimal(str(memory_gb)) * mem_rate_val
                )
                result["hourly_cost"] = f"${hourly:.6f}"
                result["monthly_cost"] = f"${hourly * Decimal(str(hours_per_month)):.2f}"
            else:
                result["hourly_cost"] = "$0.000000"
                result["monthly_cost"] = "$0.00"

            return result

    async def _get_gcs_storage_price(
        self,
        storage_class: str,
        region: str,
    ) -> list[NormalizedPrice]:
        """Fetch GCS (Cloud Storage) pricing for a given storage class and region."""
        desc_substring = _GCS_STORAGE_CLASSES[storage_class]

        cache_key_extras = {"storage_type": storage_class, "source": "gcs"}
        cached = await self._cache.get_prices("gcp", "gcs_storage", region, cache_key_extras)
        if cached:
            return [NormalizedPrice.model_validate(p) for p in cached]

        index = await self._build_gcs_price_index(region)

        price = self._lookup_price(index, desc_substring, "OnDemand")
        if price is None:
            return []

        result = NormalizedPrice(
            provider=CloudProvider.GCP,
            service="storage",
            sku_id=f"gcp:gcs:{storage_class}:{region}",
            product_family="Cloud Storage",
            description=f"GCS {storage_class} storage in {region}",
            region=region,
            attributes={"storage_type": storage_class, "class": storage_class},
            pricing_term=PricingTerm.ON_DEMAND,
            price_per_unit=price,
            unit=PriceUnit.PER_GB_MONTH,
        )

        await self._cache.set_prices(
            "gcp", "gcs_storage", region, cache_key_extras,
            [result.model_dump(mode="json")],
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return [result]

    async def get_storage_price(
        self,
        storage_type: str,
        region: str,
        size_gb: float | None = None,
    ) -> list[NormalizedPrice]:
        # Route GCS storage classes to the GCS path
        storage_type_lower = storage_type.lower()
        if storage_type_lower in _GCS_STORAGE_CLASSES:
            return await self._get_gcs_storage_price(storage_type_lower, region)

        sku_patterns = GCP_STORAGE_SKU.get(storage_type_lower)
        if sku_patterns is None:
            raise ValueError(
                f"Unknown GCP storage type: {storage_type!r}. "
                f"Supported: {sorted(GCP_STORAGE_SKU)} or GCS classes: {sorted(_GCS_STORAGE_CLASSES)}"
            )

        cache_key_extras = {"storage_type": storage_type}
        cached = await self._cache.get_prices("gcp", "storage", region, cache_key_extras)
        if cached:
            return [NormalizedPrice.model_validate(p) for p in cached]

        index = await self._build_price_index(region)

        desc = sku_patterns["desc"]
        price = self._lookup_price(index, desc, "OnDemand")
        if price is None:
            # Try alternate description
            alt_desc = sku_patterns.get("alt_desc", "")
            if alt_desc:
                price = self._lookup_price(index, alt_desc, "OnDemand")

        if price is None:
            return []

        result = NormalizedPrice(
            provider=CloudProvider.GCP,
            service="storage",
            sku_id=f"gcp:storage:{storage_type}:{region}",
            product_family="Storage",
            description=f"{storage_type} in {region}",
            region=region,
            attributes={"storage_type": storage_type},
            pricing_term=PricingTerm.ON_DEMAND,
            price_per_unit=price,
            unit=PriceUnit.PER_GB_MONTH,
        )

        await self._cache.set_prices(
            "gcp", "storage", region, cache_key_extras,
            [result.model_dump(mode="json")],
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return [result]

    async def search_pricing(
        self,
        query: str,
        region: str | None = None,
        max_results: int = 20,
    ) -> list[NormalizedPrice]:
        """
        Search GCP instance types by family or type string.
        Returns on-demand pricing for matching instances.
        """
        query_lower = query.lower()
        results: list[NormalizedPrice] = []

        target_region = region or "us-central1"

        for itype in GCP_INSTANCE_SPECS:
            if query_lower not in itype:
                continue
            try:
                prices = await self.get_compute_price(itype, target_region)
                results.extend(prices)
            except Exception:
                continue
            if len(results) >= max_results:
                break

        return results

    async def list_regions(self, service: str = "compute") -> list[str]:
        return list_gcp_regions()

    async def list_instance_types(
        self,
        region: str,
        family: str | None = None,
        min_vcpus: int | None = None,
        min_memory_gb: float | None = None,
        gpu: bool = False,
    ) -> list[InstanceTypeInfo]:
        results: list[InstanceTypeInfo] = []
        gpu_families = {"a2"}

        for itype, (vcpus, mem_gb) in GCP_INSTANCE_SPECS.items():
            fam = get_machine_family(itype)
            is_gpu = fam in gpu_families

            if gpu and not is_gpu:
                continue
            if not gpu and is_gpu:
                continue
            if family and not itype.startswith(family):
                continue
            if min_vcpus and vcpus < min_vcpus:
                continue
            if min_memory_gb and mem_gb < min_memory_gb:
                continue

            results.append(InstanceTypeInfo(
                provider=CloudProvider.GCP,
                instance_type=itype,
                vcpu=vcpus,
                memory_gb=mem_gb,
                gpu_count=1 if is_gpu else 0,
                region=region,
            ))

        return results

    async def check_availability(
        self,
        service: str,
        sku_or_type: str,
        region: str,
    ) -> bool:
        if service == "compute":
            try:
                prices = await self.get_compute_price(sku_or_type, region)
                return len(prices) > 0
            except ValueError:
                return False
        if service == "storage":
            try:
                prices = await self.get_storage_price(sku_or_type, region)
                return len(prices) > 0
            except ValueError:
                return False
        return False

    # ------------------------------------------------------------------
    # Vertex AI pricing
    # ------------------------------------------------------------------

    async def _build_vertex_price_index(
        self, region: str
    ) -> dict[tuple[str, str], Decimal]:
        """Build a price index for Vertex AI SKUs in a given region."""
        cache_key = f"gcp:vertex_price_index:{region}"
        cached = await self._cache.get_metadata(cache_key)
        if cached is not None:
            return {(k.split("|")[0], k.split("|")[1]): Decimal(v) for k, v in cached.items()}

        skus = await self._fetch_skus(_VERTEX_SERVICE_ID)

        index: dict[tuple[str, str], Decimal] = {}
        for sku in skus:
            service_regions = sku.get("serviceRegions", [])
            if region not in service_regions and "global" not in service_regions:
                continue

            desc = sku.get("description", "")
            category = sku.get("category", {})
            usage_type = category.get("usageType", "OnDemand")
            price = self._sku_price(sku)
            if price > 0:
                index[(desc, usage_type)] = price

        serialisable = {f"{k[0]}|{k[1]}": str(v) for k, v in index.items()}
        await self._cache.set_metadata(
            cache_key,
            serialisable,
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return index

    async def get_vertex_price(
        self,
        machine_type: str,
        region: str = "us-central1",
        hours: float = 730.0,
        task: str = "training",
    ) -> dict:
        """
        Returns Vertex AI custom training / prediction compute pricing.

        Looks up per-vCPU-hour and per-GiB-RAM-hour rates for the given
        machine family (e.g. "n1" from "n1-standard-4") from Vertex AI SKUs.
        Returns a dict with rates and estimated cost for the given hours.
        """
        # Extract family prefix: "n1" from "n1-standard-4", "a2" from "a2-highgpu-1g"
        family = machine_type.split("-")[0].lower() if machine_type else "n1"

        index = await self._build_vertex_price_index(region)

        if not index:
            return {
                "error": (
                    f"No Vertex AI pricing found for machine_type={machine_type!r} "
                    f"in region={region!r}. No SKUs returned from Vertex AI catalog."
                )
            }

        # Search for vCPU and RAM SKUs matching this family and task
        task_lower = task.lower()
        task_keyword = "training" if "train" in task_lower else "prediction"

        vcpu_rate: Decimal | None = None
        ram_rate: Decimal | None = None

        for (desc, _utype), price in index.items():
            desc_lower = desc.lower()
            if family not in desc_lower:
                continue
            if task_keyword not in desc_lower:
                # Be lenient: skip only if the description explicitly mentions a different task
                if "training" in desc_lower or "prediction" in desc_lower:
                    continue
            is_vcpu = "vcpu" in desc_lower or "core" in desc_lower
            is_ram = "ram" in desc_lower or "memory" in desc_lower
            if is_vcpu and vcpu_rate is None:
                vcpu_rate = price
            elif is_ram and ram_rate is None:
                ram_rate = price
            if vcpu_rate is not None and ram_rate is not None:
                break

        if vcpu_rate is None or ram_rate is None:
            return {
                "error": (
                    f"No Vertex AI SKUs matched machine_type={machine_type!r} "
                    f"(family={family!r}), task={task!r}, region={region!r}. "
                    "The SKU catalog may use different naming — try a broader family prefix."
                ),
                "hint": "Available SKU descriptions can be inspected via search_pricing(service='vertexai').",
            }

        vcpu_rate_val = vcpu_rate if vcpu_rate is not None else Decimal("0")
        ram_rate_val = ram_rate if ram_rate is not None else Decimal("0")

        result: dict = {
            "provider": "gcp",
            "service": "vertex_ai",
            "machine_type": machine_type,
            "family": family,
            "task": task,
            "region": region,
            "hours": hours,
            "vcpu_rate_per_hr": f"${vcpu_rate_val:.6f}",
            "ram_rate_per_gib_hr": f"${ram_rate_val:.6f}",
            "note": (
                f"Rates are per vCPU-hour and per GiB-RAM-hour. "
                f"To estimate total cost: (vcpus × vcpu_rate + ram_gib × ram_rate) × {hours:.0f} hours. "
                f"Use list_instance_types(provider='gcp', region='{region}') to get specs."
            ),
        }
        return result

    async def get_gemini_price(
        self,
        model: str = "gemini-1.5-flash",
        region: str = "us-central1",
    ) -> dict:
        """
        Returns Vertex AI Gemini model token/character pricing.

        Searches Vertex AI SKUs for entries whose description contains "Gemini"
        and the given model name substring. Returns input and output rates
        found in the catalog.
        """
        index = await self._build_vertex_price_index(region)

        if not index:
            return {
                "error": (
                    f"No Vertex AI SKUs found for region={region!r}. "
                    "Gemini pricing may be global — try region='global' or 'us-central1'."
                )
            }

        model_lower = model.lower()
        # Extract meaningful parts from slug like "gemini-1.5-flash" → ["1.5", "flash"]
        model_parts = [
            p for p in model_lower.replace("gemini-", "").replace("gemini", "").split("-")
            if p and len(p) > 1
        ]

        rates: dict[str, str] = {}

        for (desc, _utype), price in index.items():
            desc_lower = desc.lower()
            if "gemini" not in desc_lower:
                continue
            # Match on model name parts if we have any
            if model_parts and not all(p in desc_lower for p in model_parts):
                continue
            # Determine input vs output direction
            if "input" in desc_lower:
                direction = "input"
            elif "output" in desc_lower:
                direction = "output"
            else:
                direction = "other"
            # Use description as key to avoid collisions
            safe_key = f"{direction}:{desc}"
            rates[safe_key] = f"${price:.8f}"

        if not rates:
            return {
                "error": (
                    f"No Gemini SKUs matched model={model!r} in region={region!r}. "
                    "The model name may differ from the SKU catalog. "
                    "Try model='gemini-1.5-flash', 'gemini-1.0-pro', or 'gemini-1.5-pro'."
                ),
                "region": region,
                "model": model,
            }

        return {
            "provider": "gcp",
            "service": "vertex_ai_gemini",
            "model": model,
            "region": region,
            "rates": rates,
            "note": (
                "Rates are per character or per token depending on the SKU. "
                "Check the 'rates' keys for input/output direction and unit from SKU description."
            ),
        }

    # ------------------------------------------------------------------
    # Cloud Load Balancing / Cloud CDN / Cloud NAT pricing
    # All three use Compute Engine service ID, so _fetch_skus hits the
    # already-cached payload — no extra HTTP calls.
    # ------------------------------------------------------------------

    async def _build_networking_price_index(
        self, region: str
    ) -> dict[str, Decimal]:
        """Build a price index for LB / CDN / NAT SKUs from Compute Engine catalog.

        Filters to descriptions that mention load balancing, CDN, or NAT so the
        index stays small. Returns a dict keyed by lowercase SKU description.
        """
        cache_key = f"gcp:networking_price_index:{region}"
        cached = await self._cache.get_metadata(cache_key)
        if cached is not None:
            return {k: Decimal(v) for k, v in cached.items()}

        # Reuses the already-cached Compute Engine SKU payload.
        skus = await self._fetch_skus(_COMPUTE_SERVICE_ID)

        _NETWORKING_KEYWORDS = ("load bal", "tcp proxy", "ssl proxy", "cloud cdn", "cloud nat", "nat gateway")

        index: dict[str, Decimal] = {}
        for sku in skus:
            service_regions = sku.get("serviceRegions", [])
            if region not in service_regions and "global" not in service_regions:
                continue

            desc = sku.get("description", "")
            desc_lower = desc.lower()
            if not any(kw in desc_lower for kw in _NETWORKING_KEYWORDS):
                continue

            price = self._sku_price(sku)
            if price > 0:
                index[desc_lower] = price

        serialisable = {k: str(v) for k, v in index.items()}
        await self._cache.set_metadata(
            cache_key,
            serialisable,
            ttl_hours=self._settings.cache_ttl_hours,
        )
        return index

    async def get_cloud_lb_price(
        self,
        region: str = "us-central1",
        lb_type: str = "https",
        rule_count: int = 1,
        data_gb: float = 0.0,
        hours_per_month: float = 730.0,
    ) -> dict:
        """Return Cloud Load Balancing pricing for forwarding rules and data processed.

        Args:
            region: GCP region, e.g. "us-central1".
            lb_type: Load balancer type — "https" (External HTTP(S), default),
                     "tcp" (TCP Proxy), "ssl" (SSL Proxy), "network" (L4 Network LB),
                     "internal" (Internal LB, billed as network forwarding rules).
            rule_count: Number of forwarding rules.
            data_gb: GB of data processed per month (omit for rule cost only).
            hours_per_month: Hours active per month (default 730 = always-on).
        """
        index = await self._build_networking_price_index(region)

        # Documented GCP list price fallback: $0.008/hr per forwarding rule
        _FALLBACK_RULE_RATE = Decimal("0.008")
        # Documented GCP data-processed fallback: $0.008/GB (HTTPS LB)
        _FALLBACK_DATA_RATE = Decimal("0.008")

        lb_type_lower = lb_type.lower()

        rule_rate: Decimal | None = None
        data_rate: Decimal | None = None
        fallback = False

        for desc, price in index.items():
            # Match rule rate by lb_type
            if rule_rate is None:
                if lb_type_lower in ("https", "http"):
                    if ("external http" in desc or "https load balancing rule" in desc) and "rule" in desc:
                        rule_rate = price
                elif lb_type_lower in ("tcp",):
                    if "tcp proxy" in desc and "rule" in desc:
                        rule_rate = price
                elif lb_type_lower in ("ssl",):
                    if "ssl proxy" in desc and "rule" in desc:
                        rule_rate = price
                elif lb_type_lower in ("network", "internal"):
                    if "network load balancing" in desc and "forwarding rule" in desc:
                        rule_rate = price

            # Match data-processed rate
            if data_rate is None:
                if "data processed by" in desc and "load bal" in desc:
                    data_rate = price

        if rule_rate is None:
            rule_rate = _FALLBACK_RULE_RATE
            fallback = True
        if data_rate is None:
            data_rate = _FALLBACK_DATA_RATE

        rule_monthly = rule_rate * Decimal(str(rule_count)) * Decimal(str(hours_per_month))
        data_cost = data_rate * Decimal(str(data_gb))
        total = rule_monthly + data_cost

        result: dict[str, Any] = {
            "provider": "gcp",
            "region": region,
            "service": "cloud_load_balancing",
            "lb_type": lb_type,
            "rule_count": rule_count,
            "rule_rate_per_hr": f"${rule_rate:.6f}/hr",
            "data_processed_rate_per_gb": f"${data_rate:.6f}/GB",
            "monthly_rule_cost": f"${rule_monthly:.2f}",
        }

        if data_gb > 0:
            result["data_gb"] = data_gb
            result["monthly_data_cost"] = f"${data_cost:.2f}"
            result["monthly_total"] = f"${total:.2f}"

        if fallback:
            result["fallback"] = True

        result["note"] = (
            "Cloud Load Balancing: forwarding rules are billed per hour. "
            "First 5 rules on an LB are each billed separately; additional rules at reduced rate. "
            "Data processed (ingress + egress) is billed per GB. "
            "Internal LB pricing differs — consult GCP docs for internal forwarding rule rates."
        )
        return result

    async def get_cloud_cdn_price(
        self,
        region: str = "us-central1",
        egress_gb: float = 0.0,
        cache_fill_gb: float = 0.0,
    ) -> dict:
        """Return Cloud CDN pricing for cache egress and cache fill.

        Args:
            region: GCP region for destination/origin, e.g. "us-central1".
            egress_gb: GB egressed from CDN to end users per month.
            cache_fill_gb: GB filled into CDN from origin per month.
        """
        index = await self._build_networking_price_index(region)

        # Documented GCP list price fallback: $0.02/GB egress (NA destination)
        _FALLBACK_EGRESS_RATE = Decimal("0.02")
        # Documented GCP list price fallback: $0.01/GB cache fill
        _FALLBACK_FILL_RATE = Decimal("0.01")

        egress_rate: Decimal | None = None
        fill_rate: Decimal | None = None
        fallback = False

        for desc, price in index.items():
            if egress_rate is None and "cloud cdn cache egress" in desc:
                egress_rate = price
            if fill_rate is None and "cloud cdn cache fill" in desc:
                fill_rate = price
            if egress_rate is not None and fill_rate is not None:
                break

        if egress_rate is None:
            egress_rate = _FALLBACK_EGRESS_RATE
            fallback = True
        if fill_rate is None:
            fill_rate = _FALLBACK_FILL_RATE
            if not fallback:
                fallback = True

        egress_cost = egress_rate * Decimal(str(egress_gb))
        fill_cost = fill_rate * Decimal(str(cache_fill_gb))
        total = egress_cost + fill_cost

        result: dict[str, Any] = {
            "provider": "gcp",
            "region": region,
            "service": "cloud_cdn",
            "egress_rate_per_gb": f"${egress_rate:.6f}/GB",
            "cache_fill_rate_per_gb": f"${fill_rate:.6f}/GB",
        }

        if egress_gb > 0:
            result["egress_gb"] = egress_gb
            result["monthly_egress_cost"] = f"${egress_cost:.2f}"

        if cache_fill_gb > 0:
            result["cache_fill_gb"] = cache_fill_gb
            result["monthly_cache_fill_cost"] = f"${fill_cost:.2f}"

        if egress_gb > 0 or cache_fill_gb > 0:
            result["monthly_total"] = f"${total:.2f}"

        if fallback:
            result["fallback"] = True

        result["note"] = (
            "Cloud CDN egress rates vary by destination region (NA/EU cheapest, "
            "APAC/Oceania/LATAM/ME/Africa higher). Cache fill (origin → CDN) is "
            "charged at network egress rates. First 10 GiB/month CDN egress is free."
        )
        return result

    async def get_cloud_nat_price(
        self,
        region: str = "us-central1",
        gateway_count: int = 1,
        data_gb: float = 0.0,
        hours_per_month: float = 730.0,
    ) -> dict:
        """Return Cloud NAT pricing for gateway uptime and data processed.

        Args:
            region: GCP region, e.g. "us-central1".
            gateway_count: Number of Cloud NAT gateways.
            data_gb: GB processed through NAT per month.
            hours_per_month: Hours active per month (default 730 = always-on).
        """
        index = await self._build_networking_price_index(region)

        # Documented GCP list price fallback: $0.044/hr per NAT gateway
        _FALLBACK_GATEWAY_RATE = Decimal("0.044")
        # Documented GCP list price fallback: $0.045/GB data processed
        _FALLBACK_DATA_RATE = Decimal("0.045")

        gateway_rate: Decimal | None = None
        data_rate: Decimal | None = None
        fallback = False

        for desc, price in index.items():
            if gateway_rate is None and "cloud nat gateway" in desc:
                gateway_rate = price
            if data_rate is None and "cloud nat" in desc and (
                "data processed" in desc or "nat gateway data" in desc
            ):
                data_rate = price
            if gateway_rate is not None and data_rate is not None:
                break

        if gateway_rate is None:
            gateway_rate = _FALLBACK_GATEWAY_RATE
            fallback = True
        if data_rate is None:
            data_rate = _FALLBACK_DATA_RATE
            if not fallback:
                fallback = True

        gateway_monthly = gateway_rate * Decimal(str(gateway_count)) * Decimal(str(hours_per_month))
        data_cost = data_rate * Decimal(str(data_gb))
        total = gateway_monthly + data_cost

        result: dict[str, Any] = {
            "provider": "gcp",
            "region": region,
            "service": "cloud_nat",
            "gateway_count": gateway_count,
            "gateway_rate_per_hr": f"${gateway_rate:.6f}/hr",
            "data_processed_rate_per_gb": f"${data_rate:.6f}/GB",
            "monthly_gateway_cost": f"${gateway_monthly:.2f}",
        }

        if data_gb > 0:
            result["data_gb"] = data_gb
            result["monthly_data_cost"] = f"${data_cost:.2f}"
            result["monthly_total"] = f"${total:.2f}"

        if fallback:
            result["fallback"] = True

        result["note"] = (
            "Cloud NAT gateway uptime is billed per hour regardless of traffic. "
            "Data processed includes all bytes flowing through NAT (both directions). "
            "Intra-region traffic is not charged for data processing."
        )
        return result

    async def get_cloud_armor_price(
        self,
        policy_count: int = 1,
        monthly_requests_millions: float = 0.0,
    ) -> dict:
        """Return Cloud Armor pricing for security policies and request evaluation.

        Attempts live catalog lookup; falls back to published GCP rates if the
        service ID is unavailable or returns no matching SKUs.

        Args:
            policy_count: Number of enforced security policies.
            monthly_requests_millions: Millions of requests evaluated per month.
        """
        # Published GCP fallback rates (2024)
        _FALLBACK_POLICY_RATE = Decimal("0.75")   # per policy per month
        _FALLBACK_REQUEST_RATE = Decimal("0.75")  # per million requests

        policy_rate: Decimal | None = None
        request_rate: Decimal | None = None
        fallback = False
        catalog_available = True

        try:
            skus = await self._fetch_skus(_CLOUD_ARMOR_SERVICE_ID)
        except Exception:
            catalog_available = False
            skus = []

        if catalog_available:
            for sku in skus:
                desc = sku.get("description", "").lower()
                if "cloud armor" not in desc:
                    continue
                if policy_rate is None and ("policy" in desc or "security policy" in desc):
                    price = self._sku_price(sku)
                    if price > 0:
                        policy_rate = price
                if request_rate is None and "request" in desc:
                    price = self._sku_price(sku)
                    if price > 0:
                        request_rate = price
                if policy_rate is not None and request_rate is not None:
                    break

        if policy_rate is None:
            policy_rate = _FALLBACK_POLICY_RATE
            fallback = True
        if request_rate is None:
            request_rate = _FALLBACK_REQUEST_RATE
            if not fallback:
                fallback = True

        result: dict[str, Any] = {
            "provider": "gcp",
            "service": "cloud_armor",
            "policy_rate_per_month": f"${policy_rate:.2f}/policy/month",
            "request_rate_per_million": f"${request_rate:.6f}/million requests",
        }

        if policy_count > 0:
            policy_cost = policy_rate * Decimal(str(policy_count))
            result["estimated_policy_cost"] = f"${policy_cost:.2f}"

        if monthly_requests_millions > 0:
            request_cost = request_rate * Decimal(str(monthly_requests_millions))
            result["estimated_request_cost"] = f"${request_cost:.2f}"

        if policy_count > 0 or monthly_requests_millions > 0:
            policy_cost_val = policy_rate * Decimal(str(policy_count))
            request_cost_val = request_rate * Decimal(str(monthly_requests_millions))
            result["estimated_total_monthly"] = f"${policy_cost_val + request_cost_val:.2f}"

        if fallback:
            result["fallback"] = True

        result["note"] = (
            "Cloud Armor Standard: $0.75/policy/month + $0.75/million requests evaluated. "
            "Enterprise tier ($3,000/month per project) adds advanced DDoS protection, "
            "adaptive protection, and threat intelligence — contact GCP for details."
        )
        return result

    async def get_cloud_monitoring_price(
        self,
        ingestion_mib: float = 0.0,
    ) -> dict:
        """Return Cloud Monitoring pricing for custom/external metric ingestion.

        Applies a 150 MiB/month free tier per billing account, then tiered rates.
        Attempts live catalog lookup; falls back to published GCP rates if unavailable.

        Args:
            ingestion_mib: MiB of custom or external metrics ingested per month.
        """
        # Published GCP fallback rates (2024) — tiered, per MiB
        _FREE_TIER_MIB = Decimal("150")
        _TIER1_RATE = Decimal("0.258")   # 0 – 100,000 MiB/month
        _TIER2_RATE = Decimal("0.151")   # 100,001 – 250,000 MiB/month
        _TIER3_RATE = Decimal("0.062")   # above 250,000 MiB/month
        _TIER1_LIMIT = Decimal("100000")
        _TIER2_LIMIT = Decimal("250000")

        tier1_rate = _TIER1_RATE
        tier2_rate = _TIER2_RATE
        tier3_rate = _TIER3_RATE
        fallback = False

        try:
            skus = await self._fetch_skus(_CLOUD_MONITORING_SERVICE_ID)
        except Exception:
            skus = []
            fallback = True

        if not fallback:
            # Search for an ingestion SKU — if found, use the first-tier price
            sku_matched = False
            for sku in skus:
                desc = sku.get("description", "").lower()
                # Real SKU description: "Workload Metrics Samples Ingested"
                if ("monitoring" in desc or "workload metric" in desc) and ("metric" in desc or "ingest" in desc):
                    price = self._sku_price(sku)
                    if price > 0:
                        tier1_rate = price
                        sku_matched = True
                        break
            if not sku_matched:
                fallback = True

        result: dict[str, Any] = {
            "provider": "gcp",
            "service": "cloud_monitoring",
            "free_tier_mib": 150,
            "tier1_rate_per_mib": f"${tier1_rate:.3f}/MiB (0–100,000 MiB/month)",
            "tier2_rate_per_mib": f"${tier2_rate:.3f}/MiB (100,001–250,000 MiB/month)",
            "tier3_rate_per_mib": f"${tier3_rate:.3f}/MiB (above 250,000 MiB/month)",
        }

        if ingestion_mib > 0:
            total_mib = Decimal(str(ingestion_mib))
            # Apply free tier
            billable_mib = max(Decimal("0"), total_mib - _FREE_TIER_MIB)

            cost = Decimal("0")
            remaining = billable_mib

            # Tier 1: up to 100,000 MiB
            tier1_billable = min(remaining, _TIER1_LIMIT)
            cost += tier1_billable * tier1_rate
            remaining -= tier1_billable

            # Tier 2: 100,001 – 250,000 MiB
            if remaining > 0:
                tier2_billable = min(remaining, _TIER2_LIMIT - _TIER1_LIMIT)
                cost += tier2_billable * tier2_rate
                remaining -= tier2_billable

            # Tier 3: above 250,000 MiB
            if remaining > 0:
                cost += remaining * tier3_rate

            result["ingestion_mib"] = ingestion_mib
            result["estimated_monthly_cost"] = f"${cost:.2f}"

        if fallback:
            result["fallback"] = True

        result["note"] = (
            "Cloud Monitoring: first 150 MiB/month of custom and external metrics per "
            "billing account is free. Chargeable volume uses tiered rates: "
            "$0.258/MiB (0–100,000), $0.151/MiB (100,001–250,000), $0.062/MiB (above 250,000). "
            "GCP built-in metrics are always free."
        )
        return result

    async def get_effective_price(
        self,
        service: str,
        instance_type: str,
        region: str,
    ) -> list[EffectivePrice]:
        raise NotConfiguredError(
            "GCP effective pricing via BigQuery billing export is not yet implemented "
            "(planned for Phase 4). Use get_compute_price with term='cud_1yr' or "
            "'cud_3yr' to see committed-use discount rates."
        )

    async def close(self) -> None:
        if self._http and not self._http.is_closed:
            await self._http.aclose()
