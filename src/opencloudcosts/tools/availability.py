"""SKU and region availability MCP tools."""
from __future__ import annotations

import asyncio
import logging
from decimal import Decimal
from typing import Any

from mcp.server.fastmcp import Context

from opencloudcosts.models import PriceComparison, PricingTerm
from opencloudcosts.utils.baseline import apply_baseline_deltas
from opencloudcosts.utils.regions import region_display_name

logger = logging.getLogger(__name__)

# Default region set for AWS fan-out tools when no regions are specified.
# Querying all ~30 AWS regions cold (no cache) requires downloading ~30 bulk
# pricing files and will time out. This curated list covers the major
# commercial regions. Pass regions=["all"] to search the full set.
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


def register_availability_tools(mcp: Any) -> None:

    @mcp.tool()
    async def list_regions(
        ctx: Context,
        provider: str,
        service: str = "compute",
    ) -> dict[str, Any]:
        """
        List all regions where a cloud service is available for the given provider.

        Returns region codes and their friendly display names.

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

        return {
            "provider": provider,
            "service": service,
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

        instances.sort(key=lambda i: (i.vcpu, i.memory_gb))
        instances = instances[:max_results]

        return {
            "provider": provider,
            "region": region,
            "region_name": region_display_name(provider, region),
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
            "region_name": region_display_name(provider, region),
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
        baseline_region: str = "",
    ) -> dict[str, Any]:
        """
        Find the cheapest region for a given compute instance type across all (or specified) regions.

        Queries pricing concurrently across regions and returns results sorted cheapest first,
        with the price delta between cheapest and most expensive regions.

        Optionally compares every region against a baseline (e.g. baseline_region="us-east-1")
        to show delta $/% vs that reference point.

        Args:
            provider: Cloud provider — "aws" or "gcp"
            instance_type: Instance type, e.g. "m5.xlarge" or "c5.2xlarge"
            os: Operating system — "Linux" (default) or "Windows"
            term: Pricing term — "on_demand" (default), "reserved_1yr", "reserved_3yr"
            regions: List of region codes to compare. Omit for major regions (faster).
                     Pass ["all"] to search every available region (slow on first run without cache).
            baseline_region: Optional region code to use as comparison baseline, e.g. "us-east-1".
                             Each result will include delta_per_hour, delta_monthly, delta_pct.
        """
        pvdr = ctx.request_context.lifespan_context["providers"].get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            pricing_term = PricingTerm(term)
        except ValueError:
            return {"error": f"Unknown term '{term}'."}

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

        sorted_entries = [
            {
                "region": p.region,
                "region_name": region_display_name(provider, p.region),
                "price_per_hour": f"${p.price_per_unit:.6f}",
                "monthly_estimate": f"${p.monthly_cost:.2f}/mo",
            }
            for p in comparison.results
        ]

        if baseline_region:
            try:
                apply_baseline_deltas(sorted_entries, baseline_region)
            except ValueError as e:
                return {"error": str(e)}

        cheapest = comparison.cheapest
        most_exp = comparison.most_expensive
        result: dict[str, Any] = {
            "provider": provider,
            "instance_type": instance_type,
            "os": os,
            "term": term,
            "cheapest_region": cheapest.region if cheapest else None,
            "cheapest_region_name": region_display_name(provider, cheapest.region) if cheapest else None,
            "cheapest_price": f"${cheapest.price_per_unit:.6f}/hr" if cheapest else None,
            "most_expensive_region": most_exp.region if most_exp else None,
            "most_expensive_region_name": region_display_name(provider, most_exp.region) if most_exp else None,
            "most_expensive_price": f"${most_exp.price_per_unit:.6f}/hr" if most_exp else None,
            "price_delta_pct": comparison.price_delta_pct,
            "all_regions_sorted": sorted_entries,
            "not_available_in": not_available if not_available else None,
        }
        if baseline_region:
            result["baseline_region"] = baseline_region
            result["baseline_region_name"] = region_display_name(provider, baseline_region)
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
        regions: list[str] | None = None,
        baseline_region: str = "",
    ) -> dict[str, Any]:
        """
        Find all regions where a specific compute instance type is available.

        Queries regions concurrently and returns the ones where pricing exists,
        sorted cheapest first. Includes friendly region names, vCPU/memory specs,
        and optional baseline delta comparison.

        For AWS, defaults to major regions to avoid cold-cache timeouts.
        Pass regions=["all"] to search every available region.

        Args:
            provider: Cloud provider — "aws" or "gcp"
            instance_type: Instance type, e.g. "c6a.xlarge" or "n2-standard-4"
            os: Operating system — "Linux" (default) or "Windows"
            term: Pricing term — "on_demand" (default), "reserved_1yr", "spot"
            include_prices: If true (default), include per-hour price and monthly estimate
            regions: Specific region codes to search. Pass ["all"] for every region.
                     Omit to use major regions for AWS (faster on first run).
            baseline_region: Region to use as comparison baseline, e.g. "us-east-1".
                             Adds delta_per_hour, delta_monthly, delta_pct to each result.
        """
        pvdr = ctx.request_context.lifespan_context["providers"].get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            pricing_term = PricingTerm(term)
        except ValueError:
            return {"error": f"Unknown term '{term}'."}

        all_regions_requested = regions == ["all"]
        if not regions or all_regions_requested:
            all_available = await pvdr.list_regions("compute")
            if provider == "aws" and not all_regions_requested:
                search_regions = _AWS_MAJOR_REGIONS
                scoped = True
            else:
                search_regions = all_available
                scoped = False
        else:
            search_regions = regions
            scoped = False

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
                p = prices[0]
                entry: dict[str, Any] = {
                    "region": region,
                    "region_name": region_display_name(provider, region),
                }
                if include_prices:
                    entry["price_per_hour"] = f"${p.price_per_unit:.6f}"
                    entry["monthly_estimate"] = f"${p.monthly_cost:.2f}/mo"
                    if "vcpu" in p.attributes:
                        entry["vcpu"] = p.attributes["vcpu"]
                    if "memory" in p.attributes:
                        entry["memory"] = p.attributes["memory"]
                    entry["_sort_price"] = p.price_per_unit
                available.append(entry)

        if include_prices:
            available.sort(key=lambda x: x.pop("_sort_price", Decimal("0")))

        if baseline_region and include_prices:
            try:
                apply_baseline_deltas(available, baseline_region)
            except ValueError as e:
                return {"error": str(e)}

        result: dict[str, Any] = {
            "provider": provider,
            "instance_type": instance_type,
            "term": term,
            "available_region_count": len(available),
            "available_regions": available,
        }
        if baseline_region:
            result["baseline_region"] = baseline_region
            result["baseline_region_name"] = region_display_name(provider, baseline_region)
        if scoped:
            result["note"] = (
                f"Searched {len(search_regions)} major regions. "
                "Pass regions=['all'] to search all available regions (slower on first run)."
            )
        return result

    @mcp.tool()
    async def cache_stats(ctx: Context) -> dict[str, Any]:
        """Return statistics about the local pricing cache (entry counts, DB size)."""
        cache = ctx.request_context.lifespan_context["cache"]
        return await cache.stats()
