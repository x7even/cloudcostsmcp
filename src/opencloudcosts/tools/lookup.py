"""Price lookup MCP tools."""
from __future__ import annotations

import asyncio
import logging
from typing import Any

from mcp.server.fastmcp import Context

from opencloudcosts.models import PriceComparison, PricingTerm
from opencloudcosts.providers.base import NotConfiguredError

logger = logging.getLogger(__name__)


def _providers(ctx: Context) -> dict[str, Any]:
    return ctx.request_context.lifespan_context["providers"]


def _settings(ctx: Context) -> Any:
    return ctx.request_context.lifespan_context["settings"]


def register_lookup_tools(mcp: Any) -> None:
    """Register all lookup tools onto the FastMCP app."""

    @mcp.tool()
    async def get_compute_price(
        ctx: Context,
        provider: str,
        instance_type: str,
        region: str,
        os: str = "Linux",
        term: str = "on_demand",
    ) -> dict[str, Any]:
        """
        Get the price for a specific compute instance type in a cloud region.

        Returns pricing details including per-hour cost, monthly estimate, vCPU, and memory.
        Supports both public on-demand and reserved pricing terms.

        Args:
            provider: Cloud provider — "aws" or "gcp"
            instance_type: Instance type, e.g. "m5.xlarge" (AWS) or "n2-standard-4" (GCP)
            region: Region code, e.g. "us-east-1" (AWS) or "us-east1" (GCP)
            os: Operating system — "Linux" (default) or "Windows"
            term: Pricing term — "on_demand" (default), "reserved_1yr", "reserved_3yr", "spot"
        """
        pvdr = _providers(ctx).get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' is not configured. Available: {list(_providers(ctx))}"}

        try:
            pricing_term = PricingTerm(term)
        except ValueError:
            return {"error": f"Unknown term '{term}'. Valid: {[t.value for t in PricingTerm]}"}

        try:
            prices = await pvdr.get_compute_price(instance_type, region, os, pricing_term)
        except ValueError as e:
            return {"error": str(e)}
        except Exception as e:
            logger.error("get_compute_price error: %s", e)
            return {"error": f"API error: {e}"}

        if not prices:
            return {
                "result": "no_prices_found",
                "message": f"No pricing found for {instance_type} in {region} ({provider}). "
                           "Check instance type spelling or try search_pricing.",
            }

        return {
            "provider": provider,
            "instance_type": instance_type,
            "region": region,
            "os": os,
            "term": term,
            "prices": [p.summary() for p in prices],
            "count": len(prices),
        }

    @mcp.tool()
    async def compare_compute_prices(
        ctx: Context,
        provider: str,
        instance_type: str,
        regions: list[str],
        os: str = "Linux",
        term: str = "on_demand",
    ) -> dict[str, Any]:
        """
        Compare the price of the same compute instance type across multiple regions.

        Returns prices for each region sorted cheapest first, plus the % price delta
        between cheapest and most expensive regions.

        Args:
            provider: Cloud provider — "aws" or "gcp"
            instance_type: Instance type, e.g. "m5.xlarge"
            regions: List of region codes to compare, e.g. ["us-east-1", "ap-southeast-2"]
            os: Operating system — "Linux" (default) or "Windows"
            term: Pricing term — "on_demand" (default), "reserved_1yr", "reserved_3yr"
        """
        pvdr = _providers(ctx).get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            pricing_term = PricingTerm(term)
        except ValueError:
            return {"error": f"Unknown term '{term}'."}

        all_prices = []
        errors = {}
        for region in regions:
            try:
                prices = await pvdr.get_compute_price(instance_type, region, os, pricing_term)
                if prices:
                    all_prices.extend(prices)
                else:
                    errors[region] = "no pricing found"
            except Exception as e:
                errors[region] = str(e)

        comparison = PriceComparison.from_results(
            f"{instance_type} across {regions}", all_prices
        )

        result: dict[str, Any] = {
            "provider": provider,
            "instance_type": instance_type,
            "term": term,
            "prices_by_region": [p.summary() for p in comparison.results],
            "cheapest_region": comparison.cheapest.region if comparison.cheapest else None,
            "most_expensive_region": comparison.most_expensive.region if comparison.most_expensive else None,
            "price_delta_pct": comparison.price_delta_pct,
        }
        if errors:
            result["errors"] = errors
        return result

    @mcp.tool()
    async def get_storage_price(
        ctx: Context,
        provider: str,
        storage_type: str,
        region: str,
        size_gb: float = 100.0,
    ) -> dict[str, Any]:
        """
        Get the price for cloud storage in a given region.

        AWS storage types: gp3, gp2, io1, io2, st1, sc1, standard (EBS), or s3 (object storage).
        GCP storage types: pd-ssd, pd-balanced, pd-standard, pd-extreme.

        Args:
            provider: Cloud provider — "aws" or "gcp"
            storage_type: Storage type, e.g. "gp3", "io2", "pd-ssd"
            region: Region code, e.g. "us-east-1"
            size_gb: Storage size in GB for monthly cost estimation (default 100)
        """
        pvdr = _providers(ctx).get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            prices = await pvdr.get_storage_price(storage_type, region, size_gb)
        except ValueError as e:
            return {"error": str(e)}
        except Exception as e:
            logger.error("get_storage_price error: %s", e)
            return {"error": f"API error: {e}"}

        if not prices:
            return {"result": "no_prices_found", "message": f"No storage pricing for {storage_type} in {region}."}

        result_prices = [p.summary() for p in prices]
        # Add monthly cost estimate for the requested size
        for i, p in enumerate(prices):
            from decimal import Decimal
            monthly = p.price_per_unit * Decimal(str(size_gb))
            result_prices[i]["monthly_estimate_for_size"] = f"${monthly:.2f}/mo for {size_gb} GB"

        return {
            "provider": provider,
            "storage_type": storage_type,
            "region": region,
            "size_gb": size_gb,
            "prices": result_prices,
        }

    @mcp.tool()
    async def search_pricing(
        ctx: Context,
        provider: str,
        query: str,
        region: str = "",
        max_results: int = 10,
    ) -> dict[str, Any]:
        """
        Search the cloud pricing catalog by instance family, type, or keyword.

        Useful for discovering available instance types or finding pricing for
        a category of instances (e.g. all m5 instances, or GPU instances).

        Args:
            provider: Cloud provider — "aws" or "gcp"
            query: Search query, e.g. "m5", "c6g.xlarge", "gpu", "r5.2xlarge"
            region: Optional region code to filter results. Leave empty for any region.
            max_results: Maximum number of results to return (default 10, max 50)
        """
        pvdr = _providers(ctx).get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        max_results = min(max_results, 50)
        try:
            prices = await pvdr.search_pricing(query, region or None, max_results)
        except Exception as e:
            logger.error("search_pricing error: %s", e)
            return {"error": f"API error: {e}"}

        return {
            "provider": provider,
            "query": query,
            "region": region or "all",
            "count": len(prices),
            "results": [p.summary() for p in prices],
        }

    @mcp.tool()
    async def get_effective_price(
        ctx: Context,
        provider: str,
        service: str,
        instance_type: str,
        region: str,
    ) -> dict[str, Any]:
        """
        Get effective/bespoke pricing for an instance type, reflecting actual account discounts
        (Reserved Instances, Savings Plans, Committed Use Discounts, EDP).

        Requires cloud provider credentials with Cost Explorer (AWS) or billing export (GCP) access.
        For AWS, also requires OCC_AWS_ENABLE_COST_EXPLORER=true (note: costs $0.01/call).

        Args:
            provider: Cloud provider — "aws" or "gcp"
            service: Service type — "compute", "storage", "database"
            instance_type: Instance type, e.g. "m5.xlarge"
            region: Region code, e.g. "us-east-1"
        """
        pvdr = _providers(ctx).get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            effective = await pvdr.get_effective_price(service, instance_type, region)
        except NotConfiguredError as e:
            return {"error": str(e), "hint": "Configure credentials to enable effective pricing."}
        except Exception as e:
            logger.error("get_effective_price error: %s", e)
            return {"error": f"API error: {e}"}

        if not effective:
            return {"message": "No effective pricing data found for the given parameters."}

        return {
            "provider": provider,
            "instance_type": instance_type,
            "region": region,
            "effective_prices": [
                {
                    "on_demand_rate": f"${ep.base_price.price_per_unit:.6f}/{ep.base_price.unit.value}",
                    "effective_rate": f"${ep.effective_price_per_unit:.6f}/{ep.base_price.unit.value}",
                    "discount_type": ep.discount_type,
                    "discount_pct": f"{ep.discount_pct:.1f}%",
                    "savings": f"${ep.savings_vs_on_demand:.6f}/{ep.base_price.unit.value}",
                    "commitment": ep.commitment_term,
                    "source": ep.source,
                }
                for ep in effective
            ],
        }

    @mcp.tool()
    async def get_discount_summary(
        ctx: Context,
        provider: str = "aws",
    ) -> dict[str, Any]:
        """
        Return a summary of all active cloud discounts for the authenticated account.

        For AWS: lists active Savings Plans (type, commitment $/hr, utilization %) and
        active Reserved Instances (instance type, count, offering type, days remaining),
        plus utilization metrics from Cost Explorer for the previous month.

        Requires credentials and OCC_AWS_ENABLE_COST_EXPLORER=true for AWS.

        Args:
            provider: Cloud provider — "aws" (GCP support coming in Phase 4)
        """
        pvdr = _providers(ctx).get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            return await pvdr.get_discount_summary()
        except NotConfiguredError as e:
            return {"error": str(e), "hint": "Set OCC_AWS_ENABLE_COST_EXPLORER=true and configure AWS credentials."}
        except Exception as e:
            logger.error("get_discount_summary error: %s", e)
            return {"error": f"API error: {e}"}

    @mcp.tool()
    async def get_prices_batch(
        ctx: Context,
        provider: str,
        instance_types: list[str],
        region: str,
        os: str = "Linux",
        term: str = "on_demand",
    ) -> dict[str, Any]:
        """
        Get prices for multiple instance types in a single region in one call.

        Fetches all prices concurrently. Useful for comparing a shortlist of
        candidate instance types (e.g. m5.xlarge vs c5.xlarge vs r5.xlarge)
        without making separate tool calls.

        Args:
            provider: Cloud provider — "aws" or "gcp"
            instance_types: List of instance types, e.g. ["m5.xlarge", "c5.xlarge", "r5.large"]
            region: Region code, e.g. "us-east-1" or "us-central1"
            os: Operating system — "Linux" (default) or "Windows"
            term: Pricing term — "on_demand" (default), "reserved_1yr", "reserved_3yr", "spot"
        """
        pvdr = _providers(ctx).get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            pricing_term = PricingTerm(term)
        except ValueError:
            return {"error": f"Unknown term '{term}'. Valid: {[t.value for t in PricingTerm]}"}

        async def fetch_one(itype: str) -> tuple[str, list | str]:
            try:
                prices = await pvdr.get_compute_price(itype, region, os, pricing_term)
                return itype, prices
            except Exception as e:
                return itype, str(e)

        raw = await asyncio.gather(*[fetch_one(t) for t in instance_types])

        results = []
        errors = {}
        for itype, outcome in raw:
            if isinstance(outcome, str):
                errors[itype] = outcome
            elif not outcome:
                errors[itype] = "no pricing found"
            else:
                p = outcome[0]
                results.append({
                    "instance_type": itype,
                    "price_per_hour": f"${p.price_per_unit:.6f}",
                    "monthly_estimate": f"${p.monthly_cost:.2f}/mo",
                    **{k: v for k, v in p.summary().items()
                       if k in ("vcpu", "memory", "description")},
                })

        # Sort by price ascending
        results.sort(key=lambda x: x["price_per_hour"])

        out: dict[str, Any] = {
            "provider": provider,
            "region": region,
            "os": os,
            "term": term,
            "count": len(results),
            "results": results,
        }
        if errors:
            out["errors"] = errors
        return out

    @mcp.tool()
    async def refresh_cache(
        ctx: Context,
        provider: str = "",
    ) -> dict[str, Any]:
        """
        Invalidate the pricing cache to force fresh data on next request.

        Args:
            provider: Provider to clear ("aws", "gcp"), or empty string to clear all.
        """
        cache = ctx.request_context.lifespan_context["cache"]
        if provider:
            await cache.clear_provider(provider)
            return {"message": f"Cache cleared for provider: {provider}"}
        else:
            deleted = await cache.purge_expired()
            stats = await cache.stats()
            return {"message": f"Purged {deleted} expired entries", "cache_stats": stats}
