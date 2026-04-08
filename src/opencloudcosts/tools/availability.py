"""SKU and region availability MCP tools."""
from __future__ import annotations

import asyncio
import logging
from typing import Any

# Default region set for find_cheapest_region when no regions are specified.
# Querying all ~30 AWS regions cold (no cache) requires downloading ~30 bulk
# pricing files and will time out. This curated list covers the major
# commercial regions. Pass explicit regions= to search the full set.
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

from mcp.server.fastmcp import Context

from opencloudcosts.models import PriceComparison, PricingTerm

logger = logging.getLogger(__name__)


def register_availability_tools(mcp: Any) -> None:

    @mcp.tool()
    async def list_regions(
        ctx: Context,
        provider: str,
        service: str = "compute",
    ) -> dict[str, Any]:
        """
        List all regions where a cloud service is available for the given provider.

        Args:
            provider: Cloud provider — "aws" or "gcp"
            service: Service type — "compute" (default), "storage", "database"
        """
        pvdr = ctx.request_context.lifespan_context["providers"].get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            regions = await pvdr.list_regions(service)
        except Exception as e:
            logger.error("list_regions error: %s", e)
            return {"error": str(e)}

        return {"provider": provider, "service": service, "regions": regions, "count": len(regions)}

    @mcp.tool()
    async def list_instance_types(
        ctx: Context,
        provider: str,
        region: str,
        family: str = "",
        min_vcpus: int = 0,
        min_memory_gb: float = 0.0,
        gpu: bool = False,
        max_results: int = 30,
    ) -> dict[str, Any]:
        """
        List available compute instance types in a region, with optional filters.

        Useful for discovering what instances are available before fetching pricing,
        or for finding instances that meet specific vCPU/memory/GPU requirements.

        Args:
            provider: Cloud provider — "aws" or "gcp"
            region: Region code, e.g. "us-east-1"
            family: Optional instance family prefix, e.g. "m5", "c6g", "r5" (AWS) or "n2", "c2" (GCP)
            min_vcpus: Minimum number of vCPUs (0 = no filter)
            min_memory_gb: Minimum memory in GB (0 = no filter)
            gpu: If true, only return GPU instances
            max_results: Maximum number of results (default 30)
        """
        pvdr = ctx.request_context.lifespan_context["providers"].get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            instances = await pvdr.list_instance_types(
                region,
                family=family or None,
                min_vcpus=min_vcpus or None,
                min_memory_gb=min_memory_gb or None,
                gpu=gpu,
            )
        except Exception as e:
            logger.error("list_instance_types error: %s", e)
            return {"error": str(e)}

        # Sort by vcpu then memory, truncate
        instances.sort(key=lambda i: (i.vcpu, i.memory_gb))
        instances = instances[:max_results]

        return {
            "provider": provider,
            "region": region,
            "filters": {
                "family": family or None,
                "min_vcpus": min_vcpus or None,
                "min_memory_gb": min_memory_gb or None,
                "gpu": gpu,
            },
            "count": len(instances),
            "instance_types": [
                {
                    "instance_type": i.instance_type,
                    "vcpu": i.vcpu,
                    "memory_gb": i.memory_gb,
                    "gpu_count": i.gpu_count if i.gpu_count else None,
                    "gpu_type": i.gpu_type,
                    "network": i.network_performance,
                    "storage": i.storage,
                }
                for i in instances
            ],
        }

    @mcp.tool()
    async def check_availability(
        ctx: Context,
        provider: str,
        service: str,
        sku_or_type: str,
        region: str,
    ) -> dict[str, Any]:
        """
        Check whether a specific instance type or SKU is available in a given region.

        Args:
            provider: Cloud provider — "aws" or "gcp"
            service: Service type — "compute", "storage"
            sku_or_type: Instance type or SKU ID, e.g. "m5.xlarge" or "gp3"
            region: Region code, e.g. "us-east-1"
        """
        pvdr = ctx.request_context.lifespan_context["providers"].get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            available = await pvdr.check_availability(service, sku_or_type, region)
        except Exception as e:
            logger.error("check_availability error: %s", e)
            return {"error": str(e)}

        return {
            "provider": provider,
            "service": service,
            "sku_or_type": sku_or_type,
            "region": region,
            "available": available,
        }

    @mcp.tool()
    async def find_cheapest_region(
        ctx: Context,
        provider: str,
        instance_type: str,
        os: str = "Linux",
        term: str = "on_demand",
        regions: list[str] | None = None,
    ) -> dict[str, Any]:
        """
        Find the cheapest region for a given compute instance type across all (or specified) regions.

        Queries pricing concurrently across regions and returns results sorted cheapest first,
        with the price delta between cheapest and most expensive regions.

        Args:
            provider: Cloud provider — "aws" or "gcp"
            instance_type: Instance type, e.g. "m5.xlarge" or "c5.2xlarge"
            os: Operating system — "Linux" (default) or "Windows"
            term: Pricing term — "on_demand" (default), "reserved_1yr", "reserved_3yr"
            regions: List of region codes to compare. Omit to compare major regions (faster).
                     Pass ["all"] to search every available region (slow on first run without cache).
        """
        pvdr = ctx.request_context.lifespan_context["providers"].get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            pricing_term = PricingTerm(term)
        except ValueError:
            return {"error": f"Unknown term '{term}'."}

        # Resolve region list — for AWS without an explicit list, default to
        # major regions to avoid cold-cache timeouts (~30 bulk file downloads).
        # The caller can pass regions=["all"] or an explicit list for full coverage.
        all_regions_requested = regions == ["all"]
        if not regions or all_regions_requested:
            all_available = await pvdr.list_regions("compute")
            if provider == "aws" and not all_regions_requested:
                regions = _AWS_MAJOR_REGIONS
                scoped_search = True
            else:
                regions = all_available
                scoped_search = False
        else:
            scoped_search = False
            all_available = regions

        # Fan out concurrently — throttle to 10 at a time to avoid API rate limits
        semaphore = asyncio.Semaphore(10)

        async def fetch_one(region: str) -> tuple[str, list]:
            async with semaphore:
                try:
                    prices = await pvdr.get_compute_price(instance_type, region, os, pricing_term)
                    return region, prices
                except Exception:
                    return region, []

        results_raw = await asyncio.gather(*[fetch_one(r) for r in regions])

        all_prices = []
        not_available = []
        for region, prices in results_raw:
            if prices:
                all_prices.extend(prices)
            else:
                not_available.append(region)

        if not all_prices:
            return {
                "result": "no_prices_found",
                "message": f"No pricing found for {instance_type} in any region.",
            }

        comparison = PriceComparison.from_results(
            f"{instance_type} cheapest region ({provider})", all_prices
        )

        result: dict[str, Any] = {
            "provider": provider,
            "instance_type": instance_type,
            "os": os,
            "term": term,
            "cheapest_region": comparison.cheapest.region if comparison.cheapest else None,
            "cheapest_price": f"${comparison.cheapest.price_per_unit:.6f}/hr" if comparison.cheapest else None,
            "most_expensive_region": comparison.most_expensive.region if comparison.most_expensive else None,
            "most_expensive_price": f"${comparison.most_expensive.price_per_unit:.6f}/hr" if comparison.most_expensive else None,
            "price_delta_pct": comparison.price_delta_pct,
            "all_regions_sorted": [
                {
                    "region": p.region,
                    "price_per_hour": f"${p.price_per_unit:.6f}",
                    "monthly_estimate": f"${p.monthly_cost:.2f}",
                }
                for p in comparison.results
            ],
            "not_available_in": not_available if not_available else None,
        }
        if scoped_search:
            result["note"] = (
                f"Searched {len(regions)} major regions. "
                "Pass regions=['all'] to search all available regions (slower on first run)."
            )
        return result

    @mcp.tool()
    async def find_available_regions(
        ctx: Context,
        provider: str,
        instance_type: str,
        os: str = "Linux",
        term: str = "on_demand",
        include_prices: bool = True,
    ) -> dict[str, Any]:
        """
        Find all regions where a specific compute instance type is available.

        Queries all regions concurrently and returns the ones where pricing exists.
        Optionally includes the price in each available region, sorted cheapest first.

        For AWS without credentials, searches major regions by default (faster).
        Pass the full region list explicitly if you need exhaustive coverage.

        Args:
            provider: Cloud provider — "aws" or "gcp"
            instance_type: Instance type, e.g. "c6a.xlarge" or "n2-standard-4"
            os: Operating system — "Linux" (default) or "Windows"
            term: Pricing term — "on_demand" (default), "reserved_1yr", "spot"
            include_prices: If true (default), include per-hour price for each region
        """
        pvdr = ctx.request_context.lifespan_context["providers"].get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            pricing_term = PricingTerm(term)
        except ValueError:
            return {"error": f"Unknown term '{term}'."}

        all_regions = await pvdr.list_regions("compute")
        # For AWS, default to major regions to avoid cold-cache timeouts
        search_regions = (
            _AWS_MAJOR_REGIONS if provider == "aws" else all_regions
        )
        scoped = provider == "aws" and search_regions is _AWS_MAJOR_REGIONS

        semaphore = asyncio.Semaphore(10)

        async def probe(region: str) -> tuple[str, list]:
            async with semaphore:
                try:
                    prices = await pvdr.get_compute_price(
                        instance_type, region, os, pricing_term
                    )
                    return region, prices
                except Exception:
                    return region, []

        raw = await asyncio.gather(*[probe(r) for r in search_regions])

        available = []
        for region, prices in raw:
            if prices:
                entry: dict[str, Any] = {"region": region}
                if include_prices:
                    p = prices[0]
                    entry["price_per_hour"] = f"${p.price_per_unit:.6f}"
                    entry["monthly_estimate"] = f"${p.monthly_cost:.2f}/mo"
                available.append(entry)

        if include_prices:
            available.sort(key=lambda x: x.get("price_per_hour", ""))

        result: dict[str, Any] = {
            "provider": provider,
            "instance_type": instance_type,
            "term": term,
            "available_region_count": len(available),
            "available_regions": available,
        }
        if scoped:
            result["note"] = (
                f"Searched {len(search_regions)} major regions. "
                "Pass the full list from list_regions() for exhaustive coverage."
            )
        return result

    @mcp.tool()
    async def cache_stats(ctx: Context) -> dict[str, Any]:
        """Return statistics about the local pricing cache (entry counts, DB size)."""
        cache = ctx.request_context.lifespan_context["cache"]
        return await cache.stats()
