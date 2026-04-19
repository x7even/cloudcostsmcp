"""Discovery and availability MCP tools — v0.8.0."""
from __future__ import annotations

import asyncio
import logging
from typing import Any

from mcp.server.fastmcp import Context
from pydantic import TypeAdapter

from opencloudcosts.models import PricingSpec
from opencloudcosts.providers.base import NotConfiguredError
from opencloudcosts.utils.baseline import apply_baseline_deltas
from opencloudcosts.utils.regions import region_display_name

logger = logging.getLogger(__name__)

_SPEC_ADAPTER: TypeAdapter[PricingSpec] = TypeAdapter(PricingSpec)

# Default region sets for fan-out tools — avoids cold-cache timeouts.
# Pass regions=["all"] to search every available region (slower on first run).
_AWS_MAJOR_REGIONS = [
    "us-east-1",       # N. Virginia
    "us-east-2",       # Ohio
    "us-west-1",       # N. California
    "us-west-2",       # Oregon
    "ca-central-1",    # Canada
    "eu-west-1",       # Ireland
    "eu-west-2",       # London
    "eu-central-1",    # Frankfurt
    "ap-southeast-1",  # Singapore
    "ap-southeast-2",  # Sydney
    "ap-northeast-1",  # Tokyo
    "ap-south-1",      # Mumbai
]

_GCP_MAJOR_REGIONS = [
    "us-central1",
    "us-east1",
    "us-west1",
    "us-west2",
    "europe-west1",
    "europe-west2",
    "europe-west3",
    "europe-west4",
    "asia-east1",
    "asia-northeast1",
    "asia-southeast1",
    "australia-southeast1",
]


