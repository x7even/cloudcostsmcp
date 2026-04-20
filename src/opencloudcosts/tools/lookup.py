"""Price lookup MCP tools — v0.8.0 consolidated surface."""
from __future__ import annotations

import asyncio
import logging
from typing import Any

from mcp.server.fastmcp import Context
from pydantic import TypeAdapter

from opencloudcosts.models import ComputePricingSpec, PricingSpec
from opencloudcosts.providers.base import NotConfiguredError, NotSupportedError

logger = logging.getLogger(__name__)

_SPEC_ADAPTER: TypeAdapter[PricingSpec] = TypeAdapter(PricingSpec)


def _providers(ctx: Context) -> dict[str, Any]:
    return ctx.request_context.lifespan_context["providers"]


def _provider_for(ctx: Context, provider_name: str) -> Any | None:
    return _providers(ctx).get(provider_name)


def register_lookup_tools(mcp: Any) -> None:
    """Register all lookup tools onto the FastMCP app."""

    @mcp.tool()
    async def get_price(
        ctx: Context,
        spec: dict[str, Any],
    ) -> dict[str, Any]:
        """
        Unified pricing tool — returns public catalog rates plus contracted/effective prices
        where credentials are available.

        Pass a spec dict with at minimum: provider, domain, region.
        Domain-specific required fields (call describe_catalog for the complete list):

          COMPUTE  : resource_type ("m5.xlarge" / "n1-standard-4" / "Standard_D4s_v3")
                     os ("Linux" or "Windows"), term ("on_demand"/"spot"/"cud_1yr")
                     Fargate: vcpu (e.g. 2.0), memory_gb (e.g. 4.0), service="fargate"
          STORAGE  : storage_type ("gp3"/"standard"/"nearline"/"premium-ssd")
          DATABASE : resource_type ("db.r5.large"/"db-n1-standard-4"), engine ("MySQL"),
                     deployment ("single-az"/"ha"/"multi-az"), service ("rds"/"cloud_sql"/"memorystore")
          AI       : model ("claude-3-5-sonnet"/"gemini-1.5-flash"), service ("bedrock"/"gemini"/"vertex"),
                     input_tokens, output_tokens  |  machine_type + task for Vertex
          CONTAINER: service ("gke"/"eks"), mode ("standard"/"autopilot"), node_count, vcpu, memory_gb
          ANALYTICS: service ("bigquery"), query_tb, active_storage_gb, longterm_storage_gb, streaming_gb
          NETWORK  : service ("cloud_lb"/"cloud_cdn"/"cloud_nat"/"cloud_armor"),
                     lb_type, rule_count, data_gb, gateway_count, egress_gb, policy_count
          OBSERVABILITY: service ("cloudwatch"/"cloud_monitoring"), ingestion_mib, log_gb

        Returns public_prices[] always. When auth exists: contracted_prices[], effective_price,
        auth_available=true.

        Call describe_catalog(provider, domain, service) for an example_invocation you can
        copy directly into this tool.

        Args:
            spec: PricingSpec dict — see field descriptions above.

        Examples:
            {"provider": "aws",   "domain": "compute",     "resource_type": "m5.xlarge",       "region": "us-east-1"}
            {"provider": "aws",   "domain": "ai",          "service": "bedrock", "model": "claude-3-5-sonnet", "region": "us-east-1", "input_tokens": 1000000, "output_tokens": 1000000}
            {"provider": "gcp",   "domain": "compute",     "resource_type": "n1-standard-4",   "region": "us-central1", "term": "cud_1yr"}
            {"provider": "gcp",   "domain": "analytics",   "service": "bigquery", "query_tb": 10.0, "active_storage_gb": 500.0, "region": "us"}
            {"provider": "azure", "domain": "compute",     "resource_type": "Standard_D4s_v3", "region": "eastus"}
            {"provider": "aws",   "domain": "database",    "service": "rds", "resource_type": "db.r5.large", "engine": "MySQL", "deployment": "single-az", "region": "us-east-1"}
        """
        try:
            parsed = _SPEC_ADAPTER.validate_python(spec)
        except Exception as e:
            return {
                "error": "invalid_spec",
                "reason": str(e),
                "hint": (
                    "Call describe_catalog(provider, domain, service) to get a valid "
                    "example_invocation for your provider/domain/service combination."
                ),
            }

        if not parsed.region:
            _DEFAULT_REGIONS = {"aws": "us-east-1", "gcp": "us-central1", "azure": "eastus"}
            default = _DEFAULT_REGIONS.get(parsed.provider.value, "us-east-1")
            parsed = parsed.model_copy(update={"region": default})
            logger.info("get_price: region not specified, defaulting to %s for %s", default, parsed.provider.value)

        pvdr = _provider_for(ctx, parsed.provider.value)
        if pvdr is None:
            return {"error": f"Provider '{parsed.provider.value}' not configured. "
                             f"Available: {list(_providers(ctx))}"}

        if not pvdr.supports(parsed.domain, parsed.service):
            return NotSupportedError(
                provider=parsed.provider,
                domain=parsed.domain,
                service=parsed.service,
                reason=(
                    f"{parsed.provider.value} does not support "
                    f"{parsed.domain.value}/{parsed.service}."
                ),
                alternatives=["Call describe_catalog() to see what this provider supports."],
            ).to_response()

        try:
            result = await pvdr.get_price(parsed)
            return result.summary()
        except NotSupportedError as e:
            return e.to_response()
        except NotConfiguredError as e:
            return {
                "error": "not_configured",
                "reason": str(e),
                "hint": "Configure provider credentials to enable this feature.",
            }
        except Exception as e:
            logger.error("get_price error: %s", e)
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
        Get prices for multiple compute instance types in a single region in one call.

        Fetches all prices concurrently. Useful for comparing a shortlist of candidate
        instance types (e.g. m5.xlarge vs c5.xlarge vs r5.xlarge) without separate calls.

        Args:
            provider: Cloud provider — "aws", "gcp", or "azure"
            instance_types: List of instance types, e.g. ["m5.xlarge", "c5.xlarge", "r5.large"]
            region: Region code, e.g. "us-east-1" or "us-central1"
            os: Operating system — "Linux" (default) or "Windows"
            term: Pricing term — "on_demand" (default), "spot", "reserved_1yr", "cud_1yr"
        """
        pvdr = _provider_for(ctx, provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        async def fetch_one(itype: str) -> tuple[str, list | str]:
            try:
                spec = _SPEC_ADAPTER.validate_python({
                    "provider": provider, "domain": "compute",
                    "resource_type": itype, "region": region, "os": os, "term": term,
                })
                result = await pvdr.get_price(spec)
                return itype, result.public_prices
            except Exception as e:
                return itype, str(e)

        raw = await asyncio.gather(*[fetch_one(t) for t in instance_types])

        results = []
        errors: dict[str, str] = {}
        for itype, outcome in raw:
            if isinstance(outcome, str):
                errors[itype] = outcome
            elif not outcome:
                errors[itype] = "no pricing found"
            else:
                p = outcome[0]
                entry: dict[str, Any] = {
                    "instance_type": itype,
                    "price_per_hour": f"${p.price_per_unit:.6f}",
                    "monthly_estimate": f"${p.monthly_cost:.2f}/mo",
                }
                entry.update({k: v for k, v in p.summary().items()
                               if k in ("vcpu", "memory", "description")})
                results.append(entry)

        results.sort(key=lambda x: x["price_per_hour"])

        out: dict[str, Any] = {
            "provider": provider, "region": region, "os": os, "term": term,
            "count": len(results), "results": results,
        }
        if errors:
            out["errors"] = errors
        return out

    @mcp.tool()
    async def compare_prices(
        ctx: Context,
        spec: dict[str, Any],
        regions: list[str],
        baseline_region: str = "",
    ) -> dict[str, Any]:
        """
        Compare pricing for any service across multiple regions.

        Fetches concurrently. Returns results sorted cheapest first, with % delta between
        cheapest and most expensive. Optionally shows delta vs a baseline region.

        Args:
            spec: PricingSpec dict (same as get_price). The region field is overridden
                  per comparison — you can pass any region in the spec.
            regions: List of region codes to compare, e.g. ["us-east-1", "eu-west-1", "ap-northeast-1"]
            baseline_region: Optional region for delta comparison, e.g. "us-east-1".
        """
        from opencloudcosts.utils.regions import region_display_name
        try:
            base_spec = _SPEC_ADAPTER.validate_python(spec)
        except Exception as e:
            return {"error": "invalid_spec", "reason": str(e)}

        pvdr = _provider_for(ctx, base_spec.provider.value)
        if pvdr is None:
            return {"error": f"Provider '{base_spec.provider.value}' not configured."}

        if not pvdr.supports(base_spec.domain, base_spec.service):
            return {"error": f"{base_spec.provider.value} does not support "
                             f"{base_spec.domain.value}/{base_spec.service}."}

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

        all_prices = []
        not_available = []
        for region, prices in raw:
            if prices:
                all_prices.extend(prices)
            else:
                not_available.append(region)

        if not all_prices:
            return {"result": "no_prices_found",
                    "message": "No pricing found in any of the specified regions."}

        all_prices.sort(key=lambda p: p.price_per_unit)

        provider_str = base_spec.provider.value
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
            from opencloudcosts.utils.baseline import apply_baseline_deltas
            try:
                apply_baseline_deltas(entries, baseline_region, hourly_key="price_per_unit")
            except ValueError as e:
                return {"error": str(e)}

        cheapest = all_prices[0]
        most_exp = all_prices[-1]

        out: dict[str, Any] = {
            "provider": provider_str,
            "domain": base_spec.domain.value,
            "service": base_spec.service,
            "cheapest_region": cheapest.region,
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
        }
        if not_available:
            out["not_available_in"] = not_available
        if baseline_region:
            out["baseline_region"] = baseline_region
        return out

    @mcp.tool()
    async def search_pricing(
        ctx: Context,
        provider: str,
        query: str,
        domain: str = "",
        region: str = "",
        max_results: int = 20,
    ) -> dict[str, Any]:
        """
        Free-text search across the pricing catalog.

        Useful for exploring what SKUs are available for a service before calling get_price,
        or for finding pricing for services not yet covered by a specific domain.

        Args:
            provider: Cloud provider — "aws", "gcp", or "azure"
            query: Search string, e.g. "NAT gateway", "CloudWatch metrics", "Lambda duration"
            domain: Optional domain filter — "compute", "storage", "database", etc.
            region: Optional region filter
            max_results: Maximum results to return (default 20)
        """
        pvdr = _provider_for(ctx, provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        if not hasattr(pvdr, "search_pricing"):
            return {"error": f"Provider '{provider}' does not support search_pricing."}

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
            "tip": (
                "Use get_price(spec) with a typed spec for cost estimates. "
                "Call describe_catalog(provider) to see supported domains and services."
            ),
        }

    @mcp.tool()
    async def get_discount_summary(
        ctx: Context,
        provider: str = "aws",
    ) -> dict[str, Any]:
        """
        Return a summary of all active cloud discounts for the authenticated account.

        For AWS: active Savings Plans (type, commitment $/hr, utilization %) and
        active Reserved Instances (instance type, count, payment type, days remaining),
        plus Cost Explorer utilization for the previous month.

        Requires credentials and OCC_AWS_ENABLE_COST_EXPLORER=true for AWS.

        Args:
            provider: Cloud provider — "aws" (GCP CUD support coming later)
        """
        pvdr = _provider_for(ctx, provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        try:
            return await pvdr.get_discount_summary()
        except NotSupportedError as e:
            return e.to_response()
        except NotConfiguredError as e:
            return {
                "error": str(e),
                "hint": "Set OCC_AWS_ENABLE_COST_EXPLORER=true and configure AWS credentials.",
            }
        except Exception as e:
            logger.error("get_discount_summary error: %s", e)
            return {"error": f"API error: {e}"}

    @mcp.tool()
    async def get_spot_history(
        ctx: Context,
        spec: dict[str, Any],
        hours: int = 24,
        availability_zone: str = "",
    ) -> dict[str, Any]:
        """
        Get spot price history and stability analysis for an AWS EC2 instance type.

        Returns per-AZ spot price statistics (current, min, max, avg, sample count),
        overall volatility ratio, a stability label, and an actionable recommendation.
        Requires AWS credentials.

        Args:
            spec: PricingSpec dict with provider="aws", domain="compute", resource_type (instance type), region.
                  Example: {"provider": "aws", "domain": "compute", "resource_type": "m5.xlarge", "region": "us-east-1"}
            hours: Lookback window in hours (default 24, max 720)
            availability_zone: Filter to a specific AZ, e.g. "us-east-1a". Empty = all AZs.
        """
        try:
            parsed = _SPEC_ADAPTER.validate_python(spec)
        except Exception as e:
            return {"error": "invalid_spec", "reason": str(e)}

        if parsed.provider.value != "aws":
            return {"error": "get_spot_history only supports provider='aws'."}

        if not isinstance(parsed, ComputePricingSpec):
            return {"error": "get_spot_history requires domain='compute'."}

        pvdr = _provider_for(ctx, "aws")
        if pvdr is None:
            return {"error": "AWS provider not configured."}

        try:
            result = await pvdr._get_spot_history(
                instance_type=parsed.resource_type or "",
                region=parsed.region,
                os=parsed.os or "Linux",
                availability_zone=availability_zone,
                hours=hours,
            )
        except ValueError as e:
            return {"error": str(e)}
        except Exception as e:
            logger.error("get_spot_history error: %s", e)
            return {"error": f"API error: {e}"}

        if not result:
            from opencloudcosts.utils.regions import region_display_name
            return {
                "result": "no_data",
                "message": (
                    f"No spot price history found for {parsed.resource_type} in {parsed.region}. "
                    "Check instance type spelling or try a different region."
                ),
                "region_name": region_display_name("aws", parsed.region),
            }

        from opencloudcosts.utils.regions import region_display_name
        result["provider"] = "aws"
        result["region_name"] = region_display_name("aws", parsed.region)
        return result

    @mcp.tool()
    async def refresh_cache(
        ctx: Context,
        provider: str = "",
    ) -> dict[str, Any]:
        """
        Invalidate the pricing cache to force fresh data on next request.

        Args:
            provider: Provider to clear ("aws", "gcp", "azure"), or empty string to purge expired entries.
        """
        cache = ctx.request_context.lifespan_context["cache"]
        if provider:
            counts = await cache.clear_provider(provider)
            return {
                "message": f"Cache cleared for provider: {provider}",
                "prices_deleted": counts["prices_deleted"],
                "metadata_deleted": counts["metadata_deleted"],
            }
        else:
            deleted = await cache.purge_expired()
            stats = await cache.stats()
            return {"message": f"Purged {deleted} expired entries", "cache_stats": stats}
