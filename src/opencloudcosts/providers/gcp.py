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

    async def get_storage_price(
        self,
        storage_type: str,
        region: str,
        size_gb: float | None = None,
    ) -> list[NormalizedPrice]:
        sku_patterns = GCP_STORAGE_SKU.get(storage_type.lower())
        if sku_patterns is None:
            raise ValueError(
                f"Unknown GCP storage type: {storage_type!r}. "
                f"Supported: {sorted(GCP_STORAGE_SKU)}"
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