def register_availability_tools(mcp: Any) -> None:

    @mcp.tool()
    async def list_regions(
        ctx: Context,
        provider: str,
        domain: str = "compute",
    ) -> dict[str, Any]:
        """
        List all regions where a cloud service is available for the given provider.

        Args:
            provider: Cloud provider — "aws", "gcp", or "azure"
            domain: Domain filter — "compute" (default), "storage", "database"
        """
        pvdr = ctx.request_context.lifespan_context["providers"].get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            regions = await pvdr.list_regions(domain)
        except Exception as e:
            logger.error("list_regions error: %s", e)
            return {"error": str(e)}

        return {
            "provider": provider,
            "domain": domain,
            "regions": [
                {"code": r, "name": region_display_name(provider, r)}
                for r in regions
            ],
            "count": len(regions),
        }

    @mcp.tool()
    async def list_instance_types(
        ctx: Context,
        provider: str,
        region: str,
        family: str = "",
        min_vcpu: int | None = None,
        min_memory_gb: float | None = None,
        gpu: bool = False,
    ) -> dict[str, Any]:
        """
        List available compute instance types matching the given filters.

        Args:
            provider: Cloud provider — "aws", "gcp", or "azure"
            region: Region code, e.g. "us-east-1" (AWS), "us-central1" (GCP), "eastus" (Azure)
            family: Instance family prefix filter, e.g. "m5" (AWS), "n2" (GCP)
            min_vcpu: Minimum vCPU count filter
            min_memory_gb: Minimum memory in GB filter
            gpu: If True, only return GPU-enabled instance types
        """
        pvdr = ctx.request_context.lifespan_context["providers"].get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            types = await pvdr.list_instance_types(
                region=region,
                family=family or None,
                min_vcpus=min_vcpu,
                min_memory_gb=min_memory_gb,
                gpu=gpu,
            )
        except Exception as e:
            logger.error("list_instance_types error: %s", e)
            return {"error": str(e)}

        return {
            "provider": provider,
            "region": region,
            "count": len(types),
            "instance_types": [
                {
                    "instance_type": t.instance_type,
                    "vcpu": t.vcpu,
                    "memory_gb": t.memory_gb,
                    "gpu_count": t.gpu_count,
                    "gpu_type": t.gpu_type,
                    "network_performance": t.network_performance,
                }
                for t in types
            ],
        }

    @mcp.tool()
    async def describe_catalog(
        ctx: Context,
        provider: str = "",
        domain: str = "",
        service: str = "",
    ) -> dict[str, Any]:
        """
        Discover what each provider supports and how to call get_price.

        - No args → full support matrix across all configured providers.
        - provider only → all domains/services for that provider.
        - provider + domain [+ service] → targeted guidance with required_fields,
          supported_terms, filter_hints, and a ready-to-use example_invocation
          you can pass directly to get_price.

        Use this before get_price when unsure of exact field names or values.

        Args:
            provider: Cloud provider — "aws", "gcp", or "azure". Empty = all providers.
            domain: Domain — "compute", "storage", "database", "ai", "container",
                    "serverless", "analytics", "network", "observability". Empty = all.
            service: Service — e.g. "bedrock", "rds", "gke", "bigquery". Empty = all.
        """
        providers_map = ctx.request_context.lifespan_context["providers"]

        # Determine which providers to query
        if provider:
            pvdr_names = [provider] if provider in providers_map else []
            if not pvdr_names:
                return {"error": f"Provider '{provider}' not configured."}
        else:
            pvdr_names = list(providers_map)

        if not domain:
            # Full matrix — collect catalogs from all providers
            catalogs = {}
            for pname in pvdr_names:
                pvdr = providers_map[pname]
                if not hasattr(pvdr, "describe_catalog"):
                    continue
                try:
                    cat = await pvdr.describe_catalog()
                    catalogs[pname] = {
                        "domains": [d.value for d in cat.domains],
                        "services": cat.services,
                        "decision_matrix": cat.decision_matrix,
                    }
                except Exception as e:
                    catalogs[pname] = {"error": str(e)}
            return {
                "support_matrix": catalogs,
                "tip": (
                    "Call describe_catalog(provider, domain) for field-level guidance "
                    "and a copy-paste example_invocation."
                ),
            }

        # Targeted mode — one provider + domain [+ service]
        pname = pvdr_names[0]
        pvdr = providers_map[pname]
        if not hasattr(pvdr, "describe_catalog"):
            return {"error": f"Provider '{pname}' does not implement describe_catalog."}

        try:
            cat = await pvdr.describe_catalog()
        except Exception as e:
            return {"error": str(e)}

        svc_key = f"{domain}/{service}" if service else domain
        out: dict[str, Any] = {
            "provider": pname,
            "domain": domain,
            "service": service or None,
        }

        # Supported terms
        terms = cat.supported_terms.get(svc_key) or cat.supported_terms.get(domain)
        if terms:
            out["supported_terms"] = terms

        # Filter hints
        hints = cat.filter_hints.get(svc_key) or cat.filter_hints.get(domain)
        if hints:
            out["filter_hints"] = hints

        # Example invocation
        example = (
            cat.example_invocations.get(svc_key)
            or cat.example_invocations.get(domain)
        )
        if example:
            out["example_invocation"] = example
            out["usage"] = "Pass example_invocation directly to get_price(spec=...)."

        if not terms and not hints and not example:
            out["available_services"] = cat.services.get(domain, [])
            out["tip"] = (
                f"Specify service= to get targeted guidance. "
                f"Available services for {domain}: {cat.services.get(domain, [])}"
            )

        return out

    @mcp.tool()
    async def find_cheapest_region(
        ctx: Context,
        spec: dict[str, Any],
        regions: list[str] | None = None,
        baseline_region: str = "",
    ) -> dict[str, Any]:
        """
        Find the cheapest region for any cloud service.

        Queries pricing concurrently across regions and returns results sorted cheapest
        first, with the price delta between cheapest and most expensive regions.

        Args:
            spec: PricingSpec dict (same as get_price). The region field is overridden
                  for each comparison — pass any region in the spec.
            regions: List of region codes to compare. Omit for major regions (faster).
                     Pass ["all"] to search every available region (slow on first run without cache).
            baseline_region: Optional region for delta comparison, e.g. "us-east-1".
        """
        try:
            base_spec = _SPEC_ADAPTER.validate_python(spec)
        except Exception as e:
            return {"error": "invalid_spec", "reason": str(e)}

        pvdr = ctx.request_context.lifespan_context["providers"].get(base_spec.provider.value)
        if pvdr is None:
            return {"error": f"Provider '{base_spec.provider.value}' not configured."}

        provider_str = base_spec.provider.value
        all_regions_requested = regions == ["all"]

        if not regions or all_regions_requested:
            all_available = await pvdr.list_regions("compute")
            if provider_str == "aws" and not all_regions_requested:
                regions = _AWS_MAJOR_REGIONS
                scoped = True
            elif provider_str == "gcp" and not all_regions_requested:
                regions = _GCP_MAJOR_REGIONS
                scoped = True
            else:
                regions = all_available
                scoped = False
        else:
            scoped = False

        semaphore = asyncio.Semaphore(10)
        config_errors: list[str] = []

        async def fetch_one(region: str) -> tuple[str, list]:
            async with semaphore:
                try:
                    spec_r = base_spec.model_copy(update={"region": region})
                    result = await pvdr.get_price(spec_r)
                    return region, result.public_prices
                except NotConfiguredError as e:
                    if not config_errors:
                        config_errors.append(str(e))
                    return region, []
                except Exception:
                    return region, []

        raw = await asyncio.gather(*[fetch_one(r) for r in regions])

        all_prices = []
        not_available = []
        for region, prices in raw:
            if prices:
                all_prices.extend(prices)
            else:
                not_available.append(region)

        if not all_prices:
            if config_errors:
                return {
                    "result": "provider_not_configured",
                    "provider": provider_str,
                    "error": config_errors[0],
                }
            return {"result": "no_prices_found",
                    "message": "No pricing found in any region."}

        all_prices.sort(key=lambda p: p.price_per_unit)

        entries = []
        for p in all_prices:
            entry: dict[str, Any] = {
                "region": p.region,
                "region_name": region_display_name(provider_str, p.region),
                "price_per_unit": f"${p.price_per_unit:.6f}/{p.unit.value}",
            }
            if p.unit.value == "per_hour":
                entry["monthly_estimate"] = f"${p.monthly_cost:.2f}/mo"
            entries.append(entry)

        if baseline_region:
            try:
                apply_baseline_deltas(entries, baseline_region)
            except ValueError as e:
                return {"error": str(e)}

        cheapest = all_prices[0]
        most_exp = all_prices[-1]

        out: dict[str, Any] = {
            "provider": provider_str,
            "domain": base_spec.domain.value,
            "service": base_spec.service,
            "cheapest_region": cheapest.region,
            "cheapest_region_name": region_display_name(provider_str, cheapest.region),
            "cheapest_price": f"${cheapest.price_per_unit:.6f}/{cheapest.unit.value}",
            "most_expensive_region": most_exp.region,
            "most_expensive_price": f"${most_exp.price_per_unit:.6f}/{most_exp.unit.value}",
            "price_delta_pct": (
                round(float(
                    (most_exp.price_per_unit - cheapest.price_per_unit)
                    / cheapest.price_per_unit * 100
                ), 1)
                if cheapest.price_per_unit > 0 else None
            ),
            "all_regions_sorted": entries,
            "not_available_in": not_available or None,
        }
        if baseline_region:
            out["baseline_region"] = baseline_region
            out["baseline_region_name"] = region_display_name(provider_str, baseline_region)
        if scoped:
            out["note"] = (
                f"Searched {len(regions)} major {provider_str.upper()} regions. "
                "Pass regions=['all'] to search all available regions (slower on first run)."
            )
        return out

    @mcp.tool()
    async def find_available_regions(
        ctx: Context,
        spec: dict[str, Any],
        regions: list[str] | None = None,
        baseline_region: str = "",
    ) -> dict[str, Any]:
        """
        Find all regions where a specific service/instance type is available, cheapest first.

        Args:
            spec: PricingSpec dict (same as get_price). The region field is overridden
                  per comparison — pass any region in the spec.
            regions: Region codes to check. Omit for major regions.
                     Pass ["all"] to search every available region.
            baseline_region: Optional region for delta comparison.
        """
        try:
            base_spec = _SPEC_ADAPTER.validate_python(spec)
        except Exception as e:
            return {"error": "invalid_spec", "reason": str(e)}

        pvdr = ctx.request_context.lifespan_context["providers"].get(base_spec.provider.value)
        if pvdr is None:
            return {"error": f"Provider '{base_spec.provider.value}' not configured."}

        provider_str = base_spec.provider.value
        all_regions_requested = regions == ["all"]

        if not regions or all_regions_requested:
            all_available = await pvdr.list_regions("compute")
            if provider_str == "aws" and not all_regions_requested:
                regions = _AWS_MAJOR_REGIONS
                scoped = True
            elif provider_str == "gcp" and not all_regions_requested:
                regions = _GCP_MAJOR_REGIONS
                scoped = True
            else:
                regions = all_available
                scoped = False
        else:
            scoped = False

        semaphore = asyncio.Semaphore(10)

        async def fetch_one(region: str) -> tuple[str, list]:
            async with semaphore:
                try:
                    spec_r = base_spec.model_copy(update={"region": region})
                    result = await pvdr.get_price(spec_r)
                    return region, result.public_prices
                except Exception:
                    return region, []

        raw = await asyncio.gather(*[fetch_one(r) for r in regions])

        available = []
        not_available = []
        for region, prices in raw:
            if prices:
                available.append((region, prices[0]))
            else:
                not_available.append(region)

        if not available:
            return {"result": "not_available",
                    "message": "Not available in any of the checked regions."}

        available.sort(key=lambda x: x[1].price_per_unit)

        entries = []
        for region, p in available:
            entry: dict[str, Any] = {
                "region": region,
                "region_name": region_display_name(provider_str, region),
                "price_per_unit": f"${p.price_per_unit:.6f}/{p.unit.value}",
            }
            if p.unit.value == "per_hour":
                entry["monthly_estimate"] = f"${p.monthly_cost:.2f}/mo"
            entries.append(entry)

        if baseline_region:
            try:
                apply_baseline_deltas(entries, baseline_region)
            except ValueError as e:
                return {"error": str(e)}

        out: dict[str, Any] = {
            "provider": provider_str,
            "domain": base_spec.domain.value,
            "available_in": len(available),
            "not_available_in": not_available or None,
            "regions_sorted_cheapest_first": entries,
        }
        if baseline_region:
            out["baseline_region"] = baseline_region
        if scoped:
            out["note"] = (
                f"Searched {len(regions)} major {provider_str.upper()} regions. "
                "Pass regions=['all'] to check all available regions."
            )
        return out

    @mcp.tool()
    async def cache_stats(ctx: Context) -> dict[str, Any]:
        """Return statistics about the local pricing cache (entry counts, DB size)."""
        cache = ctx.request_context.lifespan_context["cache"]
        return await cache.stats()
